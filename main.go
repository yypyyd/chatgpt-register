package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"chatgpt-register/internal/auth"
	"chatgpt-register/internal/browserboot"
	"chatgpt-register/internal/db"
	"chatgpt-register/internal/handlers"

	"github.com/gin-gonic/gin"
)

//go:embed static
var staticFS embed.FS

func main() {
	database, err := db.Init("adskull.db")
	if err != nil {
		log.Fatalf("init db: %v", err)
	}

	// 重启后仍停留在"注册中"的记录已无存活任务，统一判定为失败。
	if err := database.Exec(
		"UPDATE registrations SET status = 'register_failed', log = log || ? WHERE status = 'registering'",
		"\n"+time.Now().Format("2006-01-02 15:04:05")+" ✗ 程序重启，任务中断，判定为失败",
	).Error; err != nil {
		log.Printf("reset registering on boot: %v", err)
	}
	if err := database.Exec(
		"UPDATE grok_registrations SET status = 'register_failed', log = log || ? WHERE status IN ('registering', 'waiting_code')",
		"\n"+time.Now().Format("2006-01-02 15:04:05")+" ✗ 程序重启，Grok 任务中断，判定为失败",
	).Error; err != nil {
		log.Printf("reset grok registering on boot: %v", err)
	}

	authSvc, err := auth.New(database)
	if err != nil {
		log.Fatalf("init auth: %v", err)
	}

	// 启动时后台确保 rod 所需浏览器已就绪，未就绪则自动下载。
	browser := browserboot.New()
	browser.EnsureAsync()

	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	h, err := handlers.New(database, authSvc, browser)
	if err != nil {
		log.Fatalf("init mailbox verifier: %v", err)
	}
	defer h.MailboxVerifier.Stop()

	r.POST("/api/login", h.Login)

	api := r.Group("/api", h.AuthRequired())
	{
		api.POST("/change-password", h.ChangePassword)

		api.GET("/stats", h.Stats)
		api.GET("/registrations", h.List)
		api.DELETE("/registrations", h.DeleteAll)
		api.GET("/registrations/:id", h.Get)
		api.POST("/registrations", h.Create)
		api.PUT("/registrations/:id", h.Update)
		api.DELETE("/registrations/:id", h.Delete)
		api.GET("/registrations/:id/logs", h.RegistrationLog)
		api.GET("/registrations/:id/shot", h.RegistrationShot)
		api.PUT("/registrations/:id/shipped", h.SetShipped)
		api.POST("/download", h.Download)

		api.POST("/produce", h.Produce)
		api.GET("/produce/status", h.ProduceStatus)
		api.POST("/produce/stop", h.ProduceStop)
		api.GET("/browser/status", h.BrowserStatus)

		api.GET("/mailboxes", h.MailboxList)
		api.DELETE("/mailboxes", h.MailboxDeleteAll)
		api.POST("/mailboxes", h.MailboxCreate)
		api.POST("/mailboxes/import", h.MailboxImport)
		api.POST("/mailboxes/reauthenticate", h.MailboxReauthenticate)
		api.POST("/mailboxes/:id/verify", h.MailboxVerify)
		api.PUT("/mailboxes/:id", h.MailboxUpdate)
		api.DELETE("/mailboxes/:id", h.MailboxDelete)
		api.GET("/mailboxes/:id/messages", h.MailboxMessages)
		api.GET("/mailboxes/:id/message", h.MailboxMessage)

		api.GET("/settings", h.SettingsGet)
		api.PUT("/settings", h.SettingsSave)

		api.POST("/proxy/test", h.ProxyTest)

		api.GET("/grok/registrations", h.GrokList)
		api.DELETE("/grok/registrations", h.GrokDeleteAll)
		api.POST("/grok/registrations", h.GrokStart)
		api.POST("/grok/produce", h.GrokProduce)
		api.POST("/grok/registrations/:id/code", h.GrokSubmitCode)
		api.POST("/grok/registrations/:id/stop", h.GrokStop)
		api.DELETE("/grok/registrations/:id", h.GrokDelete)
		api.GET("/grok/registrations/:id/logs", h.GrokLog)
		api.GET("/grok/registrations/:id/shot", h.GrokShot)
		api.POST("/grok/download", h.GrokDownload)
	}

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("static fs: %v", err)
	}
	httpFS := http.FS(sub)
	r.StaticFS("/static", httpFS)
	for _, p := range []string{"login", "dashboard", "mailboxes", "accounts", "grok", "settings"} {
		p := p
		r.GET("/"+p, func(c *gin.Context) { c.FileFromFS(p+".html", httpFS) })
	}
	r.GET("/", func(c *gin.Context) { c.FileFromFS("dashboard.html", httpFS) })

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":9000"
	}
	if !strings.Contains(addr, ":") {
		addr = ":" + addr
	}
	log.Printf("chatgpt-register listening on http://localhost%s", addr)
	if err := r.Run(addr); err != nil {
		log.Fatal(err)
	}
}
