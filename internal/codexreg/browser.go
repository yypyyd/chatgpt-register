package codexreg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
)

// ErrAccountTaken 注册时提示"账号不存在或已被删除/停用"，视为该地址已被注册，不应重试。
var ErrAccountTaken = errors.New("账号不存在或已被删除/停用")

// ErrTermsRejected 填完资料提交后命中 "We can't create your account due to our Terms of Use"
// 拒绝页——通常是出口 IP 风控命中。原地重试无意义，交由上层换出口 IP 后重试或标记为不可注册。
var ErrTermsRejected = errors.New("账号创建被 Terms of Use 拒绝")

// ErrIPBlocked 出口 IP 被 Cloudflare / OpenAI 拦下：整页人机验证、或提交邮箱后服务端一直不响应。
// 与账号无关，换出口 IP 重试即可，不应按"邮箱失败"进入冷却。
var ErrIPBlocked = errors.New("出口 IP 被拦截（Cloudflare 人机验证 / 提交无响应）")

// browserResult 浏览器流程的产出：accessToken 以及本次注册所用的浏览器指纹信息，
// 一并存进账号资料，方便下游按同样的 UA / 地区去使用这个号。
type browserResult struct {
	AccessToken string
	Cookies     []WebCookie
	UserAgent   string
	Screen      ScreenProfile
	Locale      string
	Languages   string
	EgressIP    string
	Country     string
	Timezone    string
	Warmed      bool
}

