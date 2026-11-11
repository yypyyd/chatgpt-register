// Package gmailreg 使用 rod 浏览器自动化注册 Google 账号。
package gmailreg

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"chatgpt-register/internal/proxyutil"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	launcherflags "github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
)

const (
	signupEntryURL = "https://accounts.google.com/signup"
	stepTimeout    = 30 * time.Second
)

var chromeVerRe = regexp.MustCompile(`Chrome/(\d+)\.`)

type Input struct {
	Username   string
	Password   string
	FirstName  string
	LastName   string
	BirthYear  int
	BirthMonth int
	BirthDay   int
	Gender     int // 1=女 2=男 3=不愿透露, 0=随机
	Proxy      string
	Region     string // 代理地区(US/GB/...),用于匹配时区和地理位置;BestGo 网关代理串里没有,需外部传入
	Headless   bool
	ProfileDir string // 非空则用持久化 Profile 目录,让设备跨多次注册积累信任
	Log        func(format string, a ...any)
}

type Result struct {
	Email    string         `json:"email"`
	Password string         `json:"password"`
	AuthJSON map[string]any `json:"auth_json"`
}

func (in Input) logf(f string, a ...any) {
	if in.Log != nil {
		in.Log(f, a...)
	}
}

func GenRandomUsername() string { return genUsername(randomFirstName(), randomLastName()) }

func Register(ctx context.Context, in Input) (*Result, error) {
	if in.FirstName == "" {
		in.FirstName = randomFirstName()
	}
	if in.LastName == "" {
		in.LastName = randomLastName()
	}
	if in.Password == "" {
		in.Password = genPassword(16)
	}
	if in.Username == "" {
		in.Username = genUsername(in.FirstName, in.LastName)
	}
	if in.BirthYear == 0 {
		in.BirthYear = randomBirthYear()
	}
	if in.BirthMonth == 0 {
		in.BirthMonth = randomInt(1, 12)
	}
	if in.BirthDay == 0 {
		in.BirthDay = randomInt(1, 28)
	}
	if in.Gender == 0 {
		in.Gender = randomInt(1, 3)
	}

	in.logf("Gmail 注册启动: %s (headless=%v proxy=%v)", in.Username, in.Headless, in.Proxy != "")

	browser, bridge, cleanup, err := launchStealthBrowser(in)
	if err != nil {
		return nil, err
	}
	defer func() {
		// browser.Close() 通过代理时可能很慢,限 5 秒后强杀
		done := make(chan struct{})
		go func() { browser.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			in.logf("browser.Close 超时,强制退出")
		}
		if bridge != nil {
			bridge.Close()
		}
		if cleanup != nil {
			cleanup()
		}
	}()

	// 随机化本次注册的浏览器指纹（视口），避免多次注册被聚类
	fp := randomFingerprint()
	in.logf("指纹: %dx%d", fp.Width, fp.Height)

	// 地区：优先用外部传入的 Region（BestGo 网关代理串里没有地区信息）
	region := strings.ToLower(strings.TrimSpace(in.Region))
	if region == "" {
		if m := regionRe.FindStringSubmatch(in.Proxy); m != nil {
			region = strings.ToLower(m[1])
		} else {
			region = "us"
		}
	}

	page := browser.MustPage("")
	page = page.Context(ctx)
	applyStealth(page, browser, fp, region)
	in.logf("地区=%s 时区=%s", region, tzForRegion(region))

	// 充分暖场：真实使用 Google 建立信任 Cookie(SOCS/NID/AEC)和浏览历史,
	// 提高账号创建时的信任分,让 Google 不要求手机验证(crossflowverification)。
	warmupGoogle(page, in)

	// 真正导航到注册页（带超时）
	navErr := func() error {
		np := page.Timeout(45 * time.Second)
		if e := np.Navigate(signupEntryURL); e != nil {
			return e
		}
		return np.WaitLoad()
	}()
	if navErr != nil {
		return nil, fmt.Errorf("打开注册页失败: %w", navErr)
	}
	in.logf("注册页已加载")
	humanWait()
	humanMouse(page)

	if err := stepName(page, in); err != nil {
		return nil, fmt.Errorf("姓名步骤: %w", err)
	}
	in.logf("姓名已提交: %s %s", in.FirstName, in.LastName)
	humanMouse(page)
	humanWait()

	if err := stepBirthday(page, in); err != nil {
		return nil, fmt.Errorf("生日步骤: %w", err)
	}
	in.logf("生日已提交: %d-%02d-%02d 性别=%d", in.BirthYear, in.BirthMonth, in.BirthDay, in.Gender)
	humanMouse(page)
	humanWait()

	email, err := stepUsername(page, in)
	if err != nil {
		return nil, fmt.Errorf("用户名步骤: %w", err)
	}
	in.logf("邮箱地址确认: %s", email)
	humanMouse(page)
	humanWait()

	if err := stepPassword(page, in); err != nil {
		return nil, fmt.Errorf("密码步骤: %w", err)
	}
	in.logf("密码已设置")
	humanMouse(page)

	// 密码之后 Google 可能要求设备/手机验证（crossflowverification / gap / phone / challenge）。
	// 处理并跳过条款，只有真正创建成功才返回。
	if err := stepAfterPassword(page, in); err != nil {
		return nil, err
	}

	// 关键：确认账号真的创建成功，而不是停在验证墙上。
	if err := verifyAccountCreated(page, in); err != nil {
		return nil, err
	}

	in.logf("Gmail 注册成功: %s", email)
	auth := map[string]any{
		"platform": "gmail", "email": email, "password": in.Password,
		"first_name": in.FirstName, "last_name": in.LastName,
		"captured_at": time.Now().UTC().Format(time.RFC3339),
	}
	if in.Proxy != "" {
		auth["proxy"] = in.Proxy
	}
	return &Result{Email: email, Password: in.Password, AuthJSON: auth}, nil
}

