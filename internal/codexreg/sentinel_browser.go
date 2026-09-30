package codexreg

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SentinelSmoke 是一次官方 SDK 签发的非敏感摘要，不包含 token 本身。
type SentinelSmoke struct {
	Flow     string
	HasT     bool
	HasC     bool
	PPrefix  string
	TokenLen int
	SOLen    int
	Shape    string
}

// SmokeSentinel 用协议客户端打开 chatgpt.com，再让 Node 假浏览器里的官方 SDK 签发一个 flow token。
func SmokeSentinel(ctx context.Context, proxy, flow string) (SentinelSmoke, error) {
	if strings.TrimSpace(flow) == "" {
		flow = "email_otp_validate"
	}
	c, err := newProtocolClient(Input{Proxy: proxy}, "en-US,en")
	if err != nil {
		return SentinelSmoke{}, err
	}
	defer c.closeSentinel()
	if r, err := c.get(ctx, "https://chatgpt.com/auth/login", "", true); err != nil {
		return SentinelSmoke{}, fmt.Errorf("打开 chatgpt.com 失败: %w", err)
	} else if r.Status != 200 {
		return SentinelSmoke{}, fmt.Errorf("chatgpt.com HTTP %d", r.Status)
	}
	token, so, err := c.realSentinelToken(ctx, flow, "https://auth.openai.com/")
	if err != nil {
		return SentinelSmoke{}, err
	}
	out := SentinelSmoke{Flow: flow, TokenLen: len(token), SOLen: len(so), Shape: sentinelTokenShape(token)}
	var obj map[string]any
	if json.Unmarshal([]byte(token), &obj) == nil {
		if p, _ := obj["p"].(string); p != "" {
			if len(p) > 8 {
				out.PPrefix = p[:8]
			} else {
				out.PPrefix = p
			}
		}
		_, out.HasT = obj["t"]
		_, out.HasC = obj["c"]
	}
	return out, nil
}

func (c *protocolClient) realSentinelToken(ctx context.Context, flow, pageURL string) (string, string, error) {
	if c.deviceID == "" {
		c.deviceID = cookieValue(c, "https://chatgpt.com/", "oai-did")
	}
	if c.deviceID == "" {
		c.deviceID = uuid.NewString()
	}
	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		token, so, err := c.mintSentinelNode(ctx, flow, pageURL)
		if err == nil {
			return token, so, nil
		}
		last = err
		c.in.logf("🛡 Sentinel 签发失败(%s) %d/3: %v", flow, attempt, err)
		c.closeSentinelNode()
		if strings.Contains(err.Error(), "未找到 node") || strings.Contains(err.Error(), "找不到 sentinel_node.js") {
			break
		}
		time.Sleep(time.Duration(attempt) * 800 * time.Millisecond)
	}
	return "", "", last
}

func (c *protocolClient) closeSentinel() {
	c.closeSentinelNode()
}

func (c *protocolClient) sentinelDisplay() string {
	if c.node != nil {
		return "node"
	}
	return ""
}

func sentinelMintURL() string {
	return "https://chatgpt.com/auth/login"
}

func sentinelDocumentURL(flow, pageURL string) string {
	pageURL = strings.TrimSpace(pageURL)
	if strings.HasPrefix(pageURL, "https://auth.openai.com/") && pageURL != "https://auth.openai.com/" {
		return pageURL
	}
	switch flow {
	case "oauth_create_account":
		return "https://auth.openai.com/about-you"
	case "email_otp_validate":
		return "https://auth.openai.com/email-verification"
	default:
		if strings.HasPrefix(pageURL, "https://auth.openai.com/") {
			return pageURL
		}
		return sentinelMintURL()
	}
}

func sentinelTokenID(token string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(token)), &obj) != nil {
		return ""
	}
	id, _ := obj["id"].(string)
	return strings.TrimSpace(id)
}

func authPageURLsEqual(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	if err1 != nil || err2 != nil {
		return strings.TrimSpace(a) == strings.TrimSpace(b)
	}
	return strings.EqualFold(ua.Host, ub.Host) &&
		strings.TrimSuffix(ua.Path, "/") == strings.TrimSuffix(ub.Path, "/")
}

func rejectForgedSentinelToken(token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("sentinel_sdk_empty_token")
	}
	if isForgedSentinelToken(token) {
		return fmt.Errorf("sentinel_forged_token_rejected")
	}
	return nil
}

// isForgedSentinelToken 识别旧版本地拼接的伪 token：
// p = "gAAAAAB" + base64(本地指纹) + "~S"，且没有官方 SDK 的 t/c 字段。
// 真实 SDK token 的 p 也可能以 gAAAAAB 开头，因此不能只看前缀。
func isForgedSentinelToken(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	var obj map[string]any
	if json.Unmarshal([]byte(token), &obj) != nil {
		return false
	}
	p, _ := obj["p"].(string)
	_, hasT := obj["t"]
	_, hasC := obj["c"]
	if hasT || hasC {
		return false
	}
	keys := map[string]struct{}{}
	for k := range obj {
		keys[k] = struct{}{}
	}
	_, hasID := keys["id"]
	_, hasFlow := keys["flow"]
	if !hasID || !hasFlow || !strings.HasSuffix(strings.TrimSpace(p), "~S") {
		return false
	}
	for k := range keys {
		if k != "p" && k != "id" && k != "flow" {
			return false
		}
	}
	return true
}

func sentinelTokenShape(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return "empty"
	}
	var obj map[string]any
	if json.Unmarshal([]byte(token), &obj) != nil {
		suf := token
		if len(suf) > 2 {
			suf = suf[len(suf)-2:]
		}
		pre := token
		if len(pre) > 8 {
			pre = pre[:8]
		}
		return fmt.Sprintf("raw len=%d prefix=%s suffix=%s", len(token), pre, suf)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	p, _ := obj["p"].(string)
	psuf := ""
	if len(p) >= 2 {
		psuf = p[len(p)-2:]
	}
	return fmt.Sprintf("json keys=%s p_len=%d p_suffix=%s", strings.Join(keys, ","), len(p), psuf)
}
