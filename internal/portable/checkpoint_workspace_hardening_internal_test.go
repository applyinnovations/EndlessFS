package portable

import (
	"bytes"
	"container/heap"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

type checkpointFailWriter struct{}

func (checkpointFailWriter) Write([]byte) (int, error) { return 0, errors.New("write denied") }

type checkpointBodyFailWriter struct{ wroteLength bool }

func (writer *checkpointBodyFailWriter) Write(value []byte) (int, error) {
	if !writer.wroteLength {
		writer.wroteLength = true
		return len(value), nil
	}
	return 0, errors.New("body write denied")
}

func TestCheckpointVisitSetExactRunsAndCorruptionDenials(t *testing.T) {
	set, err := newCheckpointVisitSet()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close() })
	set.limit = 2
	for _, value := range []string{"delta", "alpha", "charlie", "bravo", "echo"} {
		seen, err := set.Seen(value)
		if err != nil || seen {
			t.Fatalf("first Seen(%q) = %v, %v", value, seen, err)
		}
	}
	for _, value := range []string{"alpha", "bravo", "charlie", "delta", "echo"} {
		seen, err := set.Seen(value)
		if err != nil || !seen {
			t.Fatalf("replayed Seen(%q) = %v, %v", value, seen, err)
		}
	}
	if err := set.flush(); err != nil {
		t.Fatal(err)
	}
	if len(set.levels) < 2 {
		t.Fatalf("binary run merge levels = %v", set.levels)
	}

	const missing = "missing.visited"
	if _, err := checkpointVisitRunContains(set.root, missing, sha256.Sum256([]byte("missing"))); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("missing run error = %v", err)
	}
	const corrupt = "corrupt.visited"
	if err := set.root.WriteFile(corrupt, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpointVisitRunContains(set.root, corrupt, sha256.Sum256([]byte("corrupt"))); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt run error = %v", err)
	}
	if _, err := openCheckpointVisitRun(set.root, corrupt); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("truncated merge input error = %v", err)
	}
	if err := mergeCheckpointVisitRuns(set.root, missing, corrupt, "unused"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("missing merge input error = %v", err)
	}

	if err := set.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := set.Seen("closed"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("closed set error = %v", err)
	}
	if err := (*checkpointVisitSet)(nil).Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointReachabilityWorkspaceLifecycleAndCorruptionDenials(t *testing.T) {
	collector, err := newCheckpointReachabilityCollector()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = collector.Close() })

	if err := writeCheckpointReachabilityKey(checkpointFailWriter{}, "endlessfs/v1/key"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("key length write failure = %v", err)
	}
	if err := writeCheckpointReachabilityKey(&checkpointBodyFailWriter{}, "endlessfs/v1/key"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("key body write failure = %v", err)
	}
	if err := writeCheckpointReachabilityKey(&bytes.Buffer{}, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty key error = %v", err)
	}
	var lengthOnly bytes.Buffer
	var size [2]byte
	binary.BigEndian.PutUint16(size[:], uint16(len("endlessfs/v1/key")))
	lengthOnly.Write(size[:])
	if err := collector.root.WriteFile("truncated.keys", lengthOnly.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := openCheckpointReachabilityReader(collector.root, "truncated.keys")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.advance(); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("truncated key error = %v", err)
	}
	_ = reader.close()

	const zero = "zero.keys"
	if err := collector.root.WriteFile(zero, []byte{0, 0}, 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err = openCheckpointReachabilityReader(collector.root, zero)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.advance(); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("zero key error = %v", err)
	}
	_ = reader.close()

	const invalid = "invalid.keys"
	var invalidBody bytes.Buffer
	if err := writeCheckpointReachabilityKey(&invalidBody, "/not-canonical"); err != nil {
		t.Fatal(err)
	}
	if err := collector.root.WriteFile(invalid, invalidBody.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err = openCheckpointReachabilityReader(collector.root, invalid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.advance(); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid stored key error = %v", err)
	}
	_ = reader.close()

	if _, err := openCheckpointReachabilityReader(collector.root, "missing.keys"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("missing chunk error = %v", err)
	}
	if err := collector.merge([]string{"missing.keys"}, "merged.keys"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("missing merge input error = %v", err)
	}
	if err := writeCheckpointReachabilityKey(&bytes.Buffer{}, strings.Repeat("x", objectstore.MaxKeyBytes+1)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized key error = %v", err)
	}

	if err := collector.Close(); err != nil {
		t.Fatal(err)
	}
	if err := collector.Add(objectstore.MustKey("endlessfs/v1/closed")); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("closed collector accepted a key: %v", err)
	}
}

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

