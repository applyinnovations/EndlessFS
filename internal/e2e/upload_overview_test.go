package e2e

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

func TestE2ELatestUploadProgressExcludesFailedHistory(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	var failCompletion atomic.Bool
	var blockAdmission atomic.Bool
	admissionGate := make(chan struct{})
	var releaseAdmission sync.Once
	release := func() { releaseAdmission.Do(func() { close(admissionGate) }) }
	t.Cleanup(release)
	failCompletion.Store(true)
	harness := newPortableHarnessWithControlPlaneWrapper(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/uploads/batch/admit" && r.Method == http.MethodPost && blockAdmission.Load() {
				select {
				case <-admissionGate:
				case <-r.Context().Done():
					return
				}
			}
			if r.URL.Path == "/api/v1/uploads/batch/complete" && failCompletion.Load() {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"status":400,"code":"invalid_request","detail":"Injected historical failure"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	client := newTestBrowser(t)
	bootstrapBrowser(t, client, harness)
	upload := func(names ...string) {
		t.Helper()
		paths := make([]string, len(names))
		for index, name := range names {
			paths[index] = filepath.Join(t.TempDir(), name)
			body := name
			if name == "old-failure.bin" {
				body = strings.Repeat("x", 1<<20)
			}
			if err := os.WriteFile(paths[index], []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := chromedp.Run(client.ctx, chromedp.SetUploadFiles("#upload-input", paths, chromedp.ByQuery)); err != nil {
			t.Fatal(err)
		}
		if err := waitVisible(client.ctx, "#action-dialog", 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if err := chromedp.Run(client.ctx, chromedp.SetValue("#upload-strategy", "keep-both", chromedp.ByQuery), chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
			t.Fatal(err)
		}
	}
	wait := func(expression string) {
		t.Helper()
		if err := waitFor(client.ctx, expression, 10*time.Second); err != nil {
			var overview string
			_ = chromedp.Run(client.ctx, chromedp.Evaluate(`document.querySelector('#open-transfers').getAttribute('aria-label')`, &overview))
			var rows string
			_ = chromedp.Run(client.ctx, chromedp.Evaluate(`document.querySelector('#transfer-list').textContent`, &rows))
			t.Fatalf("%s: %v; overview=%s; rows=%s", expression, err, overview, rows)
		}
	}
	upload("old-failure.bin")
	wait(`document.querySelector('[data-transfer-count="failed"]').textContent === '(1)'`)
	wait(`document.querySelector('#header-transfer-eta').textContent === 'Attention'`)
	failCompletion.Store(false)
	blockAdmission.Store(true)
	upload("new-first.txt", "new-second.txt")
	wait(`document.querySelector('#open-transfers').getAttribute('aria-label').includes('active of 2 files') && document.querySelector('[data-transfer-count="failed"]').textContent === '(1)'`)
	release()
	wait(`document.querySelector('[data-transfer-count="complete"]').textContent === '(2)'`)
	wait(`document.querySelector('#header-transfer-percent').textContent === '100%' && document.querySelector('#header-transfer-eta').textContent === 'Complete'`)
	wait(`document.querySelector('#open-transfers').getAttribute('aria-label').includes('0 active of 2 files')`)
	wait(`document.querySelector('[data-transfer-count="failed"]').textContent === '(1)' && document.querySelector('[data-transfer-count="all"]').textContent === '(3)' && !document.querySelector('#clear-failed-transfers').disabled`)
	// The cohort is derived from the existing durable group, not a process-only
	// marker that disappears on reload. Earlier failures remain recoverable.
	if err := chromedp.Run(client.ctx, chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	wait(`document.querySelector('[data-transfer-count="all"]').textContent === '(3)'`)
	wait(`document.querySelector('#header-transfer-percent').textContent === '100%' && document.querySelector('#header-transfer-eta').textContent === 'Complete'`)
	client.assertNoExternalRequests(t, harness)
}

func TestE2ELostCompletionResponseRetainsOrderAcrossReload(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	t.Run("explicit-ordinal", func(t *testing.T) { testLostCompletionReplayOrder(t, false) })
	t.Run("existing-folder-order", func(t *testing.T) { testLostCompletionReplayOrder(t, true) })
}

func testLostCompletionReplayOrder(t *testing.T, omitOrdinal bool) {
	t.Helper()
	var committed atomic.Bool
	var calls atomic.Int64
	harness := newPortableHarnessWithControlPlaneWrapper(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/uploads/batch/complete" || calls.Add(1) != 1 {
				next.ServeHTTP(w, r)
				return
			}
			recorded := httptest.NewRecorder()
			next.ServeHTTP(recorded, r)
			if recorded.Code != http.StatusOK {
				t.Errorf("first completion returned %d", recorded.Code)
			}
			committed.Store(true)
			// Server publication succeeds, but the response is lost when the
			// browser reloads. Replay must preserve the original ordered intent.
			<-r.Context().Done()
		})
	})
	client := newTestBrowser(t)
	bootstrapBrowser(t, client, harness)
	// IndexedDB key order is deliberately the reverse of discovery order.
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(`(() => { let sequence = 999; crypto.randomUUID = () => '00000000-0000-4000-8000-' + String(sequence--).padStart(12, '0'); })()`, nil)); err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(t.TempDir(), "first.txt"), filepath.Join(t.TempDir(), "second.txt")}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte(path), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := chromedp.Run(client.ctx, chromedp.SetUploadFiles("#upload-input", paths, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(client.ctx, "#action-dialog", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, chromedp.SetValue("#upload-strategy", "keep-both", chromedp.ByQuery), chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for !committed.Load() {
		select {
		case <-deadline.C:
			t.Fatal("server did not publish the initial completion")
		case <-ticker.C:
		}
	}
	if omitOrdinal {
		// Match the pre-ordinal browser ledger: folder.transferIDs is already
		// persisted in discovery order; only the new optional ordinal is absent.
		if err := chromedp.Run(client.ctx, chromedp.Evaluate(`(async () => {
			const database = await new Promise((resolve, reject) => { const request = indexedDB.open('endlessfs-transfer-ledger-v1'); request.onsuccess = () => resolve(request.result); request.onerror = () => reject(request.error); });
			const transaction = database.transaction('items', 'readwrite');
			const store = transaction.objectStore('items');
			const request = store.openCursor();
			request.onsuccess = () => { const cursor = request.result; if (!cursor) return; const item = cursor.value; delete item.completionBatchIndex; cursor.update(item); cursor.continue(); };
			await new Promise((resolve, reject) => { transaction.oncomplete = resolve; transaction.onerror = () => reject(transaction.error); });
			database.close(); return true;
		})()`, nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
			t.Fatal(err)
		}
	}
	if err := chromedp.Run(client.ctx, chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent === '(2)' && document.querySelector('[data-transfer-count="failed"]').textContent === '(0)'`, 10*time.Second); err != nil {
		var rows string
		_ = chromedp.Run(client.ctx, chromedp.Evaluate(`document.querySelector('#transfer-list').textContent`, &rows))
		t.Fatalf("ordered completion replay failed: %v; rows=%s", err, rows)
	}
	if got := countExactRequest(client.requestSnapshot(), "POST /api/v1/uploads/batch/admit"); got != 1 {
		t.Fatalf("completion replay allocated new sessions: %d admissions", got)
	}
	client.assertNoExternalRequests(t, harness)
}
