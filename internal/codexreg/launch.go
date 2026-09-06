package codexreg

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"chatgpt-register/internal/proxyutil"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	launcherflags "github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ysmood/gson"
)

// LaunchOptions 启动注册 / 测活用浏览器的参数。
type LaunchOptions struct {
	Headless bool
	// Proxy 空=直连。http/https 带账号密码时经本地认证桥（Chrome 的 --proxy-server 不支持内嵌凭据），
	// socks5 带账号密码时回退 CDP HandleAuth。
	Proxy string
	// BrowserBin 浏览器可执行文件。空/auto=优先本机安装的 Chrome/Edge，其次 rod 下载的 Chromium；
	// rod/bundled=强制使用 rod 下载的 Chromium；其它值按路径使用。
	BrowserBin string
	// Locale 如 en-US；Languages 逗号分隔的语言列表（不带 q 值，如 "en-US,en"，
	// Chrome 会自己生成 Accept-Language 的 q 值和 navigator.languages）。为空按 en-US。
	Locale    string
	Languages string
	// UserAgent / Screen / Timezone 用于恢复已保存的网页会话。注册时留空，使用当前浏览器与随机屏幕。
	UserAgent string
	Screen    *ScreenProfile
	Timezone  string
	Log       func(format string, a ...any)
}

// Session 一个已就绪、指纹已对齐的浏览器会话：独占一个 Chrome 进程（LaunchBrowser），
// 或是共享 Chrome 进程里的一个独立 BrowserContext（Pool.Acquire）。两种情况下 cookie /
// 缓存 / 代理出口 / UA / 屏幕都彼此隔离，对页面代码而言看起来就是一台独立电脑。
type Session struct {
	Browser *rod.Browser
	// UserAgent 页面对外声明的 UA：真实浏览器自己的 UA 去掉 Headless 标记，不再写死某个版本号。
	UserAgent string
	// Platform navigator.platform，与 UA 声明的系统一致。
	Platform string
	// Screen 本会话随机选定的屏幕规格。
	Screen ScreenProfile
	// BrowserBin 实际使用的浏览器可执行文件（空=rod 下载的 Chromium）。
	BrowserBin string

	host      *host
	ownsHost  bool
	contextID proto.BrowserBrowserContextID
	pool      *Pool
	locale    string
	languages string
	timezone  string
	bridge    *proxyutil.AuthBridge
	log       func(format string, a ...any)
}

// host 一个 Chrome 进程及其只与进程相关（与账号无关）的属性：真实 UA、Client Hints。
type host struct {
	browser  *rod.Browser
	launcher *launcher.Launcher
	bin      string
	headless bool
	ua       string
	platform string
	uaMeta   *proto.EmulationUserAgentMetadata
}

// puppeteerFlags 是 rod launcher 默认附带的一批 Puppeteer 风格参数。它们会关掉正常 Chrome 的
// 后台行为（定时器节流、站点隔离、后台网络、崩溃上报……），风控脚本可以从这些差异里认出
// 自动化浏览器；grokreg / leonardoreg 早已验证去掉之后 Cloudflare 才放行，这里保持一致。
var puppeteerFlags = []string{
	"no-startup-window",
	"disable-features",
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
}

