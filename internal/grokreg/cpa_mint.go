package grokreg

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-rod/rod"
)

// xAI OIDC device-authorization flow constants. These mirror the reference
// project (AaronL725/grok-register, cpa_xai/oauth_device.py + schema.py) so the
// minted credential is a real CLIProxyAPI-compatible xAI OAuth token, not a
// browser cookie dump.
const (
	xaiOIDCClientID    = "b1a00492-073a-47ea-816f-4c329264a828"
	xaiOIDCDiscovery   = "https://auth.x.ai/.well-known/openid-configuration"
	xaiOIDCScope       = "openid profile email offline_access grok-cli:access api:access"
	xaiTokenEndpoint   = "https://auth.x.ai/oauth2/token"
	cpaDefaultBaseURL  = "https://cli-chat-proxy.grok.com/v1"
	cpaDefaultRedirect = "http://127.0.0.1:56121/callback"
)

type xaiDeviceSession struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
	tokenEndpoint           string
}

type xaiTokenResult struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// proxyHTTPClient builds an HTTP client that routes through the registration's
// upstream proxy so the OIDC requests share the account's egress IP.
func proxyHTTPClient(in Input, timeout time.Duration) (*http.Client, error) {
	tr := &http.Transport{}
	if raw := strings.TrimSpace(in.Proxy); raw != "" {
		u, err := url.Parse(normalizeProxy(raw))
		if err != nil {
			return nil, fmt.Errorf("解析代理失败: %w", err)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Timeout: timeout, Transport: tr}, nil
}

// mintXAIToken runs the OIDC device flow and approves it in the already
// logged-in browser page, then returns the CLIProxyAPI xAI credential payload.
func mintXAIToken(ctx context.Context, page *rod.Page, in Input) (map[string]any, error) {
	client, err := proxyHTTPClient(in, 20*time.Second)
	if err != nil {
		return nil, err
	}

	session, err := requestDeviceCode(ctx, client)
	if err != nil {
		return nil, err
	}
	in.logf("CPA: 已申请设备码 user_code=%s", session.UserCode)

	tokenCh := make(chan xaiTokenResult, 1)
	errCh := make(chan error, 1)
	pollCtx, cancelPoll := context.WithCancel(ctx)
	defer cancelPoll()
	go func() {
		tok, perr := pollDeviceToken(pollCtx, client, session)
		if perr != nil {
			errCh <- perr
			return
		}
		tokenCh <- tok
	}()

	approveErrCh := make(chan error, 1)
	go func() {
		approveErrCh <- approveDeviceInBrowser(pollCtx, page, session, in)
	}()

	deadline := time.NewTimer(time.Duration(minInt(session.ExpiresIn, 200)) * time.Second)
	defer deadline.Stop()

	for {
		select {
		case tok := <-tokenCh:
			in.logf("CPA: 设备授权成功，已获取 xAI OAuth token")
			return buildCPAXAIAuth(in.Email, tok, session.tokenEndpoint), nil
		case perr := <-errCh:
			return nil, fmt.Errorf("CPA token 轮询失败: %w", perr)
		case aerr := <-approveErrCh:
			if aerr != nil {
				// Approval failed hard; stop and surface. The poll goroutine
				// will exit via pollCtx cancellation.
				select {
				case tok := <-tokenCh:
					return buildCPAXAIAuth(in.Email, tok, session.tokenEndpoint), nil
				default:
				}
				return nil, fmt.Errorf("CPA 浏览器授权失败: %w", aerr)
			}
			// Approval finished without error; keep waiting for the token.
		case <-deadline.C:
			return nil, fmt.Errorf("CPA 设备授权超时")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func requestDeviceCode(ctx context.Context, client *http.Client) (*xaiDeviceSession, error) {
	deviceEndpoint, tokenEndpoint, err := discoverOIDC(ctx, client)
	if err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("client_id", xaiOIDCClientID)
	form.Set("scope", xaiOIDCScope)
	status, body, err := postForm(ctx, client, deviceEndpoint, form)
	if err != nil {
		return nil, fmt.Errorf("申请设备码失败: %w", err)
	}
	if status != 200 {
		return nil, fmt.Errorf("申请设备码 HTTP %d: %s", status, tailText(string(body), 200))
	}
	var s xaiDeviceSession
	if err = json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("解析设备码响应失败: %w", err)
	}
	if s.DeviceCode == "" || s.UserCode == "" {
		return nil, fmt.Errorf("设备码响应缺少字段")
	}
	if s.VerificationURI == "" {
		s.VerificationURI = "https://accounts.x.ai/oauth2/device"
	}
	if s.VerificationURIComplete == "" {
		s.VerificationURIComplete = s.VerificationURI + "?user_code=" + url.QueryEscape(s.UserCode)
	}
	if s.Interval < 1 {
		s.Interval = 5
	}
	if s.ExpiresIn < 1 {
		s.ExpiresIn = 1800
	}
	s.tokenEndpoint = tokenEndpoint
	return &s, nil
}

func discoverOIDC(ctx context.Context, client *http.Client) (deviceEndpoint, tokenEndpoint string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, xaiOIDCDiscovery, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "grok-register-cpa/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("OIDC discovery 请求失败: %w", err)
	}
	defer resp.Body.Close()
	var payload struct {
		DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
		TokenEndpoint               string `json:"token_endpoint"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", "", fmt.Errorf("解析 OIDC discovery 失败: %w", err)
	}
	if !validXAIEndpoint(payload.DeviceAuthorizationEndpoint) || !validXAIEndpoint(payload.TokenEndpoint) {
		return "", "", fmt.Errorf("OIDC discovery 返回的端点无效")
	}
	return payload.DeviceAuthorizationEndpoint, payload.TokenEndpoint, nil
}

func validXAIEndpoint(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "x.ai" || strings.HasSuffix(host, ".x.ai")
}

func pollDeviceToken(ctx context.Context, client *http.Client, s *xaiDeviceSession) (xaiTokenResult, error) {
	interval := time.Duration(s.Interval) * time.Second
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	for {
		if ctx.Err() != nil {
			return xaiTokenResult{}, ctx.Err()
		}
		form := url.Values{}
		form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
		form.Set("device_code", s.DeviceCode)
		form.Set("client_id", xaiOIDCClientID)
		status, body, err := postForm(ctx, client, s.tokenEndpoint, form)
		if err != nil {
			select {
			case <-ctx.Done():
				return xaiTokenResult{}, ctx.Err()
			case <-time.After(interval):
				continue
			}
		}
		var tok xaiTokenResult
		_ = json.Unmarshal(body, &tok)
		if status == 200 && tok.AccessToken != "" {
			if tok.RefreshToken == "" {
				return xaiTokenResult{}, fmt.Errorf("token 响应缺少 refresh_token")
			}
			return tok, nil
		}
		switch tok.Error {
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		case "expired_token", "access_denied":
			return xaiTokenResult{}, fmt.Errorf("设备授权失败: %s %s", tok.Error, tok.ErrorDesc)
		default:
			if status == 400 && tok.Error != "" {
				return xaiTokenResult{}, fmt.Errorf("设备授权 token 错误: %s %s", tok.Error, tok.ErrorDesc)
			}
		}
		select {
		case <-ctx.Done():
			return xaiTokenResult{}, ctx.Err()
		case <-time.After(interval):
		}
	}
}

func postForm(ctx context.Context, client *http.Client, endpoint string, form url.Values) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "grok-register-cpa/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if rerr != nil {
			break
		}
		if len(buf) > 1<<20 {
			break
		}
	}
	return resp.StatusCode, buf, nil
}

// approveDeviceInBrowser navigates the already-authenticated page to the device
// verification URL and approves the consent. The account is freshly registered
// and still signed in on this browser, so this normally lands straight on the
// consent screen; the email/password fallback covers a re-auth prompt. It is
// locale-agnostic (the registration browser runs in the account's geo locale,
// e.g. pt-BR) and relies on the OAuth `action=allow` form field plus the primary
// submit button rather than button labels.
func approveDeviceInBrowser(ctx context.Context, page *rod.Page, s *xaiDeviceSession, in Input) error {
	if err := page.Context(ctx).Navigate(s.VerificationURIComplete); err != nil {
		return err
	}
	deadline := time.Now().Add(150 * time.Second)
	lastSig := ""
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return nil
		}
		st := devicePageState(page)
		sig := st.URL + "|" + trimText(st.Text, 60)
		if sig != lastSig {
			in.logf("CPA 授权页: url=%s 文案=%s 按钮=%s", trimText(st.URL, 100), trimText(st.Text, 120), trimText(strings.Join(st.Buttons, " | "), 120))
			lastSig = sig
		}
		low := strings.ToLower(st.URL + " " + st.Text)
		if strings.Contains(st.URL, "device/done") ||
			strings.Contains(low, "authorized") || strings.Contains(low, "autorizado") ||
			strings.Contains(st.Text, "已授权") || strings.Contains(low, "you may close") ||
			strings.Contains(low, "pode fechar") {
			in.logf("CPA: 设备已授权")
			return nil
		}
		switch {
		case st.HasPassword:
			fillInput(page, "input[type='email'], input[name='email'], input[name='identifier']", in.Email)
			fillInput(page, "input[type='password']", in.Password)
			clickPrimarySubmit(page)
			in.logf("CPA 授权: 已提交登录")
			time.Sleep(3 * time.Second)
		case st.HasEmail:
			fillInput(page, "input[type='email'], input[name='email'], input[name='identifier']", in.Email)
			clickPrimarySubmit(page)
			time.Sleep(2 * time.Second)
		default:
			// Consent / user_code confirmation: approve via action=allow + submit.
			res := approveConsentJS(page)
			in.logf("CPA 授权: 批准动作=%s", res)
			time.Sleep(2500 * time.Millisecond)
		}
	}
	st := devicePageState(page)
	in.logf("CPA 授权超时，最后页面 url=%s 文案=%s", trimText(st.URL, 100), trimText(st.Text, 160))
	return nil
}

type devicePageInfo struct {
	URL         string   `json:"url"`
	Text        string   `json:"text"`
	HasEmail    bool     `json:"hasEmail"`
	HasPassword bool     `json:"hasPassword"`
	Buttons     []string `json:"buttons"`
}

func devicePageState(page *rod.Page) devicePageInfo {
	v, err := page.Eval(`() => {
		const buttons = Array.from(document.querySelectorAll("button, [role='button'], input[type='submit'], a[href]"))
			.map(b => (b.innerText || b.value || '').trim())
			.filter(t => t && t.length <= 40)
			.slice(0, 12);
		return JSON.stringify({
			url: location.href,
			text: (document.body && (document.body.innerText || document.body.textContent) || '').slice(0, 400),
			hasEmail: !!document.querySelector("input[type='email'], input[name='email'], input[name='identifier']"),
			hasPassword: !!document.querySelector("input[type='password']"),
			buttons: buttons
		});
	}`)
	if err != nil {
		return devicePageInfo{}
	}
	var info devicePageInfo
	_ = json.Unmarshal([]byte(v.Value.Str()), &info)
	return info
}

// approveConsentJS sets the OAuth `action=allow` field on the consent form and
// submits, then clicks the primary submit button. This works regardless of UI
// language. It skips cookie/privacy banners so they don't get submitted instead.
func approveConsentJS(page *rod.Page) string {
	v, err := page.Eval(`() => {
		const isBanner = (t) => /cookie|privac|隐私|全部允许/i.test(t || '');
		const forms = Array.from(document.querySelectorAll('form')).filter(f => !isBanner(f.innerText));
		const consent = forms.find(f => /grok|authorize|autoriz|scope|access/i.test(f.innerText)) || forms[0];
		if (consent) {
			let a = consent.querySelector("input[name=action]");
			if (!a) { a = document.createElement('input'); a.type='hidden'; a.name='action'; consent.appendChild(a); }
			a.value = 'allow';
			const submit = consent.querySelector("button[type=submit], input[type=submit]")
				|| consent.querySelector("button:not([type=button])");
			if (submit) { submit.click(); return 'form-submit-click'; }
			consent.submit();
			return 'form-submit';
		}
		const btn = document.querySelector("button[type=submit], input[type=submit]");
		if (btn) { btn.click(); return 'submit-btn'; }
		return 'none';
	}`)
	if err != nil {
		return "error"
	}
	return v.Value.Str()
}

func clickPrimarySubmit(page *rod.Page) bool {
	v, err := page.Eval(`() => {
		const btn = document.querySelector("button[type=submit], input[type=submit]") || document.querySelector("button:not([type=button])");
		if (btn) { btn.click(); return true; }
		return false;
	}`)
	if err != nil {
		return false
	}
	return v.Value.Bool()
}

func fillInput(page *rod.Page, selector, value string) bool {
	v, err := page.Eval(`(s, val) => {
		const node = document.querySelector(s);
		if (!node) return false;
		const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')?.set;
		if (setter) setter.call(node, val); else node.value = val;
		node.dispatchEvent(new Event('input', { bubbles: true }));
		node.dispatchEvent(new Event('change', { bubbles: true }));
		return true;
	}`, selector, value)
	if err != nil {
		return false
	}
	return v.Value.Bool()
}

// buildCPAXAIAuth maps the OAuth token to the CLIProxyAPI xAI auth-file schema.
func buildCPAXAIAuth(email string, tok xaiTokenResult, tokenEndpoint string) map[string]any {
	parsedEmail, sub, exp, iat := parseJWTIdentity(tok.IDToken, tok.AccessToken)
	if strings.TrimSpace(email) == "" {
		email = parsedEmail
	}
	expiresIn := tok.ExpiresIn
	if expiresIn == 0 {
		if exp > 0 && iat > 0 && exp >= iat {
			expiresIn = int(exp - iat)
		} else if exp > 0 {
			d := exp - time.Now().Unix()
			if d < 0 {
				d = 0
			}
			expiresIn = int(d)
		} else {
			expiresIn = 21600
		}
	}
	expired := ""
	if exp > 0 {
		expired = time.Unix(exp, 0).UTC().Format("2006-01-02T15:04:05Z")
	} else if expiresIn > 0 {
		expired = time.Now().Add(time.Duration(expiresIn) * time.Second).UTC().Format("2006-01-02T15:04:05Z")
	}
	if tokenEndpoint == "" {
		tokenEndpoint = xaiTokenEndpoint
	}
	tokenType := tok.TokenType
	if tokenType == "" {
		tokenType = "Bearer"
	}
	payload := map[string]any{
		"type":           "xai",
		"access_token":   tok.AccessToken,
		"refresh_token":  tok.RefreshToken,
		"token_type":     tokenType,
		"expires_in":     expiresIn,
		"expired":        expired,
		"last_refresh":   time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"email":          strings.TrimSpace(email),
		"sub":            sub,
		"base_url":       cpaDefaultBaseURL,
		"redirect_uri":   cpaDefaultRedirect,
		"token_endpoint": tokenEndpoint,
		"auth_kind":      "oauth",
	}
	if strings.TrimSpace(tok.IDToken) != "" {
		payload["id_token"] = tok.IDToken
	}
	return payload
}

func parseJWTIdentity(idToken, accessToken string) (email, sub string, exp, iat int64) {
	for _, tok := range []string{idToken, accessToken} {
		if strings.TrimSpace(tok) == "" {
			continue
		}
		parts := strings.Split(tok, ".")
		if len(parts) < 2 {
			continue
		}
		data, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			continue
		}
		var claims struct {
			Email       string      `json:"email"`
			Sub         string      `json:"sub"`
			PrincipalID string      `json:"principal_id"`
			Exp         json.Number `json:"exp"`
			Iat         json.Number `json:"iat"`
		}
		if json.Unmarshal(data, &claims) != nil {
			continue
		}
		email = strings.TrimSpace(claims.Email)
		sub = strings.TrimSpace(claims.Sub)
		if sub == "" {
			sub = strings.TrimSpace(claims.PrincipalID)
		}
		exp, _ = claims.Exp.Int64()
		iat, _ = claims.Iat.Int64()
		return email, sub, exp, iat
	}
	return "", "", 0, 0
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
