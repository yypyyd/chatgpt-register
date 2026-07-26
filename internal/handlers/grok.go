package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"chatgpt-register/internal/models"

	"github.com/gin-gonic/gin"
)

type grokStartInput struct {
	Email string `json:"email" binding:"required"`
	Note  string `json:"note"`
}

type grokProduceInput struct {
	Count int `json:"count" binding:"required"`
}

type grokCodeInput struct {
	Code string `json:"code" binding:"required"`
}

func validGrokStatus(s string) bool {
	return s == "" || s == "pending" || s == "registering" ||
		s == "waiting_code" || s == "registered" || s == "register_failed"
}

func (h *Handler) GrokList(c *gin.Context) {
	var regs []models.GrokRegistration
	q := h.DB.Order("created_at desc, id desc")
	if s := c.Query("status"); s != "" {
		q = q.Where("status = ?", s)
	}
	if kw := c.Query("q"); kw != "" {
		like := "%" + kw + "%"
		q = q.Where("email LIKE ? OR note LIKE ?", like, like)
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("size", "20"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 20
	}

	var total int64
	q.Model(&models.GrokRegistration{}).Count(&total)
	if err := q.Offset((page - 1) * size).Limit(size).Find(&regs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	for i := range regs {
		regs[i].AuthData = ""
		regs[i].Log = ""
	}
	c.JSON(http.StatusOK, gin.H{"data": regs, "total": total, "page": page, "size": size})
}

func (h *Handler) GrokStart(c *gin.Context) {
	var in grokStartInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if h.Browser == nil || !h.Browser.Ready() {
		c.JSON(http.StatusConflict, gin.H{"error": "缺少浏览器，无法注册：浏览器正在下载或下载失败"})
		return
	}
	reg, err := h.GrokProducer.Start(in.Email, in.Note)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, reg)
}

func (h *Handler) GrokProduce(c *gin.Context) {
	var in grokProduceInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if h.Browser == nil || !h.Browser.Ready() {
		c.JSON(http.StatusConflict, gin.H{"error": "缺少浏览器，无法注册：浏览器正在下载或下载失败"})
		return
	}
	regs, err := h.GrokProducer.StartFromAccounts(in.Count)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "started": len(regs), "data": regs})
}

func (h *Handler) GrokSubmitCode(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	var in grokCodeInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.GrokProducer.SubmitCode(uint(id64), in.Code); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) GrokStop(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	h.GrokProducer.Stop(uint(id64))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) GrokDelete(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	h.GrokProducer.Stop(uint(id64))
	if err := h.DB.Delete(&models.GrokRegistration{}, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) GrokDeleteAll(c *gin.Context) {
	var regs []models.GrokRegistration
	h.DB.Select("id").Where("status IN ?", []string{"registering", "waiting_code"}).Find(&regs)
	for _, reg := range regs {
		h.GrokProducer.Stop(reg.ID)
	}
	r := h.DB.Where("1 = 1").Delete(&models.GrokRegistration{})
	if r.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": r.Error.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": r.RowsAffected})
}

func (h *Handler) GrokLog(c *gin.Context) {
	var reg models.GrokRegistration
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

func (h *Handler) GrokShot(c *gin.Context) {
	var reg models.GrokRegistration
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

func (h *Handler) GrokDownload(c *gin.Context) {
	var in struct {
		IDs []uint `json:"ids"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(in.IDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "未选择 Grok 账号"})
		return
	}
	var regs []models.GrokRegistration
	if err := h.DB.Where("id IN ? AND status = ? AND auth_data <> ''", in.IDs, "registered").Find(&regs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if len(regs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "所选 Grok 账号没有可下载的会话数据"})
		return
	}
	ids := make([]uint, 0, len(regs))
	accounts := make([]map[string]any, 0, len(regs))
	for _, r := range regs {
		ids = append(ids, r.ID)
		var auth map[string]any
		_ = json.Unmarshal([]byte(r.AuthData), &auth)
		accounts = append(accounts, map[string]any{
			"name":        r.Email,
			"platform":    "grok",
			"type":        "browser_session",
			"credentials": auth,
		})
	}
	h.DB.Model(&models.GrokRegistration{}).Where("id IN ?", ids).Update("shipped", true)

	out, _ := json.MarshalIndent(map[string]any{
		"exported_at": time.Now().UTC().Format(time.RFC3339),
		"platform":    "grok",
		"accounts":    accounts,
	}, "", "  ")
	name := "grok_auth.json"
	if len(regs) == 1 {
		name = "grok-" + safeFileName(regs[0].Email) + ".json"
	}
	c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
	c.Data(http.StatusOK, "application/json; charset=utf-8", out)
}

func safeFileName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "account"
	}
	repl := func(r rune) rune {
		if r < 32 {
			return '_'
		}
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		default:
			return r
		}
	}
	return strings.Map(repl, s)
}
