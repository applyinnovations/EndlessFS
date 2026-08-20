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

func TestTransferMonitorUsesScalableSideSheet(t *testing.T) {
	t.Parallel()

	shell := string(mustRead("ui/index.html"))
	stylesheet := string(applicationStylesheet)
	script := string(applicationScript)
	for _, required := range []string{
		`class="transfer-panel transfer-sheet"`, `aria-controls="transfer-panel"`,
		`id="transfer-close" class="icon-button"`, `setTransferSheetOpen`,
		`id="header-transfer-summary" class="header-transfer-summary"`,
		`id="header-transfer-percent"`, `id="header-transfer-speed"`,
		`id="header-transfer-eta"`, `id="header-transfer-count"`,
		`inset: var(--efs-metric-toolbarHeight) 0 0 auto;`,
		`width: clamp(400px, 36vw, 560px);`, `flex: 1; overflow-y: auto;`,
		`.transfer-panel { inset: 0; width: 100%;`,
		`for (const view of byID("authenticated-view").querySelectorAll(".view")) view.inert = modal;`,
	} {
		if !strings.Contains(shell+stylesheet+script, required) {
			t.Errorf("transfer side sheet is missing %q", required)
		}
	}
	for _, required := range []string{
		`<span id="header-transfer-summary" class="header-transfer-summary" aria-hidden="true">`,
		`</span>
            <button id="open-transfers"`,
		`byID("header-transfer-percent").textContent =`,
		`byID("header-transfer-speed").textContent =`,
		`byID("header-transfer-eta").textContent =`,
		`byID("header-transfer-count").textContent =`,
	} {
		if !strings.Contains(shell+script, required) {
			t.Errorf("deterministic transfer header summary is missing %q", required)
		}
	}
	for _, forbidden := range []string{`headerSummary.hidden`, `active / ${summary.totalCount`, `.toLocaleString()} files`} {
		if strings.Contains(script, forbidden) {
			t.Errorf("transfer header summary retains verbose or state-dependent presentation %q", forbidden)
		}
	}
	for _, forbidden := range []string{"transfer-toggle", "transfer-compact-metrics", "transfer-panel.collapsed", `inset: auto 14px 14px auto;`} {
		if strings.Contains(shell+stylesheet+script, forbidden) {
			t.Errorf("transfer monitor retains floating or collapsible presentation %q", forbidden)
		}
	}
	if strings.Index(shell, `id="transfer-panel"`) < strings.Index(shell, `id="admin-view"`) {
		t.Error("transfer side sheet is nested in a route-specific view instead of the authenticated application shell")
	}
	for _, required := range []string{
		`.transfer-row progress::-webkit-progress-value`,
		`.transfer-group-row progress::-webkit-progress-value { background: var(--efs-color-foreground); }`,
		`.transfer-row progress::-moz-progress-bar`,
		`.transfer-group-row progress::-moz-progress-bar { background: var(--efs-color-foreground); }`,
	} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("transfer progress does not use the theme foreground: missing %q", required)
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
		"automaticTransferConcurrency", "aggregateTransferSummary", "recordTransferProgress", "scheduleTransferRender",
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

func TestLocalTransferPreviewFixtureIsExplicitAndScaleOriented(t *testing.T) {
	t.Parallel()

	script := string(applicationScript)
	for _, required := range []string{
		`state.config.localFixture`,
		`new URLSearchParams(location.search).get("fixture") !== "transfers"`,
		`for (let index = 0; index < 2000; index += 1)`,
		`fixture: true`,
		`seedTransferPreviewFixture();`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("local transfer preview fixture is missing %q", required)
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
		`id="preview-generate" class="icon-button" type="button" aria-label="Generate preview" data-icon="eye"`,
		`id="preview-regenerate" class="icon-button" type="button" aria-label="Regenerate preview" data-icon="refresh"`,
		`id="preview-generation-action" class="preview-generation-action"`,
		`id="preview-original" class="icon-button" type="button" aria-label="Show original" data-icon="file"`,
		`id="preview-download" class="icon-button" type="button" aria-label="Download" data-icon="download"`,
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
	script := string(applicationScript)
	for _, required := range []string{
		"renderVirtualGrid", "renderVirtualList", "IntersectionObserver", "gridOverscanRows = 3", "listOverscanRows = 8", "directoryLoading", "URL.revokeObjectURL",
		"/api/v1/previews/resolve", "/api/v1/previews/generations", "/api/v1/previews/operations/", "image/webp", "validatedPreviewBlob", "Invalid preview artifact body", "filterLoadedEntries",
		`crypto.subtle.digest("SHA-256"`, "Invalid preview artifact checksum", "await image.decode()", "previewLoaded",
		"viewerPreviewCache", "cachedViewerPreview", "cacheViewerPreview",
		"syncPreviewGenerationActions", `syncPreviewGenerationActions(canGenerate, result.state === "ready")`,
		`byID("preview-generation-action").hidden = !canGenerate;`,
		"waitForPreviewOperation", "previewRetryTimers", "Idempotency-Key",
		"uploadMediaType", "image/x-sony-arw", "image/x-adobe-dng",
		"showActionErrorToast", `showActionErrorToast("Preview"`, `showActionErrorToast(regenerate ? "Regenerate preview" : "Generate preview"`,
		"showPreviewIssue", `showToast("Generated previews are not configured.", "info")`,
		`unavailable: ["Preview unavailable. Original file unaffected.", "warning"]`,
		`failed: ["Preview generation failed.", "error"]`,
		"ArrowLeft", "ArrowRight",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("media browser script is missing %q", required)
		}
	}
	if strings.Contains(script, "mediaBrowserEnabled") {
		t.Error("media browsing is incorrectly gated by optional preview configuration")
	}
	if strings.Contains(script, "This file type uses its built-in icon") {
		t.Error("icon-only preview fallback includes non-actionable explanatory copy")
	}
	for _, forbidden := range []string{
		`id="preview-generate" class="secondary"`, `id="preview-regenerate" class="secondary"`,
		`id="preview-original" class="secondary"`, `id="preview-download" class="primary"`,
		`id="preview-status"`, `byID("preview-status")`,
		`Opening verified preview…`, `Loading generated preview…`, `Preview ready.`,
		`Original preview loaded.`, `Preparing a safe preview…`,
	} {
		if strings.Contains(shell+script, forbidden) {
			t.Errorf("preview still uses a non-canonical action or inline status: %q", forbidden)
		}
	}
	stylesheet := string(applicationStylesheet)
	for _, required := range []string{".media-grid", ".media-grid-spacer", "repeat(auto-fill", ".media-tile", "aspect-ratio: 1", "object-fit: contain"} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("media browser stylesheet is missing %q", required)
		}
	}
	for _, required := range []string{
		`.preview-actions {`,
		`display: flex;`,
		`gap: 6px;`,
		`.preview-generation-action > button { grid-area: 1 / 1; }`,
	} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("preview action strip is missing compact deterministic geometry %q", required)
		}
	}
	if strings.Contains(stylesheet, `grid-template-areas: "generation original download";`) {
		t.Error("preview action strip retains an empty generation track")
	}
	for _, required := range []string{
		`select {`,
		`appearance: none;`,
		`padding: 4px 32px 4px var(--efs-spacing-controlPadding);`,
		`calc(100% - 20px) 50%`,
		`calc(100% - 16px) 50%`,
	} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("select controls are missing deterministic chevron spacing %q", required)
		}
	}
	for _, required := range []string{`.tooltip-anchor { anchor-name: --efs-action-tooltip-anchor; }`, `position-anchor: --efs-action-tooltip-anchor;`, `position-try-fallbacks: bottom;`} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("top-layer action tooltips are missing CSP-safe anchor positioning %q", required)
		}
	}
	const fullViewportViewer = ".media-viewer { inset: 0; width: 100vw; max-width: none; height: 100vh; height: 100dvh; max-height: none; margin: 0; border: 0; border-radius: 0; padding: 0; box-shadow: none; }"
	if !strings.Contains(stylesheet, fullViewportViewer) {
		t.Error("media viewer does not meet every viewport edge")
	}
	markup := string(mustRead("ui/index.html"))
	for _, required := range []string{
		`<div class="preview-layout">`,
		`<aside class="preview-details" aria-label="File details">`,
		`grid-template-columns: minmax(0, 1fr) clamp(240px, 24vw, 300px)`,
		`grid-template-rows: minmax(0, 1fr) auto`,
		`.metadata-list { grid-template-columns: repeat(2, minmax(0, 1fr))`,
	} {
		if !strings.Contains(markup+stylesheet, required) {
			t.Errorf("responsive preview metadata layout is missing %q", required)
		}
	}
	for _, required := range []string{
		`<th scope="col">Name</th><th scope="col">Size</th><th scope="col">Mime type</th><th scope="col">Changed</th>`,
		`const mediaTypeCell = text("td", entry.kind === "directory" ? "—" : entry.mediaType || "application/octet-stream")`,
		`function trashBrowserEntry(item)`,
		`size: item.size || 0,`,
		`mediaType: item.mediaType || "",`,
	} {
		if !strings.Contains(shell+script+stylesheet, required) {
			t.Errorf("MIME type table presentation is missing %q", required)
		}
	}
	if strings.Contains(stylesheet, ".metadata-list { display: none; }") {
		t.Error("preview metadata is hidden instead of using the compact mobile dock")
	}
}

