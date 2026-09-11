package leonardoreg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/go-rod/rod"

	"chatgpt-register/internal/proxyutil"
)

// 协议注册（不填表单）。页面过 Vercel BotID 后 fetch，不再先做 tls-client 预热。
//
//	GET  /auth/login                     页面过边缘、刮 Turnstile sitekey
//	GET  /api/auth/cross-origin-cookie   better-auth 预热
//	页面或 2Captcha 解 Turnstile（action=signin）
//	POST /api/auth/signup               Cognito 包装（body.verificationToken）
//	等邮箱 6 位码
//	POST /api/auth/confirm-signup
//	POST /api/auth/sign-in/email        x-captcha-response
//	GET  /api/auth/get-session
//	导出 jar（含 better-auth 会话 cookie）

const (
	origin              = "https://app.leonardo.ai"
	protocolTLSProfile  = "chrome_146"
	protocolChromeMajor = "146"
	protocolStepTimeout = 45 * time.Second
	graphqlURL          = "https://api.leonardo.ai/v1/graphql"
	pathCrossOrigin     = "/api/auth/cross-origin-cookie"
	pathGetSession      = "/api/auth/get-session"
	pathSignUpEmail     = "/api/auth/sign-up/email"
	pathSignInEmail     = "/api/auth/sign-in/email"
	pathSignInOTP       = "/api/auth/sign-in/email-otp"
	pathSendOTP         = "/api/auth/email-otp/send-verification-otp"
	pathVerifyEmail     = "/api/auth/email-otp/verify-email"
	pathSignupLegacy    = "/api/auth/signup"
	pathConfirmLegacy   = "/api/auth/confirm-signup"
	pathResendLegacy    = "/api/auth/resend-confirmation-code"
)

var (
	sitekeyHintRe = regexp.MustCompile(`(?i)(?:site[_-]?key["'\s:=]+)(0x4[A-Za-z0-9_-]{10,})`)
	sitekeyAnyRe  = regexp.MustCompile(`0x4[A-Za-z0-9_-]{10,}`)
)

type leoClient struct {
	in      Input
	ua      string
	cli     tls_client.HttpClient
	page    *rod.Page
	browser *rod.Browser
	cleanup []func()
	kind    string
	html    string
}

type protoResp struct {
	Status int
	Body   []byte
	Header http.Header
}

func newTLSClient(in Input) (*leoClient, error) {
	jar := tls_client.NewCookieJar()
	prof, ok := profiles.MappedTLSClients[protocolTLSProfile]
	if !ok {
		prof = profiles.Chrome_133
	}
	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(int(protocolStepTimeout.Seconds())),
		tls_client.WithClientProfile(prof),
		tls_client.WithRandomTLSExtensionOrder(),
		tls_client.WithCookieJar(jar),
	}
	if p := strings.TrimSpace(in.Proxy); p != "" {
		opts = append(opts, tls_client.WithProxyUrl(proxyutil.Normalize(p)))
	}
	cli, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return nil, fmt.Errorf("创建 TLS 客户端失败: %w", err)
	}
	cli.SetFollowRedirect(true)
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + protocolChromeMajor + ".0.0.0 Safari/537.36"
	return &leoClient{in: in, ua: ua, cli: cli, kind: "tls"}, nil
}

func (c *leoClient) adoptSessionToken(token string) {
	token = strings.TrimSpace(token)
	if c == nil || c.cli == nil || token == "" {
		return
	}
	u, err := url.Parse(origin + "/")
	if err != nil {
		return
	}
	c.cli.SetCookies(u, []*http.Cookie{{
		Name:     "__Secure-better-auth.session_token",
		Value:    token,
		Path:     "/",
		Domain:   "app.leonardo.ai",
		Secure:   true,
		HttpOnly: true,
	}})
}

func (c *leoClient) close() {
	if c == nil {
		return
	}
	if c.page != nil {
		_ = rod.Try(func() { _ = c.page.Close() })
		c.page = nil
	}
	if c.browser != nil {
		_ = rod.Try(c.browser.MustClose)
		c.browser = nil
	}
	for i := len(c.cleanup) - 1; i >= 0; i-- {
		if c.cleanup[i] != nil {
			c.cleanup[i]()
		}
	}
	c.cleanup = nil
}

