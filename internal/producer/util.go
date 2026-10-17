package producer

import (
	"strconv"
	"strings"
)

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
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
