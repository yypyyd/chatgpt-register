package luminareg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	launcherflags "github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
)

// ErrEmailTaken 该邮箱已有 BytePlus 账号，属于永久失败：换出口/重试都没用，
// 上层应标记终态而不是过冷却后再重试。
var ErrEmailTaken = errors.New("该邮箱已注册 BytePlus")

// ErrRegionBlocked 当前出口 IP 不在 BytePlus Lumina 的开放地区，页面直接跳到
// app-unavailable，换代理出口才可能继续。
var ErrRegionBlocked = errors.New("BytePlus Lumina 在当前出口地区不可用")

// ErrCaptchaFailed 滑块人机校验没过。BytePlus 会在多次失败后临时限流，
// 上层按普通失败处理（可换出口后重试）。
var ErrCaptchaFailed = errors.New("BytePlus 滑块人机校验未通过")

// ErrRateLimited 注册接口被 BytePlus 限流（同一出口 IP 注册过密），
// 页面不显示任何提示；换出口 IP 或过冷却后重试。
var ErrRateLimited = errors.New("BytePlus 注册接口限流")

const (
	luminaURL = "https://ai.byteplus.com/lumina/en"

	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

	// BytePlus Passport 登录/注册面板的输入框（id 稳定）。
	selIdentity = `#Identity_input`
	selPassword = `#Password_input`
	selCode     = `#verifyInput`

	// 滑块验证码元素（BytePlus secsdk 组件）。
	selCaptchaBG    = `img.captcha-verify-image`
	selCaptchaPiece = `img.captcha-verify-image-slide`
	selCaptchaKnob  = `.captcha-slider-btn`

	// captchaTries 滑块最多尝试次数：每次失败组件会自动换一张图。
	captchaTries = 8
)

// trackerHosts 埋点/监控域名：对注册流程无用，但一样走代理出口。
var trackerHosts = []string{
	"mcs.byteoversea.com",
	"mon.byteoversea.com",
	"slardar",
	"google-analytics.com",
	"googletagmanager.com",
	"doubleclick.net",
	"facebook.net",
	"clarity.ms",
	"bat.bing.com",
	"hotjar",
}

