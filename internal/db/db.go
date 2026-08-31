package db

import (
	"chatgpt-register/internal/emailalias"
	"chatgpt-register/internal/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func Init(path string) (*gorm.DB, error) {
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		return nil, err
	}
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
