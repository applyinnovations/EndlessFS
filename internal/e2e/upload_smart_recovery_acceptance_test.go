package e2e

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/integrity"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	cdpwebauthn "github.com/chromedp/cdproto/webauthn"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// The first seven completions, admission identities and local history are all
// produced by the application. The injected denial prevents three accepted
// capabilities from reaching the browser; their data has never been sent.
func TestE2ESmartRecoveryPersistsPartialUploadAcrossRefreshAndLogin(t *testing.T) {
	requireSmartRecoveryE2E(t)
	for _, projection := range []string{"cold", "rebuilt", "warm"} {
		t.Run(projection, func(t *testing.T) { runSmartRecoveryPartialUpload(t, projection) })
	}
}

func runSmartRecoveryPartialUpload(t *testing.T, projection string) {
	t.Helper()
	var deny atomic.Bool
	deny.Store(true)
	var denied atomic.Int64
	writes := &smartRecoveryWrites{byIndex: make(map[int]int)}
	h := newSmartRecoveryHarness(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/uploads/batch/admit" || !deny.Load() {
				next.ServeHTTP(w, r)
				return
			}
			recorder := httptest.NewRecorder()
			next.ServeHTTP(recorder, r)
			var result struct {
				Uploads []domain.UploadAdmissionResult `json:"uploads"`
			}
			if recorder.Code == http.StatusOK {
				if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
					t.Error(err)
				}
				for index := range result.Uploads {
					if denied.Load() >= 3 || index < len(result.Uploads)-3 {
						continue
					}
					item := result.Uploads[index]
					result.Uploads[index] = domain.UploadAdmissionResult{Index: item.Index, ID: item.ID, Outcome: "rejected", ErrorKind: domain.ErrorInvalid, NextAction: "stop"}
					denied.Add(1)
				}
				encoded, err := json.Marshal(result)
				if err != nil {
					t.Error(err)
				}
				recorder.Body.Reset()
				_, _ = recorder.Body.Write(encoded)
			}
			copySmartRecoveryResponse(w, recorder)
		})
	}, writes.wrap)
	client := newSmartRecoveryBrowser(t)
	bootstrapSmartRecovery(t, client, h.harness)
	scope := smartRecoveryScope(t, h)
	root := createSmartRecoverySources(t, "partial-folder", 10)
	prepareSmartRecoveryDirectories(t, h, scope, "partial-folder", 10)
	seedSmartRecoveryAnchor(t, h, scope, 1)
	if projection != "cold" {
		_, err := h.engines[0].Files().PlanUploadSizes(context.Background(), scope.UserID(), domain.UploadSizePlanRequest{Items: []domain.UploadSizePlanItem{{ID: "prewarm", Path: domain.MustParseUserPath("/prewarm.bin"), Size: 4}}})
		if err != nil {
			t.Fatal(err)
		}
		if projection == "rebuilt" {
			seedSmartRecoveryAnchor(t, h, scope, 2)
			deleteSmartRecoveryProjection(t, h, scope.UserID())
		}
	}
	h.stateLedger.Reset()
	h.fileLedger.Reset()
	requestStart := len(client.requestSnapshot())
	if err := runStage(client.ctx, 10*time.Second, chromedp.SetUploadFiles("#folder-input", []string{root}, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(client.ctx, "#action-dialog", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := runStage(client.ctx, 10*time.Second,
		chromedp.SetValue("#upload-strategy", "smart-merge", chromedp.ByQuery),
		chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(7)' && document.querySelector('[data-transfer-count="failed"]').textContent==='(3)'`, 30*time.Second); err != nil {
		t.Fatalf("partial smart upload did not preserve seven valid siblings: %v %s", err, smartRecoveryDiagnostic(client))
	}
	if denied.Load() != 3 {
		t.Fatalf("denied admissions=%d, want3", denied.Load())
	}
	assertSmartRecoveryLedger(t, client, 10, 7)
	before := writes.snapshot()
	if len(before) != 7 {
		t.Fatalf("initial browser byte transfers=%d, want7", len(before))
	}
	deny.Store(false)
	if err := closeTransferSheet(client.ctx); err != nil {
		t.Fatal(err)
	}
	refreshAndLoginSmartRecovery(t, client, 10)
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(7)' && document.querySelector('[data-transfer-count="all"]').textContent==='(10)'`, 10*time.Second); err != nil {
		t.Fatalf("partial completion did not survive refresh/login: %v %s", err, smartRecoveryDiagnostic(client))
	}
	reconnectSmartRecovery(t, client, root)
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(10)' && document.querySelector('#transfer-check-label').textContent==='Preparing 100%'`, 30*time.Second); err != nil {
		t.Fatalf("smart reconnect did not transfer and complete remaining files: %v %s", err, smartRecoveryDiagnostic(client))
	}
	writes.assertExactly(t, 0, 10)
	assertSmartRecoveryLedger(t, client, 10, 10)
	stateEvents, fileEvents := h.stateLedger.Events(), h.fileLedger.Events()
	assertSmartRecoveryProviderBudget(t, stateEvents, fileEvents, 10, 3)
	// The selected independent-action composition measured 241 cold, 237
	// rebuilt and 240 warm requests, including logout/login and before final
	// List/download oracles. Retain the measured maximum across all setups.
	stateBudget := 241
	if len(stateEvents) > stateBudget {
		t.Fatalf("%s journey state-provider requests=%d, measured budget=%d", projection, len(stateEvents), stateBudget)
	}
	verifySmartRecoveryFiles(t, h, scope, "partial-folder", 10)
	requests := client.requestSnapshot()[requestStart:]
	if got := countExactRequest(requests, "POST /api/v1/uploads/plan/sizes"); got < 2 {
		t.Fatalf("smart size planning bypassed on retry: calls=%d", got)
	}
	if got := countExactRequest(requests, "POST /api/v1/uploads/batch/admit"); got != 2 {
		t.Fatalf("admission pages=%d, want2", got)
	}
	assertSmartRecoveryBulkRoutes(t, requests)
	h.assertProviderMetadataOnly(t)
	client.assertNoExternalRequests(t, h.harness)
	t.Logf("smart-recovery projection=%s partial=7/10 byte-transfers=10 repeated=0 admission-pages=2 size-pages=%d state-provider=%d file-provider=%d replica-requests=%d/%d", projection, countExactRequest(requests, "POST /api/v1/uploads/plan/sizes"), len(stateEvents), len(fileEvents), h.replicas[0].Load(), h.replicas[1].Load())
}

