package leonardoreg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	launcherflags "github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
)

// ErrEmailTaken 该邮箱已有 Leonardo 账号，属于永久失败：换出口/重试都没用，
// 上层应标记终态而不是过冷却后再拿来重试。
var ErrEmailTaken = errors.New("该邮箱已注册 Leonardo")

const (
	loginURL = "https://app.leonardo.ai/auth/login"
	appURL   = "https://app.leonardo.ai/"

	// Leonardo 登录页各步骤的输入框（React 表单，id 稳定）。
	selEmail    = `#email-field`
	selNewPass  = `#create-password-field`
	selConfirm  = `#confirm-password-field`
	selLoginPwd = `#password-field`
	selCode     = `#confirmation-code-field`

	// stuckReloadAfter 登录页卡住（SPA 只转圈、没渲染出邮箱框）多久后重新加载页面。
	stuckReloadAfter = 25 * time.Second
)

// launchLeonardoBrowser 按注册用的一整套反爬/代理配置启动并连接 Leonardo 专用 Chromium。
// 返回的 browser 由调用方负责关闭；若返回了 bridge（认证代理桥），也要一并 Close；
// cleanup 在浏览器关闭后调用，负责清理 launcher 的临时用户数据目录。
func launchLeonardoBrowser(in Input) (browser *rod.Browser, bridge *localAuthProxyBridge, cleanup func(), err error) {
	// 与 grokreg/adobereg 一致：删掉 rod 默认追加的一批自动化特征标志。
	l := launcher.New()
	for _, flag := range []string{
		"no-startup-window",
		"disable-features",
		"disable-dev-shm-usage",
		"disable-background-networking",
		"disable-background-timer-throttling",
		"disable-backgrounding-occluded-windows",
		"disable-breakpad",
		"disable-client-side-phishing-detection",
		"disable-component-extensions-with-background-pages",
		"disable-default-apps",
		"disable-hang-monitor",
		"disable-ipc-flooding-protection",
		"disable-prompt-on-repost",
		"disable-renderer-backgrounding",
		"disable-sync",
		"disable-site-isolation-trials",
		"enable-automation",
		"enable-features",
		"force-color-profile",
		"metrics-recording-only",
		"use-mock-keychain",
	} {
		l = l.Delete(launcherflags.Flag(flag))
	}
	if in.Headless {
		// 旧无头指纹差异大、Turnstile 基本过不去，必须显式 new headless。
		l = l.Set("headless", "new")
	} else {
		// rod 默认无头，可见模式必须显式关掉，否则 X 上根本没有真实窗口，
		// Turnstile 也就收不到真实光标事件。
		l = l.Headless(false)
	}
	l = l.
		NoSandbox(true).
		Set("no-default-browser-check").
		Set("disable-suggestions-ui").
		Set("no-first-run").
		Set("disable-infobars").
		Set("disable-popup-blocking").
		Set("hide-crash-restore-bubble").
		Set("disable-features", "PrivacySandboxSettings4")
	debugPort, dperr := availableLoopbackPort()
	if dperr != nil {
		return nil, nil, nil, fmt.Errorf("分配 Chrome 调试端口失败: %w", dperr)
	}
	l = l.Set("remote-debugging-port", strconv.Itoa(debugPort))
	if chromePath, cerr := leonardoChromiumBin(); cerr != nil {
		in.logf("准备 Leonardo 专用 Chromium 失败，回退默认浏览器: %v", cerr)
	} else {
		l = l.Bin(chromePath)
		in.logf("使用 Leonardo 专用 Chromium，与其它平台浏览器隔离")
	}

	// 加载 Turnstile 补丁扩展（同 grokreg）：不打补丁时 Cloudflare 能从
	// MouseEvent.screenX/screenY 看出点击来自自动化，复选框点了也不签发 token。
	patchDir, perr := extractTurnstilePatch()
	if perr != nil {
		in.logf("释放 Turnstile 补丁扩展失败，回退到无扩展模式: %v", perr)
		patchDir = ""
	} else {
		l = l.Set("disable-extensions-except", patchDir).Set("load-extension", patchDir)
		in.logf("已加载 Turnstile 补丁扩展")
	}
	removePatch := func() {
		if patchDir != "" {
			os.RemoveAll(patchDir)
		}
	}
	defer func() {
		if err != nil {
			removePatch()
		}
	}()

	if strings.TrimSpace(in.Proxy) != "" {
		server, user, pass, perr := parseProxy(in.Proxy)
		if perr != nil {
			return nil, nil, nil, fmt.Errorf("解析代理失败: %w", perr)
		}
		if user != "" || pass != "" {
			upstreamServer := server
			bridge, server, perr = startLocalAuthProxyBridge(in.Proxy)
			if perr != nil {
				return nil, nil, nil, fmt.Errorf("启动认证代理桥失败: %w", perr)
			}
			in.logf("已启用 Chromium 本地认证代理桥，本地 %s → 上游 %s", server, upstreamServer)
		}
		l = l.Set("proxy-server", server)
		in.logf("Chromium 使用代理入口: %s", server)
	}

	controlURL, lerr := l.Launch()
	if lerr != nil {
		if bridge != nil {
			bridge.Close()
		}
		return nil, nil, nil, fmt.Errorf("启动 Chrome 失败: %w", lerr)
	}
	browser = rod.New().NoDefaultDevice().ControlURL(controlURL)
	if cerr := browser.Connect(); cerr != nil {
		if bridge != nil {
			bridge.Close()
		}
		return nil, nil, nil, fmt.Errorf("连接 Chrome 失败: %w", cerr)
	}
	return browser, bridge, func() {
		l.Cleanup()
		removePatch()
	}, nil
}

