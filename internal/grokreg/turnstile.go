package grokreg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// SolveTurnstile tries CapSolver / 2Captcha when API key is provided.
// Returns empty string if key missing or solve fails.
func SolveTurnstile(ctx context.Context, in Input, sitekey, pageURL string) (string, error) {
	key := strings.TrimSpace(in.CaptchaAPIKey)
	if key == "" || sitekey == "" {
		return "", fmt.Errorf("未配置打码密钥或 sitekey 为空")
	}
	provider := strings.ToLower(strings.TrimSpace(in.CaptchaProvider))
	if provider == "" {
		provider = "capsolver"
	}
	switch provider {
	case "2captcha", "twocaptcha":
		return solve2Captcha(ctx, key, sitekey, pageURL, in)
	default:
		return solveCapSolver(ctx, key, sitekey, pageURL, in)
	}
}

func solveCapSolver(ctx context.Context, apiKey, sitekey, pageURL string, in Input) (string, error) {
	in.logf("CapSolver 创建 Turnstile 任务 sitekey=%s", sitekey)
	createBody := map[string]any{
		"clientKey": apiKey,
		"task": map[string]any{
			"type":       "AntiTurnstileTaskProxyLess",
			"websiteURL": pageURL,
			"websiteKey": sitekey,
		},
	}
	var createResp struct {
		ErrorID          int    `json:"errorId"`
		ErrorCode        string `json:"errorCode"`
		ErrorDescription string `json:"errorDescription"`
		TaskID           string `json:"taskId"`
	}
	if err := postJSON(ctx, "https://api.capsolver.com/createTask", createBody, &createResp); err != nil {
		return "", err
	}
	if createResp.ErrorID != 0 || createResp.TaskID == "" {
		return "", fmt.Errorf("CapSolver createTask: %s %s", createResp.ErrorCode, createResp.ErrorDescription)
	}
	in.logf("CapSolver 任务已创建: %s", createResp.TaskID)

	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
		var getResp struct {
			ErrorID          int    `json:"errorId"`
			ErrorCode        string `json:"errorCode"`
			ErrorDescription string `json:"errorDescription"`
			Status           string `json:"status"`
			Solution         struct {
				Token string `json:"token"`
			} `json:"solution"`
		}
		body := map[string]any{"clientKey": apiKey, "taskId": createResp.TaskID}
		if err := postJSON(ctx, "https://api.capsolver.com/getTaskResult", body, &getResp); err != nil {
			in.logf("CapSolver 轮询失败: %v", err)
			continue
		}
		if getResp.ErrorID != 0 {
			return "", fmt.Errorf("CapSolver getTaskResult: %s %s", getResp.ErrorCode, getResp.ErrorDescription)
		}
		if strings.EqualFold(getResp.Status, "ready") && getResp.Solution.Token != "" {
			in.logf("CapSolver 已返回 Turnstile token (%d chars)", len(getResp.Solution.Token))
			return getResp.Solution.Token, nil
		}
	}
	return "", fmt.Errorf("CapSolver 求解超时")
}

func solve2Captcha(ctx context.Context, apiKey, sitekey, pageURL string, in Input) (string, error) {
	in.logf("2Captcha 创建 Turnstile 任务 sitekey=%s", sitekey)
	createBody := map[string]any{
		"clientKey": apiKey,
		"task": map[string]any{
			"type":       "TurnstileTaskProxyless",
			"websiteURL": pageURL,
			"websiteKey": sitekey,
		},
	}
	var createResp struct {
		ErrorID          int    `json:"errorId"`
		ErrorCode        string `json:"errorCode"`
		ErrorDescription string `json:"errorDescription"`
		TaskID           int64  `json:"taskId"`
	}
	if err := postJSON(ctx, "https://api.2captcha.com/createTask", createBody, &createResp); err != nil {
		return "", err
	}
	if createResp.ErrorID != 0 || createResp.TaskID == 0 {
		return "", fmt.Errorf("2Captcha createTask: %s %s", createResp.ErrorCode, createResp.ErrorDescription)
	}
	in.logf("2Captcha 任务已创建: %d", createResp.TaskID)

	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
		var getResp struct {
			ErrorID          int    `json:"errorId"`
			ErrorCode        string `json:"errorCode"`
			ErrorDescription string `json:"errorDescription"`
			Status           string `json:"status"`
			Solution         struct {
				Token string `json:"token"`
			} `json:"solution"`
		}
		body := map[string]any{"clientKey": apiKey, "taskId": createResp.TaskID}
		if err := postJSON(ctx, "https://api.2captcha.com/getTaskResult", body, &getResp); err != nil {
			in.logf("2Captcha 轮询失败: %v", err)
			continue
		}
		if getResp.ErrorID != 0 {
			return "", fmt.Errorf("2Captcha getTaskResult: %s %s", getResp.ErrorCode, getResp.ErrorDescription)
		}
		if strings.EqualFold(getResp.Status, "ready") && getResp.Solution.Token != "" {
			in.logf("2Captcha 已返回 Turnstile token (%d chars)", len(getResp.Solution.Token))
			return getResp.Solution.Token, nil
		}
	}
	return "", fmt.Errorf("2Captcha 求解超时")
}

