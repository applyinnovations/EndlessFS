package portable

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestPersistentStateIndexMutationAndCursorStayBounded(t *testing.T) {
	t.Parallel()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2048, 4, 5, 6, 7, 8, 0, time.UTC))
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("state-index-entropy-0123456789", 1<<18)))
	versions := make(map[string]state.Version)
	for index := range 1024 {
		name := fmt.Sprintf("item-%04d", index)
		version, err := engine.Create(context.Background(), state.MustKey(state.NamespacePreferences, name), []byte(name))
		if err != nil {
			t.Fatal(err)
		}
		versions[name] = version
	}
	before := len(backend.Export())
	if _, err := engine.CompareAndSwap(context.Background(), state.MustKey(state.NamespacePreferences, "item-0512"), versions["item-0512"], []byte("updated")); err != nil {
		t.Fatal(err)
	}
	after := len(backend.Export())
	if added := after - before; added > 8 {
		t.Fatalf("one state update retained %d new objects; want at most 8", added)
	}
	page, err := engine.List(context.Background(), state.MustPrefix(state.NamespacePreferences), state.PageRequest{Limit: 7})
	if err != nil || len(page.Items) != 7 || page.NextCursor == "" || len(page.NextCursor) > 2048 {
		t.Fatalf("bounded state page = %d items, cursor bytes %d, %v", len(page.Items), len(page.NextCursor), err)
	}
	if _, err := engine.Create(context.Background(), state.MustKey(state.NamespacePreferences, "post-cursor"), []byte("new")); err != nil {
		t.Fatal(err)
	}
	seen := len(page.Items)
	for page.NextCursor != "" {
		page, err = engine.List(context.Background(), state.MustPrefix(state.NamespacePreferences), state.PageRequest{Limit: 7, Cursor: page.NextCursor})
		if err != nil {
			t.Fatal(err)
		}
		seen += len(page.Items)
	}
	if seen != 1024 {
		t.Fatalf("immutable state-index cursor returned %d entries; want 1024", seen)
	}
	root, err := engine.readStateIndexRoot(context.Background(), string(state.NamespacePreferences))
	if err != nil || root.root.EntryCount != 1025 || root.root.NodeID == "" || root.root.NodeDigest == "" {
		t.Fatalf("state index root = %+v, %v", root.root, err)
	}
	if body := backend.Export()[storageformat.StateIndexRootKey(string(state.NamespacePreferences)).String()]; len(body) == 0 || len(body) > 1024 {
		t.Fatalf("state index root bytes = %d; want constant-size root", len(body))
	}
	stale, err := engine.List(context.Background(), state.MustPrefix(state.NamespacePreferences), state.PageRequest{Limit: 7})
	if err != nil || stale.NextCursor == "" {
		t.Fatalf("state cursor before gate closure = %+v, %v", stale, err)
	}
	if err := engine.CloseWrites(context.Background(), "invalidate-state-cursor"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.List(context.Background(), state.MustPrefix(state.NamespacePreferences), state.PageRequest{Limit: 7, Cursor: stale.NextCursor}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("closed-gate state cursor error = %v; want invalid", err)
	}
}
