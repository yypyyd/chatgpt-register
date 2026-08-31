// Package luminareg 完成 BytePlus Lumina（ai.byteplus.com/lumina）的邮箱注册，
// 并采集 BytePlus 账号会话 Cookie（digest / AccountID / userInfo）供导出。
// 全程纯协议链路（protocol.go），不依赖浏览器。
package luminareg

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"
)

// ErrEmailTaken 该邮箱已有 BytePlus 账号，属于永久失败：换出口/重试都没用。
var ErrEmailTaken = errors.New("该邮箱已注册 BytePlus")

// ErrRegionBlocked 当前出口 IP 不在 BytePlus Lumina 的开放地区。
var ErrRegionBlocked = errors.New("BytePlus Lumina 在当前出口地区不可用")

// ErrCaptchaFailed 滑块人机校验没过。BytePlus 会在多次失败后临时限流。
var ErrCaptchaFailed = errors.New("BytePlus 滑块人机校验未通过")

// ErrRateLimited 注册接口被 BytePlus 限流（同一出口 IP 注册过密）。
var ErrRateLimited = errors.New("BytePlus 注册接口限流")

type Input struct {
	Email    string
	Password string
	Proxy    string

	// WaitCode 返回 BytePlus 发到邮箱的 6 位注册验证码。
	WaitCode func(ctx context.Context) (string, error)
	Log      func(format string, a ...any)
}

type Result struct {
	AuthJSON map[string]any `json:"auth_json"`
}

func (in Input) logf(format string, a ...any) {
	if in.Log != nil {
		in.Log(format, a...)
	}
}

// Register 走 Lumina 站内的 BytePlus 注册流程（纯协议）：
// 邮箱 → 创建密码并同意条款 → 邮箱验证码 → 滑块验证 → 采集会话 Cookie。
func Register(ctx context.Context, in Input) (*Result, error) {
	if in.WaitCode == nil {
		return nil, fmt.Errorf("缺少验证码回调")
	}
	if in.Email == "" {
		return nil, fmt.Errorf("缺少邮箱")
	}
	if in.Password == "" {
		in.Password = GenPassword(16)
	}
	// 单次注册硬上限：主要花在等邮件验证码，整体超时兜底。
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	return registerProtocol(ctx, in)
}

const (
	// luminaURL Lumina 首页，也用作协议请求的 Referer。
	luminaURL = "https://ai.byteplus.com/lumina/en"

	// luminaAPIBase Lumina 控制台接口域名（与页面同站，cookie 直接生效）。
	luminaAPIBase = "https://lumi-api.console.byteplus.com"

	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

// parseAccountInfo 解析 {current, resources} 两个账号接口的原始返回，
// 提取入库需要的到期时间与套餐信息。
func parseAccountInfo(rawJSON []byte, in Input) map[string]any {
	var raw struct {
		Current struct {
			Code int `json:"code"`
			Data struct {
				UserID         string `json:"user_id"`
				UserName       string `json:"user_name"`
				Email          string `json:"email"`
				AvatarURL      string `json:"avatar_url"`
				Role           string `json:"role"`
				ExpiredTime    int64  `json:"expired_time"`
				VolcAccountID  string `json:"volc_account_id"`
				IsVolcRootUser bool   `json:"is_volc_root_user"`
				IsCountryBlock bool   `json:"is_country_blocked"`
			} `json:"data"`
		} `json:"current"`
		Resources struct {
			Code int `json:"code"`
			Data struct {
				URIs   []string `json:"uris"`
				Combos []struct {
					ID        int64  `json:"id"`
					Name      string `json:"name"`
					BeginDt   int64  `json:"begin_dt"`
					EndDt     int64  `json:"end_dt"`
					Status    int    `json:"status"`
					ComboType int    `json:"combo_type"`
				} `json:"combos"`
			} `json:"data"`
		} `json:"resources"`
	}
	if jerr := json.Unmarshal(rawJSON, &raw); jerr != nil {
		in.logf("解析账号元信息失败: %v", jerr)
		return nil
	}
	cur := raw.Current.Data
	if cur.UserID == "" {
		in.logf("账号元信息接口未返回 user_id，跳过")
		return nil
	}
	info := map[string]any{
		"user_name":          cur.UserName,
		"account_email":      cur.Email,
		"avatar_url":         cur.AvatarURL,
		"role":               cur.Role,
		"expired_time":       cur.ExpiredTime,
		"volc_account_id":    cur.VolcAccountID,
		"is_volc_root_user":  cur.IsVolcRootUser,
		"is_country_blocked": cur.IsCountryBlock,
	}
	if cur.ExpiredTime > 0 {
		info["expired_at"] = time.Unix(cur.ExpiredTime, 0).UTC().Format(time.RFC3339)
	}
	res := raw.Resources.Data
	if len(res.URIs) > 0 {
		info["resource_uris"] = res.URIs
	}
	if len(res.Combos) > 0 {
		c := res.Combos[0]
		info["combo_id"] = c.ID
		info["combo_name"] = c.Name
		info["combo_status"] = c.Status
		info["combo_type"] = c.ComboType
		info["combo_begin_dt"] = c.BeginDt
		info["combo_end_dt"] = c.EndDt
		if c.EndDt > 0 {
			info["combo_end_at"] = time.Unix(c.EndDt, 0).UTC().Format(time.RFC3339)
		}
	}
	in.logf("已采集账号元信息: 邮箱=%s 套餐=%v", cur.Email, info["combo_name"])
	return info
}

func normalizeProxy(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "://") {
		return raw
	}
	parts := strings.Split(raw, ":")
	switch len(parts) {
	case 2:
		return "http://" + parts[0] + ":" + parts[1]
	case 4:
		return "http://" + url.QueryEscape(parts[2]) + ":" + url.QueryEscape(parts[3]) + "@" + parts[0] + ":" + parts[1]
	default:
		return "http://" + raw
	}
}

func trimText(s string, n int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func ri(n int) int {
	if n <= 0 {
		return 0
	}
	v, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

// GenPassword 生成满足 BytePlus 密码强度（>=8 位，含大小写、数字与符号）的随机密码。
func GenPassword(n int) string {
	const lower = "abcdefghijkmnpqrstuvwxyz"
	const upper = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	const digit = "23456789"
	const symbol = "!@#$%_-"
	all := lower + upper + digit
	if n < 12 {
		n = 12
	}
	b := make([]byte, n)
	b[0] = upper[ri(len(upper))]
	b[1] = lower[ri(len(lower))]
	b[2] = digit[ri(len(digit))]
	b[3] = symbol[ri(len(symbol))]
	for i := 4; i < n; i++ {
		b[i] = all[ri(len(all))]
	}
	return string(b)
}
