package e2e

import (
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// Opt-in hardware evidence, separate from the deterministic release gate.
// The local sink streams and attests every byte without retaining photo bodies.
// It qualifies source orchestration, not live-provider publication or throughput.
func TestE2EUploadSourceHardwareQualification(t *testing.T) {
	directory := os.Getenv("ENDLESSFS_SOURCE_QUALIFICATION_DIRECTORY")
	if directory == "" {
		t.Skip("explicit hardware source qualification was not requested")
	}
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Fatal("hardware qualification requires the Nix Chromium task")
	}
	paths, err := filepath.Glob(filepath.Join(directory, "*.ARW"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	if len(paths) < 100 {
		t.Fatal("hardware qualification requires at least 100 photos")
	}
	paths = paths[:100]
	want := make([][]byte, len(paths))
	for index, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.New()
		_, readErr := io.Copy(digest, file)
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("source attestation failed: %v, %v", readErr, closeErr)
		}
		want[index] = digest.Sum(nil)
	}
	for _, mode := range []int{1, 2, 4} {
		t.Run(fmt.Sprintf("%d-chunks", mode), func(t *testing.T) {
			var lock sync.Mutex
			digests := make([]hash.Hash, len(paths))
			offsets := make([]int64, len(paths))
			for index := range digests {
				digests[index] = sha256.New()
			}
			h := newHarnessWithStorage(t, false, true, true, nil, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !strings.HasPrefix(r.URL.Path, "/source-qualification/") {
						next.ServeHTTP(w, r)
						return
					}
					w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
					w.Header().Set("Access-Control-Allow-Methods", "PATCH, OPTIONS")
					w.Header().Set("Access-Control-Allow-Headers", "Upload-Offset")
					w.Header().Set("Access-Control-Expose-Headers", "Upload-Offset")
					if r.Method == http.MethodOptions {
						w.WriteHeader(http.StatusNoContent)
						return
					}
					index, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/source-qualification/"))
					if err != nil || index < 0 || index >= len(paths) || r.Method != http.MethodPatch {
						http.Error(w, "invalid fixture request", http.StatusBadRequest)
						return
					}
					lock.Lock()
					prior := offsets[index]
					lock.Unlock()
					if r.Header.Get("Upload-Offset") != strconv.FormatInt(prior, 10) {
						http.Error(w, "invalid offset", http.StatusConflict)
						return
					}
					// Each file has one sequential request stream. Separate digest
					// instances let different files drain without a global body lock.
					n, err := io.Copy(digests[index], r.Body)
					if err != nil {
						return
					}
					lock.Lock()
					offsets[index] += n
					confirmed := offsets[index]
					lock.Unlock()
					w.Header().Set("Upload-Offset", strconv.FormatInt(confirmed, 10))
					w.WriteHeader(http.StatusOK)
				})
			})
			client := newTestBrowser(t)
			if err := chromedp.Run(client.ctx, chromedp.Navigate(h.origin), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
				t.Fatal(err)
			}
			if err := waitFor(client.ctx, `typeof window.__endlessfsUploadSourceTest === 'object'`, 5*time.Second); err != nil {
				t.Fatal(err)
			}
			if err := chromedp.Run(client.ctx, chromedp.Evaluate(fmt.Sprintf(`window.__endlessfsUploadSourceTest.setBufferChunks(%d); const source = document.createElement('input'); source.type = 'file'; source.multiple = true; source.id = 'source-qualification-input'; document.body.append(source)`, mode), nil), chromedp.SetUploadFiles("#source-qualification-input", paths, chromedp.ByQuery)); err != nil {
				t.Fatal(err)
			}
			var result struct {
				Files        int     `json:"files"`
				Bytes        int64   `json:"bytes"`
				Milliseconds float64 `json:"milliseconds"`
				PeakBytes    int64   `json:"peakBytes"`
			}
			expression := fmt.Sprintf(`window.__endlessfsUploadSourceTest.qualify(Array.from(document.querySelector('#source-qualification-input').files), %q, false)`, h.dataOrigin+"/source-qualification")
			if err := chromedp.Run(client.ctx, chromedp.Evaluate(expression, &result, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
				t.Fatal(err)
			}
			for index := range want {
				if string(digests[index].Sum(nil)) != string(want[index]) {
					t.Fatalf("source %d changed during transfer", index)
				}
			}
			if result.Files != 100 || result.PeakBytes > int64(mode)*(8<<20) {
				t.Fatalf("hardware bound: %+v", result)
			}
			t.Logf("hardware-source-v1 chunks=%d files=%d bytes=%d milliseconds=%.3f peakBytes=%d", mode, result.Files, result.Bytes, result.Milliseconds, result.PeakBytes)
		})
	}
}
