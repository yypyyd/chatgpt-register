// Package codexreg 用浏览器自动化注册 ChatGPT 账号并保存可恢复的网页会话。
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
	"time"
)

// geoLookupUserAgent 仅用于 Go 侧查询 ip-api 的请求头；浏览器本身使用其真实 UA。
const geoLookupUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

// Input 单个账号的生产参数。
type Input struct {
	Email    string
	Password string // 注册流程要求创建密码时使用（为空则自动生成）
	FullName string
	Age      string
	Proxy    string // 空=直连
	Headless bool
	// BrowserBin 浏览器可执行文件，空=自动（优先本机 Chrome/Edge，其次 rod 下载的 Chromium）。见 LaunchOptions。
	BrowserBin string
	// Pool 非空时从共享 Chrome 进程池分配独立上下文，而不是为本账号独占启动一个进程。
	Pool *Pool
	// Warmup 注册成功后在同一浏览器里先发一条普通对话再取 token，让账号首次使用与注册同源。
	Warmup bool

	// FetchCode 拉取 ChatGPT 发到邮箱的验证码。由 producer 用 mailfetch 实现。
	// after 非零时只接受该时刻之后收到的邮件，用于重发验证码后避免抓回旧码。
	FetchCode func(ctx context.Context, after time.Time) (string, error)

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
	// 注册时的浏览器 / 出口信息，下游用号时尽量沿用同样的 UA 与地区。
	UserAgent string `json:"user_agent"`
	EgressIP  string `json:"egress_ip"`
	Country   string `json:"country"`
	Warmed    bool   `json:"warmed"`
}

func (in Input) logf(format string, a ...any) {
	if in.Log != nil {
		in.Log(format, a...)
	}
}

// Register 完整生产一个账号：浏览器注册 ChatGPT → 验证网页能力 → 保存 Cookie 与 accessToken。
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

	br, err := registerBrowser(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("ChatGPT 注册失败: %w", err)
	}

	res, claimsErr := buildChatGPTResult(br.AccessToken, in.Email)
	if claimsErr != nil {
		// Account creation has already succeeded. Claim decoding is metadata-only
		// and must not turn a successful registration into a failure.
		in.logf("⚠️ accessToken 元数据解析失败，仍按注册成功处理: %v", claimsErr)
	}
	res.UserAgent, res.EgressIP, res.Country, res.Warmed = br.UserAgent, br.EgressIP, br.Country, br.Warmed
	res.AuthJSON["user_agent"] = br.UserAgent
	res.AuthJSON["screen"] = br.Screen
	res.AuthJSON["registered_ip"] = br.EgressIP
	res.AuthJSON["registered_country"] = br.Country
	res.AuthJSON["registered_timezone"] = br.Timezone
	res.AuthJSON["registered_locale"] = br.Locale
	res.AuthJSON["registered_languages"] = br.Languages
	res.AuthJSON["registered_at"] = time.Now().UTC().Format(time.RFC3339)
	res.AuthJSON["warmed_up"] = br.Warmed
	res.AuthJSON["session_type"] = "chatgpt_web"
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
