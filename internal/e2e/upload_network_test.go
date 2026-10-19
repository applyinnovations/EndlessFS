package e2e

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

func TestLoopbackConnectivityStateOverrideRemainsActive(t *testing.T) {
	for _, offline := range []bool{false, true} {
		state := loopbackNetworkState(offline)
		if state.Offline != offline || state.Latency <= 0 || state.DownloadThroughput != -1 || state.UploadThroughput != -1 {
			t.Fatal("connectivity signal must remain explicitly overridden without a transport shaping rule")
		}
	}
}

// This is a native browser connectivity-signal test. Loopback HTTP continues
// to work; no transport outage or external-network interoperability is claimed.
func TestE2EUploadLoopbackConnectivitySignalPausesAndResumesSmartPlanning(t *testing.T) {
	parallelE2EFixture(t)
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	body := []byte("loopback connectivity upload")
	var writes atomic.Int64
	h := newSmartRecoveryHarness(t, nil, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPatch || r.Method == http.MethodPut {
				writes.Add(1)
				if r.ContentLength != int64(len(body)) {
					t.Errorf("synthetic upload body length = %d, want %d", r.ContentLength, len(body))
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	client := newSmartRecoveryBrowser(t)
	bootstrapSmartRecovery(t, client, h.harness)
	if err := waitFor(client.ctx, `navigator.onLine===true`, 5*time.Second); err != nil {
		t.Fatal("loopback browser did not declare online connectivity")
	}
	if err := chromedp.Run(client.ctx, loopbackNetworkState(true), chromedp.Evaluate(`window.__loopbackOnlineEvents=0;window.addEventListener('online',()=>window.__loopbackOnlineEvents++);`, nil)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `navigator.onLine===false`, 5*time.Second); err != nil {
		t.Fatal("native offline signal did not take effect")
	}
	root := filepath.Join(t.TempDir(), "network-folder")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source.bin"), body, 0600); err != nil {
		t.Fatal(err)
	}
	requestStart := len(client.requestSnapshot())
	if err := chromedp.Run(client.ctx, chromedp.SetUploadFiles("#folder-input", []string{root}, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(client.ctx, "#action-dialog", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, chromedp.SetValue("#upload-strategy", "smart-merge", chromedp.ByQuery), chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="all"]').textContent==='(1)'&&document.querySelector('[data-transfer-count="complete"]').textContent==='(0)'`, 5*time.Second); err != nil {
		t.Fatal("selected smart upload was not retained while offline")
	}
	for _, request := range client.requestSnapshot()[requestStart:] {
		if strings.HasPrefix(request, "POST /api/v1/uploads") || strings.HasPrefix(request, "PATCH ") || strings.HasPrefix(request, "PUT ") {
			t.Fatal("offline connectivity signal scheduled an upload request")
		}
	}
	if writes.Load() != 0 {
		t.Fatal("offline connectivity signal transferred bytes")
	}
	if err := chromedp.Run(client.ctx, loopbackNetworkState(false)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `navigator.onLine===true&&window.__loopbackOnlineEvents===1&&document.querySelector('[data-transfer-count="complete"]').textContent==='(1)'`, 15*time.Second); err != nil {
		t.Fatal("native online event did not resume smart planning and completion")
	}
	assertSmartRecoveryLedger(t, client, 1, 1)
	scope := smartRecoveryScope(t, h)
	path := domain.MustParseUserPath("/network-folder/source.bin")
	entry, err := h.engines[1].Files().Stat(client.ctx, scope, path)
	if err != nil || entry.Size != int64(len(body)) {
		t.Fatal("completed upload lacks authoritative file metadata")
	}
	capability, err := h.engines[1].Files().CreateDownload(client.ctx, scope, domain.CreateDownloadRequest{Path: path, Version: entry.Version})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	h.fileBase.ServeHTTP(recorder, httptest.NewRequest(capability.Method, capability.URL, nil))
	if recorder.Code != http.StatusOK || !bytes.Equal(recorder.Body.Bytes(), body) {
		t.Fatal("completed synthetic upload body differs")
	}
	var previousTimeOrigin float64
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(`performance.timeOrigin`, &previousTimeOrigin), reloadLoopbackBrowser()); err != nil {
		t.Fatal(err)
	}
	restored := fmt.Sprintf(`performance.timeOrigin>%.6f&&document.readyState==='complete'&&navigator.onLine===true&&document.querySelector('#loading-view').hidden&&!document.querySelector('#authenticated-view').hidden&&document.querySelector('#app').dataset.state==='authenticated'&&document.querySelector('[data-transfer-count="complete"]').textContent==='(1)'`, previousTimeOrigin)
	if err := waitFor(client.ctx, restored, 15*time.Second); err != nil {
		t.Fatal("completed upload did not survive reload")
	}
	assertSmartRecoveryLedger(t, client, 1, 1)
	requests := client.requestSnapshot()[requestStart:]
	for _, route := range []string{"POST /api/v1/uploads/plan/sizes", "POST /api/v1/uploads/batch/admit", "POST /api/v1/uploads/batch/actions"} {
		if got := countExactRequest(requests, route); got != 1 {
			t.Errorf("%s count = %d, want 1", route, got)
		}
	}
	if writes.Load() != 1 {
		t.Fatalf("actual byte transfers = %d, want exactly 1", writes.Load())
	}
	patches, puts := 0, 0
	for _, request := range requests {
		if strings.HasPrefix(request, "PATCH ") {
			patches++
		}
		if strings.HasPrefix(request, "PUT ") {
			puts++
		}
	}
	if patches != 1 || puts != 0 {
		t.Fatalf("data requests: PATCH=%d PUT=%d, want 1/0", patches, puts)
	}
	assertSmartRecoveryBulkRoutes(t, requests)
	h.assertProviderMetadataOnly(t)
	client.assertNoExternalRequests(t, h.harness)
}
