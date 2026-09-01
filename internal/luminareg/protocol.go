package luminareg

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/url"
	"strings"
	"time"

	"chatgpt-register/internal/proxyutil"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
)

// 纯协议注册：BytePlus passport 的注册链路（会话初始化 → 邮箱查重 → 发验证码 →
// 取公钥加密密码 → 提交注册）全部是普通 JSON 接口，滑块人机校验走官方校验服务的
// /captcha/get + /captcha/verify。整条链路不需要浏览器，一次注册十几秒即可完成。
const (
	passportBase = "https://console.byteplus.com/api/passport"
	consoleBase  = "https://console.byteplus.com"
	luminaAPI    = luminaAPIBase + "/api"

	// verifyHost/captchaAID 取自 BytePlus 登录页前端配置（region i18n-bd, aid 3764）。
	verifyHost = "https://verify.rmc.byteplusbiz.com"
	captchaAID = "3764"
	// captchaDisplayWidth 是前端滑块图的显示宽度，校验接口按这个宽度换算轨迹坐标。
	captchaDisplayWidth = 340

	// captchaAttempts 滑块最多重试次数：每次失败都重新领一张新图。
	captchaAttempts = 4
)

type protoClient struct {
	cli tls_client.HttpClient
	in  Input
	fp  browserProfile
}

// passportResp 是 passport 接口的统一返回：出错时 ResponseMetadata.Error 非空。
type passportResp struct {
	ResponseMetadata struct {
		Action string `json:"Action"`
		Error  *struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
		} `json:"Error"`
	} `json:"ResponseMetadata"`
	Result json.RawMessage `json:"Result"`
}

func (r *passportResp) errCode() string {
	if r == nil || r.ResponseMetadata.Error == nil {
		return ""
	}
	return r.ResponseMetadata.Error.Code
}

// registerProtocol 走纯协议注册，成功后返回与浏览器流程同构的会话数据。
func registerProtocol(ctx context.Context, in Input) (*Result, error) {
	c, err := newProtoClient(in, nil)
	if err != nil {
		return nil, err
	}
	if _, err = c.passport(ctx, http.MethodPost, "/login/getLoginCredential", map[string]any{}, nil); err != nil {
		return nil, fmt.Errorf("初始化 passport 会话失败: %v", err)
	}
	in.logf("协议会话已建立")
	uniq, err := c.passport(ctx, http.MethodPost, "/account/checkEmailUniqueV2", map[string]any{"Email": in.Email}, nil)
	if err != nil {
		return nil, fmt.Errorf("邮箱查重失败: %v", err)
	}
	var uniqResult struct {
		Success bool `json:"Success"`
	}
	if json.Unmarshal(uniq.Result, &uniqResult) == nil && !uniqResult.Success {
		return nil, fmt.Errorf("%w（checkEmailUniqueV2 返回 Success=false）", ErrEmailTaken)
	}

	encPwd, keyID, err := c.encryptPassword(ctx, in.Password)
	if err != nil {
		return nil, err
	}
	if _, err = c.passport(ctx, http.MethodPost, "/login/sendSignupEmailforBP", map[string]any{"Email": in.Email}, nil); err != nil {
		return nil, fmt.Errorf("发送注册验证码失败: %v", err)
	}
	in.logf("已请求发送邮箱验证码，等待邮件")

	code, err := in.WaitCode(ctx)
	if err != nil {
		return nil, err
	}
	in.logf("已取得邮箱验证码: %s", code)

	payload := map[string]any{
		"Email":                        in.Email,
		"Code":                         code,
		"OptIn":                        true,
		"f_console_sign_up_url_source": luminaURL,
		"Utm":                          map[string]any{},
		"Locale":                       "en",
		"apiVersion":                   "v2",
		"Password":                     encPwd,
	}
	extra := map[string]string{
		"encryptedfields":  "Password,ConfirmPassword",
		"encryptedkeyword": keyID,
	}
	for attempt := 1; ; attempt++ {
		resp, err := c.passport(ctx, http.MethodPost, "/login/signup", payload, extra)
		switch {
		case err == nil:
			in.logf("注册接口已通过")
			// 注册接口签发的会话在 Lumina 侧仍是 guest，而同一会话重登会报
			// InvalidState，所以用干净的会话账密登录一次，拿到的才是可用会话。
			// 账号此时已建好，代理瞬断（如 502）导致的登录失败重试几次即可，
			// 别把成功的注册记成失败。
			var lc *protoClient
			var lerr error
			for try := 1; try <= 3; try++ {
				lc, lerr = c.loginFresh(ctx)
				if lerr == nil {
					break
				}
				if try < 3 {
					in.logf("登录换取会话失败（第 %d/3 次）: %v，稍后重试", try, lerr)
					if !sleepCtxProto(ctx, 5*time.Second) {
						return nil, ctx.Err()
					}
				}
			}
			if lerr != nil {
				return nil, fmt.Errorf("注册成功但登录换取会话失败: %w", lerr)
			}
			in.logf("已用账密换取正式会话")
			return lc.collect(ctx)
		case resp.errCode() == "ErrorNeedCaptcha":
			if attempt > captchaAttempts {
				return nil, fmt.Errorf("%w（滑块重试 %d 次仍被要求校验）", ErrCaptchaFailed, captchaAttempts)
			}
			var res struct {
				VerifyData string `json:"verify_data"`
			}
			if jerr := json.Unmarshal(resp.Result, &res); jerr != nil || res.VerifyData == "" {
				return nil, fmt.Errorf("%w（注册接口未返回 verify_data）", ErrCaptchaFailed)
			}
			if cerr := c.solveCaptchaProto(ctx, res.VerifyData); cerr != nil {
				in.logf("滑块第 %d/%d 次未通过: %v", attempt, captchaAttempts, cerr)
				if !sleepCtxProto(ctx, 2*time.Second) {
					return nil, ctx.Err()
				}
			}
		case resp.errCode() == "ErrorRateLimit":
			return nil, fmt.Errorf("%w（RegisterAccountV2 返回 ErrorRateLimit，换出口 IP 或稍后重试）", ErrRateLimited)
		default:
			return nil, fmt.Errorf("注册接口失败: %w", err)
		}
	}
}

