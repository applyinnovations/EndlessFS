package e2e

import (
	"context"
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

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

func TestE2EClearFailedUploadsCleansSessionsAndPreservesCompletedFiles(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	var failCompletion, failCleanup atomic.Bool
	var cleanups atomic.Int64
	harness := newPortableHarnessWithControlPlaneWrapper(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/uploads/batch" && r.Method == http.MethodDelete {
				cleanups.Add(1)
				if failCleanup.Load() {
					w.Header().Set("Content-Type", "application/problem+json")
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"status":403,"code":"forbidden","detail":"Cleanup denied"}`))
					return
				}
			}
			if r.URL.Path == "/api/v1/uploads/batch/complete" && failCompletion.Load() {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"status":400,"code":"invalid_request","detail":"Injected terminal completion failure"}`))
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
			if err := os.WriteFile(paths[index], []byte(name), 0600); err != nil {
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
			var diagnostic string
			_ = chromedp.Run(client.ctx, chromedp.Evaluate(`document.querySelector('#transfer-panel').textContent`, &diagnostic))
			t.Fatalf("%s: %v (%s) panel=%s requests=%v", expression, err, browserStatus(client.ctx), diagnostic, client.requestSnapshot())
		}
	}
	click := func(selector string) {
		t.Helper()
		if err := chromedp.Run(client.ctx, chromedp.Click(selector, chromedp.ByQuery)); err != nil {
			t.Fatal(err)
		}
	}
	upload("preserved.txt")
	wait(`document.querySelector('[data-transfer-count="complete"]').textContent === '(1)'`)
	failCompletion.Store(true)
	upload("failed.txt")
	wait(`document.querySelector('[data-transfer-count="failed"]').textContent === '(1)'`)
	click(`[data-tab-value="failed"]`)
	wait(`document.querySelector('[aria-label="Clear failed upload failed.txt"]') !== null`)
	failCleanup.Store(true)
	click(`[aria-label="Clear failed upload failed.txt"]`)
	wait(`document.querySelector('#toast-region').textContent.includes('could not be cleared')`)
	if cleanups.Load() != 1 {
		t.Fatalf("cleanup attempts = %d", cleanups.Load())
	}
	wait(`document.querySelector('[data-transfer-count="failed"]').textContent === '(1)'`)
	failCleanup.Store(false)
	click(`[aria-label="Clear failed upload failed.txt"]`)
	wait(`document.querySelector('[data-transfer-count="failed"]').textContent === '(0)'`)
	upload("folder-first.txt", "folder-second.txt")
	wait(`document.querySelector('[data-transfer-count="failed"]').textContent === '(2)'`)
	wait(`document.querySelector('.transfer-group-row [aria-label^="Clear failed uploads in folder "]') !== null`)
	click(`.transfer-group-row [aria-label^="Clear failed uploads in folder "]`)
	wait(`document.querySelector('[data-transfer-count="failed"]').textContent === '(0)'`)
	upload("bulk-first.txt", "bulk-second.txt")
	wait(`document.querySelector('[data-transfer-count="failed"]').textContent === '(2)'`)
	click(`#clear-failed-transfers`)
	wait(`document.querySelector('[data-transfer-count="all"]').textContent === '(1)'`)
	// A just-completed retained row may still have a pending local ledger
	// write; allow its idempotent completion replay through the real server.
	failCompletion.Store(false)
	if err := chromedp.Run(client.ctx, chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	wait(`document.querySelector('[data-transfer-count="all"]').textContent === '(1)' && document.querySelector('#file-rows').textContent.includes('preserved.txt')`)
	wait(`document.querySelector('[data-transfer-count="failed"]').textContent === '(0)'`)
	if cleanups.Load() < 4 {
		t.Fatalf("unfinished sessions were not cleaned up: %d requests", cleanups.Load())
	}
	client.assertNoExternalRequests(t, harness)
}

