// Package proxypool owns channel-specific proxy configuration and allocation.
package proxypool

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"chatgpt-register/internal/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const MigrationKey = "internal_channel_proxy_migration_v1"

var Channels = []string{"chatgpt", "grok", "adobe", "leonardo", "lovart"}

var labels = map[string]string{
	"chatgpt": "GPT", "grok": "Grok", "adobe": "Adobe",
	"leonardo": "Leonardo", "lovart": "Lovart",
}

type Config struct {
	Enabled bool   `json:"enabled"`
	Source  string `json:"source"`
	APIURL  string `json:"api_url"`
	List    string `json:"list"`
}

func ValidChannel(channel string) bool { return labels[channel] != "" }
func Label(channel string) string      { return labels[channel] }

// RegistrationBudget includes the existing maximum for the longer workflows.
// All API leases also impose a deadline before the provider's rotation window.
func RegistrationBudget(channel string) time.Duration {
	switch channel {
	case "chatgpt":
		return 8 * time.Minute
	default:
		return 5 * time.Minute
	}
}

func settingPrefix(channel string) string { return "proxy_" + channel + "_" }

func IsSettingKey(key string) bool {
	// Keep proxy credentials out of general settings, including historical
	// channel keys after a channel is removed.
	return key == MigrationKey || strings.HasPrefix(key, "proxy_") || strings.Contains(key, "_proxy_")
}

func ReadConfig(db *gorm.DB, channel string) (Config, error) {
	if !ValidChannel(channel) {
		return Config{}, fmt.Errorf("未知代理渠道")
	}
	prefix := settingPrefix(channel)
	var items []models.Setting
	if err := db.Where("key IN ?", []string{prefix + "enabled", prefix + "source", prefix + "api_url", prefix + "list"}).Find(&items).Error; err != nil {
		return Config{}, fmt.Errorf("读取渠道代理配置失败")
	}
	cfg := Config{Source: "api"}
	for _, item := range items {
		switch item.Key {
		case prefix + "enabled":
			cfg.Enabled = item.Value == "1"
		case prefix + "source":
			cfg.Source = item.Value
		case prefix + "api_url":
			cfg.APIURL = item.Value
		case prefix + "list":
			cfg.List = item.Value
		}
	}
	return cfg, nil
}

func (c Config) normalized() Config {
	c.Source = strings.TrimSpace(c.Source)
	if c.Source == "" {
		c.Source = "api"
	}
	c.APIURL = strings.TrimSpace(c.APIURL)
	c.List = strings.TrimSpace(c.List)
	return c
}

func (c Config) Validate() error {
	c = c.normalized()
	if c.Source != "api" && c.Source != "list" {
		return fmt.Errorf("代理来源必须是 API 或独立列表")
	}
	// A broken API/list must never prevent the independent switch being turned
	// off. Inactive source drafts are preserved but do not affect allocation.
	if !c.Enabled {
		return nil
	}
	if c.Source == "api" {
		if c.APIURL == "" {
			return fmt.Errorf("启用代理前请填写该渠道的提取 API")
		}
		if _, err := parseAPIURL(c.APIURL); err != nil {
			return err
		}
		if _, err := c.sessionDuration(); err != nil {
			return err
		}
	}
	if c.Source == "list" {
		if c.List == "" {
			return fmt.Errorf("启用代理前请填写该渠道的代理列表")
		}
		if _, err := parseList(c.List); err != nil {
			return err
		}
	}
	return nil
}

func parseAPIURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Fragment != "" {
		return nil, fmt.Errorf("提取 API 必须是有效的 HTTP 或 HTTPS 地址")
	}
	if _, err := url.ParseQuery(u.RawQuery); err != nil {
		return nil, fmt.Errorf("提取 API 的查询参数格式错误")
	}
	return u, nil
}

func (c Config) sessionDuration() (time.Duration, error) {
	if c.Source != "api" || c.APIURL == "" {
		return 0, nil
	}
	u, err := parseAPIURL(c.APIURL)
	if err != nil {
		return 0, err
	}
	if mode := u.Query().Get("sessType"); mode != "" && mode != "sticky" {
		return 0, fmt.Errorf("注册代理需要粘性会话，请将 sessType 设置为 sticky")
	}
	raw := u.Query().Get("sessTime")
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 10080 {
		return 0, fmt.Errorf("API 的 sessTime 必须是 1 至 10080 的分钟数")
	}
	return time.Duration(n) * time.Minute, nil
}

func (c Config) fingerprint() string {
	hash := sha256.Sum256([]byte(fmt.Sprintf("%t\x00%s\x00%s\x00%s", c.Enabled, c.Source, c.APIURL, c.List)))
	return hex.EncodeToString(hash[:])
}

// Migrate snapshots the effective legacy configuration once. New runtime code
// never falls back to global keys.
func Migrate(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var items []models.Setting
		if err := tx.Where("key IN ?", []string{MigrationKey, "proxy_enabled", "proxy_list"}).Find(&items).Error; err != nil {
			return err
		}
		old := map[string]string{}
		for _, item := range items {
			old[item.Key] = item.Value
		}
		if old[MigrationKey] == "done" {
			return nil
		}
		list := strings.TrimSpace(old["proxy_list"])
		for _, channel := range Channels {
			cfg := Config{Source: "api"}
			if list != "" {
				cfg.Source, cfg.List = "list", list
				cfg.Enabled = old["proxy_enabled"] == "1"
				if channel == "chatgpt" {
					cfg.Enabled = old["proxy_enabled"] != "0"
				}
			}
			for key, value := range configValues(channel, cfg) {
				if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&models.Setting{Key: key, Value: value}).Error; err != nil {
					return err
				}
			}
		}
		return tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&models.Setting{Key: MigrationKey, Value: "done"}).Error
	})
}

func configValues(channel string, cfg Config) map[string]string {
	enabled := "0"
	if cfg.Enabled {
		enabled = "1"
	}
	prefix := settingPrefix(channel)
	return map[string]string{prefix + "enabled": enabled, prefix + "source": cfg.Source, prefix + "api_url": cfg.APIURL, prefix + "list": cfg.List}
}