func TestE2ESmartRecoveryResumesOnlyUnconfirmedBytesAfterRefreshLogin(t *testing.T) {
	requireSmartRecoveryE2E(t)
	const firstChunk = 8 << 20
	body := bytes.Repeat([]byte{'q'}, firstChunk+17)
	stored, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	type chunk struct{ offset, size int }
	var mu sync.Mutex
	var chunks []chunk
	h := newSmartRecoveryHarness(t, nil, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPatch {
				next.ServeHTTP(w, r)
				return
			}
			data, err := io.ReadAll(io.LimitReader(r.Body, firstChunk+1))
			if err != nil {
				t.Error(err)
				return
			}
			var offset int
			if _, err := fmt.Sscanf(r.Header.Get("Upload-Offset"), "%d", &offset); err != nil {
				t.Error(err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(data))
			recorder := httptest.NewRecorder()
			next.ServeHTTP(recorder, r)
			if recorder.Code != http.StatusNoContent {
				t.Errorf("chunk offset=%d status=%d", offset, recorder.Code)
			}
			mu.Lock()
			chunks = append(chunks, chunk{offset, len(data)})
			mu.Unlock()
			if offset == 0 {
				stored <- struct{}{}
				// Lose the successful first-chunk response during refresh, after
				// the provider has durably confirmed its offset.
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			copySmartRecoveryResponse(w, recorder)
		})
	})
	client := newSmartRecoveryBrowser(t)
	bootstrapSmartRecovery(t, client, h.harness)
	scope := smartRecoveryScope(t, h)
	root := filepath.Join(t.TempDir(), "resume-folder")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source.bin"), body, 0600); err != nil {
		t.Fatal(err)
	}
	h.stateLedger.Reset()
	h.fileLedger.Reset()
	requestStart := len(client.requestSnapshot())
	if err := runStage(client.ctx, 10*time.Second, chromedp.SetUploadFiles("#folder-input", []string{root}, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(client.ctx, "#action-dialog", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := runStage(client.ctx, 10*time.Second, chromedp.SetValue("#upload-strategy", "smart-merge", chromedp.ByQuery), chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stored:
	case <-time.After(15 * time.Second):
		t.Fatalf("first real provider chunk was never stored: %s", smartRecoveryDiagnostic(client))
	}
	var uploadID string
	if err := runStage(client.ctx, 10*time.Second, chromedp.Evaluate(`(async()=>{for(let attempt=0;attempt<500;attempt++){const db=await new Promise((resolve,reject)=>{const r=indexedDB.open('endlessfs-transfer-ledger-v1');r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});const tx=db.transaction('items','readonly');const items=await new Promise((resolve,reject)=>{const r=tx.objectStore('items').getAll();r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});db.close();if(items.length===1&&items[0].uploadID)return items[0].uploadID;await new Promise(resolve=>setTimeout(resolve,10));}return ''})()`, &uploadID, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil || uploadID == "" {
		t.Fatalf("active admission identity was not durable: id-present=%t err=%v", uploadID != "", err)
	}
	status, err := h.engines[1].Files().UploadStatus(context.Background(), scope, domain.UploadID(uploadID))
	if err != nil || status.ConfirmedOffset != firstChunk || status.State != domain.UploadStateActive {
		t.Fatalf("provider-confirmed first chunk: status=%+v err=%v", status, err)
	}
	if err := closeTransferSheet(client.ctx); err != nil {
		t.Fatal(err)
	}
	refreshAndLoginSmartRecovery(t, client, 1)
	unblock()
	reconnectSmartRecovery(t, client, root)
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(1)' && document.querySelector('#transfer-check-label').textContent==='Preparing 100%'`, 20*time.Second); err != nil {
		t.Fatalf("confirmed-offset resume did not complete: %v %s", err, smartRecoveryDiagnostic(client))
	}
	assertSmartRecoveryLedger(t, client, 1, 1)
	mu.Lock()
	actual := append([]chunk(nil), chunks...)
	mu.Unlock()
	if len(actual) != 2 || actual[0] != (chunk{0, firstChunk}) || actual[1] != (chunk{firstChunk, 17}) {
		t.Fatalf("confirmed bytes retransferred: chunks=%+v", actual)
	}
	requests := client.requestSnapshot()[requestStart:]
	if countExactRequest(requests, "POST /api/v1/uploads/plan/sizes") != 1 || countExactRequest(requests, "POST /api/v1/uploads/batch/admit") != 1 || countExactRequest(requests, "POST /api/v1/uploads/batch/resume") != 1 {
		t.Fatalf("resume re-admitted or bypassed smart planning: sizes=%d admissions=%d resume-pages=%d", countExactRequest(requests, "POST /api/v1/uploads/plan/sizes"), countExactRequest(requests, "POST /api/v1/uploads/batch/admit"), countExactRequest(requests, "POST /api/v1/uploads/batch/resume"))
	}
	assertSmartRecoveryBulkRoutes(t, requests)
	entry, err := h.engines[1].Files().Stat(context.Background(), scope, domain.MustParseUserPath("/resume-folder/source.bin"))
	if err != nil || entry.Size != int64(len(body)) {
		t.Fatalf("resumed authoritative entry: %+v err=%v", entry, err)
	}
	capability, err := h.engines[1].Files().CreateDownload(context.Background(), scope, domain.CreateDownloadRequest{Path: entry.Path, Version: entry.Version})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	h.fileBase.ServeHTTP(recorder, httptest.NewRequest(capability.Method, capability.URL, nil))
	if recorder.Code != http.StatusOK || !bytes.Equal(recorder.Body.Bytes(), body) {
		t.Fatalf("resumed body integrity failed: status=%d size=%d", recorder.Code, recorder.Body.Len())
	}
	h.assertProviderMetadataOnly(t)
	client.assertNoExternalRequests(t, h.harness)
	t.Logf("smart-resume bytes=%d first-confirmed=%d remaining=17 data-requests=2 repeated-confirmed-bytes=0 admission=1 resume-pages=1", len(body), firstChunk)
}

// This scale fixture represents the customer's cardinalities with four-byte
// synthetic bodies, not its byte volume or content. Completed ledger records
// mirror 7,966 actual upload/verification/publication results. Every remaining
// source is ambiguous by size but differs by content, forcing local hashing,
// exact planning, admission, real provider data transfer and verified completion.
func TestE2ESmartRecoveryProductionCardinalityWarmTransfersOnlyRemainingFiles(t *testing.T) {
	requireSmartRecoveryE2E(t)
	testStarted := time.Now()
	const total, completed, remaining = 10_604, 7_966, 2_638
	writes := &smartRecoveryWrites{byIndex: make(map[int]int)}
	started, release := make(chan struct{}, 1), make(chan struct{})
	var admissionMu sync.Mutex
	admitted := make(map[int]int, remaining)
	actionPageSizes := make(map[int]int)
	var actionSlowPages, actionMaximumMillis int64
	var hold atomic.Bool
	var initialSizeBytes atomic.Int64
	var dataPATCHRequests, dataOPTIONSRequests atomic.Int64
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	h := newSmartRecoveryHarness(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/uploads/batch/actions" {
				body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
				if err != nil {
					t.Error(err)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				var page struct{ Items []struct{} }
				if err := json.Unmarshal(body, &page); err != nil || len(page.Items) < 1 || len(page.Items) > 100 {
					t.Errorf("browser action page count=%d err=%v", len(page.Items), err)
				}
				started := time.Now()
				next.ServeHTTP(w, r)
				millis := time.Since(started).Milliseconds()
				admissionMu.Lock()
				actionPageSizes[len(page.Items)]++
				actionMaximumMillis = max(actionMaximumMillis, millis)
				if millis >= 15_000 {
					actionSlowPages++
				}
				admissionMu.Unlock()
				return
			}
			if r.URL.Path == "/api/v1/uploads/batch/admit" {
				body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
				if err != nil {
					t.Error(err)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				var page struct {
					Uploads []struct {
						Name string `json:"name"`
						Path string `json:"path"`
					} `json:"uploads"`
				}
				if err := json.Unmarshal(body, &page); err != nil || len(page.Uploads) < 1 || len(page.Uploads) > 100 {
					t.Errorf("browser admission page count=%d err=%v", len(page.Uploads), err)
					return
				}
				admissionMu.Lock()
				for _, item := range page.Uploads {
					var index int
					if _, err := fmt.Sscanf(item.Name, "source-%d.bin", &index); err != nil || index < completed || index >= total || item.Path != "/"+filepath.Dir(smartRecoveryRelative("scale-folder", index)) {
						t.Errorf("completed or misbound item was admitted: synthetic-index=%d err=%v", index, err)
					}
					admitted[index]++
				}
				admissionMu.Unlock()
			}
			if r.URL.Path == "/api/v1/uploads/plan/sizes" && hold.CompareAndSwap(true, false) {
				initialSizeBytes.Store(r.ContentLength)
				started <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}, func(next http.Handler) http.Handler {
		wrapped := writes.wrap(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPatch:
				dataPATCHRequests.Add(1)
			case http.MethodOptions:
				dataOPTIONSRequests.Add(1)
			}
			wrapped.ServeHTTP(w, r)
		})
	})
	client := newSmartRecoveryBrowser(t)
	bootstrapSmartRecovery(t, client, h.harness)
	scope := smartRecoveryScope(t, h)
	root := createSmartRecoverySources(t, "scale-folder", total)
	prepareSmartRecoveryDirectories(t, h, scope, "scale-folder", total)
	seedSmartRecoveryCompletedFiles(t, h, scope, "scale-folder", completed)
	assertSmartRecoveryProjectionStates(t, h, scope, "scale-folder", completed, remaining)
	seedSmartRecoveryLedger(t, client, scope.UserID(), "scale-folder", total, completed)
	// Whole-journey accounting starts after all synthetic authority, projection
	// exploration and persisted-ledger setup. It includes actual reload, logout,
	// passkey login, workspace/history restoration and foreground recovery.
	// Keep one continuous ledger so no concurrent event can fall between a
	// restore snapshot and a reset at the foreground boundary.
	journeyStarted := time.Now()
	journeyRequestStart := len(client.requestSnapshot())
	journeyControlHTTPStart := h.replicas[0].Load() + h.replicas[1].Load()
	h.stateLedger.Reset()
	h.fileLedger.Reset()
	refreshAndLoginSmartRecovery(t, client, total)
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(7966)' && document.querySelector('[data-transfer-count="all"]').textContent==='(10604)'`, 20*time.Second); err != nil {
		t.Fatalf("scale ledger restoration failed: %v %s", err, smartRecoveryDiagnostic(client))
	}
	openSmartRecoverySheet(t, client)
	if err := waitFor(client.ctx, `document.querySelector('#transfer-check-label').textContent==='Preparing 75%'`, 5*time.Second); err != nil {
		t.Fatalf("completed preparation share was not retained: %v %s", err, smartRecoveryDiagnostic(client))
	}
	expandSmartRecoveryScaleHistory(t, client)
	if err := runStage(client.ctx, 5*time.Second, chromedp.Evaluate(`(()=>{
		const label=document.querySelector('#transfer-check-label'),list=document.querySelector('#transfer-list');
		window.__smartRecoveryProgress={minimum:75,maximum:75,intermediate:false,maxRendered:0};
		const sample=()=>{const value=Number(label.textContent.match(/Preparing (\d+)%/)?.[1]);const proof=window.__smartRecoveryProgress;
		if(Number.isFinite(value)){proof.minimum=Math.min(proof.minimum,value);proof.maximum=Math.max(proof.maximum,value);proof.intermediate||=value>75&&value<100;}
		proof.maxRendered=Math.max(proof.maxRendered,list.querySelectorAll('.transfer-row').length);};
		new MutationObserver(sample).observe(label,{childList:true,subtree:true,characterData:true});
		new MutationObserver(sample).observe(list,{childList:true});sample();})()`, nil)); err != nil {
		t.Fatal(err)
	}
	waitSmartRecoveryNetworkIdle(t, client)
	requestStart := len(client.requestSnapshot())
	restoreStateEnd, restoreFileEnd := len(h.stateLedger.Events()), len(h.fileLedger.Events())
	foregroundControlHTTPStart := h.replicas[0].Load() + h.replicas[1].Load()
	restorePATCHRequests, restoreOPTIONSRequests := dataPATCHRequests.Load(), dataOPTIONSRequests.Load()
	hold.Store(true)
	recoveryStarted := time.Now()
	reconnectSmartRecovery(t, client, root)
	select {
	case <-started:
		t.Logf("real smart scale recovery started after size-route capture: remaining=%d initial-size-body-bytes=%d", remaining, initialSizeBytes.Load())
	case <-time.After(15 * time.Second):
		t.Fatalf("real smart size route never started: %s", smartRecoveryDiagnostic(client))
	}
	if err := waitFor(client.ctx, `document.querySelector('#transfer-check-label').textContent==='Preparing 75%' && !document.querySelector('#transfer-list').textContent.includes('historical-reference')`, 5*time.Second); err != nil {
		t.Fatalf("reconnect reset completed share or retained irrelevant attempt error: %v %s", err, smartRecoveryDiagnostic(client))
	}
	assertSmartRecoveryReconnectedErrors(t, client, total, completed)
	unblock()
	// This is a bounded completion deadline for a real provider/state fixture,
	// not a product latency ceiling. Coverage instrumentation repeats all
	// canonical work and is deliberately allowed to finish the same assertions.
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(10604)' && document.querySelector('#transfer-check-label').textContent==='Preparing 100%'`, 12*time.Minute); err != nil {
		t.Fatalf("representative smart recovery did not complete: %v %s writes=%d", err, smartRecoveryDiagnostic(client), len(writes.snapshot()))
	}
	writes.assertExactly(t, completed, total)
	admissionMu.Lock()
	if len(admitted) != remaining {
		t.Errorf("independent remaining admissions=%d, want%d", len(admitted), remaining)
	}
	for index, count := range admitted {
		if index < completed || index >= total || count != 1 {
			t.Errorf("completed item readmitted or remaining item duplicated: synthetic-index=%d count=%d", index, count)
		}
	}
	admissionMu.Unlock()
	assertSmartRecoveryLedger(t, client, total, total)
	assertSmartRecoveryErrorHistory(t, client)
	waitSmartRecoveryNetworkIdle(t, client)
	journeyFinished := time.Now()
	journeyControlHTTPEnd := h.replicas[0].Load() + h.replicas[1].Load()
	wholeDataPATCHRequests, wholeDataOPTIONSRequests := dataPATCHRequests.Load(), dataOPTIONSRequests.Load()
	wholeStateEvents, wholeFileEvents := h.stateLedger.Events(), h.fileLedger.Events()
	restoreStateEvents, restoreFileEvents := wholeStateEvents[:restoreStateEnd], wholeFileEvents[:restoreFileEnd]
	stateEvents, fileEvents := wholeStateEvents[restoreStateEnd:], wholeFileEvents[restoreFileEnd:]
	requestSnapshot := client.requestSnapshot()
	restoreRequests := requestSnapshot[journeyRequestStart:requestStart]
	requests, wholeRequests := requestSnapshot[requestStart:], requestSnapshot[journeyRequestStart:]
	assertSmartRecoveryProviderBudget(t, stateEvents, fileEvents, remaining, 0)
	// Measured foreground selected design: 5,960 state requests, 391,163,569 bytes
	// written and 6,018,471,664 bytes read. Keep the real amplification visible:
	// these are regression ratchets, not evidence of an optimal metadata layout.
	// A coalesced final directory refresh may contribute two additional reads.
	// Wire ceilings round to the 4 MiB pack bound; reads allow those two packs.
	const stateRequestBudget = 5_962
	const stateWriteBudget = 376 << 20
	const stateReadBudget = 5_748 << 20
	// The separate real reload/logout/passkey-login/restore phase measured 53
	// state calls, 61,220 bytes written and 7,870,749 bytes read. Its source-less,
	// quiescent auth/list flow has a fixed request ceiling. Round writes to
	// 64 KiB and reads to 8 MiB to retain modest encoding/pack-size headroom.
	// Whole-journey ceilings add this phase to the unchanged foreground caps.
	const restoreStateRequestBudget = 53
	const restoreStateWriteBudget = 64 << 10
	const restoreStateReadBudget = 8 << 20
	const restoreControlHTTPBudget = 12
	// Foreground measured 138 actual control requests; allow the same two
	// coalesced listing requests covered by the existing provider ceiling.
	const foregroundControlHTTPBudget = 140
	var stateWrites, stateReads int64
	for _, event := range stateEvents {
		stateWrites += event.RequestBytes
		stateReads += event.ResponseBytes
	}
	verifySmartRecoveryFiles(t, h, scope, "scale-folder", total)
	admissions := countExactRequest(requests, "POST /api/v1/uploads/batch/admit")
	exact := countExactRequest(requests, "POST /api/v1/uploads/plan/fingerprints")
	sizes := countExactRequest(requests, "POST /api/v1/uploads/plan/sizes")
	preparation := countExactRequest(requests, "POST /api/v1/directories/prepare")
	legacyCompletionPages := countExactRequest(requests, "POST /api/v1/uploads/batch/complete")
	completionPages := countExactRequest(requests, "POST /api/v1/uploads/batch/actions")
	dataRequests, preflights := 0, 0
	for _, request := range requests {
		if strings.HasPrefix(request, "PATCH /cap/upload/") {
			dataRequests++
		}
		if strings.HasPrefix(request, "OPTIONS /cap/upload/") {
			preflights++
		}
	}
	if dataRequests != remaining || preflights != remaining {
		t.Fatalf("remaining data-plane request budget: writes=%d preflights=%d want%d each", dataRequests, preflights, remaining)
	}
	if restorePATCHRequests != 0 || restoreOPTIONSRequests != 0 || wholeDataPATCHRequests != remaining || wholeDataOPTIONSRequests != remaining {
		t.Fatalf("actual data-server request budget: restorePATCH=%d restoreOPTIONS=%d wholePATCH=%d wholeOPTIONS=%d want0/0/%d/%d", restorePATCHRequests, restoreOPTIONSRequests, wholeDataPATCHRequests, wholeDataOPTIONSRequests, remaining, remaining)
	}
	// The selected implementation uses 100-item lifecycle/fingerprint pages.
	// Capture the demonstrated composition, including the initial 2,638-item
	// size question and one refreshed size question per exact page.
	if initialSizeBytes.Load() <= 0 || initialSizeBytes.Load() > 1<<20 {
		t.Fatalf("initial real smart size request did not fit the preserved body bound: bytes=%d", initialSizeBytes.Load())
	}
	var rendered int
	if err := runStage(client.ctx, 10*time.Second, chromedp.Evaluate(`document.querySelectorAll('#transfer-list .transfer-row').length`, &rendered)); err != nil {
		t.Fatal(err)
	}
	if rendered > 72 {
		t.Fatalf("rendered transfer rows=%d, want<=72", rendered)
	}
	var progress smartRecoveryProgressEvidence
	if err := runStage(client.ctx, 10*time.Second, chromedp.Evaluate(`window.__smartRecoveryProgress`, &progress)); err != nil {
		t.Fatal(err)
	}
	if progress.Minimum < 75 || progress.Maximum != 100 || !progress.Intermediate || progress.MaxRendered > 72 || progress.MaxRendered < 2 {
		t.Fatalf("whole-folder preparation or expanded rendering was not bounded: %+v", progress)
	}
	assertSmartRecoveryBulkRoutes(t, wholeRequests)
	h.assertProviderMetadataOnly(t)
	client.assertNoExternalRequests(t, h.harness)
	economics, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	modeledEvents := append(append([]providerbudget.Event(nil), stateEvents...), fileEvents...)
	for range remaining {
		modeledEvents = append(modeledEvents, providerbudget.Event{Role: providerbudget.RoleFileDataPlane, Kind: providerbudget.RequestDataUpload, RequestBytes: 4})
	}
	modeled, err := economics.Estimate(modeledEvents)
	if err != nil {
		t.Fatal(err)
	}
	restoreEvents := append(append([]providerbudget.Event(nil), restoreStateEvents...), restoreFileEvents...)
	restoreModeled, err := economics.Estimate(restoreEvents)
	if err != nil {
		t.Fatal(err)
	}
	wholeEvents := append(append([]providerbudget.Event(nil), restoreEvents...), modeledEvents...)
	wholeModeled, err := economics.Estimate(wholeEvents)
	if err != nil {
		t.Fatal(err)
	}
	// Restore/auth is measured independently; none of its provider work is
	// charged to, or allowed to enlarge, the existing foreground ceiling.
	if len(restoreFileEvents) != 0 {
		t.Errorf("source-less restore/auth unexpectedly touched the file provider: %+v", smartRecoveryProviderVector(restoreFileEvents))
	}
	if wholeModeled.Requests != restoreModeled.Requests+modeled.Requests || wholeModeled.RequestBytes != restoreModeled.RequestBytes+modeled.RequestBytes || wholeModeled.ResponseBytes != restoreModeled.ResponseBytes+modeled.ResponseBytes {
		t.Fatal("whole-journey provider accounting did not exactly partition into restore/auth and foreground recovery")
	}
	t.Logf("smart-recovery-scale total=%d complete-before=%d remaining=%d actual-byte-transfers=%d repeated=0 admission=%d exact=%d sizes=%d initial-size-body-bytes=%d directories=%d completion-pages=%d state-provider=%d file-provider=%d data-provider=%d rendered=%d replica-requests=%d/%d total-millis=%d recovery-millis=%d", total, completed, remaining, len(writes.snapshot()), admissions, exact, sizes, initialSizeBytes.Load(), preparation, completionPages, len(stateEvents), len(fileEvents), dataRequests, rendered, h.replicas[0].Load(), h.replicas[1].Load(), time.Since(testStarted).Milliseconds(), journeyFinished.Sub(recoveryStarted).Milliseconds())
	t.Logf("smart-recovery-provider-model phase=foreground-reconnect-to-complete profile=%s browserRequests=%d requests=%d requestBytes=%d responseBytes=%d costPicoUSD=%d aggregateP50Micros=%d aggregateP95Micros=%d aggregateP99Micros=%d (fixture model, not observed GCS latency)", economics.Profile(), len(requests), modeled.Requests, modeled.RequestBytes, modeled.ResponseBytes, modeled.CostPicoUSD, modeled.P50Micros, modeled.P95Micros, modeled.P99Micros)
	t.Logf("smart-recovery-provider-model phase=restore-auth profile=%s browserRequests=%d stateRequests=%d fileRequests=%d requests=%d requestBytes=%d responseBytes=%d costPicoUSD=%d aggregateP50Micros=%d aggregateP95Micros=%d aggregateP99Micros=%d (separate measured restore/auth ratchet; fixture model)", economics.Profile(), len(restoreRequests), len(restoreStateEvents), len(restoreFileEvents), restoreModeled.Requests, restoreModeled.RequestBytes, restoreModeled.ResponseBytes, restoreModeled.CostPicoUSD, restoreModeled.P50Micros, restoreModeled.P95Micros, restoreModeled.P99Micros)
	t.Logf("smart-recovery-provider-model phase=whole-reload-logout-login-restore-reconnect-to-complete profile=%s browserRequests=%d stateRequests=%d fileRequests=%d dataRequests=%d requests=%d requestBytes=%d responseBytes=%d costPicoUSD=%d aggregateP50Micros=%d aggregateP95Micros=%d aggregateP99Micros=%d restore-auth-millis=%d journey-millis=%d (fixture setup/probes and final independent verification excluded)", economics.Profile(), len(wholeRequests), len(wholeStateEvents), len(wholeFileEvents), remaining, wholeModeled.Requests, wholeModeled.RequestBytes, wholeModeled.ResponseBytes, wholeModeled.CostPicoUSD, wholeModeled.P50Micros, wholeModeled.P95Micros, wholeModeled.P99Micros, recoveryStarted.Sub(journeyStarted).Milliseconds(), journeyFinished.Sub(journeyStarted).Milliseconds())
	t.Logf("smart-recovery-provider-roles-kinds phase=restore-auth roles=%+v kinds=%+v", restoreModeled.ByRole, restoreModeled.ByKind)
	t.Logf("smart-recovery-provider-roles-kinds phase=whole roles=%+v kinds=%+v", wholeModeled.ByRole, wholeModeled.ByKind)
	t.Logf("smart-recovery-actual-http phase=restore-auth controlRequests=%d dataRequests=0 totalRequests=%d", foregroundControlHTTPStart-journeyControlHTTPStart, foregroundControlHTTPStart-journeyControlHTTPStart)
	t.Logf("smart-recovery-actual-http phase=foreground controlRequests=%d dataPATCH=%d dataOPTIONS=%d totalRequests=%d", journeyControlHTTPEnd-foregroundControlHTTPStart, wholeDataPATCHRequests-restorePATCHRequests, wholeDataOPTIONSRequests-restoreOPTIONSRequests, journeyControlHTTPEnd-foregroundControlHTTPStart+wholeDataPATCHRequests-restorePATCHRequests+wholeDataOPTIONSRequests-restoreOPTIONSRequests)
	t.Logf("smart-recovery-actual-http phase=whole controlRequests=%d dataPATCH=%d dataOPTIONS=%d totalRequests=%d (browser resource events may also report static-cache hits)", journeyControlHTTPEnd-journeyControlHTTPStart, wholeDataPATCHRequests, wholeDataOPTIONSRequests, journeyControlHTTPEnd-journeyControlHTTPStart+wholeDataPATCHRequests+wholeDataOPTIONSRequests)
	t.Logf("smart-recovery-restore-auth-vector %+v routes=%v", smartRecoveryProviderVector(restoreEvents), smartRecoveryRouteHistogram(restoreRequests))
	t.Logf("smart-recovery-whole-vector %+v routes=%v", smartRecoveryProviderVector(wholeEvents), smartRecoveryRouteHistogram(wholeRequests))
	t.Logf("smart-recovery-state-vector phase=foreground %+v", smartRecoveryProviderVector(stateEvents))
	t.Logf("smart-recovery-pack-shape phase=foreground %+v", smartRecoveryPackShape(stateEvents))
	t.Logf("smart-recovery-route-histogram phase=foreground %v", smartRecoveryRouteHistogram(requests))
	admissionMu.Lock()
	t.Logf("smart-recovery-action-page-sizes %v pages-at-least-15s=%d maximum-page-millis=%d", actionPageSizes, actionSlowPages, actionMaximumMillis)
	admissionMu.Unlock()
	if len(stateEvents) > stateRequestBudget || stateWrites > stateWriteBudget || stateReads > stateReadBudget {
		t.Errorf("scale state-provider regression budget: requests=%d/%d written=%d/%d read=%d/%d", len(stateEvents), stateRequestBudget, stateWrites, stateWriteBudget, stateReads, stateReadBudget)
	}
	if admissions != 27 || exact != 27 || sizes != 28 || preparation != 1 || completionPages != 27 || legacyCompletionPages != 0 {
		t.Errorf("scale control budget: admission=%d exact=%d sizes=%d directories=%d completion=%d legacy-completion=%d; want27/27/28/1/27/0", admissions, exact, sizes, preparation, completionPages, legacyCompletionPages)
	}
	if modeled.Requests > stateRequestBudget+3*remaining || modeled.RequestBytes > stateWriteBudget+4*remaining || modeled.ResponseBytes > stateReadBudget {
		t.Errorf("foreground reconnect-to-complete exceeded provider vector: requests=%d written=%d read=%d", modeled.Requests, modeled.RequestBytes, modeled.ResponseBytes)
	}
	if len(restoreStateEvents) > restoreStateRequestBudget || restoreModeled.RequestBytes > restoreStateWriteBudget || restoreModeled.ResponseBytes > restoreStateReadBudget {
		t.Errorf("restore/auth state-provider regression budget: requests=%d/%d written=%d/%d read=%d/%d", len(restoreStateEvents), restoreStateRequestBudget, restoreModeled.RequestBytes, restoreStateWriteBudget, restoreModeled.ResponseBytes, restoreStateReadBudget)
	}
	if len(wholeStateEvents) > restoreStateRequestBudget+stateRequestBudget || wholeModeled.Requests > restoreStateRequestBudget+stateRequestBudget+3*remaining || wholeModeled.RequestBytes > restoreStateWriteBudget+stateWriteBudget+4*remaining || wholeModeled.ResponseBytes > restoreStateReadBudget+stateReadBudget {
		t.Errorf("whole reload/login/recovery exceeded additive provider vector: state=%d/%d requests=%d/%d written=%d/%d read=%d/%d", len(wholeStateEvents), restoreStateRequestBudget+stateRequestBudget, wholeModeled.Requests, restoreStateRequestBudget+stateRequestBudget+3*remaining, wholeModeled.RequestBytes, restoreStateWriteBudget+stateWriteBudget+4*remaining, wholeModeled.ResponseBytes, restoreStateReadBudget+stateReadBudget)
	}
	restoreControlHTTP := foregroundControlHTTPStart - journeyControlHTTPStart
	foregroundControlHTTP := journeyControlHTTPEnd - foregroundControlHTTPStart
	wholeControlHTTP := journeyControlHTTPEnd - journeyControlHTTPStart
	wholeHTTP := wholeControlHTTP + wholeDataPATCHRequests + wholeDataOPTIONSRequests
	if restoreControlHTTP > restoreControlHTTPBudget || foregroundControlHTTP > foregroundControlHTTPBudget || wholeControlHTTP > restoreControlHTTPBudget+foregroundControlHTTPBudget || wholeHTTP > restoreControlHTTPBudget+foregroundControlHTTPBudget+2*remaining {
		t.Errorf("actual HTTP regression budget: restore-control=%d/%d foreground-control=%d/%d whole-control=%d/%d whole-total=%d/%d", restoreControlHTTP, restoreControlHTTPBudget, foregroundControlHTTP, foregroundControlHTTPBudget, wholeControlHTTP, restoreControlHTTPBudget+foregroundControlHTTPBudget, wholeHTTP, restoreControlHTTPBudget+foregroundControlHTTPBudget+2*remaining)
	}
	admissionMu.Lock()
	pageSizes := make(map[int]int, len(actionPageSizes))
	for size, count := range actionPageSizes {
		pageSizes[size] = count
	}
	slowPages, maximumPageMillis := actionSlowPages, actionMaximumMillis
	admissionMu.Unlock()
	writeSmartRecoveryEvidence(t, smartRecoveryScaleEvidence{
		SchemaVersion: 1,
		Provenance:    "Synthetic four-byte fixture; provider cost and aggregate latency are fixture-model estimates, not observed GCS cost or latency. Actual HTTP is measured separately.",
		Boundary:      "After authoritative fixture, projection probes and ledger setup; through real reload/logout/passkey login/workspace restore/reconnect/planning/admission/data/completion; final independent verification excluded from phase totals.",
		ModelProfile:  economics.Profile(),
		TotalFiles:    total, CompletedBefore: completed, RemainingFiles: remaining, BytesPerFile: 4,
		AuthoritativeFilesVerified: total, ActualFilesTransferredOnce: remaining, CompletedFilesReadmitted: 0, CompletedFilesRetransferred: 0,
		MembershipAndSampleBytesVerified: true, DurableHistoryVerified: true, Progress: progress,
		InitialSizeBodyBytes: initialSizeBytes.Load(), ActionPageSizes: pageSizes, ActionPagesAtLeast15Seconds: slowPages, MaximumActionPageMillis: maximumPageMillis,
		TestMillis: time.Since(testStarted).Milliseconds(),
		Restore: smartRecoveryPhaseEvidence{
			BrowserRequests: len(restoreRequests), StateRequests: len(restoreStateEvents), FileRequests: len(restoreFileEvents),
			Model: restoreModeled, ProviderVector: smartRecoveryProviderVector(restoreEvents), PackShape: smartRecoveryPackShape(restoreStateEvents), Routes: smartRecoveryRouteHistogram(restoreRequests),
			HTTP: smartRecoveryHTTPEvidence{ControlRequests: restoreControlHTTP, TotalRequests: restoreControlHTTP}, DurationMillis: recoveryStarted.Sub(journeyStarted).Milliseconds(),
			Budget: smartRecoveryBudgetEvidence{StateRequests: restoreStateRequestBudget, ProviderRequests: restoreStateRequestBudget, WriteBytes: restoreStateWriteBudget, ReadBytes: restoreStateReadBudget, ActualControlHTTP: restoreControlHTTPBudget, ActualTotalHTTP: restoreControlHTTPBudget},
		},
		Foreground: smartRecoveryPhaseEvidence{
			BrowserRequests: len(requests), StateRequests: len(stateEvents), FileRequests: len(fileEvents), DataTransfers: remaining,
			Model: modeled, ProviderVector: smartRecoveryProviderVector(modeledEvents), PackShape: smartRecoveryPackShape(stateEvents), Routes: smartRecoveryRouteHistogram(requests),
			HTTP: smartRecoveryHTTPEvidence{ControlRequests: foregroundControlHTTP, DataPATCH: wholeDataPATCHRequests - restorePATCHRequests, DataOPTIONS: wholeDataOPTIONSRequests - restoreOPTIONSRequests, TotalRequests: foregroundControlHTTP + 2*remaining}, DurationMillis: journeyFinished.Sub(recoveryStarted).Milliseconds(),
			Budget: smartRecoveryBudgetEvidence{StateRequests: stateRequestBudget, ProviderRequests: stateRequestBudget + 3*remaining, WriteBytes: stateWriteBudget + 4*remaining, ReadBytes: stateReadBudget, ActualControlHTTP: foregroundControlHTTPBudget, ActualTotalHTTP: foregroundControlHTTPBudget + 2*remaining},
		},
		Whole: smartRecoveryPhaseEvidence{
			BrowserRequests: len(wholeRequests), StateRequests: len(wholeStateEvents), FileRequests: len(wholeFileEvents), DataTransfers: remaining,
			Model: wholeModeled, ProviderVector: smartRecoveryProviderVector(wholeEvents), PackShape: smartRecoveryPackShape(wholeStateEvents), Routes: smartRecoveryRouteHistogram(wholeRequests),
			HTTP: smartRecoveryHTTPEvidence{ControlRequests: wholeControlHTTP, DataPATCH: wholeDataPATCHRequests, DataOPTIONS: wholeDataOPTIONSRequests, TotalRequests: wholeHTTP}, DurationMillis: journeyFinished.Sub(journeyStarted).Milliseconds(),
			Budget: smartRecoveryBudgetEvidence{StateRequests: restoreStateRequestBudget + stateRequestBudget, ProviderRequests: restoreStateRequestBudget + stateRequestBudget + 3*remaining, WriteBytes: restoreStateWriteBudget + stateWriteBudget + 4*remaining, ReadBytes: restoreStateReadBudget + stateReadBudget, ActualControlHTTP: restoreControlHTTPBudget + foregroundControlHTTPBudget, ActualTotalHTTP: restoreControlHTTPBudget + foregroundControlHTTPBudget + 2*remaining},
		},
	})
}