func TestE2EClearFailedHistoryPreservesMixedGroupsAndOtherOwners(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	harness := newPortableHarnessWithControlPlaneWrapper(t, nil)
	client := newTestBrowser(t)
	bootstrapBrowser(t, client, harness)
	accounts, err := harness.repository.Accounts(context.Background())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts = %d, %v", len(accounts), err)
	}
	script := fmt.Sprintf(`(async () => {
		const ownerID = %q;
		const database = await new Promise((resolve, reject) => {
			const request = indexedDB.open("endlessfs-transfer-ledger-v1");
			request.onsuccess = () => resolve(request.result); request.onerror = () => reject(request.error);
		});
		const transaction = database.transaction(["items", "groups", "sources"], "readwrite");
		for (const [id, state, groupID, owner] of [
			["mixed-failed", "failed", "mixed", ownerID], ["mixed-complete", "complete", "mixed", ownerID],
			["mixed-waiting", "needs-source", "mixed", ownerID], ["standalone-failed", "failed", "", ownerID],
			["mixed-failed", "failed", "", "another-owner"],
		]) {
			transaction.objectStore("items").put({key: owner + ":" + id, ownerID: owner, id, groupID,
				name: id + ".bin", relativePath: id + ".bin", relativeDirectory: "", directory: "/", baseDirectory: "/",
				size: 1, mediaType: "application/octet-stream", state, confirmed: state === "complete" ? 1 : 0,
				uploadID: "", strategy: "keep-both", planPhase: "done", createdAt: 1});
		}
		transaction.objectStore("groups").put({key: ownerID + ":mixed", ownerID, id: "mixed", name: "Mixed folder",
			baseDirectory: "/", transferIDs: ["mixed-failed", "mixed-complete", "mixed-waiting"], directories: [],
			totalSize: 3, state: "queued", error: "Old failure", discoveryDone: true, strategy: "keep-both"});
		for (const owner of [ownerID, "another-owner"]) transaction.objectStore("sources").put({
			key: owner + ":mixed-failed", ownerID: owner, handle: {marker: "test-source"}});
		await new Promise((resolve, reject) => {transaction.oncomplete = resolve; transaction.onerror = () => reject(transaction.error);});
		database.close(); return true;
	})()`, accounts[0].UserID.String())
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(script, nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }), chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="all"]').textContent === '(4)' && document.querySelector('[data-transfer-count="failed"]').textContent === '(2)'`, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, chromedp.Click("#open-transfers", chromedp.ByQuery), chromedp.Click(`[data-tab-value="all"]`, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[aria-label="Reconnect upload source for standalone-failed.bin"]') !== null`, 5*time.Second); err != nil {
		t.Fatalf("persisted failures lost their source-reconnection action: %v", err)
	}
	if err := chromedp.Run(client.ctx, chromedp.Click(`[aria-label="Clear failed uploads in folder Mixed folder"]`, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="all"]').textContent === '(3)' && document.querySelector('.transfer-group-row').textContent.includes('1 of 2 files')`, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, chromedp.Click("#clear-failed-transfers", chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="all"]').textContent === '(2)'`, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="all"]').textContent === '(2)' && document.querySelector('[data-transfer-count="failed"]').textContent === '(0)' && document.querySelector('[data-transfer-count="current"]').textContent === '(1)'`, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	var preserved bool
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(`(async () => {
		const database = await new Promise(resolve => {const r = indexedDB.open("endlessfs-transfer-ledger-v1"); r.onsuccess = () => resolve(r.result);});
		const transaction = database.transaction(["items", "sources"], "readonly");
		const read = store => new Promise(resolve => {const r = transaction.objectStore(store).get("another-owner:mixed-failed"); r.onsuccess = () => resolve(Boolean(r.result));});
		return (await Promise.all([read("items"), read("sources")])).every(Boolean);
	})()`, &preserved, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil || !preserved {
		t.Fatalf("other owner's history changed: %v, %v", preserved, err)
	}
	if got := countExactRequest(client.requestSnapshot(), "DELETE /api/v1/uploads/batch"); got != 0 {
		t.Fatalf("unallocated failures triggered %d provider cleanup requests", got)
	}
}

func TestE2EClearFailedUploadAfterLostCompletionPreservesPublishedFile(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	harness := newPortableHarnessWithControlPlaneWrapper(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/uploads/batch/complete" {
				next.ServeHTTP(w, r)
				return
			}
			recorder := httptest.NewRecorder()
			next.ServeHTTP(recorder, r)
			if recorder.Code != http.StatusOK {
				t.Errorf("real completion = %d", recorder.Code)
			}
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"status":400,"code":"invalid_request"}`))
		})
	})
	client := newTestBrowser(t)
	bootstrapBrowser(t, client, harness)
	path := filepath.Join(t.TempDir(), "published.txt")
	if err := os.WriteFile(path, []byte("preserved"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, chromedp.SetUploadFiles("#upload-input", []string{path}, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(client.ctx, "#action-dialog", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, chromedp.SetValue("#upload-strategy", "keep-both", chromedp.ByQuery), chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="failed"]').textContent === '(1)'`, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, chromedp.Click("#clear-failed-transfers", chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="all"]').textContent === '(0)'`, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	accounts, err := harness.repository.Accounts(context.Background())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts: %v", err)
	}
	scope, err := domain.NewScope(accounts[0].UserID, domain.AreaLive)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := harness.files.Stat(context.Background(), scope, domain.MustParseUserPath("/published.txt"))
	if err != nil || entry.Size != 9 {
		t.Fatalf("published file changed by history cleanup: size=%d, %v", entry.Size, err)
	}
	lookups := 0
	for _, request := range client.requestSnapshot() {
		if strings.HasPrefix(request, "GET /api/v1/uploads/") {
			lookups++
		}
	}
	if lookups != 1 || countExactRequest(client.requestSnapshot(), "DELETE /api/v1/uploads/batch") != 1 {
		t.Fatalf("terminal verification: lookups=%d, requests=%v", lookups, client.requestSnapshot())
	}
}

func TestE2ERetryFailedFinalizationAfterReloadDoesNotRequireSource(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	var completions atomic.Int64
	harness := newPortableHarnessWithControlPlaneWrapper(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/uploads/batch/complete" && r.Method == http.MethodPost && completions.Add(1) == 1 {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"status":400,"code":"invalid_request"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	client := newTestBrowser(t)
	bootstrapBrowser(t, client, harness)
	path := filepath.Join(t.TempDir(), "retry-finalization.txt")
	if err := os.WriteFile(path, []byte("body"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, chromedp.SetUploadFiles("#upload-input", []string{path}, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(client.ctx, "#action-dialog", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, chromedp.SetValue("#upload-strategy", "keep-both", chromedp.ByQuery), chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="failed"]').textContent === '(1)'`, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	// Model reload after acknowledgement of the failed ledger state. Admission
	// and finalization already persisted the real upload ID, checksum, and key.
	// Remove any source handle so successful retry must reuse uploaded data.
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(`(async () => {
		const database = await new Promise(resolve => {const r = indexedDB.open("endlessfs-transfer-ledger-v1"); r.onsuccess = () => resolve(r.result);});
		const transaction = database.transaction(["items", "sources"], "readwrite");
		const request = transaction.objectStore("items").getAll();
		request.onsuccess = () => {for (const record of request.result) {
			if (record.name !== "retry-finalization.txt") continue;
			record.state = "failed"; transaction.objectStore("items").put(record); transaction.objectStore("sources").delete(record.key);
		}};
		await new Promise((resolve, reject) => {transaction.oncomplete = resolve; transaction.onerror = () => reject(transaction.error);});
		return true;
	})()`, nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }), chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="failed"]').textContent === '(1)' && document.querySelector('[aria-label="Retry upload retry-finalization.txt"]') !== null`, 10*time.Second); err != nil {
		t.Fatalf("failed finalization lost explicit retry: %v", err)
	}
	if completions.Load() != 1 {
		t.Fatalf("terminal failure retried automatically on reload: %d", completions.Load())
	}
	if err := chromedp.Run(client.ctx, chromedp.Click("#open-transfers", chromedp.ByQuery), chromedp.Click(`[aria-label="Retry upload retry-finalization.txt"]`, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent === '(1)'`, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if completions.Load() != 2 {
		t.Fatalf("explicit retry completions = %d", completions.Load())
	}
	if got := countExactRequest(client.requestSnapshot(), "POST /api/v1/uploads"); got != 1 {
		t.Fatalf("finalization retry initialized %d upload sessions", got)
	}
}
