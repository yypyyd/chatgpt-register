package chatgptauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"chatgpt-register/internal/codexreg"
	"chatgpt-register/internal/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

type fakeUpstream struct {
	refreshSession func(context.Context, codexreg.WebSessionData, string) (codexreg.TokenRefreshResult, error)
	refreshToken   func(context.Context, string, string, string) (codexreg.TokenRefreshResult, error)
	probe          func(context.Context, string, string, string) (codexreg.AccessTokenProbeResult, error)
	refreshCalls   atomic.Int32
	probeCalls     atomic.Int32
}

func (f *fakeUpstream) RefreshWithSession(ctx context.Context, session codexreg.WebSessionData, proxy string) (codexreg.TokenRefreshResult, error) {
	f.refreshCalls.Add(1)
	if f.refreshSession != nil {
		return f.refreshSession(ctx, session, proxy)
	}
	return codexreg.TokenRefreshResult{ErrorCode: "missing_web_session", Permanent: true}, nil
}

func (f *fakeUpstream) RefreshWithToken(ctx context.Context, token, proxy, languages string) (codexreg.TokenRefreshResult, error) {
	f.refreshCalls.Add(1)
	if f.refreshToken != nil {
		return f.refreshToken(ctx, token, proxy, languages)
	}
	return codexreg.TokenRefreshResult{ErrorCode: "invalid_grant", Permanent: true}, nil
}

