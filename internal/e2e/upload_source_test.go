package e2e

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func TestE2EUploadSourceBufferBudgetAndCancellation(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	for _, chunks := range []int{1, 2, 4} {
		t.Run(fmt.Sprint(chunks), func(t *testing.T) {
			gate := newUploadConcurrencyGate()
			h := newHarnessWithStorage(t, false, true, true, nil, gate.wrap)
			t.Cleanup(gate.releaseAll)
			client := newTestBrowser(t)
			bootstrapBrowser(t, client, h)
			if err := waitFor(client.ctx, `typeof window.__endlessfsUploadSourceTest === 'object'`, 5*time.Second); err != nil {
				t.Fatalf("source pipeline fixture missing: %v", err)
			}
			if err := chromedp.Run(client.ctx, chromedp.Evaluate(fmt.Sprintf(`window.__endlessfsUploadSourceTest.setBufferChunks(%d)`, chunks), nil)); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			paths := make([]string, 8)
			for index := range paths {
				paths[index] = filepath.Join(root, fmt.Sprintf("source-%d.bin", index))
				if err := os.WriteFile(paths[index], bytes.Repeat([]byte{byte(index + 1)}, 8<<20), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := chromedp.Run(client.ctx, chromedp.SetUploadFiles("#upload-input", paths, chromedp.ByQuery)); err != nil {
				t.Fatal(err)
			}
			condition := fmt.Sprintf(`(() => { const s = window.__endlessfsUploadSourceTest.snapshot(); return s.usedBytes === %d && s.waiting > 0 && window.__endlessfsUploadWorkerPoolTest.active() === 8; })()`, chunks*(8<<20))
			if err := waitFor(client.ctx, condition, 15*time.Second); err != nil {
				t.Fatalf("source budget did not bound large bodies: %v (%s)", err, browserStatus(client.ctx))
			}
			var bounded bool
			if err := chromedp.Run(client.ctx, chromedp.Evaluate(fmt.Sprintf(`(() => { const s = window.__endlessfsUploadSourceTest.snapshot(); return s.peakBytes <= %d && s.peakReaders === 1; })()`, chunks*(8<<20)), &bounded)); err != nil || !bounded {
				t.Fatalf("source memory/read bounds: %v, bounded=%v", err, bounded)
			}
			if err := chromedp.Run(client.ctx, chromedp.Click(`[aria-label="Cancel folder upload 8 files"]`, chromedp.ByQuery)); err != nil {
				t.Fatal(err)
			}
			if err := waitFor(client.ctx, `(() => { const s = window.__endlessfsUploadSourceTest.snapshot(); return s.usedBytes === 0 && s.waiting === 0 && s.readers === 0 && window.__endlessfsUploadWorkerPoolTest.active() === 0; })()`, 15*time.Second); err != nil {
				t.Fatalf("cancelled source work retained memory or reader slots: %v", err)
			}
			gate.releaseAll()
		})
	}
}

func TestE2EUploadStalledSourceDoesNotBlockVerifiedFiles(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	blocked := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(blocked) }) }
	t.Cleanup(release)
	h := newHarnessWithStorage(t, false, true, true, nil, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if (r.Method == http.MethodPatch || r.Method == http.MethodPut) && r.ContentLength == 8<<20 {
				select {
				case <-blocked:
				case <-r.Context().Done():
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	client := newTestBrowser(t)
	bootstrapBrowser(t, client, h)
	root := t.TempDir()
	large, small := filepath.Join(root, "held-source.bin"), filepath.Join(root, "verified-small.txt")
	if err := os.WriteFile(large, bytes.Repeat([]byte{0x32}, (8<<20)+1), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(small, []byte("verified small file"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, chromedp.SetUploadFiles("#upload-input", []string{large, small}, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `window.__endlessfsUploadSourceTest.snapshot().finalizing === 1 && window.__endlessfsUploadWorkerPoolTest.active() === 1`, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := countExactRequest(client.requestSnapshot(), "POST /api/v1/uploads/batch/complete"); got != 0 {
		t.Fatalf("healthy burst prematurely split publication: %d", got)
	}
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(`window.__endlessfsUploadSourceTest.advanceMeasurement(16000)`, nil)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `window.__endlessfsUploadWorkerPoolTest.snapshot().groups.some(group => group.files === 2 && group.complete === 1) && document.querySelector('#file-rows').textContent.includes('verified-small.txt')`, 5*time.Second); err != nil {
		t.Fatalf("held source prevented verified-file publication: %v (%s)", err, browserStatus(client.ctx))
	}
	if err := waitFor(client.ctx, `document.querySelector('#header-transfer-speed').textContent === '—/s'`, 5*time.Second); err != nil {
		t.Fatalf("unacknowledged work retained a fictitious speed: %v", err)
	}
	release()
	if err := waitFor(client.ctx, `window.__endlessfsUploadWorkerPoolTest.snapshot().groups.some(group => group.files === 2 && group.complete === 2)`, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := countExactRequest(client.requestSnapshot(), "POST /api/v1/uploads/batch/complete"); got != 2 {
		t.Fatalf("stalled recovery publication batches = %d, want partial plus final", got)
	}
}

func TestE2EUploadSourceReadFailureReleasesBudgetAndRequestsReconnection(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	h := newHarnessWithStorage(t, false, true, true, nil, nil)
	client := newTestBrowser(t)
	bootstrapBrowser(t, client, h)
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(`(() => {
		const NativeReader = window.FileReader;
		let first = true;
		window.FileReader = class extends NativeReader {
			readAsArrayBuffer(blob) {
				if (!first) return super.readAsArrayBuffer(blob);
				first = false;
				queueMicrotask(() => this.onerror());
			}
		};
	})()`, nil)); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	paths := []string{filepath.Join(root, "unavailable-source.txt"), filepath.Join(root, "healthy-source.txt")}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("source read qualification"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := chromedp.Run(client.ctx, chromedp.SetUploadFiles("#upload-input", paths, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `(() => { const s = window.__endlessfsUploadSourceTest.snapshot(); const p = window.__endlessfsUploadWorkerPoolTest.snapshot(); return s.usedBytes === 0 && s.waiting === 0 && s.readers === 0 && p.states['needs-source'] === 1 && p.groups.some(group => group.files === 2 && group.complete === 1); })()`, 15*time.Second); err != nil {
		t.Fatalf("source failure leaked resources or prevented independent completion: %v (%s)", err, browserStatus(client.ctx))
	}
}

func TestE2EUploadSourceBackpressureRemainsResponsiveAndCompletes(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	for _, chunks := range []int{1, 2, 4} {
		t.Run(fmt.Sprint(chunks), func(t *testing.T) {
			h := newHarnessWithStorage(t, false, true, true, nil, nil)
			client := newTestBrowser(t)
			bootstrapBrowser(t, client, h)
			if err := chromedp.Run(client.ctx, chromedp.Evaluate(fmt.Sprintf(`window.__endlessfsUploadSourceTest.setBufferChunks(%d); window.__endlessfsUploadSourceTest.pauseReads(true)`, chunks), nil)); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			paths := make([]string, 8)
			for index := range paths {
				paths[index] = filepath.Join(root, fmt.Sprintf("slow-%d.bin", index))
				if err := os.WriteFile(paths[index], bytes.Repeat([]byte{byte(index + 1)}, (8<<20)+1), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := chromedp.Run(client.ctx, chromedp.SetUploadFiles("#upload-input", paths, chromedp.ByQuery)); err != nil {
				t.Fatal(err)
			}
			if err := waitFor(client.ctx, `window.__endlessfsUploadSourceTest.snapshot().waiting === 8`, 15*time.Second); err != nil {
				t.Fatal(err)
			}
			var responsive struct {
				Bytes  int    `json:"bytes"`
				Failed string `json:"failed"`
				Frame  bool   `json:"frame"`
			}
			if err := chromedp.Run(client.ctx, chromedp.Evaluate(`new Promise(resolve => requestAnimationFrame(() => resolve({bytes: window.__endlessfsUploadSourceTest.snapshot().usedBytes, failed: document.querySelector('[data-transfer-count="failed"]').textContent, frame: true})))`, &responsive, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil || responsive.Bytes != 0 || !responsive.Frame {
				t.Fatalf("slow source blocked UI: %v, %+v", err, responsive)
			}
			if err := chromedp.Run(client.ctx, chromedp.Evaluate(`window.__endlessfsUploadSourceTest.pauseReads(false)`, nil)); err != nil {
				t.Fatal(err)
			}
			if err := waitFor(client.ctx, `window.__endlessfsUploadWorkerPoolTest.snapshot().groups.some(group => group.files === 8 && group.complete === 8) && window.__endlessfsUploadSourceTest.snapshot().usedBytes === 0`, 30*time.Second); err != nil {
				t.Fatalf("slow source did not complete when available: %v (%s)", err, browserStatus(client.ctx))
			}
			requests := client.requestSnapshot()
			if got := countExactRequest(requests, "POST /api/v1/uploads/batch"); got != 1 {
				t.Fatalf("admission requests = %d, want one", got)
			}
			if got := countExactRequest(requests, "POST /api/v1/uploads/batch/complete"); got != 1 {
				t.Fatalf("completion requests = %d, want one", got)
			}
			client.assertNoExternalRequests(t, h)
		})
	}
}

func TestE2EUploadVerifiedFilesPublishWhileOtherAcknowledgementsContinue(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	chunks := make(chan struct{}, 1)
	var once sync.Once
	release := func() { once.Do(func() { close(chunks) }) }
	t.Cleanup(release)
	h := newHarnessWithStorage(t, false, true, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/uploads/batch/complete" {
				next.ServeHTTP(w, r)
				return
			}
			recorder := httptest.NewRecorder()
			next.ServeHTTP(recorder, r)
			if recorder.Code >= 400 {
				t.Logf("completion denial %d: %s", recorder.Code, recorder.Body.String())
			}
			for key, values := range recorder.Header() {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
		})
	}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if (r.Method == http.MethodPatch || r.Method == http.MethodPut) && r.ContentLength == 8<<20 {
				select {
				case <-chunks:
				case <-r.Context().Done():
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	c := newTestBrowser(t)
	bootstrapBrowser(t, c, h)
	root := t.TempDir()
	large := filepath.Join(root, "continuing-stream.bin")
	small := filepath.Join(root, "already-verified.txt")
	// Two full source chunks stay within the memory backend's 16 MiB integrity
	// materialization limit. Hold the second after acknowledging the first.
	if err := os.WriteFile(large, bytes.Repeat([]byte{0x43}, 16<<20), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(small, []byte("verified file"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(c.ctx, chromedp.SetUploadFiles("#upload-input", []string{large, small}, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(c.ctx, `window.__endlessfsUploadSourceTest.snapshot().finalizing===1&&window.__endlessfsUploadWorkerPoolTest.active()===1`, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(c.ctx, chromedp.Click(`[aria-label="Expand upload group 2 files"]`, chromedp.ByQuery), chromedp.Evaluate(`window.__endlessfsUploadSourceTest.advanceMeasurement(8000)`, nil)); err != nil {
		t.Fatal(err)
	}
	chunks <- struct{}{}
	if err := waitFor(c.ctx, `Array.from(document.querySelectorAll('.transfer-row')).some(row=>row.textContent.includes('continuing-stream.bin')&&row.querySelector('progress').value>=8*2**20)`, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(c.ctx, chromedp.Evaluate(`window.__endlessfsUploadSourceTest.advanceMeasurement(8000)`, nil)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(c.ctx, `window.__endlessfsUploadWorkerPoolTest.snapshot().groups.some(group=>group.files===2&&group.complete===1)&&document.querySelector('#file-rows').textContent.includes('already-verified.txt')`, 5*time.Second); err != nil {
		t.Fatalf("fresh acknowledgements held verified publication: %v (%s)", err, browserStatus(c.ctx))
	}
	release()
	if err := waitFor(c.ctx, `window.__endlessfsUploadWorkerPoolTest.snapshot().groups.some(group=>group.files===2&&group.complete===2)`, 15*time.Second); err != nil {
		var detail string
		_ = chromedp.Run(c.ctx, chromedp.Evaluate(`JSON.stringify({summary:window.__endlessfsUploadWorkerPoolTest.snapshot(),panel:document.querySelector('#transfer-panel').textContent})`, &detail))
		t.Fatalf("remaining stream did not complete: %v; %s", err, detail)
	}
	if calls := countExactRequest(c.requestSnapshot(), "POST /api/v1/uploads/batch/complete"); calls != 2 {
		t.Fatalf("publication requests=%d, want partial plus final", calls)
	}
}
