package oreatereg

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"chatgpt-register/internal/proxyutil"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	launcherflags "github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
)

// mintJS 复刻站点前端的反爬上报：ParisFactory + Banti 上报后取 r.htj.jt。
// 站点的 axios 拦截器就是用这个值替换请求体里的 jt 占位符。
const mintJS = `() => new Promise(resolve => {
  const F = window.ParisFactory;
  if (!F) { resolve({err: 'no ParisFactory'}); return; }
  const inst = F.create({
    sid: '2146', sak: '21a851acb0', timeout: 5000,
    bantiUrl: 'https://cdn.oreateai.com/static/v1/js/banti_21a851acb0_2025.js',
    bantiOptions: { reportTimeout: 200, bantiOrigin: 'https://banti.oreateai.com', ymgOrigin: 'https://banti.oreateai.com' }
  });
  let done = false;
  setTimeout(() => { if (!done) resolve({err: 'timeout'}); }, 8000);
  try {
    inst.sendBantiReport({ subid: '' }, (n, r) => { done = true; resolve({ jt: r && r.htj && r.htj.jt }); });
  } catch (e) { resolve({ err: String(e) }); }
})`

// tokens 是一次浏览器会话铸造出的反爬凭据。jt 一次性，且与铸造时的 OUID/UA
// 绑定，跨设备使用会被判成参数非法或 spam user。
type tokens struct {
	OUID string
	UA   string
	JTs  []string
}

// mintTokens 打开站点首页铸造 n 个 jt，并带回同一会话的 OUID 与 UA。
func mintTokens(ctx context.Context, in Input, n int) (result *tokens, err error) {
	if n < 1 {
		n = 1
	}
	// Banti 会把无头模式和自动化痕迹写进 jt，服务端校验时直接返回
	// Invalid parameter，所以这里必须有头运行并抹掉自动化特征。
	l := launcher.New()
	for _, flag := range []string{
		"enable-automation",
		"disable-background-networking",
		"disable-background-timer-throttling",
		"disable-backgrounding-occluded-windows",
		"disable-client-side-phishing-detection",
		"disable-hang-monitor",
		"disable-ipc-flooding-protection",
		"disable-renderer-backgrounding",
		"disable-site-isolation-trials",
		"disable-sync",
		"metrics-recording-only",
		"use-mock-keychain",
	} {
		l = l.Delete(launcherflags.Flag(flag))
	}
	l = l.NoSandbox(true).Headless(false).
		Set("no-first-run").
		Set("no-default-browser-check").
		Set("disable-popup-blocking").
		Set("disable-infobars").
		Set("disable-blink-features", "AutomationControlled")
	// remote-debugging-port=0 也会让 navigator.webdriver 变成 true，改用随机端口。
	port, perr := availableLoopbackPort()
	if perr != nil {
		return nil, fmt.Errorf("分配浏览器调试端口失败: %w", perr)
	}
	l = l.Set("remote-debugging-port", strconv.Itoa(port))
	if in.Headless {
		in.logf("Oreate 反爬校验会识别无头浏览器，本次强制使用有头模式铸造 token")
	}
	var proxyUser, proxyPass string
	if raw := strings.TrimSpace(in.Proxy); raw != "" {
		u, perr := url.Parse(proxyutil.Normalize(raw))
		if perr != nil || u.Host == "" {
			return nil, fmt.Errorf("解析代理失败: %s", raw)
		}
		scheme := u.Scheme
		if scheme == "" {
			scheme = "http"
		}
		l = l.Set("proxy-server", scheme+"://"+u.Host)
		if u.User != nil {
			proxyUser = u.User.Username()
			proxyPass, _ = u.User.Password()
		}
	}
	controlURL, err := l.Launch()
	if err != nil {
		return nil, fmt.Errorf("启动浏览器失败: %w", err)
	}
	browser := rod.New().NoDefaultDevice().ControlURL(controlURL)
	if err = browser.Connect(); err != nil {
		l.Cleanup()
		return nil, fmt.Errorf("连接浏览器失败: %w", err)
	}
	defer func() {
		_ = rod.Try(browser.MustClose)
		l.Cleanup()
	}()
	if proxyUser != "" || proxyPass != "" {
		wait := browser.HandleAuth(proxyUser, proxyPass)
		go func() { _ = wait() }()
	}

	page, err := browser.Page(proto.TargetCreateTarget{})
	if err != nil {
		return nil, fmt.Errorf("创建页面失败: %w", err)
	}
	page = page.Context(ctx)
	defer func() { _ = page.Close() }()

	if err = page.Timeout(90 * time.Second).Navigate(homeURL); err != nil {
		return nil, fmt.Errorf("打开 Oreate 首页失败: %w", err)
	}
	if err = page.Timeout(90 * time.Second).WaitLoad(); err != nil {
		return nil, fmt.Errorf("Oreate 首页加载失败: %w", err)
	}
	// 反爬脚本是首页加载后异步拉起来的，等它注册好 ParisFactory 再铸造。
	if !sleepCtx(ctx, 8*time.Second) {
		return nil, ctx.Err()
	}

	ouid := ""
	cookies, cerr := page.Cookies([]string{baseURL})
	if cerr != nil {
		return nil, fmt.Errorf("读取站点 Cookie 失败: %w", cerr)
	}
	for _, ck := range cookies {
		if ck.Name == "OUID" {
			ouid = ck.Value
		}
	}
	if ouid == "" {
		return nil, fmt.Errorf("未拿到站点设备标识 OUID")
	}

	ua := userAgent
	if ver, verr := (proto.BrowserGetVersion{}).Call(browser); verr == nil {
		if cleaned := strings.ReplaceAll(ver.UserAgent, "HeadlessChrome", "Chrome"); cleaned != "" {
			ua = cleaned
		}
	}

	out := &tokens{OUID: ouid, UA: ua}
	for i := 0; i < n; i++ {
		var minted struct {
			JT  string `json:"jt"`
			Err string `json:"err"`
		}
		obj, eerr := page.Timeout(30 * time.Second).Evaluate(rod.Eval(mintJS).ByPromise())
		if eerr != nil {
			return nil, fmt.Errorf("铸造反爬 token 失败: %w", eerr)
		}
		if err = obj.Value.Unmarshal(&minted); err != nil {
			return nil, fmt.Errorf("解析反爬 token 失败: %w", err)
		}
		if minted.JT == "" {
			return nil, fmt.Errorf("铸造反爬 token 失败: %s", minted.Err)
		}
		out.JTs = append(out.JTs, minted.JT)
	}
	in.logf("已铸造 %d 个反爬 token（OUID %s...）", len(out.JTs), trimText(ouid, 8))
	return out, nil
}

// availableLoopbackPort 取一个可用的本地端口给 Chrome 调试接口用。
func availableLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}
