package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"chatgpt-register/internal/codexreg"
	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"
	"chatgpt-register/internal/proxyutil"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

var codeRe = regexp.MustCompile(`\b(\d{6})\b`)

func main() {
	dbPath := flag.String("db", "/opt/chatgpt-register/adskull.db", "sqlite path")
	mode := flag.String("mode", "sentinel", "sentinel|register|session|watch|otp|pick|watchat")
	timeout := flag.Duration("timeout", 30*time.Minute, "overall timeout")
	domain := flag.String("domain", "", "only use this mailbox domain, e.g. hotmail.com")
	label := flag.String("label", "w", "watch label")
	first := flag.Duration("first", time.Minute, "watch: first poll offset after AT iat")
	every := flag.Duration("every", time.Minute, "watch: poll interval")
	total := flag.Duration("total", 45*time.Minute, "watch: total observation after iat")
	withCookies := flag.Bool("cookies", true, "watch: send web session cookies with probes")
	email := flag.String("email", "", "otp: mailbox email")
	sinceUnix := flag.Int64("since", 0, "otp: unix seconds; only mails after this")
	note := flag.String("note", "gptlive", "pick: registration note")
	atFile := flag.String("at-file", "", "watchat: file containing access token")
	cookiesFile := flag.String("cookies-file", "", "watchat: json cookies file")
	ua := flag.String("ua", "", "watchat: user agent")
	deviceID := flag.String("device-id", "", "watchat: oai-did")
	proxySession := flag.String("proxy-session", "", "watchat: BestGo session token to reuse")
	fetchURL := flag.String("url", "", "fetch: url to request through the protocol tls-client")
	flag.Parse()

	db, err := gorm.Open(sqlite.Open(*dbPath), &gorm.Config{})
	if err != nil {
		fatal("open db: %v", err)
	}
	baseProxy := firstProxy(db)
	proxy := baseProxy
	if proxy == "" && *mode != "otp" && *mode != "pick" {
		fatal("no proxy configured")
	}
	proxy = proxyutil.WithBestGoTaskSession(proxy)
	fmt.Printf("mode=%s proxy_host=%s\n", *mode, proxyHost(proxy))

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	wopt := watchOpts{Label: *label, First: *first, Every: *every, Total: *total, Cookies: *withCookies}
	switch *mode {
	case "watch":
		if err := runWatch(ctx, db, proxy, *domain, wopt); err != nil {
			fatal("watch: %v", err)
		}
		return
	case "otp":
		since := time.Now().Add(-2 * time.Minute)
		if *sinceUnix > 0 {
			since = time.Unix(*sinceUnix, 0)
		}
		if err := runOTP(ctx, db, *email, since); err != nil {
			fatal("otp: %v", err)
		}
		return
	case "pick":
		if err := runPick(db, *domain, *note); err != nil {
			fatal("pick: %v", err)
		}
		return
	case "watchat":
		if err := runWatchAT(ctx, db, baseProxy, *proxySession, *atFile, *cookiesFile, *ua, *deviceID, wopt); err != nil {
			fatal("watchat: %v", err)
		}
		return
	case "fetchpost":
		status, body, err := codexreg.FetchPostViaProtocolClient(ctx, proxy, *fetchURL, *note)
		if err != nil {
			fatal("fetchpost: %v", err)
		}
		fmt.Printf("HTTP %d\n%s\n", status, body)
		return
	case "fetch":
		status, body, err := codexreg.FetchViaProtocolClient(ctx, proxy, *fetchURL)
		if err != nil {
			fatal("fetch: %v", err)
		}
		fmt.Printf("HTTP %d\n%s\n", status, body)
		return
	}

	switch *mode {
	case "sentinel":
		smoke, err := codexreg.SmokeSentinel(ctx, proxy, "email_otp_validate")
		if err != nil {
			fatal("sentinel: %v", err)
		}
		fmt.Printf("sentinel_ok flow=%s token_len=%d so_len=%d p_prefix=%s has_t=%t has_c=%t shape=%s\n",
			smoke.Flow, smoke.TokenLen, smoke.SOLen, smoke.PPrefix, smoke.HasT, smoke.HasC, smoke.Shape)
	case "register":
		if err := runRegister(ctx, db, proxy, *domain); err != nil {
			fatal("register: %v", err)
		}
	case "html":
		if err := codexreg.DumpAuthPages(ctx, proxy); err != nil {
			fatal("html: %v", err)
		}
	case "session":
		if err := checkSavedSession(ctx, db, proxy, *domain); err != nil {
			fatal("session: %v", err)
		}
	default:
		fatal("unknown mode %s", *mode)
	}
}

