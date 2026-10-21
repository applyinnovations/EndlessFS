package portable

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"testing"

	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestConsistencyDomainStreamAllocationRemainsLinear(t *testing.T) {
	for _, count := range []int{64, 128, 256, 1024} {
		entries := make([]storageformat.DomainEntry, count)
		for index := range entries {
			entries[index] = storageformat.DomainEntry{Key: fmt.Sprintf("item-%06d", index), Value: bytes.Repeat([]byte{byte(index)}, 1024), LogicalVersion: "version"}
		}
		ctx := context.Background()
		session := newConsistencyDomainTreeSession(newConsistencyDomainStore(objectmemory.New(), nil), consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "stream-allocation"})
		builder := newConsistencyDomainTreeBuilder(ctx, session)
		// Cold encoder pools exercise real allocation work, including final
		// canonical validation, immutable provider writes and owned values.
		runtime.GC()
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for _, entry := range entries {
			if err := builder.Add(entry); err != nil {
				t.Fatal(err)
			}
		}
		root, err := builder.Finish()
		runtime.ReadMemStats(&after)
		if err != nil || root.EntryCount != uint64(count) || root.ByteCount != uint64(count*1024) {
			t.Fatalf("streamed authority counts=%d/%d root=%+v err=%v", count, count*1024, root, err)
		}
		allocated := after.TotalAlloc - before.TotalAlloc
		// The selected cold implementation measured 0.69/1.35/2.65/8.21MB;
		// race measured 1.35/2.73/5.29/24.74MB. Round the per-item envelope
		// to allocator-page units, with 64KiB for fixed tree/write work.
		// The old repeated-prefix builder measured 3.89/14.53/55.44/211.41MB
		// ordinary and 13.02/46.26/171.96/684.25MB with race instrumentation.
		bytesPerItem := uint64(12 << 10)
		if groupingRaceInstrumentationEnabled {
			bytesPerItem = 28 << 10
		}
		limit := uint64(count)*bytesPerItem + (64 << 10)
		if allocated > limit {
			t.Fatalf("streaming allocation regression: items=%d allocated=%d limit=%d race=%t", count, allocated, limit, groupingRaceInstrumentationEnabled)
		}
		t.Logf("streaming items=%d allocated_bytes=%d limit=%d race=%t", count, allocated, limit, groupingRaceInstrumentationEnabled)
	}
}