// warmupGoogle 真实地使用 Google 一会儿:接受 Cookie 同意、做一次搜索、看结果、滚动。
// 目的是积累信任 Cookie(SOCS/NID/AEC)和真实行为信号,提高后续建号的信任分。
func warmupGoogle(page *rod.Page, in Input) {
	defer func() { recover() }()

	searchTerms := []string{"weather today", "news", "best movies 2024", "how to cook pasta",
		"nba scores", "world cup", "stock market", "recipes", "travel tips", "music"}

	// 0. 先检测出口 IP（Google 实际看到的 IP 及其归属/类型）
	ipp := page.Timeout(25 * time.Second)
	if e := ipp.Navigate("https://ipinfo.io/json"); e == nil {
		if e2 := ipp.WaitLoad(); e2 == nil {
			if r, _ := ipp.Eval(`() => document.body ? document.body.innerText : ''`); r != nil {
				txt := r.Value.Str()
				txt = strings.ReplaceAll(txt, "\n", " ")
				txt = strings.Join(strings.Fields(txt), " ")
				in.logf("出口 IP 信息: %s", truncate(txt, 400))
			}
		}
	}

	// 1. 打开 Google 首页
	in.logf("暖场: 打开 Google 首页")
	wp := page.Timeout(35 * time.Second)
	if err := wp.Navigate("https://www.google.com/"); err != nil {
		in.logf("暖场打开首页失败(代理慢),跳过: %v", err)
		return
	}
	if err := wp.WaitLoad(); err != nil {
		in.logf("暖场首页加载超时,跳过")
		return
	}
	time.Sleep(time.Duration(randomInt(2000, 4000)) * time.Millisecond)
	humanMouse(page)

	// 2. 接受 Cookie 同意弹窗(设置 SOCS 信任 Cookie)
	page.Timeout(5*time.Second).Eval(`() => {
		const btns = document.querySelectorAll('button, div[role="button"]');
		for (const b of btns) {
			const t = (b.textContent||'').trim();
			if (/accept all|i agree|全部接受|同意|accept|agree/i.test(t)) { b.click(); return 'clicked'; }
		}
		return 'none';
	}`)
	time.Sleep(time.Duration(randomInt(1500, 3000)) * time.Millisecond)

	// 3. 真的做一次搜索:找搜索框,逐字输入,回车
	term := searchTerms[randomInt(0, len(searchTerms)-1)]
	searched := false
	if box, err := page.Timeout(8 * time.Second).Element(`textarea[name="q"], input[name="q"]`); err == nil {
		box.Click(proto.InputMouseButtonLeft, 1)
		time.Sleep(time.Duration(randomInt(300, 700)) * time.Millisecond)
		for _, ch := range term {
			box.Type(input.Key(ch))
			time.Sleep(time.Duration(randomInt(80, 220)) * time.Millisecond)
		}
		time.Sleep(time.Duration(randomInt(500, 1200)) * time.Millisecond)
		box.Type(input.Enter)
		searched = true
		in.logf("暖场: 搜索「%s」", term)
	}
	if searched {
		// 等结果页,滚动看看
		page.Timeout(15 * time.Second).WaitLoad()
		time.Sleep(time.Duration(randomInt(2000, 4000)) * time.Millisecond)
		humanMouse(page)
		page.Mouse.Scroll(0, float64(randomInt(300, 800)), 5)
		time.Sleep(time.Duration(randomInt(1500, 3500)) * time.Millisecond)
		humanMouse(page)
		page.Mouse.Scroll(0, float64(randomInt(200, 500)), 3)
		time.Sleep(time.Duration(randomInt(1000, 2500)) * time.Millisecond)
	}
	in.logf("暖场完成,信任 Cookie 已建立")
	time.Sleep(time.Duration(randomInt(1000, 2000)) * time.Millisecond)
}

/* ==================== 浏览器反检测 ==================== */

// fingerprint 一次注册使用的随机浏览器指纹。
type fingerprint struct {
	Width    int
	Height   int
	GPUVendor string
	GPUModel  string
	Cores    int
	Memory   int
}

// 常见桌面分辨率
var screenSizes = [][2]int{
	{1920, 1080}, {1366, 768}, {1536, 864}, {1440, 900},
	{1600, 900}, {1280, 720}, {1680, 1050}, {2560, 1440},
}

// 常见 GPU 组合（vendor, renderer）
var gpuModels = [][2]string{
	{"Google Inc. (NVIDIA)", "ANGLE (NVIDIA, NVIDIA GeForce GTX 1060 Direct3D11 vs_5_0 ps_5_0, D3D11)"},
	{"Google Inc. (NVIDIA)", "ANGLE (NVIDIA, NVIDIA GeForce RTX 3060 Direct3D11 vs_5_0 ps_5_0, D3D11)"},
	{"Google Inc. (AMD)", "ANGLE (AMD, AMD Radeon RX 580 Direct3D11 vs_5_0 ps_5_0, D3D11)"},
	{"Google Inc. (Intel)", "ANGLE (Intel, Intel(R) UHD Graphics 630 Direct3D11 vs_5_0 ps_5_0, D3D11)"},
	{"Google Inc. (Intel)", "ANGLE (Intel, Intel(R) Iris(R) Xe Graphics Direct3D11 vs_5_0 ps_5_0, D3D11)"},
	{"Google Inc. (NVIDIA)", "ANGLE (NVIDIA, NVIDIA GeForce RTX 2070 Direct3D11 vs_5_0 ps_5_0, D3D11)"},
}

