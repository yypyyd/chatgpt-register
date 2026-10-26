// Package lovartreg 用 HTTP 协议完成 lovart.ai 的邮箱验证码登录/注册，并采集
// 站点登录 Cookie 供导出。Lovart 没有密码注册：首次邮箱验证码登录即创建账号。
//
//	GET  /                                           预热点、拿 acw_tc
//	2Captcha 解 Turnstile                            sitekey 来自页面运行配置
//	POST /api/www/lovart/login/v2/sendLoginEmailWithCode
//	等邮箱 6 位验证码
//	POST /api/www/lovart/login/v2/loginByEmailWithCode
//	（可选）风险挑战：/api/www/lovart/auth/challenge/send → verify/login
//	GET  /actions/login?usertoken=…&refreshToken=…   写入站点 Cookie
//	POST /api/www/user/getUserInfo                   真实探针确认会话
package lovartreg

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"

	"chatgpt-register/internal/captcha"
	"chatgpt-register/internal/proxyutil"
)

const (
	origin              = "https://www.lovart.ai"
	turnstileSitekey    = "0x4AAAAAABg9KkPJzGO0csnB"
	protocolChromeMajor = "146"
	protocolTimeout     = 60 * time.Second

	pathSendCode        = "/api/www/lovart/login/v2/sendLoginEmailWithCode"
	pathLoginByCode     = "/api/www/lovart/login/v2/loginByEmailWithCode"
	pathChallengeSend   = "/api/www/lovart/auth/challenge/send"
	pathChallengeVerify = "/api/www/lovart/auth/challenge/verify/login"
	pathUserInfo        = "/api/www/user/getUserInfo"
	pathTokenRefresh    = "/api/www/lovart/login/v2/token/refresh"
	pathActionsLogin    = "/actions/login"
)

// Input 是一次 Lovart 协议注册的输入。WaitCode 由 producer 注入：有邮箱凭据时
// 自动读邮件，否则等页面手工提交。since 表示只接受该时间之后收到的验证码。
type Input struct {
	Email      string
	Proxy      string
	CaptchaKey string

	WaitCode func(ctx context.Context, since time.Time) (string, error)
	Log      func(format string, a ...any)
}

type Result struct {
	AuthJSON map[string]any `json:"auth_json"`
}

func (in Input) logf(format string, a ...any) {
	if in.Log != nil {
		in.Log(format, a...)
	}
}

func (in Input) waitCode(ctx context.Context, since time.Time) (string, error) {
	if in.WaitCode == nil {
		return "", fmt.Errorf("缺少验证码回调")
	}
	return in.WaitCode(ctx, since)
}