type smartRecoveryScaleEvidence struct {
	SchemaVersion                    int                           `json:"schema_version"`
	Provenance                       string                        `json:"provenance"`
	Boundary                         string                        `json:"boundary"`
	ModelProfile                     string                        `json:"model_profile"`
	TotalFiles                       int                           `json:"total_files"`
	CompletedBefore                  int                           `json:"completed_before"`
	RemainingFiles                   int                           `json:"remaining_files"`
	BytesPerFile                     int                           `json:"bytes_per_file"`
	AuthoritativeFilesVerified       int                           `json:"authoritative_files_verified"`
	ActualFilesTransferredOnce       int                           `json:"actual_files_transferred_once"`
	CompletedFilesReadmitted         int                           `json:"completed_files_readmitted"`
	CompletedFilesRetransferred      int                           `json:"completed_files_retransferred"`
	MembershipAndSampleBytesVerified bool                          `json:"membership_and_sample_bytes_verified"`
	DurableHistoryVerified           bool                          `json:"durable_history_verified"`
	Progress                         smartRecoveryProgressEvidence `json:"progress"`
	InitialSizeBodyBytes             int64                         `json:"initial_size_body_bytes"`
	ActionPageSizes                  map[int]int                   `json:"action_page_sizes"`
	ActionPagesAtLeast15Seconds      int64                         `json:"action_pages_at_least_15_seconds"`
	MaximumActionPageMillis          int64                         `json:"maximum_action_page_millis"`
	TestMillis                       int64                         `json:"test_millis"`
	Restore                          smartRecoveryPhaseEvidence    `json:"restore_auth"`
	Foreground                       smartRecoveryPhaseEvidence    `json:"foreground_recovery"`
	Whole                            smartRecoveryPhaseEvidence    `json:"whole_journey"`
}

