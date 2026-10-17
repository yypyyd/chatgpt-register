package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"chatgpt-register/internal/auth"
	"chatgpt-register/internal/handlers"
	"chatgpt-register/internal/models"
	"chatgpt-register/internal/proxypool"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func main() {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil { panic(err) }
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&models.Setting{}, &models.Admin{}); err != nil { panic(err) }
	if err := proxypool.Migrate(db); err != nil { panic(err) }
	authSvc, err := auth.New(db)
	if err != nil { panic(err) }
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != "api.ipify.org:443" || r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
			http.Error(w, "not allowed", http.StatusProxyAuthRequired)
			return
		}
		upstream, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
		if err != nil { http.Error(w, "upstream unavailable", 502); return }
		conn, reader, err := w.(http.Hijacker).Hijack()
		if err != nil { upstream.Close(); return }
		fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() { defer conn.Close(); defer upstream.Close(); io.Copy(upstream, reader) }()
		go func() { defer conn.Close(); defer upstream.Close(); io.Copy(conn, upstream) }()
	}))
	defer proxy.Close()
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	h := &handlers.Handler{DB: db, Auth: authSvc, Proxies: proxypool.New(db)}
	router.Static("/static", "static")
	router.GET("/login", func(c *gin.Context) { c.File("static/login.html") })
	router.GET("/settings", func(c *gin.Context) { c.File("static/settings.html") })
	router.GET("/dashboard", func(c *gin.Context) { c.Redirect(302, "/settings") })
	router.GET("/mock/extract", func(c *gin.Context) { c.String(200, "http://user:pass@%s\r\n", proxy.Listener.Addr().String()) })
	router.GET("/mock/error", func(c *gin.Context) { c.String(400, "IP Whitelist Check Failed") })
	router.POST("/api/login", h.Login)
	api := router.Group("/api", h.AuthRequired())
	api.GET("/settings", h.SettingsGet)
	api.PUT("/settings", h.SettingsSave)
	api.GET("/proxy/channels", h.ProxyChannels)
	api.PUT("/proxy/channels/:channel", h.ProxyChannelSave)
	api.POST("/proxy/channels/:channel/test", h.ProxyChannelTest)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil { panic(err) }
	fmt.Printf("UI_VERIFY_URL=http://%s/login\n", listener.Addr())
	fmt.Printf("MOCK_API=http://%s/mock/extract?sessTime=30&sessType=sticky\n", listener.Addr())
	if err := http.Serve(listener, router); err != nil { fmt.Fprintln(os.Stderr, err) }
}
