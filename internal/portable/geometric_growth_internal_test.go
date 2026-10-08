package portable

import (
	"context"
	"fmt"
	"runtime"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

// Fourfold inputs cross tree/page boundaries. Doubling the per-item work
// allowance accommodates one additional tree level; quadratic/exponential
// growth exceeds it. Timing is diagnostic, never a wall-clock gate.
func TestNamespaceBatchGeometricWorkGrowth(t *testing.T) {
	var priorItems int
	var priorBytes, priorAlloc uint64
	for _, count := range []int{64, 256, 1024} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			ctx := context.Background()
			ledger := providerbudget.NewLedger()
			engine := openNamespaceTestEngine(t, budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger))
			live := namespaceTestScope(t, domain.AreaLive)
			entries := seedNamespaceBatchFiles(t, newNamespaceStore(engine), live, count)
			requests := make([]domain.TrashRequest, count)
			for index, entry := range entries {
				requests[index] = domain.TrashRequest{Path: entry.Path, ExpectedVersion: entry.Version, TrashID: fmt.Sprintf("growth-%05d", index)}
			}
			ledger.Reset()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			result, err := engine.Files().BatchMoveToTrash(ctx, live.UserID(), requests, "geometric-trash")
			runtime.ReadMemStats(&after)
			if err != nil || len(result.Items) != count || result.Operation.State != domain.OperationSucceeded {
				t.Fatalf("geometric batch result: %d, %v", len(result.Items), err)
			}
			var transferred uint64
			for _, event := range ledger.Events() {
				if event.Role != providerbudget.RoleState {
					t.Fatal("namespace mutation contacted file storage")
				}
				transferred += uint64(event.RequestBytes + event.ResponseBytes)
			}
			if len(ledger.Events()) != 4 {
				t.Fatalf("provider requests grew with cardinality: %d", len(ledger.Events()))
			}
			allocated := after.TotalAlloc - before.TotalAlloc
			if priorItems != 0 {
				ratio := uint64(count / priorItems)
				if transferred > priorBytes*ratio*2 {
					t.Fatalf("metadata transfer grew superlinearly: %d/%d bytes for %d/%d items", transferred, priorBytes, count, priorItems)
				}
				// The 64-item sample is the compact/single-page regime. Compare
				// foreground allocation slope only after crossing into the
				// multi-page regime; transfer/call shape is checked throughout.
				if priorItems >= 256 && allocated > priorAlloc*ratio*2 {
					t.Fatalf("foreground allocation grew superlinearly: %d/%d bytes for %d/%d items", allocated, priorAlloc, count, priorItems)
				}
			}
			t.Logf("growth items=%d provider_calls=%d metadata_bytes=%d allocated_bytes=%d", count, len(ledger.Events()), transferred, allocated)
			priorItems, priorBytes, priorAlloc = count, transferred, allocated
		})
	}
}

func TestUploadAdmissionWorkGrowthAcrossProgressBoundaries(t *testing.T) {
	var priorItems int
	var priorBytes uint64
	for _, count := range []int{100, 1001, 2001} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			fixture := newUploadBatchScaleFixture(t, uint64(0x912331+count))
			capabilities, err := fixture.engine.Files().CreateUploadBatch(context.Background(), fixture.scope, scaleUploadRequestsCount("growth", count))
			if err != nil || len(capabilities) != count {
				t.Fatalf("admission at %d items: %d, %v", count, len(capabilities), err)
			}
			fileEvents, stateEvents := fixture.fileLedger.Events(), fixture.stateLedger.Events()
			if len(fileEvents) != count {
				t.Fatalf("provider work is not one session per item: %d/%d", len(fileEvents), count)
			}
			for _, event := range fileEvents {
				if event.Kind != providerbudget.RequestUploadBegin {
					t.Fatalf("unexpected file provider work: %+v", event)
				}
			}
			segments := (count + 999) / 1000
			if len(stateEvents) != 3+segments {
				t.Fatalf("state work must grow by progress segment: %d, want %d", len(stateEvents), 3+segments)
			}
			var transferred uint64
			for _, event := range stateEvents {
				transferred += uint64(event.RequestBytes + event.ResponseBytes)
			}
			if priorItems != 0 && transferred*uint64(priorItems) > priorBytes*uint64(count)*2 {
				t.Fatalf("upload metadata work grew superlinearly: %d/%d bytes for %d/%d items", transferred, priorBytes, count, priorItems)
			}
			t.Logf("upload growth items=%d file_calls=%d state_calls=%d metadata_bytes=%d", count, len(fileEvents), len(stateEvents), transferred)
			priorItems, priorBytes = count, transferred
		})
	}
}