type smartRecoveryProgressEvidence struct {
	Minimum      int  `json:"minimum"`
	Maximum      int  `json:"maximum"`
	Intermediate bool `json:"intermediate"`
	MaxRendered  int  `json:"maxRendered"`
}

type smartRecoveryPhaseEvidence struct {
	BrowserRequests int                                    `json:"browser_resource_events"`
	StateRequests   int                                    `json:"state_provider_requests"`
	FileRequests    int                                    `json:"file_provider_requests"`
	DataTransfers   int                                    `json:"data_transfers"`
	Model           providerbudget.Totals                  `json:"provider_model"`
	ProviderVector  map[string]smartRecoveryProviderTotals `json:"provider_vector"`
	PackShape       smartRecoveryPackTotals                `json:"state_pack_shape"`
	Routes          map[string]int                         `json:"browser_route_histogram"`
	HTTP            smartRecoveryHTTPEvidence              `json:"actual_http"`
	DurationMillis  int64                                  `json:"duration_millis"`
	Budget          smartRecoveryBudgetEvidence            `json:"regression_budget"`
}

type smartRecoveryHTTPEvidence struct {
	ControlRequests int64 `json:"control_requests"`
	DataPATCH       int64 `json:"data_patch"`
	DataOPTIONS     int64 `json:"data_options"`
	TotalRequests   int64 `json:"total_requests"`
}

