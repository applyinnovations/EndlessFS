package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

func TestE2ESmartMergeUploadsAndPublishesWhileAnotherFingerprintIsHeld(t *testing.T) {
	parallelE2EFixture(t)
	runHeldFingerprintPipeline(t, false)
}

func TestE2ESmartMergeCancellationReleasesHeldFingerprintWork(t *testing.T) {
	parallelE2EFixture(t)
	runHeldFingerprintPipeline(t, true)
}

func runHeldFingerprintPipeline(t *testing.T, cancel bool) {
	t.Helper()
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	h := newPortableHarnessWithControlPlaneWrapper(t, nil)
	c := newTestBrowser(t)
	bootstrapBrowser(t, c, h)
	root := t.TempDir()
	seed := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(seed, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(c.ctx, chromedp.SetUploadFiles("#upload-input", []string{seed}, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(c.ctx, "#action-dialog", conditionBound); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(c.ctx, chromedp.SetValue("#upload-strategy", "keep-both", chromedp.ByQuery), chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(c.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent === '(1)'`, conditionBound); err != nil {
		t.Fatal(err)
	}
	if err := closeTransferSheet(c.ctx); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(c.ctx, chromedp.Evaluate(`(() => {
 const send = Worker.prototype.postMessage;
 Worker.prototype.postMessage = function(message, ...args) {
   if(message.file && message.file.name === 'held.txt') { window.releaseHeldHash = () => send.call(this,message,...args); return; }
   return send.call(this,message,...args);
 };
 let offset=0;const realNow=performance.now.bind(performance);
 Object.defineProperty(performance,'now',{value:()=>realNow()+offset});window.advancePipelineClock=ms=>{offset+=ms;};
 const dt=new DataTransfer();
 for(const [name,body] of [['unique.txt','xy'],['ready.txt','abc'],['held.txt','abc']]) {
   const f=new File([body],name,{type:'text/plain'});Object.defineProperty(f,'webkitRelativePath',{value:'pipeline/'+name});dt.items.add(f);
 }
 const input=document.querySelector('#folder-input');input.files=dt.files;input.dispatchEvent(new Event('change',{bubbles:true}));
 })()`, nil)); err != nil {
		t.Fatal(err)
	}
	if err := waitVisible(c.ctx, "#action-dialog", conditionBound); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(c.ctx, chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(c.ctx, `typeof window.releaseHeldHash === 'function' && document.querySelector('#transfer-progress').value > 0`, conditionBound); err != nil {
		var evidence string
		_ = chromedp.Run(c.ctx, chromedp.Evaluate(`JSON.stringify({held:typeof window.releaseHeldHash,progress:document.querySelector('#transfer-progress').value,panel:document.querySelector('#transfer-panel').textContent})`, &evidence))
		t.Logf("pipeline evidence: %s", evidence)
		t.Fatalf("unambiguous upload waited for held fingerprint: %v (%s)", err, browserStatus(c.ctx))
	}
	// The new preparation percentage credits all prerequisites, rather than a
	// hash alone. The unique two-byte file is ready; the two three-byte files
	// still await planning/session readiness while source fingerprints overlap.
	if err := waitFor(c.ctx, `(() => {const preparation=document.querySelector('#transfer-check-progress');return preparation.max===8 && preparation.value===2 && preparation.getAttribute('aria-valuetext').includes('Reading source fingerprints') && document.querySelector('#transfer-progress').value===2})()`, conditionBound); err != nil {
		t.Fatalf("preparation lost ready-file credit or pending fingerprint feedback: %v", err)
	}
	if target := os.Getenv("ENDLESSFS_UPLOAD_PIPELINE_SCREENSHOT"); target != "" {
		var png []byte
		if !filepath.IsAbs(target) {
			t.Fatal("screenshot target must be absolute")
		}
		if err := chromedp.Run(c.ctx, chromedp.CaptureScreenshot(&png)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, png, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if cancel {
		if err := chromedp.Run(c.ctx, clickNode(`[aria-label="Cancel folder upload pipeline"]`), clickNode(`[data-tab-value="all"]`)); err != nil {
			t.Fatal(err)
		}
		if err := waitVisible(c.ctx, `[aria-label="Expand upload group pipeline"]`, conditionBound); err != nil {
			t.Fatal(err)
		}
		if err := chromedp.Run(c.ctx, clickNode(`[aria-label="Expand upload group pipeline"]`)); err != nil {
			t.Fatal(err)
		}
		if err := waitFor(c.ctx, `document.querySelectorAll('.transfer-row.cancelled').length===2&&document.querySelector('[data-transfer-count="failed"]').textContent==='(0)'`, conditionBound); err != nil {
			t.Fatal(err)
		}
		if err := closeTransferSheet(c.ctx); err != nil {
			t.Fatal(err)
		}
		fresh := filepath.Join(root, "after-cancel.txt")
		if err := os.WriteFile(fresh, []byte("fresh source after cancellation"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := chromedp.Run(c.ctx, chromedp.SetUploadFiles("#upload-input", []string{fresh}, chromedp.ByQuery)); err != nil {
			t.Fatal(err)
		}
		if err := waitVisible(c.ctx, "#action-dialog", conditionBound); err != nil {
			t.Fatal(err)
		}
		if err := chromedp.Run(c.ctx, chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter)); err != nil {
			t.Fatal(err)
		}
		if err := waitFor(c.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent==='(3)'&&document.querySelector('[data-transfer-count="failed"]').textContent==='(0)'`, conditionBound); err != nil {
			t.Fatalf("cancelled hashing held the next independent upload: %v", err)
		}
		c.assertNoExternalRequests(t, h)
		return
	}
	if err := chromedp.Run(c.ctx, chromedp.Evaluate(`window.advancePipelineClock(16000)`, nil)); err != nil {
		t.Fatal(err)
	}
	var layered bool
	if err := chromedp.Run(c.ctx, chromedp.Evaluate(`(() => { const checking=document.querySelector('#transfer-check-progress'),upload=document.querySelector('#transfer-progress'); const a=checking.getBoundingClientRect(),b=upload.getBoundingClientRect(); return checking.value >= upload.value && checking.value > 0 && a.x===b.x && a.y===b.y && a.width===b.width && a.height===b.height; })()`, &layered)); err != nil || !layered {
		t.Fatalf("checking and confirmed upload progress do not share stable geometry: %v, layered=%v", err, layered)
	}
	if err := waitFor(c.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent === '(3)'`, conditionBound); err != nil {
		t.Fatalf("verified file did not publish while hashing remained active: %v", err)
	}
	if err := chromedp.Run(c.ctx, chromedp.Evaluate(`window.releaseHeldHash()`, nil)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(c.ctx, `document.querySelector('[data-transfer-count="complete"]').textContent === '(4)'`, conditionBound); err != nil {
		t.Fatalf("remaining fingerprint did not resolve against the changed live snapshot: %v (%s)", err, browserStatus(c.ctx))
	}
	c.assertNoExternalRequests(t, h)
}
