package adobereg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/google/uuid"

	"chatgpt-register/internal/proxyutil"
)

// Adobe 全协议注册链路（不启动 Chromium）：
//
//	GET  auth.services.adobe.com/en_US/deeplink.html   (建立 relay/ftrset 会话)
//	POST /signin/v1/passkey                            (拿 X-Identity-Verification-Token)
//	POST /signin/v2/users/accounts                     (判断邮箱是否已有 Adobe 账号)
//	POST /signin/v2/authenticationstate?purpose=signUp (拿加密 auth state + Arkose blob)
//	POST /signin/v2/accounts                           (Arkose token + 资料，创建账号)
//	POST /signin/v2/tokens?credential=password         (刚建号后直接密码登录，无需 OTP)
//	POST /signin/v1/ims/tokens                         (换 IMS token)
//	POST adobeid-na1.services.adobe.com/ims/fromSusi   (写 ims_sid 等登录 Cookie)
//	GET  firefly.adobe.com                             (补 Firefly 会话 Cookie)
//
// 关键点：
//   - 建号后立刻用同一份 signUp auth state 做密码登录，Adobe 不会再要求邮箱验证码。
//   - 建号请求必须带 Arkose token（x-ims-arkose-captcha-token 由打码服务提供），
//     放在 X-IMS-EntCaptcha-Response 头里。
//   - fromSusi 用 IMS token；如果被拒，再用 SUSI token 重试一次。

// webClientID 返回 IMS 客户端 ID。默认 SunbreakWebUI1（Adobe 账号中心/注册用），
// 可用 ADOBE_WEB_CLIENT_ID 覆盖，例如 clio-playground-web（Firefly 网页端）。
// useMSExpiry 对应 deeplink 的 use_ms_for_expiry 参数。Firefly 弹窗登录用 false。
func useMSExpiry() string {
	if v := strings.TrimSpace(os.Getenv("ADOBE_USE_MS_EXPIRY")); v != "" {
		return v
	}
	return "true"
}

func webClientID() string {
	if v := strings.TrimSpace(os.Getenv("ADOBE_WEB_CLIENT_ID")); v != "" {
		return v
	}
	return adobeWebClientID
}

const (
	adobeWebClientID    = "SunbreakWebUI1"
	adobeAuthBase       = "https://auth.services.adobe.com"
	adobeIMSBase        = "https://ims-na1.adobelogin.com"
	adobeIDPBase        = "https://adobeid-na1.services.adobe.com"
	adobeSignupArkosePK = "436DD567-5435-4B14-89A6-2F1188E11334"
	adobeSigninArkosePK = "430FF2C3-1AB1-40B7-8BE7-44FC683FE02C"

	// account.adobe.com 登录页向 IMS 申请的完整 scope（抓包核对）。
	adobeAuthScope = "AdobeID,openid,acct_mgmt_api,gnav,read_countries_regions,social.link,unlink_social_account," +
		"additional_info.address.mail_to,client.scopes.read,publisher.read,additional_info.account_type," +
		"additional_info.roles,additional_info.social,additional_info.screen_name,additional_info.optionalAgreements," +
		"additional_info.secondary_email,additional_info.secondary_email_verified,additional_info.phonetic_name," +
		"additional_info.dob,additional_info.ownerOrg,update_profile.all,security_profile.read,security_profile.update," +
		"admin_manage_user_consent,admin_slo,piip_write,mps,last_password_update,update_email,read_organizations," +
		"email_verification.w,bis.read.pan,bis.write.pi,bis.read.pi,security_profile.w,security_profile.r,passkey.r," +
		"passkey.w,email.w,pps.read,accounts.read,uds_write,uds_read,firefly_api,passkey_read,passkey_write," +
		"account_cluster.read,account_cluster.update,additional_info.authenticatingAccount"
)

// signupCountryCode 决定注册账号的国家。默认 US；可用环境变量
// ADOBE_SIGNUP_COUNTRY 覆盖（例如 HK）。Adobe 会按账号国家决定可用模型，
// gpt-image 这类 3P 模型目前只在部分国家/账号灰度开放。
func signupCountryCode() string {
	if v := strings.TrimSpace(os.Getenv("ADOBE_SIGNUP_COUNTRY")); v != "" {
		return strings.ToUpper(v)
	}
	return "US"
}

