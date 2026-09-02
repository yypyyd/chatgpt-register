// Package prodcore 收拢各平台 producer 共用的那层逻辑：系统设置读取、并发槽位闸门、
// 代理池轮换、账号日志追加。这些行为与具体平台无关，之前在每个 producer 里各存一份。
package prodcore

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"chatgpt-register/internal/models"
	"chatgpt-register/internal/proxyutil"

	"gorm.io/gorm"
)

// defaultMaxConcurrency 未配置并发时的默认值：逐个开工。批量注册时多个浏览器
// 同时抢 CPU 会互相超时，串行最稳，可用设置页「最大并发数」调大。
const defaultMaxConcurrency = 1

// Core 由各平台 Producer 嵌入。自带一把锁，只保护并发计数与代理游标，
// 与 Producer 自己的锁互不影响。
type Core struct {
	db *gorm.DB

	mu     sync.Mutex
	active int // 已获得并发槽位、真正在跑的任务数
	pxIdx  int // 代理池轮转游标
}

func New(db *gorm.DB) *Core {
	return &Core{db: db}
}

// Setting 读一个系统设置项，未设置返回空串。
func (c *Core) Setting(key string) string {
	var s models.Setting
	if err := c.db.Where("key = ?", key).First(&s).Error; err != nil {
		return ""
	}
	return s.Value
}

// SettingOn 判断开关型设置是否打开（只有显式 "1" 才算开）。
func (c *Core) SettingOn(key string) bool {
	return strings.TrimSpace(c.Setting(key)) == "1"
}

// SettingInt 读一个非负整型设置，未设置或非法时用默认值。
func (c *Core) SettingInt(key string, def int) int {
	raw := strings.TrimSpace(c.Setting(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// SettingMinutes 读一个以分钟为单位的时长设置，0 表示不限。
func (c *Core) SettingMinutes(key string, defMin int) time.Duration {
	return time.Duration(c.SettingInt(key, defMin)) * time.Minute
}

// MaxConcurrency 并发上限，跟设置页「最大并发数」(max_concurrency)，最小为 1。
func (c *Core) MaxConcurrency() int {
	n := c.SettingInt("max_concurrency", defaultMaxConcurrency)
	if n < 1 {
		return 1
	}
	return n
}

// AcquireSlot 阻塞直到并发未满（获得槽位返回 true）或 ctx 取消（返回 false）。
// 限额每轮都重新读设置，改大后新任务无需重启即可生效。onQueued 在首次需要排队时
// 调用一次，供调用方写一行“并发已满，排队等待”的日志。
func (c *Core) AcquireSlot(ctx context.Context, onQueued func()) bool {
	queued := false
	for {
		if ctx.Err() != nil {
			return false
		}
		limit := c.MaxConcurrency()
		c.mu.Lock()
		if c.active < limit {
			c.active++
			c.mu.Unlock()
			return true
		}
		c.mu.Unlock()
		if !queued {
			queued = true
			if onQueued != nil {
				onQueued()
			}
		}
		if !Sleep(ctx, time.Second) {
			return false
		}
	}
}

// ReleaseSlot 释放一个并发槽位。
func (c *Core) ReleaseSlot() {
	c.mu.Lock()
	if c.active > 0 {
		c.active--
	}
	c.mu.Unlock()
}

// NextProxy 跟设置页上的全局代理开关与代理列表，按任务轮换出口；
// 关闭或代理池为空时返回空串（直连）。
func (c *Core) NextProxy() string {
	if !c.SettingOn("proxy_enabled") {
		return ""
	}
	proxies := proxyutil.List(c.Setting("proxy_list"))
	if len(proxies) == 0 {
		return ""
	}
	c.mu.Lock()
	proxy := proxies[c.pxIdx%len(proxies)]
	c.pxIdx++
	c.mu.Unlock()
	return proxyutil.WithBestGoTaskSession(proxy)
}

// AppendLogLine 给账号执行日志追加一行（带时间戳），超过 maxBytes 时保留尾部。
func AppendLogLine(existing, line string, maxBytes int) string {
	out := existing
	if strings.TrimSpace(out) == "" {
		out = ""
	} else if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	out += time.Now().Format("2006-01-02 15:04:05") + " " + line + "\n"
	if maxBytes > 0 && len(out) > maxBytes {
		out = out[len(out)-maxBytes:]
	}
	return out
}

// Truncate 把字符串截到 n 字节以内，用于写入长度有限的 note 字段。
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Sleep 等一段时间；ctx 取消时立刻返回 false。
func Sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