func (c *leoClient) headers(req *http.Request, rawURL string, hasBody bool) {
	h := req.Header
	h.Set("user-agent", c.ua)
	h.Set("accept-language", "en-US,en;q=0.9")
	h.Set("sec-ch-ua", fmt.Sprintf(`"Google Chrome";v="%s", "Chromium";v="%s", "Not_A Brand";v="24"`, protocolChromeMajor, protocolChromeMajor))
	h.Set("sec-ch-ua-mobile", "?0")
	h.Set("sec-ch-ua-platform", `"Windows"`)
	h.Set("origin", origin)
	h.Set("referer", loginURL)
	api := strings.Contains(rawURL, "/api/")
	if api {
		h.Set("accept", "application/json, text/plain, */*")
		h.Set("sec-fetch-site", "same-origin")
		h.Set("sec-fetch-mode", "cors")
		h.Set("sec-fetch-dest", "empty")
		if strings.Contains(rawURL, "api.leonardo.ai") {
			h.Set("sec-fetch-site", "cross-site")
		}
	} else {
		h.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
		h.Set("upgrade-insecure-requests", "1")
		h.Set("sec-fetch-site", "none")
		h.Set("sec-fetch-mode", "navigate")
		h.Set("sec-fetch-dest", "document")
	}
	if hasBody {
		h.Set("content-type", "application/json")
	}
	h[http.HeaderOrderKey] = []string{
		"accept", "accept-language", "content-type", "origin", "referer",
		"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform",
		"sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site",
		"upgrade-insecure-requests", "user-agent", "x-captcha-response",
	}
}

func (c *leoClient) do(ctx context.Context, method, rawURL string, body []byte, extra map[string]string) (*protoResp, error) {
	if c.page != nil {
		return c.doPage(ctx, method, rawURL, body, extra)
	}
	return c.doTLS(ctx, method, rawURL, body, extra)
}

func (c *leoClient) doTLS(ctx context.Context, method, rawURL string, body []byte, extra map[string]string) (*protoResp, error) {
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, err
	}
	c.headers(req, rawURL, len(body) > 0)
	for k, v := range extra {
		if strings.TrimSpace(k) == "" || v == "" {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := c.cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	out := &protoResp{Status: resp.StatusCode, Body: raw, Header: resp.Header}
	if !strings.Contains(rawURL, "/api/") {
		c.html = string(raw)
	}
	return out, nil
}

func (c *leoClient) get(ctx context.Context, rawURL string) (*protoResp, error) {
	return c.do(ctx, http.MethodGet, rawURL, nil, nil)
}

func (c *leoClient) postJSON(ctx context.Context, rawURL string, payload any, captcha string) (*protoResp, error) {
	return c.postJSONMode(ctx, rawURL, payload, captcha, true)
}

func (c *leoClient) postJSONMode(ctx context.Context, rawURL string, payload any, captcha string, captchaHeaders bool) (*protoResp, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	extra := map[string]string{}
	if captchaHeaders {
		if tok := strings.TrimSpace(captcha); tok != "" {
			extra["x-captcha-response"] = tok
			extra["cf-turnstile-response"] = tok
		}
	}
	return c.do(ctx, http.MethodPost, rawURL, raw, extra)
}

func (c *leoClient) warm(ctx context.Context) (*protoResp, error) {
	// www 不挂 BotID，先养一套站点 cookie 再进 app。
	for _, u := range []string{"https://www.leonardo.ai/", "https://leonardo.ai/"} {
		if _, err := c.get(ctx, u); err != nil {
			c.in.logf("预热 %s 失败（继续）: %v", u, err)
		}
	}
	var last *protoResp
	var lastErr error
	for i := 0; i < 6; i++ {
		r, err := c.get(ctx, loginURL)
		if err != nil {
			lastErr = err
			c.in.logf("打开登录页失败（第 %d 次）: %v", i+1, err)
		} else {
			last, lastErr = r, nil
			if !r.vercel() {
				if _, werr := c.get(ctx, origin+pathCrossOrigin); werr != nil {
					c.in.logf("cross-origin-cookie 预热失败（继续）: %v", werr)
				}
				return r, nil
			}
			c.in.logf("登录页仍是 Vercel BotID（HTTP %d，第 %d 次）", r.Status, i+1)
		}
		if i < 5 && !sleepCtxLeo(ctx, time.Duration(800+i*400)*time.Millisecond) {
			return last, ctx.Err()
		}
	}
	if lastErr != nil {
		return last, fmt.Errorf("打开登录页失败: %w", lastErr)
	}
	if _, werr := c.get(ctx, origin+pathCrossOrigin); werr != nil {
		c.in.logf("cross-origin-cookie 预热失败（继续）: %v", werr)
	}
	return last, nil
}

func (c *leoClient) scrapeSitekey() string {
	html := c.html
	if html == "" && c.page != nil {
		if v, err := c.page.Timeout(8 * time.Second).Eval(`() => document.documentElement ? document.documentElement.innerHTML : ''`); err == nil {
			html = v.Value.Str()
			c.html = html
		}
	}
	if m := sitekeyHintRe.FindStringSubmatch(html); len(m) > 1 {
		return m[1]
	}
	if m := sitekeyAnyRe.FindString(html); m != "" {
		return m
	}
	return ""
}

func (c *leoClient) cookies() []map[string]any {
	if c.page != nil {
		return pageCookies(c.page)
	}
	now := time.Now()
	out := make([]map[string]any, 0, 16)
	seen := map[string]bool{}
	for _, site := range []string{origin + "/", "https://leonardo.ai/"} {
		u, err := url.Parse(site)
		if err != nil || c.cli == nil {
			continue
		}
		for _, ck := range c.cli.GetCookies(u) {
			if ck == nil || ck.Name == "" {
				continue
			}
			key := strings.ToLower(ck.Domain) + "|" + ck.Path + "|" + ck.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			domain := ck.Domain
			if domain == "" {
				domain = u.Host
			}
			path := ck.Path
			if path == "" {
				path = "/"
			}
			item := map[string]any{
				"name":     ck.Name,
				"value":    ck.Value,
				"domain":   domain,
				"path":     path,
				"httpOnly": ck.HttpOnly,
				"secure":   ck.Secure,
			}
			if !ck.Expires.IsZero() {
				item["expires"] = float64(ck.Expires.Unix())
			} else if ck.MaxAge > 0 {
				item["expires"] = float64(now.Add(time.Duration(ck.MaxAge) * time.Second).Unix())
			}
			out = append(out, item)
		}
	}
	return out
}

func isVercelChallenge(status int, hdr http.Header, body []byte) bool {
	if hdr != nil && strings.EqualFold(hdr.Get("X-Vercel-Mitigated"), "challenge") {
		return true
	}
	s := strings.ToLower(string(body))
	if strings.Contains(s, "vercel security checkpoint") ||
		strings.Contains(s, "x-vercel-mitigated") ||
		strings.Contains(s, "vercel-challenge") ||
		strings.Contains(s, "we're verifying your browser") {
		return true
	}
	if status == 429 && (len(body) == 0 || strings.Contains(s, "<html") || strings.Contains(s, "checkpoint")) {
		return true
	}
	return false
}

func (r *protoResp) vercel() bool {
	if r == nil {
		return false
	}
	return isVercelChallenge(r.Status, r.Header, r.Body)
}

type authErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Error   string `json:"error"`
}

