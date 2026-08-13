package models

import "time"

// LeonardoRegistration 是独立于 ChatGPT/Grok/Adobe 账号池的 Leonardo.Ai 注册记录。
// 与其它平台一样单独建表，避免把不同平台的状态混在一起。
// 注册成功后 AuthData 里保存 Leonardo 站点 Cookie（含 better-auth 会话 cookie），供三种格式导出。
type LeonardoRegistration struct {
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

	// 手动测活结果：alive / dead / unknown（空=未测），unknown 不判死。
	Alive          string     `gorm:"size:16;default:''" json:"alive"`
	AliveCheckedAt *time.Time `json:"alive_checked_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
