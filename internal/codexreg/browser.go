package codexreg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/stealth"
)

// ErrAccountTaken 注册时提示"账号不存在或已被删除/停用"，视为该地址已被注册，不应重试。
var ErrAccountTaken = errors.New("账号不存在或已被删除/停用")

// ErrTermsRejected 填完资料提交后命中 "We can't create your account due to our Terms of Use"
// 拒绝页——通常是出口 IP 风控命中。原地重试无意义，交由上层换出口 IP 后重试或标记为不可注册。
var ErrTermsRejected = errors.New("账号创建被 Terms of Use 拒绝")

// registerBrowser 启动浏览器完成 ChatGPT 账号注册并返回 accessToken。
// in.Proxy 为空则直连；非空时 Chrome 走该代理，并按出口 IP 做 GeoIP 对齐。
func registerBrowser(ctx context.Context, in Input) (token string, err error) {
	in.logf("🚀 启动浏览器自动化注册流程...")

	// 1. 启动 Chrome，禁用自动化特征
	l := launcher.New().
		Headless(in.Headless).
		NoSandbox(true).
		Set("disable-dev-shm-usage").
		Append("--disable-blink-features", "AutomationControlled").
		Append("--disable-infobars", "").
		Append("--no-first-run", "").
		Append("--no-default-browser-check", "").
		Append("--window-size", "1280,800")

	// 1.1 挂代理（账号密码交给 HandleAuth）
	var proxyUser, proxyPass string
	if strings.TrimSpace(in.Proxy) != "" {
		server, user, pass, perr := parseProxy(in.Proxy)
		if perr != nil {
			return "", fmt.Errorf("解析代理失败: %w", perr)
		}
		l = l.Set("proxy-server", server)
		proxyUser, proxyPass = user, pass
		in.logf("🌐 使用代理: %s", server)
	}

	controlURL, err := l.Launch()
	if err != nil {
		return "", fmt.Errorf("启动 Chrome 失败: %w", err)
	}
	browser := rod.New().ControlURL(controlURL)
	if err := browser.Connect(); err != nil {
		return "", fmt.Errorf("连接 Chrome 失败: %w", err)
	}
	defer func() {
		// 关浏览器后清理 launcher 临时用户数据目录，避免残留 Profile 堆积
		_ = rod.Try(browser.MustClose)
		l.Cleanup()
	}()

	// 失败现场截图：无论是返回错误还是 MustXxx panic，都在关浏览器前把当前页面截图交给 SaveShot。
	var page *rod.Page
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("注册流程异常: %v", r)
		}
		if err == nil || page == nil || in.SaveShot == nil {
			return
		}
		func() {
			defer func() {
				if r2 := recover(); r2 != nil {
					in.logf("📸 截图失败(panic): %v", r2)
				}
			}()
			shotPage := page.CancelTimeout().Timeout(15 * time.Second)
			data, serr := shotPage.Screenshot(false, nil)
			if serr != nil {
				in.logf("📸 截图失败: %v", serr)
				return
			}
			if len(data) == 0 {
				in.logf("📸 截图失败: 空数据")
				return
			}
			in.SaveShot(data)
			in.logf("📸 已保存失败现场截图")
		}()
	}()

	// 1.2 代理需要账号密码认证时，后台处理 Chrome 弹出的认证请求。
	// 注意：必须用非 Must 版本并 recover——MustHandleAuth 在独立 goroutine 里 panic
	// 会绕过调用方的 recover 直接把整个进程带崩。
	if proxyUser != "" || proxyPass != "" {
		go func() {
			defer func() { _ = recover() }()
			wait := browser.HandleAuth(proxyUser, proxyPass)
			_ = wait()
		}()
	}

	// 2. GeoIP：先经代理出口用 HTTP 请求查询地理位置，以便创建页面时一次性注入一致指纹
	geo := lookupGeoIPViaRequest(in)
	acceptLang := "en-US,en;q=0.9"
	if geo != nil {
		_, acceptLang = localeForCountry(geo.CountryCode)
	}

	// 2.1 stealth 隐身插件 + 真实 User-Agent（创建即注入与地理位置一致的指纹）
	page = stealth.MustPage(browser)
	page.MustSetUserAgent(&proto.NetworkSetUserAgentOverride{
		UserAgent:      userAgent,
		AcceptLanguage: acceptLang,
		Platform:       "Win32",
	})

	// 2.3 对齐时区/坐标/locale（UA/语言已在上面按地理信息注入）
	if geo != nil {
		applyGeo(page, geo, in)
	}

	page = page.Timeout(120 * time.Second)

	// 3. 打开 ChatGPT 注册页
	in.logf("🌐 正在打开 ChatGPT 注册页...")
	page.MustNavigate("https://chatgpt.com/auth/login")
	page.MustWaitLoad() 
	page.MustElement("#email").MustWaitVisible()
	in.logf("✅ 注册页已加载")

	// 4. 输入邮箱并提交（用 JS 点击，避免元素被遮挡/未进入可点击态时 MustClick 失败）
	page.MustElement("#email").MustInput(in.Email)
	page.MustElement("button[type='submit']").MustEval(`() => this.click()`)
	in.logf("📧 已提交邮箱，等待下一步...")

	// 4.1 提交邮箱后可能出现"Create a password"创建密码页（在验证码之前）。
	// 用状态机识别：密码页则填入密码并 Continue；否则直接进入验证码环节。
	codeReady := false
	passwordDone := false
	for attempt := 0; attempt < 4 && !codeReady; attempt++ {
		pg := page.CancelTimeout().Timeout(60 * time.Second)
		state := ""
		pg.Race().
			Element("input[name='code']").MustHandle(func(_ *rod.Element) {
			state = "code"
		}).
			Element("input[type='password']").MustHandle(func(_ *rod.Element) {
			state = "password"
		}).
			MustDo()
		switch state {
		case "code":
			codeReady = true
		case "password":
			if passwordDone {
				// 密码页仍在（提交后的过渡态），稍等再重新检测，避免重复填写
				time.Sleep(2 * time.Second)
				continue
			}
			in.logf("🔒 创建密码页已出现，自动设置密码")
			pw := pg.MustElement("input[type='password']")
			pw.MustSelectAllText().MustInput(in.Password)
			pg.MustElement("button[type='submit']").MustEval(`() => this.click()`)
			passwordDone = true
			time.Sleep(2 * time.Second)
		}
	}
	if !codeReady {
		return "", fmt.Errorf("等待验证码输入框超时")
	}
	in.logf("📨 验证码输入框已出现，正在从邮箱读取验证码...")

	// 5. 自动读取验证码（由 producer 通过邮箱轮询提供）
	code, err := in.FetchCode(ctx)
	if err != nil {
		return "", fmt.Errorf("获取邮箱验证码失败: %w", err)
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return "", fmt.Errorf("未获取到验证码")
	}
	// FetchCode 轮询邮件可能耗时较久，会耗尽之前设置的页面超时预算；
	// 提交验证码前刷新一次超时，避免后续操作报 context canceled。
	page = page.CancelTimeout().Timeout(120 * time.Second)
	page.MustElement("input[name='code']").MustInput(code)
	page.MustElement("button[type='submit']").MustEval(`() => this.click()`)
	in.logf("🔑 已提交验证码")
	// 提交后稍等，让页面离开验证码页，避免把过渡态误判为"卡在验证码"。
	time.Sleep(3 * time.Second)

	// 6. 提交验证码后的页面状态机：
	//   主界面(成功) / 账号停用 / Terms of Use 拒绝(硬拒绝) /
	//   "Oops"(Operation timed out) 报错页(Try again) / 资料页(name/age) /
	//   仍停在验证码页(码无效/过期 → 重发并重填一次)。
	ready := false
	codeResent := false
	profileSubmitted := false
	for attempt := 0; attempt < 8 && !ready; attempt++ {
		pg := page.CancelTimeout().Timeout(60 * time.Second)
		state := ""
		pg.Race().
			Element("textarea[name='prompt-textarea']").MustHandle(func(_ *rod.Element) {
			state = "ready"
		}).
			ElementR("body", "You do not have an account|deleted or deactivated").MustHandle(func(_ *rod.Element) {
			state = "disabled"
		}).
			// 仅匹配拒绝报错文案本身，不要匹配资料页底部 "you agree to our Terms of Use" 的常规声明。
			ElementR("body", "[Cc]an.t create your account|create your account due to our Terms").MustHandle(func(_ *rod.Element) {
			state = "terms"
		}).
			ElementR("button", "Try again|重试").MustHandle(func(_ *rod.Element) {
			state = "retry"
		}).
			Element("input[name='name']").MustHandle(func(_ *rod.Element) {
			state = "profile"
		}).
			Element("input[name='code']").MustHandle(func(_ *rod.Element) {
			state = "stillcode"
		}).
			MustDo()
		switch state {
		case "ready":
			ready = true
		case "disabled":
			return "", ErrAccountTaken
		case "terms":
			in.logf("⛔ 命中 Terms of Use 拒绝页")
			return "", ErrTermsRejected
		case "retry":
			in.logf("⚠ 页面报错(Operation timed out)，点击 Try again 继续")
			pg.MustElementR("button", "Try again|重试").MustEval(`() => this.click()`)
			time.Sleep(3 * time.Second)
		case "profile":
			if profileSubmitted {
				// 已提交过资料仍停在该页（过渡态或 Terms 报错渲染中），稍等重判，避免重复提交
				time.Sleep(2 * time.Second)
				continue
			}
			in.logf("📝 账户完善页面已出现")
			name := pg.MustElement("input[name='name']")
			name.MustSelectAllText().MustInput(in.FullName)
			age := pg.MustElement("input[name='age']")
			age.MustSelectAllText().MustInput(in.Age)
			pg.MustElement("button[type='submit']").MustEval(`() => this.click()`)
			in.logf("👤 已提交资料 (name/age)")
			profileSubmitted = true
			time.Sleep(2 * time.Second)
		case "stillcode":
			if codeResent {
				return "", fmt.Errorf("验证码提交后仍停在验证码页（重发后仍未通过）")
			}
			in.logf("📨 仍停在验证码页，点击重发并重新读取验证码")
			// Resend 可能是按钮/链接；找不到就直接重新抓码重填（可能只是上次码解析有误）。
			if el, e := pg.ElementR("button, a, [role='button']", "Resend|重新发送"); e == nil && el != nil {
				el.MustEval(`() => this.click()`)
				time.Sleep(8 * time.Second)
			}
			newCode, ferr := in.FetchCode(ctx)
			if ferr != nil {
				return "", fmt.Errorf("重发后获取验证码失败: %w", ferr)
			}
			newCode = strings.TrimSpace(newCode)
			if newCode == "" {
				return "", fmt.Errorf("重发后未获取到验证码")
			}
			pg2 := page.CancelTimeout().Timeout(60 * time.Second)
			pg2.MustElement("input[name='code']").MustSelectAllText().MustInput(newCode)
			pg2.MustElement("button[type='submit']").MustEval(`() => this.click()`)
			in.logf("🔑 已重新提交验证码")
			codeResent = true
			time.Sleep(3 * time.Second)
		}
	}
	if !ready {
		return "", fmt.Errorf("等待 ChatGPT 主界面超时")
	}
	in.logf("✅ ChatGPT 主界面已就绪，提取 accessToken...")

	// 7. 导航到 /api/auth/session 读取 accessToken（重置超时，避免沿用已耗尽的预算）
	page = page.CancelTimeout().Timeout(60 * time.Second)
	page.MustNavigate("https://chatgpt.com/api/auth/session")
	page.MustWaitLoad()
	body := page.MustElement("body").MustText()

	var sessionData map[string]any
	if err := json.Unmarshal([]byte(body), &sessionData); err != nil {
		return "", fmt.Errorf("解析 session JSON 失败: %w", err)
	}
	accessToken, ok := sessionData["accessToken"].(string)
	if !ok || accessToken == "" {
		return "", fmt.Errorf("未找到 accessToken，可能未登录成功")
	}
	in.logf("🔑 accessToken 获取成功")
	return accessToken, nil
}
