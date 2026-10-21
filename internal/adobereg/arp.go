package adobereg

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// arpCaptureTimeout 限制整段 ARP 采集的时间。Firefly 编辑器首屏 + SherlockSdk
// 回调一般 20~40 秒，给足余量但绝不无限等待。
const arpCaptureTimeout = 3 * time.Minute

// CaptureARPSession 用一次无头 Chromium 打开 Firefly 编辑器，让页面里的
// SherlockSdk 现场生成一个新的 x-arp-session-id，并把浏览器里的最新 Cookie
// 一并带回。
//
// 为什么必须用浏览器：ARP 令牌由 Firefly 前端的 SherlockSdk 生成，包含
// ark（Arkose arks-client 会话）和 bfp（浏览器指纹会话）等字段，纯 HTTP 无法
// 产出。协议注册只能拿到登录 Cookie（ims_sid），拿不到 ARP；把导入时抓的旧
// 令牌或伪造的 sid+ftr 拿去提交，Adobe 会立刻返回伪装的 408 timeout_error。
//
// auth 为协议注册产出的会话 JSON（含 cookies）。返回更新后的会话 JSON，
// arp_session_token 为新生成的令牌。
// arpFireflyURL 直接进 Firefly 编辑器视图：SherlockSdk/ArpJS 只在真正挂载
// 了编辑器的页面加载，落地页不会初始化，采不到令牌。
const arpFireflyURL = "https://firefly.adobe.com/generate/images?view=generate"

func CaptureARPSession(ctx context.Context, in Input, auth map[string]any) (merged map[string]any, err error) {
	cookies := cookiesFromAuthMap(auth)
	if len(cookies) == 0 {
		return nil, fmt.Errorf("会话里没有 Cookie，无法还原浏览器登录态")
	}
	ctx, cancel := context.WithTimeout(ctx, arpCaptureTimeout)
	defer cancel()

	if in.Headless {
		in.logf("启动无头浏览器生成 ARP 会话令牌")
	} else {
		in.logf("启动可见浏览器生成 ARP 会话令牌")
	}
	browser, bridge, cleanup, err := launchAdobeBrowser(in)
	if err != nil {
		return nil, err
	}
	if bridge != nil {
		defer bridge.Close()
	}
	defer func() {
		_ = rod.Try(browser.MustClose)
		cleanup()
	}()
	// rod 的不少 API 出错会 panic，必须在命名返回值里转成 error，
	// 否则 recover 后调用方会拿到 (nil, nil)，看起来像“成功但令牌为空”。
	defer func() {
		if r := recover(); r != nil {
			merged = nil
			err = fmt.Errorf("ARP 采集流程异常: %v", r)
		}
	}()

	page := browser.MustPage("")
	_ = (proto.EmulationSetDeviceMetricsOverride{
		Width:             1280,
		Height:            900,
		DeviceScaleFactor: 1,
		Mobile:            false,
	}).Call(page)

	if err := setAdobeCookies(page, cookies); err != nil {
		return nil, fmt.Errorf("还原 Cookie 失败: %w", err)
	}
	if err := gotoStable(ctx, page, arpFireflyURL, in, 90*time.Second); err != nil {
		in.logf("打开 Firefly 编辑器异常（继续尝试采集 ARP）: %v", err)
	}
	// Firefly 应用挂载后 SherlockSdk 才会初始化；没就绪就采集必然拿空。
	_ = waitFireflyReady(ctx, page, in, 25*time.Second)
	if onEmailVerify(page, pageURL(page)) {
		return nil, fmt.Errorf("Firefly 把会话打回邮箱验证页，无法生成 ARP")
	}
	in.logf("ARP 采集页面: %s", trimText(pageURL(page), 120))

	fresh, err := captureAuth(page, in)
	if err != nil {
		return nil, fmt.Errorf("采集浏览器会话失败: %w", err)
	}
	token, _ := fresh["arp_session_token"].(string)
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("Firefly 未能生成 ARP 会话令牌")
	}
	in.logf("ARP 会话令牌已生成：%s", DescribeARPToken(token))

	// 浏览器侧的 Cookie 比协议阶段更完整（Firefly 会追加会话 Cookie），
	// 优先用浏览器里的这一份，同时保留注册阶段的元信息。
	merged = make(map[string]any, len(auth)+len(fresh))
	for k, v := range auth {
		merged[k] = v
	}
	for k, v := range fresh {
		merged[k] = v
	}
	merged["auth_mode"] = "adobe_protocol_session"
	merged["register_engine"] = "protocol"
	merged["arp_captured_at"] = time.Now().UTC().Format(time.RFC3339)
	return merged, nil
}

// cookiesFromAuthMap 从会话 JSON 里取 Cookie 列表（浏览器注入需要完整字段）。
func cookiesFromAuthMap(auth map[string]any) []map[string]any {
	raw, ok := auth["cookies"].([]map[string]any)
	if ok {
		return raw
	}
	// JSON 反序列化后一般是 []any，需要转换。
	list, ok := auth["cookies"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// arpRequiredFields 是实测可用的 ARP 令牌必须具备的字段：ark 来自 Arkose，
// bfp 来自浏览器指纹，缺任一个 Adobe 都会按伪造会话拒绝。
var arpRequiredFields = []string{"sid", "ftr", "ark", "bfp"}

// ARPMissingFields 返回令牌里缺失的必需字段；令牌为空时返回全部字段。
func ARPMissingFields(token string) []string {
	token = strings.TrimSpace(token)
	payload := map[string]any{}
	if token != "" {
		if raw, err := base64.StdEncoding.DecodeString(token); err == nil {
			_ = json.Unmarshal(raw, &payload)
		}
	}
	missing := make([]string, 0, len(arpRequiredFields))
	for _, field := range arpRequiredFields {
		if value, ok := payload[field].(string); !ok || strings.TrimSpace(value) == "" {
			missing = append(missing, field)
		}
	}
	return missing
}

// DescribeARPToken 只描述令牌字段构成，不输出令牌值本身。
func DescribeARPToken(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return "arp=none"
	}
	raw, err := base64.StdEncoding.DecodeString(token)
	var payload map[string]any
	if err != nil || json.Unmarshal(raw, &payload) != nil {
		return "arp=unparseable"
	}
	present := make([]string, 0, len(arpRequiredFields))
	for _, field := range arpRequiredFields {
		if value, ok := payload[field].(string); ok && strings.TrimSpace(value) != "" {
			present = append(present, field)
		}
	}
	return "arp_fields=" + strings.Join(present, ",")
}