type smartRecoveryBudgetEvidence struct {
	StateRequests     int   `json:"state_provider_requests"`
	ProviderRequests  int64 `json:"provider_requests"`
	WriteBytes        int64 `json:"provider_write_bytes"`
	ReadBytes         int64 `json:"provider_read_bytes"`
	ActualControlHTTP int64 `json:"actual_control_http"`
	ActualTotalHTTP   int64 `json:"actual_total_http"`
}

func writeSmartRecoveryEvidence(t *testing.T, evidence smartRecoveryScaleEvidence) {
	t.Helper()
	path := os.Getenv("ENDLESSFS_SMART_RECOVERY_EVIDENCE")
	if path == "" || t.Failed() {
		return
	}
	if !filepath.IsAbs(path) {
		t.Fatal("ENDLESSFS_SMART_RECOVERY_EVIDENCE must be an absolute output file")
	}
	body, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Fatal("could not encode synthetic smart-recovery evidence")
	}
	if err := os.WriteFile(path, append(body, '\n'), 0600); err != nil {
		t.Fatal("could not write requested synthetic smart-recovery evidence")
	}
}

func requireSmartRecoveryE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
}

func newSmartRecoveryBrowser(t *testing.T) *testBrowser {
	t.Helper()
	options := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromiumPath(t)), chromedp.UserDataDir(t.TempDir()),
		chromedp.Flag("disable-background-networking", true), chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-sync", true), chromedp.Flag("no-first-run", true), chromedp.Flag("no-default-browser-check", true))
	if os.Getenv("ENDLESSFS_CHROMIUM_NO_SANDBOX") == "1" {
		options = append(options, chromedp.NoSandbox, chromedp.CombinedOutput(os.Stderr))
	}
	allocator, cancelAllocator := chromedp.NewExecAllocator(context.Background(), options...)
	ctx, cancelBrowser := chromedp.NewContext(allocator)
	ctx, cancelTimeout := context.WithTimeout(ctx, 20*time.Minute)
	authenticationLog := &smartRecoveryAuthenticationLog{}
	ctx = context.WithValue(ctx, smartRecoveryAuthenticationLogKey{}, authenticationLog)
	client := &testBrowser{ctx: ctx}
	t.Cleanup(func() { _ = chromedp.Cancel(ctx); cancelTimeout(); cancelBrowser(); cancelAllocator() })
	chromedp.ListenTarget(ctx, func(event any) {
		if request, ok := event.(*network.EventRequestWillBeSent); ok {
			parsed, err := url.Parse(request.Request.URL)
			if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
				client.mu.Lock()
				client.origins = append(client.origins, parsed.Scheme+"://"+parsed.Host)
				client.requests = append(client.requests, request.Request.Method+" "+parsed.Path)
				client.mu.Unlock()
				authenticationLog.record(parsed.Path, 0)
				authenticationLog.requestStarted(request.RequestID, parsed.Path)
			}
		}
		if response, ok := event.(*network.EventResponseReceived); ok {
			if parsed, err := url.Parse(response.Response.URL); err == nil {
				authenticationLog.record(parsed.Path, int(response.Response.Status))
			}
		}
		if finished, ok := event.(*network.EventLoadingFinished); ok {
			authenticationLog.requestFinished(finished.RequestID)
		}
		if failed, ok := event.(*network.EventLoadingFailed); ok {
			authenticationLog.requestFinished(failed.RequestID)
		}
	})
	if err := chromedp.Run(ctx, network.Enable(), runtime.Enable(), emulation.SetDeviceMetricsOverride(800, 600, 1, false), chromedp.Navigate("about:blank"), cdpwebauthn.Enable(), chromedp.ActionFunc(func(ctx context.Context) error { _, err := addVirtualAuthenticator(ctx); return err })); err != nil {
		t.Fatal(err)
	}
	return client
}

func bootstrapSmartRecovery(t *testing.T, client *testBrowser, h harness) {
	t.Helper()
	parent := client.ctx
	bounded, cancel := context.WithTimeout(parent, 30*time.Second)
	client.ctx = bounded
	defer func() { client.ctx = parent; cancel() }()
	bootstrapBrowser(t, client, h)
}

func openSmartRecoverySheet(t *testing.T, client *testBrowser) {
	t.Helper()
	if err := waitFor(client.ctx, `(()=>{const panel=document.querySelector('#transfer-panel');if(panel.hidden){document.querySelector('#open-transfers').click();return false;}return true;})()`, 5*time.Second); err != nil {
		t.Fatalf("restored transfer sheet did not open: %v %s", err, smartRecoveryDiagnostic(client))
	}
}

func expandSmartRecoveryScaleHistory(t *testing.T, client *testBrowser) {
	t.Helper()
	if err := waitFor(client.ctx, `(()=>{const panel=document.querySelector('#transfer-panel');if(panel.hidden){document.querySelector('#open-transfers').click();return false;}const all=document.querySelector('[data-transfer-count="all"]').closest('[role="tab"]');if(all.getAttribute('aria-selected')!=='true'){all.click();return false;}const expand=document.querySelector('button[aria-label="Expand upload group scale-folder"]');if(expand){expand.click();return false;}return Boolean(document.querySelector('button[aria-label="Collapse upload group scale-folder"]'))&&Number(document.querySelector('#transfer-list').dataset.itemCount)>=10604&&document.querySelectorAll('#transfer-list .transfer-row').length>1;})()`, 5*time.Second); err != nil {
		t.Fatalf("full scale history did not visibly expand: %v %s", err, smartRecoveryDiagnostic(client))
	}
}

func refreshAndLoginSmartRecovery(t *testing.T, client *testBrowser, total int) {
	t.Helper()
	authenticationLog := client.ctx.Value(smartRecoveryAuthenticationLogKey{}).(*smartRecoveryAuthenticationLog)
	authenticationStart := authenticationLog.position()
	var previousTimeOrigin float64
	if err := runStage(client.ctx, 10*time.Second, chromedp.Evaluate(`performance.timeOrigin`, &previousTimeOrigin), chromedp.Reload()); err != nil {
		t.Fatalf("refresh command failed: %v %s", err, smartRecoveryDiagnostic(client))
	}
	refreshed := fmt.Sprintf(`performance.timeOrigin>%.6f&&document.readyState==='complete'&&document.querySelector('#loading-view').hidden&&!document.querySelector('#authenticated-view').hidden&&document.querySelector('#auth-view').hidden&&document.querySelector('#app').dataset.state==='authenticated'`, previousTimeOrigin)
	if err := waitFor(client.ctx, refreshed, 15*time.Second); err != nil {
		t.Fatalf("workspace did not become visible after refresh: %v %s", err, smartRecoveryDiagnostic(client))
	}
	if err := waitFor(client.ctx, fmt.Sprintf(`Number(document.querySelector('[data-transfer-count="all"]').textContent.replace(/[^0-9]/g,''))===%d && Boolean(document.querySelector('button[aria-label^="Reconnect upload source for"]'))`, total), 15*time.Second); err != nil {
		t.Fatalf("transfer source controls did not restore after refresh: %v %s", err, smartRecoveryDiagnostic(client))
	}
	if err := closeTransferSheet(client.ctx); err != nil {
		t.Fatalf("restored transfer sheet could not close: %v %s", err, smartRecoveryDiagnostic(client))
	}
	if err := runStage(client.ctx, 10*time.Second, chromedp.Click("#logout-button", chromedp.ByQuery)); err != nil {
		t.Fatalf("logout button could not activate: %v %s", err, smartRecoveryDiagnostic(client))
	}
	if err := waitVisible(client.ctx, "#auth-view", 10*time.Second); err != nil {
		t.Fatalf("authentication view did not appear after logout: %v %s", err, smartRecoveryDiagnostic(client))
	}
	t.Logf("smart-recovery-auth stage=ready-to-sign-in state=%s routes=%v", smartRecoveryAuthenticationDiagnostic(client), authenticationLog.snapshot(authenticationStart))
	if err := runStage(client.ctx, 10*time.Second,
		chromedp.Evaluate(`window.__smartRecoverySourceBeforeLogin=document.querySelector('button[aria-label^="Reconnect upload source for"]')`, nil),
		chromedp.Click("#signin-button", chromedp.ByQuery)); err != nil {
		t.Fatalf("sign-in button could not activate: %v %s", err, smartRecoveryDiagnostic(client))
	}
	t.Logf("smart-recovery-auth stage=activation-returned state=%s routes=%v", smartRecoveryAuthenticationDiagnostic(client), authenticationLog.snapshot(authenticationStart))
	// The drive node and old source controls survive inside the hidden
	// authenticated shell during logout. Require a completed application entry
	// and a fresh ledger render, rather than accepting their stale inner state.
	ready := fmt.Sprintf(`(()=>{const source=document.querySelector('button[aria-label^="Reconnect upload source for"]'),previous=window.__smartRecoverySourceBeforeLogin;return document.querySelector('#loading-view').hidden&&!document.querySelector('#authenticated-view').hidden&&document.querySelector('#auth-view').hidden&&document.querySelector('#app').dataset.state==='authenticated'&&!document.querySelector('#signin-button').disabled&&Boolean(source)&&source!==previous&&(!previous||!previous.isConnected)&&Number(document.querySelector('[data-transfer-count="all"]').textContent.replace(/[^0-9]/g,''))===%d;})()`, total)
	if err := waitFor(client.ctx, ready, 15*time.Second); err != nil {
		t.Fatalf("actual passkey login after logout failed: %v %s auth=%s routes=%v", err, browserStatus(client.ctx), smartRecoveryAuthenticationDiagnostic(client), authenticationLog.snapshot(authenticationStart))
	}
	routes := authenticationLog.snapshot(authenticationStart)
	for route, status := range map[string]int{"/api/v1/authentication/options": 200, "/api/v1/authentication/verify": 200, "/api/v1/logout": 204} {
		if routes[route]["requests"] != 1 || routes[route][fmt.Sprintf("status-%d", status)] != 1 {
			t.Fatalf("actual logout/passkey ceremony did not complete exactly once: routes=%v", routes)
		}
	}
	if routes["/api/v1/me"]["requests"] != 2 || routes["/api/v1/me"]["status-200"] != 2 {
		t.Fatalf("refresh and actual login did not confirm the account: routes=%v", routes)
	}
	t.Logf("smart-recovery-auth stage=authenticated-and-restored routes=%v", routes)
}