func (f *fakeUpstream) Probe(ctx context.Context, token, proxy, languages, _, _ string) (codexreg.AccessTokenProbeResult, error) {
	f.probeCalls.Add(1)
	if f.probe != nil {
		return f.probe(ctx, token, proxy, languages)
	}
	return codexreg.AccessTokenProbeResult{HTTPStatus: 200, Valid: true}, nil
}

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&models.Registration{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func jwtFor(now time.Time, iatOffset, expOffset time.Duration, jti, email, userID, accountID string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	claims := map[string]any{
		"iat": now.Add(iatOffset).Unix(), "exp": now.Add(expOffset).Unix(), "jti": jti,
		"https://api.openai.com/profile": map[string]any{"email": email},
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_user_id": userID, "chatgpt_account_id": accountID, "chatgpt_plan_type": "free",
		},
	}
	payload, _ := json.Marshal(claims)
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func createRegistration(t *testing.T, db *gorm.DB, token, refresh string, withCookies bool) models.Registration {
	t.Helper()
	auth := map[string]any{
		"access_token": token, "refresh_token": refresh, "email": "user@example.com",
		"account_id": "acct-1", "chatgpt_user_id": "user-1", "plan_type": "free",
	}
	if withCookies {
		auth["cookies"] = []codexreg.WebCookie{{Name: "__Secure-next-auth.session-token", Value: "cookie-secret", Domain: "chatgpt.com", Path: "/", Secure: true}}
	}
	raw, _ := json.Marshal(auth)
	reg := models.Registration{
		Email: "user@example.com", Status: "registered", AuthData: string(raw),
		AccountID: "acct-1", UserID: "user-1", PlanType: "free",
		AccountCredentialStatus: CredentialUnknown, AccessTokenStatus: TokenUnchecked,
	}
	if err := db.Create(&reg).Error; err != nil {
		t.Fatal(err)
	}
	return reg
}

func newTestService(db *gorm.DB, upstream Upstream, now time.Time) *Service {
	s := NewWithUpstream(db, upstream)
	s.now = func() time.Time { return now }
	return s
}

func TestFutureJWTRevokedByUpstreamIsNotValid(t *testing.T) {
	now := time.Date(2026, 9, 25, 1, 0, 0, 0, time.UTC)
	token := jwtFor(now, -10*time.Hour, 10*time.Hour, "old-jti", "user@example.com", "user-1", "acct-1")
	db := testDB(t)
	reg := createRegistration(t, db, token, "rt-1", false)
	fake := &fakeUpstream{
		refreshToken: func(context.Context, string, string, string) (codexreg.TokenRefreshResult, error) {
			return codexreg.TokenRefreshResult{HTTPStatus: 200, AccessToken: token, RefreshToken: "rt-1"}, nil
		},
		probe: func(context.Context, string, string, string) (codexreg.AccessTokenProbeResult, error) {
			return codexreg.AccessTokenProbeResult{HTTPStatus: 401, ErrorCode: "token_revoked"}, nil
		},
	}
	out := newTestService(db, fake, now).EnsureExportable(context.Background(), reg.ID, "")
	if out.Exportable || out.Category != CategoryTokenRevoked || out.Registration.AccessTokenStatus != TokenRevoked {
		t.Fatalf("unexpected outcome: %+v", out)
	}
	if !out.Registration.NeedsRelogin || out.Registration.Status != "registered" {
		t.Fatalf("registration must be preserved and require relogin: %+v", out.Registration)
	}
}

func TestRefreshRotatesATAndRTThenExports(t *testing.T) {
	now := time.Date(2026, 9, 25, 2, 0, 0, 0, time.UTC)
	oldToken := jwtFor(now, -10*time.Hour, 10*time.Hour, "old-jti", "user@example.com", "user-1", "acct-1")
	newToken := jwtFor(now, 0, 12*time.Hour, "new-jti", "user@example.com", "user-1", "acct-1")
	db := testDB(t)
	reg := createRegistration(t, db, oldToken, "old-rt", false)
	fake := &fakeUpstream{refreshToken: func(context.Context, string, string, string) (codexreg.TokenRefreshResult, error) {
		return codexreg.TokenRefreshResult{HTTPStatus: 200, AccessToken: newToken, RefreshToken: "new-rt", IDToken: "new-id"}, nil
	}}
	out := newTestService(db, fake, now).EnsureExportable(context.Background(), reg.ID, "proxy-1")
	if !out.Exportable || out.Category != CategoryRefreshedAndValid {
		t.Fatalf("unexpected outcome: %+v", out)
	}
	if out.Registration.AccessTokenJTI != "new-jti" || !out.Registration.TokenIssuedAt.Equal(now) {
		t.Fatalf("new JWT metadata was not saved: %+v", out.Registration)
	}
	var auth map[string]any
	if json.Unmarshal([]byte(out.Registration.AuthData), &auth) != nil || auth["access_token"] != newToken || auth["refresh_token"] != "new-rt" {
		t.Fatal("rotated AT/RT pair was not saved atomically")
	}
	stale := db.Model(&models.Registration{}).Where("id = ? AND credential_version = ?", reg.ID, 0).
		Update("auth_data", `{"access_token":"stale","refresh_token":"old-rt"}`)
	if stale.Error != nil || stale.RowsAffected != 0 {
		t.Fatalf("stale request overwrote rotated credentials: error=%v rows=%d", stale.Error, stale.RowsAffected)
	}
}

func TestMissingLongLivedCredentialNeedsRelogin(t *testing.T) {
	now := time.Now().UTC()
	oldToken := jwtFor(now, -time.Hour, time.Hour, "old", "user@example.com", "user-1", "acct-1")
	db := testDB(t)
	reg := createRegistration(t, db, oldToken, "", false)
	out := newTestService(db, &fakeUpstream{}, now).EnsureExportable(context.Background(), reg.ID, "")
	if out.Exportable || out.Category != CategoryNeedsRelogin || !out.Registration.NeedsRelogin {
		t.Fatalf("unexpected outcome: %+v", out)
	}
	var auth map[string]any
	_ = json.Unmarshal([]byte(out.Registration.AuthData), &auth)
	if auth["access_token"] != oldToken {
		t.Fatal("old AT should be preserved for audit but never exported")
	}
}

func TestTemporaryProbeFailuresRemainUnknown(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name string
		code int
		err  error
	}{
		{"cloudflare", 403, nil}, {"rate", 429, nil}, {"server", 503, nil}, {"timeout", 0, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := jwtFor(now, -time.Minute, time.Hour, tc.name, "user@example.com", "user-1", "acct-1")
			db := testDB(t)
			reg := createRegistration(t, db, token, "rt", false)
			fake := &fakeUpstream{
				refreshToken: func(context.Context, string, string, string) (codexreg.TokenRefreshResult, error) {
					return codexreg.TokenRefreshResult{HTTPStatus: 200, AccessToken: token, RefreshToken: "rt"}, nil
				},
				probe: func(context.Context, string, string, string) (codexreg.AccessTokenProbeResult, error) {
					return codexreg.AccessTokenProbeResult{HTTPStatus: tc.code, ErrorCode: fmt.Sprintf("http_%d", tc.code), Temporary: true}, tc.err
				},
			}
			out := newTestService(db, fake, now).EnsureExportable(context.Background(), reg.ID, "")
			if out.Exportable || out.Category != CategoryTemporaryUnknown || out.Registration.AccessTokenStatus != TokenUnknown {
				t.Fatalf("unexpected outcome: %+v", out)
			}
			if out.Registration.NeedsRelogin {
				t.Fatal("temporary error must not mark account dead")
			}
		})
	}
}

