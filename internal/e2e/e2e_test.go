package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	stdruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/config"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/drive"
	"github.com/applyinnovations/endlessfs/internal/httpapi"
	"github.com/applyinnovations/endlessfs/internal/identity"
	"github.com/applyinnovations/endlessfs/internal/preview"
	"github.com/applyinnovations/endlessfs/internal/preview/imagegen"
	previewmemory "github.com/applyinnovations/endlessfs/internal/preview/memory"
	providermemory "github.com/applyinnovations/endlessfs/internal/provider/memory"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/theme"
	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/emulation"
	cdplog "github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	cdpwebauthn "github.com/chromedp/cdproto/webauthn"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

func TestVirtualAuthenticatorUsesPlatformTransport(t *testing.T) {
	options := virtualAuthenticatorOptions()
	if options.Transport != cdpwebauthn.AuthenticatorTransportInternal {
		t.Fatalf("virtual authenticator transport = %q, want internal", options.Transport)
	}
}

func TestBrowserNonRootUIDRejectsRootAndInvalidValues(t *testing.T) {
	for _, value := range []string{"0", "-1", "not-a-uid", "4294967296"} {
		if _, err := parseBrowserNonRootUID(value); err == nil {
			t.Errorf("parseBrowserNonRootUID(%q) unexpectedly succeeded", value)
		}
	}
	uid, err := parseBrowserNonRootUID("1000")
	if err != nil || uid != 1000 {
		t.Fatalf("parseBrowserNonRootUID(1000) = %d, %v", uid, err)
	}
}

