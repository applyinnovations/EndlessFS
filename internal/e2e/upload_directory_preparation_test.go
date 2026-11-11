package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

func TestE2EUploadDirectoryPreparationPagesOverlapIndependentSiblings(t *testing.T) {
	parallelE2EFixture(t)
	requireSmartRecoveryE2E(t)
	const count = 450 // 901 selected directories: root, siblings and their children.
	var calls, active, peak atomic.Int64
	var mu sync.Mutex
	seen := make(map[string]bool)
	arrived, release := make(chan struct{}, 4), make(chan struct{})
	parentArrived, releaseParent := make(chan struct{}), make(chan struct{})
	prematureChild := make(chan struct{}, 1)
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var parentOnce sync.Once
	unblockParent := func() { parentOnce.Do(func() { close(releaseParent) }) }
	t.Cleanup(unblockParent)
	writes := &smartRecoveryWrites{byIndex: make(map[int]int)}
	h := newSmartRecoveryHarness(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/directories/prepare" {
				next.ServeHTTP(w, r)
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
			var page struct {
				Paths []string `json:"paths"`
			}
			if err != nil || len(body) > 1<<20 || json.Unmarshal(body, &page) != nil || len(page.Paths) < 1 || len(page.Paths) > 100 {
				t.Errorf("invalid bounded directory page: bytes=%d items=%d err=%v", len(body), len(page.Paths), err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			number := calls.Add(1)
			current := active.Add(1)
			defer active.Add(-1)
			for prior := peak.Load(); current > prior && !peak.CompareAndSwap(prior, current); prior = peak.Load() {
			}
			mu.Lock()
			for _, path := range page.Paths {
				if seen[path] {
					t.Errorf("directory selected twice: %s", path)
				}
				seen[path] = true
			}
			mu.Unlock()
			if number == 1 {
				close(parentArrived)
				select {
				case <-releaseParent:
				case <-r.Context().Done():
					return
				}
			} else {
				select {
				case <-releaseParent:
				default:
					select {
					case prematureChild <- struct{}{}:
					default:
					}
				}
			}
			// The root's page publishes first. Four subsequent pages have
			// independent sibling work and must all arrive before any is released.
			if number >= 2 && number <= 5 {
				arrived <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}, writes.wrap)
	client := newSmartRecoveryBrowser(t)
	bootstrapBrowser(t, client, h.harness)
	scope := smartRecoveryScope(t, h)
	root := filepath.Join(t.TempDir(), "paged-folder")
	for index := range count {
		path := filepath.Join(root, fmt.Sprintf("branch-%03d", index), "nested", "item.bin")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, smartRecoveryBody(index), 0600); err != nil {
			t.Fatal(err)
		}
	}
	queueDirectoryPreparationSources(t, client, root)
	select {
	case <-parentArrived:
	case <-time.After(conditionBound):
		t.Fatal("parent directory page never arrived")
	}
	// Hold the only root page: all other pages need this ancestor. This
	// negative observation window is a scheduling probe, not a latency budget.
	select {
	case <-prematureChild:
		unblockParent()
		unblock()
		t.Fatal("child page started before its held parent settled")
	case <-time.After(200 * time.Millisecond):
	}
	unblockParent()
	for range 4 {
		select {
		case <-arrived:
		case <-time.After(conditionBound):
			unblock()
			t.Fatalf("independent sibling pages serialized: requests=%d peak=%d", calls.Load(), peak.Load())
		}
	}
	unblock()
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(450)' && document.querySelector('[data-transfer-count="failed"]').textContent==='(0)'`, conditionBound); err != nil {
		t.Fatalf("paginated directory preparation did not complete actual uploads: %v %s", err, smartRecoveryDiagnostic(client))
	}
	mu.Lock()
	selected := len(seen)
	mu.Unlock()
	if calls.Load() != 10 || selected != 901 || peak.Load() != 4 {
		t.Fatalf("directory pagination: calls=%d selected=%d concurrency=%d; want10/901/4", calls.Load(), selected, peak.Load())
	}
	writes.assertExactly(t, 0, count)
	entry, err := h.engines[1].Files().Stat(context.Background(), scope, domain.MustParseUserPath("/paged-folder"))
	if err != nil || entry.FileCount != count || entry.Size != count*4 {
		t.Fatalf("independent replica aggregate: files=%d bytes=%d err=%v", entry.FileCount, entry.Size, err)
	}
	for index := range count {
		path := domain.MustParseUserPath(fmt.Sprintf("/paged-folder/branch-%03d/nested/item.bin", index))
		entry, err := h.engines[1].Files().Stat(context.Background(), scope, path)
		if err != nil || entry.Kind != domain.EntryFile || entry.Size != 4 {
			t.Fatalf("missing published synthetic file%d: err=%v", index, err)
		}
	}
	assertSmartRecoveryBulkRoutes(t, client.requestSnapshot())
	client.assertNoExternalRequests(t, h.harness)
}

