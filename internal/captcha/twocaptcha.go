// Package captcha 封装第三方打码服务，目前支持 2Captcha 解 hCaptcha。
package captcha

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	twoCaptchaIn  = "https://2captcha.com/in.php"
	twoCaptchaRes = "https://2captcha.com/res.php"

	// 打码一般 15~60 秒返回，超过 3 分钟按失败处理。
	solveTimeout  = 3 * time.Minute
	pollInterval  = 5 * time.Second
	firstPollWait = 12 * time.Second
)

// TwoCaptcha 用 2Captcha 的 in.php/res.php 接口解验证码。
type TwoCaptcha struct {
	Key    string
	Client *http.Client
	Log    func(format string, a ...any)
}

func (t *TwoCaptcha) logf(format string, a ...any) {
	if t.Log != nil {
		t.Log(format, a...)
	}
}

func (t *TwoCaptcha) client() *http.Client {
	if t.Client != nil {
		return t.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// SolveHCaptcha 提交 hCaptcha 任务并轮询取回 token。rqdata 为 enterprise 版的
// 附加数据（页面上没有则传空）。
func (t *TwoCaptcha) SolveHCaptcha(ctx context.Context, sitekey, pageURL, rqdata, userAgent string) (string, error) {
	if strings.TrimSpace(t.Key) == "" {
		return "", fmt.Errorf("未配置 2Captcha key")
	}
	if sitekey == "" || pageURL == "" {
		return "", fmt.Errorf("缺少 sitekey 或页面地址")
	}
	form := url.Values{
		"key":     {t.Key},
		"method":  {"hcaptcha"},
		"sitekey": {sitekey},
		"pageurl": {pageURL},
		"json":    {"1"},
	}
	if rqdata != "" {
		form.Set("data", rqdata)
	}
	if userAgent != "" {
		form.Set("userAgent", userAgent)
	}
	id, err := t.post(ctx, twoCaptchaIn, form)
	if err != nil {
		return "", err
	}
	t.logf("2Captcha 已提交 hCaptcha 任务 id=%s，等待结果", id)

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

// Report 把结果反馈给 2Captcha（好评退款/坏评），失败忽略。
func (t *TwoCaptcha) Report(ctx context.Context, id string, good bool) {
	action := "reportbad"
	if good {
		action = "reportgood"
	}
	_, _ = t.post(ctx, twoCaptchaRes, url.Values{
		"key":    {t.Key},
		"action": {action},
		"id":     {id},
		"json":   {"1"},
	})
}

// post 发一次表单请求，返回 request 字段；status != 1 时把 request 当错误码返回。
func (t *TwoCaptcha) post(ctx context.Context, endpoint string, form url.Values) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var out struct {
		Status  int    `json:"status"`
		Request string `json:"request"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("解析 2Captcha 响应失败: %w", err)
	}
	if out.Status != 1 {
		return "", fmt.Errorf("2Captcha 返回错误: %s", out.Request)
	}
	return out.Request, nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