func runRegister(ctx context.Context, db *gorm.DB, proxy, domain string) error {
	mb, err := pickMailbox(db, domain)
	if err != nil {
		return err
	}
	email := mb.Email
	fmt.Printf("mailbox=%s\n", mask(email))
	_ = db.Create(&models.Registration{
		Email: email, MailboxID: mb.ID, Status: "registering", IsMother: true, Note: "gptlive hotmail",
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
		return err
	}
	fmt.Printf("register_ok engine=%v country=%s cookies=%t\n",
		res.AuthJSON["register_engine"], res.Country, res.AuthJSON["cookies"] != nil)
	if err := saveRegistration(db, mb, in.Password, res); err != nil {
		return err
	}

	wait := 20 * time.Minute
	fmt.Printf("reprobe_in=%s\n", wait)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
	}
	session, err := codexreg.WebSessionFromAuthData(mustAuthJSON(res.AuthJSON))
	if err != nil {
		session = codexreg.WebSessionData{AccessToken: res.AccessToken, Proxy: proxy, UserAgent: uaOf(res), Languages: langsOf(res)}
	}
	if session.Proxy == "" {
		session.Proxy = proxy
	}
	alive, err := reportCredential(ctx, session, res.AccessToken)
	if err != nil {
		return err
	}
	if !alive {
		now := time.Now().UTC()
		_ = db.Model(&models.Registration{}).Where("email = ?", email).Updates(map[string]any{
			"access_token_status":       "revoked",
			"token_last_error_code":     "token_revoked",
			"alive":                     "dead",
			"needs_relogin":             true,
			"account_credential_status": "needs_relogin",
			"alive_checked_at":          &now,
		}).Error
		return fmt.Errorf("reprobe failed: token_revoked")
	}
	return nil
}

func langsOf(res *codexreg.Result) string {
	if v, _ := res.AuthJSON["registered_languages"].(string); strings.TrimSpace(v) != "" {
		return v
	}
	return "en-US,en"
}

func uaOf(res *codexreg.Result) string {
	v, _ := res.AuthJSON["user_agent"].(string)
	return v
}

func mustAuthJSON(auth map[string]any) string {
	b, _ := json.Marshal(auth)
	return string(b)
}

func reportCredential(ctx context.Context, session codexreg.WebSessionData, oldToken string) (bool, error) {
	langs := session.Languages
	if langs == "" {
		langs = "en-US,en"
	}
	deviceID := codexreg.DeviceIDFromCookies(session.Cookies)
	probeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	oldProbe, oldErr := codexreg.ProbeAccessTokenWithIdentity(probeCtx, codexreg.ProbeIdentity{
		Token: oldToken, Proxy: session.Proxy, Languages: langs, DeviceID: deviceID, UserAgent: session.UserAgent,
	})
	cancel()
	if oldErr != nil {
		fmt.Printf("old_at network=%v\n", oldErr)
	} else {
		fmt.Printf("old_at valid=%t status=%d code=%s\n", oldProbe.Valid, oldProbe.HTTPStatus, oldProbe.ErrorCode)
	}
	refreshCtx, refreshCancel := context.WithTimeout(ctx, 45*time.Second)
	refresh, refreshErr := codexreg.RefreshAccessTokenFromWebSession(refreshCtx, session, session.Proxy)
	refreshCancel()
	if refreshErr != nil {
		fmt.Printf("session_refresh network=%v\n", refreshErr)
		return oldProbe.Valid, nil
	}
	fmt.Printf("session_refresh status=%d code=%s new_at=%t\n", refresh.HTTPStatus, refresh.ErrorCode, strings.TrimSpace(refresh.AccessToken) != "")
	if strings.TrimSpace(refresh.AccessToken) == "" {
		return oldProbe.Valid, nil
	}
	same := codexreg.AccessTokenFingerprint(refresh.AccessToken) == codexreg.AccessTokenFingerprint(oldToken)
	fmt.Printf("session_at same_as_old=%t\n", same)
	newCtx, newCancel := context.WithTimeout(ctx, 45*time.Second)
	newProbe, newErr := codexreg.ProbeAccessTokenWithIdentity(newCtx, codexreg.ProbeIdentity{
		Token: refresh.AccessToken, Proxy: session.Proxy, Languages: langs, DeviceID: deviceID, UserAgent: session.UserAgent,
	})
	newCancel()
	if newErr != nil {
		fmt.Printf("new_at network=%v\n", newErr)
		return oldProbe.Valid, nil
	}
	fmt.Printf("new_at valid=%t status=%d code=%s\n", newProbe.Valid, newProbe.HTTPStatus, newProbe.ErrorCode)
	return oldProbe.Valid || newProbe.Valid, nil
}

func checkSavedSession(ctx context.Context, db *gorm.DB, proxy, domain string) error {
	var rows []models.Registration
	if err := db.Where("auth_data <> ''").Order("id desc").Limit(30).Find(&rows).Error; err != nil {
		return err
	}
	want := strings.ToLower(strings.TrimSpace(domain))
	for _, row := range rows {
		_, mbDomain, _ := strings.Cut(strings.ToLower(strings.TrimSpace(row.Email)), "@")
		if want != "" && mbDomain != want {
			continue
		}
		session, err := codexreg.WebSessionFromAuthData(row.AuthData)
		if err != nil || len(session.Cookies) == 0 {
			continue
		}
		if session.Proxy == "" {
			session.Proxy = proxy
		}
		fmt.Printf("check mailbox=%s db_at=%s db_cred=%s\n", mask(row.Email), row.AccessTokenStatus, row.AccountCredentialStatus)
		_, err = reportCredential(ctx, session, session.AccessToken)
		return err
	}
	return errors.New("no saved web session")
}