func TestE2EUploadDirectoryPreparationReplaysOneBoundedPage(t *testing.T) {
	parallelE2EFixture(t)
	requireSmartRecoveryE2E(t)
	for _, mode := range []string{"rolling-route-gap", "lost-success", "exhausted-route-gap"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			var mu sync.Mutex
			var original []byte
			writes := &smartRecoveryWrites{byIndex: make(map[int]int)}
			h := newSmartRecoveryHarness(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/api/v1/directories/prepare" {
						next.ServeHTTP(w, r)
						return
					}
					body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
					if err != nil || len(body) > 1<<20 {
						t.Errorf("directory page read: bytes=%d err=%v", len(body), err)
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(body))
					attempt := calls.Add(1)
					mu.Lock()
					if attempt == 1 {
						original = append([]byte(nil), body...)
					} else if !bytes.Equal(original, body) {
						t.Error("directory retry changed its ordered page")
					}
					mu.Unlock()
					if attempt == 1 || mode == "exhausted-route-gap" {
						status := http.StatusMethodNotAllowed
						if mode == "lost-success" {
							recorder := httptest.NewRecorder()
							next.ServeHTTP(recorder, r)
							if recorder.Code != http.StatusOK {
								t.Errorf("initial directory publication status=%d", recorder.Code)
							}
							status = http.StatusServiceUnavailable
						}
						w.Header().Set("Retry-After", "1")
						w.Header().Set("Content-Type", "application/problem+json")
						w.WriteHeader(status)
						_, _ = w.Write([]byte(`{"code":"unavailable","title":"Synthetic directory interruption"}`))
						return
					}
					next.ServeHTTP(w, r)
				})
			}, writes.wrap)
			client := newSmartRecoveryBrowser(t)
			bootstrapBrowser(t, client, h.harness)
			if mode == "exhausted-route-gap" {
				if err := chromedp.Run(client.ctx, chromedp.Evaluate(`Math.random=()=>0`, nil)); err != nil {
					t.Fatal(err)
				}
			}
			scope := smartRecoveryScope(t, h)
			root := createSmartRecoverySources(t, "retry-folder", 6)
			if err := chromedp.Run(client.ctx, chromedp.SetUploadFiles("#folder-input", []string{root}, chromedp.ByQuery)); err != nil {
				t.Fatal(err)
			}
			if err := waitVisible(client.ctx, "#action-dialog", conditionBound); err != nil {
				t.Fatal(err)
			}
			if err := chromedp.Run(client.ctx, chromedp.SetValue("#upload-strategy", "smart-merge", chromedp.ByQuery), chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
				t.Fatal(err)
			}
			if mode == "exhausted-route-gap" {
				if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="failed"]').textContent==='(6)' && document.querySelector('[data-transfer-count="complete"]').textContent==='(0)'`, conditionBound); err != nil {
					t.Fatalf("directory retry did not stop at its bound: %v %s", err, smartRecoveryDiagnostic(client))
				}
				if calls.Load() != 8 || len(writes.snapshot()) != 0 || countExactRequest(client.requestSnapshot(), "POST /api/v1/uploads/batch/admit") != 0 {
					t.Fatalf("exhausted directory page: calls=%d data=%d", calls.Load(), len(writes.snapshot()))
				}
				assertSmartRecoveryBulkRoutes(t, client.requestSnapshot())
				client.assertNoExternalRequests(t, h.harness)
				return
			}
			if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(6)' && document.querySelector('[data-transfer-count="failed"]').textContent==='(0)'`, conditionBound); err != nil {
				t.Fatalf("directory page retry did not preserve upload success: %v %s", err, smartRecoveryDiagnostic(client))
			}
			if calls.Load() != 2 {
				t.Fatalf("directory pages=%d, want original plus one exact retry", calls.Load())
			}
			writes.assertExactly(t, 0, 6)
			verifySmartRecoveryFiles(t, h, scope, "retry-folder", 6)
			assertSmartRecoveryBulkRoutes(t, client.requestSnapshot())
			client.assertNoExternalRequests(t, h.harness)
		})
	}
}

