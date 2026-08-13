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

type leonardoStartInput struct {
	Email string `json:"email" binding:"required"`
	Note  string `json:"note"`
}

type leonardoProduceInput struct {
	Count int `json:"count" binding:"required"`
}

type leonardoCodeInput struct {
	Code string `json:"code" binding:"required"`
}

func (h *Handler) LeonardoList(c *gin.Context) {
	var regs []models.LeonardoRegistration
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
	q.Model(&models.LeonardoRegistration{}).Count(&total)
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

func (h *Handler) LeonardoStart(c *gin.Context) {
	var in leonardoStartInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if h.Browser == nil || !h.Browser.Ready() {
		c.JSON(http.StatusConflict, gin.H{"error": "缺少浏览器，无法注册：浏览器正在下载或下载失败"})
		return
	}
	reg, err := h.LeonardoProducer.Start(in.Email, in.Note)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, reg)
}

func (h *Handler) LeonardoProduce(c *gin.Context) {
	var in leonardoProduceInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if h.Browser == nil || !h.Browser.Ready() {
		c.JSON(http.StatusConflict, gin.H{"error": "缺少浏览器，无法注册：浏览器正在下载或下载失败"})
		return
	}
	regs, err := h.LeonardoProducer.StartFromAccounts(in.Count)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "started": len(regs), "data": regs})
}

// LeonardoProduceStatus 返回 Leonardo 生产进度（待生产/在跑/已注册/失败）。
func (h *Handler) LeonardoProduceStatus(c *gin.Context) {
	c.JSON(http.StatusOK, h.LeonardoProducer.Snapshot())
}

// LeonardoProduceStop 停止所有在跑的 Leonardo 注册任务。
func (h *Handler) LeonardoProduceStop(c *gin.Context) {
	h.LeonardoProducer.StopAll()
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) LeonardoSubmitCode(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	var in leonardoCodeInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.LeonardoProducer.SubmitCode(uint(id64), in.Code); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) LeonardoStop(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	h.LeonardoProducer.Stop(uint(id64))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) LeonardoDelete(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	h.LeonardoProducer.Stop(uint(id64))
	if err := h.DB.Delete(&models.LeonardoRegistration{}, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) LeonardoDeleteAll(c *gin.Context) {
	var regs []models.LeonardoRegistration
	h.DB.Select("id").Where("status IN ?", []string{"registering", "waiting_code"}).Find(&regs)
	for _, reg := range regs {
		h.LeonardoProducer.Stop(reg.ID)
	}
	r := h.DB.Where("1 = 1").Delete(&models.LeonardoRegistration{})
	if r.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": r.Error.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": r.RowsAffected})
}

func (h *Handler) LeonardoLog(c *gin.Context) {
	var reg models.LeonardoRegistration
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

func (h *Handler) LeonardoShot(c *gin.Context) {
	var reg models.LeonardoRegistration
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

// LeonardoDownload 导出选中 Leonardo 账号的站点 Cookie（含 better-auth 会话 cookie），
// 仅对已注册且有会话数据的记录开放。
// 请求体：{ "ids": [1,2,3], "format": "string|json|array", "unshipped_only": false }。
// unshipped_only=true 时忽略 ids，导出全部已注册且未出库的账号。
//   - string：Cookie 字符串 k=v; k=v; ...（单账号 .txt，多账号 .zip）
//   - json  ：单个 Leonardo 的 Cookie JSON 对象（单账号 .json，多账号 .zip）
//   - array ：多个 Leonardo 批量的 Cookie 数组，始终单个 .json 文件
func (h *Handler) LeonardoDownload(c *gin.Context) {
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "未选择 Leonardo 账号"})
		return
	}
	if in.Format == "" {
		in.Format = "string"
	}
	if in.Format != "string" && in.Format != "json" && in.Format != "array" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不支持的导出格式"})
		return
	}

	var regs []models.LeonardoRegistration
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
			c.JSON(http.StatusBadRequest, gin.H{"error": "没有已注册未出库的 Leonardo 账号"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "所选 Leonardo 账号没有可下载的会话数据"})
		return
	}

	ids := make([]uint, 0, len(regs))
	for _, r := range regs {
		ids = append(ids, r.ID)
	}
	// 导出即出库。
	h.DB.Model(&models.LeonardoRegistration{}).Where("id IN ?", ids).Update("shipped", true)

	stamp := time.Now().UTC().Format("20060102_150405")
	switch in.Format {
	case "string":
		if len(regs) == 1 {
			c.Header("Content-Disposition", `attachment; filename="leonardo-`+safeFileName(regs[0].Email)+`.txt"`)
			c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(leonardoCookieString(regs[0].AuthData)))
			return
		}
		h.leonardoZipEntries(c, "leonardo_cookies_"+stamp+".zip", regs, func(r models.LeonardoRegistration) (string, []byte) {
			return "leonardo-" + safeFileName(r.Email) + ".txt", []byte(leonardoCookieString(r.AuthData))
		})
	case "json":
		if len(regs) == 1 {
			out, _ := json.MarshalIndent(leonardoExportObject(regs[0]), "", "  ")
			c.Header("Content-Disposition", `attachment; filename="leonardo-`+safeFileName(regs[0].Email)+`.json"`)
			c.Data(http.StatusOK, "application/json; charset=utf-8", out)
			return
		}
		h.leonardoZipEntries(c, "leonardo_cookies_"+stamp+".zip", regs, func(r models.LeonardoRegistration) (string, []byte) {
			out, _ := json.MarshalIndent(leonardoExportObject(r), "", "  ")
			return "leonardo-" + safeFileName(r.Email) + ".json", out
		})
	case "array":
		arr := make([]map[string]any, 0, len(regs))
		for _, r := range regs {
			arr = append(arr, leonardoExportObject(r))
		}
		out, _ := json.MarshalIndent(arr, "", "  ")
		c.Header("Content-Disposition", `attachment; filename="leonardo_cookies_array_`+stamp+`.json"`)
		c.Data(http.StatusOK, "application/json; charset=utf-8", out)
	}
}