// launchHost 启动一个尽量像"真人桌面 Chrome"的进程：
//   - new headless（旧 --headless 是另一套渲染栈，UA / WebGL / 插件 / 窗口尺寸都与正常 Chrome 不同）；
//   - 去掉 Puppeteer 风格参数、随机非零调试端口、不套 rod 默认的 "Mac Chrome/114 1280x800" 设备模拟；
//   - 优先使用本机安装的最新版 Chrome，而不是 rod 下载的两年前的 Chromium 快照；
//   - WebRTC 只走默认路由，避免代理模式下 STUN 泄露服务器真实 IP。
//
// proxy 是进程级代理（独占模式使用）；池模式传空，每个 BrowserContext 自带代理。
func launchHost(ctx context.Context, headless bool, binPref, locale, proxyServer string, logf func(string, ...any)) (*host, error) {
	l := launcher.New()
	for _, flag := range puppeteerFlags {
		l = l.Delete(launcherflags.Flag(flag))
	}
	if headless {
		l = l.Set("headless", "new")
	} else {
		l = l.Headless(false)
	}
	l = l.NoSandbox(true).
		Set("disable-dev-shm-usage").
		Set("no-first-run").
		Set("no-default-browser-check").
		Set("disable-suggestions-ui").
		Set("disable-infobars").
		Set("disable-popup-blocking").
		Set("hide-crash-restore-bubble").
		Set("disable-features", "PrivacySandboxSettings4").
		Set("disable-blink-features", "AutomationControlled").
		Set("force-webrtc-ip-handling-policy", "default_public_interface_only").
		Set("lang", strings.ReplaceAll(locale, "_", "-")).
		Set("window-size", "1280,760")

	// remote-debugging-port=0 时 Chrome 会额外暴露 navigator.webdriver，用随机非零端口。
	port, err := availableLoopbackPort()
	if err != nil {
		return nil, fmt.Errorf("分配 Chrome 调试端口失败: %w", err)
	}
	l = l.Set("remote-debugging-port", strconv.Itoa(port))

	bin := resolveBrowserBin(binPref)
	if bin != "" {
		l = l.Bin(bin)
	}
	if proxyServer != "" {
		l = l.Set("proxy-server", proxyServer)
	}

	controlURL, err := l.Launch()
	if err != nil {
		l.Kill()
		l.Cleanup()
		return nil, fmt.Errorf("启动 Chrome 失败: %w", err)
	}
	browser := rod.New().Context(ctx).NoDefaultDevice().ControlURL(controlURL)
	if err := browser.Connect(); err != nil {
		l.Kill()
		l.Cleanup()
		return nil, fmt.Errorf("连接 Chrome 失败: %w", err)
	}
	h := &host{browser: browser, launcher: l, bin: bin, headless: headless}
	if ver, verr := (proto.BrowserGetVersion{}).Call(browser); verr == nil {
		h.ua = cleanUserAgent(ver.UserAgent)
	}
	h.platform = platformForUA(h.ua)
	h.uaMeta = h.loadUAMetadata()
	logf("🧭 浏览器 %s | UA %s | Client Hints: %s", h.describeBin(), h.ua, h.describeHints())
	return h, nil
}

// close 关掉进程并清理临时用户数据目录。ctx 已取消时用干净 ctx 关，否则 Chrome 进程残留。
func (h *host) close() {
	if h == nil {
		return
	}
	if h.browser != nil {
		if rod.Try(h.browser.Context(context.Background()).MustClose) != nil && h.launcher != nil {
			// CDP 卡死时关不掉，直接杀掉 Chrome 进程兜底
			h.launcher.Kill()
		}
	}
	if h.launcher != nil {
		h.launcher.Cleanup()
	}
}

func (h *host) describeBin() string {
	if h.bin == "" {
		return fmt.Sprintf("rod Chromium(%d)", launcher.RevisionDefault)
	}
	return filepath.Base(h.bin)
}

func (h *host) describeHints() string {
	if h.uaMeta == nil {
		return "无"
	}
	parts := make([]string, 0, len(h.uaMeta.Brands))
	for _, b := range h.uaMeta.Brands {
		parts = append(parts, fmt.Sprintf("%s;v=%s", b.Brand, b.Version))
	}
	return strings.Join(parts, " ")
}

// productBrand 无头 Chrome 把产品品牌报成 HeadlessChrome，需要还原成实际产品名；
// 纯 Chromium 只有 "Chromium" 一个品牌，直接丢掉 Headless 项。
func (h *host) productBrand() string {
	if strings.Contains(h.ua, "Edg/") {
		return "Microsoft Edge"
	}
	if isBundledChromium(h.bin) {
		return ""
	}
	return "Google Chrome"
}

