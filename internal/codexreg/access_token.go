package codexreg

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	"github.com/google/uuid"
)

const oauthRefreshClientID = "app_2SKx67EdpoN0G6j64rFvigXD"

// AccessTokenMetadata contains only non-secret JWT claims used to bind a token
// to the registration that owns it. JWT payloads are decoded, not signature
// verified; authenticity is established by the real ChatGPT authorization
// probe before a token becomes exportable.
type AccessTokenMetadata struct {
	IssuedAt  time.Time
	ExpiresAt time.Time
	JTI       string
	Email     string
	UserID    string
	AccountID string
	PlanType  string
}

// ParseAccessTokenMetadata decodes the identity and lifetime claims required by
// the credential pipeline. It never includes the token in an error.
func ParseAccessTokenMetadata(token string) (AccessTokenMetadata, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return AccessTokenMetadata{}, fmt.Errorf("invalid_jwt_format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return AccessTokenMetadata{}, fmt.Errorf("invalid_jwt_payload")
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return AccessTokenMetadata{}, fmt.Errorf("invalid_jwt_claims")
	}
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	profile, _ := claims["https://api.openai.com/profile"].(map[string]any)
	iat, okIAT := numericClaim(claims["iat"])
	exp, okExp := numericClaim(claims["exp"])
	if !okIAT || !okExp || exp <= 0 {
		return AccessTokenMetadata{}, fmt.Errorf("missing_jwt_time_claims")
	}
	meta := AccessTokenMetadata{
		IssuedAt:  time.Unix(iat, 0).UTC(),
		ExpiresAt: time.Unix(exp, 0).UTC(),
		JTI:       claimString(claims, "jti"),
		Email:     claimString(profile, "email"),
		UserID:    claimString(auth, "chatgpt_user_id"),
		AccountID: claimString(auth, "chatgpt_account_id"),
		PlanType:  claimString(auth, "chatgpt_plan_type"),
	}
	if meta.PlanType == "" {
		meta.PlanType = "free"
	}
	return meta, nil
}

func numericClaim(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case int64:
		return n, true
	case int:
		return int64(n), true
	default:
		return 0, false
	}
}

func claimString(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return strings.TrimSpace(v)
}

