package codexreg

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

// ProbeWebSession 用协议客户端（Chrome TLS 指纹）把已保存的网页 Cookie 打到
// GET https://chatgpt.com/api/auth/session。判活口径与原先浏览器测活相同：
// 调用方看 HTTP 状态码和 body 里的 accessToken。
//
// Cloudflare challenge / 网络错误以 error 返回；此时 status/body 仍可能有值，
// 调用方应标 unknown，不得判死。
func ProbeWebSession(ctx context.Context, cookies []WebCookie, proxy, languages string) (int, []byte, error) {
	if len(cookies) == 0 {
		return 0, nil, fmt.Errorf("没有网页 Cookie")
	}
	c, err := newProtocolClient(Input{Proxy: proxy}, languages)
	if err != nil {
		return 0, nil, err
	}
	c.loadCookies(cookies)
	r, err := c.get(ctx, "https://chatgpt.com/api/auth/session", "https://chatgpt.com/", false)
	if r == nil {
		return 0, nil, err
	}
	return r.Status, r.Body, err
}

func (c *protocolClient) loadCookies(cookies []WebCookie) {
	grouped := map[string][]*http.Cookie{}
	for _, wc := range cookies {
		name := strings.TrimSpace(wc.Name)
		if name == "" {
			continue
		}
		domain := wc.Domain
		if domain == "" {
			if strings.HasPrefix(name, "__Host-") || strings.HasPrefix(name, "__Secure-") {
				domain = "chatgpt.com"
			} else {
				continue
			}
		}
		if !isChatGPTCookieDomain(domain) {
			continue
		}
		path := wc.Path
		if path == "" {
			path = "/"
		}
		ck := &http.Cookie{
			Name:     name,
			Value:    wc.Value,
			Path:     path,
			Domain:   domain,
			Secure:   wc.Secure,
			HttpOnly: wc.HTTPOnly,
			SameSite: sameSiteFromString(wc.SameSite),
		}
		if wc.Expires > 0 {
			ck.Expires = time.Unix(int64(wc.Expires), 0)
		}
		site := cookieSiteURL(domain)
		grouped[site] = append(grouped[site], ck)
	}
	for site, list := range grouped {
		u, err := url.Parse(site)
		if err != nil {
			continue
		}
		c.cli.SetCookies(u, list)
	}
}

func cookieSiteURL(domain string) string {
	d := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if d == "auth.openai.com" || strings.HasSuffix(d, ".auth.openai.com") {
		return "https://auth.openai.com/"
	}
	if d == "openai.com" || strings.HasSuffix(d, ".openai.com") {
		return "https://openai.com/"
	}
	return "https://chatgpt.com/"
}

func sameSiteFromString(s string) http.SameSite {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "lax":
		return http.SameSiteLaxMode
	case "strict":
		return http.SameSiteStrictMode
	case "none":
		return http.SameSiteNoneMode
	default:
		return http.SameSiteDefaultMode
	}
}
