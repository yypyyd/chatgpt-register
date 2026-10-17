package codexreg

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	"github.com/google/uuid"
)

const clientAuthSessionDumpURL = "https://auth.openai.com/api/accounts/client_auth_session_dump"
const initialPasswordlessOTPURL = "https://auth.openai.com/api/accounts/passwordless/send-otp"

// initialEmailSession is the subset used by the official email-verification
// clientLoader before it sends the first passwordless OTP.
type initialEmailSession struct {
	SessionID   string `json:"session_id"`
	Email       string `json:"email"`
	Mode        string `json:"email_verification_mode"`
	SendPending bool   `json:"passwordless_email_otp_send_pending"`
	Missing     bool   `json:"is_missing_session"`
}

type preparedInitialEmailSend struct {
	SentinelToken string
	ObserverToken string
}

// parseClientAuthSessionDump accepts both the current dump envelope and a
// direct session object, which keeps the parser tolerant of auth API rollouts.
func parseClientAuthSessionDump(body []byte) (initialEmailSession, error) {
	var envelope struct {
		ClientAuthSession json.RawMessage `json:"client_auth_session"`
		SessionID         string          `json:"session_id"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return initialEmailSession{}, fmt.Errorf("解析 auth session 失败: %w", err)
	}
	if len(envelope.ClientAuthSession) > 0 && string(envelope.ClientAuthSession) != "null" {
		var session initialEmailSession
		if err := json.Unmarshal(envelope.ClientAuthSession, &session); err != nil {
			return initialEmailSession{}, fmt.Errorf("解析 client_auth_session 失败: %w", err)
		}
		if session.SessionID == "" {
			session.SessionID = envelope.SessionID
		}
		return session, nil
	}
	var session initialEmailSession
	if err := json.Unmarshal(body, &session); err != nil {
		return initialEmailSession{}, fmt.Errorf("解析 auth session 失败: %w", err)
	}
	return session, nil
}

func (s initialEmailSession) eligibleForInitialSend() bool {
	if !s.SendPending || strings.TrimSpace(s.SessionID) == "" || strings.TrimSpace(s.Email) == "" {
		return false
	}
	return s.Mode == "" || s.Mode == "passwordless_login" || s.Mode == "passwordless_signup"
}

// runInitialEmailSend mirrors the official clientLoader's ordering. Keeping
// the orchestration callback-based makes the identity/race checks testable
// without minting live Sentinel tokens in unit tests.
func runInitialEmailSend(
	ctx context.Context,
	readSession func(context.Context) (initialEmailSession, error),
	prepare func(context.Context) (preparedInitialEmailSend, error),
	send func(context.Context, preparedInitialEmailSend) error,
	now func() time.Time,
) (time.Time, bool, error) {
	first, err := readSession(ctx)
	if err != nil {
		return time.Time{}, false, err
	}
	if !first.eligibleForInitialSend() {
		return time.Time{}, false, nil
	}
	prepared, err := prepare(ctx)
	if err != nil {
		return time.Time{}, false, err
	}
	current, err := readSession(ctx)
	if err != nil {
		return time.Time{}, false, err
	}
	if current.Missing || current.SessionID != first.SessionID || current.Email != first.Email || current.Mode != first.Mode {
		return time.Time{}, false, fmt.Errorf("passwordless email attempt changed")
	}
	if !current.SendPending {
		return time.Time{}, false, nil
	}
	sentAt := now().UTC().Truncate(time.Second)
	if err := send(ctx, prepared); err != nil {
		return time.Time{}, false, err
	}
	return sentAt, true, nil
}

func (c *protocolClient) readInitialEmailSession(ctx context.Context, referer string) (initialEmailSession, error) {
	if c.initialEmailSessionCache != nil {
		return *c.initialEmailSessionCache, nil
	}
	req, err := http.NewRequest(http.MethodGet, clientAuthSessionDumpURL, nil)
	if err != nil {
		return initialEmailSession{}, err
	}
	c.headers(req, referer, false)
	req.Header.Set("accept", "application/json")
	resp, err := c.do(ctx, req)
	if err != nil {
		return initialEmailSession{}, fmt.Errorf("读取 auth session 失败: %w", err)
	}
	if resp.Status != http.StatusOK {
		return initialEmailSession{}, fmt.Errorf("读取 auth session HTTP %d", resp.Status)
	}
	session, err := parseClientAuthSessionDump(resp.Body)
	if err != nil {
		return initialEmailSession{}, err
	}
	c.initialEmailSessionCache = &session
	return session, nil
}

func (c *protocolClient) prepareInitialEmailSend(ctx context.Context, referer string) (preparedInitialEmailSend, error) {
	token, observer, err := c.realSentinelToken(ctx, "authorize_continue", referer)
	if err != nil {
		return preparedInitialEmailSend{}, fmt.Errorf("Sentinel SDK 签发失败(authorize_continue): %w", err)
	}
	if err := rejectForgedSentinelToken(token); err != nil {
		return preparedInitialEmailSend{}, fmt.Errorf("Sentinel SDK 签发失败(authorize_continue): %w", err)
	}
	return preparedInitialEmailSend{SentinelToken: token, ObserverToken: observer}, nil
}

func (c *protocolClient) postInitialPasswordlessOTP(ctx context.Context, referer string, prepared preparedInitialEmailSend) error {
	req, err := http.NewRequest(http.MethodPost, initialPasswordlessOTPURL, strings.NewReader(""))
	if err != nil {
		return err
	}
	c.headers(req, referer, false)
	req.Header.Set("accept", "application/json")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("origin", "https://auth.openai.com")
	if c.navID == "" {
		c.navID = uuid.NewString()
	}
	req.Header.Set("x-openai-document-navigation-id", c.navID)
	req.Header.Set("x-access-flow-invocation-id", uuid.NewString())
	setDatadogTraceHeaders(req.Header)
	req.Header.Set("openai-sentinel-token", prepared.SentinelToken)
	if prepared.ObserverToken != "" {
		req.Header.Set("openai-sentinel-so-token", prepared.ObserverToken)
	}
	resp, err := c.do(ctx, req)
	if err != nil {
		return fmt.Errorf("首次发码请求失败: %w", err)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		code, msg, _ := parseAuthError(resp.Body)
		if code != "" {
			return fmt.Errorf("首次发码被拒 (HTTP %d code=%s): %s", resp.Status, code, truncateForLog(msg, 160))
		}
		return fmt.Errorf("首次发码未被接受 (HTTP %d)", resp.Status)
	}
	if code, msg, _ := parseAuthError(resp.Body); code != "" {
		return fmt.Errorf("首次发码被拒 (HTTP %d code=%s): %s", resp.Status, code, truncateForLog(msg, 160))
	}
	return nil
}

func (c *protocolClient) sendInitialEmailCodeIfPending(ctx context.Context, landing string) (time.Time, bool, error) {
	read := func(ctx context.Context) (initialEmailSession, error) {
		return c.readInitialEmailSession(ctx, landing)
	}
	prepare := func(ctx context.Context) (preparedInitialEmailSend, error) {
		return c.prepareInitialEmailSend(ctx, landing)
	}
	send := func(ctx context.Context, prepared preparedInitialEmailSend) error {
		return c.postInitialPasswordlessOTP(ctx, landing, prepared)
	}

	first, err := read(ctx)
	if err != nil {
		return time.Time{}, false, err
	}
	if !first.eligibleForInitialSend() {
		c.in.logf("📧 当前会话无需执行 passwordless 首次发码 pending=%t mode=%s", first.SendPending, first.Mode)
		return time.Time{}, false, nil
	}
	c.in.logf("📧 已确认 passwordless 首次发码待处理 mode=%s", first.Mode)

	// Supply the already-read first snapshot so the shared runner does not add
	// a third network read. It still performs the mandatory post-token recheck.
	reads := 0
	readWithFirst := func(ctx context.Context) (initialEmailSession, error) {
		reads++
		if reads == 1 {
			return first, nil
		}
		return read(ctx)
	}
	sentAt, sent, err := runInitialEmailSend(ctx, readWithFirst, prepare, send, time.Now)
	if err != nil {
		return time.Time{}, false, err
	}
	if sent {
		c.in.logf("📧 passwordless 首次发码请求已被接受，等待收件确认")
	} else {
		c.in.logf("📧 Sentinel 签发期间待发送状态已清除，不重复发信")
	}
	return sentAt, sent, nil
}
