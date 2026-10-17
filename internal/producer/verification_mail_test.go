package producer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"chatgpt-register/internal/codexreg"
	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"
)

type stubVerificationMailbox struct {
	list func(context.Context) ([]mailfetch.Message, error)
	get  func(context.Context, string) (mailfetch.Message, error)
}

func (s stubVerificationMailbox) ListMessages(ctx context.Context, _ mailfetch.Account, _ int) ([]mailfetch.Message, error) {
	return s.list(ctx)
}

func (s stubVerificationMailbox) GetMessage(ctx context.Context, _ mailfetch.Account, id string) (mailfetch.Message, error) {
	return s.get(ctx, id)
}

func TestPollCodeReadsBodyAndSelectsNewestMatchingMail(t *testing.T) {
	now := time.Now()
	mail := stubVerificationMailbox{
		list: func(context.Context) ([]mailfetch.Message, error) {
			return []mailfetch.Message{
				{ID: "expired", From: "noreply@tm.openai.com", Subject: "Verification 111111", ReceivedAt: now.Add(-time.Hour)},
				{ID: "unrelated", From: "example@example.com", Subject: "Receipt 222222", ReceivedAt: now},
				{ID: "older", From: "noreply@tm.openai.com", Subject: "Verification 333333", ReceivedAt: now.Add(-time.Second)},
				{ID: "latest", From: "noreply@tm.openai.com", Subject: "Your temporary ChatGPT verification code", ReceivedAt: now},
			}, nil
		},
		get: func(_ context.Context, id string) (mailfetch.Message, error) {
			if id != "latest" {
				t.Fatalf("unexpected body request: %s", id)
			}
			return mailfetch.Message{Text: "Your verification code is 444444."}, nil
		},
	}
	p := &Producer{mail: mail}
	code, err := p.pollCode(context.Background(), models.Mailbox{}, now.Add(-time.Minute), time.Second, time.Millisecond)
	if err != nil || code != "444444" {
		t.Fatalf("code=%q err=%v", code, err)
	}
}

func TestPollCodeDistinguishesMailboxFailuresFromMissingMail(t *testing.T) {
	now := time.Now()
	readFailure := errors.New("upstream read failed")
	for _, tc := range []struct {
		name     string
		messages []mailfetch.Message
		listErr  error
		bodyErr  error
		wantText string
		wantErr  error
	}{
		{name: "list failure", listErr: readFailure, wantText: "读取邮件列表失败", wantErr: readFailure},
		{name: "body failure", messages: []mailfetch.Message{{ID: "one", From: "noreply@tm.openai.com", ReceivedAt: now}}, bodyErr: readFailure, wantText: "读取正文失败", wantErr: readFailure},
		{name: "message without code", messages: []mailfetch.Message{{ID: "one", From: "noreply@tm.openai.com", ReceivedAt: now}}, wantText: "未提取到 6 位验证码"},
		{name: "no matching message", messages: []mailfetch.Message{{ID: "old", From: "noreply@tm.openai.com", Subject: "Code 123456", ReceivedAt: now.Add(-time.Hour)}}, wantText: "邮箱读取正常", wantErr: codexreg.ErrEmailCodeNotReceived},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Producer{mail: stubVerificationMailbox{
				list: func(context.Context) ([]mailfetch.Message, error) { return tc.messages, tc.listErr },
				get:  func(context.Context, string) (mailfetch.Message, error) { return mailfetch.Message{}, tc.bodyErr },
			}}
			_, err := p.pollCode(context.Background(), models.Mailbox{}, now.Add(-time.Minute), 50*time.Millisecond, time.Millisecond)
			if err == nil || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("expected %q, got %v", tc.wantText, err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("cause not preserved: %v", err)
			}
			if tc.name != "no matching message" && errors.Is(err, codexreg.ErrEmailCodeNotReceived) {
				t.Fatalf("mailbox failure must not trigger resend: %v", err)
			}
		})
	}
}

func TestPollCodeRetriesTransientBodyReadFailure(t *testing.T) {
	now := time.Now()
	bodyCalls := 0
	p := &Producer{mail: stubVerificationMailbox{
		list: func(context.Context) ([]mailfetch.Message, error) {
			return []mailfetch.Message{{ID: "one", From: "noreply@tm.openai.com", ReceivedAt: now}}, nil
		},
		get: func(context.Context, string) (mailfetch.Message, error) {
			bodyCalls++
			if bodyCalls == 1 {
				return mailfetch.Message{}, errors.New("temporary body failure")
			}
			return mailfetch.Message{Text: "Verification: 555555"}, nil
		},
	}}
	code, err := p.pollCode(context.Background(), models.Mailbox{}, now.Add(-time.Minute), time.Second, time.Millisecond)
	if err != nil || code != "555555" || bodyCalls != 2 {
		t.Fatalf("code=%q err=%v bodyCalls=%d", code, err, bodyCalls)
	}
}

func TestPollCodeStopsOnPermanentMailboxAuthenticationFailure(t *testing.T) {
	calls := 0
	p := &Producer{mail: stubVerificationMailbox{list: func(context.Context) ([]mailfetch.Message, error) {
		calls++
		return nil, fmt.Errorf("OAuth: %w", mailfetch.ErrAuthFailed)
	}}}
	_, err := p.pollCode(context.Background(), models.Mailbox{}, time.Time{}, time.Second, time.Millisecond)
	if !errors.Is(err, mailfetch.ErrAuthFailed) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestPollCodeDeadlineBoundsMailboxRequest(t *testing.T) {
	p := &Producer{mail: stubVerificationMailbox{list: func(ctx context.Context) ([]mailfetch.Message, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}}
	started := time.Now()
	_, err := p.pollCode(context.Background(), models.Mailbox{}, time.Time{}, 25*time.Millisecond, time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "读取邮件列表失败") {
		t.Fatalf("stalled mailbox reported incorrectly: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("mailbox request exceeded polling deadline")
	}
}

func TestPollCodePreservesCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &Producer{mail: stubVerificationMailbox{list: func(context.Context) ([]mailfetch.Message, error) {
		t.Fatal("mailbox called after cancellation")
		return nil, nil
	}}}
	_, err := p.pollCode(ctx, models.Mailbox{}, time.Time{}, time.Second, time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}
