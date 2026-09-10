package codexreg

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/google/uuid"

	"chatgpt-register/internal/proxyutil"
)

// 纯协议注册链路（不开浏览器）：
//
//	chatgpt.com/auth/login (拿 CF/NextAuth Cookie)
//	→ GET  /api/auth/csrf
//	→ POST /api/auth/signin/openai?login_hint=<email>  → {url: auth.openai.com/api/accounts/authorize?...}
//	→ GET  authorize  → 302 /email-verification（服务端已按 login_hint 发验证码）
//	→ POST auth.openai.com/api/accounts/email-otp/validate {code}
//	    → 新号: continue_url=/about-you → POST /api/accounts/create_account {name,birthdate}
//	    → 老号: continue_url=chatgpt.com/api/auth/callback/openai?code=...
//	→ GET  callback → 302 chatgpt.com/（种下 __Secure-next-auth.session-token）
//	→ GET  /api/auth/session → accessToken
//
// 前端会给 validate/create_account 附带 openai-sentinel-* 头，实测服务端目前不强制，
// 因此这里不做 Sentinel；若日后返回 403/`sentinel` 相关错误，需要回退浏览器引擎。
// 关键点是 TLS 指纹要够新：Chrome 131 指纹会被 auth.openai.com 的 Cloudflare 拦下（cf-mitigated: challenge），
// Chrome 133+ 可过。

const (
	protocolTLSProfile   = "chrome_146"
	protocolChromeMajor  = "146"
	protocolStepTimeout  = 45 * time.Second
	protocolRedirectHops = 8
)

// ErrProtocolChallenged 协议链路被 Cloudflare 人机验证拦下（cf-mitigated: challenge）。
// 与 ErrIPBlocked 同类，供上层换 IP 重试；单独定义方便统计协议引擎的拦截率。
var ErrProtocolChallenged = fmt.Errorf("%w: 协议请求命中 Cloudflare challenge", ErrIPBlocked)

type protocolClient struct {
	cli       tls_client.HttpClient
	ua        string
	acceptLng string
	languages string
	platform  string
	deviceID  string
	navID     string
	screen    ScreenProfile
	in        Input
}

func newProtocolClient(in Input, languages string) (*protocolClient, error) {
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
		tls_client.WithNotFollowRedirects(),
	}
	if p := strings.TrimSpace(in.Proxy); p != "" {
		opts = append(opts, tls_client.WithProxyUrl(proxyutil.Normalize(p)))
	}
	cli, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return nil, fmt.Errorf("创建 TLS 客户端失败: %w", err)
	}
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + protocolChromeMajor + ".0.0.0 Safari/537.36"
	return &protocolClient{
		cli: cli, ua: ua, acceptLng: acceptLanguageHeader(languages),
		languages: languages, platform: "Windows", navID: uuid.NewString(),
		screen: pickScreenProfile(), in: in,
	}, nil
}

// acceptLanguageHeader 把 "en-SG,en" 变成 "en-SG,en;q=0.9"。
func acceptLanguageHeader(languages string) string {
	parts := strings.Split(languages, ",")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return "en-US,en;q=0.9"
	}
	out := []string{strings.TrimSpace(parts[0])}
	q := 0.9
	for _, p := range parts[1:] {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, fmt.Sprintf("%s;q=%.1f", p, q))
		if q > 0.5 {
			q -= 0.1
		}
	}
	return strings.Join(out, ",")
}

func (c *protocolClient) headers(req *http.Request, referer string, doc bool) {
	h := req.Header
	h.Set("user-agent", c.ua)
	h.Set("accept-language", c.acceptLng)
	h.Set("sec-ch-ua", fmt.Sprintf(`"Google Chrome";v="%s", "Chromium";v="%s", "Not_A Brand";v="24"`, protocolChromeMajor, protocolChromeMajor))
	h.Set("sec-ch-ua-mobile", "?0")
	h.Set("sec-ch-ua-platform", `"`+c.platform+`"`)
	if referer != "" {
		h.Set("referer", referer)
	}
	if doc {
		h.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
		h.Set("upgrade-insecure-requests", "1")
		h.Set("sec-fetch-site", "cross-site")
		h.Set("sec-fetch-mode", "navigate")
		h.Set("sec-fetch-dest", "document")
	} else {
		h.Set("accept", "application/json")
		h.Set("sec-fetch-site", "same-origin")
		h.Set("sec-fetch-mode", "cors")
		h.Set("sec-fetch-dest", "empty")
	}
	h[http.HeaderOrderKey] = []string{
		"accept", "accept-language", "content-type", "origin", "referer",
		"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform",
		"sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site", "upgrade-insecure-requests", "user-agent",
	}
}

