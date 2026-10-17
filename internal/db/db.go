package db

import (
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"chatgpt-register/internal/codexreg"
	"chatgpt-register/internal/emailalias"
	"chatgpt-register/internal/models"
	"chatgpt-register/internal/proxypool"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func Init(path string) (*gorm.DB, error) {
	db, err := gorm.Open(sqlite.Open(dsn(path)), &gorm.Config{})
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	// 写操作串行化：SQLite 同一时刻只允许一个写事务，连接开多了只会互相
	// 撞 SQLITE_BUSY（面板接口和注册任务一起卡几秒）。WAL 下读不阻塞写，
	// 单连接足够，且顺带避免连接被中断事务污染。
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(time.Hour)
	dropUnusedTables(db)
	if err := db.AutoMigrate(&models.Registration{}, &models.GrokRegistration{}, &models.AdobeRegistration{}, &models.LeonardoRegistration{}, &models.Mailbox{}, &models.Setting{}, &models.Admin{}); err != nil {
		return nil, err
	}
	if err := proxypool.Migrate(db); err != nil {
		return nil, err
	}
	normalizeLegacyStatuses(db)
	reclaimOrphanRegistering(db)
	backfillRegistrationMailboxIDs(db)
	backfillChatGPTCredentialMetadata(db)
	repairVerifiedCredentialStatus(db)
	return db, nil
}

// repairVerifiedCredentialStatus repairs records downgraded by the previous
// temporary-error policy. A real probe already proved the AT and a renewable
// RT/Web Session is still stored, so a later 403/429/timeout must not erase the
// last-known credential status. The latest error remains available separately.
func repairVerifiedCredentialStatus(db *gorm.DB) {
	var regs []models.Registration
	if err := db.Select("id", "auth_data", "account_credential_status", "access_token_status", "token_last_verified_at", "needs_relogin").
		Where("status = ? AND account_credential_status = ? AND access_token_status = ? AND needs_relogin = ? AND token_last_verified_at IS NOT NULL",
			"registered", "unknown", "valid", false).
		Find(&regs).Error; err != nil {
		return
	}
	for _, reg := range regs {
		if !hasStoredLongLivedCredential(reg.AuthData) {
			continue
		}
		db.Model(&models.Registration{}).Where("id = ?", reg.ID).
			Update("account_credential_status", "valid")
	}
}

func hasStoredLongLivedCredential(raw string) bool {
	var auth struct {
		RefreshToken string            `json:"refresh_token"`
		Cookies      []json.RawMessage `json:"cookies"`
	}
	return json.Unmarshal([]byte(raw), &auth) == nil &&
		(strings.TrimSpace(auth.RefreshToken) != "" || len(auth.Cookies) > 0)
}

// dsn 给数据库路径补上 WAL / busy_timeout 等 pragma：
// WAL 让读写互不阻塞，busy_timeout 让并发写排队等待而不是立刻报
// "database is locked"。已带查询参数的路径保持原样。
func dsn(path string) string {
	if strings.Contains(path, "?") {
		return path
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(30000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	return "file:" + path + "?" + q.Encode()
}

// dropUnusedTables 丢掉已经下线的平台表。旧库里这些表只占空间，启动时有就删。
func dropUnusedTables(db *gorm.DB) {
	for _, name := range []string{
		"oreate_registrations",
		"higgsfield_registrations",
		"gmail_producer_locks",
		"gmail_purchase_intents",
		"gmail_registrations",
		"gmail_sms_orders",
		"purchase_intents",
		"sms_orders",
	} {
		_ = db.Migrator().DropTable(name)
	}
}

// reclaimOrphanRegistering 启动时把各平台残留的 registering/waiting_code 记录标为
// register_failed。生产任务状态只在内存里，程序重启后这些"注册中"记录不会再有人
// 推进，置为失败后可在下次生产时被重新领取（母号+裂变补齐规则）。
func reclaimOrphanRegistering(db *gorm.DB) {
	orphanStatuses := []string{"registering", "waiting_code"}
	for _, m := range []struct {
		model any
		note  string
	}{
		{&models.Registration{}, "程序重启中断，可重新生产"},
		{&models.GrokRegistration{}, "程序重启中断，可重新注册"},
		{&models.AdobeRegistration{}, "程序重启中断，可重新注册"},
		{&models.LeonardoRegistration{}, "程序重启中断，可重新注册"},
	} {
		db.Model(m.model).Where("status IN ?", orphanStatuses).
			Updates(map[string]any{"status": "register_failed", "note": m.note})
	}
}

// normalizeLegacyStatuses 把旧的 AdSkull 验证态注册记录迁移到新的生产态。
// Mailbox 的 unverified/verified 表示邮箱凭据是否校验通过，语义不变，保持原样。
func normalizeLegacyStatuses(db *gorm.DB) {
	regStatusMap := map[string]string{
		"unverified":    "pending",
		"verifying":     "registering",
		"verify_failed": "register_failed",
		"verified":      "registered",
	}
	for oldStatus, newStatus := range regStatusMap {
		db.Model(&models.Registration{}).Where("status = ?", oldStatus).Update("status", newStatus)
	}
}

func backfillRegistrationMailboxIDs(db *gorm.DB) {
	var regs []models.Registration
	if err := db.Where("mailbox_id IS NULL OR mailbox_id = 0").Find(&regs).Error; err != nil {
		return
	}
	for _, reg := range regs {
		baseEmail := emailalias.Base(reg.Email)
		var mb models.Mailbox
		if err := db.Where("email = ?", baseEmail).First(&mb).Error; err == nil {
			db.Model(&models.Registration{}).Where("id = ?", reg.ID).Update("mailbox_id", mb.ID)
		}
	}
}

// backfillChatGPTCredentialMetadata is a local, non-network migration. It
// records only the JWT fingerprint and non-secret claims, and deliberately
// leaves every legacy token unchecked until the one-time real upstream
// revalidation runs from the handler layer.
func backfillChatGPTCredentialMetadata(db *gorm.DB) {
	var regs []models.Registration
	if err := db.Select("id", "auth_data", "access_token_fingerprint", "access_token_status", "account_credential_status", "credential_version").
		Where("status = ?", "registered").Find(&regs).Error; err != nil {
		return
	}
	for _, reg := range regs {
		if reg.AccessTokenFingerprint != "" {
			continue
		}
		var auth map[string]any
		if json.Unmarshal([]byte(reg.AuthData), &auth) != nil {
			continue
		}
		token, _ := auth["access_token"].(string)
		refresh, _ := auth["refresh_token"].(string)
		cookies, _ := auth["cookies"].([]any)
		fields := map[string]any{
			"access_token_status":       "unchecked",
			"account_credential_status": "unknown",
			"access_token_fingerprint":  codexreg.AccessTokenFingerprint(strings.TrimSpace(token)),
		}
		if strings.TrimSpace(refresh) == "" && len(cookies) == 0 {
			fields["needs_relogin"] = true
			fields["account_credential_status"] = "needs_relogin"
		}
		if meta, err := codexreg.ParseAccessTokenMetadata(token); err == nil {
			fields["token_issued_at"] = meta.IssuedAt
			fields["token_expires_at"] = meta.ExpiresAt
			fields["access_token_jti"] = meta.JTI
		}
		db.Model(&models.Registration{}).Where("id = ?", reg.ID).Updates(fields)
	}
}
