package handlers

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBuildCredentialsForChatGPTOnly(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"auth_mode":       "chatgpt",
		"access_token":    "access-token",
		"account_id":      "account-1",
		"chatgpt_user_id": "user-1",
		"email":           "person@example.com",
		"plan_type":       "free",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := buildCredentials(string(raw), "fallback@example.com")
	if got["auth_mode"] != "chatgpt" || got["access_token"] != "access-token" {
		t.Fatalf("unexpected credentials: %#v", got)
	}
	if _, exists := got["agent_runtime_id"]; exists {
		t.Fatal("new ChatGPT credentials must not contain agent fields")
	}
}

func TestBuildCPACredentials(t *testing.T) {
	exp := time.Date(2026, 8, 1, 2, 3, 4, 0, time.UTC)
	payload, err := json.Marshal(map[string]any{"exp": exp.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	token := "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
	raw, err := json.Marshal(map[string]any{
		"access_token": token,
		"account_id":   "account-1",
		"email":        "person@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 25, 1, 2, 3, 0, time.UTC)
	got := buildCPACredentials(string(raw), "fallback@example.com", now)
	if got["type"] != "codex" || got["access_token"] != token || got["account_id"] != "account-1" {
		t.Fatalf("unexpected CPA credentials: %#v", got)
	}
	if got["expired"] != exp.Format(time.RFC3339) {
		t.Fatalf("expired = %q, want %q", got["expired"], exp.Format(time.RFC3339))
	}
	if got["refresh_token"] != "" {
		t.Fatalf("refresh_token = %q, want empty", got["refresh_token"])
	}
	if got["id_token"] != "" {
		t.Fatalf("id_token = %q, want empty", got["id_token"])
	}
}

func TestCPAFileNamePreventsPathTraversal(t *testing.T) {
	if got := cpaFileName("../bad\\name@example.com\r\nInjected: yes"); strings.ContainsAny(got, "/\\\r\n: ") {
		t.Fatalf("unsafe filename: %q", got)
	}
}

func TestBuildCredentialsKeepsLegacyAgentCompatibility(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"agent_identity": map[string]any{
			"agent_runtime_id":  "agent-1",
			"agent_private_key": "private-key",
			"account_id":        "account-1",
			"email":             "legacy@example.com",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := buildCredentials(string(raw), "fallback@example.com")
	if got["auth_mode"] != "agentIdentity" || got["agent_runtime_id"] != "agent-1" {
		t.Fatalf("unexpected legacy credentials: %#v", got)
	}
}
