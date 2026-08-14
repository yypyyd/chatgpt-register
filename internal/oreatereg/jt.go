package oreatereg

import (
	"context"
	"encoding/json"
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

// genImageJS 在已登录的生图会话页里现铸 jt 并直接发生图请求：实测同一个账号、
// 同一个出口 IP，用 HTTP 客户端发一律被判 spam user，只有请求从页面里发出才放行。
const genImageJS = `(args) => new Promise(resolve => {
  const F = window.ParisFactory;
  if (!F) { resolve({err: 'no ParisFactory'}); return; }
  const inst = F.create({
    sid: '2146', sak: '21a851acb0', timeout: 5000,
    bantiUrl: 'https://cdn.oreateai.com/static/v1/js/banti_21a851acb0_2025.js',
    bantiOptions: { reportTimeout: 200, bantiOrigin: 'https://banti.oreateai.com', ymgOrigin: 'https://banti.oreateai.com' }
  });
  let settled = false;
  const done = (v) => { if (!settled) { settled = true; resolve(v); } };
  setTimeout(() => done({err: 'timeout'}), args.timeoutMs + 10000);
  inst.sendBantiReport({ subid: '' }, async (n, r) => {
    const jt = (r && r.htj && r.htj.jt) || '';
    if (!jt) { done({err: 'no jt'}); return; }
    const body = Object.assign({}, args.body, { jt: jt, ua: navigator.userAgent });
    let acc = '';
    try {
      const res = await fetch(args.path, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'accept': 'text/event-stream', 'locale': 'en-US', 'Client-Type': 'pc' },
        body: JSON.stringify(body),
        credentials: 'include',
      });
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      const deadline = Date.now() + args.timeoutMs;
      for (;;) {
        if (Date.now() > deadline) break;
        const chunk = await reader.read();
        if (chunk.value) acc += dec.decode(chunk.value, { stream: true });
        if (chunk.done) break;
        if (acc.includes('"event":"error"') || acc.includes('"event":"end"')) break;
      }
      done({ status: res.status, text: acc });
    } catch (e) { done({ err: String(e), text: acc }); }
  });
})`

// session 是一次浏览器会话。反爬 token jt 是一次性的，且与铸造它的 OUID/UA 以及
// 页面上下文绑定：注册用的 jt 可以拿出来走 HTTP，但生图请求必须留在这个页面里发，
// 所以整个流程期间浏览器都不关。
type session struct {
	page    *rod.Page
	cleanup func()

	OUID string
	UA   string
	BID  string
}

// openSession 启动有头浏览器、打开站点首页，拿到本次会话的 OUID/UA。
func openSession(ctx context.Context, in Input) (result *session, err error) {
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
		in.logf("Oreate 反爬校验会识别无头浏览器，本次强制使用有头模式")
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
	cleanup := func() {
		_ = rod.Try(browser.MustClose)
		l.Cleanup()
	}
	defer func() {
		if err != nil {
			cleanup()
		}
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

	s := &session{page: page, cleanup: cleanup}
	if err = s.open(ctx, homeURL); err != nil {
		return nil, fmt.Errorf("打开 Oreate 首页失败: %w", err)
	}

	cookies, cerr := page.Cookies([]string{baseURL})
	if cerr != nil {
		return nil, fmt.Errorf("读取站点 Cookie 失败: %w", cerr)
	}
	for _, ck := range cookies {
		switch ck.Name {
		case "OUID":
			s.OUID = ck.Value
		case "__bid_n":
			s.BID = ck.Value
		}
	}
	if s.OUID == "" {
		return nil, fmt.Errorf("未拿到站点设备标识 OUID")
	}
	s.UA = userAgent
	if ver, verr := (proto.BrowserGetVersion{}).Call(browser); verr == nil {
		if cleaned := strings.ReplaceAll(ver.UserAgent, "HeadlessChrome", "Chrome"); cleaned != "" {
			s.UA = cleaned
		}
	}
	return s, nil
}

// open 打开一个站点页面并等反爬脚本注册好 ParisFactory（它是页面加载后异步拉起的）。
func (s *session) open(ctx context.Context, pageURL string) error {
	if err := s.page.Timeout(90 * time.Second).Navigate(pageURL); err != nil {
		return err
	}
	if err := s.page.Timeout(90 * time.Second).WaitLoad(); err != nil {
		return err
	}
	if !sleepCtx(ctx, 8*time.Second) {
		return ctx.Err()
	}
	return nil
}

func (s *session) close() {
	if s != nil && s.cleanup != nil {
		s.cleanup()
	}
}

// mint 铸造一个一次性反爬 token。
func (s *session) mint(ctx context.Context, in Input) (string, error) {
	var minted struct {
		JT  string `json:"jt"`
		Err string `json:"err"`
	}
	obj, err := s.page.Timeout(30 * time.Second).Evaluate(rod.Eval(mintJS).ByPromise())
	if err != nil {
		return "", fmt.Errorf("铸造反爬 token 失败: %w", err)
	}
	if err = obj.Value.Unmarshal(&minted); err != nil {
		return "", fmt.Errorf("解析反爬 token 失败: %w", err)
	}
	if minted.JT == "" {
		return "", fmt.Errorf("铸造反爬 token 失败: %s", minted.Err)
	}
	in.logf("已铸造反爬 token（OUID %s...）", trimText(s.OUID, 8))
	return minted.JT, nil
}

// useSession 把 HTTP 登录拿到的会话票据注入浏览器，让页面变成已登录状态。
func (s *session) useSession(ouss string) error {
	return s.page.SetCookies([]*proto.NetworkCookieParam{{
		Name: "ouss", Value: ouss, Domain: ".oreateai.com", Path: "/",
	}})
}

// generateImage 打开生图会话页，在页面里现铸 jt 并发生图请求，从 SSE 流里取出图地址。
func (s *session) generateImage(ctx context.Context, chatID, prompt, email string) (string, error) {
	if err := s.open(ctx, baseURL+"/home/chat/aiImage/"+chatID); err != nil {
		return "", fmt.Errorf("打开生图会话页失败: %w", err)
	}
	body := map[string]any{
		"js_env": "h5",
		"extra": map[string]any{
			"email":       email,
			"vip":         "0",
			"reg_ts":      time.Now().Unix(),
			"deviceID":    s.OUID,
			"bid":         s.BID,
			"doc_name":    "",
			"module_name": "gpt4o",
		},
		"clientType": "pc",
		"type":       "chat",
		"chatType":   "aiImage",
		"chatTitle":  "Unnamed Session",
		"focusId":    chatID,
		"chatId":     chatID,
		"from":       "home",
		"messages": []map[string]any{{
			"role":        "user",
			"content":     prompt,
			"attachments": []any{},
		}},
		"imageConfig": map[string]any{
			"modelName":  imageModel,
			"ratio":      imageRatio,
			"resolution": imageResolution,
		},
		"isFirst": true,
	}
	obj, err := s.page.Timeout(imageWaitTimeout + time.Minute).Evaluate(rod.Eval(genImageJS, map[string]any{
		"path":      pathStream,
		"body":      body,
		"timeoutMs": imageWaitTimeout.Milliseconds(),
	}).ByPromise())
	if err != nil {
		return "", fmt.Errorf("页面内生图失败: %w", err)
	}
	var out struct {
		Status int    `json:"status"`
		Text   string `json:"text"`
		Err    string `json:"err"`
	}
	raw, merr := obj.Value.MarshalJSON()
	if merr != nil {
		return "", merr
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	imageURL, perr := parseImageStream(out.Text)
	if perr != nil {
		return imageURL, perr
	}
	if imageURL == "" {
		return "", fmt.Errorf("生图流已结束但没有拿到出图地址(HTTP %d %s): %s",
			out.Status, out.Err, trimText(out.Text, 500))
	}
	return imageURL, nil
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