// LaunchBrowser 为单个任务独占启动一个 Chrome 进程（代理挂在进程上）。
// 批量场景请用 Pool，共享进程、按账号隔离上下文，省掉每号一个进程的内存与启动时间。
func LaunchBrowser(ctx context.Context, opt LaunchOptions) (*Session, error) {
	logf := opt.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	locale, languages := normalizeLocale(opt.Locale, opt.Languages)

	server, bridge, authUser, authPass, err := prepareProxy(opt.Proxy, logf)
	if err != nil {
		return nil, err
	}
	h, err := launchHost(ctx, opt.Headless, opt.BrowserBin, locale, server, logf)
	if err != nil {
		if bridge != nil {
			bridge.Close()
		}
		return nil, err
	}
	if authUser != "" || authPass != "" {
		// 必须用非 Must 版本并 recover——MustHandleAuth 在独立 goroutine 里 panic 会把整个进程带崩。
		go func() {
			defer func() { _ = recover() }()
			wait := h.browser.HandleAuth(authUser, authPass)
			_ = wait()
		}()
	}
	userAgent := h.ua
	if strings.TrimSpace(opt.UserAgent) != "" {
		userAgent = cleanUserAgent(opt.UserAgent)
	}
	screen := pickScreenProfile()
	if opt.Screen != nil && opt.Screen.valid() {
		screen = *opt.Screen
	}
	sess := &Session{
		Browser:    h.browser,
		UserAgent:  userAgent,
		Platform:   platformForUA(userAgent),
		Screen:     screen,
		BrowserBin: h.bin,
		host:       h,
		ownsHost:   true,
		locale:     locale,
		languages:  languages,
		timezone:   strings.TrimSpace(opt.Timezone),
		bridge:     bridge,
		log:        logf,
	}
	logf("🖥 屏幕 %s", sess.Screen)
	return sess, nil
}

// prepareProxy 把代理串变成 Chrome 可用的 --proxy-server 值：有账号密码的 http(s) 代理起本地认证桥，
// socks5 带账号密码回退 CDP HandleAuth。
func prepareProxy(raw string, logf func(string, ...any)) (server string, bridge *proxyutil.AuthBridge, user, pass string, err error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", nil, "", "", nil
	}
	server, user, pass, err = proxyutil.Parse(p)
	if err != nil {
		return "", nil, "", "", fmt.Errorf("解析代理失败: %w", err)
	}
	if user == "" && pass == "" {
		logf("🌐 使用代理: %s", server)
		return server, nil, "", "", nil
	}
	if b, local, berr := proxyutil.StartAuthBridge(p); berr == nil {
		logf("🌐 使用代理: %s（经本地认证桥 %s）", server, local)
		return local, b, "", "", nil
	}
	logf("🌐 使用代理: %s（CDP 认证）", server)
	return server, nil, user, pass, nil
}

func normalizeLocale(locale, languages string) (string, string) {
	locale = strings.TrimSpace(locale)
	if locale == "" {
		locale = "en-US"
	}
	languages = strings.TrimSpace(languages)
	if languages == "" {
		languages = "en-US,en"
	}
	return locale, languages
}

// Close 释放会话：独占模式关进程；池模式销毁本账号的 BrowserContext（cookie/缓存随之清空）并归还名额。
func (s *Session) Close() {
	if s == nil {
		return
	}
	if s.ownsHost {
		s.host.close()
	} else if s.pool != nil {
		s.pool.release(s)
	}
	if s.bridge != nil {
		s.bridge.Close()
	}
}

