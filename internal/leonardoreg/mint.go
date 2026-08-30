package leonardoreg

import (
	"context"
	"fmt"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// MintOptions 铸造一枚 Turnstile token 的入参。
type MintOptions struct {
	// PageURL 铸造 token 的页面：必须是目标站点自己的页面，Cloudflare 会校验域名。
	PageURL string
	Sitekey string
	// Action 与站点前端渲染组件时传的 action 一致，留空则不传。
	Action   string
	Proxy    string
	Headless bool
	Log      func(format string, a ...any)
}

// MintTurnstile 打开目标站点页面、在页面里显式渲染一个 Turnstile 组件并真实点选，
// 返回 Cloudflare 签发的 token。整套点选逻辑与 Leonardo 注册共用（含反自动化补丁
// 扩展、真光标点击），其它平台（如 higgsfield 的 Clerk 注册）走协议时借它拿 token。
// 不做任何绕过：拿不到 token 就按失败返回。
func MintTurnstile(ctx context.Context, opt MintOptions) (token string, err error) {
	if opt.PageURL == "" || opt.Sitekey == "" {
		return "", fmt.Errorf("缺少铸造 Turnstile token 所需的页面地址或 sitekey")
	}
	in := Input{Proxy: opt.Proxy, Headless: opt.Headless, Log: opt.Log}

	browser, authBridge, cleanup, err := launchLeonardoBrowser(in)
	if err != nil {
		return "", err
	}
	if authBridge != nil {
		defer authBridge.Close()
	}
	defer func() {
		_ = rod.Try(browser.MustClose)
		cleanup()
	}()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("铸造 Turnstile token 异常: %v", r)
		}
	}()

	page := browser.MustPage("")
	if !in.Headless {
		if slot := acquireWindowSlot(); slot >= 0 {
			defer releaseWindowSlot(slot)
			placeBrowserWindow(browser, page, slot, in)
		}
	} else {
		_ = (proto.EmulationSetDeviceMetricsOverride{
			Width: 1280, Height: 900, DeviceScaleFactor: 1,
		}).Call(page)
		if ver, verr := (proto.BrowserGetVersion{}).Call(browser); verr == nil {
			if ua := cleanUserAgent(ver.UserAgent); ua != "" {
				_ = (proto.EmulationSetUserAgentOverride{
					UserAgent:      ua,
					AcceptLanguage: "en-US,en;q=0.9",
					Platform:       platformForUA(ua),
				}).Call(page)
			}
		}
	}

	if err = rod.Try(func() { page.Timeout(90 * time.Second).MustNavigate(opt.PageURL) }); err != nil {
		return "", fmt.Errorf("打开 %s 失败: %w", opt.PageURL, err)
	}
	_ = rod.Try(func() { page.Timeout(30 * time.Second).MustWaitLoad() })
	in.logf("已打开 %s，准备渲染 Turnstile 组件", opt.PageURL)

	if err = renderTurnstile(page, opt); err != nil {
		return "", err
	}
	if err = waitTurnstile(ctx, page, in, 120*time.Second); err != nil {
		return "", err
	}
	token = readTurnstileToken(page.Timeout(10 * time.Second))
	if len(token) < 20 {
		return "", fmt.Errorf("Turnstile 已通过但未读到 token")
	}
	return token, nil
}

// renderTurnstile 在页面里显式渲染一个 Turnstile 组件：站点自己的挑战组件要等表单
// 提交才出现，显式渲染能在同域下直接拿到 token。turnstile 脚本已被站点加载时直接复用。
func renderTurnstile(page *rod.Page, opt MintOptions) error {
	loaded, err := page.Timeout(60 * time.Second).Eval(`() => new Promise(resolve => {
		if (window.turnstile) return resolve(true);
		const s = document.createElement('script');
		s.src = 'https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit';
		s.async = true;
		s.onload = () => resolve(true);
		s.onerror = () => resolve(false);
		document.head.appendChild(s);
		setTimeout(() => resolve(!!window.turnstile), 30000);
	})`)
	if err != nil || !loaded.Value.Bool() {
		return fmt.Errorf("页面未能加载 Turnstile 脚本")
	}
	ok, err := page.Timeout(30*time.Second).Eval(`(sitekey, action) => {
		const box = document.createElement('div');
		box.id = 'hf-turnstile-mint';
		box.style.cssText = 'position:fixed;left:32px;top:120px;z-index:2147483647;background:#fff;padding:8px';
		document.body.appendChild(box);
		const opts = { sitekey, callback: t => { window.__mintedTurnstileToken = t; } };
		if (action) opts.action = action;
		window.turnstile.render('#hf-turnstile-mint', opts);
		return true;
	}`, opt.Sitekey, opt.Action)
	if err != nil || !ok.Value.Bool() {
		return fmt.Errorf("渲染 Turnstile 组件失败: %v", err)
	}
	return nil
}

// readTurnstileToken 读出已签发的 token。
func readTurnstileToken(page *rod.Page) string {
	v, err := page.Eval(`() => {
		let token = String(window.__mintedTurnstileToken || '').trim();
		if (token.length < 20) {
			const input = document.querySelector('input[name="cf-turnstile-response"], textarea[name="cf-turnstile-response"]');
			token = String((input && input.value) || '').trim();
		}
		if (token.length < 20) {
			try {
				if (window.turnstile && typeof window.turnstile.getResponse === 'function') {
					token = String(window.turnstile.getResponse() || '').trim();
				}
			} catch (e) {}
		}
		return token;
	}`)
	if err != nil {
		return ""
	}
	return v.Value.Str()
}
