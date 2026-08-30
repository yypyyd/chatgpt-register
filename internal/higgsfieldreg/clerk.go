package higgsfieldreg

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"chatgpt-register/internal/proxyutil"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// Clerk 前端 API（FAPI）：higgsfield.ai 的注册/登录全部由 Clerk 承载，
// 站点前端只是把 clerk-js 的请求包了一层，所以注册可以完全走协议，
// 唯一需要浏览器的是 Cloudflare Turnstile 的 token。
const (
	siteURL   = "https://higgsfield.ai"
	signUpURL = siteURL + "/auth/email/sign-up"
	clerkBase = "https://clerk.higgsfield.ai"

	// clerkAPIVersion / clerkJSVersion 与站点线上 clerk-js 保持一致：
	// 版本不匹配时 FAPI 会拒绝请求或返回旧结构。
	clerkAPIVersion = "2026-05-12"
	clerkJSVersion  = "6.25.10"

	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"
)

// SignUpPageURL 站点的邮箱注册页：铸 Turnstile token 必须在这个域下做。
const SignUpPageURL = signUpURL

// client 是 Clerk FAPI 的协议客户端，cookie jar 里保存 __client 等会话 cookie。
type client struct {
	cli tls_client.HttpClient
	ua  string
}

func newClient(proxy string) (*client, error) {
	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(60),
		tls_client.WithClientProfile(profiles.Chrome_131),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
	}
	if pu := proxyutil.Normalize(proxy); pu != "" {
		opts = append(opts, tls_client.WithProxyUrl(pu))
	}
	cli, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return nil, err
	}
	cli.SetFollowRedirect(true)
	return &client{cli: cli, ua: userAgent}, nil
}

// clerkErr 是 FAPI 的错误响应，code 决定上层如何处置（重试/换邮箱/换出口）。
type clerkErr struct {
	Status int
	Code   string
	Msg    string
}

func (e *clerkErr) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("Clerk 接口失败(%d): %s", e.Status, e.Msg)
	}
	return fmt.Sprintf("Clerk 接口失败(%d) %s: %s", e.Status, e.Code, e.Msg)
}

// clerkURL 拼接 FAPI 地址：clerk-js 的每个请求都带 API 版本与 js 版本。
func clerkURL(path string) string {
	q := url.Values{
		"__clerk_api_version": {clerkAPIVersion},
		"_clerk_js_version":   {clerkJSVersion},
	}
	return clerkBase + path + "?" + q.Encode()
}

// call 发一次 FAPI 请求（表单编码，与 clerk-js 一致），解析出 response 字段。
func (c *client) call(ctx context.Context, method, path string, form url.Values) (json.RawMessage, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, clerkURL(path), body)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header = http.Header{
		"accept":          {"*/*"},
		"accept-language": {"en-US,en;q=0.9"},
		"origin":          {siteURL},
		"referer":         {siteURL + "/"},
		"user-agent":      {c.ua},
	}
	if form != nil {
		req.Header.Set("content-type", "application/x-www-form-urlencoded")
	}
	res, err := c.cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	var parsed struct {
		Response json.RawMessage `json:"response"`
		Errors   []struct {
			Code        string `json:"code"`
			Message     string `json:"message"`
			LongMessage string `json:"long_message"`
		} `json:"errors"`
	}
	if err = json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%s 返回非 JSON(%d): %s", path, res.StatusCode, trimText(string(raw), 200))
	}
	if len(parsed.Errors) > 0 {
		e := parsed.Errors[0]
		msg := e.LongMessage
		if msg == "" {
			msg = e.Message
		}
		return nil, &clerkErr{Status: res.StatusCode, Code: e.Code, Msg: msg}
	}
	if res.StatusCode >= 400 {
		return nil, &clerkErr{Status: res.StatusCode, Msg: trimText(string(raw), 200)}
	}
	if len(parsed.Response) == 0 {
		// /v1/environment 直接返回对象，没有 response 包装。
		return raw, nil
	}
	return parsed.Response, nil
}

// clerkEnv 是 /v1/environment 里注册流程要用到的部分：Turnstile 站点公钥与开关。
type clerkEnv struct {
	DisplayConfig struct {
		CaptchaProvider          string `json:"captcha_provider"`
		CaptchaPublicKey         string `json:"captcha_public_key"`
		CaptchaPublicKeyInvisibl string `json:"captcha_public_key_invisible"`
		CaptchaWidgetType        string `json:"captcha_widget_type"`
	} `json:"display_config"`
	UserSettings struct {
		SignUp struct {
			CaptchaEnabled    bool   `json:"captcha_enabled"`
			CaptchaWidgetType string `json:"captcha_widget_type"`
		} `json:"sign_up"`
	} `json:"user_settings"`
}