type protoResp struct {
	Status   int
	Location string
	Body     []byte
	Header   http.Header
}

func (c *protocolClient) do(ctx context.Context, req *http.Request) (*protoResp, error) {
	req = req.WithContext(ctx)
	resp, err := c.cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	out := &protoResp{Status: resp.StatusCode, Location: resp.Header.Get("Location"), Body: body, Header: resp.Header}
	if resp.Header.Get("cf-mitigated") == "challenge" {
		return out, ErrProtocolChallenged
	}
	return out, nil
}

func (c *protocolClient) get(ctx context.Context, u, referer string, doc bool) (*protoResp, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c.headers(req, referer, doc)
	return c.do(ctx, req)
}

func (c *protocolClient) postJSON(ctx context.Context, u, referer string, payload any) (*protoResp, error) {
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, u, strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	c.headers(req, referer, false)
	req.Header.Set("content-type", "application/json")
	pu, _ := url.Parse(u)
	req.Header.Set("origin", pu.Scheme+"://"+pu.Host)
	if strings.Contains(u, "/api/accounts/create_account") {
		c.attachSentinel(req, "oauth_create_account")
	}
	if strings.Contains(u, "/api/accounts/email-otp/validate") {
		c.attachSentinel(req, "email_otp_validate")
	}
	return c.do(ctx, req)
}

// followRedirects 从 start 出发一路跟 3xx，直到非 3xx 或跳数用尽；返回最终 URL 与响应。
func (c *protocolClient) followRedirects(ctx context.Context, start, referer string) (string, *protoResp, error) {
	cur := start
	for i := 0; i < protocolRedirectHops; i++ {
		resp, err := c.get(ctx, cur, referer, true)
		if err != nil {
			return cur, resp, err
		}
		if resp.Status/100 != 3 || resp.Location == "" {
			return cur, resp, nil
		}
		base, _ := url.Parse(cur)
		next, perr := base.Parse(resp.Location)
		if perr != nil {
			return cur, resp, fmt.Errorf("解析跳转地址失败: %w", perr)
		}
		referer = cur
		cur = next.String()
	}
	return cur, nil, fmt.Errorf("跳转次数过多（>%d）", protocolRedirectHops)
}

// authAPIError 是 auth.openai.com 的统一错误结构。
type authAPIError struct {
	Error struct {
		Message     string `json:"message"`
		Code        string `json:"code"`
		RedirectURI string `json:"redirect_uri"`
	} `json:"error"`
}

func parseAuthError(body []byte) (code, msg, redirect string) {
	var e authAPIError
	if json.Unmarshal(body, &e) == nil && e.Error.Code != "" {
		return e.Error.Code, e.Error.Message, e.Error.RedirectURI
	}
	return "", strings.TrimSpace(string(body)), ""
}

// authErrorPayload 是 /error?payload=<base64 json> 里的内容。
type authErrorPayload struct {
	Kind      string `json:"kind"`
	ErrorCode string `json:"errorCode"`
}

func mapAuthError(code, msg string) error {
	switch code {
	case "account_deactivated":
		return ErrAccountTaken
	case "user_already_exists":
		return ErrEmailExists
	case "registration_disallowed_too_young":
		return fmt.Errorf("%w: 生日未满 18 岁", ErrTermsRejected)
	case "registration_disallowed":
		return ErrTermsRejected
	case "rate_limit_exceeded":
		return fmt.Errorf("%w: auth.openai.com 限流(rate_limit_exceeded)", ErrIPBlocked)
	}
	if code != "" {
		return fmt.Errorf("auth.openai.com 返回 %s: %s", code, msg)
	}
	return fmt.Errorf("auth.openai.com 返回异常: %s", truncateForLog(msg, 200))
}

