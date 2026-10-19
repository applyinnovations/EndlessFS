package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

func readUploadCompletionActionPage(t *testing.T, r *http.Request) []domain.UploadActionItem {
	t.Helper()
	encoded, err := io.ReadAll(r.Body)
	if err != nil {
		t.Error(err)
		return nil
	}
	r.Body = io.NopCloser(bytes.NewReader(encoded))
	r.ContentLength = int64(len(encoded))
	var body struct {
		Items []domain.UploadActionItem `json:"items"`
	}
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Error(err)
		return nil
	}
	if len(body.Items) < 1 || len(body.Items) > 100 {
		t.Errorf("unbounded terminal action page: %d", len(body.Items))
	}
	for _, item := range body.Items {
		if item.Action != "complete" || item.UploadID == "" || item.CRC32C == "" {
			t.Errorf("incomplete completion action fixture: %+v", item)
		}
	}
	return body.Items
}

func TestE2EUploadAdmissionIgnoresReplyForReplacedAttempt(t *testing.T) {
	parallelE2EFixture(t)
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var admissions atomic.Int64
	var mu sync.Mutex
	var capabilities [][]domain.UploadCapability
	h := newPortableHarnessWithControlPlaneWrapper(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || (r.URL.Path != "/api/v1/uploads/batch" && r.URL.Path != "/api/v1/uploads/batch/admit") {
				next.ServeHTTP(w, r)
				return
			}
			recorder := httptest.NewRecorder()
			next.ServeHTTP(recorder, r)
			var body struct {
				Uploads []struct {
					Capability domain.UploadCapability `json:"capability"`
				} `json:"uploads"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Error(err)
			}
			page := make([]domain.UploadCapability, len(body.Uploads))
			for i, item := range body.Uploads {
				page[i] = item.Capability
			}
			mu.Lock()
			capabilities = append(capabilities, page)
			mu.Unlock()
			if admissions.Add(1) == 1 {
				close(started)
				<-release
			}
			for name, values := range recorder.Header() {
				for _, value := range values {
					w.Header().Add(name, value)
				}
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
		})
	})
	client := newTestBrowser(t)
	bootstrapBrowser(t, client, h)
	paths := make([]string, 3)
	for i := range paths {
		paths[i] = filepath.Join(t.TempDir(), fmt.Sprintf("late-admission-%d.bin", i))
		if err := os.WriteFile(paths[i], []byte("abc"), 0600); err != nil {
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
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("admission was not dispatched")
	}
	if err := waitVisible(client.ctx, `button[aria-label^="Cancel folder upload"]`, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(`document.querySelector('button[aria-label^="Cancel folder upload"]').click()`, nil)); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(`document.querySelector('[data-transfer-count="all"]').click()`, nil)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(client.ctx, `button[aria-label^="Retry folder upload"]`, 5*time.Second); err != nil {
		var content string
		_ = chromedp.Run(client.ctx, chromedp.Evaluate(`document.querySelector("#transfer-panel").textContent`, &content))
		t.Fatalf("retry missing: %v %s", err, content)
	}
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(`document.querySelector('button[aria-label^="Retry folder upload"]').click()`, nil)); err != nil {
		t.Fatal(err)
	}
	unblock()
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(3)'`, 15*time.Second); err != nil {
		t.Fatalf("replacement did not complete: %v %s", err, browserStatus(client.ctx))
	}
	mu.Lock()
	defer mu.Unlock()
	if admissions.Load() != 2 || len(capabilities) != 2 || len(capabilities[0]) != 3 || len(capabilities[1]) != 3 {
		t.Fatalf("replaced attempt reused admission: calls=%d pages=%d", admissions.Load(), len(capabilities))
	}
	oldURLs := make(map[string]bool)
	for _, capability := range capabilities[0] {
		oldURLs[capability.URL] = true
	}
	for _, capability := range capabilities[1] {
		if oldURLs[capability.URL] {
			t.Fatal("replacement reused an old capability")
		}
	}
	writes := 0
	for _, request := range client.requestSnapshot() {
		if !strings.HasPrefix(request, "PATCH /cap/upload/") {
			continue
		}
		writes++
		for oldURL := range oldURLs {
			if strings.Contains(oldURL, strings.TrimPrefix(request, "PATCH ")) {
				t.Fatal("late capability sent replacement source data")
			}
		}
	}
	if writes != 3 {
		t.Fatalf("data writes=%d,want3", writes)
	}
	client.assertNoExternalRequests(t, h)
}

func TestE2EUploadActionsRetryOnlyRejectedFinalizationWithoutResendingData(t *testing.T) {
	parallelE2EFixture(t)
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	var calls, rejectedChecksums, legacyCompletions atomic.Int64
	var mu sync.Mutex
	var pages [][]domain.UploadID
	h := newPortableHarnessWithControlPlaneWrapper(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/uploads/batch/complete" {
				legacyCompletions.Add(1)
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"status":409,"code":"conflict"}`))
				return
			}
			if r.URL.Path == "/api/v1/uploads/batch/actions" {
				var body struct {
					Items []domain.UploadActionItem `json:"items"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if len(body.Items) < 1 || len(body.Items) > 100 {
					t.Errorf("unbounded completion action page: %d", len(body.Items))
					return
				}
				ids := make([]domain.UploadID, len(body.Items))
				for i, item := range body.Items {
					ids[i] = item.UploadID
				}
				mu.Lock()
				pages = append(pages, ids)
				mu.Unlock()
				if calls.Add(1) == 1 {
					body.Items[0].CRC32C = objectstore.FingerprintFor([]byte("wrong")).CRC32C
					rejectedChecksums.Add(1)
				}
				encoded, _ := json.Marshal(body)
				r.Body = io.NopCloser(bytes.NewReader(encoded))
				r.ContentLength = int64(len(encoded))
			}
			next.ServeHTTP(w, r)
		})
	})
	client := newTestBrowser(t)
	bootstrapBrowser(t, client, h)
	paths := make([]string, 3)
	for index := range paths {
		paths[index] = filepath.Join(t.TempDir(), fmt.Sprintf("action-%d.bin", index))
		if err := os.WriteFile(paths[index], []byte("abc"), 0600); err != nil {
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
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(2)' && document.querySelector('[data-transfer-count="failed"]').textContent==='(1)'`, 10*time.Second); err != nil {
		t.Fatalf("rejected item poisoned valid siblings: %v %s", err, browserStatus(client.ctx))
	}
	before := client.requestSnapshot()
	writes := 0
	for _, request := range before {
		if strings.HasPrefix(request, "PATCH /cap/upload/") {
			writes++
		}
	}
	if writes != 3 {
		t.Fatalf("synthetic data transfers=%d,want3", writes)
	}
	if err := chromedp.Run(client.ctx, chromedp.Click("#retry-failed-transfers", chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(3)' && document.querySelector('[data-transfer-count="failed"]').textContent==='(0)'`, 10*time.Second); err != nil {
		t.Fatalf("selective finalization retry failed: %v %s", err, browserStatus(client.ctx))
	}
	afterWrites := 0
	for _, request := range client.requestSnapshot() {
		if strings.HasPrefix(request, "PATCH /cap/upload/") {
			afterWrites++
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if afterWrites != writes || len(pages) != 2 || len(pages[0]) != 3 || len(pages[1]) != 1 || pages[1][0] != pages[0][0] {
		t.Fatalf("retry repeated successful work: writes=%d=>%d pages=%+v", writes, afterWrites, pages)
	}
	if calls.Load() != 2 || rejectedChecksums.Load() != 1 || legacyCompletions.Load() != 0 {
		t.Fatalf("fresh action rejection/retry was not exercised exactly: calls=%d injections=%d legacy=%d", calls.Load(), rejectedChecksums.Load(), legacyCompletions.Load())
	}
	client.assertNoExternalRequests(t, h)
}

func expiredRecoveryPage(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var body struct {
		UploadIDs []string `json:"uploadIDs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Error(err)
		return
	}
	if len(body.UploadIDs) < 1 || len(body.UploadIDs) > 100 {
		t.Errorf("unbounded recovery page: %d", len(body.UploadIDs))
	}
	items := make([]map[string]any, len(body.UploadIDs))
	for index, id := range body.UploadIDs {
		items[index] = map[string]any{"index": index, "uploadID": id, "status": map[string]any{"uploadID": id, "state": "expired", "declaredSize": 1, "confirmedOffset": 0, "expiresAt": "2000-01-01T00:00:00Z"}, "retryable": false, "nextAction": "restart"}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"uploads": items})
}

func TestE2EUploadRecoveryRejectsChangedReconnectedSourceBeforeData(t *testing.T) {
	parallelE2EFixture(t)
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	var writes atomic.Int64
	data := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writes.Add(1); w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(data.Close)
	h := newPortableHarnessWithControlPlaneWrapper(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/uploads/batch/resume" {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"uploads": []any{map[string]any{"index": 0, "uploadID": "old-source", "status": map[string]any{"uploadID": "old-source", "state": "active", "declaredSize": 3, "confirmedOffset": 0, "dataComplete": false}, "capability": map[string]any{"uploadID": "old-source", "url": data.URL, "method": "PUT", "protocol": "whole-object", "expiresAt": "2099-01-01T00:00:00Z"}, "retryable": false, "nextAction": "transfer"}}})
		})
	})
	client := newTestBrowser(t)
	bootstrapBrowser(t, client, h)
	accounts, err := h.repository.Accounts(client.ctx)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts: %v", err)
	}
	file := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(file, []byte("xyz"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	original := objectstore.FingerprintFor([]byte("abc"))
	seed := fmt.Sprintf(`(async()=>{const ownerID=%q;const db=await new Promise((resolve,reject)=>{const r=indexedDB.open('endlessfs-transfer-ledger-v1');r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});const tx=db.transaction(['items'],'readwrite');tx.objectStore('items').put({key:ownerID+':source-mismatch',ownerID,id:'source-mismatch',groupID:'',name:'source.bin',relativePath:'source.bin',directory:'/',baseDirectory:'/',size:3,lastModified:%d,state:'needs-source',confirmed:0,uploadID:'old-source',strategy:'keep-both',planPhase:'done',md5:%q,crc32c:%q,createdAt:1});await new Promise((resolve,reject)=>{tx.oncomplete=resolve;tx.onerror=()=>reject(tx.error)});db.close();return true})()`, accounts[0].UserID.String(), info.ModTime().UnixMilli(), original.MD5, original.CRC32C)
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(seed, nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }), chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="all"]').textContent==='(1)'`, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, chromedp.Click("#open-transfers", chromedp.ByQuery), chromedp.Click(`button[aria-label="Reconnect upload source for source.bin"]`, chromedp.ByQuery), chromedp.SetUploadFiles("#upload-input", []string{file}, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('#transfer-list').textContent.includes('differs from the original')`, 10*time.Second); err != nil {
		t.Fatalf("changed bytes accepted despite identical name/size/mtime: %v %s", err, browserStatus(client.ctx))
	}
	if writes.Load() != 0 {
		t.Fatalf("changed source sent %d requests to old session", writes.Load())
	}
	client.assertNoExternalRequests(t, h)
}

func TestE2EUploadRecoveryRequestBudgetAndBoundedRendering(t *testing.T) {
	parallelE2EFixture(t)
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	var pages atomic.Int64
	h := newHarnessWithStorage(t, false, true, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/uploads/batch/resume" {
				pages.Add(1)
				expiredRecoveryPage(t, w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}, nil)
	client := newTestBrowser(t)
	bootstrapBrowser(t, client, h)
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(`window.__endlessfsUploadWorkerPoolTest.seedRecovery(2638)`, nil)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `window.__endlessfsUploadWorkerPoolTest.snapshot().states.failed===2638`, 20*time.Second); err != nil {
		t.Fatalf("recovery wave did not settle: %v (%s)", err, browserStatus(client.ctx))
	}
	if got := pages.Load(); got != 27 {
		t.Fatalf("2638-item recovery requests=%d,want27", got)
	}
	if err := chromedp.Run(client.ctx, chromedp.Click("#open-transfers", chromedp.ByQuery), chromedp.Click(`.transfer-group-row button[aria-expanded]`, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(`document.querySelectorAll('#transfer-list [role="listitem"]').length`, &rows)); err != nil {
		t.Fatal(err)
	}
	if rows > 200 {
		t.Fatalf("recovery exploded rendered rows: %d", rows)
	}
	for _, request := range client.requestSnapshot() {
		if strings.HasPrefix(request, "GET /api/v1/uploads/") || strings.HasPrefix(request, "POST /api/v1/uploads/old-probe-") {
			t.Fatalf("per-file recovery request: %s", request)
		}
	}
	client.assertNoExternalRequests(t, h)
}

func TestE2EUploadRecoveryCancelledItemCannotBeResurrectedBySharedPage(t *testing.T) {
	parallelE2EFixture(t)
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	var pages atomic.Int64
	h := newHarnessWithStorage(t, false, true, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/uploads/batch/resume" {
				pages.Add(1)
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
				expiredRecoveryPage(t, w, r)
				return
			}
			if r.URL.Path == "/api/v1/uploads/batch" && r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}, nil)
	client := newTestBrowser(t)
	bootstrapBrowser(t, client, h)
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(`window.__endlessfsUploadWorkerPoolTest.seedRecovery(100)`, nil), chromedp.Click("#open-transfers", chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `window.__endlessfsUploadWorkerPoolTest.active()===100`, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, chromedp.Click(`.transfer-group-row button[aria-expanded]`, chromedp.ByQuery), chromedp.Click(`button[aria-label="Cancel upload probe-0.bin"]`, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	release()
	if err := waitFor(client.ctx, `window.__endlessfsUploadWorkerPoolTest.snapshot().states.cancelled===1 && window.__endlessfsUploadWorkerPoolTest.snapshot().states.failed===99 && window.__endlessfsUploadWorkerPoolTest.active()===0`, 10*time.Second); err != nil {
		var status string
		_ = chromedp.Run(client.ctx, chromedp.Evaluate(`JSON.stringify(window.__endlessfsUploadWorkerPoolTest.snapshot())`, &status))
		t.Fatalf("stale recovery page resurrected cancelled task: %v %s", err, status)
	}
	if got := pages.Load(); got != 1 {
		t.Fatalf("cancelled sibling split recovery page: %d", got)
	}
	client.assertNoExternalRequests(t, h)
}