const adobeMarketingConsentHTML = `<section data-id="MarketingConsent-Implicit"><p>By clicking Create account, I agree that:</p><ul class="pl-300"><li>I have read and accepted the <a class="dA8iBG_spectrum-Link dA8iBG_spectrum-Link--secondary" href="https://www.adobe.com/legal/terms.html" target="_blank" rel="noreferrer" tabindex="0" data-react-aria-pressable="true">Terms of Use</a>.</li><li>The <a class="dA8iBG_spectrum-Link dA8iBG_spectrum-Link--secondary" href="https://www.adobe.com/privacy/policy.html#info-share" target="_blank" rel="noreferrer" tabindex="0" data-react-aria-pressable="true">Adobe family of companies</a> may keep me informed with <a class="dA8iBG_spectrum-Link dA8iBG_spectrum-Link--secondary" href="https://www.adobe.com/privacy/marketing.html#mktg-email" target="_blank" rel="noreferrer" tabindex="0" data-react-aria-pressable="true">personalized</a> emails about products and services.</li></ul><p>See our <a class="dA8iBG_spectrum-Link dA8iBG_spectrum-Link--secondary" href="https://www.adobe.com/privacy/policy.html" target="_blank" rel="noreferrer" tabindex="0" data-react-aria-pressable="true">Privacy Policy</a> for more details or to opt-out at any time.</p></section>`

type adobeProtocolClient struct {
	cli tls_client.HttpClient
	in  Input

	ua  string
	ivt string

	authState   string
	captchaBlob string

	// redirectOverride / forceDefaultRedirect 用于“Firefly 弹窗入口注册 +
	// 用默认 account.adobe.com 回调落地会话”的组合。
	redirectOverride     string
	forceDefaultRedirect bool

	relay       string
	state       string
	callback    string
	redirectURI string
	deeplink    string
}

type adobeResp struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r *adobeResp) text() string { return string(r.Body) }

func (r *adobeResp) header(name string) string { return r.Header.Get(name) }

func newAdobeProtocolClient(in Input) (*adobeProtocolClient, error) {
	prof, ok := profiles.MappedTLSClients["Chrome_133"]
	if !ok {
		prof = profiles.Chrome_133
	}
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
		return nil, fmt.Errorf("创建 Adobe TLS 客户端失败: %w", err)
	}
	return &adobeProtocolClient{
		cli: cli,
		in:  in,
		ua:  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36",
	}, nil
}

func (c *adobeProtocolClient) headers(req *http.Request, doc bool) {
	h := req.Header
	h.Set("user-agent", c.ua)
	h.Set("accept-language", "en-US,en;q=0.9")
	h.Set("sec-ch-ua", `"Chromium";v="133", "Not(A:Brand";v="24", "Google Chrome";v="133"`)
	h.Set("sec-ch-ua-mobile", "?0")
	h.Set("sec-ch-ua-platform", `"Windows"`)
	h.Set("x-ims-clientid", webClientID())
	h.Set("origin", adobeAuthBase)
	h.Set("referer", c.deeplink)
	if doc {
		h.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
		h.Set("sec-fetch-dest", "document")
		h.Set("sec-fetch-mode", "navigate")
		h.Set("sec-fetch-site", "none")
		h.Set("sec-fetch-user", "?1")
		h.Set("upgrade-insecure-requests", "1")
	} else {
		h.Set("accept", "application/json, text/plain, */*")
		h.Set("sec-fetch-dest", "empty")
		h.Set("sec-fetch-mode", "cors")
		h.Set("sec-fetch-site", "same-origin")
	}
}

func (c *adobeProtocolClient) do(ctx context.Context, req *http.Request) (*adobeResp, error) {
	resp, err := c.cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	out := &adobeResp{Status: resp.StatusCode, Header: resp.Header, Body: body}
	if v := out.header("x-identity-verification-token"); v != "" {
		c.ivt = v
	}
	if v := out.header("x-ims-authentication-state-encrypted"); v != "" {
		c.authState = v
	}
	return out, nil
}

func (c *adobeProtocolClient) absolute(path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	return adobeAuthBase + path
}