// registerBrowser 启动浏览器完成 ChatGPT 账号注册并返回 accessToken。
// in.Proxy 为空则直连；非空时 Chrome 走该代理，并按出口 IP 做 GeoIP 对齐。
func registerBrowser(ctx context.Context, in Input) (res *browserResult, err error) {
	in.logf("🚀 启动浏览器自动化注册流程...")

	// 1. 先经代理出口查地理位置（Go 自己发 HTTP，不占浏览器），启动浏览器时就把语言 / 时区对齐。
	geo := lookupGeoIPViaRequest(in)
	locale, languages := "en-US", "en-US,en"
	if geo != nil {
		locale, languages = localeForCountry(geo.CountryCode)
	}

	// 2. 拿浏览器：有池就从池里分配一个独立上下文，否则独占启动一个进程。
	var sess *Session
	if in.Pool != nil {
		sess, err = in.Pool.Acquire(ctx, ContextOptions{Proxy: in.Proxy, Locale: locale, Languages: languages, Log: in.Log})
	} else {
		sess, err = LaunchBrowser(ctx, LaunchOptions{
			Headless: in.Headless, Proxy: in.Proxy, BrowserBin: in.BrowserBin,
			Locale: locale, Languages: languages, Log: in.Log,
		})
	}
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	res = &browserResult{UserAgent: sess.UserAgent, Screen: sess.Screen, Locale: locale, Languages: languages}
	if geo != nil {
		res.EgressIP, res.Country, res.Timezone = geo.Query, geo.CountryCode, geo.Timezone
	}

	// 失败现场截图：无论是返回错误还是 MustXxx panic，都在关浏览器前把当前页面截图交给 SaveShot。
	// base 只绑任务 ctx、不带超时；每一步操作都从它派生自己的 Timeout，谁也不会把别人的 ctx 取消掉。
	var base *rod.Page
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("注册流程异常: %v", r)
		}
		if err == nil || base == nil || in.SaveShot == nil {
			return
		}
		func() {
			defer func() {
				if r2 := recover(); r2 != nil {
					in.logf("📸 截图失败(panic): %v", r2)
				}
			}()
			// 任务 ctx 可能已超时，截图用独立 ctx。
			shotPage := base.Context(context.Background()).Timeout(15 * time.Second)
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

	base, err = sess.NewPage()
	if err != nil {
		return nil, fmt.Errorf("打开标签页失败: %w", err)
	}
	if geo != nil {
		applyGeo(base, geo, in)
	}

	// 3. 打开 ChatGPT 注册页
	in.logf("🌐 正在打开 ChatGPT 注册页...")
	nav := base.Timeout(120 * time.Second)
	nav.MustNavigate("https://chatgpt.com/auth/login")
	nav.MustWaitLoad()
	// 登录页有 A/B 多套布局，邮箱输入框不一定是 #email，按多个候选选择器匹配。
	const emailSel = `#email, input[type="email"], input[name="email"], input[autocomplete="email"]`
	emailEl, err := waitEmailForm(ctx, base, in, emailSel, 60*time.Second)
	if err != nil {
		return nil, err
	}
	in.logf("✅ 注册页已加载")
	// 真人会先看一眼页面再动手。
	pause(900*time.Millisecond, 2200*time.Millisecond)

	// 4. 输入邮箱并提交（逐键打字 + 真实鼠标点击）
	page := base.Timeout(60 * time.Second)
	if err := HumanType(page, emailEl, in.Email, false); err != nil {
		return nil, fmt.Errorf("输入邮箱失败: %w", err)
	}
	pause(300*time.Millisecond, 900*time.Millisecond)
	clickSubmit(page, in, "Continue|继续")
	in.logf("📧 已提交邮箱，等待下一步...")

	// 4.1 提交邮箱后可能出现"Create a password"创建密码页（在验证码之前）。
	// 用状态机识别：密码页则填入密码并 Continue；否则直接进入验证码环节。
	// 提交后页面长时间没有任何变化（按钮一直转圈）= 出口 IP 被 OpenAI 拦在服务端，换 IP 重试。
	codeReady := false
	passwordDone := false
	emailResubmitted := false
	for attempt := 0; attempt < 5 && !codeReady; attempt++ {
		wait := 45 * time.Second
		if emailResubmitted || passwordDone {
			wait = 60 * time.Second
		}
		state, serr := pollState(ctx, base, wait, map[string]string{
			"code":     "input[name='code']",
			"password": "input[type='password']",
		})
		if serr != nil {
			return nil, serr
		}
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
			pause(700*time.Millisecond, 1600*time.Millisecond)
			pg := base.Timeout(60 * time.Second)
			pw := pg.MustElement("input[type='password']")
			if err := HumanType(pg, pw, in.Password, true); err != nil {
				return nil, fmt.Errorf("输入密码失败: %w", err)
			}
			pause(300*time.Millisecond, 800*time.Millisecond)
			clickSubmit(pg, in, "Continue|继续")
			passwordDone = true
			time.Sleep(2 * time.Second)
		case "":
			pg := base.Timeout(30 * time.Second)
			if !emailResubmitted && !passwordDone && !submitPending(pg) {
				// 按钮没在转圈：说明点击根本没生效（可能被 label/浮层吃掉），再提交一次。
				in.logf("⚠ 提交邮箱后页面无变化且按钮未进入处理态，重新提交一次。页面按钮: %s", describeButtons(pg))
				if el, e := pg.Timeout(5 * time.Second).Element(emailSel); e == nil && el != nil {
					_ = HumanClick(pg, el)
					pause(200*time.Millisecond, 500*time.Millisecond)
				}
				clickSubmit(pg, in, "Continue|继续")
				emailResubmitted = true
				continue
			}
			// 按钮一直转圈没有响应：邮箱提交被服务端吞掉，基本是 IP 层面被拦。
			in.logf("🛡 提交后按钮持续处理中无响应。页面按钮: %s", describeButtons(pg))
			return nil, fmt.Errorf("%w：提交邮箱后 %s 无响应", ErrIPBlocked, wait)
		}
	}
	if !codeReady {
		return nil, fmt.Errorf("等待验证码输入框超时")
	}
	in.logf("📨 验证码输入框已出现，正在从邮箱读取验证码...")

	// 5. 自动读取验证码（由 producer 通过邮箱轮询提供）
	code, err := in.FetchCode(ctx, time.Time{})
	if err != nil {
		return nil, fmt.Errorf("获取邮箱验证码失败: %w", err)
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, fmt.Errorf("未获取到验证码")
	}
	// FetchCode 轮询邮件可能耗时较久，这里为提交验证码单独派生一份超时。
	page = base.Timeout(120 * time.Second)
	// 真人要切到邮箱看码再切回来。
	pause(1200*time.Millisecond, 2800*time.Millisecond)
	if err := HumanType(page, page.MustElement("input[name='code']"), code, true); err != nil {
		return nil, fmt.Errorf("输入验证码失败: %w", err)
	}
	pause(300*time.Millisecond, 800*time.Millisecond)
	clickSubmit(page, in, "Continue|继续")
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
	stillCode := 0
	for attempt := 0; attempt < 12 && !ready; attempt++ {
		state, serr := pollStateJS(ctx, base, 60*time.Second, afterCodeStateJS)
		if serr != nil {
			return nil, serr
		}
		// 本轮动作用的页面句柄，与轮询互不影响。
		pg := base.Timeout(60 * time.Second)
		switch state {
		case "ready":
			ready = true
		case "disabled":
			return nil, ErrAccountTaken
		case "terms":
			in.logf("⛔ 命中 Terms of Use 拒绝页")
			return nil, ErrTermsRejected
		case "retry":
			in.logf("⚠ 页面报错(Operation timed out)，点击 Try again 继续")
			if el, e := pg.Timeout(10*time.Second).ElementR("button", "Try again|重试"); e == nil && el != nil {
				_ = HumanClick(pg, el)
			}
			time.Sleep(3 * time.Second)
		case "profile":
			if profileSubmitted {
				// 已提交过资料仍停在该页（过渡态或 Terms 报错渲染中），稍等重判，避免重复提交
				time.Sleep(2 * time.Second)
				continue
			}
			in.logf("📝 账户完善页面已出现")
			pause(800*time.Millisecond, 1800*time.Millisecond)
			t0 := time.Now()
			nameEl, e1 := pg.Timeout(10 * time.Second).Element("input[name='name']")
			if e1 != nil {
				return nil, fmt.Errorf("定位姓名输入框失败: %w", e1)
			}
			if err := HumanType(pg, nameEl, in.FullName, true); err != nil {
				return nil, fmt.Errorf("输入姓名失败: %w", err)
			}
			pause(400*time.Millisecond, 1000*time.Millisecond)
			ageEl, e2 := pg.Timeout(10 * time.Second).Element("input[name='age']")
			if e2 != nil {
				return nil, fmt.Errorf("定位年龄输入框失败: %w", e2)
			}
			if err := HumanType(pg, ageEl, in.Age, true); err != nil {
				return nil, fmt.Errorf("输入年龄失败: %w", err)
			}
			pause(500*time.Millisecond, 1200*time.Millisecond)
			clickSubmit(pg, in, "Continue|继续|Agree|同意")
			in.logf("👤 已提交资料 (name/age)，耗时 %s", time.Since(t0).Round(time.Millisecond))
			profileSubmitted = true
			time.Sleep(2 * time.Second)
		case "stillcode":
			if codeResent {
				return nil, fmt.Errorf("验证码提交后仍停在验证码页（重发后仍未通过）")
			}
			// 提交后验证码页可能短暂停留（服务端校验中/下一页未渲染），先多等几轮再当作验证码无效。
			if stillCode < 5 {
				stillCode++
				time.Sleep(3 * time.Second)
				continue
			}
			in.logf("📨 仍停在验证码页，点击重发并重新读取验证码")
			// Resend 可能是按钮/链接；找不到就直接重新抓码重填（可能只是上次码解析有误）。
			var resentAt time.Time
			if el, e := pg.Timeout(10*time.Second).ElementR("button, a, [role='button']", "Resend|重新发送"); e == nil && el != nil {
				resentAt = time.Now()
				_ = HumanClick(pg, el)
				time.Sleep(8 * time.Second)
			}
			// 点过重发就只等新邮件，否则会把刚被拒绝的旧码再提交一遍。
			newCode, ferr := in.FetchCode(ctx, resentAt)
			if ferr != nil {
				return nil, fmt.Errorf("重发后获取验证码失败: %w", ferr)
			}
			newCode = strings.TrimSpace(newCode)
			if newCode == "" {
				return nil, fmt.Errorf("重发后未获取到验证码")
			}
			pg2 := base.Timeout(60 * time.Second)
			if err := HumanType(pg2, pg2.MustElement("input[name='code']"), newCode, true); err != nil {
				return nil, fmt.Errorf("重新输入验证码失败: %w", err)
			}
			pause(300*time.Millisecond, 800*time.Millisecond)
			clickSubmit(pg2, in, "Continue|继续")
			in.logf("🔑 已重新提交验证码")
			codeResent = true
			time.Sleep(3 * time.Second)
		case "":
			return nil, fmt.Errorf("提交验证码后页面 60 秒无变化")
		}
	}
	if !ready {
		return nil, fmt.Errorf("等待 ChatGPT 主界面超时")
	}
	in.logf("✅ ChatGPT 主界面已就绪")

	// 6.1 预热：在注册浏览器里发一条普通对话，让账号的首次使用与注册同源。
	if in.Warmup {
		in.logf("💬 预热：在注册浏览器里发一条普通对话...")
		if werr := warmupChat(base, in); werr != nil {
			in.logf("⛔ 网页会话验证失败: %v", werr)
			// 留一张现场图，方便看前端改版后的界面长什么样。
			if in.SaveShot != nil {
				if data, serr := base.Timeout(15*time.Second).Screenshot(false, nil); serr == nil && len(data) > 0 {
					in.SaveShot(data)
					in.logf("📸 已保存预热现场截图")
				}
			}
			return nil, fmt.Errorf("网页会话验证失败: %w", werr)
		} else {
			res.Warmed = true
			in.logf("💬 预热对话完成")
		}
	}

	// 7. 导航到 /api/auth/session 读取 accessToken（重置超时，避免沿用已耗尽的预算）
	in.logf("🔑 提取 accessToken...")
	page = base.Timeout(60 * time.Second)
	page.MustNavigate("https://chatgpt.com/api/auth/session")
	page.MustWaitLoad()
	body := page.MustElement("body").MustText()

	var sessionData map[string]any
	if err := json.Unmarshal([]byte(body), &sessionData); err != nil {
		return nil, fmt.Errorf("解析 session JSON 失败: %w", err)
	}
	accessToken, ok := sessionData["accessToken"].(string)
	if !ok || accessToken == "" {
		return nil, fmt.Errorf("未找到 accessToken，可能未登录成功")
	}
	in.logf("🔑 accessToken 获取成功")
	res.AccessToken = accessToken

	// accessToken 只是网页会话派生出的短期凭据，不能代替登录 Cookie。
	// 在销毁临时 BrowserContext 前保存完整 Cookie，后续测活和网页导出才能恢复同一个会话。
	cookies, cerr := CaptureWebCookies(sess.Browser)
	if cerr != nil {
		return nil, cerr
	}
	res.Cookies = cookies
	in.logf("🍪 ChatGPT 网页会话已保存（%d 个 Cookie）", len(cookies))
	return res, nil
}