func randomFingerprint() fingerprint {
	size := screenSizes[randomInt(0, len(screenSizes)-1)]
	gpu := gpuModels[randomInt(0, len(gpuModels)-1)]
	cores := []int{4, 6, 8, 12, 16}[randomInt(0, 4)]
	mem := []int{4, 8, 16, 32}[randomInt(0, 3)]
	return fingerprint{
		Width: size[0], Height: size[1],
		GPUVendor: gpu[0], GPUModel: gpu[1],
		Cores: cores, Memory: mem,
	}
}

// buildStealthJS 返回注入脚本（现已最小化,不再依赖指纹占位符）。
func buildStealthJS(fp fingerprint) string {
	return stealthJSTemplate
}

// applyStealth 给页面套上全套反检测：stealth JS、视口、时区、UA+品牌伪装、地理位置。
// region 为小写国家代码（us/gb/...）。Register 和诊断测试共用,确保测的就是实际跑的。
func applyStealth(page *rod.Page, browser *rod.Browser, fp fingerprint, region string) {
	page.MustEvalOnNewDocument(buildStealthJS(fp))

	_ = (proto.EmulationSetDeviceMetricsOverride{
		Width: fp.Width, Height: fp.Height, DeviceScaleFactor: 1, Mobile: false,
	}).Call(page)

	// 时区匹配代理地区
	_ = (proto.EmulationSetTimezoneOverride{TimezoneID: tzForRegion(region)}).Call(page)

	// UA + 品牌伪装：去 HeadlessChrome,品牌补上 "Google Chrome"（否则是 Chromium/红旗）
	ua := ""
	chromeMajor := "128"
	chromeFull := "128.0.6613.138"
	if ver, verr := (proto.BrowserGetVersion{}).Call(browser); verr == nil {
		ua = ver.UserAgent
		if m := chromeVerRe.FindStringSubmatch(ua); m != nil {
			chromeMajor = m[1]
		}
	}
	ua = strings.ReplaceAll(ua, "HeadlessChrome", "Chrome")
	ua = strings.ReplaceAll(ua, "Headless", "")
	ua = strings.Join(strings.Fields(ua), " ")
	if ua == "" {
		ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + chromeMajor + ".0.0.0 Safari/537.36"
	}
	_ = (proto.EmulationSetUserAgentOverride{
		UserAgent:      ua,
		AcceptLanguage: "en-US,en;q=0.9",
		Platform:       "Win32",
		UserAgentMetadata: &proto.EmulationUserAgentMetadata{
			Brands: []*proto.EmulationUserAgentBrandVersion{
				{Brand: "Chromium", Version: chromeMajor},
				{Brand: "Google Chrome", Version: chromeMajor},
				{Brand: "Not;A=Brand", Version: "24"},
			},
			FullVersionList: []*proto.EmulationUserAgentBrandVersion{
				{Brand: "Chromium", Version: chromeFull},
				{Brand: "Google Chrome", Version: chromeFull},
				{Brand: "Not;A=Brand", Version: "24.0.0.0"},
			},
			Platform:        "Windows",
			PlatformVersion: "15.0.0",
			Architecture:    "x86",
			Model:           "",
			Mobile:          false,
			Bitness:         "64",
		},
	}).Call(page)

	// 地理位置匹配代理地区
	lat, lng := geoForRegion(region)
	_ = (proto.EmulationSetGeolocationOverride{
		Latitude: f64(lat), Longitude: f64(lng), Accuracy: f64(100),
	}).Call(page)
}

// stealthJSTemplate 在每个页面文档加载前注入。
// 原则：最小化篡改 —— 过度伪装（WebGL/硬件参数覆盖）反而会被 nativeTampering 检测到。
// headful 模式下真实的 GPU/plugins/hardware 值本身就正常,不需要伪装。
// navigator.webdriver 由 --disable-blink-features=AutomationControlled 处理为 false,不在 JS 里碰。
const stealthJSTemplate = `
// 1. 删除 CDP 注入的 $cdc_ / $wdc_ 标记（无害清理）
(function removeCDC() {
	try {
		for (const p of Object.getOwnPropertyNames(document)) {
			if (p.startsWith('$cdc_') || p.startsWith('$chrome_') || p.startsWith('$wdc_')) delete document[p];
		}
	} catch(e) {}
})();

// 2. chrome.app 补全（真实 Chrome 有,无头/自动化常缺）
if (window.chrome && !window.chrome.app) {
	window.chrome.app = { isInstalled: false,
		InstallState: { DISABLED: 'disabled', INSTALLED: 'installed', NOT_INSTALLED: 'not_installed' },
		RunningState: { CANNOT_RUN: 'cannot_run', READY_TO_RUN: 'ready_to_run', RUNNING: 'running' } };
}

// 3. Permissions.query 与 notification 权限一致（自动化下常不一致）
try {
	const origQuery = window.navigator.permissions.query.bind(window.navigator.permissions);
	window.navigator.permissions.query = (params) => {
		if (params && params.name === 'notifications') return Promise.resolve({ state: Notification.permission });
		return origQuery(params);
	};
} catch(e) {}

// 4. 禁用 WebRTC —— 防止 STUN 泄露代理背后的真实 IP
try {
	window.RTCPeerConnection = undefined;
	window.webkitRTCPeerConnection = undefined;
	window.mozRTCPeerConnection = undefined;
	window.RTCDataChannel = undefined;
	if (navigator.mediaDevices) navigator.mediaDevices.getUserMedia = () => Promise.reject(new Error('disabled'));
} catch(e) {}
`

