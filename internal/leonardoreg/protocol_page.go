package leonardoreg

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

func newPageClient(ctx context.Context, in Input) (*leoClient, error) {
	if in.Headless {
		in.logf("启动无头浏览器过 Vercel BotID，随后走页面 fetch")
	} else {
		in.logf("启动可见浏览器过 Vercel BotID，随后走页面 fetch")
	}
	browser, authBridge, cleanup, err := launchLeonardoBrowser(in)
	if err != nil {
		return nil, err
	}
	c := &leoClient{
		in:      in,
		ua:      userAgent,
		browser: browser,
		kind:    "page",
	}
	c.cleanup = append(c.cleanup, cleanup)
	if authBridge != nil {
		c.cleanup = append(c.cleanup, func() { authBridge.Close() })
	}
	ok := false
	defer func() {
		if !ok {
			c.close()
		}
	}()

	page, err := browser.Page(proto.TargetCreateTarget{})
	if err != nil {
		return nil, fmt.Errorf("打开空白页失败: %w", err)
	}
	c.page = page
	if !in.Headless {
		if slot := acquireWindowSlot(); slot >= 0 {
			c.cleanup = append(c.cleanup, func() { releaseWindowSlot(slot) })
			placeBrowserWindow(browser, page, slot, in)
		} else {
			in.logf("可见窗口格子已用满，窗口可能与其它任务重叠")
		}
	} else {
		_ = (proto.EmulationSetDeviceMetricsOverride{
			Width:             1280,
			Height:            900,
			DeviceScaleFactor: 1,
			Mobile:            false,
		}).Call(page)
		if ver, verr := (proto.BrowserGetVersion{}).Call(browser); verr == nil {
			if ua := cleanUserAgent(ver.UserAgent); ua != "" {
				c.ua = ua
				_ = (proto.EmulationSetUserAgentOverride{
					UserAgent:      ua,
					AcceptLanguage: "en-US,en;q=0.9",
					Platform:       platformForUA(ua),
				}).Call(page)
			}
		}
	}
	if err = gotoStable(ctx, page, loginURL, in, 120*time.Second); err != nil {
		return nil, err
	}
	if err = waitPastBotID(ctx, page, in, 120*time.Second); err != nil {
		return nil, err
	}
	in.logf("登录页已出现邮箱框，后续 auth 走页面 fetch")
	if _, werr := c.get(ctx, origin+pathCrossOrigin); werr != nil {
		in.logf("cross-origin-cookie 预热失败（继续）: %v", werr)
	}
	ok = true
	return c, nil
}

func waitPastBotID(ctx context.Context, page *rod.Page, in Input, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	nudgeAt := time.Now()
	nextReload := time.Now().Add(stuckReloadAfter)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if hasVisibleSel(page, selEmail) {
			return nil
		}
		if time.Now().After(nextReload) {
			in.logf("登录页仍无邮箱框（title=%s），重新加载", trimText(pageTitle(page), 60))
			_ = gotoStable(ctx, page, loginURL, in, 45*time.Second)
			nextReload = time.Now().Add(stuckReloadAfter)
			continue
		}
		if !in.Headless && time.Now().After(nudgeAt) {
			nudgeRealCursor()
			nudgeAt = time.Now().Add(3 * time.Second)
		}
		dismissCookieBanner(page)
		clickByText(page, `a,span,button`, `continue with email`)
		time.Sleep(400 * time.Millisecond)
	}
	return fmt.Errorf("未能通过 Vercel BotID 进入邮箱表单（当前页 %s title=%s）",
		trimText(pageURL(page), 80), trimText(pageTitle(page), 60))
}

