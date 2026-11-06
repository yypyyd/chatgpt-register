package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"chatgpt-register/internal/proxypool"
	"chatgpt-register/internal/proxyutil"

	"github.com/gin-gonic/gin"
)

type proxyTestInput struct {
	Proxy string `json:"proxy"`
}

// ProxyTest 通过给定代理请求一个 IP 探测服务，返回出口 IP，用于验证代理可用。
func (h *Handler) ProxyTest(c *gin.Context) {
	var in proxyTestInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(in.Proxy) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "代理为空"})
		return
	}
	ip, ms, err := probeProxy(c.Request.Context(), in.Proxy, "https://api.ipify.org?format=text")
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "ip": ip, "ms": ms})
}

func probeProxy(ctx context.Context, raw, endpoint string) (string, int64, error) {
	transport, err := proxyutil.Transport(raw)
	if err != nil || transport == nil {
		return "", 0, fmt.Errorf("代理格式错误")
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 12 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", 0, fmt.Errorf("出口探测地址错误")
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("代理连接失败或出口探测超时：%s", describeProbeError(err))
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	ip := strings.TrimSpace(string(buf))
	if err != nil || resp.StatusCode != http.StatusOK || net.ParseIP(ip) == nil {
		return "", 0, fmt.Errorf("代理出口探测未返回有效 IP")
	}
	return ip, time.Since(start).Milliseconds(), nil
}

var proxyCredPattern = regexp.MustCompile(`//[^/@\s]+@`)

// describeProbeError 把探测的底层网络错误转成可读文本：剥掉 url.Error 外壳里的目标地址、
// 脱敏代理账号密码，并在超时时补充最常见的原因（协议不匹配）。
func describeProbeError(err error) string {
	inner := err
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		inner = ue.Err
	}
	msg := proxyCredPattern.ReplaceAllString(inner.Error(), "//***@")
	if r := []rune(msg); len(r) > 200 {
		msg = string(r[:200]) + "…"
	}
	var ne net.Error
	if (errors.As(err, &ne) && ne.Timeout()) || errors.Is(err, context.DeadlineExceeded) {
		msg += "（12 秒内无响应，常见于协议不匹配：例如把 SOCKS5 端口当作 HTTP 代理使用，请检查提取 API 的 proto 参数或代理前缀）"
	}
	return msg
}

func (h *Handler) proxyManager() *proxypool.Manager {
	if h.Proxies != nil {
		return h.Proxies
	}
	return proxypool.New(h.DB)
}

func (h *Handler) ProxyChannels(c *gin.Context) {
	rows := make([]gin.H, 0, len(proxypool.Channels))
	manager := h.proxyManager()
	for _, channel := range proxypool.Channels {
		cfg, err := proxypool.ReadConfig(h.DB, channel)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		rows = append(rows, gin.H{"channel": channel, "label": proxypool.Label(channel), "config": cfg, "status": manager.Status(channel, cfg), "min_session_minutes": int(proxypool.RegistrationBudget(channel)/time.Minute) + 1})
	}
	c.JSON(http.StatusOK, gin.H{"channels": rows})
}

func (h *Handler) ProxyChannelSave(c *gin.Context) {
	channel := c.Param("channel")
	if !proxypool.ValidChannel(channel) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "未知代理渠道"})
		return
	}
	var cfg proxypool.Config
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "代理配置格式错误"})
		return
	}
	if err := cfg.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.proxyManager().Save(channel, cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Tests use an unsaved draft without putting test credentials in the live pool.
func (h *Handler) ProxyChannelTest(c *gin.Context) {
	if !proxypool.ValidChannel(c.Param("channel")) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "未知代理渠道"})
		return
	}
	var cfg proxypool.Config
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "代理配置格式错误"})
		return
	}
	cfg.Enabled = true
	ctx, cancel := context.WithTimeout(c.Request.Context(), 40*time.Second)
	defer cancel()
	proxies, err := h.proxyManager().Fetch(ctx, cfg)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": err.Error()})
		return
	}
	proxy := proxies[0]
	if cfg.Source == "list" {
		proxy = proxyutil.WithBestGoTaskSession(proxy)
	}
	ip, ms, err := probeProxy(ctx, proxy, "https://api.ipify.org?format=text")
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "count": len(proxies), "error": "已提取代理，但测试失败：" + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "count": len(proxies), "ip": ip, "ms": ms})
}