func (c *client) environment(ctx context.Context) (*clerkEnv, error) {
	raw, err := c.call(ctx, http.MethodGet, "/v1/environment", nil)
	if err != nil {
		return nil, err
	}
	var env clerkEnv
	if err = json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("解析 Clerk 环境配置失败: %w", err)
	}
	return &env, nil
}

// signUpAttempt 是 sign_ups 系列接口返回的注册尝试状态。
type signUpAttempt struct {
	ID               string   `json:"id"`
	Status           string   `json:"status"`
	MissingFields    []string `json:"missing_fields"`
	UnverifiedFields []string `json:"unverified_fields"`
	CreatedSessionID string   `json:"created_session_id"`
	CreatedUserID    string   `json:"created_user_id"`
}

// signUp 创建注册尝试：邮箱 + 密码 + Turnstile token（captcha_widget_type 必须与
// 环境配置一致，Clerk 用它决定去 Cloudflare 校验哪个 sitekey）。
func (c *client) signUp(ctx context.Context, email, password, captchaToken, widgetType string) (*signUpAttempt, error) {
	form := url.Values{
		"email_address": {email},
		"password":      {password},
	}
	if captchaToken != "" {
		form.Set("captcha_token", captchaToken)
		form.Set("captcha_widget_type", widgetType)
	}
	return c.attempt(ctx, http.MethodPost, "/v1/client/sign_ups", form)
}

// prepareVerification 让 Clerk 给邮箱发 6 位验证码。
func (c *client) prepareVerification(ctx context.Context, id string) (*signUpAttempt, error) {
	return c.attempt(ctx, http.MethodPost, "/v1/client/sign_ups/"+id+"/prepare_verification",
		url.Values{"strategy": {"email_code"}})
}

// attemptVerification 提交邮箱验证码，通过后 Clerk 直接创建会话。
func (c *client) attemptVerification(ctx context.Context, id, code string) (*signUpAttempt, error) {
	return c.attempt(ctx, http.MethodPost, "/v1/client/sign_ups/"+id+"/attempt_verification",
		url.Values{"strategy": {"email_code"}, "code": {code}})
}

func (c *client) attempt(ctx context.Context, method, path string, form url.Values) (*signUpAttempt, error) {
	raw, err := c.call(ctx, method, path, form)
	if err != nil {
		return nil, err
	}
	var out signUpAttempt
	if err = json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析注册状态失败: %w", err)
	}
	return &out, nil
}

// sessionToken 用会话 id 换一枚短期 JWT，站点所有业务接口都用它做 Bearer。
func (c *client) sessionToken(ctx context.Context, sessionID string) (string, error) {
	raw, err := c.call(ctx, http.MethodPost, "/v1/client/sessions/"+sessionID+"/tokens", url.Values{})
	if err != nil {
		return "", err
	}
	var out struct {
		JWT string `json:"jwt"`
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("解析会话 token 失败: %w", err)
	}
	if out.JWT == "" {
		return "", fmt.Errorf("Clerk 未返回会话 token")
	}
	return out.JWT, nil
}

// clientSession 是 /v1/client 里的一条会话，用来确认注册后会话确实是活的。
type clientSession struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// clientSessions 读当前 cookie jar 对应的 Clerk 客户端会话列表。
func (c *client) clientSessions(ctx context.Context) ([]clientSession, string, error) {
	raw, err := c.call(ctx, http.MethodGet, "/v1/client", nil)
	if err != nil {
		return nil, "", err
	}
	var out struct {
		Sessions            []clientSession `json:"sessions"`
		LastActiveSessionID string          `json:"last_active_session_id"`
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return nil, "", fmt.Errorf("解析 Clerk 客户端失败: %w", err)
	}
	return out.Sessions, out.LastActiveSessionID, nil
}

// setClerkCookie 往 FAPI 域写 cookie：用导出的 __client 恢复会话时要用。
func (c *client) setClerkCookie(name, value string) {
	u, err := url.Parse(clerkBase)
	if err != nil {
		return
	}
	c.cli.SetCookies(u, []*http.Cookie{{
		Name: name, Value: value, Path: "/", Domain: "clerk.higgsfield.ai",
	}})
}

// cookies 导出 FAPI 域下的 cookie（含 __client 长期会话 cookie）。
func (c *client) cookies() []map[string]any {
	u, err := url.Parse(clerkBase)
	if err != nil {
		return nil
	}
	out := make([]map[string]any, 0, 4)
	for _, ck := range c.cli.GetCookies(u) {
		domain := ck.Domain
		if domain == "" {
			domain = "clerk.higgsfield.ai"
		}
		out = append(out, map[string]any{
			"name":     ck.Name,
			"value":    ck.Value,
			"domain":   domain,
			"path":     "/",
			"secure":   true,
			"httpOnly": ck.HttpOnly,
		})
	}
	return out
}

func trimText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n]
}
