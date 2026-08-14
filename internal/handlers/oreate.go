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

type oreateStartInput struct {
	Email string `json:"email" binding:"required"`
	Note  string `json:"note"`
}

type oreateProduceInput struct {
	Count int `json:"count" binding:"required"`
}

func (h *Handler) OreateList(c *gin.Context) {
	var regs []models.OreateRegistration
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
	q.Model(&models.OreateRegistration{}).Count(&total)
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

func (h *Handler) OreateStart(c *gin.Context) {
	var in oreateStartInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if h.Browser == nil || !h.Browser.Ready() {
		c.JSON(http.StatusConflict, gin.H{"error": "缺少浏览器，无法注册：浏览器正在下载或下载失败"})
		return
	}
	reg, err := h.OreateProducer.Start(in.Email, in.Note)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, reg)
}

func (h *Handler) OreateProduce(c *gin.Context) {
	var in oreateProduceInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if h.Browser == nil || !h.Browser.Ready() {
		c.JSON(http.StatusConflict, gin.H{"error": "缺少浏览器，无法注册：浏览器正在下载或下载失败"})
		return
	}
	regs, err := h.OreateProducer.StartFromAccounts(in.Count)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "started": len(regs), "data": regs})
}

func (h *Handler) OreateProduceStatus(c *gin.Context) {
	c.JSON(http.StatusOK, h.OreateProducer.Snapshot())
}

func (h *Handler) OreateProduceStop(c *gin.Context) {
	h.OreateProducer.StopAll()
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) OreateStop(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	h.OreateProducer.Stop(uint(id64))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) OreateDelete(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	h.OreateProducer.Stop(uint(id64))
	if err := h.DB.Delete(&models.OreateRegistration{}, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) OreateDeleteAll(c *gin.Context) {
	var regs []models.OreateRegistration
	h.DB.Select("id").Where("status = ?", "registering").Find(&regs)
	for _, reg := range regs {
		h.OreateProducer.Stop(reg.ID)
	}
	r := h.DB.Where("1 = 1").Delete(&models.OreateRegistration{})
	if r.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": r.Error.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": r.RowsAffected})
}

func (h *Handler) OreateLog(c *gin.Context) {
	var reg models.OreateRegistration
	if err := h.DB.First(&reg, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"email": reg.Email, "status": reg.Status,
		"note": reg.Note, "log": reg.Log,
		"points": reg.Points, "image_url": reg.ImageURL,
	})
}

// OreateDownload 导出选中 Oreate 账号，仅对已注册且有会话数据的记录开放。
// 请求体：{ "ids": [1,2,3], "format": "sub2api|json|array", "unshipped_only": false }。
// unshipped_only=true 时忽略 ids，导出全部已注册且未出库的账号。
//   - sub2api：Sub2API 账号备份 JSON（accounts 数组，platform=oreate、type=cookie），可直接被 2api 导入
//   - json   ：单个账号的凭据 JSON（多账号打包 .zip）
//   - array  ：多个账号的凭据数组，始终单个 .json
func (h *Handler) OreateDownload(c *gin.Context) {
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "未选择 Oreate 账号"})
		return
	}
	if in.Format == "" {
		in.Format = "sub2api"
	}
	if in.Format != "sub2api" && in.Format != "json" && in.Format != "array" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不支持的导出格式"})
		return
	}

	var regs []models.OreateRegistration
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
			c.JSON(http.StatusBadRequest, gin.H{"error": "没有已注册未出库的 Oreate 账号"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "所选 Oreate 账号没有可下载的会话数据"})
		return
	}

	ids := make([]uint, 0, len(regs))
	for _, r := range regs {
		ids = append(ids, r.ID)
	}
	// 导出即出库。
	h.DB.Model(&models.OreateRegistration{}).Where("id IN ?", ids).Update("shipped", true)

	stamp := time.Now().UTC().Format("20060102_150405")
	switch in.Format {
	case "sub2api":
		accounts := make([]exportAccount, 0, len(regs))
		for _, r := range regs {
			accounts = append(accounts, exportAccount{
				Name:        r.Email,
				Platform:    "oreate",
				Type:        "cookie",
				Credentials: oreateCredentials(r),
			})
		}
		out, _ := json.MarshalIndent(exportBundle{
			ExportedAt: time.Now().UTC().Format(time.RFC3339),
			Proxies:    []any{},
			Accounts:   accounts,
		}, "", "  ")
		c.Header("Content-Disposition", `attachment; filename="oreate-sub2api.json"`)
		c.Data(http.StatusOK, "application/json; charset=utf-8", out)
	case "json":
		if len(regs) == 1 {
			out, _ := json.MarshalIndent(oreateCredentials(regs[0]), "", "  ")
			c.Header("Content-Disposition", `attachment; filename="oreate-`+safeFileName(regs[0].Email)+`.json"`)
			c.Data(http.StatusOK, "application/json; charset=utf-8", out)
			return
		}
		h.oreateZipEntries(c, "oreate_accounts_"+stamp+".zip", regs)
	case "array":
		arr := make([]map[string]any, 0, len(regs))
		for _, r := range regs {
			arr = append(arr, oreateCredentials(r))
		}
		out, _ := json.MarshalIndent(arr, "", "  ")
		c.Header("Content-Disposition", `attachment; filename="oreate_accounts_array_`+stamp+`.json"`)
		c.Data(http.StatusOK, "application/json; charset=utf-8", out)
	}
}

func (h *Handler) oreateZipEntries(c *gin.Context, name string, regs []models.OreateRegistration) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, r := range regs {
		entry, err := zw.Create("oreate-" + safeFileName(r.Email) + ".json")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "创建压缩包失败"})
			return
		}
		out, _ := json.MarshalIndent(oreateCredentials(r), "", "  ")
		if _, err = entry.Write(out); err != nil {
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

// oreateCredentials 把库里存的会话数据映射成 2api 可用的凭据：
// Cookie 头字符串 + ouss 会话票据 + 设备标识/UA（站点会校验设备一致性）。
func oreateCredentials(r models.OreateRegistration) map[string]any {
	var auth struct {
		Cookies     map[string]string `json:"cookies"`
		OUID        string            `json:"ouid"`
		UserAgent   string            `json:"user_agent"`
		Points      int               `json:"points"`
		PointDetail map[string]int    `json:"point_detail"`
		ImageURL    string            `json:"image_url"`
		ImageModel  string            `json:"image_model"`
		Password    string            `json:"password"`
		CapturedAt  string            `json:"captured_at"`
	}
	_ = json.Unmarshal([]byte(r.AuthData), &auth)

	parts := make([]string, 0, len(auth.Cookies))
	for _, name := range []string{"OUID", "ouss"} {
		if v := auth.Cookies[name]; v != "" {
			parts = append(parts, name+"="+v)
		}
	}
	for name, v := range auth.Cookies {
		if name == "OUID" || name == "ouss" {
			continue
		}
		parts = append(parts, name+"="+v)
	}

	password := auth.Password
	if password == "" {
		password = r.Password
	}
	return map[string]any{
		"email":        r.Email,
		"password":     password,
		"cookie":       strings.Join(parts, "; "),
		"cookies":      auth.Cookies,
		"ouss":         auth.Cookies["ouss"],
		"ouid":         auth.OUID,
		"user_agent":   auth.UserAgent,
		"base_url":     "https://www.oreateai.com",
		"points":       auth.Points,
		"point_detail": auth.PointDetail,
		"image_model":  auth.ImageModel,
		"last_image":   auth.ImageURL,
		"captured_at":  auth.CapturedAt,
	}
}
