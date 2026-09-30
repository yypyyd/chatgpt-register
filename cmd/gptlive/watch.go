package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"chatgpt-register/internal/codexreg"
	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"
	"chatgpt-register/internal/proxyutil"

	"gorm.io/gorm"
)

// watchOpts 描述注册后的轮询策略。所有时间都相对 AT 的 JWT iat（服务端签发时间）。
type watchOpts struct {
	Label   string
	First   time.Duration // 距 iat 多久发第一次探测
	Every   time.Duration // 之后的间隔
	Total   time.Duration // 观察总时长（距 iat）
	Cookies bool          // 探测时是否带上网页会话 Cookie
}

func jwtIAT(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var c struct {
		IAT int64 `json:"iat"`
	}
	if json.Unmarshal(b, &c) != nil || c.IAT == 0 {
		return time.Time{}
	}
	return time.Unix(c.IAT, 0)
}

func fmtProbes(ps []codexreg.EndpointProbe) string {
	parts := make([]string, 0, len(ps))
	for _, p := range ps {
		short := p.Path[strings.LastIndex(p.Path, "/")+1:]
		parts = append(parts, fmt.Sprintf("%s=%d/%s", short, p.Status, p.Code))
	}
	return strings.Join(parts, " ")
}

// watchAT 按策略轮询同一张 AT，直到 conversation/init 返回 token_revoked 或观察期结束。
// 返回是否仍然存活以及死亡时相对 iat 的分钟数。
func watchAT(ctx context.Context, session codexreg.WebSessionData, at string, opt watchOpts) (alive bool, deathMin float64) {
	iat := jwtIAT(at)
	if iat.IsZero() {
		iat = time.Now()
	}
	langs := session.Languages
	if langs == "" {
		langs = "en-US,en"
	}
	deviceID := codexreg.DeviceIDFromCookies(session.Cookies)
	var cookies []codexreg.WebCookie
	if opt.Cookies {
		cookies = session.Cookies
	}
	next := iat.Add(opt.First)
	n := 0
	for {
		if wait := time.Until(next); wait > 0 {
			select {
			case <-ctx.Done():
				fmt.Printf("[%s] ctx done: %v\n", opt.Label, ctx.Err())
				return true, -1
			case <-time.After(wait):
			}
		}
		n++
		pc, cancel := context.WithTimeout(ctx, 60*time.Second)
		ps, err := codexreg.ProbeAccessTokenEndpoints(pc, codexreg.ProbeIdentity{
			Token: at, Proxy: session.Proxy, Languages: langs, DeviceID: deviceID, UserAgent: session.UserAgent,
		}, cookies)
		cancel()
		age := time.Since(iat).Minutes()
		if err != nil {
			fmt.Printf("[%s] +%.1fmin poll#%d error=%v\n", opt.Label, age, n, err)
		} else {
			fmt.Printf("[%s] +%.1fmin poll#%d %s\n", opt.Label, age, n, fmtProbes(ps))
			for _, p := range ps {
				if p.Code == "token_revoked" {
					fmt.Printf("[%s] REVOKED first seen at +%.1fmin (previous poll ok)\n", opt.Label, age)
					return false, age
				}
			}
		}
		next = next.Add(opt.Every)
		if time.Since(iat) > opt.Total {
			fmt.Printf("[%s] SURVIVED %.1fmin, stop watching\n", opt.Label, time.Since(iat).Minutes())
			return true, -1
		}
	}
}

// registerOne 选一个未用邮箱走一次协议注册并入库，返回结果。
func registerOne(ctx context.Context, db *gorm.DB, proxy, domain, note string) (*codexreg.Result, models.Mailbox, error) {
	mb, err := pickMailbox(db, domain)
	if err != nil {
		return nil, mb, err
	}
	email := mb.Email
	fmt.Printf("mailbox=%s\n", mask(email))
	_ = db.Create(&models.Registration{
		Email: email, MailboxID: mb.ID, Status: "registering", IsMother: true, Note: note,
	}).Error
	mail := mailfetch.New()
	since := time.Now().Add(-30 * time.Second)
	in := codexreg.Input{
		Email:    email,
		Password: codexreg.GenPassword(16),
		Proxy:    proxy,
		Log: func(f string, a ...any) {
			fmt.Println(time.Now().Format("15:04:05"), fmt.Sprintf(f, a...))
		},
		FetchCode: func(ctx context.Context, after time.Time) (string, error) {
			if after.After(since) {
				return fetchCode(ctx, mail, mb, after)
			}
			return fetchCode(ctx, mail, mb, since)
		},
	}
	res, err := codexreg.Register(ctx, in)
	if err != nil {
		_ = db.Model(&models.Registration{}).Where("email = ?", email).Updates(map[string]any{
			"status": "register_failed", "note": err.Error(),
		}).Error
		return nil, mb, err
	}
	fmt.Printf("register_ok engine=%v country=%s ip_prefix=%s cookies=%t\n",
		res.AuthJSON["register_engine"], res.Country, ipPrefix(res.EgressIP), res.AuthJSON["cookies"] != nil)
	if err := saveRegistration(db, mb, in.Password, res); err != nil {
		return nil, mb, err
	}
	_ = db.Model(&models.Registration{}).Where("email = ?", email).Update("note", note).Error
	return res, mb, nil
}

