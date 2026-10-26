package adobereg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
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
	signInURL  = "https://account.adobe.com/"
	fireflyURL = "https://firefly.adobe.com/generate/images"

	// 与下游 image2api 完全一致的 cookie→token 交换端点/参数：注册末尾用它主动探测
	// 账号是否被 Adobe ride 身份核验拦住（邮箱未验证 acct_evs），并拿到跳转链接去过验证。
	adobeTokenURL = "https://adobeid-na1.services.adobe.com/ims/check/v6/token?jslVersion=v2-v0.48.0-1-g1e322cb"
	adobeClientID = "clio-playground-web"
	adobeScope    = "AdobeID,firefly_api,openid,pps.read,pps.write,additional_info.projectedProductContext,additional_info.ownerOrg,uds_read,uds_write,ab.manage,read_organizations,additional_info.roles,account_cluster.read,creative_production,profile"

	// stuckReloadAfter 登录页卡住（只转圈、没渲染出创建账号入口）多久后重新加载页面。
	stuckReloadAfter = 25 * time.Second
)

// launchAdobeBrowser 按注册用的一整套反爬/代理配置启动并连接 Adobe 专用 Chromium。
// 返回的 browser 由调用方负责关闭；若返回了 bridge（认证代理桥），也要一并 Close；
// cleanup 在浏览器关闭后调用，负责清理 launcher 的临时用户数据目录。
func launchAdobeBrowser(in Input) (browser *rod.Browser, bridge *proxyutil.AuthBridge, cleanup func(), err error) {
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
	if in.Headless {
		// 与 grokreg 一致：Chrome 128 的 --headless 仍是旧无头，指纹差异大容易被拦，
		// 必须显式 new headless。
		l = l.Set("headless", "new")
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
	debugPort, perr := availableLoopbackPort()
	if perr != nil {
		return nil, nil, nil, fmt.Errorf("分配 Chrome 调试端口失败: %w", perr)
	}
	l = l.Set("remote-debugging-port", strconv.Itoa(debugPort))
	if chromePath, cerr := adobeChromiumBin(); cerr != nil {
		in.logf("准备 Adobe 专用 Chromium 失败，回退默认浏览器: %v", cerr)
	} else {
		l = l.Bin(chromePath)
		in.logf("使用 Adobe 专用 Chromium，与 GPT/Grok 浏览器隔离")
	}

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
	return browser, bridge, l.Cleanup, nil
}

func registerBrowser(ctx context.Context, in Input) (res *Result, err error) {
	if in.Headless {
		in.logf("启动无头浏览器，打开 Adobe 注册页")
	} else {
		in.logf("启动可见浏览器，打开 Adobe 注册页")
	}

	browser, authBridge, cleanup, err := launchAdobeBrowser(in)
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
	// 出口 IP 探测仅用于排障，默认跳过以省一次整页加载；需要时置 EgressCheck。
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
	if _, e := page.EvalOnNewDocument(adobeArkoseIDScript); e != nil {
		in.logf("安装 Arkose 消息 id 监听失败: %v", e)
	}
	_ = (proto.EmulationSetDeviceMetricsOverride{
		Width:             1280,
		Height:            900,
		DeviceScaleFactor: 1,
		Mobile:            false,
	}).Call(page)
	if in.Headless {
		// 与 grokreg 一致：无头 Chrome 的 UA 带 HeadlessChrome 标记，会被 Adobe 风控拦住
		// （页面停在加载动画），改回普通 Chrome。
		if ver, verr := (proto.BrowserGetVersion{}).Call(browser); verr == nil {
			if ua := cleanUserAgent(ver.UserAgent); ua != "" {
				_ = (proto.EmulationSetUserAgentOverride{
					UserAgent:      ua,
					AcceptLanguage: "en-US,en;q=0.9",
					Platform:       "Linux x86_64",
				}).Call(page)
				in.logf("无头 UA 已修正: %s", ua)
			}
		}
	}

	if err = gotoStable(ctx, page, signInURL, in, 120*time.Second); err != nil {
		return nil, err
	}
	in.logf("Adobe 登录页已加载")

	if err = gotoCreateForm(ctx, page, in); err != nil {
		return nil, err
	}
	if err = fillStep1(ctx, page, in); err != nil {
		// 邮箱已有 Adobe 账号时直接返回给 producer，由它标记为“已存在/跳过”，
		// 后续批量任务也会排除这个邮箱，不再重复注册或尝试登录。
		return nil, err
	}
	// 步骤顺序自适应：姓名/生日步与邮箱验证可能以任意顺序出现。
	if err = completeSignup(ctx, page, in); err != nil {
		return nil, err
	}

	// 新号邮箱可能未验证（Adobe 把邮箱核验延后到换 token 时），下游用
	// clio-playground-web 换 token 会被 ride_AdobeID_acct_evs 身份核验拦住。建号后
	// 立刻用浏览器 cookie 探一次交换：命中核验就直接去核验页，省掉先打开 Firefly
	// 白等一轮就绪超时。探测本身失败（网络等）则回退到采集会话后再探的老路径。
	in.logf("账号创建完成，检查换 token 状态")
	probed := false
	if cookie := cookieHeaderFromPage(page); cookie != "" {
		switch jump, perr := adobeRideJump(ctx, cookie, in.Proxy); {
		case perr != nil:
			in.logf("检查换 token 状态失败（稍后重试）: %v", perr)
		case jump == "":
			probed = true
			in.logf("cookie→token 交换成功，登录会话有效")
		default:
			probed = true
			if verr := rideVerify(ctx, page, in, jump); verr != nil {
				in.logf("自动通过身份核验失败: %v", verr)
			}
		}
	}

	in.logf("打开 Firefly 确认会话")
	if err = gotoStable(ctx, page, fireflyURL, in, 120*time.Second); err != nil {
		in.logf("打开 Firefly 时页面跳转异常，继续检测验证页: %v", err)
	}
	if err = handleEmailVerification(ctx, page, in); err != nil {
		return nil, err
	}
	// 登录 Cookie 的保存不依赖 Firefly 编辑器或 ARP SDK 就绪。
	auth, cerr := captureAuth(page, in)
	if cerr != nil {
		return nil, cerr
	}

	// 建号后那次探测没跑通时，用采集到的会话再探一次并按需过核验，
	// 确保导出的号下游可直接用。失败只记日志、保留原会话（账号仍算注册成功）。
	if !probed {
		if newAuth, ok := passAdobeRide(ctx, page, in, auth); ok {
			auth = newAuth
		}
	}
	return &Result{AuthJSON: auth}, nil
}

// cleanUserAgent 与 grokreg 一致：去掉无头 Chrome UA 里的 Headless 标记。
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

// passAdobeRide 用与下游一致的 cookie→token 交换探测账号是否被 Adobe ride 拦。
// 被拦时打开跳转核验页、自动填邮箱验证码过掉核验并重新采集会话；返回是否已更新会话。
func passAdobeRide(ctx context.Context, page *rod.Page, in Input, auth map[string]any) (map[string]any, bool) {
	cookie := cookieHeaderFromAuth(auth)
	if cookie == "" {
		return auth, false
	}
	jump, err := adobeRideJump(ctx, cookie, in.Proxy)
	if err != nil {
		in.logf("检查换 token 状态失败（不影响注册结果）: %v", err)
		return auth, false
	}
	if jump == "" {
		in.logf("cookie→token 交换成功，登录会话有效")
		return auth, false
	}
	if err := rideVerify(ctx, page, in, jump); err != nil {
		in.logf("自动通过身份核验失败: %v", err)
		return auth, false
	}
	if err := gotoStable(ctx, page, fireflyURL, in, 60*time.Second); err != nil {
		in.logf("核验后打开 Firefly 异常: %v", err)
	}
	newAuth, err := captureAuth(page, in)
	if err != nil {
		in.logf("核验后重新采集会话失败: %v", err)
		return auth, false
	}
	// 复验一次确认现在能换到 token。
	if jump2, e := adobeRideJump(ctx, cookieHeaderFromAuth(newAuth), in.Proxy); e == nil && jump2 == "" {
		in.logf("身份核验已通过，登录会话有效")
	} else {
		in.logf("身份核验后仍未能换到 token，可能需人工处理")
	}
	return newAuth, true
}

// rideVerify 打开 ride 身份核验页并用邮箱验证码过掉。取码与打开核验页并行：
// 核验页是 deeplink SPA，验证码框要几秒才渲染，这段时间正好用来收码。
func rideVerify(ctx context.Context, page *rod.Page, in Input, jump string) error {
	in.logf("检测到 Adobe 身份核验(ride_AdobeID_acct_evs)，打开核验页尝试用邮箱验证码通过")
	// 重置验证码基线，确保取到的是核验页新发的验证码而非注册阶段旧码。
	if in.ResetCodeBaseline != nil {
		in.ResetCodeBaseline()
	}
	codeCtx, cancelCode := context.WithCancel(ctx)
	defer cancelCode()
	type codeOutcome struct {
		code string
		err  error
	}
	codeCh := make(chan codeOutcome, 1)
	go func() {
		code, err := in.WaitCode(codeCtx)
		codeCh <- codeOutcome{code: code, err: err}
	}()
	verifyIn := in
	verifyIn.WaitCode = func(ctx context.Context) (string, error) {
		select {
		case out := <-codeCh:
			return out.code, out.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	if err := gotoStable(ctx, page, jump, in, 60*time.Second); err != nil {
		return fmt.Errorf("打开身份核验页失败: %w", err)
	}
	// 先等验证码框出现再处理，否则会误判为"无需验证"直接跳过。
	if !waitCleared(ctx, page, 30*time.Second, func() bool { return onEmailVerify(page, pageURL(page)) }) {
		return fmt.Errorf("等待身份核验页出现超时，当前页面: %s", trimText(pageURL(page), 120))
	}
	return handleEmailVerification(ctx, page, verifyIn)
}

// RescueRide 针对已注册但被 ride 卡住的号：用导出的 cookie 还原会话、打开核验页
// 自动用邮箱验证码过掉身份核验，再重新采集会话返回。cookies 为注册时导出的 cookie
// 列表（每项含 name/value/domain 等字段）。过验证失败不报错，返回当前会话由调用方判定。
func RescueRide(ctx context.Context, in Input, cookies []map[string]any) (res *Result, err error) {
	if in.Headless {
		in.logf("启动无头浏览器，还原会话过身份核验")
	} else {
		in.logf("启动可见浏览器，还原会话过身份核验")
	}
	browser, bridge, cleanup, err := launchAdobeBrowser(in)
	if err != nil {
		return nil, err
	}
	if bridge != nil {
		defer bridge.Close()
	}
	defer func() {
		// 关浏览器后清理 launcher 临时用户数据目录，避免残留 Profile 堆积
		_ = rod.Try(browser.MustClose)
		cleanup()
	}()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("Adobe 救回流程异常: %v", r)
		}
	}()

	page := browser.MustPage("")
	_ = (proto.EmulationSetDeviceMetricsOverride{
		Width:             1280,
		Height:            900,
		DeviceScaleFactor: 1,
		Mobile:            false,
	}).Call(page)

	if err := setAdobeCookies(page, cookies); err != nil {
		return nil, fmt.Errorf("还原 cookie 失败: %w", err)
	}
	if err := gotoStable(ctx, page, fireflyURL, in, 90*time.Second); err != nil {
		in.logf("打开 Firefly 异常（继续尝试过核验）: %v", err)
	}
	// 恢复登录只需要 Cookie，无需等待编辑器或采集 ARP。
	auth, cerr := captureAuth(page, in)
	if cerr != nil {
		return nil, cerr
	}
	if newAuth, ok := passAdobeRide(ctx, page, in, auth); ok {
		auth = newAuth
	}
	return &Result{AuthJSON: auth}, nil
}

// setAdobeCookies 把导出的 cookie 列表注入浏览器，用于还原已注册号的登录态。
func setAdobeCookies(page *rod.Page, cookies []map[string]any) error {
	params := make([]*proto.NetworkCookieParam, 0, len(cookies))
	for _, c := range cookies {
		name, _ := c["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		value, _ := c["value"].(string)
		p := &proto.NetworkCookieParam{Name: name, Value: value}
		if d, ok := c["domain"].(string); ok {
			p.Domain = d
		}
		if pt, ok := c["path"].(string); ok {
			p.Path = pt
		}
		if b, ok := c["secure"].(bool); ok {
			p.Secure = b
		}
		if b, ok := c["httpOnly"].(bool); ok {
			p.HTTPOnly = b
		}
		if f, ok := c["expires"].(float64); ok && f > 0 {
			p.Expires = proto.TimeSinceEpoch(f)
		}
		if ss, ok := c["sameSite"].(string); ok {
			switch ss {
			case "Strict", "Lax", "None":
				p.SameSite = proto.NetworkCookieSameSite(ss)
			}
		}
		params = append(params, p)
	}
	if len(params) == 0 {
		return fmt.Errorf("无有效 cookie")
	}
	return page.SetCookies(params)
}

// cookieHeaderFromPage 直接从浏览器读全量 cookie 拼成 HTTP Cookie 请求头，
// 用于建号后立即探测换 token 状态（不必先采集整份会话）。
func cookieHeaderFromPage(page *rod.Page) string {
	all, err := proto.NetworkGetAllCookies{}.Call(page)
	if err != nil {
		return ""
	}
	parts := make([]string, 0, len(all.Cookies))
	for _, c := range all.Cookies {
		if strings.TrimSpace(c.Name) == "" {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// cookieHeaderFromAuth 把 captureAuth 产出的 cookies 拼成 HTTP Cookie 请求头。
func cookieHeaderFromAuth(auth map[string]any) string {
	list := cookiesFromAuthMap(auth)
	parts := make([]string, 0, len(list))
	for _, ck := range list {
		name, _ := ck["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		value, _ := ck["value"].(string)
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, "; ")
}

// adobeRideJump 做一次 cookie→token 交换：换到 token 返回空串（登录会话有效）；被 ride
// 身份核验拦住则返回跳转核验页的 URL；其它错误返回 err（调用方按不阻断处理）。
func adobeRideJump(ctx context.Context, cookie, proxy string) (string, error) {
	body := "client_id=" + adobeClientID + "&guest_allowed=true&scope=" + url.QueryEscape(adobeScope)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, adobeTokenURL, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded;charset=UTF-8")
	req.Header.Set("origin", "https://firefly.adobe.com")
	req.Header.Set("referer", "https://firefly.adobe.com/")
	req.Header.Set("cookie", cookie)
	req.Header.Set("user-agent", userAgent)
	client := httpClientWithProxy(proxy)
	client.Timeout = 30 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload struct {
		Error       string `json:"error"`
		Jump        string `json:"jump"`
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(raw, &payload)
	if resp.StatusCode == http.StatusOK && strings.TrimSpace(payload.AccessToken) != "" {
		return "", nil
	}
	if payload.Jump != "" {
		return payload.Jump, nil
	}
	return "", fmt.Errorf("换 token 失败(%d) %s", resp.StatusCode, payload.Error)
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

// gotoCreateForm 从登录页进入「创建账号」表单（同时出现邮箱与密码输入框）。
// 登录页是 SPA，代理慢时偶发一直转圈、什么都不渲染；卡住就重新加载页面重试，
// 而不是干等到超时判失败（同一个号第二次点生产往往就过，就是这个原因）。
func gotoCreateForm(ctx context.Context, page *rod.Page, in Input) error {
	deadline := time.Now().Add(90 * time.Second)
	nextReload := time.Now().Add(stuckReloadAfter)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if hasSel(page, `input[name="username"]`) && hasSel(page, `input[name="password"]`) {
			in.logf("已进入创建账号表单")
			return nil
		}
		if time.Now().After(nextReload) {
			in.logf("登录页仍未渲染出创建账号入口，重新加载后再试")
			_ = gotoStable(ctx, page, signInURL, in, 45*time.Second)
			nextReload = time.Now().Add(stuckReloadAfter)
			continue
		}
		clickByText(page, `a,span,button`, `create an account`)
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("未能进入 Adobe 创建账号表单")
}

// fillStep1 填写邮箱+密码并提交第一步。整段最多重试 3 次：某次输入/提交
// 卡住或超时，就重新加载注册页、重新进入创建表单后再来一遍，而不是直接判失败
// （headed 模式下 React 表单偶发重渲染/节点失效，整体重试比单点重试更稳）。
func fillStep1(ctx context.Context, page *rod.Page, in Input) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt > 0 {
			in.logf("第一步重试第 %d 次：重新加载注册页", attempt)
			_ = gotoStable(ctx, page, signInURL, in, 60*time.Second)
			if err := gotoCreateForm(ctx, page, in); err != nil {
				lastErr = err
				continue
			}
		}
		if err := fillInput(ctx, page, `input[name="username"]`, in.Email, 45*time.Second); err != nil {
			lastErr = fmt.Errorf("输入邮箱失败: %w", err)
			continue
		}
		if onAccountExists(page) {
			return ErrAccountExists
		}
		if err := fillInput(ctx, page, `input[name="password"]`, in.Password, 30*time.Second); err != nil {
			lastErr = fmt.Errorf("输入密码失败: %w", err)
			continue
		}
		if onAccountExists(page) {
			return ErrAccountExists
		}
		in.logf("已填写邮箱与密码，提交第一步")
		// 提交后等待离开邮箱/密码步：出现姓名框、出现验证码框，或密码框消失。
		leftStep1 := func() bool {
			return hasSel(page, `input[name="firstname"]`) ||
				onEmailVerify(page, pageURL(page)) ||
				!hasSel(page, `input[name="password"]`)
		}
		if err := submitAndAdvance(ctx, page, in, leftStep1, 60*time.Second); err != nil {
			if onAccountExists(page) {
				return ErrAccountExists
			}
			lastErr = fmt.Errorf("提交第一步失败: %w", err)
			if errors.Is(err, errCaptchaPuzzle) {
				break // 图形验证是 IP 级风控，重新加载注册页也过不去
			}
			continue
		}
		return nil
	}
	return lastErr
}

// completeSignup 在第一步之后自适应处理后续步骤：邮箱验证与姓名/生日步
// 可能以任意顺序出现，循环处理直到两者都已完成、页面离开注册表单。
//
// 超时按“阶段”计算：每完成邮箱验证或创建账号提交，就重置 3 分钟窗口。
// Arkose 打码可能耗时数分钟，不能用进入函数时的固定截止时间把提交后的
// 结果检查窗口挤掉；判定超时前也会再查一次页面，避免刚好跳转完成被误判。
func completeSignup(ctx context.Context, page *rod.Page, in Input) error {
	const stepTimeout = 180 * time.Second
	deadline := time.Now().Add(stepTimeout)
	extend := func(reason string) {
		deadline = time.Now().Add(stepTimeout)
		in.logf("注册阶段完成（%s），重置 3 分钟步骤超时", reason)
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		u := pageURL(page)
		if isSignupErrorURL(u) {
			return fmt.Errorf("注册落到错误页: %s", trimText(u, 200))
		}
		if isSignupDoneURL(u) {
			in.logf("注册表单已完成: %s", trimText(u, 120))
			return nil
		}
		if onEmailVerify(page, u) {
			if err := handleEmailVerification(ctx, page, in); err != nil {
				return err
			}
			extend("邮箱验证")
			continue
		}
		if hasSel(page, `input[name="firstname"]`) {
			if err := fillStep2(ctx, page, in); err != nil {
				return err
			}
			extend("创建账号提交")
			continue
		}
		if time.Now().After(deadline) {
			// 页面可能刚好在超时边界跳转完成，失败前再确认一次。
			u = pageURL(page)
			if isSignupDoneURL(u) {
				in.logf("注册表单已完成: %s", trimText(u, 120))
				return nil
			}
			return fmt.Errorf("完成注册步骤超时（可能遇到额外校验，当前页面: %s）", trimText(u, 200))
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// isSignupErrorURL 判断是否落到了 Adobe 的错误页。
func isSignupErrorURL(u string) bool {
	lu := strings.ToLower(strings.TrimSpace(u))
	return strings.Contains(lu, "/error") || strings.Contains(lu, "error=")
}

// isSignupDoneURL 判断注册表单是否已经完成（离开 signup/create-account 流程）。
// 空 URL（正在跳转）和邮箱验证页不算完成。
func isSignupDoneURL(u string) bool {
	lu := strings.ToLower(strings.TrimSpace(u))
	if lu == "" || isSignupErrorURL(lu) || !strings.Contains(lu, "adobe.com") {
		return false
	}
	for _, block := range []string{"signup", "create-account", "#/create", "email-verification", "/password"} {
		if strings.Contains(lu, block) {
			return false
		}
	}
	return true
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

// errCaptchaPuzzle 表示 Adobe 弹出了 hCaptcha 图形验证（同一出口 IP 注册过多触发），
// 刷新页面无法消除，直接失败并提示换 IP 比反复重试更省时间。
var errCaptchaPuzzle = errors.New("触发 Adobe 图形验证（同 IP 注册过多），建议开启 2Captcha 打码、开启 Adobe 独立代理或降低注册频率")

// errArkosePuzzle 表示 Adobe 走到了 Arkose FunCaptcha。它和 hCaptcha 是两套
// 完全不同的接口，不能用 hCaptcha 的打码结果顶替。
var errArkosePuzzle = errors.New("触发 Adobe Arkose 图形验证，请在系统设置启用 2Captcha 打码后重试")

// errArkoseTokenRejected 表示打码服务返回了 token，但页面没有接受（Arkose 仍存在）。
// 这类情况应切换到备用打码服务重试，而不是用同一家继续解。
var errArkoseTokenRejected = errors.New("Arkose token 未被页面接受")

// ErrAccountExists 表示 Adobe 明确提示该邮箱已有账号。此时继续重试注册页
// 没有意义，应改用库存密码登录复用这个账号。
var ErrAccountExists = errors.New("Adobe 邮箱已有账号")

// onCaptchaPuzzle 检测 hCaptcha 验证弹窗（"Please solve a few puzzles"）。
func onCaptchaPuzzle(page *rod.Page) bool {
	return hasSel(page, `iframe[src*="hcaptcha.com"]`)
}

// onArkosePuzzle 检测 Adobe 当前使用的 Arkose FunCaptcha（arks-client/adobe 或
// arkoselabs 的 enforcement iframe，新版还会同时渲染 arkose-<id>-wrapper）。
func onArkosePuzzle(page *rod.Page) bool {
	return hasSel(page, `iframe[src*="arkoselabs"],iframe[src*="arks-client"],div[class*="arkose-"]`)
}

// onAccountExists 检测 Adobe 创建账号页的“该邮箱已有账号”提示。
func onAccountExists(page *rod.Page) bool {
	res, err := page.Timeout(3 * time.Second).Eval(`() => (document.body ? document.body.innerText : '').toLowerCase()`)
	if err != nil {
		return false
	}
	s := res.Value.Str()
	return strings.Contains(s, "already exists") ||
		strings.Contains(s, "account with this email") ||
		strings.Contains(s, "已有账号")
}

// maxCaptchaSolves 单次提交里最多打码几次，避免反复失败白烧打码额度。
const maxCaptchaSolves = 2

// maxArkoseSolves 单次提交里最多解几次 Arkose；token 被拒时再解一次仍失败就
// 直接报错，避免无限循环。
const maxArkoseSolves = 2

// adobeArkoseIDScript 在页面脚本之前挂上 message 监听，记录 Arkose iframe
// 发给父页面的消息 id。父页面 Arkose API 校验的 id 是它自己生成的 io.id，
// 不是 iframe hash 里的 session；直接拿 hash 里的 session 注入会被丢弃。
const adobeArkoseIDScript = `window.__adobeArkId = ''; window.addEventListener('message', function (e) {
  try {
    var d = (typeof e.data === 'string') ? JSON.parse(e.data) : e.data;
    if (d && d.id && d.message) { window.__adobeArkId = d.id; }
  } catch (_) {}
}, true);`

// arkoseChallenge 是 Arkose enforcement iframe 里的打码参数。iframe 的 hash
// 形如 #<publickey>&<session-id>&<parent-origin>；GameURL 是内层 game-core
// 谜题 iframe，2Captcha 只有拿这个 URL 当 pageurl 才能解出来。
type arkoseChallenge struct {
	Src       string
	PublicKey string
	SessionID string
	MessageID string
	Surl      string
	GameURL   string
}

// arkoseInfo 从当前页面的 Arkose iframe 解析 publickey / session id / surl，
// 并读取提前捕获到的父页面消息 id。
func arkoseInfo(page *rod.Page) (*arkoseChallenge, error) {
	res, err := page.Timeout(10 * time.Second).Eval(`() => {
		const f = [...document.querySelectorAll('iframe')].find(x => /enforcement/.test(x.src) && /(arkoselabs|arks-client)/.test(x.src));
		if (!f) return '';
		const u = new URL(f.src);
		const parts = decodeURIComponent((u.hash || '').replace(/^#/, '')).split('&');
		return JSON.stringify({ src: f.src, pk: parts[0] || '', sid: parts[1] || '', surl: u.origin, mid: window.__adobeArkId || '' });
	}`)
	if err != nil {
		return nil, fmt.Errorf("读取 Arkose 参数失败: %w", err)
	}
	var raw struct {
		Src  string `json:"src"`
		PK   string `json:"pk"`
		SID  string `json:"sid"`
		Surl string `json:"surl"`
		MID  string `json:"mid"`
	}
	if json.Unmarshal([]byte(res.Value.Str()), &raw) != nil || raw.PK == "" {
		return nil, fmt.Errorf("未取到 Arkose publickey")
	}
	mid := raw.MID
	if mid == "" {
		mid = raw.SID
	}
	return &arkoseChallenge{Src: raw.Src, PublicKey: raw.PK, SessionID: raw.SID, MessageID: mid, Surl: raw.Surl}, nil
}

// arkoseGameURL 从 CDP 的 frame tree 里找内层 game-core 谜题 iframe。
func arkoseGameURL(page *rod.Page) string {
	res, err := proto.PageGetFrameTree{}.Call(page)
	if err != nil || res == nil || res.FrameTree == nil {
		return ""
	}
	var walk func(*proto.PageFrameTree) string
	walk = func(t *proto.PageFrameTree) string {
		if t == nil {
			return ""
		}
		if t.Frame != nil && strings.Contains(t.Frame.URL, "game-core") {
			return t.Frame.URL
		}
		for _, c := range t.ChildFrames {
			if u := walk(c); u != "" {
				return u
			}
		}
		return ""
	}
	return walk(res.FrameTree)
}

// solveArkose 用 2Captcha 解 Arkose，并把 token 以 Arkose iframe 完成事件的
// 形式回送给页面。父页面监听 message 事件，并校验 event.source / id，所以这里
// 用 MessageEvent 构造一个 source 指向该 iframe 的事件，而不是直接改隐藏域。
func solveArkose(ctx context.Context, page *rod.Page, in Input, challenge *arkoseChallenge) error {
	ua := userAgent
	if v, err := page.Timeout(5 * time.Second).Eval(`navigator.userAgent`); err == nil && strings.TrimSpace(v.Value.Str()) != "" {
		ua = v.Value.Str()
	}
	// game-core 谜题 iframe 通常比 enforcement iframe 晚几百毫秒出现。
	for i := 0; i < 40 && challenge.GameURL == ""; i++ {
		challenge.GameURL = arkoseGameURL(page)
		if challenge.GameURL != "" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if challenge.GameURL == "" {
		return fmt.Errorf("未找到 Arkose game-core 谜题 iframe")
	}
	solveURL := challenge.GameURL
	in.logf("检测到 Adobe Arkose 图形验证，提交打码 (game=%s id=%s)", trimText(challenge.GameURL, 160), trimText(challenge.MessageID, 60))
	token, err := in.Captcha.SolveFunCaptcha(ctx, challenge.PublicKey, solveURL, challenge.Surl, ua)
	if err != nil {
		return fmt.Errorf("Arkose 打码失败: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("Arkose 打码返回空 token")
	}
	if err := injectArkoseToken(page, challenge, token); err != nil {
		return err
	}
	in.logf("Arkose 打码成功，已注入 token 并等待页面继续")
	// 页面收到完成事件后才会发 create-account 请求。等 Arkose 消失；若仍在，
	// 说明 token 被页面/服务端拒绝，交给调用方切换备用打码服务。
	if !waitCleared(ctx, page, 10*time.Second, func() bool { return !onArkosePuzzle(page) }) {
		return errArkoseTokenRejected
	}
	return nil
}

func injectArkoseToken(page *rod.Page, challenge *arkoseChallenge, token string) error {
	// 1.40.0 的父页面只接受 source=window、id 为捕获到的 message id 的完成事件；
	// iframe 当 source 或 hash 里的 session 当 id 都会被静默忽略，页面会重弹 Arkose。
	mid := challenge.MessageID
	if mid == "" {
		mid = challenge.SessionID
	}
	ok, err := page.Timeout(15*time.Second).Eval(`(token, mid, src) => {
		const ev = new MessageEvent('message', {
			data: JSON.stringify({ message: 'challenge completed', id: mid, data: { token: token } }),
			origin: new URL(src).origin,
			source: window,
		});
		window.dispatchEvent(ev);
		return true;
	}`, token, mid, challenge.Src)
	if err != nil {
		return fmt.Errorf("注入 Arkose token 失败: %w", err)
	}
	if !ok.Value.Bool() {
		return fmt.Errorf("注入 Arkose token 失败")
	}
	return nil
}

// solveCaptcha 调打码服务解掉页面上的 hCaptcha，并把 token 回填进页面。
func solveCaptcha(ctx context.Context, page *rod.Page, in Input) error {
	sitekey, rqdata := hcaptchaInfo(page)
	if sitekey == "" {
		return fmt.Errorf("未取到 hCaptcha sitekey")
	}
	in.logf("检测到 hCaptcha（sitekey=%s），提交打码服务", trimText(sitekey, 40))
	token, err := in.Captcha.SolveHCaptcha(ctx, sitekey, pageURL(page), rqdata, userAgent)
	if err != nil {
		return err
	}
	if err := injectHCaptchaToken(page, token); err != nil {
		return fmt.Errorf("回填打码结果失败: %w", err)
	}
	in.logf("打码成功，已回填 token 并继续提交")
	return nil
}

// hcaptchaInfo 从 hCaptcha iframe 的 URL（参数可能在 query 或 hash 里）取 sitekey 与
// enterprise 版的 rqdata；取不到 iframe 时退回读 data-sitekey 属性。
func hcaptchaInfo(page *rod.Page) (sitekey, rqdata string) {
	res, err := page.Eval(`() => {
		const pick = (raw) => {
			const p = new URLSearchParams(raw.replace(/^[?#]/, ''));
			return { sitekey: p.get('sitekey') || '', rqdata: p.get('rqdata') || '' };
		};
		for (const f of document.querySelectorAll('iframe[src*="hcaptcha.com"]')) {
			try {
				const u = new URL(f.src);
				const a = pick(u.search), b = pick(u.hash);
				const sk = a.sitekey || b.sitekey;
				if (sk) return JSON.stringify({ sitekey: sk, rqdata: a.rqdata || b.rqdata });
			} catch (e) {}
		}
		const el = document.querySelector('[data-sitekey]');
		if (el) return JSON.stringify({ sitekey: el.getAttribute('data-sitekey'), rqdata: '' });
		return '';
	}`)
	if err != nil || res == nil {
		return "", ""
	}
	var info struct {
		Sitekey string `json:"sitekey"`
		Rqdata  string `json:"rqdata"`
	}
	if json.Unmarshal([]byte(res.Value.Str()), &info) != nil {
		return "", ""
	}
	return info.Sitekey, info.Rqdata
}

// injectHCaptchaToken 把打码拿到的 token 写进 h-captcha-response 隐藏域并触发
// 页面注册的 data-callback，让表单认为验证已通过。
func injectHCaptchaToken(page *rod.Page, token string) error {
	_, err := page.Eval(`(token) => {
		const setVal = (el) => {
			el.value = token;
			el.dispatchEvent(new Event('input', { bubbles: true }));
			el.dispatchEvent(new Event('change', { bubbles: true }));
		};
		const sel = 'textarea[name="h-captcha-response"],input[name="h-captcha-response"],textarea[name="g-recaptcha-response"]';
		const fields = document.querySelectorAll(sel);
		fields.forEach(setVal);
		if (!fields.length) {
			const ta = document.createElement('textarea');
			ta.name = 'h-captcha-response';
			ta.style.display = 'none';
			(document.querySelector('form') || document.body).appendChild(ta);
			setVal(ta);
		}
		const holder = document.querySelector('[data-callback]');
		const cb = holder && holder.getAttribute('data-callback');
		if (cb && typeof window[cb] === 'function') {
			try { window[cb](token); } catch (e) {}
		}
		return fields.length;
	}`, token)
	return err
}

// submitAndAdvance 点击当前表单主 CTA（Continue / Create account）并确认页面
// 真正进入下一步；Adobe 的提交按钮在表单校验通过前为 disabled，且偶尔首次
// 点击不生效，因此等按钮可点后点击，未推进则重试。
func submitAndAdvance(ctx context.Context, page *rod.Page, in Input, advanced func() bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	solves := 0
	arkoseSolves := 0
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if advanced() {
			return nil
		}
		if onArkosePuzzle(page) || onCaptchaPuzzle(page) {
			// 已经出现验证，别再点提交，直接进入下面的打码分支。
		} else {
			clickPrimary(page)
		}
		// 细粒度轮询：页面通常 1~2 秒内推进，1 秒一探会白等大半秒。
		for i := 0; i < 24; i++ {
			if advanced() {
				return nil
			}
			if onArkosePuzzle(page) || onCaptchaPuzzle(page) {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if onCaptchaPuzzle(page) {
			if in.Captcha == nil || solves >= maxCaptchaSolves {
				return errCaptchaPuzzle
			}
			solves++
			if err := solveCaptcha(ctx, page, in); err != nil {
				in.logf("打码失败: %v", err)
				return errCaptchaPuzzle
			}
			deadline = time.Now().Add(timeout) // 打码耗时不算进提交超时
			continue
		}
		if onArkosePuzzle(page) {
			if in.Captcha == nil {
				return errArkosePuzzle
			}
			if arkoseSolves >= maxArkoseSolves {
				return errArkosePuzzle
			}
			arkoseSolves++
			challenge, err := arkoseInfo(page)
			if err != nil {
				in.logf("读取 Arkose 参数失败: %v", err)
				return errArkosePuzzle
			}
			if err := solveArkose(ctx, page, in, challenge); err != nil {
				in.logf("%v", err)
				if errors.Is(err, errArkoseTokenRejected) {
					if p, ok := in.Captcha.(interface{ PreferSecondary() }); ok {
						p.PreferSecondary()
						in.logf("token 被页面拒绝，已切换备用打码服务重试")
						deadline = time.Now().Add(timeout)
						continue
					}
				}
				return errArkosePuzzle
			}
			deadline = time.Now().Add(timeout)
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
		in.logf("等待 Adobe 核验验证码（最多 2 分钟）...")
		codeCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		code, err := in.WaitCode(codeCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("获取邮箱验证码失败: %w", err)
		}
		if err = fillInput(ctx, page, `input.PinInput-Input`, code, 45*time.Second); err != nil {
			return fmt.Errorf("填写验证码失败: %w", err)
		}
		// 填满后 Adobe 通常自动提交；再点一次主按钮并按回车兜底。
		clickPrimary(page)
		if pin, perr := waitVisible(page, `input.PinInput-Input`, 10*time.Second); perr == nil {
			_ = pin.Type(input.Enter)
		}
		if waitCleared(ctx, page, 25*time.Second, func() bool { return !onEmailVerify(page, pageURL(page)) }) {
			in.logf("邮箱验证码校验通过")
			return nil
		}
		// 记录页面实际提示，区分“码错/过期”与“按钮没点上”。
		if txt, terr := page.Timeout(5 * time.Second).Eval(`document.body ? document.body.innerText : ''`); terr == nil {
			in.logf("验证码未通过，页面提示: %s", trimText(txt.Value.Str(), 200))
		}
		if attempt == 0 {
			clickByText(page, `button,a,span`, `resend code`)
			clickByText(page, `button,a,span`, `resend`)
			clickByText(page, `button,a,span`, `重新发送`)
			if in.ResetCodeBaseline != nil {
				in.ResetCodeBaseline()
			}
			in.logf("已请求重发验证码，等待新邮件后重试一次")
			continue
		}
		return fmt.Errorf("邮箱验证码校验未通过")
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
		if strings.Contains(u, "firefly.adobe.com") {
			ready, err := page.Timeout(3 * time.Second).Eval(`() => {
				try {
					return !!sessionStorage.getItem('riskSessionId') &&
						!!(window.SherlockSdk && typeof window.SherlockSdk.Sherlock === 'function');
				} catch (_) { return false; }
			}`)
			if err == nil && ready.Value.Bool() {
				in.logf("Firefly 编辑器与 ARP SDK 已就绪: %s", trimText(u, 120))
				return nil
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("等待 Firefly 编辑器与 ARP SDK 就绪超时，当前页面: %s", trimText(pageURL(page), 160))
}

// captureArpSessionToken 取 Firefly 的 x-arp-session-id。
// ArpJS（Sherlock）用 sessionStorage.riskSessionId 生成加密 token，再通过
// tokenCallback 交给 Firefly；这里按同样方式主动实例化一次 Sherlock，
// 把回调值捕获下来，不需要真的发起一次生图请求。
func captureArpSessionToken(page *rod.Page, in Input) string {
	readStr := func(expr string) string {
		res, err := page.Timeout(8 * time.Second).Eval(expr)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(res.Value.Str())
	}
	// ArpJS 往往要等 Firefly 页面交互后才加载；缺 riskSessionId 时轻触一下提示框。
	// Firefly 落地页不会加载 ArpJS；先进入编辑器视图再采集。
	if !strings.Contains(pageURL(page), "view=generate") {
		if err := gotoStable(context.Background(), page, "https://firefly.adobe.com/generate/images?view=generate", in, 60*time.Second); err != nil {
			in.logf("打开 Firefly 编辑器失败: %v", err)
		}
	}
	in.logf("ARP 采集页面: %s", trimText(pageURL(page), 120))
	for i := 0; i < 45; i++ {
		if readStr(`sessionStorage.getItem('riskSessionId')||''`) != "" &&
			readStr(`(window.SherlockSdk && typeof window.SherlockSdk.Sherlock) ? '1' : ''`) != "" {
			break
		}
		switch i {
		case 2, 7, 12, 17, 22, 27, 32:
			_, _ = page.Timeout(5 * time.Second).Eval(`() => {
				const visible = e => !!(e.offsetWidth || e.offsetHeight || e.getClientRects().length);
				const t = [...document.querySelectorAll('textarea, [contenteditable="true"]')].find(visible) || document.querySelector('textarea');
				if (!t) return false;
				t.focus();
				t.value = 'a';
				t.dispatchEvent(new Event('input', { bubbles: true }));
				t.dispatchEvent(new Event('change', { bubbles: true }));
				return true;
			}`)
		}
		time.Sleep(1 * time.Second)
	}
	res, err := page.Timeout(60 * time.Second).Eval(`() => new Promise((resolve) => {
		let done = false;
		const diag = {};
		const finish = (v) => { if (!done) { done = true; resolve(String(v || '')); } };
		try {
			const sid = sessionStorage.getItem('riskSessionId') || '';
			diag.sid = !!sid;
			diag.sdk = !!(window.SherlockSdk && window.SherlockSdk.Sherlock);
			if (!sid || !window.SherlockSdk || !window.SherlockSdk.Sherlock) return finish('DIAG:' + JSON.stringify(diag));
			const cfg = {
				clientId: 'clio-playground-web',
				sessionId: sid,
				prodEnv: true,
				logging: false,
				vendors: ['forter', 'arkose', 'bfp'],
				tokenCallback: function (token) { finish(token); }
			};
			const inst = new window.SherlockSdk.Sherlock(cfg);
			diag.inst = !!inst;
			diag.build = typeof (inst && inst._buildAndSendSessionToken);
			setTimeout(function () {
				try { inst._buildAndSendSessionToken(); diag.called = true; }
				catch (e) { diag.callErr = String(e); }
			}, 2500);
		} catch (e) { diag.err = String(e); }
		setTimeout(function () { finish('DIAG:' + JSON.stringify(diag)); }, 20000);
	})`)
	if err != nil {
		in.logf("ARP token 采集失败: %v", err)
		return ""
	}
	token := strings.TrimSpace(res.Value.Str())
	if strings.HasPrefix(token, "DIAG:") {
		in.logf("ARP token 采集诊断: %s", trimText(strings.TrimPrefix(token, "DIAG:"), 200))
		return ""
	}
	if token == "" {
		in.logf("等待 ARP session token 超时（页面 %s）", trimText(pageURL(page), 120))
		return ""
	}
	in.logf("已捕获 ARP session token（长度 %d）", len(token))
	return token
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

	storageRaw := page.MustEval(`() => {
		const read = (storage) => { try { return Object.fromEntries(Object.entries(storage)); } catch (_) { return {}; } };
		return JSON.stringify({
			localStorage: read(window.localStorage),
			sessionStorage: read(window.sessionStorage),
			location: location.href
		});
	}`).String()
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

// fillInput 往输入框写入文本：优先逐字符人工输入（有真实按键节奏，降低被
// Adobe 行为风控判为机器人、进而触发 ride 身份核验的概率），人工输入未生效
// 时再退回原生 setter 兜底。每步都有独立超时并在总预算内重试。
func fillInput(ctx context.Context, page *rod.Page, selector, value string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = func() error {
			// 先确认输入框已出现（有独立超时，不会拖到几分钟）。
			el, err := waitVisible(page, selector, 12*time.Second)
			if err != nil {
				return err
			}
			// 优先逐字符人工输入（真实按键事件+节奏，最像真人）。
			if err := typeHuman(el, value); err != nil {
				return err
			}
			if inputValue(page, selector) == value {
				return nil
			}
			// 人工输入未生效（React 重渲染替换了节点等），退回原生 setter 兜底。
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

// setInputValue 聚焦输入框并用原生 setter 赋值、派发 input/change，兼容 React
// 受控组件。带独立超时，避免 CDP 卡顿时单次 eval 阻塞过久。
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

// waitVisible 等待选择器命中的「可见」元素：页面可能同时渲染多份表单
// （一份隐藏），首个匹配不一定可见，这里在所有匹配里挑第一个可见的。
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
	// 点击聚焦可能因浮层遮挡一直等「可交互」直到超时；限时尝试，失败改用 JS 聚焦
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

func trimText(s string, n int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if len(s) <= n {
		return s
	}
	return s[:n]
}
