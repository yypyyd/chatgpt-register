// Package chatgptauth owns the refresh, validation and atomic persistence
// boundary for ChatGPT access tokens. An access token is never exportable just
// because its JWT exp is in the future.
package chatgptauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"chatgpt-register/internal/codexreg"
	"chatgpt-register/internal/models"

	"gorm.io/gorm"
)

const (
	CredentialValid        = "valid"
	CredentialNeedsRelogin = "needs_relogin"
	CredentialUnknown      = "unknown"

	TokenValid     = "valid"
	TokenRevoked   = "revoked"
	TokenExpired   = "expired"
	TokenUnknown   = "unknown"
	TokenUnchecked = "unchecked"

	CategoryRefreshedAndValid = "refreshed_and_valid"
	CategoryAlreadyValid      = "already_valid"
	CategoryTokenRevoked      = "token_revoked"
	CategoryRefreshFailed     = "refresh_failed"
	CategoryNeedsRelogin      = "needs_relogin"
	CategoryTemporaryUnknown  = "temporary_unknown"
)

const (
	newTokenIATWindow     = 15 * time.Minute
	credentialStepTimeout = 45 * time.Second
	// exportReuseProbeFor 内已有真实探针通过的 AT，导出不再走代理刷新。
	exportReuseProbeFor = 24 * time.Hour
)

var errCASConflict = errors.New("credential_cas_conflict")

// Upstream isolates network behavior so refresh rotation, revoked-token and
// transient-error semantics can be tested without contacting ChatGPT.
type Upstream interface {
	RefreshWithSession(context.Context, codexreg.WebSessionData, string) (codexreg.TokenRefreshResult, error)
	RefreshWithToken(context.Context, string, string, string) (codexreg.TokenRefreshResult, error)
	Probe(ctx context.Context, token, proxy, languages, deviceID, userAgent string) (codexreg.AccessTokenProbeResult, error)
}

type realUpstream struct{}

func (realUpstream) RefreshWithSession(ctx context.Context, session codexreg.WebSessionData, proxy string) (codexreg.TokenRefreshResult, error) {
	return codexreg.RefreshAccessTokenFromWebSession(ctx, session, proxy)
}

func (realUpstream) RefreshWithToken(ctx context.Context, token, proxy, languages string) (codexreg.TokenRefreshResult, error) {
	return codexreg.RefreshAccessTokenWithOAuth(ctx, token, proxy, languages)
}

func (realUpstream) Probe(ctx context.Context, token, proxy, languages, deviceID, userAgent string) (codexreg.AccessTokenProbeResult, error) {
	return codexreg.ProbeAccessTokenWithIdentity(ctx, codexreg.ProbeIdentity{
		Token: token, Proxy: proxy, Languages: languages, DeviceID: deviceID, UserAgent: userAgent,
	})
}

// Outcome deliberately carries no plaintext credential.
type Outcome struct {
	Category     string
	ErrorCode    string
	Exportable   bool
	Registration models.Registration
}

// Service serializes refreshes per account and uses database CAS in addition
// to the in-process lock. This prevents a late request from overwriting a
// rotated refresh token produced by an earlier request.
type Service struct {
	db          *gorm.DB
	upstream    Upstream
	locks       sync.Map // map[uint]*sync.Mutex
	completions sync.Map // map[uint]credentialCompletion
	now         func() time.Time
}

type credentialCompletion struct {
	completedAt time.Time
	outcome     Outcome
}

func New(db *gorm.DB) *Service {
	return NewWithUpstream(db, realUpstream{})
}

func NewWithUpstream(db *gorm.DB, upstream Upstream) *Service {
	return &Service{db: db, upstream: upstream, now: time.Now}
}

