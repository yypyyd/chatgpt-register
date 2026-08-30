// Package higgsfieldreg 走协议完成 higgsfield.ai 的邮箱注册：
// 站点用 Clerk 做账号体系，注册/发码/验码/建会话全部是 Clerk FAPI 的 HTTP 接口，
// 唯一必须由浏览器（或打码平台）产出的是 Cloudflare Turnstile 的 token。
package higgsfieldreg

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

// ErrEmailTaken 该邮箱已有 Higgsfield 账号，属于永久失败：换出口重试也没用。
var ErrEmailTaken = errors.New("该邮箱已注册 Higgsfield")

// ErrCaptcha 拿不到 / Cloudflare 不认这枚 Turnstile token，换出口或换 token 来源后重试。
var ErrCaptcha = errors.New("Turnstile 人机校验未通过")

// captchaAttempts 一次注册最多换几枚 Turnstile token：token 是一次性的，
// 被 Cloudflare 判定时再换也是白搭，试满就早失败让上层换出口。
const captchaAttempts = 2

type Input struct {
	Email    string
	Password string
	Proxy    string

	// MintToken 产出一枚可用的 Turnstile token（浏览器真实点选或打码平台），
	// sitekey 由 Clerk 环境配置下发。协议层不做任何伪造/绕过。
	MintToken func(ctx context.Context, sitekey string) (string, error)

	// WaitCode 返回 Clerk 发到邮箱的 6 位验证码。
	WaitCode func(ctx context.Context) (string, error)
	Log      func(format string, a ...any)
}

type Result struct {
	AuthJSON map[string]any `json:"auth_json"`
	// SessionToken 是 Clerk 会话 JWT，绑卡优惠等站点接口用它做 Bearer。
	SessionToken string
	UserID       string
}

func (in Input) logf(format string, a ...any) {
	if in.Log != nil {
		in.Log(format, a...)
	}
}

// Register 走 Higgsfield 的邮箱注册：
// Clerk 环境 → Turnstile token → sign_ups → 发码 → 验码 → 会话 JWT → 校验账号可用。
func Register(ctx context.Context, in Input) (*Result, error) {
	if in.Email == "" {
		return nil, fmt.Errorf("缺少邮箱")
	}
	if in.WaitCode == nil {
		return nil, fmt.Errorf("缺少验证码回调")
	}
	if in.MintToken == nil {
		return nil, fmt.Errorf("缺少 Turnstile token 来源")
	}
	if in.Password == "" {
		in.Password = GenPassword(16)
	}

	c, err := newClient(in.Proxy)
	if err != nil {
		return nil, fmt.Errorf("创建 HTTP 客户端失败: %w", err)
	}

	env, err := c.environment(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取 Clerk 环境配置失败: %w", err)
	}
	widget := env.UserSettings.SignUp.CaptchaWidgetType
	if widget == "" {
		widget = env.DisplayConfig.CaptchaWidgetType
	}
	sitekey := env.DisplayConfig.CaptchaPublicKey
	if widget == "invisible" && env.DisplayConfig.CaptchaPublicKeyInvisibl != "" {
		sitekey = env.DisplayConfig.CaptchaPublicKeyInvisibl
	}
	in.logf("Clerk 环境已就绪：人机校验=%v(%s/%s)", env.UserSettings.SignUp.CaptchaEnabled,
		env.DisplayConfig.CaptchaProvider, widget)

	attempt, err := createSignUp(ctx, c, in, env, sitekey, widget)
	if err != nil {
		return nil, err
	}

	if needsEmailCode(attempt) {
		in.logf("注册尝试已创建，请求发送邮箱验证码")
		if attempt, err = c.prepareVerification(ctx, attempt.ID); err != nil {
			return nil, fmt.Errorf("请求发送验证码失败: %w", err)
		}
		code, cerr := in.WaitCode(ctx)
		if cerr != nil {
			return nil, fmt.Errorf("获取邮箱验证码失败: %w", cerr)
		}
		in.logf("已取到验证码，提交校验")
		if attempt, err = c.attemptVerification(ctx, attempt.ID, code); err != nil {
			return nil, fmt.Errorf("验证码校验失败: %w", err)
		}
	}
	if attempt.Status != "complete" || attempt.CreatedSessionID == "" {
		return nil, fmt.Errorf("注册未完成：status=%s，仍缺少 %s",
			attempt.Status, strings.Join(append(attempt.MissingFields, attempt.UnverifiedFields...), ","))
	}

	token, err := c.sessionToken(ctx, attempt.CreatedSessionID)
	if err != nil {
		return nil, fmt.Errorf("获取会话 token 失败: %w", err)
	}
	in.logf("注册完成，已拿到 Clerk 会话")

	// 会话自检：Clerk 侧必须确实有一条活的会话，否则不算注册成功。
	if err = verifySession(ctx, c, attempt.CreatedSessionID); err != nil {
		return nil, fmt.Errorf("会话自检失败: %w", err)
	}
	in.logf("会话自检通过，账号可用")
	// 再用会话 JWT 打一次站点业务网关，能读到用户就顺带记一笔（网关偶发不可用不影响注册结果）。
	if err = checkSiteUser(ctx, c, token); err != nil {
		in.logf("站点用户接口自检未通过（不影响注册结果）: %v", err)
	}

	cookies := c.cookies()
	// __session 是站点域下的会话 cookie（clerk-js 用 JWT 写入），导出后浏览器可直接用。
	cookies = append(cookies, map[string]any{
		"name":     "__session",
		"value":    token,
		"domain":   ".higgsfield.ai",
		"path":     "/",
		"secure":   true,
		"httpOnly": false,
	})

	return &Result{
		SessionToken: token,
		UserID:       attempt.CreatedUserID,
		AuthJSON: map[string]any{
			"auth_mode":     "higgsfield_clerk_session",
			"platform":      "higgsfield",
			"email":         in.Email,
			"password":      in.Password,
			"user_id":       attempt.CreatedUserID,
			"session_id":    attempt.CreatedSessionID,
			"session_token": token,
			"captured_at":   time.Now().UTC().Format(time.RFC3339),
			"cookies":       cookies,
		},
	}, nil
}

