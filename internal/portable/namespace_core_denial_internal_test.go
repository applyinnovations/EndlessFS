package portable

import (
	"bytes"
	"context"
	"encoding/base64"
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

func validNamespaceTestFile(t *testing.T, engine *Engine, name string, size int64) storageformat.NamespaceEntry {
	t.Helper()
	entry := storageformat.NamespaceEntry{SchemaVersion: 1, NodeID: "node-" + name, Entry: storageformat.DirectoryEntry{
		Name: name, NameDigest: storageformat.NameDigest(name), Kind: domain.EntryFile, BlobID: "blob-" + name,
		Size: size, MediaType: "application/octet-stream", MD5: testProviderMD5, CRC32C: testProviderCRC32C,
		ModifiedAt: engine.clock.Now().UTC(),
	}}
	var err error
	entry.Entry.LogicalVersion, err = directoryEntryVersion(entry.Entry)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestSchema008NamespaceEntryTreeAndAggregateDenialMatrix(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	view, err := store.loadView(ctx, live.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}

	valid := validNamespaceTestFile(t, engine, "valid", 1)
	invalidVersion := valid
	invalidVersion.Entry.LogicalVersion = "wrong"
	body, err := storageformat.EncodeCanonical(invalidVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeNamespaceEntry(body); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("decode logical-version mismatch error = %v", err)
	}
	if _, err := encodeNamespaceEntry(invalidVersion); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("encode logical-version mismatch error = %v", err)
	}

	validBody, err := encodeNamespaceEntry(valid)
	if err != nil {
		t.Fatal(err)
	}
	children, err := view.session.buildTree(ctx, []storageformat.DomainEntry{{Key: "different", Value: validBody, LogicalVersion: valid.Entry.LogicalVersion}})
	if err != nil {
		t.Fatal(err)
	}
	parent := view.roots[domain.AreaLive]
	parent.Children = children
	parent.EntryCount = 1
	if _, _, err := store.child(ctx, view, parent, "different"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("child key-binding error = %v", err)
	}

	before := valid
	parent = view.roots[domain.AreaLive]
	parent.EntryCount, parent.Entry.FileCount = 0, 1
	if _, err := store.applyDirectoryEdits(ctx, view, parent, []namespaceDirectoryEdit{{before: &before}}, engine.clock.Now()); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("entry-count underflow error = %v", err)
	}
	parent = view.roots[domain.AreaLive]
	parent.EntryCount, parent.Entry.Size, parent.Entry.FileCount = 1, 0, 1
	if _, err := store.applyDirectoryEdits(ctx, view, parent, []namespaceDirectoryEdit{{before: &before}}, engine.clock.Now()); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("byte underflow error = %v", err)
	}
	parent = view.roots[domain.AreaLive]
	parent.EntryCount, parent.Entry.Size, parent.Entry.FileCount = 1, 1, 0
	if _, err := store.applyDirectoryEdits(ctx, view, parent, []namespaceDirectoryEdit{{before: &before}}, engine.clock.Now()); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("file-count underflow error = %v", err)
	}
	parent = view.roots[domain.AreaLive]
	parent.Entry.FileCount = math.MaxInt64
	if _, err := store.applyDirectoryEdits(ctx, view, parent, []namespaceDirectoryEdit{{after: &valid}}, engine.clock.Now()); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("file-count overflow error = %v", err)
	}
	invalid := storageformat.NamespaceEntry{}
	if _, err := store.applyDirectoryEdits(ctx, view, view.roots[domain.AreaLive], []namespaceDirectoryEdit{{after: &invalid}}, engine.clock.Now()); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid child encoding error = %v", err)
	}
}

