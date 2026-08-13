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

// Leonardo 测活口径：拿账号 Cookie 请求 better-auth 的会话接口。
// 该接口未登录时返回 200 + 空会话（null），登录时返回带 user 的会话对象。
const (
	leonardoSessionURL = "https://app.leonardo.ai/api/auth/get-session"
	leonardoUserAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
)

// LeonardoCookie 从 AuthData 还原出来的单条 Cookie。
type LeonardoCookie struct {
	Name  string
	Value string
}

// LeonardoItem 一个待测活的 Leonardo 账号：ID + 已保存的登录 Cookie 集合。
type LeonardoItem struct {
	ID      uint
	Cookies []LeonardoCookie
}

// CheckLeonardo 批量测活 Leonardo 账号：
//
//	会话接口返回带 user 的会话对象           -> alive
//	返回空会话 / 401 / 403                   -> dead
//	网络错误 / 超时 / 429 / 5xx / 无 Cookie  -> unknown
func CheckLeonardo(ctx context.Context, items []LeonardoItem, onChunk Chunk) map[uint]string {
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
		if ctx.Err() != nil || len(it.Cookies) == 0 {
			record(it.ID, StatusUnknown)
			continue
		}
		wg.Add(1)
		go func(it LeonardoItem) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			record(it.ID, probeLeonardoSession(ctx, client, it))
		}(it)
	}
	wg.Wait()
	return out
}

func probeLeonardoSession(ctx context.Context, client *http.Client, it LeonardoItem) string {
	cookie := leonardoCookieHeader(it.Cookies)
	if cookie == "" {
		return StatusUnknown
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, leonardoSessionURL, nil)
	if err != nil {
		return StatusUnknown
	}
	req.Header.Set("accept", "application/json, text/plain, */*")
	req.Header.Set("accept-language", "en-US,en;q=0.9")
	req.Header.Set("referer", "https://app.leonardo.ai/")
	req.Header.Set("user-agent", leonardoUserAgent)
	req.Header.Set("cookie", cookie)

	resp, err := client.Do(req)
	if err != nil {
		return StatusUnknown
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusOK:
		var payload map[string]any
		if json.Unmarshal(raw, &payload) != nil || payload == nil {
			// 未登录时该接口返回 null。
			return StatusDead
		}
		if _, ok := payload["user"]; ok {
			return StatusAlive
		}
		if _, ok := payload["session"]; ok {
			return StatusAlive
		}
		return StatusDead
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return StatusDead
	default:
		// 429 / 5xx 等临时状况，无法确定，绝不判死。
		return StatusUnknown
	}
}

// LeonardoCookiesFromAuthJSON 从注册产出的 AuthData(JSON 文本) 里解析出 Cookie 列表，
// 供注册成功后立即自检会话使用。请求只需 name=value，故此处只取这两项。
func LeonardoCookiesFromAuthJSON(authData string) []LeonardoCookie {
	var parsed struct {
		Cookies []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"cookies"`
	}
	if json.Unmarshal([]byte(authData), &parsed) != nil {
		return nil
	}
	out := make([]LeonardoCookie, 0, len(parsed.Cookies))
	for _, c := range parsed.Cookies {
		if strings.TrimSpace(c.Name) == "" {
			continue
		}
		out = append(out, LeonardoCookie{Name: c.Name, Value: c.Value})
	}
	return out
}

// leonardoCookieHeader 把 Cookie 列表拼成 HTTP Cookie 请求头（name=value; ...）。
func leonardoCookieHeader(cookies []LeonardoCookie) string {
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
