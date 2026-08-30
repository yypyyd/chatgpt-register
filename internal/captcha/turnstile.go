package captcha

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// SolveTurnstile 提交 Cloudflare Turnstile 任务并轮询取回 token。
// action / cdata 为站点渲染组件时传的参数，页面上没有则传空。
// 返回的是打码平台在真实挑战里拿到的 token，程序自身不做任何伪造或绕过。
func (t *TwoCaptcha) SolveTurnstile(ctx context.Context, sitekey, pageURL, action, cdata string) (string, error) {
	if strings.TrimSpace(t.Key) == "" {
		return "", fmt.Errorf("未配置 2Captcha key")
	}
	if sitekey == "" || pageURL == "" {
		return "", fmt.Errorf("缺少 sitekey 或页面地址")
	}
	form := url.Values{
		"key":     {t.Key},
		"method":  {"turnstile"},
		"sitekey": {sitekey},
		"pageurl": {pageURL},
		"json":    {"1"},
	}
	if action != "" {
		form.Set("action", action)
	}
	if cdata != "" {
		form.Set("data", cdata)
	}
	id, err := t.post(ctx, twoCaptchaIn, form)
	if err != nil {
		return "", err
	}
	t.logf("2Captcha 已提交 Turnstile 任务 id=%s，等待结果", id)

	deadline := time.Now().Add(solveTimeout)
	if !sleepCtx(ctx, firstPollWait) {
		return "", ctx.Err()
	}
	for time.Now().Before(deadline) {
		token, err := t.post(ctx, twoCaptchaRes, url.Values{
			"key":    {t.Key},
			"action": {"get"},
			"id":     {id},
			"json":   {"1"},
		})
		if err == nil {
			return token, nil
		}
		if !strings.Contains(err.Error(), "CAPCHA_NOT_READY") {
			return "", err
		}
		if !sleepCtx(ctx, pollInterval) {
			return "", ctx.Err()
		}
	}
	return "", fmt.Errorf("2Captcha 打码超时")
}
