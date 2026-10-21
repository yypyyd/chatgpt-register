// Package captcha 封装第三方打码服务，目前支持 2Captcha 解 hCaptcha、Cloudflare Turnstile
// 与 Adobe 使用的 Arkose FunCaptcha。
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
	// Proxy 可选，格式 user:pass@host:port。Turnstile 站点校验会核对出口 IP 时，
	// 让打码工人走与注册相同的代理，token 才过得了 siteverify。
	Proxy     string
	ProxyType string
	UserAgent string
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

// SolveFunCaptcha 提交 Arkose FunCaptcha 任务并轮询取回 token。
// publicKey/surl/sessionID 从 Arkose enforcement iframe 的 URL 中解析；
// 实测 Adobe 的 Arkose 令牌不绑定打码出口 IP，传注册代理反而会让工人连不上，
// 因此这里不向 2Captcha 传 proxy。
func (t *TwoCaptcha) SolveFunCaptcha(ctx context.Context, publicKey, pageURL, surl, userAgent string) (string, error) {
	if strings.TrimSpace(t.Key) == "" {
		return "", fmt.Errorf("未配置 2Captcha key")
	}
	if publicKey == "" || pageURL == "" {
		return "", fmt.Errorf("缺少 Arkose publickey 或页面地址")
	}
	form := url.Values{
		"key":       {t.Key},
		"method":    {"funcaptcha"},
		"publickey": {publicKey},
		"pageurl":   {pageURL},
		"json":      {"1"},
	}
	if surl != "" {
		form.Set("surl", surl)
	}
	if userAgent != "" {
		form.Set("userAgent", userAgent)
	}
	// Adobe 的 Arkose 必须给打码工人一条能连出去的代理：直接用浏览器那条
	// sticky 住宅代理时，2Captcha 工人往往连不上（UNSOLVABLE）。这里使用
	// 设置里的 captcha_2captcha_proxy（公网可访问的 HTTP 代理）。
	if proxy := strings.TrimSpace(t.Proxy); proxy != "" {
		form.Set("proxy", normalizeTwoCaptchaProxy(proxy))
		pt := strings.TrimSpace(t.ProxyType)
		if pt == "" {
			pt = "HTTP"
		}
		form.Set("proxytype", pt)
	}
	// Adobe 的 Arkose 挑战带有较强的风控随机性：同一组参数偶尔会被工人判为
	// UNSOLVABLE，隔几秒重新提交通常就能解出来。这里最多重试 3 次，
	// token 被页面拒绝的情况由调用方通过重新触发挑战处理。
	const attempts = 6
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		id, err := t.post(ctx, twoCaptchaIn, form)
		if err != nil {
			lastErr = err
			if isUnsovable(err) && attempt < attempts {
				t.logf("2Captcha Arkose 第 %d 次判定 UNSOLVABLE，重新提交", attempt)
				if !sleepCtx(ctx, 3*time.Second) {
					return "", ctx.Err()
				}
				continue
			}
			return "", err
		}
		t.logf("2Captcha 已提交 Arkose FunCaptcha 任务 id=%s（第 %d/%d 次），等待结果", id, attempt, attempts)
		token, err := t.poll(ctx, id)
		if err == nil {
			return token, nil
		}
		lastErr = err
		if !isUnsovable(err) || attempt == attempts {
			return "", err
		}
		t.logf("2Captcha Arkose 第 %d 次判定 UNSOLVABLE，重新提交", attempt)
		if !sleepCtx(ctx, 3*time.Second) {
			return "", ctx.Err()
		}
	}
	return "", lastErr
}

func isUnsovable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNSOLVABLE")
}

func (t *TwoCaptcha) poll(ctx context.Context, id string) (string, error) {
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

// normalizeTwoCaptchaProxy 把 http://user:pass@host:port 转成 2Captcha 需要的
// user:pass@host:port 形式。
func normalizeTwoCaptchaProxy(proxy string) string {
	proxy = strings.TrimSpace(proxy)
	proxy = strings.TrimPrefix(proxy, "http://")
	proxy = strings.TrimPrefix(proxy, "https://")
	return strings.TrimRight(proxy, "/")
}
