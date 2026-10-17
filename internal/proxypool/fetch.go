package proxypool

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"chatgpt-register/internal/proxyutil"
)

const maxResponseBytes = 1024 * 1024

func normalizeProxy(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if strings.ContainsAny(raw, " \t\r\n") || raw == "" {
		return "", fmt.Errorf("代理格式错误")
	}
	u, err := url.Parse(proxyutil.Normalize(raw))
	if err != nil || u == nil {
		return "", fmt.Errorf("代理格式错误")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", fmt.Errorf("不支持的代理协议")
	}
	host, port, err := net.SplitHostPort(u.Host)
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || n < 1 || n > 65535 || host == "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("代理必须包含有效的主机和端口")
	}
	if u.User != nil && u.User.Username() == "" {
		return "", fmt.Errorf("代理用户名为空")
	}
	if net.ParseIP(host) == nil {
		for _, ch := range host {
			if !(ch >= 'a' && ch <= 'z') && !(ch >= 'A' && ch <= 'Z') && !(ch >= '0' && ch <= '9') && ch != '.' && ch != '-' {
				return "", fmt.Errorf("代理主机格式错误")
			}
		}
	}
	u.Path = ""
	return u.String(), nil
}

func parseList(raw string) ([]string, error) {
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "\ufeff")
	raw = strings.ReplaceAll(raw, "\\r\\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	raw = strings.ReplaceAll(raw, ",", "\n")
	var out []string
	seen := map[string]bool{}
	for i, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		proxy, err := normalizeProxy(line)
		if err != nil {
			return nil, fmt.Errorf("第 %d 行不是有效代理：%s", i+1, err)
		}
		if !seen[proxy] {
			seen[proxy] = true
			out = append(out, proxy)
		}
		if len(out) > 1000 {
			return nil, fmt.Errorf("单次提取最多支持 1000 条代理")
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("未提取到有效代理")
	}
	return out, nil
}

// Fetch only calls the extraction API. It never inherits system proxy settings
// or uses the pool it is filling, and never echoes credentials or response bodies.
func (m *Manager) Fetch(ctx context.Context, cfg Config) ([]string, error) {
	cfg = cfg.normalized()
	cfg.Enabled = true
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Source == "list" {
		return parseList(cfg.List)
	}
	if cfg.APIURL == "" {
		return nil, fmt.Errorf("请填写提取 API")
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		proxies, retry, err := m.fetchOnce(ctx, cfg.APIURL)
		if err == nil || !retry || attempt == 1 {
			return proxies, err
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, fmt.Errorf("代理提取失败")
}

func (m *Manager) fetchOnce(ctx context.Context, raw string) ([]string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, false, fmt.Errorf("提取 API 地址格式错误")
	}
	req.Header.Set("Accept", "text/plain")
	resp, err := m.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, true, fmt.Errorf("代理提取请求失败，请检查 API 连通性或超时")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if strings.Contains(strings.ToLower(string(body)), "ip whitelist check failed") {
			return nil, false, fmt.Errorf("代理提取接口拒绝了当前服务器 IP，请在供应商后台加入 IP 白名单（HTTP %d）", resp.StatusCode)
		}
		return nil, resp.StatusCode == 429 || resp.StatusCode >= 500, fmt.Errorf("代理提取接口返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, true, fmt.Errorf("读取代理提取结果失败")
	}
	if len(body) > maxResponseBytes {
		return nil, false, fmt.Errorf("代理提取结果超过 1 MB")
	}
	proxies, err := parseList(string(body))
	return proxies, false, err
}
