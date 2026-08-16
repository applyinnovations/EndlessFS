package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/config"
)

func TestIntegrationPublicEndpoints(t *testing.T) {
	t.Parallel()

	handler := New(config.PublicConfig{InviteRegistration: true}, "test-version")
	tests := []struct {
		path        string
		contentType string
		body        string
	}{
		{path: "/healthz", contentType: "text/plain; charset=utf-8", body: "ok\n"},
		{path: "/readyz", contentType: "text/plain; charset=utf-8", body: "ready\n"},
		{path: "/", contentType: "text/html; charset=utf-8", body: "EndlessFS"},
		{path: "/bootstrap", contentType: "text/html; charset=utf-8", body: "EndlessFS"},
		{path: "/register", contentType: "text/html; charset=utf-8", body: "EndlessFS"},
		{path: "/trash", contentType: "text/html; charset=utf-8", body: "EndlessFS"},
		{path: "/settings", contentType: "text/html; charset=utf-8", body: "EndlessFS"},
		{path: "/admin", contentType: "text/html; charset=utf-8", body: "EndlessFS"},
		{path: "/assets/app.css", contentType: "text/css; charset=utf-8", body: "color-scheme"},
		{path: "/assets/app.js", contentType: "text/javascript; charset=utf-8", body: "addEventListener"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.path, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
			}
			if got := response.Header().Get("Content-Type"); got != test.contentType {
				t.Fatalf("Content-Type = %q, want %q", got, test.contentType)
			}
			if !strings.Contains(response.Body.String(), test.body) {
				t.Fatalf("body %q does not contain %q", response.Body.String(), test.body)
			}
			assertSecurityHeaders(t, response.Header())
		})
	}
}

func TestIntegrationPublicConfigExposesNoSecrets(t *testing.T) {
	t.Parallel()

	handler := New(config.PublicConfig{AllowRegistration: true, InviteRegistration: false}, "v0-test")
	request := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	var value map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if value["product"] != "EndlessFS" || value["version"] != "v0-test" {
		t.Fatalf("response = %#v", value)
	}
	if value["allowRegistration"] != true || value["inviteRegistration"] != false {
		t.Fatalf("response = %#v", value)
	}
	for key := range value {
		if strings.Contains(strings.ToLower(key), "secret") || strings.Contains(strings.ToLower(key), "token") {
			t.Fatalf("secret-shaped key exposed: %q", key)
		}
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestIntegrationMutatingPublicRouteIsNotAccepted(t *testing.T) {
	t.Parallel()

	handler := New(config.PublicConfig{}, "test")
	request := httptest.NewRequest(http.MethodPost, "/healthz", strings.NewReader("payload"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}

func TestUnknownPathDoesNotFallBackToApplicationShell(t *testing.T) {
	t.Parallel()

	handler := New(config.PublicConfig{}, "test")
	request := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

func assertSecurityHeaders(t *testing.T, header http.Header) {
	t.Helper()
	for _, name := range []string{
		"Content-Security-Policy",
		"Cross-Origin-Opener-Policy",
		"Permissions-Policy",
		"Referrer-Policy",
		"X-Content-Type-Options",
	} {
		if header.Get(name) == "" {
			t.Errorf("%s is missing", name)
		}
	}
	if csp := header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-src 'self'") {
		t.Errorf("Content-Security-Policy does not constrain preview frames: %q", csp)
	}
}
