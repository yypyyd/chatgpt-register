package livecheck

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// xAI OAuth 默认参数（与 internal/grokreg 注册时铸造 CPA 凭证所用一致）。
const (
	defaultXaiTokenEndpoint = "https://auth.x.ai/oauth2/token"
	defaultXaiClientID      = "b1a00492-073a-47ea-816f-4c329264a828"
)

// GrokItem 一个待测活的 Grok 账号：ID + 从 cpa_xai 取出的 refresh_token 及可选端点/客户端。
type GrokItem struct {
	ID            uint
	RefreshToken  string
	TokenEndpoint string
	ClientID      string
}

// CheckGrok 批量测活 Grok 账号。Grok 不需要浏览器：用注册时铸造的 xAI OAuth
// refresh_token 向 token 端点做一次 refresh_token 授权——这是最轻量且稳定的活性信号。
//
//	200 且返回 access_token           -> alive
//	400/401 且 invalid_grant 等认证错 -> dead
//	网络 / 429 / 5xx / 超时 / 无 refresh_token -> unknown
func CheckGrok(ctx context.Context, items []GrokItem, onChunk Chunk) map[uint]string {
	out := make(map[uint]string, len(items))
	if len(items) == 0 {
		return out
	}
	client := &http.Client{Timeout: 20 * time.Second}

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, 5)
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
		if strings.TrimSpace(it.RefreshToken) == "" {
			// 没有 refresh_token（如仅有 sso 的旧账号）无法用该方式验证，判 unknown。
			record(it.ID, StatusUnknown)
			continue
		}
		wg.Add(1)
		go func(it GrokItem) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			record(it.ID, probeGrokOne(ctx, client, it))
		}(it)
	}
	wg.Wait()
	return out
}

func probeGrokOne(ctx context.Context, client *http.Client, it GrokItem) string {
	endpoint := strings.TrimSpace(it.TokenEndpoint)
	if endpoint == "" {
		endpoint = defaultXaiTokenEndpoint
	}
	clientID := strings.TrimSpace(it.ClientID)
	if clientID == "" {
		clientID = defaultXaiClientID
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", it.RefreshToken)
	form.Set("client_id", clientID)

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return StatusUnknown
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "grok-register-cpa/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return StatusUnknown
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == 200:
		var parsed struct {
			AccessToken string `json:"access_token"`
		}
		_ = json.Unmarshal(body, &parsed)
		if strings.TrimSpace(parsed.AccessToken) != "" {
			return StatusAlive
		}
		return StatusUnknown
	case resp.StatusCode == 400 || resp.StatusCode == 401:
		// 只有明确的认证类错误才判死；其它 4xx（如限流误报）保持 unknown。
		if isAuthError(body) {
			return StatusDead
		}
		return StatusUnknown
	default:
		return StatusUnknown
	}
}

// isAuthError 判断 OAuth 错误体是否属于"凭据失效"类（可判死）。
func isAuthError(body []byte) bool {
	var parsed struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(parsed.Error)) {
	case "invalid_grant", "invalid_token", "unauthorized_client", "invalid_client", "access_denied":
		return true
	default:
		return false
	}
}