// launchLuminaBrowser 启动并连接 Lumina 专用 Chromium（与其它平台浏览器目录隔离）。
// 返回的 browser 由调用方关闭；若返回了 bridge（认证代理桥）也要一并 Close；
// cleanup 在浏览器关闭后调用，清理 launcher 的临时用户数据目录。
func launchLuminaBrowser(in Input) (browser *rod.Browser, bridge *localAuthProxyBridge, cleanup func(), err error) {
	// 与其它平台一致：删掉 rod 默认追加的一批自动化特征标志。
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
		l = l.Set("headless", "new")
	} else {
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
		Set("lang", "en-US").
		Set("disable-features", "PrivacySandboxSettings4")
	debugPort, dperr := availableLoopbackPort()
	if dperr != nil {
		return nil, nil, nil, fmt.Errorf("分配 Chrome 调试端口失败: %w", dperr)
	}
	l = l.Set("remote-debugging-port", strconv.Itoa(debugPort))
	// HTTP 磁盘缓存跨注册复用：用户目录每次都是临时新建的（cookie 不串号），
	// 但 BytePlus 的 JS 包十几 MB，固定缓存目录能让后续注册只下增量。
	if cacheDir := luminaCacheDir(); cacheDir != "" {
		l = l.Set("disk-cache-dir", cacheDir)
	}
	if chromePath, cerr := luminaChromiumBin(); cerr != nil {
		in.logf("准备 Lumina 专用 Chromium 失败，回退默认浏览器: %v", cerr)
	} else {
		l = l.Bin(chromePath)
		in.logf("使用 Lumina 专用 Chromium，与其它平台浏览器隔离")
	}

	if strings.TrimSpace(in.Proxy) != "" {
		server, user, pass, perr := parseProxy(in.Proxy)
		if perr != nil {
			return nil, nil, nil, fmt.Errorf("解析代理失败: %w", perr)
		}
		// 无认证代理也走本地桥：除了补认证，桥上还带每次注册的代理流量计量。
		upstreamServer := server
		if bridged, local, berr := startLocalAuthProxyBridge(in.Proxy); berr == nil {
			bridge, server = bridged, local
			in.logf("已启用 Chromium 本地代理桥（含流量计量），本地 %s → 上游 %s", server, upstreamServer)
		} else if user != "" || pass != "" {
			return nil, nil, nil, fmt.Errorf("启动认证代理桥失败: %w", berr)
		} else {
			in.logf("本地代理桥启动失败，直连上游代理（本次无流量计量）: %v", berr)
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
	return browser, bridge, l.Cleanup, nil
}

// blockResources 丢弃图片/媒体/字体与埋点请求，脚本/样式改由本地静态缓存直连
// 供给，省掉代理上的整页素材流量。登录面板会开新标签页，所以挂在 browser 级；
// 滑块阶段 readCaptcha 需要图片真正加载，由 in.mediaBlock 临时关掉屏蔽。
func blockResources(browser *rod.Browser, in Input, sc *staticCache) func() {
	router := browser.HijackRequests()
	router.MustAdd("*", func(h *rod.Hijack) {
		if isTracker(h.Request.URL().Host) {
			h.Response.Fail(proto.NetworkErrorReasonBlockedByClient)
			return
		}
		if in.mediaBlock != nil && in.mediaBlock.Load() {
			switch h.Request.Type() {
			case proto.NetworkResourceTypeImage,
				proto.NetworkResourceTypeMedia,
				proto.NetworkResourceTypeFont:
				h.Response.Fail(proto.NetworkErrorReasonBlockedByClient)
				return
			}
		}
		if sc.eligible(h) && sc.serve(h, in) {
			return
		}
		h.ContinueRequest(&proto.FetchContinueRequest{})
	})
	go router.Run()
	in.logf("已开启资源屏蔽: image/media/font 与埋点（滑块阶段自动放行图片），脚本/样式走本地静态缓存")
	return router.MustStop
}

func isTracker(host string) bool {
	host = strings.ToLower(host)
	for _, h := range trackerHosts {
		if strings.Contains(host, h) {
			return true
		}
	}
	return false
}

// allowMedia 在滑块阶段放行图片，返回恢复屏蔽的函数。
func allowMedia(in Input) func() {
	if in.mediaBlock == nil || !in.mediaBlock.CompareAndSwap(true, false) {
		return func() {}
	}
	return func() { in.mediaBlock.Store(true) }
}

func registerBrowser(ctx context.Context, in Input) (res *Result, err error) {
	if in.Headless {
		in.logf("启动无头浏览器，打开 Lumina 首页")
	} else {
		in.logf("启动可见浏览器，打开 Lumina 首页")
	}

	browser, authBridge, cleanup, err := launchLuminaBrowser(in)
	if err != nil {
		return nil, err
	}
	proxyConfigured := strings.TrimSpace(in.Proxy) != ""
	if authBridge != nil {
		defer authBridge.Close()
	}
	defer func() {
		_ = rod.Try(browser.MustClose)
		cleanup()
	}()

	in.mediaBlock = &atomic.Bool{}
	in.mediaBlock.Store(true)
	sc := newStaticCache()
	if sc == nil {
		in.logf("静态资源缓存目录不可用，脚本/样式回退代理拉取")
	}
	// 拦截器要在浏览器关闭前停掉（defer 逆序），否则 CDP 连接已断，MustStop 会 panic。
	stopBlock := blockResources(browser, in, sc)
	defer func() { _ = rod.Try(stopBlock) }()
	defer func() {
		if sc != nil {
			in.logf("%s", sc.summary())
		}
		if authBridge != nil {
			up, down := authBridge.Traffic()
			in.logf("本次注册代理流量: 上行 %s / 下行 %s", humanBytes(up), humanBytes(down))
		}
	}()

	var page *rod.Page
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("Lumina 注册流程异常: %v", r)
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
		// 多个任务共用一个 X 显示，窗口默认叠在同一位置，真光标拖动会拖到别人的
		// 窗口上，按格子摆开各自独占一块屏幕。
		if slot := acquireWindowSlot(); slot >= 0 {
			defer releaseWindowSlot(slot)
			placeBrowserWindow(browser, page, slot, in)
		} else {
			in.logf("可见窗口格子已用满，窗口可能与其它任务重叠")
		}
	}
	if in.Headless {
		_ = (proto.EmulationSetDeviceMetricsOverride{
			Width:             1440,
			Height:            900,
			DeviceScaleFactor: 1,
			Mobile:            false,
		}).Call(page)
		// 无头 Chrome 的 UA 带 HeadlessChrome 标记，BytePlus 风控会直接拦下。
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

	if err = gotoStable(ctx, page, luminaURL, in, 120*time.Second); err != nil {
		return nil, err
	}
	if strings.Contains(pageURL(page), "app-unavailable") {
		return nil, ErrRegionBlocked
	}
	in.logf("Lumina 首页已加载")

	panelPage, perr := openLoginPanel(ctx, browser, page, in)
	if perr != nil {
		return nil, perr
	}
	page = panelPage
	if err = submitIdentity(ctx, page, in); err != nil {
		return nil, err
	}
	if err = createPassword(ctx, page, in); err != nil {
		return nil, err
	}
	if err = submitEmailCode(ctx, page, in); err != nil {
		return nil, err
	}

	auth, cerr := captureAuth(ctx, page, in)
	if cerr != nil {
		return nil, cerr
	}
	return &Result{AuthJSON: auth}, nil
}

// openLoginPanel 关掉首页弹窗后点开 Login，等 BytePlus Passport 面板渲染出邮箱输入框。
// 首页前端脚本经代理加载慢，入口点击在脚本就绪前无效，超时前重载一次再试。
func openLoginPanel(ctx context.Context, browser *rod.Browser, page *rod.Page, in Input) (*rod.Page, error) {
	start := time.Now()
	deadline := start.Add(180 * time.Second)
	reloadAt := start.Add(75 * time.Second)
	reloaded := false
	clicks := 0
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if hasVisibleSel(page, selIdentity) {
			in.logf("已打开 BytePlus 登录面板（点击 %d 次）", clicks)
			return page, nil
		}
		// 登录入口可能开新标签页跳 BytePlus Passport，命中就切过去。
		if p := findIdentityPage(browser, page); p != nil {
			in.logf("BytePlus 登录页在新标签页打开: %s", trimText(pageURL(p), 120))
			return p, nil
		}
		if strings.Contains(pageURL(page), "app-unavailable") {
			return nil, ErrRegionBlocked
		}
		if !reloaded && time.Now().After(reloadAt) {
			reloaded = true
			in.logf("登录入口点击 %d 次仍未出面板，重载首页再试", clicks)
			if err := gotoStable(ctx, page, luminaURL, in, 90*time.Second); err != nil {
				return nil, err
			}
		}
		dismissBanners(page)
		// 首页入口文案是「Login」，有的版本写「Log in / Sign up」，逐个试；
		// 用真光标点击，纯 JS click 会被前端的手势校验忽略。
		for _, text := range []string{"login", "signup", "signin", "getstarted"} {
			if mouseClickByText(page, `button,a`, text) {
				if clicks == 0 {
					in.logf("已点击登录入口: %s", text)
				}
				clicks++
				break
			}
		}
		time.Sleep(1500 * time.Millisecond)
	}
	return nil, fmt.Errorf("未能打开 BytePlus 登录面板（当前页面: %s）", trimText(pageURL(page), 120))
}

// findIdentityPage 在所有标签页里找已渲染出邮箱输入框的那一页。
func findIdentityPage(browser *rod.Browser, current *rod.Page) *rod.Page {
	if browser == nil {
		return nil
	}
	pages, err := browser.Pages()
	if err != nil {
		return nil
	}
	for _, p := range pages {
		if current != nil && p.TargetID == current.TargetID {
			continue
		}
		if hasVisibleSel(p, selIdentity) {
			return p
		}
	}
	return nil
}

// mouseClickByText 用真光标点击文案匹配的可见元素（文本忽略大小写与空白）。
func mouseClickByText(page *rod.Page, selector, lowerText string) bool {
	els, err := page.Timeout(10 * time.Second).Elements(selector)
	if err != nil {
		return false
	}
	for _, el := range els {
		if v, verr := el.Visible(); verr != nil || !v {
			continue
		}
		text, terr := el.Text()
		if terr != nil {
			continue
		}
		if !strings.Contains(strings.Join(strings.Fields(strings.ToLower(text)), ""), lowerText) {
			continue
		}
		if mouseClickElement(el.CancelTimeout().Timeout(15 * time.Second)) {
			return true
		}
	}
	return false
}

// submitIdentity 填邮箱并继续。BytePlus 按邮箱是否已有账号分流：
// 新邮箱进入「设置密码 + Verify email address」，老邮箱进入登录密码步骤。
func submitIdentity(ctx context.Context, page *rod.Page, in Input) error {
	if err := fillInput(ctx, page, selIdentity, in.Email, 45*time.Second); err != nil {
		return fmt.Errorf("输入邮箱失败: %w", err)
	}
	in.logf("已填写邮箱，提交邮箱步骤")
	advanced := func() bool {
		return hasVisibleSel(page, selPassword) || hasVisibleSel(page, selCode)
	}
	if err := submitAndAdvance(ctx, page, advanced, `continue`, 60*time.Second); err != nil {
		return fmt.Errorf("提交邮箱失败: %w（%s）", err, trimText(pageAlert(page), 160))
	}
	if hasVisibleSel(page, selPassword) && !hasVisibleButtonText(page, `verifyemailaddress`) {
		// 面板进入的是登录密码步骤，说明该邮箱已有 BytePlus 账号。
		return fmt.Errorf("%w（页面进入登录密码步骤）", ErrEmailTaken)
	}
	return nil
}

// createPassword 在注册步骤设置密码并提交，页面随后发注册验证码邮件。
func createPassword(ctx context.Context, page *rod.Page, in Input) error {
	if !hasVisibleSel(page, selPassword) {
		if hasVisibleSel(page, selCode) {
			in.logf("面板已在验证码步骤，跳过设置密码")
			return nil
		}
		return fmt.Errorf("未出现设置密码表单")
	}
	if err := fillInput(ctx, page, selPassword, in.Password, 30*time.Second); err != nil {
		return fmt.Errorf("输入密码失败: %w", err)
	}
	// 提交按钮被禁用时才勾选表单里的同意项，避免顺手勾上营销订阅。
	if !hasVisibleButtonText(page, `verifyemailaddress`) || primaryDisabled(page) {
		if clickAgreement(page) {
			in.logf("已勾选表单同意项")
		}
	}
	in.logf("已设置密码，提交注册并请求邮箱验证码")
	if err := submitAndAdvance(ctx, page, func() bool { return hasVisibleSel(page, selCode) },
		`verifyemailaddress`, 90*time.Second); err != nil {
		return fmt.Errorf("提交注册失败: %w（%s）", err, trimText(pageAlert(page), 160))
	}
	return nil
}

// submitEmailCode 取邮箱验证码填入并提交，通过后进入滑块人机校验。
func submitEmailCode(ctx context.Context, page *rod.Page, in Input) error {
	if !hasVisibleSel(page, selCode) {
		return nil
	}
	in.logf("等待 BytePlus 邮箱验证码")
	code, err := in.WaitCode(ctx)
	if err != nil {
		return fmt.Errorf("获取邮箱验证码失败: %w", err)
	}
	if err = fillInput(ctx, page, selCode, code, 45*time.Second); err != nil {
		return fmt.Errorf("填写验证码失败: %w", err)
	}
	in.logf("已填写验证码，提交校验")
	installPassportTrace(page)
	// 限流时页面无任何提示，只能从接口响应判断；一旦命中就别再反复点提交加重限流。
	rateLimited := false
	advanced := func() bool {
		if hasCaptcha(page) || !hasVisibleSel(page, selCode) {
			return true
		}
		if strings.Contains(passportTrace(page), `"Code":"ErrorRateLimit"`) {
			rateLimited = true
			return true
		}
		return false
	}
	if err = submitAndAdvance(ctx, page, advanced, `verify`, 90*time.Second); err != nil {
		return fmt.Errorf("提交验证码失败: %w（提示: %s / 接口: %s）", err,
			trimText(pageAlert(page), 160), trimText(passportTrace(page), 1200))
	}
	if rateLimited {
		return fmt.Errorf("%w（RegisterAccountV2 返回 ErrorRateLimit，换出口 IP 或稍后重试）", ErrRateLimited)
	}
	if alert := pageAlert(page); strings.Contains(strings.ToLower(alert), "rate limit") {
		return fmt.Errorf("BytePlus 限流: %s", trimText(alert, 160))
	}
	if hasCaptcha(page) {
		if err = solveCaptcha(ctx, page, in); err != nil {
			return err
		}
	}
	return nil
}

/* ===== 滑块人机校验 ===== */

// captchaInfo 是滑块组件在页面上的实际几何信息与图片地址。
type captchaInfo struct {
	BG struct {
		X, Y, W, H float64
		Src        string
		NW         float64
	}
	Piece struct {
		X, Y, W, H float64
		Src        string
	}
	Knob struct {
		X, Y, W, H float64
	}
}

const captchaInfoJS = `() => {
	const bg = document.querySelector('img.captcha-verify-image');
	const pc = document.querySelector('img.captcha-verify-image-slide');
	const knob = document.querySelector('.captcha-slider-btn');
	if (!bg || !pc || !knob || !bg.naturalWidth) return '';
	const g = e => { const r = e.getBoundingClientRect(); return {X: r.x, Y: r.y, W: r.width, H: r.height}; };
	return JSON.stringify({
		BG: Object.assign(g(bg), {Src: bg.src, NW: bg.naturalWidth}),
		Piece: Object.assign(g(pc), {Src: pc.src}),
		Knob: g(knob),
	});
}`

func hasCaptcha(page *rod.Page) bool {
	return hasVisibleSel(page, selCaptchaBG) && hasVisibleSel(page, selCaptchaKnob)
}

func readCaptcha(page *rod.Page) (*captchaInfo, error) {
	v, err := page.Timeout(10 * time.Second).Eval(captchaInfoJS)
	if err != nil {
		return nil, err
	}
	raw := v.Value.Str()
	if raw == "" {
		return nil, nil
	}
	var info captchaInfo
	if err = json.Unmarshal([]byte(raw), &info); err != nil {
		return nil, err
	}
	if info.BG.W <= 0 || info.BG.NW <= 0 || info.Knob.W <= 0 {
		return nil, fmt.Errorf("滑块组件尺寸读取异常")
	}
	return &info, nil
}

// solveCaptcha 定位缺口并拖动滑块；每次失败组件会换图，最多试 captchaTries 次。
func solveCaptcha(ctx context.Context, page *rod.Page, in Input) error {
	// 验证码背景图/滑块图必须真正加载，否则读不到原图尺寸也算不出偏移。
	defer allowMedia(in)()
	reloadCaptchaImages(page)

	var lastErr error
	for attempt := 0; attempt < captchaTries; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		info, err := waitCaptchaReady(ctx, page, 25*time.Second)
		if err != nil {
			lastErr = err
			time.Sleep(time.Second)
			continue
		}
		if info == nil {
			in.logf("滑块校验已通过")
			return nil
		}
		// 页面把原图按比例缩放显示，匹配在原图坐标系做，拖动再换算回页面坐标。
		scale := info.BG.NW / info.BG.W
		bgImg, err := fetchImage(info.BG.Src, in.Proxy)
		if err != nil {
			lastErr = err
			continue
		}
		pieceImg, err := fetchImage(info.Piece.Src, in.Proxy)
		if err != nil {
			lastErr = err
			continue
		}
		offset, score, err := solveOffset(bgImg, pieceImg, (info.Piece.Y-info.BG.Y)*scale)
		if err != nil {
			lastErr = err
			continue
		}
		in.logf("滑块第 %d 次：横移 %d 原图像素（得分 %.1f）", attempt+1, offset, score)
		dragSlider(page, info, float64(offset)/scale)
		// 通过后组件先播一段成功动画再关闭，给足关闭时间再判失败。
		if waitCleared(ctx, page, 10*time.Second, func() bool { return !hasCaptcha(page) }) {
			in.logf("滑块校验通过")
			return nil
		}
		lastErr = fmt.Errorf("滑块位置未通过（得分 %.1f）", score)
		time.Sleep(time.Duration(1200+ri(1200)) * time.Millisecond)
	}
	if lastErr != nil {
		return fmt.Errorf("%w: %v", ErrCaptchaFailed, lastErr)
	}
	return ErrCaptchaFailed
}

