package livecheck

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Lovart 测活口径：用保存的 accessToken 请求 /api/www/user/getUserInfo，
// accessToken 过期时先用 refreshToken 刷新。只把明确的登录拒绝判死。

const (
	lovartUserInfoURL = "https://www.lovart.ai/api/www/user/getUserInfo"
	lovartRefreshURL  = "https://www.lovart.ai/api/www/lovart/login/v2/token/refresh"
	lovartUserAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
)

type LovartCookie struct {
	Name  string
	Value string
}

// LovartItem 一个待测活的 Lovart 账号。
type LovartItem struct {
	ID           uint
	AccessToken  string
	RefreshToken string
	Cookies      []LovartCookie
	Proxy        string
}

// CheckLovart 批量测活。返回 id -> alive/dead/unknown。
func CheckLovart(ctx context.Context, items []LovartItem, onChunk Chunk) map[uint]string {
	out := make(map[uint]string, len(items))
	if len(items) == 0 {
		return out
	}

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
		if ctx.Err() != nil || (it.AccessToken == "" && it.RefreshToken == "") {
			record(it.ID, StatusUnknown)
			continue
		}
		wg.Add(1)
		go func(it LovartItem) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			client, closeClient, err := probeClient(it.Proxy)
			if err != nil {
				record(it.ID, StatusUnknown)
				return
			}
			defer closeClient()
			record(it.ID, probeLovartSession(ctx, client, it))
		}(it)
	}
	wg.Wait()
	return out
}

func probeLovartSession(ctx context.Context, client *http.Client, it LovartItem) string {
	access := strings.TrimSpace(it.AccessToken)
	expired := access == "" || jwtExpired(access)
	if expired {
		tok, err := refreshLovartToken(ctx, client, it)
		if err != nil {
			// accessToken 已过期又无法刷新时，不能拿它去探活，否则会把
			// 网络问题误判成 dead。
			return StatusUnknown
		}
		access = tok
	}
	if access == "" {
		return StatusUnknown
	}
	st := callLovartUserInfo(ctx, client, it, access)
	if st == StatusDead && strings.TrimSpace(it.RefreshToken) != "" {
		if tok, err := refreshLovartToken(ctx, client, it); err == nil && tok != access {
			st = callLovartUserInfo(ctx, client, it, tok)
		}
	}
	return st
}

func callLovartUserInfo(ctx context.Context, client *http.Client, it LovartItem, accessToken string) string {
	reqCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, lovartUserInfoURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return StatusUnknown
	}
	req.Header.Set("accept", "application/json, text/plain, */*")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("origin", "https://www.lovart.ai")
	req.Header.Set("referer", "https://www.lovart.ai/")
	req.Header.Set("user-agent", lovartUserAgent)
	req.Header.Set("token", accessToken)
	if cookie := lovartCookieHeader(it.Cookies); cookie != "" {
		req.Header.Set("cookie", cookie)
	}

	resp, err := client.Do(req)
	if err != nil {
		return StatusUnknown
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return StatusDead
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return StatusUnknown
	case resp.StatusCode == http.StatusOK:
		var payload struct {
			Code int             `json:"code"`
			Msg  string          `json:"msg"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return StatusUnknown
		}
		if payload.Code == 0 {
			var user map[string]any
			if json.Unmarshal(payload.Data, &user) == nil && user != nil {
				if id, ok := user["id"]; ok && id != nil {
					return StatusAlive
				}
			}
			return StatusUnknown
		}
		if payload.Code == 401 || strings.Contains(strings.ToLower(payload.Msg), "no login") {
			return StatusDead
		}
		return StatusUnknown
	default:
		return StatusUnknown
	}
}

func refreshLovartToken(ctx context.Context, client *http.Client, it LovartItem) (string, error) {
	rt := strings.TrimSpace(it.RefreshToken)
	if rt == "" {
		return "", io.EOF
	}
	body, _ := json.Marshal(map[string]any{"refreshToken": rt})
	reqCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, lovartRefreshURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("accept", "application/json, text/plain, */*")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("origin", "https://www.lovart.ai")
	req.Header.Set("referer", "https://www.lovart.ai/")
	req.Header.Set("user-agent", lovartUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", io.EOF
	}
	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken string `json:"accessToken"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Code != 0 || payload.Data.AccessToken == "" {
		return "", io.EOF
	}
	return payload.Data.AccessToken, nil
}

func jwtExpired(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return false
	}
	return time.Now().Unix() > claims.Exp-60
}

// LovartItemsFromAuthJSON 从 AuthData 解析 accessToken / refreshToken / Cookie。
func LovartItemsFromAuthJSON(id uint, authData, proxy string) LovartItem {
	item := LovartItem{ID: id, Proxy: proxy}
	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Cookies      []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"cookies"`
	}
	if json.Unmarshal([]byte(authData), &parsed) != nil {
		return item
	}
	item.AccessToken = parsed.AccessToken
	item.RefreshToken = parsed.RefreshToken
	for _, ck := range parsed.Cookies {
		if strings.TrimSpace(ck.Name) == "" {
			continue
		}
		item.Cookies = append(item.Cookies, LovartCookie{Name: ck.Name, Value: ck.Value})
	}
	return item
}

func lovartCookieHeader(cookies []LovartCookie) string {
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
