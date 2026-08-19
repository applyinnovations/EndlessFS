package web

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPinnedInterAssetsMatchApprovedReleaseDigests(t *testing.T) {
	t.Parallel()
	for name, expected := range map[string]string{
		"ui/fonts/inter-regular.woff2":  "b6f9db9e45be20f3c1312c97fbee7ec36b7d8280f8caa4d53c9ba0408cc9997a",
		"ui/fonts/inter-medium.woff2":   "8458f8afa67b5691c1fcbe51607a2dafb53a9839e48131c608a186b65415d96d",
		"ui/fonts/inter-semibold.woff2": "8e52a861dc26ff4608c50bd7ff89b65d0d6216a2afe7b47ce5d84544811ca400",
	} {
		data := mustRead(name)
		if len(data) < 4 || string(data[:4]) != "wOF2" {
			t.Errorf("%s is not WOFF2", name)
			continue
		}
		if digest := fmt.Sprintf("%x", sha256.Sum256(data)); digest != expected {
			t.Errorf("%s digest = %s, want %s", name, digest, expected)
		}
	}
}

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

	script := string(mustRead("ui/app.js"))
	for _, forbidden := range []string{"localStorage", "sessionStorage", "indexedDB", "innerHTML", "document.write", "serviceWorker"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("browser source contains forbidden API %q", forbidden)
		}
	}
	for _, forbidden := range []string{".style.", ".style =", ".style=", `setAttribute("style"`, `setAttribute('style'`} {
		if strings.Contains(script, forbidden) {
			t.Errorf("browser source creates a CSP-blocked inline style through %q", forbidden)
		}
	}
	for _, required := range []string{
		"navigator.credentials.create", "navigator.credentials.get", "textContent",
		"Upload-Offset", "Content-Range", "webkitRelativePath", "history.replaceState",
		"webkitGetAsEntry", "getAsFileSystemHandle", "readEntries", "transferGroups",
		"maximumRenderedTransferGroups", "maximumRenderedGroupFiles", "summarizeTransferRows",
		"dataset.appearance", `state.themeAssets["brand.mark"]`, `state.themeAssets["brand.favicon"]`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("browser source is missing workflow primitive %q", required)
		}
	}
}

func TestMediaBrowserShellUsesVirtualizedLazyWebPGridAndAccessibleViewer(t *testing.T) {
	t.Parallel()

	shell := string(mustRead("ui/index.html"))
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
	script := string(mustRead("ui/app.js"))
	for _, required := range []string{
		"renderVirtualGrid", "renderVirtualList", "IntersectionObserver", "gridOverscanRows = 3", "listOverscanRows = 8", "directoryLoading", "URL.revokeObjectURL",
		"/api/v1/previews/resolve", "/api/v1/previews/generations", "/api/v1/previews/operations/", "image/webp", "validatedPreviewBlob", "Invalid preview artifact body", "filterLoadedEntries",
		`crypto.subtle.digest("SHA-256"`, "Invalid preview artifact checksum", "await image.decode()", "previewLoaded",
		"viewerPreviewCache", "cachedViewerPreview", "cacheViewerPreview",
		"waitForPreviewOperation", "previewRetryTimers", "Idempotency-Key",
		"uploadMediaType", "image/x-sony-arw", "image/x-adobe-dng",
		"ArrowLeft", "ArrowRight",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("media browser script is missing %q", required)
		}
	}
	if strings.Contains(script, "mediaBrowserEnabled") {
		t.Error("media browsing is incorrectly gated by optional preview configuration")
	}
	stylesheet := string(mustRead("ui/app.css"))
	for _, required := range []string{".media-grid", ".media-grid-spacer", "repeat(auto-fill", ".media-tile", "aspect-ratio: 1", "object-fit: contain"} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("media browser stylesheet is missing %q", required)
		}
	}
}

func TestNewProjectBrandShellAndAssetManifest(t *testing.T) {
	t.Parallel()

	shell := string(mustRead("ui/index.html"))
	for _, required := range []string{
		`href="/assets/ui.css"`, `src="/assets/ui.js"`, `href="/assets/brand/endlessfs-mark.svg"`,
		`class="brand-mark`, `class="app-rail`, `class="heading-actions command-bar"`, `class="file-workspace`,
	} {
		if !strings.Contains(shell, required) {
			t.Errorf("new project shell is missing %q", required)
		}
	}

	stylesheet := string(mustRead("ui/app.css"))
	for _, required := range []string{
		"--efs-color-background", "--efs-color-foreground", "--efs-color-primary", "--efs-color-primary-tint",
		"--efs-color-success", "--efs-color-warning", "--efs-color-error", "@media (prefers-reduced-motion: reduce)",
		`font-family: "Inter"`, `url("/assets/fonts/inter-regular.woff2")`, `url("/assets/fonts/inter-medium.woff2")`, `url("/assets/fonts/inter-semibold.woff2")`,
	} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("new project stylesheet is missing semantic contract %q", required)
		}
	}
	for _, forbidden := range []string{"--efs-color-blue", "--efs-color-red", "--efs-color-green", "--efs-color-yellow", "--efs-color-accent", "--efs-color-danger"} {
		if strings.Contains(stylesheet, forbidden) {
			t.Errorf("new project stylesheet consumes legacy or appearance-named token %q", forbidden)
		}
	}

	for _, asset := range []struct {
		path        string
		contentType string
	}{
		{path: "/assets/ui.css", contentType: "text/css; charset=utf-8"},
		{path: "/assets/ui.js", contentType: "text/javascript; charset=utf-8"},
		{path: "/assets/brand/endlessfs-mark.svg", contentType: "image/svg+xml"},
		{path: "/assets/fonts/inter-regular.woff2", contentType: "font/woff2"},
		{path: "/assets/fonts/inter-medium.woff2", contentType: "font/woff2"},
		{path: "/assets/fonts/inter-semibold.woff2", contentType: "font/woff2"},
	} {
		response := httptest.NewRecorder()
		Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, asset.path, nil))
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != asset.contentType {
			t.Errorf("GET %s = %d %q", asset.path, response.Code, response.Header().Get("Content-Type"))
		}
	}
	for _, obsolete := range []string{"/assets/app.css", "/assets/app.js"} {
		response := httptest.NewRecorder()
		Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, obsolete, nil))
		if response.Code != http.StatusNotFound {
			t.Errorf("obsolete asset GET %s = %d, want 404", obsolete, response.Code)
		}
	}
}

func TestRoutineTrashIsImmediateAndRecoverable(t *testing.T) {
	t.Parallel()

	shell := string(mustRead("ui/index.html"))
	if !strings.Contains(shell, `id="toast-region"`) {
		t.Fatal("new project shell is missing the non-blocking recovery surface")
	}
	script := string(mustRead("ui/app.js"))
	for _, required := range []string{
		"showTrashUndo", `/api/v1/trash/${encodeURIComponent(item.trashID)}/restore`,
		`body: { conflict: "rename" }`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("routine trash flow is missing %q", required)
		}
	}
	if strings.Contains(script, "Move ${paths.length} item${paths.length === 1 ? \"\" : \"s\"} to trash?") {
		t.Error("routine trash still asks for confirmation")
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
