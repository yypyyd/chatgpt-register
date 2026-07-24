package handlers

import (
	"encoding/json"
	"testing"
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
