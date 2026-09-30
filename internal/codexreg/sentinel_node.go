package codexreg

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

// 认证页（auth.openai.com）里官方 SDK 来自 sentinel.openai.com：先拉 /backend-api/sentinel/sdk.js 引导脚本，
// 再拉 /sentinel/<版本>/sdk.js，iframe 与 /req 也都在 sentinel.openai.com 上（真实浏览器抓包）。
const sentinelHost = "https://sentinel.openai.com"

var sentinelVersionedSDKRe = regexp.MustCompile(`https://(?:sentinel\.openai|chatgpt)\.com/sentinel/([^/"'\s]+)/sdk\.js`)

type sentinelNode struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	sc     *bufio.Scanner
	stderr bytesBuffer
	mu     sync.Mutex
	sdk    string
	sdkURL string
	ready  bool
}

type bytesBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *bytesBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.b.Len() > 8000 {
		return len(p), nil
	}
	w.b.Write(p)
	return len(p), nil
}

func (w *bytesBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

type nodeMsg struct {
	Type    string            `json:"type"`
	ID      int               `json:"id"`
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	Token   string            `json:"token"`
	SO      string            `json:"so"`
	Error   string            `json:"error"`
	Status  int               `json:"status"`
}

func parseSentinelSDKURL(body []byte) string {
	m := sentinelVersionedSDKRe.FindSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return sentinelHost + "/sentinel/" + string(m[1]) + "/sdk.js"
}

func isFullSentinelSDK(body []byte) bool {
	s := string(body)
	return strings.Contains(s, "var SentinelSDK") || strings.Contains(s, "SentinelSDK=function")
}

func sentinelNodeScript() (string, error) {
	exe, _ := os.Executable()
	cands := []string{
		filepath.Join(filepath.Dir(exe), "scripts", "sentinel_node.js"),
		"/usr/local/share/chatgpt-register/scripts/sentinel_node.js",
		"scripts/sentinel_node.js",
		filepath.Join(".", "scripts", "sentinel_node.js"),
	}
	if wd, err := os.Getwd(); err == nil {
		cands = append(cands, filepath.Join(wd, "scripts", "sentinel_node.js"))
	}
	tried := make([]string, 0, len(cands))
	for _, p := range cands {
		if strings.TrimSpace(p) == "" {
			continue
		}
		tried = append(tried, p)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("找不到 sentinel_node.js，已查找: %s", strings.Join(tried, " ; "))
}

func sentinelNodeBin() string {
	for _, name := range []string{"node", "nodejs"} {
		if p, err := exec.LookPath(name); err == nil && strings.TrimSpace(p) != "" {
			return p
		}
	}
	return ""
}

func (c *protocolClient) ensureSentinelNode(ctx context.Context) error {
	if c.node != nil {
		return nil
	}
	bin := sentinelNodeBin()
	if bin == "" {
		return fmt.Errorf("未找到 node，无法用官方 Sentinel SDK 签发")
	}
	script, err := sentinelNodeScript()
	if err != nil {
		return err
	}
	sdkURL, sdk, err := c.fetchOfficialSentinelSDK(ctx)
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, script)
	cmd.Dir = filepath.Dir(script)
	cmd.Env = append(os.Environ(), "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8", "LC_TIME=en_US.UTF-8", "LANGUAGE=en_US")
	if tz := strings.TrimSpace(c.timezone); tz != "" {
		cmd.Env = append(cmd.Env, "TZ="+tz)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	n := &sentinelNode{cmd: cmd, stdin: stdin, sdk: string(sdk), sdkURL: sdkURL}
	cmd.Stderr = &n.stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 node Sentinel 失败: %w", err)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	n.sc = sc
	c.node = n
	if _, err := n.readMsg(ctx, 8*time.Second); err != nil {
		c.closeSentinelNode()
		return fmt.Errorf("node Sentinel 无握手: %w", err)
	}
	return nil
}

func (c *protocolClient) nodeLanguages() []string {
	langs := make([]string, 0, 4)
	for _, p := range strings.Split(c.languages, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			langs = append(langs, p)
		}
	}
	return langs
}

func (c *protocolClient) initSentinelNode(ctx context.Context, snap map[string]any) error {
	n := c.node
	if n == nil {
		return fmt.Errorf("node Sentinel 未启动")
	}
	msg := map[string]any{
		"type":         "init",
		"sdk":          n.sdk,
		"sdkURL":       n.sdkURL,
		"ua":           c.ua,
		"languages":    c.nodeLanguages(),
		"deviceID":     c.deviceID,
		"screenWidth":  c.screen.Width,
		"screenHeight": c.screen.Height,
		"dpr":          c.screen.DPR,
		"hwc":          c.screen.HWConcurrency,
		"mem":          c.screen.DeviceMemory,
		"touch":        c.screen.MaxTouch,
		"chromeMajor":  c.chromeMajor,
		"chromeFull":   chromeFullVersion(c.chromeMajor),
		"timezone":     c.timezone,
		"locale":       c.locale,
	}
	for k, v := range snap {
		msg[k] = v
	}
	if err := n.writeMsg(msg); err != nil {
		c.closeSentinelNode()
		return err
	}
	ready, err := n.loopUntil(ctx, c, 45*time.Second, "ready")
	if err != nil {
		c.closeSentinelNode()
		return fmt.Errorf("node Sentinel 初始化失败: %w stderr=%s", err, truncateForLog(n.stderr.String(), 300))
	}
	if ready.Type != "ready" {
		c.closeSentinelNode()
		return fmt.Errorf("node Sentinel 初始化异常 type=%s err=%s", ready.Type, ready.Error)
	}
	n.ready = true
	c.in.logf("🛡 Sentinel 已改用 Node 假浏览器跑官方 SDK tls=%s ua=Chrome/%s tz=%s", protocolTLSProfileName(), c.chromeMajor, c.timezone)
	c.logSentinelHydration("init", snap)
	return nil
}

func (c *protocolClient) logSentinelHydration(phase string, snap map[string]any) {
	n := 0
	switch s := snap["scripts"].(type) {
	case []string:
		n = len(s)
	case []any:
		n = len(s)
	}
	build, _ := snap["dataBuild"].(string)
	ago := snap["loadedAgoMs"]
	search, _ := snap["searchKeys"].(string)
	href, _ := snap["href"].(string)
	c.in.logf("🛡 Sentinel 页面水合 phase=%s scripts=%d build=%s age_ms=%v search=%s page=%s",
		phase, n, truncateForLog(build, 48), ago, search, truncateForLog(href, 96))
}

func (c *protocolClient) fetchOfficialSentinelSDK(ctx context.Context) (string, []byte, error) {
	boot, err := c.get(ctx, sentinelHost+"/backend-api/sentinel/sdk.js", "https://auth.openai.com/", false)
	if err != nil {
		return "", nil, fmt.Errorf("拉取 Sentinel bootstrap 失败: %w", err)
	}
	if boot.Status != 200 || len(boot.Body) == 0 {
		return "", nil, fmt.Errorf("Sentinel bootstrap HTTP %d", boot.Status)
	}
	sdkURL := parseSentinelSDKURL(boot.Body)
	if sdkURL == "" && isFullSentinelSDK(boot.Body) {
		return sentinelHost + "/backend-api/sentinel/sdk.js", boot.Body, nil
	}
	if sdkURL == "" {
		return "", nil, fmt.Errorf("Sentinel bootstrap 未给出官方 sdk.js 地址")
	}
	full, err := c.get(ctx, sdkURL, "https://auth.openai.com/", false)
	if err != nil {
		return "", nil, fmt.Errorf("拉取官方 Sentinel SDK 失败: %w", err)
	}
	if full.Status != 200 || !isFullSentinelSDK(full.Body) {
		return "", nil, fmt.Errorf("官方 Sentinel SDK HTTP %d 或内容无法识别", full.Status)
	}
	c.in.logf("🛡 已加载官方 Sentinel SDK %s bytes=%d", sdkURL, len(full.Body))
	return sdkURL, full.Body, nil
}

func (n *sentinelNode) writeMsg(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	_, err = n.stdin.Write(append(b, '\n'))
	return err
}

func (n *sentinelNode) readMsg(ctx context.Context, timeout time.Duration) (nodeMsg, error) {
	type result struct {
		msg nodeMsg
		err error
	}
	ch := make(chan result, 1)
	go func() {
		if n.sc.Scan() {
			var msg nodeMsg
			if err := json.Unmarshal(n.sc.Bytes(), &msg); err != nil {
				ch <- result{err: fmt.Errorf("node 输出不是 JSON")}
				return
			}
			ch <- result{msg: msg}
			return
		}
		if err := n.sc.Err(); err != nil {
			ch <- result{err: err}
			return
		}
		ch <- result{err: io.EOF}
	}()
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case <-tctx.Done():
		return nodeMsg{}, tctx.Err()
	case r := <-ch:
		return r.msg, r.err
	}
}

