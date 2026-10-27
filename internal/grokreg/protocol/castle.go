package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

type castleWorker struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Scanner
}

func castleScript() (string, error) {
	exe, _ := os.Executable()
	for _, p := range []string{filepath.Join(filepath.Dir(exe), "scripts", "grok_castle.cjs"), "/usr/local/share/chatgpt-register/scripts/grok_castle.cjs", "scripts/grok_castle.cjs"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("缺少 scripts/grok_castle.cjs")
}

func (c *Client) StartCastle(ctx context.Context, cfg SignupConfig) error {
	path, err := castleScript()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "node", "--max-old-space-size=192", path)
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("启动 Castle SDK 失败，请检查 Node.js: %w", err)
	}
	w := &castleWorker{cmd: cmd, in: in, out: bufio.NewScanner(out)}
	w.out.Buffer(make([]byte, 8192), 1<<20)
	c.castle = w
	var cookies []string
	for _, ck := range c.sess.Cookies(SiteURL) {
		cookies = append(cookies, ck.Name+"="+ck.Value)
	}
	result, err := w.call(ctx, map[string]any{"type": "init", "source": cfg.CastleScript, "sdkURL": cfg.CastleURL, "pk": cfg.CastlePK, "url": SignupURLGrok, "userAgent": c.ua, "cookies": strings.Join(cookies, "; ")})
	if err == nil && result["type"] != "ready" {
		err = fmt.Errorf("Castle SDK 初始化响应无效")
	}
	if err != nil {
		c.CloseCastle()
	}
	return err
}

func (c *Client) CloseCastle() {
	if c.castle == nil {
		return
	}
	c.castle.in.Close()
	_ = c.castle.cmd.Process.Kill()
	_ = c.castle.cmd.Wait()
	c.castle = nil
}

func (w *castleWorker) call(ctx context.Context, input map[string]any) (map[string]string, error) {
	if err := json.NewEncoder(w.in).Encode(input); err != nil {
		return nil, fmt.Errorf("Castle SDK 输入失败")
	}
	done := make(chan map[string]string, 1)
	go func() {
		var result map[string]string
		if w.out.Scan() {
			_ = json.Unmarshal(w.out.Bytes(), &result)
		}
		done <- result
	}()
	select {
	case <-ctx.Done():
		_ = w.cmd.Process.Kill()
		return nil, ctx.Err()
	case <-time.After(20 * time.Second):
		_ = w.cmd.Process.Kill()
		return nil, fmt.Errorf("Castle SDK 计算超时")
	case result := <-done:
		if result == nil || result["type"] == "error" {
			return nil, fmt.Errorf("Castle SDK 计算失败")
		}
		return result, nil
	}
}

func (c *Client) castleToken(ctx context.Context) (string, error) {
	if c.castle == nil {
		return "", fmt.Errorf("Castle SDK 未初始化")
	}
	r, err := c.castle.call(ctx, map[string]any{"type": "mint"})
	if err != nil {
		return "", err
	}
	if r["type"] != "token" || len(r["token"]) < 100 {
		return "", fmt.Errorf("Castle SDK 未返回有效令牌")
	}
	var cookies []*http.Cookie
	for _, part := range strings.Split(r["cookies"], ";") {
		if k, v, ok := strings.Cut(strings.TrimSpace(part), "="); ok && k != "" {
			cookies = append(cookies, &http.Cookie{Name: k, Value: v, Domain: "accounts.x.ai", Path: "/", Secure: true})
		}
	}
	c.sess.SetCookies(SiteURL, cookies)
	return r["token"], nil
}
