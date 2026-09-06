package portable

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func seedNamespaceOutcome(t *testing.T, store *namespaceStore, owner domain.UserID, mutationID string, resultBody []byte) {
	t.Helper()
	if _, err := store.domain.mutate(context.Background(), namespaceReference(owner), consistencyDomainMutation{
		ID: mutationID, Changes: []consistencyDomainChange{{Key: "test-outcome/" + mutationID, Require: domainValueAbsent, Value: []byte("retained")}}, Result: resultBody,
	}); err != nil {
		t.Fatal(err)
	}
}

func encodeNamespaceResultForTest(t *testing.T, result storageformat.NamespaceMutationResult) []byte {
	t.Helper()
	body, err := storageformat.EncodeCanonical(result)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestSchema008NamespaceReplayTypeConfusionFailsClosed(t *testing.T) {
	ctx := context.Background()
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	live, _ := domain.NewScope(owner, domain.AreaLive)
	now := time.Date(2064, 1, 2, 3, 4, 5, 0, time.UTC)
	validEntry := storageformat.DirectoryEntry{Name: "result", NameDigest: storageformat.NameDigest("result"), Kind: domain.EntryFile, BlobID: "result", Size: 1, MediaType: "application/octet-stream", MD5: testProviderMD5, CRC32C: testProviderCRC32C, ModifiedAt: now}
	validEntry.LogicalVersion, _ = directoryEntryVersion(validEntry)
	validOperation := domain.Operation{ID: "result-operation", State: domain.OperationSucceeded, StartedAt: now, UpdatedAt: now}

	t.Run("copy-requires-operation", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		store := newNamespaceStore(engine)
		source := seedNamespaceBatchFiles(t, store, live, 1)[0]
		destination := domain.MustParseUserPath("/copy-target")
		key := "copy-result-type"
		mutationID := string(namespaceOperationID(owner, operationCopy, key))
		fingerprint := namespaceRequestFingerprint(operationCopy, "live", "live", source.Path.String(), destination.String(), string(domain.ConflictFail), "", "")
		seedNamespaceOutcome(t, store, owner, mutationID, encodeNamespaceResultForTest(t, storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: fingerprint, Entry: &validEntry}))
		if _, err := store.copyOrMove(ctx, false, live, live, domain.CopyRequest{Source: source.Path, Destination: destination, IdempotencyKey: key}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("copy result type error = %v", err)
		}
	})

	t.Run("delete-requires-operation", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		store := newNamespaceStore(engine)
		source := seedNamespaceBatchFiles(t, store, live, 1)[0]
		key := "delete-result-type"
		mutationID := string(namespaceOperationID(owner, operationDelete, key))
		fingerprint := namespaceRequestFingerprint(operationDelete, "live", source.Path.String(), "")
		seedNamespaceOutcome(t, store, owner, mutationID, encodeNamespaceResultForTest(t, storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: fingerprint, Entry: &validEntry}))
		if _, err := store.delete(ctx, live, domain.DeleteRequest{Path: source.Path, IdempotencyKey: key}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("delete result type error = %v", err)
		}
	})

	t.Run("file-publication-requires-entry", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		store := newNamespaceStore(engine)
		mutationID, fingerprint := "publish-result-type", storageformat.Digest([]byte("publish-result-type"))
		seedNamespaceOutcome(t, store, owner, mutationID, encodeNamespaceResultForTest(t, storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: fingerprint, Operation: &validOperation}))
		entry := validNamespaceTestFile(t, engine, "published", 1).Entry
		if _, err := store.publishFileWithChanges(ctx, live, domain.MustParseUserPath("/published"), domain.ConflictFail, "", mutationID, fingerprint, entry, nil); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("publication result type error = %v", err)
		}
	})

	t.Run("batch-copy-requires-batch", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		store := newNamespaceStore(engine)
		source := seedNamespaceBatchFiles(t, store, live, 1)[0]
		key := "batch-result-type"
		specs := []namespaceBatchMoveSpec{{from: live, to: live, request: domain.CopyRequest{Source: source.Path, Destination: domain.MustParseUserPath("/batch-target")}}}
		fingerprint, err := namespaceBatchFingerprint("batch-copy", specs)
		if err != nil {
			t.Fatal(err)
		}
		mutationID := string(namespaceOperationID(owner, "batch-copy", key))
		seedNamespaceOutcome(t, store, owner, mutationID, encodeNamespaceResultForTest(t, storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: fingerprint, Operation: &validOperation}))
		if _, err := store.batchCopyOrMove(ctx, owner, specs, false, "batch-copy", key); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("batch copy result type error = %v", err)
		}
	})

	t.Run("batch-delete-requires-batch", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		store := newNamespaceStore(engine)
		trashIDs := []string{"result-type"}
		type intent struct {
			TrashIDs []string `json:"trashIDs"`
		}
		intentBody, err := storageformat.EncodeCanonical(intent{TrashIDs: trashIDs})
		if err != nil {
			t.Fatal(err)
		}
		fingerprint := storageformat.Digest(append([]byte("endlessfs-namespace-batch-delete-trash-v1\x00"), intentBody...))
		key := "batch-delete-result-type"
		mutationID := string(namespaceOperationID(owner, "batch-delete-trash", key))
		seedNamespaceOutcome(t, store, owner, mutationID, encodeNamespaceResultForTest(t, storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: fingerprint, Operation: &validOperation}))
		if _, err := engine.Files().BatchDeleteFromTrash(ctx, owner, trashIDs, key); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("batch delete result type error = %v", err)
		}
	})

	t.Run("batch-operation-rejects-malformed-result", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		store := newNamespaceStore(engine)
		operationID := domain.OperationID("malformed-batch-operation")
		seedNamespaceOutcome(t, store, owner, string(operationID), []byte("not canonical"))
		if _, err := engine.Files().GetBatchOperation(ctx, owner, operationID); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("malformed batch operation error = %v", err)
		}
	})
}

