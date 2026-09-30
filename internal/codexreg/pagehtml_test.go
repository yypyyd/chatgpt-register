package codexreg

import (
	"strings"
	"testing"
)

func TestParsePageFingerprint(t *testing.T) {
	html := []byte(`<!doctype html><html lang="en" data-build="9f3a2b1c"><head>
	<script src="/_next/static/chunks/webpack-123.js"></script>
	<script src="https://cdn.oaistatic.com/assets/c/9f3a2b1c/_index.js"></script>
	<script src="https://chatgpt.com/sentinel/20260219f9f6/sdk.js"></script>
	</head><body></body></html>`)
	fp := parsePageFingerprint("https://auth.openai.com/email-verification?id=abc&state=1", html)
	if fp.DataBuild != "9f3a2b1c" {
		t.Fatalf("data-build=%q", fp.DataBuild)
	}
	if len(fp.Scripts) < 3 {
		t.Fatalf("scripts=%v", fp.Scripts)
	}
	if fp.Scripts[0] != "https://auth.openai.com/_next/static/chunks/webpack-123.js" {
		t.Fatalf("relative script=%q", fp.Scripts[0])
	}
	foundChunk := false
	for _, s := range fp.Scripts {
		if strings.Contains(s, "c/9f3a2b1c/_") {
			foundChunk = true
		}
	}
	if !foundChunk {
		t.Fatalf("missing c/<id>/_ script: %v", fp.Scripts)
	}
}

func TestParsePageFingerprintNextBuildId(t *testing.T) {
	html := []byte(`<html><script id="__NEXT_DATA__" type="application/json">{"buildId":"build-from-json"}</script></html>`)
	fp := parsePageFingerprint("https://auth.openai.com/about-you", html)
	if fp.DataBuild != "build-from-json" {
		t.Fatalf("buildId=%q", fp.DataBuild)
	}
}

func TestURLSearchKeys(t *testing.T) {
	if got := urlSearchKeys("https://auth.openai.com/email-verification?id=1&state=x"); got != "id,state" {
		t.Fatalf("keys=%q", got)
	}
}

func TestParsePageFingerprintAuthRemixHasNoScriptSrc(t *testing.T) {
	html := []byte(`<html lang="en-US" data-build="prod"><head>
	<link rel="modulepreload" href="https://auth-cdn.oaistatic.com/assets/entry.client-DQBwZBQi.js">
	<link rel="modulepreload" href="https://auth-cdn.oaistatic.com/assets/root-C415VUvO.js">
	</head></html>`)
	fp := parsePageFingerprint("https://auth.openai.com/email-verification", html)
	if fp.DataBuild != "prod" {
		t.Fatalf("build=%q", fp.DataBuild)
	}
	if len(fp.Scripts) != 0 {
		t.Fatalf("modulepreload must not become document.scripts: %v", fp.Scripts)
	}
}

func TestResolveAuthURL(t *testing.T) {
	if got := resolveAuthURL("/about-you?x=1"); got != "https://auth.openai.com/about-you?x=1" {
		t.Fatalf("got=%q", got)
	}
	full := "https://auth.openai.com/about-you?step=1"
	if got := resolveAuthURL(full); got != full {
		t.Fatalf("full=%q", got)
	}
}