// Register 完成 Lovart 邮箱验证码登录并返回可导出的登录凭据。
func Register(ctx context.Context, in Input) (*Result, error) {
	if strings.TrimSpace(in.Email) == "" {
		return nil, fmt.Errorf("缺少邮箱")
	}
	if strings.TrimSpace(in.CaptchaKey) == "" {
		return nil, fmt.Errorf("未配置 2Captcha key（设置 captcha_2captcha_key）")
	}

	c, err := newClient(in)
	if err != nil {
		return nil, err
	}

	in.logf("Lovart 协议注册启动")
	if strings.TrimSpace(in.Proxy) != "" {
		server, _, _, perr := proxyutil.Parse(in.Proxy)
		if perr == nil {
			in.logf("Lovart 协议客户端使用上游代理: %s", server)
		}
	}

	if err := c.warmup(ctx); err != nil {
		in.logf("Lovart 首页预热失败（继续）: %v", err)
	}

	tok, err := c.sendCode(ctx, in.Email)
	if err != nil {
		return nil, err
	}
	in.logf("Lovart 验证码已发送，等待邮箱收码")

	code, err := in.waitCode(ctx, time.Now().Add(-30*time.Second))
	if err != nil {
		return nil, err
	}
	in.logf("已获取 Lovart 邮箱验证码")

	data, err := c.loginByCode(ctx, in.Email, code, tok)
	if err != nil && needsFreshToken(err.Error()) {
		in.logf("Lovart 验证码登录提示 token 失效，重新打码重试: %v", err)
		if fresh, merr := c.mintTurnstile(ctx); merr == nil {
			data, err = c.loginByCode(ctx, in.Email, code, fresh)
		}
	}
	if err != nil {
		return nil, err
	}

	if data.RequireChallenge || strings.TrimSpace(data.ChallengeToken) != "" {
		in.logf("Lovart 要求二次风险验证，走邮箱挑战流程")
		if err := c.sendChallenge(ctx, in.Email, data); err != nil {
			return nil, err
		}
		challengeCode, err := in.waitCode(ctx, time.Now().Add(-30*time.Second))
		if err != nil {
			return nil, err
		}
		data, err = c.verifyChallenge(ctx, data.ChallengeToken, challengeCode)
		if err != nil {
			return nil, err
		}
	}

	if strings.TrimSpace(data.AccessToken) == "" || strings.TrimSpace(data.RefreshToken) == "" {
		return nil, fmt.Errorf("Lovart 登录未返回完整 token")
	}

	// 和浏览器一致：/actions/login 把两个 token 写成站点 Cookie。
	if err := c.establishSession(ctx, data); err != nil {
		in.logf("Lovart /actions/login 写入 Cookie 失败（继续用手工 Cookie）: %v", err)
	}

	user, err := c.userInfo(ctx, data.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("Lovart 会话探针失败: %w", err)
	}

	cookies := c.exportCookies(data)
	auth := map[string]any{
		"platform":                "lovart",
		"email":                   in.Email,
		"webid":                   c.webid,
		"access_token":            data.AccessToken,
		"refresh_token":           data.RefreshToken,
		"access_token_expire_at":  data.AccessTokenExpireAt,
		"refresh_token_expire_at": data.RefreshTokenExpireAt,
		"cookies":                 cookies,
		"captured_at":             time.Now().UTC().Format(time.RFC3339),
		"is_new_user":             data.IsNewUser,
	}
	if user != nil {
		auth["user"] = user
	}
	if in.Proxy != "" {
		auth["proxy"] = in.Proxy
	}
	return &Result{AuthJSON: auth}, nil
}

type protocolClient struct {
	cli   tls_client.HttpClient
	in    Input
	ua    string
	webid string
}

func newClient(in Input) (*protocolClient, error) {
	prof := profiles.Chrome_146
	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(60),
		tls_client.WithClientProfile(prof),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
		tls_client.WithNotFollowRedirects(),
	}
	if p := strings.TrimSpace(in.Proxy); p != "" {
		opts = append(opts, tls_client.WithProxyUrl(proxyutil.Normalize(p)))
	}
	cli, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return nil, fmt.Errorf("创建 Lovart TLS 客户端失败: %w", err)
	}
	c := &protocolClient{
		cli:   cli,
		in:    in,
		ua:    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + protocolChromeMajor + ".0.0.0 Safari/537.36",
		webid: newWebID(),
	}
	u, _ := url.Parse(origin)
	cli.SetCookies(u, []*http.Cookie{{
		Name:     "webid",
		Value:    c.webid,
		Path:     "/",
		Domain:   "www.lovart.ai",
		Expires:  time.Now().AddDate(10, 0, 0),
		Secure:   true,
		SameSite: http.SameSiteNoneMode,
	}})
	return c, nil
}

func newWebID() string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 8)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
		if err != nil {
			b[i] = letters[i%len(letters)]
			continue
		}
		b[i] = letters[n.Int64()]
	}
	return fmt.Sprintf("%d%s", time.Now().UnixMilli(), string(b))
}