// waitEmailForm 等注册页渲染出邮箱输入框。期间若出现 Cloudflare 整页人机验证（"Verify you are human"），
// 说明出口 IP 信誉不够，直接按 ErrIPBlocked 返回让上层换 IP，而不是白等 60 秒再报 deadline。
func waitEmailForm(ctx context.Context, page *rod.Page, in Input, sel string, timeout time.Duration) (*rod.Element, error) {
	deadline := time.Now().Add(timeout)
	cfLogged := false
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		pg := page.Timeout(5 * time.Second)
		if has, el, err := pg.Has(sel); err == nil && has && el != nil {
			if vis, _ := el.Visible(); vis {
				return el, nil
			}
		}
		if isCloudflareChallenge(pg) {
			if !cfLogged {
				in.logf("🛡 页面出现 Cloudflare 人机验证，等待其自动放行...")
				cfLogged = true
			}
			// managed 模式常常几秒后自己过；给它 25 秒，过不了就是 IP 被判定了。
			if time.Until(deadline) > 25*time.Second {
				deadline = time.Now().Add(25 * time.Second)
			}
		}
		time.Sleep(700 * time.Millisecond)
	}
	if cfLogged {
		return nil, fmt.Errorf("%w：Cloudflare 人机验证未放行", ErrIPBlocked)
	}
	return nil, fmt.Errorf("等待注册页邮箱输入框超时")
}

