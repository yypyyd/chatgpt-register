package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"chatgpt-register/internal/codexreg"
)

var (
	replay   = flag.Bool("replay", false, "重放模式")
	proto_   = flag.Bool("proto", false, "纯协议探针模式")
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

func capture() error {
	email := os.Getenv("PROBE_EMAIL")
	if email == "" {
		return fmt.Errorf("缺少 PROBE_EMAIL")
	}
	in := codexreg.Input{
		Email:     email,
		Password:  os.Getenv("PROBE_PASSWORD"),
		Proxy:     os.Getenv("PROXY_URL"),
		FetchCode: makeCodeFetcher(),
		Log: func(f string, a ...any) {
			fmt.Printf("[reg] "+f+"\n", a...)
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	res, err := codexreg.Register(ctx, in)
	if err != nil {
		return err
	}
	aj, _ := json.Marshal(res.AuthJSON)
	fmt.Printf("[reg] 成功 account=%s user=%s plan=%s\n%s\n", res.AccountID, res.UserID, res.PlanType, trunc(string(aj), 600))
	return nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
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

// replayFlow 读取抓包文件，用 Chrome TLS 指纹客户端离线重放注册请求。
func replayFlow() error {
	return replayOpenAI(*dumpFile)
}
