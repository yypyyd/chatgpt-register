package codexreg

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	http "github.com/bogdanfinn/fhttp"
	"github.com/google/uuid"
)

// FetchViaProtocolClient 用与注册完全相同的 tls-client 配置请求一个地址并返回响应正文，
// 仅用于诊断（例如访问 TLS/HTTP2 指纹回显服务对比真实 Chrome）。
func FetchViaProtocolClient(ctx context.Context, proxy, rawURL string) (int, string, error) {
	c, err := newProtocolClient(Input{Proxy: proxy}, "en-US,en")
	if err != nil {
		return 0, "", err
	}
	c.strict = true // 与注册流程一致的“严格线上头”，便于和真实 Chrome 的回显逐项对比
	r, err := c.get(ctx, rawURL, "", true)
	if err != nil && r == nil {
		return 0, "", err
	}
	if r == nil {
		return 0, "", fmt.Errorf("empty response")
	}
	return r.Status, strings.TrimSpace(string(maybeGunzip(r.Body))), nil
}

// FetchPostViaProtocolClient 按注册流程里各类 POST 的头构造方式发一次请求（诊断用）。
// kind: form | json | json_sentinel | min。
func FetchPostViaProtocolClient(ctx context.Context, proxy, rawURL, kind string) (int, string, error) {
	c, err := newProtocolClient(Input{Proxy: proxy}, "en-US,en")
	if err != nil {
		return 0, "", err
	}
	c.strict = true
	pu, err := url.Parse(rawURL)
	if err != nil {
		return 0, "", err
	}
	referer := pu.Scheme + "://" + pu.Host + "/"
	var body string
	switch kind {
	case "form":
		body = "callbackUrl=x&csrfToken=y&json=true"
	default:
		body = `{"code":"123456"}`
	}
	req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	c.headers(req, referer, false)
	req.Header.Set("origin", pu.Scheme+"://"+pu.Host)
	switch kind {
	case "form":
		req.Header.Set("accept", "*/*")
		req.Header.Set("content-type", "application/x-www-form-urlencoded")
	case "json", "json_sentinel":
		req.Header.Set("accept", "application/json")
		req.Header.Set("content-type", "application/json")
		if c.navID == "" {
			c.navID = uuid.NewString()
		}
		req.Header.Set("x-openai-document-navigation-id", c.navID)
		req.Header.Set("x-access-flow-invocation-id", uuid.NewString())
		setDatadogTraceHeaders(req.Header)
		if kind == "json_sentinel" {
			req.Header.Set("openai-sentinel-token", `{"p":"x"}`)
			req.Header.Set("openai-sentinel-so-token", `{"so":"x"}`)
		}
	default: // min
		req.Header.Set("content-type", "application/json")
	}
	r, err := c.do(ctx, req)
	if err != nil && r == nil {
		return 0, "", err
	}
	if r == nil {
		return 0, "", fmt.Errorf("empty response")
	}
	return r.Status, strings.TrimSpace(string(maybeGunzip(r.Body))), nil
}