// patchChromeBinary 实现 undetected-chromedriver 的核心原理：
// 在 Chrome 二进制文件中将 "cdc_" 字符串替换为随机字符串，
// 防止 Google 通过 document.$cdc_* 属性检测 CDP 连接。
// Windows 上 cdc_ 在 chrome.dll 里（不在 chrome.exe 里）。
func patchChromeBinary(binPath string) error {
	if binPath == "" {
		return nil
	}
	// Windows: chrome.exe 是 stub，真正的 CDP 代码在同目录的 chrome.dll
	dir := filepath.Dir(binPath)
	targets := []string{binPath}
	dllPath := filepath.Join(dir, "chrome.dll")
	if _, err := os.Stat(dllPath); err == nil {
		targets = append(targets, dllPath)
	}
	marker := []byte("cdc_")
	replacement := []byte("apc_") // 替换为同长度的无害字符串
	patched := false
	for _, target := range targets {
		data, err := os.ReadFile(target)
		if err != nil {
			continue
		}
		if !bytes.Contains(data, marker) {
			continue
		}
		data = bytes.ReplaceAll(data, marker, replacement)
		if err := os.WriteFile(target, data, 0755); err != nil {
			return fmt.Errorf("写回 %s 失败: %w", filepath.Base(target), err)
		}
		patched = true
	}
	_ = patched
	return nil
}

// launchStealthBrowser 与 adobereg/grokreg 一致的反检测浏览器启动：
// 删除 rod 默认的自动化标志、处理代理认证桥、NoDefaultDevice。
func launchStealthBrowser(in Input) (browser *rod.Browser, bridge *proxyutil.AuthBridge, cleanup func(), err error) {
	l := launcher.New()
	// 删除 rod 默认追加的自动化特征标志
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
		// 关键：rod 默认无头,必须显式关掉,否则 UA 带 HeadlessChrome
		l = l.Headless(false)
	}
	l = l.
		NoSandbox(true).
		Set("no-default-browser-check").
		Set("no-first-run").
		Set("disable-infobars").
		Set("disable-blink-features", "AutomationControlled").
		Set("lang", "en-US").
		Set("window-size", "1366,768").
		// 关键：防止 WebRTC 泄露代理背后的真实 IP（Google 会对比 IP 一致性）
		Set("force-webrtc-ip-handling-policy", "disable_non_proxied_udp").
		Set("webrtc-ip-handling-policy", "disable_non_proxied_udp").
		// 强制所有流量走代理,DNS 也走代理,防止真实 IP/DNS 泄露
		Set("proxy-bypass-list", "<-loopback>")

	// 持久化 Profile：让设备跨多次注册积累 Google 信任（NID/SOCS 等 Cookie + 浏览历史）,
	// 提高信任分,降低 crossflowverification 手机验证墙出现的概率。
	persist := strings.TrimSpace(in.ProfileDir) != ""
	if persist {
		_ = os.MkdirAll(in.ProfileDir, 0755)
		l = l.UserDataDir(in.ProfileDir)
	}

	// 在启动前 patch Chrome 二进制,移除 $cdc_ 指纹(undetected-chromedriver 原理)
	if binPath, berr := launcher.NewBrowser().Get(); berr == nil && binPath != "" {
		_ = patchChromeBinary(binPath)
	}

	// 代理：带账密的走认证桥（与 adobereg/grokreg 一致）
	if p := strings.TrimSpace(in.Proxy); p != "" {
		server, user, pass, perr := proxyutil.Parse(p)
		if perr != nil {
			return nil, nil, nil, fmt.Errorf("解析代理失败: %w", perr)
		}
		if user != "" || pass != "" {
			bridge, server, perr = proxyutil.StartAuthBridge(p)
			if perr != nil {
				return nil, nil, nil, fmt.Errorf("启动认证代理桥失败: %w", perr)
			}
		}
		l = l.Set("proxy-server", server)
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
	// 持久化 Profile 时不清理用户目录（否则信任 Cookie 全丢）
	cleanupFn := l.Cleanup
	if persist {
		cleanupFn = func() {}
	}
	return browser, bridge, cleanupFn, nil
}

// safeURL 带超时地读取当前页面 URL，防止 page.Info() 阻塞导致整个流程挂死。
func safeURL(page *rod.Page) string {
	ch := make(chan string, 1)
	go func() {
		r, err := page.Timeout(5 * time.Second).Eval(`() => location.href`)
		if err == nil && r != nil {
			ch <- r.Value.Str()
		} else {
			ch <- ""
		}
	}()
	select {
	case u := <-ch:
		return u
	case <-time.After(6 * time.Second):
		return ""
	}
}

// safeBodyText 带超时地读取页面正文前 300 字符（调试用）。
func safeBodyText(page *rod.Page) string {
	ch := make(chan string, 1)
	go func() {
		r, err := page.Timeout(5 * time.Second).Eval(`() => document.body ? document.body.innerText.substring(0,300) : ''`)
		if err == nil && r != nil {
			ch <- r.Value.Str()
		} else {
			ch <- ""
		}
	}()
	select {
	case t := <-ch:
		return t
	case <-time.After(6 * time.Second):
		return ""
	}
}

