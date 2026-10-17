package e2e

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

func TestE2EUploadAdmissionWaitsForNamespaceTurnWithoutBlockingData(t *testing.T) {
	requireSmartRecoveryE2E(t)
	planStarted, planRelease := make(chan struct{}), make(chan struct{})
	actionStarted, actionRelease := make(chan struct{}), make(chan struct{})
	dataStarted, dataRelease, dataFinished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var planOnce, actionOnce, dataOnce sync.Once
	releasePlan := func() { planOnce.Do(func() { close(planRelease) }) }
	releaseAction := func() { actionOnce.Do(func() { close(actionRelease) }) }
	releaseData := func() { dataOnce.Do(func() { close(dataRelease) }) }
	t.Cleanup(func() { releasePlan(); releaseAction(); releaseData() })
	var planning, completing atomic.Bool
	var sizes, actions, admissions, overlaps atomic.Int64
	writes := &smartRecoveryWrites{byIndex: make(map[int]int)}
	h := newSmartRecoveryHarness(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/uploads/plan/sizes":
				if sizes.Add(1) == 1 {
					planning.Store(true)
					defer planning.Store(false)
					close(planStarted)
					select {
					case <-planRelease:
					case <-r.Context().Done():
						return
					}
				}
			case "/api/v1/uploads/batch/actions":
				if actions.Add(1) == 1 {
					completing.Store(true)
					defer completing.Store(false)
					close(actionStarted)
					select {
					case <-actionRelease:
					case <-r.Context().Done():
						return
					}
				}
			case "/api/v1/uploads/batch/admit":
				admissions.Add(1)
				if planning.Load() || completing.Load() {
					overlaps.Add(1)
				}
			}
			next.ServeHTTP(w, r)
		})
	}, func(next http.Handler) http.Handler {
		observed := writes.wrap(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPatch || r.Method == http.MethodPut {
				body, err := io.ReadAll(io.LimitReader(r.Body, 5))
				r.Body = io.NopCloser(bytes.NewReader(body))
				if err == nil && len(body) == 4 && binary.BigEndian.Uint32(body) == 1 {
					close(dataStarted)
					select {
					case <-dataRelease:
					case <-r.Context().Done():
						return
					}
					observed.ServeHTTP(w, r)
					close(dataFinished)
					return
				}
			}
			observed.ServeHTTP(w, r)
		})
	})
	client := newSmartRecoveryBrowser(t)
	bootstrapSmartRecovery(t, client, h.harness)
	scope := smartRecoveryScope(t, h)
	paths := make([]string, 3)
	for index, name := range []string{"planned.bin", "held-data.bin", "after-actions.bin"} {
		paths[index] = filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(paths[index], smartRecoveryBody(index), 0600); err != nil {
			t.Fatal(err)
		}
	}
	queueNamespaceTurnSource(t, client, paths[0], "smart-merge")
	select {
	case <-planStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("real smart size request never reached its gate")
	}
	queueNamespaceTurnSource(t, client, paths[1], "keep-both")
	waitNamespaceTurnAdmissionIntent(t, client, "held-data.bin", false)
	if got := admissions.Load(); got != 0 || overlaps.Load() != 0 {
		t.Fatalf("fresh admission overlapped held planning: admissions=%d overlaps=%d", got, overlaps.Load())
	}
	releasePlan()
	select {
	case <-dataStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("queued admission did not release fairly after planning")
	}
	// The first verified file must become durable even while another file's
	// bytes are held. Keep the product's real 15-second completion age intact.
	select {
	case <-actionStarted:
	case <-time.After(20 * time.Second):
		t.Fatalf("held data indefinitely blocked an independently verified completion: %s", smartRecoveryDiagnostic(client))
	}
	queueNamespaceTurnSource(t, client, paths[2], "keep-both")
	waitNamespaceTurnAdmissionIntent(t, client, "after-actions.bin", false)
	if got := admissions.Load(); got != 2 || overlaps.Load() != 0 {
		t.Fatalf("fresh admission overlapped held actions: admissions=%d overlaps=%d", got, overlaps.Load())
	}
	releaseData()
	select {
	case <-dataFinished:
	case <-time.After(5 * time.Second):
		t.Fatal("namespace turn blocked an already admitted data transfer")
	}
	if !completing.Load() || writes.snapshot()[1] != 1 {
		t.Fatal("data independence was not proved while the actions request remained held")
	}
	releaseAction()
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(3)' && document.querySelector('[data-transfer-count="failed"]').textContent==='(0)'`, 15*time.Second); err != nil {
		t.Fatalf("queued admission/completion did not settle: %v %s", err, smartRecoveryDiagnostic(client))
	}
	if admissions.Load() != 3 || overlaps.Load() != 0 {
		t.Fatalf("admission duplicated or bypassed namespace ordering: admissions=%d overlaps=%d", admissions.Load(), overlaps.Load())
	}
	writes.assertExactly(t, 0, 3)
	for _, path := range paths {
		entry, err := h.engines[1].Files().Stat(context.Background(), scope, domain.MustParseUserPath("/"+filepath.Base(path)))
		if err != nil || entry.Kind != domain.EntryFile || entry.Size != 4 {
			t.Fatalf("independent replica did not see the completed synthetic file: %v", err)
		}
	}
	assertSmartRecoveryBulkRoutes(t, client.requestSnapshot())
	client.assertNoExternalRequests(t, h.harness)
}

