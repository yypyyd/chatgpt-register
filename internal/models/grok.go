package models

import "time"

// GrokRegistration 是独立于 ChatGPT/Codex 账号池的 Grok 注册记录。
// 后续新增平台时，应新增各自的记录表和页面，避免把不同平台的状态混在一起。
type GrokRegistration struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	Email     string `gorm:"size:255;not null;uniqueIndex" json:"email"`
	MailboxID uint   `gorm:"index" json:"mailbox_id"`
	Password  string `gorm:"size:255" json:"-"`
	Status    string `gorm:"size:32;default:pending" json:"status"`
	Shipped   bool   `gorm:"default:false" json:"shipped"`
	AuthData  string `gorm:"type:text" json:"auth_data,omitempty"`
	Log       string `gorm:"type:text" json:"log,omitempty"`
	Shot      []byte `gorm:"type:blob" json:"-"`
	Note      string `gorm:"type:text" json:"note"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