func (c *adobeProtocolClient) getDocument(ctx context.Context, target string) (*adobeResp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	c.headers(req, true)
	req.Header.Set("sec-fetch-site", "none")
	return c.do(ctx, req)
}

func (c *adobeProtocolClient) jsonRequest(ctx context.Context, method, path string, body any, extra map[string]string) (*adobeResp, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.absolute(path), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	c.headers(req, false)
	req.Header.Set("content-type", "application/json")
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	return c.do(ctx, req)
}

func (c *adobeProtocolClient) postJSON(ctx context.Context, path string, body any, extra map[string]string) (*adobeResp, error) {
	return c.jsonRequest(ctx, http.MethodPost, path, body, extra)
}

func (c *adobeProtocolClient) putJSON(ctx context.Context, path string, body any, extra map[string]string) (*adobeResp, error) {
	return c.jsonRequest(ctx, http.MethodPut, path, body, extra)
}

func (c *adobeProtocolClient) postForm(ctx context.Context, target string, form url.Values) (*adobeResp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	c.headers(req, false)
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("sec-fetch-dest", "document")
	req.Header.Set("sec-fetch-mode", "navigate")
	req.Header.Set("sec-fetch-site", "same-site")
	return c.do(ctx, req)
}

var adobeMetaRefreshRe = regexp.MustCompile(`(?i)http-equiv=["']refresh["'][^>]*content=["']\d+\s*;\s*url=([^"']+)`)

func (c *adobeProtocolClient) follow(ctx context.Context, target string, max int) (string, *adobeResp, error) {
	cur := target
	var last *adobeResp
	for i := 0; i < max; i++ {
		resp, err := c.getDocument(ctx, cur)
		if err != nil {
			return cur, nil, err
		}
		last = resp
		if resp.Status >= 300 && resp.Status < 400 {
			loc := resp.header("location")
			if loc == "" {
				return cur, resp, nil
			}
			cur = resolveURL(cur, loc)
			continue
		}
		if m := adobeMetaRefreshRe.FindStringSubmatch(resp.text()); len(m) == 2 {
			next := strings.TrimSpace(strings.ReplaceAll(m[1], "&amp;", "&"))
			if next != "" && next != cur {
				cur = resolveURL(cur, next)
				continue
			}
		}
		return cur, resp, nil
	}
	return cur, last, nil
}

func resolveURL(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

func (c *adobeProtocolClient) setCookie(rawURL, name, value string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return
	}
	c.cli.SetCookies(u, []*http.Cookie{{Name: name, Value: value, Path: "/", Secure: true}})
}