// NewPage 新建标签页并套上与真实浏览器一致的指纹：
//   - UA 去掉 Headless 标记，Client Hints（Sec-CH-UA / navigator.userAgentData）按真实值同步改写。
//     只改 UA 不改 Client Hints 会让二者互相矛盾，或让 Client Hints 整体消失，都是典型自动化特征；
//   - 无头下按随机屏幕规格模拟"最大化窗口 + 系统任务栏 + 浏览器工具栏"，避免 screen == innerWidth/innerHeight；
//     窗口（outerWidth/outerHeight）也按同一规格调整，池模式下每个账号各有自己的窗口尺寸；
//   - Accept-Language / navigator.platform / Intl locale 与 UA 声明的系统和出口 IP 一致。
func (s *Session) NewPage() (*rod.Page, error) {
	page, err := s.Browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return nil, err
	}
	if s.host.headless {
		if win, werr := (proto.BrowserGetWindowForTarget{TargetID: page.TargetID}).Call(s.Browser); werr == nil && win != nil {
			w, h := s.Screen.Width, s.Screen.WindowHeight()
			_ = (proto.BrowserSetWindowBounds{WindowID: win.WindowID, Bounds: &proto.BrowserBounds{Width: &w, Height: &h}}).Call(s.Browser)
		}
		sw, sh := s.Screen.Width, s.Screen.Height
		if err := (proto.EmulationSetDeviceMetricsOverride{
			Width:             s.Screen.Width,
			Height:            s.Screen.ViewportHeight(),
			DeviceScaleFactor: 1,
			Mobile:            false,
			ScreenWidth:       &sw,
			ScreenHeight:      &sh,
		}).Call(page); err != nil {
			s.log("⚠️ 设置视口失败: %v", err)
		}
	}
	if s.UserAgent != "" {
		if err := (proto.EmulationSetUserAgentOverride{
			UserAgent:         s.UserAgent,
			AcceptLanguage:    s.languages,
			Platform:          s.Platform,
			UserAgentMetadata: userAgentMetadataFor(s.host.uaMeta, s.UserAgent),
		}).Call(page); err != nil {
			s.log("⚠️ 设置 UA 失败: %v", err)
		}
	}
	_ = (proto.EmulationSetLocaleOverride{Locale: strings.ReplaceAll(s.locale, "-", "_")}).Call(page)
	if s.timezone != "" {
		_ = (proto.EmulationSetTimezoneOverride{TimezoneID: s.timezone}).Call(page)
	}
	return page, nil
}

var (
	hintsProbeOnce sync.Once
	hintsProbeURL  string
)

// hintsProbePage 本机回环上的一个空白页。navigator.userAgentData 只在安全上下文里存在，
// about:blank 的 opaque origin 拿不到；127.0.0.1 属于可信来源，且 Chrome 对回环地址默认不走代理，
// 读取本身不经过出口 IP。
func hintsProbePage() string {
	hintsProbeOnce.Do(func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, "<!doctype html><title>.</title>")
		})
		go func() { _ = http.Serve(ln, mux) }()
		hintsProbeURL = "http://" + ln.Addr().String() + "/"
	})
	return hintsProbeURL
}

// loadUAMetadata 在一个临时标签页里读出浏览器真实的 Client Hints，然后关掉它，
// 真正干活的标签页不带任何历史。
func (h *host) loadUAMetadata() *proto.EmulationUserAgentMetadata {
	u := hintsProbePage()
	if u == "" {
		return nil
	}
	var meta *proto.EmulationUserAgentMetadata
	_ = rod.Try(func() {
		probe := h.browser.MustPage("")
		defer func() { _ = probe.Close() }()
		pg := probe.Timeout(15 * time.Second)
		pg.MustNavigate(u)
		pg.MustWaitLoad()
		meta = h.readUAMetadata(pg)
	})
	return meta
}

// readUAMetadata 读出页面里的 Client Hints，只把无头模式下的 "HeadlessChrome" 品牌换回实际产品名，
// 其余（架构、位数、平台版本、完整版本号）原样保留，保证与真实运行环境一致。
func (h *host) readUAMetadata(page *rod.Page) *proto.EmulationUserAgentMetadata {
	res, err := page.Eval(`async () => {
		const d = navigator.userAgentData;
		if (!d || !d.getHighEntropyValues) return null;
		const hi = await d.getHighEntropyValues(["architecture","bitness","model","platformVersion","fullVersionList","wow64"]);
		return {
			brands: d.brands, mobile: d.mobile, platform: d.platform,
			architecture: hi.architecture, bitness: hi.bitness, model: hi.model,
			platformVersion: hi.platformVersion, fullVersionList: hi.fullVersionList, wow64: hi.wow64,
		};
	}`)
	if err != nil || res == nil || res.Value.Nil() {
		return nil
	}
	v := res.Value
	meta := &proto.EmulationUserAgentMetadata{
		Platform:        v.Get("platform").Str(),
		PlatformVersion: v.Get("platformVersion").Str(),
		Architecture:    v.Get("architecture").Str(),
		Model:           v.Get("model").Str(),
		Mobile:          v.Get("mobile").Bool(),
		Bitness:         v.Get("bitness").Str(),
		Wow64:           v.Get("wow64").Bool(),
		Brands:          h.fixBrands(v.Get("brands")),
		FullVersionList: h.fixBrands(v.Get("fullVersionList")),
	}
	if len(meta.Brands) == 0 {
		return nil
	}
	return meta
}