func (h *Handler) leonardoZipEntries(c *gin.Context, name string, regs []models.LeonardoRegistration, build func(models.LeonardoRegistration) (string, []byte)) {
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

// leonardoCookies 从 AuthData 里解析出 Cookie 列表（含元数据）。
func leonardoCookies(authData string) []map[string]any {
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

// leonardoCookieString 拼成浏览器 Cookie 头字符串：k=v; k=v; ...
func leonardoCookieString(authData string) string {
	var parts []string
	for _, ck := range leonardoCookies(authData) {
		name, _ := ck["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		value, _ := ck["value"].(string)
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, "; ")
}

// leonardoExportObject 构造单个 Leonardo 账号的导出对象（Cookie JSON 对象 / 数组元素）。
func leonardoExportObject(r models.LeonardoRegistration) map[string]any {
	var auth map[string]any
	_ = json.Unmarshal([]byte(r.AuthData), &auth)

	cookies := leonardoCookies(r.AuthData)
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
		"platform":      "leonardo",
		"cookie_string": leonardoCookieString(r.AuthData),
		"cookies_map":   cookieMap,
		"cookies":       cookies,
	}
	if v, ok := auth["captured_at"]; ok {
		obj["captured_at"] = v
	}
	if v, ok := auth["storage"]; ok {
		obj["storage"] = v
	}
	return obj
}

/* ===================== 测活 ===================== */

func (h *Handler) LeonardoLiveCheckStart(c *gin.Context) {
	var in liveCheckReq
	_ = c.ShouldBindJSON(&in)
	runner := h.liveRunnerFor("leonardo")
	if !runner.tryStart() {
		c.JSON(http.StatusConflict, gin.H{"error": "已有测活任务进行中"})
		return
	}
	items, err := h.loadLeonardoItems(in.IDs)
	if err != nil {
		runner.finish("加载账号失败")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if len(items) == 0 {
		runner.finish("没有可测活的账号")
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有可测活的 Leonardo 账号（需已注册且有会话数据）"})
		return
	}
	runner.setTotal(len(items))
	go func() {
		defer runner.finish("")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		livecheck.CheckLeonardo(ctx, items, func(chunk map[uint]string) {
			for id, st := range chunk {
				h.applyLeonardoAlive(id, st)
				runner.tally(st)
			}
		})
	}()
	c.JSON(http.StatusOK, gin.H{"ok": true, "total": len(items)})
}

func (h *Handler) LeonardoLiveCheckStatus(c *gin.Context) {
	c.JSON(http.StatusOK, h.liveRunnerFor("leonardo").snapshot())
}

func (h *Handler) LeonardoLiveCheckOne(c *gin.Context) {
	id64, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	item, ok := h.loadLeonardoItem(uint(id64))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该 Leonardo 账号没有可测活的会话数据"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	st := livecheck.CheckLeonardo(ctx, []livecheck.LeonardoItem{item}, nil)[item.ID]
	if st == "" {
		st = livecheck.StatusUnknown
	}
	now := h.applyLeonardoAlive(item.ID, st)
	c.JSON(http.StatusOK, gin.H{"id": item.ID, "alive": st, "checked_at": now})
}

func (h *Handler) applyLeonardoAlive(id uint, st string) time.Time {
	now := time.Now()
	h.DB.Model(&models.LeonardoRegistration{}).Where("id = ?", id).
		Updates(map[string]any{"alive": st, "alive_checked_at": now})
	return now
}

func (h *Handler) loadLeonardoItems(ids []uint) ([]livecheck.LeonardoItem, error) {
	var regs []models.LeonardoRegistration
	q := h.DB.Select("id", "auth_data").Where("auth_data <> ''")
	if len(ids) > 0 {
		q = q.Where("id IN ?", ids)
	} else {
		q = q.Where("status = ?", "registered")
	}
	if err := q.Find(&regs).Error; err != nil {
		return nil, err
	}
	items := make([]livecheck.LeonardoItem, 0, len(regs))
	for _, r := range regs {
		items = append(items, livecheck.LeonardoItem{ID: r.ID, Cookies: livecheck.LeonardoCookiesFromAuthJSON(r.AuthData)})
	}
	return items, nil
}

func (h *Handler) loadLeonardoItem(id uint) (livecheck.LeonardoItem, bool) {
	var r models.LeonardoRegistration
	if err := h.DB.Select("id", "auth_data").First(&r, id).Error; err != nil {
		return livecheck.LeonardoItem{}, false
	}
	if r.AuthData == "" {
		return livecheck.LeonardoItem{}, false
	}
	return livecheck.LeonardoItem{ID: r.ID, Cookies: livecheck.LeonardoCookiesFromAuthJSON(r.AuthData)}, true
}