func TestApplicationSurfacesUseBordersInsteadOfDropShadows(t *testing.T) {
	t.Parallel()

	for lineNumber, line := range strings.Split(string(applicationStylesheet), "\n") {
		if strings.Contains(line, "box-shadow:") && !strings.Contains(line, "inset") && !strings.Contains(line, "none") {
			t.Errorf("stylesheet line %d uses an off-brand drop shadow: %s", lineNumber+1, strings.TrimSpace(line))
		}
	}
}

func TestHorizontalDividersOnlySeparateRepeatedPeerItems(t *testing.T) {
	t.Parallel()

	stylesheet := string(applicationStylesheet)
	for _, forbidden := range []string{
		".drive-controls {\n  display: flex;\n  min-height: 42px;\n  align-items: center;\n  gap: 7px;\n  border-block:",
		".settings-section { border-top:",
		".record-list > li { border-top:",
	} {
		if strings.Contains(stylesheet, forbidden) {
			t.Errorf("singleton or container is bounded by a horizontal divider: %q", forbidden)
		}
	}
	if !strings.Contains(stylesheet, ".record-list > li + li { border-top: 1px solid var(--efs-color-border);") {
		t.Error("repeated record rows do not retain a divider between peer items")
	}
}

func TestWorkspaceNavigationSharesTheHeaderLineWithTheBrand(t *testing.T) {
	t.Parallel()

	shell := string(mustRead("ui/index.html"))
	headerStart := strings.Index(shell, `<header class="app-header">`)
	headerEnd := strings.Index(shell, `</header>`)
	if headerStart < 0 || headerEnd <= headerStart {
		t.Fatal("application header is missing")
	}
	header := shell[headerStart:headerEnd]
	if !strings.Contains(header, `id="workspace-tabs" class="tabs app-tabs" aria-label="Workspace"`) {
		t.Error("workspace tabs do not share the header line with the EndlessFS brand")
	}
	if !strings.Contains(header, `<a class="tab" href="/" data-route="drive">Files</a>`) {
		t.Error("workspace navigation does not use the canonical tab component")
	}
	if !strings.Contains(header, `<button id="logout-button" class="tab header-signout" type="button">Sign out</button>`) {
		t.Error("header sign-out action does not use the requested text tab presentation")
	}
	for _, forbidden := range []string{`class="brand-name"`, `id="account-name"`, `id="logout-button" class="icon-button"`} {
		if strings.Contains(header, forbidden) {
			t.Errorf("header retains removed identity presentation %q", forbidden)
		}
	}
	authenticatedStart := strings.Index(shell, `<div id="authenticated-view"`)
	if authenticatedStart < 0 {
		t.Fatal("authenticated workspace is missing")
	}
	if strings.Contains(shell[authenticatedStart:], `<nav class="app-tabs"`) {
		t.Error("authenticated content retains a separate navigation row")
	}
	for _, forbidden := range []string{`class="app-rail`, `class="app-tabs sidebar"`, `class="app-tabs loading-tabs"`} {
		if strings.Contains(shell, forbidden) {
			t.Errorf("workspace retains sidebar markup %q", forbidden)
		}
	}

	stylesheet := string(applicationStylesheet)
	for _, required := range []string{
		`grid-template-columns: auto minmax(0, 1fr) auto;`,
		`.app-tabs {`, `.app-tabs .tab {`, `.tab[aria-current="page"]`,
		`font-weight: var(--efs-type-weight-regular);`, `cursor: default;`,
		`.transfer-tabs { display: grid;`, `color: var(--efs-color-foreground);`,
	} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("top workspace tab layout is missing %q", required)
		}
	}
	for _, forbidden := range []string{"--efs-metric-sidebarWidth", "--efs-metric-tabsHeight", ".app-rail", "grid-template-rows: var(--efs-metric-tabsHeight)"} {
		if strings.Contains(stylesheet, forbidden) {
			t.Errorf("workspace retains sidebar styling %q", forbidden)
		}
	}
	for _, forbidden := range []string{`.app-tabs a[aria-current="page"]`, `.app-tabs a:hover { background:`, `text-decoration-line: underline`, `text-decoration-color: var(--efs-color-primary)`, `.tabs {
  display: flex;
  border-block-end:`, `.tab::after`} {
		if strings.Contains(stylesheet, forbidden) {
			t.Errorf("workspace tabs retain the legacy button-like presentation %q", forbidden)
		}
	}
}

