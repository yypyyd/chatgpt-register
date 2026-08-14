package models

import "time"

// OreateRegistration 是独立于其它平台账号池的 Oreate AI（oreateai.com）注册记录。
// 注册走全协议：邮箱提交 → 邮件里的确认链接 → 登录，AuthData 里保存站点会话
// Cookie（含 ouss 会话票据）供 2api 导出。
// 注册后积分自动到账，随后调用 Kling3.0 Omini 1k 生成一张图再自动加赠积分，
// Points 记录生图后的积分快照，ImageURL 记录出图地址。
type OreateRegistration struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	Email     string `gorm:"size:255;not null;uniqueIndex" json:"email"`
	MailboxID uint   `gorm:"index" json:"mailbox_id"`
	Password  string `gorm:"size:255" json:"-"`
	Status    string `gorm:"size:32;default:pending" json:"status"`
	Shipped   bool   `gorm:"default:false" json:"shipped"`
	AuthData  string `gorm:"type:text" json:"-"`
	Log       string `gorm:"type:text" json:"log,omitempty"`
	Note      string `gorm:"type:text" json:"note"`

	Points   int    `gorm:"default:0" json:"points"`
	ImageURL string `gorm:"type:text" json:"image_url"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
