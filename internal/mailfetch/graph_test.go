package mailfetch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type graphRoundTripper func(*http.Request) (*http.Response, error)

func (r graphRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return r(req) }

func TestGraphMailboxReadReportsPartialFolderFailure(t *testing.T) {
	for _, failedFolder := range []string{"Inbox", "JunkEmail"} {
		t.Run(failedFolder, func(t *testing.T) {
			client := New()
			client.http.Transport = graphRoundTripper(func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.Path, "/"+failedFolder+"/") {
					return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"value":[{"id":"one","subject":"old mail","receivedDateTime":"2026-10-12T10:00:00Z"}]}`))}, nil
			})
			messages, err := client.listGraphMessages(context.Background(), "test-token", 15)
			if !errors.Is(err, ErrAuthTemporary) || len(messages) != 1 {
				t.Fatalf("partial read lost data or suppressed error: messages=%d err=%v", len(messages), err)
			}
		})
	}
}
