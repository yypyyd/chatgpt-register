package adobereg

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	launcherflags "github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
)

const (
	signInURL  = "https://account.adobe.com/"
	fireflyURL = "https://firefly.adobe.com/generate/images"
)

func registerBrowser(ctx context.Context, in Input) (res *Result, err error) {
	if in.Headless {
		in.logf("启动无头浏览器，打开 Adobe 注册页")
	} else {
		in.logf("启动可见浏览器，打开 Adobe 注册页")
	}

	// 与 grokreg 一致：删掉 rod 默认追加的一批自动化特征标志，降低被反爬识别的概率。
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
	l = l.
		Headless(in.Headless).
		NoSandbox(true).
		Set("no-default-browser-check").
		Set("disable-suggestions-ui").
		Set("no-first-run").
		Set("disable-infobars").
		Set("disable-popup-blocking").
		Set("hide-crash-restore-bubble").
		Set("disable-features", "PrivacySandboxSettings4")
	debugPort, perr := availableLoopbackPort()
	if perr != nil {
		return nil, fmt.Errorf("分配 Chrome 调试端口失败: %w", perr)
	}
	l = l.Set("remote-debugging-port", strconv.Itoa(debugPort))
	if chromePath, cerr := adobeChromiumBin(); cerr != nil {
		in.logf("准备 Adobe 专用 Chromium 失败，回退默认浏览器: %v", cerr)
	} else {
		l = l.Bin(chromePath)
		in.logf("使用 Adobe 专用 Chromium，与 GPT/Grok 浏览器隔离")
	}

	var authBridge *localAuthProxyBridge
	proxyConfigured := strings.TrimSpace(in.Proxy) != ""
	if proxyConfigured {
		server, user, pass, perr := parseProxy(in.Proxy)
		if perr != nil {
			return nil, fmt.Errorf("解析代理失败: %w", perr)
		}
		if user != "" || pass != "" {
			authBridge, server, perr = startLocalAuthProxyBridge(in.Proxy)
			if perr != nil {
				return nil, fmt.Errorf("启动认证代理桥失败: %w", perr)
			}
			defer authBridge.Close()
			in.logf("已启用 Chromium 本地认证代理桥")
		}
		l = l.Set("proxy-server", server)
		in.logf("使用代理: %s", server)
	}

	controlURL, err := l.Launch()
	if err != nil {
		return nil, fmt.Errorf("启动 Chrome 失败: %w", err)
	}
	browser := rod.New().NoDefaultDevice().ControlURL(controlURL)
	if err = browser.Connect(); err != nil {
		return nil, fmt.Errorf("连接 Chrome 失败: %w", err)
	}
	defer browser.MustClose()

	var page *rod.Page
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("Adobe 注册流程异常: %v", r)
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
	if proxyConfigured {
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
	_ = (proto.EmulationSetDeviceMetricsOverride{
		Width:             1280,
		Height:            900,
		DeviceScaleFactor: 1,
		Mobile:            false,
	}).Call(page)

	if err = gotoStable(ctx, page, signInURL, in, 120*time.Second); err != nil {
		return nil, err
	}
	in.logf("Adobe 登录页已加载")

	if err = gotoCreateForm(ctx, page, in); err != nil {
		return nil, err
	}
	if err = fillStep1(ctx, page, in); err != nil {
		return nil, err
	}
	// 步骤顺序自适应：姓名/生日步与邮箱验证可能以任意顺序出现。
	if err = completeSignup(ctx, page, in); err != nil {
		return nil, err
	}

	in.logf("账号创建完成，打开 Firefly 确认会话")
	if err = gotoStable(ctx, page, fireflyURL, in, 120*time.Second); err != nil {
		in.logf("打开 Firefly 时页面跳转异常，继续检测验证页: %v", err)
	}
	if err = handleEmailVerification(ctx, page, in); err != nil {
		return nil, err
	}
	if err = waitFireflyReady(ctx, page, in, 120*time.Second); err != nil {
		in.logf("等待 Firefly 就绪超时（账号已创建），继续采集会话: %v", err)
	}

	auth, cerr := captureAuth(page, in)
	if cerr != nil {
		return nil, cerr
	}
	return &Result{AuthJSON: auth}, nil
}

// gotoStable 导航到目标 URL 并容忍 Adobe 的多级跳转：
// account.adobe.com 会连续重定向到 IMS 登录域，期间 CDP 目标可能短暂
// 报「target navigated or closed」，这里吞掉瞬时错误，轮询到 URL 稳定在
// adobe 域后返回，避免误判为注册失败。
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
		if strings.Contains(u, "adobe.com") {
			if u == last {
				if stable++; stable >= 2 {
					return nil
				}
			} else {
				stable = 0
				last = u
			}
		}
		time.Sleep(1500 * time.Millisecond)
	}
	if last != "" {
		in.logf("页面加载完成(可能仍在跳转): %s", trimText(last, 120))
		return nil
	}
	return fmt.Errorf("导航到 %s 超时", target)
}

