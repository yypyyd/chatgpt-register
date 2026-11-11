package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"

	"github.com/gin-gonic/gin"
)

type gmailStartInput struct {
	Username string `json:"username" binding:"required"`
	Note     string `json:"note"`
}

type gmailProduceInput struct {
	Count int `json:"count" binding:"required"`
}

func (h *Handler) GmailList(c *gin.Context) {
	var regs []models.GmailRegistration
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
	q.Model(&models.GmailRegistration{}).Count(&total)
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

func (h *Handler) GmailStart(c *gin.Context) {
	var in gmailStartInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	reg, err := h.GmailProducer.Start(in.Username, in.Note)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, reg)
}

func (h *Handler) GmailProduce(c *gin.Context) {
	var in gmailProduceInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	regs, err := h.GmailProducer.StartBatch(in.Count)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "started": len(regs), "data": regs})
}

func (h *Handler) GmailProduceStatus(c *gin.Context) {
	c.JSON(http.StatusOK, h.GmailProducer.Snapshot())
}

func (h *Handler) GmailProduceStop(c *gin.Context) {
	h.GmailProducer.StopAll()
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) GmailStop(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	h.GmailProducer.Stop(uint(id64))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) GmailDelete(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	h.GmailProducer.Stop(uint(id64))
	if err := h.DB.Delete(&models.GmailRegistration{}, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) GmailDeleteAll(c *gin.Context) {
	var regs []models.GmailRegistration
	h.DB.Select("id").Where("status = ?", "registering").Find(&regs)
	for _, reg := range regs {
		h.GmailProducer.Stop(reg.ID)
	}
	r := h.DB.Where("1 = 1").Delete(&models.GmailRegistration{})
	if r.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": r.Error.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": r.RowsAffected})
}

func (h *Handler) GmailLog(c *gin.Context) {
	var reg models.GmailRegistration
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

func (h *Handler) GmailShot(c *gin.Context) {
	var reg models.GmailRegistration
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

// gmailMailAccount 构造一个走 Gmail IMAP（账密登录）的收信账号。
// 注册产出的 Google 账号没有 OAuth client_id/refresh_token，用邮箱+密码走 imap.gmail.com。
func gmailMailAccount(reg models.GmailRegistration) mailfetch.Account {
	return mailfetch.Account{
		Email:    reg.Email,
		Password: reg.Password,
		Provider: "gmail",
	}
}

// GmailMessages 取件：用注册得到的邮箱+密码走 Gmail IMAP 拉收件箱最新邮件。
// 注意：Google 账号需开启 IMAP 且允许账密登录，否则会返回鉴权失败。
func (h *Handler) GmailMessages(c *gin.Context) {
	var reg models.GmailRegistration
	if err := h.DB.First(&reg, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Gmail 账号不存在"})
		return
	}
	if strings.TrimSpace(reg.Password) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该账号没有密码，无法取件", "email": reg.Email})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	msgs, err := h.Mail.ListMessages(c.Request.Context(), gmailMailAccount(reg), limit)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, mailfetch.ErrMissingCreds) || errors.Is(err, mailfetch.ErrAuthFailed) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error(), "email": reg.Email})
		return
	}
	c.JSON(http.StatusOK, gin.H{"email": reg.Email, "items": msgs})
}

// GmailMessage 按消息 ID 拉取单封邮件完整正文。
func (h *Handler) GmailMessage(c *gin.Context) {
	var reg models.GmailRegistration
	if err := h.DB.First(&reg, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Gmail 账号不存在"})
		return
	}
	msg, err := h.Mail.GetMessage(c.Request.Context(), gmailMailAccount(reg), c.Query("mid"))
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, mailfetch.ErrMissingCreds) || errors.Is(err, mailfetch.ErrAuthFailed) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error(), "email": reg.Email})
		return
	}
	c.JSON(http.StatusOK, msg)
}

