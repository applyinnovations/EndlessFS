package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// This uses Chromium's actual drag data store. The older synthetic drop test
// leaves its JavaScript object readable forever and skips production options.
func TestE2EProductionFolderDropRetainsSourcesAcrossUploadOptions(t *testing.T) {
	if os.Getenv("ENDLESSFS_RUN_E2E") != "1" {
		t.Skip("the Nix test-e2e task enables Chromium qualification")
	}
	harness := newPortableHarnessWithControlPlaneWrapper(t, nil)
	client := newTestBrowser(t)
	bootstrapBrowser(t, client, harness)
	folder := filepath.Join(t.TempDir(), "Dropped")
	if err := os.MkdirAll(filepath.Join(folder, "Nested"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"first.txt", "Nested/second.txt"} {
		if err := os.WriteFile(filepath.Join(folder, relative), []byte(relative), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var point struct{ X, Y float64 }
	if err := chromedp.Run(client.ctx, chromedp.Evaluate(`(() => {
		const bounds = document.querySelector("#drop-target").getBoundingClientRect();
		return {X: bounds.x + bounds.width / 2, Y: bounds.y + Math.min(bounds.height / 2, 40)};
	})()`, &point)); err != nil {
		t.Fatal(err)
	}
	data := &input.DragData{Items: []*input.DragDataItem{}, Files: []string{folder}, DragOperationsMask: 1}
	if err := chromedp.Run(client.ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		for _, event := range []input.DispatchDragEventType{input.DragEnter, input.DragOver, input.Drop} {
			if err := input.DispatchDragEvent(event, point.X, point.Y, data).Do(ctx); err != nil {
				return err
			}
		}
		return nil
	})); err != nil {
		t.Fatalf("trusted folder drop: %v", err)
	}
	if err := waitVisible(client.ctx, "#action-dialog", 5*time.Second); err != nil {
		t.Fatalf("production upload options: %v (%s)", err, browserStatus(client.ctx))
	}
	if err := chromedp.Run(client.ctx,
		chromedp.SetValue("#upload-strategy", "keep-both", chromedp.ByQuery),
		chromedp.Focus("#dialog-confirm", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter),
	); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector("[data-transfer-count='complete']")?.textContent === "(2)"`, 15*time.Second); err != nil {
		t.Fatalf("folder drop lost its sources after options: %v (%s) requests=%v", err, browserStatus(client.ctx), client.requestSnapshot())
	}
	if err := chromedp.Run(client.ctx, chromedp.Click("[data-tab-value='all']", chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(client.ctx, `document.querySelector(".transfer-group-row.complete")?.textContent.includes("2 of 2 files")`, 5*time.Second); err != nil {
		t.Fatalf("completed folder did not preserve group structure: %v", err)
	}
	client.assertNoExternalRequests(t, harness)
}
