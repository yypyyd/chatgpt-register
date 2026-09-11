package leonardoreg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"

	"chatgpt-register/internal/proxyutil"
)

var errUnconfirmed = errors.New("账号未确认")

func registerProtocol(ctx context.Context, in Input) (res *Result, err error) {
	if strings.TrimSpace(in.CaptchaKey) == "" {
		return nil, fmt.Errorf("未配置 2Captcha key（设置 captcha_2captcha_key）")
	}
	in.logf("Leonardo 协议注册启动")
	if strings.TrimSpace(in.Proxy) != "" {
		server, _, _, perr := proxyutil.Parse(in.Proxy)
		if perr != nil {
			return nil, fmt.Errorf("解析代理失败: %w", perr)
		}
		in.logf("Leonardo 协议客户端使用上游代理: %s", server)
	}

	var c *leoClient
	c, err = openLeoClient(ctx, in)
	if err != nil {
		return nil, err
	}
	defer func() { c.close() }()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("Leonardo 协议注册异常: %v", r)
		}
		if err == nil || c == nil || c.page == nil {
			return
		}
		shotPage(c.page, in.SaveShot, in.logf)
	}()

	sitekey := c.scrapeSitekey()
	if sitekey == "" {
		sitekey = fallbackTurnstileSitekey
	}
	in.logf("注册配置就绪 sitekey=%s transport=%s", trimText(sitekey, 18), c.kind)

	tok, err := c.mintCaptcha(ctx, sitekey)
	if err != nil {
		return nil, err
	}
	if err = c.signup(ctx, in, sitekey, tok); err != nil {
		if errors.Is(err, ErrEmailTaken) {
			return nil, err
		}
		if errors.Is(err, errUnconfirmed) {
			in.logf("注册接口提示账号未确认，改走验证码确认")
			return c.finishWithCode(ctx, in, sitekey, true)
		}
		return nil, err
	}
	if c.sessionOK(ctx) {
		in.logf("注册后已直接拿到会话")
		return c.result(in)
	}
	return c.finishWithCode(ctx, in, sitekey, false)
}

func captchaRejected(err error) bool {
	if err == nil {
		return false
	}
	return needsCaptcha("", "", err.Error())
}

func openLeoClient(ctx context.Context, in Input) (*leoClient, error) {
	return newPageClient(ctx, in)
}

func (c *leoClient) signup(ctx context.Context, in Input, sitekey, token string) error {
	// 纯协议不先走 GraphQL 查重：那会核销一枚 2Captcha token，邮箱占用由 signup 自己返回。
	in.logf("提交注册 captcha_len=%d transport=%s", len(token), c.kind)
	if err := c.signupLegacy(ctx, in, token); err == nil || errors.Is(err, ErrEmailTaken) || errors.Is(err, errUnconfirmed) {
		return err
	} else if captchaRejected(err) {
		in.logf("注册 captcha 未通过，换新 token 重试: %v", err)
		fresh, merr := c.remintCaptcha(ctx, sitekey)
		if merr != nil {
			return merr
		}
		rerr := c.signupLegacy(ctx, in, fresh)
		if rerr == nil || errors.Is(rerr, ErrEmailTaken) || errors.Is(rerr, errUnconfirmed) {
			return rerr
		}
		if !isNotFoundErr(rerr) {
			return rerr
		}
		err = rerr
	} else if !isNotFoundErr(err) {
		return err
	}
	in.logf("旧注册接口不可用，改走 sign-up/email")
	fresh, merr := c.remintCaptcha(ctx, sitekey)
	if merr != nil {
		return merr
	}
	return c.signupBetterAuth(ctx, in, fresh)
}

func captchaMissing(code, msg, raw string) bool {
	s := strings.ToLower(code + " " + msg + " " + raw)
	return strings.Contains(s, "missing captcha")
}

func (c *leoClient) remintCaptcha(ctx context.Context, sitekey string) (string, error) {
	return c.mintCaptcha(ctx, sitekey)
}