// isCloudflareChallenge 当前页面是否为 Cloudflare 的整页拦截 / 可见 Turnstile 挑战。
func isCloudflareChallenge(pg *rod.Page) bool {
	v, err := pg.Eval(`() => {
		const t = document.body ? (document.body.innerText || '') : '';
		if (/Verify you are human|Checking your browser|Just a moment|Attention Required|验证您是真人|确认您是真人/i.test(t)) return true;
		if (document.querySelector('#challenge-running, #challenge-stage, #cf-challenge-running, .cf-browser-verification')) return true;
		for (const f of document.querySelectorAll('iframe')) {
			const s = (f.src || '') + ' ' + (f.title || '');
			const r = f.getBoundingClientRect();
			if (/challenges\.cloudflare\.com|turnstile/i.test(s) && r.width >= 100 && r.height >= 40) return true;
		}
		return false;
	}`)
	return err == nil && v.Value.Bool()
}

// pollState 轮询若干选择器，返回第一个出现的状态名；超时返回 ""（不 panic，交由调用方判定）。
func pollState(ctx context.Context, page *rod.Page, timeout time.Duration, states map[string]string) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		pg := page.Timeout(5 * time.Second)
		for name, sel := range states {
			if has, el, err := pg.Has(sel); err == nil && has && el != nil {
				return name, nil
			}
		}
		if isCloudflareChallenge(pg) {
			return "", fmt.Errorf("%w：流程中途出现 Cloudflare 人机验证", ErrIPBlocked)
		}
		time.Sleep(700 * time.Millisecond)
	}
	return "", nil
}

