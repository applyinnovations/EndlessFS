package portable

import (
	"bytes"
	"container/heap"
	"errors"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

func TestCheckpointReachabilityConsolidatesManyRunsAndDeduplicates(t *testing.T) {
	collector, err := newCheckpointReachabilityCollector()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = collector.Close() })
	// A one-entry buffer deterministically forces more runs than the merge
	// width without requiring a large allocation or provider interaction.
	collector.buffer = make([]string, 0, 1)
	for index := 69; index >= 0; index-- {
		key := objectstore.MustKey("endlessfs/v1/reachable/" + checkpointReachabilityChunkName(uint64(index)))
		if err := collector.Add(key); err != nil {
			t.Fatal(err)
		}
	}
	duplicate := objectstore.MustKey("endlessfs/v1/reachable/" + checkpointReachabilityChunkName(12))
	if err := collector.Add(duplicate); err != nil {
		t.Fatal(err)
	}

	stream, err := collector.Stream()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	previous := ""
	count := 0
	for {
		key, found, err := stream.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			break
		}
		if key.String() <= previous {
			t.Fatalf("reachability stream is not strictly ordered: %q after %q", key, previous)
		}
		previous = key.String()
		count++
	}
	if count != 70 {
		t.Fatalf("unique reachable key count = %d, want 70", count)
	}
	if len(collector.chunks) > checkpointReachabilityMergeWidth {
		t.Fatalf("collector retained %d runs after consolidation", len(collector.chunks))
	}
}

func TestCheckpointReachabilityMergeAndStreamDenyCorruptWorkspaces(t *testing.T) {
	collector, err := newCheckpointReachabilityCollector()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = collector.Close() })
	if err := collector.flush(); err != nil {
		t.Fatalf("empty flush: %v", err)
	}

	const valid = "valid.keys"
	var validBody bytes.Buffer
	if err := writeCheckpointReachabilityKey(&validBody, "endlessfs/v1/valid"); err != nil {
		t.Fatal(err)
	}
	if err := collector.root.WriteFile(valid, validBody.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	const output = "existing.keys"
	if err := collector.root.WriteFile(output, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := collector.merge([]string{valid}, output); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("existing merge output error = %v", err)
	}

	const corrupt = "corrupt.keys"
	if err := collector.root.WriteFile(corrupt, []byte{0, 4, 'b'}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := collector.merge([]string{valid, corrupt}, "merge-corrupt.keys"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("corrupt merge input error = %v", err)
	}

	const trailingCorruption = "trailing-corrupt.keys"
	if err := collector.root.WriteFile(trailingCorruption, append(validBody.Bytes(), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := openCheckpointReachabilityReader(collector.root, trailingCorruption)
	if err != nil {
		t.Fatal(err)
	}
	found, err := reader.advance()
	if err != nil || !found {
		t.Fatalf("first entry = %v, %v", found, err)
	}
	stream := &checkpointReachabilityStream{readers: []*checkpointReachabilityReader{reader}, queue: checkpointReachabilityHeap{reader}}
	heap.Init(&stream.queue)
	if _, _, err := stream.Next(); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("trailing corruption error = %v", err)
	}
	_ = stream.Close()

	collector.chunks = []string{corrupt}
	if _, err := collector.Stream(); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("corrupt stream chunk error = %v", err)
	}
	missingCollector, err := newCheckpointReachabilityCollector()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = missingCollector.Close() })
	missingCollector.chunks = []string{"missing.keys"}
	if _, err := missingCollector.Stream(); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("missing stream chunk error = %v", err)
	}
}