func (c *protocolClient) baseHeaders(req *http.Request) {
	h := req.Header
	setIfEmpty := func(key, value string) {
		if h.Get(key) == "" {
			h.Set(key, value)
		}
	}
	setIfEmpty("user-agent", c.ua)
	setIfEmpty("accept-language", "en-US,en;q=0.9")
	setIfEmpty("sec-ch-ua", `"Chromium";v="146", "Not-A.Brand";v="24", "Google Chrome";v="146"`)
	setIfEmpty("sec-ch-ua-mobile", "?0")
	setIfEmpty("sec-ch-ua-platform", `"Windows"`)
	setIfEmpty("origin", origin)
	setIfEmpty("referer", origin+"/")
	setIfEmpty("webid", c.webid)
	setIfEmpty("x-language", "en")
	setIfEmpty("sec-fetch-site", "same-origin")
	setIfEmpty("sec-fetch-mode", "cors")
	setIfEmpty("sec-fetch-dest", "empty")
}

func (c *protocolClient) do(ctx context.Context, req *http.Request) (*http.Response, []byte, error) {
	req = req.WithContext(ctx)
	c.baseHeaders(req)
	resp, err := c.cli.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, nil, err
	}
	return resp, body, nil
}

func (c *protocolClient) warmup(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/", nil)
	if err != nil {
		return err
	}
	req.Header.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("upgrade-insecure-requests", "1")
	req.Header.Set("sec-fetch-site", "none")
	req.Header.Set("sec-fetch-mode", "navigate")
	req.Header.Set("sec-fetch-dest", "document")
	req.Header.Set("sec-fetch-user", "?1")
	resp, body, err := c.do(ctx, req)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, snippet(body))
	}
	return nil
}

type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func (e *envelope) errorText() string {
	if strings.TrimSpace(e.Msg) != "" {
		return e.Msg
	}
	return fmt.Sprintf("code=%d", e.Code)
}

func (c *protocolClient) postJSON(ctx context.Context, path string, payload any, token string) (*envelope, []byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+path, bytes.NewReader(raw))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("accept", "application/json, text/plain, */*")
	req.Header.Set("content-type", "application/json")
	if token != "" {
		req.Header.Set("token", token)
	}
	resp, body, err := c.do(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, body, fmt.Errorf("Lovart 接口返回非 JSON（HTTP %d）: %s", resp.StatusCode, snippet(body))
	}
	return &env, body, nil
}

func (c *protocolClient) mintTurnstile(ctx context.Context) (string, error) {
	solver := &captcha.TwoCaptcha{Key: c.in.CaptchaKey, Log: c.in.logf}
	c.in.logf("提交 2Captcha 解 Lovart Turnstile")
	return solver.SolveTurnstile(ctx, turnstileSitekey, origin+"/", "", "")
}

func needsFreshToken(msg string) bool {
	s := strings.ToLower(msg)
	return strings.Contains(s, "token is necessary") ||
		strings.Contains(s, "token is invalid") ||
		strings.Contains(s, "token invalid") ||
		strings.Contains(s, "captcha") ||
		strings.Contains(s, "turnstile") ||
		strings.Contains(s, "verify token")
}

func (c *protocolClient) sendCode(ctx context.Context, email string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := c.mintTurnstile(ctx)
		if err != nil {
			return "", fmt.Errorf("Lovart Turnstile 打码失败: %w", err)
		}
		env, _, err := c.postJSON(ctx, pathSendCode, map[string]any{
			"email":       email,
			"cid":         c.webid,
			"redirectUrl": origin + "/",
			"token":       tok,
		}, "")
		if err != nil {
			return "", err
		}
		if env.Code == 0 {
			return tok, nil
		}
		lastErr = fmt.Errorf("Lovart 发送验证码失败: %s", env.errorText())
		if attempt == 0 && needsFreshToken(env.Msg) {
			c.in.logf("Lovart 验证码请求提示 token 无效，重新打码重试: %s", env.Msg)
			continue
		}
		return "", lastErr
	}
	return "", lastErr
}

