package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func TestE2EUploadPreparingCreditsCompletedSiblingsAndReconnectsExpiredPage(t *testing.T) {
	parallelE2EFixture(t)
	runPreparingExpiredRecovery(t, 0)
}

func TestE2EUploadExpiredRetryWaitsForDurableIdentity(t *testing.T) {
	parallelE2EFixture(t)
	runPreparingExpiredRecovery(t, 1)
}

func TestE2EUploadExpiredRetryWaitsForDurableAdmissionKey(t *testing.T) {
	parallelE2EFixture(t)
	runPreparingExpiredRecovery(t, 2)
}

func runPreparingExpiredRecovery(t *testing.T, rejectStage int) {
	t.Helper()
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	var recoveryCalls atomic.Int64
	started := make(chan struct{}, 1)
	recoveryGate := make(chan struct{})
	var recoveryOnce sync.Once
	release := func() { recoveryOnce.Do(func() { close(recoveryGate) }) }
	t.Cleanup(release)
	h := newPortableHarnessWithControlPlaneWrapper(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/uploads/batch/resume" {
				next.ServeHTTP(w, r)
				return
			}
			recoveryCalls.Add(1)
			select {
			case started <- struct{}{}:
			default:
			}
			var body struct {
				UploadIDs []string `json:"uploadIDs"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			select {
			case <-recoveryGate:
			case <-r.Context().Done():
				return
			}
			items := make([]map[string]any, len(body.UploadIDs))
			for index, id := range body.UploadIDs {
				items[index] = map[string]any{"index": index, "uploadID": id, "status": map[string]any{"uploadID": id, "state": "expired", "declaredSize": 3, "confirmedOffset": 0, "expiresAt": "2000-01-01T00:00:00Z"}, "retryable": false, "nextAction": "restart"}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"uploads": items})
		})
	})
	client := newTestBrowser(t)
	bootstrapBrowser(t, client, h)
	accounts, err := h.repository.Accounts(client.ctx)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts: %v", err)
	}
	root := filepath.Join(t.TempDir(), "reconnect-folder")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	for index := 7; index < 10; index++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("source-%d.bin", index)), []byte("abc"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	seed := fmt.Sprintf(`(async () => {
		const ownerID=%q;
		const db=await new Promise((resolve,reject)=>{const r=indexedDB.open('endlessfs-transfer-ledger-v1');r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});
		const tx=db.transaction(['items','groups'],'readwrite'); const ids=[];
		for(let i=0;i<10;i++){const id='preparing-'+i;ids.push(id);tx.objectStore('items').put({key:ownerID+':'+id,ownerID,id,groupID:'preparing-group',name:'source-'+i+'.bin',relativePath:'reconnect-folder/source-'+i+'.bin',relativeDirectory:'reconnect-folder',directory:'/reconnect-folder',baseDirectory:'/',size:3,state:i<7?'complete':'needs-source',confirmed:i<7?3:0,uploadID:i<7?'':'expired-'+i,strategy:'keep-both',planPhase:'done',createdAt:1,error:i<7?'':'Reconnect the source to continue.'});}
		tx.objectStore('groups').put({key:ownerID+':preparing-group',ownerID,id:'preparing-group',name:'Reconnect folder',transferIDs:ids,directories:['reconnect-folder'],baseDirectory:'/',totalSize:30,state:'queued',discoveryDone:true,strategy:'keep-both',createdAt:1,error:'Too many attempts. Wait briefly and try again. Reference: historical-reference.'});
		await new Promise((resolve,reject)=>{tx.oncomplete=resolve;tx.onerror=()=>reject(tx.error)});db.close();return true;
	})()`, accounts[0].UserID.String())
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(seed, nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }), reloadLoopbackBrowser()); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(7)'`, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(client.ctx, chromedp.Click("#open-transfers", chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if rejectStage != 0 {
		if err := chromedp.Run(client.ctx, chromedp.Evaluate(fmt.Sprintf(`(()=>{const put=IDBObjectStore.prototype.put;IDBObjectStore.prototype.put=function(value,...args){if(this.name==='items' && value.id && !value.id.startsWith('preparing-') && (%d===1 || value.admissionBatchKey || value.admissionIntent))throw new DOMException('Synthetic quota failure','QuotaExceededError');return put.call(this,value,...args)}})()`, rejectStage), nil)); err != nil {
			t.Fatal(err)
		}
	}
	if err := waitFor(client.ctx, `document.querySelector('#transfer-check-label').textContent==='Preparing 70%' && document.querySelector('#transfer-check-progress').getAttribute('aria-label')==='Upload preparation progress'`, 5*time.Second); err != nil {
		t.Fatalf("completed preparation credit missing: %v (%s)", err, browserStatus(client.ctx))
	}
	if err := chromedp.Run(client.ctx, chromedp.Click(`button[aria-label="Reconnect upload source for Reconnect folder"]`, chromedp.ByQuery), chromedp.SetUploadFiles("#folder-input", []string{root}, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector('#transfer-list').textContent.includes('Recovering') && !document.querySelector('#transfer-list').textContent.includes('historical-reference')`, 10*time.Second); err != nil {
		t.Fatalf("reconnect lost preparation phase or retained stale error: %v (%s)", err, browserStatus(client.ctx))
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery page was not dispatched")
	}
	if got := recoveryCalls.Load(); got != 1 {
		t.Fatalf("recovery pages=%d,want1", got)
	}
	var label string
	_ = chromedp.Run(client.ctx, chromedp.Evaluate(`document.querySelector('#transfer-check-label').textContent`, &label))
	if label != "Preparing 70%" {
		t.Fatalf("pending source changed completed credit: %s", label)
	}
	release()
	if rejectStage != 0 {
		if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="failed"]').textContent==='(3)'`, 5*time.Second); err != nil {
			t.Fatalf("retry admitted without durable identity: %v %s", err, browserStatus(client.ctx))
		}
		for _, request := range client.requestSnapshot() {
			if request == "POST /api/v1/uploads" || request == "POST /api/v1/uploads/batch" || request == "POST /api/v1/uploads/batch/admit" {
				t.Fatalf("admitted after failed identity persistence: %s", request)
			}
		}
		if err := chromedp.Run(client.ctx, reloadLoopbackBrowser()); err != nil {
			t.Fatal(err)
		}
		if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="all"]').textContent==='(10)' && document.querySelector('[data-transfer-count="complete"]').textContent==='(7)'`, 5*time.Second); err != nil {
			t.Fatalf("failed identity write lost existing ledger history: %v", err)
		}
		client.assertNoExternalRequests(t, h)
		return
	}
	if err := waitFor(client.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(10)' && document.querySelector('#transfer-check-label').textContent==='Preparing 100%'`, 20*time.Second); err != nil {
		t.Fatalf("expired source recovery did not finish: %v (%s)", err, browserStatus(client.ctx))
	}
	for _, request := range client.requestSnapshot() {
		if strings.HasPrefix(request, "GET /api/v1/uploads/") || strings.HasPrefix(request, "POST /api/v1/uploads/expired-") {
			t.Fatalf("per-file recovery storm: %s", request)
		}
	}
	client.assertNoExternalRequests(t, h)
}