func TestE2EQueuedUploadAdmissionPreservesCancellationAndReleasesTurn(t *testing.T) {
	requireSmartRecoveryE2E(t)
	for _, mode := range []string{"cancel-all", "cancel-one", "logout"} {
		t.Run(mode, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			var sizes atomic.Int64
			var mu sync.Mutex
			var admittedNames []string
			writes := &smartRecoveryWrites{byIndex: make(map[int]int)}
			h := newSmartRecoveryHarness(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/uploads/plan/sizes" && sizes.Add(1) == 1 {
						close(started)
						select {
						case <-release:
						case <-r.Context().Done():
							return
						}
					}
					if r.URL.Path == "/api/v1/uploads/batch/admit" {
						body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
						if err != nil {
							t.Error(err)
							return
						}
						r.Body = io.NopCloser(bytes.NewReader(body))
						var page struct {
							Uploads []struct{ Name string } `json:"uploads"`
						}
						if err := json.Unmarshal(body, &page); err != nil {
							t.Error(err)
							return
						}
						mu.Lock()
						for _, item := range page.Uploads {
							admittedNames = append(admittedNames, item.Name)
						}
						mu.Unlock()
					}
					next.ServeHTTP(w, r)
				})
			}, writes.wrap)
			client := newSmartRecoveryBrowser(t)
			bootstrapSmartRecovery(t, client, h.harness)
			scope := smartRecoveryScope(t, h)
			paths := make([]string, 3)
			for index, name := range []string{"planned.bin", "cancelled.bin", "survivor.bin"} {
				paths[index] = filepath.Join(t.TempDir(), name)
				if err := os.WriteFile(paths[index], smartRecoveryBody(index), 0600); err != nil {
					t.Fatal(err)
				}
			}
			queueNamespaceTurnSource(t, client, paths[0], "smart-merge")
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("smart planning never held the namespace turn")
			}
			pending := paths[1:2]
			if mode == "cancel-one" {
				pending = paths[1:]
			}
			queueNamespaceTurnSources(t, client, pending, "keep-both")
			waitNamespaceTurnAdmissionIntent(t, client, "cancelled.bin", mode == "cancel-one")
			if mode == "cancel-one" {
				waitNamespaceTurnAdmissionIntent(t, client, "survivor.bin", true)
				if err := runStage(client.ctx, 5*time.Second, chromedp.Click(`button[aria-label="Expand upload group 2 files"]`, chromedp.ByQuery)); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "logout" {
				if err := closeTransferSheet(client.ctx); err != nil {
					t.Fatal(err)
				}
				if err := runStage(client.ctx, 5*time.Second, chromedp.Click("#logout-button", chromedp.ByQuery)); err != nil {
					t.Fatal(err)
				}
				if err := waitVisible(client.ctx, "#auth-view", 5*time.Second); err != nil {
					t.Fatal(err)
				}
				unblock()
				if err := runStage(client.ctx, 5*time.Second, chromedp.Click("#signin-button", chromedp.ByQuery)); err != nil {
					t.Fatal(err)
				}
				if err := waitFor(client.ctx, `document.querySelector('#app').dataset.state==='authenticated' && document.querySelector('#auth-view').hidden && Boolean(document.querySelector('button[aria-label="Reconnect upload source for cancelled.bin"]'))`, 10*time.Second); err != nil {
					t.Fatalf("login did not restore suspended source controls: %v %s", err, smartRecoveryDiagnostic(client))
				}
				queueNamespaceTurnSource(t, client, paths[2], "keep-both")
			} else {
				if err := runStage(client.ctx, 5*time.Second, chromedp.Click(`button[aria-label="Cancel upload cancelled.bin"]`, chromedp.ByQuery)); err != nil {
					t.Fatal(err)
				}
				unblock()
			}
			wantComplete := 1
			if mode == "cancel-one" {
				wantComplete = 2
			}
			if err := waitFor(client.ctx, fmt.Sprintf(`document.querySelector('[data-transfer-count="complete"]').textContent==='(%d)' && document.querySelector('[data-transfer-count="failed"]').textContent==='(0)'`, wantComplete), 15*time.Second); err != nil {
				t.Fatalf("cancelled admission retained the turn or failed its sibling: %v %s", err, smartRecoveryDiagnostic(client))
			}
			mu.Lock()
			names := append([]string(nil), admittedNames...)
			mu.Unlock()
			if len(names) != wantComplete {
				t.Fatalf("queued cancellation allocated an upload or lost a live consumer: names=%v", names)
			}
			for _, name := range names {
				if name == "cancelled.bin" || mode == "logout" && name != "survivor.bin" {
					t.Fatalf("suspended/cancelled intent reached admission: names=%v", names)
				}
				entry, err := h.engines[1].Files().Stat(context.Background(), scope, domain.MustParseUserPath("/"+name))
				if err != nil || entry.Size != 4 {
					t.Fatalf("live consumer did not publish on replica2: name=%s err=%v", name, err)
				}
			}
			if _, err := h.engines[1].Files().Stat(context.Background(), scope, domain.MustParseUserPath("/cancelled.bin")); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("cancelled queued upload became visible: %v", err)
			}
			actual := writes.snapshot()
			if len(actual) != wantComplete || actual[1] != 0 {
				t.Fatalf("queued cancellation transferred bytes or lost a survivor: writes=%v", actual)
			}
			for _, count := range actual {
				if count != 1 {
					t.Fatalf("surviving upload retransferred: writes=%v", actual)
				}
			}
			assertSmartRecoveryBulkRoutes(t, client.requestSnapshot())
			client.assertNoExternalRequests(t, h.harness)
		})
	}
}

