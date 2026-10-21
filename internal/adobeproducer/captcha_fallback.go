package adobeproducer

import (
	"context"
	"sync"

	"chatgpt-register/internal/adobereg"
)

// captchaFallback 先走主打码服务（YesCaptcha），失败后自动切换到备用
// （2Captcha + 公网代理）。这样设置里可以优先使用 YesCaptcha，同时不放弃
// 已经验证可用的 2Captcha 组合。
type captchaFallback struct {
	primary   adobereg.CaptchaSolver
	secondary adobereg.CaptchaSolver
	logf      func(string, ...any)

	mu            sync.Mutex
	secondaryOnly bool
}

// PreferSecondary 在主打码服务的 token 被页面拒绝后调用，后续直接走备用。
func (f *captchaFallback) PreferSecondary() {
	f.mu.Lock()
	f.secondaryOnly = true
	f.mu.Unlock()
}

func (f *captchaFallback) useSecondaryOnly() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.secondaryOnly && f.secondary != nil
}

func (f *captchaFallback) SolveFunCaptcha(ctx context.Context, publicKey, pageURL, surl, userAgent string) (string, error) {
	if f.useSecondaryOnly() {
		f.logf("使用备用打码服务解 Arkose")
		return f.secondary.SolveFunCaptcha(ctx, publicKey, pageURL, surl, userAgent)
	}
	token, err := f.primary.SolveFunCaptcha(ctx, publicKey, pageURL, surl, userAgent)
	if err == nil {
		return token, nil
	}
	if f.secondary == nil {
		return "", err
	}
	f.logf("主打码服务失败，自动切换备用打码: %v", err)
	return f.secondary.SolveFunCaptcha(ctx, publicKey, pageURL, surl, userAgent)
}

func (f *captchaFallback) SolveHCaptcha(ctx context.Context, sitekey, pageURL, rqdata, userAgent string) (string, error) {
	if f.useSecondaryOnly() {
		return f.secondary.SolveHCaptcha(ctx, sitekey, pageURL, rqdata, userAgent)
	}
	token, err := f.primary.SolveHCaptcha(ctx, sitekey, pageURL, rqdata, userAgent)
	if err == nil {
		return token, nil
	}
	if f.secondary == nil {
		return "", err
	}
	f.logf("主打码服务失败，自动切换备用打码: %v", err)
	return f.secondary.SolveHCaptcha(ctx, sitekey, pageURL, rqdata, userAgent)
}
