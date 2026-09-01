package db

import (
	"net/url"
	"strings"
	"time"

	"chatgpt-register/internal/emailalias"
	"chatgpt-register/internal/models"

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
	if err := db.AutoMigrate(&models.Registration{}, &models.GrokRegistration{}, &models.AdobeRegistration{}, &models.LeonardoRegistration{}, &models.OreateRegistration{}, &models.HiggsfieldRegistration{}, &models.LuminaRegistration{}, &models.Mailbox{}, &models.Setting{}, &models.Admin{}); err != nil {
		return nil, err
	}
	normalizeLegacyStatuses(db)
	reclaimOrphanRegistering(db)
	reclaimOrphanGrokRegistering(db)
	reclaimOrphanAdobeRegistering(db)
	reclaimOrphanLeonardoRegistering(db)
	reclaimOrphanOreateRegistering(db)
	reclaimOrphanHiggsfieldRegistering(db)
	reclaimOrphanLuminaRegistering(db)
	backfillRegistrationMailboxIDs(db)
	return db, nil
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

// reclaimOrphanRegistering 启动时把残留的 registering 记录标为 register_failed。
// 生产任务状态只在内存里，程序重启后这些"注册中"记录不会再有人推进，
// 置为失败后可在下次生产时被重新领取（母号+裂变补齐规则）。
func reclaimOrphanRegistering(db *gorm.DB) {
	db.Model(&models.Registration{}).Where("status = ?", "registering").
		Updates(map[string]any{"status": "register_failed", "note": "程序重启中断，可重新生产"})
}

func reclaimOrphanGrokRegistering(db *gorm.DB) {
	db.Model(&models.GrokRegistration{}).Where("status IN ?", []string{"registering", "waiting_code"}).
		Updates(map[string]any{"status": "register_failed", "note": "程序重启中断，可重新注册"})
}

func reclaimOrphanAdobeRegistering(db *gorm.DB) {
	db.Model(&models.AdobeRegistration{}).Where("status IN ?", []string{"registering", "waiting_code"}).
		Updates(map[string]any{"status": "register_failed", "note": "程序重启中断，可重新注册"})
}

func reclaimOrphanLeonardoRegistering(db *gorm.DB) {
	db.Model(&models.LeonardoRegistration{}).Where("status IN ?", []string{"registering", "waiting_code"}).
		Updates(map[string]any{"status": "register_failed", "note": "程序重启中断，可重新注册"})
}

func reclaimOrphanHiggsfieldRegistering(db *gorm.DB) {
	db.Model(&models.HiggsfieldRegistration{}).Where("status IN ?", []string{"registering", "waiting_code"}).
		Updates(map[string]any{"status": "register_failed", "note": "程序重启中断，可重新注册"})
}

func reclaimOrphanLuminaRegistering(db *gorm.DB) {
	db.Model(&models.LuminaRegistration{}).Where("status IN ?", []string{"registering", "waiting_code"}).
		Updates(map[string]any{"status": "register_failed", "note": "程序重启中断，可重新注册"})
}

func reclaimOrphanOreateRegistering(db *gorm.DB) {
	db.Model(&models.OreateRegistration{}).Where("status = ?", "registering").
		Updates(map[string]any{"status": "register_failed", "note": "程序重启中断，可重新注册"})
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
