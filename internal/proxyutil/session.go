package proxyutil

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// sessionParamRe 匹配 BestGo 用户名里的 -session-<值> 片段（值不含连字符）。
var sessionParamRe = regexp.MustCompile(`(?i)(-session-)[^-]*`)

// WithBestGoTaskSession gives a dynamic BestGo residential proxy a stable
// task-level session. Without it, BestGo rotates the exit per request, which
// makes one browser/protocol registration appear from multiple IP addresses.
// 用户串里写死的 session 会被换成本任务的随机值：固定 session 会让所有任务共用
// 同一个出口 IP，站点很快就把这个 IP 判成 spam。
func WithBestGoTaskSession(raw string) string {
	raw = strings.TrimSpace(raw)
	lower := strings.ToLower(raw)
	if !strings.Contains(raw, "://") ||
		(!strings.Contains(lower, "bestgo") && !strings.Contains(lower, "zone-custom")) {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	user := u.User.Username()
	token := sessionToken(8)
	if sessionParamRe.MatchString(user) {
		user = sessionParamRe.ReplaceAllString(user, "${1}"+token)
	} else {
		user += "-session-" + token
	}
	pass, hasPass := u.User.Password()
	if hasPass {
		u.User = url.UserPassword(user, pass)
	} else {
		u.User = url.User(user)
	}
	return u.String()
}

func sessionToken(n int) string {
	b := make([]byte, n)
	if _, err := cryptorand.Read(b); err != nil {
		return "fallback" + strconv.Itoa(n)
	}
	return hex.EncodeToString(b)
}