func TestBrowserRuntimeExistsBeforeDirectoryHandoff(t *testing.T) {
	profile := t.TempDir()
	downloads := t.TempDir()
	var handedOff []string
	runtimeDirectory, err := prepareBrowserRuntime(profile, []string{downloads}, 1000, func(path string, _, _ int) error {
		if _, statErr := os.Stat(filepath.Join(profile, "runtime")); statErr != nil {
			return fmt.Errorf("runtime missing before handoff: %w", statErr)
		}
		handedOff = append(handedOff, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{downloads, runtimeDirectory, profile}
	if strings.Join(handedOff, "\n") != strings.Join(want, "\n") {
		t.Fatalf("handed-off directories = %v, want %v", handedOff, want)
	}
}

func TestE2EBrowserBootstrapLoginDriveShareAndTrash(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("set ENDLESSFS_RUN_E2E=1; the Nix test-e2e task does this")
	}
	browserPath := chromiumPath(t)
	harness := newHarness(t)

	profile := browserTempDir(t, "endlessfs-browser-profile-")
	downloads := browserTempDir(t, "endlessfs-browser-downloads-")
	options := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(browserPath),
		chromedp.UserDataDir(profile),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
	)
	if os.Getenv("ENDLESSFS_CHROMIUM_NO_SANDBOX") == "1" {
		options = append(options, chromedp.NoSandbox, chromedp.CombinedOutput(os.Stderr))
	}
	options = append(options, browserRuntimeOptions(t, profile, downloads)...)
	allocator, cancelAllocator := chromedp.NewExecAllocator(context.Background(), options...)
	t.Cleanup(cancelAllocator)
	ctx, cancelBrowser := chromedp.NewContext(allocator)
	t.Cleanup(cancelBrowser)
	ctx, cancelTimeout := context.WithTimeout(ctx, 60*time.Second)
	t.Cleanup(cancelTimeout)
	t.Cleanup(func() {
		// Wait for Chrome to stop writing its temporary profile before the
		// testing package removes the directory.
		_ = chromedp.Cancel(ctx)
	})

	var mu sync.Mutex
	var browserFailures []string
	var requestedOrigins []string
	var requestedURLs []string
	downloadStarted := make(chan struct{}, 1)
	chromedp.ListenTarget(ctx, func(event any) {
		switch value := event.(type) {
		case *runtime.EventExceptionThrown:
			mu.Lock()
			browserFailures = append(browserFailures, value.ExceptionDetails.Error())
			mu.Unlock()
		case *cdplog.EventEntryAdded:
			if strings.Contains(value.Entry.Text, "Content Security Policy") || strings.Contains(value.Entry.Text, "Refused to") {
				mu.Lock()
				browserFailures = append(browserFailures, value.Entry.Text)
				mu.Unlock()
			}
		case *network.EventRequestWillBeSent:
			parsed, err := url.Parse(value.Request.URL)
			if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
				mu.Lock()
				requestedOrigins = append(requestedOrigins, parsed.Scheme+"://"+parsed.Host)
				requestedURLs = append(requestedURLs, value.Request.Method+" "+value.Request.URL)
				mu.Unlock()
			}
			if value.Request.Method == http.MethodPost && parsed != nil && parsed.Path == "/api/v1/downloads" {
				select {
				case downloadStarted <- struct{}{}:
				default:
				}
			}
		}
	})

	var authenticatorID cdpwebauthn.AuthenticatorID
	// The first Run owns the browser allocation and therefore uses the parent
	// context; cancelling a derived first-run context would terminate Chrome.
	if err := chromedp.Run(ctx, network.Enable(), runtime.Enable(), cdplog.Enable(), chromedp.Navigate(harness.origin+"/bootstrap")); err != nil {
		t.Fatalf("launch browser: %v", err)
	}
	if err := runStage(ctx, 10*time.Second,
		browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorAllow).WithDownloadPath(downloads).WithEventsEnabled(true),
		cdpwebauthn.Enable(),
		chromedp.ActionFunc(func(actionContext context.Context) error {
			var addErr error
			authenticatorID, addErr = addVirtualAuthenticator(actionContext)
			return addErr
		}),
	); err != nil {
		t.Fatalf("configure browser: %v (%s)", err, browserStatus(ctx))
	}
	if authenticatorID == "" {
		t.Fatal("virtual authenticator was not created")
	}
	if err := waitVisible(ctx, "#registration-view", 10*time.Second); err != nil {
		mu.Lock()
		failureSnapshot := append([]string(nil), browserFailures...)
		originSnapshot := append([]string(nil), requestedOrigins...)
		mu.Unlock()
		t.Fatalf("open bootstrap: %v (%s) exceptions=%v origins=%v", err, browserStatus(ctx), failureSnapshot, originSnapshot)
	}
	if err := chromedp.Run(ctx, bootstrapKeyboardActions(harness.bootstrapToken)); err != nil {
		t.Fatalf("submit keyboard bootstrap: %v", err)
	}
	if err := waitVisible(ctx, "#auth-view", 15*time.Second); err != nil {
		mu.Lock()
		failures := append([]string(nil), browserFailures...)
		requests := append([]string(nil), requestedURLs...)
		mu.Unlock()
		t.Fatalf("keyboard bootstrap: %v (%s) exceptions=%v requests=%v", err, browserStatus(ctx), failures, requests)
	}
	if err := chromedp.Run(ctx,
		chromedp.Focus("#signin-button", chromedp.ByQuery),
		chromedp.KeyEvent(kb.Enter),
	); err != nil {
		t.Fatalf("submit keyboard sign-in: %v", err)
	}
	if err := waitVisible(ctx, "#drive-view", 15*time.Second); err != nil {
		t.Fatalf("keyboard sign-in: %v (%s)", err, browserStatus(ctx))
	}

	uploadPath := filepath.Join(t.TempDir(), "browser-proof.txt")
	if err := os.WriteFile(uploadPath, []byte("EndlessFS browser proof\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(ctx,
		chromedp.SetUploadFiles("#upload-input", []string{uploadPath}, chromedp.ByQuery),
		chromedp.WaitVisible("#file-rows tr", chromedp.ByQuery),
		chromedp.Click("#file-rows input[type='checkbox']", chromedp.ByQuery),
		chromedp.WaitEnabled("#download-selected", chromedp.ByQuery),
		chromedp.Click("#download-selected", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("upload and download initiation: %v", err)
	}
	downloadContext, cancelDownloadWait := context.WithTimeout(ctx, 5*time.Second)
	defer cancelDownloadWait()
	select {
	case <-downloadStarted:
	case <-downloadContext.Done():
		mu.Lock()
		requests := append([]string(nil), requestedURLs...)
		mu.Unlock()
		t.Fatalf("download capability was not requested: %v (%s) requests=%v", downloadContext.Err(), browserStatus(ctx), requests)
	}

	var shareLink string
	if err := runStage(ctx, 5*time.Second,
		chromedp.WaitEnabled("#share-selected", chromedp.ByQuery),
		chromedp.Focus("#share-selected", chromedp.ByQuery),
		chromedp.KeyEvent(kb.Enter),
	); err != nil {
		t.Fatalf("open share creation: %v (%s)", err, browserStatus(ctx))
	}
	if err := waitVisible(ctx, "#action-dialog", 5*time.Second); err != nil {
		var selection string
		_ = chromedp.Run(ctx, chromedp.Text("#selection-count", &selection, chromedp.ByQuery))
		mu.Lock()
		failures := append([]string(nil), browserFailures...)
		mu.Unlock()
		t.Fatalf("wait for share dialog: %v (%s) selection=%q exceptions=%v", err, browserStatus(ctx), selection, failures)
	}
	if err := chromedp.Run(ctx, chromedp.Click("#dialog-confirm", chromedp.ByQuery)); err != nil {
		t.Fatalf("submit share creation: %v (%s)", err, browserStatus(ctx))
	}
	if err := waitVisible(ctx, "#dialog-output", 10*time.Second); err != nil {
		var dialogTitle string
		_ = chromedp.Run(ctx, chromedp.Text("#dialog-title", &dialogTitle, chromedp.ByQuery))
		mu.Lock()
		requests := append([]string(nil), requestedURLs...)
		mu.Unlock()
		t.Fatalf("wait for share link: %v (%s) dialog=%q requests=%v", err, browserStatus(ctx), dialogTitle, requests)
	}
	if err := chromedp.Run(ctx,
		chromedp.Text("#dialog-output", &shareLink, chromedp.ByQuery),
		chromedp.Click("#dialog-confirm", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("share creation: %v", err)
	}
	if !strings.HasPrefix(shareLink, harness.origin+"/s/") {
		t.Fatalf("share link = %q, want same-origin public link", shareLink)
	}

	if err := chromedp.Run(ctx, chromedp.Focus("#trash-selected", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatalf("open trash confirmation: %v", err)
	}
	if err := waitVisible(ctx, "#action-dialog", 5*time.Second); err != nil {
		t.Fatalf("wait for trash confirmation: %v (%s)", err, browserStatus(ctx))
	}
	if err := chromedp.Run(ctx, chromedp.Click("#dialog-confirm", chromedp.ByQuery)); err != nil {
		t.Fatalf("confirm trash: %v", err)
	}
	if err := runStage(ctx, 10*time.Second, chromedp.WaitNotPresent("#file-rows tr", chromedp.ByQuery)); err != nil {
		t.Fatalf("wait for trashed file to leave drive: %v (%s)", err, browserStatus(ctx))
	}
	if err := chromedp.Run(ctx, chromedp.Click("a[data-route='trash']", chromedp.ByQuery)); err != nil {
		t.Fatalf("open trash: %v", err)
	}
	if err := waitVisible(ctx, "#trash-rows tr", 10*time.Second); err != nil {
		t.Fatalf("wait for trash listing: %v (%s)", err, browserStatus(ctx))
	}
	if err := chromedp.Run(ctx, chromedp.Focus("#trash-rows .row-actions button:first-child", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatalf("open restore confirmation: %v", err)
	}
	if err := waitVisible(ctx, "#action-dialog", 5*time.Second); err != nil {
		t.Fatalf("wait for restore confirmation: %v (%s)", err, browserStatus(ctx))
	}
	if err := chromedp.Run(ctx, chromedp.Click("#dialog-confirm", chromedp.ByQuery)); err != nil {
		t.Fatalf("confirm restore: %v", err)
	}
	if err := runStage(ctx, 10*time.Second, chromedp.WaitNotPresent("#trash-rows tr", chromedp.ByQuery)); err != nil {
		t.Fatalf("wait for restored file to leave trash: %v (%s)", err, browserStatus(ctx))
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(harness.origin+"/"),
		chromedp.WaitVisible("#drive-view", chromedp.ByQuery),
		chromedp.Evaluate(`(() => {
			const fileEntry = (name, body) => ({
				isFile: true, isDirectory: false, name,
				file: (success) => success(new File([body], name, {type: "text/plain"})),
			});
			const directoryEntry = (name, batches) => ({
				isFile: false, isDirectory: true, name,
				createReader: () => {
					let index = 0;
					return {readEntries: (success) => success(index < batches.length ? batches[index++] : [])};
				},
			});
			const nested = directoryEntry("Nested", [[fileEntry("second.txt", "second")], []]);
			const root = directoryEntry("Dropped Folder", [[fileEntry("first.txt", "first"), fileEntry("..", "must-not-escape")], [nested], []]);
			const event = new Event("drop", {bubbles: true, cancelable: true});
			Object.defineProperty(event, "dataTransfer", {value: {
				items: [{kind: "file", webkitGetAsEntry: () => root}],
				files: [],
			}});
			document.querySelector("#drop-target").dispatchEvent(event);
		})()`, nil),
	); err != nil {
		t.Fatalf("drop folder tree: %v", err)
	}
	if err := waitFor(ctx, `document.querySelector(".transfer-group-row.complete")?.textContent.includes("2 of 2 files") && (() => { const progress = document.querySelector("progress[aria-label='Upload progress for folder Dropped Folder']"); return progress && progress.value === progress.max; })()`, 15*time.Second); err != nil {
		t.Fatalf("wait for aggregate folder upload progress: %v (%s)", err, browserStatus(ctx))
	}
	if err := waitFor(ctx, `document.querySelector("#file-rows").textContent.includes("Dropped Folder")`, 10*time.Second); err != nil {
		t.Fatalf("wait for dropped folder listing: %v (%s)", err, browserStatus(ctx))
	}
	if err := chromedp.Run(ctx, chromedp.Click(`[aria-label="Open folder Dropped Folder"]`, chromedp.ByQuery)); err != nil {
		t.Fatalf("open dropped folder: %v", err)
	}
	if err := waitFor(ctx, `document.querySelector("#file-rows").textContent.includes("first.txt") && document.querySelector("#file-rows").textContent.includes("Nested")`, 10*time.Second); err != nil {
		t.Fatalf("wait for dropped folder contents: %v (%s)", err, browserStatus(ctx))
	}
	if err := chromedp.Run(ctx, chromedp.Click(`[aria-label="Open folder Nested"]`, chromedp.ByQuery)); err != nil {
		t.Fatalf("open nested dropped folder: %v", err)
	}
	if err := waitFor(ctx, `document.querySelector("#file-rows").textContent.includes("second.txt")`, 10*time.Second); err != nil {
		t.Fatalf("wait for nested dropped file: %v (%s)", err, browserStatus(ctx))
	}

	mediaPath := writeMediaFixture(t, "media-proof.png", 96, 48)
	if err := chromedp.Run(ctx,
		chromedp.Navigate(harness.origin+"/"),
		chromedp.WaitVisible("#drive-view", chromedp.ByQuery),
		chromedp.SetUploadFiles("#upload-input", []string{mediaPath}, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("upload media fixture: %v", err)
	}
	if err := waitFor(ctx, `document.querySelector("#file-rows").textContent.includes("media-proof.png")`, 10*time.Second); err != nil {
		t.Fatalf("wait for media fixture: %v (%s)", err, browserStatus(ctx))
	}
	gridBinding, gridClaim, gridArtifact := claimConcurrentBrowserPreview(t, harness, domain.MustParseUserPath("/media-proof.png"), mediaPath, 256)
	if err := chromedp.Run(ctx, chromedp.Focus("#file-view-grid", chromedp.ByQuery), chromedp.KeyEvent(" ")); err != nil {
		t.Fatalf("switch to media grid: %v", err)
	}
	if err := waitFor(ctx, `document.querySelector(".media-frame[data-path='/media-proof.png']").dataset.previewState === "generating"`, 5*time.Second); err != nil {
		t.Fatalf("wait for contending grid generation: %v (%s)", err, browserStatus(ctx))
	}
	if err := harness.previewStore.Commit(context.Background(), gridBinding, gridClaim, gridArtifact); err != nil {
		t.Fatalf("complete contending grid generation: %v", err)
	}
	if err := waitFor(ctx, `document.querySelector(".media-frame[data-path='/media-proof.png']")?.dataset.previewLoaded === "true" && document.querySelector(".media-frame img[alt='Preview of media-proof.png']")?.naturalWidth > 0`, 15*time.Second); err != nil {
		var gridState string
		_ = chromedp.Run(ctx, chromedp.Evaluate(`JSON.stringify(Array.from(document.querySelectorAll(".media-frame")).map((node) => ({path: node.dataset.path, state: node.dataset.previewState, html: node.innerHTML, complete: node.querySelector("img")?.complete, naturalWidth: node.querySelector("img")?.naturalWidth})))`, &gridState))
		mu.Lock()
		failures := append([]string(nil), browserFailures...)
		requests := append([]string(nil), requestedURLs...)
		mu.Unlock()
		t.Fatalf("wait for lazy WebP grid preview: %v (%s) grid=%s exceptions=%v requests=%v", err, browserStatus(ctx), gridState, failures, requests)
	}
	var squareFrame bool
	var previewAspect string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`Math.abs(document.querySelector(".media-frame img[alt='Preview of media-proof.png']").parentElement.getBoundingClientRect().width - document.querySelector(".media-frame img[alt='Preview of media-proof.png']").parentElement.getBoundingClientRect().height) < 1`, &squareFrame),
		chromedp.Evaluate(`document.querySelector(".media-frame img[alt='Preview of media-proof.png']").getAttribute("width") + "x" + document.querySelector(".media-frame img[alt='Preview of media-proof.png']").getAttribute("height")`, &previewAspect),
		chromedp.Focus(".media-tile-open[aria-label='View file media-proof.png']", chromedp.ByQuery),
		chromedp.KeyEvent(kb.Enter),
	); err != nil {
		t.Fatalf("open generated preview viewer: %v", err)
	}
	if !squareFrame || previewAspect != "96x48" {
		t.Fatalf("media geometry: squareFrame=%v intrinsic=%q", squareFrame, previewAspect)
	}
	if err := waitFor(ctx, `document.querySelector("#preview-status").textContent.includes("Preview ready") && document.querySelector("#preview-content img")?.naturalWidth > 0`, 15*time.Second); err != nil {
		t.Fatalf("wait for full-screen generated preview: %v (%s)", err, browserStatus(ctx))
	}
	mu.Lock()
	resolveRequestsBeforeViewerReopen := countRequestPath(requestedURLs, "/api/v1/previews/resolve")
	mu.Unlock()
	reopenStarted := time.Now()
	if err := chromedp.Run(ctx,
		chromedp.KeyEvent(kb.Escape),
		chromedp.Focus(".media-tile-open[aria-label='View file media-proof.png']", chromedp.ByQuery),
		chromedp.KeyEvent(kb.Enter),
	); err != nil {
		t.Fatalf("reopen verified generated preview: %v", err)
	}
	if err := waitFor(ctx, `document.querySelector("#preview-status").textContent.includes("Preview ready") && document.querySelector("#preview-content img")?.naturalWidth > 0`, time.Second); err != nil {
		t.Fatalf("verified generated preview was not reused immediately: %v (%s)", err, browserStatus(ctx))
	}
	mu.Lock()
	resolveRequestsAfterViewerReopen := countRequestPath(requestedURLs, "/api/v1/previews/resolve")
	mu.Unlock()
	if resolveRequestsAfterViewerReopen != resolveRequestsBeforeViewerReopen || time.Since(reopenStarted) > time.Second {
		t.Fatalf("viewer reopen missed document-memory preview: resolves before=%d after=%d elapsed=%s", resolveRequestsBeforeViewerReopen, resolveRequestsAfterViewerReopen, time.Since(reopenStarted))
	}
	if err := chromedp.Run(ctx, chromedp.KeyEvent(kb.Escape)); err != nil {
		t.Fatalf("close reused generated preview: %v", err)
	}
	mu.Lock()
	resolveRequestsBeforeExpiredReopen := countRequestPath(requestedURLs, "/api/v1/previews/resolve")
	mu.Unlock()
	if err := chromedp.Run(ctx, chromedp.Evaluate(`(() => {
		const originalNow = Date.now;
		Date.now = () => originalNow() + 60 * 60 * 1000;
		try { document.querySelector(".media-tile-open[aria-label='View file media-proof.png']").click(); }
		finally { Date.now = originalNow; }
	})()`, nil)); err != nil {
		t.Fatalf("reopen expired document-memory preview: %v", err)
	}
	if err := waitFor(ctx, `document.querySelector("#preview-status").textContent.includes("Preview ready") && document.querySelector("#preview-content img")?.naturalWidth > 0`, 15*time.Second); err != nil {
		t.Fatalf("expired generated preview was not reacquired: %v (%s)", err, browserStatus(ctx))
	}
	mu.Lock()
	resolveRequestsAfterExpiredReopen := countRequestPath(requestedURLs, "/api/v1/previews/resolve")
	mu.Unlock()
	if resolveRequestsAfterExpiredReopen <= resolveRequestsBeforeExpiredReopen {
		t.Fatalf("viewer reused preview beyond capability expiry: resolves before=%d after=%d", resolveRequestsBeforeExpiredReopen, resolveRequestsAfterExpiredReopen)
	}
	binding, claim, artifact := claimConcurrentBrowserPreview(t, harness, domain.MustParseUserPath("/media-proof.png"), mediaPath, 1600)
	if err := chromedp.Run(ctx, chromedp.Click("#preview-regenerate", chromedp.ByQuery)); err != nil {
		t.Fatalf("regenerate preview: %v", err)
	}
	if err := waitFor(ctx, `document.querySelector("#preview-status").textContent.includes("generation is running")`, 5*time.Second); err != nil {
		t.Fatalf("wait for contending preview operation: %v (%s)", err, browserStatus(ctx))
	}
	if err := harness.previewStore.Commit(context.Background(), binding, claim, artifact); err != nil {
		t.Fatalf("complete contending preview generation: %v", err)
	}
	if err := waitFor(ctx, `document.querySelector("#preview-status").textContent.includes("Preview ready")`, 15*time.Second); err != nil {
		t.Fatalf("wait for regenerated preview: %v (%s)", err, browserStatus(ctx))
	}
	if err := chromedp.Run(ctx, chromedp.KeyEvent(kb.ArrowLeft), chromedp.KeyEvent(kb.ArrowRight), chromedp.KeyEvent(kb.Escape)); err != nil {
		t.Fatalf("preview navigation and close: %v", err)
	}
	harness.corruptPreview.Store(true)
	if err := chromedp.Run(ctx,
		chromedp.Navigate(harness.origin+"/"),
		chromedp.WaitVisible("#drive-view", chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector("#file-view-grid").click()`, nil),
	); err != nil {
		t.Fatalf("open grid for corrupt preview denial: %v", err)
	}
	if err := waitFor(ctx, `document.querySelector(".media-frame[data-path='/media-proof.png']")?.dataset.previewState === "unavailable" && document.querySelector(".media-frame[data-path='/media-proof.png'] img[alt='Preview of media-proof.png']") === null`, 15*time.Second); err != nil {
		var gridState string
		_ = chromedp.Run(ctx, chromedp.Evaluate(`JSON.stringify({frames:Array.from(document.querySelectorAll(".media-frame")).map((node) => ({path:node.dataset.path,state:node.dataset.previewState,html:node.innerHTML})), mode:document.querySelector("input[name='file-view']:checked")?.value, drive:document.querySelector("#file-rows")?.textContent})`, &gridState))
		t.Fatalf("browser displayed a preview whose SHA-256 did not match its manifest: %v (%s) grid=%s", err, browserStatus(ctx), gridState)
	}
	harness.corruptPreview.Store(false)

	seedVirtualFiles(t, harness, 10_000)
	mu.Lock()
	resolveRequestsBeforeScale := countRequestPath(requestedURLs, "/api/v1/previews/resolve")
	mu.Unlock()
	if err := chromedp.Run(ctx,
		chromedp.Navigate(harness.origin+"/"),
		chromedp.WaitVisible("#drive-view", chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector("#file-view-grid").click()`, nil),
	); err != nil {
		t.Fatalf("open large virtual grid: %v", err)
	}
	loaded := 0
	for page := 0; page < 110; page++ {
		if err := waitFor(ctx, `Number(document.querySelector("#media-grid-content").dataset.itemCount || 0) > `+fmt.Sprint(loaded), 10*time.Second); err != nil {
			t.Fatalf("load virtual grid page %d after %d items: %v (%s)", page, loaded, err, browserStatus(ctx))
		}
		if err := chromedp.Run(ctx, chromedp.Evaluate(`Number(document.querySelector("#media-grid-content").dataset.itemCount)`, &loaded)); err != nil {
			t.Fatal(err)
		}
		var nextHidden bool
		if err := chromedp.Run(ctx, chromedp.Evaluate(`document.querySelector("#next-page").hidden`, &nextHidden)); err != nil {
			t.Fatal(err)
		}
		if nextHidden {
			break
		}
		if err := chromedp.Run(ctx, chromedp.Click("#next-page", chromedp.ByQuery)); err != nil {
			t.Fatalf("load virtual grid page after %d items: %v", loaded, err)
		}
	}
	var renderedTiles int
	if err := chromedp.Run(ctx, chromedp.Evaluate(`document.querySelectorAll(".media-tile").length`, &renderedTiles)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	resolveRequestsAfterScale := countRequestPath(requestedURLs, "/api/v1/previews/resolve")
	mu.Unlock()
	if loaded != 10_003 || renderedTiles > 64 || resolveRequestsAfterScale-resolveRequestsBeforeScale > 32 {
		t.Fatalf("virtual grid bounds: logical=%d rendered=%d previewRequests=%d", loaded, renderedTiles, resolveRequestsAfterScale-resolveRequestsBeforeScale)
	}
	if err := waitFor(ctx, `document.querySelector(".media-frame img[alt='Preview of media-proof.png']") !== null`, 15*time.Second); err != nil {
		t.Fatalf("wait for preview before virtual eviction: %v (%s)", err, browserStatus(ctx))
	}
	mu.Lock()
	resolveRequestsBeforeEviction := countRequestPath(requestedURLs, "/api/v1/previews/resolve")
	mu.Unlock()
	if err := chromedp.Run(ctx, chromedp.Evaluate(`(() => { const grid = document.querySelector("#media-grid"); grid.scrollTop = grid.scrollHeight; grid.dispatchEvent(new Event("scroll")); })()`, nil)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(ctx, `document.querySelector(".media-frame[data-path='/media-proof.png']") === null`, 10*time.Second); err != nil {
		t.Fatalf("preview tile was not virtually evicted: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`(() => { const grid = document.querySelector("#media-grid"); grid.scrollTop = 0; grid.dispatchEvent(new Event("scroll")); })()`, nil)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(ctx, `document.querySelector(".media-frame img[alt='Preview of media-proof.png']") !== null`, 15*time.Second); err != nil {
		t.Fatalf("evicted preview was not reacquired: %v (%s)", err, browserStatus(ctx))
	}
	mu.Lock()
	resolveRequestsAfterEviction := countRequestPath(requestedURLs, "/api/v1/previews/resolve")
	mu.Unlock()
	if resolveRequestsAfterEviction <= resolveRequestsBeforeEviction {
		t.Fatalf("evicted preview reused a revoked object URL: requests before=%d after=%d", resolveRequestsBeforeEviction, resolveRequestsAfterEviction)
	}

	var fitsMobile bool
	var namedControls bool
	var focusOutline string
	var focusID string
	if err := chromedp.Run(ctx,
		emulation.SetDeviceMetricsOverride(320, 720, 1, false),
		chromedp.Navigate(harness.origin+"/"),
		chromedp.WaitVisible("#drive-view", chromedp.ByQuery),
		chromedp.Evaluate(`document.documentElement.scrollWidth <= 320`, &fitsMobile),
		chromedp.Evaluate(`Array.from(document.querySelectorAll("button:not([hidden])")).every((node) => (node.textContent || node.getAttribute("aria-label") || "").trim().length > 0)`, &namedControls),
		chromedp.Evaluate(`document.activeElement.blur()`, nil),
		chromedp.KeyEvent(kb.Tab),
		chromedp.Evaluate(`document.activeElement.id || document.activeElement.className`, &focusID),
		chromedp.Evaluate(`getComputedStyle(document.activeElement).outlineStyle`, &focusOutline),
	); err != nil {
		t.Fatalf("mobile and accessibility assertions: %v", err)
	}
	if !fitsMobile || !namedControls || focusOutline == "none" {
		t.Fatalf("accessibility result: fitsMobile=%v namedControls=%v focus=%q focusOutline=%q", fitsMobile, namedControls, focusID, focusOutline)
	}
	if err := chromedp.Run(ctx,
		chromedp.Click("#logout-button", chromedp.ByQuery),
		chromedp.WaitVisible("#auth-view", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("sign out: %v (%s)", err, browserStatus(ctx))
	}

	mu.Lock()
	defer mu.Unlock()
	if len(browserFailures) != 0 {
		t.Fatalf("browser exceptions: %v", browserFailures)
	}
	for _, requestedOrigin := range requestedOrigins {
		if requestedOrigin != harness.origin && requestedOrigin != harness.dataOrigin && requestedOrigin != harness.previewOrigin {
			t.Errorf("unexpected browser request origin: %s", requestedOrigin)
		}
	}
}

func claimConcurrentBrowserPreview(t *testing.T, harness harness, path domain.UserPath, sourcePath string, variant int) (preview.Binding, preview.GenerationClaim, preview.Artifact) {
	t.Helper()
	accounts, err := harness.repository.Accounts(context.Background())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("resolve browser preview owner: %v, accounts=%d", err, len(accounts))
	}
	scope, err := domain.NewScope(accounts[0].UserID, domain.AreaLive)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := harness.storage.Stat(context.Background(), scope, path)
	if err != nil {
		t.Fatal(err)
	}
	binding := preview.Binding{
		Owner: accounts[0].UserID, ContentID: entry.ContentID, ContentVersion: entry.ContentVersion,
		MediaType: entry.MediaType, SourceSize: entry.Size, RecipeID: "image-webp-q80-v1", Variant: variant,
	}
	const generationID = "browser-contending-generation"
	claim, err := harness.previewStore.Claim(context.Background(), binding, generationID, harness.clock.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := imagegen.New(imagegen.Options{}).Generate(context.Background(), preview.GenerationRequest{
		Source: bytes.NewReader(source), SourceSize: int64(len(source)), MediaType: entry.MediaType, Variant: variant,
	})
	if err != nil {
		t.Fatal(err)
	}
	data := generated.Bytes
	sum := sha256.Sum256(data)
	artifact := preview.Artifact{
		GenerationID: generationID, Variant: variant, Width: generated.Width, Height: generated.Height, ContentType: preview.ContentTypeWebP,
		Size: int64(len(data)), SHA256: base64.RawURLEncoding.EncodeToString(sum[:]), CRC32C: preview.ChecksumCRC32C(data), Bytes: data,
	}
	return binding, claim, artifact
}

func TestE2EInviteSettingsAdminRecoveryAndShareRevocation(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("set ENDLESSFS_RUN_E2E=1; the Nix test-e2e task does this")
	}
	harness := newHarness(t)
	admin := newTestBrowser(t)
	bootstrapBrowser(t, admin, harness)

	if err := chromedp.Run(admin.ctx,
		chromedp.Click("a[data-route='admin']", chromedp.ByQuery),
		chromedp.WaitVisible("#admin-view", chromedp.ByQuery),
		chromedp.Focus("#create-invite", chromedp.ByQuery),
		chromedp.KeyEvent(kb.Enter),
		chromedp.WaitVisible("#action-dialog", chromedp.ByQuery),
		chromedp.Click("#dialog-confirm", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if err := waitFor(admin.ctx, `document.querySelector("#dialog-output") !== null`, 10*time.Second); err != nil {
		t.Fatalf("wait for invite link: %v (%s)", err, browserStatus(admin.ctx))
	}
	var inviteLink string
	if err := chromedp.Run(admin.ctx,
		chromedp.Text("#dialog-output", &inviteLink, chromedp.ByQuery),
		chromedp.Click("#dialog-confirm", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read invite link: %v", err)
	}
	if !strings.HasPrefix(inviteLink, harness.origin+"/register/invite/") {
		t.Fatalf("invite link = %q", inviteLink)
	}

	member := newTestBrowser(t)
	if err := chromedp.Run(member.ctx,
		chromedp.Navigate(inviteLink),
		chromedp.WaitVisible("#registration-view", chromedp.ByQuery),
		chromedp.SendKeys("#display-name", "Invited Member"+kb.Tab+kb.Enter, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("invite registration: %v", err)
	}
	if err := waitVisible(member.ctx, "#auth-view", 15*time.Second); err != nil {
		t.Fatalf("finish invite registration: %v (%s)", err, browserStatus(member.ctx))
	}
	if err := chromedp.Run(member.ctx, chromedp.Focus("#signin-button", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatalf("invitee sign-in: %v", err)
	}
	if err := waitVisible(member.ctx, "#drive-view", 15*time.Second); err != nil {
		t.Fatalf("finish invitee sign-in: %v (%s)", err, browserStatus(member.ctx))
	}

	if err := chromedp.Run(member.ctx,
		chromedp.Click("a[data-route='settings']", chromedp.ByQuery),
		chromedp.WaitVisible("#settings-view", chromedp.ByQuery),
		chromedp.SetValue("#profile-name", "Renamed Member", chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector("#profile-form").requestSubmit()`, nil),
	); err != nil {
		t.Fatalf("update profile: %v", err)
	}
	if err := waitFor(member.ctx, `document.querySelector("#account-name").textContent === "Renamed Member"`, 10*time.Second); err != nil {
		t.Fatalf("wait for renamed profile: %v", err)
	}
	if err := chromedp.Run(member.ctx,
		chromedp.Evaluate(`document.querySelector("#theme-select").value = "endlessfs-dark"; document.querySelector("#theme-form").requestSubmit()`, nil),
	); err != nil {
		t.Fatalf("select dark theme: %v", err)
	}
	if err := waitFor(member.ctx, `document.documentElement.dataset.theme === "endlessfs-dark"`, 10*time.Second); err != nil {
		t.Fatalf("wait for dark theme: %v (%s)", err, browserStatus(member.ctx))
	}

	if err := chromedp.Run(member.ctx,
		chromedp.Focus("#add-passkey", chromedp.ByQuery),
		chromedp.KeyEvent(kb.Enter),
		chromedp.WaitVisible("#action-dialog", chromedp.ByQuery),
		chromedp.SetValue("#passkey-label", "Second device", chromedp.ByQuery),
		chromedp.Click("#dialog-confirm", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("add second passkey: %v", err)
	}
	if err := waitFor(member.ctx, `document.querySelector("#passkey-list").children.length === 2`, 15*time.Second); err != nil {
		t.Fatalf("wait for second passkey: %v (%s)", err, browserStatus(member.ctx))
	}
	if err := chromedp.Run(member.ctx,
		chromedp.Focus("#passkey-list button", chromedp.ByQuery),
		chromedp.KeyEvent(kb.Enter),
		chromedp.WaitVisible("#action-dialog", chromedp.ByQuery),
		chromedp.Click("#dialog-confirm", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("remove one passkey: %v", err)
	}
	if err := waitFor(member.ctx, `document.querySelector("#passkey-list").children.length === 1`, 10*time.Second); err != nil {
		t.Fatalf("wait for passkey removal: %v", err)
	}

	uploadPath := filepath.Join(t.TempDir(), "member-file.txt")
	if err := os.WriteFile(uploadPath, []byte("member share proof\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(member.ctx,
		chromedp.Click("a[data-route='drive']", chromedp.ByQuery),
		chromedp.WaitVisible("#drive-view", chromedp.ByQuery),
		chromedp.SetUploadFiles("#upload-input", []string{uploadPath}, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("member upload: %v", err)
	}
	if err := waitFor(member.ctx, `document.querySelectorAll("#file-rows tr").length === 1`, 15*time.Second); err != nil {
		t.Fatalf("wait for member upload: %v (%s)", err, browserStatus(member.ctx))
	}

	if err := chromedp.Run(member.ctx,
		chromedp.Focus("#new-folder-button", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter),
		chromedp.WaitVisible("#action-dialog", chromedp.ByQuery),
		chromedp.SetValue("#folder-name", "moved", chromedp.ByQuery),
		chromedp.Click("#dialog-confirm", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("create move destination: %v", err)
	}
	if err := waitFor(member.ctx, `document.querySelectorAll("#file-rows tr").length === 2`, 10*time.Second); err != nil {
		t.Fatalf("wait for destination folder: %v", err)
	}
	if err := chromedp.Run(member.ctx,
		chromedp.Click("#file-rows input[type='checkbox']", chromedp.ByQuery),
		chromedp.Focus("#copy-selected", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter),
		chromedp.WaitVisible("#action-dialog", chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector("#conflict").value = "rename"`, nil),
		chromedp.Click("#dialog-confirm", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("copy with rename policy: %v", err)
	}
	if err := waitFor(member.ctx, `document.querySelectorAll("#file-rows tr").length === 3`, 10*time.Second); err != nil {
		t.Fatalf("wait for copied file: %v (%s) requests=%v", err, browserStatus(member.ctx), member.requestSnapshot())
	}
	if err := chromedp.Run(member.ctx,
		chromedp.Click("#file-rows input[type='checkbox']", chromedp.ByQuery),
		chromedp.Focus("#move-selected", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter),
		chromedp.WaitVisible("#action-dialog", chromedp.ByQuery),
		chromedp.SetValue("#destination", "/moved", chromedp.ByQuery),
		chromedp.Click("#dialog-confirm", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("move copied file: %v", err)
	}
	if err := waitFor(member.ctx, `document.querySelectorAll("#file-rows tr").length === 2`, 10*time.Second); err != nil {
		t.Fatalf("wait for moved file: %v (%s)", err, browserStatus(member.ctx))
	}
	if err := chromedp.Run(member.ctx,
		chromedp.Evaluate(`Array.from(document.querySelectorAll("#file-rows .file-name")).find((node) => node.textContent.includes("moved")).click()`, nil),
	); err != nil {
		t.Fatalf("open moved folder: %v", err)
	}
	if err := waitFor(member.ctx, `document.querySelectorAll("#file-rows tr").length === 1`, 10*time.Second); err != nil {
		t.Fatalf("wait for moved folder: %v", err)
	}

	if err := chromedp.Run(member.ctx,
		chromedp.Click("#file-rows input[type='checkbox']", chromedp.ByQuery),
		chromedp.Focus("#share-selected", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter),
		chromedp.WaitVisible("#action-dialog", chromedp.ByQuery),
		chromedp.Click("#dialog-confirm", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("create member share: %v", err)
	}
	if err := waitFor(member.ctx, `document.querySelector("#dialog-output") !== null`, 10*time.Second); err != nil {
		t.Fatalf("wait for member share: %v", err)
	}
	var shareLink string
	if err := chromedp.Run(member.ctx, chromedp.Text("#dialog-output", &shareLink, chromedp.ByQuery), chromedp.Click("#dialog-confirm", chromedp.ByQuery)); err != nil {
		t.Fatalf("read member share: %v", err)
	}

	if err := chromedp.Run(admin.ctx, chromedp.Navigate(harness.origin+"/admin"), chromedp.WaitVisible("#admin-view", chromedp.ByQuery)); err != nil {
		t.Fatalf("open user administration: %v", err)
	}
	if err := waitFor(admin.ctx, `Array.from(document.querySelectorAll("#user-list li")).some((node) => node.textContent.includes("Renamed Member"))`, 10*time.Second); err != nil {
		t.Fatalf("wait for invited user: %v (%s)", err, browserStatus(admin.ctx))
	}
	if err := chromedp.Run(admin.ctx, chromedp.Evaluate(`Array.from(document.querySelectorAll("#user-list li")).find((node) => node.textContent.includes("Renamed Member")).querySelector("button").click()`, nil), chromedp.WaitVisible("#action-dialog", chromedp.ByQuery), chromedp.Click("#dialog-confirm", chromedp.ByQuery)); err != nil {
		t.Fatalf("disable invited user: %v", err)
	}
	if err := waitFor(admin.ctx, `Array.from(document.querySelectorAll("#user-list li")).some((node) => node.textContent.includes("Renamed Member") && node.textContent.includes("disabled"))`, 10*time.Second); err != nil {
		t.Fatalf("wait for disabled user: %v", err)
	}
	if err := chromedp.Run(admin.ctx, chromedp.Navigate(shareLink)); err != nil {
		t.Fatalf("open disabled-owner share: %v", err)
	}
	if err := waitFor(admin.ctx, `document.querySelector("#public-state").textContent.includes("unavailable")`, 10*time.Second); err != nil {
		t.Fatalf("disabled owner share was not denied: %v (%s)", err, browserStatus(admin.ctx))
	}

	if err := chromedp.Run(admin.ctx, chromedp.Navigate(harness.origin+"/admin"), chromedp.WaitVisible("#admin-view", chromedp.ByQuery)); err != nil {
		t.Fatalf("return to administration: %v", err)
	}
	if err := waitFor(admin.ctx, `Array.from(document.querySelectorAll("#user-list li")).some((node) => node.textContent.includes("Renamed Member"))`, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(admin.ctx, chromedp.Evaluate(`Array.from(document.querySelectorAll("#user-list li")).find((node) => node.textContent.includes("Renamed Member")).querySelector("button").click()`, nil), chromedp.WaitVisible("#action-dialog", chromedp.ByQuery), chromedp.Click("#dialog-confirm", chromedp.ByQuery)); err != nil {
		t.Fatalf("enable invited user: %v", err)
	}
	if err := waitFor(admin.ctx, `Array.from(document.querySelectorAll("#user-list li")).some((node) => node.textContent.includes("Renamed Member") && node.textContent.includes("enabled"))`, 10*time.Second); err != nil {
		t.Fatalf("wait for enabled user: %v", err)
	}
	if err := chromedp.Run(admin.ctx, chromedp.Evaluate(`Array.from(document.querySelectorAll("#user-list li")).find((node) => node.textContent.includes("Renamed Member")).querySelectorAll("button")[2].click()`, nil), chromedp.WaitVisible("#action-dialog", chromedp.ByQuery), chromedp.Click("#dialog-confirm", chromedp.ByQuery)); err != nil {
		t.Fatalf("create recovery link: %v", err)
	}
	if err := waitFor(admin.ctx, `document.querySelector("#dialog-output") !== null`, 10*time.Second); err != nil {
		t.Fatalf("wait for recovery link: %v", err)
	}
	var recoveryLink string
	if err := chromedp.Run(admin.ctx, chromedp.Text("#dialog-output", &recoveryLink, chromedp.ByQuery), chromedp.Click("#dialog-confirm", chromedp.ByQuery)); err != nil {
		t.Fatalf("read recovery link: %v", err)
	}
	if !strings.HasPrefix(recoveryLink, harness.origin+"/recover/") {
		t.Fatalf("recovery link = %q", recoveryLink)
	}

	if err := chromedp.Run(member.ctx,
		chromedp.Navigate(recoveryLink),
		chromedp.WaitVisible("#registration-view", chromedp.ByQuery),
		chromedp.Focus("#registration-form button", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter),
	); err != nil {
		t.Fatalf("start recovery: %v", err)
	}
	if err := waitVisible(member.ctx, "#auth-view", 15*time.Second); err != nil {
		t.Fatalf("finish recovery: %v (%s)", err, browserStatus(member.ctx))
	}
	if err := chromedp.Run(member.ctx, chromedp.Focus("#signin-button", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatalf("sign in after recovery: %v", err)
	}
	if err := waitVisible(member.ctx, "#drive-view", 15*time.Second); err != nil {
		t.Fatalf("finish sign in after recovery: %v (%s)", err, browserStatus(member.ctx))
	}

	if err := chromedp.Run(member.ctx, chromedp.Click("a[data-route='settings']", chromedp.ByQuery), chromedp.WaitVisible("#settings-view", chromedp.ByQuery)); err != nil {
		t.Fatalf("open share management: %v", err)
	}
	if err := waitFor(member.ctx, `document.querySelector("#share-list button") !== null`, 10*time.Second); err != nil {
		t.Fatalf("wait for active share management: %v", err)
	}
	if err := chromedp.Run(member.ctx, chromedp.Focus("#share-list button", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter), chromedp.WaitVisible("#action-dialog", chromedp.ByQuery), chromedp.Click("#dialog-confirm", chromedp.ByQuery)); err != nil {
		t.Fatalf("revoke share: %v", err)
	}
	if err := waitFor(member.ctx, `document.querySelector("#share-list").textContent.includes("Revoked")`, 10*time.Second); err != nil {
		t.Fatalf("wait for share revocation: %v", err)
	}
	if err := chromedp.Run(admin.ctx, chromedp.Navigate(shareLink)); err != nil {
		t.Fatalf("reopen revoked share: %v", err)
	}
	if err := waitFor(admin.ctx, `document.querySelector("#public-state").textContent.includes("unavailable")`, 10*time.Second); err != nil {
		t.Fatalf("revoked share was not denied: %v (%s)", err, browserStatus(admin.ctx))
	}

	darkMediaPath := writeMediaFixture(t, "dark-mobile-preview.png", 48, 96)
	if err := chromedp.Run(member.ctx,
		emulation.SetDeviceMetricsOverride(320, 720, 1, false),
		chromedp.Click("a[data-route='drive']", chromedp.ByQuery),
		chromedp.WaitVisible("#drive-view", chromedp.ByQuery),
		chromedp.SetUploadFiles("#upload-input", []string{darkMediaPath}, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("upload dark mobile media fixture: %v", err)
	}
	if err := waitFor(member.ctx, `document.querySelector("#file-rows").textContent.includes("dark-mobile-preview.png")`, 15*time.Second); err != nil {
		t.Fatalf("wait for dark mobile fixture: %v (%s)", err, browserStatus(member.ctx))
	}
	if err := chromedp.Run(member.ctx, chromedp.Focus("#file-view-grid", chromedp.ByQuery), chromedp.KeyEvent(" ")); err != nil {
		t.Fatalf("open dark mobile grid: %v", err)
	}
	if err := waitFor(member.ctx, `document.querySelector(".media-frame img[alt='Preview of dark-mobile-preview.png']") !== null`, 15*time.Second); err != nil {
		t.Fatalf("wait for dark mobile preview: %v (%s)", err, browserStatus(member.ctx))
	}
	if err := chromedp.Run(member.ctx,
		chromedp.Focus(".media-tile-open[aria-label='View file dark-mobile-preview.png']", chromedp.ByQuery),
		chromedp.KeyEvent(kb.Enter),
	); err != nil {
		t.Fatalf("open dark mobile viewer by keyboard: %v", err)
	}
	if err := waitFor(member.ctx, `document.querySelector("#preview-status").textContent.includes("Preview ready")`, 15*time.Second); err != nil {
		t.Fatalf("wait for dark mobile viewer: %v (%s)", err, browserStatus(member.ctx))
	}
	var darkMobileViewer bool
	if err := chromedp.Run(member.ctx,
		chromedp.Evaluate(`document.documentElement.dataset.theme === "endlessfs-dark" && document.documentElement.scrollWidth <= 320 && document.querySelector("#preview-dialog").getBoundingClientRect().width <= 320`, &darkMobileViewer),
		chromedp.KeyEvent(kb.ArrowLeft),
		chromedp.KeyEvent(kb.ArrowRight),
		chromedp.KeyEvent(kb.Escape),
	); err != nil {
		t.Fatalf("navigate dark mobile viewer by keyboard: %v", err)
	}
	if !darkMobileViewer {
		t.Fatal("dark media viewer did not fit the 320-pixel viewport")
	}

	admin.assertNoExternalRequests(t, harness)
	member.assertNoExternalRequests(t, harness)
}

func TestE2EMediaBrowserIsAvailableWithoutGeneratedPreviews(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("set ENDLESSFS_RUN_E2E=1; the Nix test-e2e task does this")
	}
	harness := newHarnessWithPreviews(t, false)
	client := newTestBrowser(t)
	bootstrapBrowser(t, client, harness)

	mediaPath := writeMediaFixture(t, "icon-only.png", 96, 48)
	if err := chromedp.Run(client.ctx,
		chromedp.SetUploadFiles("#upload-input", []string{mediaPath}, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("upload icon-only fixture: %v", err)
	}
	if err := waitFor(client.ctx, `document.querySelector("#file-rows").textContent.includes("icon-only.png")`, 15*time.Second); err != nil {
		t.Fatalf("wait for icon-only fixture: %v (%s)", err, browserStatus(client.ctx))
	}
	if err := chromedp.Run(client.ctx,
		chromedp.Focus("#file-view-grid", chromedp.ByQuery),
		chromedp.KeyEvent(" "),
		chromedp.Focus(".media-tile-open[aria-label='View file icon-only.png']", chromedp.ByQuery),
		chromedp.KeyEvent(kb.Enter),
	); err != nil {
		t.Fatalf("open icon-only grid and viewer: %v", err)
	}
	if err := waitFor(client.ctx, `document.querySelector("#preview-dialog").open && document.querySelector("#preview-content .viewer-fallback-icon") !== null`, 10*time.Second); err != nil {
		t.Fatalf("wait for icon-only viewer: %v (%s)", err, browserStatus(client.ctx))
	}
	var correctBoundary bool
	if err := chromedp.Run(client.ctx,
		chromedp.Evaluate(`!document.querySelector("#file-presentation").hidden && !document.querySelector("#metadata-filters").hidden && document.querySelector("#preview-generate").hidden && document.querySelector("#preview-regenerate").hidden && document.querySelector("#preview-status").textContent.includes("not configured")`, &correctBoundary),
		chromedp.KeyEvent(kb.ArrowLeft),
		chromedp.KeyEvent(kb.ArrowRight),
		chromedp.KeyEvent(kb.Escape),
	); err != nil {
		t.Fatalf("verify icon-only media browser: %v", err)
	}
	if !correctBoundary {
		t.Fatal("grid, metadata filters, or full-screen icon viewer depended on generated-preview configuration")
	}
	for _, request := range client.requestSnapshot() {
		if strings.Contains(request, "/api/v1/previews/") {
			t.Fatalf("preview-disabled browser made generated-preview request %q", request)
		}
	}
	client.assertNoExternalRequests(t, harness)
}

func writeMediaFixture(t *testing.T, name string, width, height int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	imageValue := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			imageValue.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 2), G: uint8(y * 4), B: 140, A: 255})
		}
	}
	if err := png.Encode(file, imageValue); err != nil {
		if closeErr := file.Close(); closeErr != nil {
			t.Errorf("close failed after encode error: %v", closeErr)
		}
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

type testBrowser struct {
	ctx      context.Context
	mu       sync.Mutex
	origins  []string
	requests []string
}

func newTestBrowser(t *testing.T) *testBrowser {
	t.Helper()
	profile := browserTempDir(t, "endlessfs-browser-profile-")
	options := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromiumPath(t)), chromedp.UserDataDir(profile),
		chromedp.Flag("disable-background-networking", true), chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-sync", true), chromedp.Flag("no-first-run", true), chromedp.Flag("no-default-browser-check", true),
	)
	if os.Getenv("ENDLESSFS_CHROMIUM_NO_SANDBOX") == "1" {
		options = append(options, chromedp.NoSandbox, chromedp.CombinedOutput(os.Stderr))
	}
	options = append(options, browserRuntimeOptions(t, profile)...)
	allocator, cancelAllocator := chromedp.NewExecAllocator(context.Background(), options...)
	ctx, cancelBrowser := chromedp.NewContext(allocator)
	ctx, cancelTimeout := context.WithTimeout(ctx, 90*time.Second)
	client := &testBrowser{ctx: ctx}
	t.Cleanup(func() {
		_ = chromedp.Cancel(ctx)
		cancelTimeout()
		cancelBrowser()
		cancelAllocator()
	})
	chromedp.ListenTarget(ctx, func(event any) {
		if request, ok := event.(*network.EventRequestWillBeSent); ok {
			parsed, err := url.Parse(request.Request.URL)
			if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
				client.mu.Lock()
				client.origins = append(client.origins, parsed.Scheme+"://"+parsed.Host)
				client.requests = append(client.requests, request.Request.Method+" "+parsed.Path)
				client.mu.Unlock()
			}
		}
	})
	if err := chromedp.Run(ctx,
		network.Enable(), runtime.Enable(), chromedp.Navigate("about:blank"), cdpwebauthn.Enable(),
		chromedp.ActionFunc(func(actionContext context.Context) error {
			_, err := addVirtualAuthenticator(actionContext)
			return err
		}),
	); err != nil {
		t.Fatalf("prepare Chromium: %v", err)
	}
	return client
}

func browserTempDir(t *testing.T, pattern string) string {
	t.Helper()
	directory, err := os.MkdirTemp("", pattern)
	if err != nil {
		t.Fatalf("create browser temporary directory: %v", err)
	}
	t.Cleanup(func() {
		if err := removeBrowserTempDir(directory); err != nil {
			t.Errorf("remove browser temporary directory: %v", err)
		}
	})
	return directory
}

func removeBrowserTempDir(directory string) error {
	rawUID := os.Getenv("CI_BROWSER_NONROOT_UID")
	if rawUID == "" || stdruntime.GOOS != "linux" || os.Geteuid() != 0 {
		return os.RemoveAll(directory)
	}
	uid, err := parseBrowserNonRootUID(rawUID)
	if err != nil {
		return err
	}
	command := exec.CommandContext(context.Background(), "rm", "-rf", "--", directory)
	configureBrowserCommand(command, uid)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("remove as browser uid %d: %w: %s", uid, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func browserRuntimeOptions(t *testing.T, profile string, writableDirectories ...string) []chromedp.ExecAllocatorOption {
	t.Helper()
	rawUID := os.Getenv("CI_BROWSER_NONROOT_UID")
	if rawUID == "" {
		return nil
	}
	if stdruntime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Fatalf("CI_BROWSER_NONROOT_UID requires a Linux root test process")
	}
	uid, err := parseBrowserNonRootUID(rawUID)
	if err != nil {
		t.Fatal(err)
	}
	runtimeDirectory, err := prepareBrowserRuntime(profile, writableDirectories, uid, os.Chown)
	if err != nil {
		t.Fatal(err)
	}
	return []chromedp.ExecAllocatorOption{
		chromedp.Env(
			"HOME="+profile,
			"XDG_CACHE_HOME="+filepath.Join(profile, ".cache"),
			"XDG_CONFIG_HOME="+filepath.Join(profile, ".config"),
			"XDG_RUNTIME_DIR="+runtimeDirectory,
		),
		chromedp.ModifyCmdFunc(func(command *exec.Cmd) {
			configureBrowserCommand(command, uid)
		}),
	}
}

func prepareBrowserRuntime(profile string, writableDirectories []string, uid uint32, chown func(string, int, int) error) (string, error) {
	runtimeDirectory := filepath.Join(profile, "runtime")
	if err := os.Mkdir(runtimeDirectory, 0o700); err != nil {
		return "", fmt.Errorf("create browser runtime directory: %w", err)
	}
	directories := append(append([]string(nil), writableDirectories...), runtimeDirectory, profile)
	for _, directory := range directories {
		if err := chown(directory, int(uid), int(uid)); err != nil {
			return "", fmt.Errorf("assign browser directory %q to uid %d: %w", directory, uid, err)
		}
	}
	return runtimeDirectory, nil
}

func parseBrowserNonRootUID(rawUID string) (uint32, error) {
	parsedUID, err := strconv.ParseUint(rawUID, 10, 32)
	if err != nil || parsedUID == 0 {
		return 0, fmt.Errorf("invalid CI_BROWSER_NONROOT_UID %q", rawUID)
	}
	return uint32(parsedUID), nil
}

func (browserClient *testBrowser) requestSnapshot() []string {
	browserClient.mu.Lock()
	defer browserClient.mu.Unlock()
	return append([]string(nil), browserClient.requests...)
}

func (browserClient *testBrowser) assertNoExternalRequests(t *testing.T, harness harness) {
	t.Helper()
	browserClient.mu.Lock()
	defer browserClient.mu.Unlock()
	for _, origin := range browserClient.origins {
		if origin != harness.origin && origin != harness.dataOrigin && origin != harness.previewOrigin {
			t.Errorf("unexpected browser request origin: %s", origin)
		}
	}
}

func bootstrapBrowser(t *testing.T, client *testBrowser, harness harness) {
	t.Helper()
	if err := chromedp.Run(client.ctx,
		chromedp.Navigate(harness.origin+"/bootstrap"), chromedp.WaitVisible("#registration-view", chromedp.ByQuery),
		bootstrapKeyboardActions(harness.bootstrapToken),
	); err != nil {
		t.Fatalf("bootstrap browser: %v", err)
	}
	if err := waitVisible(client.ctx, "#auth-view", 15*time.Second); err != nil {
		t.Fatalf("finish browser bootstrap: %v (%s) requests=%v", err, browserStatus(client.ctx), client.requestSnapshot())
	}
	if err := chromedp.Run(client.ctx, chromedp.Focus("#signin-button", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatalf("sign in bootstrap administrator: %v", err)
	}
	if err := waitVisible(client.ctx, "#drive-view", 15*time.Second); err != nil {
		t.Fatalf("finish administrator sign-in: %v (%s)", err, browserStatus(client.ctx))
	}
}

func bootstrapKeyboardActions(token string) chromedp.Tasks {
	return chromedp.Tasks{
		chromedp.Focus("#display-name", chromedp.ByQuery),
		chromedp.KeyEvent("First Administrator"),
		chromedp.Focus("#bootstrap-token", chromedp.ByQuery),
		chromedp.KeyEvent(token),
		chromedp.Focus("#registration-form button[type='submit']", chromedp.ByQuery),
		chromedp.KeyEvent(kb.Enter),
	}
}

func addVirtualAuthenticator(ctx context.Context) (cdpwebauthn.AuthenticatorID, error) {
	authenticatorID, err := cdpwebauthn.AddVirtualAuthenticator(virtualAuthenticatorOptions()).Do(ctx)
	if err != nil {
		return "", err
	}
	if err := cdpwebauthn.SetAutomaticPresenceSimulation(authenticatorID, true).Do(ctx); err != nil {
		return "", err
	}
	if err := cdpwebauthn.SetUserVerified(authenticatorID, true).Do(ctx); err != nil {
		return "", err
	}
	return authenticatorID, nil
}

func virtualAuthenticatorOptions() *cdpwebauthn.VirtualAuthenticatorOptions {
	return &cdpwebauthn.VirtualAuthenticatorOptions{
		Protocol: cdpwebauthn.AuthenticatorProtocolCtap2, Ctap2version: cdpwebauthn.Ctap2versionCtap20,
		Transport: cdpwebauthn.AuthenticatorTransportInternal, HasResidentKey: true,
		HasUserVerification: true, AutomaticPresenceSimulation: true, IsUserVerified: true,
	}
}

func browserStatus(ctx context.Context) string {
	statusContext, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var urgent string
	var locationValue string
	var documentState string
	_ = chromedp.Run(statusContext,
		chromedp.Text("#urgent-status", &urgent, chromedp.ByQuery),
		chromedp.Location(&locationValue),
		chromedp.Evaluate(`document.readyState + " loading=" + document.querySelector("#loading-view").hidden + " auth=" + document.querySelector("#auth-view").hidden + " registration=" + document.querySelector("#registration-view").hidden + " dialogOpen=" + document.querySelector("#action-dialog").open + " dialogDisplay=" + getComputedStyle(document.querySelector("#action-dialog")).display`, &documentState),
	)
	return "url=" + locationValue + " state=" + documentState + " alert=" + urgent
}

func waitVisible(ctx context.Context, selector string, timeout time.Duration) error {
	return runStage(ctx, timeout, chromedp.WaitVisible(selector, chromedp.ByQuery))
}

func waitFor(ctx context.Context, expression string, timeout time.Duration) error {
	var result bool
	return runStage(ctx, timeout, chromedp.Poll(expression, &result, chromedp.WithPollingMutation(), chromedp.WithPollingTimeout(timeout)))
}

func runStage(ctx context.Context, timeout time.Duration, actions ...chromedp.Action) error {
	stageContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return chromedp.Run(stageContext, actions...)
}

type harness struct {
	origin         string
	dataOrigin     string
	previewOrigin  string
	bootstrapToken string
	repository     *identity.Repository
	storage        *providermemory.Provider
	previewStore   *previewmemory.Store
	corruptPreview *atomic.Bool
	clock          domain.Clock
}

func newHarness(t *testing.T) harness {
	return newHarnessWithPreviews(t, true)
}

func newHarnessWithPreviews(t *testing.T, withPreviews bool) harness {
	t.Helper()
	controlListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dataListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		controlListener.Close()
		t.Fatal(err)
	}
	previewListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		controlListener.Close()
		dataListener.Close()
		t.Fatal(err)
	}
	_, controlPort, err := net.SplitHostPort(controlListener.Addr().String())
	if err != nil {
		controlListener.Close()
		dataListener.Close()
		previewListener.Close()
		t.Fatal(err)
	}
	origin := "http://localhost:" + controlPort
	dataOrigin := "http://" + dataListener.Addr().String()
	previewOrigin := "http://" + previewListener.Addr().String()
	store := state.NewMemoryStore()
	repository := identity.NewRepository(store)
	ids := domain.SystemIDGenerator()
	clock := domain.SystemClock{}
	bootstrapToken := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	sessionKeyBytes := make([]byte, 32)
	for index := range sessionKeyBytes {
		sessionKeyBytes[index] = byte(index + 1)
	}
	sessionKey := secret.Value(base64.RawURLEncoding.EncodeToString(sessionKeyBytes))
	webAuthn, err := auth.NewGoWebAuthn("localhost", "EndlessFS browser test", origin)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := auth.NewSessionManager(repository, ids, clock, 12*time.Hour, origin, false, sessionKey)
	if err != nil {
		t.Fatal(err)
	}
	identityService, err := identity.NewService(repository, webAuthn, sessions, ids, clock, identity.NewMutablePolicy(identity.RegistrationPolicy{AllowPublic: true, AllowInvite: true}), secret.Value(bootstrapToken), origin)
	if err != nil {
		t.Fatal(err)
	}
	storage := providermemory.New(providermemory.Options{Clock: clock, IDs: ids, AllowedOrigin: origin})
	if err := storage.SetDataPlaneBaseURL(dataOrigin); err != nil {
		t.Fatal(err)
	}
	previewStore, err := previewmemory.New(previewmemory.Options{Clock: clock, IDs: ids, Key: sessionKey, AllowedOrigin: origin})
	if err != nil {
		t.Fatal(err)
	}
	if err := previewStore.SetDataPlaneBaseURL(previewOrigin); err != nil {
		t.Fatal(err)
	}
	corruptPreview := &atomic.Bool{}
	driveService, err := drive.NewService(storage, store, repository, ids, clock, sessionKey, origin, dataOrigin, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	themeRegistry, err := theme.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	themeManager, err := theme.NewManager(themeRegistry, store, "endlessfs-light", "endlessfs-dark", false, clock)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{BaseURL: origin, AllowedOrigin: origin, Secure: false, AllowRegistration: true, InviteRegistration: true, PreviewProvider: "disabled", PreviewFormats: []string{"image"}, PreviewResolutions: []int{256, 512, 1600}, PreviewMaxConcurrency: 2}
	var controlHandler http.Handler = httpapi.NewCompleteApplication(cfg, "e2e", identityService, sessions, driveService, themeManager)
	if withPreviews {
		cfg.PreviewProvider = "mock"
		cfg.PreviewAutomatic = true
		previewService, serviceErr := preview.NewService(preview.Options{Automatic: true, Resolutions: cfg.PreviewResolutions, MaxConcurrency: cfg.PreviewMaxConcurrency, ApplicationState: store}, storage, previewStore, []preview.Generator{imagegen.New(imagegen.Options{})}, http.DefaultClient, ids, clock)
		if serviceErr != nil {
			t.Fatal(serviceErr)
		}
		controlHandler = httpapi.NewCompleteApplicationWithPreview(cfg, "e2e", identityService, sessions, driveService, previewService, themeManager)
	}
	controlServer := &http.Server{Handler: controlHandler}
	dataServer := &http.Server{Handler: storage}
	previewServer := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		response := httptest.NewRecorder()
		previewStore.ServeHTTP(response, request)
		for name, values := range response.Header() {
			for _, value := range values {
				writer.Header().Add(name, value)
			}
		}
		body := append([]byte(nil), response.Body.Bytes()...)
		if corruptPreview.Load() && response.Code == http.StatusOK && strings.HasPrefix(request.URL.Path, "/cap/preview/") && len(body) > 12 {
			body[len(body)-1] ^= 0xff
		}
		writer.WriteHeader(response.Code)
		_, _ = writer.Write(body)
	})}
	serveErrors := make(chan error, 3)
	go func() { serveErrors <- controlServer.Serve(controlListener) }()
	go func() { serveErrors <- dataServer.Serve(dataListener) }()
	go func() { serveErrors <- previewServer.Serve(previewListener) }()
	t.Cleanup(func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = controlServer.Shutdown(shutdownContext)
		_ = dataServer.Shutdown(shutdownContext)
		_ = previewServer.Shutdown(shutdownContext)
		for range 3 {
			if serveErr := <-serveErrors; serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				t.Errorf("browser test server: %v", serveErr)
			}
		}
	})
	return harness{
		origin: origin, dataOrigin: dataOrigin, previewOrigin: previewOrigin, bootstrapToken: bootstrapToken,
		repository: repository, storage: storage, previewStore: previewStore, corruptPreview: corruptPreview, clock: clock,
	}
}

func seedVirtualFiles(t *testing.T, harness harness, count int) {
	t.Helper()
	accounts, err := harness.repository.Accounts(context.Background())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("resolve browser account for large listing: %v, accounts=%d", err, len(accounts))
	}
	scope, err := domain.NewScope(accounts[0].UserID, domain.AreaLive)
	if err != nil {
		t.Fatal(err)
	}
	for index := range count {
		path := domain.MustParseUserPath(fmt.Sprintf("/virtual-%05d.bin", index))
		capability, err := harness.storage.CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: path, Size: 0, MediaType: "application/octet-stream"})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(capability.Method, capability.URL, nil)
		for name, value := range capability.Headers {
			request.Header.Set(name, value)
		}
		response := httptest.NewRecorder()
		harness.storage.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("seed upload %d status = %d", index, response.Code)
		}
		if _, err := harness.storage.CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: 0, MediaType: "application/octet-stream"}); err != nil {
			t.Fatal(err)
		}
	}
}

func countRequestPath(requests []string, path string) int {
	count := 0
	for _, request := range requests {
		if strings.Contains(request, path) {
			count++
		}
	}
	return count
}

func chromiumPath(t *testing.T) string {
	t.Helper()
	if configured := os.Getenv("ENDLESSFS_CHROMIUM"); configured != "" {
		if _, err := os.Stat(configured); err != nil {
			t.Fatalf("ENDLESSFS_CHROMIUM: %v", err)
		}
		return configured
	}
	if stdruntime.GOOS == "darwin" {
		path := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	t.Fatal("Chromium was not found; use the Nix test-e2e task")
	return ""
}
