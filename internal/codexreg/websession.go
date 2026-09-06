package codexreg

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// WebCookie is the portable part of a Chrome cookie needed to restore a
// ChatGPT browser session. Values are credentials and must never be logged.
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

// CaptureWebCookies captures only OpenAI/ChatGPT cookies from the current
// browser context. Keeping the domain/path/httpOnly metadata is required for
// restoring the session; an access token alone is not a browser session.
func CaptureWebCookies(browser *rod.Browser) ([]WebCookie, error) {
	if browser == nil {
		return nil, fmt.Errorf("浏览器会话为空")
	}
	raw, err := browser.GetCookies()
	if err != nil {
		return nil, fmt.Errorf("读取网页 Cookie 失败: %w", err)
	}
	out := make([]WebCookie, 0, len(raw))
	for _, c := range raw {
		if c == nil || c.Name == "" || !isChatGPTCookieDomain(c.Domain) {
			continue
		}
		out = append(out, WebCookie{
			Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
			Expires: float64(c.Expires), HTTPOnly: c.HTTPOnly, Secure: c.Secure,
			Session: c.Session, SameSite: string(c.SameSite), Priority: string(c.Priority),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("登录成功但没有捕获到 ChatGPT 网页 Cookie")
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Domain != out[j].Domain {
			return out[i].Domain < out[j].Domain
		}
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func isChatGPTCookieDomain(domain string) bool {
	d := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
	return d == "chatgpt.com" || strings.HasSuffix(d, ".chatgpt.com") ||
		d == "openai.com" || strings.HasSuffix(d, ".openai.com")
}

// WebCookieParams converts persisted cookies back to CDP parameters.
func WebCookieParams(cookies []WebCookie) []*proto.NetworkCookieParam {
	out := make([]*proto.NetworkCookieParam, 0, len(cookies))
	for _, c := range cookies {
		if c.Name == "" || c.Domain == "" || !isChatGPTCookieDomain(c.Domain) {
			continue
		}
		expires := proto.TimeSinceEpoch(c.Expires)
		if c.Session && c.Expires == 0 {
			expires = -1
		}
		out = append(out, &proto.NetworkCookieParam{
			Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
			Expires: expires, HTTPOnly: c.HTTPOnly, Secure: c.Secure,
			SameSite: proto.NetworkCookieSameSite(c.SameSite),
			Priority: proto.NetworkCookiePriority(c.Priority),
		})
	}
	return out
}

// WebSessionData 是恢复网页登录所需的 Cookie、出口和浏览器现场。
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
