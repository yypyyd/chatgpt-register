package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"chatgpt-register/internal/livecheck"
	"chatgpt-register/internal/models"

	"github.com/gin-gonic/gin"
)

// liveCheckReq 批量测活请求体：ids 为空表示测活全部"已注册且有会话数据"的账号，
// 否则只测选中的这些账号。
type liveCheckReq struct {
	IDs []uint `json:"ids"`
}

// liveRunner 跟踪单个平台一次批量测活任务的进度（只在内存里，供页面轮询展示）。
// 每个平台一个 runner，同一平台同一时刻只允许一个批量任务在跑。
type liveRunner struct {
	mu      sync.Mutex
	running bool
	total   int
	done    int
	alive   int
	dead    int
	unknown int
	message string
}

func newLiveRunners() map[string]*liveRunner {
	return map[string]*liveRunner{
		"chatgpt": {},
		"grok":    {},
		"adobe":   {},
	}
}

// tryStart 尝试占用 runner；已有任务在跑则返回 false。
func (r *liveRunner) tryStart() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return false
	}
	r.running = true
	r.total, r.done, r.alive, r.dead, r.unknown = 0, 0, 0, 0, 0
	r.message = "测活中..."
	return true
}

func (r *liveRunner) setTotal(n int) {
	r.mu.Lock()
	r.total = n
	r.mu.Unlock()
}

func (r *liveRunner) tally(status string) {
	r.mu.Lock()
	r.done++
	switch status {
	case livecheck.StatusAlive:
		r.alive++
	case livecheck.StatusDead:
		r.dead++
	default:
		r.unknown++
	}
	r.mu.Unlock()
}

func (r *liveRunner) finish(msg string) {
	r.mu.Lock()
	r.running = false
	if msg != "" {
		r.message = msg
	} else {
		r.message = "测活完成"
	}
	r.mu.Unlock()
}

func (r *liveRunner) snapshot() gin.H {
	r.mu.Lock()
	defer r.mu.Unlock()
	return gin.H{
		"running": r.running,
		"total":   r.total,
		"done":    r.done,
		"alive":   r.alive,
		"dead":    r.dead,
		"unknown": r.unknown,
		"message": r.message,
	}
}

func (h *Handler) liveRunnerFor(platform string) *liveRunner {
	if h.Live == nil {
		return &liveRunner{}
	}
	if r, ok := h.Live[platform]; ok {
		return r
	}
	return &liveRunner{}
}

/* ===================== ChatGPT ===================== */

