package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
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
	// Settings becomes visible before its asynchronous catalog has arrived.
	// Hold that response explicitly so the theme setup cannot depend on timing.
	var holdCatalog atomic.Bool
	catalogRequested := make(chan struct{})
	catalogRelease := make(chan struct{})
	var catalogOnce, releaseOnce sync.Once
	releaseCatalog := func() { releaseOnce.Do(func() { close(catalogRelease) }) }
	t.Cleanup(releaseCatalog)
	harness := newProductionLikeHarnessWithControlPlaneWrapper(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/themes" && holdCatalog.Load() {
				catalogOnce.Do(func() { close(catalogRequested) })
				select {
				case <-catalogRelease:
				case <-r.Context().Done():
					return
				}
			}
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
	if err := chromedp.Run(client.ctx, emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: "light"}})); err != nil {
		t.Fatal(err)
	}
	bootstrapBrowser(t, client, harness)
	if err := waitFor(client.ctx, `document.documentElement.dataset.theme === 'endlessfs-light'`, 5*time.Second); err != nil {
		t.Fatal("initial light theme: ", err)
	}
	if themeName == "endlessfs-dark" {
		holdCatalog.Store(true)
		if err := chromedp.Run(client.ctx, chromedp.Click(`a[href="/settings"]`, chromedp.ByQuery)); err != nil {
			t.Fatal(err)
		}
		if err := waitVisible(client.ctx, "#theme-select", 5*time.Second); err != nil {
			t.Fatal(err)
		}
		requestContext, cancelRequest := context.WithTimeout(client.ctx, 5*time.Second)
		defer cancelRequest()
		select {
		case <-catalogRequested:
		case <-requestContext.Done():
			t.Fatal("settings did not request its theme catalog")
		}
		var catalogPending bool
		if err := chromedp.Run(client.ctx, chromedp.Evaluate(`document.querySelector('#theme-select').options.length === 0`, &catalogPending)); err != nil {
			t.Fatal(err)
		}
		if !catalogPending {
			t.Fatal("catalog delay did not exercise a visible, unpopulated theme selector")
		}
		releaseCatalog()
		if err := waitFor(client.ctx, `document.querySelector('#theme-select option[value="endlessfs-dark"]') !== null`, 5*time.Second); err != nil {
			t.Fatal("theme catalog readiness: ", err)
		}
		if err := chromedp.Run(client.ctx, chromedp.Evaluate(`document.querySelector('#theme-select').value = 'endlessfs-dark'; document.querySelector('#theme-form').requestSubmit()`, nil)); err != nil {
			t.Fatal(err)
		}
		if err := waitFor(client.ctx, `document.documentElement.dataset.theme === 'endlessfs-dark'`, 5*time.Second); err != nil {
			t.Fatal("apply dark theme: ", err)
		}
		if err := chromedp.Run(client.ctx, chromedp.Click(`a[href="/"]`, chromedp.ByQuery)); err != nil {
			t.Fatal(err)
		}
	}
	if err := waitFor(client.ctx, fmt.Sprintf(`document.documentElement.dataset.theme === %q && (() => { const link = document.querySelector('#theme-stylesheet'); return link.sheet !== null && link.sheet.href === link.href; })()`, themeName), 5*time.Second); err != nil {
		t.Fatal("selected theme stylesheet readiness: ", err)
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
			const error = index === 0 ? 'The upload could not be completed. Reconnect the original source and retry after the temporary connection problem has been resolved. Reference: unique-reference.' : 'Upload interrupted. Reconnect source and retry. Reference: ' + (index < 3 ? 'separate-reference.' : 'layout-error-reference-abcdefghijklmnopqrstuvwxyz.');
			transaction.objectStore('items').put({key: ownerID + ':' + id, ownerID, id, groupID: 'layout-group', name: id + '.bin', relativePath: id + '.bin', relativeDirectory: '', directory: '/', baseDirectory: '/', size: 100, state: 'failed', confirmed: 0, uploadID: '', strategy: 'keep-both', planPhase: 'done', createdAt: 1, errorCode: index === 3 ? 'different_code' : 'connection_failed', error});
		}
		transaction.objectStore('groups').put({key: ownerID + ':layout-group', ownerID, id: 'layout-group', name: 'Interrupted folder', transferIDs: ids, directories: [], baseDirectory: '/', totalSize: 12000, state: 'failed', discoveryDone: true, strategy: 'keep-both', createdAt: 1, error: %q});
		await new Promise((resolve, reject) => { transaction.oncomplete = resolve; transaction.onerror = () => reject(transaction.error); }); database.close(); return true;
	})()`, accounts[0].UserID.String(), "Upload interrupted. Reconnect source and retry. Reference: layout-error-reference-abcdefghijklmnopqrstuvwxyz.")
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
			if err := chromedp.Run(client.ctx, chromedp.Evaluate(`[...document.querySelectorAll('#transfer-list .transfer-row-error')].filter(error => !error.closest('details:not([open])')).filter(error => { const item = error.closest('.transfer-item'); return error.scrollHeight > error.clientHeight + 1 || error.getBoundingClientRect().bottom > item.getBoundingClientRect().bottom + 1; }).length`, &cropped)); err != nil {
				t.Fatal(err)
			}
			if cropped != 0 {
				t.Fatalf("%d cropped transfer errors at %d px", cropped, width)
			}
			var clipped int
			if err := chromedp.Run(client.ctx, chromedp.Evaluate(`[...document.querySelectorAll('.transfer-tabs .tab, .transfer-group-row .transfer-row-metrics span')].filter(node => node.scrollWidth > node.clientWidth + 1 || node.getBoundingClientRect().right > document.querySelector('#transfer-panel').getBoundingClientRect().right).length`, &clipped)); err != nil {
				t.Fatal(err)
			}
			if clipped != 0 {
				t.Fatalf("%d clipped history tabs or group metrics at %d px", clipped, width)
			}
		}
	})
	t.Run("shared-failure-density-and-disclosure", func(t *testing.T) {
		// CDP viewport changes complete before the browser's resize/render tasks.
		// Settle that explicit setup transition before measuring row geometry.
		if err := chromedp.Run(client.ctx, emulation.SetDeviceMetricsOverride(1000, 900, 1, false), chromedp.Evaluate(`new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))`, nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
			t.Fatal(err)
		}
		var valid bool
		if err := chromedp.Run(client.ctx, chromedp.Evaluate(`(() => {
			const list = document.querySelector('#transfer-list');
			const shared = list.querySelectorAll('.transfer-shared-error');
			const rows = [...list.querySelectorAll('.transfer-row.failed')].filter(row => row.querySelector('details:not([open])'));
			return shared.length === 2 && [...shared].some(error => error.textContent.includes('116 files')) && [...shared].some(error => error.textContent.includes('2 files') && error.textContent.includes('separate-reference'))
				&& rows.length > 0 && rows.every(row => row.getBoundingClientRect().height === 36 && !row.querySelector('progress'))
				&& list.querySelector('details[open]').textContent.includes('unique-reference') && list.querySelector('[data-transfer-id="layout-003"] details').open;
		})()`, &valid)); err != nil {
			t.Fatal(err)
		}
		if !valid {
			t.Fatal("shared failures must appear once, with compact stable rows and inspectable diagnostics")
		}
		if err := chromedp.Run(client.ctx, chromedp.Focus("[data-transfer-id='layout-004'] summary", chromedp.ByQuery), chromedp.KeyEvent(" ")); err != nil {
			t.Fatal(err)
		}
		if err := waitFor(client.ctx, `document.querySelector('[data-transfer-id="layout-004"] details').open`, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if err := chromedp.Run(client.ctx, chromedp.Evaluate(`(() => { const detail = document.querySelector('[data-transfer-id="layout-004"] details'); return detail.textContent.includes('layout-error-reference-abcdefghijklmnopqrstuvwxyz') && detail.textContent.includes('0 B of 100 B') && detail.textContent.includes('connection_failed'); })()`, &valid)); err != nil {
			t.Fatal(err)
		}
		if !valid {
			t.Fatal("file diagnostics lost the exact reference or confirmed byte count")
		}
		if err := chromedp.Run(client.ctx, chromedp.KeyEvent(" ")); err != nil {
			t.Fatal(err)
		}
		for _, expanded := range []bool{false, true} {
			if err := chromedp.Run(client.ctx, chromedp.Focus(".transfer-group-row button[aria-expanded]", chromedp.ByQuery), chromedp.KeyEvent(" ")); err != nil {
				t.Fatal(err)
			}
			if err := waitFor(client.ctx, fmt.Sprintf(`document.activeElement.dataset.transferAction === 'disclosure' && document.activeElement.getAttribute('aria-expanded') === %q`, fmt.Sprint(expanded)), 5*time.Second); err != nil {
				t.Fatal("group disclosure lost focus: ", err)
			}
		}
		if err := chromedp.Run(client.ctx, chromedp.Evaluate(`!document.querySelector('[data-transfer-id="layout-004"] details').open`, &valid)); err != nil {
			t.Fatal(err)
		}
		if !valid {
			t.Fatal("group rerender reopened a closed failure disclosure")
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
	t.Run("toolbar-stable-single-row", func(t *testing.T) {
		var aligned bool
		if err := chromedp.Run(client.ctx, chromedp.Evaluate(`(() => { const tools = document.querySelector('.transfer-tools'); const children = [...tools.children]; return children.every(child => Math.abs(child.getBoundingClientRect().top - children[0].getBoundingClientRect().top) < 1); })()`, &aligned)); err != nil {
			t.Fatal(err)
		}
		if !aligned {
			t.Fatal("transfer clear action wraps below the search and retry controls")
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
		if err := chromedp.Run(client.ctx, emulation.SetDeviceMetricsOverride(320, 900, 1, false), emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{{Name: "prefers-reduced-motion", Value: "reduce"}}), chromedp.FullScreenshot(&screenshot, 90)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target+"-"+themeName+"-transfers-320.jpg", screenshot, 0600); err != nil {
			t.Fatal(err)
		}
		if err := chromedp.Run(client.ctx, emulation.SetDeviceMetricsOverride(1000, 900, 1, false), chromedp.Evaluate(`new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))`, nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
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
			var diagnostic string
			_ = chromedp.Run(client.ctx, chromedp.Evaluate(`(() => { const list = document.querySelector('#transfer-list'); return JSON.stringify({start: list.dataset.virtualStart, count: list.dataset.itemCount, rendered: list.dataset.renderedCount, top: list.scrollTop, height: list.scrollHeight, viewport: list.clientHeight, expanded: document.querySelector('.transfer-group-row button[aria-expanded]')?.getAttribute('aria-expanded')}); })()`, &diagnostic))
			t.Fatalf("%v: %s", err, diagnostic)
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
