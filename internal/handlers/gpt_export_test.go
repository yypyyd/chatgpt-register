package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"chatgpt-register/internal/chatgptauth"
	"chatgpt-register/internal/codexreg"
	"chatgpt-register/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

type exportFakeUpstream struct {
	goodToken string
	badToken  string
}

func (f exportFakeUpstream) RefreshWithSession(context.Context, codexreg.WebSessionData, string) (codexreg.TokenRefreshResult, error) {
	return codexreg.TokenRefreshResult{ErrorCode: "missing_web_session", Permanent: true}, nil
}

func (f exportFakeUpstream) RefreshWithToken(_ context.Context, refresh, _, _ string) (codexreg.TokenRefreshResult, error) {
	if refresh == "good-rt" {
		return codexreg.TokenRefreshResult{HTTPStatus: 200, AccessToken: f.goodToken, RefreshToken: "good-rt-rotated"}, nil
	}
	return codexreg.TokenRefreshResult{HTTPStatus: 200, AccessToken: f.badToken, RefreshToken: refresh}, nil
}

func (f exportFakeUpstream) Probe(_ context.Context, token, _, _, _, _ string) (codexreg.AccessTokenProbeResult, error) {
	if token == f.goodToken {
		return codexreg.AccessTokenProbeResult{HTTPStatus: 200, Valid: true}, nil
	}
	return codexreg.AccessTokenProbeResult{HTTPStatus: 401, ErrorCode: "token_revoked"}, nil
}

func handlerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&models.Registration{}, &models.Setting{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func handlerJWT(now time.Time, jti, email, userID, accountID string) string {
	claims := map[string]any{
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "jti": jti,
		"https://api.openai.com/profile": map[string]any{"email": email},
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_user_id": userID, "chatgpt_account_id": accountID, "chatgpt_plan_type": "free",
		},
	}
	payload, _ := json.Marshal(claims)
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func createHandlerReg(t *testing.T, db *gorm.DB, email, token, refresh, password string) models.Registration {
	t.Helper()
	meta, _ := codexreg.ParseAccessTokenMetadata(token)
	auth, _ := json.Marshal(map[string]any{
		"access_token": token, "refresh_token": refresh, "email": email,
		"account_id": meta.AccountID, "chatgpt_user_id": meta.UserID, "cookies": []map[string]any{{"name": "session", "value": "cookie-secret"}},
	})
	reg := models.Registration{
		Email: email, Password: password, Status: "registered", AuthData: string(auth),
		AccountID: meta.AccountID, UserID: meta.UserID, AccountCredentialStatus: chatgptauth.CredentialValid,
		AccessTokenStatus: chatgptauth.TokenUnchecked,
	}
	if err := db.Create(&reg).Error; err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestDownloadExportsStoredTokensWithoutRefresh(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now().UTC()
	goodToken := handlerJWT(now, "good", "good@example.com", "user-good", "acct-good")
	revokedToken := handlerJWT(now, "revoked", "revoked@example.com", "user-revoked", "acct-revoked")
	expiredToken := handlerJWT(now.Add(-2*time.Hour), "expired", "expired@example.com", "user-expired", "acct-expired")
	db := handlerTestDB(t)
	good := createHandlerReg(t, db, "good@example.com", goodToken, "good-rt", "password-secret")
	revoked := createHandlerReg(t, db, "revoked@example.com", revokedToken, "bad-rt", "password-secret")
	expired := createHandlerReg(t, db, "expired@example.com", expiredToken, "old-rt", "password-secret")
	relogin := createHandlerReg(t, db, "relogin@example.com", goodToken, "rt", "password-secret")
	db.Model(&models.Registration{}).Where("id = ?", revoked.ID).Update("access_token_status", chatgptauth.TokenRevoked)
	db.Model(&models.Registration{}).Where("id = ?", relogin.ID).Update("needs_relogin", true)
	h := &Handler{DB: db, GPTCredentials: chatgptauth.NewWithUpstream(db, exportFakeUpstream{})}

	body := `{"ids":[` + jsonNumber(good.ID) + `,` + jsonNumber(revoked.ID) + `,` + jsonNumber(expired.ID) + `,` + jsonNumber(relogin.ID) + `],"format":"sub2api"}`
	req := httptest.NewRequest(http.MethodPost, "/api/download", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	h.Download(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), goodToken) || strings.Contains(w.Body.String(), revokedToken) || strings.Contains(w.Body.String(), expiredToken) {
		t.Fatal("export must keep the stored token and drop revoked or expired tokens")
	}
	if strings.Contains(w.Body.String(), "relogin@example.com") {
		t.Fatal("needs_relogin account was exported")
	}
	var stats exportStats
	if json.Unmarshal([]byte(w.Header().Get("X-Export-Stats")), &stats) != nil ||
		stats.AlreadyValid != 1 || stats.TokenRevoked != 1 || stats.TokenExpired != 1 || stats.NeedsRelogin != 1 || stats.RefreshedAndValid != 0 {
		t.Fatalf("wrong stats: %s", w.Header().Get("X-Export-Stats"))
	}
	var gotGood, gotRevoked models.Registration
	db.First(&gotGood, good.ID)
	db.First(&gotRevoked, revoked.ID)
	if !gotGood.Shipped || gotRevoked.Shipped {
		t.Fatalf("shipping flags wrong: good=%v revoked=%v", gotGood.Shipped, gotRevoked.Shipped)
	}
}

func TestRegistrationListSeparatesStatusAndRedactsSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now().UTC()
	token := handlerJWT(now, "revoked", "user@example.com", "user-1", "acct-1")
	db := handlerTestDB(t)
	reg := createHandlerReg(t, db, "user@example.com", token, "refresh-secret", "password-secret")
	db.Model(&models.Registration{}).Where("id = ?", reg.ID).Updates(map[string]any{
		"account_credential_status": chatgptauth.CredentialValid,
		"access_token_status":       chatgptauth.TokenRevoked,
		"token_last_error_code":     "token_revoked",
	})
	h := &Handler{DB: db}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/registrations", nil)
	h.List(c)
	text := w.Body.String()
	for _, secret := range []string{token, "refresh-secret", "password-secret", "cookie-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("list response leaked secret %q", secret)
		}
	}
	if !strings.Contains(text, `"status":"registered"`) ||
		!strings.Contains(text, `"account_credential_status":"valid"`) ||
		!strings.Contains(text, `"access_token_status":"revoked"`) {
		t.Fatalf("split status missing: %s", text)
	}
}

func TestCredentialLogRedaction(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJlbWFpbCI6InVzZXJAZXhhbXBsZS5jb20ifQ.signature1234"
	input := `session={"accessToken":"` + jwt + `","refresh_token":"refresh-secret","password":"password-secret","cookie":"cookie-secret"}`
	got := redactCredentialText(input)
	for _, secret := range []string{jwt, "refresh-secret", "password-secret", "cookie-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("redaction leaked %q: %s", secret, got)
		}
	}
}

func jsonNumber(id uint) string {
	b, _ := json.Marshal(id)
	return string(b)
}
