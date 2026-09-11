package leonardoreg

import (
	"testing"

	http "github.com/bogdanfinn/fhttp"
)

func TestIsVercelChallenge(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("X-Vercel-Mitigated", "challenge")
	if !isVercelChallenge(200, hdr, nil) {
		t.Fatal("header challenge")
	}
	if !isVercelChallenge(429, nil, []byte("<html>checkpoint</html>")) {
		t.Fatal("429 html")
	}
	if !isVercelChallenge(200, nil, []byte("Vercel Security Checkpoint")) {
		t.Fatal("checkpoint html")
	}
	if isVercelChallenge(429, nil, []byte(`{"error":"rate limited"}`)) {
		t.Fatal("json 429 should not be treated as vercel")
	}
}

func TestIsEmailTaken(t *testing.T) {
	if !isEmailTaken("USER_ALREADY_EXISTS", "User already exists.", "") {
		t.Fatal("better-auth")
	}
	if !isEmailTaken("UsernameExistsException", "", "") {
		t.Fatal("cognito")
	}
	if isEmailTaken("INVALID_PASSWORD", "too short", "") {
		t.Fatal("password error is not taken")
	}
}

func TestIsUnconfirmed(t *testing.T) {
	if !isUnconfirmed("EMAIL_NOT_VERIFIED", "", "") {
		t.Fatal("better-auth verify")
	}
	if !isUnconfirmed("", "User is not confirmed", "") {
		t.Fatal("cognito")
	}
}

func TestSessionHasUser(t *testing.T) {
	if sessionHasUser([]byte("null")) {
		t.Fatal("null")
	}
	if sessionHasUser([]byte(`{"user":{"email":"a@b.c"}}`)) == false {
		t.Fatal("user")
	}
	if sessionHasUser([]byte(`{"session":{"accessToken":"x"}}`)) == false {
		t.Fatal("accessToken")
	}
}

func TestScrapeSitekey(t *testing.T) {
	c := &leoClient{html: `siteKey:"0x4AAAAAAAjpS3rLKnsHyb79"`}
	if got := c.scrapeSitekey(); got != fallbackTurnstileSitekey {
		t.Fatalf("got %q", got)
	}
}

func TestDisplayName(t *testing.T) {
	if displayName("foo.bar+1@x.com") != "foo bar 1" {
		t.Fatalf("got %q", displayName("foo.bar+1@x.com"))
	}
}

func TestHasBetterAuthCookie(t *testing.T) {
	if hasBetterAuthCookie([]map[string]any{{"name": "sid", "value": "1"}}) {
		t.Fatal("false positive")
	}
	if !hasBetterAuthCookie([]map[string]any{{"name": "better-auth.session_token", "value": "abc"}}) {
		t.Fatal("missing session")
	}
}