// afterCodeStateJS 一次 Eval 判定提交验证码后的页面状态，避免多个 ElementR 逐个全文匹配。
const afterCodeStateJS = `() => {
	const q = s => document.querySelector(s);
	if (q("#prompt-textarea, textarea[name='prompt-textarea']")) return 'ready';
	const t = document.body ? (document.body.innerText || '') : '';
	if (/You do not have an account|deleted or deactivated/i.test(t)) return 'disabled';
	if (/[Cc]an.t create your account|create your account due to our Terms/.test(t)) return 'terms';
	for (const b of document.querySelectorAll('button')) {
		if (/Try again|重试/.test(b.innerText || '')) return 'retry';
	}
	if (q("input[name='name']")) return 'profile';
	if (q("input[name='code']")) return 'stillcode';
	return '';
}`

// pollStateJS 用一段 JS 反复判定页面状态，直到返回非空或超时。
func pollStateJS(ctx context.Context, page *rod.Page, timeout time.Duration, js string) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		pg := page.Timeout(5 * time.Second)
		if v, err := pg.Eval(js); err == nil {
			if st := v.Value.Str(); st != "" {
				return st, nil
			}
		}
		if isCloudflareChallenge(pg) {
			return "", fmt.Errorf("%w：流程中途出现 Cloudflare 人机验证", ErrIPBlocked)
		}
		time.Sleep(700 * time.Millisecond)
	}
	return "", nil
}