func registerBrowser(ctx context.Context, in Input) (res *Result, err error) {
	if in.Headless {
		in.logf("启动无头浏览器，打开 Leonardo 注册页")
	} else {
		in.logf("启动可见浏览器，打开 Leonardo 注册页")
	}

	browser, authBridge, cleanup, err := launchLeonardoBrowser(in)
	if err != nil {
		return nil, err
	}
	proxyConfigured := strings.TrimSpace(in.Proxy) != ""
	if authBridge != nil {
		defer authBridge.Close()
	}
	defer func() {
		// 关浏览器后清理 launcher 临时用户数据目录，避免残留 Profile 堆积
		_ = rod.Try(browser.MustClose)
		cleanup()
	}()

	var page *rod.Page
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("Leonardo 注册流程异常: %v", r)
		}
		if err == nil || page == nil || in.SaveShot == nil {
			return
		}
		func() {
			defer func() {
				if r2 := recover(); r2 != nil {
					in.logf("截图失败(panic): %v", r2)
				}
			}()
			data, serr := page.Timeout(15*time.Second).Screenshot(false, nil)
			if serr != nil || len(data) == 0 {
				in.logf("截图失败: %v", serr)
				return
			}
			in.SaveShot(data)
			in.logf("已保存失败现场截图")
		}()
	}()

	// 出口 IP 探测仅用于排障，默认跳过以省一次整页加载。
	if proxyConfigured && in.EgressCheck {
		checkPage := browser.MustPage("https://api.ipify.org?format=json")
		checkPage.MustWaitLoad()
		if body, berr := checkPage.Timeout(15 * time.Second).Element("body"); berr == nil && body != nil {
			if value, terr := body.Text(); terr == nil {
				in.logf("Chromium 实际代理出口: %s", trimText(value, 160))
			}
		}
		_ = checkPage.Close()
	}

	page = browser.MustPage("")
	if !in.Headless {
		// 多个注册任务共用一个 X 显示，窗口默认叠在同一位置，真光标点击会点到别人
		// 的窗口，按格子摆开各自独占一块屏幕。
		if slot := acquireWindowSlot(); slot >= 0 {
			defer releaseWindowSlot(slot)
			placeBrowserWindow(browser, page, slot, in)
		} else {
			in.logf("可见窗口格子已用满，窗口可能与其它任务重叠")
		}
	}
	if in.Headless {
		// 可见模式覆盖视口会让 outerHeight 小于 innerHeight，真实光标坐标算不出来。
		_ = (proto.EmulationSetDeviceMetricsOverride{
			Width:             1280,
			Height:            900,
			DeviceScaleFactor: 1,
			Mobile:            false,
		}).Call(page)
	}
	if in.Headless {
		// 无头 Chrome 的 UA 带 HeadlessChrome 标记，会被 Cloudflare 直接拦下。
		if ver, verr := (proto.BrowserGetVersion{}).Call(browser); verr == nil {
			if ua := cleanUserAgent(ver.UserAgent); ua != "" {
				_ = (proto.EmulationSetUserAgentOverride{
					UserAgent:      ua,
					AcceptLanguage: "en-US,en;q=0.9",
					Platform:       platformForUA(ua),
				}).Call(page)
				in.logf("无头 UA 已修正: %s", ua)
			}
		}
	}

	if err = gotoStable(ctx, page, loginURL, in, 120*time.Second); err != nil {
		return nil, err
	}
	in.logf("Leonardo 登录页已加载")

	if err = gotoEmailForm(ctx, page, in); err != nil {
		return nil, err
	}
	if err = submitEmail(ctx, page, in); err != nil {
		return nil, err
	}
	if err = createPassword(ctx, page, in); err != nil {
		return nil, err
	}
	if err = confirmSignup(ctx, page, in); err != nil {
		return nil, err
	}

	auth, cerr := captureAuth(ctx, page, in)
	if cerr != nil {
		return nil, cerr
	}
	return &Result{AuthJSON: auth}, nil
}

