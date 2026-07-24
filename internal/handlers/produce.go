package handlers

import (
	"encoding/json"
	"net/http"
	"time"

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

// Download 下载选中账号的 auth.json：单个→对象，多个→数组；下载即标记出库。
// 请求体：{ "ids": [1,2,3] }。
func (h *Handler) Download(c *gin.Context) {
	var in struct {
		IDs []uint `json:"ids"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(in.IDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "未选择账号"})
		return
	}

	var regs []models.Registration
	if err := h.DB.Where("id IN ? AND status = ? AND auth_data <> ''", in.IDs, "registered").
		Find(&regs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if len(regs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "所选账号没有可下载的已注册数据"})
		return
	}

	accounts := make([]exportAccount, 0, len(regs))
	ids := make([]uint, 0, len(regs))
	for _, r := range regs {
		accounts = append(accounts, exportAccount{
			Name:        r.Email,
			Platform:    "openai",
			Type:        "oauth",
			Credentials: buildCredentials(r.AuthData, r.Email),
		})
		ids = append(ids, r.ID)
	}

	// 下载即出库
	h.DB.Model(&models.Registration{}).Where("id IN ?", ids).Update("shipped", true)

	bundle := exportBundle{
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Proxies:    []any{},
		Accounts:   accounts,
	}
	out, _ := json.MarshalIndent(bundle, "", "  ")
	c.Header("Content-Disposition", "attachment; filename=auth.json")
	c.Data(http.StatusOK, "application/json; charset=utf-8", out)
}
