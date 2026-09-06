package portable

import (
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestDirectoryContentAccumulatorMatchesFullRebuildAcrossSmallChanges(t *testing.T) {
	now := time.Date(2045, 3, 4, 5, 6, 7, 0, time.UTC)
	entry := func(name string, size int64) storageformat.DirectoryEntry {
		return withCurrentTestFingerprint(storageformat.DirectoryEntry{
			Name: name, NameDigest: storageformat.NameDigest(name), Kind: domain.EntryFile,
			BlobID: "blob-" + name, Size: size, MediaType: "application/octet-stream", ModifiedAt: now,
		})
	}
	before := []storageformat.DirectoryEntry{entry("alpha", 1), entry("bravo", 2), entry("charlie", 3)}
	sort.Slice(before, func(i, j int) bool { return before[i].NameDigest < before[j].NameDigest })
	accumulator, digest, err := directoryContentIdentity(before)
	if err != nil {
		t.Fatal(err)
	}
	replacement := entry("bravo", 9)
	after := make([]storageformat.DirectoryEntry, 0, len(before))
	for _, value := range before {
		if value.Name == "alpha" {
			continue
		}
		if value.Name == "bravo" {
			value = replacement
		}
		after = append(after, value)
	}
	after = append(after, entry("delta", 4))
	sort.Slice(after, func(i, j int) bool { return after[i].NameDigest < after[j].NameDigest })
	incrementalAccumulator, incrementalDigest, err := updateDirectoryContentIdentity(accumulator, before, after)
	if err != nil {
		t.Fatal(err)
	}
	rebuiltAccumulator, rebuiltDigest, err := directoryContentIdentity(after)
	if err != nil {
		t.Fatal(err)
	}
	if incrementalAccumulator != rebuiltAccumulator || incrementalDigest != rebuiltDigest || digest == rebuiltDigest {
		t.Fatalf("incremental directory identity mismatch: incremental=(%q,%q) rebuilt=(%q,%q)", incrementalAccumulator, incrementalDigest, rebuiltAccumulator, rebuiltDigest)
	}
	restoredAccumulator, restoredDigest, err := updateDirectoryContentIdentity(incrementalAccumulator, after, before)
	if err != nil {
		t.Fatal(err)
	}
	if restoredAccumulator != accumulator || restoredDigest != digest {
		t.Fatalf("restored directory identity = (%q,%q); want (%q,%q)", restoredAccumulator, restoredDigest, accumulator, digest)
	}
}

func TestDirectoryIndexNegativeCountsFailClosedBeforeUnsignedComparison(t *testing.T) {
	tests := map[string]func() error{
		"sort roots": func() error {
			return validateDirectorySortIndexRoots(nil, -1)
		},
		"content root": func() error {
			_, err := directoryContentIndexManifestRoot(storageformat.DirectoryManifest{RecursiveFileCount: -1})
			return err
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			if err := run(); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v; want invalid", err)
			}
		})
	}
}
