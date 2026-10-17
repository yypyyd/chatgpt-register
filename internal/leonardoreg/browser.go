package leonardoreg

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"chatgpt-register/internal/proxyutil"
	"chatgpt-register/internal/turnstilepatch"

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

	// 登录页用于显示邮箱入口的输入框。
	selEmail = `#email-field`

	// stuckReloadAfter 登录页卡住（SPA 只转圈、没渲染出邮箱框）多久后重新加载页面。
	stuckReloadAfter = 25 * time.Second
)

// launchLeonardoBrowser 按注册用的一整套反爬/代理配置启动并连接 Leonardo 专用 Chromium。
// 返回的 browser 由调用方负责关闭；若返回了 bridge（认证代理桥），也要一并 Close；
// cleanup 在浏览器关闭后调用，负责清理 launcher 的临时用户数据目录。
func launchLeonardoBrowser(in Input) (browser *rod.Browser, bridge *proxyutil.AuthBridge, cleanup func(), err error) {
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
	patchDir, perr := turnstilepatch.Extract("leonardo-turnstilepatch-*")
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
		server, user, pass, perr := proxyutil.Parse(in.Proxy)
		if perr != nil {
			return nil, nil, nil, fmt.Errorf("解析代理失败: %w", perr)
		}
		if user != "" || pass != "" {
			upstreamServer := server
			bridge, server, perr = proxyutil.StartAuthBridge(in.Proxy)
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

// hasVisibleSel 判断选择器是否命中「可见」元素；登录页会把各步骤输入框都预置在
// DOM 里但只显示当前步骤，需要检查可见性。
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