func ipPrefix(ip string) string {
	p := strings.Split(ip, ".")
	if len(p) < 2 {
		return ""
	}
	return p[0] + "." + p[1] + ".x.x"
}

func runWatch(ctx context.Context, db *gorm.DB, proxy, domain string, opt watchOpts) error {
	res, mb, err := registerOne(ctx, db, proxy, domain, "gptlive watch "+opt.Label)
	if err != nil {
		return err
	}
	session, err := codexreg.WebSessionFromAuthData(mustAuthJSON(res.AuthJSON))
	if err != nil {
		session = codexreg.WebSessionData{AccessToken: res.AccessToken, Proxy: proxy, UserAgent: uaOf(res), Languages: langsOf(res)}
	}
	if session.Proxy == "" {
		session.Proxy = proxy
	}
	fmt.Printf("[%s] watch start iat=%s first=%s every=%s total=%s cookies=%t\n",
		opt.Label, jwtIAT(res.AccessToken).UTC().Format("15:04:05"), opt.First, opt.Every, opt.Total, opt.Cookies)
	alive, death := watchAT(ctx, session, res.AccessToken, opt)
	now := time.Now().UTC()
	if !alive {
		_ = db.Model(&models.Registration{}).Where("email = ?", mb.Email).Updates(map[string]any{
			"access_token_status": "revoked", "token_last_error_code": "token_revoked", "alive": "dead",
			"needs_relogin": true, "account_credential_status": "needs_relogin", "alive_checked_at": &now,
		}).Error
		fmt.Printf("[%s] RESULT dead at +%.1fmin\n", opt.Label, death)
		return nil
	}
	_ = db.Model(&models.Registration{}).Where("email = ?", mb.Email).Updates(map[string]any{
		"alive": "alive", "alive_checked_at": &now,
	}).Error
	fmt.Printf("[%s] RESULT alive\n", opt.Label)
	return nil
}

// runOTP 从邮箱里取最新的 OpenAI 验证码（对照实验里给真浏览器用）。
func runOTP(ctx context.Context, db *gorm.DB, email string, since time.Time) error {
	var mb models.Mailbox
	if err := db.Where("email = ?", email).First(&mb).Error; err != nil {
		return err
	}
	code, err := fetchCode(ctx, mailfetch.New(), mb, since)
	if err != nil {
		return err
	}
	fmt.Println("OTP=" + code)
	return nil
}

// runPick 选一个未用邮箱、占位入库，并打印邮箱（真浏览器对照实验用）。
func runPick(db *gorm.DB, domain, note string) error {
	mb, err := pickMailbox(db, domain)
	if err != nil {
		return err
	}
	_ = db.Create(&models.Registration{
		Email: mb.Email, MailboxID: mb.ID, Status: "registering", IsMother: true, Note: note,
	}).Error
	fmt.Println("EMAIL=" + mb.Email)
	return nil
}

// runWatchAT 轮询一张外部给定的 AT（例如真浏览器注册出来的）。
func runWatchAT(ctx context.Context, db *gorm.DB, proxyBase, sessionToken, atFile, cookiesFile, ua, deviceID string, opt watchOpts) error {
	raw, err := os.ReadFile(atFile)
	if err != nil {
		return err
	}
	at := strings.TrimSpace(string(raw))
	proxy := proxyBase
	if sessionToken != "" {
		proxy = proxyutil.WithBestGoSession(proxyBase, sessionToken)
	}
	session := codexreg.WebSessionData{AccessToken: at, Proxy: proxy, UserAgent: ua, Languages: "en-US,en"}
	if cookiesFile != "" {
		b, err := os.ReadFile(cookiesFile)
		if err != nil {
			return err
		}
		var cookies []codexreg.WebCookie
		if err := json.Unmarshal(b, &cookies); err != nil {
			return fmt.Errorf("cookies json: %w", err)
		}
		session.Cookies = cookies
	}
	if deviceID != "" && codexreg.DeviceIDFromCookies(session.Cookies) == "" {
		session.Cookies = append(session.Cookies, codexreg.WebCookie{Name: "oai-did", Value: deviceID, Domain: ".chatgpt.com", Path: "/"})
	}
	alive, death := watchAT(ctx, session, at, opt)
	if alive {
		fmt.Printf("[%s] RESULT alive\n", opt.Label)
	} else {
		fmt.Printf("[%s] RESULT dead at +%.1fmin\n", opt.Label, death)
	}
	return nil
}