func (n *sentinelNode) loopUntil(ctx context.Context, c *protocolClient, timeout time.Duration, want string) (nodeMsg, error) {
	deadline := time.Now().Add(timeout)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return nodeMsg{}, fmt.Errorf("node Sentinel 等待 %s 超时", want)
		}
		msg, err := n.readMsg(ctx, left)
		if err != nil {
			return nodeMsg{}, err
		}
		switch msg.Type {
		case "fetch":
			if err := n.fulfillFetch(ctx, c, msg); err != nil {
				_ = n.writeMsg(map[string]any{"type": "fetch_err", "id": msg.ID, "error": err.Error()})
			}
		case "hello":
			continue
		case want, "ok", "err", "ready":
			if want == "ready" && msg.Type == "ready" {
				return msg, nil
			}
			if want == "ok" && (msg.Type == "ok" || msg.Type == "err") {
				return msg, nil
			}
			if msg.Type == want {
				return msg, nil
			}
			if msg.Type == "err" {
				return msg, fmt.Errorf("%s", msg.Error)
			}
		default:
			continue
		}
	}
}

// SDK 在假浏览器里带上的这些头由协议客户端自己管理，不能让 Node 覆盖。
func skipHijackRequestHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "cookie", "host", "content-length", "connection", "proxy-connection", "keep-alive", "transfer-encoding":
		return true
	default:
		return false
	}
}

