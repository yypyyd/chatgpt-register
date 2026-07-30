package livecheck

import (
	"context"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/stealth"
	"github.com/ysmood/gson"
)

// CGItem 一个待测活的 ChatGPT 账号：ID + 从 auth.json 取出的 access_token。
type CGItem struct {
	ID    uint
	Token string
}

// CheckChatGPT 批量测活 ChatGPT 账号。ChatGPT 直连 chatgpt.com 会被 Cloudflare
// 拦截，因此复用项目现成的 go-rod 浏览器：先在浏览器里打开 chatgpt.com 过盾，
// 再在页面上下文里用各账号的 Bearer 调 /backend-api/me。
//
//	HTTP 200 -> alive（token 有效）
//	HTTP 401 -> dead （token 失效）
//	其它（403 / 429 / 5xx / 网络错误 / 起浏览器失败 / CF 拦截）-> unknown
func CheckChatGPT(ctx context.Context, items []CGItem, onChunk Chunk) map[uint]string {
	out := make(map[uint]string, len(items))
	if len(items) == 0 {
		return out
	}

	fail := func() map[uint]string {
		res := make(map[uint]string, len(items))
		for _, it := range items {
			res[it.ID] = StatusUnknown
		}
		emit(onChunk, res)
		for k, v := range res {
			out[k] = v
		}
		return out
	}

	browser, closeBrowser, err := launchBrowser(true)
	if err != nil {
		return fail()
	}
	defer closeBrowser()

	var page *rod.Page
	if err := rod.Try(func() { page = stealth.MustPage(browser) }); err != nil || page == nil {
		return fail()
	}
	defer func() { _ = rod.Try(func() { page.MustClose() }) }()
	page = page.Context(ctx)

	// 先加载 chatgpt.com 过 Cloudflare，失败则整批 unknown（不判死）。
	if err := rod.Try(func() {
		page.Timeout(60 * time.Second).MustNavigate("https://chatgpt.com/")
		page.Timeout(60 * time.Second).MustWaitLoad()
	}); err != nil {
		return fail()
	}
	time.Sleep(2 * time.Second)

	const batchSize = 6
	for i := 0; i < len(items); i += batchSize {
		if ctx.Err() != nil {
			res := make(map[uint]string)
			for _, it := range items[i:] {
				res[it.ID] = StatusUnknown
			}
			emit(onChunk, res)
			for k, v := range res {
				out[k] = v
			}
			break
		}
		end := i + batchSize
		if end > len(items) {
			end = len(items)
		}
		res := probeCGBatch(page, items[i:end])
		emit(onChunk, res)
		for k, v := range res {
			out[k] = v
		}
	}
	return out
}

// probeCGBatch 在页面上下文里并发对一小批 token 请求 /backend-api/me，返回 id->status。
func probeCGBatch(page *rod.Page, batch []CGItem) map[uint]string {
	res := make(map[uint]string, len(batch))
	args := make([]map[string]any, 0, len(batch))
	for _, it := range batch {
		args = append(args, map[string]any{"id": it.ID, "token": it.Token})
	}

	const js = `async (list) => {
		const out = [];
		await Promise.all(list.map(async (it) => {
			try {
				const r = await fetch('/backend-api/me', {
					method: 'GET',
					credentials: 'omit',
					headers: { 'Authorization': 'Bearer ' + it.token },
				});
				out.push({ id: it.id, code: r.status });
			} catch (e) {
				out.push({ id: it.id, code: -1 });
			}
		}));
		return out;
	}`

	var rows []gson.JSON
	err := rod.Try(func() {
		rows = page.Timeout(45*time.Second).MustEval(js, args).Arr()
	})
	if err != nil {
		for _, it := range batch {
			res[it.ID] = StatusUnknown
		}
		return res
	}

	seen := make(map[uint]bool, len(batch))
	for _, row := range rows {
		id := uint(row.Get("id").Int())
		code := row.Get("code").Int()
		res[id] = classifyHTTP(int(code))
		seen[id] = true
	}
	// 未拿到结果的一律 unknown。
	for _, it := range batch {
		if !seen[it.ID] {
			res[it.ID] = StatusUnknown
		}
	}
	return res
}

func classifyHTTP(code int) string {
	switch code {
	case 200:
		return StatusAlive
	case 401:
		return StatusDead
	default:
		return StatusUnknown
	}
}
