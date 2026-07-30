package adobereg

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
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

	// 与下游 image2api 完全一致的 cookie→token 交换端点/参数：注册末尾用它主动探测
	// 账号是否被 Adobe ride 身份核验拦住（邮箱未验证 acct_evs），并拿到跳转链接去过验证。
	adobeTokenURL = "https://adobeid-na1.services.adobe.com/ims/check/v6/token?jslVersion=v2-v0.48.0-1-g1e322cb"
	adobeClientID = "clio-playground-web"
	adobeScope    = "AdobeID,firefly_api,openid,pps.read,pps.write,additional_info.projectedProductContext,additional_info.ownerOrg,uds_read,uds_write,ab.manage,read_organizations,additional_info.roles,account_cluster.read,creative_production,profile"
)

// launchAdobeBrowser 按注册用的一整套反爬/代理配置启动并连接 Adobe 专用 Chromium。
// 返回的 browser 由调用方负责关闭；若返回了 bridge（认证代理桥），也要一并 Close。
func launchAdobeBrowser(in Input) (browser *rod.Browser, bridge *localAuthProxyBridge, err error) {
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
		return nil, nil, fmt.Errorf("分配 Chrome 调试端口失败: %w", perr)
	}
	l = l.Set("remote-debugging-port", strconv.Itoa(debugPort))
	if chromePath, cerr := adobeChromiumBin(); cerr != nil {
		in.logf("准备 Adobe 专用 Chromium 失败，回退默认浏览器: %v", cerr)
	} else {
		l = l.Bin(chromePath)
		in.logf("使用 Adobe 专用 Chromium，与 GPT/Grok 浏览器隔离")
	}

	if strings.TrimSpace(in.Proxy) != "" {
		server, user, pass, perr := parseProxy(in.Proxy)
		if perr != nil {
			return nil, nil, fmt.Errorf("解析代理失败: %w", perr)
		}
		if user != "" || pass != "" {
			bridge, server, perr = startLocalAuthProxyBridge(in.Proxy)
			if perr != nil {
				return nil, nil, fmt.Errorf("启动认证代理桥失败: %w", perr)
			}
			in.logf("已启用 Chromium 本地认证代理桥")
		}
		l = l.Set("proxy-server", server)
		in.logf("使用代理: %s", server)
	}

	controlURL, lerr := l.Launch()
	if lerr != nil {
		if bridge != nil {
			bridge.Close()
		}
		return nil, nil, fmt.Errorf("启动 Chrome 失败: %w", lerr)
	}
	browser = rod.New().NoDefaultDevice().ControlURL(controlURL)
	if cerr := browser.Connect(); cerr != nil {
		if bridge != nil {
			bridge.Close()
		}
		return nil, nil, fmt.Errorf("连接 Chrome 失败: %w", cerr)
	}
	return browser, bridge, nil
}

