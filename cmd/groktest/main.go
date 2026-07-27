// Command groktest runs one headless Grok registration with a single mailbox.
//
//	usage: groktest <config.json>
//	config: {"email","client_id","refresh_token","proxy","headless"}
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"chatgpt-register/internal/grokreg"
	"chatgpt-register/internal/mailfetch"
)

type cfg struct {
	Email        string `json:"email"`
	ClientID     string `json:"client_id"`
	RefreshToken string `json:"refresh_token"`
	Proxy        string `json:"proxy"`
	Headless     bool   `json:"headless"`
}

var (
	digitCodeRe = regexp.MustCompile(`\b(\d{6})\b`)
	xaiCodeRe   = regexp.MustCompile(`(?i)\b([a-z0-9]{3})-([a-z0-9]{3})\b`)
)

func main() {
	path := "groktest.json"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		fmt.Println("读取配置失败:", err)
		os.Exit(1)
	}
	var c cfg
	if err := json.Unmarshal(raw, &c); err != nil {
		fmt.Println("解析配置失败:", err)
		os.Exit(1)
	}
	if c.Email == "" || c.ClientID == "" || c.RefreshToken == "" {
		fmt.Println("配置缺少 email / client_id / refresh_token")
		os.Exit(1)
	}

	mail := mailfetch.New()
	acc := mailfetch.Account{Email: c.Email, ClientID: c.ClientID, RefreshToken: c.RefreshToken}
	since := time.Now().Add(-30 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	in := grokreg.Input{
		Email:    c.Email,
		Proxy:    c.Proxy,
		Headless: c.Headless,
		Log: func(f string, a ...any) {
			fmt.Println(time.Now().Format("15:04:05"), fmt.Sprintf(f, a...))
		},
		WaitCode: func(ctx context.Context) (string, error) {
			deadline := time.Now().Add(4 * time.Minute)
			fmt.Println(time.Now().Format("15:04:05"), "开始自动读取 Grok 邮件验证码")
			for time.Now().Before(deadline) {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				msgs, err := mail.ListMessages(ctx, acc, 15)
				if err != nil {
					fmt.Println(time.Now().Format("15:04:05"), "读邮件失败:", err)
					time.Sleep(5 * time.Second)
					continue
				}
				for _, m := range msgs {
					if m.ReceivedAt.Before(since) || !looksLikeGrok(m) {
						continue
					}
					if code := extractGrokCode(m.Subject); code != "" {
						fmt.Println(time.Now().Format("15:04:05"), "从标题读到验证码")
						return code, nil
					}
					full, gerr := mail.GetMessage(ctx, acc, m.ID)
					if gerr != nil {
						continue
					}
					if code := extractGrokCode(full.Subject + " " + full.Text); code != "" {
						fmt.Println(time.Now().Format("15:04:05"), "从正文读到验证码")
						return code, nil
					}
				}
				time.Sleep(5 * time.Second)
			}
			return "", fmt.Errorf("超时未收到 Grok 验证码邮件")
		},
		SaveShot: func(png []byte) {
			out := "groktest-fail.png"
			if err := os.WriteFile(out, png, 0o644); err == nil {
				fmt.Println(time.Now().Format("15:04:05"), "失败截图已保存:", out)
			}
		},
	}

	res, err := grokreg.Register(ctx, in)
	if err != nil {
		fmt.Println("RESULT: FAIL ->", err)
		os.Exit(2)
	}
	b, _ := json.MarshalIndent(res.AuthJSON, "", "  ")
	_ = os.WriteFile("groktest-auth.json", b, 0o644)
	fmt.Println("RESULT: OK email=", c.Email)
	fmt.Println("会话已写入 groktest-auth.json")
}

func extractGrokCode(s string) string {
	if code := xaiCodeRe.FindStringSubmatch(s); code != nil {
		return strings.ToUpper(code[1] + code[2])
	}
	if code := digitCodeRe.FindStringSubmatch(s); code != nil {
		return code[1]
	}
	return ""
}

func looksLikeGrok(m mailfetch.Message) bool {
	s := strings.ToLower(m.From + " " + m.FromName + " " + m.Subject)
	return strings.Contains(s, "grok") ||
		strings.Contains(s, "x.ai") ||
		strings.Contains(s, "xai") ||
		strings.Contains(s, "security code") ||
		strings.Contains(s, "verification")
}
