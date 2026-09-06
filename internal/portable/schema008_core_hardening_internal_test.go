package portable

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestConsistencyDomainCurrentAuthorityDenialMatrix(t *testing.T) {
	ctx := context.Background()
	valid := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner-core-denials"}
	for name, reference := range map[string]consistencyDomainRef{
		"empty-id": {Kind: storageformat.DomainOwnerControl},
		"kind":     {Kind: "retired-authority", ID: "owner"},
	} {
		t.Run("reference-"+name, func(t *testing.T) {
			if err := validateConsistencyDomainRef(reference); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("reference error = %v", err)
			}
		})
	}
	if _, err := (*consistencyDomainStore)(nil).loadHead(ctx, valid); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("nil store error = %v", err)
	}

	invalidMutations := map[string]consistencyDomainMutation{
		"empty":              {},
		"no-change":          {ID: "empty"},
		"empty-key":          {ID: "empty-key", Changes: []consistencyDomainChange{{Require: domainValueAny}}},
		"duplicate-key":      {ID: "duplicate", Changes: []consistencyDomainChange{{Key: "same", Require: domainValueAny}, {Key: "same", Require: domainValueAny}}},
		"requirement":        {ID: "requirement", Changes: []consistencyDomainChange{{Key: "key"}}},
		"unexpected-version": {ID: "unexpected-version", Changes: []consistencyDomainChange{{Key: "key", Require: domainValueAbsent, ExpectedVersion: "version"}}},
		"delete-value":       {ID: "delete-value", Changes: []consistencyDomainChange{{Key: "key", Require: domainValuePresent, Delete: true, Value: []byte("value")}}},
	}
	for name, mutation := range invalidMutations {
		t.Run("mutation-"+name, func(t *testing.T) {
			if _, _, err := normalizeConsistencyDomainMutation(mutation); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("normalization error = %v", err)
			}
		})
	}
	oversized := consistencyDomainMutation{ID: "oversized", Changes: []consistencyDomainChange{{Key: "key", Require: domainValueAbsent, Value: make([]byte, storageformat.MaxCanonicalBytes)}}}
	if _, _, err := normalizeConsistencyDomainMutation(oversized); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized mutation error = %v", err)
	}

	backend := objectmemory.New()
	store := newConsistencyDomainStore(backend, nil)
	if _, _, _, err := store.list(ctx, valid, "", "", 1, time.Now()); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty list prefix error = %v", err)
	}
	if _, _, _, err := store.list(ctx, valid, "prefix/", "", 0, time.Now()); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("zero list limit error = %v", err)
	}
	if values, revision, snapshot, err := store.list(ctx, valid, "prefix/", "", 1, time.Now()); err != nil || len(values) != 0 || revision != 0 || snapshot != "" {
		t.Fatalf("absent domain list = %+v, %d, %q, %v", values, revision, snapshot, err)
	}
	if _, err := store.get(ctx, valid, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty get key error = %v", err)
	}
	if _, err := store.get(ctx, valid, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing get error = %v", err)
	}
	if err := store.compact(ctx, valid); err != nil {
		t.Fatalf("empty compaction = %v", err)
	}
	if err := store.freeze(ctx, valid, 0); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("zero freeze error = %v", err)
	}
	if err := store.unfreeze(ctx, valid, 0); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("zero unfreeze error = %v", err)
	}
	if err := store.unfreeze(ctx, valid, 1); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing unfreeze error = %v", err)
	}

	if _, err := store.mutate(ctx, valid, consistencyDomainMutation{ID: "seed", Changes: []consistencyDomainChange{{Key: "record", Require: domainValueAbsent, Value: []byte("one")}}}); err != nil {
		t.Fatal(err)
	}
	value, err := store.get(ctx, valid, "record")
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]consistencyDomainChange{
		"already-present": {Key: "record", Require: domainValueAbsent, Value: []byte("two")},
		"missing":         {Key: "missing", Require: domainValuePresent, Value: []byte("two")},
		"stale":           {Key: "record", Require: domainValuePresent, ExpectedVersion: "stale", Value: []byte("two")},
		"delete-missing":  {Key: "missing", Require: domainValueAny, Delete: true},
	} {
		t.Run("precondition-"+name, func(t *testing.T) {
			_, err := store.mutate(ctx, valid, consistencyDomainMutation{ID: "denied-" + name, Changes: []consistencyDomainChange{change}})
			if err == nil {
				t.Fatal("denied mutation succeeded")
			}
		})
	}
	if value.LogicalVersion == "" {
		t.Fatal("seed logical version is empty")
	}
	if _, err := store.mutate(ctx, valid, consistencyDomainMutation{ID: "seed", Changes: []consistencyDomainChange{{Key: "different", Require: domainValueAbsent, Value: []byte("two")}}}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("reused mutation identity error = %v", err)
	}
	if err := store.freeze(ctx, valid, 11); err != nil {
		t.Fatal(err)
	}
	if err := store.freeze(ctx, valid, 11); err != nil {
		t.Fatalf("idempotent freeze = %v", err)
	}
	if err := store.freeze(ctx, valid, 12); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("different freeze epoch error = %v", err)
	}
	if _, err := store.mutate(ctx, valid, consistencyDomainMutation{ID: "frozen", Changes: []consistencyDomainChange{{Key: "frozen", Require: domainValueAbsent, Value: []byte("no")}}}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("frozen mutation error = %v", err)
	}
	if err := store.compact(ctx, valid); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("frozen compaction error = %v", err)
	}
	if err := store.unfreeze(ctx, valid, 12); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("different unfreeze epoch error = %v", err)
	}
	if err := store.unfreeze(ctx, valid, 11); err != nil {
		t.Fatal(err)
	}
	if err := store.unfreeze(ctx, valid, 11); err != nil {
		t.Fatalf("idempotent unfreeze = %v", err)
	}
}

