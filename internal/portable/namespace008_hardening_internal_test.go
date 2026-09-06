package portable

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
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

func TestSchema011NamespaceBatchReplacementValidationAndTrashDenialMatrix(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	store := newNamespaceStore(engine)
	files := engine.Files()
	live := namespaceTestScope(t, domain.AreaLive)
	seeded := seedNamespaceBatchFilesWithMutation(t, store, live, 2, "schema011-batch-boundary-seed")

	replaced, err := files.BatchCopyMove(ctx, live.UserID(), []domain.CopyRequest{{
		Source: seeded[0].Path, Destination: seeded[1].Path, Conflict: domain.ConflictReplace,
		ExpectedSource: seeded[0].Version, ExpectedTarget: seeded[1].Version,
	}}, false, "schema011-batch-replace")
	if err != nil || len(replaced.Items) != 1 || replaced.Items[0].Destination != seeded[1].Path {
		t.Fatalf("replacement batch = %+v, %v", replaced, err)
	}

	failure := domain.NewError(domain.ErrorUnavailable, "selection changed")
	spec := namespaceBatchMoveSpec{from: live, to: live, request: domain.CopyRequest{
		Source: seeded[0].Path, Destination: domain.MustParseUserPath("/validated-copy.bin"), ExpectedSource: seeded[0].Version,
	}}
	if _, err := store.batchCopyOrMoveValidated(ctx, live.UserID(), []namespaceBatchMoveSpec{spec}, false, "validated-copy", "schema011-validated-copy", "bound-intent", func(context.Context, *namespaceView) error {
		return failure
	}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("view validation failure = %v", err)
	}

	view, err := store.loadView(ctx, live.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	for name, candidate := range map[string]*namespaceView{
		"nil":        nil,
		"no-session": {reference: view.reference},
	} {
		t.Run("bind-"+name, func(t *testing.T) {
			if err := candidate.bindMutation("mutation", "fingerprint"); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if err := view.bindMutation("", "fingerprint"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty mutation binding = %v", err)
	}
	if err := view.bindMutation("mutation", ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty fingerprint binding = %v", err)
	}

	for name, run := range map[string]func() error{
		"restore-owner": func() error {
			_, err := files.BatchRestoreFromTrash(ctx, domain.UserID{}, []string{"trash"}, domain.ConflictFail, "schema011-restore-owner")
			return err
		},
		"restore-conflict": func() error {
			_, err := files.BatchRestoreFromTrash(ctx, live.UserID(), []string{"trash"}, "invalid", "schema011-restore-conflict")
			return err
		},
		"restore-duplicate": func() error {
			_, err := files.BatchRestoreFromTrash(ctx, live.UserID(), []string{"trash", "trash"}, domain.ConflictFail, "schema011-restore-duplicate")
			return err
		},
		"restore-nested": func() error {
			_, err := files.BatchRestoreFromTrash(ctx, live.UserID(), []string{"nested/id"}, domain.ConflictFail, "schema011-restore-nested")
			return err
		},
		"restore-missing": func() error {
			_, err := files.BatchRestoreFromTrash(ctx, live.UserID(), []string{"missing"}, domain.ConflictFail, "schema011-restore-missing")
			return err
		},
		"delete-duplicate": func() error {
			_, err := files.BatchDeleteFromTrash(ctx, live.UserID(), []string{"trash", "trash"}, "schema011-delete-duplicate")
			return err
		},
		"delete-nested": func() error {
			_, err := files.BatchDeleteFromTrash(ctx, live.UserID(), []string{"nested/id"}, "schema011-delete-nested")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(); err == nil {
				t.Fatal("invalid batch was accepted")
			}
		})
	}

	parent := view.roots[domain.AreaLive]
	if _, _, err := store.resolveDestination(ctx, view, parent, seeded[0].Path, "invalid", ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid destination conflict = %v", err)
	}
}

func TestSchema011NamespaceBatchAndProjectionRecoveryFailureMatrix(t *testing.T) {
	ctx := context.Background()
	base := objectmemory.New()
	engine := openNamespaceTestEngine(t, base)
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	trash := namespaceTestScope(t, domain.AreaTrash)
	seeded := seedNamespaceBatchFilesWithMutation(t, store, live, 2, "schema011-recovery-seed")

	spec := namespaceBatchMoveSpec{from: live, to: live, request: domain.CopyRequest{
		Source: seeded[0].Path, Destination: domain.MustParseUserPath("/validator-bound.bin"), ExpectedSource: seeded[0].Version,
	}}
	if _, err := store.batchCopyOrMoveValidated(ctx, live.UserID(), []namespaceBatchMoveSpec{spec}, false, "copy", "validator-rebind", "", func(_ context.Context, view *namespaceView) error {
		return view.bindMutation("another-mutation", "another-fingerprint")
	}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("validator mutation rebind error = %v", err)
	}

	trashEntry := publishNamespaceTestFile(t, store, trash, "/missing-trash-metadata", 1, "missing-trash-metadata")
	if _, err := store.batchCopyOrMove(ctx, live.UserID(), []namespaceBatchMoveSpec{{
		from: trash, to: live, restoreTrash: true,
		request: domain.CopyRequest{Source: trashEntry.Path, Conflict: domain.ConflictFail},
	}}, true, "restore", "missing-trash-metadata"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("restore without trash metadata error = %v", err)
	}

	if _, err := store.createDirectory(ctx, trash, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/nested")}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.batchCopyOrMove(ctx, live.UserID(), []namespaceBatchMoveSpec{{
		from: live, to: trash, attachTrash: true, trashID: "nested-target",
		request: domain.CopyRequest{Source: seeded[1].Path, Destination: domain.MustParseUserPath("/nested/target"), Conflict: domain.ConflictFail},
	}}, true, "trash", "nested-trash-placement"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("nested trash placement error = %v", err)
	}

	largeTrashIDs := make([]string, maximumNamespaceBatchItems)
	for index := range largeTrashIDs {
		largeTrashIDs[index] = strings.Repeat("x", 128) + fmt.Sprintf("-%05d", index)
	}
	if _, err := engine.Files().BatchDeleteFromTrash(ctx, live.UserID(), largeTrashIDs, "oversized-trash-intent"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized trash intent error = %v", err)
	}

	view, err := store.loadView(ctx, live.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	sourceEntry, found, err := store.child(ctx, view, view.roots[domain.AreaLive], seeded[0].Path.Name())
	if err != nil || !found {
		t.Fatalf("projection source entry found=%v error=%v", found, err)
	}
	sourceBody, err := encodeNamespaceEntry(sourceEntry)
	if err != nil {
		t.Fatal(err)
	}
	reference := consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "projection-failure"}
	leaf := storageformat.DomainPage{
		SchemaVersion: 1, DomainID: view.reference.ID, Kind: view.reference.Kind, Level: 0,
		Entries: []storageformat.DomainEntry{{Key: "first", Value: sourceBody, LogicalVersion: sourceEntry.Entry.LogicalVersion}},
	}
	leafBody, err := storageformat.EncodeCanonical(leaf)
	if err != nil {
		t.Fatal(err)
	}
	leafDigest := storageformat.Digest(leafBody)
	leafDescriptor, err := consistencyDomainPageDescriptor(leaf, leafDigest)
	if err != nil {
		t.Fatal(err)
	}
	missingDigest := storageformat.Digest([]byte("missing-projection-sibling"))
	branch := storageformat.DomainPage{
		SchemaVersion: 1, DomainID: view.reference.ID, Kind: view.reference.Kind, Level: 1,
		Children: []storageformat.DomainPageChild{
			{
				FirstKey: leafDescriptor.firstKey, LastKey: leafDescriptor.lastKey, Digest: leafDescriptor.root.Digest,
				PackID: leafDescriptor.root.PackID, LeafKeyFilter: leafDescriptor.leafKeyFilter, Level: leafDescriptor.root.Level,
				EntryCount: leafDescriptor.root.EntryCount, ByteCount: leafDescriptor.root.ByteCount,
			},
			{FirstKey: "second", LastKey: "second", Digest: missingDigest, Level: 0, EntryCount: 1},
		},
	}
	branchBody, err := storageformat.EncodeCanonical(branch)
	if err != nil {
		t.Fatal(err)
	}
	branchDigest := storageformat.Digest(branchBody)
	branchDescriptor, err := consistencyDomainPageDescriptor(branch, branchDigest)
	if err != nil {
		t.Fatal(err)
	}
	view.session.pages[leafDigest] = leaf
	view.session.pages[branchDigest] = branch
	projection := newConsistencyDomainTreeSession(newConsistencyDomainStore(objectmemory.New(), nil), reference)
	if _, err := store.buildNamespaceSortProjection(ctx, view, projection, branchDescriptor.root, domain.SortSize); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("projection sibling read error = %v (leaf=%+v)", err, leafDescriptor.root)
	}

	view, err = store.loadView(ctx, live.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	failure := domain.NewError(domain.ErrorUnavailable, "projection publication unavailable")
	hooks := &hookedBackend{Backend: base, put: func(callCtx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
		if strings.Contains(key.String(), "/projections/") {
			return "", failure
		}
		return base.Put(callCtx, key, body, condition)
	}}
	store.engine.backend = hooks
	store.domain.backend = hooks
	if _, err := store.namespaceSortProjection(ctx, view, domain.AreaLive, view.roots[domain.AreaLive], domain.SortSize); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("projection publication error = %v", err)
	}

	childFrame := namespaceFrame{key: "live:/child", parentKey: "live:/", depth: 1, entry: storageformat.NamespaceEntry{
		SchemaVersion: 1, NodeID: "child", Entry: storageformat.DirectoryEntry{Kind: domain.EntryDirectory, DirectoryID: "child", Size: 1, FileCount: 1},
	}}
	parentFrame := namespaceFrame{key: "live:/", entry: view.roots[domain.AreaLive]}
	parentFrame.entry.Entry.Size = math.MaxInt64
	changedChild := childFrame.entry
	changedChild.Entry.Size = 2
	if err := store.propagate(ctx, view, map[string]namespaceFrame{childFrame.key: childFrame, parentFrame.key: parentFrame}, map[string]storageformat.NamespaceEntry{childFrame.key: changedChild}, engine.clock.Now()); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("propagated aggregate overflow error = %v", err)
	}
}

