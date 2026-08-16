package e2e

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	stdruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/config"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/drive"
	"github.com/applyinnovations/endlessfs/internal/httpapi"
	"github.com/applyinnovations/endlessfs/internal/identity"
	providermemory "github.com/applyinnovations/endlessfs/internal/provider/memory"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/theme"
	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	cdpwebauthn "github.com/chromedp/cdproto/webauthn"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

func TestE2EBrowserBootstrapLoginDriveShareAndTrash(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("set ENDLESSFS_RUN_E2E=1; the Nix test-e2e task does this")
	}
	browserPath := chromiumPath(t)
	harness := newHarness(t)

	profile := t.TempDir()
	downloads := t.TempDir()
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
		options = append(options, chromedp.NoSandbox)
	}
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
	if err := chromedp.Run(ctx, network.Enable(), runtime.Enable(), chromedp.Navigate(harness.origin+"/bootstrap")); err != nil {
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

	mu.Lock()
	defer mu.Unlock()
	if len(browserFailures) != 0 {
		t.Fatalf("browser exceptions: %v", browserFailures)
	}
	for _, requestedOrigin := range requestedOrigins {
		if requestedOrigin != harness.origin && requestedOrigin != harness.dataOrigin {
			t.Errorf("unexpected browser request origin: %s", requestedOrigin)
		}
	}
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

	admin.assertNoExternalRequests(t, harness)
	member.assertNoExternalRequests(t, harness)
}

type testBrowser struct {
	ctx      context.Context
	mu       sync.Mutex
	origins  []string
	requests []string
}

func newTestBrowser(t *testing.T) *testBrowser {
	t.Helper()
	profile := t.TempDir()
	options := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromiumPath(t)), chromedp.UserDataDir(profile),
		chromedp.Flag("disable-background-networking", true), chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-sync", true), chromedp.Flag("no-first-run", true), chromedp.Flag("no-default-browser-check", true),
	)
	if os.Getenv("ENDLESSFS_CHROMIUM_NO_SANDBOX") == "1" {
		options = append(options, chromedp.NoSandbox)
	}
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
		if origin != harness.origin && origin != harness.dataOrigin {
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
	authenticatorID, err := cdpwebauthn.AddVirtualAuthenticator(&cdpwebauthn.VirtualAuthenticatorOptions{
		Protocol: cdpwebauthn.AuthenticatorProtocolCtap2, Ctap2version: cdpwebauthn.Ctap2versionCtap20,
		Transport: cdpwebauthn.AuthenticatorTransportUsb, HasResidentKey: true,
		HasUserVerification: true, AutomaticPresenceSimulation: true, IsUserVerified: true,
	}).Do(ctx)
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
	bootstrapToken string
}

func newHarness(t *testing.T) harness {
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
	_, controlPort, err := net.SplitHostPort(controlListener.Addr().String())
	if err != nil {
		controlListener.Close()
		dataListener.Close()
		t.Fatal(err)
	}
	origin := "http://localhost:" + controlPort
	dataOrigin := "http://" + dataListener.Addr().String()
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
	cfg := config.Config{BaseURL: origin, AllowedOrigin: origin, Secure: false, AllowRegistration: true, InviteRegistration: true}
	controlServer := &http.Server{Handler: httpapi.NewCompleteApplication(cfg, "e2e", identityService, sessions, driveService, themeManager)}
	dataServer := &http.Server{Handler: storage}
	serveErrors := make(chan error, 2)
	go func() { serveErrors <- controlServer.Serve(controlListener) }()
	go func() { serveErrors <- dataServer.Serve(dataListener) }()
	t.Cleanup(func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = controlServer.Shutdown(shutdownContext)
		_ = dataServer.Shutdown(shutdownContext)
		for range 2 {
			if serveErr := <-serveErrors; serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				t.Errorf("browser test server: %v", serveErr)
			}
		}
	})
	return harness{origin: origin, dataOrigin: dataOrigin, bootstrapToken: bootstrapToken}
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