func postJSON(ctx context.Context, url string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, trimText(string(data), 200))
	}
	return json.Unmarshal(data, out)
}

// solveTurnstileViaAPI extracts the sitekey, solves the challenge through the
// configured third-party provider, then injects the token into the page. This
// is the reliable path for x.ai's invisible managed Turnstile, which does not
// auto-issue a token for automated browsers no matter how often the widget is
// clicked.
func solveTurnstileViaAPI(ctx context.Context, page *rod.Page, in Input) bool {
	sitekey := extractTurnstileSitekey(page)
	if sitekey == "" {
		in.logf("未能从页面提取 Turnstile sitekey，暂无法调用打码服务")
		return false
	}
	target := pageURL(page)
	in.logf("调用第三方打码服务求解 Turnstile: provider=%s sitekey=%s", providerName(in), sitekey)
	token, err := SolveTurnstile(ctx, in, sitekey, target)
	if err != nil {
		in.logf("打码服务求解失败: %v", err)
		return false
	}
	if !injectTurnstileToken(page, token) {
		in.logf("Turnstile token 注入失败")
		return false
	}
	in.logf("已注入 Turnstile token，准备提交完成注册")
	return true
}

func providerName(in Input) string {
	p := strings.ToLower(strings.TrimSpace(in.CaptchaProvider))
	if p == "" {
		return "capsolver"
	}
	return p
}

func injectTurnstileToken(page *rod.Page, token string) bool {
	ok, err := page.Eval(`(token) => {
		const set = (el) => {
			if (!el) return;
			const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')?.set
				|| Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')?.set;
			if (setter) setter.call(el, token); else el.value = token;
			el.dispatchEvent(new Event('input', { bubbles: true }));
			el.dispatchEvent(new Event('change', { bubbles: true }));
		};
		let n = 0;
		for (const el of document.querySelectorAll('input[name="cf-turnstile-response"], textarea[name="cf-turnstile-response"], input[name*="turnstile"], textarea[name*="turnstile"]')) {
			set(el); n++;
		}
		// also create field if missing
		if (n === 0) {
			const form = document.querySelector('form') || document.body;
			const input = document.createElement('input');
			input.type = 'hidden';
			input.name = 'cf-turnstile-response';
			input.value = token;
			form.appendChild(input);
			n = 1;
		}
		// Make turnstile.getResponse() return our token — some forms read this
		// at submit time instead of the hidden cf-turnstile-response field.
		try {
			if (window.turnstile) {
				window.turnstile.getResponse = () => token;
			}
		} catch (e) {}
		const ev = new CustomEvent('cf-turnstile-response', { detail: token });
		window.dispatchEvent(ev);
		document.dispatchEvent(ev);
		return n > 0;
	}`, token)
	if err != nil {
		return false
	}
	return ok.Value.Bool()
}

// reuseTurnstile mirrors the reference project's getTurnstileToken routine.
// In particular, it never resizes a hidden managed iframe: doing that turns a
// normal invisible widget into a false "interactive challenge" in our own
// diagnostics. DrissionPage can pierce the widget's closed shadow roots; Rod's
// DOM-domain ShadowRoot/Frame methods provide the same behavior here.
func reuseTurnstile(page *rod.Page, in Input) bool {
	in.logf("页面安全校验 token 尚未签发，按参考项目复用 Turnstile 组件")
	_, _ = page.Eval(`() => {
		try {
			if (window.turnstile && typeof window.turnstile.reset === 'function') {
				window.turnstile.reset();
			}
		} catch (e) {}
	}`)

	// Mirror the reference getTurnstileToken loop: click the real checkbox on
	// every pass (not once), and each pass copy any issued token from the input
	// or turnstile.getResponse() back into cf-turnstile-response so the form can
	// read it. x.ai's managed widget issues a token to a genuine checkbox click
	// without any third-party solver.
	deadline := time.Now().Add(20 * time.Second)
	clickedAny := false
	for time.Now().Before(deadline) {
		if syncTurnstileToken(page.Timeout(3*time.Second)) >= 20 {
			in.logf("Turnstile 已签发 token")
			return true
		}
		if tryReuseTurnstileClick(page.Timeout(3 * time.Second)) {
			clickedAny = true
		}
		time.Sleep(time.Second)
	}
	if clickedAny {
		in.logf("Turnstile 组件已自动点击，继续等待 token")
	}
	return false
}

