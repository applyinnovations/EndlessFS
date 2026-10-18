package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// A URL fragment is part of the opaque capability representation but is never
// sent to the provider. It exercises real response capacity and browser fetch
// without adding headers, changing the native transfer protocol or fake bytes.
type responseCapacityBackend struct {
	objectstore.Backend
	objectstore.DirectTransferBackend
	padding string
}

func TestE2EUploadResponseCapacityTerminalAttemptCanBeCleared(t *testing.T) {
	requireSmartRecoveryE2E(t)
	var admissions atomic.Int64
	writes := &smartRecoveryWrites{byIndex: make(map[int]int)}
	h := newSmartRecoveryHarnessWithFileBackend(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/uploads/batch/admit" {
				admissions.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	}, writes.wrap, func(backend objectstore.Backend) objectstore.Backend {
		return responseCapacityBackend{Backend: backend, DirectTransferBackend: backend.(objectstore.DirectTransferBackend), padding: strings.Repeat("x", 1<<20)}
	})
	client := newSmartRecoveryBrowser(t)
	bootstrapSmartRecovery(t, client, h.harness)
	root := createSmartRecoverySources(t, "terminal-capacity", 1)
	if err := runStage(client.ctx, 10*time.Second, chromedp.SetUploadFiles("#folder-input", []string{root}, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(client.ctx, "#action-dialog", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := runStage(client.ctx, 10*time.Second, chromedp.SetValue("#upload-strategy", "smart-merge", chromedp.ByQuery), chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="failed"]').textContent==='(1)'`, 10*time.Second); err != nil {
		t.Fatalf("unrepresentable capability did not require attention: %v %s", err, smartRecoveryDiagnostic(client))
	}
	var admitted struct {
		UploadID   string `json:"uploadID"`
		RetryCount int    `json:"retryCount"`
		Error      string `json:"error"`
	}
	script := `(async()=>{for(let attempt=0;attempt<500;attempt++){const db=await new Promise((resolve,reject)=>{const r=indexedDB.open('endlessfs-transfer-ledger-v1');r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});const tx=db.transaction('items','readonly');const items=await new Promise((resolve,reject)=>{const r=tx.objectStore('items').getAll();r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});db.close();if(items.length===1&&items[0].uploadID&&items[0].state==='failed'&&items[0].error)return items[0];await new Promise(resolve=>setTimeout(resolve,10));}return null})()`
	if err := runStage(client.ctx, 10*time.Second, chromedp.Evaluate(script, &admitted, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
		t.Fatal(err)
	}
	if admitted.UploadID == "" || admitted.RetryCount != 0 || admitted.Error == "" || admissions.Load() != 1 {
		t.Fatalf("terminal admission identity/retry semantics: retained=%t retries=%d error=%t admissions=%d", admitted.UploadID != "", admitted.RetryCount, admitted.Error != "", admissions.Load())
	}
	if err := runStage(client.ctx, 10*time.Second, chromedp.Click("#clear-failed-transfers", chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="all"]').textContent==='(0)'`, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	status, err := h.engines[1].Files().UploadStatus(context.Background(), smartRecoveryScope(t, h), domain.UploadID(admitted.UploadID))
	if err != nil || status.State != domain.UploadStateAborted {
		t.Fatalf("cleared oversized capability left an active attempt: state=%s err=%v", status.State, err)
	}
	writes.assertExactly(t, 0, 0)
	if admissions.Load() != 1 || countExactRequest(client.requestSnapshot(), "POST /api/v1/uploads/batch/resume") != 1 || countExactRequest(client.requestSnapshot(), "DELETE /api/v1/uploads/batch") != 1 {
		t.Fatalf("terminal response retried or failed cleanup: admissions=%d requests=%v", admissions.Load(), client.requestSnapshot())
	}
	client.assertNoExternalRequests(t, h.harness)
	t.Log("terminal capacity: one retained admission, zero retries/file bytes, one owned batch cancellation")
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

// Observe the real native session even when the bounded application response
// omits its capability. The response-capacity adapter and native lease remain
// unchanged; only synthetic data requests are associated with their outer ID.
type recordedResponseCapacityBackend struct {
	responseCapacityBackend
	recordBegin func(string, string)
}

func (backend recordedResponseCapacityBackend) BeginUpload(ctx context.Context, request objectstore.UploadRequest) (objectstore.UploadHandle, error) {
	handle, err := backend.responseCapacityBackend.BeginUpload(ctx, request)
	if err == nil {
		backend.recordBegin(request.UploadID, handle.Capability.URL)
	}
	return handle, err
}

func TestE2EUploadResponseCapacityCancelsDeferredReplyForReplacedAttempt(t *testing.T) {
	requireSmartRecoveryE2E(t)
	type admissionItem struct {
		domain.UploadAdmissionResult
		CapacityDeferred bool `json:"capacityDeferred"`
	}
	type admissionPage struct {
		Items     []admissionItem
		Code      int
		BodyBytes int
		Parsed    bool
	}
	held, release := make(chan admissionPage, 1), make(chan struct{})
	var releaseOnce sync.Once
	var releases atomic.Int64
	unblock := func() {
		releaseOnce.Do(func() {
			releases.Add(1)
			close(release)
		})
	}
	// Release before the harness cleanup waits for its held HTTP handler.
	defer unblock()
	var admissions, injections atomic.Int64
	var mu sync.Mutex
	var pages []admissionPage
	nativeByID := make(map[domain.UploadID]string)
	nativeByURI := make(map[string]domain.UploadID)
	var dataUploads []domain.UploadID
	var malformedNative, duplicateNative, unknownData int
	writes := &smartRecoveryWrites{byIndex: make(map[int]int)}
	h := newSmartRecoveryHarnessWithFileBackend(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/api/v1/uploads/batch/admit" {
				next.ServeHTTP(w, r)
				return
			}
			pageNumber := admissions.Add(1)
			recorder := httptest.NewRecorder()
			next.ServeHTTP(recorder, r)
			var body struct {
				Uploads []admissionItem `json:"uploads"`
			}
			parseErr := json.Unmarshal(recorder.Body.Bytes(), &body)
			page := admissionPage{Items: body.Uploads, Code: recorder.Code, BodyBytes: recorder.Body.Len(), Parsed: parseErr == nil}
			mu.Lock()
			pages = append(pages, page)
			mu.Unlock()
			if pageNumber == 1 {
				// The actual handler has already durably admitted this cohort
				// and chosen its bounded response. Never synthesize a deferral.
				injections.Add(1)
				held <- page
				<-release
			}
			copySmartRecoveryResponse(w, recorder)
		})
	}, func(next http.Handler) http.Handler {
		actualWrites := writes.wrap(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPatch || r.Method == http.MethodPut {
				mu.Lock()
				uploadID, found := nativeByURI[r.URL.RequestURI()]
				if !found {
					unknownData++
				}
				dataUploads = append(dataUploads, uploadID)
				mu.Unlock()
			}
			actualWrites.ServeHTTP(w, r)
		})
	}, func(backend objectstore.Backend) objectstore.Backend {
		return recordedResponseCapacityBackend{
			responseCapacityBackend: responseCapacityBackend{Backend: backend, DirectTransferBackend: backend.(objectstore.DirectTransferBackend), padding: strings.Repeat("x", 600_000)},
			recordBegin: func(id, capabilityURL string) {
				// Fragments never reach the provider. Bind every real allocated
				// session, including the capabilities omitted from the reply.
				nativeURL, _, _ := strings.Cut(capabilityURL, "#")
				parsed, err := url.Parse(nativeURL)
				mu.Lock()
				defer mu.Unlock()
				if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.Path == "" || id == "" {
					malformedNative++
					return
				}
				uploadID := domain.UploadID(id)
				if _, found := nativeByID[uploadID]; found {
					duplicateNative++
				}
				if _, found := nativeByURI[parsed.RequestURI()]; found {
					duplicateNative++
				}
				nativeByID[uploadID] = parsed.RequestURI()
				nativeByURI[parsed.RequestURI()] = uploadID
			},
		}
	})
	client := newSmartRecoveryBrowser(t)
	bootstrapSmartRecovery(t, client, h.harness)
	root := createSmartRecoverySources(t, "late-capacity", 3)
	if err := runStage(client.ctx, 10*time.Second, chromedp.SetUploadFiles("#folder-input", []string{root}, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(client.ctx, "#action-dialog", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := runStage(client.ctx, 10*time.Second, chromedp.SetValue("#upload-strategy", "smart-merge", chromedp.ByQuery), chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
	var original admissionPage
	select {
	case original = <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the actual admission response was never held")
	}
	if !original.Parsed || original.Code != http.StatusOK || original.BodyBytes > 1<<20 || len(original.Items) != 3 || injections.Load() != 1 || admissions.Load() != 1 {
		t.Fatalf("actual held admission shape: parsed=%t status=%d bytes=%d items=%d injections=%d admissions=%d", original.Parsed, original.Code, original.BodyBytes, len(original.Items), injections.Load(), admissions.Load())
	}
	oldIDs := make(map[domain.UploadID]bool, 3)
	oldItemKeys := make(map[string]bool, 3)
	oldItemIDs := make([]string, 0, 3)
	deferred, usable := 0, 0
	for index, item := range original.Items {
		if item.Index != index || item.ID == "" || oldItemKeys[item.ID] || item.UploadID == "" || oldIDs[item.UploadID] || item.Outcome != "accepted" || item.Status == nil || item.Status.UploadID != item.UploadID {
			t.Fatalf("actual original admission lost item binding at index=%d", index)
		}
		oldIDs[item.UploadID] = true
		oldItemKeys[item.ID] = true
		oldItemIDs = append(oldItemIDs, item.ID)
		if item.CapacityDeferred && item.NextAction == "resume" && item.Capability == nil {
			deferred++
		} else if !item.CapacityDeferred && item.Capability != nil && item.Capability.UploadID == item.UploadID {
			usable++
		} else {
			t.Fatalf("actual original admission had an unexpected capacity contract at index=%d", index)
		}
	}
	if deferred == 0 || usable == 0 {
		t.Fatalf("actual handler did not exercise mixed omitted/usable capabilities: deferred=%d usable=%d", deferred, usable)
	}
	mu.Lock()
	allocated := len(nativeByID)
	for uploadID := range oldIDs {
		if _, found := nativeByID[uploadID]; !found {
			allocated = -1
		}
	}
	mu.Unlock()
	if allocated != 3 {
		t.Fatalf("original outer IDs were not bound to all real native sessions: allocated=%d", allocated)
	}
	// Expand the ordinary UI so its rendered identities independently prove
	// that retry replaced every attempt before the old reply is released.
	if err := runStage(client.ctx, 5*time.Second, chromedp.Evaluate(`document.querySelector('[data-transfer-count="all"]').click()`, nil)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(client.ctx, `button[aria-label^="Expand upload group"]`, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := runStage(client.ctx, 5*time.Second, chromedp.Click(`button[aria-label^="Expand upload group"]`, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	oldItemJSON, err := json.Marshal(oldItemIDs)
	if err != nil {
		t.Fatal(err)
	}
	rowBinding := `(()=>{const old=new Set(` + string(oldItemJSON) + `),rows=[...document.querySelectorAll('#transfer-list [data-transfer-id]')];return rows.length===3&&rows.every(row=>old.has(row.dataset.transferId))})()`
	if err := waitFor(client.ctx, rowBinding, 5*time.Second); err != nil {
		t.Fatal("rendered original transfer identities did not match the held actual admission")
	}
	if err := runStage(client.ctx, 5*time.Second, chromedp.Click(`button[aria-label^="Cancel folder upload"]`, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelectorAll('#transfer-list [data-transfer-id].cancelled').length===3`, 5*time.Second); err != nil {
		t.Fatal("the held original cohort was not cancelled before retry")
	}
	if err := waitVisible(client.ctx, `button[aria-label^="Retry folder upload"]`, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := runStage(client.ctx, 5*time.Second, chromedp.Click(`button[aria-label^="Retry folder upload"]`, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	replacedBinding := `(()=>{const old=new Set(` + string(oldItemJSON) + `),rows=[...document.querySelectorAll('#transfer-list [data-transfer-id]')],ids=rows.map(row=>row.dataset.transferId);return ids.length===3&&new Set(ids).size===3&&ids.every(id=>id&&!old.has(id))&&rows.every(row=>!row.classList.contains('cancelled')&&!row.classList.contains('failed'))&&document.querySelector('[data-transfer-count="complete"]').textContent==='(0)'})()`
	if err := waitFor(client.ctx, replacedBinding, 5*time.Second); err != nil {
		t.Fatal("retry did not replace all three rendered attempt identities before release")
	}
	if releases.Load() != 0 || admissions.Load() != 1 {
		t.Fatal("the replacement proof did not occur while the original admission reply was held")
	}
	var replacementItemIDs []string
	if err := runStage(client.ctx, 5*time.Second, chromedp.Evaluate(`[...document.querySelectorAll('#transfer-list [data-transfer-id]')].map(row=>row.dataset.transferId)`, &replacementItemIDs)); err != nil {
		t.Fatal(err)
	}
	replacementKeys := make(map[string]bool, 3)
	for _, id := range replacementItemIDs {
		replacementKeys[id] = true
	}
	if len(replacementItemIDs) != 3 || len(replacementKeys) != 3 {
		t.Fatal("replacement identities changed while the original response remained held")
	}
	writes.assertExactly(t, 0, 0)
	unblock()
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(3)'`, 30*time.Second); err != nil {
		t.Fatalf("replacement actual uploads did not complete: %v %s", err, smartRecoveryDiagnostic(client))
	}
	mu.Lock()
	capturedPages := append([]admissionPage(nil), pages...)
	oldData, replacementData := 0, make(map[domain.UploadID]int)
	for _, uploadID := range dataUploads {
		if oldIDs[uploadID] {
			oldData++
		} else {
			replacementData[uploadID]++
		}
	}
	nativeCount, malformed, duplicates, unknown := len(nativeByID), malformedNative, duplicateNative, unknownData
	mu.Unlock()
	if admissions.Load() != 2 || injections.Load() != 1 || releases.Load() != 1 || len(capturedPages) != 2 || len(capturedPages[1].Items) != 3 {
		t.Fatalf("replacement admission contract: calls=%d injections=%d releases=%d pages=%d", admissions.Load(), injections.Load(), releases.Load(), len(capturedPages))
	}
	if oldData != 0 || nativeCount != 6 || malformed != 0 || duplicates != 0 || unknown != 0 || len(replacementData) != 3 {
		t.Fatalf("native data ownership: old_requests=%d sessions=%d malformed=%d duplicate=%d unknown=%d replacement_sessions=%d", oldData, nativeCount, malformed, duplicates, unknown, len(replacementData))
	}
	scope := smartRecoveryScope(t, h)
	for uploadID := range oldIDs {
		status, statusErr := h.engines[1].Files().UploadStatus(context.Background(), scope, uploadID)
		if statusErr != nil || status.State != domain.UploadStateAborted {
			t.Fatalf("late outer upload ID was not aborted by the browser cleanup: state=%s query_failed=%t", status.State, statusErr != nil)
		}
	}
	replacement := capturedPages[1]
	if !replacement.Parsed || replacement.Code != http.StatusOK || replacement.BodyBytes > 1<<20 {
		t.Fatal("replacement did not receive an actual bounded successful admission response")
	}
	seenReplacementIDs, seenReplacementKeys := make(map[domain.UploadID]bool, 3), make(map[string]bool, 3)
	for index, item := range replacement.Items {
		if item.Index != index || !replacementKeys[item.ID] || oldItemKeys[item.ID] || seenReplacementKeys[item.ID] || item.UploadID == "" || oldIDs[item.UploadID] || seenReplacementIDs[item.UploadID] || replacementData[item.UploadID] != 1 {
			t.Fatalf("replacement admission/data did not preserve a distinct once-only attempt at index=%d", index)
		}
		seenReplacementIDs[item.UploadID], seenReplacementKeys[item.ID] = true, true
		status, statusErr := h.engines[1].Files().UploadStatus(context.Background(), scope, item.UploadID)
		if statusErr != nil || status.State != domain.UploadStateCompleted {
			t.Fatalf("replacement completion was not independently authoritative: state=%s query_failed=%t", status.State, statusErr != nil)
		}
	}
	writes.assertExactly(t, 0, 3)
	assertSmartRecoveryLedger(t, client, 3, 3)
	verifySmartRecoveryFiles(t, h, scope, "late-capacity", 3)
	h.assertProviderMetadataOnly(t)
	client.assertNoExternalRequests(t, h.harness)
	t.Logf("late actual capacity admission: deferred=%d usable=%d originals_aborted=3 old_attempt_data_requests=0 replacement_files=3 replacement_bytes=12 each_once=true", deferred, usable)
}

func TestE2EUploadResponseCapacityKeepsAdmissionAndRecoversOnlyDeferred(t *testing.T) {
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
