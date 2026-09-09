package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"chatgpt-register/internal/mailfetch"
)

var codeRe = regexp.MustCompile(`(\d{6})`)

// makeCodeFetcher 优先用 mailfetch 自动收码（PROBE_CLIENT_ID + PROBE_REFRESH_TOKEN + PROBE_MAILBOX），
// 未配置时回退到终端手动输入。
func makeCodeFetcher() func(context.Context, time.Time) (string, error) {
	clientID := os.Getenv("PROBE_CLIENT_ID")
	refreshToken := os.Getenv("PROBE_REFRESH_TOKEN")
	mailbox := os.Getenv("PROBE_MAILBOX")
	if clientID == "" || refreshToken == "" || mailbox == "" {
		fmt.Println("[code] 未配置 PROBE_CLIENT_ID/REFRESH_TOKEN/MAILBOX，验证码需手动输入")
		return manualCode
	}
	fmt.Printf("[code] 自动收码：邮箱 %s\n", mailbox)
	mc := mailfetch.New()
	acc := mailfetch.Account{Email: mailbox, ClientID: clientID, RefreshToken: refreshToken}
	return func(ctx context.Context, after time.Time) (string, error) {
		return pollCode(ctx, mc, acc, after)
	}
}

func manualCode(ctx context.Context, after time.Time) (string, error) {
	fmt.Print("[code] 验证码（6 位）: ")
	sc := bufio.NewScanner(os.Stdin)
	if sc.Scan() {
		return strings.TrimSpace(sc.Text()), nil
	}
	return "", fmt.Errorf("读取验证码失败")
}

func pollCode(ctx context.Context, mc *mailfetch.Client, acc mailfetch.Account, after time.Time) (string, error) {
	deadline := time.Now().Add(3 * time.Minute)
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		msgs, err := mc.ListMessages(ctx, acc, 15)
		if err != nil {
			lastErr = err
			fmt.Printf("[code] 读邮箱出错 (temporary=%v): %v\n", errors.Is(err, mailfetch.ErrAuthTemporary), err)
		} else {
			best, bestAt := "", time.Time{}
			fmt.Printf("[code] 列出 %d 封邮件 (after=%s)\n", len(msgs), after.Format(time.RFC3339))
			for _, m := range msgs {
				fmt.Printf("[code]   %s | %s | %s\n", m.ReceivedAt.Format(time.RFC3339), m.From, m.Subject)
				if !after.IsZero() && m.ReceivedAt.Before(after) {
					continue
				}
				if !looksOpenAI(m) {
					continue
				}
				code := ""
				if c := codeRe.FindStringSubmatch(m.Subject); c != nil {
					code = c[1]
				} else if full, gerr := mc.GetMessage(ctx, acc, m.ID); gerr == nil {
					if c := codeRe.FindStringSubmatch(full.Subject + " " + full.Text); c != nil {
						code = c[1]
					} else {
						fmt.Printf("[code]   正文无验证码 textLen=%d\n", len(full.Text))
					}
				} else {
					fmt.Printf("[code]   GetMessage 失败: %v\n", gerr)
				}
				if code != "" && (best == "" || m.ReceivedAt.After(bestAt)) {
					best, bestAt = code, m.ReceivedAt
				}
			}
			if best != "" {
				fmt.Printf("[code] 收到验证码 %s\n", best)
				return best, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("超时未收到验证码（最近错误: %v）", lastErr)
	}
	return "", fmt.Errorf("超时未收到验证码")
}

func looksOpenAI(m mailfetch.Message) bool {
	s := strings.ToLower(m.From + " " + m.FromName + " " + m.Subject)
	return strings.Contains(s, "openai") || strings.Contains(s, "chatgpt")
}