func registerBrowser(ctx context.Context, in Input) (res *Result, err error) {
	if in.Headless {
		in.logf("启动无头浏览器，打开 Adobe 注册页")
	} else {
		in.logf("启动可见浏览器，打开 Adobe 注册页")
	}

	browser, authBridge, err := launchAdobeBrowser(in)
	if err != nil {
		return nil, err
	}
	proxyConfigured := strings.TrimSpace(in.Proxy) != ""
	if authBridge != nil {
		defer authBridge.Close()
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
	// 会话无论 Firefly 是否按预期跳转都会照常采集，这里不必久等：25s 足够正常
	// 跳转完成，超时也直接进入采集，避免像账号建成后白等两分钟。
	if err = waitFireflyReady(ctx, page, in, 25*time.Second); err != nil {
		in.logf("等待 Firefly 就绪超时（账号已创建），继续采集会话: %v", err)
	}

	auth, cerr := captureAuth(page, in)
	if cerr != nil {
		return nil, cerr
	}

	// 新号邮箱可能未验证（Adobe 现在把邮箱核验延后到换 token 时），下游用
	// clio-playground-web 换 token 会被 ride_AdobeID_acct_evs 身份核验拦住。这里
	// 主动做一次交换：若被拦，就打开核验页用邮箱验证码过掉，再重新采集会话，
	// 确保导出的号下游可直接用。失败只记日志、保留原会话（账号仍算注册成功）。
	if newAuth, ok := passAdobeRide(ctx, page, in, auth); ok {
		auth = newAuth
	}
	return &Result{AuthJSON: auth}, nil
}

// passAdobeRide 用与下游一致的 cookie→token 交换探测账号是否被 Adobe ride 拦。
// 被拦时打开跳转核验页、自动填邮箱验证码过掉核验并重新采集会话；返回是否已更新会话。
func passAdobeRide(ctx context.Context, page *rod.Page, in Input, auth map[string]any) (map[string]any, bool) {
	cookie := cookieHeaderFromAuth(auth)
	if cookie == "" {
		return auth, false
	}
	jump, err := adobeRideJump(ctx, cookie)
	if err != nil {
		in.logf("检查换 token 状态失败（不影响注册结果）: %v", err)
		return auth, false
	}
	if jump == "" {
		in.logf("cookie→token 交换成功，账号下游可用")
		return auth, false
	}
	in.logf("检测到 Adobe 身份核验(ride_AdobeID_acct_evs)，打开核验页尝试用邮箱验证码通过")
	// 重置验证码基线，确保取到的是核验页新发的验证码而非注册阶段旧码。
	if in.ResetCodeBaseline != nil {
		in.ResetCodeBaseline()
	}
	if err := gotoStable(ctx, page, jump, in, 60*time.Second); err != nil {
		in.logf("打开身份核验页失败: %v", err)
		return auth, false
	}
	// 跳转是 deeplink SPA，验证码框（PinInput）要几秒才渲染，先等它出现再处理，
	// 否则会误判为"无需验证"直接跳过。
	if !waitCleared(ctx, page, 30*time.Second, func() bool { return onEmailVerify(page, pageURL(page)) }) {
		in.logf("等待身份核验页出现超时，当前页面: %s", trimText(pageURL(page), 120))
		return auth, false
	}
	if err := handleEmailVerification(ctx, page, in); err != nil {
		in.logf("自动通过身份核验失败: %v", err)
		return auth, false
	}
	if err := gotoStable(ctx, page, fireflyURL, in, 60*time.Second); err != nil {
		in.logf("核验后打开 Firefly 异常: %v", err)
	}
	_ = waitFireflyReady(ctx, page, in, 20*time.Second)
	newAuth, err := captureAuth(page, in)
	if err != nil {
		in.logf("核验后重新采集会话失败: %v", err)
		return auth, false
	}
	// 复验一次确认现在能换到 token。
	if jump2, e := adobeRideJump(ctx, cookieHeaderFromAuth(newAuth)); e == nil && jump2 == "" {
		in.logf("身份核验已通过，账号下游可用")
	} else {
		in.logf("身份核验后仍未能换到 token，可能需人工处理")
	}
	return newAuth, true
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
	browser, bridge, err := launchAdobeBrowser(in)
	if err != nil {
		return nil, err
	}
	if bridge != nil {
		defer bridge.Close()
	}
	defer browser.MustClose()
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

// cookieHeaderFromAuth 把 captureAuth 产出的 cookies 拼成 HTTP Cookie 请求头。
func cookieHeaderFromAuth(auth map[string]any) string {
	list, ok := auth["cookies"].([]map[string]any)
	if !ok {
		return ""
	}
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

// adobeRideJump 做一次 cookie→token 交换：换到 token 返回空串（下游可用）；被 ride
// 身份核验拦住则返回跳转核验页的 URL；其它错误返回 err（调用方按不阻断处理）。
func adobeRideJump(ctx context.Context, cookie string) (string, error) {
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
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusOK {
		return "", nil
	}
	var payload struct {
		Error string `json:"error"`
		Jump  string `json:"jump"`
	}
	_ = json.Unmarshal(raw, &payload)
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
		time.Sleep(700 * time.Millisecond)
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
		time.Sleep(700 * time.Millisecond)
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
		if err := fillInput(ctx, page, `input[name="password"]`, in.Password, 30*time.Second); err != nil {
			lastErr = fmt.Errorf("输入密码失败: %w", err)
			continue
		}
		in.logf("已填写邮箱与密码，提交第一步")
		// 提交后等待离开邮箱/密码步：出现姓名框、出现验证码框，或密码框消失。
		leftStep1 := func() bool {
			return hasSel(page, `input[name="firstname"]`) ||
				onEmailVerify(page, pageURL(page)) ||
				!hasSel(page, `input[name="password"]`)
		}
		if err := submitAndAdvance(ctx, page, in, leftStep1, 60*time.Second); err != nil {
			lastErr = fmt.Errorf("提交第一步失败: %w", err)
			continue
		}
		return nil
	}
	return lastErr
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
		time.Sleep(700 * time.Millisecond)
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
		time.Sleep(1 * time.Second)
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
		const el = document.querySelector(selector);
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
