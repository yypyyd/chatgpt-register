package producer

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
)

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

// proxyList 把多行/逗号分隔的代理串拆成切片，去空行。
func proxyList(raw string) []string {
	raw = strings.ReplaceAll(raw, ",", "\n")
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// isRotatable 判断代理是否为可换出口 IP 的 bestgo 住宅代理
// （用户名带 zone-custom / -session- 特征，或 host 含 bestgo）。
// 只有可轮换代理，Terms 拒绝时才值得换 IP 重试。
func isRotatable(raw string) bool {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return false
	}
	return strings.Contains(s, "bestgo") || strings.Contains(s, "zone-custom") || strings.Contains(s, "-session-")
}

// rotateBestgoSession 给 bestgo 住宅代理换一个新的随机 session
// （bestgo 规则：用户名不带 session 时每请求换 IP，带 -session-xxx 则该 session 内粘住同一 IP）。
// 这里为每次注册尝试挂一个随机 session：单次流程内出口 IP 一致（避免指纹错乱），
// 每次重试换新 session = 换新住宅 IP。非 bestgo/无法解析的代理原样返回。
func rotateBestgoSession(raw string) string {
	raw = strings.TrimSpace(raw)
	if !isRotatable(raw) || !strings.Contains(raw, "://") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	user := u.User.Username()
	pass, hasPass := u.User.Password()
	if i := strings.Index(user, "-session-"); i >= 0 { // 去掉已有的 -session- 后缀
		user = user[:i]
	}
	user += "-session-" + randToken(8)
	if hasPass {
		u.User = url.UserPassword(user, pass)
	} else {
		u.User = url.User(user)
	}
	return u.String()
}

// randToken 返回 2n 位十六进制随机串。
func randToken(n int) string {
	b := make([]byte, n)
	if _, err := cryptorand.Read(b); err != nil {
		return "sess" + strconv.Itoa(n)
	}
	return hex.EncodeToString(b)
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// mask 隐去邮箱本地部分中段，避免日志泄露完整地址。
func mask(email string) string {
	at := strings.Index(email, "@")
	if at <= 0 {
		return email
	}
	local, domain := email[:at], email[at:]
	if len(local) <= 2 {
		return local[:1] + "*" + domain
	}
	return local[:2] + strings.Repeat("*", len(local)-2) + domain
}
