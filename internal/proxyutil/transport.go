package proxyutil

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/proxy"
)

// Normalize 把 host:port / host:port:user:pass 之类的写法转成标准代理 URL；带 scheme 的原样返回。
func Normalize(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "://") {
		return raw
	}
	parts := strings.Split(raw, ":")
	switch len(parts) {
	case 2: // host:port
		return "http://" + parts[0] + ":" + parts[1]
	case 4: // host:port:user:pass
		return "http://" + url.QueryEscape(parts[2]) + ":" + url.QueryEscape(parts[3]) + "@" + parts[0] + ":" + parts[1]
	default:
		return "http://" + raw
	}
}

// Parse 解析代理串，返回 Chrome --proxy-server 用的 scheme://host:port（不含账号密码）
// 以及单独的账号密码（交给 browser.MustHandleAuth / 认证桥处理）。
func Parse(raw string) (server, user, pass string, err error) {
	u, err := url.Parse(Normalize(raw))
	if err != nil {
		return "", "", "", err
	}
	if u.Host == "" {
		return "", "", "", fmt.Errorf("代理缺少 host: %s", raw)
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "http"
	}
	server = scheme + "://" + u.Host
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	return server, user, pass, nil
}

// List 把多行/逗号分隔的代理串拆成切片，去掉空行。
func List(raw string) []string {
	raw = strings.ReplaceAll(raw, ",", "\n")
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Transport 按代理串构造 http 出口；代理为空返回 nil（直连）。
func Transport(raw string) (*http.Transport, error) {
	pu := Normalize(raw)
	if pu == "" {
		return nil, nil
	}
	u, err := url.Parse(pu)
	if err != nil {
		return nil, errors.New("代理格式错误")
	}
	transport := &http.Transport{}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(u)
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pw}
		}
		dialer, derr := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if derr != nil {
			return nil, derr
		}
		if cd, ok := dialer.(proxy.ContextDialer); ok {
			transport.DialContext = cd.DialContext
		} else {
			transport.Dial = dialer.Dial //nolint:staticcheck
		}
	default:
		return nil, errors.New("不支持的代理类型: " + u.Scheme)
	}
	return transport, nil
}