// gotoCreateForm 从登录页进入「创建账号」表单（同时出现邮箱与密码输入框）。
func gotoCreateForm(ctx context.Context, page *rod.Page, in Input) error {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if hasSel(page, `input[name="username"]`) && hasSel(page, `input[name="password"]`) {
			in.logf("已进入创建账号表单")
			return nil
		}
		clickByText(page, `a,span,button`, `create an account`)
		time.Sleep(1500 * time.Millisecond)
	}
	return fmt.Errorf("未能进入 Adobe 创建账号表单")
}

func fillStep1(ctx context.Context, page *rod.Page, in Input) error {
	if err := fillInput(ctx, page, `input[name="username"]`, in.Email, 60*time.Second); err != nil {
		return fmt.Errorf("输入邮箱失败: %w", err)
	}
	if err := fillInput(ctx, page, `input[name="password"]`, in.Password, 45*time.Second); err != nil {
		return fmt.Errorf("输入密码失败: %w", err)
	}
	in.logf("已填写邮箱与密码，提交第一步")
	// 提交后等待离开邮箱/密码步：出现姓名框、出现验证码框，或密码框消失。
	leftStep1 := func() bool {
		return hasSel(page, `input[name="firstname"]`) ||
			onEmailVerify(page, pageURL(page)) ||
			!hasSel(page, `input[name="password"]`)
	}
	if err := submitAndAdvance(ctx, page, in, leftStep1, 60*time.Second); err != nil {
		return fmt.Errorf("提交第一步失败: %w", err)
	}
	return nil
}

// completeSignup 在第一步之后自适应处理后续步骤：邮箱验证与姓名/生日步
// 可能以任意顺序出现，循环处理直到两者都已完成、页面离开注册表单。
func completeSignup(ctx context.Context, page *rod.Page, in Input) error {
	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		u := pageURL(page)
		if onEmailVerify(page, u) {
			if err := handleEmailVerification(ctx, page, in); err != nil {
				return err
			}
			continue
		}
		if hasSel(page, `input[name="firstname"]`) {
			if err := fillStep2(ctx, page, in); err != nil {
				return err
			}
			continue
		}
		if !strings.Contains(u, "signup") && !strings.Contains(u, "create-account") &&
			!strings.Contains(u, "#/create") {
			in.logf("注册表单已完成: %s", trimText(u, 120))
			return nil
		}
		time.Sleep(1500 * time.Millisecond)
	}
	return fmt.Errorf("完成注册步骤超时（可能遇到额外校验）")
}