// createSignUp 带 Turnstile token 创建注册尝试；token 被拒时换一枚重试。
func createSignUp(ctx context.Context, c *client, in Input, env *clerkEnv, sitekey, widget string) (*signUpAttempt, error) {
	if !env.UserSettings.SignUp.CaptchaEnabled {
		return c.signUp(ctx, in.Email, in.Password, "", "")
	}
	var lastErr error
	for i := 0; i < captchaAttempts; i++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		in.logf("获取 Turnstile token（第 %d 次）", i+1)
		token, err := in.MintToken(ctx, sitekey)
		if err != nil {
			lastErr = fmt.Errorf("%w: %v", ErrCaptcha, err)
			continue
		}
		attempt, err := c.signUp(ctx, in.Email, in.Password, token, widget)
		if err == nil {
			return attempt, nil
		}
		var ce *clerkErr
		if errors.As(err, &ce) {
			switch ce.Code {
			case "form_identifier_exists", "form_identifier_exists_verified_email_address":
				return nil, fmt.Errorf("%w（%s）", ErrEmailTaken, trimText(ce.Msg, 120))
			case "captcha_invalid", "captcha_missing_token", "captcha_unavailable":
				lastErr = fmt.Errorf("%w: %v", ErrCaptcha, err)
				in.logf("Clerk 拒绝了这枚 Turnstile token（%s），换一枚重试", ce.Code)
				continue
			}
		}
		return nil, fmt.Errorf("创建注册尝试失败: %w", err)
	}
	return nil, lastErr
}

// needsEmailCode 判断注册尝试是否还需要走邮箱验证码。
func needsEmailCode(a *signUpAttempt) bool {
	if a.Status == "complete" && a.CreatedSessionID != "" {
		return false
	}
	for _, f := range a.UnverifiedFields {
		if f == "email_address" {
			return true
		}
	}
	return a.Status != "complete"
}

// verifySession 确认 Clerk 侧真有这条活会话，避免把半成品当成注册成功。
func verifySession(ctx context.Context, c *client, sessionID string) error {
	sessions, _, err := c.clientSessions(ctx)
	if err != nil {
		return err
	}
	for _, s := range sessions {
		if s.ID == sessionID && s.Status == "active" {
			return nil
		}
	}
	return fmt.Errorf("Clerk 没有活跃会话 %s", sessionID)
}

// checkSiteUser 用会话 JWT 读一次站点用户信息。
func checkSiteUser(ctx context.Context, c *client, token string) error {
	res, err := c.apiRequest(ctx, http.MethodGet, "/user", nil, token)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("站点用户接口返回 %d", res.StatusCode)
	}
	return nil
}

func ri(n int) int {
	if n <= 0 {
		return 0
	}
	v, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

// GenPassword 生成满足 Clerk 密码强度（>=8 位、含大小写与数字）的随机密码。
func GenPassword(n int) string {
	const lower = "abcdefghijkmnpqrstuvwxyz"
	const upper = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	const digit = "23456789"
	all := lower + upper + digit
	if n < 12 {
		n = 12
	}
	b := make([]byte, n)
	b[0] = upper[ri(len(upper))]
	b[1] = lower[ri(len(lower))]
	b[2] = digit[ri(len(digit))]
	for i := 3; i < n; i++ {
		b[i] = all[ri(len(all))]
	}
	return string(b)
}
