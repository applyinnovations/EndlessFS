package portable

import (
	"errors"
	"math"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestDuplicateDirectoryComparisonRejectsByteOverflow(t *testing.T) {
	var files, bytes int64
	err := addDuplicateTotals(&files, &bytes, 2, math.MaxInt64)
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("addDuplicateTotals() error = %v; want invalid overflow", err)
	}
}

func TestDuplicateDirectoryComparisonRejectsZeroByteIdentitySizeMismatch(t *testing.T) {
	store := &FileStore{}
	iterator := &directoryContentIndexIterator{exhausted: true, values: []storageformat.DirectoryContentIndexEntry{
		{GroupID: "group", Size: 0}, {GroupID: "group", Size: 1},
	}}
	_, _, _, _, err := store.nextDirectoryContentGroup(iterator)
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("nextDirectoryContentGroup() error = %v; want invalid size mismatch", err)
	}
}