func truncateForLog(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// registerProtocol 用纯 HTTP 请求完成注册，产出与浏览器流程等价的 browserResult。
func registerProtocol(ctx context.Context, in Input) (res *browserResult, err error) {
	in.logf("🚀 启动纯协议注册流程（TLS 指纹 %s）...", protocolTLSProfile)

	geo := lookupGeoIPViaRequest(in)
	locale, languages := "en-US", "en-US,en"
	if geo != nil {
		locale, languages = localeForCountry(geo.CountryCode)
	}
	c, err := newProtocolClient(in, languages)
	if err != nil {
		return nil, err
	}
	res = &browserResult{UserAgent: c.ua, Screen: c.screen, Locale: locale, Languages: languages}
	if geo != nil {
		res.EgressIP, res.Country, res.Timezone = geo.Query, geo.CountryCode, geo.Timezone
	}

	// 1. 首页：种 __cf_bm / oai-did / next-auth csrf
	const loginPage = "https://chatgpt.com/auth/login"
	if r, err := c.get(ctx, loginPage, "", true); err != nil {
		return nil, fmt.Errorf("打开 chatgpt.com 失败: %w", err)
	} else if r.Status != 200 {
		return nil, fmt.Errorf("chatgpt.com 首页 HTTP %d", r.Status)
	}
	in.logf("✅ 注册页已加载")

	deviceID := cookieValue(c, "https://chatgpt.com/", "oai-did")
	if deviceID == "" {
		deviceID = uuid.NewString()
	}
	c.deviceID = deviceID

	// 2. csrf
	r, err := c.get(ctx, "https://chatgpt.com/api/auth/csrf", loginPage, false)
	if err != nil {
		return nil, fmt.Errorf("获取 csrf 失败: %w", err)
	}
	var csrf struct {
		CSRFToken string `json:"csrfToken"`
	}
	if json.Unmarshal(r.Body, &csrf) != nil || csrf.CSRFToken == "" {
		return nil, fmt.Errorf("csrf 响应异常 (HTTP %d): %s", r.Status, truncateForLog(string(r.Body), 200))
	}

	// 3. signin/openai → authorize url（ext-oai-did 必须与 chatgpt.com 的 oai-did cookie 相同）
	q := url.Values{}
	q.Set("prompt", "login")
	q.Set("ext-oai-did", deviceID)
	q.Set("auth_session_logging_id", uuid.NewString())
	q.Set("screen_hint", "login_or_signup")
	// login_hint 必须保留 plus addressing 的 '+'。url.Values.Encode 会把 '+'
	// 编成空格，OpenAI 就会把 user+001@ 当成母号 user@，OTP 后 create_account
	// 报 user_already_exists / Terms。
	authQuery := q.Encode() + "&login_hint=" + url.QueryEscape(in.Email)
	form := url.Values{}
	form.Set("callbackUrl", "/")
	form.Set("csrfToken", csrf.CSRFToken)
	form.Set("json", "true")
	req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/api/auth/signin/openai?"+authQuery, strings.NewReader(form.Encode()))
	c.headers(req, loginPage, false)
	req.Header.Set("accept", "*/*")
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("origin", "https://chatgpt.com")
	r, err = c.do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("signin/openai 失败: %w", err)
	}
	var signin struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(r.Body, &signin) != nil || !strings.Contains(signin.URL, "auth.openai.com") {
		return nil, fmt.Errorf("signin/openai 响应异常 (HTTP %d): %s", r.Status, truncateForLog(string(r.Body), 200))
	}
	in.logf("🔗 authorize login_hint=%s did=%s", extractLoginHint(signin.URL), deviceID)

	// 4. authorize → 跟随跳转。落地 /email-verification 表示验证码已发出。
	otpSentAt := time.Now().Add(-5 * time.Second)
	landing, lr, err := c.followRedirects(ctx, signin.URL, "https://chatgpt.com/")
	if err != nil && isTransientNetErr(err) {
		in.logf("♻ authorize 连接中断，重试一次")
		landing, lr, err = c.followRedirects(ctx, signin.URL, "https://chatgpt.com/")
	}
	if err != nil {
		return nil, fmt.Errorf("authorize 跳转失败: %w", err)
	}
	switch {
	case strings.Contains(landing, "/email-verification"):
		in.logf("📧 已提交邮箱，验证码已发出")
	case strings.Contains(landing, "/create-account/password"):
		in.logf("🔒 创建密码页已出现，自动设置密码")
		next, err := c.submitCreatePassword(ctx, landing, in.Password)
		if err != nil {
			return nil, err
		}
		if next != "" {
			landing2, lr2, ferr := c.followRedirects(ctx, next, landing)
			if ferr != nil {
				return nil, fmt.Errorf("提交密码后跳转失败: %w", ferr)
			}
			landing, lr = landing2, lr2
		}
		if !strings.Contains(landing, "/email-verification") && !strings.Contains(landing, "/about-you") {
			return nil, fmt.Errorf("提交密码后未进入验证码页: %s", truncateForLog(landing, 200))
		}
		if strings.Contains(landing, "/email-verification") {
			in.logf("📧 已提交邮箱，验证码已发出")
		}
	case strings.Contains(landing, "/log-in/password"):
		in.logf("🔒 该邮箱已有账号（密码登录页）")
		return nil, ErrAccountTaken
	case strings.Contains(landing, "/error"):
		if lu, perr := url.Parse(landing); perr == nil {
			if payload := lu.Query().Get("payload"); payload != "" {
				var ep authErrorPayload
				if b, derr := base64Decode(payload); derr == nil && json.Unmarshal(b, &ep) == nil {
					return nil, mapAuthError(ep.ErrorCode, ep.Kind)
				}
			}
		}
		return nil, fmt.Errorf("authorize 落到错误页: %s", truncateForLog(landing, 200))
	default:
		status := 0
		if lr != nil {
			status = lr.Status
		}
		return nil, fmt.Errorf("authorize 落地页未知 (HTTP %d): %s", status, truncateForLog(landing, 200))
	}

	// 5. 收码 + 校验（验证码错误则重发一次）
	code, err := in.FetchCode(ctx, otpSentAt)
	if err != nil {
		return nil, fmt.Errorf("获取邮箱验证码失败: %w", err)
	}
	continueURL, err := c.validateOTP(ctx, landing, strings.TrimSpace(code))
	if err != nil && strings.Contains(err.Error(), "wrong_email_otp_code") {
		in.logf("📨 验证码被拒，重发后再试一次")
		resentAt := time.Now()
		if _, rerr := c.postJSON(ctx, "https://auth.openai.com/api/accounts/email-otp/resend", landing, map[string]any{}); rerr != nil {
			return nil, fmt.Errorf("重发验证码失败: %w", rerr)
		}
		code, err = in.FetchCode(ctx, resentAt)
		if err != nil {
			return nil, fmt.Errorf("重发后获取验证码失败: %w", err)
		}
		continueURL, err = c.validateOTP(ctx, landing, strings.TrimSpace(code))
	}
	if err != nil {
		return nil, err
	}
	in.logf("🔑 验证码通过")

	// 6. 新号需要填资料（about-you → create_account）
	if strings.Contains(continueURL, "/about-you") {
		in.logf("📝 账户完善页面已出现")
		if _, err := c.get(ctx, continueURL, landing, true); err != nil {
			return nil, fmt.Errorf("打开资料页失败: %w", err)
		}
		birth := protocolBirthdate(in.Age)
		in.logf("👤 提交资料 name=%s age=%s birthdate=%s email=%s", in.FullName, in.Age, birth, in.Email)
		r, err = c.postJSON(ctx, "https://auth.openai.com/api/accounts/create_account", continueURL, map[string]any{
			"name":      in.FullName,
			"birthdate": birth,
		})
		if err != nil {
			return nil, fmt.Errorf("create_account 失败: %w", err)
		}
		if r.Status != 200 {
			code, msg, redirect := parseAuthError(r.Body)
			in.logf("⛔ create_account HTTP %d code=%s redirect=%s body=%s", r.Status, code, redirect, truncateForLog(string(r.Body), 800))
			if code == "user_already_exists" {
				who, probeErr := c.probeExistingSessionEmail(ctx, continueURL, redirect)
				if probeErr != nil {
					in.logf("🔎 跟随 login 探测失败: %v", probeErr)
				} else {
					in.logf("🔎 user_already_exists 登录后 session.email=%s 提交邮箱=%s", who, in.Email)
				}
			}
			return nil, mapAuthError(code, msg)
		}
		var cr struct {
			ContinueURL string `json:"continue_url"`
		}
		if json.Unmarshal(r.Body, &cr) != nil || cr.ContinueURL == "" {
			return nil, fmt.Errorf("create_account 响应缺少 continue_url: %s", truncateForLog(string(r.Body), 200))
		}
		in.logf("👤 资料已接受")
		continueURL = cr.ContinueURL
	}

	// 7. OAuth 回调 → chatgpt.com 会话
	if !strings.Contains(continueURL, "chatgpt.com/api/auth/callback/") {
		return nil, fmt.Errorf("未拿到 chatgpt.com 回调地址: %s", truncateForLog(continueURL, 200))
	}
	if _, _, err := c.followRedirects(ctx, continueURL, "https://auth.openai.com/"); err != nil {
		return nil, fmt.Errorf("OAuth 回调失败: %w", err)
	}

	// 8. accessToken
	in.logf("🔑 提取 accessToken...")
	r, err = c.get(ctx, "https://chatgpt.com/api/auth/session", "https://chatgpt.com/", false)
	if err != nil {
		return nil, fmt.Errorf("读取 session 失败: %w", err)
	}
	var sess map[string]any
	if json.Unmarshal(r.Body, &sess) != nil {
		return nil, fmt.Errorf("解析 session JSON 失败: %s", truncateForLog(string(r.Body), 200))
	}
	token, _ := sess["accessToken"].(string)
	if token == "" {
		return nil, fmt.Errorf("未找到 accessToken，可能未登录成功")
	}
	res.AccessToken = token
	in.logf("🔑 accessToken 获取成功")

	res.Cookies = c.webCookies()
	if len(res.Cookies) == 0 {
		return nil, fmt.Errorf("登录成功但没有捕获到会话 Cookie")
	}
	in.logf("🍪 协议会话已保存（%d 个 Cookie）", len(res.Cookies))
	return res, nil
}