// bootstrap 构造与 account.adobe.com 登录页一致的 deeplink 会话。
func (c *adobeProtocolClient) bootstrap(ctx context.Context) error {
	nonce := fmt.Sprintf("%d", time.Now().UnixNano()/1000)
	c.relay = uuid.NewString()
	if os.Getenv("ADOBE_STATE_MODE") == "popup" {
		c.state = fmt.Sprintf(`{"name":"access-token-flow","side":"popup","data":{"returnOrigin":"https://firefly.adobe.com","client_id":"%s","relay":"%s"}}`, webClientID(), c.relay)
	} else {
		c.state = fmt.Sprintf(`{"jslibver":"v2-v0.55.0-1-g6de2b7c","nonce":"%s"}`, nonce)
	}
	if c.redirectOverride != "" {
		c.redirectURI = c.redirectOverride
	} else if v := strings.TrimSpace(os.Getenv("ADOBE_REDIRECT_URI")); v != "" && !c.forceDefaultRedirect {
		c.redirectURI = v
	} else {
		c.redirectURI = "https://account.adobe.com/#old_hash=&from_ims=true?client_id=" + webClientID() +
			"&api=authorize&scope=" + adobeAuthScope + "&reauth=true"
	}

	cb := url.Values{}
	cb.Set("redirect_uri", c.redirectURI)
	cb.Set("client_id", webClientID())
	cb.Set("api", "authorize")
	cb.Set("scope", adobeAuthScope)
	cb.Set("reauth", "true")
	cb.Set("state", c.state)
	cb.Set("code_challenge_method", "plain")
	cb.Set("use_ms_for_expiry", useMSExpiry())
	c.callback = adobeIMSBase + "/ims/adobeid/" + webClientID() + "/AdobeID/token?" + cb.Encode()

	dq := url.Values{}
	dq.Set("deeplink", "ssofirst")
	dq.Set("callback", c.callback)
	dq.Set("client_id", webClientID())
	dq.Set("scope", adobeAuthScope)
	dq.Set("state", c.state)
	dq.Set("relay", c.relay)
	dq.Set("locale", "en_US")
	dq.Set("flow_type", "token")
	dq.Set("idp_flow_type", "login")
	dq.Set("reauthenticate", "force")
	dq.Set("s_p", "google,facebook,apple,microsoft,line,kakao")
	dq.Set("response_type", "token")
	dq.Set("code_challenge_method", "plain")
	dq.Set("redirect_uri", c.redirectURI)
	dq.Set("use_ms_for_expiry", useMSExpiry())
	if pu := strings.TrimSpace(os.Getenv("ADOBE_PUSER")); pu != "" {
		dq.Set("puser", pu)
	}
	c.deeplink = adobeAuthBase + "/en_US/deeplink.html?" + dq.Encode()

	c.setCookie(adobeAuthBase+"/", "relay", c.relay)
	c.setCookie(adobeIDPBase+"/", "relay", c.relay)

	// 先走一遍 IMS authorize，让 Adobe 下发 relay/locale Cookie；失败不阻断，
	// 后面的 deeplink 页面本身也能建立会话。
	if _, _, err := c.follow(ctx, adobeIMSBase+"/ims/authorize/v1?reauth=true&eu=false&redirect_uri="+
		url.QueryEscape(c.redirectURI)+"&client_id="+webClientID()+"&scope="+url.QueryEscape(adobeAuthScope), 6); err != nil {
		c.in.logf("Adobe IMS authorize 预检失败（继续）: %v", err)
	}
	r, err := c.getDocument(ctx, c.deeplink)
	if err != nil {
		return fmt.Errorf("打开 Adobe 注册页失败: %w", err)
	}
	if r.Status >= 400 {
		return fmt.Errorf("打开 Adobe 注册页 HTTP %d", r.Status)
	}
	c.in.logf("Adobe 协议客户端已建立会话")
	return nil
}

func (c *adobeProtocolClient) checkAccount(ctx context.Context) (bool, error) {
	r, err := c.postJSON(ctx, "/signin/v2/users/accounts", map[string]any{
		"username": c.in.Email, "usernameType": "EMAIL",
	}, nil)
	if err != nil {
		return false, err
	}
	if r.Status != 200 {
		return false, fmt.Errorf("Adobe 查号失败 HTTP %d: %s", r.Status, trimAdobe(r.text(), 200))
	}
	var accounts []map[string]any
	if err := json.Unmarshal(r.Body, &accounts); err != nil {
		return false, fmt.Errorf("Adobe 查号响应异常: %w", err)
	}
	return len(accounts) > 0, nil
}

func (c *adobeProtocolClient) createSignupState(ctx context.Context) error {
	r, err := c.postJSON(ctx, "/signin/v2/authenticationstate?purpose=signUp", map[string]any{
		"username":       c.in.Email,
		"usernameType":   "EMAIL",
		"accountType":    "individual",
		"pbaPolicy":      nil,
		"extraPbaChecks": false,
	}, map[string]string{
		"x-identity-verification-token": c.ivt,
	})
	if err != nil {
		return fmt.Errorf("创建 Adobe 注册态失败: %w", err)
	}
	if r.Status != 201 {
		return fmt.Errorf("创建 Adobe 注册态 HTTP %d: %s", r.Status, trimAdobe(r.text(), 300))
	}
	if c.authState == "" {
		return fmt.Errorf("Adobe 注册态缺少加密头")
	}
	c.captchaBlob = r.header("x-ims-captcha-encrypted")
	return nil
}

