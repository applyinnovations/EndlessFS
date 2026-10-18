package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// These are browser protocol-binding tests, using the existing synthetic
// recovery queue. They do not claim durable provider publication acceptance.
// Matching controls prove the fixture can complete, including a real one-byte
// browser fetch; mismatched nested IDs must stop before consuming that result.
func TestE2EUploadRecoveryRejectsMisboundNestedIDsBeforeProgressOrData(t *testing.T) {
	requireSmartRecoveryE2E(t)
	for _, nested := range []string{"status", "capability"} {
		for _, valid := range []bool{true, false} {
			name := nested + "/misbound"
			if valid {
				name = nested + "/matching-control"
			}
			t.Run(name, func(t *testing.T) {
				var pages, writes, completions, cancels atomic.Int64
				var h harness
				h = newHarnessWithStorage(t, false, true, true, func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/api/v1/uploads/batch/resume" {
							pages.Add(1)
							var request struct {
								UploadIDs []string `json:"uploadIDs"`
							}
							if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.UploadIDs) != 1 || request.UploadIDs[0] != "old-probe-0" {
								t.Error("binding fixture did not request the original synthetic attempt")
								w.WriteHeader(http.StatusBadRequest)
								return
							}
							id := request.UploadIDs[0]
							status := map[string]any{"uploadID": id, "state": "completed", "declaredSize": 1, "confirmedOffset": 1, "dataComplete": true, "protocol": "resumable", "expiresAt": "2030-01-01T00:00:00Z"}
							item := map[string]any{"index": 0, "uploadID": id, "status": status, "retryable": false, "nextAction": "done"}
							if nested == "status" && !valid {
								status["uploadID"] = "foreign-upload"
							}
							if nested == "capability" {
								status["state"], status["confirmedOffset"], status["dataComplete"] = "active", 0, false
								capID := id
								if !valid {
									capID = "foreign-upload"
								}
								item["nextAction"] = "transfer"
								item["capability"] = map[string]any{"uploadID": capID, "url": h.dataOrigin + "/binding-write", "method": http.MethodPatch, "headers": map[string]string{}, "framing": "offset-header", "protocol": "resumable", "declaredSize": 1, "expiresAt": "2030-01-01T00:00:00Z"}
							}
							w.Header().Set("Content-Type", "application/json")
							_ = json.NewEncoder(w).Encode(map[string]any{"uploads": []any{item}})
							return
						}
						if r.URL.Path == "/api/v1/uploads/batch/complete" {
							completions.Add(1)
							w.Header().Set("Content-Type", "application/json")
							_, _ = w.Write([]byte(`{"entries":[{"path":"/probe-0.bin","name":"probe-0.bin","type":"file","size":1}]}`))
							return
						}
						if r.URL.Path == "/api/v1/uploads/batch" && r.Method == http.MethodDelete {
							cancels.Add(1)
							var request struct {
								UploadIDs []string `json:"uploadIDs"`
							}
							if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.UploadIDs) != 1 || request.UploadIDs[0] != "old-probe-0" {
								t.Error("misbound nested ID replaced the original cancellation identity")
							}
							w.WriteHeader(http.StatusNoContent)
							return
						}
						next.ServeHTTP(w, r)
					})
				}, func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path != "/binding-write" {
							next.ServeHTTP(w, r)
							return
						}
						w.Header().Set("Access-Control-Allow-Origin", h.origin)
						w.Header().Set("Access-Control-Allow-Methods", "PATCH, OPTIONS")
						w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Upload-Offset")
						w.Header().Set("Access-Control-Expose-Headers", "Upload-Offset")
						if r.Method == http.MethodOptions {
							w.WriteHeader(http.StatusNoContent)
							return
						}
						body, err := io.ReadAll(r.Body)
						if err != nil || r.Method != http.MethodPatch || string(body) != "x" {
							t.Error("binding fixture did not transfer the exact synthetic one-byte source")
						}
						writes.Add(1)
						w.Header().Set("Upload-Offset", "1")
						w.WriteHeader(http.StatusNoContent)
					})
				})
				client := newTestBrowser(t)
				bootstrapBrowser(t, client, h)
				// Inject a held retry scheduler rather than racing a one-second
				// timer. Cancellation below removes its token; no sleeps or
				// production timeout/retry settings are changed.
				const seed = `(()=>{const nativeSet=window.setTimeout.bind(window),nativeClear=window.clearTimeout.bind(window);let token=-1;const held=new Set();window.__bindingReviewHeld=held;window.setTimeout=(fn,delay,...args)=>{if(delay>=1000){const id=token--;held.add(id);return id}return nativeSet(fn,delay,...args)};window.clearTimeout=id=>{if(!held.delete(id))nativeClear(id)};window.__endlessfsUploadWorkerPoolTest.seedRecovery(1)})()`
				if err := chromedp.Run(client.ctx, chromedp.Evaluate(seed, nil)); err != nil {
					t.Fatal(err)
				}
				if err := waitFor(client.ctx, `(()=>{const s=window.__endlessfsUploadWorkerPoolTest.snapshot();return s.active===0&&(s.states.complete===1||s.states['retry-wait']===1)})()`, 10*time.Second); err != nil {
					t.Fatalf("binding fixture did not settle: %v (%s)", err, browserStatus(client.ctx))
				}
				var snapshot struct {
					Active int            `json:"active"`
					States map[string]int `json:"states"`
				}
				if err := chromedp.Run(client.ctx, chromedp.Evaluate(`window.__endlessfsUploadWorkerPoolTest.snapshot()`, &snapshot)); err != nil {
					t.Fatal(err)
				}
				if !valid && snapshot.States["retry-wait"] == 1 {
					if err := chromedp.Run(client.ctx, chromedp.Click("#open-transfers", chromedp.ByQuery)); err != nil {
						t.Fatal(err)
					}
					if err := waitVisible(client.ctx, `button[aria-label="Cancel folder upload Recovery probe"]`, 5*time.Second); err != nil {
						t.Fatal(err)
					}
					if err := chromedp.Run(client.ctx, chromedp.Click(`button[aria-label="Cancel folder upload Recovery probe"]`, chromedp.ByQuery)); err != nil {
						t.Fatal(err)
					}
					if err := waitFor(client.ctx, `window.__endlessfsUploadWorkerPoolTest.snapshot().states.cancelled===1`, 5*time.Second); err != nil {
						t.Fatal(err)
					}
				}
				if pages.Load() != 1 {
					t.Fatal("binding fixture repeated recovery instead of stopping its original attempt")
				}
				if valid {
					wantWrites := int64(0)
					wantCompletions := int64(0)
					if nested == "capability" {
						wantWrites = 1
						wantCompletions = 1
					}
					if snapshot.States["complete"] != 1 || writes.Load() != wantWrites || completions.Load() != wantCompletions {
						t.Fatalf("matching positive control did not complete: states=%v writes=%d completions=%d", snapshot.States, writes.Load(), completions.Load())
					}
				} else if snapshot.States["retry-wait"] != 1 || snapshot.States["complete"] != 0 || writes.Load() != 0 || completions.Load() != 0 || cancels.Load() != 1 {
					t.Fatalf("misbound nested %s was consumed: states=%v writes=%d completions=%d cancels=%d", nested, snapshot.States, writes.Load(), completions.Load(), cancels.Load())
				}
				for _, request := range client.requestSnapshot() {
					if strings.HasPrefix(request, "POST /api/v1/uploads/batch/admit") {
						t.Fatal("binding fixture created a replacement admission")
					}
				}
				client.assertNoExternalRequests(t, h)
				t.Logf("nested=%s matching=%t recovery_pages=%d provider_writes=%d completion_requests=%d canceled_original=%d", nested, valid, pages.Load(), writes.Load(), completions.Load(), cancels.Load())
			})
		}
	}
}