func sessionUserEmail(body []byte) string {
	var sess map[string]any
	if json.Unmarshal(body, &sess) != nil {
		return ""
	}
	if u, ok := sess["user"].(map[string]any); ok {
		if e, _ := u["email"].(string); e != "" {
			return e
		}
	}
	if e, _ := sess["email"].(string); e != "" {
		return e
	}
	return ""
}

func (c *protocolClient) probeExistingSessionEmail(ctx context.Context, aboutYouURL, redirect string) (string, error) {
	start := strings.TrimSpace(redirect)
	if start == "" {
		start = "https://chatgpt.com/auth/login_with?callback_path=/"
	}
	landing, _, err := c.followRedirects(ctx, start, aboutYouURL)
	if err != nil {
		return "", err
	}
	c.in.logf("🔎 login redirect 落地=%s", truncateForLog(landing, 180))
	r, err := c.get(ctx, "https://chatgpt.com/api/auth/session", "https://chatgpt.com/", false)
	if err != nil {
		return "", err
	}
	c.in.logf("🔎 session HTTP %d body=%s", r.Status, truncateForLog(string(r.Body), 400))
	return sessionUserEmail(r.Body), nil
}

func cookieValue(c *protocolClient, site, name string) string {
	u, err := url.Parse(site)
	if err != nil {
		return ""
	}
	for _, ck := range c.cli.GetCookies(u) {
		if ck != nil && ck.Name == name && ck.Value != "" {
			return ck.Value
		}
	}
	return ""
}

func isTransientNetErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, k := range []string{"eof", "connection reset", "broken pipe", "i/o timeout", "tls handshake"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func extractLoginHint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Query().Get("login_hint")
}

func preserveLoginHintPlus(raw, email string) string {
	if !strings.Contains(email, "+") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	hint := q.Get("login_hint")
	if hint == email {
		return raw
	}
	// url.Parse Query() 会把 %2B 解成 '+'；若服务端回的是空格替换后的母号，强制写回原邮箱。
	q.Set("login_hint", email)
	// Query().Encode() 会把 space 编成 '+'，必须手动拼 login_hint。
	q.Del("login_hint")
	enc := q.Encode()
	if enc != "" {
		enc += "&"
	}
	u.RawQuery = enc + "login_hint=" + url.QueryEscape(email)
	return u.String()
}

func (c *protocolClient) attachSentinel(req *http.Request, flow string) {
	id := c.deviceID
	if id == "" {
		id = uuid.NewString()
		c.deviceID = id
	}
	if c.navID == "" {
		c.navID = uuid.NewString()
	}
	tok := map[string]any{
		"p":    c.sentinelP(),
		"id":   id,
		"flow": flow,
	}
	b, _ := json.Marshal(tok)
	req.Header.Set("openai-sentinel-token", string(b))
	req.Header.Set("x-openai-document-navigation-id", c.navID)
	req.Header.Set("x-access-flow-invocation-id", uuid.NewString())
}

func (c *protocolClient) sentinelP() string {
	lang := c.languages
	if lang == "" {
		lang = "en-US,en"
	}
	primary := strings.Split(lang, ",")[0]
	sdk := "https://sentinel.openai.com/backend-api/sentinel/sdk.js"
	now := time.Now()
	fp := []any{
		c.screen.Width,
		now.Format("Mon Jan 02 2006 15:04:05 GMT-0700 (MST)"),
		c.screen.Width * c.screen.Height * 4,
		5,
		c.ua,
		sdk,
		nil,
		primary,
		lang,
		26,
		"languages−" + lang,
		"location",
		"cookieStore",
		float64(55000+ri(2000)) + 0.9,
		c.navID,
		"",
		16,
		float64(now.UnixMilli()) + 0.6,
		0, 0, 0, 0, 0, 0, 0,
	}
	raw, _ := json.Marshal(fp)
	return "gAAAAAB" + base64.StdEncoding.EncodeToString(raw) + "~S"
}

