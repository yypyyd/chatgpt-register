package adobereg

import (
	"context"
	cryptorand "crypto/rand"
	"fmt"
	"math/big"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"

type Input struct {
	Email     string
	Password  string
	FirstName string
	LastName  string
	Proxy     string
	Headless  bool

	// WaitCode 返回 Adobe 发到邮箱的 6 位邮箱验证码（访问 Firefly 时触发）。
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

// Register 走 Adobe 账号注册流程（免费 Firefly：注册即得免费生图/生视频额度）。
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
	if in.FirstName == "" {
		in.FirstName = firstNames[ri(len(firstNames))]
	}
	if in.LastName == "" {
		in.LastName = lastNames[ri(len(lastNames))]
	}
	return registerBrowser(ctx, in)
}

var firstNames = []string{"Alex", "Jamie", "Taylor", "Jordan", "Casey", "Morgan", "Riley", "Avery", "Quinn", "Parker", "Cameron", "Reese"}
var lastNames = []string{"Ray", "Lee", "Cole", "Reed", "Hunt", "Ford", "Shaw", "Gray", "Vance", "Brooks", "Hayes", "Sloan"}

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

// GenPassword 生成满足 Adobe 密码强度（大小写+数字/符号、>=8 位）的随机密码。
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

// GenBirthYear 返回一个成年（约 22~45 岁）出生年份，避免年龄限制。
func GenBirthYear() int {
	return 1980 + ri(24) // 1980~2003
}
