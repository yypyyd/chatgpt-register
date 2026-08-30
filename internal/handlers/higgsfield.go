package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"chatgpt-register/internal/models"

	"github.com/gin-gonic/gin"
)

type higgsfieldStartInput struct {
	Email string `json:"email" binding:"required"`
	Note  string `json:"note"`
}

type higgsfieldProduceInput struct {
	Count int `json:"count" binding:"required"`
}

type higgsfieldCodeInput struct {
	Code string `json:"code" binding:"required"`
}

func (h *Handler) HiggsfieldList(c *gin.Context) {
	var regs []models.HiggsfieldRegistration
	q := h.DB.Order("updated_at desc, id desc")
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
	q.Model(&models.HiggsfieldRegistration{}).Count(&total)
	if err := q.Offset((page - 1) * size).Limit(size).Find(&regs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// 列表不返回敏感数据：AuthData(json:"-") 与日志一律清空。
	for i := range regs {
		regs[i].AuthData = ""
		regs[i].Log = ""
	}
	c.JSON(http.StatusOK, gin.H{"data": regs, "total": total, "page": page, "size": size})
}

func (h *Handler) HiggsfieldStart(c *gin.Context) {
	var in higgsfieldStartInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	reg, err := h.HiggsfieldProducer.Start(in.Email, in.Note)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, reg)
}

func (h *Handler) HiggsfieldProduce(c *gin.Context) {
	var in higgsfieldProduceInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	regs, err := h.HiggsfieldProducer.StartFromAccounts(in.Count)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "started": len(regs), "data": regs})
}

func (h *Handler) HiggsfieldProduceStatus(c *gin.Context) {
	c.JSON(http.StatusOK, h.HiggsfieldProducer.Snapshot())
}

func (h *Handler) HiggsfieldProduceStop(c *gin.Context) {
	h.HiggsfieldProducer.StopAll()
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) HiggsfieldSubmitCode(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	var in higgsfieldCodeInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.HiggsfieldProducer.SubmitCode(uint(id64), in.Code); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) HiggsfieldStop(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	h.HiggsfieldProducer.Stop(uint(id64))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) HiggsfieldDelete(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	h.HiggsfieldProducer.Stop(uint(id64))
	if err := h.DB.Delete(&models.HiggsfieldRegistration{}, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) HiggsfieldDeleteAll(c *gin.Context) {
	var regs []models.HiggsfieldRegistration
	h.DB.Select("id").Where("status IN ?", []string{"registering", "waiting_code"}).Find(&regs)
	for _, reg := range regs {
		h.HiggsfieldProducer.Stop(reg.ID)
	}
	r := h.DB.Where("1 = 1").Delete(&models.HiggsfieldRegistration{})
	if r.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": r.Error.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": r.RowsAffected})
}

func (h *Handler) HiggsfieldLog(c *gin.Context) {
	var reg models.HiggsfieldRegistration
	if err := h.DB.First(&reg, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"email": reg.Email, "status": reg.Status,
		"note": reg.Note, "log": reg.Log,
		"trial_status": reg.TrialStatus, "checkout_url": reg.CheckoutURL,
	})
}

// HiggsfieldTrial 对已注册账号跑 pricing 页的绑卡优惠：查资格并开出 Stripe 收银台，
// 停在需要填真实银行卡那一步；本接口不提交任何卡信息。
func (h *Handler) HiggsfieldTrial(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	res, err := h.HiggsfieldProducer.RunTrial(uint(id64))
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, res)
}