func (h *host) fixBrands(arr gson.JSON) []*proto.EmulationUserAgentBrandVersion {
	product := h.productBrand()
	seen := map[string]bool{}
	var out []*proto.EmulationUserAgentBrandVersion
	for _, b := range arr.Arr() {
		brand := b.Get("brand").Str()
		if strings.Contains(brand, "Headless") {
			if product == "" {
				continue
			}
			brand = product
		}
		if seen[brand] {
			continue
		}
		seen[brand] = true
		out = append(out, &proto.EmulationUserAgentBrandVersion{Brand: brand, Version: b.Get("version").Str()})
	}
	return out
}

// resolveBrowserBin 选浏览器：显式路径 > 环境变量 CHATGPT_CHROME_BIN > 本机安装的 Chrome/Edge > rod Chromium。
// rod 下载的 Chromium 快照停在 Chrome 128、只有 "Chromium" 一个品牌、没有专有编解码器，
// 与真实用户里几乎不存在的组合撞在一起，本身就是风险信号。
func resolveBrowserBin(pref string) string {
	pref = strings.TrimSpace(pref)
	if pref == "" {
		pref = strings.TrimSpace(os.Getenv("CHATGPT_CHROME_BIN"))
	}
	switch strings.ToLower(pref) {
	case "", "auto", "system":
		if p, ok := launcher.LookPath(); ok {
			return p
		}
		return ""
	case "rod", "bundled", "chromium":
		return ""
	default:
		return pref
	}
}

func isBundledChromium(bin string) bool {
	return bin == "" || strings.Contains(strings.ToLower(filepath.ToSlash(bin)), "chromium")
}

// cleanUserAgent 去掉无头 Chrome UA 里的 Headless 标记。
func cleanUserAgent(ua string) string {
	ua = strings.TrimSpace(ua)
	ua = strings.ReplaceAll(ua, "HeadlessChrome", "Chrome")
	ua = strings.ReplaceAll(ua, "Headless", "")
	return strings.Join(strings.Fields(ua), " ")
}

// platformForUA 按 UA 声明的系统给出 navigator.platform。
func platformForUA(ua string) string {
	switch {
	case strings.Contains(ua, "Windows"):
		return "Win32"
	case strings.Contains(ua, "Macintosh"):
		return "MacIntel"
	case strings.Contains(ua, "Linux") || strings.Contains(ua, "X11"):
		return "Linux x86_64"
	default:
		return ""
	}
}

func clientHintPlatformForUA(ua string) string {
	switch {
	case strings.Contains(ua, "Windows"):
		return "Windows"
	case strings.Contains(ua, "Macintosh"):
		return "macOS"
	case strings.Contains(ua, "Linux") || strings.Contains(ua, "X11"):
		return "Linux"
	default:
		return ""
	}
}

