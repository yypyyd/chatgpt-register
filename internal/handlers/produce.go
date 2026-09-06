package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode"

	"chatgpt-register/internal/codexreg"
	"chatgpt-register/internal/models"

	"github.com/gin-gonic/gin"
)

// exportBundle 是导出的顶层结构：一个文件包含多个账号。
type exportBundle struct {
	ExportedAt string          `json:"exported_at"`
	Proxies    []any           `json:"proxies"`
	Accounts   []exportAccount `json:"accounts"`
}

type exportAccount struct {
	Name        string         `json:"name"`
	Platform    string         `json:"platform"`
	Type        string         `json:"type"`
	Credentials map[string]any `json:"credentials"`
}

// buildCPACredentials maps a stored ChatGPT session to CLIProxyAPI's flat
// Codex auth-file format. A session-only registration has no refresh token,
// so the field is kept empty instead of inventing a non-functional value.
func buildCPACredentials(authData, email string, now time.Time) map[string]any {
	var parsed map[string]any
	_ = json.Unmarshal([]byte(authData), &parsed)
	str := func(k string) string { s, _ := parsed[k].(string); return s }

	accessToken := str("access_token")
	em := str("email")
	if em == "" {
		em = email
	}
	expired := tokenExpiryRFC3339(accessToken)

	return map[string]any{
		"type":          "codex",
		"id_token":      str("id_token"),
		"access_token":  accessToken,
		"refresh_token": str("refresh_token"),
		"account_id":    str("account_id"),
		"last_refresh":  now.UTC().Format(time.RFC3339),
		"email":         em,
		"expired":       expired,
	}
}

func tokenExpiryRFC3339(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if err = json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	exp, err := claims.Exp.Int64()
	if err != nil || exp <= 0 {
		return ""
	}
	return time.Unix(exp, 0).UTC().Format(time.RFC3339)
}

func cpaFileName(email string) string {
	safe := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return '_'
		}
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		default:
			return r
		}
	}, strings.TrimSpace(email))
	if safe == "" {
		safe = "account"
	}
	return "codex-" + safe + ".json"
}

func webSessionFileName(email string) string {
	return strings.TrimSuffix(cpaFileName(email), ".json") + "-web-session.json"
}

// buildWebSession exports only the browser-session material needed to restore
// chatgpt.com. Cookie values are intentionally available only through the
// authenticated download endpoint; list and log endpoints never expose them.
func buildWebSession(authData, email string) (map[string]any, bool) {
	var parsed map[string]any
	if json.Unmarshal([]byte(authData), &parsed) != nil {
		return nil, false
	}
	cookies, err := codexreg.WebCookiesFromAuthData(authData)
	if err != nil || len(cookies) == 0 {
		return nil, false
	}
	str := func(k string) string { s, _ := parsed[k].(string); return s }
	em := str("email")
	if em == "" {
		em = email
	}
	return map[string]any{
		"format":               "chatgpt_web_session_v1",
		"url":                  "https://chatgpt.com/",
		"email":                em,
		"user_agent":           str("user_agent"),
		"screen":               parsed["screen"],
		"registered_ip":        str("registered_ip"),
		"registered_country":   str("registered_country"),
		"registered_timezone":  str("registered_timezone"),
		"registered_locale":    str("registered_locale"),
		"registered_languages": str("registered_languages"),
		"proxy":                str("proxy"),
		"cookie_header":        codexreg.WebCookieHeader(cookies),
		"cookies":              cookies,
	}, true
}

// buildCredentials 把库里存的 auth.json（agent_identity 结构）映射成导出用的 credentials。
func buildCredentials(authData, email string) map[string]any {
	var parsed map[string]any
	_ = json.Unmarshal([]byte(authData), &parsed)
	ai, _ := parsed["agent_identity"].(map[string]any)
	if ai != nil {
		str := func(k string) string { s, _ := ai[k].(string); return s }
		planType := str("plan_type")
		if planType == "" {
			planType = "free"
		}
		em := str("email")
		if em == "" {
			em = email
		}
		fedramp, _ := ai["chatgpt_account_is_fedramp"].(bool)
		return map[string]any{
			"agent_private_key":          str("agent_private_key"),
			"agent_runtime_id":           str("agent_runtime_id"),
			"auth_mode":                  "agentIdentity",
			"chatgpt_account_id":         str("account_id"),
			"chatgpt_account_is_fedramp": fedramp,
			"chatgpt_user_id":            str("chatgpt_user_id"),
			"email":                      em,
			"plan_type":                  planType,
		}
	}

	str := func(k string) string { s, _ := parsed[k].(string); return s }
	planType := str("plan_type")
	if planType == "" {
		planType = "free"
	}
	em := str("email")
	if em == "" {
		em = email
	}
	return map[string]any{
		"access_token":       str("access_token"),
		"auth_mode":          "chatgpt",
		"chatgpt_account_id": str("account_id"),
		"chatgpt_user_id":    str("chatgpt_user_id"),
		"email":              em,
		"plan_type":          planType,
	}
}