func smartRecoveryAuthenticationDiagnostic(client *testBrowser) string {
	ctx, cancel := context.WithTimeout(client.ctx, time.Second)
	defer cancel()
	var result string
	_ = chromedp.Run(ctx, chromedp.Evaluate(`JSON.stringify({signInDisabled:document.querySelector('#signin-button').disabled,signInText:document.querySelector('#signin-button').textContent.slice(0,80),activeControl:(document.activeElement?.id||'').slice(0,80),documentFocused:document.hasFocus(),authenticatedHidden:document.querySelector('#authenticated-view').hidden,driveHidden:document.querySelector('#drive-view').hidden,appState:(document.querySelector('#app').dataset.state||'').slice(0,32),sheetHidden:document.querySelector('#transfer-panel').hidden})`, &result))
	return result
}

type smartRecoveryAuthenticationLogKey struct{}

type smartRecoveryAuthenticationLog struct {
	mu                sync.Mutex
	base              int
	events            []smartRecoveryAuthenticationEvent
	active            map[network.RequestID]struct{}
	lastNetworkChange time.Time
}

func (log *smartRecoveryAuthenticationLog) requestStarted(id network.RequestID, path string) {
	if !strings.HasPrefix(path, "/api/v1/") {
		return
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.active == nil {
		log.active = make(map[network.RequestID]struct{})
	}
	log.active[id] = struct{}{}
	log.lastNetworkChange = time.Now()
}

func (log *smartRecoveryAuthenticationLog) requestFinished(id network.RequestID) {
	log.mu.Lock()
	defer log.mu.Unlock()
	if _, found := log.active[id]; found {
		delete(log.active, id)
		log.lastNetworkChange = time.Now()
	}
}

func waitSmartRecoveryNetworkIdle(t *testing.T, client *testBrowser) {
	t.Helper()
	// Final directory refreshes are application work too. Observe a short quiet
	// window before cutting phases or invoking independent engine oracles, so
	// their requests cannot be counted while their provider work is omitted.
	log := client.ctx.Value(smartRecoveryAuthenticationLogKey{}).(*smartRecoveryAuthenticationLog)
	ctx, cancel := context.WithTimeout(client.ctx, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		log.mu.Lock()
		pending, quiet := len(log.active), time.Since(log.lastNetworkChange)
		log.mu.Unlock()
		if pending == 0 && quiet >= 100*time.Millisecond {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("synthetic upload application did not quiesce for complete provider accounting: pending=%d quiet-millis=%d err=%v", pending, quiet.Milliseconds(), ctx.Err())
		case <-ticker.C:
		}
	}
}

type smartRecoveryAuthenticationEvent struct {
	route  string
	status int
}

func (log *smartRecoveryAuthenticationLog) record(route string, status int) {
	switch route {
	case "/api/v1/authentication/options", "/api/v1/authentication/verify", "/api/v1/me", "/api/v1/logout":
		log.mu.Lock()
		event := smartRecoveryAuthenticationEvent{route: route, status: status}
		if len(log.events) == 256 {
			copy(log.events, log.events[1:])
			log.events[len(log.events)-1] = event
			log.base++
		} else {
			log.events = append(log.events, event)
		}
		log.mu.Unlock()
	}
}

func (log *smartRecoveryAuthenticationLog) position() int {
	log.mu.Lock()
	defer log.mu.Unlock()
	return log.base + len(log.events)
}

func (log *smartRecoveryAuthenticationLog) snapshot(start int) map[string]map[string]int {
	log.mu.Lock()
	defer log.mu.Unlock()
	result := make(map[string]map[string]int)
	for _, event := range log.events[max(0, start-log.base):] {
		if result[event.route] == nil {
			result[event.route] = make(map[string]int)
		}
		field := "requests"
		if event.status != 0 {
			field = fmt.Sprintf("status-%d", event.status)
		}
		result[event.route][field]++
	}
	return result
}

func reconnectSmartRecovery(t *testing.T, client *testBrowser, root string) {
	t.Helper()
	// Ledger restoration can finish after the workspace becomes interactive;
	// its final render closes the sheet. Wait for the source action to exist,
	// then open the completed projection before activating the real control.
	if err := waitFor(client.ctx, `Boolean(document.querySelector('button[aria-label^="Reconnect upload source for"]'))`, 10*time.Second); err != nil {
		t.Fatalf("reconnect action missing: %v %s", err, smartRecoveryDiagnostic(client))
	}
	openSmartRecoverySheet(t, client)
	if err := waitVisible(client.ctx, `button[aria-label^="Reconnect upload source for"]`, 5*time.Second); err != nil {
		t.Fatalf("restored reconnect action was not visible: %v %s", err, smartRecoveryDiagnostic(client))
	}
	if err := runStage(client.ctx, 10*time.Second, chromedp.Click(`button[aria-label^="Reconnect upload source for"]`, chromedp.ByQuery), chromedp.SetUploadFiles("#folder-input", []string{root}, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
}

func smartRecoveryScope(t *testing.T, h *smartRecoveryHarness) domain.Scope {
	t.Helper()
	accounts, err := h.repository.Accounts(context.Background())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("registered account: count=%d err=%v", len(accounts), err)
	}
	scope, err := domain.NewScope(accounts[0].UserID, domain.AreaLive)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func smartRecoveryRelative(root string, index int) string {
	return fmt.Sprintf("%s/existing-%02d/nested/source-%05d.bin", root, index%43, index)
}

func smartRecoveryBody(index int) []byte {
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body, uint32(index))
	return body
}

func createSmartRecoverySources(t *testing.T, name string, count int) string {
	t.Helper()
	parent := t.TempDir()
	for index := range count {
		path := filepath.Join(parent, smartRecoveryRelative(name, index))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, smartRecoveryBody(index), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(parent, name)
}

func prepareSmartRecoveryDirectories(t *testing.T, h *smartRecoveryHarness, scope domain.Scope, root string, count int) {
	t.Helper()
	paths := []domain.UserPath{domain.MustParseUserPath("/" + root)}
	for index := range min(43, count) {
		paths = append(paths, domain.MustParseUserPath(fmt.Sprintf("/%s/existing-%02d", root, index)), domain.MustParseUserPath(fmt.Sprintf("/%s/existing-%02d/nested", root, index)))
	}
	for _, path := range paths {
		if _, err := h.engines[0].Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: path, Conflict: domain.ConflictFail}); err != nil {
			t.Fatal(err)
		}
	}
}

func seedSmartRecoveryAnchor(t *testing.T, h *smartRecoveryHarness, scope domain.Scope, index int) {
	t.Helper()
	path := domain.MustParseUserPath(fmt.Sprintf("/anchor-%d.bin", index))
	body := smartRecoveryBody(100000 + index)
	capability, err := h.engines[0].Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: path, Size: 4, MediaType: "application/octet-stream", Conflict: domain.ConflictFail, Resumable: true, IdempotencyKey: fmt.Sprintf("anchor-%d", index)})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(capability.Method, capability.URL, bytes.NewReader(body))
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	h.fileBase.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("anchor data status=%d", recorder.Code)
	}
	if _, err := h.engines[1].Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: 4, MediaType: "application/octet-stream", CRC32C: integrity.CRC32C(body)}); err != nil {
		t.Fatal(err)
	}
}

func deleteSmartRecoveryProjection(t *testing.T, h *smartRecoveryHarness, owner domain.UserID) {
	t.Helper()
	projectionID := storageformat.Digest([]byte("endlessfs-upload-planning-projection-v1\x00" + owner.String()))
	key := storageformat.ScopedProjectionHeadKey(owner.String(), storageformat.ProjectionDuplicates, projectionID)
	head, err := h.stateBase.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.stateBase.Delete(context.Background(), key, objectstore.DeleteCondition{Version: head.Version}); err != nil {
		t.Fatal(err)
	}
}

func seedSmartRecoveryCompletedFiles(t *testing.T, h *smartRecoveryHarness, scope domain.Scope, root string, count int) {
	t.Helper()
	requests := make([]domain.CreateUploadRequest, count)
	for index := range requests {
		requests[index] = domain.CreateUploadRequest{Path: domain.MustParseUserPath("/" + smartRecoveryRelative(root, index)), Size: 4, MediaType: "application/octet-stream", Conflict: domain.ConflictFail, Resumable: true, IdempotencyKey: fmt.Sprintf("seed-upload-%05d", index)}
	}
	// Keep fixture admission and publication at the same 100-item envelope
	// shape as the production browser lifecycle.
	for start := 0; start < count; start += 100 {
		end := min(start+100, count)
		caps, err := h.engines[0].Files().CreateUploadBatch(context.Background(), scope, requests[start:end])
		if err != nil || len(caps) != end-start {
			t.Fatalf("authoritative seed admission %d: caps=%d err=%v", start, len(caps), err)
		}
		items := make([]domain.CompleteUploadBatchItem, len(caps))
		for offset, capability := range caps {
			index := start + offset
			body := smartRecoveryBody(index)
			request := httptest.NewRequest(capability.Method, capability.URL, bytes.NewReader(body))
			for name, value := range capability.Headers {
				request.Header.Set(name, value)
			}
			recorder := httptest.NewRecorder()
			h.fileBase.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusNoContent {
				t.Fatalf("seed actual data transfer %d: status=%d", index, recorder.Code)
			}
			items[offset] = domain.CompleteUploadBatchItem{UploadID: capability.UploadID, CRC32C: integrity.CRC32C(body)}
		}
		result, err := h.engines[1].Files().CompleteUploadBatch(context.Background(), scope, domain.CompleteUploadBatchRequest{Items: items, IdempotencyKey: fmt.Sprintf("seed-completion-%05d", start)})
		if err != nil || len(result.Entries) != len(caps) {
			t.Fatalf("authoritative seed completion %d on second replica: entries=%d err=%v", start, len(result.Entries), err)
		}
		if end%2000 == 0 || end == count {
			t.Logf("smart-recovery authoritative completed fixture=%d/%d", end, count)
		}
	}
}