func (s *Service) lockFor(id uint) *sync.Mutex {
	v, _ := s.locks.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// EnsureExportable refreshes from an RT or Web Session, validates JWT identity
// and lifetime, then calls a real ChatGPT authorization probe. Only a Valid
// outcome is allowed into an export.
func (s *Service) EnsureExportable(ctx context.Context, id uint, fallbackProxy string) Outcome {
	requestStarted := time.Now()
	lock := s.lockFor(id)
	lock.Lock()
	defer lock.Unlock()
	if raw, ok := s.completions.Load(id); ok {
		completed := raw.(credentialCompletion)
		if completed.completedAt.After(requestStarted) {
			return completed.outcome
		}
	}

	// A simultaneous waiter may reuse the first request's just-verified result.
	// Older cached tokens are never trusted without another real probe.
	var final Outcome
	for attempt := 0; attempt < 2; attempt++ {
		out, err := s.ensureLocked(ctx, id, fallbackProxy)
		if !errors.Is(err, errCASConflict) {
			if err != nil && out.ErrorCode == "" {
				out.ErrorCode = "credential_update_failed"
				out.Category = CategoryRefreshFailed
			}
			final = out
			s.completions.Store(id, credentialCompletion{completedAt: time.Now(), outcome: final})
			return final
		}
	}
	final = Outcome{Category: CategoryRefreshFailed, ErrorCode: "credential_cas_conflict"}
	s.completions.Store(id, credentialCompletion{completedAt: time.Now(), outcome: final})
	return final
}

func (s *Service) ensureLocked(ctx context.Context, id uint, fallbackProxy string) (Outcome, error) {
	var reg models.Registration
	if err := s.db.First(&reg, id).Error; err != nil {
		return Outcome{Category: CategoryRefreshFailed, ErrorCode: "registration_not_found"}, err
	}
	now := s.now().UTC()
	auth, session, refreshToken, oldToken, err := parseAuthData(reg.AuthData)
	if err != nil {
		return s.persistFailure(reg, nil, CategoryNeedsRelogin, "invalid_auth_data", CredentialNeedsRelogin, TokenUnchecked, true)
	}
	proxy := strings.TrimSpace(session.Proxy)
	if proxy == "" {
		proxy = strings.TrimSpace(fallbackProxy)
	}
	oldFingerprint := codexreg.AccessTokenFingerprint(oldToken)
	oldMeta, _ := codexreg.ParseAccessTokenMetadata(oldToken)
	if canReuseVerifiedToken(reg, oldToken, oldMeta, now) {
		return Outcome{Category: CategoryAlreadyValid, Exportable: true, Registration: reg}, nil
	}

	stepCtx, cancel := context.WithTimeout(ctx, credentialStepTimeout)
	defer cancel()
	refreshResult, refreshErr := s.refresh(stepCtx, session, refreshToken, proxy)
	if refreshErr != nil && refreshResult.ErrorCode == "" {
		refreshResult.ErrorCode = "network_error"
	}
	refreshCode := safeCode(refreshResult.ErrorCode, "refresh_failed")
	if refreshResult.AccessToken == "" && !refreshResult.Permanent &&
		isTemporaryCode(refreshCode, refreshResult.HTTPStatus) &&
		fallbackProxy != "" && fallbackProxy != proxy {
		retryCtx, retryCancel := context.WithTimeout(ctx, credentialStepTimeout)
		refreshResult, refreshErr = s.refresh(retryCtx, session, refreshToken, fallbackProxy)
		retryCancel()
		if refreshErr != nil && refreshResult.ErrorCode == "" {
			refreshResult.ErrorCode = "network_error"
		}
		if refreshResult.AccessToken != "" {
			proxy = fallbackProxy
		}
		refreshCode = safeCode(refreshResult.ErrorCode, "refresh_failed")
	}
	if refreshResult.AccessToken == "" {
		if !refreshResult.Permanent && isTemporaryCode(refreshCode, refreshResult.HTTPStatus) && oldToken != "" {
			if out, ok, persistErr := s.exportExistingToken(ctx, reg, auth, session, oldToken, proxy, oldMeta); ok || persistErr != nil {
				return out, persistErr
			}
		}
		category := CategoryRefreshFailed
		credentialStatus := CredentialUnknown
		needsRelogin := false
		if refreshResult.Permanent || (refreshToken == "" && len(session.Cookies) == 0) {
			category = CategoryNeedsRelogin
			credentialStatus = CredentialNeedsRelogin
			needsRelogin = true
		}
		if !refreshResult.Permanent && isTemporaryCode(refreshCode, refreshResult.HTTPStatus) {
			category = CategoryTemporaryUnknown
			// A transient transport/CF/rate-limit failure says nothing about a
			// credential that was already proven usable. Preserve that last-known
			// state and record this attempt in refresh_last_error; only an explicit
			// permanent authentication failure may downgrade it to needs_relogin.
			if reg.AccountCredentialStatus == CredentialValid && !reg.NeedsRelogin {
				credentialStatus = CredentialValid
			}
		}
		return s.persistFailure(reg, nil, category, refreshCode, credentialStatus, currentTokenStatus(reg), needsRelogin)
	}

	candidate := refreshResult.AccessToken
	candidateFingerprint := codexreg.AccessTokenFingerprint(candidate)
	meta, metaErr := codexreg.ParseAccessTokenMetadata(candidate)
	if metaErr != nil {
		return s.persistCandidate(reg, auth, refreshResult, codexreg.AccessTokenMetadata{}, CategoryRefreshFailed,
			"invalid_jwt", CredentialUnknown, TokenUnknown, false, false)
	}
	if code := validateMetadata(reg, oldFingerprint, candidateFingerprint, oldMeta, meta, now); code != "" {
		status := TokenUnknown
		needsRelogin := false
		credentialStatus := CredentialUnknown
		category := CategoryRefreshFailed
		if code == "token_expired" {
			status = TokenExpired
			needsRelogin = true
			credentialStatus = CredentialNeedsRelogin
			category = CategoryTokenRevoked
		}
		return s.persistCandidate(reg, auth, refreshResult, meta, category, code,
			credentialStatus, status, needsRelogin, false)
	}

	// A token already proven revoked cannot be resurrected by a duplicate import
	// or another session response returning the exact same bytes.
	if reg.AccessTokenStatus == TokenRevoked && reg.AccessTokenFingerprint != "" &&
		reg.AccessTokenFingerprint == candidateFingerprint {
		rootCode := reg.TokenLastErrorCode
		if rootCode == "" || rootCode == "duplicate_revoked_token" {
			rootCode = "token_revoked"
		}
		out, persistErr := s.persistCandidate(reg, auth, refreshResult, meta, CategoryTokenRevoked,
			rootCode, CredentialNeedsRelogin, TokenRevoked, true, false)
		out.ErrorCode = "duplicate_revoked_token"
		return out, persistErr
	}

	probeCtx, probeCancel := context.WithTimeout(ctx, credentialStepTimeout)
	defer probeCancel()
	probe, probeErr := s.upstream.Probe(probeCtx, candidate, proxy, session.Languages, codexreg.DeviceIDFromCookies(session.Cookies), session.UserAgent)
	if probeErr != nil && probe.ErrorCode == "" {
		probe.ErrorCode = "network_error"
		probe.Temporary = true
	}
	if probe.Valid {
		category := CategoryAlreadyValid
		if candidateFingerprint != oldFingerprint {
			category = CategoryRefreshedAndValid
		}
		return s.persistCandidate(reg, auth, refreshResult, meta, category, "",
			CredentialValid, TokenValid, false, true)
	}
	code := safeCode(probe.ErrorCode, statusCode(probe.HTTPStatus))
	if codexreg.IsDefinitiveAccessTokenError(code) || probe.HTTPStatus == 401 {
		status := TokenRevoked
		if code == "token_expired" {
			status = TokenExpired
		}
		return s.persistCandidate(reg, auth, refreshResult, meta, CategoryTokenRevoked,
			code, CredentialNeedsRelogin, status, true, false)
	}
	return s.persistCandidate(reg, auth, refreshResult, meta, CategoryTemporaryUnknown,
		code, CredentialValid, TokenUnknown, false, false)
}

func canReuseVerifiedToken(reg models.Registration, token string, meta codexreg.AccessTokenMetadata, now time.Time) bool {
	if strings.TrimSpace(token) == "" || reg.NeedsRelogin {
		return false
	}
	if reg.AccountCredentialStatus != CredentialValid || reg.AccessTokenStatus != TokenValid {
		return false
	}
	if reg.TokenLastVerifiedAt == nil || now.Sub(reg.TokenLastVerifiedAt.UTC()) > exportReuseProbeFor {
		return false
	}
	if meta.ExpiresAt.IsZero() || !meta.ExpiresAt.After(now) {
		return false
	}
	return validateMetadata(reg, "", "", codexreg.AccessTokenMetadata{}, meta, now) == ""
}

func (s *Service) exportExistingToken(ctx context.Context, reg models.Registration, auth map[string]any, session codexreg.WebSessionData, token, proxy string, meta codexreg.AccessTokenMetadata) (Outcome, bool, error) {
	if meta.Email == "" {
		if parsed, err := codexreg.ParseAccessTokenMetadata(token); err == nil {
			meta = parsed
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, credentialStepTimeout)
	defer cancel()
	probe, probeErr := s.upstream.Probe(probeCtx, token, proxy, session.Languages, codexreg.DeviceIDFromCookies(session.Cookies), session.UserAgent)
	if probeErr != nil && probe.ErrorCode == "" {
		probe.ErrorCode = "network_error"
		probe.Temporary = true
	}
	if !probe.Valid {
		return Outcome{}, false, nil
	}
	out, err := s.persistCandidate(reg, auth, codexreg.TokenRefreshResult{AccessToken: token, HTTPStatus: 200}, meta,
		CategoryAlreadyValid, "", CredentialValid, TokenValid, false, true)
	return out, true, err
}

func (s *Service) refresh(ctx context.Context, session codexreg.WebSessionData, refreshToken, proxy string) (codexreg.TokenRefreshResult, error) {
	if refreshToken != "" {
		res, err := s.upstream.RefreshWithToken(ctx, refreshToken, proxy, session.Languages)
		if res.AccessToken != "" || !res.Permanent || len(session.Cookies) == 0 {
			return res, err
		}
		// A permanently invalid RT may coexist with a still-valid web session.
		// Falling back is safe because the same per-account lock and proxy are kept.
	}
	if len(session.Cookies) > 0 {
		return s.upstream.RefreshWithSession(ctx, session, proxy)
	}
	return codexreg.TokenRefreshResult{ErrorCode: "missing_long_lived_credential", Permanent: true}, nil
}

func parseAuthData(raw string) (map[string]any, codexreg.WebSessionData, string, string, error) {
	var auth map[string]any
	if json.Unmarshal([]byte(raw), &auth) != nil || auth == nil {
		return nil, codexreg.WebSessionData{}, "", "", fmt.Errorf("invalid_auth_data")
	}
	session, err := codexreg.WebSessionFromAuthData(raw)
	if err != nil {
		return nil, codexreg.WebSessionData{}, "", "", fmt.Errorf("invalid_auth_data")
	}
	refreshToken, _ := auth["refresh_token"].(string)
	return auth, session, strings.TrimSpace(refreshToken), strings.TrimSpace(session.AccessToken), nil
}

func validateMetadata(reg models.Registration, oldFingerprint, newFingerprint string, oldMeta, meta codexreg.AccessTokenMetadata, now time.Time) string {
	if !meta.ExpiresAt.After(now) {
		return "token_expired"
	}
	if oldFingerprint != "" && oldFingerprint != newFingerprint {
		if meta.IssuedAt.Before(now.Add(-newTokenIATWindow)) || meta.IssuedAt.After(now.Add(2*time.Minute)) {
			return "stale_issued_at"
		}
		if oldMeta.JTI != "" && meta.JTI != "" && oldMeta.JTI == meta.JTI {
			return "unchanged_jti"
		}
	}
	if reg.Email != "" && meta.Email != "" && !strings.EqualFold(strings.TrimSpace(reg.Email), meta.Email) {
		return "email_mismatch"
	}
	if reg.UserID != "" && meta.UserID != "" && reg.UserID != meta.UserID {
		return "user_id_mismatch"
	}
	if reg.AccountID != "" && meta.AccountID != "" && reg.AccountID != meta.AccountID {
		return "account_id_mismatch"
	}
	return ""
}

func (s *Service) persistCandidate(
	reg models.Registration,
	auth map[string]any,
	refresh codexreg.TokenRefreshResult,
	meta codexreg.AccessTokenMetadata,
	category, code, credentialStatus, tokenStatus string,
	needsRelogin, verified bool,
) (Outcome, error) {
	now := s.now().UTC()
	auth["access_token"] = refresh.AccessToken
	if refresh.RefreshToken != "" {
		auth["refresh_token"] = refresh.RefreshToken
	}
	if refresh.IDToken != "" {
		auth["id_token"] = refresh.IDToken
	}
	if len(refresh.Cookies) > 0 {
		auth["cookies"] = refresh.Cookies
	}
	if meta.Email != "" {
		auth["email"] = meta.Email
	}
	if meta.UserID != "" {
		auth["chatgpt_user_id"] = meta.UserID
	}
	if meta.AccountID != "" {
		auth["account_id"] = meta.AccountID
	}
	if meta.PlanType != "" {
		auth["plan_type"] = meta.PlanType
	}
	authJSON, err := json.Marshal(auth)
	if err != nil {
		return Outcome{Category: CategoryRefreshFailed, ErrorCode: "auth_data_encode_failed"}, err
	}
	fields := map[string]any{
		"auth_data":                 string(authJSON),
		"account_credential_status": credentialStatus,
		"access_token_status":       tokenStatus,
		"access_token_fingerprint":  codexreg.AccessTokenFingerprint(refresh.AccessToken),
		"access_token_jti":          meta.JTI,
		"token_issued_at":           nullableTime(meta.IssuedAt),
		"token_expires_at":          nullableTime(meta.ExpiresAt),
		"token_last_error_code":     code,
		"refresh_last_success_at":   now,
		"refresh_last_error":        "",
		"needs_relogin":             needsRelogin,
		"alive":                     compatibilityAlive(tokenStatus),
		"alive_checked_at":          now,
		"credential_version":        gorm.Expr("credential_version + 1"),
	}
	if verified {
		fields["token_last_verified_at"] = now
	}
	if meta.UserID != "" {
		fields["user_id"] = meta.UserID
	}
	if meta.AccountID != "" {
		fields["account_id"] = meta.AccountID
	}
	if meta.PlanType != "" {
		fields["plan_type"] = meta.PlanType
	}
	if err := s.casUpdate(reg, fields); err != nil {
		return Outcome{Category: category, ErrorCode: code}, err
	}
	if err := s.db.First(&reg, reg.ID).Error; err != nil {
		return Outcome{Category: category, ErrorCode: code}, err
	}
	return Outcome{Category: category, ErrorCode: code, Exportable: verified, Registration: reg}, nil
}

func (s *Service) persistFailure(
	reg models.Registration,
	meta *codexreg.AccessTokenMetadata,
	category, code, credentialStatus, tokenStatus string,
	needsRelogin bool,
) (Outcome, error) {
	now := s.now().UTC()
	fields := map[string]any{
		"account_credential_status": credentialStatus,
		"access_token_status":       tokenStatus,
		"token_last_error_code":     code,
		"refresh_last_error":        code,
		"needs_relogin":             needsRelogin,
		"alive":                     compatibilityAlive(tokenStatus),
		"alive_checked_at":          now,
		"credential_version":        gorm.Expr("credential_version + 1"),
	}
	if meta != nil {
		fields["token_issued_at"] = nullableTime(meta.IssuedAt)
		fields["token_expires_at"] = nullableTime(meta.ExpiresAt)
		fields["access_token_jti"] = meta.JTI
	}
	if err := s.casUpdate(reg, fields); err != nil {
		return Outcome{Category: category, ErrorCode: code}, err
	}
	if err := s.db.First(&reg, reg.ID).Error; err != nil {
		return Outcome{Category: category, ErrorCode: code}, err
	}
	return Outcome{Category: category, ErrorCode: code, Registration: reg}, nil
}

func (s *Service) casUpdate(reg models.Registration, fields map[string]any) error {
	res := s.db.Model(&models.Registration{}).
		Where("id = ? AND credential_version = ?", reg.ID, reg.CredentialVersion).
		Updates(fields)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return errCASConflict
	}
	return nil
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func currentTokenStatus(reg models.Registration) string {
	if reg.AccessTokenStatus == "" {
		return TokenUnchecked
	}
	return reg.AccessTokenStatus
}

func compatibilityAlive(tokenStatus string) string {
	switch tokenStatus {
	case TokenValid:
		return "alive"
	case TokenRevoked, TokenExpired:
		return "dead"
	default:
		return "unknown"
	}
}

func safeCode(code, fallback string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		code = fallback
	}
	if len(code) > 64 {
		code = code[:64]
	}
	return code
}

func statusCode(status int) string {
	if status <= 0 {
		return "temporary_error"
	}
	return fmt.Sprintf("http_%d", status)
}

func isTemporaryCode(code string, status int) bool {
	if status == 403 || status == 429 || status >= 500 || status == 0 {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "network_error", "request_cancelled", "http_403", "http_429":
		return true
	default:
		return false
	}
}