func (c *leoClient) signupLegacy(ctx context.Context, in Input, token string) error {
	// 旧 Cognito 包装只读 body.verificationToken。不要带 x-captcha-response：
	// better-auth 插件若拦了这条路径会先 siteverify，Lambda 再核一次就失败。
	body := map[string]any{"email": in.Email, "password": in.Password, "verificationToken": token}
	var last error
	for attempt := 0; attempt < 4; attempt++ {
		r, err := c.postJSONMode(ctx, absURL(pathSignupLegacy), body, token, false)
		if err != nil {
			last = fmt.Errorf("提交注册失败: %w", err)
		} else if r.vercel() {
			last = fmt.Errorf("注册接口命中 Vercel BotID")
			in.logf("legacy signup 命中 BotID（第 %d 次）", attempt+1)
		} else {
			code, msg := parseAuthError(r.Body)
			raw := string(r.Body)
			in.logf("legacy signup HTTP %d %s", r.Status, trimText(raw, 240))
			if isUnconfirmed(code, msg, raw) {
				return errUnconfirmed
			}
			if isEmailTaken(code, msg, raw) {
				return ErrEmailTaken
			}
			if r.Status == http.StatusNotFound || r.Status == http.StatusMethodNotAllowed {
				return fmt.Errorf("not found: %s", pathSignupLegacy)
			}
			if r.Status >= 200 && r.Status < 300 && !looksLikeAPIError(r.Body) {
				return nil
			}
			return fmt.Errorf("HTTP %d: %s", r.Status, trimText(firstNonEmpty(msg, raw), 240))
		}
		if attempt < 3 && !sleepCtxLeo(ctx, time.Duration(700+attempt*400)*time.Millisecond) {
			return ctx.Err()
		}
	}
	return last
}

func (c *leoClient) signupBetterAuth(ctx context.Context, in Input, token string) error {
	body := map[string]any{
		"email":       in.Email,
		"password":    in.Password,
		"name":        displayName(in.Email),
		"callbackURL": "/",
	}
	r, err := c.postJSON(ctx, absURL(pathSignUpEmail), body, token)
	if err != nil {
		return fmt.Errorf("提交注册失败: %w", err)
	}
	if r.vercel() {
		return fmt.Errorf("注册接口命中 Vercel BotID")
	}
	code, msg := parseAuthError(r.Body)
	raw := string(r.Body)
	in.logf("sign-up/email HTTP %d %s", r.Status, trimText(raw, 240))
	if isUnconfirmed(code, msg, raw) {
		return errUnconfirmed
	}
	if isEmailTaken(code, msg, raw) {
		return ErrEmailTaken
	}
	if r.Status >= 200 && r.Status < 300 && !looksLikeAPIError(r.Body) {
		return nil
	}
	return fmt.Errorf("HTTP %d: %s", r.Status, trimText(firstNonEmpty(msg, raw), 240))
}

func (c *leoClient) finishWithCode(ctx context.Context, in Input, sitekey string, resendFirst bool) (*Result, error) {
	if resendFirst {
		if err := c.resendCode(ctx, in, sitekey); err != nil {
			in.logf("重发验证码失败（继续等信箱）: %v", err)
		}
	}
	in.logf("等待 Leonardo 邮箱验证码")
	code, err := in.WaitCode(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取邮箱验证码失败: %w", err)
	}
	if err = c.confirm(ctx, in, sitekey, code); err != nil {
		in.logf("验证码提交失败，请求重发后重试: %v", err)
		if rerr := c.resendCode(ctx, in, sitekey); rerr != nil {
			in.logf("重发验证码失败: %v", rerr)
		}
		code, err = in.WaitCode(ctx)
		if err != nil {
			return nil, fmt.Errorf("获取邮箱验证码失败: %w", err)
		}
		if err = c.confirm(ctx, in, sitekey, code); err != nil {
			return nil, err
		}
	}
	if err := c.ensureSession(ctx, in, sitekey); err != nil {
		return nil, err
	}
	return c.result(in)
}

func (c *leoClient) resendCode(ctx context.Context, in Input, sitekey string) error {
	token, err := c.mintCaptcha(ctx, sitekey)
	if err != nil {
		in.logf("重发验证码未签发 Turnstile，改走无 captcha 请求: %v", err)
		token = ""
	}
	attempts := []struct {
		path string
		body map[string]any
	}{
		{pathSendOTP, map[string]any{"email": in.Email, "type": "email-verification"}},
		{pathResendLegacy, map[string]any{"email": in.Email, "verificationToken": token}},
	}
	var last error
	for _, a := range attempts {
		r, err := c.postJSON(ctx, absURL(a.path), a.body, token)
		if err != nil {
			last = err
			continue
		}
		if r.Status == http.StatusNotFound {
			continue
		}
		if r.Status >= 200 && r.Status < 300 {
			in.logf("已请求重发验证码 %s HTTP %d", a.path, r.Status)
			return nil
		}
		last = fmt.Errorf("%s HTTP %d %s", a.path, r.Status, trimText(string(r.Body), 160))
	}
	if last == nil {
		return fmt.Errorf("没有可用的重发验证码接口")
	}
	return last
}