// HiggsfieldDownload 导出选中 Higgsfield 账号的会话数据（Clerk cookie + 会话 token）。
// 请求体：{ "ids": [1,2,3], "format": "string|json|array", "unshipped_only": false }。
func (h *Handler) HiggsfieldDownload(c *gin.Context) {
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "未选择 Higgsfield 账号"})
		return
	}
	if in.Format == "" {
		in.Format = "string"
	}
	if in.Format != "string" && in.Format != "json" && in.Format != "array" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不支持的导出格式"})
		return
	}

	var regs []models.HiggsfieldRegistration
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
			c.JSON(http.StatusBadRequest, gin.H{"error": "没有已注册未出库的 Higgsfield 账号"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "所选 Higgsfield 账号没有可下载的会话数据"})
		return
	}

	ids := make([]uint, 0, len(regs))
	for _, r := range regs {
		ids = append(ids, r.ID)
	}
	// 导出即出库。
	h.DB.Model(&models.HiggsfieldRegistration{}).Where("id IN ?", ids).Update("shipped", true)

	stamp := time.Now().UTC().Format("20060102_150405")
	switch in.Format {
	case "string":
		if len(regs) == 1 {
			c.Header("Content-Disposition", `attachment; filename="higgsfield-`+safeFileName(regs[0].Email)+`.txt"`)
			c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(higgsfieldCookieString(regs[0].AuthData)))
			return
		}
		h.higgsfieldZipEntries(c, "higgsfield_cookies_"+stamp+".zip", regs, func(r models.HiggsfieldRegistration) (string, []byte) {
			return "higgsfield-" + safeFileName(r.Email) + ".txt", []byte(higgsfieldCookieString(r.AuthData))
		})
	case "json":
		if len(regs) == 1 {
			out, _ := json.MarshalIndent(higgsfieldExportObject(regs[0]), "", "  ")
			c.Header("Content-Disposition", `attachment; filename="higgsfield-`+safeFileName(regs[0].Email)+`.json"`)
			c.Data(http.StatusOK, "application/json; charset=utf-8", out)
			return
		}
		h.higgsfieldZipEntries(c, "higgsfield_cookies_"+stamp+".zip", regs, func(r models.HiggsfieldRegistration) (string, []byte) {
			out, _ := json.MarshalIndent(higgsfieldExportObject(r), "", "  ")
			return "higgsfield-" + safeFileName(r.Email) + ".json", out
		})
	case "array":
		arr := make([]map[string]any, 0, len(regs))
		for _, r := range regs {
			arr = append(arr, higgsfieldExportObject(r))
		}
		out, _ := json.MarshalIndent(arr, "", "  ")
		c.Header("Content-Disposition", `attachment; filename="higgsfield_cookies_array_`+stamp+`.json"`)
		c.Data(http.StatusOK, "application/json; charset=utf-8", out)
	}
}

func (h *Handler) higgsfieldZipEntries(c *gin.Context, name string, regs []models.HiggsfieldRegistration, build func(models.HiggsfieldRegistration) (string, []byte)) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, r := range regs {
		fname, data := build(r)
		entry, err := zw.Create(fname)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "创建压缩包失败"})
			return
		}
		if _, err = entry.Write(data); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "写入压缩包失败"})
			return
		}
	}
	if err := zw.Close(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "完成压缩包失败"})
		return
	}
	c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
	c.Data(http.StatusOK, "application/zip", buf.Bytes())
}

func higgsfieldCookies(authData string) []map[string]any {
	var auth map[string]any
	_ = json.Unmarshal([]byte(authData), &auth)
	raw, _ := auth["cookies"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func higgsfieldCookieString(authData string) string {
	var parts []string
	for _, ck := range higgsfieldCookies(authData) {
		name, _ := ck["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		value, _ := ck["value"].(string)
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, "; ")
}

func higgsfieldExportObject(r models.HiggsfieldRegistration) map[string]any {
	var auth map[string]any
	_ = json.Unmarshal([]byte(r.AuthData), &auth)

	cookies := higgsfieldCookies(r.AuthData)
	cookieMap := map[string]string{}
	for _, ck := range cookies {
		name, _ := ck["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		value, _ := ck["value"].(string)
		cookieMap[name] = value
	}
	obj := map[string]any{
		"email":         r.Email,
		"platform":      "higgsfield",
		"cookie_string": higgsfieldCookieString(r.AuthData),
		"cookies_map":   cookieMap,
		"cookies":       cookies,
		"trial_status":  r.TrialStatus,
	}
	for _, k := range []string{"password", "user_id", "session_id", "session_token", "captured_at"} {
		if v, ok := auth[k]; ok {
			obj[k] = v
		}
	}
	return obj
}
