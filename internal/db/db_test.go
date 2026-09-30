package db

import "testing"

func TestHasStoredLongLivedCredential(t *testing.T) {
	for _, raw := range []string{
		`{"refresh_token":"rt"}`,
		`{"cookies":[{"name":"session","value":"secret"}]}`,
	} {
		if !hasStoredLongLivedCredential(raw) {
			t.Fatalf("stored credential not detected: %s", raw)
		}
	}
	for _, raw := range []string{`{}`, `{"refresh_token":"","cookies":[]}`, `not-json`} {
		if hasStoredLongLivedCredential(raw) {
			t.Fatalf("missing credential reported as present: %s", raw)
		}
	}
}
