package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"chatgpt-register/internal/codexreg"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

var (
	replay   = flag.Bool("replay", false, "重放模式")
	proto_   = flag.Bool("proto", false, "纯协议探针模式（不开浏览器）")
	dumpFile = flag.String("dump", "requests.jsonl", "抓包文件")
)

func main() {
	flag.Parse()
	if *proto_ {
		if err := protoProbe(); err != nil {
			fmt.Fprintln(os.Stderr, "proto 失败:", err)
			os.Exit(1)
		}
		return
	}
	if *replay {
		if err := replayFlow(); err != nil {
			fmt.Fprintln(os.Stderr, "replay 失败:", err)
			os.Exit(1)
		}
		return
	}
	if err := capture(); err != nil {
		fmt.Fprintln(os.Stderr, "capture 失败:", err)
		os.Exit(1)
	}
}

type rec struct {
	TS      string            `json:"ts"`
	Kind    string            `json:"kind"`
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Status  int               `json:"status,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

var interesting = []string{
	"auth.openai.com",
	"auth0.openai.com",
	"chatgpt.com/api",
	"chatgpt.com/backend-api",
	"challenges.cloudflare.com",
	"sentinel.openai.com",
}

func want(u string) bool {
	if strings.Contains(u, "/awe/api/v2/rum") || strings.Contains(u, "/ces/") {
		return false
	}
	for _, k := range interesting {
		if strings.Contains(u, k) {
			return true
		}
	}
	return false
}

func capture() error {
	email := os.Getenv("PROBE_EMAIL")
	if email == "" {
		return fmt.Errorf("缺少 PROBE_EMAIL")
	}
	f, err := os.Create(*dumpFile)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()
	var mu sync.Mutex
	emit := func(r rec) {
		b, _ := json.Marshal(r)
		mu.Lock()
		w.Write(b)
		w.WriteByte('\n')
		w.Flush()
		mu.Unlock()
	}

	hook := func(page *rod.Page) {
		_ = proto.NetworkEnable{}.Call(page)
		_ = proto.NetworkSetCacheDisabled{CacheDisabled: true}.Call(page)
		go page.EachEvent(func(e *proto.NetworkRequestWillBeSent) {
			u := e.Request.URL
			if !want(u) {
				return
			}
			if e.RedirectResponse != nil {
				rh := map[string]string{}
				for k, v := range e.RedirectResponse.Headers {
					rh[strings.ToLower(fmt.Sprint(k))] = fmt.Sprint(v)
				}
				emit(rec{TS: time.Now().UTC().Format(time.RFC3339Nano), Kind: "redirect",
					URL: e.RedirectResponse.URL, Status: e.RedirectResponse.Status, Headers: rh})
			}
			h := map[string]string{}
			for k, v := range e.Request.Headers {
				h[strings.ToLower(fmt.Sprint(k))] = fmt.Sprint(v)
			}
			emit(rec{TS: time.Now().UTC().Format(time.RFC3339Nano), Kind: "request",
				Method: string(e.Request.Method), URL: u, Headers: h, Body: e.Request.PostData})
		}, func(e *proto.NetworkResponseReceived) {
			u := e.Response.URL
			if !want(u) {
				return
			}
			h := map[string]string{}
			for k, v := range e.Response.Headers {
				h[strings.ToLower(fmt.Sprint(k))] = fmt.Sprint(v)
			}
			body := ""
			mt := string(e.Type)
			limit := 8192
			if mt == "XHR" || mt == "Fetch" || strings.Contains(u, "/api/") || strings.Contains(e.Response.MIMEType, "json") {
				if rb, err := (proto.NetworkGetResponseBody{RequestID: e.RequestID}).Call(page); err == nil {
					body = rb.Body
				}
				limit = 1 << 20
			} else if mt == "Script" || mt == "Document" {
				// 脚本/页面全文单独落盘，供离线分析 Sentinel SDK。
				go func(id proto.NetworkRequestID, u string) {
					time.Sleep(500 * time.Millisecond)
					if rb, err := (proto.NetworkGetResponseBody{RequestID: id}).Call(page); err == nil {
						name := fmt.Sprintf("body_%d_%s.txt", time.Now().UnixNano(), sanitize(u))
						_ = os.WriteFile(name, []byte(rb.Body), 0o644)
					}
				}(e.RequestID, u)
			}
			emit(rec{TS: time.Now().UTC().Format(time.RFC3339Nano), Kind: "response",
				URL: u, Status: e.Response.Status, Headers: h, Body: trunc(body, limit)})
		})()
	}

	in := codexreg.Input{
		Email:     email,
		Password:  os.Getenv("PROBE_PASSWORD"),
		Proxy:     os.Getenv("PROXY_URL"),
		Headless:  true,
		Engine:    "browser",
		PageHook:  hook,
		FetchCode: makeCodeFetcher(),
		Log: func(f string, a ...any) {
			fmt.Printf("[reg] "+f+"\n", a...)
		},
		SaveShot: func(png []byte) {
			_ = os.WriteFile("probe_fail.png", png, 0o644)
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	res, err := codexreg.Register(ctx, in)
	if err != nil {
		return err
	}
	aj, _ := json.Marshal(res.AuthJSON)
	fmt.Printf("[reg] 成功 account=%s user=%s plan=%s\n%s\n", res.AccountID, res.UserID, res.PlanType, trunc(string(aj), 600))
	return nil
}

func sanitize(u string) string {
	if i := strings.Index(u, "?"); i >= 0 {
		u = u[:i]
	}
	u = strings.TrimPrefix(u, "https://")
	var b strings.Builder
	for _, r := range u {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return trunc(b.String(), 120)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// replayFlow 读取抓包文件，用 Chrome TLS 指纹客户端离线重放注册请求。
// 目的：验证去掉浏览器后，auth.openai.com 是否仍接受同样的请求序列。
func replayFlow() error {
	return replayOpenAI(*dumpFile)
}
