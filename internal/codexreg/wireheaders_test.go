package codexreg

import (
	"reflect"
	"strings"
	"testing"

	http "github.com/bogdanfinn/fhttp"
)

func newTestReq(t *testing.T, method, url string, hdr map[string]string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return req
}

func TestRequestKind(t *testing.T) {
	cases := []struct {
		name   string
		method string
		hdr    map[string]string
		want   string
	}{
		{"navigation", "GET", map[string]string{"Sec-Fetch-Mode": "navigate"}, "nav"},
		{"navigation case-insensitive", "GET", map[string]string{"Sec-Fetch-Mode": "Navigate"}, "nav"},
		{"xhr get", "GET", map[string]string{"Sec-Fetch-Mode": "cors"}, "xhr_get"},
		{"form post", "POST", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, "xhr_post_form"},
		{"json with sentinel", "POST", map[string]string{
			"Content-Type": "application/json", "Openai-Sentinel-Token": "x", "X-Datadog-Trace-Id": "1"}, "xhr_post_json_sentinel"},
		{"json with datadog only", "POST", map[string]string{
			"Content-Type": "application/json", "X-Datadog-Trace-Id": "1"}, "xhr_post_json"},
		{"bare json (sentinel iframe fetch)", "POST", map[string]string{"Content-Type": "text/plain;charset=UTF-8"}, "xhr_post_min"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := newTestReq(t, tc.method, "https://auth.openai.com/x", tc.hdr)
			if got := requestKind(req); got != tc.want {
				t.Fatalf("requestKind = %q, want %q", got, tc.want)
			}
		})
	}
}

// 真实 Chrome 的固定尾部：accept-encoding → accept-language → cookie → priority。
// 表被误改时这里会先报警。
func TestWireOrdersInvariants(t *testing.T) {
	for kind, order := range wireOrders {
		seen := map[string]bool{}
		for _, n := range order {
			if n != strings.ToLower(n) {
				t.Errorf("%s: header %q must be lower-case", kind, n)
			}
			if seen[n] {
				t.Errorf("%s: duplicate header %q", kind, n)
			}
			seen[n] = true
		}
		tail := order[len(order)-4:]
		want := []string{"accept-encoding", "accept-language", "cookie", "priority"}
		if !reflect.DeepEqual(tail, want) {
			t.Errorf("%s: tail = %v, want %v", kind, tail, want)
		}
		for _, must := range []string{"user-agent", "sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest", "referer"} {
			if !seen[must] {
				t.Errorf("%s: missing %q", kind, must)
			}
		}
		// 高熵 Client Hints、设备标识头在真实注册流量里不存在。
		for _, banned := range []string{"sec-ch-ua-full-version-list", "sec-ch-ua-arch", "sec-ch-ua-model", "oai-device-id", "oai-language"} {
			if seen[banned] {
				t.Errorf("%s: must not contain %q", kind, banned)
			}
		}
	}
	for _, kind := range []string{"nav", "xhr_get", "xhr_post_form", "xhr_post_json_sentinel", "xhr_post_json", "xhr_post_min"} {
		if _, ok := wireOrders[kind]; !ok {
			t.Errorf("missing order for %s", kind)
		}
	}
	// 只有 POST 才有 content-length / origin；导航与 GET 不应出现。
	for _, kind := range []string{"nav", "xhr_get"} {
		for _, n := range wireOrders[kind] {
			if n == "content-length" || n == "origin" || n == "content-type" {
				t.Errorf("%s: unexpected %q", kind, n)
			}
		}
	}
}

