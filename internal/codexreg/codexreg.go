// Package codexreg 用纯协议注册 ChatGPT 账号并保存可恢复的网页会话。
// Agent Identity 注册不属于当前生产流程。
package codexreg

import (
	"context"
	"fmt"
	"time"
)

// geoLookupUserAgent 仅用于 Go 侧查询 ip-api 的请求头。
const geoLookupUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

// Input 单个账号的生产参数。
type Input struct {
	Email    string
	Password string // 注册流程要求创建密码时使用（为空则自动生成）
	FullName string
	Age      string
	Proxy    string // 空=直连

	// FetchCode 拉取 ChatGPT 发到邮箱的验证码。由 producer 用 mailfetch 实现。
	// after 非零时只接受该时刻之后收到的邮件，用于重发验证码后避免抓回旧码。
	FetchCode func(ctx context.Context, after time.Time) (string, error)

	// Log 输出进度（可为 nil）。
	Log func(format string, a ...any)
}

// Result 生产结果。
type Result struct {
	AccessToken string         `json:"-"`
	AuthJSON    map[string]any `json:"auth_json"`
	AccountID   string         `json:"account_id"`
	UserID      string         `json:"user_id"`
	PlanType    string         `json:"plan_type"`
	UserAgent   string         `json:"user_agent"`
	EgressIP    string         `json:"egress_ip"`
	Country     string         `json:"country"`
}

func (in Input) logf(format string, a ...any) {
	if in.Log != nil {
		in.Log(format, a...)
	}
}

// Register 纯协议生产一个 ChatGPT 账号并保存 Cookie 与 accessToken。
func Register(ctx context.Context, in Input) (*Result, error) {
	if in.FetchCode == nil {
		return nil, fmt.Errorf("缺少 FetchCode 回调，无法自动读取验证码")
	}
	if in.FullName == "" {
		in.FullName = genName()
	}
	if in.Age == "" {
		in.Age = genAge()
	}
	if in.Password == "" {
		in.Password = GenPassword(16)
	}

	br, err := registerProtocol(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("ChatGPT 注册失败: %w", err)
	}

	res, claimsErr := buildChatGPTResult(br.AccessToken, in.Email)
	if claimsErr != nil {
		in.logf("⚠️ accessToken 元数据解析失败，仍按注册成功处理: %v", claimsErr)
	}
	res.UserAgent, res.EgressIP, res.Country = br.UserAgent, br.EgressIP, br.Country
	res.AuthJSON["user_agent"] = br.UserAgent
	res.AuthJSON["screen"] = br.Screen
	res.AuthJSON["registered_ip"] = br.EgressIP
	res.AuthJSON["registered_country"] = br.Country
	res.AuthJSON["registered_timezone"] = br.Timezone
	res.AuthJSON["registered_locale"] = br.Locale
	res.AuthJSON["registered_languages"] = br.Languages
	res.AuthJSON["registered_at"] = time.Now().UTC().Format(time.RFC3339)
	res.AuthJSON["session_type"] = "chatgpt_web"
	res.AuthJSON["register_engine"] = "protocol"
	res.AuthJSON["cookies"] = br.Cookies
	if in.Proxy != "" {
		res.AuthJSON["proxy"] = in.Proxy
	}
	in.logf("✅ ChatGPT 注册完成（已跳过 Agent Identity）")
	return res, nil
}

func buildChatGPTResult(accessToken, fallbackEmail string) (*Result, error) {
	res := &Result{AccessToken: accessToken}
	res.AuthJSON = map[string]any{
		"auth_mode":    "chatgpt",
		"access_token": accessToken,
		"email":        fallbackEmail,
	}

	accountID, userID, email, planType, err := decodeJWTClaims(accessToken)
	if err != nil {
		return res, err
	}
	if email == "" {
		email = fallbackEmail
	}
	res.AccountID = accountID
	res.UserID = userID
	res.PlanType = planType
	res.AuthJSON["account_id"] = accountID
	res.AuthJSON["chatgpt_user_id"] = userID
	res.AuthJSON["email"] = email
	res.AuthJSON["plan_type"] = planType
	return res, nil
}