func protocolBirthdate(ageStr string) string {
	age, _ := strconv.Atoi(strings.TrimSpace(ageStr))
	if age < 19 || age > 90 {
		age = 21 + ri(20) // 21–40，避开刚满 18 的边界
	}
	now := time.Now()
	year := now.Year() - age
	month := time.Month(1 + ri(12))
	day := 1 + ri(28)
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
}

func (c *protocolClient) submitCreatePassword(ctx context.Context, referer, password string) (string, error) {
	if strings.TrimSpace(password) == "" {
		password = GenPassword(16)
	}
	r, err := c.postJSON(ctx, "https://auth.openai.com/api/accounts/user/register", referer,
		map[string]string{"password": password})
	if err != nil {
		return "", fmt.Errorf("创建密码失败: %w", err)
	}
	if r.Status/100 == 3 && r.Location != "" {
		base, _ := url.Parse(referer)
		next, perr := base.Parse(r.Location)
		if perr == nil {
			return next.String(), nil
		}
		return r.Location, nil
	}
	if r.Status == 200 {
		var v struct {
			ContinueURL string `json:"continue_url"`
		}
		_ = json.Unmarshal(r.Body, &v)
		return v.ContinueURL, nil
	}
	code, msg, _ := parseAuthError(r.Body)
	return "", mapAuthError(code, msg)
}

func (c *protocolClient) validateOTP(ctx context.Context, referer, code string) (string, error) {
	r, err := c.postJSON(ctx, "https://auth.openai.com/api/accounts/email-otp/validate", referer, map[string]string{"code": code})
	if err != nil {
		return "", fmt.Errorf("提交验证码失败: %w", err)
	}
	if r.Status != 200 {
		ecode, msg, _ := parseAuthError(r.Body)
		if ecode == "wrong_email_otp_code" {
			return "", fmt.Errorf("wrong_email_otp_code: %s", msg)
		}
		return "", mapAuthError(ecode, msg)
	}
	var v struct {
		ContinueURL string `json:"continue_url"`
	}
	if json.Unmarshal(r.Body, &v) != nil || v.ContinueURL == "" {
		return "", fmt.Errorf("验证码响应缺少 continue_url: %s", truncateForLog(string(r.Body), 200))
	}
	c.in.logf("🔑 OTP continue_url=%s", truncateForLog(v.ContinueURL, 180))
	return v.ContinueURL, nil
}

// webCookies 从 jar 里导出 chatgpt.com / openai.com 的 Cookie，格式与浏览器版一致。
func (c *protocolClient) webCookies() []WebCookie {
	seen := map[string]bool{}
	var out []WebCookie
	for _, site := range []string{"https://chatgpt.com/", "https://auth.openai.com/", "https://openai.com/"} {
		u, _ := url.Parse(site)
		for _, ck := range c.cli.GetCookies(u) {
			if ck == nil || ck.Name == "" {
				continue
			}
			domain := ck.Domain
			if domain == "" {
				domain = u.Host
			}
			if !isChatGPTCookieDomain(domain) {
				continue
			}
			key := domain + "|" + ck.Path + "|" + ck.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			path := ck.Path
			if path == "" {
				path = "/"
			}
			wc := WebCookie{Name: ck.Name, Value: ck.Value, Domain: domain, Path: path,
				HTTPOnly: ck.HttpOnly, Secure: ck.Secure, SameSite: sameSiteString(ck.SameSite)}
			if !ck.Expires.IsZero() {
				wc.Expires = float64(ck.Expires.Unix())
			} else {
				wc.Session = true
			}
			out = append(out, wc)
		}
	}
	return out
}

func sameSiteString(s http.SameSite) string {
	switch s {
	case http.SameSiteLaxMode:
		return "Lax"
	case http.SameSiteStrictMode:
		return "Strict"
	case http.SameSiteNoneMode:
		return "None"
	}
	return ""
}

// base64Decode 兼容 URL-safe / 标准字母表，带或不带 padding。
func base64Decode(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "=")); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}
