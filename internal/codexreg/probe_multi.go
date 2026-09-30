package codexreg

import (
	"context"
	"strings"

	http "github.com/bogdanfinn/fhttp"
	"github.com/google/uuid"
)

// EndpointProbe 是一次 backend-api 探测的脱敏结果（不含 body 原文）。
type EndpointProbe struct {
	Method string
	Path   string
	Status int
	Code   string
}

// DiagEndpoints 是诊断用的端点集合：用来区分「整张 AT 被撤销」和「只有某个端点拒绝」。
var DiagEndpoints = []struct{ Method, Path, Body string }{
	{http.MethodGet, "/backend-api/me", ""},
	{http.MethodGet, "/backend-api/accounts/check/v4-2023-04-27?timezone_offset_min=240", ""},
	{http.MethodPost, "/backend-api/conversation/init",
		`{"gizmo_id":null,"requested_default_model":null,"conversation_id":null,"timezone_offset_min":240}`},
}

// ProbeAccessTokenEndpoints 用同一个 tls-client 依次打多个端点。
// cookies 非空时会把网页会话 Cookie 灌进 jar（像真浏览器一样带 Cookie 请求）。
func ProbeAccessTokenEndpoints(ctx context.Context, in ProbeIdentity, cookies []WebCookie) ([]EndpointProbe, error) {
	languages := strings.TrimSpace(in.Languages)
	if languages == "" {
		languages = "en-US,en"
	}
	c, err := newProtocolClient(Input{Proxy: in.Proxy}, languages)
	if err != nil {
		return nil, err
	}
	if ua := strings.TrimSpace(in.UserAgent); ua != "" {
		c.ua = ua
		if major := chromeMajorFromUA(ua); major != "" {
			c.chromeMajor = major
			c.chUA = ""
		}
	}
	c.deviceID = strings.TrimSpace(in.DeviceID)
	if len(cookies) > 0 {
		c.loadCookies(cookies)
	}
	out := make([]EndpointProbe, 0, len(DiagEndpoints))
	for _, ep := range DiagEndpoints {
		var body *strings.Reader
		if ep.Body != "" {
			body = strings.NewReader(ep.Body)
		} else {
			body = strings.NewReader("")
		}
		req, err := http.NewRequest(ep.Method, "https://chatgpt.com"+ep.Path, body)
		if err != nil {
			out = append(out, EndpointProbe{Method: ep.Method, Path: ep.Path, Code: "request_build_failed"})
			continue
		}
		c.headers(req, "https://chatgpt.com/", false)
		req.Header.Set("authorization", "Bearer "+in.Token)
		req.Header.Set("origin", "https://chatgpt.com")
		if ep.Method == http.MethodPost {
			req.Header.Set("content-type", "application/json")
			path := strings.SplitN(ep.Path, "?", 2)[0]
			req.Header.Set("x-openai-target-path", path)
			req.Header.Set("x-openai-target-route", path)
		}
		if c.deviceID != "" {
			req.Header.Set("oai-device-id", c.deviceID)
		}
		req.Header.Set("oai-session-id", uuid.NewString())
		req.Header.Set("oai-language", primaryLanguageTag(languages))
		r, derr := c.do(ctx, req)
		p := EndpointProbe{Method: ep.Method, Path: strings.SplitN(ep.Path, "?", 2)[0]}
		if derr != nil && r == nil {
			p.Code = "network_error"
			out = append(out, p)
			continue
		}
		p.Status = r.Status
		if r.Status >= 200 && r.Status < 300 {
			p.Code = "ok"
		} else {
			p.Code = upstreamErrorCode(r.Body, r.Status)
		}
		out = append(out, p)
	}
	return out, nil
}
