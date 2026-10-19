package e2e

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// This focused restoration regression uses a browser-native, structured-cloned
// OPFS handle. It does not replace queryPermission/getFile or inject File values
// into live transfers. The real application's reload path must recover the File
// and enter real smart planning; the first size response is held so subsequent
// successful planning cannot hide obsolete source-required state.
//
// It is separate from the aggregate538/338 nonempty-field counts in the earlier
// concurrent diagnostic, which did not record message/code categories.
func TestE2EUploadStoredSourceRestoreClearsObsoleteSourceRequiredBeforeSmartPlanning(t *testing.T) {
	parallelE2EFixture(t)
	for _, scenario := range []struct {
		name    string
		message string
	}{
		{name: "message-and-code", message: "Reconnect the source to continue."},
		{name: "code-only"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			testStoredSourceRestoreClearsObsoleteSourceRequired(t, scenario.message)
		})
	}
}

func testStoredSourceRestoreClearsObsoleteSourceRequired(t *testing.T, currentMessage string) {
	t.Helper()
	requireSmartRecoveryE2E(t)
	started, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var hold atomic.Bool
	writes := &smartRecoveryWrites{byIndex: make(map[int]int)}
	h := newSmartRecoveryHarness(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/uploads/plan/sizes" && hold.CompareAndSwap(true, false) {
				started <- struct{}{}
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
	bootstrapSmartRecovery(t, client, h.harness)
	scope := smartRecoveryScope(t, h)
	const root = "stored-source-folder"
	prepareSmartRecoveryDirectories(t, h, scope, root, 1)
	// Another real four-byte entry makes this source ambiguous by size, forcing
	// local fingerprinting and the actual fingerprint-planning endpoint.
	seedSmartRecoveryAnchor(t, h, scope, 1)

	var seeded bool
	seed := fmt.Sprintf(`(async()=>{
		if(typeof FileSystemFileHandle!=='function'||typeof navigator.storage?.getDirectory!=='function')return false;
		const opfs=await navigator.storage.getDirectory();
		const handle=await opfs.getFileHandle('source-00000.bin',{create:true});
		if(!(handle instanceof FileSystemFileHandle)||typeof handle.queryPermission!=='function'||await handle.queryPermission({mode:'read'})!=='granted')return false;
		const writer=await handle.createWritable();await writer.write(new Uint8Array([0,0,0,0]));await writer.close();
		const file=await handle.getFile(),ownerID=%q,id='stored-source-item',groupID='stored-source-group',relativeDirectory=%q;
		if(file.size!==4)return false;
		const db=await new Promise((resolve,reject)=>{const r=indexedDB.open('endlessfs-transfer-ledger-v1');r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});
		const tx=db.transaction(['items','groups','sources'],'readwrite');
		tx.objectStore('items').put({key:ownerID+':'+id,ownerID,id,groupID,name:file.name,relativePath:relativeDirectory+'/'+file.name,relativeDirectory,directory:'/'+relativeDirectory,baseDirectory:'/',size:file.size,lastModified:file.lastModified,mediaType:'application/octet-stream',state:'needs-source',confirmed:0,uploadID:'',strategy:'smart-merge',planPhase:'size-pending',createdAt:1,errorCode:'source_required',error:%q,errorHistory:[{message:'Reference: stored-source-history.',code:'unavailable',at:1}]});
		tx.objectStore('groups').put({key:ownerID+':'+groupID,ownerID,id:groupID,name:%q,transferIDs:[id],directories:[%q,%q,relativeDirectory],baseDirectory:'/',totalSize:4,state:'queued',discoveryDone:true,strategy:'smart-merge',createdAt:1});
		tx.objectStore('sources').put({key:ownerID+':'+id,ownerID,handle});
		await new Promise((resolve,reject)=>{tx.oncomplete=resolve;tx.onerror=()=>reject(tx.error);tx.onabort=()=>reject(tx.error)});
		const stored=await new Promise((resolve,reject)=>{const r=db.transaction('sources','readonly').objectStore('sources').get(ownerID+':'+id);r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});db.close();
		return stored?.handle instanceof FileSystemFileHandle&&await stored.handle.queryPermission({mode:'read'})==='granted'&&(await stored.handle.getFile()).size===4;
	})()`, scope.UserID().String(), root+"/existing-00/nested", currentMessage, root, root, root+"/existing-00")
	if err := runStage(client.ctx, 10*time.Second, chromedp.Evaluate(seed, &seeded, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil || !seeded {
		t.Fatalf("native permission-granted stored-source fixture unavailable: ready=%t err=%v", seeded, err)
	}

	h.stateLedger.Reset()
	h.fileLedger.Reset()
	requestStart := len(client.requestSnapshot())
	hold.Store(true)
	var priorTimeOrigin float64
	if err := runStage(client.ctx, 10*time.Second, chromedp.Evaluate(`performance.timeOrigin`, &priorTimeOrigin), reloadLoopbackBrowser()); err != nil {
		t.Fatal(err)
	}
	ready := fmt.Sprintf(`performance.timeOrigin>%.6f&&document.readyState==='complete'&&document.querySelector('#loading-view').hidden&&!document.querySelector('#authenticated-view').hidden&&document.querySelector('#auth-view').hidden&&document.querySelector('#app').dataset.state==='authenticated'`, priorTimeOrigin)
	if err := waitFor(client.ctx, ready, 15*time.Second); err != nil {
		t.Fatalf("stored-source reload did not enter the authenticated workspace: %v", err)
	}
	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("native stored source did not automatically enter real smart size planning")
	}
	// This persisted-state check is deliberately before the held size response.
	// No reconnect button, picker, explicit retry or successful size decision
	// clears the code on behalf of automatic stored-handle restoration.
	assertSmartRecoveryReconnectedErrors(t, client, 1, 0)
	assertStoredSourceRestoreHistory(t, client)
	if len(writes.snapshot()) != 0 {
		t.Fatal("source bytes transferred before the held size decision")
	}
	unblock()
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(1)'&&document.querySelector('#transfer-check-label').textContent==='Preparing 100%'`, 30*time.Second); err != nil {
		t.Fatalf("restored source did not complete real smart upload: %v", err)
	}
	writes.assertExactly(t, 0, 1)
	assertSmartRecoveryLedger(t, client, 1, 1)
	assertStoredSourceRestoreHistory(t, client)
	stateEvents, fileEvents := h.stateLedger.Events(), h.fileLedger.Events()
	assertSmartRecoveryProviderBudget(t, stateEvents, fileEvents, 1, 0)
	requests := client.requestSnapshot()[requestStart:]
	for _, route := range []string{"POST /api/v1/uploads/plan/sizes", "POST /api/v1/uploads/plan/fingerprints", "POST /api/v1/uploads/batch/admit", "POST /api/v1/uploads/batch/actions"} {
		if countExactRequest(requests, route) == 0 {
			t.Fatalf("automatic stored-source recovery bypassed %s", route)
		}
	}
	assertSmartRecoveryBulkRoutes(t, requests)
	h.assertProviderMetadataOnly(t)
	client.assertNoExternalRequests(t, h.harness)
	verifySmartRecoveryFiles(t, h, scope, root, 1)
}

func assertStoredSourceRestoreHistory(t *testing.T, client *testBrowser) {
	t.Helper()
	var valid bool
	script := `(async()=>{for(let attempt=0;attempt<500;attempt++){
		const db=await new Promise((resolve,reject)=>{const r=indexedDB.open('endlessfs-transfer-ledger-v1');r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});
		const item=await new Promise((resolve,reject)=>{const r=db.transaction('items','readonly').objectStore('items').getAll();r.onsuccess=()=>resolve(r.result[0]);r.onerror=()=>reject(r.error)});db.close();
		const history=item?.errorHistory||[];
		if(item&&history.length<=3&&history.filter(h=>h.message==='Reference: stored-source-history.').length===1&&history.every(h=>String(h.message||'').length<=240&&String(h.code||'').length<=80))return true;
		await new Promise(resolve=>setTimeout(resolve,10));}return false})()`
	if err := runStage(client.ctx, 10*time.Second, chromedp.Evaluate(script, &valid, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil || !valid {
		t.Fatalf("automatic stored-source restoration lost or duplicated useful bounded history: valid=%t err=%v", valid, err)
	}
}