func TestSchema008NamespaceViewOutcomeAndCommitFailClosed(t *testing.T) {
	ctx := context.Background()
	live := namespaceTestScope(t, domain.AreaLive)

	t.Run("missing-snapshot", func(t *testing.T) {
		store := newNamespaceStore(openNamespaceTestEngine(t, objectmemory.New()))
		if _, err := store.loadView(ctx, live.UserID(), storageformat.Digest([]byte("missing"))); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing snapshot error = %v", err)
		}
	})

	t.Run("corrupt-root-value", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		store := newNamespaceStore(engine)
		if _, err := store.domain.mutate(ctx, namespaceReference(live.UserID()), consistencyDomainMutation{ID: "corrupt-root", Changes: []consistencyDomainChange{{Key: namespaceRootKey(domain.AreaLive), Require: domainValueAbsent, Value: []byte("not canonical")}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.loadView(ctx, live.UserID(), ""); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("corrupt root error = %v", err)
		}
	})

	t.Run("invalid-outcome-and-idempotency-conflict", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		store := newNamespaceStore(engine)
		if _, err := store.domain.mutate(ctx, namespaceReference(live.UserID()), consistencyDomainMutation{ID: "bad-outcome", Changes: []consistencyDomainChange{{Key: "opaque", Require: domainValueAbsent, Value: []byte("value")}}, Result: []byte("bad")}); err != nil {
			t.Fatal(err)
		}
		view, err := store.loadView(ctx, live.UserID(), "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.operationReplay(ctx, view, "bad-outcome", "fingerprint"); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid outcome error = %v", err)
		}
		for name, corrupt := range map[string]storageformat.NamespaceMutationResult{
			"operation": {SchemaVersion: 1, RequestFingerprint: storageformat.Digest([]byte("operation")), Operation: &domain.Operation{ID: "corrupt", State: domain.OperationSucceeded}},
			"entry":     {SchemaVersion: 1, RequestFingerprint: storageformat.Digest([]byte("entry")), Entry: &storageformat.DirectoryEntry{Name: "corrupt"}},
		} {
			body, encodeErr := storageformat.EncodeCanonical(corrupt)
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			mutationID := "corrupt-" + name
			if _, mutateErr := store.domain.mutate(ctx, namespaceReference(live.UserID()), consistencyDomainMutation{ID: mutationID, Changes: []consistencyDomainChange{{Key: "opaque-" + name, Require: domainValueAbsent, Value: []byte("value")}}, Result: body}); mutateErr != nil {
				t.Fatal(mutateErr)
			}
			corruptView, loadErr := store.loadView(ctx, live.UserID(), "")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if _, replayErr := store.operationReplay(ctx, corruptView, mutationID, ""); !errors.Is(replayErr, domain.ErrInvalid) {
				t.Fatalf("corrupt %s replay error = %v", name, replayErr)
			}
		}

		operation := domain.Operation{ID: "operation", State: domain.OperationSucceeded, StartedAt: engine.clock.Now(), UpdatedAt: engine.clock.Now()}
		firstFingerprint := storageformat.Digest([]byte("first"))
		result := storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: firstFingerprint, Operation: &operation}
		body, err := storageformat.EncodeCanonical(result)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.domain.mutate(ctx, namespaceReference(live.UserID()), consistencyDomainMutation{ID: "bound-outcome", Changes: []consistencyDomainChange{{Key: "opaque-two", Require: domainValueAbsent, Value: []byte("value")}}, Result: body}); err != nil {
			t.Fatal(err)
		}
		view, err = store.loadView(ctx, live.UserID(), "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.operationReplay(ctx, view, "bound-outcome", storageformat.Digest([]byte("second"))); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("reused idempotency error = %v", err)
		}
	})

	t.Run("invalid-root-change-and-provider-failure", func(t *testing.T) {
		memory := objectmemory.New()
		hooks := &hookedBackend{Backend: memory}
		engine := openNamespaceTestEngine(t, hooks)
		store := newNamespaceStore(engine)
		view, err := store.loadView(ctx, live.UserID(), "")
		if err != nil {
			t.Fatal(err)
		}
		invalid := storageformat.NamespaceEntry{}
		if _, err := store.commit(ctx, view, "invalid-root", "fingerprint", map[string]storageformat.NamespaceEntry{namespaceFrameKey(domain.AreaLive, namespaceRootPath()): invalid}, storageformat.NamespaceMutationResult{}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid root commit error = %v", err)
		}
		child := validNamespaceTestFile(t, engine, "commit-child", 1)
		valid, err := store.applyDirectoryEdits(ctx, view, view.roots[domain.AreaLive], []namespaceDirectoryEdit{{after: &child}}, engine.clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		invalidResult := storageformat.NamespaceMutationResult{Operation: &domain.Operation{ID: "invalid-result", State: domain.OperationSucceeded}}
		if _, err := store.commit(ctx, view, "invalid-result", storageformat.Digest([]byte("invalid-result")), map[string]storageformat.NamespaceEntry{namespaceFrameKey(domain.AreaLive, namespaceRootPath()): valid}, invalidResult); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid outcome commit error = %v", err)
		}
		hooks.put = func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return "", domain.NewError(domain.ErrorUnavailable, "publication failed")
		}
		operation := domain.Operation{ID: "provider-failure", State: domain.OperationSucceeded, StartedAt: engine.clock.Now(), UpdatedAt: engine.clock.Now()}
		if _, err := store.commit(ctx, view, "provider-failure", storageformat.Digest([]byte("provider-failure")), map[string]storageformat.NamespaceEntry{namespaceFrameKey(domain.AreaLive, namespaceRootPath()): valid}, storageformat.NamespaceMutationResult{Operation: &operation}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("provider failure error = %v", err)
		}
	})
}