// gotoEmailForm 等待登录页渲染出邮箱输入框；页面默认就是「Continue with Email」，
// 若被折叠在按钮后面则点开。SPA 卡住时重新加载再试。
func gotoEmailForm(ctx context.Context, page *rod.Page, in Input) error {
	deadline := time.Now().Add(90 * time.Second)
	nextReload := time.Now().Add(stuckReloadAfter)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if hasVisibleSel(page, selEmail) {
			in.logf("已进入邮箱注册表单")
			return nil
		}
		if time.Now().After(nextReload) {
			in.logf("登录页仍未渲染出邮箱输入框，重新加载后再试")
			_ = gotoStable(ctx, page, loginURL, in, 45*time.Second)
			nextReload = time.Now().Add(stuckReloadAfter)
			continue
		}
		dismissCookieBanner(page)
		clickByText(page, `a,span,button`, `continue with email`)
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("未能进入 Leonardo 邮箱注册表单")
}

// submitEmail 填邮箱、等 Turnstile 签发 token 后提交；Leonardo 随后用 GraphQL 查这个
// 邮箱是新号还是老号，新号会进入「创建密码」步骤。
func submitEmail(ctx context.Context, page *rod.Page, in Input) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt > 0 {
			in.logf("邮箱步骤重试第 %d 次：重新加载登录页", attempt)
			_ = gotoStable(ctx, page, loginURL, in, 60*time.Second)
			if err := gotoEmailForm(ctx, page, in); err != nil {
				lastErr = err
				continue
			}
		}
		if err := fillInput(ctx, page, selEmail, in.Email, 45*time.Second); err != nil {
			lastErr = fmt.Errorf("输入邮箱失败: %w", err)
			continue
		}
		if err := waitTurnstile(ctx, page, in, 90*time.Second); err != nil {
			lastErr = err
			continue
		}
		in.logf("已填写邮箱，提交邮箱步骤")
		advanced := func() bool {
			return hasVisibleSel(page, selNewPass) || hasVisibleSel(page, selCode) || hasVisibleSel(page, selLoginPwd)
		}
		if err := submitAndAdvance(ctx, page, advanced, 60*time.Second); err != nil {
			// 页面上的报错文案才是真正原因（邮箱域名被拒、Turnstile token 失效等）。
			lastErr = fmt.Errorf("提交邮箱失败: %w（%s）", err, trimText(pageAlert(page), 160))
			continue
		}
		if hasVisibleSel(page, selLoginPwd) && !hasVisibleSel(page, selNewPass) {
			// Leonardo 判定该邮箱已有账号，走的是登录而不是注册。
			return fmt.Errorf("%w（页面进入登录密码步骤）", ErrEmailTaken)
		}
		return nil
	}
	return lastErr
}

