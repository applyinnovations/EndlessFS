package portable

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestSchema008BatchCopyMoveAndInputDenialMatrix(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	files := engine.Files()
	live := namespaceTestScope(t, domain.AreaLive)
	seeded := seedNamespaceBatchFiles(t, newNamespaceStore(engine), live, 4)
	copies := make([]domain.CopyRequest, len(seeded))
	for index, entry := range seeded {
		copies[index] = domain.CopyRequest{Source: entry.Path, Destination: domain.MustParseUserPath("/copy-" + entry.Path.Name()), ExpectedSource: entry.Version}
	}
	copyResult, err := files.BatchCopyMove(ctx, live.UserID(), copies, false, "batch-copy-current")
	if err != nil || len(copyResult.Items) != len(copies) || copyResult.Operation.State != domain.OperationSucceeded {
		t.Fatalf("batch copy = %+v, %v", copyResult, err)
	}
	if replay, err := files.BatchCopyMove(ctx, live.UserID(), copies, false, "batch-copy-current"); err != nil || replay.Operation.ID != copyResult.Operation.ID {
		t.Fatalf("batch copy replay = %+v, %v", replay, err)
	}

	moves := make([]domain.CopyRequest, len(copies))
	for index, item := range copyResult.Items {
		copied, err := files.Stat(ctx, live, item.Destination)
		if err != nil {
			t.Fatal(err)
		}
		moves[index] = domain.CopyRequest{Source: copied.Path, Destination: domain.MustParseUserPath("/moved-" + copied.Path.Name()), ExpectedSource: copied.Version}
	}
	moveResult, err := files.BatchCopyMove(ctx, live.UserID(), moves, true, "batch-move-current")
	if err != nil || len(moveResult.Items) != len(moves) || moveResult.Operation.State != domain.OperationSucceeded {
		t.Fatalf("batch move = %+v, %v", moveResult, err)
	}
	for _, item := range moveResult.Items {
		if _, err := files.Stat(ctx, live, item.Source); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("moved source %s error = %v", item.Source.String(), err)
		}
		if _, err := files.Stat(ctx, live, item.Destination); err != nil {
			t.Fatalf("moved destination %s error = %v", item.Destination.String(), err)
		}
	}

	if _, err := files.BatchCopyMove(ctx, domain.UserID{}, copies, false, "invalid-owner"); err == nil {
		t.Fatal("batch copy accepted invalid owner")
	}
	if _, err := files.BatchMoveToTrash(ctx, domain.UserID{}, nil, "invalid-owner"); err == nil {
		t.Fatal("batch trash accepted invalid owner")
	}
	if err := validateNamespaceBatchSize(0); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty batch error = %v", err)
	}
	if err := validateNamespaceBatchSize(maximumNamespaceBatchItems + 1); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized batch error = %v", err)
	}
	if _, err := files.BatchMoveToTrash(ctx, live.UserID(), []domain.TrashRequest{{Path: seeded[0].Path, TrashID: "nested/id"}}, "invalid-trash-id"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid batch trash ID error = %v", err)
	}
	if _, err := files.BatchCopyMove(ctx, live.UserID(), []domain.CopyRequest{{Source: seeded[0].Path, Destination: seeded[0].Path}}, true, "same-path"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("same-path batch move error = %v", err)
	}
	if _, err := files.BatchCopyMove(ctx, live.UserID(), []domain.CopyRequest{{Source: domain.MustParseUserPath("/missing"), Destination: domain.MustParseUserPath("/destination")}}, true, "missing-source"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing-source batch move error = %v", err)
	}
	if _, err := files.BatchCopyMove(ctx, live.UserID(), []domain.CopyRequest{{Source: seeded[0].Path, Destination: domain.MustParseUserPath("/same")}, {Source: seeded[1].Path, Destination: domain.MustParseUserPath("/same")}}, false, "overlap"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("overlapping batch destination error = %v", err)
	}
	if _, err := files.BatchDeleteFromTrash(ctx, domain.UserID{}, []string{"trash"}, "invalid-owner"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid batch-delete owner error = %v", err)
	}
	if _, err := files.BatchDeleteFromTrash(ctx, live.UserID(), nil, "empty"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty batch-delete error = %v", err)
	}
	if _, err := files.BatchDeleteFromTrash(ctx, live.UserID(), []string{"missing"}, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing batch-delete error = %v", err)
	}
}