func (c *adobeProtocolClient) solveSignupCaptcha(ctx context.Context) (string, error) {
	if strings.TrimSpace(c.captchaBlob) == "" {
		return "", nil
	}
	if c.in.Captcha == nil {
		return "", fmt.Errorf("Adobe 要求图形验证，但未配置打码服务")
	}
	c.in.logf("Adobe 注册需要 Arkose，提交打码")
	token, err := c.in.Captcha.SolveFunCaptcha(ctx, adobeSignupArkosePK, c.deeplink, "https://arks-client.adobe.com", c.ua)
	if err != nil {
		return "", fmt.Errorf("Adobe Arkose 打码失败: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("Adobe Arkose 打码返回空 token")
	}
	return token, nil
}

func (c *adobeProtocolClient) createAccount(ctx context.Context, captchaToken string) error {
	day := 1 + ri(28)
	month := ri(12)
	year := GenBirthYear()
	account := map[string]any{
		"email":       c.in.Email,
		"firstName":   c.in.FirstName,
		"lastName":    c.in.LastName,
		"password":    c.in.Password,
		"countryCode": signupCountryCode(),
		"dateOfBirth": map[string]any{"day": day, "month": month, "year": year},
		"marketingConsent": map[string]any{
			"accepted": true,
			"text":     adobeMarketingConsentHTML,
		},
		"termsOfUseAcceptances": []map[string]any{{
			"accepted": true, "name": "ADOBE_MASTER", "language": "en_US",
		}},
	}
	body := map[string]any{
		"account":            account,
		"clientRedirect":     nil,
		"redirectUri":        c.redirectURI,
		"regionalOptInKorea": nil,
		"regionalOptInChina": nil,
		"locale":             "en_US",
		"idpFlow":            "login",
		"inviteCode":         nil,
	}
	extra := map[string]string{
		"x-identity-verification-token":        c.ivt,
		"x-ims-authentication-state-encrypted": c.authState,
		"x-ims-genuine-token":                  uuid.NewString(),
	}
	if captchaToken != "" {
		extra["X-IMS-EntCaptcha-Response"] = captchaToken
	}
	r, err := c.postJSON(ctx, "/signin/v2/accounts", body, extra)
	if err != nil {
		return fmt.Errorf("创建 Adobe 账号失败: %w", err)
	}
	if r.Status == 200 {
		c.in.logf("Adobe 账号创建成功")
		return nil
	}
	var payload struct {
		ErrorCode string `json:"errorCode"`
		Message   string `json:"errorMessage"`
	}
	_ = json.Unmarshal(r.Body, &payload)
	if payload.ErrorCode == "account_exists" {
		return ErrAccountExists
	}
	if payload.ErrorCode == "captcha_required" {
		return fmt.Errorf("Adobe 创建账号仍要求图形验证: %s", trimAdobe(payload.Message, 200))
	}
	return fmt.Errorf("创建 Adobe 账号 HTTP %d: %s", r.Status, trimAdobe(r.text(), 400))
}

func (c *adobeProtocolClient) passwordLogin(ctx context.Context) (string, error) {
	body := map[string]any{
		"username": c.in.Email, "usernameType": "EMAIL", "password": c.in.Password,
		"accountType": "individual", "rememberMe": false,
	}
	r, err := c.postJSON(ctx, "/signin/v2/tokens?credential=password", body, map[string]string{
		"x-identity-verification-token":        c.ivt,
		"x-ims-authentication-state-encrypted": c.authState,
	})
	if err != nil {
		return "", fmt.Errorf("Adobe 密码登录失败: %w", err)
	}
	if r.Status == 200 {
		var payload struct {
			Token string `json:"token"`
		}
		if json.Unmarshal(r.Body, &payload) != nil || payload.Token == "" {
			return "", fmt.Errorf("Adobe 密码登录响应异常: %s", trimAdobe(r.text(), 200))
		}
		c.in.logf("Adobe 密码登录成功")
		return payload.Token, nil
	}
	var payload struct {
		ErrorCode string `json:"errorCode"`
		Message   string `json:"errorMessage"`
	}
	_ = json.Unmarshal(r.Body, &payload)
	if payload.ErrorCode == "challenge_required" {
		return "", errAdobeChallengeRequired
	}
	return "", fmt.Errorf("Adobe 密码登录 HTTP %d: %s", r.Status, trimAdobe(r.text(), 300))
}

var errAdobeChallengeRequired = fmt.Errorf("Adobe 要求二次验证")

// verifyEmailChallenge 走一次邮箱验证码 challenge，把新账号的邮箱标记为已验证。
// 建号后的密码会话仍然有效，验证完成后不需要再换 token。
func (c *adobeProtocolClient) verifyEmailChallenge(ctx context.Context) error {
	if c.in.WaitCode == nil {
		return errAdobeChallengeRequired
	}
	c.in.logf("Adobe 邮箱验证：发送验证码并自动提交")
	lsID := uuid.NewString()
	body := map[string]any{
		"extraPbaChecks": false, "pbaPolicy": nil,
		"username": c.in.Email, "usernameType": "EMAIL", "accountType": "individual",
		"deviceInfo": map[string]any{"lsId": lsID, "hdId": nil},
	}
	r, err := c.postJSON(ctx, "/signin/v2/authenticationstate?purpose=multiFactorAuthentication", body, map[string]string{
		"x-identity-verification-token": c.ivt,
	})
	if err != nil {
		return err
	}
	if r.Status != 201 {
		return fmt.Errorf("Adobe 创建登录态 HTTP %d: %s", r.Status, trimAdobe(r.text(), 300))
	}
	if c.authState == "" {
		return fmt.Errorf("Adobe 登录态缺少加密头")
	}
	auth := c.authState
	ivt := c.ivt
	if _, err := c.postJSON(ctx, "/signin/v3/challenges?purpose=multiFactorAuthentication", map[string]any{}, map[string]string{
		"x-ims-authentication-state-encrypted": auth, "x-identity-verification-token": ivt,
	}); err != nil {
		return err
	}
	r, err = c.postJSON(ctx, "/signin/v3/challenges?purpose=multiFactorAuthentication&factor=email&extendedAuthState=false", map[string]any{}, map[string]string{
		"x-ims-authentication-state-encrypted": auth, "x-identity-verification-token": ivt,
	})
	if err != nil {
		return err
	}
	if r.Status != 200 {
		return fmt.Errorf("Adobe 发送验证码 HTTP %d: %s", r.Status, trimAdobe(r.text(), 300))
	}
	ivt = c.ivt
	code, err := c.in.WaitCode(ctx)
	if err != nil {
		return fmt.Errorf("读取 Adobe 验证码失败: %w", err)
	}
	r, err = c.putJSON(ctx, "/signin/v3/challenges?purpose=multiFactorAuthentication", map[string]any{"code": code}, map[string]string{
		"x-ims-authentication-state-encrypted": auth, "x-identity-verification-token": ivt,
	})
	if err != nil {
		return err
	}
	if r.Status != 200 {
		return fmt.Errorf("Adobe 验证码校验 HTTP %d: %s", r.Status, trimAdobe(r.text(), 300))
	}
	c.authState = auth
	return nil
}

// passwordLoginWithOTP 在密码登录要求二次验证时，先过邮箱 challenge 再重试密码登录。
func (c *adobeProtocolClient) passwordLoginWithOTP(ctx context.Context) (string, error) {
	if err := c.verifyEmailChallenge(ctx); err != nil {
		return "", err
	}
	return c.passwordLogin(ctx)
}

func (c *adobeProtocolClient) imsToken(ctx context.Context, susiToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, adobeAuthBase+"/signin/v1/ims/tokens", strings.NewReader("{}"))
	if err != nil {
		return "", err
	}
	c.headers(req, false)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("Authorization", "Bearer "+susiToken)
	r, err := c.do(ctx, req)
	if err != nil {
		return "", fmt.Errorf("Adobe 换 IMS token 失败: %w", err)
	}
	if r.Status != 200 {
		return "", fmt.Errorf("Adobe 换 IMS token HTTP %d: %s", r.Status, trimAdobe(r.text(), 300))
	}
	var payload struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(r.Body, &payload) != nil || payload.Token == "" {
		return "", fmt.Errorf("Adobe IMS token 响应异常: %s", trimAdobe(r.text(), 200))
	}
	return payload.Token, nil
}

