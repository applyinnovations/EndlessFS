package e2e

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

func TestE2ESmartMergePartitionsUTF8RequestsWithinControlBodyLimit(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	type measurement struct {
		route         string
		bytes, status int
	}
	var mu sync.Mutex
	var measured []measurement
	harness := newPortableHarnessWithControlPlaneWrapper(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			observe := r.Method == http.MethodPost && (r.URL.Path == "/api/v1/uploads/plan/sizes" || r.URL.Path == "/api/v1/uploads/plan/fingerprints" || r.URL.Path == "/api/v1/files/copy")
			if !observe {
				next.ServeHTTP(w, r)
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
			if err != nil {
				t.Error(err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			recorder := httptest.NewRecorder()
			next.ServeHTTP(recorder, r)
			mu.Lock()
			measured = append(measured, measurement{r.URL.Path, len(body), recorder.Code})
			mu.Unlock()
			for key, values := range recorder.Header() {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
		})
	})
	client := newTestBrowser(t)
	bootstrapBrowser(t, client, harness)
	file := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(file, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, chromedp.SetUploadFiles("#upload-input", []string{file}, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(client.ctx, "#action-dialog", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, chromedp.SetValue("#upload-strategy", "keep-both", chromedp.ByQuery), chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent === '(1)'`, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := closeTransferSheet(client.ctx); err != nil {
		t.Fatal(err)
	}
	// Valid paths below 4096 UTF-8 bytes and 255 bytes per component. Their
	// UTF-16 lengths and item count fit, but one encoded request exceeds 1 MiB.
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(`(() => {
		const transfer = new DataTransfer();
		const prefix = Array(6).fill('界"'.repeat(60)).join('/') + '/';
		for (let index = 0; index < 900; index++) {
			const file = new File(['abc'], 'item-' + index + '.txt', {type:'text/plain'});
			Object.defineProperty(file, 'webkitRelativePath', {value: prefix + file.name}); transfer.items.add(file);
		}
		const input = document.querySelector('#folder-input'); input.files = transfer.files;
		input.dispatchEvent(new Event('change', {bubbles:true}));
	})()`, nil)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(client.ctx, "#action-dialog", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent === '(901)' || document.querySelector('[data-transfer-count="failed"]').textContent !== '(0)'`, 45*time.Second); err != nil {
		var panel string
		_ = chromedp.Run(client.ctx, chromedp.Evaluate(`document.querySelector('#transfer-panel').textContent`, &panel))
		mu.Lock()
		t.Logf("planning requests: %+v", measured)
		mu.Unlock()
		t.Fatalf("Smart merge stalled: %v, panel=%s, requests=%v", err, panel, client.requestSnapshot())
	}
	mu.Lock()
	defer mu.Unlock()
	counts := map[string]int{}
	for _, request := range measured {
		counts[request.route]++
		wantStatus := http.StatusOK
		if request.route == "/api/v1/files/copy" {
			wantStatus = http.StatusAccepted
		}
		if request.bytes > 1<<20 || request.status != wantStatus {
			t.Errorf("%s: %d UTF-8 bytes, status %d", request.route, request.bytes, request.status)
		}
	}
	for _, route := range []string{"/api/v1/uploads/plan/sizes", "/api/v1/uploads/plan/fingerprints", "/api/v1/files/copy"} {
		if counts[route] < 2 {
			t.Errorf("%s used %d requests; this workload requires partitioning", route, counts[route])
		}
	}
	var completed string
	if err := chromedp.Run(client.ctx, chromedp.Text(`[data-transfer-count="complete"]`, &completed, chromedp.ByQuery)); err != nil || completed != "(901)" {
		t.Errorf("Smart merge completed %s, want (901): %v", completed, err)
	}
	accounts, err := harness.repository.Accounts(context.Background())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts: %v", err)
	}
	scope, err := domain.NewScope(accounts[0].UserID, domain.AreaLive)
	if err != nil {
		t.Fatal(err)
	}
	source, err := harness.files.Stat(context.Background(), scope, domain.MustParseUserPath("/source.txt"))
	if err != nil {
		t.Fatal(err)
	}
	prefix := "/" + strings.TrimSuffix(strings.Repeat(strings.Repeat("界\"", 60)+"/", 6), "/")
	for _, name := range []string{"item-0.txt", "item-899.txt"} {
		entry, err := harness.files.Stat(context.Background(), scope, domain.MustParseUserPath(prefix+"/"+name))
		if err != nil || entry.Size != 3 || entry.ContentID != source.ContentID {
			t.Errorf("reuse authority %s: size=%d, shared=%v, %v", name, entry.Size, entry.ContentID == source.ContentID, err)
		}
	}
}