func parseAuthError(body []byte) (code, msg string) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return "", ""
	}
	var e authErr
	if json.Unmarshal(body, &e) == nil {
		code = strings.TrimSpace(e.Code)
		msg = firstNonEmpty(e.Message, e.Error)
		if code != "" || msg != "" {
			return code, msg
		}
	}
	return "", trimText(string(body), 300)
}

func looksLikeAPIError(body []byte) bool {
	code, msg := parseAuthError(body)
	if code == "" && msg == "" {
		return false
	}
	s := strings.ToLower(code + " " + msg + " " + string(body))
	return strings.Contains(s, `"error"`) ||
		strings.Contains(s, `"code"`) && (strings.Contains(s, "fail") || strings.Contains(s, "invalid") || strings.Contains(s, "exist") || strings.Contains(s, "captcha"))
}

func isEmailTaken(code, msg, raw string) bool {
	s := strings.ToLower(code + " " + msg + " " + raw)
	for _, k := range []string{
		"user_already_exists",
		"usernameexistsexception",
		"already registered",
		"already exists",
		"email already",
		"user already",
		"account with the given email",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func isUnconfirmed(code, msg, raw string) bool {
	s := strings.ToLower(code + " " + msg + " " + raw)
	for _, k := range []string{
		"usernotconfirmed",
		"user_not_confirmed",
		"not confirmed",
		"unconfirmed",
		"email_not_verified",
		"email not verified",
		"verify your email",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func needsCaptcha(code, msg, raw string) bool {
	s := strings.ToLower(code + " " + msg + " " + raw)
	return strings.Contains(s, "captcha") ||
		strings.Contains(s, "turnstile") ||
		strings.Contains(s, "verification token") ||
		strings.Contains(s, "userlambdavalidation")
}

func sessionHasUser(body []byte) bool {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || bytes.Equal(body, []byte("null")) {
		return false
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return false
	}
	if _, ok := payload["user"]; ok {
		return true
	}
	sess, _ := payload["session"].(map[string]any)
	if sess == nil {
		return false
	}
	if _, ok := sess["accessToken"]; ok {
		return true
	}
	if _, ok := sess["userId"]; ok {
		return true
	}
	if _, ok := sess["hasuraUserId"]; ok {
		return true
	}
	return false
}

func hasBetterAuthCookie(cookies []map[string]any) bool {
	for _, ck := range cookies {
		name, _ := ck["name"].(string)
		val, _ := ck["value"].(string)
		if val == "" {
			continue
		}
		if strings.Contains(strings.ToLower(name), "better-auth") {
			return true
		}
	}
	return false
}

func displayName(email string) string {
	local := strings.TrimSpace(email)
	if i := strings.Index(local, "@"); i > 0 {
		local = local[:i]
	}
	local = strings.ReplaceAll(local, ".", " ")
	local = strings.ReplaceAll(local, "+", " ")
	local = strings.TrimSpace(local)
	if local == "" {
		return "User"
	}
	return local
}

func absURL(path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return origin + path
}
