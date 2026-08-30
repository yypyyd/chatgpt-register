package models

import "time"

// HiggsfieldRegistration 一条 higgsfield.ai 注册任务/账号记录。
// 注册走 Clerk 协议，会话（含 __client cookie 与会话 JWT）存在 AuthData 里；
// TrialStatus / CheckoutURL 记录 pricing 页绑卡优惠流程跑到哪一步。
type HiggsfieldRegistration struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	Email     string `gorm:"size:255;not null;uniqueIndex" json:"email"`
	MailboxID uint   `gorm:"index" json:"mailbox_id"`
	Password  string `gorm:"size:255" json:"-"`
	Status    string `gorm:"size:32;default:pending" json:"status"`
	Shipped   bool   `gorm:"default:false" json:"shipped"`
	AuthData  string `gorm:"type:text" json:"-"`
	Log       string `gorm:"type:text" json:"log,omitempty"`
	Note      string `gorm:"type:text" json:"note"`

	// TrialStatus 绑卡优惠状态：need_card（收银台已开好，等真实卡）/ already_active /
	// not_eligible / failed；空表示还没跑过。
	TrialStatus string `gorm:"size:32;default:''" json:"trial_status"`
	// CheckoutURL Stripe 收银台地址，卡信息在这里填。
	CheckoutURL string `gorm:"type:text" json:"checkout_url,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