func (c *leoClient) confirm(ctx context.Context, in Input, sitekey, code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return fmt.Errorf("邮箱验证码为空")
	}
	type attempt struct {
		path        string
		body        map[string]any
		needCaptcha bool
	}
	attempts := []attempt{
		{pathConfirmLegacy, map[string]any{"email": in.Email, "password": in.Password, "confirmation_code": code}, false},
		{pathVerifyEmail, map[string]any{"email": in.Email, "otp": code}, false},
		{pathSignInOTP, map[string]any{"email": in.Email, "otp": code}, false},
		{pathConfirmLegacy, map[string]any{"email": in.Email, "password": in.Password, "confirmation_code": code}, true},
	}
	var last error
	for _, a := range attempts {
		if err := c.confirmOnce(ctx, in, sitekey, a.path, a.body, a.needCaptcha); err == nil {
			in.logf("验证码校验通过（%s）", a.path)
			return nil
		} else {
			last = err
			in.logf("%s 未通过: %v", a.path, err)
			if isNotFoundErr(err) {
				continue
			}
		}
	}
	if last == nil {
		last = fmt.Errorf("没有可用的确认注册接口")
	}
	return fmt.Errorf("邮箱验证码校验未通过: %w", last)
}

func (c *leoClient) confirmOnce(ctx context.Context, in Input, sitekey, path string, body map[string]any, needCaptcha bool) error {
	post := func(token string) (*protoResp, error) {
		payload := map[string]any{}
		for k, v := range body {
			payload[k] = v
		}
		if strings.TrimSpace(token) != "" {
			payload["verificationToken"] = token
		}
		r, err := c.postJSON(ctx, absURL(path), payload, token)
		if err != nil {
			return nil, err
		}
		if r.vercel() {
			return r, fmt.Errorf("确认接口命中 Vercel BotID")
		}
		return r, nil
	}
	token := ""
	if needCaptcha {
		var err error
		token, err = c.remintCaptcha(ctx, sitekey)
		if err != nil {
			return err
		}
	}
	r, err := post(token)
	if err != nil {
		return err
	}
	code, msg := parseAuthError(r.Body)
	raw := string(r.Body)
	if r.Status == http.StatusNotFound {
		return fmt.Errorf("not found: %s", path)
	}
	if needsCaptcha(code, msg, raw) && token == "" {
		token, err = c.remintCaptcha(ctx, sitekey)
		if err != nil {
			return err
		}
		r, err = post(token)
		if err != nil {
			return err
		}
		code, msg = parseAuthError(r.Body)
		raw = string(r.Body)
	}
	if r.Status >= 200 && r.Status < 300 && !looksLikeAPIError(r.Body) {
		return nil
	}
	if strings.Contains(strings.ToLower(raw), "invalid") && strings.Contains(strings.ToLower(raw), "code") {
		return fmt.Errorf("验证码无效: %s", trimText(firstNonEmpty(msg, raw), 160))
	}
	return fmt.Errorf("HTTP %d: %s", r.Status, trimText(firstNonEmpty(msg, raw), 200))
}

func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "not found")
}

func (c *leoClient) sessionOK(ctx context.Context) bool {
	if _, err := c.get(ctx, absURL(pathCrossOrigin)); err != nil {
		c.in.logf("cross-origin-cookie 失败: %v", err)
	}
	r, err := c.get(ctx, absURL(pathGetSession))
	if err != nil {
		c.in.logf("get-session 失败: %v", err)
		return false
	}
	if r.vercel() {
		c.in.logf("get-session 命中 Vercel BotID")
		return false
	}
	ok := sessionHasUser(r.Body)
	c.in.logf("get-session HTTP %d session=%t %s", r.Status, ok, trimText(string(r.Body), 180))
	return ok
}

