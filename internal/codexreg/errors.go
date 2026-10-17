package codexreg

import "errors"

// ErrEmailCodeNotReceived 仅用于邮箱读取正常、等待期内没有匹配验证邮件的情况。
// 鉴权、邮件列表、正文读取和验证码解析错误不能归入此类。
var ErrEmailCodeNotReceived = errors.New("超时未收到验证码邮件")

// ErrAccountTaken 注册时提示账号停用/已删除，视为该地址不可再用。
var ErrAccountTaken = errors.New("账号不存在或已被删除/停用")

// ErrEmailExists create_account 返回 user_already_exists。
// OTP 已通过并进入 about-you，说明本次别名曾被当成新号；随后建号失败通常是
// OpenAI 把 plus addressing 归一到母号（母号已存在），不代表这个 +00x 自己注册成功过。
var ErrEmailExists = errors.New("OpenAI 拒绝建号：该邮箱（或归一后的母号）已存在")

// ErrTermsRejected 填完资料提交后命中 "We can't create your account due to our Terms of Use"
// 拒绝页——通常是出口 IP 风控命中。原地重试无意义，交由上层换出口 IP 后重试或标记为不可注册。
var ErrTermsRejected = errors.New("账号创建被 Terms of Use 拒绝")

// ErrIPBlocked 出口 IP 被 Cloudflare / OpenAI 拦下：人机验证或提交后无响应。
// 与账号无关，换出口 IP 重试即可，不应按"邮箱失败"进入冷却。
var ErrIPBlocked = errors.New("出口 IP 被拦截（Cloudflare 人机验证 / 提交无响应）")
