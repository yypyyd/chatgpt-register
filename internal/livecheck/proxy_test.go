package livecheck

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegistrationProbeUsesAuthenticatedProxy(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" || r.URL.Host != "registration-probe.example" {
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		fmt.Fprint(w, "through-registration-proxy")
	}))
	defer proxy.Close()
	client, closeClient, err := probeClient(strings.Replace(proxy.URL, "://", "://user:pass@", 1))
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	resp, err := client.Get("http://registration-probe.example/check")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "through-registration-proxy" {
		t.Fatal("registration self-check lost the assigned proxy")
	}
}

func TestInvalidProxyCannotTriggerDirectRegistrationSelfCheck(t *testing.T) {
	adobe := CheckAdobe(context.Background(), []AdobeItem{{ID: 1, Proxy: "ftp://proxy.example:8080", Cookies: []AdobeCookie{{Name: "session", Value: "test"}}}}, nil)
	leonardo := CheckLeonardo(context.Background(), []LeonardoItem{{ID: 1, Proxy: "ftp://proxy.example:8080", Cookies: []LeonardoCookie{{Name: "session", Value: "test"}}}}, nil)
	if adobe[1] != StatusUnknown || leonardo[1] != StatusUnknown {
		t.Fatal("an invalid proxy produced a credential verdict")
	}
}
