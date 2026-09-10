package grokreg

import (
	"context"
	"fmt"
	"strings"

	"chatgpt-register/internal/captcha"
)

const (
	fallbackTurnstileSitekey = "0x4AAAAAAAhr9JGVDZbrZOo0"
	turnstileSignURL         = "https://accounts.x.ai/sign-up"
)

func mintTurnstileToken(ctx context.Context, in Input, sitekey, pageURL string) (string, error) {
	if strings.TrimSpace(sitekey) == "" {
		sitekey = fallbackTurnstileSitekey
	}
	if strings.TrimSpace(pageURL) == "" {
		pageURL = turnstileSignURL
	}
	key := strings.TrimSpace(in.CaptchaKey)
	if key == "" {
		return "", fmt.Errorf("未配置 2Captcha key（设置 captcha_2captcha_key）")
	}
	in.logf("Turnstile 走 2Captcha HTTP（不开浏览器） sitekey=%s", trimText(sitekey, 18))
	solver := &captcha.TwoCaptcha{Key: key, Log: in.Log}
	tok, err := solver.SolveTurnstile(ctx, sitekey, pageURL, "", "")
	if err != nil {
		return "", fmt.Errorf("2Captcha Turnstile: %w", err)
	}
	return tok, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func trimText(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n]
}
