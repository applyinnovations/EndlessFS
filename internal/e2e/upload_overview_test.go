package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

func TestE2ELatestUploadProgressExcludesFailedHistory(t *testing.T) {
	parallelE2EFixture(t)
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	var failCompletion atomic.Bool
	var blockAdmission atomic.Bool
	admissionGate := make(chan struct{})
	var releaseAdmission sync.Once
	release := func() { releaseAdmission.Do(func() { close(admissionGate) }) }
	t.Cleanup(release)
	failCompletion.Store(true)
	harness := newPortableHarnessWithControlPlaneWrapper(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/uploads/batch/admit" && r.Method == http.MethodPost && blockAdmission.Load() {
				select {
				case <-admissionGate:
				case <-r.Context().Done():
					return
				}
			}
			if r.URL.Path == "/api/v1/uploads/batch/actions" && failCompletion.Load() {
				var request struct {
					Items []domain.UploadActionItem `json:"items"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				results := make([]domain.UploadActionResult, len(request.Items))
				for index, item := range request.Items {
					results[index] = domain.UploadActionResult{Index: index, UploadID: item.UploadID, Action: item.Action, Outcome: "rejected", ErrorKind: domain.ErrorInvalid, NextAction: "stop"}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"uploads": results})
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	client := newTestBrowser(t)
	bootstrapBrowser(t, client, harness)
	upload := func(names ...string) {
		t.Helper()
		paths := make([]string, len(names))
		for index, name := range names {
			paths[index] = filepath.Join(t.TempDir(), name)
			body := name
			if name == "old-failure.bin" {
				body = strings.Repeat("x", 1<<20)
			}
			if err := os.WriteFile(paths[index], []byte(body), 0600); err != nil {
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
	}
	wait := func(expression string) {
		t.Helper()
		if err := waitFor(client.ctx, expression, 10*time.Second); err != nil {
			var overview string
			_ = chromedp.Run(client.ctx, chromedp.Evaluate(`document.querySelector('#open-transfers').getAttribute('aria-label')`, &overview))
			var rows string
			_ = chromedp.Run(client.ctx, chromedp.Evaluate(`document.querySelector('#transfer-list').textContent`, &rows))
			t.Fatalf("%s: %v; overview=%s; rows=%s", expression, err, overview, rows)
		}
	}
	upload("old-failure.bin")
	wait(`document.querySelector('[data-transfer-count="failed"]').textContent === '(1)'`)
	wait(`document.querySelector('#header-transfer-eta').textContent === 'Attention'`)
	failCompletion.Store(false)
	blockAdmission.Store(true)
	upload("new-first.txt", "new-second.txt")
	wait(`document.querySelector('#open-transfers').getAttribute('aria-label').includes('active of 2 files') && document.querySelector('[data-transfer-count="failed"]').textContent === '(1)'`)
	release()
	wait(`document.querySelector('[data-transfer-count="complete"]').textContent === '(2)'`)
	wait(`document.querySelector('#header-transfer-percent').textContent === '100%' && document.querySelector('#header-transfer-eta').textContent === 'Complete'`)
	wait(`document.querySelector('#open-transfers').getAttribute('aria-label').includes('0 active of 2 files')`)
	wait(`document.querySelector('[data-transfer-count="failed"]').textContent === '(1)' && document.querySelector('[data-transfer-count="all"]').textContent === '(3)' && !document.querySelector('#clear-failed-transfers').disabled`)
	// The cohort is derived from the existing durable group, not a process-only
	// marker that disappears on reload. Earlier failures remain recoverable.
	if err := chromedp.Run(client.ctx, chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	wait(`document.querySelector('[data-transfer-count="all"]').textContent === '(3)'`)
	wait(`document.querySelector('#header-transfer-percent').textContent === '100%' && document.querySelector('#header-transfer-eta').textContent === 'Complete'`)
	client.assertNoExternalRequests(t, harness)
}

func TestE2ELostCompletionResponseRetainsOrderAcrossReload(t *testing.T) {
	parallelE2EFixture(t)
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	t.Run("explicit-ordinal", func(t *testing.T) { testLostCompletionReplayOrder(t, false) })
	t.Run("existing-folder-order", func(t *testing.T) { testLostCompletionReplayOrder(t, true) })
}

func testLostCompletionReplayOrder(t *testing.T, omitOrdinal bool) {
	t.Helper()
	var committed atomic.Bool
	var calls atomic.Int64
	var admissions, actionCalls, dataWrites, dataBytes atomic.Int64
	type completionCapture struct {
		Key   string
		Items []domain.CompleteUploadBatchItem
	}
	var capturesMu sync.Mutex
	var captures []completionCapture
	harness := newHarnessWithStorage(t, false, false, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/uploads/batch/admit" {
				admissions.Add(1)
			}
			if r.URL.Path == "/api/v1/uploads/batch/actions" {
				actionCalls.Add(1)
			}
			if r.URL.Path != "/api/v1/uploads/batch/complete" {
				next.ServeHTTP(w, r)
				return
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var request struct {
				Uploads []domain.CompleteUploadBatchItem `json:"uploads"`
			}
			if err := json.Unmarshal(body, &request); err != nil {
				t.Error(err)
				return
			}
			capturesMu.Lock()
			captures = append(captures, completionCapture{Key: r.Header.Get("Idempotency-Key"), Items: request.Uploads})
			capturesMu.Unlock()
			if calls.Add(1) != 1 {
				next.ServeHTTP(w, r)
				return
			}
			recorded := httptest.NewRecorder()
			next.ServeHTTP(recorded, r)
			if recorded.Code != http.StatusOK {
				t.Errorf("first completion returned %d", recorded.Code)
			}
			committed.Store(true)
			// Server publication succeeds, but the response is lost when the
			// browser reloads. Replay must preserve the original ordered intent.
			<-r.Context().Done()
		})
	}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut || r.Method == http.MethodPatch {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				dataWrites.Add(1)
				dataBytes.Add(int64(len(body)))
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
			next.ServeHTTP(w, r)
		})
	})
	client := newTestBrowser(t)
	bootstrapBrowser(t, client, harness)
	// IndexedDB key order is deliberately the reverse of discovery order.
	// Model an old browser's completion intent over genuine new admissions and
	// transferred bytes. Persist the legacy key/order BEFORE its first request;
	// only that first transport envelope is changed. Reload runs ordinary code.
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(`(() => {
		let sequence=999;crypto.randomUUID=()=>'00000000-0000-4000-8000-'+String(sequence--).padStart(12,'0');
		const fixture=window.__legacyOrderedCompletion={key:'legacy-ordered-completion-00001',used:false,records:new Map(),omitOrdinal:false};
		const put=IDBObjectStore.prototype.put;
		IDBObjectStore.prototype.put=function(value,...rest){
			if(this.name==='items'&&fixture.records.has(value?.id)){
				value={...value,completionProtocol:'',completionBatchKey:fixture.key,completionBatchIndex:fixture.records.get(value.id)};
				if(fixture.omitOrdinal)delete value.completionBatchIndex;
			}
			return put.call(this,value,...rest);
		};
		const original=window.fetch.bind(window);
		window.fetch=async(input,options={})=>{
			if(String(input)!=='/api/v1/uploads/batch/actions'||fixture.used)return original(input,options);
			const request=JSON.parse(options.body);
			if(request.items?.length!==2||request.items.some(x=>x.action!=='complete'))throw new Error('Legacy fixture requires both completed sources');
			fixture.used=true;
			const db=await new Promise((resolve,reject)=>{const r=indexedDB.open('endlessfs-transfer-ledger-v1');r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});
			const tx=db.transaction(['items','groups'],'readwrite'),store=tx.objectStore('items');
			const records=await new Promise((resolve,reject)=>{const r=store.getAll();r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});
			for(const [index,item] of request.items.entries()){
				const record=records.find(x=>x.uploadID===item.uploadID);
				if(!record?.awaitingCompletion||record.crc32c!==item.crc32c)throw new Error('Legacy fixture has no durable uploaded receipt');
				fixture.records.set(record.id,index);store.put({...record,completionProtocol:'',completionBatchKey:fixture.key,completionBatchIndex:index});
			}
			await new Promise((resolve,reject)=>{tx.oncomplete=resolve;tx.onerror=()=>reject(tx.error)});db.close();
			const headers=new Headers(options.headers);headers.set('Idempotency-Key',fixture.key);
			return original('/api/v1/uploads/batch/complete',{...options,headers,body:JSON.stringify({uploads:request.items.map(({uploadID,crc32c})=>({uploadID,crc32c}))})});
		};
	})()`, nil)); err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(t.TempDir(), "first.txt"), filepath.Join(t.TempDir(), "second.txt")}
	var expectedBytes int64
	for _, path := range paths {
		body := []byte("synthetic source " + filepath.Base(path))
		expectedBytes += int64(len(body))
		if err := os.WriteFile(path, body, 0600); err != nil {
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
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for !committed.Load() {
		select {
		case <-deadline.C:
			t.Fatal("server did not publish the initial completion")
		case <-ticker.C:
		}
	}
	accounts, err := harness.repository.Accounts(client.ctx)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("registered account: %v", err)
	}
	scope, err := domain.NewScope(accounts[0].UserID, domain.AreaLive)
	if err != nil {
		t.Fatal(err)
	}
	versions := make(map[string]domain.Version)
	for _, name := range []string{"first.txt", "second.txt"} {
		entry, err := harness.files.Stat(client.ctx, scope, domain.MustParseUserPath("/"+name))
		if err != nil || entry.Kind != domain.EntryFile {
			t.Fatalf("lost-response fixture did not actually publish %s: %+v %v", name, entry, err)
		}
		versions[name] = entry.Version
	}
	var persisted bool
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(`(async()=>{
		const db=await new Promise((resolve,reject)=>{const r=indexedDB.open('endlessfs-transfer-ledger-v1');r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});
		const tx=db.transaction(['items','groups'],'readonly');
		const records=await new Promise((resolve,reject)=>{const r=tx.objectStore('items').getAll();r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});
		const groups=await new Promise((resolve,reject)=>{const r=tx.objectStore('groups').getAll();r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});db.close();
		const ordered=[...records].sort((a,b)=>a.completionBatchIndex-b.completionBatchIndex),group=groups.find(x=>x.id===ordered[0]?.groupID);
		return records.length===2&&records.map(x=>x.name).join(',')==='second.txt,first.txt'
			&&ordered.map(x=>x.name).join(',')==='first.txt,second.txt'&&records.every(x=>x.awaitingCompletion&&x.completionBatchKey==='legacy-ordered-completion-00001'&&x.completionProtocol!=='actions')
			&&Boolean(group)&&group.transferIDs.join(',')===ordered.map(x=>x.id).join(',');
	})()`, &persisted, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil || !persisted {
		t.Fatalf("legacy completion intent/order was not durably captured: valid=%t err=%v", persisted, err)
	}
	if omitOrdinal {
		// Match the pre-ordinal browser ledger: folder.transferIDs is already
		// persisted in discovery order; only the new optional ordinal is absent.
		if err := chromedp.Run(client.ctx, chromedp.Evaluate(`(async () => {
			window.__legacyOrderedCompletion.omitOrdinal=true;
			const database = await new Promise((resolve, reject) => { const request = indexedDB.open('endlessfs-transfer-ledger-v1'); request.onsuccess = () => resolve(request.result); request.onerror = () => reject(request.error); });
			const transaction = database.transaction('items', 'readwrite');
			const store = transaction.objectStore('items');
			const request = store.openCursor();
			request.onsuccess = () => { const cursor = request.result; if (!cursor) return; const item = cursor.value; delete item.completionBatchIndex; cursor.update(item); cursor.continue(); };
			await new Promise((resolve, reject) => { transaction.oncomplete = resolve; transaction.onerror = () => reject(transaction.error); });
			database.close(); return true;
		})()`, nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
			t.Fatal(err)
		}
	}
	if err := chromedp.Run(client.ctx, chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent === '(2)' && document.querySelector('[data-transfer-count="failed"]').textContent === '(0)'`, 10*time.Second); err != nil {
		var rows string
		_ = chromedp.Run(client.ctx, chromedp.Evaluate(`document.querySelector('#transfer-list').textContent`, &rows))
		t.Fatalf("ordered completion replay failed: %v; rows=%s", err, rows)
	}
	if got := countExactRequest(client.requestSnapshot(), "POST /api/v1/uploads/batch/admit"); got != 1 {
		t.Fatalf("completion replay allocated new sessions: %d admissions", got)
	}
	if admissions.Load() != 1 || actionCalls.Load() != 0 || calls.Load() != 2 {
		t.Fatalf("legacy replay changed protocol or admission membership: admissions=%d actions=%d legacy-completions=%d", admissions.Load(), actionCalls.Load(), calls.Load())
	}
	capturesMu.Lock()
	recorded := append([]completionCapture(nil), captures...)
	capturesMu.Unlock()
	if len(recorded) != 2 || recorded[0].Key != "legacy-ordered-completion-00001" || recorded[1].Key != recorded[0].Key || len(recorded[0].Items) != 2 || !reflect.DeepEqual(recorded[0].Items, recorded[1].Items) {
		t.Fatalf("legacy cohort key/checksum/order changed across reload: %+v", recorded)
	}
	if dataWrites.Load() != 2 || dataBytes.Load() != expectedBytes {
		t.Fatalf("lost completion retransferred bytes: writes=%d bytes=%d expected-bytes=%d", dataWrites.Load(), dataBytes.Load(), expectedBytes)
	}
	for name, version := range versions {
		entry, err := harness.files.Stat(client.ctx, scope, domain.MustParseUserPath("/"+name))
		if err != nil || entry.Version != version {
			t.Fatalf("legacy replay republished %s: before=%s after=%+v err=%v", name, version, entry, err)
		}
	}
	t.Logf("legacy ordered completion replay: omitted-ordinal=%t admissions=1 completion-requests=2 data-writes=2 retransferred-bytes=0", omitOrdinal)
	client.assertNoExternalRequests(t, harness)
}