// createPassword 填写「创建密码 + 再输一次」并提交，随后页面进入验证码步骤。
// 已经直接跳到验证码步骤（例如上次注册未确认）时跳过。
func createPassword(ctx context.Context, page *rod.Page, in Input) error {
	if !hasVisibleSel(page, selNewPass) {
		if hasVisibleSel(page, selCode) {
			in.logf("账号已存在但未确认，直接进入验证码步骤")
			return nil
		}
		return fmt.Errorf("未出现创建密码表单")
	}
	if err := fillInput(ctx, page, selNewPass, in.Password, 30*time.Second); err != nil {
		return fmt.Errorf("输入密码失败: %w", err)
	}
	if err := fillInput(ctx, page, selConfirm, in.Password, 30*time.Second); err != nil {
		return fmt.Errorf("确认密码失败: %w", err)
	}
	// 创建账号这一步同样要带 Turnstile token（signup 接口的 verificationToken）。
	if err := waitTurnstile(ctx, page, in, 90*time.Second); err != nil {
		return err
	}
	in.logf("已填写密码，提交创建账号")
	if err := submitAndAdvance(ctx, page, func() bool { return hasVisibleSel(page, selCode) }, 90*time.Second); err != nil {
		return fmt.Errorf("创建账号失败: %w（%s）", err, trimText(pageAlert(page), 160))
	}
	return nil
}

// confirmSignup 取邮箱验证码填入并提交，成功后 Leonardo 自动登录并跳出登录页。
func confirmSignup(ctx context.Context, page *rod.Page, in Input) error {
	for attempt := 0; attempt < 2; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !hasVisibleSel(page, selCode) {
			return nil
		}
		in.logf("等待 Leonardo 邮箱验证码")
		code, err := in.WaitCode(ctx)
		if err != nil {
			return fmt.Errorf("获取邮箱验证码失败: %w", err)
		}
		if err = fillInput(ctx, page, selCode, code, 45*time.Second); err != nil {
			return fmt.Errorf("填写验证码失败: %w", err)
		}
		// 确认注册接口同样要 Turnstile token，组件还在页面上就得先过一次。
		if hasTurnstileWidget(page) {
			if err = waitTurnstile(ctx, page, in, 90*time.Second); err != nil {
				return err
			}
		}
		in.logf("已填写验证码，提交确认注册")
		if err = submitAndAdvance(ctx, page, func() bool { return !hasVisibleSel(page, selCode) }, 90*time.Second); err == nil {
			in.logf("验证码校验通过，等待自动登录")
			return nil
		}
		// 旧码被拒时点「重新发送」，让下一轮等的是新邮件。
		clickByText(page, `button,a,span`, `resend`)
		in.logf("验证码提交后仍停留在验证步骤（%s），已请求重发验证码后重试", trimText(pageAlert(page), 120))
	}
	return fmt.Errorf("邮箱验证码校验未通过")
}

