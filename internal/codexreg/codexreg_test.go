package codexreg

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func testAccessToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestBuildChatGPTResultDoesNotCreateAgentIdentity(t *testing.T) {
	token := testAccessToken(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "account-1",
			"chatgpt_user_id":    "user-1",
			"chatgpt_plan_type":  "free",
		},
		"https://api.openai.com/profile": map[string]any{
			"email": "person@example.com",
		},
	})

	result, err := buildChatGPTResult(token, "fallback@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if result.AuthJSON["auth_mode"] != "chatgpt" || result.AuthJSON["access_token"] != token {
		t.Fatalf("unexpected auth JSON: %#v", result.AuthJSON)
	}
	if _, exists := result.AuthJSON["agent_identity"]; exists {
		t.Fatal("agent_identity must not be generated")
	}
	if result.AccountID != "account-1" || result.UserID != "user-1" || result.PlanType != "free" {
		t.Fatalf("unexpected metadata: %#v", result)
	}
}

func TestBuildChatGPTResultKeepsSuccessfulRegistrationOnOpaqueToken(t *testing.T) {
	result, err := buildChatGPTResult("opaque-token", "fallback@example.com")
	if err == nil {
		t.Fatal("expected claim parsing error")
	}
	if result.AuthJSON["access_token"] != "opaque-token" || result.AuthJSON["email"] != "fallback@example.com" {
		t.Fatalf("unexpected fallback result: %#v", result.AuthJSON)
	}
}
