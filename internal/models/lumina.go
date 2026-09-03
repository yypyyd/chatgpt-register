package models

import "time"

// LuminaRegistration 是独立于其它平台账号池的 BytePlus Lumina（ai.byteplus.com/lumina）注册记录。
// 与其它平台一样单独建表，避免把不同平台的状态混在一起。
// 注册成功后 AuthData 里保存 console.byteplus.com 域 + Lumina 侧的全部 Cookie（含 digest/AccountID
// 会话 cookie）。导出给 2API 的是 email + password + cookie_string，会话到期后由 2API 用账密自行重登。
type LuminaRegistration struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	Email     string `gorm:"size:255;not null;uniqueIndex" json:"email"`
	MailboxID uint   `gorm:"index" json:"mailbox_id"`
	Password  string `gorm:"size:255" json:"-"`
	Status    string `gorm:"size:32;default:pending" json:"status"`
	Shipped   bool   `gorm:"default:false" json:"shipped"`
	AuthData  string `gorm:"type:text" json:"-"`
	Log       string `gorm:"type:text" json:"log,omitempty"`
	Shot      []byte `gorm:"type:blob" json:"-"`
	Note      string `gorm:"type:text" json:"note"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