// dragSlider 用真实鼠标事件把滑块拖到目标位置：先加速冲过目标再回拉并抖动，
// 匀速直线拖动会被 BytePlus 的轨迹检测判为机器操作。
func dragSlider(page *rod.Page, info *captchaInfo, dist float64) {
	mouse := page.Mouse
	sx := info.Knob.X + info.Knob.W/2
	sy := info.Knob.Y + info.Knob.H/2
	if err := mouse.MoveTo(proto.NewPoint(sx, sy)); err != nil {
		return
	}
	time.Sleep(time.Duration(120+ri(140)) * time.Millisecond)
	if err := mouse.Down(proto.InputMouseButtonLeft, 1); err != nil {
		return
	}
	steps := 30 + ri(16)
	over := 3 + float64(ri(5))
	peak := dist + over
	for i := 1; i <= steps; i++ {
		t := float64(i) / float64(steps)
		ease := 1 - math.Pow(1-t, 3)
		x := sx + peak*ease + jitter(0.8)
		y := sy + math.Sin(t*math.Pi)*(1.5+float64(ri(3))) + jitter(0.7)
		_ = mouse.MoveTo(proto.NewPoint(x, y))
		time.Sleep(time.Duration(8+ri(20)) * time.Millisecond)
	}
	for i := 1; i <= 6; i++ {
		x := sx + peak - over*float64(i)/6
		_ = mouse.MoveTo(proto.NewPoint(x, sy+jitter(0.6)))
		time.Sleep(time.Duration(20+ri(30)) * time.Millisecond)
	}
	time.Sleep(time.Duration(120+ri(180)) * time.Millisecond)
	_ = mouse.Up(proto.InputMouseButtonLeft, 1)
}