// submitButtonJS 找当前表单的提交按钮，与页面语言无关：优先输入框所在 form 里的 submit 按钮，
// 其次页面上唯一可见的主按钮（排除 Google / Apple / 手机号等第三方登录按钮）。
const submitButtonJS = `() => {
	const visible = el => { const r = el.getBoundingClientRect(); return r.width > 0 && r.height > 0 && getComputedStyle(el).visibility !== 'hidden'; };
	const input = document.activeElement && /^(INPUT|TEXTAREA)$/.test(document.activeElement.tagName) ? document.activeElement : document.querySelector('input:focus, input[type=email], input[name=code], input[name=age], input[type=password]');
	const form = input && input.form;
	if (form) {
		const b = [...form.querySelectorAll("button[type=submit], input[type=submit]")].find(visible) ||
			[...form.querySelectorAll("button:not([type=button]):not([type=reset])")].find(visible);
		if (b) return b;
	}
	const cands = [...document.querySelectorAll("button[type=submit]")].filter(visible);
	if (cands.length === 1) return cands[0];
	const third = /google|apple|microsoft|phone|電話|电话|手机/i;
	const main = [...document.querySelectorAll('button')].filter(b => visible(b) && !third.test(b.innerText || '') && !b.querySelector('svg + span, img'));
	return main.length === 1 ? main[0] : null;
}`

// clickSubmit 提交当前表单：先用真实鼠标点表单的提交按钮（与语言无关），点不到就按文案匹配，
// 再不行就在当前输入框里按回车——真人也常这么提交。返回是否成功触发了提交动作。
func clickSubmit(pg *rod.Page, in Input, textRe string) bool {
	if el, err := pg.Timeout(6 * time.Second).ElementByJS(rod.Eval(submitButtonJS)); err == nil && el != nil {
		if cerr := HumanClick(pg, el); cerr == nil {
			return true
		}
	}
	if el, err := pg.Timeout(4*time.Second).ElementR("button", textRe); err == nil && el != nil {
		if cerr := HumanClick(pg, el); cerr == nil {
			return true
		}
	}
	in.logf("⚠ 未找到提交按钮，改按回车提交。页面按钮: %s", describeButtons(pg))
	if err := pg.Keyboard.Type(input.Enter); err != nil {
		in.logf("⚠ 回车提交失败: %v", err)
		return false
	}
	return true
}

// describeButtons 列出页面上可见按钮的文案/类型/禁用状态，排障用。
func describeButtons(pg *rod.Page) string {
	v, err := pg.Timeout(3 * time.Second).Eval(`() => [...document.querySelectorAll('button')]
		.filter(b => { const r = b.getBoundingClientRect(); return r.width > 0 && r.height > 0; })
		.slice(0, 8)
		.map(b => ((b.innerText || b.getAttribute('aria-label') || '').replace(/\s+/g, ' ').trim().slice(0, 30) || '(无文案)') + '[' + (b.type || '') + (b.disabled ? ',disabled' : '') + ']')
		.join(' | ')`)
	if err != nil {
		return "(读取失败: " + err.Error() + ")"
	}
	return v.Value.Str()
}

// submitPending 提交按钮是否处于"正在处理"态（禁用 / 转圈）。用来区分"点击没生效"和"服务端不响应"。
func submitPending(pg *rod.Page) bool {
	v, err := pg.Timeout(3 * time.Second).Eval(`() => {
		const b = [...document.querySelectorAll("button[type=submit], form button")].find(x => { const r = x.getBoundingClientRect(); return r.width > 0 && r.height > 0; });
		if (!b) return false;
		return b.disabled || b.getAttribute('aria-busy') === 'true' || !!b.querySelector('svg.animate-spin, [class*="spin"], [class*="loading"]');
	}`)
	return err == nil && v.Value.Bool()
}
