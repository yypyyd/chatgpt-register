// Package codexreg 用浏览器自动化注册 ChatGPT 账号并保存 access token。
// Agent Identity 注册不属于当前生产流程。
//
// 迁移自独立的 got 命令行工具：
//   - browser.go  : 打开 chatgpt.com 完成注册（邮箱→验证码→资料），提取 accessToken
//   - geoip.go    : 代理解析 + 按出口 IP 对齐时区/坐标/语言 + 资源屏蔽
//   - codex.go    : accessToken 元数据解析
//
// 与命令行版的区别：验证码不再手动 fmt.Scan，而是由调用方通过 FetchCode 回调
// 从邮箱自动读取。
package codexreg

import (
	"context"
	"fmt"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"

// Input 单个账号的生产参数。
type Input struct {
	Email    string
	Password string // 注册流程要求创建密码时使用（为空则自动生成）
	FullName string
	Age      string
	Proxy    string // 空=直连
	Headless bool

	// FetchCode 拉取 ChatGPT 发到邮箱的验证码。由 producer 用 mailfetch 实现。
	FetchCode func(ctx context.Context) (string, error)

	// Log 输出进度（可为 nil）。
	Log func(format string, a ...any)

	// SaveShot 保存注册失败时的页面截图(PNG)，用于事后排查（可为 nil）。
	SaveShot func(png []byte)
}

// Result 生产结果。
type Result struct {
	AccessToken string         `json:"-"`
	AuthJSON    map[string]any `json:"auth_json"`
	AccountID   string         `json:"account_id"`
	UserID      string         `json:"user_id"`
	PlanType    string         `json:"plan_type"`
}

func (in Input) logf(format string, a ...any) {
	if in.Log != nil {
		in.Log(format, a...)
	}
}

// Register 完整生产一个账号：浏览器注册 ChatGPT → 获取并保存 accessToken。
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

	accessToken, err := registerBrowser(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("ChatGPT 注册失败: %w", err)
	}

	res, claimsErr := buildChatGPTResult(accessToken, in.Email)
	if claimsErr != nil {
		// Account creation has already succeeded. Claim decoding is metadata-only
		// and must not turn a successful registration into a failure.
		in.logf("⚠️ accessToken 元数据解析失败，仍按注册成功处理: %v", claimsErr)
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