// reloadCaptchaImages 重新触发没加载出来的滑块图片；被拦截过的图片不会自己重试。
func reloadCaptchaImages(page *rod.Page) {
	_, _ = page.Timeout(10 * time.Second).Eval(`() => {
		for (const img of document.querySelectorAll('img.captcha-verify-image, img.captcha-verify-image-slide')) {
			if (!img.naturalWidth && img.src) img.src = img.src;
		}
	}`)
}

// waitCaptchaReady 等滑块图片加载出来再返回几何信息；组件已消失时返回 (nil, nil)。
// 图片没加载完时 readCaptcha 也返回 nil，直接当成校验通过会把滑块留在页面上。
func waitCaptchaReady(ctx context.Context, page *rod.Page, timeout time.Duration) (*captchaInfo, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !hasCaptcha(page) {
			return nil, nil
		}
		info, err := readCaptcha(page)
		if err != nil {
			lastErr = err
		} else if info != nil {
			return info, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("滑块图片未加载完成")
}

func jitter(amp float64) float64 {
	return (float64(ri(200))/100 - 1) * amp
}

/* ===== 会话采集 ===== */

// captureAuth 采集 BytePlus 站点 Cookie。必须拿到会话 cookie（digest + AccountID）
// 才算注册成功，只看表单点击结果会把风控拦截误判成成功。
func captureAuth(ctx context.Context, page *rod.Page, in Input) (map[string]any, error) {
	// 注册通过后面板关闭、前端自动登录，等会话 cookie 下发；
	// BytePlus 常在校验后再补一次滑块，出现就继续解，否则会白等到超时。
	sessionReady := func() bool {
		return !hasVisibleSel(page, selCode) && !hasCaptcha(page) && hasSessionCookie(page)
	}
	deadline := time.Now().Add(4 * time.Minute)
	for {
		if waitCleared(ctx, page, 45*time.Second, sessionReady) {
			break
		}
		if !hasCaptcha(page) || time.Now().After(deadline) {
			break
		}
		in.logf("会话尚未下发且滑块再次出现，继续处理滑块")
		if err := solveCaptcha(ctx, page, in); err != nil {
			return nil, err
		}
	}
	in.logf("当前页面: %s", trimText(pageURL(page), 120))

	// 首次登录 Lumina 会弹使用条款，不接受则整站不可用、拿到 cookie 也不能生图。
	termsAccepted := false
	if hasSessionCookie(page) {
		// 条款弹窗只需要勾选框，Lumina 首页的大图/视频照屏。
		if in.mediaBlock != nil {
			in.mediaBlock.Store(true)
		}
		termsAccepted = acceptTerms(ctx, page, in)
	}

	all, err := proto.NetworkGetAllCookies{}.Call(page)
	if err != nil {
		return nil, fmt.Errorf("读取 Cookie 失败: %w", err)
	}
	cookieList := make([]map[string]any, 0, len(all.Cookies))
	names := map[string]bool{}
	for _, c := range all.Cookies {
		names[strings.ToLower(c.Name)] = true
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
	if !names["digest"] || !names["accountid"] {
		return nil, fmt.Errorf("未采集到 BytePlus 会话 cookie（digest/AccountID），注册可能未完成（当前页面: %s，提示: %s，现有 cookie: %s）",
			trimText(pageURL(page), 120), trimText(pageAlert(page), 120), trimText(cookieNames(all.Cookies), 300))
	}
	in.logf("已采集 %d 条 Cookie（含 BytePlus 会话 cookie）", len(cookieList))

	// 前端会往 localStorage 塞几 MB 的素材缓存，只留登录态相关的小条目，避免整条记录膨胀。
	var storage map[string]any
	if sv, serr := page.Timeout(30 * time.Second).Eval(`() => {
		const pick = s => Object.fromEntries(Object.entries(s).filter(([, v]) => (v || '').length <= 8192));
		return JSON.stringify({
			localStorage: pick(localStorage),
			sessionStorage: pick(sessionStorage),
			location: location.href
		});
	}`); serr == nil {
		_ = json.Unmarshal([]byte(sv.Value.Str()), &storage)
	} else {
		in.logf("读取浏览器存储失败: %v", serr)
	}

	auth := map[string]any{
		"auth_mode":      "lumina_browser_session",
		"platform":       "lumina",
		"email":          in.Email,
		"captured_at":    time.Now().UTC().Format(time.RFC3339),
		"cookies":        cookieList,
		"storage":        storage,
		"terms_accepted": termsAccepted,
	}
	// 账号元信息（到期 / 套餐有效期等）只在接口里，cookie 和 localStorage 都没有，
	// 注册完顺手拉一次存进去；user_id / tenant_id / 额度属于实时数据，取号方自己带 cookie 现查，不落库。
	if info := fetchAccountInfo(page, in); info != nil {
		auth["account"] = info
	}
	return auth, nil
}

// luminaAPIBase Lumina 控制台接口域名（与页面同站，cookie 直接生效）。
const luminaAPIBase = "https://lumi-api.console.byteplus.com"

// fetchAccountInfo 在页面上带 cookie 请求 Lumina 账号接口，把账号到期、
// 区域限制状态与套餐有效期取回；接口失败不影响注册结果。
func fetchAccountInfo(page *rod.Page, in Input) map[string]any {
	v, err := page.Timeout(45*time.Second).Eval(`async base => {
		const get = async path => {
			try {
				const r = await fetch(base + path, { credentials: 'include', headers: { accept: 'application/json' } });
				return await r.json();
			} catch (e) { return null; }
		};
		const [current, resources] = await Promise.all([
			get('/api/user/current'),
			get('/api/user/get_user_resources'),
		]);
		return JSON.stringify({ current, resources });
	}`, luminaAPIBase)
	if err != nil {
		in.logf("读取账号元信息失败: %v", err)
		return nil
	}
	var raw struct {
		Current struct {
			Code int `json:"code"`
			Data struct {
				UserID         string `json:"user_id"`
				UserName       string `json:"user_name"`
				Email          string `json:"email"`
				AvatarURL      string `json:"avatar_url"`
				Role           string `json:"role"`
				ExpiredTime    int64  `json:"expired_time"`
				VolcAccountID  string `json:"volc_account_id"`
				IsVolcRootUser bool   `json:"is_volc_root_user"`
				IsCountryBlock bool   `json:"is_country_blocked"`
			} `json:"data"`
		} `json:"current"`
		Resources struct {
			Code int `json:"code"`
			Data struct {
				URIs   []string `json:"uris"`
				Combos []struct {
					ID        int64  `json:"id"`
					Name      string `json:"name"`
					BeginDt   int64  `json:"begin_dt"`
					EndDt     int64  `json:"end_dt"`
					Status    int    `json:"status"`
					ComboType int    `json:"combo_type"`
				} `json:"combos"`
			} `json:"data"`
		} `json:"resources"`
	}
	if jerr := json.Unmarshal([]byte(v.Value.Str()), &raw); jerr != nil {
		in.logf("解析账号元信息失败: %v", jerr)
		return nil
	}
	cur := raw.Current.Data
	if cur.UserID == "" {
		in.logf("账号元信息接口未返回 user_id，跳过")
		return nil
	}
	info := map[string]any{
		"user_name":          cur.UserName,
		"account_email":      cur.Email,
		"avatar_url":         cur.AvatarURL,
		"role":               cur.Role,
		"expired_time":       cur.ExpiredTime,
		"volc_account_id":    cur.VolcAccountID,
		"is_volc_root_user":  cur.IsVolcRootUser,
		"is_country_blocked": cur.IsCountryBlock,
	}
	if cur.ExpiredTime > 0 {
		info["expired_at"] = time.Unix(cur.ExpiredTime, 0).UTC().Format(time.RFC3339)
	}
	res := raw.Resources.Data
	if len(res.URIs) > 0 {
		info["resource_uris"] = res.URIs
	}
	if len(res.Combos) > 0 {
		c := res.Combos[0]
		info["combo_id"] = c.ID
		info["combo_name"] = c.Name
		info["combo_status"] = c.Status
		info["combo_type"] = c.ComboType
		info["combo_begin_dt"] = c.BeginDt
		info["combo_end_dt"] = c.EndDt
		if c.EndDt > 0 {
			info["combo_end_at"] = time.Unix(c.EndDt, 0).UTC().Format(time.RFC3339)
		}
	}
	in.logf("已采集账号元信息: 邮箱=%s 套餐=%v", cur.Email, info["combo_name"])
	return info
}

// acceptTerms 处理首次登录的 Lumina 使用条款弹窗：勾选两个协议再点同意。
// 不接受这一步，账号登录进去也只能看到条款弹窗，无法生图。
func acceptTerms(ctx context.Context, page *rod.Page, in Input) bool {
	if err := gotoStable(ctx, page, luminaURL, in, 90*time.Second); err != nil {
		in.logf("打开 Lumina 首页确认条款失败: %v", err)
		return false
	}
	// 首次点击可能落在弹窗动画中间的旧坐标上，勾不上就整轮重来。
	for attempt := 0; attempt < 3; attempt++ {
		done, retry := tryAcceptTerms(ctx, page, in)
		if done || !retry {
			return done
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

// tryAcceptTerms 跑一轮勾选 + 同意；retry 为 true 表示还值得再试一轮。
func tryAcceptTerms(ctx context.Context, page *rod.Page, in Input) (done, retry bool) {
	boxes := waitCheckboxes(ctx, page, 45*time.Second)
	if len(boxes) == 0 {
		in.logf("未出现使用条款弹窗，跳过")
		return false, false
	}
	for _, box := range boxes {
		// 隐藏的 input 点不动，要点外层 label；React 受控组件只认真实事件。
		if r, err := box.Eval(`() => { const p = this.closest('label') || this.parentElement; const b = p.getBoundingClientRect(); return [b.x + Math.min(12, b.width / 2), b.y + b.height / 2]; }`); err == nil {
			if arr := r.Value.Arr(); len(arr) == 2 {
				mouseClickAt(page, arr[0].Num(), arr[1].Num())
			}
		}
		time.Sleep(600 * time.Millisecond)
		if c, err := box.Eval(`() => this.checked`); err == nil && !c.Value.Bool() {
			_, _ = box.Eval(`() => { const p = this.closest('label') || this.parentElement; p.dispatchEvent(new MouseEvent('click', {bubbles: true})); this.dispatchEvent(new MouseEvent('click', {bubbles: true})); }`)
			time.Sleep(600 * time.Millisecond)
		}
	}
	if !mouseClickByText(page, `button,div[role=button],a`, "readandagree") {
		in.logf("未找到使用条款同意按钮")
		return false, false
	}
	if !waitCleared(ctx, page, 30*time.Second, func() bool { return !hasVisibleSel(page, `input[type=checkbox]`) }) {
		in.logf("使用条款弹窗未关闭（勾选状态: %s）", checkboxStates(page))
		return false, true
	}
	in.logf("已接受 Lumina 使用条款")
	return true, false
}

// checkboxStates 输出当前页面勾选框的勾选情况，条款没点成时用于排障。
func checkboxStates(page *rod.Page) string {
	v, err := page.Timeout(10 * time.Second).Eval(`() => [...document.querySelectorAll('input[type=checkbox]')].map(b => b.checked ? '1' : '0').join(',')`)
	if err != nil {
		return "读取失败"
	}
	return v.Value.Str()
}

// waitCheckboxes 等条款弹窗里的勾选框渲染出来。
func waitCheckboxes(ctx context.Context, page *rod.Page, timeout time.Duration) []*rod.Element {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return nil
		}
		if boxes := elements(page, `input[type=checkbox]`); len(boxes) > 0 {
			return boxes
		}
		time.Sleep(time.Second)
	}
	return nil
}

func elements(page *rod.Page, selector string) []*rod.Element {
	els, err := page.Timeout(10 * time.Second).Elements(selector)
	if err != nil {
		return nil
	}
	out := make([]*rod.Element, 0, len(els))
	for _, el := range els {
		out = append(out, el.CancelTimeout())
	}
	return out
}

// cookieNames 汇总当前 cookie 名字，失败时写进日志便于判断卡在哪一步。
func cookieNames(cookies []*proto.NetworkCookie) string {
	names := make([]string, 0, len(cookies))
	for _, c := range cookies {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func hasSessionCookie(page *rod.Page) bool {
	all, err := proto.NetworkGetAllCookies{}.Call(page)
	if err != nil {
		return false
	}
	digest, account := false, false
	for _, c := range all.Cookies {
		switch strings.ToLower(c.Name) {
		case "digest":
			digest = c.Value != ""
		case "accountid":
			account = c.Value != ""
		}
	}
	return digest && account
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
		if strings.Contains(u, "byteplus.com") {
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
func submitAndAdvance(ctx context.Context, page *rod.Page, advanced func() bool, buttonText string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if advanced() {
			return nil
		}
		clickPrimary(page, buttonText)
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

// clickPrimary 点击面板主按钮：优先按钮文案精确匹配，否则点可用的 arco 主按钮。
func clickPrimary(page *rod.Page, buttonText string) bool {
	if clickByText(page, `button:not([disabled])`, buttonText) {
		return true
	}
	el, err := page.Timeout(10 * time.Second).Element(`button.arco-btn-primary:not([disabled])`)
	if err != nil || el == nil {
		return false
	}
	if verr := el.WaitVisible(); verr != nil {
		return false
	}
	if mouseClickElement(el) {
		return true
	}
	return el.Click(proto.InputMouseButtonLeft, 1) == nil
}

// primaryDisabled 面板主按钮当前是否为禁用态。
func primaryDisabled(page *rod.Page) bool {
	v, err := page.Timeout(8 * time.Second).Eval(`() => {
		const visible = e => !!(e.offsetWidth || e.offsetHeight || e.getClientRects().length);
		const el = [...document.querySelectorAll('button.arco-btn-primary')].find(visible);
		return !!el && el.disabled;
	}`)
	if err != nil {
		return false
	}
	return v.Value.Bool()
}

// clickAgreement 勾选表单里的协议同意项（arco 复选框，真正可点的是 mask 层）。
func clickAgreement(page *rod.Page) bool {
	v, err := page.Timeout(8 * time.Second).Eval(`() => {
		const visible = e => !!(e.offsetWidth || e.offsetHeight || e.getClientRects().length);
		const box = [...document.querySelectorAll('input[type=checkbox]')].find(b => !b.checked);
		if (!box) return false;
		const mask = box.parentElement && box.parentElement.querySelector('.arco-checkbox-mask-wrapper');
		const target = mask && visible(mask) ? mask : box;
		target.click();
		return true;
	}`)
	if err != nil {
		return false
	}
	return v.Value.Bool()
}

// hasVisibleButtonText 页面上是否有文案匹配的可见按钮。
func hasVisibleButtonText(page *rod.Page, lowerText string) bool {
	v, err := page.Timeout(8*time.Second).Eval(`needle => {
		const visible = e => !!(e.offsetWidth || e.offsetHeight || e.getClientRects().length);
		const norm = e => (e.textContent || '').toLowerCase().replace(/\s+/g, '');
		return [...document.querySelectorAll('button')].some(e =>
			visible(e) && norm(e).includes(needle));
	}`, lowerText)
	if err != nil {
		return false
	}
	return v.Value.Bool()
}

// installPassportTrace 挂钩 fetch/XHR，记录 passport 接口的响应，便于定位提交失败的真实原因。
func installPassportTrace(page *rod.Page) {
	_, _ = page.Timeout(8 * time.Second).Eval(`() => {
		if (window.__ppTrace) return;
		window.__ppTrace = [];
		const push = (url, status, body) => {
			if (!/passport|verify|sign|captcha/i.test(url)) return;
			if (window.__ppTrace.length >= 20) return;
			const line = status + ' ' + url.replace(/^https?:\/\/[^/]+/, '').slice(0, 120) +
				' => ' + String(body || '').slice(0, 500);
			// 非 2xx 响应是定位失败原因的关键，排到前面不被截断。
			if (status >= 400) window.__ppTrace.unshift(line);
			else window.__ppTrace.push(line);
		};
		const of = window.fetch;
		window.fetch = function (...args) {
			const url = typeof args[0] === 'string' ? args[0] : (args[0] && args[0].url) || '';
			return of.apply(this, args).then(resp => {
				try { resp.clone().text().then(t => push(url, resp.status, t)); } catch (e) {}
				return resp;
			});
		};
		const oo = XMLHttpRequest.prototype.open, os = XMLHttpRequest.prototype.send;
		XMLHttpRequest.prototype.open = function (m, url, ...rest) {
			this.__ppURL = String(url); return oo.call(this, m, url, ...rest);
		};
		XMLHttpRequest.prototype.send = function (...args) {
			this.addEventListener('loadend', () => {
				try { push(this.__ppURL || '', this.status, this.responseText); } catch (e) {}
			});
			return os.apply(this, args);
		};
	}`)
}

// passportTrace 读取 installPassportTrace 记录到的接口响应摘要。
func passportTrace(page *rod.Page) string {
	v, err := page.Timeout(8 * time.Second).Eval(`() => (window.__ppTrace || []).join(' | ')`)
	if err != nil {
		return ""
	}
	return v.Value.Str()
}

// pageAlert 读取面板上的错误/提示文案，便于把失败原因写进日志。
func pageAlert(page *rod.Page) string {
	v, err := page.Timeout(8 * time.Second).Eval(`() => {
		const visible = el => !!(el.offsetWidth || el.offsetHeight || el.getClientRects().length);
		const el = [...document.querySelectorAll('[role="alert"], .arco-message, .arco-form-item-message, .arco-input-error-message')]
			.find(e => visible(e) && (e.textContent || '').trim());
		return el ? el.textContent.trim() : '';
	}`)
	if err != nil {
		return ""
	}
	return v.Value.Str()
}

// dismissBanners 关闭 cookie 同意条与首页弹窗，避免遮挡按钮点击。
func dismissBanners(page *rod.Page) {
	for _, text := range []string{"acceptall", "accept", "gotit"} {
		if clickByText(page, `button`, text) {
			return
		}
	}
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

// platformForUA 返回与 UA 匹配的 navigator.platform。
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
	info, err := page.Timeout(10 * time.Second).Info()
	if err != nil || info == nil {
		return ""
	}
	return info.URL
}

// hasVisibleSel 判断选择器是否命中「可见」元素；面板会把各步骤输入框都留在 DOM 里，
// 仅判断存在会误判。
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

// fillInput 往输入框写入文本：优先逐字符人工输入（真实按键节奏），未生效时退回
// 原生 setter 兜底。
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
			// 面板动画/遮挡层会让逐字符输入报错，这时不要放弃，直接用 setter 兜底。
			if terr := typeHuman(el, value); terr == nil && inputValue(page, selector) == value {
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

// setInputValue 聚焦输入框并用原生 setter 赋值、派发 input/change，兼容受控组件。
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
		time.Sleep(500 * time.Millisecond)
	}
	return cleared()
}

// clickByText 点击选择器命中的、可见且文本匹配的第一个元素。
// 文本比较忽略大小写与空白，needle 传小写去空白形式（如 login、verifyemailaddress）。
func clickByText(page *rod.Page, selector, lowerText string) bool {
	// 页面 JS 忙时无超时的 Eval 会一直挂着（曾出现注册整体卡死），这里必须带超时。
	ok, err := page.Timeout(10*time.Second).Eval(`(selector, needle) => {
		const visible = el => !!(el.offsetWidth || el.offsetHeight || el.getClientRects().length);
		const norm = e => (e.textContent || '').toLowerCase().replace(/\s+/g, '');
		const el = [...document.querySelectorAll(selector)].find(e =>
			visible(e) && norm(e).includes(needle));
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
			return false
		}
	}
	time.Sleep(40*time.Millisecond + time.Duration(ri(90))*time.Millisecond)
	return mouse.Click(proto.InputMouseButtonLeft, 1) == nil
}

// luminaChromiumBin 在 Lumina 专用 rod 目录（browser-lumina）管理 Chromium，
// 与其它平台各自隔离，彼此不共享浏览器二进制或用户目录。
func luminaChromiumBin() (string, error) {
	b := launcher.NewBrowser()
	b.RootDir = filepath.Join(filepath.Dir(launcher.DefaultBrowserDir), "browser-lumina")
	return b.Get()
}

// luminaCacheDir Lumina 专用的 Chromium 磁盘缓存目录，只存 HTTP 缓存（JS/CSS 等
// 静态资源），不含 cookie/登录态，多个账号共用安全。
func luminaCacheDir() string {
	dir := filepath.Join(filepath.Dir(launcher.DefaultBrowserDir), "browser-lumina", "http-cache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	return dir
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

// x11 显示上可摆放的窗口格子：可见模式下每个任务占一格，避免窗口重叠导致真光标
// 拖动落到别的任务窗口上。格子数即可见模式下的实际并发上限。
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