func TestCheckpointWorkspaceNestedFilesystemFailureMatrix(t *testing.T) {
	t.Run("reachability-flush-create-and-record", func(t *testing.T) {
		collector, err := newCheckpointReachabilityCollector()
		if err != nil {
			t.Fatal(err)
		}
		name := checkpointReachabilityChunkName(0)
		if err := collector.root.WriteFile(name, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = collector.Close() })
		collector.buffer = []string{"endlessfs/v1/key"}
		if err := collector.flush(); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("chunk creation error = %v", err)
		}

		invalid, err := newCheckpointReachabilityCollector()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = invalid.Close() })
		invalid.buffer = []string{strings.Repeat("x", objectstore.MaxKeyBytes+1)}
		if err := invalid.flush(); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid buffered key error = %v", err)
		}
	})

	t.Run("reachability-merge-late-corruption", func(t *testing.T) {
		collector, err := newCheckpointReachabilityCollector()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = collector.Close() })
		const input = "late-corrupt.keys"
		var body bytes.Buffer
		if err := writeCheckpointReachabilityKey(&body, "endlessfs/v1/first"); err != nil {
			t.Fatal(err)
		}
		body.Write([]byte{0, 8, 's'})
		if err := collector.root.WriteFile(input, body.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := collector.merge([]string{input}, "late-output.keys"); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("late merge corruption error = %v", err)
		}
	})

	t.Run("reachability-consolidation-boundaries", func(t *testing.T) {
		missing, err := newCheckpointReachabilityCollector()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = missing.Close() })
		for range checkpointReachabilityMergeWidth + 1 {
			missing.chunks = append(missing.chunks, "missing.keys")
		}
		if err := missing.consolidate(); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("missing merge run error = %v", err)
		}

		closed, err := newCheckpointReachabilityCollector()
		if err != nil {
			t.Fatal(err)
		}
		name := checkpointReachabilityChunkName(0)
		if err := closed.root.WriteFile(name, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		closed.buffer = []string{"endlessfs/v1/key"}
		if err := closed.consolidate(); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("consolidation flush error = %v", err)
		}
		closed.sequence = 0
		if _, err := closed.Stream(); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("stream consolidation error = %v", err)
		}
	})

	t.Run("reachability-removal-failure", func(t *testing.T) {
		collector, err := newCheckpointReachabilityCollector()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = collector.Close() })
		const name = "duplicate.keys"
		var body bytes.Buffer
		if err := writeCheckpointReachabilityKey(&body, "endlessfs/v1/duplicate"); err != nil {
			t.Fatal(err)
		}
		if err := collector.root.WriteFile(name, body.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		for range checkpointReachabilityMergeWidth + 1 {
			collector.chunks = append(collector.chunks, name)
		}
		if err := collector.consolidate(); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("duplicate run removal error = %v", err)
		}
	})

	t.Run("visited-run-insertion", func(t *testing.T) {
		set, err := newCheckpointVisitSet()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = set.Close() })
		set.limit = 1
		name := checkpointReachabilityChunkName(0) + ".visited"
		if err := set.root.WriteFile(name, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := set.Seen("flush-create-failure"); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("visited flush creation error = %v", err)
		}

		mergeSet, err := newCheckpointVisitSet()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = mergeSet.Close() })
		const valid = "valid.visited"
		if err := mergeSet.root.WriteFile(valid, make([]byte, sha256.Size), 0o600); err != nil {
			t.Fatal(err)
		}
		mergeSet.levels = []string{"missing.visited"}
		if err := mergeSet.insertRun(0, valid); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("visited missing merge input error = %v", err)
		}

		duplicateSet, err := newCheckpointVisitSet()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = duplicateSet.Close() })
		const duplicate = "duplicate.visited"
		if err := duplicateSet.root.WriteFile(duplicate, make([]byte, sha256.Size), 0o600); err != nil {
			t.Fatal(err)
		}
		duplicateSet.levels = []string{duplicate}
		if err := duplicateSet.insertRun(0, duplicate); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("visited duplicate input removal error = %v", err)
		}

		if err := mergeCheckpointVisitRuns(mergeSet.root, valid, "missing-right.visited", "unused.visited"); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("visited missing right input error = %v", err)
		}
	})
}

func TestCheckpointVisitRunMergeBoundaries(t *testing.T) {
	set, err := newCheckpointVisitSet()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close() })
	if err := set.flush(); err != nil {
		t.Fatalf("empty flush: %v", err)
	}

	const missing = "missing.visited"
	const valid = "valid.visited"
	digest := sha256.Sum256([]byte("valid"))
	if err := set.root.WriteFile(valid, digest[:], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mergeCheckpointVisitRuns(set.root, valid, missing, "unused.visited"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("missing right input error = %v", err)
	}
	const existing = "existing.visited"
	if err := set.root.WriteFile(existing, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mergeCheckpointVisitRuns(set.root, valid, valid, existing); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("existing output error = %v", err)
	}

	set.levels = []string{missing}
	if err := set.insertRun(0, valid); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("missing resident run error = %v", err)
	}
}

func TestCheckpointVisitRunReaderAcceptsExactEOF(t *testing.T) {
	directory := t.TempDir()
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile("empty.visited", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := openCheckpointVisitRun(root, "empty.visited")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.file.Close()
	if reader.found {
		t.Fatal("empty visited run reported an entry")
	}
	if err := reader.advance(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("empty run advance error = %v", err)
	}
}

func TestCheckpointWorkspacesDenyPathEscape(t *testing.T) {
	if _, err := openCheckpointReachabilityReader(nil, "closed.keys"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("closed reachability reader error = %v", err)
	}
	if _, err := checkpointVisitRunContains(nil, "closed.visited", sha256.Sum256([]byte("closed"))); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("closed visited lookup error = %v", err)
	}
	if _, err := openCheckpointVisitRun(nil, "closed.visited"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("closed visited reader error = %v", err)
	}

	collector, err := newCheckpointReachabilityCollector()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = collector.Close() })
	if err := collector.root.WriteFile("../escape.keys", []byte("denied"), 0o600); err == nil {
		t.Fatal("reachability workspace permitted parent traversal")
	}
	if _, err := openCheckpointReachabilityReader(collector.root, "../escape.keys"); err == nil {
		t.Fatal("reachability reader permitted parent traversal")
	}

	set, err := newCheckpointVisitSet()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close() })
	if err := set.root.WriteFile("../escape.visited", []byte("denied"), 0o600); err == nil {
		t.Fatal("visited workspace permitted parent traversal")
	}
	if _, err := openCheckpointVisitRun(set.root, "../escape.visited"); err == nil {
		t.Fatal("visited reader permitted parent traversal")
	}
}