func TestConcurrentEnsureRefreshesOnce(t *testing.T) {
	now := time.Now().UTC()
	oldToken := jwtFor(now, -time.Hour, time.Hour, "old", "user@example.com", "user-1", "acct-1")
	newToken := jwtFor(now, 0, time.Hour, "new", "user@example.com", "user-1", "acct-1")
	db := testDB(t)
	reg := createRegistration(t, db, oldToken, "rt", false)
	fake := &fakeUpstream{refreshToken: func(context.Context, string, string, string) (codexreg.TokenRefreshResult, error) {
		time.Sleep(40 * time.Millisecond)
		return codexreg.TokenRefreshResult{HTTPStatus: 200, AccessToken: newToken, RefreshToken: "rotated"}, nil
	}}
	service := newTestService(db, fake, now)
	var wg sync.WaitGroup
	outcomes := make([]Outcome, 8)
	for i := range outcomes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outcomes[i] = service.EnsureExportable(context.Background(), reg.ID, "")
		}(i)
	}
	wg.Wait()
	if fake.refreshCalls.Load() != 1 || fake.probeCalls.Load() != 1 {
		t.Fatalf("refresh=%d probe=%d, want one each", fake.refreshCalls.Load(), fake.probeCalls.Load())
	}
	for _, out := range outcomes {
		if !out.Exportable {
			t.Fatalf("waiter did not reuse verified result: %+v", out)
		}
	}
}

func TestSequentialExportsAlwaysRefreshAndProbe(t *testing.T) {
	now := time.Now().UTC()
	token := jwtFor(now, 0, time.Hour, "current", "user@example.com", "user-1", "acct-1")
	db := testDB(t)
	reg := createRegistration(t, db, token, "rt", false)
	fake := &fakeUpstream{refreshToken: func(context.Context, string, string, string) (codexreg.TokenRefreshResult, error) {
		return codexreg.TokenRefreshResult{HTTPStatus: 200, AccessToken: token, RefreshToken: "rt"}, nil
	}}
	service := newTestService(db, fake, now)
	first := service.EnsureExportable(context.Background(), reg.ID, "")
	if !first.Exportable {
		t.Fatalf("first export rejected: %+v", first)
	}
	second := service.EnsureExportable(context.Background(), reg.ID, "")
	if !second.Exportable || second.Category != CategoryAlreadyValid {
		t.Fatalf("cached export rejected: %+v", second)
	}
	if fake.refreshCalls.Load() != 1 || fake.probeCalls.Load() != 1 {
		t.Fatalf("recent verified export still used proxy: refresh=%d probe=%d", fake.refreshCalls.Load(), fake.probeCalls.Load())
	}
}

func TestDuplicateRevokedTokenCannotBecomeGreen(t *testing.T) {
	now := time.Now().UTC()
	token := jwtFor(now, -time.Hour, time.Hour, "revoked", "user@example.com", "user-1", "acct-1")
	db := testDB(t)
	reg := createRegistration(t, db, token, "rt", false)
	db.Model(&models.Registration{}).Where("id = ?", reg.ID).Updates(map[string]any{
		"access_token_status": TokenRevoked, "access_token_fingerprint": codexreg.AccessTokenFingerprint(token),
	})
	fake := &fakeUpstream{refreshToken: func(context.Context, string, string, string) (codexreg.TokenRefreshResult, error) {
		return codexreg.TokenRefreshResult{HTTPStatus: 200, AccessToken: token, RefreshToken: "rt"}, nil
	}, probe: func(context.Context, string, string, string) (codexreg.AccessTokenProbeResult, error) {
		return codexreg.AccessTokenProbeResult{HTTPStatus: 200, Valid: true}, nil
	}}
	out := newTestService(db, fake, now).EnsureExportable(context.Background(), reg.ID, "")
	if out.Exportable || out.ErrorCode != "duplicate_revoked_token" || fake.probeCalls.Load() != 0 {
		t.Fatalf("duplicate revoked token was reconsidered: %+v", out)
	}
	if out.Registration.TokenLastErrorCode != "token_revoked" {
		t.Fatalf("duplicate detection overwrote root revocation reason: %+v", out.Registration)
	}
}

func TestRefreshNetworkFailureIsNotRelogin(t *testing.T) {
	now := time.Now().UTC()
	token := jwtFor(now, -time.Hour, time.Hour, "old", "user@example.com", "user-1", "acct-1")
	db := testDB(t)
	reg := createRegistration(t, db, token, "rt", false)
	fake := &fakeUpstream{refreshToken: func(context.Context, string, string, string) (codexreg.TokenRefreshResult, error) {
		return codexreg.TokenRefreshResult{ErrorCode: "network_error"}, errors.New("secret-free network failure")
	}}
	out := newTestService(db, fake, now).EnsureExportable(context.Background(), reg.ID, "")
	if !out.Exportable || out.Category != CategoryAlreadyValid || out.Registration.NeedsRelogin {
		t.Fatalf("existing AT should still export after temporary refresh failure: %+v", out)
	}
}