func TestE2EUploadDirectoryPreparationCancellationReleasesNamespaceQueue(t *testing.T) {
	parallelE2EFixture(t)
	requireSmartRecoveryE2E(t)
	for _, mode := range []string{"backoff-logout", "inflight-logout", "backoff-cancel", "backoff-queued-empty-logout"} {
		t.Run(mode, func(t *testing.T) {
			var heldCalls atomic.Int64
			var queuedEmptyCalls atomic.Int64
			arrived := make(chan struct{}, 1)
			writes := &smartRecoveryWrites{byIndex: make(map[int]int)}
			h := newSmartRecoveryHarness(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/api/v1/directories/prepare" && r.URL.Path != "/api/v1/uploads/batch/admit" {
						next.ServeHTTP(w, r)
						return
					}
					body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
					if err != nil {
						t.Error(err)
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(body))
					if bytes.Contains(body, []byte("queued-empty")) {
						queuedEmptyCalls.Add(1)
						<-r.Context().Done()
						return
					}
					if !bytes.Contains(body, []byte("held-folder")) {
						next.ServeHTTP(w, r)
						return
					}
					if r.URL.Path == "/api/v1/uploads/batch/admit" {
						t.Error("cancelled directory preparation admitted its upload")
						next.ServeHTTP(w, r)
						return
					}
					if heldCalls.Add(1) == 1 {
						arrived <- struct{}{}
					}
					if mode == "inflight-logout" {
						<-r.Context().Done()
						return
					}
					w.Header().Set("Retry-After", "120")
					w.Header().Set("Content-Type", "application/problem+json")
					w.WriteHeader(http.StatusMethodNotAllowed)
					_, _ = w.Write([]byte(`{"code":"unavailable","title":"Synthetic rolling route gap"}`))
				})
			}, writes.wrap)
			client := newSmartRecoveryBrowser(t)
			bootstrapBrowser(t, client, h.harness)
			scope := smartRecoveryScope(t, h)
			queueDirectoryPreparationSources(t, client, createSmartRecoverySources(t, "held-folder", 1))
			select {
			case <-arrived:
			case <-time.After(conditionBound):
				t.Fatal("directory cancellation fixture was never reached")
			}
			if mode != "inflight-logout" {
				if err := waitFor(client.ctx, `performance.getEntriesByType('resource').some(item=>item.name.endsWith('/api/v1/directories/prepare'))`, conditionBound); err != nil {
					t.Fatalf("directory failure was not consumed before cancellation: %v", err)
				}
			}
			if mode == "backoff-queued-empty-logout" {
				if err := closeTransferSheet(client.ctx); err != nil {
					t.Fatal(err)
				}
				queueEmptyDirectoryPreparation(t, client)
			}
			if mode == "backoff-cancel" {
				if err := chromedp.Run(client.ctx, clickNode(`button[aria-label="Cancel folder upload held-folder"]`)); err != nil {
					t.Fatal(err)
				}
				if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="current"]').textContent==='(0)' && document.querySelector('[data-transfer-count="all"]').textContent==='(1)' && document.querySelector('[data-transfer-count="failed"]').textContent==='(0)'`, conditionBound); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := closeTransferSheet(client.ctx); err != nil {
					t.Fatal(err)
				}
				if err := chromedp.Run(client.ctx, clickNode("#logout-button")); err != nil {
					t.Fatal(err)
				}
				if err := waitVisible(client.ctx, "#auth-view", conditionBound); err != nil {
					t.Fatal(err)
				}
				if err := chromedp.Run(client.ctx, clickNode("#signin-button")); err != nil {
					t.Fatal(err)
				}
				if err := waitVisible(client.ctx, "#drive-view", conditionBound); err != nil {
					t.Fatal(err)
				}
			}
			queueDirectoryPreparationSources(t, client, createSmartRecoverySources(t, "next-folder", 1))
			if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(1)' && document.querySelector('[data-transfer-count="failed"]').textContent==='(0)'`, conditionBound); err != nil {
				t.Fatalf("cancelled preparation retained the namespace queue: %v %s", err, smartRecoveryDiagnostic(client))
			}
			if heldCalls.Load() != 1 {
				t.Fatalf("cancelled preparation replayed %d times", heldCalls.Load())
			}
			if queuedEmptyCalls.Load() != 0 {
				t.Fatalf("queued empty-folder preparation started after suspension: calls=%d", queuedEmptyCalls.Load())
			}
			writes.assertExactly(t, 0, 1)
			verifySmartRecoveryFiles(t, h, scope, "next-folder", 1)
			assertSmartRecoveryBulkRoutes(t, client.requestSnapshot())
			client.assertNoExternalRequests(t, h.harness)
		})
	}
}