// Produce 启动一次生产：{ "count": N }。
func (h *Handler) Produce(c *gin.Context) {
	var in struct {
		Count int `json:"count"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if h.Browser == nil || !h.Browser.Ready() {
		c.JSON(http.StatusConflict, gin.H{"error": "缺少浏览器，无法生产：浏览器正在下载或下载失败"})
		return
	}
	if err := h.Producer.Start(in.Count); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ProduceStatus 返回生产进度（待生产/在跑/已注册/失败/日志）。
func (h *Handler) ProduceStatus(c *gin.Context) {
	c.JSON(http.StatusOK, h.Producer.Snapshot())
}

// BrowserStatus 返回 rod 浏览器的下载/就绪状态，供仪表盘展示进度。
func (h *Handler) BrowserStatus(c *gin.Context) {
	if h.Browser == nil {
		c.JSON(http.StatusOK, gin.H{"ready": true, "phase": "ready"})
		return
	}
	c.JSON(http.StatusOK, h.Browser.Snapshot())
}

// ProduceStop 停止生产。
func (h *Handler) ProduceStop(c *gin.Context) {
	h.Producer.Stop()
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// RegistrationLog 返回单个账号的执行日志。
func (h *Handler) RegistrationLog(c *gin.Context) {
	var reg models.Registration
	if err := h.DB.First(&reg, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"email": reg.Email, "status": reg.Status,
		"note": reg.Note, "log": reg.Log,
		"has_shot": len(reg.Shot) > 0,
	})
}

// RegistrationShot 返回单个账号注册失败时保存的页面截图(PNG)。
func (h *Handler) RegistrationShot(c *gin.Context) {
	var reg models.Registration
	if err := h.DB.First(&reg, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if len(reg.Shot) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "暂无异常截图"})
		return
	}
	c.Data(http.StatusOK, "image/png", reg.Shot)
}

// SetShipped 禁止手动切换出库状态。
// 出库状态只能由下载接口自动标记，避免库存状态被人工改乱。
func (h *Handler) SetShipped(c *gin.Context) {
	c.JSON(http.StatusForbidden, gin.H{"error": "出库状态已锁定，只能由下载操作自动更新"})
}

// Download 下载选中账号。默认导出 Sub2API 聚合 JSON；format=cpa 时
// 按 CLIProxyAPI auth-dir 格式导出；format=web 时导出可恢复的 ChatGPT 网页 Cookie 会话。
// 请求体：{ "ids": [1,2,3], "format": "sub2api|cpa|web", "unshipped_only": false }。
// unshipped_only=true 时忽略 ids，导出全部已注册且未出库的账号。
func (h *Handler) Download(c *gin.Context) {
	var in struct {
		IDs           []uint `json:"ids"`
		Format        string `json:"format"`
		UnshippedOnly bool   `json:"unshipped_only"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(in.IDs) == 0 && !in.UnshippedOnly {
		c.JSON(http.StatusBadRequest, gin.H{"error": "未选择账号"})
		return
	}
	if in.Format == "" {
		in.Format = "sub2api"
	}
	if in.Format != "sub2api" && in.Format != "cpa" && in.Format != "web" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不支持的导出格式"})
		return
	}

	var regs []models.Registration
	q := h.DB.Where("status = ? AND auth_data <> ''", "registered")
	if in.UnshippedOnly {
		q = q.Where("shipped = ?", false)
	} else {
		q = q.Where("id IN ?", in.IDs)
	}
	if err := q.Find(&regs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if len(regs) == 0 {
		if in.UnshippedOnly {
			c.JSON(http.StatusBadRequest, gin.H{"error": "没有已注册未出库的账号"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "所选账号没有可下载的已注册数据"})
		return
	}
	if in.Format == "cpa" {
		for _, r := range regs {
			if buildCPACredentials(r.AuthData, r.Email, time.Now())["access_token"] == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "账号 " + r.Email + " 缺少 CPA 所需的 access_token"})
				return
			}
		}
	}
	if in.Format == "web" {
		for _, r := range regs {
			if _, ok := buildWebSession(r.AuthData, r.Email); !ok {
				c.JSON(http.StatusBadRequest, gin.H{"error": "账号 " + r.Email + " 是旧版 token-only 记录，没有可恢复的网页 Cookie"})
				return
			}
		}
	}

	ids := make([]uint, 0, len(regs))
	for _, r := range regs {
		ids = append(ids, r.ID)
	}

	// 下载即出库
	h.DB.Model(&models.Registration{}).Where("id IN ?", ids).Update("shipped", true)

	if in.Format == "cpa" {
		h.downloadCPA(c, regs)
		return
	}
	if in.Format == "web" {
		h.downloadWebSessions(c, regs)
		return
	}

	accounts := make([]exportAccount, 0, len(regs))
	for _, r := range regs {
		accounts = append(accounts, exportAccount{
			Name:        r.Email,
			Platform:    "openai",
			Type:        "oauth",
			Credentials: buildCredentials(r.AuthData, r.Email),
		})
	}

	bundle := exportBundle{
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Proxies:    []any{},
		Accounts:   accounts,
	}
	out, _ := json.MarshalIndent(bundle, "", "  ")
	c.Header("Content-Disposition", "attachment; filename=auth.json")
	c.Data(http.StatusOK, "application/json; charset=utf-8", out)
}

