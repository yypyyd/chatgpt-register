package leonardoreg

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"chatgpt-register/internal/captcha"
	"chatgpt-register/internal/proxyutil"

	"github.com/go-rod/rod"
)

const (
	fallbackTurnstileSitekey = "0x4AAAAAAAjpS3rLKnsHyb79"
	turnstilePageURL         = loginURL
)

func (c *leoClient) mintCaptcha(ctx context.Context, sitekey string) (string, error) {
	if c.page != nil {
		tok, err := mintPageTurnstile(ctx, c.in, c.page)
		if err == nil && len(tok) >= 20 {
			return tok, nil
		}
		c.in.logf("页面 Turnstile 未拿到 token: %v，改走 2Captcha", err)
	}
	return mintTurnstileToken(ctx, c.in, sitekey, loginURL)
}

func mintPageTurnstile(ctx context.Context, in Input, page *rod.Page) (string, error) {
	if page == nil {
		return "", fmt.Errorf("无页面")
	}
	// 组件里若还留着上一口已核销的 token，必须先 reset，否则会直接复用导致 siteverify 失败。
	if turnstileTokenLen(page.Timeout(5*time.Second)) >= 20 {
		in.logf("丢弃页面上已签发的旧 Turnstile token")
		resetTurnstile(page.Timeout(5 * time.Second))
		if !sleepCtxLeo(ctx, 2*time.Second) {
			return "", ctx.Err()
		}
	}
	// 登录页要先填邮箱，Turnstile 组件才会渲染出来。
	if strings.TrimSpace(in.Email) != "" {
		if err := fillInput(ctx, page, selEmail, in.Email, 20*time.Second); err != nil {
			in.logf("预填邮箱以唤出 Turnstile 失败（继续等组件）: %v", err)
		}
	}
	if err := waitTurnstile(ctx, page, in, 90*time.Second); err != nil {
		return "", err
	}
	tok := readTurnstileToken(page.Timeout(10 * time.Second))
	if len(tok) < 20 {
		return "", fmt.Errorf("Turnstile 已通过但未读到 token")
	}
	in.logf("页面 Turnstile 令牌已就绪(len=%d)", len(tok))
	return tok, nil
}

func mintTurnstileToken(ctx context.Context, in Input, sitekey, pageURL string) (string, error) {
	if strings.TrimSpace(sitekey) == "" {
		sitekey = fallbackTurnstileSitekey
	}
	if strings.TrimSpace(pageURL) == "" {
		pageURL = turnstilePageURL
	}
	key := strings.TrimSpace(in.CaptchaKey)
	if key == "" {
		return "", fmt.Errorf("未配置 2Captcha key（设置 captcha_2captcha_key）")
	}
	proxy, proxyType := twoCaptchaProxy(in.Proxy)
	if proxy != "" {
		in.logf("Turnstile 走 2Captcha HTTP（工人走同一代理） sitekey=%s", trimText(sitekey, 18))
	} else {
		in.logf("Turnstile 走 2Captcha HTTP sitekey=%s", trimText(sitekey, 18))
	}
	solver := &captcha.TwoCaptcha{
		Key:       key,
		Log:       in.Log,
		Proxy:     proxy,
		ProxyType: proxyType,
		UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + protocolChromeMajor + ".0.0.0 Safari/537.36",
	}
	tok, err := solver.SolveTurnstile(ctx, sitekey, pageURL, "signin", "")
	if err != nil {
		return "", fmt.Errorf("2Captcha Turnstile: %w", err)
	}
	return tok, nil
}

func twoCaptchaProxy(raw string) (proxy, proxyType string) {
	u, err := url.Parse(proxyutil.Normalize(raw))
	if err != nil || u.Host == "" {
		return "", ""
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h":
		proxyType = "SOCKS5"
	case "socks4":
		proxyType = "SOCKS4"
	case "https":
		proxyType = "HTTPS"
	default:
		proxyType = "HTTP"
	}
	if u.User != nil {
		pass, _ := u.User.Password()
		return u.User.Username() + ":" + pass + "@" + u.Host, proxyType
	}
	return u.Host, proxyType
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