func queueDirectoryPreparationSources(t *testing.T, client *testBrowser, root string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(client.ctx, conditionBound)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.SetUploadFiles("#folder-input", []string{root}, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(client.ctx, "#action-dialog", conditionBound); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(ctx, chromedp.SetValue("#upload-strategy", "smart-merge", chromedp.ByQuery), chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
}

func queueEmptyDirectoryPreparation(t *testing.T, client *testBrowser) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "queued-empty")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(client.ctx, conditionBound)
	defer cancel()
	// A held preparation's retry reopens the transfer sheet, which covers the
	// drop target; a drop there reaches no handler. Drop only once the target
	// is the element under the pointer, and again if the drop missed it.
	data := &input.DragData{Items: []*input.DragDataItem{}, Files: []string{root}, DragOperationsMask: 1}
	for {
		if err := closeTransferSheet(ctx); err != nil {
			t.Fatalf("close transfer sheet before folder drop: %v", err)
		}
		var point struct {
			X, Y  float64
			Ready bool
		}
		if err := chromedp.Run(ctx, chromedp.Evaluate(`(()=>{
			const target=document.querySelector('#drop-target');
			const box=target.getBoundingClientRect();
			const X=box.x+box.width/2, Y=box.y+Math.min(box.height/2,40);
			const hit=document.elementFromPoint(X,Y);
			window.__folderDropReached=false;
			target.addEventListener('drop',()=>{window.__folderDropReached=true;},{capture:true,once:true});
			return {X,Y,Ready:!document.querySelector('#action-dialog').open && Boolean(hit) && target.contains(hit)};
		})()`, &point)); err != nil {
			t.Fatalf("locate folder drop target: %v", err)
		}
		if point.Ready {
			if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
				for _, event := range []input.DispatchDragEventType{input.DragEnter, input.DragOver, input.Drop} {
					if err := input.DispatchDragEvent(event, point.X, point.Y, data).Do(ctx); err != nil {
						return err
					}
				}
				return nil
			})); err != nil {
				t.Fatalf("drop folder: %v", err)
			}
			var reached bool
			if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__folderDropReached`, &reached)); err != nil {
				t.Fatalf("observe folder drop: %v", err)
			}
			if reached {
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("folder drop target never became reachable: %v (%s)", ctx.Err(), browserStatus(client.ctx))
		case <-time.After(25 * time.Millisecond):
		}
	}
	if err := waitVisible(client.ctx, "#action-dialog", conditionBound); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(ctx, chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `Boolean(document.querySelector('button[aria-label="Cancel folder upload queued-empty"]'))`, conditionBound); err != nil {
		t.Fatalf("empty folder did not queue behind active preparation: %v %s", err, smartRecoveryDiagnostic(client))
	}
}