// newProtoClient 建一个协议客户端。fp 为空时随机取一套浏览器特征，
// 同一个账号的后续请求要沿用同一套，避免中途换指纹。
func newProtoClient(in Input, fp *browserProfile) (*protoClient, error) {
	if fp == nil {
		p := randomBrowserProfile()
		fp = &p
	}
	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(60),
		tls_client.WithClientProfile(fp.clientProfile),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
	}
	if pu := proxyutil.Normalize(in.Proxy); pu != "" {
		opts = append(opts, tls_client.WithProxyUrl(pu))
	}
	cli, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return nil, fmt.Errorf("创建协议客户端失败: %w", err)
	}
	cli.SetFollowRedirect(true)
	return &protoClient{cli: cli, in: in, fp: *fp}, nil
}

// syncLuminaCookies 把 console 域的会话 Cookie 复制到 lumi-api 上。
// tls-client 的 jar 对 Domain=.byteplus.com 的 Cookie 只能匹配到 console.byteplus.com，
// 不同步的话请求 lumi-api 会不带会话，Lumina 只会返回 guest。
func (c *protoClient) syncLuminaCookies() {
	src, err := url.Parse(consoleBase)
	if err != nil {
		return
	}
	dst, err := url.Parse(luminaAPIBase)
	if err != nil {
		return
	}
	cks := c.cli.GetCookies(src)
	copied := make([]*http.Cookie, 0, len(cks))
	for _, ck := range cks {
		copied = append(copied, &http.Cookie{Name: ck.Name, Value: ck.Value, Path: "/"})
	}
	c.cli.SetCookies(dst, copied)
}

