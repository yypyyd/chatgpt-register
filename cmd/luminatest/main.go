// luminatest 用生产环境同一条协议链路，对指定邮箱做一次真实的 Lumina 注册，
// 用来线上验证限流到底按什么维度计。用法：
//
//	luminatest -db /opt/chatgpt-register/adskull.db -email a@b.com [-proxy 'http://...'] [-save]
//
// 只读取邮箱凭据与代理设置；带 -save 且注册成功时才把结果写回 lumina_registrations。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"chatgpt-register/internal/luminareg"
	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"
	"chatgpt-register/internal/proxyutil"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

var digitCodeRe = regexp.MustCompile(`\b(\d{6})\b`)

func main() {
	dbPath := flag.String("db", "/opt/chatgpt-register/adskull.db", "sqlite 路径")
	email := flag.String("email", "", "要注册的邮箱（必须在 mailboxes 表里）")
	proxy := flag.String("proxy", "", "代理；留空则按系统设置 proxy_list 取（自动挂 bestgo session）；'direct' 表示直连")
	save := flag.Bool("save", false, "注册成功时写回 lumina_registrations")
	flag.Parse()
	if *email == "" {
		log.Fatal("缺少 -email")
	}

	db, err := gorm.Open(sqlite.Open("file:"+*dbPath+"?_pragma=busy_timeout(30000)"), &gorm.Config{})
	if err != nil {
		log.Fatal(err)
	}

	px := *proxy
	switch px {
	case "direct":
		px = ""
	case "":
		var st models.Setting
		if err := db.Where("key = ?", "proxy_list").First(&st).Error; err == nil {
			if list := proxyutil.List(st.Value); len(list) > 0 {
				px = proxyutil.WithBestGoTaskSession(list[0])
			}
		}
	}
	if px != "" {
		fmt.Println("proxy:", maskProxy(px))
	} else {
		fmt.Println("proxy: direct")
	}

	var mb models.Mailbox
	if err := db.Where("email = ?", *email).First(&mb).Error; err != nil {
		log.Fatalf("mailboxes 里没有 %s: %v", *email, err)
	}
	var prior models.LuminaRegistration
	if err := db.Where("email = ?", *email).First(&prior).Error; err == nil {
		fmt.Printf("prior record: #%d status=%s note=%s\n", prior.ID, prior.Status, trunc(prior.Note, 80))
	} else {
		fmt.Println("prior record: none (never attempted)")
	}

	mail := mailfetch.New()
	acc := mailfetch.Account{Email: mb.Email, ClientID: mb.ClientID, RefreshToken: mb.RefreshToken}
	since := time.Now().Add(-30 * time.Second)
	password := luminareg.GenPassword(16)
	logf := func(f string, a ...any) {
		fmt.Printf("%s %s\n", time.Now().UTC().Format("15:04:05"), fmt.Sprintf(f, a...))
	}
	in := luminareg.Input{
		Email:    mb.Email,
		Password: password,
		Proxy:    px,
		Log:      logf,
		WaitCode: func(ctx context.Context) (string, error) {
			deadline := time.Now().Add(4 * time.Minute)
			for time.Now().Before(deadline) {
				msgs, err := mail.ListMessages(ctx, acc, 15)
				if err != nil {
					logf("读取邮件失败，重试: %v", err)
				}
				for _, m := range msgs {
					if m.ReceivedAt.Before(since) || !looksLikeBytePlus(m) {
						continue
					}
					if c := extractCode(m.Subject); c != "" {
						return c, nil
					}
					full, gerr := mail.GetMessage(ctx, acc, m.ID)
					if gerr != nil {
						continue
					}
					if c := extractCode(full.Subject + " " + full.Text); c != "" {
						return c, nil
					}
				}
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(2 * time.Second):
				}
			}
			return "", fmt.Errorf("超时未收到验证码")
		},
	}

	start := time.Now()
	res, err := luminareg.Register(context.Background(), in)
	fmt.Printf("\n=== RESULT for %s after %.0fs ===\n", mb.Email, time.Since(start).Seconds())
	switch {
	case err == nil:
		fmt.Println("SUCCESS")
	case errors.Is(err, luminareg.ErrRateLimited):
		fmt.Println("RATE_LIMITED:", err)
	default:
		fmt.Println("FAILED:", err)
	}
	if err == nil && *save {
		authBytes, _ := json.MarshalIndent(res.AuthJSON, "", "  ")
		row := models.LuminaRegistration{
			Email: mb.Email, MailboxID: mb.ID, Password: password,
			Status: "registered", AuthData: string(authBytes), Note: "来源: 线上限流验证测试（luminatest）",
		}
		if prior.ID != 0 {
			row.ID = prior.ID
			row.Log = prior.Log
		}
		row.Log += time.Now().Format("2006-01-02 15:04:05") + " luminatest 手动验证注册成功\n"
		if err := db.Save(&row).Error; err != nil {
			fmt.Println("写回数据库失败:", err)
		} else {
			fmt.Printf("已写回 lumina_registrations #%d\n", row.ID)
		}
	}
}

func extractCode(s string) string {
	if m := digitCodeRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

func looksLikeBytePlus(m mailfetch.Message) bool {
	s := strings.ToLower(m.From + " " + m.FromName + " " + m.Subject)
	if strings.Contains(s, "openai") || strings.Contains(s, "chatgpt") {
		return false
	}
	return strings.Contains(s, "byteplus") || strings.Contains(s, "volcengine") ||
		strings.Contains(s, "verification code") || strings.Contains(s, "confirmation code") ||
		strings.Contains(s, "verify your") || strings.Contains(s, "one-time")
}

func maskProxy(p string) string {
	if i := strings.Index(p, "@"); i > 0 {
		if j := strings.Index(p, "://"); j > 0 {
			return p[:j+3] + "***@" + p[i+1:]
		}
	}
	return p
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