func TestDangerActionsUseTheFilledSemanticComponent(t *testing.T) {
	t.Parallel()

	for name, source := range map[string][]byte{
		"ui/index.html":          mustRead("ui/index.html"),
		"application JavaScript": applicationScript,
		"application CSS":        applicationStylesheet,
	} {
		if strings.Contains(string(source), "danger-quiet") {
			t.Errorf("%s retains an unfilled danger variant", name)
		}
	}
	stylesheet := string(applicationStylesheet)
	if !strings.Contains(stylesheet, ".danger { border-color: var(--efs-color-error); background: var(--efs-color-error);") {
		t.Error("danger component is not filled with its semantic background")
	}
}

func TestBrowserSourcesAreSplitIntoOrderedDomains(t *testing.T) {
	t.Parallel()

	scriptMarkers := map[string]string{
		"ui/js/core.js":          "const state = {",
		"ui/js/files.js":         "async function loadDirectory",
		"ui/js/transfers.js":     "function transferFileSize",
		"ui/js/previews.js":      "async function download",
		"ui/js/operations.js":    "async function copyMove",
		"ui/js/account-admin.js": "async function createShare",
		"ui/js/bootstrap.js":     "function ask",
	}
	stylesheetMarkers := map[string]string{
		"ui/css/foundation.css":     ":root {",
		"ui/css/shell.css":          ".app-header {",
		"ui/css/files.css":          ".surface-header {",
		"ui/css/transfers.css":      ".transfer-panel {",
		"ui/css/settings-admin.css": ".settings-list {",
		"ui/css/overlays.css":       "dialog {",
		"ui/css/responsive.css":     "@media (max-width: 900px)",
	}
	for _, sources := range []struct {
		names   []string
		markers map[string]string
	}{
		{names: applicationScriptSources, markers: scriptMarkers},
		{names: applicationStylesheetSources, markers: stylesheetMarkers},
	} {
		for _, name := range sources.names {
			source := string(mustRead(name))
			if !strings.Contains(source, sources.markers[name]) {
				t.Errorf("domain source %s is missing boundary marker %q", name, sources.markers[name])
			}
		}
	}
}

func TestTrashActionsUseCompactAccessibleSymbols(t *testing.T) {
	t.Parallel()

	shell := string(mustRead("ui/index.html"))
	for _, required := range []string{`id="empty-trash" class="danger icon-button"`, `aria-label="Empty trash"`, `data-icon="trash-x"`} {
		if !strings.Contains(shell, required) {
			t.Errorf("empty-trash symbol control is missing %q", required)
		}
	}
	script := string(applicationScript)
	for _, required := range []string{`iconButton("restore", "Restore"`, `iconButton("trash-x", "Delete Permanently"`, `node.setAttribute("aria-label", label)`, `node.dataset.tooltip = label`} {
		if !strings.Contains(script, required) {
			t.Errorf("trash row symbol controls are missing %q", required)
		}
	}
	if strings.Contains(script, `button("Delete Permanently"`) {
		t.Error("trash row still renders the permanent-delete label as visible button text")
	}
}

func TestFileRowsExposeDirectIconActionsWithoutAnActionMenu(t *testing.T) {
	t.Parallel()

	script := string(applicationScript)
	for _, required := range []string{
		`actions.className = "row-actions file-row-actions"`,
		"iconButton(\"download\", `Download ${entry.name}`",
		"iconButton(\"share-3\", `Create public share for ${entry.name}`",
		"iconButton(\"copy\", `Copy ${entry.name}`",
		"iconButton(\"folder-symlink\", `Move ${entry.name}`",
		"iconButton(\"trash\", `Move ${entry.name} to trash`",
		`const iconButton = (name, label, action, className = "", tooltip = label)`,
		`node.dataset.tooltip = tooltip;`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("direct file-row action is missing %q", required)
		}
	}
	for _, forbidden := range []string{"openItemActions", `button("More"`, `id: "item-action"`} {
		if strings.Contains(script, forbidden) {
			t.Errorf("file row retains the action-menu detour %q", forbidden)
		}
	}
	if !strings.Contains(script, `"file-row-action danger"`) {
		t.Error("trash icon does not retain a distinct destructive semantic treatment")
	}
}

func TestSelectionActionsFloatWithoutReplacingDriveControls(t *testing.T) {
	t.Parallel()

	shell := string(mustRead("ui/index.html"))
	for _, required := range []string{
		`id="selection-bar" class="selection-bar owner-only" role="toolbar" aria-label="Selection actions" hidden`,
		`id="download-selected" class="icon-button" type="button" aria-label="Download selected" data-icon="download"`,
		`id="share-selected" class="icon-button" type="button" aria-label="Share selected" data-icon="share-3"`,
		`id="copy-selected" class="icon-button" type="button" aria-label="Copy selected" data-icon="copy"`,
		`id="move-selected" class="icon-button" type="button" aria-label="Move selected" data-icon="folder-symlink"`,
		`id="trash-selected" class="danger icon-button" type="button" aria-label="Move selected to trash" data-icon="trash"`,
		`id="clear-selection" class="icon-button" type="button" aria-label="Clear selection" data-icon="x"`,
	} {
		if !strings.Contains(shell, required) {
			t.Errorf("floating selection control is missing %q", required)
		}
	}
	for _, forbidden := range []string{`id="download-selected" type="button">Download`, `id="share-selected" type="button">Share`, `id="trash-selected" class="danger" type="button">Trash`} {
		if strings.Contains(shell, forbidden) {
			t.Errorf("selection action retains a visible text button %q", forbidden)
		}
	}

	stylesheet := string(applicationStylesheet)
	for _, required := range []string{
		`.drive-control-stack { display: block; }`,
		`position: fixed;`,
		`transform: translateX(-50%);`,
		`.selection-bar .icon-button { flex: 0 0 var(--efs-metric-controlHeight); }`,
		`#app[data-selection-active="true"] .toast-region`,
	} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("selection action surface is missing %q", required)
		}
	}
	if strings.Contains(stylesheet, `.drive-control-stack.selection-active > .drive-controls`) {
		t.Error("selection still hides the persistent drive controls")
	}

	script := string(applicationScript)
	if strings.Contains(script, `classList.toggle("selection-active"`) {
		t.Error("selection still swaps the drive control surface")
	}
}

