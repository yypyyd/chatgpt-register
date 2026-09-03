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

type luminaStartInput struct {
	Email string `json:"email" binding:"required"`
	Note  string `json:"note"`
}

type luminaProduceInput struct {
	Count int `json:"count" binding:"required"`
}

type luminaCodeInput struct {
	Code string `json:"code" binding:"required"`
}

func (h *Handler) LuminaList(c *gin.Context) {
	var regs []models.LuminaRegistration
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
	q.Model(&models.LuminaRegistration{}).Count(&total)
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

func (h *Handler) LuminaStart(c *gin.Context) {
	var in luminaStartInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if h.Browser == nil || !h.Browser.Ready() {
		c.JSON(http.StatusConflict, gin.H{"error": "缺少浏览器，无法注册：浏览器正在下载或下载失败"})
		return
	}
	reg, err := h.LuminaProducer.Start(in.Email, in.Note)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, reg)
}

func (h *Handler) LuminaProduce(c *gin.Context) {
	var in luminaProduceInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if h.Browser == nil || !h.Browser.Ready() {
		c.JSON(http.StatusConflict, gin.H{"error": "缺少浏览器，无法注册：浏览器正在下载或下载失败"})
		return
	}
	regs, err := h.LuminaProducer.StartFromAccounts(in.Count)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "started": len(regs), "data": regs})
}

// LuminaProduceStatus 返回 Lumina 生产进度（待生产/在跑/已注册/失败）。
func (h *Handler) LuminaProduceStatus(c *gin.Context) {
	c.JSON(http.StatusOK, h.LuminaProducer.Snapshot())
}

// LuminaProduceStop 停止所有在跑的 Lumina 注册任务。
func (h *Handler) LuminaProduceStop(c *gin.Context) {
	h.LuminaProducer.StopAll()
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) LuminaSubmitCode(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	var in luminaCodeInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.LuminaProducer.SubmitCode(uint(id64), in.Code); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) LuminaStop(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	h.LuminaProducer.Stop(uint(id64))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) LuminaDelete(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	h.LuminaProducer.Stop(uint(id64))
	if err := h.DB.Delete(&models.LuminaRegistration{}, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) LuminaDeleteAll(c *gin.Context) {
	var regs []models.LuminaRegistration
	h.DB.Select("id").Where("status IN ?", []string{"registering", "waiting_code"}).Find(&regs)
	for _, reg := range regs {
		h.LuminaProducer.Stop(reg.ID)
	}
	r := h.DB.Where("1 = 1").Delete(&models.LuminaRegistration{})
	if r.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": r.Error.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": r.RowsAffected})
}

func (h *Handler) LuminaLog(c *gin.Context) {
	var reg models.LuminaRegistration
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

func (h *Handler) LuminaShot(c *gin.Context) {
	var reg models.LuminaRegistration
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

// LuminaDownload 导出选中 Lumina 账号给 2API，仅对已注册且有会话数据的记录开放。
// 请求体：{ "ids": [1,2,3], "format": "string|json|array", "unshipped_only": false }。
// unshipped_only=true 时忽略 ids，导出全部已注册且未出库的账号。
//   - string：Cookie 字符串 k=v; k=v; ...（单账号 .txt，多账号 .zip）
//   - json  ：单个账号的 {email, password, cookie_string}（单账号 .json，多账号 .zip）
//   - array ：多个账号的 {email, password, cookie_string} 数组，始终单个 .json 文件
func (h *Handler) LuminaDownload(c *gin.Context) {
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "未选择 Lumina 账号"})
		return
	}
	if in.Format == "" {
		in.Format = "string"
	}
	if in.Format != "string" && in.Format != "json" && in.Format != "array" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不支持的导出格式"})
		return
	}

	var regs []models.LuminaRegistration
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
			c.JSON(http.StatusBadRequest, gin.H{"error": "没有已注册未出库的 Lumina 账号"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "所选 Lumina 账号没有可下载的会话数据"})
		return
	}

	ids := make([]uint, 0, len(regs))
	for _, r := range regs {
		ids = append(ids, r.ID)
	}
	// 导出即出库。
	h.DB.Model(&models.LuminaRegistration{}).Where("id IN ?", ids).Update("shipped", true)

	stamp := time.Now().UTC().Format("20060102_150405")
	switch in.Format {
	case "string":
		if len(regs) == 1 {
			c.Header("Content-Disposition", `attachment; filename="lumina-`+safeFileName(regs[0].Email)+`.txt"`)
			c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(luminaCookieString(regs[0].AuthData)))
			return
		}
		h.luminaZipEntries(c, "lumina_cookies_"+stamp+".zip", regs, func(r models.LuminaRegistration) (string, []byte) {
			return "lumina-" + safeFileName(r.Email) + ".txt", []byte(luminaCookieString(r.AuthData))
		})
	case "json":
		if len(regs) == 1 {
			out, _ := json.MarshalIndent(luminaExportObject(regs[0]), "", "  ")
			c.Header("Content-Disposition", `attachment; filename="lumina-`+safeFileName(regs[0].Email)+`.json"`)
			c.Data(http.StatusOK, "application/json; charset=utf-8", out)
			return
		}
		h.luminaZipEntries(c, "lumina_cookies_"+stamp+".zip", regs, func(r models.LuminaRegistration) (string, []byte) {
			out, _ := json.MarshalIndent(luminaExportObject(r), "", "  ")
			return "lumina-" + safeFileName(r.Email) + ".json", out
		})
	case "array":
		arr := make([]map[string]any, 0, len(regs))
		for _, r := range regs {
			arr = append(arr, luminaExportObject(r))
		}
		out, _ := json.MarshalIndent(arr, "", "  ")
		c.Header("Content-Disposition", `attachment; filename="lumina_cookies_array_`+stamp+`.json"`)
		c.Data(http.StatusOK, "application/json; charset=utf-8", out)
	}
}

func (h *Handler) luminaZipEntries(c *gin.Context, name string, regs []models.LuminaRegistration, build func(models.LuminaRegistration) (string, []byte)) {
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

// luminaCookies 从 AuthData 里解析出 Cookie 列表（含元数据）。
func luminaCookies(authData string) []map[string]any {
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

// luminaCookieString 拼成浏览器 Cookie 头字符串：k=v; k=v; ...
func luminaCookieString(authData string) string {
	var parts []string
	for _, ck := range luminaCookies(authData) {
		name, _ := ck["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		value, _ := ck["value"].(string)
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, "; ")
}

// luminaExportObject 构造单个 Lumina 账号的导出对象（JSON 对象 / 数组元素）。
// 2API 只认这三个字段：email 是登录账号，password 供其到期时自行重登续期，
// cookie_string 是登录成功后的完整 Cookie（含 csrfToken / digest / AccountID）；
// 到期时间、AccountID、账号资料等它都直接从 Cookie 里读，不要额外带。
func luminaExportObject(r models.LuminaRegistration) map[string]any {
	return map[string]any{
		"email":         r.Email,
		"password":      r.Password,
		"cookie_string": luminaCookieString(r.AuthData),
	}
}