// userAgentMetadataFor 复制真实浏览器的 Client Hints，并在恢复旧会话 UA 时同步 Chrome 主版本。
func userAgentMetadataFor(base *proto.EmulationUserAgentMetadata, ua string) *proto.EmulationUserAgentMetadata {
	if base == nil {
		return nil
	}
	out := *base
	out.Brands = cloneBrands(base.Brands)
	out.FullVersionList = cloneBrands(base.FullVersionList)
	out.Platform = clientHintPlatformForUA(ua)
	full, major := browserVersionFromUA(ua)
	if full == "" {
		return &out
	}
	out.FullVersion = full
	align := func(brands []*proto.EmulationUserAgentBrandVersion, version string) {
		for _, brand := range brands {
			if brand != nil && (brand.Brand == "Google Chrome" || brand.Brand == "Microsoft Edge" || brand.Brand == "Chromium") {
				brand.Version = version
			}
		}
	}
	align(out.Brands, major)
	align(out.FullVersionList, full)
	return &out
}

func cloneBrands(in []*proto.EmulationUserAgentBrandVersion) []*proto.EmulationUserAgentBrandVersion {
	out := make([]*proto.EmulationUserAgentBrandVersion, len(in))
	for i, brand := range in {
		if brand != nil {
			copy := *brand
			out[i] = &copy
		}
	}
	return out
}

func browserVersionFromUA(ua string) (full, major string) {
	for _, marker := range []string{"Edg/", "Chrome/", "Chromium/"} {
		if i := strings.Index(ua, marker); i >= 0 {
			fields := strings.Fields(ua[i+len(marker):])
			if len(fields) == 0 {
				continue
			}
			full = fields[0]
			major = strings.SplitN(full, ".", 2)[0]
			return full, major
		}
	}
	return "", ""
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

// ScreenProfile 一套桌面屏幕规格。无头浏览器没有真实窗口，screen / outer / inner 三者默认完全相等，
// 这是明显的无头特征；这里按"最大化窗口 = 屏幕高 - 任务栏，视口 = 窗口高 - 浏览器工具栏"
// 还原正常桌面浏览器的层次，并且每次注册随机换一套，避免所有账号共用同一个屏幕指纹。
type ScreenProfile struct {
	Width  int `json:"width"`
	Height int `json:"height"`
	// Taskbar 系统任务栏高度；Toolbar 浏览器标签栏 + 地址栏（+ 书签栏）高度。
	Taskbar int `json:"taskbar"`
	Toolbar int `json:"toolbar"`
}

func (p ScreenProfile) valid() bool {
	return p.Width >= 1024 && p.Height >= 600 && p.Taskbar >= 0 && p.Toolbar > 0 && p.ViewportHeight() > 400
}

// WindowHeight 最大化窗口高度（window.outerHeight）。
func (p ScreenProfile) WindowHeight() int { return p.Height - p.Taskbar }

// ViewportHeight 页面视口高度（window.innerHeight）。
func (p ScreenProfile) ViewportHeight() int { return p.WindowHeight() - p.Toolbar }

func (p ScreenProfile) String() string { return fmt.Sprintf("%dx%d", p.Width, p.Height) }

// screenPool 常见桌面分辨率及其大致市场占比（权重）。
var screenPool = []struct{ w, h, weight int }{
	{1920, 1080, 34},
	{1366, 768, 14},
	{1536, 864, 12},
	{2560, 1440, 9},
	{1440, 900, 8},
	{1600, 900, 6},
	{1280, 720, 5},
	{1280, 800, 4},
	{1680, 1050, 3},
	{1920, 1200, 3},
	{1600, 1200, 2},
}

func pickScreenProfile() ScreenProfile {
	total := 0
	for _, s := range screenPool {
		total += s.weight
	}
	pick := ri(total)
	p := ScreenProfile{Width: screenPool[0].w, Height: screenPool[0].h}
	for _, s := range screenPool {
		if pick < s.weight {
			p.Width, p.Height = s.w, s.h
			break
		}
		pick -= s.weight
	}
	switch runtime.GOOS {
	case "windows":
		p.Taskbar = []int{40, 48}[ri(2)]
	case "darwin":
		p.Taskbar = 25 + []int{0, 70}[ri(2)]
	default:
		p.Taskbar = []int{0, 27, 32}[ri(3)]
	}
	// 无书签栏 ≈ 85px，有书签栏 ≈ 120px，各占一半，再加一点抖动。
	p.Toolbar = []int{85, 120}[ri(2)] + ri(6)
	return p
}