// GmailToMailbox 把已注册的 Gmail 账号加入「邮箱管理」池，供其它渠道注册时读验证码。
func (h *Handler) GmailToMailbox(c *gin.Context) {
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
	var regs []models.GmailRegistration
	if err := h.DB.Where("id IN ? AND status = ? AND password <> ''", in.IDs, "registered").Find(&regs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	added, skipped := 0, 0
	for _, reg := range regs {
		var count int64
		h.DB.Model(&models.Mailbox{}).Where("email = ?", reg.Email).Count(&count)
		if count > 0 {
			skipped++
			continue
		}
		mb := models.Mailbox{
			Email:    reg.Email,
			Password: reg.Password,
			Provider: "gmail",
			Status:   "unverified",
			Note:     "来源: Gmail 注册",
		}
		if err := h.DB.Create(&mb).Error; err != nil {
			skipped++
			continue
		}
		added++
	}
	if added > 0 {
		h.MailboxVerifier.Wake()
	}
	c.JSON(http.StatusOK, gin.H{"added": added, "skipped": skipped})
}

// GmailDownload 导出 Gmail 账号凭据（email + password），导出即出库。
func (h *Handler) GmailDownload(c *gin.Context) {
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "未选择 Gmail 账号"})
		return
	}
	if in.Format == "" {
		in.Format = "text"
	}
	if in.Format != "text" && in.Format != "json" && in.Format != "array" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不支持的导出格式"})
		return
	}

	var regs []models.GmailRegistration
	q := h.DB.Where("status = ? AND password <> ''", "registered")
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
			c.JSON(http.StatusBadRequest, gin.H{"error": "没有已注册未出库的 Gmail 账号"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "所选 Gmail 账号没有可下载的凭据"})
		return
	}

	ids := make([]uint, 0, len(regs))
	for _, r := range regs {
		ids = append(ids, r.ID)
	}
	h.DB.Model(&models.GmailRegistration{}).Where("id IN ?", ids).Update("shipped", true)

	stamp := time.Now().UTC().Format("20060102_150405")
	switch in.Format {
	case "text":
		if len(regs) == 1 {
			c.Header("Content-Disposition", `attachment; filename="gmail-`+safeFileName(regs[0].Email)+`.txt"`)
			c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(gmailCredLine(regs[0])))
			return
		}
		var lines []string
		for _, r := range regs {
			lines = append(lines, gmailCredLine(r))
		}
		c.Header("Content-Disposition", `attachment; filename="gmail_accounts_`+stamp+`.txt"`)
		c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(strings.Join(lines, "\n")))
	case "json":
		if len(regs) == 1 {
			out, _ := json.MarshalIndent(gmailExportObject(regs[0]), "", "  ")
			c.Header("Content-Disposition", `attachment; filename="gmail-`+safeFileName(regs[0].Email)+`.json"`)
			c.Data(http.StatusOK, "application/json; charset=utf-8", out)
			return
		}
		h.gmailZipEntries(c, "gmail_accounts_"+stamp+".zip", regs, func(r models.GmailRegistration) (string, []byte) {
			out, _ := json.MarshalIndent(gmailExportObject(r), "", "  ")
			return "gmail-" + safeFileName(r.Email) + ".json", out
		})
	case "array":
		arr := make([]map[string]any, 0, len(regs))
		for _, r := range regs {
			arr = append(arr, gmailExportObject(r))
		}
		out, _ := json.MarshalIndent(arr, "", "  ")
		c.Header("Content-Disposition", `attachment; filename="gmail_accounts_array_`+stamp+`.json"`)
		c.Data(http.StatusOK, "application/json; charset=utf-8", out)
	}
}

func (h *Handler) gmailZipEntries(c *gin.Context, name string, regs []models.GmailRegistration, build func(models.GmailRegistration) (string, []byte)) {
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

// gmailCredLine 生成 email----password 格式的凭据行。
func gmailCredLine(r models.GmailRegistration) string {
	return r.Email + "----" + r.Password
}

func gmailExportObject(r models.GmailRegistration) map[string]any {
	obj := map[string]any{
		"email":    r.Email,
		"password": r.Password,
		"platform": "gmail",
	}
	if r.AuthData != "" {
		var auth map[string]any
		if json.Unmarshal([]byte(r.AuthData), &auth) == nil {
			for _, key := range []string{"captured_at", "proxy", "cookies", "first_name", "last_name"} {
				if v, ok := auth[key]; ok {
					obj[key] = v
				}
			}
		}
	}
	return obj
}