// AccessTokenFingerprint is safe for logs and duplicate detection. The full
// SHA-256 digest is deliberately used so the database can enforce exact token
// identity without retaining another copy of the credential.
func AccessTokenFingerprint(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// TokenRefreshResult is the sanitized result of obtaining an access token from
// either a rotating OAuth refresh token or the stored ChatGPT Web Session.
type TokenRefreshResult struct {
	HTTPStatus   int
	AccessToken  string
	RefreshToken string
	IDToken      string
	Cookies      []WebCookie
	ErrorCode    string
	Permanent    bool
}

// AccessTokenProbeResult is a real ChatGPT backend authorization result. Body
// text is intentionally not exposed because upstream error bodies can echo
// credentials or session identifiers.
type AccessTokenProbeResult struct {
	HTTPStatus int
	Valid      bool
	Temporary  bool
	ErrorCode  string
}

// RefreshAccessTokenFromWebSession asks NextAuth for the current access token
// using the stored login cookies and returns any rotated cookies as one atomic
// credential candidate.
func RefreshAccessTokenFromWebSession(ctx context.Context, session WebSessionData, proxy string) (TokenRefreshResult, error) {
	if len(session.Cookies) == 0 {
		return TokenRefreshResult{ErrorCode: "missing_web_session", Permanent: true}, nil
	}
	c, err := newProtocolClient(Input{Proxy: proxy}, session.Languages)
	if err != nil {
		return TokenRefreshResult{ErrorCode: "client_init_failed"}, err
	}
	c.loadCookies(session.Cookies)
	r, err := c.get(ctx, "https://chatgpt.com/api/auth/session", "https://chatgpt.com/", false)
	if err != nil {
		if r != nil {
			return TokenRefreshResult{HTTPStatus: r.Status, ErrorCode: statusCode(r.Status)}, err
		}
		return TokenRefreshResult{ErrorCode: "network_error"}, err
	}
	result := TokenRefreshResult{HTTPStatus: r.Status, Cookies: c.webCookies()}
	if r.Status != http.StatusOK {
		result.ErrorCode = upstreamErrorCode(r.Body, r.Status)
		result.Permanent = r.Status == http.StatusUnauthorized
		return result, nil
	}
	var payload struct {
		AccessToken string `json:"accessToken"`
	}
	if json.Unmarshal(r.Body, &payload) != nil {
		result.ErrorCode = "invalid_session_response"
		return result, nil
	}
	result.AccessToken = strings.TrimSpace(payload.AccessToken)
	if result.AccessToken == "" {
		result.ErrorCode = "session_logged_out"
		result.Permanent = true
	}
	return result, nil
}

func (c *protocolClient) loadCookies(cookies []WebCookie) {
	grouped := map[string][]*http.Cookie{}
	for _, wc := range cookies {
		name := strings.TrimSpace(wc.Name)
		if name == "" {
			continue
		}
		domain := wc.Domain
		if domain == "" {
			if strings.HasPrefix(name, "__Host-") || strings.HasPrefix(name, "__Secure-") {
				domain = "chatgpt.com"
			} else {
				continue
			}
		}
		if !isChatGPTCookieDomain(domain) {
			continue
		}
		path := wc.Path
		if path == "" {
			path = "/"
		}
		ck := &http.Cookie{
			Name: name, Value: wc.Value, Path: path, Domain: domain,
			Secure: wc.Secure, HttpOnly: wc.HTTPOnly, SameSite: sameSiteFromString(wc.SameSite),
		}
		if wc.Expires > 0 {
			ck.Expires = time.Unix(int64(wc.Expires), 0)
		}
		grouped[cookieSiteURL(domain)] = append(grouped[cookieSiteURL(domain)], ck)
	}
	for site, list := range grouped {
		u, err := url.Parse(site)
		if err == nil {
			c.cli.SetCookies(u, list)
		}
	}
}

func cookieSiteURL(domain string) string {
	d := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if d == "auth.openai.com" || strings.HasSuffix(d, ".auth.openai.com") {
		return "https://auth.openai.com/"
	}
	if d == "openai.com" || strings.HasSuffix(d, ".openai.com") {
		return "https://openai.com/"
	}
	return "https://chatgpt.com/"
}

func sameSiteFromString(s string) http.SameSite {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "lax":
		return http.SameSiteLaxMode
	case "strict":
		return http.SameSiteStrictMode
	case "none":
		return http.SameSiteNoneMode
	default:
		return http.SameSiteDefaultMode
	}
}

