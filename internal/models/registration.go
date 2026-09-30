package models

import "time"

// Registration 一个待生产 / 已生产的 ChatGPT + Codex 账号。
//
// Status 流转:
//
//	pending(待生产) / registering(注册中) / registered(已注册) / register_failed(注册失败)
//
// 生产成功后 AuthData 存完整的 auth.json（agent_identity 结构），下载时导出。
// Shipped 表示是否已"出库"（下载即出库）。
type Registration struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	Email     string `gorm:"size:255;not null;uniqueIndex" json:"email"`
	MailboxID uint   `gorm:"index" json:"mailbox_id"`
	Password  string `gorm:"size:255" json:"password"`
	Username  string `gorm:"size:255" json:"username"`

	Status  string `gorm:"size:32;default:pending" json:"status"`
	Shipped bool   `gorm:"default:false" json:"shipped"` // 出库状态：true=已出库

	// 生产结果
	AuthData  string `gorm:"type:text" json:"auth_data,omitempty"` // 完整 auth.json
	AccountID string `gorm:"size:255" json:"account_id"`
	UserID    string `gorm:"size:255" json:"user_id"`
	PlanType  string `gorm:"size:32" json:"plan_type"`
	IsMother  bool   `gorm:"default:false" json:"is_mother"` // 是否母号（该邮箱主号）

	Log  string `gorm:"type:text" json:"log,omitempty"` // 本账号执行日志
	Shot []byte `gorm:"type:blob" json:"-"`             // 注册失败时的页面截图(PNG)
	Note string `gorm:"type:text" json:"note"`

	// 手动测活结果：alive(有效) / dead(明确失效) / unknown(网络·CF·限流等无法确定)；空=未测过。
	// Deprecated: 仅为旧客户端兼容保留。新代码分别使用 AccountCredentialStatus 和
	// AccessTokenStatus，不能再把网页登录态与当前 access token 混成一个“存活”状态。
	Alive          string     `gorm:"size:16;default:''" json:"alive"`
	AliveCheckedAt *time.Time `json:"alive_checked_at,omitempty"`

	// 长期凭据（refresh_token 或 ChatGPT Web Session）与短期 access token 分开建模。
	// account_credential_status: valid / needs_relogin / unknown
	// access_token_status: valid / revoked / expired / unknown / unchecked
	AccountCredentialStatus string     `gorm:"size:24;default:unknown;index" json:"account_credential_status"`
	AccessTokenStatus       string     `gorm:"size:16;default:unchecked;index" json:"access_token_status"`
	TokenIssuedAt           *time.Time `json:"token_issued_at,omitempty"`
	TokenExpiresAt          *time.Time `json:"token_expires_at,omitempty"`
	TokenLastVerifiedAt     *time.Time `json:"token_last_verified_at,omitempty"`
	TokenLastErrorCode      string     `gorm:"size:64" json:"token_last_error_code,omitempty"`
	RefreshLastSuccessAt    *time.Time `json:"refresh_last_success_at,omitempty"`
	RefreshLastError        string     `gorm:"size:128" json:"refresh_last_error,omitempty"`
	NeedsRelogin            bool       `gorm:"default:false;index" json:"needs_relogin"`

	// 只保存不可逆 SHA-256 指纹及非秘密 JWT 元数据；绝不把 token 放入列表或日志。
	AccessTokenFingerprint string    `gorm:"size:64;index" json:"-"`
	AccessTokenJTI         string    `gorm:"size:128" json:"-"`
	CredentialVersion      uint64    `gorm:"default:0;not null" json:"-"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}