// loginFresh 用全新的 cookie jar 走一次账密登录，返回拿到正式会话的客户端。
func (c *protoClient) loginFresh(ctx context.Context) (*protoClient, error) {
	lc, err := newProtoClient(c.in, &c.fp)
	if err != nil {
		return nil, err
	}
	if _, err = lc.passport(ctx, http.MethodPost, "/login/getLoginCredential", map[string]any{}, nil); err != nil {
		return nil, fmt.Errorf("初始化登录会话失败: %w", err)
	}
	if _, err = lc.passport(ctx, http.MethodPost, "/login/mixtureLogin", map[string]any{
		"Identity":  c.in.Email,
		"Password":  c.in.Password,
		"EventName": "AuthAccountWithPassword",
	}, nil); err != nil {
		return nil, err
	}
	return lc, nil
}

// cookie 读取 console.byteplus.com 域下的 Cookie 值。
func (c *protoClient) cookie(name string) string {
	u, err := url.Parse(consoleBase)
	if err != nil {
		return ""
	}
	for _, ck := range c.cli.GetCookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

func (c *protoClient) do(ctx context.Context, method, rawURL string, payload any, extra map[string]string) ([]byte, int, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		return nil, 0, err
	}
	req = req.WithContext(ctx)
	req.Header = http.Header{
		"accept":             {"application/json, text/plain, */*"},
		"accept-language":    {c.fp.lang},
		"content-type":       {"application/json"},
		"origin":             {"https://ai.byteplus.com"},
		"referer":            {"https://ai.byteplus.com/"},
		"sec-ch-ua":          {c.fp.chUA},
		"sec-ch-ua-mobile":   {"?0"},
		"sec-ch-ua-platform": {c.fp.platform},
		"sec-fetch-dest":     {"empty"},
		"sec-fetch-mode":     {"cors"},
		"sec-fetch-site":     {"cross-site"},
		"user-agent":         {c.fp.ua},
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	res, err := c.cli.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, res.StatusCode, err
	}
	return raw, res.StatusCode, nil
}

// passport 发一次 passport 请求。csrfToken 每次响应都会轮换，必须用当前 Cookie 里的值。
func (c *protoClient) passport(ctx context.Context, method, path string, payload any, extra map[string]string) (*passportResp, error) {
	hdr := map[string]string{"x-csrf-token": c.cookie("csrfToken")}
	for k, v := range extra {
		hdr[k] = v
	}
	raw, status, err := c.do(ctx, method, passportBase+path, payload, hdr)
	if err != nil {
		return nil, err
	}
	var parsed passportResp
	if err = json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%s 返回非 JSON(%d): %s", path, status, trimText(string(raw), 200))
	}
	if e := parsed.ResponseMetadata.Error; e != nil {
		return &parsed, fmt.Errorf("%s 失败: %s %s", path, e.Code, e.Message)
	}
	return &parsed, nil
}

