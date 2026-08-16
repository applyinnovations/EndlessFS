package state

import (
	"context"
	"testing"
)

func FuzzPaginationCursorBoundary(f *testing.F) {
	for _, seed := range []string{"", "missing", "cs0000000000000001", "../escape", "\x00"} {
		f.Add(seed, 1)
	}
	f.Fuzz(func(t *testing.T, cursor string, limit int) {
		store := NewMemoryStore()
		_, _ = store.List(context.Background(), MustPrefix(NamespaceUsers), PageRequest{Limit: limit, Cursor: cursor})
	})
}