func TestTimestampsUseTheCanonicalCompactCalendarFormat(t *testing.T) {
	t.Parallel()

	script := string(applicationScript)
	for _, required := range []string{
		`sameCalendarDate(date, today) ? "Today"`,
		`sameCalendarDate(date, yesterday) ? "Yesterday"`,
		`const period = date.getHours() < 12 ? "am" : "pm"`,
		"return `${dayLabel} at ${hour}:${minute} ${period}`",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("canonical timestamp formatting is missing %q", required)
		}
	}
	if strings.Contains(script, `new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" })`) {
		t.Error("timestamps still depend on the browser's variable locale format")
	}
}

func TestAdminUsersUseDeterministicTableColumns(t *testing.T) {
	t.Parallel()

	shell := string(mustRead("ui/index.html"))
	for _, required := range []string{`id="users-table"`, `<tbody id="user-list">`, `class="settings-section settings-wide admin-users-section"`} {
		if !strings.Contains(shell, required) {
			t.Errorf("admin user table is missing %q", required)
		}
	}
	stylesheet := string(applicationStylesheet)
	for _, required := range []string{
		".admin-users-section { border-top: 0;",
		".user-actions { display: grid;",
		"grid-template-columns: repeat(3, var(--efs-metric-controlHeight));",
		"@media (max-width: 900px)",
		"#users-table { min-width: 0; }",
		"#users-table th:nth-child(4)",
		"#users-table td:nth-child(4) { display: none; }",
		"@media (max-width: 560px)",
		"#users-table th:nth-child(3)",
		"#users-table td:nth-child(3) { display: none; }",
		".user-actions { gap: 2px; }",
	} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("admin user layout is missing %q", required)
		}
	}
	script := string(applicationScript)
	for _, required := range []string{
		`iconButton(user.status === "enabled" ? "user-off" : "user-check"`,
		`iconButton(user.admin ? "shield-minus" : "shield-plus"`,
		`iconButton("key", `,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("admin user icon action is missing %q", required)
		}
	}
	for _, forbidden := range []string{`button(user.status === "enabled" ? "Disable" : "Enable"`, `button(user.admin ? "Remove admin" : "Make admin"`, `button("Recovery link"`} {
		if strings.Contains(script, forbidden) {
			t.Errorf("admin user action still exposes visible text: %q", forbidden)
		}
	}
}

func TestIconControlsUsePinnedTablerGeometryAndDeterministicTooltips(t *testing.T) {
	t.Parallel()

	shell := string(mustRead("ui/index.html"))
	stylesheet := string(applicationStylesheet)
	for _, required := range []string{`aria-label="Sign out" data-icon="logout"`, `aria-label="Upload folder" data-icon="folder-up"`, `id="action-tooltip" class="action-tooltip" role="tooltip" hidden`} {
		if !strings.Contains(shell, required) {
			t.Errorf("icon tooltip shell is missing %q", required)
		}
	}
	for _, control := range []string{`aria-label="Create folder" data-icon="folder-plus"`, `aria-label="Upload files" data-icon="upload"`} {
		if count := strings.Count(shell, control); count != 2 {
			t.Errorf("loaded and fixed-geometry loading headers must both use %q; got %d occurrences", control, count)
		}
	}
	for _, forbidden := range []string{`>New folder</button>`, `>Upload</button>`, `for="upload-input">Upload</label>`} {
		if strings.Contains(shell, forbidden) {
			t.Errorf("file header retains a text action %q", forbidden)
		}
	}
	for _, forbidden := range []string{`#new-folder-button::before`, `content: "+"`} {
		if strings.Contains(stylesheet, forbidden) {
			t.Errorf("mobile file header replaces the canonical folder icon through %q", forbidden)
		}
	}
	for _, tag := range strings.Split(shell, "<") {
		if strings.Contains(tag, `data-icon="`) && !strings.Contains(tag, `aria-label="`) {
			t.Errorf("static icon control is missing a tooltip source and accessible label: <%s", strings.SplitN(tag, ">", 2)[0])
		}
	}
	script := string(applicationScript)
	for _, required := range []string{
		"const iconPaths = Object.freeze({",
		`"folder-plus":`,
		`upload:`,
		`svg.setAttribute("class", "app-icon")`,
		`node.dataset.tooltip = label`,
		`document.querySelectorAll("[data-icon]").forEach`,
		`function activeActionTooltip()`,
		`document.querySelectorAll("dialog[open]")`,
		`const tooltip = activeActionTooltip();`,
		`function showActionTooltip(target)`,
		`document.addEventListener("pointerover"`,
		`document.addEventListener("pointerdown", () => hideActionTooltip())`,
		`window.addEventListener("scroll", () => hideActionTooltip(), true)`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("deterministic icon tooltip behavior is missing %q", required)
		}
	}
	stylesheet = string(applicationStylesheet)
	for _, required := range []string{
		".icon-button {",
		"border: 0;",
		"background: transparent;",
		".icon-button:hover { background: transparent; color: var(--efs-color-foreground); }",
		".icon-button.danger:hover { background: transparent; color: var(--efs-color-error); }",
		".app-icon {",
		"width: 16px;",
		"stroke-width: 1.8;",
		".action-tooltip {",
		"position: fixed;",
		"pointer-events: none;",
		"table button::after { content: none; }",
	} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("compact icon or tooltip styling is missing %q", required)
		}
	}
	if strings.Contains(stylesheet, "[data-tooltip]::before") {
		t.Error("tooltip pseudo-elements can expand scrollable table geometry and intercept row actions")
	}
}

func TestTrailingTableIconActionsDoNotRepeatEndPadding(t *testing.T) {
	t.Parallel()

	stylesheet := string(applicationStylesheet)
	if !strings.Contains(stylesheet, `.table-icon-action-cell { padding-inline-end: 0; }`) {
		t.Error("trailing transparent icon controls still repeat the table cell end padding")
	}

	script := string(applicationScript)
	for _, required := range []string{
		`if (columnIndex === columns - 1) cell.className = "table-icon-action-cell";`,
		`actionCell.className = "table-icon-action-cell";`,
		`actions.className = "table-icon-action-cell";`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("table action padding audit is missing %q", required)
		}
	}
	if count := strings.Count(script, `className = "table-icon-action-cell"`); count < 5 {
		t.Errorf("table action padding audit covers %d render paths, want at least 5", count)
	}
}

