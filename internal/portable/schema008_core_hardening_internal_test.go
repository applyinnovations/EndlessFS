package portable

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

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