func saveRegistration(db *gorm.DB, mb models.Mailbox, password string, res *codexreg.Result) error {
	authBytes, _ := json.MarshalIndent(res.AuthJSON, "", "  ")
	verifiedAt := time.Now().UTC()
	row := models.Registration{
		Email: emailOf(mb), MailboxID: mb.ID, Password: password,
		Status: "registered", IsMother: true, Note: "gptlive hotmail",
		AuthData: string(authBytes), AccountID: res.AccountID,
		UserID: res.UserID, PlanType: res.PlanType,
		AccountCredentialStatus: "valid", AccessTokenStatus: "valid",
		TokenLastVerifiedAt: &verifiedAt, RefreshLastSuccessAt: &verifiedAt,
		NeedsRelogin: false, Alive: "alive", AliveCheckedAt: &verifiedAt,
		AccessTokenFingerprint: codexreg.AccessTokenFingerprint(res.AccessToken),
	}
	if meta, err := codexreg.ParseAccessTokenMetadata(res.AccessToken); err == nil {
		row.TokenIssuedAt = &meta.IssuedAt
		row.TokenExpiresAt = &meta.ExpiresAt
		row.AccessTokenJTI = meta.JTI
	}
	return db.Where("email = ?", row.Email).Assign(row).FirstOrCreate(&row).Error
}

func emailOf(mb models.Mailbox) string { return mb.Email }

func pickMailbox(db *gorm.DB, domain string) (models.Mailbox, error) {
	var boxes []models.Mailbox
	if err := db.Where("status = ?", "verified").Order("id asc").Find(&boxes).Error; err != nil {
		return models.Mailbox{}, err
	}
	want := strings.ToLower(strings.TrimSpace(domain))
	for _, mb := range boxes {
		_, mbDomain, ok := strings.Cut(strings.ToLower(strings.TrimSpace(mb.Email)), "@")
		if want != "" {
			if !ok || mbDomain != want {
				continue
			}
		} else if mbDomain == "hotmail.com" {
			continue
		}
		var n int64
		db.Model(&models.Registration{}).Where("email = ? AND status IN ?", mb.Email,
			[]string{"registered", "already_registered", "register_rejected", "registering", "register_failed"}).Count(&n)
		if n == 0 {
			return mb, nil
		}
	}
	return models.Mailbox{}, errors.New("no unused verified mailbox")
}

func fetchCode(ctx context.Context, mail *mailfetch.Client, mb models.Mailbox, since time.Time) (string, error) {
	acc := mailfetch.Account{Email: mb.Email, ClientID: mb.ClientID, RefreshToken: mb.RefreshToken}
	deadline := time.Now().Add(3 * time.Minute)
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		msgs, err := mail.ListMessages(ctx, acc, 15)
		if err != nil {
			lastErr = err
		} else {
			var bestCode string
			var bestAt time.Time
			for _, m := range msgs {
				if m.ReceivedAt.Before(since) || !looksLikeOpenAI(m) {
					continue
				}
				code := ""
				if c := codeRe.FindStringSubmatch(m.Subject); c != nil {
					code = c[1]
				} else if full, gerr := mail.GetMessage(ctx, acc, m.ID); gerr == nil {
					if c := codeRe.FindStringSubmatch(full.Subject + " " + full.Text); c != nil {
						code = c[1]
					}
				}
				if code != "" && (bestCode == "" || m.ReceivedAt.After(bestAt)) {
					bestCode, bestAt = code, m.ReceivedAt
				}
			}
			if bestCode != "" {
				return bestCode, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("timeout waiting code: %v", lastErr)
	}
	return "", fmt.Errorf("timeout waiting code")
}

func looksLikeOpenAI(m mailfetch.Message) bool {
	s := strings.ToLower(m.From + " " + m.FromName + " " + m.Subject)
	return strings.Contains(s, "openai") || strings.Contains(s, "chatgpt")
}

func firstProxy(db *gorm.DB) string {
	var enabled models.Setting
	if err := db.Where("key = ?", "proxy_enabled").First(&enabled).Error; err == nil && enabled.Value == "0" {
		return ""
	}
	var list models.Setting
	if err := db.Where("key = ?", "proxy_list").First(&list).Error; err != nil {
		return ""
	}
	proxies := proxyutil.List(list.Value)
	if len(proxies) == 0 {
		return ""
	}
	return proxies[0]
}

func proxyHost(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "socks5://")
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	return s
}

func mask(email string) string {
	at := strings.Index(email, "@")
	if at <= 2 {
		return "***"
	}
	return email[:2] + "***" + email[at:]
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(1)
}