func TestSchema008NamespaceMutationInputAndDestinationDenials(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	store := newNamespaceStore(engine)
	files := engine.Files()
	live := namespaceTestScope(t, domain.AreaLive)
	trash := namespaceTestScope(t, domain.AreaTrash)
	seeded := publishNamespaceTestFile(t, store, live, "/existing.txt", 1, "existing")
	if _, err := store.createDirectory(ctx, live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/folder")}); err != nil {
		t.Fatal(err)
	}
	view, err := store.loadView(ctx, live.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	parent := view.roots[domain.AreaLive]
	if _, _, err := store.resolveDestination(ctx, view, parent, seeded.Path, domain.ConflictFail, ""); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("conflict-fail error = %v", err)
	}
	if _, _, err := store.resolveDestination(ctx, view, parent, seeded.Path, domain.ConflictReplace, ""); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("replace without version error = %v", err)
	}
	if resolved, existing, err := store.resolveDestination(ctx, view, parent, seeded.Path, domain.ConflictReplace, seeded.Version); err != nil || resolved != seeded.Path || existing == nil {
		t.Fatalf("valid replace = %s, %+v, %v", resolved.String(), existing, err)
	}
	if _, _, err := store.resolveDestination(ctx, view, parent, seeded.Path, "invalid", ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid conflict error = %v", err)
	}
	if renamed, existing, err := store.resolveDestination(ctx, view, parent, seeded.Path, domain.ConflictRename, ""); err != nil || renamed == seeded.Path || existing != nil {
		t.Fatalf("rename resolution = %s, %+v, %v", renamed.String(), existing, err)
	}
	longName := strings.Repeat("a", 250) + ".txt"
	longFile := publishNamespaceTestFile(t, store, live, "/"+longName, 1, "long")
	view, err = store.loadView(ctx, live.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	if renamed, _, err := store.resolveDestination(ctx, view, view.roots[domain.AreaLive], longFile.Path, domain.ConflictRename, ""); err != nil || len(renamed.Name()) > 255 {
		t.Fatalf("bounded rename = %q, %v", renamed.Name(), err)
	}
	if _, _, err := store.prepareDestinationAtView(ctx, view, live, domain.MustParseUserPath("/missing/target"), domain.ConflictFail, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing upload parent error = %v", err)
	}

	if _, err := store.publishFileWithChanges(ctx, live, domain.UserPath{}, domain.ConflictFail, "", "id", "fingerprint", storageformat.DirectoryEntry{}, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid publication error = %v", err)
	}
	otherOwner, _ := domain.ParseUserID("WFhYWFhYWFhYWFhYWFhYWA")
	other, _ := domain.NewScope(otherOwner, domain.AreaLive)
	if _, err := store.copyOrMove(ctx, true, live, other, domain.CopyRequest{Source: seeded.Path, Destination: domain.MustParseUserPath("/other")}); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("cross-owner move error = %v", err)
	}
	if _, err := store.copyOrMove(ctx, true, live, live, domain.CopyRequest{Source: seeded.Path, Destination: domain.MustParseUserPath("/folder/child"), Conflict: "invalid"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid move conflict error = %v", err)
	}
	if _, err := store.copyOrMove(ctx, true, live, live, domain.CopyRequest{Source: domain.MustParseUserPath("/folder"), Destination: domain.MustParseUserPath("/folder/child")}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("descendant move error = %v", err)
	}
	if _, err := store.copyOrMove(ctx, true, live, live, domain.CopyRequest{Source: domain.MustParseUserPath("/missing"), Destination: domain.MustParseUserPath("/target"), IdempotencyKey: "missing-source"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing source error = %v", err)
	}
	if _, err := store.copyOrMove(ctx, true, live, trash, domain.CopyRequest{Source: seeded.Path, Destination: domain.MustParseUserPath("/trash"), ExpectedSource: "stale", IdempotencyKey: "stale-source"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale source error = %v", err)
	}
	if _, err := files.RestoreFromTrash(ctx, live.UserID(), "", domain.ConflictFail, "restore"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty restore error = %v", err)
	}
	if _, err := files.DeleteFromTrash(ctx, live.UserID(), "", "delete"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty trash deletion error = %v", err)
	}
	if _, err := files.MoveToTrash(ctx, live.UserID(), domain.TrashRequest{Path: seeded.Path, TrashID: "nested/id"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("nested trash ID error = %v", err)
	}
	if _, err := namespaceTrashEntry(live.UserID(), domain.MustParseUserPath("/trash"), storageformat.NamespaceEntry{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("missing trash metadata error = %v", err)
	}
	brokenTrash := validNamespaceTestFile(t, engine, "trash", 1)
	brokenTrash.Trash = &storageformat.NamespaceTrashMetadata{OriginalPath: "/", OriginalVersion: "version", TrashedAt: engine.clock.Now()}
	if _, err := namespaceTrashEntry(live.UserID(), domain.MustParseUserPath("/trash"), brokenTrash); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid original trash path error = %v", err)
	}
	if page, err := files.ListTrash(ctx, live.UserID(), domain.TrashListRequest{}); err != nil || len(page.Items) != 0 {
		t.Fatalf("empty trash page = %+v, %v", page, err)
	}
}

func TestSchema008NamespaceBatchStoredOutcomeDenialMatrix(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	view, err := store.loadView(ctx, live.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	operation := domain.Operation{ID: "batch-operation", State: domain.OperationSucceeded, StartedAt: engine.clock.Now(), UpdatedAt: engine.clock.Now()}
	if _, err := store.decodeBatchResult(ctx, view, storageformat.NamespaceBatch{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty batch error = %v", err)
	}
	if _, err := store.decodeBatchResult(ctx, view, storageformat.NamespaceBatch{Operation: operation, ItemCount: 1, Items: storageformat.DomainTreeRoot{EntryCount: 2}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("count-binding error = %v", err)
	}
	if _, err := store.decodeBatchResult(ctx, view, storageformat.NamespaceBatch{Operation: operation, ItemCount: maximumNamespaceBatchItems + 1, Items: storageformat.DomainTreeRoot{EntryCount: maximumNamespaceBatchItems + 1}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized stored batch error = %v", err)
	}

	rootFor := func(t *testing.T, key string, stored storageformat.NamespaceBatchItem) storageformat.DomainTreeRoot {
		t.Helper()
		body, err := storageformat.EncodeCanonical(stored)
		if err != nil {
			t.Fatal(err)
		}
		root, err := view.session.buildTree(ctx, []storageformat.DomainEntry{{Key: key, Value: body, LogicalVersion: storageformat.Digest(body)}})
		if err != nil {
			t.Fatal(err)
		}
		return root
	}
	valid := storageformat.NamespaceBatchItem{Index: 0, Source: "/source", Destination: "/destination", OperationID: operation.ID, State: domain.OperationSucceeded}
	if _, err := store.decodeBatchResult(ctx, view, storageformat.NamespaceBatch{Operation: operation, ItemCount: 1, Items: rootFor(t, "0000000000000001", valid)}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("misordered item error = %v", err)
	}
	wrongOperation := valid
	wrongOperation.OperationID = "other-operation"
	if _, err := store.decodeBatchResult(ctx, view, storageformat.NamespaceBatch{Operation: operation, ItemCount: 1, Items: rootFor(t, "0000000000000000", wrongOperation)}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("operation binding error = %v", err)
	}
	badSource := valid
	badSource.Source = "relative"
	if _, err := store.decodeBatchResult(ctx, view, storageformat.NamespaceBatch{Operation: operation, ItemCount: 1, Items: rootFor(t, "0000000000000000", badSource)}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid source error = %v", err)
	}
	badDestination := valid
	badDestination.Destination = "relative"
	if _, err := store.decodeBatchResult(ctx, view, storageformat.NamespaceBatch{Operation: operation, ItemCount: 1, Items: rootFor(t, "0000000000000000", badDestination)}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid destination error = %v", err)
	}

	simple := storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: storageformat.Digest([]byte("simple")), Operation: &operation}
	body, err := storageformat.EncodeCanonical(simple)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.domain.mutate(ctx, namespaceReference(live.UserID()), consistencyDomainMutation{ID: "simple-outcome", Changes: []consistencyDomainChange{{Key: "batch-test", Require: domainValueAbsent, Value: []byte("value")}}, Result: body}); err != nil {
		t.Fatal(err)
	}
	view, err = store.loadView(ctx, live.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.batchReplay(ctx, view, "simple-outcome", ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("non-batch replay error = %v", err)
	}
	if _, err := engine.Files().GetBatchOperation(ctx, live.UserID(), "missing-batch"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing batch operation error = %v", err)
	}
}

func TestSchema008NamespaceBatchRequestDenialMatrix(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	seeded := seedNamespaceBatchFiles(t, store, live, 2)
	otherOwner, _ := domain.ParseUserID("WFhYWFhYWFhYWFhYWFhYWA")
	other, _ := domain.NewScope(otherOwner, domain.AreaLive)

	if _, err := store.batchCopyOrMove(ctx, live.UserID(), []namespaceBatchMoveSpec{{from: other, to: other, request: domain.CopyRequest{Source: seeded[0].Path, Destination: domain.MustParseUserPath("/target")}}}, true, "batch", "owner-mismatch"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("misbound owner error = %v", err)
	}
	if _, err := store.batchCopyOrMove(ctx, live.UserID(), []namespaceBatchMoveSpec{{from: live, to: live, request: domain.CopyRequest{Source: seeded[0].Path, Destination: domain.MustParseUserPath("/target")}}}, true, "batch", strings.Repeat("x", 129)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("idempotency error = %v", err)
	}
	if _, err := store.batchCopyOrMove(ctx, live.UserID(), []namespaceBatchMoveSpec{{from: live, to: live, request: domain.CopyRequest{Source: seeded[0].Path, Destination: domain.MustParseUserPath("/target"), Conflict: "invalid"}}}, true, "batch", "invalid-conflict"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("conflict mode error = %v", err)
	}
	if _, err := store.batchCopyOrMove(ctx, live.UserID(), []namespaceBatchMoveSpec{{from: live, to: live, request: domain.CopyRequest{Source: seeded[0].Path, Destination: domain.MustParseUserPath("/missing/target")}}}, true, "batch", "missing-parent"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing destination parent error = %v", err)
	}
	if _, err := store.batchCopyOrMove(ctx, live.UserID(), []namespaceBatchMoveSpec{{from: live, to: live, request: domain.CopyRequest{Source: domain.MustParseUserPath("/missing/source"), Destination: domain.MustParseUserPath("/target")}}}, true, "batch", "missing-source-parent"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing source parent error = %v", err)
	}
	if _, err := store.batchCopyOrMove(ctx, live.UserID(), []namespaceBatchMoveSpec{{from: live, to: live, request: domain.CopyRequest{Source: seeded[0].Path, Destination: domain.MustParseUserPath("/target"), ExpectedSource: "stale"}}}, true, "batch", "stale-source"); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale source error = %v", err)
	}
	if _, err := store.batchCopyOrMove(ctx, live.UserID(), []namespaceBatchMoveSpec{{from: live, to: live, request: domain.CopyRequest{Source: seeded[0].Path, Destination: seeded[1].Path, Conflict: domain.ConflictFail}}}, false, "batch", "destination-conflict"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("destination conflict error = %v", err)
	}
	trash, _ := domain.NewScope(live.UserID(), domain.AreaTrash)
	if _, err := store.createDirectory(ctx, trash, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/nested")}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.batchCopyOrMove(ctx, live.UserID(), []namespaceBatchMoveSpec{{from: live, to: trash, attachTrash: true, trashID: "nested", request: domain.CopyRequest{Source: seeded[0].Path, Destination: domain.MustParseUserPath("/nested/target")}}}, true, "batch", "nested-trash"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("nested trash destination error = %v", err)
	}
	if _, err := engine.Files().BatchDeleteFromTrash(ctx, live.UserID(), []string{"nested/id"}, "invalid-trash-id"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid delete trash identity error = %v", err)
	}
	if _, err := engine.Files().BatchDeleteFromTrash(ctx, live.UserID(), []string{"missing"}, strings.Repeat("x", 129)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("batch delete idempotency error = %v", err)
	}
}

func TestSchema008NamespaceReadCursorAndProjectionFailureMatrix(t *testing.T) {
	ctx := context.Background()
	live := namespaceTestScope(t, domain.AreaLive)

	t.Run("cursor-authentication-and-schema", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		store := newNamespaceStore(engine)
		nonce := bytes.Repeat([]byte{1}, engine.cursorAEAD.NonceSize())
		sealed := engine.cursorAEAD.Seal(append([]byte(nil), nonce...), nonce, []byte("not-json"), []byte("endlessfs-namespace-cursor-v1"))
		if _, err := store.decodeListCursor(base64.RawURLEncoding.EncodeToString(sealed)); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid cursor body error = %v", err)
		}
		sealed[len(sealed)-1] ^= 1
		if _, err := store.decodeListCursor(base64.RawURLEncoding.EncodeToString(sealed)); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("tampered cursor error = %v", err)
		}
	})

	t.Run("cursor-randomness-failure", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		store := newNamespaceStore(engine)
		seedNamespaceBatchFiles(t, store, live, 2)
		engine.ids = domain.NewIDGenerator(bytes.NewReader(nil))
		if _, err := store.list(ctx, live, domain.ListRequest{Directory: namespaceRootPath(), PageSize: 1}); err == nil {
			t.Fatal("list succeeded without cursor randomness")
		}
	})

	t.Run("projection-head-read-failure", func(t *testing.T) {
		memory := objectmemory.New()
		hooks := &hookedBackend{Backend: memory}
		engine := openNamespaceTestEngine(t, hooks)
		store := newNamespaceStore(engine)
		seedNamespaceBatchFiles(t, store, live, 2)
		view, err := store.loadView(ctx, live.UserID(), "")
		if err != nil {
			t.Fatal(err)
		}
		projectionKey := storageformat.ScopedProjectionHeadKey(live.UserID().String(), storageformat.ProjectionSize, namespaceProjectionID(live.UserID(), domain.AreaLive, view.roots[domain.AreaLive], domain.SortSize))
		hooks.get = func(callCtx context.Context, key objectstore.Key) (objectstore.Object, error) {
			if key == projectionKey {
				return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "projection unavailable")
			}
			return memory.Get(callCtx, key)
		}
		if _, err := store.list(ctx, live, domain.ListRequest{Directory: namespaceRootPath(), Sort: domain.SortSize}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("projection list error = %v", err)
		}
	})

	t.Run("corrupt-child-body", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		store := newNamespaceStore(engine)
		view, err := store.loadView(ctx, live.UserID(), "")
		if err != nil {
			t.Fatal(err)
		}
		metadata := validNamespaceTestFile(t, engine, "corrupt", 0).Entry
		children, err := view.session.buildTree(ctx, []storageformat.DomainEntry{{Key: metadata.Name, Value: []byte("bad"), LogicalVersion: metadata.LogicalVersion}})
		if err != nil {
			t.Fatal(err)
		}
		accumulator, digest, err := directoryContentIdentity([]storageformat.DirectoryEntry{metadata})
		if err != nil {
			t.Fatal(err)
		}
		root := view.roots[domain.AreaLive]
		root.Children, root.EntryCount, root.Entry.FileCount = children, 1, 1
		root.ContentAccumulator, root.Entry.ContentDigest = accumulator, digest
		root.Entry.LogicalVersion, err = directoryEntryVersion(root.Entry)
		if err != nil {
			t.Fatal(err)
		}
		body, err := encodeNamespaceEntry(root)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.domain.mutate(ctx, view.reference, consistencyDomainMutation{ID: "corrupt-child", Changes: []consistencyDomainChange{{Key: namespaceRootKey(domain.AreaLive), Require: domainValueAbsent, Value: body}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.list(ctx, live, domain.ListRequest{Directory: namespaceRootPath()}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("corrupt child list error = %v", err)
		}
	})
}

func TestSchema008NamespaceTrashPlacementRestoreAndDeleteDenials(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	trash := namespaceTestScope(t, domain.AreaTrash)
	if _, err := store.createDirectory(ctx, trash, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/nested")}); err != nil {
		t.Fatal(err)
	}
	liveFile := publishNamespaceTestFile(t, store, live, "/live.bin", 1, "live-placement")
	if _, err := store.copyOrMove(ctx, true, live, trash, domain.CopyRequest{Source: liveFile.Path, Destination: domain.MustParseUserPath("/nested/live.bin"), IdempotencyKey: "nested-trash-placement"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("nested trash placement error = %v", err)
	}

	plainTrash := publishNamespaceTestFile(t, store, trash, "/plain.bin", 1, "plain-trash")
	if _, err := store.restoreFromTrash(ctx, trash, live, domain.CopyRequest{Source: plainTrash.Path, IdempotencyKey: "missing-trash-metadata"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("restore without metadata error = %v", err)
	}
	if _, err := store.deleteFromTrash(ctx, trash, domain.DeleteRequest{Path: plainTrash.Path, IdempotencyKey: "delete-missing-trash-metadata"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("delete without trash metadata error = %v", err)
	}
	if _, err := store.delete(ctx, trash, domain.DeleteRequest{Path: plainTrash.Path, ExpectedVersion: "stale", IdempotencyKey: "stale-delete"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale delete error = %v", err)
	}
	if _, err := store.delete(ctx, trash, domain.DeleteRequest{Path: plainTrash.Path, IdempotencyKey: strings.Repeat("x", 129)}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("delete idempotency error = %v", err)
	}
}

func TestSchema008NamespaceBatchConstructionAndReplayProviderDenials(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	if _, err := store.batchCopyOrMove(ctx, live.UserID(), nil, true, "", "key"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty kind error = %v", err)
	}
	if _, err := store.batchCopyOrMove(ctx, live.UserID(), nil, true, "batch", "key"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty batch error = %v", err)
	}
	invalidItem := domain.NamespaceBatchItemResult{State: domain.OperationSucceeded}
	view, err := store.loadView(ctx, live.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.writeBatchItems(ctx, view, []domain.NamespaceBatchItemResult{invalidItem}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid batch item error = %v", err)
	}
	if _, err := engine.Files().GetBatchOperation(ctx, domain.UserID{}, "operation"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid batch owner error = %v", err)
	}
}

func TestSchema008NamespaceCrossParentReplacementAndReplayPaths(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	for _, path := range []string{"/source", "/destination", "/source/nested"} {
		if _, err := store.createDirectory(ctx, live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(path)}); err != nil {
			t.Fatal(err)
		}
	}
	first := publishNamespaceTestFile(t, store, live, "/source/first.bin", 1, "cross-first")
	second := publishNamespaceTestFile(t, store, live, "/source/second.bin", 2, "cross-second")
	existing := publishNamespaceTestFile(t, store, live, "/destination/existing.bin", 3, "cross-existing")

	if _, err := store.copyOrMove(ctx, false, live, live, domain.CopyRequest{Source: first.Path, Destination: domain.MustParseUserPath("/destination/copied.bin"), IdempotencyKey: "cross-copy"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.copyOrMove(ctx, true, live, live, domain.CopyRequest{Source: second.Path, Destination: domain.MustParseUserPath("/destination/moved.bin"), IdempotencyKey: "cross-move"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.copyOrMove(ctx, false, live, live, domain.CopyRequest{Source: first.Path, Destination: existing.Path, Conflict: domain.ConflictReplace, ExpectedTarget: existing.Version, IdempotencyKey: "cross-replace"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.delete(ctx, live, domain.DeleteRequest{Path: domain.MustParseUserPath("/destination/moved.bin"), IdempotencyKey: "cross-delete"}); err != nil {
		t.Fatal(err)
	}

	directory, err := store.createDirectory(ctx, live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/replace-directory")})
	if err != nil {
		t.Fatal(err)
	}
	if renamed, err := store.createDirectory(ctx, live, domain.CreateDirectoryRequest{Path: directory.Path, Conflict: domain.ConflictRename}); err != nil || renamed.Path == directory.Path {
		t.Fatalf("directory rename = %+v, %v", renamed, err)
	}
	if _, err := store.createDirectory(ctx, live, domain.CreateDirectoryRequest{Path: directory.Path, Conflict: domain.ConflictReplace, ExpectedVersion: directory.Version}); err != nil {
		t.Fatalf("directory replacement error = %v", err)
	}

	requests := []domain.CopyRequest{{Source: first.Path, Destination: domain.MustParseUserPath("/destination/batch-copy.bin")}}
	batch, err := engine.Files().BatchCopyMove(ctx, live.UserID(), requests, false, "batch-operation-lookup")
	if err != nil {
		t.Fatal(err)
	}
	operation, err := engine.Files().GetOperation(ctx, live.UserID(), batch.Operation.ID)
	if err != nil || operation.ID != batch.Operation.ID {
		t.Fatalf("batch operation lookup = %+v, %v", operation, err)
	}
	if replay, err := engine.Files().BatchCopyMove(ctx, live.UserID(), requests, false, "batch-operation-lookup"); err != nil || replay.Operation.ID != batch.Operation.ID {
		t.Fatalf("batch replay = %+v, %v", replay, err)
	}
}

func TestSchema008NamespaceMutationEntropyFailures(t *testing.T) {
	ctx := context.Background()
	live := namespaceTestScope(t, domain.AreaLive)
	for name, run := range map[string]func(*namespaceStore) error{
		"create-directory": func(store *namespaceStore) error {
			_, err := store.createDirectory(ctx, live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/new")})
			return err
		},
		"copy": func(store *namespaceStore) error {
			_, err := store.copyOrMove(ctx, false, live, live, domain.CopyRequest{Source: domain.MustParseUserPath("/source"), Destination: domain.MustParseUserPath("/copy")})
			return err
		},
		"delete": func(store *namespaceStore) error {
			_, err := store.delete(ctx, live, domain.DeleteRequest{Path: domain.MustParseUserPath("/source")})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			engine := openNamespaceTestEngine(t, objectmemory.New())
			store := newNamespaceStore(engine)
			engine.ids = domain.NewIDGenerator(bytes.NewReader(nil))
			if err := run(store); err == nil {
				t.Fatal("mutation succeeded without entropy")
			}
		})
	}
}

func TestSchema008CreateDirectoryMutationIDCollisionFailsWithoutRetryLoop(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	engine := openInternalTestEngine(t, backend, domain.NewFixedClock(time.Date(2046, 1, 2, 3, 4, 5, 0, time.UTC)), strings.NewReader(strings.Repeat("x", 1<<20)))
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	first, err := store.createDirectory(ctx, live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/first")})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.createDirectory(ctx, live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/first")})
	if err != nil || replayed.Path != first.Path || replayed.Version != first.Version {
		t.Fatalf("colliding same-intent replay = %+v, %v; first=%+v", replayed, err, first)
	}
	if _, err := store.createDirectory(ctx, live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/second")}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("colliding mutation ID error = %v; want durable idempotency conflict", err)
	}
}

func TestSchema008TrashListProviderAndCursorFailureMatrix(t *testing.T) {
	ctx := context.Background()
	live := namespaceTestScope(t, domain.AreaLive)
	setup := func(t *testing.T) (*objectmemory.Backend, *hookedBackend, *Engine) {
		t.Helper()
		memory := objectmemory.New()
		hooks := &hookedBackend{Backend: memory}
		engine := openNamespaceTestEngine(t, hooks)
		for index := 0; index < 2; index++ {
			path := domain.MustParseUserPath(fmt.Sprintf("/trash-source-%d", index))
			entry, err := engine.Files().CreateDirectory(ctx, live, domain.CreateDirectoryRequest{Path: path})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := engine.Files().MoveToTrash(ctx, live.UserID(), domain.TrashRequest{Path: path, TrashID: fmt.Sprintf("trash-%d", index), ExpectedVersion: entry.Version, IdempotencyKey: fmt.Sprintf("trash-list-%d", index)}); err != nil {
				t.Fatal(err)
			}
		}
		return memory, hooks, engine
	}

	t.Run("view-read", func(t *testing.T) {
		memory, hooks, engine := setup(t)
		hooks.get = func(callCtx context.Context, key objectstore.Key) (objectstore.Object, error) {
			if key == storageformat.DomainHeadKey(storageformat.DomainNamespace, live.UserID().String()) {
				return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "namespace head unavailable")
			}
			return memory.Get(callCtx, key)
		}
		if _, err := engine.Files().ListTrash(ctx, live.UserID(), domain.TrashListRequest{}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("trash view error = %v", err)
		}
	})

	t.Run("projection-read", func(t *testing.T) {
		memory, hooks, engine := setup(t)
		hooks.get = func(callCtx context.Context, key objectstore.Key) (objectstore.Object, error) {
			if strings.Contains(key.String(), "/projections/") && strings.Contains(key.String(), "/heads/") {
				return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "projection unavailable")
			}
			return memory.Get(callCtx, key)
		}
		if _, err := engine.Files().ListTrash(ctx, live.UserID(), domain.TrashListRequest{}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("trash projection error = %v", err)
		}
	})

	t.Run("snapshot-write", func(t *testing.T) {
		memory, hooks, engine := setup(t)
		hooks.put = func(callCtx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if strings.Contains(key.String(), "/snapshots/") {
				return "", domain.NewError(domain.ErrorUnavailable, "snapshot write failed")
			}
			return memory.Put(callCtx, key, body, condition)
		}
		if _, err := engine.Files().ListTrash(ctx, live.UserID(), domain.TrashListRequest{Limit: 1}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("trash snapshot error = %v", err)
		}
	})

	t.Run("cursor-randomness", func(t *testing.T) {
		_, _, engine := setup(t)
		engine.ids = domain.NewIDGenerator(bytes.NewReader(nil))
		if _, err := engine.Files().ListTrash(ctx, live.UserID(), domain.TrashListRequest{Limit: 1}); err == nil {
			t.Fatal("trash cursor succeeded without entropy")
		}
	})
}
