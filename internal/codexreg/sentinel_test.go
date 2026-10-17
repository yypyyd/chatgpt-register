package codexreg

import (
	"testing"
)

func TestIsForgedSentinelToken(t *testing.T) {
	forgedP := `gAAAAAB` + "dGVzdA==" + `~S`
	forgedJSON := `{"p":"` + forgedP + `","id":"device","flow":"email_otp_validate"}`
	realJSON := `{"p":"gAAAAABrealpayload","t":{"work":1},"c":{"turnstile":"ok"},"id":"device","flow":"email_otp_validate"}`

	cases := []struct {
		token  string
		forged bool
	}{
		{forgedP, false},
		{forgedJSON, true},
		{realJSON, false},
		{`{"p":"gAAAAABrealpayload","t":1,"id":"x","flow":"oauth_create_account"}`, false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isForgedSentinelToken(tc.token); got != tc.forged {
			t.Fatalf("isForgedSentinelToken(%q)=%v want %v", tc.token, got, tc.forged)
		}
	}
	if err := rejectForgedSentinelToken(forgedJSON); err == nil {
		t.Fatal("forged JSON must be rejected")
	}
	if err := rejectForgedSentinelToken(""); err == nil {
		t.Fatal("empty token must be rejected")
	}
	if err := rejectForgedSentinelToken(realJSON); err != nil {
		t.Fatalf("real SDK token rejected: %v", err)
	}
}

func TestSentinelFlowForURL(t *testing.T) {
	cases := map[string]string{
		"https://auth.openai.com/api/accounts/create_account":        "oauth_create_account",
		"https://auth.openai.com/api/accounts/email-otp/validate":    "email_otp_validate",
		"https://auth.openai.com/api/accounts/passwordless/send-otp": "authorize_continue",
		"https://auth.openai.com/api/accounts/authorize/continue":    "authorize_continue",
		"https://auth.openai.com/api/accounts/password/verify":       "password_verify",
		"https://auth.openai.com/api/accounts/email-otp/resend":      "",
		"https://chatgpt.com/api/auth/session":                       "",
	}
	for u, want := range cases {
		if got := sentinelFlowForURL(u); got != want {
			t.Fatalf("sentinelFlowForURL(%s)=%q want %q", u, got, want)
		}
	}
}
