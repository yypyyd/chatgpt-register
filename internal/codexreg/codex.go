package codexreg

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// decodeJWTClaims decodes access-token metadata without verifying the JWT
// signature. The token itself came from the authenticated ChatGPT session.
func decodeJWTClaims(token string) (accountID, userID, email, planType string, err error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", "", "", "", fmt.Errorf("invalid JWT format")
	}
	payload := parts[1]
	if rem := len(payload) % 4; rem != 0 {
		payload += strings.Repeat("=", 4-rem)
	}
	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return "", "", "", "", fmt.Errorf("base64 decode: %w", err)
	}
	var claims map[string]any
	if err = json.Unmarshal(decoded, &claims); err != nil {
		return "", "", "", "", fmt.Errorf("json unmarshal: %w", err)
	}
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	profile, _ := claims["https://api.openai.com/profile"].(map[string]any)
	accountID, _ = auth["chatgpt_account_id"].(string)
	userID, _ = auth["chatgpt_user_id"].(string)
	email, _ = profile["email"].(string)
	planType, _ = auth["chatgpt_plan_type"].(string)
	if planType == "" {
		planType = "free"
	}
	return
}