func (h *Handler) downloadWebSessions(c *gin.Context, regs []models.Registration) {
	if len(regs) == 1 {
		payload, _ := buildWebSession(regs[0].AuthData, regs[0].Email)
		out, _ := json.MarshalIndent(payload, "", "  ")
		c.Header("Content-Disposition", `attachment; filename="`+webSessionFileName(regs[0].Email)+`"`)
		c.Data(http.StatusOK, "application/json; charset=utf-8", out)
		return
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, r := range regs {
		entry, err := zw.Create(webSessionFileName(r.Email))
		if err != nil {
			_ = zw.Close()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "创建网页会话压缩包失败"})
			return
		}
		payload, _ := buildWebSession(r.AuthData, r.Email)
		out, _ := json.MarshalIndent(payload, "", "  ")
		if _, err = entry.Write(out); err != nil {
			_ = zw.Close()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "写入网页会话压缩包失败"})
			return
		}
	}
	if err := zw.Close(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "完成网页会话压缩包失败"})
		return
	}
	c.Header("Content-Disposition", `attachment; filename="chatgpt_web_sessions_`+time.Now().UTC().Format("20060102_150405")+`.zip"`)
	c.Data(http.StatusOK, "application/zip", buf.Bytes())
}

func (h *Handler) downloadCPA(c *gin.Context, regs []models.Registration) {
	now := time.Now().UTC()
	if len(regs) == 1 {
		out, _ := json.MarshalIndent(buildCPACredentials(regs[0].AuthData, regs[0].Email, now), "", "  ")
		c.Header("Content-Disposition", `attachment; filename="`+cpaFileName(regs[0].Email)+`"`)
		c.Data(http.StatusOK, "application/json; charset=utf-8", out)
		return
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, r := range regs {
		entry, err := zw.Create(cpaFileName(r.Email))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "创建 CPA 压缩包失败"})
			return
		}
		out, _ := json.MarshalIndent(buildCPACredentials(r.AuthData, r.Email, now), "", "  ")
		if _, err = entry.Write(out); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "写入 CPA 压缩包失败"})
			return
		}
	}
	if err := zw.Close(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "完成 CPA 压缩包失败"})
		return
	}
	c.Header("Content-Disposition", `attachment; filename="cpa_auth_`+time.Now().UTC().Format("20060102_150405")+`.zip"`)
	c.Data(http.StatusOK, "application/zip", buf.Bytes())
}