func TestSchema008NamespaceBatchResultCorruptionAndCommitFailureMatrix(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	view, err := store.loadView(ctx, live.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	now := engine.clock.Now().UTC()
	operation := domain.Operation{ID: "batch-result", State: domain.OperationSucceeded, StartedAt: now, UpdatedAt: now}
	validItem := storageformat.NamespaceBatchItem{Index: 0, Source: "/source", Destination: "/destination", OperationID: operation.ID, State: domain.OperationSucceeded}
	validBody, err := storageformat.EncodeCanonical(validItem)
	if err != nil {
		t.Fatal(err)
	}
	build := func(key string, body []byte) storageformat.DomainTreeRoot {
		root, buildErr := view.session.buildTree(ctx, []storageformat.DomainEntry{{Key: key, Value: body, LogicalVersion: "version"}})
		if buildErr != nil {
			t.Fatal(buildErr)
		}
		return root
	}

	if _, err := store.decodeBatchResult(ctx, view, storageformat.NamespaceBatch{Operation: domain.Operation{}, Items: build("0000000000000000", validBody), ItemCount: 1}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid batch operation error = %v", err)
	}
	missing := storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("missing-batch-result")), Level: 0, EntryCount: 1}
	if _, err := store.decodeBatchResult(ctx, view, storageformat.NamespaceBatch{Operation: operation, Items: missing, ItemCount: 1}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing batch result page error = %v", err)
	}
	if _, err := store.decodeBatchResult(ctx, view, storageformat.NamespaceBatch{Operation: operation, Items: build("0000000000000001", validBody), ItemCount: 1}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("misordered batch result error = %v", err)
	}
	if _, err := store.decodeBatchResult(ctx, view, storageformat.NamespaceBatch{Operation: operation, Items: build("0000000000000000", []byte("not canonical")), ItemCount: 1}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("malformed batch item error = %v", err)
	}

	t.Run("trash-metadata", func(t *testing.T) {
		plain := publishNamespaceTestFile(t, store, namespaceTestScope(t, domain.AreaTrash), "/plain-trash", 1, "plain-trash-batch")
		if _, err := engine.Files().BatchDeleteFromTrash(ctx, live.UserID(), []string{plain.Path.Name()}, "plain-trash-batch"); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("plain trash batch delete error = %v", err)
		}
	})

	t.Run("head-publication", func(t *testing.T) {
		memory := objectmemory.New()
		hooks := &hookedBackend{Backend: memory}
		failureEngine := openNamespaceTestEngine(t, hooks)
		failureStore := newNamespaceStore(failureEngine)
		failureLive := namespaceTestScope(t, domain.AreaLive)
		entry := seedNamespaceBatchFiles(t, failureStore, failureLive, 1)[0]
		hooks.put = func(callCtx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if key == storageformat.DomainHeadKey(storageformat.DomainNamespace, failureLive.UserID().String()) {
				return "", domain.NewError(domain.ErrorUnavailable, "namespace head publication failed")
			}
			return memory.Put(callCtx, key, body, condition)
		}
		request := domain.TrashRequest{Path: entry.Path, ExpectedVersion: entry.Version, TrashID: "publication-failure"}
		if _, err := failureEngine.Files().BatchMoveToTrash(ctx, failureLive.UserID(), []domain.TrashRequest{request}, "publication-failure"); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("batch head publication error = %v", err)
		}
	})
}