func (c *adobeProtocolClient) hasIMSSID() bool {
	for _, raw := range []string{
		"https://services.adobe.com/",
		"https://adobeid-na1.services.adobe.com/",
		"https://ims-na1.adobelogin.com/",
	} {
		u, _ := url.Parse(raw)
		for _, ck := range c.cli.GetCookies(u) {
			if ck.Name == "ims_sid" && strings.TrimSpace(ck.Value) != "" {
				return true
			}
		}
	}
	return false
}

func (c *adobeProtocolClient) fromSusi(ctx context.Context, token, flow string) error {
	u, err := url.Parse(c.deeplink)
	if err != nil {
		return err
	}
	form := u.Query()
	form.Set("token", token)
	form.Set("redirect_uri", c.redirectURI)
	form.Set("response_type", "token")
	form.Set("remember_me", "undefined")
	form.Set("flow", flow)

	r, err := c.postForm(ctx, adobeIDPBase+"/ims/fromSusi", form)
	if err != nil {
		return fmt.Errorf("Adobe fromSusi 失败: %w", err)
	}
	// 成功时可能是 200 + meta refresh，也可能直接 302。
	if r.Status >= 300 && r.Status < 400 {
		target := resolveURL(adobeIDPBase+"/ims/fromSusi", r.header("location"))
		if _, _, ferr := c.follow(ctx, target, 6); ferr != nil {
			c.in.logf("Adobe fromSusi 跟随跳转失败: %v", ferr)
		}
	} else if m := adobeMetaRefreshRe.FindStringSubmatch(r.text()); len(m) == 2 {
		target := strings.TrimSpace(strings.ReplaceAll(m[1], "&amp;", "&"))
		if _, _, ferr := c.follow(ctx, target, 6); ferr != nil {
			c.in.logf("Adobe fromSusi 跟随 meta refresh 失败: %v", ferr)
		}
	}
	if c.hasIMSSID() {
		c.in.logf("Adobe IMS 登录 Cookie 已写入")
		return nil
	}
	return fmt.Errorf("Adobe fromSusi 未写入 ims_sid (HTTP %d): %s", r.Status, trimAdobe(r.text(), 200))
}