func (h *Handler) LiveCheckStart(c *gin.Context) {
	var in liveCheckReq
	_ = c.ShouldBindJSON(&in)
	runner := h.liveRunnerFor("chatgpt")
	if !runner.tryStart() {
		c.JSON(http.StatusConflict, gin.H{"error": "已有测活任务进行中"})
		return
	}
	items, err := h.loadCGItems(in.IDs)
	if err != nil {
		runner.finish("加载账号失败")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if len(items) == 0 {
		runner.finish("没有可测活的账号")
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有可测活的账号（需已注册且有会话数据）"})
		return
	}
	runner.setTotal(len(items))
	go func() {
		defer runner.finish("")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		livecheck.CheckChatGPT(ctx, items, func(chunk map[uint]string) {
			for id, st := range chunk {
				h.applyCGAlive(id, st)
				runner.tally(st)
			}
		})
	}()
	c.JSON(http.StatusOK, gin.H{"ok": true, "total": len(items)})
}

func (h *Handler) LiveCheckStatus(c *gin.Context) {
	c.JSON(http.StatusOK, h.liveRunnerFor("chatgpt").snapshot())
}

func (h *Handler) LiveCheckOne(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	item, ok := h.loadCGItem(uint(id64))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该账号没有可测活的会话数据"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	res := livecheck.CheckChatGPT(ctx, []livecheck.CGItem{item}, nil)
	st := res[item.ID]
	if st == "" {
		st = livecheck.StatusUnknown
	}
	now := h.applyCGAlive(item.ID, st)
	c.JSON(http.StatusOK, gin.H{"id": item.ID, "alive": st, "checked_at": now})
}

func (h *Handler) applyCGAlive(id uint, st string) time.Time {
	now := time.Now()
	h.DB.Model(&models.Registration{}).Where("id = ?", id).
		Updates(map[string]any{"alive": st, "alive_checked_at": now})
	return now
}

func (h *Handler) loadCGItems(ids []uint) ([]livecheck.CGItem, error) {
	var regs []models.Registration
	q := h.DB.Select("id", "auth_data").Where("auth_data <> ''")
	if len(ids) > 0 {
		q = q.Where("id IN ?", ids)
	} else {
		q = q.Where("status = ?", "registered")
	}
	if err := q.Find(&regs).Error; err != nil {
		return nil, err
	}
	items := make([]livecheck.CGItem, 0, len(regs))
	for _, r := range regs {
		if tok := cgAccessToken(r.AuthData); tok != "" {
			items = append(items, livecheck.CGItem{ID: r.ID, Token: tok})
		}
	}
	return items, nil
}

func (h *Handler) loadCGItem(id uint) (livecheck.CGItem, bool) {
	var r models.Registration
	if err := h.DB.Select("id", "auth_data").First(&r, id).Error; err != nil {
		return livecheck.CGItem{}, false
	}
	tok := cgAccessToken(r.AuthData)
	if tok == "" {
		return livecheck.CGItem{}, false
	}
	return livecheck.CGItem{ID: r.ID, Token: tok}, true
}

func cgAccessToken(authData string) string {
	var m map[string]any
	if json.Unmarshal([]byte(authData), &m) != nil {
		return ""
	}
	v, _ := m["access_token"].(string)
	return v
}

/* ===================== Grok ===================== */

func (h *Handler) GrokLiveCheckStart(c *gin.Context) {
	var in liveCheckReq
	_ = c.ShouldBindJSON(&in)
	runner := h.liveRunnerFor("grok")
	if !runner.tryStart() {
		c.JSON(http.StatusConflict, gin.H{"error": "已有测活任务进行中"})
		return
	}
	items, err := h.loadGrokItems(in.IDs)
	if err != nil {
		runner.finish("加载账号失败")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if len(items) == 0 {
		runner.finish("没有可测活的账号")
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有可测活的 Grok 账号（需已注册且有会话数据）"})
		return
	}
	runner.setTotal(len(items))
	go func() {
		defer runner.finish("")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		livecheck.CheckGrok(ctx, items, func(chunk map[uint]string) {
			for id, st := range chunk {
				h.applyGrokAlive(id, st)
				runner.tally(st)
			}
		})
	}()
	c.JSON(http.StatusOK, gin.H{"ok": true, "total": len(items)})
}

func (h *Handler) GrokLiveCheckStatus(c *gin.Context) {
	c.JSON(http.StatusOK, h.liveRunnerFor("grok").snapshot())
}

func (h *Handler) GrokLiveCheckOne(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	item, ok := h.loadGrokItem(uint(id64))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该 Grok 账号没有可测活的会话数据"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()
	res := livecheck.CheckGrok(ctx, []livecheck.GrokItem{item}, nil)
	st := res[item.ID]
	if st == "" {
		st = livecheck.StatusUnknown
	}
	now := h.applyGrokAlive(item.ID, st)
	c.JSON(http.StatusOK, gin.H{"id": item.ID, "alive": st, "checked_at": now})
}

func (h *Handler) applyGrokAlive(id uint, st string) time.Time {
	now := time.Now()
	h.DB.Model(&models.GrokRegistration{}).Where("id = ?", id).
		Updates(map[string]any{"alive": st, "alive_checked_at": now})
	return now
}

func (h *Handler) loadGrokItems(ids []uint) ([]livecheck.GrokItem, error) {
	var regs []models.GrokRegistration
	q := h.DB.Select("id", "auth_data").Where("auth_data <> ''")
	if len(ids) > 0 {
		q = q.Where("id IN ?", ids)
	} else {
		q = q.Where("status = ?", "registered")
	}
	if err := q.Find(&regs).Error; err != nil {
		return nil, err
	}
	items := make([]livecheck.GrokItem, 0, len(regs))
	for _, r := range regs {
		refresh, endpoint, clientID := grokRefreshCreds(r.AuthData)
		items = append(items, livecheck.GrokItem{
			ID: r.ID, RefreshToken: refresh, TokenEndpoint: endpoint, ClientID: clientID,
		})
	}
	return items, nil
}

func (h *Handler) loadGrokItem(id uint) (livecheck.GrokItem, bool) {
	var r models.GrokRegistration
	if err := h.DB.Select("id", "auth_data").First(&r, id).Error; err != nil {
		return livecheck.GrokItem{}, false
	}
	if r.AuthData == "" {
		return livecheck.GrokItem{}, false
	}
	refresh, endpoint, clientID := grokRefreshCreds(r.AuthData)
	return livecheck.GrokItem{ID: r.ID, RefreshToken: refresh, TokenEndpoint: endpoint, ClientID: clientID}, true
}

// grokRefreshCreds 从 AuthData.cpa_xai 取出 refresh_token / token 端点 / client_id。
func grokRefreshCreds(authData string) (refresh, endpoint, clientID string) {
	var m map[string]any
	if json.Unmarshal([]byte(authData), &m) != nil {
		return "", "", ""
	}
	cpa, _ := m["cpa_xai"].(map[string]any)
	if cpa == nil {
		return "", "", ""
	}
	refresh, _ = cpa["refresh_token"].(string)
	endpoint, _ = cpa["token_endpoint"].(string)
	clientID, _ = cpa["client_id"].(string)
	return refresh, endpoint, clientID
}

/* ===================== Adobe ===================== */

func (h *Handler) AdobeLiveCheckStart(c *gin.Context) {
	var in liveCheckReq
	_ = c.ShouldBindJSON(&in)
	runner := h.liveRunnerFor("adobe")
	if !runner.tryStart() {
		c.JSON(http.StatusConflict, gin.H{"error": "已有测活任务进行中"})
		return
	}
	items, err := h.loadAdobeItems(in.IDs)
	if err != nil {
		runner.finish("加载账号失败")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if len(items) == 0 {
		runner.finish("没有可测活的账号")
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有可测活的 Adobe 账号（需已注册且有会话数据）"})
		return
	}
	runner.setTotal(len(items))
	go func() {
		defer runner.finish("")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		livecheck.CheckAdobe(ctx, items, func(chunk map[uint]string) {
			for id, st := range chunk {
				h.applyAdobeAlive(id, st)
				runner.tally(st)
			}
		})
	}()
	c.JSON(http.StatusOK, gin.H{"ok": true, "total": len(items)})
}

func (h *Handler) AdobeLiveCheckStatus(c *gin.Context) {
	c.JSON(http.StatusOK, h.liveRunnerFor("adobe").snapshot())
}

func (h *Handler) AdobeLiveCheckOne(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	item, ok := h.loadAdobeItem(uint(id64))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该 Adobe 账号没有可测活的会话数据"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	res := livecheck.CheckAdobe(ctx, []livecheck.AdobeItem{item}, nil)
	st := res[item.ID]
	if st == "" {
		st = livecheck.StatusUnknown
	}
	now := h.applyAdobeAlive(item.ID, st)
	c.JSON(http.StatusOK, gin.H{"id": item.ID, "alive": st, "checked_at": now})
}

func (h *Handler) applyAdobeAlive(id uint, st string) time.Time {
	now := time.Now()
	h.DB.Model(&models.AdobeRegistration{}).Where("id = ?", id).
		Updates(map[string]any{"alive": st, "alive_checked_at": now})
	return now
}

func (h *Handler) loadAdobeItems(ids []uint) ([]livecheck.AdobeItem, error) {
	var regs []models.AdobeRegistration
	q := h.DB.Select("id", "auth_data").Where("auth_data <> ''")
	if len(ids) > 0 {
		q = q.Where("id IN ?", ids)
	} else {
		q = q.Where("status = ?", "registered")
	}
	if err := q.Find(&regs).Error; err != nil {
		return nil, err
	}
	items := make([]livecheck.AdobeItem, 0, len(regs))
	for _, r := range regs {
		items = append(items, livecheck.AdobeItem{ID: r.ID, Cookies: adobeLiveCookies(r.AuthData)})
	}
	return items, nil
}

func (h *Handler) loadAdobeItem(id uint) (livecheck.AdobeItem, bool) {
	var r models.AdobeRegistration
	if err := h.DB.Select("id", "auth_data").First(&r, id).Error; err != nil {
		return livecheck.AdobeItem{}, false
	}
	if r.AuthData == "" {
		return livecheck.AdobeItem{}, false
	}
	return livecheck.AdobeItem{ID: r.ID, Cookies: adobeLiveCookies(r.AuthData)}, true
}

// adobeLiveCookies 把 AuthData 里的 Cookie 列表转成测活所需结构。
func adobeLiveCookies(authData string) []livecheck.AdobeCookie {
	raw := adobeCookies(authData)
	out := make([]livecheck.AdobeCookie, 0, len(raw))
	for _, m := range raw {
		name, _ := m["name"].(string)
		if name == "" {
			continue
		}
		out = append(out, livecheck.AdobeCookie{
			Name:     name,
			Value:    asString(m["value"]),
			Domain:   asString(m["domain"]),
			Path:     asString(m["path"]),
			Secure:   asBool(m["secure"]),
			HTTPOnly: asBool(m["httpOnly"]),
			Expires:  asFloat(m["expires"]),
			SameSite: asString(m["sameSite"]),
		})
	}
	return out
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}

func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return 0
	}
}