func TestDirectorySizesRemainBlankWithoutRecursiveAggregate(t *testing.T) {
	t.Parallel()

	script := string(applicationScript)
	for _, required := range []string{
		`text("span", entry.kind === "directory" ? "" : formatBytes(entry.size), "media-tile-meta")`,
		`const sizeCell = text("td", entry.kind === "directory" ? "" : formatBytes(entry.size));`,
		`text("td", entry.kind === "directory" ? "" : formatBytes(entry.size))`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("directory size presentation is missing %q", required)
		}
	}
	if strings.Contains(script, `entry.kind === "directory" ? "Folder" : formatBytes(entry.size)`) {
		t.Error("directory rows still use a type label in the size field")
	}
}

func TestSettingsAndAdminActionsUseCanonicalTransparentIcons(t *testing.T) {
	t.Parallel()

	shell := string(mustRead("ui/index.html"))
	for _, required := range []string{
		`aria-label="Save account" data-icon="device-floppy"`,
		`aria-label="Apply theme" data-icon="check"`,
		`id="add-passkey" class="icon-button" type="button" aria-label="Add passkey" data-icon="key-plus"`,
		`id="refresh-shares" class="icon-button" type="button" aria-label="Refresh shares" data-icon="refresh"`,
		`id="settings-logout" class="danger icon-button" type="button" aria-label="Sign out" data-icon="logout"`,
		`id="create-invite" class="icon-button" type="button" aria-label="Create invite" data-icon="user-plus"`,
		`id="copy-invite" class="icon-button" type="button" aria-label="Copy invite link" data-icon="copy"`,
		`id="refresh-users" class="icon-button" type="button" aria-label="Refresh users" data-icon="refresh"`,
		`id="users-next" class="icon-button more-button" type="button" aria-label="Load more users" data-icon="chevron-down"`,
		`id="dialog-cancel" class="icon-button"`,
		`id="dialog-confirm" class="icon-button"`,
	} {
		if !strings.Contains(shell, required) {
			t.Errorf("Settings or Admin icon action is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		`type="submit">Save</button>`, `type="submit">Apply</button>`, `id="add-passkey" class="secondary"`,
		`id="refresh-shares" class="secondary"`, `id="settings-logout" class="danger" type="button">Sign out`,
		`id="create-invite" class="primary"`, `id="copy-invite" type="button">Copy`,
		`id="refresh-users" class="secondary"`, `id="users-next" class="secondary`,
	} {
		if strings.Contains(shell, forbidden) {
			t.Errorf("Settings or Admin retains a visible text button: %q", forbidden)
		}
	}

	script := string(applicationScript)
	for _, required := range []string{
		`iconButton("key-off", `, `"Remove passkey"`,
		`iconButton("link-off", `, `"Revoke share"`, `"Revoke invite"`,
		`setIconControl(confirmControl, options.confirmIcon || "check", confirmLabel)`,
		`iconButton("copy", "Copy link"`,
		`.icon-button.more-button { min-width: var(--efs-metric-controlHeight); }`,
	} {
		if !strings.Contains(script+string(applicationStylesheet), required) {
			t.Errorf("dynamic Settings or Admin icon action is missing %q", required)
		}
	}
	for _, forbidden := range []string{`button("Remove"`, `button("Revoke"`, `main.append(button("Revoke"`, `fields.append(button("Copy link"`} {
		if strings.Contains(script, forbidden) {
			t.Errorf("dynamic Settings or Admin action retains visible text: %q", forbidden)
		}
	}
}

func TestDestructiveConfirmationActionsUseExplicitText(t *testing.T) {
	t.Parallel()

	script := string(applicationScript)
	for _, required := range []string{
		`confirmControl.className = "danger"`,
		`confirmControl.replaceChildren(document.createTextNode(confirmLabel))`,
		`delete confirmControl.dataset.icon`,
		`confirm: "Delete Permanently", danger: true`,
		`confirm: "Empty Trash Permanently", danger: true`,
		`confirm: "Revoke Share", danger: true`,
		`confirm: "Remove Passkey", danger: true`,
		`const title = action === "disable" ? `,
		"`Disable ${user.displayName}?`",
		`removingAdministrator ? "Remove Administrator"`,
		`confirm: "Revoke Invite", danger: true`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("destructive confirmation is missing explicit text treatment %q", required)
		}
	}
	for _, forbidden := range []string{
		`options.danger ? "danger icon-button"`,
		`options.danger ? "trash-x"`,
		`confirm: "Confirm change"`,
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("destructive confirmation retains ambiguous icon or casing behavior %q", forbidden)
		}
	}
}

func TestAccountAndThemeUseTheCanonicalSettingsRow(t *testing.T) {
	t.Parallel()

	shell := string(mustRead("ui/index.html"))
	for _, formID := range []string{"profile-form", "theme-form"} {
		formStart := strings.Index(shell, `id="`+formID+`"`)
		if formStart < 0 {
			t.Fatalf("settings form %q is missing", formID)
		}
		formEnd := strings.Index(shell[formStart:], "</form>")
		if formEnd < 0 {
			t.Fatalf("settings form %q is incomplete", formID)
		}
		form := shell[formStart : formStart+formEnd]
		headingStart := strings.Index(form, `class="panel-heading"`)
		bodyStart := strings.Index(form, `class="settings-form-body"`)
		submitStart := strings.Index(form, `type="submit"`)
		if headingStart < 0 || submitStart < headingStart || bodyStart < submitStart {
			t.Errorf("settings form %q does not use the canonical heading-and-content section layout", formID)
		}
	}
	stylesheet := string(applicationStylesheet)
	for _, required := range []string{
		".settings-form { display: block; }",
		"margin-block-start: 8px;",
	} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("canonical settings section is missing %q", required)
		}
	}
	for _, forbidden := range []string{"grid-template-columns: 120px minmax(0, 1fr) 88px;", "settings-actions"} {
		if strings.Contains(shell, forbidden) || strings.Contains(stylesheet, forbidden) {
			t.Errorf("obsolete side-by-side settings layout remains: %q", forbidden)
		}
	}
}

func TestSettingsAndAdminHeadingActionsShareCenterline(t *testing.T) {
	t.Parallel()

	shell := string(mustRead("ui/index.html"))
	settingsStart := strings.Index(shell, `id="settings-view"`)
	adminStart := strings.Index(shell, `id="admin-view"`)
	if settingsStart < 0 || adminStart < settingsStart {
		t.Fatal("settings or admin view is missing")
	}
	settingsView := shell[settingsStart:adminStart]
	adminView := shell[adminStart:]
	if count := strings.Count(settingsView, `class="panel-heading"`); count != 5 {
		t.Errorf("settings view does not expose five canonical action headings; got %d", count)
	}
	if count := strings.Count(adminView, `class="panel-heading"`); count != 2 {
		t.Errorf("admin view does not expose two canonical action headings; got %d", count)
	}

	stylesheet := string(applicationStylesheet)
	for _, required := range []string{
		`.settings-section > .panel-heading {`,
		`min-height: var(--efs-metric-controlHeight);`,
		`align-items: center;`,
		`.settings-section > .panel-heading h2 { margin-block: 0; }`,
	} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("settings/admin action headings are not center-aligned: missing %q", required)
		}
	}
}