// captureAuth 采集 Leonardo 站点 Cookie（含 better-auth 会话 cookie）与本地存储。
func captureAuth(ctx context.Context, page *rod.Page, in Input) (map[string]any, error) {
	// 确认注册后前端会自动登录并跳转，等它落到应用页再采集，确保会话 cookie 已下发。
	if !waitCleared(ctx, page, 60*time.Second, func() bool {
		u := pageURL(page)
		return strings.Contains(u, "leonardo.ai") && !strings.Contains(u, "/auth/login")
	}) {
		in.logf("等待跳转到 Leonardo 应用页超时，主动打开应用页采集会话")
		_ = gotoStable(ctx, page, appURL, in, 60*time.Second)
	}
	in.logf("当前页面: %s", trimText(pageURL(page), 120))

	all, err := proto.NetworkGetAllCookies{}.Call(page)
	if err != nil {
		return nil, fmt.Errorf("读取 Cookie 失败: %w", err)
	}
	cookieList := make([]map[string]any, 0, len(all.Cookies))
	hasSession := false
	for _, c := range all.Cookies {
		if strings.Contains(strings.ToLower(c.Name), "better-auth") {
			hasSession = true
		}
		cookieList = append(cookieList, map[string]any{
			"name":     c.Name,
			"value":    c.Value,
			"domain":   c.Domain,
			"path":     c.Path,
			"expires":  c.Expires,
			"httpOnly": c.HTTPOnly,
			"secure":   c.Secure,
			"sameSite": c.SameSite,
		})
	}
	if !hasSession {
		return nil, fmt.Errorf("未采集到 better-auth 会话 cookie，登录可能未完成（当前页面: %s）", trimText(pageURL(page), 120))
	}
	in.logf("已采集 %d 条 Cookie（含 better-auth 会话 cookie）", len(cookieList))

	storageRaw := page.MustEval(`() => JSON.stringify({
		localStorage: Object.fromEntries(Object.entries(localStorage)),
		sessionStorage: Object.fromEntries(Object.entries(sessionStorage)),
		location: location.href
	})`).String()
	var storage map[string]any
	_ = json.Unmarshal([]byte(storageRaw), &storage)

	return map[string]any{
		"auth_mode":   "leonardo_browser_session",
		"platform":    "leonardo",
		"email":       in.Email,
		"captured_at": time.Now().UTC().Format(time.RFC3339),
		"cookies":     cookieList,
		"storage":     storage,
	}, nil
}

/* ===== 页面推进 ===== */