func (c *leoClient) ensureSession(ctx context.Context, in Input, sitekey string) error {
	if c.sessionOK(ctx) {
		return nil
	}
	// Cognito confirm-signup 不会下 better-auth cookie，必须再走一次账密登录。
	in.logf("会话尚未就绪，用账密 sign-in/email 换会话")
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.remintCaptcha(ctx, sitekey)
		if err != nil {
			return fmt.Errorf("签发登录 Turnstile 失败: %w", err)
		}
		r, err := c.postJSON(ctx, absURL(pathSignInEmail), map[string]any{
			"email":       in.Email,
			"password":    in.Password,
			"callbackURL": "/",
		}, token)
		if err != nil {
			return fmt.Errorf("自动登录失败: %w", err)
		}
		in.logf("sign-in/email HTTP %d %s", r.Status, trimText(string(r.Body), 200))
		code, msg := parseAuthError(r.Body)
		if r.Status < 400 && !looksLikeAPIError(r.Body) {
			var wrap struct {
				Token string `json:"token"`
			}
			if json.Unmarshal(r.Body, &wrap) == nil && strings.TrimSpace(wrap.Token) != "" {
				c.adoptSessionToken(wrap.Token)
			}
			last = nil
			break
		}
		last = fmt.Errorf("自动登录失败 HTTP %d: %s", r.Status, trimText(firstNonEmpty(msg, code, string(r.Body)), 200))
		if !captchaRejected(last) {
			return last
		}
		in.logf("登录 captcha 未通过，换新 token 重试")
	}
	if last != nil {
		return last
	}
	for i := 0; i < 5; i++ {
		if c.sessionOK(ctx) {
			return nil
		}
		if !sleepCtxLeo(ctx, 2*time.Second) {
			return ctx.Err()
		}
	}
	if hasBetterAuthCookie(c.cookies()) {
		in.logf("get-session 仍为空，但已采集到 better-auth cookie，按成功导出")
		return nil
	}
	return fmt.Errorf("未采集到 better-auth 会话 cookie，登录可能未完成")
}

func (c *leoClient) result(in Input) (*Result, error) {
	cookies := c.cookies()
	if !hasBetterAuthCookie(cookies) {
		return nil, fmt.Errorf("未采集到 better-auth 会话 cookie，登录可能未完成")
	}
	in.logf("已采集 %d 条 Cookie（含 better-auth 会话 cookie）", len(cookies))
	auth := map[string]any{
		"auth_mode":   "leonardo_protocol_session",
		"platform":    "leonardo",
		"email":       in.Email,
		"captured_at": time.Now().UTC().Format(time.RFC3339),
		"cookies":     cookies,
	}
	return &Result{AuthJSON: auth}, nil
}

func (c *leoClient) checkEmail(ctx context.Context, in Input, token string) (taken, unconfirmed bool, err error) {
	queries := []string{
		`query GetUserByUserEmail($arg1: GetUserByUserEmailInput!) { getUserByUserEmail(arg1: $arg1) { userId confirmationStatus cognitoProvider } }`,
		`mutation GetUserByUserEmail($arg1: GetUserByUserEmailInput!) { getUserByUserEmail(arg1: $arg1) { userId confirmationStatus cognitoProvider } }`,
		`query GetUserByUserEmail($arg1: getUserByUserEmailInput!) { getUserByUserEmail(arg1: $arg1) { userId confirmationStatus cognitoProvider } }`,
	}
	vars := map[string]any{
		"arg1": map[string]any{
			"userEmail":         in.Email,
			"verificationToken": token,
		},
	}
	var last string
	for _, q := range queries {
		payload := map[string]any{"query": q, "variables": vars}
		r, rerr := c.postJSON(ctx, graphqlURL, payload, "")
		if rerr != nil {
			last = rerr.Error()
			continue
		}
		in.logf("getUserByUserEmail HTTP %d %s", r.Status, trimText(string(r.Body), 200))
		if r.vercel() {
			return false, false, fmt.Errorf("graphql 命中 Vercel BotID")
		}
		var wrap struct {
			Data struct {
				GetUserByUserEmail *struct {
					UserID             *string `json:"userId"`
					ConfirmationStatus *string `json:"confirmationStatus"`
					CognitoProvider    *string `json:"cognitoProvider"`
				} `json:"getUserByUserEmail"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if json.Unmarshal(r.Body, &wrap) != nil {
			last = trimText(string(r.Body), 180)
			continue
		}
		if len(wrap.Errors) > 0 {
			last = wrap.Errors[0].Message
			if captchaRejected(fmt.Errorf("%s", last)) {
				return false, false, fmt.Errorf("%s", last)
			}
			continue
		}
		u := wrap.Data.GetUserByUserEmail
		if u == nil || u.UserID == nil || strings.TrimSpace(*u.UserID) == "" {
			status := ""
			if u != nil && u.ConfirmationStatus != nil {
				status = *u.ConfirmationStatus
			}
			if strings.EqualFold(status, "UNCONFIRMED") {
				return true, true, nil
			}
			return false, false, nil
		}
		status := ""
		if u.ConfirmationStatus != nil {
			status = *u.ConfirmationStatus
		}
		if strings.EqualFold(status, "UNCONFIRMED") {
			return true, true, nil
		}
		return true, false, nil
	}
	if last == "" {
		last = "graphql 无响应"
	}
	return false, false, fmt.Errorf("%s", last)
}

func sleepCtxLeo(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
