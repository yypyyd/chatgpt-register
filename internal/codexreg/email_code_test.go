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
	tlsclient "github.com/bogdanfinn/tls-client"
)

type emailCodeHTTP struct {
	tlsclient.HttpClient
	do func(*http.Request) (*http.Response, error)
}

func (c emailCodeHTTP) Do(req *http.Request) (*http.Response, error) { return c.do(req) }

func TestFetchEmailCodeResendsOnceAfterConfirmedMissingMail(t *testing.T) {
	const landing = "https://auth.openai.com/email-verification"
	initialSince := time.Now().Add(-3 * time.Minute)
	fetches, resends := 0, 0
	var resendStarted time.Time
	c := &protocolClient{
		cli: emailCodeHTTP{do: func(req *http.Request) (*http.Response, error) {
			resends++
			resendStarted = time.Now()
			if req.Method != "POST" || req.URL.Path != "/api/accounts/email-otp/resend" || req.Header.Get("referer") != landing {
				t.Fatalf("unexpected resend: %s %s", req.Method, req.URL.Path)
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}},
		in: Input{FetchCode: func(_ context.Context, since time.Time) (string, error) {
			fetches++
			if fetches == 1 {
				if !since.Equal(initialSince) {
					t.Fatal("initial mail cutoff changed")
				}
				return "", fmt.Errorf("mail polling: %w", ErrEmailCodeNotReceived)
			}
			if since.Nanosecond() != 0 || since.Before(resendStarted.Add(-time.Second)) || since.After(resendStarted) {
				t.Fatalf("resend mail cutoff must allow delivery in the request second: %s", since)
			}
			return "123456", nil
		}},
	}
	code, resent, err := c.fetchEmailCode(context.Background(), landing, initialSince)
	if err != nil || code != "123456" || !resent || fetches != 2 || resends != 1 {
		t.Fatalf("code=%q resent=%t fetches=%d resends=%d err=%v", code, resent, fetches, resends, err)
	}
}

func TestFetchEmailCodeDoesNotResendForMailboxFailure(t *testing.T) {
	for _, fetchErr := range []error{errors.New("mailbox read failed"), context.Canceled, context.DeadlineExceeded, nil} {
		c := &protocolClient{
			cli: emailCodeHTTP{do: func(*http.Request) (*http.Response, error) {
				t.Fatal("resend issued without a confirmed missing-mail timeout")
				return nil, nil
			}},
			in: Input{FetchCode: func(context.Context, time.Time) (string, error) { return "654321", fetchErr }},
		}
		_, resent, err := c.fetchEmailCode(context.Background(), "https://auth.openai.com/email-verification", time.Time{})
		if resent || !errors.Is(err, fetchErr) {
			t.Fatalf("resent=%t err=%v wanted=%v", resent, err, fetchErr)
		}
	}
}

func TestFetchEmailCodeStopsAfterOneResend(t *testing.T) {
	fetches, resends := 0, 0
	c := &protocolClient{
		cli: emailCodeHTTP{do: func(*http.Request) (*http.Response, error) {
			resends++
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}},
		in: Input{FetchCode: func(context.Context, time.Time) (string, error) {
			fetches++
			return "", ErrEmailCodeNotReceived
		}},
	}
	_, resent, err := c.fetchEmailCode(context.Background(), "https://auth.openai.com/email-verification", time.Time{})
	if !resent || !errors.Is(err, ErrEmailCodeNotReceived) || fetches != 2 || resends != 1 {
		t.Fatalf("resent=%t err=%v fetches=%d resends=%d", resent, err, fetches, resends)
	}
}

func TestFetchEmailCodeStopsWhenResendIsRejected(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{429, `{}`},
		{403, `{"error":{"code":"access_denied"}}`},
		{200, `{"error":{"code":"rate_limit_exceeded"}}`},
	} {
		t.Run(fmt.Sprintf("status_%d", tc.status), func(t *testing.T) {
			fetches := 0
			c := &protocolClient{
				cli: emailCodeHTTP{do: func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
				}},
				in: Input{FetchCode: func(context.Context, time.Time) (string, error) {
					fetches++
					return "", ErrEmailCodeNotReceived
				}},
			}
			_, _, err := c.fetchEmailCode(context.Background(), "https://auth.openai.com/email-verification", time.Time{})
			if err == nil || fetches != 1 || errors.Is(err, ErrEmailCodeNotReceived) {
				t.Fatalf("rejected resend was treated as accepted: fetches=%d err=%v", fetches, err)
			}
		})
	}
}

func TestFetchEmailCodeDoesNotResendAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &protocolClient{in: Input{FetchCode: func(context.Context, time.Time) (string, error) {
		cancel()
		return "", ErrEmailCodeNotReceived
	}}}
	_, resent, err := c.fetchEmailCode(ctx, "https://auth.openai.com/email-verification", time.Time{})
	if resent || !errors.Is(err, context.Canceled) {
		t.Fatalf("resent=%t err=%v", resent, err)
	}
}
