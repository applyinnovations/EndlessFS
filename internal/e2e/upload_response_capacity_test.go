package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// A URL fragment is part of the opaque capability representation but is never
// sent to the provider. It exercises real response capacity and browser fetch
// without adding headers, changing the native transfer protocol or fake bytes.
type responseCapacityBackend struct {
	objectstore.Backend
	objectstore.DirectTransferBackend
	padding string
}

func (backend responseCapacityBackend) BeginUpload(ctx context.Context, request objectstore.UploadRequest) (objectstore.UploadHandle, error) {
	handle, err := backend.DirectTransferBackend.BeginUpload(ctx, request)
	if err == nil {
		handle.Capability.URL += "#" + backend.padding
	}
	return handle, err
}

func (backend responseCapacityBackend) ResumeUpload(ctx context.Context, session []byte) (objectstore.UploadCapability, error) {
	capability, err := backend.DirectTransferBackend.ResumeUpload(ctx, session)
	if err == nil {
		capability.URL += "#" + backend.padding
	}
	return capability, err
}

func TestE2EUploadResponseCapacityKeepsAdmissionAndRecoversOnlyDeferred(t *testing.T) {
	parallelE2EFixture(t)
	for _, fixture := range []struct {
		name       string
		padding    int
		zeroDetail bool
	}{{"usable-siblings", 600_000, false}, {"all-deferred-page", (1 << 20) - 1400, true}} {
		t.Run(fixture.name, func(t *testing.T) { qualifyUploadResponseCapacity(t, fixture.padding, fixture.zeroDetail) })
	}
}

func qualifyUploadResponseCapacity(t *testing.T, padding int, requireZeroDetail bool) {
	requireSmartRecoveryE2E(t)
	var admissions atomic.Int64
	repeatedAdmission := make(chan struct{}, 1)
	var mu sync.Mutex
	var recoveryPages []int
	var allDeferredPages int
	writes := &smartRecoveryWrites{byIndex: make(map[int]int)}
	h := newSmartRecoveryHarnessWithFileBackend(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/uploads/batch/admit" && admissions.Add(1) > 1 {
				select {
				case repeatedAdmission <- struct{}{}:
				default:
				}
			}
			if r.URL.Path == "/api/v1/uploads/batch/resume" {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				var page struct {
					UploadIDs []string `json:"uploadIDs"`
				}
				if err := json.Unmarshal(body, &page); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				recoveryPages = append(recoveryPages, len(page.UploadIDs))
				mu.Unlock()
				recorder := httptest.NewRecorder()
				next.ServeHTTP(recorder, r)
				var result struct {
					Uploads []struct {
						CapacityDeferred bool `json:"capacityDeferred"`
					} `json:"uploads"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
					t.Error(err)
				}
				allDeferred := len(result.Uploads) > 1
				for _, item := range result.Uploads {
					allDeferred = allDeferred && item.CapacityDeferred
				}
				if allDeferred {
					mu.Lock()
					allDeferredPages++
					mu.Unlock()
				}
				for key, values := range recorder.Header() {
					w.Header()[key] = values
				}
				w.WriteHeader(recorder.Code)
				_, _ = w.Write(recorder.Body.Bytes())
				return
			}
			next.ServeHTTP(w, r)
		})
	}, writes.wrap, func(backend objectstore.Backend) objectstore.Backend {
		return responseCapacityBackend{Backend: backend, DirectTransferBackend: backend.(objectstore.DirectTransferBackend), padding: strings.Repeat("x", padding)}
	})
	client := newSmartRecoveryBrowser(t)
	bootstrapSmartRecovery(t, client, h.harness)
	root := createSmartRecoverySources(t, "capacity-folder", 8)
	if err := runStage(client.ctx, 10*time.Second, chromedp.SetUploadFiles("#folder-input", []string{root}, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(client.ctx, "#action-dialog", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := runStage(client.ctx, 10*time.Second, chromedp.SetValue("#upload-strategy", "smart-merge", chromedp.ByQuery), chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		finished <- waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(8)'`, 30*time.Second)
	}()
	select {
	case <-repeatedAdmission:
		t.Fatalf("response-capacity handling repeated an already durable admission: calls=%d", admissions.Load())
	case err := <-finished:
		if err != nil {
			t.Fatalf("capacity recovery did not complete actual bytes: %v %s", err, smartRecoveryDiagnostic(client))
		}
	}
	if admissions.Load() != 1 {
		t.Fatalf("admission calls=%d, want one durable cohort", admissions.Load())
	}
	writes.assertExactly(t, 0, 8)
	mu.Lock()
	pages := append([]int(nil), recoveryPages...)
	zeroDetailPages := allDeferredPages
	mu.Unlock()
	if requireZeroDetail && zeroDetailPages == 0 {
		t.Fatal("near-limit capabilities never exercised a page containing only deferrals")
	}
	if len(pages) == 0 {
		t.Fatal("deferred admission never exercised explicit resume")
	}
	var retried bool
	for _, count := range pages {
		if count > 1 {
			retried = true
		}
		if count < 1 || count > 100 {
			t.Fatalf("unbounded recovery cohort=%d", count)
		}
	}
	if !retried {
		t.Fatalf("capacity test never exercised a multi-item recovery cohort: pages=%v", pages)
	}
	assertSmartRecoveryLedger(t, client, 8, 8)
	var retries []int
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(`(async()=>{const db=await new Promise((resolve,reject)=>{const r=indexedDB.open('endlessfs-transfer-ledger-v1');r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});const tx=db.transaction('items','readonly');const items=await new Promise((resolve,reject)=>{const r=tx.objectStore('items').getAll();r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});db.close();return items.map(item=>item.retryCount||0)})()`, &retries, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
		t.Fatal(err)
	}
	for _, count := range retries {
		if count != 0 {
			t.Fatal("capacity partition consumed a transient-error retry")
		}
	}
	verifySmartRecoveryFiles(t, h, smartRecoveryScope(t, h), "capacity-folder", 8)
	h.assertProviderMetadataOnly(t)
	client.assertNoExternalRequests(t, h.harness)
	t.Logf("capacity actual_files=8 actual_bytes=32 completed_retransfers=0 admission_pages=1 recovery_page_sizes=%v all_deferred_pages=%d retry_count=0", pages, zeroDetailPages)
}
