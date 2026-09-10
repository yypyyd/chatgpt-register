package livecheck

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"chatgpt-register/internal/codexreg"
	"chatgpt-register/internal/proxyutil"
)

// CGItem 一个待测活的 ChatGPT 账号。Cookies 是网页会话本体；Token 仅用于识别旧记录，
// 不再拿它冒充网页会话做 Bearer 探测。
type CGItem struct {
	ID        uint
	Token     string
	Cookies   []codexreg.WebCookie
	Languages string
	// Proxy 该账号注册时用的代理（auth.json 的 proxy 字段），有则原样复用其粘性 session。
	Proxy string
}

// CGOptions ChatGPT 测活参数。
type CGOptions struct {
	// Proxies 代理池。每个账号独立走一个出口（BestGo 动态住宅代理自动换 session）。
	// 为空则直连——此时所有账号都会从服务器同一个 IP 被探测，OpenAI 会把它们关联到一起，慎用。
	Proxies []string
	Log     func(format string, a ...any)
}

// perAccountTimeout 单个账号：协议客户端恢复 Cookie 后验证 /api/auth/session。
const perAccountTimeout = 25 * time.Second

const liveConcurrency = 4

// EstimateChatGPTDuration 估算整批测活耗时上限，供调用方设置 ctx 超时。
func EstimateChatGPTDuration(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	batches := (n + liveConcurrency - 1) / liveConcurrency
	return time.Minute + time.Duration(batches)*perAccountTimeout
}

// CheckChatGPT 用协议客户端逐个验证 ChatGPT 网页会话，不再启动浏览器。
// 旧实现把 /api/auth/session 派生出的 access token 塞进全新无 Cookie 的浏览器，
// 再用 Bearer 请求 /backend-api/me；那条请求不是原网页会话，401 不能证明账号已死。
// 现在只以保存的 Cookie 能否打通 /api/auth/session 为准。
//
//	HTTP 200 且返回 accessToken -> alive（网页会话有效）
//	HTTP 200 但无 accessToken / HTTP 401 -> dead（网页会话已退出）
//	其它（403 / 429 / 5xx / 网络错误 / CF 拦截）-> unknown
func CheckChatGPT(ctx context.Context, items []CGItem, onChunk Chunk, opt CGOptions) map[uint]string {
	out := make(map[uint]string, len(items))
	if len(items) == 0 {
		return out
	}
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, liveConcurrency)
	)
	record := func(id uint, st string) {
		mu.Lock()
		out[id] = st
		mu.Unlock()
		emit(onChunk, map[uint]string{id: st})
	}
	for i, it := range items {
		if ctx.Err() != nil {
			record(it.ID, StatusUnknown)
			continue
		}
		if len(it.Cookies) == 0 {
			record(it.ID, StatusUnknown)
			continue
		}
		wg.Add(1)
		go func(idx int, it CGItem) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			record(it.ID, probeCGOne(ctx, it, pickProxy(opt.Proxies, it, idx), opt))
		}(i, it)
	}
	wg.Wait()
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

// probeCGOne 为单个账号用 Chrome TLS 指纹客户端注入 Cookie，再请求 /api/auth/session。
// 没有 Cookie 的旧记录无法证明网页会话状态，只能 unknown。
func probeCGOne(ctx context.Context, it CGItem, proxy string, opt CGOptions) string {
	if len(it.Cookies) == 0 {
		return StatusUnknown
	}
	pctx, cancel := context.WithTimeout(ctx, perAccountTimeout)
	defer cancel()
	code, body, err := codexreg.ProbeWebSession(pctx, it.Cookies, proxy, it.Languages)
	if err != nil {
		if opt.Log != nil {
			opt.Log("chatgpt livecheck #%d: %v", it.ID, err)
		}
		return StatusUnknown
	}
	return classifyWebSession(code, string(body))
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