func fillStep2(ctx context.Context, page *rod.Page, in Input) error {
	if err := fillInput(ctx, page, `input[name="firstname"]`, in.FirstName, 60*time.Second); err != nil {
		return fmt.Errorf("输入名字失败: %w", err)
	}
	if err := fillInput(ctx, page, `input[name="lastname"]`, in.LastName, 45*time.Second); err != nil {
		return fmt.Errorf("输入姓氏失败: %w", err)
	}

	month := ri(12)
	year := GenBirthYear()
	set, evErr := page.Eval(`(month, year) => {
		const setNative = (el, val) => {
			if (!el) return;
			const proto = el.tagName === 'SELECT' ? HTMLSelectElement.prototype : HTMLInputElement.prototype;
			const setter = Object.getOwnPropertyDescriptor(proto, 'value').set;
			setter.call(el, val);
			el.dispatchEvent(new Event('input', { bubbles: true }));
			el.dispatchEvent(new Event('change', { bubbles: true }));
		};
		const mo = document.querySelector('select[name="month"]');
		const yr = document.querySelector('input[name="bday-year"]');
		const cc = document.querySelector('select[name="countryCode"]');
		setNative(mo, String(month));
		setNative(yr, String(year));
		if (cc && cc.value !== 'US') setNative(cc, 'US');
		return JSON.stringify({ mo: mo && mo.value, yr: yr && yr.value, cc: cc && cc.value });
	}`, month, year)
	if evErr != nil {
		return fmt.Errorf("填写生日/地区失败: %w", evErr)
	}
	in.logf("已填写姓名与生日(%d/%d)/地区: %s", month+1, year, trimText(set.Value.Str(), 80))

	// 提交后等待离开姓名步（姓名框消失）或出现邮箱验证。
	leftStep2 := func() bool {
		return !hasSel(page, `input[name="firstname"]`) || onEmailVerify(page, pageURL(page))
	}
	if err := submitAndAdvance(ctx, page, in, leftStep2, 60*time.Second); err != nil {
		return fmt.Errorf("提交创建账号失败: %w", err)
	}
	in.logf("已提交创建账号")
	return nil
}

// submitAndAdvance 点击当前表单主 CTA（Continue / Create account）并确认页面
// 真正进入下一步；Adobe 的提交按钮在表单校验通过前为 disabled，且偶尔首次
// 点击不生效，因此等按钮可点后点击，未推进则重试。
func submitAndAdvance(ctx context.Context, page *rod.Page, in Input, advanced func() bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if advanced() {
			return nil
		}
		clickPrimary(page)
		for i := 0; i < 8; i++ {
			if advanced() {
				return nil
			}
			time.Sleep(1 * time.Second)
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
	return clickByText(page, `button`, `continue`) || clickByText(page, `button`, `create account`)
}

// handleEmailVerification 处理 Adobe「验证您的身份」邮箱验证码页面（自动取码填入）。
func handleEmailVerification(ctx context.Context, page *rod.Page, in Input) error {
	for attempt := 0; attempt < 2; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !onEmailVerify(page, pageURL(page)) {
			return nil
		}
		in.logf("检测到 Adobe 邮箱验证码页面，开始自动读取验证码")
		code, err := in.WaitCode(ctx)
		if err != nil {
			return fmt.Errorf("获取邮箱验证码失败: %w", err)
		}
		if err = fillInput(ctx, page, `input.PinInput-Input`, code, 45*time.Second); err != nil {
			return fmt.Errorf("填写验证码失败: %w", err)
		}
		// 6 位填满后 Adobe 自动提交；补按一次回车兜底。
		if pin, perr := waitVisible(page, `input.PinInput-Input`, 10*time.Second); perr == nil {
			_ = pin.Type(input.Enter)
		}
		if waitCleared(ctx, page, 40*time.Second, func() bool { return !onEmailVerify(page, pageURL(page)) }) {
			in.logf("邮箱验证码校验通过")
			return nil
		}
		in.logf("验证码提交后仍停留在验证页，重试")
	}
	return fmt.Errorf("邮箱验证码校验未通过")
}

