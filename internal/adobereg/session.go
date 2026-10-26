package adobereg

import (
	"context"
	"fmt"
	"strings"
)

// RestoreSession 检查保存的 Cookie，仅在 Adobe 要求邮箱身份核验时恢复浏览器。
// 注册端不生成生图用的 ARP；下游应在实际生成时维护自己的生成会话。
func RestoreSession(ctx context.Context, in Input, auth map[string]any) (map[string]any, error) {
	cookies := cookiesFromAuthMap(auth)
	if len(cookies) == 0 {
		return nil, fmt.Errorf("没有可恢复的登录 Cookie")
	}
	jump, err := adobeRideJump(ctx, cookieHeaderFromAuth(auth), in.Proxy)
	if err != nil {
		return nil, err
	}
	merged := make(map[string]any, len(auth))
	for k, v := range auth {
		merged[k] = v
	}
	if jump != "" {
		if in.WaitCode == nil {
			return nil, fmt.Errorf("Adobe 会话需要邮箱身份核验")
		}
		res, err := RescueRide(ctx, in, cookies)
		if err != nil {
			return nil, err
		}
		if res == nil || res.AuthJSON == nil {
			return nil, fmt.Errorf("身份核验未返回登录会话")
		}
		if jump, err := adobeRideJump(ctx, cookieHeaderFromAuth(res.AuthJSON), in.Proxy); err != nil {
			return nil, err
		} else if jump != "" {
			return nil, fmt.Errorf("Adobe 邮箱身份核验尚未完成")
		}
		for _, key := range []string{"cookies", "storage", "captured_at"} {
			if value, ok := res.AuthJSON[key]; ok {
				merged[key] = value
			}
		}
	}
	for key := range merged {
		switch strings.ToLower(key) {
		case "arp_session_token", "sherlocktoken", "x-arp-session-id", "arp_captured_at", "arp_error":
			delete(merged, key)
		}
	}
	in.logf("Cookie 登录会话已验证；生图会话由下游维护")
	return merged, nil
}
