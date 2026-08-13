// Package leonardoreg 用浏览器完成 leonardo.ai 的邮箱注册，并采集站点 Cookie
// （含 better-auth 会话 cookie）供导出。
package leonardoreg

import (
	"context"
	cryptorand "crypto/rand"
	"fmt"
	"math/big"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"

type Input struct {
	Email    string
	Password string
	Proxy    string
	Headless bool

	// EgressCheck 为 true 时先打开 api.ipify.org 打印 Chromium 实际出口 IP（排障用）。
	EgressCheck bool

	// WaitCode 返回 Leonardo 发到邮箱的 6 位注册验证码。
	WaitCode func(ctx context.Context) (string, error)
	Log      func(format string, a ...any)
	SaveShot func(png []byte)
}

type Result struct {
	AuthJSON map[string]any `json:"auth_json"`
}

func (in Input) logf(format string, a ...any) {
	if in.Log != nil {
		in.Log(format, a...)
	}
}

// Register 走 Leonardo.Ai 的邮箱注册流程：
// 邮箱 + Turnstile → 创建密码 → 邮箱验证码 → 自动登录 → 采集站点 Cookie。
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

// GenPassword 生成满足 Leonardo 密码强度（>=8 位、含大小写与数字、无首尾空格）的随机密码。
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
