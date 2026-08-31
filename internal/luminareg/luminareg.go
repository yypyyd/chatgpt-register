// Package luminareg 用浏览器完成 BytePlus Lumina（ai.byteplus.com/lumina）的邮箱注册，
// 并采集 BytePlus 账号会话 Cookie（digest / AccountID / userInfo）供导出。
package luminareg

import (
	"context"
	cryptorand "crypto/rand"
	"fmt"
	"math/big"
	"sync/atomic"
	"time"
)

type Input struct {
	Email    string
	Password string
	Proxy    string
	Headless bool

	// EgressCheck 为 true 时先打开 api.ipify.org 打印 Chromium 实际出口 IP（排障用）。
	EgressCheck bool

	// WaitCode 返回 BytePlus 发到邮箱的 6 位注册验证码。
	WaitCode func(ctx context.Context) (string, error)
	Log      func(format string, a ...any)
	SaveShot func(png []byte)

	// mediaBlock 是资源屏蔽开关：滑块阶段要真正加载验证码图，那时临时置 false。
	mediaBlock *atomic.Bool
}

type Result struct {
	AuthJSON map[string]any `json:"auth_json"`
}

func (in Input) logf(format string, a ...any) {
	if in.Log != nil {
		in.Log(format, a...)
	}
}

// Register 走 Lumina 站内的 BytePlus 注册流程：
// 邮箱 → 创建密码并同意条款 → 邮箱验证码 → 滑块验证 → 采集会话 Cookie。
func Register(ctx context.Context, in Input) (*Result, error) {
	if in.WaitCode == nil {
		return nil, fmt.Errorf("缺少验证码回调")
	}
	if in.Email == "" {
		return nil, fmt.Errorf("缺少邮箱")
	}
	if in.Password == "" {
		in.Password = GenPassword(16)
	}
	// 单次注册硬上限：页面卡死时个别 CDP 调用可能无限阻塞，再加整体超时兜底。
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	return registerBrowser(ctx, in)
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

// GenPassword 生成满足 BytePlus 密码强度（>=8 位，含大小写、数字与符号）的随机密码。
func GenPassword(n int) string {
	const lower = "abcdefghijkmnpqrstuvwxyz"
	const upper = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	const digit = "23456789"
	const symbol = "!@#$%_-"
	all := lower + upper + digit
	if n < 12 {
		n = 12
	}
	b := make([]byte, n)
	b[0] = upper[ri(len(upper))]
	b[1] = lower[ri(len(lower))]
	b[2] = digit[ri(len(digit))]
	b[3] = symbol[ri(len(symbol))]
	for i := 4; i < n; i++ {
		b[i] = all[ri(len(all))]
	}
	return string(b)
}