func (n *sentinelNode) fulfillFetch(ctx context.Context, c *protocolClient, msg nodeMsg) error {
	raw := strings.TrimSpace(msg.URL)
	if raw == "" {
		return fmt.Errorf("empty_url")
	}
	method := strings.ToUpper(strings.TrimSpace(msg.Method))
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if method != http.MethodGet && method != http.MethodHead {
		body = strings.NewReader(msg.Body)
	}
	req, err := http.NewRequest(method, raw, body)
	if err != nil {
		return err
	}
	referer := "https://chatgpt.com/"
	if strings.HasPrefix(raw, sentinelHost+"/") {
		// iframe 内的同源请求：Referer 是 frame.html?sv=<版本>（真实抓包）。
		referer = sentinelHost + "/backend-api/sentinel/frame.html"
		if m := sentinelVersionedSDKRe.FindStringSubmatch(n.sdkURL); len(m) > 1 {
			referer += "?sv=" + m[1]
		}
	}
	c.headers(req, referer, false)
	if pu, err := url.Parse(raw); err == nil && pu.Host != "" {
		req.Header.Set("origin", pu.Scheme+"://"+pu.Host)
	} else {
		req.Header.Set("origin", "https://chatgpt.com")
	}
	for k, v := range msg.Headers {
		if skipHijackRequestHeader(k) {
			continue
		}
		req.Header.Set(k, v)
	}
	if method != http.MethodGet && req.Header.Get("content-type") == "" {
		req.Header.Set("content-type", "application/json")
	}
	resp, err := c.do(ctx, req)
	if err != nil && resp == nil {
		return err
	}
	status := 0
	var respBody string
	hdrs := map[string]string{}
	if resp != nil {
		status = resp.Status
		respBody = string(resp.Body)
		if ct := resp.Header.Get("content-type"); ct != "" {
			hdrs["content-type"] = ct
		}
		dumpSentinelExchange(raw, msg.Body, respBody)
	}
	return n.writeMsg(map[string]any{
		"type":    "fetch_ok",
		"id":      msg.ID,
		"status":  status,
		"headers": hdrs,
		"body":    respBody,
	})
}

// startSentinelPage 在认证页加载完成时调用。与真实浏览器一致：页面就绪即 SDK.init(flow)，
// Session Observer 收集器从这一刻开始观察；等验证码/停留期间 Node 侧会做稀疏的鼠标漂移，
// 提交时再由 mintSentinelNode 补上点击与逐键输入。
//
// 只有这样，so 快照才会带上真人会话的交互量。零交互(so 全为 "0")的会话即便 Sentinel
// 同步校验通过，也会在数分钟后被异步风控撤销 access token。
// 失败只记录日志，不阻断注册（提交时的 token() 仍会兜底签发）。
func (c *protocolClient) startSentinelPage(ctx context.Context, flow, pageURL string) {
	if err := c.ensureSentinelNode(ctx); err != nil {
		c.in.logf("⚠️ Sentinel 页面预热失败(%s): %v", flow, err)
		return
	}
	href := sentinelDocumentURL(flow, pageURL)
	snap := c.sentinelPageSnapshot(ctx, href)
	if !c.node.ready {
		if err := c.initSentinelNode(ctx, snap); err != nil {
			c.in.logf("⚠️ Sentinel 页面预热失败(%s): %v", flow, err)
			return
		}
	} else {
		c.logSentinelHydration(flow, snap)
	}
	payload := map[string]any{"type": "page", "flow": flow}
	for k, v := range snap {
		payload[k] = v
	}
	if err := c.node.writeMsg(payload); err != nil {
		c.in.logf("⚠️ Sentinel 页面预热失败(%s): %v", flow, err)
		return
	}
	if _, err := c.node.loopUntil(ctx, c, 45*time.Second, "page_ok"); err != nil {
		c.in.logf("⚠️ Sentinel 页面预热失败(%s): %v", flow, err)
		return
	}
	if c.sentinelPageAt == nil {
		c.sentinelPageAt = map[string]time.Time{}
	}
	c.sentinelPageAt[flow] = time.Now()
	c.in.logf("🛡 Sentinel 页面已就绪 flow=%s，Session Observer 开始观察", flow)
}

