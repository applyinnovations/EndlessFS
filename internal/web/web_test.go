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
			`id="open-transfers"`, `id="transfer-close"`, `id="transfer-progress"`, `id="transfer-filter"`,
			`class="transfer-sheet-content"`,
			`class="tabs transfer-tabs"`, `role="tablist"`, `data-tab-value="current"`,
			`data-tab-value="failed"`, `data-tab-value="complete"`, `data-tab-value="all"`,
		} {
			if !strings.Contains(body, required) {
				t.Errorf("GET %s shell is missing %q", path, required)
			}
		}
		if strings.Contains(body, `<select id="transfer-filter"`) {
			t.Errorf("GET %s transfer filter still uses a select instead of one-click tabs", path)
		}
		if strings.Contains(body, `<button class="transfer-tab"`) {
			t.Errorf("GET %s transfer tabs inherit button presentation", path)
		}
	}
}

func TestBrowserSourceKeepsSecretsEphemeralAndUntrustedTextOutOfHTML(t *testing.T) {
	t.Parallel()

	script := string(applicationScript)
	for _, forbidden := range []string{"localStorage", "sessionStorage", "innerHTML", "document.write", "serviceWorker"} {
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
		"indexedDB.open", "transferLedgerDatabaseName", "restoreTransferLedger", "persistTransferItem",
		"transferVirtualWindowSize", "renderTransferWindow", "retryFailedTransfers",
		"discoverLegacyEntry", "discoverFileSystemHandle",
		"requestPermission", "reconnectStoredTransferSources",
		"automaticTransferConcurrency", "configuredTransferConcurrency", "runTransferControlPool", "aggregateTransferSummary", "recordTransferProgress", "scheduleTransferRender",
		"transferByID", "nextQueuedTransfer", "updateRenderedTransferProgress", "scheduleTransferStructureRender",
		"dataset.appearance", `state.themeAssets["brand.mark"]`, `state.themeAssets["brand.favicon"]`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("browser source is missing workflow primitive %q", required)
		}
	}
	progressStart := strings.Index(script, "function renderTransferProgress()")
	progressEnd := strings.Index(script[progressStart:], "\n  function renderTransfers()")
	if progressStart < 0 || progressEnd < 0 {
		t.Fatal("transfer progress renderer is missing")
	}
	if strings.Contains(script[progressStart:progressStart+progressEnd], "renderTransferWindow()") {
		t.Error("high-frequency progress rendering replaces transfer rows and can discard keyboard focus")
	}
	if strings.Contains(script+string(mustRead("ui/index.html")), "transfer-concurrency") {
		t.Error("transfer concurrency remains exposed as a user-controlled input")
	}
	schedulerStart := strings.Index(script, "function automaticTransferConcurrency()")
	if schedulerStart < 0 {
		t.Fatal("automatic transfer scheduler is missing")
	}
	schedulerEnd := strings.Index(script[schedulerStart:], "\n  function installUploadWorkerPoolTestFixture")
	if schedulerEnd < 0 {
		t.Fatal("automatic transfer scheduler boundary is missing")
	}
	scheduler := script[schedulerStart : schedulerStart+schedulerEnd]
	for _, forbidden := range []string{"navigator.hardwareConcurrency", "connection.downlink", "connection.effectiveType"} {
		if strings.Contains(scheduler, forbidden) {
			t.Errorf("upload concurrency still depends on unreliable device/network guess %q", forbidden)
		}
	}
	for _, required := range []string{"maximumTransferConcurrency", "Math.min(configured, pending)", "queueUploadDirectoryRefresh", "flushUploadDirectoryRefresh"} {
		if !strings.Contains(script, required) {
			t.Errorf("upload concurrency is missing upstream worker-pool primitive %q", required)
		}
	}
	for _, forbidden := range []string{"adaptiveTransferScheduler", "recordSuccessfulTransfer", "recordTransferFailure"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("upload concurrency retains unproven adaptive primitive %q", forbidden)
		}
	}
	if strings.Contains(script, "Promise.all(uploadIDs.map") {
		t.Error("group cancellation can issue one simultaneous provider request per transfer")
	}
	for _, required := range []string{"batchCount: Number.isSafeInteger(transfer.batchCount)", "const completeBatch =", "if (completeBatch) body.batchID = batchID"} {
		if !strings.Contains(script, required) {
			t.Errorf("durable compact upload cancellation is missing %q", required)
		}
	}
	for _, forbidden := range []string{"maximumRenderedTransferGroups", "maximumRenderedGroupFiles", "summarizeTransferRows", "more files"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("transfer monitor retains manual pagination primitive %q", forbidden)
		}
	}
	for _, forbidden := range []string{"capabilityURL", "capabilityHeaders", "csrf", "absolutePath"} {
		if strings.Contains(script, `transferLedgerRecord.${forbidden}`) {
			t.Errorf("durable transfer record persists forbidden field %q", forbidden)
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