func queueNamespaceTurnSource(t *testing.T, client *testBrowser, path, strategy string) {
	t.Helper()
	queueNamespaceTurnSources(t, client, []string{path}, strategy)
}

func queueNamespaceTurnSources(t *testing.T, client *testBrowser, paths []string, strategy string) {
	t.Helper()
	if err := closeTransferSheet(client.ctx); err != nil {
		t.Fatal(err)
	}
	if err := runStage(client.ctx, 5*time.Second, chromedp.SetUploadFiles("#upload-input", paths, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(client.ctx, "#action-dialog", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := runStage(client.ctx, 5*time.Second, chromedp.SetValue("#upload-strategy", strategy, chromedp.ByQuery), chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
}

func waitNamespaceTurnAdmissionIntent(t *testing.T, client *testBrowser, name string, grouped bool) {
	t.Helper()
	// Single root-file selections have no group or directory preparation turn.
	// The persisted per-item intent proves admission reached its HTTP boundary.
	script := fmt.Sprintf(`window.__namespaceAdmissionReady=false;(async()=>{for(let i=0;i<200;i++){
		const db=await new Promise((resolve,reject)=>{const r=indexedDB.open('endlessfs-transfer-ledger-v1');r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});
		const records=await new Promise((resolve,reject)=>{const r=db.transaction('items','readonly').objectStore('items').getAll();r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});db.close();
		if(records.some(item=>item.name===%q&&(!item.groupID||%t)&&!item.relativeDirectory&&item.admissionProtocol==='independent'&&item.admissionIntent?.path==='/'&&item.admissionIntent.idempotencyKey===item.id)){window.__namespaceAdmissionReady=true;return;}
		await new Promise(resolve=>setTimeout(resolve,20));}})().catch(error=>{window.__namespaceAdmissionError=String(error)});`, name, grouped)
	if err := runStage(client.ctx, 5*time.Second, chromedp.Evaluate(script, nil)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `window.__namespaceAdmissionReady===true`, 5*time.Second); err != nil {
		t.Fatalf("root-file admission never durably reached its request boundary: %v %s", err, smartRecoveryDiagnostic(client))
	}
	if err := runStage(client.ctx, 5*time.Second, chromedp.Evaluate(`new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(()=>resolve(true))))`, nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
		t.Fatal(err)
	}
}