// RefreshAccessTokenWithOAuth exchanges a stored rotating refresh token. A new
// refresh_token, when returned, travels in the same result so callers can save
// the AT/RT pair atomically.
func RefreshAccessTokenWithOAuth(ctx context.Context, refreshToken, proxy, languages string) (TokenRefreshResult, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return TokenRefreshResult{ErrorCode: "missing_refresh_token", Permanent: true}, nil
	}
	c, err := newProtocolClient(Input{Proxy: proxy}, languages)
	if err != nil {
		return TokenRefreshResult{ErrorCode: "client_init_failed"}, err
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", oauthRefreshClientID)
	req, err := http.NewRequest(http.MethodPost, "https://auth.openai.com/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return TokenRefreshResult{ErrorCode: "request_build_failed"}, err
	}
	c.headers(req, "https://auth.openai.com/", false)
	req.Header.Set("accept", "application/json")
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("origin", "https://auth.openai.com")
	r, err := c.do(ctx, req)
	if err != nil {
		if r != nil {
			return TokenRefreshResult{HTTPStatus: r.Status, ErrorCode: statusCode(r.Status)}, err
		}
		return TokenRefreshResult{ErrorCode: "network_error"}, err
	}
	result := TokenRefreshResult{HTTPStatus: r.Status}
	if r.Status != http.StatusOK {
		result.ErrorCode = upstreamErrorCode(r.Body, r.Status)
		result.Permanent = r.Status == http.StatusBadRequest || r.Status == http.StatusUnauthorized
		return result, nil
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if json.Unmarshal(r.Body, &payload) != nil || strings.TrimSpace(payload.AccessToken) == "" {
		result.ErrorCode = "invalid_refresh_response"
		return result, nil
	}
	result.AccessToken = strings.TrimSpace(payload.AccessToken)
	result.RefreshToken = strings.TrimSpace(payload.RefreshToken)
	if result.RefreshToken == "" {
		result.RefreshToken = refreshToken
	}
	result.IDToken = strings.TrimSpace(payload.IDToken)
	return result, nil
}

// ProbeIdentity binds a backend-api probe to the same device/UA that created the session.
type ProbeIdentity struct {
	Token     string
	Proxy     string
	Languages string
	DeviceID  string
	UserAgent string
}

// ProbeAccessToken verifies the exact token against the same real, non-image
// ChatGPT endpoint used by image2api for account bootstrap. It consumes no
// image quota and never relies on JWT exp alone.
func ProbeAccessToken(ctx context.Context, token, proxy, languages string) (AccessTokenProbeResult, error) {
	return ProbeAccessTokenWithIdentity(ctx, ProbeIdentity{Token: token, Proxy: proxy, Languages: languages})
}

func ProbeAccessTokenWithIdentity(ctx context.Context, in ProbeIdentity) (AccessTokenProbeResult, error) {
	if strings.TrimSpace(in.Token) == "" {
		return AccessTokenProbeResult{ErrorCode: "missing_access_token"}, nil
	}
	languages := strings.TrimSpace(in.Languages)
	if languages == "" {
		languages = "en-US,en"
	}
	c, err := newProtocolClient(Input{Proxy: in.Proxy}, languages)
	if err != nil {
		return AccessTokenProbeResult{ErrorCode: "client_init_failed", Temporary: true}, err
	}
	if ua := strings.TrimSpace(in.UserAgent); ua != "" {
		c.ua = ua
		if major := chromeMajorFromUA(ua); major != "" {
			c.chromeMajor = major
			c.chUA = ""
		}
	}
	c.deviceID = strings.TrimSpace(in.DeviceID)
	body := `{"gizmo_id":null,"requested_default_model":null,"conversation_id":null,"timezone_offset_min":-480}`
	path := "/backend-api/conversation/init"
	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com"+path, strings.NewReader(body))
	if err != nil {
		return AccessTokenProbeResult{ErrorCode: "request_build_failed", Temporary: true}, err
	}
	c.headers(req, "https://chatgpt.com/", false)
	req.Header.Set("authorization", "Bearer "+in.Token)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("origin", "https://chatgpt.com")
	req.Header.Set("x-openai-target-path", path)
	req.Header.Set("x-openai-target-route", path)
	if c.deviceID != "" {
		req.Header.Set("oai-device-id", c.deviceID)
	}
	req.Header.Set("oai-session-id", uuid.NewString())
	req.Header.Set("oai-language", primaryLanguageTag(languages))
	r, err := c.do(ctx, req)
	if err != nil {
		if r != nil {
			return AccessTokenProbeResult{HTTPStatus: r.Status, ErrorCode: statusCode(r.Status), Temporary: true}, err
		}
		return AccessTokenProbeResult{ErrorCode: "network_error", Temporary: true}, err
	}
	result := AccessTokenProbeResult{HTTPStatus: r.Status}
	if r.Status >= 200 && r.Status < 300 {
		result.Valid = true
		return result, nil
	}
	result.ErrorCode = upstreamErrorCode(r.Body, r.Status)
	result.Temporary = r.Status == http.StatusForbidden || r.Status == http.StatusTooManyRequests || r.Status >= 500
	if r.Status != http.StatusUnauthorized && !result.Temporary {
		result.Temporary = true
	}
	return result, nil
}

func upstreamErrorCode(body []byte, status int) string {
	var payload struct {
		Error json.RawMessage `json:"error"`
		Code  string          `json:"code"`
	}
	if json.Unmarshal(body, &payload) == nil {
		if payload.Code != "" {
			return safeErrorCode(payload.Code)
		}
		var nested struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(payload.Error, &nested) == nil && nested.Code != "" {
			return safeErrorCode(nested.Code)
		}
		var oauthCode string
		if json.Unmarshal(payload.Error, &oauthCode) == nil && oauthCode != "" {
			return safeErrorCode(oauthCode)
		}
	}
	return statusCode(status)
}

func safeErrorCode(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	var b strings.Builder
	for _, r := range code {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			b.WriteRune(r)
		}
		if b.Len() >= 64 {
			break
		}
	}
	if b.Len() == 0 {
		return "upstream_error"
	}
	return b.String()
}

func statusCode(status int) string {
	if status <= 0 {
		return "network_error"
	}
	return fmt.Sprintf("http_%d", status)
}

// IsDefinitiveAccessTokenError returns true only for the 401 codes that must
// never be exported or silently converted into a green state.
func IsDefinitiveAccessTokenError(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "token_revoked", "invalid_access_token", "token_expired", "http_401":
		return true
	default:
		return false
	}
}
