package e2e

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

func TestE2ELostUploadResponseFinalizesEmptyAndChunkedObjectsOnce(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	var dataCalls atomic.Int64
	harness := newHarnessWithStorage(t, false, false, true, nil, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPatch && r.Method != http.MethodPut {
				next.ServeHTTP(w, r)
				return
			}
			dataCalls.Add(1)
			recorded := httptest.NewRecorder()
			next.ServeHTTP(recorded, r)
			// The provider accepts the bytes, but Chrome cannot read the response.
			// Status reconciliation must not resend data or allocate another session.
			recorded.Header().Del("Access-Control-Allow-Origin")
			for name, values := range recorded.Header() {
				w.Header()[name] = values
			}
			w.WriteHeader(recorded.Code)
			_, _ = w.Write(recorded.Body.Bytes())
		})
	})
	client := newTestBrowser(t)
	bootstrapBrowser(t, client, harness)
	root := t.TempDir()
	paths := []string{filepath.Join(root, "empty.txt"), filepath.Join(root, "small.txt"), filepath.Join(root, "chunked.bin")}
	for index, body := range [][]byte{nil, []byte("lost response"), bytes.Repeat([]byte{0x51}, (8<<20)+1)} {
		if err := os.WriteFile(paths[index], body, 0600); err != nil {
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
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent === '(3)'`, 15*time.Second); err != nil {
		t.Fatalf("lost-response uploads did not complete: %v (%s), requests=%v", err, browserStatus(client.ctx), client.requestSnapshot())
	}
	if got := dataCalls.Load(); got != 4 {
		t.Fatalf("data requests = %d, want one empty, one small, and two chunks", got)
	}
	if got := countExactRequest(client.requestSnapshot(), "POST /api/v1/uploads"); got != 0 {
		t.Fatalf("recovery allocated %d replacement sessions", got)
	}
}

func TestE2EAutomaticRetryResumesOriginalBatchSession(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	var calls atomic.Int64
	var chunkTarget atomic.Value
	chunkTarget.Store("")
	harness := newHarnessWithStorage(t, false, false, true, nil, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPatch || r.Method == http.MethodPut {
				if r.ContentLength == 8<<20 {
					chunkTarget.Store(r.URL.Path)
				}
				if r.URL.Path != chunkTarget.Load().(string) {
					next.ServeHTTP(w, r)
					return
				}
				call := calls.Add(1)
				if call >= 2 && call <= 5 {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if call == 6 && r.Header.Get("Upload-Offset") != "8388608" {
					t.Errorf("resumed offset = %q", r.Header.Get("Upload-Offset"))
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	client := newTestBrowser(t)
	bootstrapBrowser(t, client, harness)
	path := filepath.Join(t.TempDir(), "retry-chunk.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0x53}, (8<<20)+1), 0600); err != nil {
		t.Fatal(err)
	}
	small := filepath.Join(t.TempDir(), "other.txt")
	if err := os.WriteFile(small, []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, chromedp.SetUploadFiles("#upload-input", []string{path, small}, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(client.ctx, "#action-dialog", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, chromedp.SetValue("#upload-strategy", "keep-both", chromedp.ByQuery), chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent === '(2)'`, 15*time.Second); err != nil {
		t.Fatalf("resume failed: %v (%s)", err, browserStatus(client.ctx))
	}
	if calls.Load() != 6 {
		t.Fatalf("data attempts = %d", calls.Load())
	}
	requests := client.requestSnapshot()
	resumes := 0
	for _, request := range requests {
		if strings.HasPrefix(request, "POST /api/v1/uploads/") && strings.HasSuffix(request, "/resume") {
			resumes++
		}
	}
	if resumes != 1 || countExactRequest(requests, "POST /api/v1/uploads") != 0 || countExactRequest(requests, "POST /api/v1/uploads/batch") != 1 {
		t.Fatalf("session was reallocated: resumes=%d, requests=%v", resumes, requests)
	}
}