func TestSchema008NamespaceSortProjectionAndCursorMatrix(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	files := engine.Files()
	live := namespaceTestScope(t, domain.AreaLive)
	seedNamespaceBatchFiles(t, newNamespaceStore(engine), live, 6)
	for _, sortField := range []domain.SortField{domain.SortName, domain.SortModified, domain.SortSize, domain.SortKind} {
		for _, descending := range []bool{false, true} {
			request := domain.ListRequest{Directory: domain.MustParseUserPath("/"), PageSize: 2, Sort: sortField, Descending: descending}
			first, err := files.List(ctx, live, request)
			if err != nil || len(first.Entries) != 2 || first.NextCursor == "" {
				t.Fatalf("first %s/%v page = %+v, %v", sortField, descending, first, err)
			}
			request.Cursor = first.NextCursor
			second, err := files.List(ctx, live, request)
			if err != nil || len(second.Entries) == 0 {
				t.Fatalf("second %s/%v page = %+v, %v", sortField, descending, second, err)
			}
		}
	}
	if _, err := namespaceSortKey(domain.SortSize, storageformat.DirectoryEntry{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty sort entry error = %v", err)
	}
	if _, err := namespaceSortKey(domain.SortSize, storageformat.DirectoryEntry{Name: "negative", Size: -1}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("negative sort size error = %v", err)
	}
	entry := storageformat.DirectoryEntry{Name: "file", Kind: domain.EntryFile, ModifiedAt: time.Now().UTC()}
	if _, err := namespaceSortKey("invalid", entry); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid sort field error = %v", err)
	}
	if _, err := namespaceProjectionKind("invalid"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid projection kind error = %v", err)
	}
	if _, err := files.List(ctx, live, domain.ListRequest{Directory: domain.MustParseUserPath("/"), PageSize: 10001}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid namespace page size error = %v", err)
	}
}