func seedNamespaceProjectionHead(t *testing.T, backend *objectmemory.Backend, view *namespaceView, owner domain.UserID, root storageformat.DomainTreeRoot) {
	t.Helper()
	directory := view.roots[domain.AreaTrash]
	projectionID := namespaceProjectionID(owner, domain.AreaTrash, directory, domain.SortModified)
	key := storageformat.ScopedProjectionHeadKey(owner.String(), storageformat.ProjectionModified, projectionID)
	head := storageformat.ProjectionHead{SchemaVersion: 1, OwnerID: owner.String(), ProjectionID: projectionID, Kind: storageformat.ProjectionModified, SourceDomainID: view.reference.ID, SourceRevision: view.head.Revision, SourceRoot: directory.Children, Root: root}
	body, err := storageformat.EncodeEnvelope(namespaceProjectionHeadSchema, key, 1, head)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(context.Background(), key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
}

func TestSchema008NamespaceProjectionAndTrashListingNestedFailureMatrix(t *testing.T) {
	ctx := context.Background()
	live := namespaceTestScope(t, domain.AreaLive)

	t.Run("sort-projection-invalid-field-and-source", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		store := newNamespaceStore(engine)
		seedNamespaceBatchFiles(t, store, live, 1)
		view, err := store.loadView(ctx, live.UserID(), "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.namespaceSortProjection(ctx, view, domain.AreaLive, view.roots[domain.AreaLive], "invalid"); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid projection field error = %v", err)
		}
		directory := view.roots[domain.AreaLive]
		directory.Children = storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("missing-projection-source")), Level: 0, EntryCount: 1}
		if _, err := store.namespaceSortProjection(ctx, view, domain.AreaLive, directory, domain.SortSize); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing projection source error = %v", err)
		}
	})

	t.Run("projection-tree-publication", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		store := newNamespaceStore(engine)
		seedNamespaceBatchFiles(t, store, live, 1)
		view, err := store.loadView(ctx, live.UserID(), "")
		if err != nil {
			t.Fatal(err)
		}
		failure := domain.NewError(domain.ErrorUnavailable, "projection page publication failed")
		hooks := &hookedBackend{Backend: memory, put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return "", failure
		}}
		failureStore := newNamespaceStore(&Engine{backend: hooks, scheduler: engine.scheduler, clock: engine.clock})
		projection := newNamespaceProjectionTreeSession(failureStore.domain, live.UserID(), storageformat.Digest([]byte("failed-projection")), storageformat.ProjectionSize)
		if _, err := failureStore.buildNamespaceSortProjection(ctx, view, projection, view.roots[domain.AreaLive].Children, domain.SortSize); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("projection page publication error = %v", err)
		}
	})

	for name, projectionValue := range map[string][]byte{
		"malformed-entry":        []byte("not canonical"),
		"missing-trash-metadata": nil,
	} {
		t.Run("trash-"+name, func(t *testing.T) {
			backend := objectmemory.New()
			engine := openNamespaceTestEngine(t, backend)
			store := newNamespaceStore(engine)
			plain := publishNamespaceTestFile(t, store, namespaceTestScope(t, domain.AreaTrash), "/plain", 1, "trash-list-"+name)
			view, err := store.loadView(ctx, live.UserID(), "")
			if err != nil {
				t.Fatal(err)
			}
			if projectionValue == nil {
				entry, err := store.resolveEntryAtView(ctx, view, namespaceTestScope(t, domain.AreaTrash), plain.Path)
				if err != nil {
					t.Fatal(err)
				}
				projectionValue, err = encodeNamespaceEntry(entry)
				if err != nil {
					t.Fatal(err)
				}
			}
			projectionID := namespaceProjectionID(live.UserID(), domain.AreaTrash, view.roots[domain.AreaTrash], domain.SortModified)
			projectionSession := newNamespaceProjectionTreeSession(store.domain, live.UserID(), projectionID, storageformat.ProjectionModified)
			projectionRoot, err := projectionSession.buildTree(ctx, []storageformat.DomainEntry{{Key: "projection", Value: projectionValue, LogicalVersion: "version"}})
			if err != nil {
				t.Fatal(err)
			}
			seedNamespaceProjectionHead(t, backend, view, live.UserID(), projectionRoot)
			if _, err := engine.Files().ListTrash(ctx, live.UserID(), domain.TrashListRequest{}); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("trash projection error = %v", err)
			}
		})
	}

	t.Run("trash-missing-projection-page", func(t *testing.T) {
		backend := objectmemory.New()
		engine := openNamespaceTestEngine(t, backend)
		store := newNamespaceStore(engine)
		publishNamespaceTestFile(t, store, namespaceTestScope(t, domain.AreaTrash), "/plain", 1, "trash-list-missing-page")
		view, err := store.loadView(ctx, live.UserID(), "")
		if err != nil {
			t.Fatal(err)
		}
		missing := storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("missing-trash-projection")), Level: 0, EntryCount: 1}
		seedNamespaceProjectionHead(t, backend, view, live.UserID(), missing)
		if _, err := engine.Files().ListTrash(ctx, live.UserID(), domain.TrashListRequest{}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing trash projection error = %v", err)
		}
	})
}