func TestTemporaryRefreshFailureDoesNotDowngradeVerifiedCredential(t *testing.T) {
	now := time.Now().UTC()
	token := jwtFor(now, -time.Minute, time.Hour, "verified", "user@example.com", "user-1", "acct-1")
	db := testDB(t)
	reg := createRegistration(t, db, token, "", true)
	verifiedAt := now.Add(-time.Minute)
	if err := db.Model(&models.Registration{}).Where("id = ?", reg.ID).Updates(map[string]any{
		"account_credential_status": CredentialValid,
		"access_token_status":       TokenValid,
		"token_last_verified_at":    verifiedAt,
		"refresh_last_success_at":   verifiedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	fake := &fakeUpstream{refreshSession: func(context.Context, codexreg.WebSessionData, string) (codexreg.TokenRefreshResult, error) {
		return codexreg.TokenRefreshResult{HTTPStatus: 403, ErrorCode: "http_403"}, nil
	}}
	out := newTestService(db, fake, now).EnsureExportable(context.Background(), reg.ID, "")
	if !out.Exportable || out.Category != CategoryAlreadyValid {
		t.Fatalf("recent verified AT should export without proxy: %+v", out)
	}
	if fake.refreshCalls.Load() != 0 || fake.probeCalls.Load() != 0 {
		t.Fatalf("recent verified export used proxy: refresh=%d probe=%d", fake.refreshCalls.Load(), fake.probeCalls.Load())
	}
	if out.Registration.AccountCredentialStatus != CredentialValid || out.Registration.AccessTokenStatus != TokenValid {
		t.Fatalf("cached export lost last-known valid states: %+v", out.Registration)
	}
}

func TestStaleVerifiedTokenRefreshesAgain(t *testing.T) {
	now := time.Now().UTC()
	token := jwtFor(now, -time.Minute, time.Hour, "stale", "user@example.com", "user-1", "acct-1")
	db := testDB(t)
	reg := createRegistration(t, db, token, "rt", false)
	old := now.Add(-exportReuseProbeFor - time.Hour)
	if err := db.Model(&models.Registration{}).Where("id = ?", reg.ID).Updates(map[string]any{
		"account_credential_status": CredentialValid,
		"access_token_status":       TokenValid,
		"token_last_verified_at":    old,
	}).Error; err != nil {
		t.Fatal(err)
	}
	fake := &fakeUpstream{refreshToken: func(context.Context, string, string, string) (codexreg.TokenRefreshResult, error) {
		return codexreg.TokenRefreshResult{HTTPStatus: 200, AccessToken: token, RefreshToken: "rt"}, nil
	}}
	out := newTestService(db, fake, now).EnsureExportable(context.Background(), reg.ID, "")
	if !out.Exportable {
		t.Fatalf("stale verified token should refresh: %+v", out)
	}
	if fake.refreshCalls.Load() != 1 || fake.probeCalls.Load() != 1 {
		t.Fatalf("stale token skipped refresh: refresh=%d probe=%d", fake.refreshCalls.Load(), fake.probeCalls.Load())
	}
}

func TestTemporaryRefreshAndTemporaryProbeStillBlockExport(t *testing.T) {
	now := time.Now().UTC()
	token := jwtFor(now, -time.Minute, time.Hour, "verified", "user@example.com", "user-1", "acct-1")
	db := testDB(t)
	reg := createRegistration(t, db, token, "", true)
	fake := &fakeUpstream{
		refreshSession: func(context.Context, codexreg.WebSessionData, string) (codexreg.TokenRefreshResult, error) {
			return codexreg.TokenRefreshResult{HTTPStatus: 403, ErrorCode: "http_403"}, nil
		},
		probe: func(context.Context, string, string, string) (codexreg.AccessTokenProbeResult, error) {
			return codexreg.AccessTokenProbeResult{HTTPStatus: 403, ErrorCode: "http_403", Temporary: true}, nil
		},
	}
	out := newTestService(db, fake, now).EnsureExportable(context.Background(), reg.ID, "")
	if out.Exportable || out.Category != CategoryTemporaryUnknown {
		t.Fatalf("double 403 must still block export: %+v", out)
	}
}
