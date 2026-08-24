package portable

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func openNamespaceTestEngine(t *testing.T, backend objectstore.Backend) *Engine {
	t.Helper()
	return openInternalTestEngine(t, backend, domain.NewFixedClock(time.Date(2046, 1, 2, 3, 4, 5, 0, time.UTC)), strings.NewReader(strings.Repeat("abcdefghijklmnopqrstuvwxyz012345", 1<<15)))
}

func namespaceTestScope(t *testing.T, area domain.Area) domain.Scope {
	t.Helper()
	owner, err := domain.ParseUserID("WVhXWVhXWVhXWVhXWVhXWQ")
	if err != nil {
		t.Fatal(err)
	}
	scope, err := domain.NewScope(owner, area)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func publishNamespaceTestFile(t *testing.T, store *namespaceStore, scope domain.Scope, path string, size int64, id string) domain.Entry {
	t.Helper()
	fingerprint := objectstore.FingerprintFor(bytes.Repeat([]byte{byte(size % 251)}, int(size)))
	entry, err := store.publishFile(context.Background(), scope, domain.MustParseUserPath(path), domain.ConflictFail, "", id, namespaceRequestFingerprint("test-file", id), storageformat.DirectoryEntry{
		Kind: domain.EntryFile, BlobID: "blob-" + id, Size: size, MediaType: "application/octet-stream", MD5: fingerprint.MD5, CRC32C: fingerprint.CRC32C,
		ModifiedAt: store.engine.clock.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("publish %s: %v", path, err)
	}
	return entry
}

func TestNamespaceStoreMoveTrashRestoreDoesNotVisitDescendantsOrFileProvider(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	base := objectmemory.New()
	backend := budgettest.Wrap(providerbudget.RoleState, base, ledger)
	engine := openNamespaceTestEngine(t, backend)
	store := newNamespaceStore(engine)
	live, trash := namespaceTestScope(t, domain.AreaLive), namespaceTestScope(t, domain.AreaTrash)
	if _, err := store.createDirectory(ctx, live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/project")}); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 256; index++ {
		publishNamespaceTestFile(t, store, live, fmt.Sprintf("/project/file-%04d", index), 1, fmt.Sprintf("file-%04d", index))
	}
	project, err := store.stat(ctx, live, domain.MustParseUserPath("/project"))
	if err != nil || project.FileCount != 256 || project.Size != 256 {
		t.Fatalf("project before trash = %+v, %v", project, err)
	}

	ledger.Reset()
	trashed, err := store.copyOrMove(ctx, true, live, trash, domain.MoveRequest{Source: domain.MustParseUserPath("/project"), Destination: domain.MustParseUserPath("/project"), ExpectedSource: project.Version, IdempotencyKey: "trash-project-0001"})
	if err != nil || trashed.State != domain.OperationSucceeded {
		t.Fatalf("trash = %+v, %v", trashed, err)
	}
	trashEvents := ledger.Events()
	t.Logf("measured 256-descendant subtree trash provider requests: %d", len(trashEvents))
	for _, event := range trashEvents {
		if event.Role != providerbudget.RoleState || event.Kind == providerbudget.RequestObjectList || event.Kind == providerbudget.RequestObjectCopy || event.Kind == providerbudget.RequestObjectDelete {
			t.Fatalf("trash performed descendant/file/provider discovery work: %+v", event)
		}
	}
	if len(trashEvents) != 6 {
		t.Fatalf("trash provider requests=%d, want measured schema-008 ratchet 6 independent of 256 descendants: %+v", len(trashEvents), trashEvents)
	}
	if _, err := store.stat(ctx, live, domain.MustParseUserPath("/project")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("live project after trash error = %v", err)
	}
	trashedProject, err := store.stat(ctx, trash, domain.MustParseUserPath("/project"))
	if err != nil || trashedProject.FileCount != 256 {
		t.Fatalf("trashed project = %+v, %v", trashedProject, err)
	}

	ledger.Reset()
	restored, err := store.copyOrMove(ctx, true, trash, live, domain.MoveRequest{Source: domain.MustParseUserPath("/project"), Destination: domain.MustParseUserPath("/project"), ExpectedSource: trashedProject.Version, IdempotencyKey: "restore-project-01"})
	if err != nil || restored.State != domain.OperationSucceeded {
		t.Fatalf("restore = %+v, %v", restored, err)
	}
	if len(ledger.Events()) != 5 {
		t.Fatalf("restore requests=%d, want measured schema-008 ratchet 5", len(ledger.Events()))
	}
	restoredProject, err := store.stat(ctx, live, domain.MustParseUserPath("/project"))
	if err != nil || restoredProject.FileCount != 256 || restoredProject.Size != 256 {
		t.Fatalf("restored project = %+v, %v", restoredProject, err)
	}
}

func TestNamespaceStoreDirectoryCopyUsesPersistentCopyOnWriteRoots(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	if _, err := store.createDirectory(ctx, live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/source")}); err != nil {
		t.Fatal(err)
	}
	publishNamespaceTestFile(t, store, live, "/source/original.bin", 3, "original")
	source, err := store.stat(ctx, live, domain.MustParseUserPath("/source"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.copyOrMove(ctx, false, live, live, domain.CopyRequest{Source: domain.MustParseUserPath("/source"), Destination: domain.MustParseUserPath("/copy"), ExpectedSource: source.Version, IdempotencyKey: "copy-directory-01"}); err != nil {
		t.Fatal(err)
	}
	publishNamespaceTestFile(t, store, live, "/source/later.bin", 5, "later")
	copyPage, err := store.list(ctx, live, domain.ListRequest{Directory: domain.MustParseUserPath("/copy"), PageSize: 10})
	if err != nil || len(copyPage.Entries) != 1 || copyPage.Entries[0].Name != "original.bin" {
		t.Fatalf("copy after source mutation = %+v, %v", copyPage, err)
	}
	sourcePage, err := store.list(ctx, live, domain.ListRequest{Directory: domain.MustParseUserPath("/source"), PageSize: 10})
	if err != nil || len(sourcePage.Entries) != 2 {
		t.Fatalf("source after copy-on-write = %+v, %v", sourcePage, err)
	}
}

func TestNamespaceStoreSortedCursorPinsImmutableOwnerSnapshot(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	for index, size := range []int64{7, 2, 9, 4} {
		publishNamespaceTestFile(t, store, live, fmt.Sprintf("/file-%d", index), size, fmt.Sprintf("sorted-%d", index))
	}
	first, err := store.list(ctx, live, domain.ListRequest{Directory: domain.MustParseUserPath("/"), PageSize: 2, Sort: domain.SortSize, Descending: true})
	if err != nil || first.NextCursor == "" || len(first.Entries) != 2 || first.Entries[0].Size != 9 || first.Entries[1].Size != 7 {
		t.Fatalf("first sorted page = %+v, %v", first, err)
	}
	publishNamespaceTestFile(t, store, live, "/later.bin", 100, "sorted-later")
	second, err := store.list(ctx, live, domain.ListRequest{Directory: domain.MustParseUserPath("/"), PageSize: 2, Sort: domain.SortSize, Descending: true, Cursor: first.NextCursor})
	if err != nil || second.NextCursor != "" || len(second.Entries) != 2 || second.Entries[0].Size != 4 || second.Entries[1].Size != 2 {
		t.Fatalf("continued immutable sorted page = %+v, %v", second, err)
	}
}

func TestNamespaceStoreIdempotencyReplaySurvivesOutcomeCompaction(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	store := newNamespaceStore(engine)
	live, trash := namespaceTestScope(t, domain.AreaLive), namespaceTestScope(t, domain.AreaTrash)
	publishNamespaceTestFile(t, store, live, "/file.bin", 1, "replay-file")
	file, err := store.stat(ctx, live, domain.MustParseUserPath("/file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	request := domain.MoveRequest{Source: domain.MustParseUserPath("/file.bin"), Destination: domain.MustParseUserPath("/file.bin"), ExpectedSource: file.Version, IdempotencyKey: "replay-trash-001"}
	first, err := store.copyOrMove(ctx, true, live, trash, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.domain.compact(ctx, namespaceReference(live.UserID())); err != nil {
		t.Fatal(err)
	}
	replay, err := store.copyOrMove(ctx, true, live, trash, request)
	if err != nil || replay != first {
		t.Fatalf("compacted replay = %+v, want %+v, err=%v", replay, first, err)
	}
}
