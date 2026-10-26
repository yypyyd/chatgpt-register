package handlers

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"chatgpt-register/internal/livecheck"
	"chatgpt-register/internal/models"

	"github.com/gin-gonic/gin"
)

type lovartStartInput struct {
	Email string `json:"email" binding:"required"`
	Note  string `json:"note"`
}

type lovartProduceInput struct {
	Count int `json:"count" binding:"required"`
}

type lovartCodeInput struct {
	Code string `json:"code" binding:"required"`
}

func (h *Handler) LovartList(c *gin.Context) {
	var regs []models.LovartRegistration
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
	q.Model(&models.LovartRegistration{}).Count(&total)
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

func (h *Handler) LovartStart(c *gin.Context) {
	var in lovartStartInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	reg, err := h.LovartProducer.Start(in.Email, in.Note)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, reg)
}

func (h *Handler) LovartProduce(c *gin.Context) {
	var in lovartProduceInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	regs, err := h.LovartProducer.StartFromAccounts(in.Count)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "started": len(regs), "data": regs})
}

// LovartProduceStatus 返回 Lovart 生产进度（待生产/在跑/已注册/失败）。
func (h *Handler) LovartProduceStatus(c *gin.Context) {
	c.JSON(http.StatusOK, h.LovartProducer.Snapshot())
}

// LovartProduceStop 停止所有在跑的 Lovart 注册任务。
func (h *Handler) LovartProduceStop(c *gin.Context) {
	h.LovartProducer.StopAll()
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) LovartSubmitCode(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	var in lovartCodeInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.LovartProducer.SubmitCode(uint(id64), in.Code); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) LovartStop(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	h.LovartProducer.Stop(uint(id64))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) LovartDelete(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	h.LovartProducer.Stop(uint(id64))
	if err := h.DB.Delete(&models.LovartRegistration{}, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) LovartDeleteAll(c *gin.Context) {
	var regs []models.LovartRegistration
	h.DB.Select("id").Where("status IN ?", []string{"registering", "waiting_code"}).Find(&regs)
	for _, reg := range regs {
		h.LovartProducer.Stop(reg.ID)
	}
	r := h.DB.Where("1 = 1").Delete(&models.LovartRegistration{})
	if r.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": r.Error.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": r.RowsAffected})
}

func (h *Handler) LovartLog(c *gin.Context) {
	var reg models.LovartRegistration
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

func (h *Handler) LovartShot(c *gin.Context) {
	var reg models.LovartRegistration
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

// LovartDownload 导出选中 Lovart 账号的登录 token 与站点 Cookie，
// 仅对已注册且有会话数据的记录开放。
// 请求体：{ "ids": [1,2,3], "format": "string|json|array", "unshipped_only": false }。
// unshipped_only=true 时忽略 ids，导出全部已注册且未出库的账号。
//   - string：Cookie 字符串 k=v; k=v; ...（单账号 .txt，多账号 .zip）
//   - json  ：单个 Lovart 的 Cookie JSON 对象（单账号 .json，多账号 .zip）
//   - array ：多个 Lovart 批量的 Cookie 数组，始终单个 .json 文件
func (h *Handler) LovartDownload(c *gin.Context) {
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "未选择 Lovart 账号"})
		return
	}
	if in.Format == "" {
		in.Format = "string"
	}
	if in.Format != "string" && in.Format != "json" && in.Format != "array" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不支持的导出格式"})
		return
	}

	var regs []models.LovartRegistration
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
			c.JSON(http.StatusBadRequest, gin.H{"error": "没有已注册未出库的 Lovart 账号"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "所选 Lovart 账号没有可下载的会话数据"})
		return
	}

	ids := make([]uint, 0, len(regs))
	for _, r := range regs {
		ids = append(ids, r.ID)
	}
	// 导出即出库。
	h.DB.Model(&models.LovartRegistration{}).Where("id IN ?", ids).Update("shipped", true)

	stamp := time.Now().UTC().Format("20060102_150405")
	switch in.Format {
	case "string":
		if len(regs) == 1 {
			c.Header("Content-Disposition", `attachment; filename="lovart-`+safeFileName(regs[0].Email)+`.txt"`)
			c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(lovartCookieString(regs[0].AuthData)))
			return
		}
		h.lovartZipEntries(c, "lovart_cookies_"+stamp+".zip", regs, func(r models.LovartRegistration) (string, []byte) {
			return "lovart-" + safeFileName(r.Email) + ".txt", []byte(lovartCookieString(r.AuthData))
		})
	case "json":
		if len(regs) == 1 {
			out, _ := json.MarshalIndent(lovartExportObject(regs[0]), "", "  ")
			c.Header("Content-Disposition", `attachment; filename="lovart-`+safeFileName(regs[0].Email)+`.json"`)
			c.Data(http.StatusOK, "application/json; charset=utf-8", out)
			return
		}
		h.lovartZipEntries(c, "lovart_cookies_"+stamp+".zip", regs, func(r models.LovartRegistration) (string, []byte) {
			out, _ := json.MarshalIndent(lovartExportObject(r), "", "  ")
			return "lovart-" + safeFileName(r.Email) + ".json", out
		})
	case "array":
		arr := make([]map[string]any, 0, len(regs))
		for _, r := range regs {
			arr = append(arr, lovartExportObject(r))
		}
		out, _ := json.MarshalIndent(arr, "", "  ")
		c.Header("Content-Disposition", `attachment; filename="lovart_cookies_array_`+stamp+`.json"`)
		c.Data(http.StatusOK, "application/json; charset=utf-8", out)
	}
}

