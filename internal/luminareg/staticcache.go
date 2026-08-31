package luminareg

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// BytePlus Lumina 前端是个十几 MB 的 SPA（JS/CSS/字体），每个号都要重新拉一遍，
// 这部分是代理流量的大头，但它们都是公共 CDN 静态文件：既不带 cookie、也不参与
// 风控，用本机直连拉取 + 落盘复用即可，注册相关的文档/接口/滑块仍走代理出口。
const (
	// staticEntryTTL 缓存有效期：BytePlus 前端发版后旧包会 404，过期就重新直连拉。
	staticEntryTTL = 24 * time.Hour
	// staticMaxSize 单个文件缓存上限，超过的直接放行走代理，避免磁盘被大包塞满。
	// Lumina 有单个 20 MB 左右的 JS 包，上限要盖得住它。
	staticMaxSize = 32 << 20
)

// staticCache 静态资源直连缓存：命中则由本地回放给 Chromium，未命中则本机直连
// 下载并落盘，两种情况都不消耗代理流量。
type staticCache struct {
	dir    string
	client *http.Client

	hitN, hitBytes       atomic.Int64
	directN, directBytes atomic.Int64
	fallbackN            atomic.Int64
}

// newStaticCache 准备静态资源缓存目录；目录不可用时返回 nil，调用方退回全代理。
func newStaticCache() *staticCache {
	dir := filepath.Join(filepath.Dir(launcher.DefaultBrowserDir), "browser-lumina", "static-cache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	return &staticCache{
		dir: dir,
		client: &http.Client{
			Timeout: 30 * time.Second,
			// 明确不设代理：这里就是为了绕开代理出口省流量。
			Transport: &http.Transport{
				Proxy:               nil,
				MaxIdleConnsPerHost: 8,
				ForceAttemptHTTP2:   true,
			},
		},
	}
}

// staticExts 静态资源文件名后缀白名单：只接管带这些后缀的 URL，动态脚本接口
// （可能按会话返回不同内容）继续走代理。
var staticExts = []string{".js", ".mjs", ".css", ".woff", ".woff2", ".ttf", ".otf", ".eot"}

// eligible 只接管公共静态资源：GET 的脚本/样式/字体文件。
// 文档、XHR、滑块图片一律不碰，避免影响登录态与风控判定。
func (sc *staticCache) eligible(h *rod.Hijack) bool {
	if sc == nil || h.Request.Method() != http.MethodGet {
		return false
	}
	switch h.Request.Type() {
	case proto.NetworkResourceTypeScript,
		proto.NetworkResourceTypeStylesheet,
		proto.NetworkResourceTypeFont:
	default:
		return false
	}
	u := h.Request.URL()
	if u.Scheme != "https" && u.Scheme != "http" {
		return false
	}
	path := strings.ToLower(u.Path)
	for _, ext := range staticExts {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

// serve 用缓存或本机直连回放响应；返回 false 表示这条请求仍需走代理。
func (sc *staticCache) serve(h *rod.Hijack, in Input) bool {
	raw := h.Request.URL().String()
	if body, ctype, ok := sc.load(raw); ok {
		sc.fulfill(h, ctype, body)
		sc.hitN.Add(1)
		sc.hitBytes.Add(int64(len(body)))
		return true
	}
	body, ctype, err := sc.fetch(raw, h.Request.Type())
	if err != nil {
		sc.fallbackN.Add(1)
		in.logf("静态资源直连失败，回退代理: %s（%v）", trimText(raw, 100), err)
		return false
	}
	sc.store(raw, ctype, body)
	sc.fulfill(h, ctype, body)
	sc.directN.Add(1)
	sc.directBytes.Add(int64(len(body)))
	return true
}

// fetch 本机直连下载静态资源，并校验类型与预期一致（出口被限制时 CDN 会返回
// 提示页，内容类型对不上就当失败，交回代理拉取，避免把坏内容喂给页面）。
func (sc *staticCache) fetch(raw string, kind proto.NetworkResourceType) ([]byte, string, error) {
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", luminaURL)
	req.Header.Set("Accept", "*/*")
	res, err := sc.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("HTTP %d", res.StatusCode)
	}
	ctype := res.Header.Get("Content-Type")
	if !typeMatches(kind, ctype) {
		return nil, "", fmt.Errorf("类型不符: %s", trimText(ctype, 60))
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, staticMaxSize+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) == 0 {
		return nil, "", fmt.Errorf("响应为空")
	}
	if len(body) > staticMaxSize {
		return nil, "", fmt.Errorf("响应过大")
	}
	return body, ctype, nil
}

// typeMatches 校验响应类型与浏览器请求的资源类型是否匹配。
func typeMatches(kind proto.NetworkResourceType, ctype string) bool {
	ctype = strings.ToLower(ctype)
	switch kind {
	case proto.NetworkResourceTypeScript:
		return strings.Contains(ctype, "javascript") || strings.Contains(ctype, "ecmascript") ||
			strings.Contains(ctype, "application/json")
	case proto.NetworkResourceTypeStylesheet:
		return strings.Contains(ctype, "text/css")
	case proto.NetworkResourceTypeFont:
		return strings.Contains(ctype, "font") || strings.Contains(ctype, "application/octet-stream")
	default:
		return false
	}
}

func (sc *staticCache) fulfill(h *rod.Hijack, ctype string, body []byte) {
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	h.Response.Payload().ResponseCode = http.StatusOK
	h.Response.SetHeader(
		"Content-Type", ctype,
		"Cache-Control", "max-age=31536000",
		"Access-Control-Allow-Origin", "*",
	)
	h.Response.SetBody(body)
}

// staticMeta 落盘缓存的元信息，与内容文件同名（后缀 .json）。
type staticMeta struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	SavedAt     int64  `json:"saved_at"`
}

func (sc *staticCache) paths(raw string) (body, meta string) {
	sum := sha1.Sum([]byte(raw))
	base := filepath.Join(sc.dir, hex.EncodeToString(sum[:]))
	return base + ".bin", base + ".json"
}

func (sc *staticCache) load(raw string) ([]byte, string, bool) {
	bodyPath, metaPath := sc.paths(raw)
	metaRaw, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, "", false
	}
	var meta staticMeta
	if err = json.Unmarshal(metaRaw, &meta); err != nil || meta.URL != raw {
		return nil, "", false
	}
	if time.Since(time.Unix(meta.SavedAt, 0)) > staticEntryTTL {
		return nil, "", false
	}
	body, err := os.ReadFile(bodyPath)
	if err != nil || len(body) == 0 {
		return nil, "", false
	}
	return body, meta.ContentType, true
}

func (sc *staticCache) store(raw, ctype string, body []byte) {
	bodyPath, metaPath := sc.paths(raw)
	if err := writeFileAtomic(bodyPath, body); err != nil {
		return
	}
	metaRaw, err := json.Marshal(staticMeta{URL: raw, ContentType: ctype, SavedAt: time.Now().Unix()})
	if err != nil {
		return
	}
	_ = writeFileAtomic(metaPath, metaRaw)
}

// writeFileAtomic 先写临时文件再改名，避免多个注册任务并发写出半截缓存。
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err = tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err = os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// summary 本次注册省下的静态资源流量。
func (sc *staticCache) summary() string {
	if sc == nil {
		return ""
	}
	saved := sc.hitBytes.Load() + sc.directBytes.Load()
	return fmt.Sprintf("静态资源未走代理 %s（缓存命中 %d 个 / 直连下载 %d 个，回退代理 %d 个）",
		humanBytes(saved), sc.hitN.Load(), sc.directN.Load(), sc.fallbackN.Load())
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