func TestSchema011NamespaceAuthenticatedReadsAndProjectionMergeFailClosed(t *testing.T) {
	ctx := context.Background()
	live := namespaceTestScope(t, domain.AreaLive)
	failure := domain.NewError(domain.ErrorUnavailable, "namespace metadata unavailable")

	t.Run("materialized-root-page-read", func(t *testing.T) {
		base := objectmemory.New()
		engine := openNamespaceTestEngine(t, base)
		store := newNamespaceStore(engine)
		publishNamespaceTestFile(t, store, live, "/page-read.bin", 1, "page-read")
		reference := namespaceReference(live.UserID())
		if err := engine.stateDomainStore().compact(ctx, reference); err != nil {
			t.Fatal(err)
		}
		headKey := storageformat.DomainHeadKey(reference.Kind, reference.ID)
		engine.backend = &hookedBackend{Backend: base, get: func(callCtx context.Context, key objectstore.Key) (objectstore.Object, error) {
			if key == headKey {
				return base.Get(callCtx, key)
			}
			return objectstore.Object{}, failure
		}}
		if _, err := newNamespaceStore(engine).loadView(ctx, live.UserID(), ""); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("namespace page read error = %v", err)
		}
	})

	t.Run("corrupt-materialized-root", func(t *testing.T) {
		base := objectmemory.New()
		engine := openNamespaceTestEngine(t, base)
		reference := namespaceReference(live.UserID())
		if _, err := engine.stateDomainStore().mutate(ctx, reference, consistencyDomainMutation{
			ID: "corrupt-live-root", Changes: []consistencyDomainChange{{Key: namespaceRootKey(domain.AreaLive), Require: domainValueAbsent, Value: []byte("invalid")}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := engine.stateDomainStore().compact(ctx, reference); err != nil {
			t.Fatal(err)
		}
		if _, err := newNamespaceStore(engine).loadView(ctx, live.UserID(), ""); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("corrupt namespace root error = %v", err)
		}
	})

	t.Run("projection-source-page-read", func(t *testing.T) {
		base := objectmemory.New()
		reference := consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "projection-source-read"}
		session := newConsistencyDomainTreeSession(newConsistencyDomainStore(base, nil), reference)
		missing := storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("missing-projection-run")), Level: 0, EntryCount: 1}
		if _, err := mergeNamespaceProjectionRuns(ctx, session, []storageformat.DomainTreeRoot{missing}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("projection source page error = %v", err)
		}
	})

	t.Run("projection-output-publication", func(t *testing.T) {
		base := objectmemory.New()
		reference := consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "projection-output-write"}
		sourceSession := newConsistencyDomainTreeSession(newConsistencyDomainStore(base, nil), reference)
		entries := make([]storageformat.DomainEntry, domainPageMaximumItems+1)
		for index := range entries {
			entries[index] = storageformat.DomainEntry{Key: fmt.Sprintf("%08d", index), Value: []byte("value"), LogicalVersion: fmt.Sprintf("version-%08d", index)}
		}
		root, err := sourceSession.buildTree(ctx, entries)
		if err != nil {
			t.Fatal(err)
		}
		hooks := &hookedBackend{Backend: base, put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return "", failure
		}}
		destination := newConsistencyDomainTreeSession(newConsistencyDomainStore(hooks, nil), reference)
		if _, err := mergeNamespaceProjectionRuns(ctx, destination, []storageformat.DomainTreeRoot{root}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("projection output publication error = %v", err)
		}
	})
}