// encryptPassword 取 passport 下发的 RSA 公钥（JWK）加密密码，返回密文与密钥 ID；
// 提交注册时密钥 ID 要放在 encryptedkeyword 头里。
func (c *protoClient) encryptPassword(ctx context.Context, password string) (string, string, error) {
	raw, status, err := c.do(ctx, http.MethodGet, passportBase+"/security/encCerts", nil, nil)
	if err != nil {
		return "", "", fmt.Errorf("获取加密公钥失败: %w", err)
	}
	var certs struct {
		Keys []struct {
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err = json.Unmarshal(raw, &certs); err != nil || len(certs.Keys) == 0 {
		return "", "", fmt.Errorf("解析加密公钥失败(%d): %s", status, trimText(string(raw), 200))
	}
	k := certs.Keys[0]
	n, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.N, "="))
	if err != nil {
		return "", "", fmt.Errorf("解析公钥模数失败: %w", err)
	}
	e, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.E, "="))
	if err != nil {
		return "", "", fmt.Errorf("解析公钥指数失败: %w", err)
	}
	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	enc, err := rsa.EncryptPKCS1v15(cryptorand.Reader, pub, []byte(password))
	if err != nil {
		return "", "", fmt.Errorf("加密密码失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(enc), k.Kid, nil
}

// solveCaptchaProto 用注册接口返回的 verify_data 领一张滑块图，本地定位缺口后提交轨迹。
func (c *protoClient) solveCaptchaProto(ctx context.Context, verifyData string) error {
	var vd map[string]any
	if err := json.Unmarshal([]byte(verifyData), &vd); err != nil {
		return fmt.Errorf("解析 verify_data 失败: %w", err)
	}
	q := url.Values{}
	for k, v := range map[string]string{
		"lang": "en", "app_name": "byteplus", "iid": "0", "did": "0", "aid": captchaAID,
		"ch": "web_code", "os_type": "2", "os_name": "windows", "platform": "web",
		"webdriver": "false", "challenge_code": "99999",
		"tmp": fmt.Sprint(time.Now().UnixMilli()),
	} {
		q.Set(k, v)
	}
	// verify_data 里的字段（subtype/fp/detail/server_sdk_env/verify_event 等）原样带给校验服务。
	for _, k := range []string{"subtype", "type", "fp", "detail", "server_sdk_env", "verify_event", "region"} {
		if v, ok := vd[k]; ok {
			q.Set(k, fmt.Sprint(v))
		}
	}

	hdr := map[string]string{
		"origin":  consoleBase,
		"referer": consoleBase + "/",
	}
	raw, status, err := c.do(ctx, http.MethodGet, verifyHost+"/captcha/get?"+q.Encode(), nil, hdr)
	if err != nil {
		return fmt.Errorf("领取滑块失败: %w", err)
	}
	var got struct {
		Code int `json:"code"`
		Data struct {
			ID       string `json:"id"`
			Mode     string `json:"mode"`
			Question struct {
				URL1 string `json:"url1"`
				URL2 string `json:"url2"`
			} `json:"question"`
		} `json:"data"`
	}
	if err = json.Unmarshal(raw, &got); err != nil || got.Code != 200 || got.Data.ID == "" {
		return fmt.Errorf("领取滑块失败(%d): %s", status, trimText(string(raw), 200))
	}

	bg, err := fetchImage(got.Data.Question.URL1, c.in.Proxy, c.fp.ua)
	if err != nil {
		return err
	}
	piece, err := fetchImage(got.Data.Question.URL2, c.in.Proxy, c.fp.ua)
	if err != nil {
		return err
	}
	// 协议流程拿到的滑块图是独立小图，缺口纵坐标未知，用 -1 让匹配全图扫行。
	offset, score, err := solveOffset(bg, piece, -1)
	if err != nil {
		return err
	}
	// 轨迹坐标按前端显示宽度换算：原图宽 → captchaDisplayWidth。
	bgW := bg.Bounds().Dx()
	if bgW <= 0 {
		return fmt.Errorf("滑块背景图宽度异常")
	}
	target := int(math.Round(float64(offset) * captchaDisplayWidth / float64(bgW)))
	c.in.logf("滑块缺口定位: 原图 %dpx → 轨迹 %dpx（得分 %.1f）", offset, target, score)

	raw, status, err = c.do(ctx, http.MethodPost, verifyHost+"/captcha/verify?"+q.Encode(), map[string]any{
		"modified_img_width": captchaDisplayWidth,
		"id":                 got.Data.ID,
		"mode":               got.Data.Mode,
		"reply":              dragTrack(target),
	}, hdr)
	if err != nil {
		return fmt.Errorf("提交滑块失败: %w", err)
	}
	var verified struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err = json.Unmarshal(raw, &verified); err != nil {
		return fmt.Errorf("解析滑块校验结果失败(%d): %s", status, trimText(string(raw), 200))
	}
	if verified.Code != 200 {
		return fmt.Errorf("滑块校验未通过: %s", trimText(verified.Message, 120))
	}
	c.in.logf("滑块校验通过")
	return nil
}

// dragTrack 造一条到 target 的拖拽轨迹：步长与耗时都有抖动，纵向小幅偏移。
func dragTrack(target int) []map[string]int {
	track := make([]map[string]int, 0, target/4+1)
	x, t := 0, 0
	for x < target {
		x += 3 + ri(7)
		if x > target {
			x = target
		}
		t += 12 + ri(19)
		track = append(track, map[string]int{"x": x, "y": ri(5) - 2, "relative_time": t})
	}
	if len(track) == 0 {
		track = append(track, map[string]int{"x": 0, "y": 0, "relative_time": 20})
	}
	return track
}

// collect 采集注册成功后的会话：会话 Cookie（digest / AccountID / userInfo）、
// Lumina 使用条款 flag 与账号元信息，结构与浏览器流程保持一致。
func (c *protoClient) collect(ctx context.Context) (*Result, error) {
	u, err := url.Parse(consoleBase)
	if err != nil {
		return nil, err
	}
	cookieList := make([]map[string]any, 0, 8)
	names := map[string]bool{}
	for _, ck := range c.cli.GetCookies(u) {
		names[strings.ToLower(ck.Name)] = true
		cookieList = append(cookieList, map[string]any{
			"name":     ck.Name,
			"value":    ck.Value,
			"domain":   ".byteplus.com",
			"path":     "/",
			"expires":  ck.Expires.Unix(),
			"httpOnly": ck.HttpOnly,
			"secure":   ck.Secure,
		})
	}
	if !names["digest"] || !names["accountid"] {
		got := make([]string, 0, len(names))
		for n := range names {
			got = append(got, n)
		}
		return nil, fmt.Errorf("未采集到 BytePlus 会话 cookie（digest/AccountID），注册可能未完成（现有 cookie: %s）",
			trimText(strings.Join(got, " "), 300))
	}
	c.in.logf("已采集 %d 条 Cookie（含 BytePlus 会话 cookie）", len(cookieList))
	c.syncLuminaCookies()

	auth := map[string]any{
		"auth_mode":      "lumina_protocol_session",
		"platform":       "lumina",
		"email":          c.in.Email,
		"captured_at":    time.Now().UTC().Format(time.RFC3339),
		"cookies":        cookieList,
		"terms_accepted": c.acceptTermsProto(ctx),
	}
	if info := c.accountInfo(ctx); info != nil {
		auth["account"] = info
	}
	return &Result{AuthJSON: auth}, nil
}

// acceptTermsProto 直接调 Lumina 的 flag 接口同意使用条款（对应首登弹窗的两个勾选框）。
func (c *protoClient) acceptTermsProto(ctx context.Context) bool {
	raw, _, err := c.do(ctx, http.MethodPost, luminaAPI+"/user/flag", map[string]any{
		"has_agreed_terms_and_legal_age": true,
		"lumi_seedance2_my_portrait":     true,
	}, map[string]string{
		"origin":       luminaURL,
		"referer":      luminaURL,
		"x-csrf-token": c.cookie("csrfToken"),
	})
	if err != nil {
		c.in.logf("同意使用条款失败: %v", err)
		return false
	}
	var res struct {
		Code int `json:"code"`
	}
	if json.Unmarshal(raw, &res) != nil || res.Code != 0 {
		c.in.logf("同意使用条款未生效: %s", trimText(string(raw), 200))
		return false
	}
	return true
}

// accountInfo 带会话 Cookie 拉一次 Lumina 账号接口，取到期时间与套餐信息；失败不影响注册。
func (c *protoClient) accountInfo(ctx context.Context) map[string]any {
	hdr := map[string]string{"origin": luminaURL, "referer": luminaURL}
	get := func(path string) json.RawMessage {
		raw, _, err := c.do(ctx, http.MethodGet, luminaAPI+path, nil, hdr)
		if err != nil {
			c.in.logf("读取 %s 失败: %v", path, err)
			return nil
		}
		return json.RawMessage(raw)
	}
	combined, err := json.Marshal(map[string]json.RawMessage{
		"current":   get("/user/current"),
		"resources": get("/user/get_user_resources"),
	})
	if err != nil {
		return nil
	}
	info := parseAccountInfo(combined, c.in)
	if info != nil && info["role"] == "guest" {
		// guest 说明 Lumina 侧没认出会话（一般是没走账密登录），元信息没有价值。
		c.in.logf("Lumina 只返回 guest 身份，跳过账号元信息")
		return nil
	}
	return info
}

func sleepCtxProto(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
