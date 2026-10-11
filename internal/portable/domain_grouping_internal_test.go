package portable

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestConsistencyDomainGroupingPreservesCanonicalBoundaries(t *testing.T) {
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "grouping-evidence"}
	session := newConsistencyDomainTreeSession(nil, reference)
	for _, count := range []int{0, 1, 7, 255, 256, 257, 1024} {
		for _, width := range []int{0, 1024, 7000} {
			t.Run(fmt.Sprintf("items-%d/value-%d", count, width), func(t *testing.T) {
				entries := make([]storageformat.DomainEntry, count)
				for index := range entries {
					entries[index] = storageformat.DomainEntry{Key: fmt.Sprintf("item-%04d-\"\\\n<测试", index), Value: bytes.Repeat([]byte{0xfb}, width), LogicalVersion: "version"}
				}
				groups, err := session.leafGroups(entries)
				if err != nil {
					t.Fatal(err)
				}
				offset := 0
				for _, group := range groups {
					page := storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Entries: group}
					body, err := storageformat.EncodeCanonical(page)
					if err != nil || len(group) > domainPageMaximumItems || len(group) == 0 {
						t.Fatalf("invalid leaf boundary: %d items, %v", len(group), err)
					}
					want, err := storageformat.EncodeCanonical(storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Entries: entries[offset : offset+len(group)]})
					if err != nil || !bytes.Equal(body, want) {
						t.Fatalf("page bytes changed at offset %d: %v", offset, err)
					}
					offset += len(group)
					if offset < len(entries) && len(group) < domainPageMaximumItems {
						page.Entries = entries[offset-len(group) : offset+1]
						if _, err := storageformat.EncodeCanonical(page); err == nil {
							t.Fatal("leaf split before the exact canonical byte limit")
						}
					}
				}
				if offset != len(entries) {
					t.Fatalf("lost entries: %d/%d", offset, len(entries))
				}
			})
		}
	}
	for _, width := range []int{64, 7000, storageformat.MaxCanonicalBytes} {
		children := make([]domainPageRef, 257)
		for index := range children {
			children[index] = domainPageRef{firstKey: strings.Repeat("a", width), lastKey: "\"\\\n<测试", root: storageformat.DomainTreeRoot{Digest: strings.Repeat("d", 64), Level: 0, EntryCount: 1}}
		}
		groups, err := session.branchGroups(1, children)
		if width == storageformat.MaxCanonicalBytes {
			if err == nil {
				t.Fatal("oversized child was admitted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		offset := 0
		for _, group := range groups {
			if len(group) == 0 || len(group) > domainPageMaximumItems {
				t.Fatal("invalid branch item boundary")
			}
			page := storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Level: 1}
			for _, child := range group {
				page.Children = append(page.Children, storageformat.DomainPageChild{FirstKey: child.firstKey, LastKey: child.lastKey, Digest: child.root.Digest, Level: child.root.Level, EntryCount: child.root.EntryCount})
			}
			if _, err := storageformat.EncodeCanonical(page); err != nil {
				t.Fatal(err)
			}
			offset += len(group)
			if offset < len(children) && len(group) < domainPageMaximumItems {
				page.Children = append(page.Children, page.Children[0])
				if _, err := storageformat.EncodeCanonical(page); err == nil {
					t.Fatal("branch split before the exact canonical byte limit")
				}
			}
		}
		if offset != len(children) {
			t.Fatal("lost children")
		}
	}
}

func TestConsistencyDomainGroupingAllocationEvidence(t *testing.T) {
	session := newConsistencyDomainTreeSession(nil, consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "grouping-allocation"})
	for _, count := range []int{64, 128, 256, 1024} {
		entries := make([]storageformat.DomainEntry, count)
		for index := range entries {
			entries[index] = storageformat.DomainEntry{Key: fmt.Sprintf("item-%04d", index), Value: bytes.Repeat([]byte{0xfb}, 1024), LogicalVersion: "version"}
		}
		if _, err := session.leafGroups(entries); err != nil { // warm encoding caches
			t.Fatal(err)
		}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		groups, err := session.leafGroups(entries)
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatal(err)
		}
		allocated := after.TotalAlloc - before.TotalAlloc
		// Measured selected implementation: 110/219/438/1,754 kB for
		// 64/128/256/1,024 one-KiB values. Retain a linear ratchet with
		// encoding/cache headroom; the former repeated-prefix algorithm
		// allocated 3.2/12.3/48/194 MB for these same inputs.
		bytesPerItem := uint64(3072)
		if groupingRaceInstrumentationEnabled {
			// Go's race build randomly discards sync.Pool values. Ten
			// instrumented measurements observed 310–435/703–782/
			// 1,490–1,717/6,114–6,586 kB at the same cardinalities.
			// Keep a separate linear guard for that uncached encoding work;
			// the released-runtime guard above remains unchanged.
			bytesPerItem = 8192
		}
		if limit := uint64(count)*bytesPerItem + 16384; allocated > limit {
			t.Fatalf("grouping allocation regression: %d bytes, limit %d for %d items (race=%t)", allocated, limit, count, groupingRaceInstrumentationEnabled)
		}
		t.Logf("grouping items=%d pages=%d allocated_bytes=%d", count, len(groups), allocated)
	}
}

func TestConsistencyDomainGroupingRejectsOversizedFirstItemWithBoundedMemory(t *testing.T) {
	session := newConsistencyDomainTreeSession(nil, consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "grouping-early-denial"})
	entry := storageformat.DomainEntry{Key: "oversized", Value: make([]byte, storageformat.MaxCanonicalBytes), LogicalVersion: "version"}
	measure := func(count int) uint64 {
		t.Helper()
		entries := make([]storageformat.DomainEntry, count)
		for index := range entries {
			entries[index] = entry
		}
		// Both samples must start with an empty encoder pool. A warm sample
		// compared with a randomly uncached race sample measures pool reuse,
		// not whether the rejected suffix was serialized. Two collections
		// clear both current and victim sync.Pool caches before accounting.
		runtime.GC()
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := session.leafGroups(entries)
		runtime.ReadMemStats(&after)
		if err == nil {
			t.Fatal("oversized first item was admitted")
		}
		return after.TotalAlloc - before.TotalAlloc
	}
	single := measure(1)
	wide := measure(16)
	if wide > single*2+(1<<20) {
		t.Fatalf("early denial serialized unrelated oversized entries: single=%d wide=%d", single, wide)
	}
}