func TestHeaderActionsAlignWithThePageContentEdge(t *testing.T) {
	t.Parallel()

	stylesheet := string(applicationStylesheet)
	for _, required := range []string{
		`padding-inline: var(--efs-spacing-pageGutter);`,
		`.app-header { padding-inline: 8px; }`,
		`.view { padding-inline: 8px; }`,
	} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("header actions do not share the page content edge: missing %q", required)
		}
	}
	for _, forbidden := range []string{`padding-inline: 14px 0;`, `.app-header { padding-inline: 8px 0; }`} {
		if strings.Contains(stylesheet, forbidden) {
			t.Errorf("header retains asymmetric edge spacing: found %q", forbidden)
		}
	}
}

func TestNewProjectBrandShellAndAssetManifest(t *testing.T) {
	t.Parallel()

	shell := string(mustRead("ui/index.html"))
	for _, required := range []string{
		`href="/assets/ui.css"`, `src="/assets/ui.js"`, `href="/assets/brand/endlessfs-mark.svg"`,
		`class="brand-mark`, `class="tabs app-tabs`, `class="heading-actions command-bar"`, `class="file-workspace`,
	} {
		if !strings.Contains(shell, required) {
			t.Errorf("new project shell is missing %q", required)
		}
	}

	stylesheet := string(applicationStylesheet)
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

func TestBrandMarkUsesApprovedInfiniteFolderGeometry(t *testing.T) {
	t.Parallel()

	mark := string(mustRead("ui/brand/endlessfs-mark.svg"))
	for _, required := range []string{
		`viewBox="0 0 160 120"`,
		`M94 34C111 26 130 25 139 33C142 36 144 42 144 50V94C144 102 138 108 130 108H30C22 108 16 102 16 94V44`,
		`M16 44V28C16 19 23 12 32 12H58C69 12 78 16 86 23L107 40C118 49 130 52 144 52`,
		`M16 44C29 55 47 58 64 52L82 44`,
	} {
		if !strings.Contains(mark, required) {
			t.Errorf("brand mark does not match approved geometry: missing %q", required)
		}
	}
	if strings.Count(mark, "<path") != 3 {
		t.Fatalf("brand mark path count = %d, want three clean continuous paths", strings.Count(mark, "<path"))
	}
}

func TestOriginalFileReusesViewerWithoutCollapsingHeader(t *testing.T) {
	t.Parallel()

	script := string(applicationScript)
	for _, required := range []string{
		`function syncViewerNavigation(standalone = false)`,
		`byID("preview-previous").hidden = false;`,
		`byID("preview-next").hidden = false;`,
		`syncViewerNavigation(Boolean(publicToken) || state.viewerIndex < 0);`,
		`byID("preview-original").disabled = true;`,
		`byID("preview-original").disabled = false;`,
		`state.viewerController.abort();`,
		`showActionErrorToast("Show original"`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("original-file viewer mode is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		`byID("preview-previous").hidden = true;`,
		`byID("preview-next").hidden = true;`,
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("original-file mode still collapses the viewer header with %q", forbidden)
		}
	}

	stylesheet := string(applicationStylesheet)
	for _, required := range []string{
		`grid-template-columns: var(--efs-metric-controlHeight) minmax(0, 1fr) var(--efs-metric-controlHeight) var(--efs-metric-controlHeight);`,
		`#preview-previous { grid-column: 1; }`,
		`#preview-title { grid-column: 2; }`,
		`#preview-next { grid-column: 3; }`,
		`#preview-close { grid-column: 4; }`,
	} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("viewer header does not retain deterministic tracks: missing %q", required)
		}
	}
}

func TestRoutineTrashIsImmediateAndRecoverable(t *testing.T) {
	t.Parallel()

	shell := string(mustRead("ui/index.html"))
	if !strings.Contains(shell, `id="toast-region"`) {
		t.Fatal("new project shell is missing the non-blocking recovery surface")
	}
	script := string(applicationScript)
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

func TestToastsUseNeutralSurfacesWithSemanticAccents(t *testing.T) {
	t.Parallel()

	stylesheet := string(applicationStylesheet)
	for _, required := range []string{
		`background: var(--efs-color-surface);`,
		`color: var(--efs-color-foreground);`,
		`border-inline-start: 3px solid var(--efs-toast-accent);`,
		`.toast.info { --efs-toast-accent: var(--efs-color-primary); }`,
		`.toast.success { --efs-toast-accent: var(--efs-color-success); }`,
		`.toast.warning { --efs-toast-accent: var(--efs-color-warning); }`,
		`.toast.error { --efs-toast-accent: var(--efs-color-error); }`,
	} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("toast presentation is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		`.toast.error { background: var(--efs-color-error);`,
		`background: var(--efs-color-foreground);\n  color: var(--efs-color-background);\n  pointer-events: auto;`,
	} {
		if strings.Contains(stylesheet, forbidden) {
			t.Errorf("toast still uses a full status fill: found %q", forbidden)
		}
	}

	script := string(applicationScript)
	for _, required := range []string{
		`toast.className = "toast success";`,
		`function showToast(message, level, duration = 8000)`,
		"toast.className = `toast ${level}`;",
		`showToast(message, "error", 12000);`,
		`function activeToastRegion()`,
		`document.querySelectorAll("dialog[open]")`,
		`activeToastRegion().append(toast);`,
		`function showTopLayerDialog(dialog)`,
		`function closeTopLayerDialog(dialog)`,
		`if (byID("toast-region").childElementCount) activeToastRegion();`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("toast status is not explicit: missing %q", required)
		}
	}
	if count := strings.Count(script, `.showModal()`); count != 1 {
		t.Errorf("dialog opening bypasses the toast-aware top-layer helper; got %d direct calls", count)
	}
}

