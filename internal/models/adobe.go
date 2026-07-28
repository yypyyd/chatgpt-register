package models

import "time"

// AdobeRegistration 是独立于 ChatGPT/Codex 与 Grok 账号池的 Adobe 注册记录。
// 与 GrokRegistration 一样单独建表，避免把不同平台的状态混在一起。
// 注册成功后 AuthData 里保存 Adobe 登录会话（Cookie/Storage），供三种格式导出。
type AdobeRegistration struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	Email     string `gorm:"size:255;not null;uniqueIndex" json:"email"`
	MailboxID uint   `gorm:"index" json:"mailbox_id"`
	Password  string `gorm:"size:255" json:"-"`
	Product   string `gorm:"size:32;default:firefly" json:"product"`
	Status    string `gorm:"size:32;default:pending" json:"status"`
	Shipped   bool   `gorm:"default:false" json:"shipped"`
	AuthData  string `gorm:"type:text" json:"-"`
	Log       string `gorm:"type:text" json:"log,omitempty"`
	Shot      []byte `gorm:"type:blob" json:"-"`
	Note      string `gorm:"type:text" json:"note"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