func TestConsistencyDomainSnapshotsRejectDigestBindingAndSchemaCorruption(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	store := newConsistencyDomainStore(backend, nil)
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner-snapshots"}
	if _, err := store.loadHeadSnapshot(ctx, reference, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty snapshot digest error = %v", err)
	}
	if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: "seed", Changes: []consistencyDomainChange{{Key: "entry", Require: domainValueAbsent, Value: []byte("value")}}}); err != nil {
		t.Fatal(err)
	}
	head, err := store.loadHead(ctx, reference)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := store.writeHeadSnapshot(ctx, reference, head.head, time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if loaded, err := store.loadHeadSnapshot(ctx, reference, digest); err != nil || loaded.Revision != head.head.Revision {
		t.Fatalf("valid snapshot = %+v, %v", loaded, err)
	}

	wrongReference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "other-owner"}
	misbound := storageformat.DomainSnapshot{SchemaVersion: 1, DomainID: wrongReference.ID, Kind: wrongReference.Kind, Head: storageformat.DomainHead{SchemaVersion: 1, DomainID: wrongReference.ID, Kind: wrongReference.Kind, Registered: true}, ExpiresAt: time.Now().UTC().Add(time.Minute)}
	body, err := storageformat.EncodeCanonical(misbound)
	if err != nil {
		t.Fatal(err)
	}
	misboundDigest := storageformat.Digest(body)
	if _, err := backend.Put(ctx, storageformat.DomainSnapshotKey(reference.Kind, reference.ID, misboundDigest), body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.loadHeadSnapshot(ctx, reference, misboundDigest); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("misbound snapshot error = %v", err)
	}

	key := storageformat.DomainSnapshotKey(reference.Kind, reference.ID, digest)
	object, err := backend.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, key, append(object.Body, '\n'), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.loadHeadSnapshot(ctx, reference, digest); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("digest-corrupt snapshot error = %v", err)
	}
}

