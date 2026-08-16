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
			authenticatorID, addErr = cdpwebauthn.AddVirtualAuthenticator(&cdpwebauthn.VirtualAuthenticatorOptions{
				Protocol: cdpwebauthn.AuthenticatorProtocolCtap2, Ctap2version: cdpwebauthn.Ctap2versionCtap21,
				Transport: cdpwebauthn.AuthenticatorTransportInternal, HasResidentKey: true,
				HasUserVerification: true, AutomaticPresenceSimulation: true, IsUserVerified: true,
			}).Do(actionContext)
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
	if err := chromedp.Run(ctx, chromedp.SendKeys("#display-name", "First Administrator"+kb.Tab+harness.bootstrapToken+kb.Tab+kb.Enter, chromedp.ByQuery)); err != nil {
		t.Fatalf("submit keyboard bootstrap: %v", err)
	}
	if err := waitVisible(ctx, "#auth-view", 15*time.Second); err != nil {
		t.Fatalf("keyboard bootstrap: %v (%s)", err, browserStatus(ctx))
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
