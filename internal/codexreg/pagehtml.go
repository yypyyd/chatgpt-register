package codexreg

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	scriptSrcRe     = regexp.MustCompile(`(?is)<script\b[^>]*?\bsrc\s*=\s*["']([^"']+)["']`)
	scriptSrcBareRe = regexp.MustCompile(`(?is)<script\b[^>]*?\bsrc\s*=\s*([^\s>"']+)`)
	dataBuildRe     = regexp.MustCompile(`(?is)data-build\s*=\s*["']([^"']+)["']`)
	nextBuildRe     = regexp.MustCompile(`(?i)"buildId"\s*:\s*"([^"]+)"`)
)

type pageFingerprint struct {
	Scripts   []string
	DataBuild string
}

func resolveAuthURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://") {
		return raw
	}
	base, _ := url.Parse("https://auth.openai.com/")
	next, err := base.Parse(raw)
	if err != nil {
		return raw
	}
	return next.String()
}

func urlSearchKeys(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	keys := make([]string, 0, len(u.Query()))
	for k := range u.Query() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func resolvePageURL(pageURL, src string) string {
	src = strings.TrimSpace(src)
	if src == "" {
		return ""
	}
	if strings.HasPrefix(src, "https://") || strings.HasPrefix(src, "http://") {
		return src
	}
	base, err := url.Parse(pageURL)
	if err != nil || base.Host == "" {
		return src
	}
	next, err := base.Parse(src)
	if err != nil {
		return src
	}
	return next.String()
}

func addScript(pageURL, src string, seen map[string]bool, out *pageFingerprint) {
	src = resolvePageURL(pageURL, strings.Trim(strings.TrimSpace(src), `"'`))
	if src == "" || seen[src] {
		return
	}
	if !strings.HasPrefix(src, "http://") && !strings.HasPrefix(src, "https://") {
		return
	}
	seen[src] = true
	out.Scripts = append(out.Scripts, src)
}

func parsePageFingerprint(pageURL string, html []byte) pageFingerprint {
	html = maybeGunzip(html)
	out := pageFingerprint{}
	if len(html) == 0 {
		return out
	}
	seen := map[string]bool{}
	for _, m := range scriptSrcRe.FindAllSubmatch(html, -1) {
		if len(m) >= 2 {
			addScript(pageURL, string(m[1]), seen, &out)
		}
	}
	if len(out.Scripts) == 0 {
		for _, m := range scriptSrcBareRe.FindAllSubmatch(html, -1) {
			if len(m) >= 2 {
				addScript(pageURL, string(m[1]), seen, &out)
			}
		}
	}
	// document.scripts 只对应 <script src>。auth.openai.com 当前是 Remix
	// modulepreload，没有 script src；真实页面在 Sentinel 注入 sdk.js 之前
	// 这份列表就是空的。不要把 <link rel="modulepreload"> 塞进 scripts，
	// 那会让官方 SDK 的随机 src 快照和真浏览器对不上。
	if m := dataBuildRe.FindSubmatch(html); len(m) >= 2 {
		out.DataBuild = strings.TrimSpace(string(m[1]))
	}
	if out.DataBuild == "" {
		if m := nextBuildRe.FindSubmatch(html); len(m) >= 2 {
			out.DataBuild = strings.TrimSpace(string(m[1]))
		}
	}
	return out
}

func maybeGunzip(body []byte) []byte {
	if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
		r, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return body
		}
		defer r.Close()
		out, err := io.ReadAll(io.LimitReader(r, 4<<20))
		if err == nil && len(out) > 0 {
			return out
		}
	}
	return body
}

func looksLikeHTMLDocument(body []byte) bool {
	if len(body) < 32 {
		return false
	}
	s := strings.ToLower(string(body))
	return strings.Contains(s, "<html") || strings.Contains(s, "<script") || strings.Contains(s, "<!doctype")
}

func (c *protocolClient) rememberAuthPage(pageURL string, body []byte) {
	pageURL = strings.TrimSpace(pageURL)
	if pageURL == "" {
		return
	}
	body = maybeGunzip(body)
	c.authPageURL = pageURL
	c.pageLoadedAt = time.Now()
	if looksLikeHTMLDocument(body) {
		c.authHTML = append([]byte(nil), body...)
	}
	prefix := strings.ReplaceAll(string(body), "\n", " ")
	c.in.logf("🛡 捕获页面 HTML url=%s bytes=%d html=%t prefix=%s",
		truncateForLog(pageURL, 96), len(body), looksLikeHTMLDocument(body), truncateForLog(prefix, 220))
}

