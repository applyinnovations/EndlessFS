package e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

type previewResponseGate struct {
	armed            atomic.Bool
	started, release chan struct{}
	once             sync.Once
}

func newPreviewResponseGate() *previewResponseGate {
	return &previewResponseGate{started: make(chan struct{}), release: make(chan struct{})}
}
func (g *previewResponseGate) wait(request *http.Request) {
	if !g.armed.CompareAndSwap(true, false) {
		return
	}
	close(g.started)
	select {
	case <-g.release:
	case <-request.Context().Done():
	}
}
func (g *previewResponseGate) unblock() { g.once.Do(func() { close(g.release) }) }

func TestE2EThumbnailQueueProgressesAcrossRerenderWithoutRefresh(t *testing.T) {
	parallelE2EFixture(t)
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("set ENDLESSFS_RUN_E2E=1; the Nix test-e2e task does this")
	}
	acknowledged, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	var held atomic.Bool
	harness := newHarnessWithWrappers(t, true, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/previews/resolve" || !held.CompareAndSwap(false, true) {
				next.ServeHTTP(w, r)
				return
			}
			response := httptest.NewRecorder()
			next.ServeHTTP(response, r)
			close(acknowledged)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			for name, values := range response.Header() {
				for _, value := range values {
					w.Header().Add(name, value)
				}
			}
			w.WriteHeader(response.Code)
			_, _ = w.Write(response.Body.Bytes())
		})
	}, nil)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	client := newTestBrowser(t)
	bootstrapBrowser(t, client, harness)
	accounts, err := harness.repository.Accounts(t.Context())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("fixture owner: %d %v", len(accounts), err)
	}
	scope, _ := domain.NewScope(accounts[0].UserID, domain.AreaLive)
	root, err := harness.files.Stat(t.Context(), scope, domain.MustParseUserPath("/"))
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 96)
	for i := range paths {
		paths[i] = writeMediaFixture(t, fmt.Sprintf("queued-%03d.png", i), 32+i, 16)
	}
	if err := chromedp.Run(client.ctx, emulation.SetDeviceMetricsOverride(1280, 1600, 1.6, false), chromedp.SetUploadFiles("#upload-input", paths, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector("#file-rows").textContent.includes("queued-095.png")`, 45*time.Second); err != nil {
		t.Fatalf("uploads did not complete: %v (%s)", err, browserStatus(client.ctx))
	}
	if err := waitFor(client.ctx, fmt.Sprintf(`document.querySelector("#path-aggregate").textContent.includes("%d files")`, root.FileCount+96), 15*time.Second); err != nil {
		t.Fatalf("not every fixture image committed: %v (%s)", err, browserStatus(client.ctx))
	}
	var warmName string
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(`document.querySelector('#file-rows [aria-label^="Preview file queued-"]').getAttribute("aria-label").slice("Preview file ".length)`, &warmName)); err != nil {
		t.Fatal(err)
	}
	var warmIndex int
	if _, err := fmt.Sscanf(warmName, "queued-%03d.png", &warmIndex); err != nil {
		t.Fatal(err)
	}
	binding, claim, artifact := claimConcurrentBrowserPreview(t, harness, domain.MustParseUserPath("/"+warmName), paths[warmIndex], 256)
	if err := harness.previewStore.Commit(t.Context(), binding, claim, artifact); err != nil {
		t.Fatal(err)
	}
	harness.previewGate.armed.Store(true)
	t.Cleanup(harness.previewGate.unblock)
	if err := closeTransferSheet(client.ctx); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(`document.querySelector("#filter-media").value="image";document.querySelector("#filter-media").dispatchEvent(new Event("input"));document.querySelector("#file-view-grid").click()`, nil)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-acknowledged:
	case <-time.After(15 * time.Second):
		t.Fatal("visible grid did not request thumbnails")
	}
	// Replace DOM frames while the first 64-item batch is held and the
	// remaining visible tiles are queued behind it. Let observer delivery run
	// against those replacement frames before allowing the queue to drain.
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(`(async()=>{const nativeSetTimeout=window.setTimeout.bind(window);window.setTimeout=(callback,delay,...args)=>nativeSetTimeout(()=>{callback(...args);if(delay===1000&&document.querySelector(".media-frame[data-preview-state='queued']"))document.documentElement.dataset.thumbnailRetryObserved="true";},delay);window.dispatchEvent(new Event("resize"));for(let i=0;i<3;i++)await new Promise(requestAnimationFrame);})()`, nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-harness.previewGate.started:
	case <-time.After(15 * time.Second):
		t.Fatal("ready artifact download did not reach the response gate")
	}
	if err := waitFor(client.ctx, `document.documentElement.dataset.thumbnailRetryObserved==="true"`, 15*time.Second); err != nil {
		t.Fatalf("pending retries did not run while a ready download was held: %v", err)
	}
	harness.previewGate.unblock()
	if err := waitFor(client.ctx, `document.querySelectorAll(".media-frame[data-preview-loaded='true']").length===96`, 60*time.Second); err != nil {
		t.Fatalf("visible thumbnails did not finish without refresh: %v (%s)", err, browserStatus(client.ctx))
	}
	client.assertNoExternalRequests(t, harness)
}