func TestApplyWireOrderNavigation(t *testing.T) {
	req := newTestReq(t, "GET", "https://auth.openai.com/email-verification", map[string]string{
		"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "same-origin", "Sec-Fetch-Dest": "document",
		"Sec-Ch-Ua": `"Chromium";v="146"`, "Sec-Ch-Ua-Mobile": "?0", "Sec-Ch-Ua-Platform": `"Windows"`,
		"User-Agent": "UA", "Accept": "text/html", "Referer": "https://auth.openai.com/",
		"Accept-Language": "en-US,en;q=0.9",
		"Oai-Device-Id":   "must-be-pruned", // 真实导航请求不带
		"X-Custom":        "must-be-pruned",
	})
	applyWireOrder(req, true)

	if got := req.Header.Get("Priority"); got != "u=0, i" {
		t.Errorf("priority = %q", got)
	}
	if got := req.Header.Get("Sec-Fetch-User"); got != "?1" {
		t.Errorf("sec-fetch-user = %q", got)
	}
	if got := req.Header.Get("Accept-Encoding"); got != acceptEncodingChrome {
		t.Errorf("accept-encoding = %q", got)
	}
	if req.Header.Get("Oai-Device-Id") != "" || req.Header.Get("X-Custom") != "" {
		t.Errorf("non-browser headers must be pruned: %v", req.Header)
	}
	if !reflect.DeepEqual(req.Header[http.HeaderOrderKey], wireOrders["nav"]) {
		t.Errorf("header order = %v", req.Header[http.HeaderOrderKey])
	}
}

func TestApplyWireOrderSentinelPost(t *testing.T) {
	req := newTestReq(t, "POST", "https://auth.openai.com/api/accounts/user/register", map[string]string{
		"Content-Type": "application/json", "Content-Length": "10", "Origin": "https://auth.openai.com",
		"Sec-Fetch-Mode": "cors", "Sec-Fetch-Site": "same-origin", "Sec-Fetch-Dest": "empty",
		"Sec-Ch-Ua": `"Chromium";v="146"`, "Sec-Ch-Ua-Mobile": "?0", "Sec-Ch-Ua-Platform": `"Windows"`,
		"User-Agent": "UA", "Referer": "https://auth.openai.com/create-account/password",
		"Openai-Sentinel-Token": "tok", "Openai-Sentinel-So-Token": "so",
		"X-Datadog-Trace-Id": "1", "X-Datadog-Parent-Id": "2", "Traceparent": "00-x-y-01", "Tracestate": "dd=s:1",
		"X-Datadog-Origin": "rum", "X-Datadog-Sampling-Priority": "1",
		"X-Openai-Document-Navigation-Id": "nav", "X-Access-Flow-Invocation-Id": "flow",
		"Upgrade-Insecure-Requests": "1", "Sec-Fetch-User": "?1", // 非导航请求不应带
		"Oai-Device-Id": "must-be-pruned",
	})
	applyWireOrder(req, true)

	if got := req.Header.Get("Priority"); got != "u=1, i" {
		t.Errorf("priority = %q", got)
	}
	if req.Header.Get("Accept") != "*/*" {
		t.Errorf("accept default = %q", req.Header.Get("Accept"))
	}
	for _, gone := range []string{"Upgrade-Insecure-Requests", "Sec-Fetch-User", "Oai-Device-Id"} {
		if req.Header.Get(gone) != "" {
			t.Errorf("%s must be removed", gone)
		}
	}
	for _, kept := range []string{"Openai-Sentinel-Token", "Openai-Sentinel-So-Token", "X-Datadog-Trace-Id", "Traceparent", "X-Openai-Document-Navigation-Id"} {
		if req.Header.Get(kept) == "" {
			t.Errorf("%s must be kept", kept)
		}
	}
	if !reflect.DeepEqual(req.Header[http.HeaderOrderKey], wireOrders["xhr_post_json_sentinel"]) {
		t.Errorf("header order = %v", req.Header[http.HeaderOrderKey])
	}
}

func TestApplyWireOrderLegacyKeepsHeaders(t *testing.T) {
	req := newTestReq(t, "GET", "https://chatgpt.com/backend-api/me", map[string]string{
		"Authorization": "Bearer x", "Accept": "application/json", "User-Agent": "UA",
	})
	applyWireOrder(req, false)
	if req.Header.Get("Authorization") == "" {
		t.Fatal("legacy mode must not prune headers")
	}
	if !reflect.DeepEqual(req.Header[http.HeaderOrderKey], legacyWireOrder) {
		t.Errorf("header order = %v", req.Header[http.HeaderOrderKey])
	}
}
