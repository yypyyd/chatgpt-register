// Command regtest 用单个指定邮箱跑一遍完整生产流程（代理→GeoIP→ChatGPT注册→读码→Codex身份），
// 便于端到端验证，不走 producer 的邮箱选择逻辑。凭据从 JSON 文件读取，避免写死。
//
//	用法: regtest <config.json>
//	config: {"email","client_id","refresh_token","proxy","headless"}
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"chatgpt-register/internal/codexreg"
	"chatgpt-register/internal/mailfetch"
)

type cfg struct {
	Email        string `json:"email"`
	ClientID     string `json:"client_id"`
	RefreshToken string `json:"refresh_token"`
	Proxy        string `json:"proxy"`
	Headless     bool   `json:"headless"`
}

var codeRe = regexp.MustCompile(`\b(\d{6})\b`)

func main() {
	path := "regtest.json"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("读取配置失败:", err)
		os.Exit(1)
	}
	var c cfg
	if err := json.Unmarshal(raw, &c); err != nil {
		fmt.Println("解析配置失败:", err)
		os.Exit(1)
	}

	mail := mailfetch.New()
	acc := mailfetch.Account{Email: c.Email, ClientID: c.ClientID, RefreshToken: c.RefreshToken}
	since := time.Now().Add(-30 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	in := codexreg.Input{
		Email:    c.Email,
		Proxy:    c.Proxy,
		Headless: c.Headless,
		Log:      func(f string, a ...any) { fmt.Println("LOG:", fmt.Sprintf(f, a...)) },
		FetchCode: func(ctx context.Context, after time.Time) (string, error) {
			from := since
			if after.After(from) {
				from = after
			}
			deadline := time.Now().Add(3 * time.Minute)
			for time.Now().Before(deadline) {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				msgs, err := mail.ListMessages(ctx, acc, 15)
				if err == nil {
					for _, m := range msgs {
						if m.ReceivedAt.Before(from) {
							continue
						}
						s := strings.ToLower(m.From + " " + m.FromName + " " + m.Subject)
						if !strings.Contains(s, "openai") && !strings.Contains(s, "chatgpt") {
							continue
						}
						if code := codeRe.FindStringSubmatch(m.Subject); code != nil {
							return code[1], nil
						}
						full, gerr := mail.GetMessage(ctx, acc, m.ID)
						if gerr != nil {
							continue
						}
						if code := codeRe.FindStringSubmatch(full.Subject + " " + full.Text); code != nil {
							return code[1], nil
						}
					}
				}
				time.Sleep(5 * time.Second)
			}
			return "", fmt.Errorf("超时未收到验证码")
		},
	}

	res, err := codexreg.Register(ctx, in)
	if err != nil {
		fmt.Println("RESULT: FAIL ->", err)
		os.Exit(2)
	}
	fmt.Printf("RESULT: OK account_id=%s user_id=%s plan=%s email=%s\n",
		res.AccountID, res.UserID, res.PlanType, c.Email)
}