func gotoStable(ctx context.Context, page *rod.Page, target string, in Input, timeout time.Duration) error {
	_ = rod.Try(func() { page.Timeout(timeout).MustNavigate(target) })
	deadline := time.Now().Add(timeout)
	var last string
	stable := 0
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_ = rod.Try(func() { page.Timeout(8 * time.Second).MustWaitLoad() })
		u := pageURL(page)
		if strings.Contains(u, "leonardo.ai") {
			if u == last {
				if stable++; stable >= 1 {
					return nil
				}
			} else {
				stable = 0
				last = u
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	if last != "" {
		in.logf("页面加载完成(可能仍在跳转): %s", trimText(last, 120))
		return nil
	}
	return fmt.Errorf("导航到 %s 超时", target)
}

// submitAndAdvance 反复点主按钮直到页面进入下一步或超时。
func submitAndAdvance(ctx context.Context, page *rod.Page, advanced func() bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if advanced() {
			return nil
		}
		clickPrimary(page)
		for i := 0; i < 24; i++ {
			if advanced() {
				return nil
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	if advanced() {
		return nil
	}
	return fmt.Errorf("提交后页面未进入下一步")
}

// clickPrimary 点击表单主按钮：优先可点的 type=submit，否则按文本兜底。
func clickPrimary(page *rod.Page) bool {
	el, err := page.Timeout(10 * time.Second).Element(`button[type="submit"]:not([disabled])`)
	if err == nil && el != nil {
		if verr := el.WaitVisible(); verr == nil {
			if mouseClickElement(el) {
				return true
			}
			return el.Click(proto.InputMouseButtonLeft, 1) == nil
		}
	}
	return clickByText(page, `button:not([disabled])`, `continue`) ||
		clickByText(page, `button:not([disabled])`, `create account`)
}

// pageAlert 读取表单上的错误/提示文案，便于把失败原因写进日志。
func pageAlert(page *rod.Page) string {
	v, err := page.Timeout(8 * time.Second).Eval(`() => {
		const visible = el => !!(el.offsetWidth || el.offsetHeight || el.getClientRects().length);
		const el = [...document.querySelectorAll('[role="alert"], [aria-live], .text-negative')]
			.find(e => visible(e) && (e.textContent || '').trim());
		return el ? el.textContent.trim() : '';
	}`)
	if err != nil {
		return ""
	}
	return v.Value.Str()
}

/* ===== 通用小工具 ===== */

// cleanUserAgent 去掉无头 Chrome UA 里的 Headless 标记。
func cleanUserAgent(ua string) string {
	ua = strings.TrimSpace(ua)
	ua = strings.ReplaceAll(ua, "HeadlessChrome", "Chrome")
	ua = strings.ReplaceAll(ua, "Headless", "")
	ua = strings.Join(strings.Fields(ua), " ")
	if !strings.Contains(ua, "Chrome/") {
		return userAgent
	}
	return ua
}

// platformForUA 返回与 UA 匹配的 navigator.platform：Windows UA 配 Win32 的浏览器跑在
// Linux 上时，TLS/WebGL 等原生特征与 platform 不一致，Turnstile 会判为自动化。
func platformForUA(ua string) string {
	switch {
	case strings.Contains(ua, "Windows"):
		return "Win32"
	case strings.Contains(ua, "Macintosh"):
		return "MacIntel"
	default:
		return "Linux x86_64"
	}
}

func pageURL(page *rod.Page) string {
	info, err := page.Info()
	if err != nil || info == nil {
		return ""
	}
	return info.URL
}

func hasSel(page *rod.Page, selector string) bool {
	has, _, err := page.Has(selector)
	if err != nil {
		return false
	}
	return has
}

// hasVisibleSel 判断选择器是否命中「可见」元素；登录页会把各步骤输入框都预置在
// DOM 里但只显示当前步骤，仅凭 hasSel 会误判。
func hasVisibleSel(page *rod.Page, selector string) bool {
	ok, err := page.Timeout(6*time.Second).Eval(`selector => {
		const visible = e => !!(e.offsetWidth || e.offsetHeight || e.getClientRects().length);
		return [...document.querySelectorAll(selector)].some(visible);
	}`, selector)
	if err != nil {
		return false
	}
	return ok.Value.Bool()
}

// dismissCookieBanner 关闭 cookie 同意弹窗，避免遮挡表单与按钮点击。
func dismissCookieBanner(page *rod.Page) {
	clickByText(page, `button`, `accept all`)
}

// fillInput 往输入框写入文本：优先逐字符人工输入（真实按键节奏），未生效时退回
// 原生 setter 兜底。每步都有独立超时并在总预算内重试。
func fillInput(ctx context.Context, page *rod.Page, selector, value string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = func() error {
			el, err := waitVisible(page, selector, 12*time.Second)
			if err != nil {
				return err
			}
			if err := typeHuman(el, value); err != nil {
				return err
			}
			if inputValue(page, selector) == value {
				return nil
			}
			if err := setInputValue(page, selector, value); err != nil {
				return err
			}
			if got := inputValue(page, selector); got != value {
				return fmt.Errorf("写入后内容不符(实际长度 %d)", len(got))
			}
			return nil
		}()
		if lastErr == nil {
			return nil
		}
		time.Sleep(400 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("等待输入框超时")
	}
	return lastErr
}

// setInputValue 聚焦输入框并用原生 setter 赋值、派发 input/change，兼容 React 受控组件。
func setInputValue(page *rod.Page, selector, value string) error {
	ok, err := page.Timeout(10*time.Second).Eval(`(selector, value) => {
		const els = [...document.querySelectorAll(selector)];
		const visible = e => !!(e.offsetWidth || e.offsetHeight || e.getClientRects().length);
		const el = els.find(visible) || els[0];
		if (!el) return false;
		el.focus();
		const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
		setter.call(el, value);
		el.dispatchEvent(new Event('input', { bubbles: true }));
		el.dispatchEvent(new Event('change', { bubbles: true }));
		return true;
	}`, selector, value)
	if err != nil {
		return err
	}
	if !ok.Value.Bool() {
		return fmt.Errorf("未找到输入框 %s", selector)
	}
	return nil
}

func inputValue(page *rod.Page, selector string) string {
	got, err := page.Timeout(8*time.Second).Eval(`selector => {
		const els = [...document.querySelectorAll(selector)];
		const visible = e => !!(e.offsetWidth || e.offsetHeight || e.getClientRects().length);
		const el = els.find(visible) || els[0];
		return el ? el.value : '';
	}`, selector)
	if err != nil {
		return ""
	}
	return got.Value.Str()
}

// waitVisible 等待选择器命中的「可见」元素。
func waitVisible(page *rod.Page, selector string, timeout time.Duration) (*rod.Element, error) {
	deadline := time.Now().Add(timeout)
	for {
		els, err := page.Timeout(6 * time.Second).Elements(selector)
		if err == nil {
			for _, el := range els {
				if v, verr := el.Visible(); verr == nil && v {
					return el.CancelTimeout().Timeout(15 * time.Second), nil
				}
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("等待可见元素超时: %s", selector)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func waitCleared(ctx context.Context, page *rod.Page, timeout time.Duration, cleared func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		if cleared() {
			return true
		}
		_ = page
		time.Sleep(300 * time.Millisecond)
	}
	return cleared()
}

// clickByText 点击选择器命中的、可见且文本匹配（大小写不敏感）的第一个元素。
func clickByText(page *rod.Page, selector, lowerText string) bool {
	ok, err := page.Eval(`(selector, needle) => {
		const visible = el => !!(el.offsetWidth || el.offsetHeight || el.getClientRects().length);
		const el = [...document.querySelectorAll(selector)].find(e =>
			visible(e) && (e.textContent || '').trim().toLowerCase().includes(needle));
		if (!el) return false;
		el.click();
		return true;
	}`, selector, lowerText)
	if err != nil {
		return false
	}
	return ok.Value.Bool()
}

func typeHuman(el *rod.Element, text string) error {
	if el == nil {
		return fmt.Errorf("nil element")
	}
	_ = el.ScrollIntoView()
	if err := rod.Try(func() { el.Timeout(5 * time.Second).MustClick() }); err != nil {
		if _, ferr := el.Eval(`() => this.focus()`); ferr != nil {
			return ferr
		}
	}
	_ = el.SelectAllText()
	_ = el.Input("")
	for _, r := range text {
		if err := el.Input(string(r)); err != nil {
			return err
		}
		time.Sleep(25*time.Millisecond + time.Duration(ri(45))*time.Millisecond)
	}
	return nil
}

func mouseClickElement(el *rod.Element) bool {
	if el == nil {
		return false
	}
	_ = el.ScrollIntoView()
	shape, err := el.Shape()
	if err != nil || shape == nil {
		return el.Click(proto.InputMouseButtonLeft, 1) == nil
	}
	pt := shape.OnePointInside()
	if pt == nil {
		if box := shape.Box(); box != nil {
			return mouseClickAt(el.Page(), box.X+box.Width/2, box.Y+box.Height/2)
		}
		return el.Click(proto.InputMouseButtonLeft, 1) == nil
	}
	return mouseClickAt(el.Page(), pt.X, pt.Y)
}

func mouseClickAt(page *rod.Page, x, y float64) bool {
	if x < 0 || y < 0 {
		return false
	}
	mouse := page.Mouse
	if err := mouse.MoveLinear(proto.NewPoint(x, y), 8+ri(8)); err != nil {
		if err2 := mouse.MoveTo(proto.NewPoint(x, y)); err2 != nil {
			return cdpClick(page, x, y)
		}
	}
	time.Sleep(40*time.Millisecond + time.Duration(ri(90))*time.Millisecond)
	if err := mouse.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return cdpClick(page, x, y)
	}
	return true
}

func cdpClick(page *rod.Page, x, y float64) bool {
	_ = (proto.InputDispatchMouseEvent{
		Type: proto.InputDispatchMouseEventTypeMouseMoved,
		X:    x, Y: y,
	}).Call(page)
	time.Sleep(30 * time.Millisecond)
	_ = (proto.InputDispatchMouseEvent{
		Type: proto.InputDispatchMouseEventTypeMousePressed,
		X:    x, Y: y,
		Button:     proto.InputMouseButtonLeft,
		ClickCount: 1,
	}).Call(page)
	time.Sleep(40*time.Millisecond + time.Duration(ri(40))*time.Millisecond)
	err := (proto.InputDispatchMouseEvent{
		Type: proto.InputDispatchMouseEventTypeMouseReleased,
		X:    x, Y: y,
		Button:     proto.InputMouseButtonLeft,
		ClickCount: 1,
	}).Call(page)
	return err == nil
}

// leonardoChromiumBin 在 Leonardo 专用 rod 目录（browser-leonardo）管理 Chromium，
// 与其它平台各自隔离，彼此不共享浏览器二进制或用户目录。
func leonardoChromiumBin() (string, error) {
	b := launcher.NewBrowser()
	b.RootDir = filepath.Join(filepath.Dir(launcher.DefaultBrowserDir), "browser-leonardo")
	return b.Get()
}

func availableLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err = ln.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

func normalizeProxy(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "://") {
		return raw
	}
	parts := strings.Split(raw, ":")
	switch len(parts) {
	case 2:
		return "http://" + parts[0] + ":" + parts[1]
	case 4:
		return "http://" + url.QueryEscape(parts[2]) + ":" + url.QueryEscape(parts[3]) + "@" + parts[0] + ":" + parts[1]
	default:
		return "http://" + raw
	}
}

func parseProxy(raw string) (server, user, pass string, err error) {
	u, err := url.Parse(normalizeProxy(raw))
	if err != nil {
		return "", "", "", err
	}
	if u.Host == "" {
		return "", "", "", fmt.Errorf("代理缺少 host: %s", raw)
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "http"
	}
	server = scheme + "://" + u.Host
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	return server, user, pass, nil
}

func trimText(s string, n int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// x11 显示上可摆放的窗口格子：可见模式下每个注册任务占一格，避免窗口重叠导致
// 真光标点击点到别的任务。格子数即可见模式下的实际并发上限。
const (
	windowSlotCols = 3
	windowSlotRows = 2
	windowSlotW    = 1180
	windowSlotH    = 960
)

var (
	windowSlotMu   sync.Mutex
	windowSlotUsed [windowSlotCols * windowSlotRows]bool
)

// acquireWindowSlot 占一个空格子，没有空格子时返回 -1。
func acquireWindowSlot() int {
	windowSlotMu.Lock()
	defer windowSlotMu.Unlock()
	for i, used := range windowSlotUsed {
		if !used {
			windowSlotUsed[i] = true
			return i
		}
	}
	return -1
}

func releaseWindowSlot(slot int) {
	windowSlotMu.Lock()
	defer windowSlotMu.Unlock()
	if slot >= 0 && slot < len(windowSlotUsed) {
		windowSlotUsed[slot] = false
	}
}

// placeBrowserWindow 把浏览器窗口摆到指定格子。
func placeBrowserWindow(browser *rod.Browser, page *rod.Page, slot int, in Input) {
	target, err := proto.BrowserGetWindowForTarget{TargetID: page.TargetID}.Call(browser)
	if err != nil || target == nil {
		in.logf("获取浏览器窗口失败，窗口位置按默认: %v", err)
		return
	}
	left := (slot % windowSlotCols) * windowSlotW
	top := (slot / windowSlotCols) * windowSlotH
	width, height := windowSlotW-20, windowSlotH-20
	if err = (proto.BrowserSetWindowBounds{
		WindowID: target.WindowID,
		Bounds: &proto.BrowserBounds{
			Left:        &left,
			Top:         &top,
			Width:       &width,
			Height:      &height,
			WindowState: proto.BrowserWindowStateNormal,
		},
	}).Call(browser); err != nil {
		in.logf("摆放浏览器窗口失败: %v", err)
	}
}