func TestAllActionsUseResponsiveSheetInsteadOfCenteredModal(t *testing.T) {
	t.Parallel()

	shell := string(mustRead("ui/index.html"))
	if !strings.Contains(shell, `id="action-dialog" class="action-sheet"`) {
		t.Fatal("shared action dialog is not unconditionally rendered as a responsive sheet")
	}
	for _, required := range []string{`<div class="sheet-heading"><h2 id="dialog-title">`, `id="dialog-cancel" class="icon-button" value="cancel" type="button" aria-label="Close" data-icon="x"`} {
		if !strings.Contains(shell, required) {
			t.Errorf("responsive action sheet header is missing %q", required)
		}
	}

	script := string(applicationScript)
	if strings.Contains(script, `dialog.classList.toggle("action-sheet"`) {
		t.Error("shared action dialog still conditionally falls back to a centered modal")
	}

	stylesheet := string(applicationStylesheet)
	for _, required := range []string{
		`.action-sheet {`,
		`inset: 0 0 0 auto;`,
		`height: 100dvh;`,
		`.action-sheet #dialog-form {`,
		`.action-sheet .sheet-heading {`,
		`.action-sheet .dialog-actions {`,
		`animation: action-sheet-enter var(--efs-motion-duration-normal) var(--efs-motion-easing);`,
		`.action-sheet { inset: 0; width: 100%;`,
	} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("responsive action sheet is missing %q", required)
		}
	}
}

func TestActionSheetTextWrapsWithoutHorizontalOverflow(t *testing.T) {
	t.Parallel()

	stylesheet := string(applicationStylesheet)
	for _, required := range []string{
		`.action-sheet .sheet-heading {`,
		`grid-template-columns: minmax(0, 1fr) var(--efs-metric-controlHeight);`,
		`.action-sheet #dialog-title {`,
		`overflow-wrap: anywhere;`,
		`white-space: normal;`,
		`.action-sheet #dialog-description {`,
		`.action-sheet #dialog-fields { min-width: 0; max-width: 100%;`,
		`overflow-x: hidden;`,
		`overflow-y: auto;`,
		`.action-sheet #dialog-fields > * { min-width: 0; max-width: 100%; }`,
		`.action-sheet #dialog-output { display: block; max-width: 100%; overflow-wrap: anywhere; white-space: normal; }`,
	} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("action-sheet text can still clip: missing %q", required)
		}
	}
	if strings.Contains(stylesheet, `.action-sheet #dialog-title { min-width: 0; margin: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }`) {
		t.Error("action-sheet title is still constrained to one clipped line")
	}
}

func TestLoadingWorkspaceOwnsTheLoadedGeometry(t *testing.T) {
	t.Parallel()

	shell := string(mustRead("ui/index.html"))
	for _, required := range []string{
		`id="loading-view" class="app-frame loading-shell"`,
		`class="app-content" aria-hidden="true"`,
		`class="view loading-content"`,
		`class="loading-table table-scroll"`,
		`<th scope="col">Name</th><th scope="col">Size</th><th scope="col">Mime type</th><th scope="col">Changed</th>`,
		`<tr class="loading-item-row loading-file-row">`,
	} {
		if !strings.Contains(shell, required) {
			t.Errorf("loading workspace is missing fixed loaded geometry %q", required)
		}
	}

	stylesheet := string(applicationStylesheet)
	for _, required := range []string{
		`.loading-shell { min-height: calc(100vh - var(--efs-metric-toolbarHeight));`,
		`.loading-shell { position: fixed;`,
		`.loading-content { min-height: calc(100vh - var(--efs-metric-toolbarHeight));`,
		`.loading-table { height: calc(100vh - 224px);`,
	} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("loading workspace geometry is missing %q", required)
		}
	}

	script := string(applicationScript)
	for _, required := range []string{
		`async function enterApplication()`,
		`const routeLoad = setRoute(routeFromPath(), false);`,
		`await Promise.all([routeLoad, themeLoad]);`,
		`byID("loading-view").hidden = true;`,
		`await enterApplication();`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("loading shell is dismissed before loaded geometry is ready: missing %q", required)
		}
	}
}

func TestDriveBreadcrumbsSitBetweenControlsAndFileContent(t *testing.T) {
	t.Parallel()

	shell := string(mustRead("ui/index.html"))
	controlsIndex := strings.Index(shell, `id="drive-control-stack"`)
	breadcrumbIndex := strings.Index(shell, `id="breadcrumbs" class="breadcrumbs drive-breadcrumbs"`)
	workspaceIndex := strings.Index(shell, `id="drop-target"`)
	if controlsIndex < 0 || breadcrumbIndex < controlsIndex || workspaceIndex < breadcrumbIndex {
		t.Error("drive breadcrumb is not positioned between controls and file content")
	}
	if !strings.Contains(shell, `<nav class="breadcrumbs drive-breadcrumbs" aria-label="Current folder">Files</nav>`) {
		t.Error("fixed loading geometry does not reserve the drive breadcrumb row")
	}
	for _, forbidden := range []string{
		`<div class="location-heading"><h1 id="loading-title">Files</h1><nav`,
		`<div class="location-heading"><h1 id="browser-title" tabindex="-1">Files</h1><nav`,
	} {
		if strings.Contains(shell, forbidden) {
			t.Errorf("drive breadcrumb remains beside the page title: %q", forbidden)
		}
	}

	stylesheet := string(applicationStylesheet)
	for _, required := range []string{
		`.drive-breadcrumbs { min-height: 30px; padding-block: 1px; }`,
		`.loading-table { height: calc(100vh - 224px);`,
		`#list-presentation { height: calc(100vh - 224px);`,
		`.media-grid { position: relative; height: calc(100vh - 224px);`,
	} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("drive breadcrumb geometry is missing %q", required)
		}
	}
}

func TestFileStateOccupiesContentWithoutMovingTableHeader(t *testing.T) {
	t.Parallel()

	shell := string(mustRead("ui/index.html"))
	workspaceIndex := strings.Index(shell, `id="drop-target" class="file-workspace drop-target" data-presentation="list"`)
	tableIndex := strings.Index(shell, `id="list-presentation"`)
	stateIndex := strings.Index(shell, `id="drive-state"`)
	if workspaceIndex < 0 {
		t.Error("file workspace does not declare its initial presentation")
	}
	if tableIndex < 0 || stateIndex < 0 || stateIndex < tableIndex {
		t.Error("file state is not placed after the persistent table structure")
	}

	stylesheet := string(applicationStylesheet)
	for _, required := range []string{
		`#drive-state {`,
		`position: absolute;`,
		`inset: var(--efs-metric-rowHeight) 0 auto;`,
		`background: var(--efs-color-background);`,
		`#drop-target[data-presentation="grid"] #drive-state { inset-block-start: 0; }`,
	} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("file state can still move persistent file geometry: missing %q", required)
		}
	}

	script := string(applicationScript)
	for _, required := range []string{
		`function syncFilePresentation()`,
		`byID("drop-target").dataset.presentation = gridEnabled ? "grid" : "list";`,
		`const gridEnabled = syncFilePresentation();`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("file state is not synchronized with the active content presentation: missing %q", required)
		}
	}
}