// stepAfterPassword 处理密码之后的页面：手机/设备验证墙、条款页，或直接成功。
// 遇到强制验证且无法跳过时如实返回错误，绝不假报成功。
func stepAfterPassword(page *rod.Page, in Input) error {
	time.Sleep(3 * time.Second) // 等页面加载

	deadline := time.Now().Add(60 * time.Second)
	lastURL := ""
	for time.Now().Before(deadline) {
		u := strings.ToLower(safeURL(page))
		if u == "" {
			time.Sleep(2 * time.Second)
			continue
		}
		if u != lastURL {
			in.logf("密码后页面: %s", u)
			lastURL = u

			// 立刻在首次看到 URL 变化时做判断,不等下一轮循环:

			// Google 反自动化错误页 — 立即失败
			if strings.Contains(u, "signup/error") || strings.Contains(u, "/error/1") {
				in.logf("检测到 Google error 页面,立即报失败")
				return fmt.Errorf("Google 拒绝注册（检测到自动化/可疑环境）。建议：换干净住宅 IP 重试")
			}

			// 已经创建成功
			if !strings.Contains(u, "/signup/") && !strings.Contains(u, "lifecycle/steps") {
				in.logf("已跳出注册流程: %s", u)
				return nil
			}
		}

		// 强制验证墙
		if strings.Contains(u, "crossflowverification") || strings.Contains(u, "/gap") ||
			strings.Contains(u, "phoneverification") || strings.Contains(u, "phonechallenge") ||
			strings.Contains(u, "verifyphone") {
			time.Sleep(3 * time.Second) // 等页面完全加载
			// 抓取验证页上所有可点的选项,看有没有不用手机的路径
			opts := inspectVerificationPage(page)
			in.logf("验证页选项: %s", opts)
			// 尝试找"换种方式"/"其他选项"/"跳过"
			if tryAlternativeVerification(page, in) {
				in.logf("点击了替代验证选项,继续观察")
				time.Sleep(4 * time.Second)
				continue
			}
			return fmt.Errorf("Google 要求设备/手机验证（%s），页面选项: %s", stepName2(u), truncate(opts, 300))
		}

		// 添加手机号页面（可选/可跳过的版本）
		if strings.Contains(u, "phone") || strings.Contains(u, "recovery") || strings.Contains(u, "addphone") {
			if skipIfPossible(page) {
				in.logf("手机号页面已跳过")
				time.Sleep(3 * time.Second)
				continue
			}
			// 不一定是强制要求，可能有其他按钮推进
			if clickAnyPrimary(page) {
				time.Sleep(3 * time.Second)
				continue
			}
			return fmt.Errorf("卡在手机号页面（%s），无法继续", stepName2(u))
		}

		// 条款 / 隐私 / 同意页
		if strings.Contains(u, "tos") || strings.Contains(u, "terms") || strings.Contains(u, "consent") ||
			strings.Contains(u, "agreement") || strings.Contains(u, "privacy") {
			in.logf("条款/同意页，尝试同意")
			agreeTerms(page, in)
			time.Sleep(3 * time.Second)
			continue
		}

		// 其他 signup 子步骤：尝试推进
		if clickAnyPrimary(page) {
			time.Sleep(3 * time.Second)
			continue
		}
		time.Sleep(2 * time.Second)
	}
	finalURL := safeURL(page)
	bodySnippet := safeBodyText(page)
	return fmt.Errorf("密码后流程超时，URL: %s 页面: %s", finalURL, truncate(bodySnippet, 150))
}