func (c *leoClient) doPage(ctx context.Context, method, rawURL string, body []byte, extra map[string]string) (*protoResp, error) {
	if c.page == nil {
		return nil, fmt.Errorf("页面客户端未就绪")
	}
	headers := map[string]string{
		"accept":          "application/json, text/plain, */*",
		"accept-language": "en-US,en;q=0.9",
		"origin":          origin,
		"referer":         loginURL,
	}
	if len(body) > 0 {
		headers["content-type"] = "application/json"
	}
	for k, v := range extra {
		if strings.TrimSpace(k) == "" || v == "" {
			continue
		}
		headers[k] = v
	}
	headerJSON, _ := json.Marshal(headers)
	bodyStr := ""
	if len(body) > 0 {
		bodyStr = string(body)
	}
	js := `(url, method, headerJSON, body) => new Promise(resolve => {
		const headers = JSON.parse(headerJSON || '{}');
		const opt = { method, credentials: 'include', cache: 'no-store', headers };
		if (body) opt.body = body;
		fetch(url, opt).then(async r => {
			const text = await r.text();
			resolve(JSON.stringify({ status: r.status, text }));
		}).catch(e => {
			resolve(JSON.stringify({ status: 0, text: String(e) }));
		});
	})`
	timeout := protocolStepTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remain := time.Until(deadline); remain > 0 && remain < timeout {
			timeout = remain
		}
	}
	v, err := c.page.Timeout(timeout).Eval(js, rawURL, method, string(headerJSON), bodyStr)
	if err != nil {
		return nil, fmt.Errorf("页面 fetch 失败: %w", err)
	}
	var parsed struct {
		Status int    `json:"status"`
		Text   string `json:"text"`
	}
	if json.Unmarshal([]byte(v.Value.Str()), &parsed) != nil {
		return nil, fmt.Errorf("页面 fetch 响应无法解析: %s", trimText(v.Value.Str(), 200))
	}
	out := &protoResp{Status: parsed.Status, Body: []byte(parsed.Text)}
	if parsed.Status == 0 {
		return out, fmt.Errorf("页面 fetch 失败: %s", trimText(parsed.Text, 200))
	}
	return out, nil
}

func pageCookies(page *rod.Page) []map[string]any {
	if page == nil {
		return nil
	}
	all, err := proto.NetworkGetAllCookies{}.Call(page)
	if err != nil || all == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(all.Cookies))
	for _, ck := range all.Cookies {
		if ck == nil || ck.Name == "" {
			continue
		}
		host := strings.ToLower(ck.Domain)
		if !strings.Contains(host, "leonardo") {
			continue
		}
		out = append(out, map[string]any{
			"name":     ck.Name,
			"value":    ck.Value,
			"domain":   ck.Domain,
			"path":     ck.Path,
			"expires":  ck.Expires,
			"httpOnly": ck.HTTPOnly,
			"secure":   ck.Secure,
			"sameSite": ck.SameSite,
		})
	}
	return out
}

func pageHTML(page *rod.Page) string {
	if page == nil {
		return ""
	}
	v, err := page.Timeout(8 * time.Second).Eval(`() => document.documentElement ? document.documentElement.innerHTML : ''`)
	if err != nil {
		return ""
	}
	return v.Value.Str()
}

func pageTitle(page *rod.Page) string {
	if page == nil {
		return ""
	}
	v, err := page.Timeout(5 * time.Second).Eval(`() => document.title || ''`)
	if err != nil {
		return ""
	}
	return v.Value.Str()
}

func nudgeRealCursor() {
	if os.Getenv("DISPLAY") == "" {
		return
	}
	tool, err := exec.LookPath("xdotool")
	if err != nil {
		return
	}
	_ = exec.Command(tool, "mousemove", "--sync", "420", "380").Run()
	time.Sleep(120 * time.Millisecond)
	_ = exec.Command(tool, "mousemove", "--sync", "510", "450").Run()
}

func shotPage(page *rod.Page, save func([]byte), logf func(string, ...any)) {
	if page == nil || save == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil && logf != nil {
			logf("截图失败(panic): %v", r)
		}
	}()
	data, err := page.Timeout(15*time.Second).Screenshot(false, nil)
	if err != nil || len(data) == 0 {
		if logf != nil {
			logf("截图失败: %v", err)
		}
		return
	}
	save(data)
	if logf != nil {
		logf("已保存失败现场截图")
	}
}