func TestSchema008NamespaceHelperFailureBoundaries(t *testing.T) {
	ctx := context.Background()
	memory := objectmemory.New()
	engine := openNamespaceTestEngine(t, memory)
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	seeded := seedNamespaceBatchFiles(t, store, live, 1)[0]

	failure := domain.NewError(domain.ErrorUnavailable, "namespace unavailable")
	hooks := &hookedBackend{Backend: memory, get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
		return objectstore.Object{}, failure
	}}
	failingEngine := *engine
	failingEngine.backend = hooks
	if _, err := newNamespaceStore(&failingEngine).resolveEntry(ctx, live, seeded.Path); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("resolve entry provider error = %v", err)
	}
	if _, err := newNamespaceStore(&failingEngine).lookupChildren(ctx, live, domain.ChildLookupRequest{Directory: namespaceRootPath(), Names: []string{seeded.Path.Name()}}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("lookup children provider error = %v", err)
	}
	failingFile := validNamespaceTestFile(t, engine, "provider-failure", 1).Entry
	if _, err := newNamespaceStore(&failingEngine).publishFileWithChanges(ctx, live, domain.MustParseUserPath("/provider-failure"), domain.ConflictFail, "", "provider-failure", "fingerprint", failingFile, nil); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("file publication provider error = %v", err)
	}
	if _, err := newNamespaceStore(&failingEngine).getOperation(ctx, live.UserID(), "provider-failure"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("operation lookup provider error = %v", err)
	}
	if _, err := store.lookupChildren(ctx, live, domain.ChildLookupRequest{Directory: domain.MustParseUserPath("/missing"), Names: []string{"child"}}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("lookup children missing parent error = %v", err)
	}

	view, err := store.loadView(ctx, live.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	missingChildren := storageformat.NamespaceEntry{SchemaVersion: 1, NodeID: "missing-children", Entry: storageformat.DirectoryEntry{Kind: domain.EntryDirectory, DirectoryID: "missing-children"}, Children: storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("missing-children")), Level: 0, EntryCount: 1}, EntryCount: 1}
	if _, _, err := store.resolveDestination(ctx, view, missingChildren, domain.MustParseUserPath("/target"), domain.ConflictFail, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("destination child read error = %v", err)
	}

	parent := view.roots[domain.AreaLive]
	zero := validNamespaceTestFile(t, engine, "zero", 0)
	parent.EntryCount, parent.Entry.Size, parent.Entry.FileCount = 0, 0, 1
	if _, err := store.applyDirectoryEdits(ctx, view, parent, []namespaceDirectoryEdit{{before: &zero}}, engine.clock.Now()); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("directory count underflow error = %v", err)
	}
	parent = view.roots[domain.AreaLive]
	parent.NodeID = ""
	if _, err := store.applyDirectoryEdits(ctx, view, parent, nil, engine.clock.Now()); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("updated directory validation error = %v", err)
	}

	now := engine.clock.Now().UTC()
	if err := validateNamespaceMutationResult(storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: storageformat.Digest([]byte("result")), Operation: &domain.Operation{ID: "operation", State: domain.OperationSucceeded, StartedAt: now, UpdatedAt: now, ErrorKind: domain.ErrorInternal, Error: "impossible"}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("successful operation error metadata = %v", err)
	}
	if _, err := store.encodeListCursor(namespaceListCursor{SchemaVersion: 1, OwnerID: strings.Repeat("x", storageformat.MaxCanonicalBytes), Area: "live", Directory: "/", PageSize: 1, Snapshot: "snapshot", Bound: "bound", ExpiresAt: now.Add(time.Hour)}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized cursor error = %v", err)
	}

	if _, err := store.createDirectory(ctx, live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/missing/child")}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("create directory missing parent error = %v", err)
	}
	if _, err := store.delete(ctx, live, domain.DeleteRequest{Path: domain.MustParseUserPath("/missing/child"), IdempotencyKey: "missing-delete-parent"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("delete missing parent error = %v", err)
	}
	entry := validNamespaceTestFile(t, engine, "invalid-provider-hash", 1).Entry
	entry.MD5 = "invalid"
	if _, err := store.publishFileWithChanges(ctx, live, domain.MustParseUserPath("/invalid-provider-hash"), domain.ConflictFail, "", "invalid-provider-hash", "fingerprint", entry, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid provider fingerprint publication error = %v", err)
	}
	if _, err := store.publishFileWithChanges(ctx, live, domain.MustParseUserPath("/missing/file"), domain.ConflictFail, "", "missing-parent-file", "fingerprint", validNamespaceTestFile(t, engine, "file", 1).Entry, nil); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("file publication missing parent error = %v", err)
	}
	seedNamespaceOutcome(t, store, live.UserID(), "malformed-publication-replay", []byte("not canonical"))
	if _, err := store.publishFileWithChanges(ctx, live, domain.MustParseUserPath("/malformed-publication-replay"), domain.ConflictFail, "", "malformed-publication-replay", "fingerprint", validNamespaceTestFile(t, engine, "malformed-publication-replay", 1).Entry, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("malformed publication replay error = %v", err)
	}
	seedNamespaceOutcome(t, store, live.UserID(), "malformed-operation-replay", []byte("not canonical"))
	if _, err := store.getOperation(ctx, live.UserID(), "malformed-operation-replay"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("malformed operation replay error = %v", err)
	}
	if _, err := store.copyOrMove(ctx, false, live, live, domain.CopyRequest{Source: seeded.Path, Destination: domain.MustParseUserPath("/target"), IdempotencyKey: strings.Repeat("x", 129)}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("copy idempotency bound error = %v", err)
	}

	// Same-parent replacement exercises the single-root edit path, including
	// removal of both source and existing destination before publication.
	existing := publishNamespaceTestFile(t, store, live, "/existing", 1, "same-parent-existing")
	if _, err := store.copyOrMove(ctx, true, live, live, domain.CopyRequest{Source: seeded.Path, Destination: existing.Path, Conflict: domain.ConflictReplace, ExpectedTarget: existing.Version, IdempotencyKey: "same-parent-replace"}); err != nil {
		t.Fatalf("same-parent replacement error = %v", err)
	}

	// The canonical decoder must reject a structurally canonical but invalid
	// namespace entry, not just malformed JSON.
	invalidBody, err := storageformat.EncodeCanonical(storageformat.NamespaceEntry{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeNamespaceEntry(invalidBody); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid namespace entry body error = %v", err)
	}

	// Cursor entropy failures must remain explicit and never emit a weak token.
	entropyEngine := *engine
	entropyEngine.ids = domain.NewIDGenerator(bytes.NewReader(nil))
	if _, err := newNamespaceStore(&entropyEngine).encodeListCursor(namespaceListCursor{SchemaVersion: 1, OwnerID: live.UserID().String(), Area: "live", Directory: "/", PageSize: 1, Snapshot: "snapshot", Bound: "bound", ExpiresAt: now.Add(time.Hour)}); err == nil {
		t.Fatal("cursor encoding succeeded without entropy")
	}
}