func (h *Handler) lovartZipEntries(c *gin.Context, name string, regs []models.LovartRegistration, build func(models.LovartRegistration) (string, []byte)) {
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

// lovartCookies 从 AuthData 里解析出 Cookie 列表（含元数据）。
func lovartCookies(authData string) []map[string]any {
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

// lovartCookieString 拼成浏览器 Cookie 头字符串：k=v; k=v; ...
func lovartCookieString(authData string) string {
	var parts []string
	for _, ck := range lovartCookies(authData) {
		name, _ := ck["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		value, _ := ck["value"].(string)
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, "; ")
}

// lovartExportObject 构造单个 Lovart 账号的导出对象（Cookie JSON 对象 / 数组元素）。
func lovartExportObject(r models.LovartRegistration) map[string]any {
	var auth map[string]any
	_ = json.Unmarshal([]byte(r.AuthData), &auth)

	cookies := lovartCookies(r.AuthData)
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
		"platform":      "lovart",
		"cookie_string": lovartCookieString(r.AuthData),
		"cookies_map":   cookieMap,
		"cookies":       cookies,
	}
	for _, key := range []string{
		"access_token", "refresh_token", "access_token_expire_at", "refresh_token_expire_at",
		"webid", "captured_at", "is_new_user", "proxy", "user",
	} {
		if v, ok := auth[key]; ok {
			obj[key] = v
		}
	}
	return obj
}

/* ===================== 测活 ===================== */

func (h *Handler) LovartLiveCheckStart(c *gin.Context) {
	var in liveCheckReq
	_ = c.ShouldBindJSON(&in)
	runner := h.liveRunnerFor("lovart")
	if !runner.tryStart() {
		c.JSON(http.StatusConflict, gin.H{"error": "已有测活任务进行中"})
		return
	}
	items, err := h.loadLovartItems(in.IDs)
	if err != nil {
		runner.finish("加载账号失败")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if len(items) == 0 {
		runner.finish("没有可测活的账号")
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有可测活的 Lovart 账号（需已注册且有会话数据）"})
		return
	}
	runner.setTotal(len(items))
	go func() {
		defer runner.finish("")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		livecheck.CheckLovart(ctx, items, func(chunk map[uint]string) {
			for id, st := range chunk {
				h.applyLovartAlive(id, st)
				runner.tally(st)
			}
		})
	}()
	c.JSON(http.StatusOK, gin.H{"ok": true, "total": len(items)})
}

func (h *Handler) LovartLiveCheckStatus(c *gin.Context) {
	c.JSON(http.StatusOK, h.liveRunnerFor("lovart").snapshot())
}

func (h *Handler) LovartLiveCheckOne(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	item, ok := h.loadLovartItem(uint(id64))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该 Lovart 账号没有可测活的会话数据"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	st := livecheck.CheckLovart(ctx, []livecheck.LovartItem{item}, nil)[item.ID]
	if st == "" {
		st = livecheck.StatusUnknown
	}
	now := h.applyLovartAlive(item.ID, st)
	c.JSON(http.StatusOK, gin.H{"id": item.ID, "alive": st, "checked_at": now})
}

func (h *Handler) applyLovartAlive(id uint, st string) time.Time {
	now := time.Now()
	h.DB.Model(&models.LovartRegistration{}).Where("id = ?", id).
		Updates(map[string]any{"alive": st, "alive_checked_at": now})
	return now
}

func (h *Handler) loadLovartItems(ids []uint) ([]livecheck.LovartItem, error) {
	var regs []models.LovartRegistration
	q := h.DB.Select("id", "auth_data").Where("auth_data <> ''")
	if len(ids) > 0 {
		q = q.Where("id IN ?", ids)
	} else {
		q = q.Where("status = ?", "registered")
	}
	if err := q.Find(&regs).Error; err != nil {
		return nil, err
	}
	items := make([]livecheck.LovartItem, 0, len(regs))
	for _, r := range regs {
		items = append(items, lovartItemFromAuth(r.ID, r.AuthData))
	}
	return items, nil
}

func (h *Handler) loadLovartItem(id uint) (livecheck.LovartItem, bool) {
	var r models.LovartRegistration
	if err := h.DB.Select("id", "auth_data").First(&r, id).Error; err != nil {
		return livecheck.LovartItem{}, false
	}
	if r.AuthData == "" {
		return livecheck.LovartItem{}, false
	}
	item := lovartItemFromAuth(r.ID, r.AuthData)
	if item.AccessToken == "" && item.RefreshToken == "" {
		return livecheck.LovartItem{}, false
	}
	return item, true
}

func lovartItemFromAuth(id uint, authData string) livecheck.LovartItem {
	var meta struct {
		Proxy string `json:"proxy"`
	}
	_ = json.Unmarshal([]byte(authData), &meta)
	return livecheck.LovartItemsFromAuthJSON(id, authData, meta.Proxy)
}
