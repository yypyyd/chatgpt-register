package livecheck

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Adobe 测活口径与下游 image2api 完全一致：拿账号 Cookie 去 IMS
// check/v6/token 端点做真正的 cookie→token 交换。能换出 access_token 才算"有效"，
// 因为下游图片生成正是靠这一步拿 token；只能登录 account.adobe.com 但换不到 token
// 的账号（例如被 Adobe 卡在 ride 身份二次核验）下游同样不可用，判为失效。
const (
	adobeTokenURL  = "https://adobeid-na1.services.adobe.com/ims/check/v6/token?jslVersion=v2-v0.48.0-1-g1e322cb"
	adobeClientID  = "clio-playground-web"
	adobeScope     = "AdobeID,firefly_api,openid,pps.read,pps.write,additional_info.projectedProductContext,additional_info.ownerOrg,uds_read,uds_write,ab.manage,read_organizations,additional_info.roles,account_cluster.read,creative_production,profile"
	adobeUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
)

// AdobeCookie 从 AuthData 还原出来的单条 Cookie。
type AdobeCookie struct {
	Name     string
	Value    string
	Domain   string
	Path     string
	Secure   bool
	HTTPOnly bool
	Expires  float64
	SameSite string
}

// AdobeItem 一个待测活的 Adobe 账号：ID + 已保存的登录 Cookie 集合。
type AdobeItem struct {
	ID      uint
	Cookies []AdobeCookie
}

// CheckAdobe 批量测活 Adobe 账号：对每个账号用其 Cookie 做 cookie→token 交换。
//
//	换出 access_token（HTTP 200）                 -> alive
//	被上游明确拒绝（400/401/403，如 ride 核验）    -> dead
//	网络错误 / 超时 / 429 / 5xx / 无 Cookie        -> unknown
func CheckAdobe(ctx context.Context, items []AdobeItem, onChunk Chunk) map[uint]string {
	out := make(map[uint]string, len(items))
	if len(items) == 0 {
		return out
	}

	client := &http.Client{Timeout: 30 * time.Second}
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, 4)
	)
	record := func(id uint, st string) {
		mu.Lock()
		out[id] = st
		mu.Unlock()
		emit(onChunk, map[uint]string{id: st})
	}

	for _, it := range items {
		if ctx.Err() != nil {
			record(it.ID, StatusUnknown)
			continue
		}
		if len(it.Cookies) == 0 {
			record(it.ID, StatusUnknown)
			continue
		}
		wg.Add(1)
		go func(it AdobeItem) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			record(it.ID, probeAdobeToken(ctx, client, it))
		}(it)
	}
	wg.Wait()
	return out
}

func probeAdobeToken(ctx context.Context, client *http.Client, it AdobeItem) string {
	cookie := adobeCookieHeader(it.Cookies)
	if cookie == "" {
		return StatusUnknown
	}

	body := "client_id=" + adobeClientID + "&guest_allowed=true&scope=" + strings.ReplaceAll(adobeScope, ",", "%2C")
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, adobeTokenURL, strings.NewReader(body))
	if err != nil {
		return StatusUnknown
	}
	req.Header.Set("accept", "*/*")
	req.Header.Set("accept-language", "zh-CN,zh;q=0.9")
	req.Header.Set("content-type", "application/x-www-form-urlencoded;charset=UTF-8")
	req.Header.Set("origin", "https://firefly.adobe.com")
	req.Header.Set("referer", "https://firefly.adobe.com/")
	req.Header.Set("user-agent", adobeUserAgent)
	req.Header.Set("cookie", cookie)

	resp, err := client.Do(req)
	if err != nil {
		return StatusUnknown
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusOK:
		// 必须真的换出 access_token 才算有效（与下游判据一致）。
		var payload map[string]any
		if json.Unmarshal(raw, &payload) == nil {
			if tok, _ := payload["access_token"].(string); strings.TrimSpace(tok) != "" {
				return StatusAlive
			}
		}
		return StatusUnknown
	case resp.StatusCode == http.StatusBadRequest ||
		resp.StatusCode == http.StatusUnauthorized ||
		resp.StatusCode == http.StatusForbidden:
		// 上游明确拒绝签发 token（会话失效、需 ride 身份核验等）-> 下游不可用。
		return StatusDead
	default:
		// 429 / 5xx 等临时状况，无法确定，绝不判死。
		return StatusUnknown
	}
}

// CookiesFromAuthJSON 从注册产出的 AuthData(JSON 文本) 里解析出 Cookie 列表，
// 供注册成功后立即自检 cookie→token 交换使用。交换只需 name=value，故此处只取这两项。
func CookiesFromAuthJSON(authData string) []AdobeCookie {
	var parsed struct {
		Cookies []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"cookies"`
	}
	if json.Unmarshal([]byte(authData), &parsed) != nil {
		return nil
	}
	out := make([]AdobeCookie, 0, len(parsed.Cookies))
	for _, c := range parsed.Cookies {
		if strings.TrimSpace(c.Name) == "" {
			continue
		}
		out = append(out, AdobeCookie{Name: c.Name, Value: c.Value})
	}
	return out
}

// adobeCookieHeader 把 Cookie 列表拼成 HTTP Cookie 请求头（name=value; ...）。
func adobeCookieHeader(cookies []AdobeCookie) string {
	parts := make([]string, 0, len(cookies))
	for _, ck := range cookies {
		name := strings.TrimSpace(ck.Name)
		if name == "" {
			continue
		}
		parts = append(parts, name+"="+ck.Value)
	}
	return strings.Join(parts, "; ")
}
