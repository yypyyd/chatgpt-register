package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"chatgpt-register/internal/models"
	"chatgpt-register/internal/proxypool"

	"github.com/gin-gonic/gin"
)

func proxyRouter(t *testing.T) (*Handler, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := handlerTestDB(t)
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	h := &Handler{DB: db, Proxies: proxypool.New(db)}
	router := gin.New()
	router.GET("/api/proxy/channels", h.ProxyChannels)
	router.PUT("/api/proxy/channels/:channel", h.ProxyChannelSave)
	router.POST("/api/proxy/channels/:channel/test", h.ProxyChannelTest)
	router.PUT("/api/settings", h.SettingsSave)
	router.GET("/api/settings", h.SettingsGet)
	return h, router
}

func proxyRequest(t *testing.T, router *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(string(data)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestProxyChannelEndpointsSaveAllChannelsIndependently(t *testing.T) {
	h, router := proxyRouter(t)
	for i, channel := range proxypool.Channels {
		cfg := proxypool.Config{Enabled: i%2 == 0, Source: "api", APIURL: fmt.Sprintf("https://extract.example/%s?count=1&sessTime=30", channel)}
		w := proxyRequest(t, router, http.MethodPut, "/api/proxy/channels/"+channel, cfg)
		if w.Code != http.StatusOK {
			t.Fatalf("%s save failed: %d %s", channel, w.Code, w.Body.String())
		}
	}
	w := proxyRequest(t, router, http.MethodGet, "/api/proxy/channels", nil)
	var data struct {
		Channels []struct {
			Channel string           `json:"channel"`
			Config  proxypool.Config `json:"config"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil || len(data.Channels) != len(proxypool.Channels) {
		t.Fatalf("bad channel response: %s", w.Body.String())
	}
	for i, row := range data.Channels {
		if row.Channel != proxypool.Channels[i] || row.Config.Enabled != (i%2 == 0) || !strings.Contains(row.Config.APIURL, "/"+row.Channel+"?") {
			t.Fatal("saving another channel overwrote a configuration")
		}
	}
	w = proxyRequest(t, router, http.MethodPut, "/api/proxy/channels/leonardo", proxypool.Config{Source: "api"})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	lease, err := h.Proxies.Acquire(context.Background(), "leonardo", 0)
	if err != nil || lease.Proxy != "" {
		t.Fatal("channel switch was bypassed")
	}
}

func TestInvalidProxySaveDoesNotPartiallyChangeSettings(t *testing.T) {
	h, router := proxyRouter(t)
	cfg := proxypool.Config{Enabled: true, Source: "api", APIURL: "https://extract.example/gen?sessTime=10"}
	if err := h.Proxies.Save("chatgpt", cfg); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []proxypool.Config{
		{Enabled: true, Source: "api"},
		{Enabled: true, Source: "api", APIURL: "file:///secret"},
		{Enabled: true, Source: "api", APIURL: "https://extract.example/?sessTime=invalid"},
		{Enabled: true, Source: "api", APIURL: "https://extract.example/?sessType=rotating"},
		{Enabled: true, Source: "list", List: "bad proxy"},
	} {
		w := proxyRequest(t, router, http.MethodPut, "/api/proxy/channels/chatgpt", invalid)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid configuration saved: %d", w.Code)
		}
		current, _ := proxypool.ReadConfig(h.DB, "chatgpt")
		if current != cfg {
			t.Fatal("failed validation partially modified the live channel")
		}
	}
	w := proxyRequest(t, router, http.MethodPut, "/api/settings", map[string]string{"proxy_chatgpt_enabled": "0", "max_concurrency": "99"})
	if w.Code != http.StatusBadRequest {
		t.Fatal("generic settings bypassed channel validation")
	}
	w = proxyRequest(t, router, http.MethodGet, "/api/settings", nil)
	if strings.Contains(w.Body.String(), "proxy_chatgpt") || strings.Contains(w.Body.String(), "extract.example") || strings.Contains(w.Body.String(), "max_concurrency") {
		t.Fatal("generic settings exposed proxy secrets or saved a rejected mixed request")
	}
}

func TestRetiredChannelsRejectOperationsAndKeepHistoricalProxySecretsHidden(t *testing.T) {
	h, router := proxyRouter(t)
	for _, channel := range []string{"lumina", "createvision", "dola"} {
		for _, key := range []string{"proxy_" + channel + "_api_url", channel + "_proxy_list"} {
			if err := h.DB.Create(&models.Setting{Key: key, Value: "historical-proxy-secret"}).Error; err != nil {
				t.Fatal(err)
			}
			w := proxyRequest(t, router, http.MethodPut, "/api/settings", map[string]string{key: "overwrite"})
			if w.Code != http.StatusBadRequest {
				t.Fatal("retired proxy key accepted through general settings")
			}
		}
		for _, request := range []struct{ method, path string }{
			{http.MethodPut, "/api/proxy/channels/" + channel},
			{http.MethodPost, "/api/proxy/channels/" + channel + "/test"},
		} {
			w := proxyRequest(t, router, request.method, request.path, proxypool.Config{Source: "api"})
			if w.Code != http.StatusBadRequest {
				t.Fatal("retired proxy channel still accepts operations")
			}
		}
	}
	for _, endpoint := range []string{"/api/settings", "/api/proxy/channels"} {
		w := proxyRequest(t, router, http.MethodGet, endpoint, nil)
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "historical-proxy-secret") {
			t.Fatal("historical proxy credentials exposed after channel removal")
		}
		for _, channel := range []string{"lumina", "createvision", "dola"} {
			if strings.Contains(w.Body.String(), channel) {
				t.Fatal("retired channel remains in settings response")
			}
		}
	}
}

func TestProxyDraftTestReportsWhitelistWithoutSaving(t *testing.T) {
	h, router := proxyRouter(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "IP Whitelist Check Failed")
	}))
	defer server.Close()
	cfg := proxypool.Config{Enabled: true, Source: "api", APIURL: server.URL + "?key=api-secret"}
	w := proxyRequest(t, router, http.MethodPost, "/api/proxy/channels/grok/test", cfg)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "白名单") || strings.Contains(w.Body.String(), "api-secret") || strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("bad extraction failure response: %s", w.Body.String())
	}
	current, _ := proxypool.ReadConfig(h.DB, "grok")
	if current.Enabled || current.APIURL != "" {
		t.Fatal("testing an unsaved draft changed a live channel")
	}
}

func TestProxyProbeUsesProxyAuthenticationAndValidatesReturnedIP(t *testing.T) {
	var invalid atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" || r.URL.Host != "egress.example" {
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		if invalid.Load() {
			fmt.Fprint(w, "<html>not an IP</html>")
		} else {
			fmt.Fprint(w, "203.0.113.24\n")
		}
	}))
	defer proxy.Close()
	raw := strings.Replace(proxy.URL, "://", "://user:pass@", 1)
	ip, _, err := probeProxy(context.Background(), raw, "http://egress.example/ip")
	if err != nil || ip != "203.0.113.24" {
		t.Fatalf("authenticated proxy probe failed: %v", err)
	}
	invalid.Store(true)
	if _, _, err := probeProxy(context.Background(), raw, "http://egress.example/ip"); err == nil {
		t.Fatal("HTML was accepted as an exit IP")
	}
}