func TestTablesUseOneCanonicalDivider(t *testing.T) {
	t.Parallel()

	stylesheet := string(applicationStylesheet)
	for _, required := range []string{
		`--efs-border-tableDivider: 1px solid var(--efs-color-border);`,
		`table { width: 100%; border-collapse: separate; border-spacing: 0; table-layout: fixed; }`,
		`border-bottom: var(--efs-border-tableDivider);`,
		`.state-panel { min-height: 42px; border-bottom: var(--efs-border-tableDivider);`,
	} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("table divider treatment is not canonical: missing %q", required)
		}
	}
	for _, forbidden := range []string{
		`border-collapse: collapse;`,
		`.settings-table-scroll tbody tr:last-child td { border-bottom: 0; }`,
	} {
		if strings.Contains(stylesheet, forbidden) {
			t.Errorf("table divider can render with inconsistent weight: %q", forbidden)
		}
	}
}

func TestOwnerPublicAndTrashReuseOneFileBrowserSurface(t *testing.T) {
	t.Parallel()

	shell := string(mustRead("ui/index.html"))
	for _, required := range []string{
		`id="public-browser-host"`,
		`id="owner-browser-host"`,
		`id="trash-browser-host"`,
		`id="file-browser-surface" data-access="owner"`,
		`id="file-filter" type="search" placeholder="Filter loaded files"`,
		`id="file-sort"`,
		`id="file-presentation"`,
		`id="metadata-filters"`,
		`<th scope="col">Name</th><th scope="col">Size</th><th scope="col">Mime type</th><th scope="col">Changed</th>`,
	} {
		if !strings.Contains(shell, required) {
			t.Errorf("shared file browser surface is missing %q", required)
		}
	}
	for _, forbidden := range []string{`id="public-table"`, `id="public-rows"`, `id="public-breadcrumbs"`, `id="public-state"`, `id="trash-table"`, `id="trash-rows"`, `id="trash-state"`, `id="trash-next"`} {
		if strings.Contains(shell, forbidden) {
			t.Errorf("a file listing retains a divergent browser component %q", forbidden)
		}
	}

	script := string(applicationScript)
	for _, required := range []string{
		`function syncFileBrowserAccess(access)`,
		`host.append(surface);`,
		`syncFileBrowserAccess("public");`,
		`syncFileBrowserAccess("trash");`,
		`state.entries = append ? state.entries.concat(entries) : entries;`,
		`const entries = (page.items || []).map(trashBrowserEntry);`,
		`renderFiles();`,
		`state.browserAccess === "public" ? state.publicToken : ""`,
		`const previewState = state.browserAccess === "owner" ? byID("filter-preview").value : "";`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("owner, public, and trash access do not share browser behavior: missing %q", required)
		}
	}

	stylesheet := string(applicationStylesheet)
	for _, required := range []string{
		`#file-browser-surface:not([data-access="owner"]) .owner-only { display: none !important; }`,
		`#file-browser-surface[data-access="trash"] .trash-only { display: flex !important; }`,
	} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("shared file-browser access policy is missing %q", required)
		}
	}
}

func TestListLoadingOnlyReplacesPerItemContent(t *testing.T) {
	t.Parallel()

	script := string(applicationScript)
	for _, required := range []string{
		`renderTableLoadingRows("file-rows", "file", 6, 6)`,
		`async function loadTrash(append = false)`,
		`renderFileLoadingItems();`,
		`renderTableLoadingRows("passkey-list", "passkey", 4, 3)`,
		`renderTableLoadingRows("share-list", "share", 6, 3)`,
		`renderRecordLoadingItems("invite-list")`,
		`renderTableLoadingRows("user-list", "user", 5, 3)`,
		`target.setAttribute("aria-busy", "true")`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("list loading audit is missing %q", required)
		}
	}
	for _, forbidden := range []string{`"Loading files…"`, `"Loading trash…"`, `"Loading shared items…"`} {
		if strings.Contains(script, forbidden) {
			t.Errorf("structural list UI is still replaced by a generic loading panel %q", forbidden)
		}
	}

	stylesheet := string(applicationStylesheet)
	for _, forbidden := range []string{".loading-table-heading", ".loading-folder-control", ".loading-controls"} {
		if strings.Contains(stylesheet, forbidden) {
			t.Errorf("non-item component retains loading styling %q", forbidden)
		}
	}
}

func TestSettingsCollectionsUseBoundedVirtualTables(t *testing.T) {
	t.Parallel()

	shell := string(mustRead("ui/index.html"))
	for _, required := range []string{
		`id="passkey-table-scroll" class="table-scroll settings-table-scroll"`,
		`<table id="passkey-table">`,
		`<th scope="col">Label</th><th scope="col">Added</th><th scope="col">Last used</th>`,
		`<tbody id="passkey-list"></tbody>`,
		`id="share-table-scroll" class="table-scroll settings-table-scroll"`,
		`<table id="share-table">`,
		`<th scope="col">Location</th><th scope="col">Status</th><th scope="col">Type</th><th scope="col">Created</th><th scope="col">Expires</th>`,
		`<tbody id="share-list"></tbody>`,
	} {
		if !strings.Contains(shell, required) {
			t.Errorf("scalable settings table is missing %q", required)
		}
	}
	for _, forbidden := range []string{`<ul id="passkey-list"`, `<ul id="share-list"`} {
		if strings.Contains(shell, forbidden) {
			t.Errorf("settings collection retains list markup %q", forbidden)
		}
	}

	script := string(applicationScript)
	for _, required := range []string{
		`function renderVirtualSettingsTable(`,
		`const settingsOverscanRows = 6;`,
		`rows.dataset.windowKey === windowKey`,
		`passkeys !== undefined`,
		`shares !== undefined`,
		`rows.dataset.itemCount = String(entries.length);`,
		`rows.dataset.renderedCount = String(end - start);`,
		`byID("passkey-table-scroll").addEventListener("scroll"`,
		`byID("share-table-scroll").addEventListener("scroll"`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("virtual settings table behavior is missing %q", required)
		}
	}

	stylesheet := string(applicationStylesheet)
	for _, required := range []string{
		`.settings-table-scroll {`,
		`grid-template-columns: minmax(0, 1fr);`,
		`.settings-section { min-width: 0; max-width: 100%;`,
		`max-width: 100%;`,
		`max-height: min(42vh, 360px);`,
		`overscroll-behavior: contain;`,
		`scrollbar-gutter: stable;`,
		`#passkey-table { min-width: 600px; }`,
		`#share-table { min-width: 820px; }`,
		`@media (max-width: 900px) {`,
		`#share-table { min-width: 0; }`,
		`@media (max-width: 560px) {`,
		`#passkey-table { min-width: 0; }`,
	} {
		if !strings.Contains(stylesheet, required) {
			t.Errorf("bounded settings table styling is missing %q", required)
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