type signedUser struct {
	UserID   int64  `json:"userId"`
	UUID     string `json:"uuid"`
	Email    string `json:"email"`
	Nickname string `json:"nickname"`
	Avatar   string `json:"avatar"`
	CreateAt int64  `json:"createAt"`
	LoginAt  int64  `json:"loginAt"`
}

type loginData struct {
	AccessToken          string     `json:"accessToken"`
	RefreshToken         string     `json:"refreshToken"`
	AccessTokenExpireAt  int64      `json:"accessTokenExpireAt"`
	RefreshTokenExpireAt int64      `json:"refreshTokenExpireAt"`
	ChallengeToken       string     `json:"challengeToken"`
	MaskedEmail          string     `json:"maskedEmail"`
	MaskedPhone          string     `json:"maskedPhone"`
	VerifyMethod         string     `json:"verifyMethod"`
	SMSOnly              bool       `json:"smsOnly"`
	EmailOnly            bool       `json:"emailOnly"`
	RequireChallenge     bool       `json:"requireChallenge"`
	IsNewUser            bool       `json:"isNewUser"`
	SignedUser           signedUser `json:"signedUser"`
}

func decodeLoginData(env *envelope) (*loginData, error) {
	if env.Code != 0 {
		return nil, fmt.Errorf("Lovart 登录失败: %s", env.errorText())
	}
	var data loginData
	if len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, &data); err != nil {
			return nil, fmt.Errorf("解析 Lovart 登录响应失败: %w", err)
		}
	}
	return &data, nil
}

func (c *protocolClient) loginByCode(ctx context.Context, email, code, token string) (*loginData, error) {
	env, _, err := c.postJSON(ctx, pathLoginByCode, map[string]any{
		"email":       email,
		"verifyCode":  code,
		"cid":         c.webid,
		"invitedCode": "",
		"token":       token,
	}, "")
	if err != nil {
		return nil, err
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("Lovart 验证码登录失败: %s", env.errorText())
	}
	data, err := decodeLoginData(env)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(data.VerifyMethod, "SMS") && data.ChallengeToken != "" {
		return nil, fmt.Errorf("Lovart 要求短信二次验证，邮箱协议无法完成")
	}
	return data, nil
}

func (c *protocolClient) sendChallenge(ctx context.Context, email string, data *loginData) error {
	method := strings.TrimSpace(data.VerifyMethod)
	if method == "" {
		method = "EMAIL"
	}
	if !strings.EqualFold(method, "EMAIL") {
		return fmt.Errorf("Lovart 二次验证方式为 %s，邮箱协议无法完成", method)
	}
	env, _, err := c.postJSON(ctx, pathChallengeSend, map[string]any{
		"method":         method,
		"challengeToken": data.ChallengeToken,
		"contact":        email,
	}, "")
	if err != nil {
		return err
	}
	if env.Code != 0 {
		return fmt.Errorf("Lovart 发送二次验证码失败: %s", env.errorText())
	}
	return nil
}

func (c *protocolClient) verifyChallenge(ctx context.Context, challengeToken, code string) (*loginData, error) {
	env, _, err := c.postJSON(ctx, pathChallengeVerify, map[string]any{
		"challengeToken": challengeToken,
		"code":           code,
	}, "")
	if err != nil {
		return nil, err
	}
	return decodeLoginData(env)
}

func (c *protocolClient) establishSession(ctx context.Context, data *loginData) error {
	q := url.Values{}
	q.Set("usertoken", data.AccessToken)
	q.Set("refreshToken", data.RefreshToken)
	q.Set("redirectUrl", base64.StdEncoding.EncodeToString([]byte(origin+"/")))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+pathActionsLogin+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("upgrade-insecure-requests", "1")
	req.Header.Set("sec-fetch-site", "none")
	req.Header.Set("sec-fetch-mode", "navigate")
	req.Header.Set("sec-fetch-dest", "document")
	resp, body, err := c.do(ctx, req)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, snippet(body))
	}
	return nil
}

