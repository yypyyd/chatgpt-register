package codexreg

import (
	"encoding/json"
	"fmt"
	"strings"
)

// WebCookie is the portable part of a ChatGPT web session cookie.
// Values are credentials and must never be logged.
type WebCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires,omitempty"`
	HTTPOnly bool    `json:"httpOnly,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
	Session  bool    `json:"session,omitempty"`
	SameSite string  `json:"sameSite,omitempty"`
	Priority string  `json:"priority,omitempty"`
}

func isChatGPTCookieDomain(domain string) bool {
	d := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
	return d == "chatgpt.com" || strings.HasSuffix(d, ".chatgpt.com") ||
		d == "openai.com" || strings.HasSuffix(d, ".openai.com")
}

// WebSessionData 是恢复网页登录所需的 Cookie、出口和现场信息。
type WebSessionData struct {
	AccessToken string
	Cookies     []WebCookie
	Proxy       string
	UserAgent   string
	Screen      *ScreenProfile
	Timezone    string
	Locale      string
	Languages   string
}

// WebSessionFromAuthData 同时兼容早期的 "1920x1080" 屏幕字符串和新版结构化屏幕参数。
func WebSessionFromAuthData(authData string) (WebSessionData, error) {
	var envelope struct {
		AccessToken string          `json:"access_token"`
		Cookies     []WebCookie     `json:"cookies"`
		Proxy       string          `json:"proxy"`
		UserAgent   string          `json:"user_agent"`
		Screen      json.RawMessage `json:"screen"`
		Timezone    string          `json:"registered_timezone"`
		Locale      string          `json:"registered_locale"`
		Languages   string          `json:"registered_languages"`
	}
	if err := json.Unmarshal([]byte(authData), &envelope); err != nil {
		return WebSessionData{}, err
	}
	out := WebSessionData{
		AccessToken: envelope.AccessToken, Cookies: envelope.Cookies, Proxy: envelope.Proxy,
		UserAgent: envelope.UserAgent, Timezone: envelope.Timezone,
		Locale: envelope.Locale, Languages: envelope.Languages,
	}
	if len(envelope.Screen) > 0 && string(envelope.Screen) != "null" {
		var screen ScreenProfile
		if json.Unmarshal(envelope.Screen, &screen) == nil && screen.valid() {
			out.Screen = &screen
		} else {
			var text string
			if json.Unmarshal(envelope.Screen, &text) == nil {
				if _, err := fmt.Sscanf(text, "%dx%d", &screen.Width, &screen.Height); err == nil {
					screen.Toolbar = 90
					if screen.valid() {
						out.Screen = &screen
					}
				}
			}
		}
	}
	return out, nil
}

// WebCookiesFromAuthData reads cookies from a registration auth_data JSON.
func WebCookiesFromAuthData(authData string) ([]WebCookie, error) {
	session, err := WebSessionFromAuthData(authData)
	return session.Cookies, err
}

// WebCookieHeader returns a conventional Cookie header string for exports.
func WebCookieHeader(cookies []WebCookie) string {
	parts := make([]string, 0, len(cookies))
	for _, c := range cookies {
		if c.Name != "" && isChatGPTCookieDomain(c.Domain) {
			parts = append(parts, c.Name+"="+c.Value)
		}
	}
	return strings.Join(parts, "; ")
}