func seedSmartRecoveryLedger(t *testing.T, client *testBrowser, owner domain.UserID, root string, total, completed int) {
	t.Helper()
	script := fmt.Sprintf(`(async()=>{const ownerID=%q,root=%q;
		const db=await new Promise((resolve,reject)=>{const r=indexedDB.open('endlessfs-transfer-ledger-v1');r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});
		const tx=db.transaction(['items','groups'],'readwrite'), ids=[],dirs=new Set([root]);
		for(let i=0;i<%d;i++){const id='smart-scale-'+i,relativeDirectory=root+'/existing-'+String(i%%43).padStart(2,'0')+'/nested',name='source-'+String(i).padStart(5,'0')+'.bin';ids.push(id);dirs.add(relativeDirectory);dirs.add(relativeDirectory.slice(0,relativeDirectory.lastIndexOf('/')));
		tx.objectStore('items').put({key:ownerID+':'+id,ownerID,id,groupID:'smart-scale-group',name,relativePath:relativeDirectory+'/'+name,relativeDirectory,directory:'/'+relativeDirectory,baseDirectory:'/',size:4,mediaType:'application/octet-stream',state:i<%d?'complete':'needs-source',confirmed:i<%d?4:0,uploadID:'',strategy:'smart-merge',planPhase:i<%d?'done':'size-pending',createdAt:i+1,error:i<%d?'':'Reconnect the source to continue.'});}
		tx.objectStore('groups').put({key:ownerID+':smart-scale-group',ownerID,id:'smart-scale-group',name:root,transferIDs:ids,directories:[...dirs],baseDirectory:'/',totalSize:%d*4,state:'queued',discoveryDone:true,strategy:'smart-merge',createdAt:1,error:'Reference: historical-reference.'});
		await new Promise((resolve,reject)=>{tx.oncomplete=resolve;tx.onerror=()=>reject(tx.error)});db.close();return true;})()`, owner.String(), root, total, completed, completed, completed, completed, total)
	if err := runStage(client.ctx, 10*time.Second, chromedp.Evaluate(script, nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
		t.Fatal(err)
	}
}

func assertSmartRecoveryProjectionStates(t *testing.T, h *smartRecoveryHarness, scope domain.Scope, root string, completed, remaining int) {
	t.Helper()
	items := make([]domain.UploadSizePlanItem, remaining)
	for index := range items {
		items[index] = domain.UploadSizePlanItem{ID: fmt.Sprintf("probe-%d", index), Path: domain.MustParseUserPath("/" + smartRecoveryRelative(root, completed+index)), Size: 4}
	}
	counts := make(map[string]int)
	var expected []domain.UploadSizePlanDecision
	// Projection readiness and process-local cache heat are distinct. The first
	// query on replica1 has a ready projection but a cold immutable-wire cache;
	// its later queries may fetch fewer packs without changing any decision.
	for _, probe := range []struct {
		stage   string
		replica int
	}{{"cold", 0}, {"warm", 1}, {"rebuilt", 0}, {"warm-other-replica", 1}, {"warm-repeat", 1}} {
		stage := probe.stage
		if stage == "rebuilt" {
			deleteSmartRecoveryProjection(t, h, scope.UserID())
		}
		h.stateLedger.Reset()
		h.fileLedger.Reset()
		plan, err := h.engines[probe.replica].Files().PlanUploadSizes(context.Background(), scope.UserID(), domain.UploadSizePlanRequest{Items: items})
		if err != nil || len(plan.Items) != remaining {
			t.Fatalf("%s authoritative size planning: items=%d err=%v", stage, len(plan.Items), err)
		}
		for index, item := range plan.Items {
			if item.ID != items[index].ID || !item.FingerprintRequired || item.TargetExists {
				t.Fatalf("%s fixture did not force bound content comparison for absent target: %+v", stage, item)
			}
			if expected != nil && item != expected[index] {
				t.Fatalf("%s replica changed authoritative planning decision at synthetic index%d", stage, index)
			}
		}
		if expected == nil {
			expected = append([]domain.UploadSizePlanDecision(nil), plan.Items...)
		}
		if len(h.fileLedger.Events()) != 0 {
			t.Fatalf("%s planning accessed file provider", stage)
		}
		for _, event := range h.stateLedger.Events() {
			if event.Kind != providerbudget.RequestObjectGet && event.Kind != providerbudget.RequestObjectPut {
				t.Fatalf("%s planning provider operation=%s", stage, event.Kind)
			}
			if strings.HasPrefix(stage, "warm") && event.Kind != providerbudget.RequestObjectGet {
				t.Fatalf("%s ready projection unexpectedly mutated provider state", stage)
			}
		}
		counts[stage] = len(h.stateLedger.Events())
		if stage == "warm-repeat" {
			projectionID := storageformat.Digest([]byte("endlessfs-upload-planning-projection-v1\x00" + scope.UserID().String()))
			wanted := map[string]bool{
				storageformat.DomainHeadKey(storageformat.DomainNamespace, scope.UserID().String()).String():                              false,
				storageformat.ScopedProjectionHeadKey(scope.UserID().String(), storageformat.ProjectionDuplicates, projectionID).String(): false,
			}
			for _, event := range h.stateLedger.Events() {
				key := event.Target
				if seen, expected := wanted[key]; !expected || seen {
					t.Fatal("hot planning did not read each expected authority head exactly once")
				}
				wanted[key] = true
			}
			for _, seen := range wanted {
				if !seen {
					t.Fatal("hot planning omitted a fresh authority head")
				}
			}
		}
	}
	for _, stage := range []string{"warm", "warm-other-replica"} {
		if counts[stage] >= counts["cold"] || counts[stage] >= counts["rebuilt"] {
			t.Fatalf("cold/rebuilt/warm provider consistency: %+v", counts)
		}
	}
	// A repeated query on the same replica still reads both mutable authority
	// heads, but must not redownload already verified immutable packs.
	if counts["warm-repeat"] != 2 {
		t.Fatalf("hot immutable planning did not retain fresh authority reads: %+v", counts)
	}
	for stage, maximum := range map[string]int{"cold": 8, "rebuilt": 10, "warm": 5, "warm-other-replica": 5, "warm-repeat": 2} {
		if counts[stage] > maximum {
			t.Fatalf("%s real projection planning exceeded measured request budget: %d/%d", stage, counts[stage], maximum)
		}
	}
	t.Logf("smart-recovery projection-states existing=%d remaining=%d provider-calls=%v file-provider=0", completed, remaining, counts)
}

func assertSmartRecoveryLedger(t *testing.T, client *testBrowser, total, complete int) {
	t.Helper()
	var valid bool
	script := fmt.Sprintf(`(async()=>{for(let attempt=0;attempt<500;attempt++){const db=await new Promise((resolve,reject)=>{const r=indexedDB.open('endlessfs-transfer-ledger-v1');r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});const tx=db.transaction('items','readonly');const items=await new Promise((resolve,reject)=>{const r=tx.objectStore('items').getAll();r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});db.close();if(items.length===%d&&items.filter(x=>x.state==='complete').length===%d)return true;await new Promise(resolve=>setTimeout(resolve,10));}return false})()`, total, complete)
	if err := runStage(client.ctx, 10*time.Second, chromedp.Evaluate(script, &valid, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil || !valid {
		t.Fatalf("durable ledger total=%d complete=%d: valid=%t err=%v", total, complete, valid, err)
	}
}

// Check before releasing the first real size request. Final completion alone
// would hide obsolete reconnect errors that persisted through preparation. The
// planner intentionally uses the message field for current informational text;
// a nonempty-field count is not an error-category oracle.
func assertSmartRecoveryReconnectedErrors(t *testing.T, client *testBrowser, total, completed int) {
	t.Helper()
	var proof struct {
		Valid             bool `json:"valid"`
		Items             int  `json:"items"`
		Complete          int  `json:"complete"`
		SourceRequired    int  `json:"sourceRequired"`
		ObsoleteReconnect int  `json:"obsoleteReconnect"`
		UnboundedHistory  int  `json:"unboundedHistory"`
	}
	script := fmt.Sprintf(`(async()=>{let proof;for(let attempt=0;attempt<500;attempt++){
		const db=await new Promise((resolve,reject)=>{const r=indexedDB.open('endlessfs-transfer-ledger-v1');r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});
		const tx=db.transaction('items','readonly');
		const items=await new Promise((resolve,reject)=>{const r=tx.objectStore('items').getAll();r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});db.close();
		proof={items:items.length,complete:items.filter(x=>x.state==='complete').length,sourceRequired:items.filter(x=>x.errorCode==='source_required').length,obsoleteReconnect:items.filter(x=>x.error==='Reconnect the source to continue.').length,unboundedHistory:items.filter(x=>(x.errorHistory||[]).length>3||(x.errorHistory||[]).some(h=>String(h.message||'').length>240||String(h.code||'').length>80)).length};
		proof.valid=proof.items===%d&&proof.complete===%d&&proof.sourceRequired===0&&proof.obsoleteReconnect===0&&proof.unboundedHistory===0;
		if(proof.valid)return proof;
		await new Promise(resolve=>setTimeout(resolve,10));}return proof})()`, total, completed)
	if err := runStage(client.ctx, 10*time.Second, chromedp.Evaluate(script, &proof, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil || !proof.Valid {
		t.Fatalf("reconnected items retained obsolete source errors or unbounded history: closed-counts=%+v err=%v", proof, err)
	}
}

func assertSmartRecoveryErrorHistory(t *testing.T, client *testBrowser) {
	t.Helper()
	var valid bool
	script := `(async()=>{for(let attempt=0;attempt<500;attempt++){
		const db=await new Promise((resolve,reject)=>{const r=indexedDB.open('endlessfs-transfer-ledger-v1');r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});
		const tx=db.transaction('groups','readonly');
		const groups=await new Promise((resolve,reject)=>{const r=tx.objectStore('groups').getAll();r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});db.close();
		const group=groups.find(x=>x.id==='smart-scale-group'),history=group?.errorHistory||[];
		if(group&&!group.error&&history.length<=3&&history.filter(x=>x.message==='Reference: historical-reference.').length===1&&history.every(x=>String(x.message||'').length<=240&&String(x.code||'').length<=80))return true;
		await new Promise(resolve=>setTimeout(resolve,10));}return false})()`
	if err := runStage(client.ctx, 10*time.Second, chromedp.Evaluate(script, &valid, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil || !valid {
		t.Fatalf("obsolete group error was not cleared with exactly one bounded historical reference retained: valid=%t err=%v", valid, err)
	}
}

func verifySmartRecoveryFiles(t *testing.T, h *smartRecoveryHarness, scope domain.Scope, root string, count int) {
	t.Helper()
	rootEntry, err := h.engines[1].Files().Stat(context.Background(), scope, domain.MustParseUserPath("/"+root))
	if err != nil || rootEntry.Kind != domain.EntryDirectory || rootEntry.Size != int64(count*4) || rootEntry.FileCount != int64(count) {
		t.Fatalf("completed folder aggregate: files=%d bytes=%d err=%v", rootEntry.FileCount, rootEntry.Size, err)
	}
	// List every nested directory through the second replica: no complete ledger
	// item can stand in for an authoritative namespace entry.
	seen := make(map[string]domain.Entry, count)
	for directory := range min(43, count) {
		cursor := ""
		for {
			page, err := h.engines[1].Files().List(context.Background(), scope, domain.ListRequest{Directory: domain.MustParseUserPath(fmt.Sprintf("/%s/existing-%02d/nested", root, directory)), PageSize: 1000, Cursor: cursor})
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range page.Entries {
				if entry.Kind != domain.EntryFile || entry.Size != 4 {
					t.Fatalf("incorrect authoritative result: %+v", entry)
				}
				seen[entry.Path.String()] = entry
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
	}
	if len(seen) != count {
		t.Fatalf("authoritative completed files=%d, want%d", len(seen), count)
	}
	for index := range count {
		path := domain.MustParseUserPath("/" + smartRecoveryRelative(root, index))
		if _, found := seen[path.String()]; !found {
			t.Fatalf("authoritative completion missing synthetic file %d", index)
		}
	}
	// Verify representative bodies from both previously completed and newly
	// completed cohorts through signed downloads, independent of ledger counters.
	for _, index := range []int{0, count / 2, count - 1} {
		path := domain.MustParseUserPath("/" + smartRecoveryRelative(root, index))
		capability, err := h.engines[1].Files().CreateDownload(context.Background(), scope, domain.CreateDownloadRequest{Path: path, Version: seen[path.String()].Version})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(capability.Method, capability.URL, nil)
		for name, value := range capability.Headers {
			request.Header.Set(name, value)
		}
		recorder := httptest.NewRecorder()
		h.fileBase.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || !bytes.Equal(recorder.Body.Bytes(), smartRecoveryBody(index)) {
			t.Fatalf("completed body %d: status=%d bytes=%v", index, recorder.Code, recorder.Body.Bytes())
		}
	}
}

func assertSmartRecoveryBulkRoutes(t *testing.T, requests []string) {
	t.Helper()
	for _, request := range requests {
		if request == "POST /api/v1/uploads" || strings.HasPrefix(request, "GET /api/v1/uploads/") || request == "GET /api/v1/files/stat" || request == "POST /api/v1/directories" || strings.HasPrefix(request, "POST /api/v1/uploads/") && !strings.HasPrefix(request, "POST /api/v1/uploads/batch/") && !strings.HasPrefix(request, "POST /api/v1/uploads/plan/") {
			t.Fatalf("scalar upload lifecycle request explosion: %s", request)
		}
	}
}

func assertSmartRecoveryProviderBudget(t *testing.T, stateEvents, fileEvents []providerbudget.Event, files, replayed int) {
	t.Helper()
	kinds := make(map[providerbudget.RequestKind]int)
	for _, event := range fileEvents {
		kinds[event.Kind]++
	}
	want := map[providerbudget.RequestKind]int{
		providerbudget.RequestUploadBegin:  files,
		providerbudget.RequestObjectVerify: files,
	}
	if replayed > 0 {
		want[providerbudget.RequestUploadProgress] = replayed
		want[providerbudget.RequestUploadResume] = replayed
	}
	if len(kinds) != len(want) {
		t.Fatalf("file-provider lifecycle request budget: observed=%v want=%v", kinds, want)
	}
	for kind, count := range want {
		if kinds[kind] != count {
			t.Fatalf("file-provider lifecycle request budget %s=%d want%d; all=%v", kind, kinds[kind], count, kinds)
		}
	}
	for _, event := range stateEvents {
		if event.Kind != providerbudget.RequestObjectGet && event.Kind != providerbudget.RequestObjectPut {
			t.Fatalf("unexpected state-provider lifecycle operation=%s", event.Kind)
		}
	}
}

type smartRecoveryProviderTotals struct {
	Requests, Failed            int
	RequestBytes, ResponseBytes int64
}

func smartRecoveryProviderVector(events []providerbudget.Event) map[string]smartRecoveryProviderTotals {
	result := make(map[string]smartRecoveryProviderTotals)
	for _, event := range events {
		// These labels are fixed provider instrumentation; never print targets,
		// owner IDs, page digests, request IDs, leases or capabilities.
		key := fmt.Sprintf("%s/%s/%s/%s/%s", event.Role, event.Operation, event.Subsystem, event.Kind, smartRecoveryTargetClass(event.Target))
		value := result[key]
		value.Requests++
		if event.Failed {
			value.Failed++
		}
		value.RequestBytes += event.RequestBytes
		value.ResponseBytes += event.ResponseBytes
		result[key] = value
	}
	return result
}

func smartRecoveryTargetClass(target string) string {
	kind := "other"
	for _, candidate := range []storageformat.ConsistencyDomainKind{storageformat.DomainNamespace, storageformat.DomainOwnerControl, storageformat.DomainAdmin, storageformat.DomainCapability, storageformat.DomainShare, storageformat.DomainIdentity, storageformat.DomainOwnerJobs} {
		if strings.Contains(target, "/domains/"+string(candidate)+"/") {
			kind = string(candidate)
			break
		}
	}
	shape := "other"
	switch {
	case strings.HasSuffix(target, "/head.json"):
		shape = "head"
	case strings.Contains(target, "/packs/"):
		shape = "pack"
	case strings.Contains(target, "/pages/"):
		shape = "page"
	}
	return kind + "-" + shape
}

type smartRecoveryPackTotals struct {
	LargeReadRequests, UniqueLargeReadTargets   int
	LargeWriteRequests, UniqueLargeWriteTargets int
	LargeReadBytes, LargeWriteBytes             int64
	LargestRead, LargestWrite                   int64
}

func smartRecoveryPackShape(events []providerbudget.Event) smartRecoveryPackTotals {
	var result smartRecoveryPackTotals
	reads, writes := make(map[string]struct{}), make(map[string]struct{})
	for _, event := range events {
		result.LargestRead = max(result.LargestRead, event.ResponseBytes)
		result.LargestWrite = max(result.LargestWrite, event.RequestBytes)
		if event.ResponseBytes > 64<<10 {
			result.LargeReadRequests++
			result.LargeReadBytes += event.ResponseBytes
			reads[event.Target] = struct{}{}
		}
		if event.RequestBytes > 64<<10 {
			result.LargeWriteRequests++
			result.LargeWriteBytes += event.RequestBytes
			writes[event.Target] = struct{}{}
		}
	}
	result.UniqueLargeReadTargets, result.UniqueLargeWriteTargets = len(reads), len(writes)
	return result
}

func smartRecoveryRouteHistogram(requests []string) map[string]int {
	result := make(map[string]int)
	for _, request := range requests {
		label := "other"
		switch request {
		case "POST /api/v1/uploads/plan/sizes", "POST /api/v1/uploads/plan/fingerprints", "POST /api/v1/uploads/batch/admit", "POST /api/v1/uploads/batch/actions", "POST /api/v1/uploads/batch/complete", "POST /api/v1/uploads/batch/status", "POST /api/v1/uploads/batch/resume", "POST /api/v1/directories/prepare", "GET /api/v1/files", "GET /", "GET /favicon.ico", "GET /api/v1/config", "GET /api/v1/me", "GET /api/v1/me/preferences/theme", "POST /api/v1/authentication/options", "POST /api/v1/authentication/verify", "POST /api/v1/logout":
			label = request
		default:
			if strings.HasPrefix(request, "PATCH /cap/upload/") {
				label = "PATCH upload-data"
			} else if strings.HasPrefix(request, "OPTIONS /cap/upload/") {
				label = "OPTIONS upload-data"
			} else if strings.HasPrefix(request, "GET /assets/") {
				label = "GET static-assets"
			}
		}
		result[label]++
	}
	return result
}

func smartRecoveryDiagnostic(client *testBrowser) string {
	ctx, cancel := context.WithTimeout(client.ctx, time.Second)
	defer cancel()
	var text string
	_ = chromedp.Run(ctx, chromedp.Evaluate(`JSON.stringify({counts:[...document.querySelectorAll('[data-transfer-count]')].map(x=>[x.dataset.transferCount,x.textContent]),preparing:document.querySelector('#transfer-check-label')?.textContent,list:document.querySelector('#transfer-list')?.textContent.slice(0,2000)})`, &text))
	return browserStatus(client.ctx) + " " + text
}

type smartRecoveryWrites struct {
	mu      sync.Mutex
	byIndex map[int]int
	invalid []string
}

func (writes *smartRecoveryWrites) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch && r.Method != http.MethodPut {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 5))
		r.Body = io.NopCloser(bytes.NewReader(body))
		recorder := httptest.NewRecorder()
		next.ServeHTTP(recorder, r)
		writes.mu.Lock()
		if err != nil || len(body) != 4 || recorder.Code != http.StatusNoContent {
			writes.invalid = append(writes.invalid, fmt.Sprintf("body=%d status=%d err=%v", len(body), recorder.Code, err))
		} else {
			writes.byIndex[int(binary.BigEndian.Uint32(body))]++
		}
		writes.mu.Unlock()
		copySmartRecoveryResponse(w, recorder)
	})
}

func (writes *smartRecoveryWrites) snapshot() map[int]int {
	writes.mu.Lock()
	defer writes.mu.Unlock()
	copy := make(map[int]int, len(writes.byIndex))
	for index, count := range writes.byIndex {
		copy[index] = count
	}
	return copy
}

func (writes *smartRecoveryWrites) assertExactly(t *testing.T, start, end int) {
	t.Helper()
	writes.mu.Lock()
	defer writes.mu.Unlock()
	if len(writes.invalid) != 0 || len(writes.byIndex) != end-start {
		t.Fatalf("actual transferred bodies=%d want%d invalid=%v", len(writes.byIndex), end-start, writes.invalid)
	}
	for index, count := range writes.byIndex {
		if index < start || index >= end || count != 1 {
			t.Fatalf("completed source retransmitted or remaining source repeated: synthetic-index=%d count=%d allowed=[%d,%d)", index, count, start, end)
		}
	}
}

func copySmartRecoveryResponse(w http.ResponseWriter, recorder *httptest.ResponseRecorder) {
	for name, values := range recorder.Header() {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
}