func (c *protocolClient) userInfo(ctx context.Context, accessToken string) (map[string]any, error) {
	env, _, err := c.postJSON(ctx, pathUserInfo, map[string]any{}, accessToken)
	if err != nil {
		return nil, err
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("%s", env.errorText())
	}
	var data map[string]any
	if len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, &data); err != nil {
			return nil, err
		}
	}
	if data == nil {
		return nil, fmt.Errorf("会话探针未返回用户数据")
	}
	if id, ok := data["id"]; !ok || id == nil {
		return nil, fmt.Errorf("会话探针未返回用户 ID")
	}
	return data, nil
}

func (c *protocolClient) exportCookies(data *loginData) []map[string]any {
	now := time.Now()
	out := []map[string]any{cookieObject("usertoken", data.AccessToken, data.AccessTokenExpireAt, false)}
	out = append(out, cookieObject("refreshToken", data.RefreshToken, data.RefreshTokenExpireAt, false))
	webidExp := now.AddDate(10, 0, 0).UnixMilli()
	out = append(out, cookieObject("webid", c.webid, webidExp, false))

	// 其它站点 Cookie（如 acw_tc）没有可导出的登录价值，但保留在会话里便于排查。
	seen := map[string]bool{"usertoken": true, "refreshToken": true, "webid": true}
	u, _ := url.Parse(origin)
	for _, ck := range c.cli.GetCookies(u) {
		if ck == nil || seen[ck.Name] || strings.TrimSpace(ck.Name) == "" {
			continue
		}
		seen[ck.Name] = true
		exp := int64(0)
		if !ck.Expires.IsZero() {
			exp = ck.Expires.UnixMilli()
		}
		out = append(out, cookieObject(ck.Name, ck.Value, exp, ck.HttpOnly))
	}
	return out
}

func cookieObject(name, value string, expiresUnixMilli int64, httpOnly bool) map[string]any {
	return map[string]any{
		"name":      name,
		"value":     value,
		"domain":    "www.lovart.ai",
		"path":      "/",
		"expires":   expiresUnixMilli,
		"http_only": httpOnly,
		"secure":    true,
		"same_site": "None",
	}
}

// RefreshAccessToken 用 refreshToken 换新的 accessToken，供测活/下游刷新使用。
func RefreshAccessToken(ctx context.Context, refreshToken, proxy string) (accessToken string, accessExpireAt int64, err error) {
	raw, err := json.Marshal(map[string]any{"refreshToken": refreshToken})
	if err != nil {
		return "", 0, err
	}
	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(45),
		tls_client.WithClientProfile(profiles.Chrome_133),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
		tls_client.WithNotFollowRedirects(),
	}
	if p := strings.TrimSpace(proxy); p != "" {
		opts = append(opts, tls_client.WithProxyUrl(proxyutil.Normalize(p)))
	}
	cli, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+pathTokenRefresh, bytes.NewReader(raw))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json, text/plain, */*")
	req.Header.Set("origin", origin)
	req.Header.Set("referer", origin+"/")
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36")
	resp, err := cli.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return "", 0, fmt.Errorf("刷新接口返回非 JSON（HTTP %d）", resp.StatusCode)
	}
	if env.Code != 0 {
		return "", 0, fmt.Errorf("%s", env.errorText())
	}
	var data struct {
		AccessToken         string `json:"accessToken"`
		AccessTokenExpireAt int64  `json:"accessTokenExpireAt"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return "", 0, err
	}
	if strings.TrimSpace(data.AccessToken) == "" {
		return "", 0, fmt.Errorf("刷新接口未返回 accessToken")
	}
	return data.AccessToken, data.AccessTokenExpireAt, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}
