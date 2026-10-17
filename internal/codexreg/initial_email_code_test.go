package codexreg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

func TestParseClientAuthSessionDump(t *testing.T) {
	body := []byte(`{"client_auth_session":{"session_id":"inner","email":"person@example.com","email_verification_mode":"passwordless_signup","passwordless_email_otp_send_pending":true},"checksum":"sum","session_id":"outer"}`)
	got, err := parseClientAuthSessionDump(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "inner" || got.Email != "person@example.com" || got.Mode != "passwordless_signup" || !got.SendPending {
		t.Fatalf("unexpected session: %+v", got)
	}

	body = []byte(`{"client_auth_session":{"email":"person@example.com","passwordless_email_otp_send_pending":true},"session_id":"outer"}`)
	got, err = parseClientAuthSessionDump(body)
	if err != nil || got.SessionID != "outer" {
		t.Fatalf("outer session id fallback failed: %+v err=%v", got, err)
	}
}

func TestReadInitialEmailSessionUsesClientCacheForRecheck(t *testing.T) {
	requests := 0
	c := &protocolClient{cli: emailCodeHTTP{do: func(req *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"client_auth_session":{"session_id":"s","email":"person@example.com","passwordless_email_otp_send_pending":true}}`))}, nil
	}}}
	first, err := c.readInitialEmailSession(context.Background(), "https://auth.openai.com/email-verification")
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.readInitialEmailSession(context.Background(), "https://auth.openai.com/email-verification")
	if err != nil || first != second || requests != 1 {
		t.Fatalf("first=%+v second=%+v requests=%d err=%v", first, second, requests, err)
	}
}

func TestRunInitialEmailSendPendingSignup(t *testing.T) {
	ctx := context.Background()
	session := initialEmailSession{SessionID: "session-1", Email: "person@example.com", Mode: "passwordless_signup", SendPending: true}
	reads, prepares, sends := 0, 0, 0
	wantNow := time.Date(2026, 10, 14, 1, 2, 3, 987654321, time.FixedZone("test", 8*60*60))
	sentAt, sent, err := runInitialEmailSend(ctx,
		func(context.Context) (initialEmailSession, error) { reads++; return session, nil },
		func(context.Context) (preparedInitialEmailSend, error) {
			prepares++
			return preparedInitialEmailSend{SentinelToken: "token", ObserverToken: "observer"}, nil
		},
		func(_ context.Context, got preparedInitialEmailSend) error {
			sends++
			if got.SentinelToken != "token" || got.ObserverToken != "observer" {
				t.Fatalf("prepared tokens changed: %+v", got)
			}
			return nil
		},
		func() time.Time { return wantNow },
	)
	if err != nil || !sent || reads != 2 || prepares != 1 || sends != 1 {
		t.Fatalf("sent=%t reads=%d prepares=%d sends=%d err=%v", sent, reads, prepares, sends, err)
	}
	if !sentAt.Equal(wantNow.UTC().Truncate(time.Second)) || sentAt.Nanosecond() != 0 {
		t.Fatalf("sentAt=%s", sentAt)
	}
}

func TestRunInitialEmailSendSkipsIneligibleSessions(t *testing.T) {
	cases := []initialEmailSession{
		{SessionID: "s", Email: "person@example.com", Mode: "passwordless_signup", SendPending: false},
		{SessionID: "s", Email: "person@example.com", Mode: "onboarding", SendPending: true},
		{SessionID: "", Email: "person@example.com", Mode: "passwordless_signup", SendPending: true},
		{SessionID: "s", Email: "", Mode: "passwordless_signup", SendPending: true},
	}
	for i, session := range cases {
		t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
			reads := 0
			_, sent, err := runInitialEmailSend(context.Background(),
				func(context.Context) (initialEmailSession, error) { reads++; return session, nil },
				func(context.Context) (preparedInitialEmailSend, error) {
					t.Fatal("prepared ineligible session")
					return preparedInitialEmailSend{}, nil
				},
				func(context.Context, preparedInitialEmailSend) error { t.Fatal("sent ineligible session"); return nil },
				time.Now,
			)
			if err != nil || sent || reads != 1 {
				t.Fatalf("sent=%t reads=%d err=%v", sent, reads, err)
			}
		})
	}
}

func TestRunInitialEmailSendRejectsChangedAttempt(t *testing.T) {
	first := initialEmailSession{SessionID: "session-1", Email: "person@example.com", Mode: "passwordless_signup", SendPending: true}
	changes := []initialEmailSession{
		{SessionID: "session-2", Email: first.Email, Mode: first.Mode, SendPending: true},
		{SessionID: first.SessionID, Email: "other@example.com", Mode: first.Mode, SendPending: true},
		{SessionID: first.SessionID, Email: first.Email, Mode: "passwordless_login", SendPending: true},
		{SessionID: first.SessionID, Email: first.Email, Mode: first.Mode, SendPending: true, Missing: true},
	}
	for i, second := range changes {
		t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
			reads, sends := 0, 0
			_, sent, err := runInitialEmailSend(context.Background(),
				func(context.Context) (initialEmailSession, error) {
					reads++
					if reads == 1 {
						return first, nil
					}
					return second, nil
				},
				func(context.Context) (preparedInitialEmailSend, error) {
					return preparedInitialEmailSend{SentinelToken: "token"}, nil
				},
				func(context.Context, preparedInitialEmailSend) error { sends++; return nil },
				time.Now,
			)
			if err == nil || sent || sends != 0 || !strings.Contains(err.Error(), "attempt changed") {
				t.Fatalf("sent=%t sends=%d err=%v", sent, sends, err)
			}
		})
	}
}

func TestRunInitialEmailSendStopsWhenPendingClears(t *testing.T) {
	first := initialEmailSession{SessionID: "session-1", Email: "person@example.com", Mode: "passwordless_signup", SendPending: true}
	second := first
	second.SendPending = false
	reads := 0
	_, sent, err := runInitialEmailSend(context.Background(),
		func(context.Context) (initialEmailSession, error) {
			reads++
			if reads == 1 {
				return first, nil
			}
			return second, nil
		},
		func(context.Context) (preparedInitialEmailSend, error) {
			return preparedInitialEmailSend{SentinelToken: "token"}, nil
		},
		func(context.Context, preparedInitialEmailSend) error {
			t.Fatal("sent after pending cleared")
			return nil
		},
		time.Now,
	)
	if err != nil || sent {
		t.Fatalf("sent=%t err=%v", sent, err)
	}
}

func TestPostInitialPasswordlessOTP(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{name: "accepted", status: 200, body: `{}`},
		{name: "http_error", status: 429, body: `{}`, wantErr: true},
		{name: "json_error", status: 200, body: `{"error":{"code":"rate_limit_exceeded","message":"limited"}}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			c := &protocolClient{cli: emailCodeHTTP{do: func(req *http.Request) (*http.Response, error) {
				requests++
				if req.Method != http.MethodPost || req.URL.Path != "/api/accounts/passwordless/send-otp" {
					t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
				}
				if req.Header.Get("openai-sentinel-token") != "sdk-token" || req.Header.Get("openai-sentinel-so-token") != "so-token" {
					t.Fatalf("missing sentinel headers")
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}}}
			err := c.postInitialPasswordlessOTP(context.Background(), "https://auth.openai.com/email-verification", preparedInitialEmailSend{SentinelToken: "sdk-token", ObserverToken: "so-token"})
			if (err != nil) != tc.wantErr || requests != 1 {
				t.Fatalf("requests=%d err=%v", requests, err)
			}
		})
	}
}

func TestRunInitialEmailSendPropagatesFailures(t *testing.T) {
	session := initialEmailSession{SessionID: "s", Email: "person@example.com", Mode: "passwordless_signup", SendPending: true}
	want := errors.New("send rejected")
	_, sent, err := runInitialEmailSend(context.Background(),
		func(context.Context) (initialEmailSession, error) { return session, nil },
		func(context.Context) (preparedInitialEmailSend, error) {
			return preparedInitialEmailSend{SentinelToken: "token"}, nil
		},
		func(context.Context, preparedInitialEmailSend) error { return want },
		time.Now,
	)
	if sent || !errors.Is(err, want) {
		t.Fatalf("sent=%t err=%v", sent, err)
	}
}
