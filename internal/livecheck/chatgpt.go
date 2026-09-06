package livecheck

import (
	"context"
	"encoding/json"
	"time"

	"chatgpt-register/internal/codexreg"
	"chatgpt-register/internal/proxyutil"

	"github.com/go-rod/rod"
)

// CGItem 一个待测活的 ChatGPT 账号。Cookies 是网页会话本体；Token 仅用于识别旧记录，
// 不再拿它冒充网页会话做 Bearer 探测。
type CGItem struct {
	ID        uint
	Token     string
	Cookies   []codexreg.WebCookie
	UserAgent string
	Screen    *codexreg.ScreenProfile
	Timezone  string
	Locale    string
	Languages string
	// Proxy 该账号注册时用的代理（auth.json 的 proxy 字段），有则原样复用其粘性 session。
	Proxy string
}

// CGOptions ChatGPT 测活参数。
type CGOptions struct {
	// Proxies 代理池。每个账号独立走一个出口（BestGo 动态住宅代理自动换 session）。
	// 为空则直连——此时所有账号都会从服务器同一个 IP 被探测，OpenAI 会把它们关联到一起，慎用。
	Proxies []string
	// BrowserBin 与注册一致的浏览器选择（设置 chatgpt_browser_bin）。
	BrowserBin string
	// UsePool 批量时共享一个 Chrome 进程、每个账号独立 BrowserContext（cookie/出口/窗口互相隔离），
	// 省掉每号起一个进程。单个账号测活时不值得建池。
	UsePool bool
	Log     func(format string, a ...any)
}

// perAccountTimeout 单个账号：起浏览器 + 恢复 Cookie + 验证 /api/auth/session。
const perAccountTimeout = 100 * time.Second

// EstimateChatGPTDuration 估算整批测活耗时上限，供调用方设置 ctx 超时。
func EstimateChatGPTDuration(n int) time.Duration {
	return 2*time.Minute + time.Duration(n)*perAccountTimeout
}

// CheckChatGPT 逐个恢复并验证 ChatGPT 网页会话。旧实现把 /api/auth/session 派生出的
// access token 塞进全新无 Cookie 的浏览器，再用 Bearer 请求 /backend-api/me；那条请求不是
// 原网页会话，401 不能证明账号已死。现在只以保存的 Cookie 能否恢复 /api/auth/session 为准。
//
//	HTTP 200 且返回 accessToken -> alive（网页会话有效）
//	HTTP 200 但无 accessToken / HTTP 401 -> dead（网页会话已退出）
//	其它（403 / 429 / 5xx / 网络错误 / 起浏览器失败 / CF 拦截）-> unknown
func CheckChatGPT(ctx context.Context, items []CGItem, onChunk Chunk, opt CGOptions) map[uint]string {
	out := make(map[uint]string, len(items))
	var pool *codexreg.Pool
	if opt.UsePool && len(items) > 1 {
		pool = codexreg.NewPool(codexreg.PoolOptions{Headless: true, BrowserBin: opt.BrowserBin, ContextsPerHost: 1, Log: opt.Log})
		defer pool.Close()
	}
	for i, it := range items {
		if ctx.Err() != nil {
			rest := make(map[uint]string)
			for _, r := range items[i:] {
				rest[r.ID] = StatusUnknown
			}
			emit(onChunk, rest)
			for k, v := range rest {
				out[k] = v
			}
			break
		}
		st := probeCGOne(ctx, it, pickProxy(opt.Proxies, it, i), opt, pool)
		out[it.ID] = st
		emit(onChunk, map[uint]string{it.ID: st})
		if i+1 < len(items) {
			// 账号之间错开一点，不要像脚本一样一个接一个毫秒级连发。
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(1500+int(time.Now().UnixNano()%2500)) * time.Millisecond):
			}
		}
	}
	return out
}

// pickProxy 优先原样沿用账号注册时的粘性代理 session；只有旧记录没有代理时，
// 才从代理池生成新的独立 session。
func pickProxy(pool []string, it CGItem, idx int) string {
	if it.Proxy != "" {
		return proxyutil.Normalize(it.Proxy)
	}
	if len(pool) == 0 {
		return ""
	}
	return proxyutil.WithBestGoTaskSession(proxyutil.Normalize(pool[idx%len(pool)]))
}

// probeCGOne 为单个账号拿一个独立浏览器上下文，恢复注册结束时保存的 Cookie，
// 再由网页自身请求 /api/auth/session。没有 Cookie 的旧记录无法证明网页会话状态，只能 unknown。
func probeCGOne(ctx context.Context, it CGItem, proxy string, opt CGOptions, pool *codexreg.Pool) string {
	if len(it.Cookies) == 0 {
		return StatusUnknown
	}
	pctx, cancel := context.WithTimeout(ctx, perAccountTimeout)
	defer cancel()
	var sess *codexreg.Session
	var err error
	if pool != nil {
		sess, err = pool.Acquire(pctx, codexreg.ContextOptions{
			Proxy: proxy, UserAgent: it.UserAgent, Screen: it.Screen, Timezone: it.Timezone,
			Locale: it.Locale, Languages: it.Languages, Log: opt.Log,
		})
	} else {
		sess, err = codexreg.LaunchBrowser(pctx, codexreg.LaunchOptions{
			Headless: true, Proxy: proxy, BrowserBin: opt.BrowserBin,
			UserAgent: it.UserAgent, Screen: it.Screen, Timezone: it.Timezone,
			Locale: it.Locale, Languages: it.Languages, Log: opt.Log,
		})
	}
	if err != nil {
		return StatusUnknown
	}
	defer sess.Close()
	params := codexreg.WebCookieParams(it.Cookies)
	if len(params) == 0 || sess.Browser.SetCookies(params) != nil {
		return StatusUnknown
	}
	page, err := sess.NewPage()
	if err != nil {
		return StatusUnknown
	}

	const js = `async () => {
			try {
				const r = await fetch('/api/auth/session', {
					method: 'GET', credentials: 'include', headers: { 'Accept': 'application/json' },
				});
				return { code: r.status, body: (await r.text()).slice(0, 8192) };
			} catch (e) {
				return { code: -1, body: '' };
			}
		}`
	status := StatusUnknown
	_ = rod.Try(func() {
		page.Timeout(60 * time.Second).MustNavigate("https://chatgpt.com/")
		page.Timeout(60 * time.Second).MustWaitLoad()
		time.Sleep(time.Duration(1500+int(time.Now().UnixNano()%1500)) * time.Millisecond)
		result := page.Timeout(45 * time.Second).MustEval(js)
		status = classifyWebSession(result.Get("code").Int(), result.Get("body").Str())
	})
	return status
}

func classifyWebSession(code int, body string) string {
	if code == 401 {
		return StatusDead
	}
	if code != 200 {
		return StatusUnknown
	}
	var payload struct {
		AccessToken string `json:"accessToken"`
	}
	if json.Unmarshal([]byte(body), &payload) != nil {
		return StatusUnknown
	}
	if payload.AccessToken == "" {
		return StatusDead
	}
	return StatusAlive
}
