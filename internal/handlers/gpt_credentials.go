package handlers

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"chatgpt-register/internal/chatgptauth"
	"chatgpt-register/internal/codexreg"
	"chatgpt-register/internal/models"

	"gorm.io/gorm/clause"
)

const (
	gptCredentialConcurrency = 4
	gptMigrationSettingKey   = "internal_chatgpt_credential_migration_v2"
	categoryTokenExpired     = "token_expired"
)

// exportStats is intentionally aggregate-only: failures never echo an email,
// token, cookie, password or upstream response body into an API response.
type exportStats struct {
	RefreshedAndValid int `json:"refreshed_and_valid"`
	AlreadyValid      int `json:"already_valid"`
	TokenRevoked      int `json:"token_revoked"`
	TokenExpired      int `json:"token_expired"`
	RefreshFailed     int `json:"refresh_failed"`
	NeedsRelogin      int `json:"needs_relogin"`
	TemporaryUnknown  int `json:"temporary_unknown"`
}

func (s *exportStats) add(category string) {
	switch category {
	case chatgptauth.CategoryRefreshedAndValid:
		s.RefreshedAndValid++
	case chatgptauth.CategoryAlreadyValid:
		s.AlreadyValid++
	case chatgptauth.CategoryTokenRevoked:
		s.TokenRevoked++
	case categoryTokenExpired:
		s.TokenExpired++
	case chatgptauth.CategoryRefreshFailed:
		s.RefreshFailed++
	case chatgptauth.CategoryNeedsRelogin:
		s.NeedsRelogin++
	default:
		s.TemporaryUnknown++
	}
}

type credentialBatchResult struct {
	Outcomes []chatgptauth.Outcome
	Stats    exportStats
}

// classifyStoredCredential 只看库存状态，不访问网络。
// 已撤销、需重新登录、JWT 已过期的号不能出库；其余号直接导出库里的 access token 与网页 Cookie。
func classifyStoredCredential(reg models.Registration, now time.Time) chatgptauth.Outcome {
	out := chatgptauth.Outcome{Registration: reg}
	switch {
	case reg.NeedsRelogin || reg.AccountCredentialStatus == chatgptauth.CredentialNeedsRelogin:
		out.Category = chatgptauth.CategoryNeedsRelogin
	case reg.AccessTokenStatus == chatgptauth.TokenRevoked:
		out.Category = chatgptauth.CategoryTokenRevoked
	case reg.AccessTokenStatus == chatgptauth.TokenExpired:
		out.Category = categoryTokenExpired
	case storedTokenExpired(reg, now):
		out.Category = categoryTokenExpired
	case storedAccessToken(reg.AuthData) == "":
		out.Category = chatgptauth.CategoryNeedsRelogin
	default:
		out.Category = chatgptauth.CategoryAlreadyValid
		out.Exportable = true
	}
	return out
}

func storedAccessToken(authData string) string {
	var parsed map[string]any
	if json.Unmarshal([]byte(authData), &parsed) != nil {
		return ""
	}
	token, _ := parsed["access_token"].(string)
	return strings.TrimSpace(token)
}

func storedTokenExpired(reg models.Registration, now time.Time) bool {
	token := storedAccessToken(reg.AuthData)
	if meta, err := codexreg.ParseAccessTokenMetadata(token); err == nil && !meta.ExpiresAt.IsZero() {
		return !meta.ExpiresAt.After(now)
	}
	return reg.TokenExpiresAt != nil && !reg.TokenExpiresAt.After(now)
}

// runGPTCredentialBatch limits the whole batch to four accounts while the
// service itself serializes requests for the same account. The proxy chosen for
// an account is passed unchanged through refresh and probe.
func (h *Handler) runGPTCredentialBatch(ctx context.Context, regs []models.Registration) credentialBatchResult {
	result := credentialBatchResult{Outcomes: make([]chatgptauth.Outcome, len(regs))}
	if len(regs) == 0 {
		return result
	}
	manager := h.proxyManager()
	var wg sync.WaitGroup
	sem := make(chan struct{}, gptCredentialConcurrency)
	for i := range regs {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				result.Outcomes[i] = chatgptauth.Outcome{Category: chatgptauth.CategoryTemporaryUnknown, ErrorCode: "request_cancelled"}
				return
			}
			lease, err := manager.Acquire(ctx, "chatgpt", 2*time.Minute)
			if err != nil {
				result.Outcomes[i] = chatgptauth.Outcome{Category: chatgptauth.CategoryTemporaryUnknown, ErrorCode: "proxy_unavailable"}
				return
			}
			proxyCtx, cancelProxy := lease.Context(ctx)
			defer cancelProxy()
			result.Outcomes[i] = h.GPTCredentials.EnsureExportable(proxyCtx, regs[i].ID, lease.Proxy)
		}()
	}
	wg.Wait()
	for _, out := range result.Outcomes {
		result.Stats.add(out.Category)
	}
	return result
}

// startGPTCredentialMigration performs the requested one-time revalidation of
// legacy records. It never deletes registrations; unavailable long-term
// credentials become needs_relogin and revoked ATs remain attached but blocked.
func (h *Handler) startGPTCredentialMigration() {
	if h.GPTCredentials == nil || h.settingValue(gptMigrationSettingKey) == "done" {
		return
	}
	go func() {
		var regs []models.Registration
		if err := h.DB.Select("id").Where("status = ?", "registered").Order("id").Find(&regs).Error; err != nil {
			log.Printf("ChatGPT credential migration load failed")
			return
		}
		result := h.runGPTCredentialBatch(context.Background(), regs)
		setting := models.Setting{Key: gptMigrationSettingKey, Value: "done"}
		if err := h.DB.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "key"}},
			DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at"}),
		}).Create(&setting).Error; err != nil {
			log.Printf("ChatGPT credential migration checkpoint failed")
			return
		}
		log.Printf("ChatGPT credential migration complete: valid=%d revoked=%d relogin=%d unknown=%d",
			result.Stats.RefreshedAndValid+result.Stats.AlreadyValid,
			result.Stats.TokenRevoked, result.Stats.NeedsRelogin,
			result.Stats.RefreshFailed+result.Stats.TemporaryUnknown)
	}()
}