// syncTurnstileToken reads the Turnstile token from the response input or from
// turnstile.getResponse() and writes it back into the cf-turnstile-response
// field, mirroring the reference project's token sync. Returns the token length.
func syncTurnstileToken(page *rod.Page) int {
	v, err := page.Eval(`() => {
		const input = document.querySelector('input[name="cf-turnstile-response"], textarea[name="cf-turnstile-response"]');
		let token = String((input && input.value) || '').trim();
		if (token.length < 20) {
			try {
				if (window.turnstile && typeof window.turnstile.getResponse === 'function') {
					token = String(window.turnstile.getResponse() || '').trim();
				}
			} catch (e) {}
		}
		if (token.length >= 20 && input) {
			const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')?.set
				|| Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')?.set;
			if (setter) setter.call(input, token); else input.value = token;
			input.dispatchEvent(new Event('input', { bubbles: true }));
			input.dispatchEvent(new Event('change', { bubbles: true }));
		}
		return token.length;
	}`)
	if err != nil {
		return 0
	}
	return int(v.Value.Num())
}

// Turnstile replaces its closed-shadow iframe while it is resetting. Rod may
// therefore observe a valid shadow root followed immediately by a detached
// frame. Keep that transient race inside one guarded attempt instead of
// allowing a stale DOM object to abort the whole registration.
func tryReuseTurnstileClick(page *rod.Page) (clicked bool) {
	defer func() {
		if recover() != nil {
			clicked = false
		}
	}()
	response, err := page.Element(`[name="cf-turnstile-response"]`)
	if err != nil || response == nil {
		return tryReuseTurnstileFallbackClick(page)
	}
	// Mirror the reference getTurnstileToken exactly: the widget mounts in the
	// wrapper's shadow root, and the real checkbox is an <input> inside the
	// cross-origin iframe body's own shadow root. Click that element directly.
	// A computed page-coordinate click on the host box turns the invisible
	// managed widget into a stuck interactive challenge that never issues a
	// token, so coordinate clicking is only a last-resort fallback.
	wrapper, err := response.Parent()
	if err != nil || wrapper == nil {
		return tryReuseTurnstileFallbackClick(page)
	}
	shadow, err := wrapper.ShadowRoot()
	if err != nil || shadow == nil || shadow.Page() == nil {
		return tryReuseTurnstileFallbackClick(page)
	}
	iframe, err := shadow.Element("iframe")
	if err != nil || iframe == nil || iframe.Page() == nil {
		return tryReuseTurnstileFallbackClick(page)
	}
	frame, err := iframe.Frame()
	if err != nil || frame == nil || frame.FrameID == "" {
		return tryReuseTurnstileFallbackClick(page)
	}
	// Spoof screenX/screenY so the synthetic click looks like a real cursor,
	// matching the reference project's iframe.run_js injection.
	_, _ = frame.Eval(`() => {
		try {
			const sx = 800 + Math.floor(Math.random() * 400);
			const sy = 400 + Math.floor(Math.random() * 300);
			Object.defineProperty(MouseEvent.prototype, 'screenX', { configurable: true, get: () => sx });
			Object.defineProperty(MouseEvent.prototype, 'screenY', { configurable: true, get: () => sy });
		} catch (e) {}
	}`)
	body, err := frame.Element("body")
	if err != nil || body == nil || body.Page() == nil {
		return tryReuseTurnstileFallbackClick(page)
	}
	root := body
	if inner, innerErr := body.ShadowRoot(); innerErr == nil && inner != nil && inner.Page() != nil {
		root = inner
	}
	button, err := root.Element(`input, [role="checkbox"], label, button`)
	if err != nil || button == nil || button.Page() == nil {
		return tryReuseTurnstileFallbackClick(page)
	}
	if err := button.Click(proto.InputMouseButtonLeft, 1); err == nil {
		return true
	}
	if mouseClickElement(button) {
		return true
	}
	return tryReuseTurnstileFallbackClick(page)
}

// tryReuseTurnstileFallbackClick clicks the widget's visible host box by page
// coordinate. Rod cannot always pierce Cloudflare's closed shadow root; when the
// element-click path is unavailable this hits the 64px host hotspot instead.
func tryReuseTurnstileFallbackClick(page *rod.Page) bool {
	point, err := page.Eval(`() => {
		const response = document.querySelector('[name="cf-turnstile-response"]');
		const host = response && (response.parentElement || {}).parentElement;
		if (!host) return null;
		const style = getComputedStyle(host);
		const r = host.getBoundingClientRect();
		if (style.display === 'none' || style.visibility === 'hidden' || Number(style.opacity || 1) === 0) return null;
		if (r.width < 100 || r.height < 40) return null;
		return { x: r.left + 21, y: r.top + Math.min(35, r.height / 2), w: r.width, h: r.height };
	}`)
	if err != nil || point == nil {
		return false
	}
	raw, _ := json.Marshal(point.Value)
	var p struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
		W float64 `json:"w"`
		H float64 `json:"h"`
	}
	if json.Unmarshal(raw, &p) != nil || p.W < 100 || p.H < 40 {
		return false
	}
	return mouseClickAt(page, p.X, p.Y)
}

func pageURL(page *rod.Page) string {
	info, err := page.Info()
	if err != nil || info == nil {
		return "https://accounts.x.ai/sign-up"
	}
	if info.URL == "" {
		return "https://accounts.x.ai/sign-up"
	}
	return info.URL
}
