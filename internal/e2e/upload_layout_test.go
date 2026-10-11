package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func TestE2EUploadLayoutKeepsColumnsErrorsAndActionsReadable(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	for _, themeName := range []string{"endlessfs-light", "endlessfs-dark"} {
		t.Run(themeName, func(t *testing.T) { testUploadLayout(t, themeName) })
	}
}

func testUploadLayout(t *testing.T, themeName string) {
	t.Helper()
	harness := newProductionLikeHarnessWithControlPlaneWrapper(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/files" || r.Method != http.MethodGet {
				next.ServeHTTP(w, r)
				return
			}
			response := httptest.NewRecorder()
			next.ServeHTTP(response, r)
			for key, values := range response.Header() {
				if key != "Content-Length" {
					w.Header()[key] = values
				}
			}
			if response.Code != http.StatusOK {
				w.WriteHeader(response.Code)
				_, _ = w.Write(response.Body.Bytes())
				return
			}
			// Layout-only API metadata: no large synthetic file body or provider
			// interoperability claim is needed to qualify column geometry.
			var result map[string]json.RawMessage
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Error(err)
				return
			}
			result["entries"] = json.RawMessage(`[{"path":"/layout-metadata-long-file-name-for-column-fit-check.bin","name":"layout-metadata-long-file-name-for-column-fit-check.bin","kind":"file","size":351902105,"fileCount":1,"mediaType":"application/octet-stream","modifiedAt":"2000-01-01T12:34:56Z","version":"layout-1"},{"path":"/larger.bin","name":"larger.bin","kind":"file","size":1324999999999,"fileCount":1,"mediaType":"application/octet-stream","modifiedAt":"2000-01-01T12:34:56Z","version":"layout-2"}]`)
			if err := json.NewEncoder(w).Encode(result); err != nil {
				t.Error(err)
			}
		})
	})
	client := newTestBrowser(t)
	bootstrapBrowser(t, client, harness)
	if themeName == "endlessfs-dark" {
		if err := chromedp.Run(client.ctx, chromedp.Click(`a[href="/settings"]`, chromedp.ByQuery)); err != nil {
			t.Fatal(err)
		}
		if err := waitVisible(client.ctx, "#theme-select", 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if err := chromedp.Run(client.ctx, chromedp.Evaluate(`document.querySelector('#theme-select').value = 'endlessfs-dark'; document.querySelector('#theme-form').requestSubmit()`, nil)); err != nil {
			t.Fatal(err)
		}
		if err := waitFor(client.ctx, `document.documentElement.dataset.theme === 'endlessfs-dark'`, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if err := chromedp.Run(client.ctx, chromedp.Click(`a[href="/"]`, chromedp.ByQuery)); err != nil {
			t.Fatal(err)
		}
	}
	if err := waitFor(client.ctx, `document.querySelectorAll('#file-rows .file-name').length === 2`, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	t.Run("columns", func(t *testing.T) {
		for _, width := range []int64{1440, 1000, 768, 320} {
			if err := chromedp.Run(client.ctx, emulation.SetDeviceMetricsOverride(width, 900, 1, false)); err != nil {
				t.Fatal(err)
			}
			var clipped []string
			if err := chromedp.Run(client.ctx, chromedp.Evaluate(`(() => {
				const clipped = [];
				for (const row of document.querySelectorAll('#file-rows tr:not(.list-spacer)')) {
					for (const [index, cell] of [...row.cells].entries()) {
						if (getComputedStyle(cell).display === 'none') { clipped.push('hidden column ' + index); continue; }
						if (cell.scrollWidth > cell.clientWidth + 1) clipped.push('overflow column ' + index);
					}
					const name = row.querySelector('.file-name');
					if (name && name.scrollWidth > name.clientWidth + 1) clipped.push('clipped filename');
				}
				return clipped;
			})()`, &clipped)); err != nil {
				t.Fatal(err)
			}
			if len(clipped) != 0 {
				t.Fatalf("columns at %d px: %v", width, clipped)
			}
		}
	})
	if target := os.Getenv("ENDLESSFS_LAYOUT_SCREENSHOT_PREFIX"); target != "" && !t.Failed() {
		var screenshot []byte
		if err := chromedp.Run(client.ctx, emulation.SetDeviceMetricsOverride(1000, 900, 1, false), chromedp.FullScreenshot(&screenshot, 90)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target+"-"+themeName+"-columns.jpg", screenshot, 0600); err != nil {
			t.Fatal(err)
		}
	}
	accounts, err := harness.repository.Accounts(client.ctx)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts=%d, %v", len(accounts), err)
	}
	seed := fmt.Sprintf(`(async () => {
		const ownerID = %q;
		const database = await new Promise((resolve, reject) => { const request = indexedDB.open('endlessfs-transfer-ledger-v1'); request.onsuccess = () => resolve(request.result); request.onerror = () => reject(request.error); });
		const transaction = database.transaction(['items', 'groups'], 'readwrite');
		const ids = [];
		for (let index = 0; index < 120; index++) {
			const id = 'layout-' + String(index).padStart(3, '0'); ids.push(id);
			transaction.objectStore('items').put({key: ownerID + ':' + id, ownerID, id, groupID: 'layout-group', name: id + '.bin', relativePath: id + '.bin', relativeDirectory: '', directory: '/', baseDirectory: '/', size: 100, state: 'failed', confirmed: 0, uploadID: '', strategy: 'keep-both', planPhase: 'done', createdAt: 1, error: 'The upload could not be completed. Reconnect the original source and retry after the temporary connection problem has been resolved. Reference: layout-error-reference-abcdefghijklmnopqrstuvwxyz.'});
		}
		transaction.objectStore('groups').put({key: ownerID + ':layout-group', ownerID, id: 'layout-group', name: 'Interrupted folder', transferIDs: ids, directories: [], baseDirectory: '/', totalSize: 12000, state: 'failed', discoveryDone: true, strategy: 'keep-both', createdAt: 1, error: %q});
		await new Promise((resolve, reject) => { transaction.oncomplete = resolve; transaction.onerror = () => reject(transaction.error); }); database.close(); return true;
	})()`, accounts[0].UserID.String(), strings.Repeat("The folder could not be uploaded. ", 6))
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(seed, nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }), chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="failed"]').textContent === '(120)'`, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, emulation.SetDeviceMetricsOverride(1440, 900, 1, false), chromedp.Click("#open-transfers", chromedp.ByQuery), chromedp.Click(".transfer-group-row button[aria-expanded]", chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	t.Run("errors", func(t *testing.T) {
		for _, width := range []int64{1440, 1000, 320} {
			if err := chromedp.Run(client.ctx, emulation.SetDeviceMetricsOverride(width, 900, 1, false)); err != nil {
				t.Fatal(err)
			}
			var cropped int
			if err := chromedp.Run(client.ctx, chromedp.Evaluate(`[...document.querySelectorAll('#transfer-list .transfer-row-error')].filter(error => { const item = error.closest('.transfer-item'); return error.scrollHeight > error.clientHeight + 1 || error.getBoundingClientRect().bottom > item.getBoundingClientRect().bottom + 1; }).length`, &cropped)); err != nil {
				t.Fatal(err)
			}
			if cropped != 0 {
				t.Fatalf("%d cropped transfer errors at %d px", cropped, width)
			}
		}
	})
	t.Run("scrollbar-clearance", func(t *testing.T) {
		var overlapping int
		if err := chromedp.Run(client.ctx, chromedp.Evaluate(`(() => { const list = document.querySelector('#transfer-list'); const edge = list.getBoundingClientRect().right - 16; return [...list.querySelectorAll('.transfer-row-tail button, .transfer-row-tail .app-icon')].filter(control => control.getBoundingClientRect().right > edge + 1).length; })()`, &overlapping)); err != nil {
			t.Fatal(err)
		}
		if overlapping != 0 {
			t.Fatalf("%d action controls overlap the reserved scrollbar area", overlapping)
		}
	})
	if target := os.Getenv("ENDLESSFS_LAYOUT_SCREENSHOT_PREFIX"); target != "" && !t.Failed() {
		var screenshot []byte
		if err := chromedp.Run(client.ctx, emulation.SetDeviceMetricsOverride(1000, 900, 1, false), chromedp.FullScreenshot(&screenshot, 90)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target+"-"+themeName+"-transfers.jpg", screenshot, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("variable-height-scroll-anchor", func(t *testing.T) {
		if err := chromedp.Run(client.ctx, chromedp.Evaluate(`(() => {
			const list = document.querySelector('#transfer-list'); list.scrollTop = list.scrollHeight - list.clientHeight - 10;
			const bounds = list.getBoundingClientRect();
			const item = [...list.querySelectorAll('.transfer-item')].find(item => item.getBoundingClientRect().bottom > bounds.top);
			list.dataset.anchorPosition = item.getAttribute('aria-posinset'); list.dataset.anchorTop = String(item.getBoundingClientRect().top); list.dispatchEvent(new Event('scroll'));
		})()`, nil)); err != nil {
			t.Fatal(err)
		}
		if err := waitFor(client.ctx, `Number(document.querySelector('#transfer-list').dataset.virtualStart) > 0`, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		var delta float64
		if err := chromedp.Run(client.ctx, chromedp.Evaluate(`(() => { const list = document.querySelector('#transfer-list'); const item = list.querySelector('[aria-posinset="' + list.dataset.anchorPosition + '"]'); return item ? Math.abs(item.getBoundingClientRect().top - Number(list.dataset.anchorTop)) : 99999; })()`, &delta)); err != nil {
			t.Fatal(err)
		}
		if delta > 2 {
			t.Fatalf("visible transfer jumped %.1f px when its virtual window shifted", delta)
		}
		var rendered int
		if err := chromedp.Run(client.ctx, chromedp.Evaluate(`document.querySelectorAll('#transfer-list .transfer-item').length`, &rendered)); err != nil {
			t.Fatal(err)
		}
		if rendered > 72 {
			t.Fatalf("virtual window rendered %d items", rendered)
		}
		if err := chromedp.Run(client.ctx, chromedp.Evaluate(`(() => {
			const list = document.querySelector('#transfer-list'); list.scrollTop = 1;
			const item = list.querySelector('.transfer-item'); list.dataset.anchorPosition = item.getAttribute('aria-posinset'); list.dataset.anchorTop = String(item.getBoundingClientRect().top); list.dispatchEvent(new Event('scroll'));
		})()`, nil)); err != nil {
			t.Fatal(err)
		}
		if err := waitFor(client.ctx, `Number(document.querySelector('#transfer-list').dataset.virtualStart) === 0`, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if err := chromedp.Run(client.ctx, chromedp.Evaluate(`(() => { const list = document.querySelector('#transfer-list'); const item = list.querySelector('[aria-posinset="' + list.dataset.anchorPosition + '"]'); return item ? Math.abs(item.getBoundingClientRect().top - Number(list.dataset.anchorTop)) : 99999; })()`, &delta)); err != nil {
			t.Fatal(err)
		}
		if delta > 2 {
			t.Fatalf("visible transfer jumped %.1f px on reverse scrolling", delta)
		}
	})
	client.assertNoExternalRequests(t, harness)
}