func waitFireflyReady(ctx context.Context, page *rod.Page, in Input, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		u := pageURL(page)
		if onEmailVerify(page, u) {
			time.Sleep(1 * time.Second)
			continue
		}
		if strings.Contains(u, "firefly.adobe.com") || strings.Contains(u, "account.adobe.com") {
			in.logf("Firefly/Adobe 会话就绪: %s", trimText(u, 120))
			return nil
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("等待 Firefly 就绪超时")
}

func captureAuth(page *rod.Page, in Input) (map[string]any, error) {
	if !strings.Contains(pageURL(page), "adobe.com") {
		_ = gotoStable(context.Background(), page, "https://firefly.adobe.com/", in, 60*time.Second)
	}

	all, err := proto.NetworkGetAllCookies{}.Call(page)
	if err != nil {
		return nil, fmt.Errorf("读取 Cookie 失败: %w", err)
	}
	cookieList := make([]map[string]any, 0, len(all.Cookies))
	for _, c := range all.Cookies {
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

	storageRaw := page.MustEval(`() => JSON.stringify({
		localStorage: Object.fromEntries(Object.entries(localStorage)),
		sessionStorage: Object.fromEntries(Object.entries(sessionStorage)),
		location: location.href
	})`).String()
	var storage map[string]any
	_ = json.Unmarshal([]byte(storageRaw), &storage)

	return map[string]any{
		"auth_mode":   "adobe_browser_session",
		"platform":    "adobe",
		"product":     "firefly",
		"email":       in.Email,
		"first_name":  in.FirstName,
		"last_name":   in.LastName,
		"captured_at": time.Now().UTC().Format(time.RFC3339),
		"cookies":     cookieList,
		"storage":     storage,
	}, nil
}

/* ===== 通用小工具 ===== */

func pageURL(page *rod.Page) string {
	info, err := page.Info()
	if err != nil || info == nil {
		return ""
	}
	return info.URL
}

// onEmailVerify 判断当前是否停留在 Adobe 邮箱验证码页面。
func onEmailVerify(page *rod.Page, u string) bool {
	if strings.Contains(u, "email-verification") {
		return true
	}
	return hasSel(page, `input.PinInput-Input`)
}

func hasSel(page *rod.Page, selector string) bool {
	has, _, err := page.Has(selector)
	if err != nil {
		return false
	}
	return has
}

// fillInput 往输入框写入文本：每次尝试都重新定位元素并使用独立的超时，避免
// 元素继承等待阶段的绝对 deadline（此前邮箱框会因此报 context deadline
// exceeded）；React 重渲染导致节点失效时重试，写入后校验实际值，连续失败
// 后改用原生 setter + 事件派发兜底。
func fillInput(ctx context.Context, page *rod.Page, selector, value string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = func() error {
			if attempt < 2 {
				el, err := waitVisible(page, selector, 20*time.Second)
				if err != nil {
					return err
				}
				if err = typeHuman(el, value); err != nil {
					return err
				}
			} else if err := setInputValue(page, selector, value); err != nil {
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
		time.Sleep(1 * time.Second)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("等待输入框超时")
	}
	return lastErr
}

// setInputValue 用原生 setter 赋值并派发 input/change，兼容 React 受控组件。
func setInputValue(page *rod.Page, selector, value string) error {
	ok, err := page.Eval(`(selector, value) => {
		const el = document.querySelector(selector);
		if (!el) return false;
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
	got, err := page.Eval(`selector => {
		const el = document.querySelector(selector);
		return el ? el.value : '';
	}`, selector)
	if err != nil {
		return ""
	}
	return got.Value.Str()
}

func waitVisible(page *rod.Page, selector string, timeout time.Duration) (*rod.Element, error) {
	el, err := page.Timeout(timeout).Element(selector)
	if err != nil {
		return nil, err
	}
	if err = el.WaitVisible(); err != nil {
		return nil, err
	}
	return el, nil
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
		time.Sleep(1 * time.Second)
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
	if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return err
	}
	_ = el.SelectAllText()
	_ = el.Input("")
	for _, r := range text {
		if err := el.Input(string(r)); err != nil {
			return err
		}
		time.Sleep(40*time.Millisecond + time.Duration(ri(70))*time.Millisecond)
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

// adobeChromiumBin 在 Adobe 专用 rod 目录（browser-adobe）管理 Chromium，
// 与 GPT/Grok 流程各自隔离，彼此不共享浏览器二进制或用户目录。
func adobeChromiumBin() (string, error) {
	b := launcher.NewBrowser()
	b.RootDir = filepath.Join(filepath.Dir(launcher.DefaultBrowserDir), "browser-adobe")
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
