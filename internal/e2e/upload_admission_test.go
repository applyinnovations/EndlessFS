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
	"github.com/applyinnovations/endlessfs/internal/provider"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

func TestE2EUploadIndependentAdmissionRetriesOnlyFailedItem(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("Nix test-e2e enables Chromium")
	}
	var calls atomic.Int64
	var mu sync.Mutex
	var pages [][]string
	h := newPortableHarnessWithControlPlaneWrapper(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/uploads/batch/admit" {
				var body struct {
					Defaults map[string]any   `json:"defaults"`
					Uploads  []map[string]any `json:"uploads"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				ids := make([]string, len(body.Uploads))
				for i, item := range body.Uploads {
					ids[i], _ = item["idempotencyKey"].(string)
				}
				mu.Lock()
				pages = append(pages, ids)
				mu.Unlock()
				if calls.Add(1) == 1 {
					body.Uploads[1]["size"] = -1
				}
				encoded, _ := json.Marshal(body)
				r.Body = io.NopCloser(bytes.NewReader(encoded))
				r.ContentLength = int64(len(encoded))
			}
			next.ServeHTTP(w, r)
		})
	})
	c := newTestBrowser(t)
	bootstrapBrowser(t, c, h)
	paths := make([]string, 3)
	for i := range paths {
		paths[i] = filepath.Join(t.TempDir(), fmt.Sprintf("partial-admit-%d.bin", i))
		if err := os.WriteFile(paths[i], []byte("abc"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := chromedp.Run(c.ctx, chromedp.SetUploadFiles("#upload-input", paths, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(c.ctx, "#action-dialog", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(c.ctx, chromedp.SetValue("#upload-strategy", "keep-both", chromedp.ByQuery), chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(c.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(2)' && document.querySelector('[data-transfer-count="failed"]').textContent==='(1)'`, 10*time.Second); err != nil {
		t.Fatalf("independent admission was not used or sibling failed: calls=%d %v %s", calls.Load(), err, browserStatus(c.ctx))
	}
	if err := chromedp.Run(c.ctx, chromedp.Click("#retry-failed-transfers", chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(c.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(3)'`, 10*time.Second); err != nil {
		t.Fatalf("failed-item retry: %v %s", err, browserStatus(c.ctx))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(pages) != 2 || len(pages[0]) != 3 || len(pages[1]) != 1 || pages[1][0] != pages[0][1] {
		t.Fatalf("admission retry was not selective or changed key: %+v", pages)
	}
	writes := 0
	for _, request := range c.requestSnapshot() {
		if strings.HasPrefix(request, "PATCH /cap/upload/") {
			writes++
		}
		if request == "POST /api/v1/uploads/batch" {
			t.Fatal("new page used legacy admission")
		}
	}
	if writes != 3 {
		t.Fatalf("accepted siblings retransferred: writes=%d", writes)
	}
	c.assertNoExternalRequests(t, h)
}

func TestE2EUploadIndependentAdmissionReloadsAfterLostReply(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("Nix test-e2e enables Chromium")
	}
	var calls atomic.Int64
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var mu sync.Mutex
	var accepted []map[string]domain.UploadID
	h := newPortableHarnessWithControlPlaneWrapper(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/uploads/batch/admit" {
				next.ServeHTTP(w, r)
				return
			}
			recorder := httptest.NewRecorder()
			next.ServeHTTP(recorder, r)
			var body struct {
				Uploads []domain.UploadAdmissionResult `json:"uploads"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Error(err)
			}
			ids := make(map[string]domain.UploadID, len(body.Uploads))
			for _, item := range body.Uploads {
				ids[item.ID] = item.UploadID
			}
			mu.Lock()
			accepted = append(accepted, ids)
			mu.Unlock()
			if calls.Add(1) == 1 {
				close(started)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			for key, values := range recorder.Header() {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
		})
	})
	c := newTestBrowser(t)
	bootstrapBrowser(t, c, h)
	root := filepath.Join(t.TempDir(), "lost-admission-source")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("source-%d.bin", i)), []byte("abc"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := chromedp.Run(c.ctx, chromedp.SetUploadFiles("#folder-input", []string{root}, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(c.ctx, "#action-dialog", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(c.ctx, chromedp.SetValue("#upload-strategy", "keep-both", chromedp.ByQuery), chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("independent admission was not dispatched")
	}
	if err := chromedp.Run(c.ctx, chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(c.ctx, `document.querySelector('[data-transfer-count="all"]').textContent==='(3)'`, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(c.ctx, chromedp.Click("#open-transfers", chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(c.ctx, `button[aria-label^="Reconnect upload source for"]`, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(c.ctx, chromedp.Click(`button[aria-label^="Reconnect upload source for"]`, chromedp.ByQuery), chromedp.SetUploadFiles("#folder-input", []string{root}, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	unblock()
	if err := waitFor(c.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(3)'`, 15*time.Second); err != nil {
		t.Fatalf("reload/reconnect: %v %s", err, browserStatus(c.ctx))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(accepted) != 2 || len(accepted[0]) != 3 || len(accepted[1]) != 3 {
		t.Fatalf("unexpected admission attempts: %+v", accepted)
	}
	for key, id := range accepted[0] {
		if id == "" || accepted[1][key] != id {
			t.Fatalf("lost response allocated another attempt: %+v", accepted)
		}
	}
	writes := 0
	for _, request := range c.requestSnapshot() {
		if strings.HasPrefix(request, "PATCH /cap/upload/") {
			writes++
		}
	}
	if writes != 3 {
		t.Fatalf("data writes=%d want3", writes)
	}
	c.assertNoExternalRequests(t, h)
}

func TestE2EUploadIndependentAdmissionRetriesRollingRouteGap(t *testing.T) {
	runAdmissionTransientReply(t, http.StatusMethodNotAllowed, 3)
}

func TestE2EUploadIndependentAdmissionCoalescesPageRetries(t *testing.T) {
	runAdmissionTransientReply(t, http.StatusServiceUnavailable, 100)
}

func runAdmissionTransientReply(t *testing.T, status, count int) {
	t.Helper()
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("Nix test-e2e enables Chromium")
	}
	var calls atomic.Int64
	var mu sync.Mutex
	var keys [][]string
	h := newPortableHarnessWithControlPlaneWrapper(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/uploads/batch/admit" {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(raw))
				var body struct {
					Uploads []struct {
						Key string `json:"idempotencyKey"`
					} `json:"uploads"`
				}
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Error(err)
				}
				page := make([]string, len(body.Uploads))
				for i, item := range body.Uploads {
					page[i] = item.Key
				}
				mu.Lock()
				keys = append(keys, page)
				mu.Unlock()
				if calls.Add(1) == 1 {
					w.Header().Set("Content-Type", "application/problem+json")
					w.WriteHeader(status)
					_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "code": "unavailable", "title": "Transient replica"})
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	c := newTestBrowser(t)
	bootstrapBrowser(t, c, h)
	paths := make([]string, count)
	for i := range paths {
		paths[i] = filepath.Join(t.TempDir(), fmt.Sprintf("rolling-admission-%d.bin", i))
		if err := os.WriteFile(paths[i], []byte("abc"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := chromedp.Run(c.ctx, chromedp.SetUploadFiles("#upload-input", paths, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(c.ctx, "#action-dialog", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(c.ctx, chromedp.SetValue("#upload-strategy", "keep-both", chromedp.ByQuery), chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(c.ctx, fmt.Sprintf(`document.querySelector('[data-transfer-count="complete"]').textContent==='(%d)'`, count), 15*time.Second); err != nil {
		t.Fatalf("route rollout gap became permanent failure: %v %s", err, browserStatus(c.ctx))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 || len(keys[0]) != count || len(keys[1]) != count {
		sizes := make([]int, len(keys))
		for i, page := range keys {
			sizes[i] = len(page)
		}
		t.Fatalf("gap retry page sizes: %v", sizes)
	}
	for i, key := range keys[0] {
		if keys[1][i] != key {
			t.Fatalf("gap changed stable item keys: %+v", keys)
		}
	}
	c.assertNoExternalRequests(t, h)
}

func TestE2EUploadIndependentAdmissionMultiplePagesPreserveEveryItem(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("Nix test-e2e enables Chromium")
	}
	var calls atomic.Int64
	var mu sync.Mutex
	var sizes []int
	h := newPortableHarnessWithControlPlaneWrapper(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/uploads/batch/admit" {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(raw))
				var body struct {
					Uploads []json.RawMessage `json:"uploads"`
				}
				_ = json.Unmarshal(raw, &body)
				mu.Lock()
				sizes = append(sizes, len(body.Uploads))
				mu.Unlock()
				calls.Add(1)
			}
			recorder := httptest.NewRecorder()
			next.ServeHTTP(recorder, r)
			if recorder.Code >= 400 && strings.HasPrefix(r.URL.Path, "/api/v1/uploads/") {
				t.Logf("upload control reply: %s %d %s", r.URL.Path, recorder.Code, recorder.Body.String())
			}
			for key, values := range recorder.Header() {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
		})
	})
	c := newTestBrowser(t)
	bootstrapBrowser(t, c, h)
	paths := make([]string, 201)
	root := t.TempDir()
	for i := range paths {
		paths[i] = filepath.Join(root, fmt.Sprintf("multipage-admission-%03d.bin", i))
		if err := os.WriteFile(paths[i], []byte("abc"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := chromedp.Run(c.ctx, chromedp.SetUploadFiles("#upload-input", paths, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(c.ctx, "#action-dialog", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(c.ctx, chromedp.SetValue("#upload-strategy", "keep-both", chromedp.ByQuery), chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(c.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(201)'`, 20*time.Second); err != nil {
		t.Fatalf("multi-page upload failed: %v %s", err, browserStatus(c.ctx))
	}
	mu.Lock()
	defer mu.Unlock()
	if calls.Load() != 3 || len(sizes) != 3 || sizes[0] != 100 || sizes[1] != 100 || sizes[2] != 1 {
		t.Fatalf("admission page budget=%v calls=%d", sizes, calls.Load())
	}
	writes := 0
	for _, request := range c.requestSnapshot() {
		if strings.HasPrefix(request, "PATCH /cap/upload/") {
			writes++
		}
	}
	if writes != 201 {
		t.Fatalf("source writes=%d want201", writes)
	}
	c.assertNoExternalRequests(t, h)
}

func TestE2EUploadLegacyAdmissionLedgerRetainsExactEnvelope(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("Nix test-e2e enables Chromium")
	}
	var legacy, independent atomic.Int64
	h := newPortableHarnessWithControlPlaneWrapper(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				if r.URL.Path == "/api/v1/uploads/batch" {
					legacy.Add(1)
				}
				if r.URL.Path == "/api/v1/uploads/batch/admit" {
					independent.Add(1)
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	c := newTestBrowser(t)
	bootstrapBrowser(t, c, h)
	accounts, err := h.repository.Accounts(c.ctx)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts: %v", err)
	}
	scope, err := domain.NewScope(accounts[0].UserID, domain.AreaLive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.files.CreateDirectory(c.ctx, scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/legacy-admission"), Conflict: domain.ConflictFail}); err != nil {
		t.Fatal(err)
	}
	const key = "historical-browser-admission-key"
	requests := make([]domain.CreateUploadRequest, 2)
	for i := range requests {
		requests[i] = domain.CreateUploadRequest{Path: domain.MustParseUserPath(fmt.Sprintf("/legacy-admission/source-%d.bin", i)), Size: 3, MediaType: "application/octet-stream", Resumable: true, Conflict: domain.ConflictRename, IdempotencyKey: "upload-batch:" + secret.Hash(key+"\x00"+fmt.Sprint(i))}
	}
	caps, err := h.files.(provider.UploadBatchStorage).CreateUploadBatch(c.ctx, scope, requests)
	if err != nil {
		t.Fatal(err)
	}
	seed := fmt.Sprintf(`(async()=>{const ownerID=%q,key=%q;const db=await new Promise((resolve,reject)=>{const r=indexedDB.open('endlessfs-transfer-ledger-v1');r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});const tx=db.transaction(['items','groups'],'readwrite');const ids=[];for(let i=0;i<2;i++){const id='legacy-browser-item-'+i;ids.push(id);const name='source-'+i+'.bin';tx.objectStore('items').put({key:ownerID+':'+id,ownerID,id,groupID:'legacy-admission-group',name,relativePath:'legacy-admission/'+name,relativeDirectory:'legacy-admission',directory:'/legacy-admission',baseDirectory:'/',size:3,mediaType:'application/octet-stream',state:'failed',confirmed:0,uploadID:'',admissionBatchKey:key,admissionBatchIndex:i,admissionBatchCount:2,strategy:'keep-both',planPhase:'done',createdAt:1,error:'Reconnect source.'})}tx.objectStore('groups').put({key:ownerID+':legacy-admission-group',ownerID,id:'legacy-admission-group',name:'Legacy admission',transferIDs:ids,directories:['legacy-admission'],baseDirectory:'/',totalSize:6,state:'failed',discoveryDone:true,strategy:'keep-both',createdAt:1});await new Promise((resolve,reject)=>{tx.oncomplete=resolve;tx.onerror=()=>reject(tx.error)});db.close();})()`, accounts[0].UserID.String(), key)
	if err := chromedp.Run(c.ctx, chromedp.Evaluate(seed, nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }), chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(c.ctx, `document.querySelector('[data-transfer-count="all"]').textContent==='(2)'`, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(c.ctx, chromedp.Click("#open-transfers", chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "legacy-admission")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("source-%d.bin", i)), []byte("abc"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := chromedp.Run(c.ctx, chromedp.Click(`button[aria-label="Reconnect upload source for Legacy admission"]`, chromedp.ByQuery), chromedp.SetUploadFiles("#folder-input", []string{root}, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(c.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(2)'`, 15*time.Second); err != nil {
		t.Fatalf("historical admission replay: %v %s", err, browserStatus(c.ctx))
	}
	if legacy.Load() != 1 || independent.Load() != 0 {
		t.Fatalf("historical envelope changed protocol: legacy=%d independent=%d", legacy.Load(), independent.Load())
	}
	ids := []domain.UploadID{caps[0].UploadID, caps[1].UploadID}
	states, err := h.files.(provider.UploadLifecycleStorage).UploadLifecycle(c.ctx, scope, ids, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		if state.Status == nil || state.Status.State != domain.UploadStateCompleted {
			t.Fatalf("historical accepted item recreated: %+v", states)
		}
	}
	c.assertNoExternalRequests(t, h)
}

func TestE2EUploadIndependentAdmissionExplicitReplanRenewsOnlyRejectedAttempt(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("Nix test-e2e enables Chromium")
	}
	var reject atomic.Bool
	var mu sync.Mutex
	var keys []string
	mutate := func() {}
	h := newPortableHarnessWithControlPlaneWrapper(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/uploads/batch/admit" {
				var body struct {
					Uploads []map[string]any `json:"uploads"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				for _, item := range body.Uploads {
					key, _ := item["idempotencyKey"].(string)
					mu.Lock()
					keys = append(keys, key)
					mu.Unlock()
				}
				if reject.CompareAndSwap(true, false) {
					mutate()
				}
				encoded, _ := json.Marshal(body)
				r.Body = io.NopCloser(bytes.NewReader(encoded))
				r.ContentLength = int64(len(encoded))
			}
			next.ServeHTTP(w, r)
		})
	})
	c := newTestBrowser(t)
	bootstrapBrowser(t, c, h)
	upload := func(body, strategy string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "replan-admission.bin")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := chromedp.Run(c.ctx, chromedp.SetUploadFiles("#upload-input", []string{path}, chromedp.ByQuery)); err != nil {
			t.Fatal(err)
		}
		if err := waitVisible(c.ctx, "#action-dialog", 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if err := chromedp.Run(c.ctx, chromedp.SetValue("#upload-strategy", strategy, chromedp.ByQuery), chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
			t.Fatal(err)
		}
	}
	upload("abc", "keep-both")
	if err := waitFor(c.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(1)'`, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := closeTransferSheet(c.ctx); err != nil {
		t.Fatal(err)
	}
	accounts, err := h.repository.Accounts(c.ctx)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts: %v", err)
	}
	scope, err := domain.NewScope(accounts[0].UserID, domain.AreaLive)
	if err != nil {
		t.Fatal(err)
	}
	target := domain.MustParseUserPath("/replan-admission.bin")
	original, err := h.files.Stat(c.ctx, scope, target)
	if err != nil {
		t.Fatal(err)
	}
	mutate = func() {
		cap, err := h.files.CreateUpload(c.ctx, scope, domain.CreateUploadRequest{Path: target, Size: 3, MediaType: "application/octet-stream", Conflict: domain.ConflictReplace, ExpectedVersion: original.Version, Resumable: true, IdempotencyKey: "admission-concurrent-actor"})
		if err != nil {
			t.Error(err)
			return
		}
		request, err := http.NewRequestWithContext(c.ctx, cap.Method, cap.URL, strings.NewReader("def"))
		if err != nil {
			t.Error(err)
			return
		}
		for key, value := range cap.Headers {
			request.Header.Set(key, value)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Error(err)
			return
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Errorf("synthetic concurrent write=%d", response.StatusCode)
			return
		}
		if _, err := h.files.CompleteUpload(c.ctx, scope, domain.CompleteUploadRequest{UploadID: cap.UploadID, Path: target, Size: 3, MediaType: "application/octet-stream", CRC32C: objectstore.FingerprintFor([]byte("def")).CRC32C}); err != nil {
			t.Error(err)
		}
	}

	reject.Store(true)
	upload("xyz", "replace-changed")
	if err := waitFor(c.ctx, `document.querySelector('[data-transfer-count="failed"]').textContent==='(1)'`, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(c.ctx, chromedp.Click("#retry-failed-transfers", chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(c.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(2)'`, 10*time.Second); err != nil {
		t.Fatalf("explicit replan: %v %s", err, browserStatus(c.ctx))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 3 || keys[1] == keys[2] {
		t.Fatalf("explicit new metadata attempt reused its key: admissions=%d", len(keys))
	}
	writes := 0
	for _, request := range c.requestSnapshot() {
		if strings.HasPrefix(request, "PATCH /cap/upload/") {
			writes++
		}
	}
	if writes != 2 {
		t.Fatalf("replan retransferred accepted source: %d writes", writes)
	}
	c.assertNoExternalRequests(t, h)
}
