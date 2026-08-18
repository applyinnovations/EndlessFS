package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestApplicationShellExposesCompleteAccessibleWorkspaces(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"/", "/bootstrap", "/register", "/settings", "/trash", "/admin"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", path, response.Code)
		}
		body := response.Body.String()
		for _, required := range []string{
			`href="#workspace"`, `id="workspace"`, `id="live-status"`,
			`id="drive-view"`, `id="trash-view"`, `id="settings-view"`, `id="admin-view"`,
			`id="upload-input"`, `webkitdirectory`, `id="share-list"`, `role="dialog"`,
		} {
			if !strings.Contains(body, required) {
				t.Errorf("GET %s shell is missing %q", path, required)
			}
		}
	}
}

func TestBrowserSourceKeepsSecretsEphemeralAndUntrustedTextOutOfHTML(t *testing.T) {
	t.Parallel()

	script := string(mustRead("static/app.js"))
	for _, forbidden := range []string{"localStorage", "sessionStorage", "indexedDB", "innerHTML", "document.write", "serviceWorker"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("browser source contains forbidden API %q", forbidden)
		}
	}
	for _, required := range []string{
		"navigator.credentials.create", "navigator.credentials.get", "textContent",
		"Upload-Offset", "Content-Range", "webkitRelativePath", "history.replaceState",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("browser source is missing workflow primitive %q", required)
		}
	}
}

func TestMediaBrowserShellUsesVirtualizedLazyWebPGridAndAccessibleViewer(t *testing.T) {
	t.Parallel()

	shell := string(mustRead("static/index.html"))
	for _, required := range []string{
		`id="file-view-list"`, `id="file-view-grid"`, `id="media-grid"`, `aria-label="File presentation"`,
		`id="filter-kind"`, `id="filter-media"`, `id="filter-preview"`,
		`id="preview-previous"`, `id="preview-next"`, `id="preview-generate"`, `id="preview-regenerate"`, `id="preview-original"`,
	} {
		if !strings.Contains(shell, required) {
			t.Errorf("media browser shell is missing %q", required)
		}
	}
	for _, alwaysAvailable := range []string{`<fieldset id="file-presentation" class="presentation-choice" aria-label="File presentation">`, `<div id="metadata-filters" class="metadata-filters">`} {
		if !strings.Contains(shell, alwaysAvailable) {
			t.Errorf("media browsing control is not always available: missing %q", alwaysAvailable)
		}
	}
	script := string(mustRead("static/app.js"))
	for _, required := range []string{
		"renderVirtualGrid", "IntersectionObserver", "gridOverscanRows = 3", "URL.revokeObjectURL",
		"/api/v1/previews/resolve", "/api/v1/previews/generations", "/api/v1/previews/operations/", "image/webp", "validatedPreviewBlob", "Invalid preview artifact body", "filterLoadedEntries",
		`crypto.subtle.digest("SHA-256"`, "Invalid preview artifact checksum",
		"waitForPreviewOperation", "previewRetryTimers", "Idempotency-Key",
		"ArrowLeft", "ArrowRight",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("media browser script is missing %q", required)
		}
	}
	if strings.Contains(script, "mediaBrowserEnabled") {
		t.Error("media browsing is incorrectly gated by optional preview configuration")
	}
	stylesheet := string(mustRead("static/app.css"))
	for _, required := range []string{".media-grid", ".media-tile", "aspect-ratio: 1", "object-fit: contain"} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("media browser stylesheet is missing %q", required)
		}
	}
}

func TestUnknownBrowserRouteIsNotAClientSideFallback(t *testing.T) {
	t.Parallel()

	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/unknown", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
}

func TestThemeResolverCanOnlyInjectValidatedSameOriginStylesheet(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		resolved string
		want     string
	}{
		{name: "validated", resolved: "/assets/themes/digest/theme.css", want: `href="/assets/themes/digest/theme.css"`},
		{name: "external rejected", resolved: "https://example.test/theme.css", want: themeLink},
		{name: "attribute injection rejected", resolved: `/assets/themes/x/theme.css\" onload=\"bad`, want: themeLink},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			Handler(func(*http.Request) string { return test.resolved }).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
			if !strings.Contains(response.Body.String(), test.want) {
				t.Fatalf("shell does not contain %q", test.want)
			}
		})
	}
}