// dwellOnPage 让“看页面”的停留时间至少达到 minSec~maxSec 之间的随机值（从页面就绪算起）。
// 真人切到邮箱、复制验证码、回到页面输入，或读表单、想名字，最少要十几秒；期间 Node 侧持续做
// 稀疏的鼠标漂移。停留过短、交互量过小的会话，so 快照会显得像脚本。
func (c *protocolClient) dwellOnPage(ctx context.Context, flow string, minSec, maxSec int) {
	start, ok := c.sentinelPageAt[flow]
	if !ok {
		return
	}
	target := time.Duration(minSec+ri(maxSec-minSec+1)) * time.Second
	if left := target - time.Since(start); left > 0 {
		c.in.logf("🕒 在页面上停留 %.0fs（flow=%s）", left.Seconds(), flow)
		select {
		case <-ctx.Done():
		case <-time.After(left):
		}
	}
}

func (c *protocolClient) mintSentinelNode(ctx context.Context, flow, pageURL string) (string, string, error) {
	if err := c.ensureSentinelNode(ctx); err != nil {
		return "", "", err
	}
	href := sentinelDocumentURL(flow, pageURL)
	snap := c.sentinelPageSnapshot(ctx, href)
	if !c.node.ready {
		if err := c.initSentinelNode(ctx, snap); err != nil {
			return "", "", err
		}
	} else {
		c.logSentinelHydration(flow, snap)
	}
	payload := map[string]any{"type": "mint", "flow": flow}
	for k, v := range snap {
		payload[k] = v
	}
	interacted := false
	if c.pendingInteract != nil {
		payload["interact"] = c.pendingInteract
		c.pendingInteract = nil
		interacted = true
	}
	if err := c.node.writeMsg(payload); err != nil {
		return "", "", err
	}
	msg, err := c.node.loopUntil(ctx, c, 60*time.Second, "ok")
	if err != nil {
		return "", "", err
	}
	if msg.Type == "err" || strings.TrimSpace(msg.Token) == "" {
		errText := strings.TrimSpace(msg.Error)
		if errText == "" {
			errText = "empty_token"
		}
		return "", "", fmt.Errorf("sentinel_%s", errText)
	}
	if err := rejectForgedSentinelToken(msg.Token); err != nil {
		return "", "", fmt.Errorf("%w shape=%s", err, sentinelTokenShape(msg.Token))
	}
	if err := checkSentinelT(msg.Token); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(msg.SO) != "" {
		if snap, ok := decodeSessionObserver(msg.SO); ok {
			c.in.logf("🛡 Session Observer flow=%s %s", flow, snap)
			// 明明模拟了输入却一次交互都没记到，说明收集器没接上事件。这样的会话即便
			// 同步签发成功，也会被异步风控撤销 AT——直接失败，不提交。
			if interacted && snap.Empty() {
				return "", "", fmt.Errorf("sentinel_so_empty")
			}
		} else {
			c.in.logf("⚠️ Session Observer 快照无法解读 flow=%s len=%d", flow, len(msg.SO))
		}
	}
	dumpSentinelTokens(flow, msg.Token, msg.SO)
	c.in.logf("🛡 Sentinel 已签发 flow=%s page=%s display=node", flow, href)
	return msg.Token, msg.SO, nil
}

func (c *protocolClient) closeSentinelNode() {
	n := c.node
	c.node = nil
	if n == nil {
		return
	}
	if n.stdin != nil {
		_ = n.stdin.Close()
	}
	if n.cmd != nil && n.cmd.Process != nil {
		_ = n.cmd.Process.Kill()
		_, _ = n.cmd.Process.Wait()
	}
}