func TestSchema008NamespaceInternalBindingAndAggregateDenials(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	view, err := store.loadView(ctx, live.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeNamespaceEntry([]byte("{}")); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("malformed namespace entry error = %v", err)
	}
	if _, err := encodeNamespaceEntry(storageformat.NamespaceEntry{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid namespace entry encoding error = %v", err)
	}
	if _, _, err := store.child(ctx, view, view.roots[domain.AreaLive], "missing"); err != nil {
		t.Fatalf("missing child lookup error = %v", err)
	}
	if _, err := store.resolveTrail(ctx, view, domain.AreaLive, domain.MustParseUserPath("/missing")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing trail error = %v", err)
	}
	file := publishNamespaceTestFile(t, store, live, "/component", 1, "component-file")
	view, err = store.loadView(ctx, live.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.resolveTrail(ctx, view, domain.AreaLive, file.Path); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("file-as-directory trail error = %v", err)
	}
	if _, _, err := store.prepareDestinationAtView(ctx, nil, live, domain.MustParseUserPath("/target"), domain.ConflictFail, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("misbound destination view error = %v", err)
	}
	if _, err := store.resolveEntryAtView(ctx, nil, live, domain.MustParseUserPath("/target")); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("misbound entry view error = %v", err)
	}
	if _, err := store.commit(ctx, nil, "mutation", "fingerprint", nil, storageformat.NamespaceMutationResult{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("nil namespace commit error = %v", err)
	}
	if _, err := store.commit(ctx, view, "mutation", "fingerprint", nil, storageformat.NamespaceMutationResult{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty namespace commit error = %v", err)
	}

	parent := view.roots[domain.AreaLive]
	parent.EntryCount = math.MaxUint64
	if _, err := store.applyDirectoryEdits(ctx, view, parent, nil, engine.clock.Now()); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("namespace entry-count overflow error = %v", err)
	}
	parent = view.roots[domain.AreaLive]
	parent.Entry.Size = math.MaxInt64
	child := storageformat.NamespaceEntry{SchemaVersion: 1, NodeID: "overflow", Entry: storageformat.DirectoryEntry{Name: "overflow", NameDigest: storageformat.NameDigest("overflow"), Kind: domain.EntryFile, BlobID: "overflow", Size: 1, MediaType: "application/octet-stream", MD5: testProviderMD5, CRC32C: testProviderCRC32C, ModifiedAt: engine.clock.Now().UTC()}}
	child.Entry.LogicalVersion, _ = directoryEntryVersion(child.Entry)
	misboundView := *view
	misboundView.reference.ID = "another-owner"
	if _, err := store.publishFileWithChangesAtView(ctx, &misboundView, live, domain.MustParseUserPath("/misbound"), domain.ConflictFail, "", "misbound", "fingerprint", child.Entry, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("misbound file publication view error = %v", err)
	}
	if _, err := store.applyDirectoryEdits(ctx, view, parent, []namespaceDirectoryEdit{{after: &child}}, engine.clock.Now()); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("namespace byte overflow error = %v", err)
	}

	if _, err := filesForStore(store).Delete(ctx, live, domain.DeleteRequest{Path: domain.MustParseUserPath("/")}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("root delete error = %v", err)
	}
	if _, err := filesForStore(store).MoveToTrash(ctx, live.UserID(), domain.TrashRequest{Path: domain.MustParseUserPath("/"), TrashID: "root"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("root trash error = %v", err)
	}
	if _, err := filesForStore(store).RestoreFromTrash(ctx, live.UserID(), "nested/id", domain.ConflictFail, "restore"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("nested trash restore error = %v", err)
	}
	if _, err := filesForStore(store).DeleteFromTrash(ctx, live.UserID(), "nested/id", "delete"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("nested trash delete error = %v", err)
	}
	if _, err := filesForStore(store).GetOperation(ctx, live.UserID(), "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing namespace operation error = %v", err)
	}
}

func filesForStore(store *namespaceStore) *FileStore { return store.engine.Files() }

func TestSchema008NamespaceBatchFingerprintBindsOrderAndIntent(t *testing.T) {
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	live, _ := domain.NewScope(owner, domain.AreaLive)
	specs := []namespaceBatchMoveSpec{
		{from: live, to: live, request: domain.CopyRequest{Source: domain.MustParseUserPath("/a"), Destination: domain.MustParseUserPath("/b")}},
		{from: live, to: live, request: domain.CopyRequest{Source: domain.MustParseUserPath("/c"), Destination: domain.MustParseUserPath("/d"), Conflict: domain.ConflictReplace}},
	}
	first, err := namespaceBatchFingerprint("copy", specs)
	if err != nil {
		t.Fatal(err)
	}
	reversed := append([]namespaceBatchMoveSpec(nil), specs...)
	reversed[0], reversed[1] = reversed[1], reversed[0]
	second, err := namespaceBatchFingerprint("copy", reversed)
	if err != nil || first == second {
		t.Fatalf("ordered fingerprints first=%q second=%q error=%v", first, second, err)
	}
	if err := rejectOverlappingNamespaceBatchPaths([]namespaceBatchMovePlan{{spec: specs[0], resolved: domain.MustParseUserPath("/parent")}, {spec: specs[1], resolved: domain.MustParseUserPath("/parent/child")}}, false); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("ancestor overlap error = %v", err)
	}
	if err := validatePortableIdempotencyKey(strings.Repeat("x", 129)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized idempotency key error = %v", err)
	}
}