func (c *adobeProtocolClient) fireflySession(ctx context.Context) {
	if _, err := c.getDocument(ctx, "https://firefly.adobe.com/"); err != nil {
		c.in.logf("打开 Firefly 失败（账号已创建）: %v", err)
		return
	}
	_, _ = c.getDocument(ctx, "https://firefly.adobe.com/generate/images?view=generate")
}

func (c *adobeProtocolClient) cookieList() []map[string]any {
	seen := map[string]bool{}
	out := make([]map[string]any, 0, 40)
	for _, raw := range []string{
		"https://account.adobe.com/",
		"https://auth.services.adobe.com/",
		"https://adobeid-na1.services.adobe.com/",
		"https://ims-na1.adobelogin.com/",
		"https://services.adobe.com/",
		"https://firefly.adobe.com/",
		"https://idg.adobe.com/",
		"https://arks-client.adobe.com/",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		for _, ck := range c.cli.GetCookies(u) {
			domain := ck.Domain
			if domain == "" {
				domain = u.Hostname()
			}
			key := domain + "|" + ck.Path + "|" + ck.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			item := map[string]any{
				"name":     ck.Name,
				"value":    ck.Value,
				"domain":   domain,
				"path":     ck.Path,
				"expires":  float64(ck.Expires.Unix()),
				"httpOnly": ck.HttpOnly,
				"secure":   ck.Secure,
				"sameSite": "",
			}
			if ck.Path == "" {
				item["path"] = "/"
			}
			if ck.Expires.IsZero() {
				item["expires"] = -1
			}
			out = append(out, item)
		}
	}
	return out
}

func (c *adobeProtocolClient) authJSON() map[string]any {
	return map[string]any{
		"auth_mode":         "adobe_protocol_session",
		"platform":          "adobe",
		"product":           "firefly",
		"email":             c.in.Email,
		"first_name":        c.in.FirstName,
		"last_name":         c.in.LastName,
		"captured_at":       time.Now().UTC().Format(time.RFC3339),
		"arp_session_token": "",
		"cookies":           c.cookieList(),
		"storage": map[string]any{
			"localStorage":   map[string]any{},
			"sessionStorage": map[string]any{},
			"location":       "https://firefly.adobe.com/",
		},
	}
}

