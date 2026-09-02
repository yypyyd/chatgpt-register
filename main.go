package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
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
	// db.Init 内部会把重启前残留的"注册中"记录统一判定为失败（reclaimOrphanRegistering）。
	database, err := db.Init("adskull.db")
	if err != nil {
		log.Fatalf("init db: %v", err)
	}

	authSvc, err := auth.New(database)
	if err != nil {
		log.Fatalf("init auth: %v", err)
	}

	// 启动时后台确保 rod 所需浏览器已就绪，未就绪则自动下载。
	browser := browserboot.New()
	browser.EnsureAsync()

	// /tmp 为 tmpfs（占内存），异常退出会残留 rod 浏览器 Profile 目录，
	// 堆积会耗尽内存导致并发注册失败：每 10 分钟清理超过 30 分钟没动过的残留
	//（单次注册只有几分钟，不会误删正在跑的）。
	go func() {
		for {
			cleanStaleRodProfiles(30 * time.Minute)
			time.Sleep(10 * time.Minute)
		}
	}()

	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	h, err := handlers.New(database, authSvc, browser)
	if err != nil {
		log.Fatalf("init mailbox verifier: %v", err)
	}
	defer h.MailboxVerifier.Stop()
	defer h.OreateMinter.Close()

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

		api.POST("/registrations/livecheck", h.LiveCheckStart)
		api.GET("/registrations/livecheck/status", h.LiveCheckStatus)
		api.POST("/registrations/:id/livecheck", h.LiveCheckOne)

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
		api.GET("/grok/produce/status", h.GrokProduceStatus)
		api.POST("/grok/produce/stop", h.GrokProduceStop)
		api.POST("/grok/registrations/:id/code", h.GrokSubmitCode)
		api.POST("/grok/registrations/:id/stop", h.GrokStop)
		api.DELETE("/grok/registrations/:id", h.GrokDelete)
		api.GET("/grok/registrations/:id/logs", h.GrokLog)
		api.GET("/grok/registrations/:id/shot", h.GrokShot)
		api.POST("/grok/download", h.GrokDownload)

		api.POST("/grok/registrations/livecheck", h.GrokLiveCheckStart)
		api.GET("/grok/registrations/livecheck/status", h.GrokLiveCheckStatus)
		api.POST("/grok/registrations/:id/livecheck", h.GrokLiveCheckOne)

		api.GET("/adobe/registrations", h.AdobeList)
		api.DELETE("/adobe/registrations", h.AdobeDeleteAll)
		api.POST("/adobe/registrations", h.AdobeStart)
		api.POST("/adobe/produce", h.AdobeProduce)
		api.GET("/adobe/produce/status", h.AdobeProduceStatus)
		api.POST("/adobe/produce/stop", h.AdobeProduceStop)
		api.POST("/adobe/registrations/:id/code", h.AdobeSubmitCode)
		api.POST("/adobe/registrations/:id/stop", h.AdobeStop)
		api.POST("/adobe/registrations/:id/rescue", h.AdobeRescue)
		api.POST("/adobe/registrations/rescue-dead", h.AdobeRescueDead)
		api.DELETE("/adobe/registrations/:id", h.AdobeDelete)
		api.GET("/adobe/registrations/:id/logs", h.AdobeLog)
		api.GET("/adobe/registrations/:id/shot", h.AdobeShot)
		api.POST("/adobe/download", h.AdobeDownload)

		api.POST("/adobe/registrations/livecheck", h.AdobeLiveCheckStart)
		api.GET("/adobe/registrations/livecheck/status", h.AdobeLiveCheckStatus)
		api.POST("/adobe/registrations/:id/livecheck", h.AdobeLiveCheckOne)

		api.GET("/leonardo/registrations", h.LeonardoList)
		api.DELETE("/leonardo/registrations", h.LeonardoDeleteAll)
		api.POST("/leonardo/registrations", h.LeonardoStart)
		api.POST("/leonardo/produce", h.LeonardoProduce)
		api.GET("/leonardo/produce/status", h.LeonardoProduceStatus)
		api.POST("/leonardo/produce/stop", h.LeonardoProduceStop)
		api.POST("/leonardo/registrations/:id/code", h.LeonardoSubmitCode)
		api.POST("/leonardo/registrations/:id/stop", h.LeonardoStop)
		api.DELETE("/leonardo/registrations/:id", h.LeonardoDelete)
		api.GET("/leonardo/registrations/:id/logs", h.LeonardoLog)
		api.GET("/leonardo/registrations/:id/shot", h.LeonardoShot)
		api.POST("/leonardo/download", h.LeonardoDownload)

		api.POST("/leonardo/registrations/livecheck", h.LeonardoLiveCheckStart)
		api.GET("/leonardo/registrations/livecheck/status", h.LeonardoLiveCheckStatus)
		api.POST("/leonardo/registrations/:id/livecheck", h.LeonardoLiveCheckOne)

		api.GET("/oreate/registrations", h.OreateList)
		api.DELETE("/oreate/registrations", h.OreateDeleteAll)
		api.POST("/oreate/registrations", h.OreateStart)
		api.POST("/oreate/produce", h.OreateProduce)
		api.GET("/oreate/produce/status", h.OreateProduceStatus)
		api.POST("/oreate/produce/stop", h.OreateProduceStop)
		api.POST("/oreate/registrations/:id/stop", h.OreateStop)
		api.DELETE("/oreate/registrations/:id", h.OreateDelete)
		api.GET("/oreate/registrations/:id/logs", h.OreateLog)
		api.POST("/oreate/download", h.OreateDownload)
		api.POST("/oreate/jt", h.OreateMintJT)

		api.GET("/higgsfield/registrations", h.HiggsfieldList)
		api.DELETE("/higgsfield/registrations", h.HiggsfieldDeleteAll)
		api.POST("/higgsfield/registrations", h.HiggsfieldStart)
		api.POST("/higgsfield/produce", h.HiggsfieldProduce)
		api.GET("/higgsfield/produce/status", h.HiggsfieldProduceStatus)
		api.POST("/higgsfield/produce/stop", h.HiggsfieldProduceStop)
		api.POST("/higgsfield/registrations/:id/code", h.HiggsfieldSubmitCode)
		api.POST("/higgsfield/registrations/:id/stop", h.HiggsfieldStop)
		api.POST("/higgsfield/registrations/:id/trial", h.HiggsfieldTrial)
		api.DELETE("/higgsfield/registrations/:id", h.HiggsfieldDelete)
		api.GET("/higgsfield/registrations/:id/logs", h.HiggsfieldLog)
		api.POST("/higgsfield/download", h.HiggsfieldDownload)

		api.GET("/lumina/registrations", h.LuminaList)
		api.DELETE("/lumina/registrations", h.LuminaDeleteAll)
		api.POST("/lumina/registrations", h.LuminaStart)
		api.POST("/lumina/produce", h.LuminaProduce)
		api.GET("/lumina/produce/status", h.LuminaProduceStatus)
		api.POST("/lumina/produce/stop", h.LuminaProduceStop)
		api.POST("/lumina/registrations/:id/code", h.LuminaSubmitCode)
		api.POST("/lumina/registrations/:id/stop", h.LuminaStop)
		api.DELETE("/lumina/registrations/:id", h.LuminaDelete)
		api.GET("/lumina/registrations/:id/logs", h.LuminaLog)
		api.GET("/lumina/registrations/:id/shot", h.LuminaShot)
		api.POST("/lumina/download", h.LuminaDownload)
	}

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("static fs: %v", err)
	}
	httpFS := http.FS(sub)
	r.StaticFS("/static", httpFS)
	for _, p := range []string{"login", "dashboard", "mailboxes", "accounts", "grok", "adobe", "leonardo", "oreate", "higgsfield", "lumina", "settings"} {
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
	displayAddr := addr
	if strings.HasPrefix(displayAddr, ":") {
		displayAddr = "localhost" + displayAddr
	}
	log.Printf("chatgpt-register listening on http://%s", displayAddr)
	if err := r.Run(addr); err != nil {
		log.Fatal(err)
	}
}

// cleanStaleRodProfiles 删除 rod 残留的临时浏览器 Profile 目录（超过 maxAge 未修改的）。
func cleanStaleRodProfiles(maxAge time.Duration) {
	dir := filepath.Join(os.TempDir(), "rod", "user-data")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err == nil {
			removed++
		}
	}
	if removed > 0 {
		log.Printf("已清理 %d 个残留的浏览器临时 Profile 目录", removed)
	}
}
