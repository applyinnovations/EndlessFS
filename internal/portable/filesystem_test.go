package portable_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
)

func TestPortableDirectoriesAreSharedAcrossReplicasAndConditionallyPublished(t *testing.T) {
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2038, 1, 2, 3, 4, 5, 0, time.UTC))
	first := openEngine(t, backend, clock, 31, nil)
	second := openEngine(t, backend, clock, 32, nil)
	user, err := domain.ParseUserID("EREREREREREREREREREREQ")
	if err != nil {
		t.Fatal(err)
	}
	scope, _ := domain.NewScope(user, domain.AreaLive)
	path := domain.MustParseUserPath("/shared")
	var successes atomic.Int32
	var wait sync.WaitGroup
	for _, engine := range []*portable.Engine{first, second} {
		for range 12 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				if _, createErr := engine.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: path}); createErr == nil {
					successes.Add(1)
				} else if !errors.Is(createErr, domain.ErrConflict) {
					t.Errorf("CreateDirectory() error = %v", createErr)
				}
			}()
		}
	}
	wait.Wait()
	if successes.Load() != 1 {
		t.Fatalf("successful creates = %d, want 1", successes.Load())
	}
	entry, err := second.Files().Stat(context.Background(), scope, path)
	if err != nil || entry.Kind != domain.EntryDirectory || entry.Version == "" {
		t.Fatalf("Stat() = %+v, %v", entry, err)
	}
	page, err := first.Files().List(context.Background(), scope, domain.ListRequest{Directory: domain.MustParseUserPath("/"), PageSize: 1})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Path != path {
		t.Fatalf("List() = %+v, %v", page, err)
	}
}

func TestPortableDirectoryCursorAndRawCopyIgnoreNativeVersions(t *testing.T) {
	source := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2038, 2, 3, 4, 5, 6, 0, time.UTC))
	engine := openEngine(t, source, clock, 33, nil)
	user, _ := domain.ParseUserID("IiIiIiIiIiIiIiIiIiIiIg")
	scope, _ := domain.NewScope(user, domain.AreaTrash)
	for _, name := range []string{"a", "b", "c"} {
		if _, err := engine.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/" + name)}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := engine.Files().List(context.Background(), scope, domain.ListRequest{Directory: domain.MustParseUserPath("/"), PageSize: 2})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("first List() = %+v, %v", first, err)
	}
	if _, err := engine.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/later")}); err != nil {
		t.Fatal(err)
	}
	destination := objectmemory.New()
	if err := destination.Import(source.Export()); err != nil {
		t.Fatal(err)
	}
	reopened := openEngine(t, destination, clock, 34, nil)
	second, err := reopened.Files().List(context.Background(), scope, domain.ListRequest{Directory: domain.MustParseUserPath("/"), PageSize: 2, Cursor: first.NextCursor})
	if err != nil || len(second.Entries) != 1 || second.Entries[0].Name != "c" || second.NextCursor != "" {
		t.Fatalf("continued List() = %+v, %v", second, err)
	}
	for _, entry := range append(first.Entries, second.Entries...) {
		got, statErr := reopened.Files().Stat(context.Background(), scope, entry.Path)
		if statErr != nil || got.Version != entry.Version {
			t.Fatalf("portable Stat(%s) = %+v, %v", entry.Path.String(), got, statErr)
		}
	}
}