// verifyAccountCreated 确认账号确实创建：必须脱离 signup 流程。
func verifyAccountCreated(page *rod.Page, in Input) error {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		u := strings.ToLower(safeURL(page))
		if u != "" && !strings.Contains(u, "/signup/") && !strings.Contains(u, "lifecycle/steps") {
			in.logf("账号已创建并落地: %s", u)
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("未能确认账号创建成功，URL: %s", safeURL(page))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// inspectVerificationPage 抓取验证页上所有按钮/链接/可点元素的文本,用于分析有无非手机路径。
func inspectVerificationPage(page *rod.Page) string {
	r, err := page.Timeout(8 * time.Second).Eval(`() => {
		const items = [];
		const els = document.querySelectorAll('button, a, [role="button"], [role="link"], input[type="tel"], input[type="submit"]');
		for (const e of els) {
			const t = (e.textContent || e.value || e.getAttribute('aria-label') || '').trim();
			if (t) items.push(t.substring(0, 40));
		}
		// 也看页面有没有手机号输入框
		const hasPhoneInput = !!document.querySelector('input[type="tel"], input[name*="phone" i], #phoneNumberId');
		return 'hasPhoneInput=' + hasPhoneInput + ' | buttons/links: ' + [...new Set(items)].join(' || ');
	}`)
	if err != nil || r == nil {
		return "(抓取失败)"
	}
	return r.Value.Str()
}

// tryAlternativeVerification 在验证页尝试点"其他方式/换种方法/Try another way/Skip"等非手机路径。
func tryAlternativeVerification(page *rod.Page, in Input) bool {
	r, _ := page.Timeout(6 * time.Second).Eval(`() => {
		const els = document.querySelectorAll('button, a, [role="button"], [role="link"]');
		for (const e of els) {
			const t = (e.textContent || e.getAttribute('aria-label') || '').trim();
			if (/try another way|another way|other option|more options|换种方式|其他方式|其他选项|skip|not now|跳过|以后/i.test(t)) {
				e.click();
				return t;
			}
		}
		return '';
	}`)
	if r != nil && r.Value.Str() != "" {
		in.logf("点击替代选项: %s", r.Value.Str())
		return true
	}
	return false
}

// skipIfPossible 在验证页寻找"跳过/以后再说/Skip/Not now"之类的按钮并点击。
func skipIfPossible(page *rod.Page) bool {
	r, _ := page.Eval(`() => {
		const els = document.querySelectorAll('button, a, [role="button"]');
		for (const e of els) {
			const t = (e.textContent || '').trim();
			if (/^(skip|not now|i'?ll do (this|it) later|跳过|以后再说|暂不|稍后)/i.test(t)) { e.click(); return true; }
		}
		return false;
	}`)
	return r != nil && r.Value.Bool()
}

// clickAnyPrimary 点击页面主按钮（Next/下一步/继续/Continue）推进未知子步骤。
func clickAnyPrimary(page *rod.Page) bool {
	r, _ := page.Eval(`() => {
		const btns = [...document.querySelectorAll('button')];
		const b = btns.find(x => /next|continue|下一步|继续|确认/i.test((x.textContent||'').trim()));
		if (b) { b.click(); return true; }
		return false;
	}`)
	return r != nil && r.Value.Bool()
}

// agreeTerms 在条款页点"我同意/Agree"。
func agreeTerms(page *rod.Page, in Input) {
	page.Eval(`() => {
		const btns = document.querySelectorAll('button');
		for (const b of btns) {
			if (/i agree|agree|同意|接受|confirm|确认/i.test((b.textContent||'').trim())) { b.click(); return; }
		}
	}`)
	in.logf("已尝试同意条款")
}

// stepName2 从 URL 提取当前 signup 子步骤名，便于日志。
func stepName2(u string) string {
	i := strings.Index(u, "/signup/")
	if i < 0 {
		return u
	}
	rest := u[i+len("/signup/"):]
	if j := strings.IndexAny(rest, "/?"); j >= 0 {
		return rest[:j]
	}
	return rest
}

/* ==================== 步骤实现 ==================== */

func stepName(page *rod.Page, in Input) error {
	if err := waitForURL(page, "signup/name"); err != nil {
		return err
	}
	inputs, err := page.Timeout(stepTimeout).Elements("input[type=text], input:not([type]):not([hidden])")
	if err != nil || len(inputs) < 2 {
		inputs, _ = page.Timeout(stepTimeout).Elements("input:not([hidden]):not([type=hidden]):not([type=submit])")
		if len(inputs) < 2 {
			return fmt.Errorf("未找到姓名输入框 (found %d)", len(inputs))
		}
	}
	typeInto(inputs[0], in.LastName)
	time.Sleep(300 * time.Millisecond)
	typeInto(inputs[1], in.FirstName)
	time.Sleep(500 * time.Millisecond)
	return clickNext(page)
}

func stepBirthday(page *rod.Page, in Input) error {
	if err := waitForURL(page, "birthday"); err != nil {
		return err
	}
	time.Sleep(1 * time.Second)

	// 月份名称映射（英文）
	monthNames := []string{"", "January", "February", "March", "April", "May", "June",
		"July", "August", "September", "October", "November", "December"}
	genderNames := []string{"", "Female", "Male", "Rather not say", "Custom"}
	monthName := monthNames[in.BirthMonth]
	genderName := genderNames[in.Gender]

	// 1. 月份下拉：只能用 JS 点击 Material UI 的选项
	mr, _ := page.Eval(`(monthText) => {
		const main = document.querySelector('#view_container') || document.body;
		const combos = main.querySelectorAll('[role="combobox"]');
		if (combos.length < 1) return 'no-combo';
		combos[0].click();
		return new Promise(resolve => setTimeout(() => {
			const opts = document.querySelectorAll('[role="option"]');
			for (const o of opts) {
				if (o.offsetHeight === 0) continue;
				if (o.textContent.trim() === monthText || o.textContent.includes(monthText)) {
					o.click(); resolve('ok'); return;
				}
			}
			resolve('notfound');
		}, 800));
	}`, monthName)
	monthRes := ""
	if mr != nil {
		monthRes = mr.Value.Str()
	}
	time.Sleep(time.Duration(randomInt(500, 1200)) * time.Millisecond)

	// 2. 日期：真实键盘输入
	if dayEl, derr := page.Timeout(10 * time.Second).Element(`input[name="day"]`); derr == nil {
		typeInto(dayEl, fmt.Sprintf("%d", in.BirthDay))
	}
	time.Sleep(time.Duration(randomInt(300, 700)) * time.Millisecond)

	// 3. 年份：真实键盘输入
	if yearEl, yerr := page.Timeout(10 * time.Second).Element(`input[name="year"]`); yerr == nil {
		typeInto(yearEl, fmt.Sprintf("%d", in.BirthYear))
	}
	time.Sleep(time.Duration(randomInt(300, 700)) * time.Millisecond)

	// 4. 性别下拉：JS 点击
	gr, _ := page.Eval(`(genderText) => {
		const main = document.querySelector('#view_container') || document.body;
		const combos = main.querySelectorAll('[role="combobox"]');
		if (combos.length < 2) return 'no-combo';
		combos[1].click();
		return new Promise(resolve => setTimeout(() => {
			const opts = document.querySelectorAll('[role="option"]');
			for (const o of opts) {
				if (o.offsetHeight === 0) continue;
				if (o.textContent.trim() === genderText || o.textContent.includes(genderText)) {
					o.click(); resolve('ok'); return;
				}
			}
			resolve('notfound');
		}, 800));
	}`, genderName)
	genderRes := ""
	if gr != nil {
		genderRes = gr.Value.Str()
	}
	in.logf("生日填写: 月=%s 性别=%s", monthRes, genderRes)
	time.Sleep(time.Duration(randomInt(500, 1000)) * time.Millisecond)
	return clickNext(page)
}

func stepUsername(page *rod.Page, in Input) (string, error) {
	if err := waitForURL(page, "username"); err != nil {
		if err2 := waitForURL(page, "choose"); err2 != nil {
			return "", fmt.Errorf("等待用户名页超时: %w", err)
		}
	}
	time.Sleep(1500 * time.Millisecond)

	username := in.Username
	// 最多试 3 个用户名（被占用就换）
	for attempt := 0; attempt < 3; attempt++ {
		// 若页面是"选建议地址/创建自己的"变体,先点"创建自己的 Gmail 地址"
		page.Timeout(5*time.Second).Eval(`() => {
			const opts = document.querySelectorAll('[role="radio"], [data-value], div[jsname]');
			for (const el of opts) {
				const t = (el.textContent||'').trim();
				if (el.getAttribute('data-value') === 'custom' || /create a? ?(your )?own|创建(自己|您自己)/i.test(t)) {
					el.click(); return 'clicked-custom';
				}
			}
			return 'no-custom';
		}`)
		time.Sleep(1200 * time.Millisecond)

		// 找用户名输入框:优先 name=Username,否则找可见文本框
		var box *rod.Element
		if el, e := page.Timeout(8 * time.Second).Element(`input[name="Username"]`); e == nil {
			box = el
		} else {
			if els, _ := page.Timeout(5 * time.Second).Elements(`input[type=text]:not([hidden]), input[type=email]:not([hidden])`); len(els) > 0 {
				box = els[len(els)-1] // 自定义输入框通常在建议项之后
			}
		}
		if box == nil {
			return "", fmt.Errorf("找不到用户名输入框")
		}
		typeInto(box, username)
		time.Sleep(800 * time.Millisecond)

		oldURL := safeURL(page)
		_ = clickNext(page)
		time.Sleep(3 * time.Second)

		newURL := safeURL(page)
		// 跳转到密码页 = 成功
		if strings.Contains(strings.ToLower(newURL), "password") || (newURL != oldURL && !strings.Contains(strings.ToLower(newURL), "username")) {
			return username + "@gmail.com", nil
		}

		// 还在用户名页:看是不是被占用/非法,换个用户名重试
		errTxt, _ := page.Timeout(5 * time.Second).Eval(`() => {
			const t = document.body ? document.body.innerText : '';
			if (/taken|already (taken|in use|exists)|not available|已被(占用|使用)|无法使用|choose another|try another/i.test(t)) return 'taken';
			if (/can'?t be used|letters, numbers|字母.*数字|invalid|格式/i.test(t)) return 'invalid';
			return '';
		}`)
		reason := ""
		if errTxt != nil {
			reason = errTxt.Value.Str()
		}
		in.logf("用户名 %s 未通过(%s),换一个重试", username, reason)
		username = gmailUser() // 换全新随机用户名
		time.Sleep(1 * time.Second)
	}
	return "", fmt.Errorf("连续 3 个用户名都未通过（可能被占用或页面异常）")
}

// gmailUser 生成一个较独特的随机用户名。
func gmailUser() string {
	return strings.ToLower(randomFirstName()) + strings.ToLower(randomLastName()) + fmt.Sprintf("%d", randomInt(1000, 999999))
}

func stepPassword(page *rod.Page, in Input) error {
	if err := waitForURL(page, "password"); err != nil {
		return err
	}
	time.Sleep(1 * time.Second)
	inputs, _ := page.Timeout(stepTimeout).Elements("input[type=password]")
	if len(inputs) == 0 {
		return fmt.Errorf("找不到密码输入框")
	}
	typeInto(inputs[0], in.Password)
	time.Sleep(200 * time.Millisecond)
	if len(inputs) >= 2 {
		typeInto(inputs[1], in.Password)
		time.Sleep(200 * time.Millisecond)
	}
	return clickNext(page)
}

/* ==================== 辅助函数 ==================== */

func f64(v float64) *float64 { return &v }

// regionTimezone 国家代码 → 时区。用于让浏览器时区匹配代理 IP 所在地。
var regionTimezone = map[string]string{
	"us": "America/New_York", "ca": "America/Toronto", "gb": "Europe/London",
	"uk": "Europe/London", "de": "Europe/Berlin", "fr": "Europe/Paris",
	"nl": "Europe/Amsterdam", "es": "Europe/Madrid", "it": "Europe/Rome",
	"jp": "Asia/Tokyo", "sg": "Asia/Singapore", "au": "Australia/Sydney",
	"br": "America/Sao_Paulo", "in": "Asia/Kolkata", "kr": "Asia/Seoul",
	"hk": "Asia/Hong_Kong", "tw": "Asia/Taipei", "my": "Asia/Kuala_Lumpur",
	"th": "Asia/Bangkok", "vn": "Asia/Ho_Chi_Minh", "ph": "Asia/Manila",
	"id": "Asia/Jakarta", "pl": "Europe/Warsaw", "se": "Europe/Stockholm",
	"mx": "America/Mexico_City", "ar": "America/Argentina/Buenos_Aires",
}

var regionRe = regexp.MustCompile(`(?i)region[-=]([a-z]{2})`)

// tzForRegion 国家代码 → 时区。默认美国东部。
func tzForRegion(region string) string {
	if tz, ok := regionTimezone[strings.ToLower(region)]; ok {
		return tz
	}
	return "America/New_York"
}

// geoForRegion 国家代码 → 粗略经纬度。
func geoForRegion(region string) (lat, lng float64) {
	geos := map[string][2]float64{
		"us": {39.0, -98.0}, "ca": {56.0, -106.0}, "gb": {54.0, -2.0},
		"uk": {54.0, -2.0}, "de": {51.0, 10.0}, "fr": {46.0, 2.0},
		"nl": {52.1, 5.3}, "jp": {36.0, 138.0}, "sg": {1.35, 103.8},
		"au": {-25.0, 133.0}, "br": {-14.0, -51.0}, "in": {21.0, 78.0},
	}
	if g, ok := geos[strings.ToLower(region)]; ok {
		return g[0], g[1]
	}
	return 39.0, -98.0
}

// humanWait 随机等待 3~7 秒，模拟人类阅读和思考。
func humanWait() {
	ms := 3000 + randomInt(0, 4000)
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

// mouseState 记录当前鼠标位置,用于连续移动。
var mouseX, mouseY = 683.0, 384.0

// humanMouse 连续曲线移动鼠标(贝塞尔路径,很多小步),模拟真人,提升 reCAPTCHA 分数。
func humanMouse(page *rod.Page) {
	targets := randomInt(2, 4)
	for t := 0; t < targets; t++ {
		destX := float64(randomInt(100, 1200))
		destY := float64(randomInt(80, 650))
		moveMouseSmooth(page, destX, destY)
		time.Sleep(time.Duration(randomInt(150, 600)) * time.Millisecond)
	}
}

// moveMouseSmooth 用二次贝塞尔曲线从当前位置平滑移动到目标,中间插很多步。
func moveMouseSmooth(page *rod.Page, destX, destY float64) {
	startX, startY := mouseX, mouseY
	// 控制点:在起点终点中间加随机偏移,形成弧线
	ctrlX := (startX+destX)/2 + float64(randomInt(-150, 150))
	ctrlY := (startY+destY)/2 + float64(randomInt(-150, 150))
	steps := randomInt(20, 40)
	for i := 1; i <= steps; i++ {
		tt := float64(i) / float64(steps)
		// 二次贝塞尔: B(t) = (1-t)^2*P0 + 2(1-t)t*P1 + t^2*P2
		mt := 1 - tt
		x := mt*mt*startX + 2*mt*tt*ctrlX + tt*tt*destX
		y := mt*mt*startY + 2*mt*tt*ctrlY + tt*tt*destY
		_ = proto.InputDispatchMouseEvent{
			Type: proto.InputDispatchMouseEventTypeMouseMoved,
			X:    x, Y: y,
		}.Call(page)
		// 每步间隔很短,但有随机抖动
		time.Sleep(time.Duration(randomInt(6, 18)) * time.Millisecond)
	}
	mouseX, mouseY = destX, destY
}

func typeInto(el *rod.Element, text string) {
	el.Click(proto.InputMouseButtonLeft, 1)
	time.Sleep(time.Duration(randomInt(200, 500)) * time.Millisecond)
	el.SelectAllText()
	time.Sleep(time.Duration(randomInt(100, 300)) * time.Millisecond)
	for _, ch := range text {
		el.Type(input.Key(ch))
		time.Sleep(time.Duration(randomInt(60, 200)) * time.Millisecond)
	}
	// 输入完后短暂停顿，像人一样检查
	time.Sleep(time.Duration(randomInt(300, 800)) * time.Millisecond)
}

func waitForURL(page *rod.Page, fragment string) error {
	deadline := time.Now().Add(stepTimeout)
	for time.Now().Before(deadline) {
		info, err := page.Info()
		if err == nil && strings.Contains(info.URL, fragment) {
			time.Sleep(1 * time.Second)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	info, _ := page.Info()
	cur := ""
	if info != nil {
		cur = info.URL
	}
	return fmt.Errorf("等待 %s 超时，URL: %s", fragment, cur)
}

// clickNext 用 JS 找到 Next/下一步 按钮并通过 JS 点击，等待 URL 变化。
func clickNext(page *rod.Page) error {
	info, _ := page.Info()
	oldURL := ""
	if info != nil {
		oldURL = info.URL
	}

	result, _ := page.Eval(`() => {
		const btns = document.querySelectorAll('button');
		for (const b of btns) {
			if (/next|下一步/i.test(b.textContent.trim())) {
				b.dispatchEvent(new PointerEvent('pointerdown', {bubbles:true}));
				b.dispatchEvent(new MouseEvent('mousedown', {bubbles:true}));
				b.dispatchEvent(new PointerEvent('pointerup', {bubbles:true}));
				b.dispatchEvent(new MouseEvent('mouseup', {bubbles:true}));
				b.click();
				return b.textContent.trim();
			}
		}
		return 'not-found';
	}`)
	btnText := ""
	if result != nil {
		btnText = result.Value.Str()
	}
	if btnText == "not-found" {
		return fmt.Errorf("找不到 Next 按钮")
	}

	// 住宅代理延迟大,等 30 秒 URL 变化
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		u := safeURL(page)
		if u != "" && u != oldURL {
			time.Sleep(1 * time.Second)
			return nil
		}
	}

	errResult, _ := page.Eval(`() => {
		const all = document.body.innerText;
		if (/enter a month|enter a day|enter a year|select a gender|请输入|请选择/i.test(all))
			return 'form-validation: ' + all.substring(0, 200);
		return '';
	}`)
	if errResult != nil && errResult.Value.Str() != "" {
		return fmt.Errorf("表单验证: %s", errResult.Value.Str())
	}
	return fmt.Errorf("页面未导航 (btn: %s)", btnText)
}

/* ==================== 随机数据 ==================== */

var firstNames = []string{
	"James", "Mary", "John", "Patricia", "Robert", "Jennifer", "Michael", "Linda",
	"David", "Elizabeth", "William", "Barbara", "Richard", "Susan", "Joseph", "Jessica",
	"Thomas", "Sarah", "Christopher", "Karen", "Charles", "Lisa", "Daniel", "Nancy",
	"Matthew", "Betty", "Anthony", "Margaret", "Mark", "Sandra", "Donald", "Ashley",
	"Steven", "Kimberly", "Paul", "Emily", "Andrew", "Donna", "Joshua", "Michelle",
}

var lastNames = []string{
	"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller", "Davis",
	"Rodriguez", "Martinez", "Hernandez", "Lopez", "Gonzalez", "Wilson", "Anderson",
	"Thomas", "Taylor", "Moore", "Jackson", "Martin", "Lee", "Perez", "Thompson",
	"White", "Harris", "Sanchez", "Clark", "Ramirez", "Lewis", "Robinson", "Walker",
	"Young", "Allen", "King", "Wright", "Scott", "Torres", "Nguyen", "Hill", "Flores",
}

func randomFirstName() string { return firstNames[randomInt(0, len(firstNames)-1)] }
func randomLastName() string  { return lastNames[randomInt(0, len(lastNames)-1)] }
func randomBirthYear() int    { return randomInt(1975, 2000) }

func randomInt(min, max int) int {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max-min+1)))
	if err != nil {
		return min
	}
	return min + int(n.Int64())
}

func genUsername(first, last string) string {
	return strings.ToLower(first) + strings.ToLower(last) + fmt.Sprintf("%d", randomInt(100, 99999))
}

func genPassword(length int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%"
	b := make([]byte, length)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		b[i] = chars[n.Int64()]
	}
	return string(b)
}