func (c *protocolClient) loadAuthDocument(ctx context.Context, pageURL, referer string) (string, error) {
	pageURL = resolveAuthURL(pageURL)
	if pageURL == "" {
		return "", nil
	}
	landing, resp, err := c.followRedirects(ctx, pageURL, referer)
	if err != nil {
		return landing, err
	}
	if resp != nil {
		c.rememberAuthPage(landing, resp.Body)
	} else {
		c.authPageURL = landing
		c.pageLoadedAt = time.Now()
	}
	return landing, nil
}

func (c *protocolClient) sentinelPageSnapshot(ctx context.Context, href string) map[string]any {
	href = strings.TrimSpace(href)
	html := c.authHTML
	if href != "" && (len(html) == 0 || (c.authPageURL != "" && !authPageURLsEqual(c.authPageURL, href))) {
		if resp, err := c.get(ctx, href, href, true); err == nil && resp != nil && resp.Status == 200 {
			body := maybeGunzip(resp.Body)
			if looksLikeHTMLDocument(body) {
				c.rememberAuthPage(href, body)
				html = c.authHTML
			}
		}
	}
	fp := parsePageFingerprint(href, html)
	if fp.Scripts == nil {
		fp.Scripts = []string{}
	}
	ago := int64(0)
	if !c.pageLoadedAt.IsZero() {
		ago = time.Since(c.pageLoadedAt).Milliseconds()
		if ago < 0 {
			ago = 0
		}
	}
	return map[string]any{
		"href":          href,
		"scripts":       fp.Scripts,
		"dataBuild":     fp.DataBuild,
		"loadedAgoMs":   ago,
		"searchKeys":    urlSearchKeys(href),
		"cookies":       c.documentCookieHeader(href),
		"iframeCookies": c.documentCookieHeader(sentinelHost + "/"),
		"taskbar":       c.screen.Taskbar,
		"toolbar":       c.screen.Toolbar,
	}
}

func (c *protocolClient) documentCookieHeader(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || c == nil || c.cli == nil {
		return ""
	}
	parts := make([]string, 0, 8)
	for _, ck := range c.cli.GetCookies(u) {
		if ck == nil || ck.HttpOnly || strings.TrimSpace(ck.Name) == "" || ck.Value == "" {
			continue
		}
		parts = append(parts, ck.Name+"="+ck.Value)
	}
	return strings.Join(parts, "; ")
}

// DumpAuthPages fetches the signup HTML over the protocol TLS client and prints
// script/build fingerprint fields. It never logs cookies or tokens.
func DumpAuthPages(ctx context.Context, proxy string) error {
	c, err := newProtocolClient(Input{Proxy: proxy}, "en-US,en")
	if err != nil {
		return err
	}
	defer c.closeSentinel()
	pages := []string{
		"https://chatgpt.com/auth/login",
		"https://auth.openai.com/email-verification",
		"https://auth.openai.com/about-you",
	}
	for _, u := range pages {
		r, getErr := c.get(ctx, u, "", true)
		status := 0
		raw := []byte(nil)
		ct, ce := "", ""
		if r != nil {
			status = r.Status
			raw = r.Body
			if r.Header != nil {
				ct = r.Header.Get("content-type")
				ce = r.Header.Get("content-encoding")
			}
		}
		body := maybeGunzip(raw)
		fp := parsePageFingerprint(u, body)
		prefix := strings.ReplaceAll(string(body), "\n", " ")
		gzipped := len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b
		srcTags := len(scriptSrcRe.FindAll(body, -1))
		preloads := bytes.Count(body, []byte("modulepreload"))
		fmt.Printf("url=%s err=%v status=%d bytes=%d ct=%s ce=%s html=%t gzip=%t scripts=%d build=%s script_src_tags=%d modulepreload=%d\n",
			u, getErr, status, len(body), ct, ce, looksLikeHTMLDocument(body), gzipped, len(fp.Scripts), fp.DataBuild, srcTags, preloads)
		for i, s := range fp.Scripts {
			if i >= 12 {
				fmt.Printf(" src ...+%d\n", len(fp.Scripts)-12)
				break
			}
			fmt.Printf(" src %s\n", s)
		}
		fmt.Printf(" prefix %s\n", truncateForLog(prefix, 280))
	}
	return nil
}
