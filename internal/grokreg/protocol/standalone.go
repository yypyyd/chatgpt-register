package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

var ErrEmailTaken = errors.New("该邮箱已注册 Grok")
var ErrEmailSignupUnavailable = errors.New("Grok 官方暂不接受邮箱注册，请使用官网提供的其他注册方式；本批次已停止自动重试")

func (c *Client) standaloneRequest(ctx context.Context, path string, payload map[string]any) (map[string]any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, SiteURL+"/api/auth/"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setBrowserHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", SiteURL)
	req.Header.Set("Referer", SignupURLGrok)
	resp, err := c.sess.Do(req)
	if err != nil {
		return nil, Wrap(path, "请求失败", err)
	}
	data, err := readAllBody(resp)
	if err != nil {
		return nil, Wrap(path, "读取响应失败", err)
	}
	var reply map[string]any
	if !strings.Contains(resp.Header.Get("Content-Type"), "json") || json.Unmarshal(data, &reply) != nil || reply == nil {
		return nil, Failf(path, "接口返回非 JSON 响应 HTTP %d", resp.StatusCode)
	}
	code, _ := reply["code"].(string)
	if code == "account:email-signup-unavailable" {
		return nil, ErrEmailSignupUnavailable
	}
	if code == "account:existing-email" || reply["existingEmail"] == true {
		return nil, ErrEmailTaken
	}
	message, hasError := reply["error"]
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || hasError && message != nil && message != "" {
		// Report stable server codes only; response bodies can contain tokens.
		return nil, Failf(path, "注册接口拒绝 HTTP %d code=%s", resp.StatusCode, code)
	}
	return reply, nil
}

func (c *Client) SendSignupCode(ctx context.Context, email string) error {
	token, err := c.castleToken(ctx)
	if err != nil {
		return err
	}
	_, err = c.standaloneRequest(ctx, "send-verification-code", map[string]any{"email": email, "castleRequestToken": token})
	return err
}

func (c *Client) VerifySignupCode(ctx context.Context, email, code string) error {
	reply, err := c.standaloneRequest(ctx, "sign-up/verify-email", map[string]any{"email": email, "code": code})
	if err != nil {
		return err
	}
	if methods, ok := reply["signInMethods"].([]any); ok && len(methods) > 0 {
		return ErrEmailTaken
	}
	return nil
}

func (c *Client) CreateSignupAccount(ctx context.Context, email, password, first, last, code, turnstile string) (string, error) {
	token, err := c.castleToken(ctx)
	if err != nil {
		return "", err
	}
	reply, err := c.standaloneRequest(ctx, "sign-up/create-account", map[string]any{
		"email": email, "password": password, "givenName": first, "familyName": last,
		"emailValidationCode": code, "turnstileToken": turnstile, "castleRequestToken": token,
	})
	if err != nil {
		return "", err
	}
	if reply["mfaRequired"] == true {
		return "", Failf(CodeSignup, "注册要求额外身份验证")
	}
	if sso := c.jarSSO(); sso != "" {
		return sso, nil
	}
	if raw, ok := reply["redirectUrl"].(string); ok && raw != "" {
		base, _ := url.Parse(SiteURL)
		next, parseErr := base.Parse(raw)
		if parseErr != nil || next.Scheme != "https" || !trustedSignupHost(next.Hostname()) {
			return "", Failf(CodeSignup, "注册返回未知跳转地址")
		}
		if sso, e := c.followSSOHop(next.String()); e == nil && sso != "" {
			return sso, nil
		}
	}
	return "", Failf(CodeSignupNoSSO, "注册未返回有效 SSO，会话不能标记成功")
}

func trustedSignupHost(host string) bool {
	return host == "x.ai" || strings.HasSuffix(host, ".x.ai") || host == "grok.com" || strings.HasSuffix(host, ".grok.com") || host == "auth.grokusercontent.com" || host == "auth.grokipedia.com"
}
