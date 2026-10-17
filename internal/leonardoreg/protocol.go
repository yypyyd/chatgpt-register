package leonardoreg

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	"github.com/go-rod/rod"
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
//	导出浏览器 Cookie（含 better-auth 会话 cookie）

const (
	origin              = "https://app.leonardo.ai"
	protocolChromeMajor = "146"
	protocolStepTimeout = 45 * time.Second
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

func (c *leoClient) do(ctx context.Context, method, rawURL string, body []byte, extra map[string]string) (*protoResp, error) {
	return c.doPage(ctx, method, rawURL, body, extra)
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
	return pageCookies(c.page)
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