func TestConsistencyDomainStreamingTreeTraversesThreeLevelsWithBoundedState(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner-streaming-tree"}
	session := newConsistencyDomainTreeSession(newConsistencyDomainStore(backend, nil), reference)
	builder := newConsistencyDomainTreeBuilder(ctx, session)
	const count = domainPageMaximumItems*domainPageMaximumItems + 1
	for index := 0; index < count; index++ {
		key := fmt.Sprintf("entry/%06d", index)
		if err := builder.Add(storageformat.DomainEntry{Key: key, Value: []byte(key), LogicalVersion: fmt.Sprintf("version-%06d", index)}); err != nil {
			t.Fatalf("Add(%d) = %v", index, err)
		}
	}
	root, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if root.Level < 2 || root.EntryCount != count {
		t.Fatalf("streaming root = %+v", root)
	}
	if len(session.pages) > 1 {
		t.Fatalf("builder retained %d pages in memory", len(session.pages))
	}

	after := fmt.Sprintf("entry/%06d", count-4)
	iterator, err := newConsistencyDomainTreeIteratorAfter(ctx, newConsistencyDomainTreeSession(newConsistencyDomainStore(backend, nil), reference), root, after)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for {
		entry, found, err := iterator.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			break
		}
		keys = append(keys, entry.Key)
	}
	if len(keys) != 3 || keys[0] != fmt.Sprintf("entry/%06d", count-3) || keys[2] != fmt.Sprintf("entry/%06d", count-1) {
		t.Fatalf("iterator tail = %+v", keys)
	}
	end, err := newConsistencyDomainTreeIteratorAfter(ctx, newConsistencyDomainTreeSession(newConsistencyDomainStore(backend, nil), reference), root, "z")
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := end.Next(); err != nil || found {
		t.Fatalf("iterator after end found=%v error=%v", found, err)
	}
	empty, err := newConsistencyDomainTreeIteratorAfter(ctx, session, storageformat.DomainTreeRoot{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := empty.Next(); err != nil || found {
		t.Fatalf("empty iterator found=%v error=%v", found, err)
	}

	tooLarge := newConsistencyDomainTreeBuilder(ctx, newConsistencyDomainTreeSession(newConsistencyDomainStore(objectmemory.New(), nil), reference))
	if err := tooLarge.Add(storageformat.DomainEntry{Key: "oversized", Value: make([]byte, storageformat.MaxCanonicalBytes), LogicalVersion: "version"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized streamed entry error = %v", err)
	}
	unsorted := newConsistencyDomainTreeBuilder(ctx, newConsistencyDomainTreeSession(newConsistencyDomainStore(objectmemory.New(), nil), reference))
	if err := unsorted.Add(storageformat.DomainEntry{Key: "b", Value: []byte("b"), LogicalVersion: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := unsorted.Add(storageformat.DomainEntry{Key: "a", Value: []byte("a"), LogicalVersion: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := unsorted.Finish(); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("unsorted streamed tree error = %v", err)
	}
}

func TestCheckpointReachabilityExternalMergeIsSortedExactAndBounded(t *testing.T) {
	collector, err := newCheckpointReachabilityCollector()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = collector.Close() })
	collector.buffer = make([]string, 0, 2)
	for index := 0; index < checkpointReachabilityMergeWidth*2+3; index++ {
		key := objectstore.MustKey(fmt.Sprintf("endlessfs/v1/test/reachable-%04d", checkpointReachabilityMergeWidth*2+2-index))
		if err := collector.Add(key); err != nil {
			t.Fatal(err)
		}
		if index%7 == 0 {
			if err := collector.Add(key); err != nil {
				t.Fatal(err)
			}
		}
	}
	stream, err := collector.Stream()
	if err != nil {
		t.Fatal(err)
	}
	previous, count := "", 0
	for {
		key, found, err := stream.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			break
		}
		if previous != "" && key.String() <= previous {
			t.Fatalf("reachability stream order %q after %q", key.String(), previous)
		}
		previous = key.String()
		count++
	}
	if count != checkpointReachabilityMergeWidth*2+3 {
		t.Fatalf("reachability count = %d", count)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := (*checkpointReachabilityCollector)(nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := collector.Add(objectstore.Key{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid reachability key error = %v", err)
	}
	if err := writeCheckpointReachabilityKey(&bytes.Buffer{}, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty reachability key error = %v", err)
	}
}

func TestSchema008TrashListingPermanentDeleteAndBatchOutcome(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	engine := openNamespaceTestEngine(t, backend)
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	seeded := seedNamespaceBatchFiles(t, store, live, 4)
	for index, entry := range seeded {
		trashID := fmt.Sprintf("trash-%02d", index)
		if _, err := engine.Files().MoveToTrash(ctx, live.UserID(), domain.TrashRequest{Path: entry.Path, ExpectedVersion: entry.Version, TrashID: trashID, IdempotencyKey: "move-" + trashID}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := engine.Files().ListTrash(ctx, live.UserID(), domain.TrashListRequest{Limit: 2})
	if err != nil || len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("first trash page = %+v, %v", first, err)
	}
	second, err := engine.Files().ListTrash(ctx, live.UserID(), domain.TrashListRequest{Limit: 2, Cursor: first.NextCursor})
	if err != nil || len(second.Items) != 2 {
		t.Fatalf("second trash page = %+v, %v", second, err)
	}
	if _, err := engine.Files().ListTrash(ctx, live.UserID(), domain.TrashListRequest{Limit: 3, Cursor: first.NextCursor}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("out-of-scope trash cursor error = %v", err)
	}
	if _, err := engine.Files().DeleteFromTrash(ctx, live.UserID(), "trash-00", "delete-trash-00"); err != nil {
		t.Fatal(err)
	}
	trash, _ := domain.NewScope(live.UserID(), domain.AreaTrash)
	if _, err := engine.Files().Stat(ctx, trash, domain.MustParseUserPath("/trash-00")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("permanently deleted trash entry error = %v", err)
	}

	batch, err := engine.Files().BatchDeleteFromTrash(ctx, live.UserID(), []string{"trash-01", "trash-02"}, "batch-delete-trash")
	if err != nil || batch.Operation.State != domain.OperationSucceeded || len(batch.Items) != 2 {
		t.Fatalf("batch delete = %+v, %v", batch, err)
	}
	replay, err := engine.Files().BatchDeleteFromTrash(ctx, live.UserID(), []string{"trash-01", "trash-02"}, "batch-delete-trash")
	if err != nil || replay.Operation.ID != batch.Operation.ID || len(replay.Items) != 2 {
		t.Fatalf("batch replay = %+v, %v", replay, err)
	}
	operation, err := engine.Files().GetBatchOperation(ctx, live.UserID(), batch.Operation.ID)
	if err != nil || operation.ID != batch.Operation.ID {
		t.Fatalf("batch operation = %+v, %v", operation, err)
	}
	if _, err := engine.Files().GetBatchOperation(ctx, live.UserID(), "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing batch operation error = %v", err)
	}
	if _, err := engine.Files().BatchDeleteFromTrash(ctx, live.UserID(), []string{"trash-03", "trash-03"}, "duplicate-trash"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("duplicate trash identity error = %v", err)
	}
	if _, err := engine.Files().ListTrash(ctx, domain.UserID{}, domain.TrashListRequest{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid trash owner error = %v", err)
	}
	if _, err := engine.Files().ListTrash(ctx, live.UserID(), domain.TrashListRequest{Limit: 10001}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid trash limit error = %v", err)
	}
}

func TestSchema008PublicRuntimeWrappersRejectCanceledMutations(t *testing.T) {
	engine := openNamespaceTestEngine(t, objectmemory.New())
	scope := namespaceTestScope(t, domain.AreaLive)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	checks := []func() error{
		func() error { _, err := engine.Files().List(canceled, scope, domain.ListRequest{}); return err },
		func() error {
			_, err := engine.Files().LookupChildren(canceled, scope, domain.ChildLookupRequest{})
			return err
		},
		func() error {
			_, err := engine.Files().Stat(canceled, scope, domain.MustParseUserPath("/file"))
			return err
		},
		func() error {
			_, err := engine.Files().CreateDirectory(canceled, scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/dir")})
			return err
		},
		func() error { _, err := engine.Files().Copy(canceled, scope, scope, domain.CopyRequest{}); return err },
		func() error { _, err := engine.Files().Move(canceled, scope, scope, domain.MoveRequest{}); return err },
		func() error { _, err := engine.Files().Delete(canceled, scope, domain.DeleteRequest{}); return err },
		func() error {
			_, err := engine.Files().CreateUpload(canceled, scope, domain.CreateUploadRequest{})
			return err
		},
		func() error { _, err := engine.Files().UploadStatus(canceled, scope, "upload"); return err },
		func() error {
			_, err := engine.Files().CompleteUpload(canceled, scope, domain.CompleteUploadRequest{})
			return err
		},
		func() error { return engine.Files().AbortUpload(canceled, scope, "upload") },
		func() error {
			_, err := engine.Files().CreateDownload(canceled, scope, domain.CreateDownloadRequest{})
			return err
		},
	}
	for index, check := range checks {
		if err := check(); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("wrapper %d cancellation error = %v", index, err)
		}
	}
	if _, err := engine.Files().GetOperation(canceled, scope.UserID(), "operation"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("GetOperation cancellation error = %v", err)
	}
	if _, err := engine.Files().GetOperation(context.Background(), domain.UserID{}, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("GetOperation input error = %v", err)
	}
}

func TestDomainCatalogRejectsInvalidFreezeAndVisitorInputs(t *testing.T) {
	catalog := newDomainCatalog(objectmemory.New(), nil)
	ctx := context.Background()
	if err := catalog.register(ctx, consistencyDomainRef{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid registration error = %v", err)
	}
	if _, err := catalog.freeze(ctx, 0); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("zero catalog freeze error = %v", err)
	}
	if err := catalog.unfreeze(ctx, 0); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("zero catalog unfreeze error = %v", err)
	}
	if err := catalog.visitEntries(ctx, storageformat.DomainCatalogHead{SchemaVersion: 1}, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("nil catalog visitor error = %v", err)
	}
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner-catalog-matrix"}
	if err := catalog.store.ensureRegistered(ctx, reference); err != nil {
		t.Fatal(err)
	}
	if err := catalog.register(ctx, reference); err != nil {
		t.Fatalf("idempotent registration = %v", err)
	}
	if _, err := catalog.freeze(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.freeze(ctx, 8); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("different catalog freeze epoch error = %v", err)
	}
	if err := catalog.register(ctx, consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "late-owner"}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("registration during freeze error = %v", err)
	}
	if err := catalog.unfreeze(ctx, 8); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("different catalog unfreeze epoch error = %v", err)
	}
	if err := catalog.unfreeze(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if err := catalog.unfreeze(ctx, 7); err != nil {
		t.Fatalf("idempotent catalog unfreeze = %v", err)
	}
}

func TestSchema008NoRetiredAuthorityNamesRemainInOrdinaryRuntime(t *testing.T) {
	for _, file := range []string{"runtime008.go", "namespace_store.go", "namespace_batch.go", "namespace_trash.go", "transfers008.go", "duplicates008.go", "state.go", "state_query.go"} {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"startAdmission", "startFileOperation", "prepareFileOperation", "candidate-to-admitted", "readFileOperationObject"} {
			if strings.Contains(string(body), forbidden) {
				t.Fatalf("ordinary runtime %s references retired authority %q", file, forbidden)
			}
		}
	}
}