// registerProtocol 用纯 HTTP 完成 Adobe 注册、密码登录和 IMS 会话落地。
func registerProtocol(ctx context.Context, in Input) (*Result, error) {
	c, err := newAdobeProtocolClient(in)
	if err != nil {
		return nil, err
	}
	if err := c.bootstrap(ctx); err != nil {
		return nil, err
	}
	// 先拿 IVT（passkey 接口只返回 token 选项，不实际登录）。
	r, err := c.postJSON(ctx, "/signin/v1/passkey", map[string]any{}, nil)
	if err != nil {
		return nil, fmt.Errorf("Adobe 获取身份 token 失败: %w", err)
	}
	if r.Status != 200 || c.ivt == "" {
		return nil, fmt.Errorf("Adobe 获取身份 token HTTP %d", r.Status)
	}
	exists, err := c.checkAccount(ctx)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, ErrAccountExists
	}
	if err := c.createSignupState(ctx); err != nil {
		return nil, err
	}
	captchaToken, err := c.solveSignupCaptcha(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.createAccount(ctx, captchaToken); err != nil {
		return nil, err
	}
	susi, err := c.passwordLogin(ctx)
	if err != nil {
		if strings.Contains(err.Error(), errAdobeChallengeRequired.Error()) {
			susi, err = c.passwordLoginWithOTP(ctx)
		}
		if err != nil {
			return nil, err
		}
	}
	// 刚建的账号密码登录会成功，但邮箱还没有完成验证；主动走一次邮箱验证码
	// challenge，否则下游 cookie→token 会命中 ride_AdobeID_acct_evs。
	// 验证完成后当前密码会话仍然有效，不需要再换 token。
	if c.in.WaitCode != nil {
		if verr := c.verifyEmailChallenge(ctx); verr == nil {
			c.in.logf("Adobe 邮箱验证完成")
		} else {
			c.in.logf("Adobe 邮箱验证码流程失败（继续用密码会话）: %v", verr)
		}
	}
	ims, err := c.imsToken(ctx, susi)
	if err != nil {
		return nil, err
	}
	if err := c.fromSusi(ctx, ims, "signUp"); err != nil {
		// 少数会话只接受 SUSI token（aud=auth.services.adobe.com）。
		c.in.logf("Adobe IMS token 落地失败，改用 SUSI token 重试: %v", err)
		if err2 := c.fromSusi(ctx, susi, "signUp"); err2 != nil {
			// 弹窗回调（auth-light wrapper）只把 token 放进 URL fragment，落不了
			// ims_sid；用默认 account.adobe.com 回调重建一次 deeplink 再落地。
			if os.Getenv("ADOBE_STATE_MODE") == "popup" {
				c.in.logf("弹窗回调无法落地会话，改用默认回调重建会话")
				c.forceDefaultRedirect = true
				if berr := c.bootstrap(ctx); berr == nil {
					if e3 := c.fromSusi(ctx, ims, "signUp"); e3 == nil {
						goto sessionReady
					} else if e4 := c.fromSusi(ctx, susi, "signUp"); e4 == nil {
						goto sessionReady
					}
				}
			}
			return nil, fmt.Errorf("Adobe 登录会话落地失败: %v（SUSI 重试: %v）", err, err2)
		}
	}
sessionReady:
	c.fireflySession(ctx)
	if !c.hasIMSSID() {
		return nil, fmt.Errorf("Adobe 账号已创建，但未拿到 ims_sid 会话 Cookie")
	}

	// 协议链路只能拿到登录 Cookie，拿不到 Image 2 需要的 x-arp-session-id：
	// 该令牌由 Firefly 前端的 SherlockSdk 现场生成（含 arkose / 浏览器指纹字段），
	// 纯 HTTP 产不出。旧令牌或伪造的 sid+ftr 会被 Adobe 以伪装的 408 拒绝，
	// 所以注册末尾必须补一次浏览器采集，拿一个刚生成的新令牌。
	auth := c.authJSON()
	enriched, arpErr := CaptureARPSession(ctx, c.in, auth)
	if arpErr != nil {
		c.in.logf("ARP 会话令牌采集失败，账号无生成能力（下游 Image 2 不可用）: %v", arpErr)
		auth["arp_error"] = arpErr.Error()
		return &Result{AuthJSON: auth}, nil
	}
	return &Result{AuthJSON: enriched}, nil
}

func trimAdobe(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
