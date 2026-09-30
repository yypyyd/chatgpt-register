package codexreg

import (
	"strings"
	"testing"
)

func TestParseSentinelSDKURL(t *testing.T) {
	// 认证页里 SDK 来自 sentinel.openai.com；引导脚本里给出 chatgpt.com 或 sentinel.openai.com 的版本化地址，
	// 都统一改成认证页实际使用的 sentinel.openai.com。
	for _, boot := range [][]byte{
		[]byte(`script.src = 'https://chatgpt.com/sentinel/20260219f9f6/sdk.js';`),
		[]byte(`script.src = 'https://sentinel.openai.com/sentinel/20260219f9f6/sdk.js';`),
	} {
		got := parseSentinelSDKURL(boot)
		if got != "https://sentinel.openai.com/sentinel/20260219f9f6/sdk.js" {
			t.Fatalf("url=%s", got)
		}
		if isFullSentinelSDK(boot) {
			t.Fatal("bootstrap should not look like full sdk")
		}
	}
	boot := []byte(`script.src = 'https://chatgpt.com/sentinel/20260219f9f6/sdk.js';`)
	if !isFullSentinelSDK([]byte(`var SentinelSDK=function(t){"use strict";`)) {
		t.Fatal("full sdk not detected")
	}
	if isFullSentinelSDK(boot) {
		t.Fatal("bootstrap should not look like full sdk")
	}
}

func TestProtocolTLSProfileName(t *testing.T) {
	if got := protocolTLSProfileName(); got != "chrome_146_PSK" {
		t.Fatalf("tls profile=%q", got)
	}
}

func TestChromeFullVersion(t *testing.T) {
	if got := chromeFullVersion("146"); got != "146.0.7680.164" {
		t.Fatalf("full=%q", got)
	}
	ua := chromeUserAgent("146")
	if !strings.Contains(ua, "Chrome/146.0.7680.164") {
		t.Fatalf("ua=%q", ua)
	}
}

func TestParseChromeMajor(t *testing.T) {
	cases := map[string]string{
		"Google Chrome 151.0.3770.100": "151",
		"Chromium 128.0.6613.84":       "128",
		"Google Chrome/146.0.0.0":      "146",
		"":                             "",
		"firefox 120":                  "",
	}
	for in, want := range cases {
		if got := parseChromeMajor(in); got != want {
			t.Fatalf("parseChromeMajor(%q)=%q want %q", in, got, want)
		}
	}
}

func TestChromeMajorFromUA(t *testing.T) {
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.3770.100 Safari/537.36"
	if got := chromeMajorFromUA(ua); got != "151" {
		t.Fatalf("chromeMajorFromUA=%q", got)
	}
}

func TestDeviceIDFromCookies(t *testing.T) {
	if DeviceIDFromCookies(nil) != "" {
		t.Fatal("empty cookies should be empty")
	}
	got := DeviceIDFromCookies([]WebCookie{
		{Name: "oai-did", Value: "device-1"},
		{Name: "session", Value: "x"},
	})
	if got != "device-1" {
		t.Fatalf("DeviceIDFromCookies=%q", got)
	}
}

func TestSentinelDocumentURL(t *testing.T) {
	otp := "https://auth.openai.com/email-verification?id=abc"
	if got := sentinelDocumentURL("email_otp_validate", otp); got != otp {
		t.Fatalf("otp url=%s", got)
	}
	about := "https://auth.openai.com/about-you?step=1"
	if got := sentinelDocumentURL("oauth_create_account", about); got != about {
		t.Fatalf("about url=%s", got)
	}
	if got := sentinelDocumentURL("oauth_create_account", "https://chatgpt.com/"); got != "https://auth.openai.com/about-you" {
		t.Fatalf("about fallback=%s", got)
	}
}

func TestSentinelTokenID(t *testing.T) {
	if got := sentinelTokenID(`{"p":"x","id":"device-9","flow":"email_otp_validate"}`); got != "device-9" {
		t.Fatalf("id=%q", got)
	}
	if got := sentinelTokenID("not-json"); got != "" {
		t.Fatalf("raw id=%q", got)
	}
}

func TestPickScreenProfileLooksLikeWindows(t *testing.T) {
	for i := 0; i < 12; i++ {
		p := pickScreenProfile()
		if p.Taskbar != 40 && p.Taskbar != 48 {
			t.Fatalf("taskbar=%d want Windows 40/48", p.Taskbar)
		}
		if p.Toolbar < 85 || p.Toolbar > 130 {
			t.Fatalf("toolbar=%d", p.Toolbar)
		}
	}
}

func TestAuthPageURLsEqual(t *testing.T) {
	if !authPageURLsEqual("https://auth.openai.com/email-verification?x=1", "https://auth.openai.com/email-verification?y=2") {
		t.Fatal("same path should match")
	}
	if authPageURLsEqual("https://auth.openai.com/email-verification", "https://auth.openai.com/about-you") {
		t.Fatal("different paths must not match")
	}
}
