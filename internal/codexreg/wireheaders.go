package codexreg

import (
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

// 线上头（HTTP/2 HEADERS）的集合、取值与顺序。
//
// Cloudflare 这类边缘的 Bot 特征里，请求头的“顺序”和“集合”是硬指标。Chrome 的顺序并不是字母序，
// 也不是我们随手写的顺序：页面可见的头（含 fetch() 里设置的自定义头）按 Blink 的哈希表顺序输出，
// 网络栈再追加 origin / sec-fetch-* / referer / accept-encoding / accept-language / cookie / priority。
// 下面每一类的顺序都是用真实 Chrome 154 + tls.peet.ws 回显逐类测出来的（scripts/lab/measure_header_orders.py），
// 头名集合来自真实注册流程的抓包（例如 email-otp/validate 请求根本没有 oai-device-id / oai-language）。

const acceptEncodingChrome = "gzip, deflate, br, zstd"

var wireOrders = map[string][]string{
	"nav": {
		"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "upgrade-insecure-requests", "user-agent", "accept",
		"sec-fetch-site", "sec-fetch-mode", "sec-fetch-user", "sec-fetch-dest", "referer",
		"accept-encoding", "accept-language", "cookie", "priority",
	},
	"xhr_get": {
		"sec-ch-ua-platform", "user-agent", "sec-ch-ua", "sec-ch-ua-mobile", "accept",
		"sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest", "referer",
		"accept-encoding", "accept-language", "cookie", "priority",
	},
	"xhr_post_form": {
		"content-length", "sec-ch-ua-platform", "user-agent", "sec-ch-ua", "content-type", "sec-ch-ua-mobile", "accept", "origin",
		"sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest", "referer",
		"accept-encoding", "accept-language", "cookie", "priority",
	},
	"xhr_post_json_sentinel": {
		"content-length", "x-datadog-parent-id", "sec-ch-ua-platform", "openai-sentinel-token", "sec-ch-ua", "sec-ch-ua-mobile",
		"x-access-flow-invocation-id", "x-datadog-trace-id", "traceparent", "accept", "content-type", "tracestate", "x-datadog-origin",
		"x-openai-document-navigation-id", "user-agent", "openai-sentinel-so-token", "x-datadog-sampling-priority", "origin",
		"sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest", "referer",
		"accept-encoding", "accept-language", "cookie", "priority",
	},
	"xhr_post_json": {
		"content-length", "x-datadog-origin", "x-datadog-parent-id", "sec-ch-ua-platform", "sec-ch-ua", "sec-ch-ua-mobile",
		"x-access-flow-invocation-id", "x-datadog-trace-id", "traceparent", "x-openai-document-navigation-id", "user-agent", "accept",
		"content-type", "tracestate", "x-datadog-sampling-priority", "origin",
		"sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest", "referer",
		"accept-encoding", "accept-language", "cookie", "priority",
	},
	// Sentinel iframe 里 fetch(.../sentinel/req)：只设 content-type，其余全是默认头。
	"xhr_post_min": {
		"content-length", "sec-ch-ua-platform", "user-agent", "sec-ch-ua", "content-type", "sec-ch-ua-mobile", "accept", "origin",
		"sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest", "referer",
		"accept-encoding", "accept-language", "cookie", "priority",
	},
}

// legacyWireOrder 用于探针/刷新等“非注册”请求：保留原有行为，只是去掉我们不该发的高熵提示头。
var legacyWireOrder = []string{
	"accept", "accept-language", "content-type", "origin", "referer",
	"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform",
	"sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site", "upgrade-insecure-requests", "user-agent",
}

// requestKind 根据已设置的头判断该请求在真实浏览器里属于哪一类。
func requestKind(req *http.Request) string {
	h := req.Header
	if strings.EqualFold(h.Get("sec-fetch-mode"), "navigate") {
		return "nav"
	}
	if req.Method == http.MethodGet || req.Method == http.MethodHead {
		return "xhr_get"
	}
	ct := strings.ToLower(h.Get("content-type"))
	switch {
	case strings.Contains(ct, "application/x-www-form-urlencoded"):
		return "xhr_post_form"
	case h.Get("openai-sentinel-token") != "":
		return "xhr_post_json_sentinel"
	case h.Get("x-datadog-trace-id") != "":
		return "xhr_post_json"
	default:
		return "xhr_post_min"
	}
}

// applyWireOrder 在发送前把请求头规整成真实 Chrome 的线上形态。
// strict=false 时只设置 legacy 顺序，不删任何头（供探针使用）。
func applyWireOrder(req *http.Request, strict bool) {
	h := req.Header
	if !strict {
		h[http.HeaderOrderKey] = legacyWireOrder
		return
	}
	kind := requestKind(req)
	h.Set("accept-encoding", acceptEncodingChrome)
	switch kind {
	case "nav":
		h.Set("sec-fetch-user", "?1")
		h.Set("priority", "u=0, i")
	default:
		h.Set("priority", "u=1, i")
		if h.Get("accept") == "" {
			h.Set("accept", "*/*")
		}
		h.Del("upgrade-insecure-requests")
		h.Del("sec-fetch-user")
	}
	order := wireOrders[kind]
	allowed := make(map[string]bool, len(order))
	for _, n := range order {
		allowed[n] = true
	}
	// 真实请求里没有的头一律不发（例如 auth.openai.com 的账户 API 没有 oai-device-id / oai-language）。
	for name := range h {
		ln := strings.ToLower(name)
		if ln == strings.ToLower(http.HeaderOrderKey) || ln == strings.ToLower(http.PHeaderOrderKey) {
			continue
		}
		if !allowed[ln] {
			h.Del(name)
		}
	}
	h[http.HeaderOrderKey] = order
}
