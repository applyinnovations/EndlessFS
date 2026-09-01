package memory

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func TestNamespaceExtensionsAreAtomicScopedAndIdempotent(t *testing.T) {
	provider, live, _ := boundaryProvider(t)
	ctx := context.Background()
	owner := live.UserID()
	for _, name := range []string{"one", "two", "copy-one", "copy-two"} {
		if _, err := provider.CreateDirectory(ctx, live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/" + name)}); err != nil {
			t.Fatal(err)
		}
	}

	requests := []domain.TrashRequest{
		{Path: domain.MustParseUserPath("/one"), TrashID: "trash-one"},
		{Path: domain.MustParseUserPath("/two"), TrashID: "trash-two"},
	}
	batch, err := provider.BatchMoveToTrash(ctx, owner, requests, "trash-batch")
	if err != nil || len(batch.Items) != 2 || batch.Operation.State != domain.OperationSucceeded {
		t.Fatalf("trash batch = %+v, %v", batch, err)
	}
	if replay, err := provider.BatchMoveToTrash(ctx, owner, requests, "trash-batch"); err != nil || replay.Operation.ID != batch.Operation.ID {
		t.Fatalf("trash batch replay = %+v, %v", replay, err)
	}
	changed := append([]domain.TrashRequest(nil), requests...)
	changed[0].TrashID = "different"
	if _, err := provider.BatchMoveToTrash(ctx, owner, changed, "trash-batch"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("trash batch key reuse = %v", err)
	}

	first, err := provider.ListTrash(ctx, owner, domain.TrashListRequest{Limit: 1})
	if err != nil || len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatalf("first trash page = %+v, %v", first, err)
	}
	if _, err := provider.ListTrash(ctx, owner, domain.TrashListRequest{Limit: 2, Cursor: first.NextCursor}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("cursor limit binding = %v", err)
	}
	second, err := provider.ListTrash(ctx, owner, domain.TrashListRequest{Limit: 1, Cursor: first.NextCursor})
	if err != nil || len(second.Items) != 1 || second.NextCursor != "" {
		t.Fatalf("second trash page = %+v, %v", second, err)
	}
	if _, err := provider.ListTrash(ctx, owner, domain.TrashListRequest{Cursor: "missing"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("missing cursor = %v", err)
	}
	if _, err := provider.ListTrash(ctx, domain.UserID{}, domain.TrashListRequest{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid owner = %v", err)
	}
	if _, err := provider.ListTrash(ctx, owner, domain.TrashListRequest{Limit: 1001}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid limit = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := provider.ListTrash(canceled, owner, domain.TrashListRequest{}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled list = %v", err)
	}

	restored, err := provider.RestoreFromTrash(ctx, owner, "trash-one", domain.ConflictFail, "restore-one")
	if err != nil || restored.State != domain.OperationSucceeded {
		t.Fatalf("restore = %+v, %v", restored, err)
	}
	if replay, err := provider.RestoreFromTrash(ctx, owner, "trash-one", domain.ConflictFail, "restore-one"); err != nil || replay.ID != restored.ID {
		t.Fatalf("restore replay = %+v, %v", replay, err)
	}
	if _, err := provider.RestoreFromTrash(ctx, owner, "trash-one", domain.ConflictRename, "restore-one"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("restore key reuse = %v", err)
	}
	deleted, err := provider.DeleteFromTrash(ctx, owner, "trash-two", "delete-two")
	if err != nil || deleted.State != domain.OperationSucceeded {
		t.Fatalf("delete = %+v, %v", deleted, err)
	}
	if replay, err := provider.DeleteFromTrash(ctx, owner, "trash-two", "delete-two"); err != nil || replay.ID != deleted.ID {
		t.Fatalf("delete replay = %+v, %v", replay, err)
	}
	if _, err := provider.DeleteFromTrash(ctx, owner, "different", "delete-two"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("delete key reuse = %v", err)
	}

	copies := []domain.CopyRequest{
		{Source: domain.MustParseUserPath("/copy-one"), Destination: domain.MustParseUserPath("/copy-one-target")},
		{Source: domain.MustParseUserPath("/copy-two"), Destination: domain.MustParseUserPath("/copy-two-target")},
	}
	copyBatch, err := provider.BatchCopyMove(ctx, owner, copies, false, "copy-batch")
	if err != nil || len(copyBatch.Items) != 2 {
		t.Fatalf("copy batch = %+v, %v", copyBatch, err)
	}
	if replay, err := provider.BatchCopyMove(ctx, owner, copies, false, "copy-batch"); err != nil || replay.Operation.ID != copyBatch.Operation.ID {
		t.Fatalf("copy replay = %+v, %v", replay, err)
	}
	copies[0].Destination = domain.MustParseUserPath("/changed")
	if _, err := provider.BatchCopyMove(ctx, owner, copies, false, "copy-batch"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("copy key reuse = %v", err)
	}

	atomic := []domain.CopyRequest{
		{Source: domain.MustParseUserPath("/copy-one"), Destination: domain.MustParseUserPath("/atomic-target")},
		{Source: domain.MustParseUserPath("/missing"), Destination: domain.MustParseUserPath("/never")},
	}
	if _, err := provider.BatchCopyMove(ctx, owner, atomic, true, "atomic-failure"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("atomic batch failure = %v", err)
	}
	if _, err := provider.Stat(ctx, live, domain.MustParseUserPath("/copy-one")); err != nil {
		t.Fatalf("atomic rollback lost source: %v", err)
	}
}

func TestNamespaceExtensionDenialMatrix(t *testing.T) {
	provider, live, _ := boundaryProvider(t)
	ctx := context.Background()
	owner := live.UserID()
	root := domain.MustParseUserPath("/")
	invalidTrash := []domain.TrashRequest{{}, {Path: root, TrashID: "root"}, {Path: domain.MustParseUserPath("/missing"), TrashID: "nested/id"}, {Path: domain.MustParseUserPath("/missing"), TrashID: "id", IdempotencyKey: strings.Repeat("x", 129)}}
	for index, request := range invalidTrash {
		if _, err := provider.MoveToTrash(ctx, owner, request); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid trash %d = %v", index, err)
		}
	}
	if _, err := provider.MoveToTrash(ctx, domain.UserID{}, domain.TrashRequest{Path: domain.MustParseUserPath("/missing"), TrashID: "id"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid trash owner = %v", err)
	}
	for _, call := range []func() error{
		func() error {
			_, err := provider.RestoreFromTrash(ctx, owner, "", domain.ConflictFail, "key")
			return err
		},
		func() error { _, err := provider.DeleteFromTrash(ctx, owner, "nested/id", "key"); return err },
		func() error { _, err := provider.BatchCopyMove(ctx, owner, nil, false, "key"); return err },
		func() error { _, err := provider.BatchMoveToTrash(ctx, owner, nil, "key"); return err },
		func() error {
			_, err := provider.BatchRestoreFromTrash(ctx, owner, nil, domain.ConflictFail, "key")
			return err
		},
		func() error {
			_, err := provider.BatchRestoreFromTrash(ctx, owner, []string{"nested/id"}, domain.ConflictFail, "key")
			return err
		},
		func() error {
			_, err := provider.BatchRestoreFromTrash(ctx, owner, []string{"same", "same"}, domain.ConflictFail, "key")
			return err
		},
		func() error { _, err := provider.BatchDeleteFromTrash(ctx, owner, nil, "key"); return err },
		func() error {
			_, err := provider.BatchDeleteFromTrash(ctx, owner, []string{"nested/id"}, "key")
			return err
		},
		func() error {
			_, err := provider.BatchDeleteFromTrash(ctx, owner, []string{"same", "same"}, "key")
			return err
		},
	} {
		if err := call(); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("denial error = %v", err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := provider.RestoreFromTrash(canceled, owner, "missing", domain.ConflictFail, "key"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled restore = %v", err)
	}
	if _, err := provider.BatchRestoreFromTrash(canceled, owner, []string{"missing"}, domain.ConflictFail, "key"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled batch restore = %v", err)
	}
	if _, err := provider.DeleteFromTrash(canceled, owner, "missing", "key"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled delete = %v", err)
	}
	if _, err := provider.GetBatchOperation(ctx, owner, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing batch operation = %v", err)
	}
}

func TestNamespaceBatchCompletionFailureRollsBackEveryItem(t *testing.T) {
	provider, live, _ := boundaryProvider(t)
	ctx := context.Background()
	owner := live.UserID()
	source := domain.MustParseUserPath("/source")
	destination := domain.MustParseUserPath("/destination")
	if _, err := provider.CreateDirectory(ctx, live, domain.CreateDirectoryRequest{Path: source}); err != nil {
		t.Fatal(err)
	}

	// The item operation consumes the only available opaque ID. Failure to
	// allocate the enclosing batch operation must roll the item back.
	provider.ids = domain.NewIDGenerator(bytes.NewReader(make([]byte, 16)))
	if _, err := provider.BatchCopyMove(ctx, owner, []domain.CopyRequest{{Source: source, Destination: destination}}, true, "batch-finish-failure"); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("batch completion failure = %v", err)
	}
	if _, err := provider.Stat(ctx, live, source); err != nil {
		t.Fatalf("source was not restored: %v", err)
	}
	if _, err := provider.Stat(ctx, live, destination); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("destination survived rollback: %v", err)
	}
}

func TestNamespaceTrashBatchDeleteReplayAndRollback(t *testing.T) {
	provider, live, _ := boundaryProvider(t)
	ctx := context.Background()
	owner := live.UserID()
	for _, name := range []string{"one", "two", "three", "four"} {
		if _, err := provider.CreateDirectory(ctx, live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/" + name)}); err != nil {
			t.Fatal(err)
		}
	}
	trashRequests := []domain.TrashRequest{
		{Path: domain.MustParseUserPath("/one"), TrashID: "one"},
		{Path: domain.MustParseUserPath("/two"), TrashID: "two"},
		{Path: domain.MustParseUserPath("/three"), TrashID: "three"},
		{Path: domain.MustParseUserPath("/four"), TrashID: "four"},
	}
	if _, err := provider.BatchMoveToTrash(ctx, owner, trashRequests, "trash-three"); err != nil {
		t.Fatal(err)
	}

	deleted, err := provider.BatchDeleteFromTrash(ctx, owner, []string{"one", "two"}, "delete-two")
	if err != nil || len(deleted.Items) != 2 || deleted.Operation.State != domain.OperationSucceeded {
		t.Fatalf("delete batch = %+v, %v", deleted, err)
	}
	if replay, err := provider.BatchDeleteFromTrash(ctx, owner, []string{"one", "two"}, "delete-two"); err != nil || replay.Operation.ID != deleted.Operation.ID {
		t.Fatalf("delete replay = %+v, %v", replay, err)
	}
	if _, err := provider.BatchDeleteFromTrash(ctx, owner, []string{"three"}, "delete-two"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("delete idempotency conflict = %v", err)
	}
	if operation, err := provider.GetBatchOperation(ctx, owner, deleted.Operation.ID); err != nil || operation.ID != deleted.Operation.ID {
		t.Fatalf("batch operation = %+v, %v", operation, err)
	}

	if _, err := provider.BatchDeleteFromTrash(ctx, owner, []string{"three", "missing"}, "rollback-delete"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("delete rollback error = %v", err)
	}
	page, err := provider.ListTrash(ctx, owner, domain.TrashListRequest{})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("trash rollback page = %+v, %v", page, err)
	}

	otherOwner, err := domain.ParseUserID("WFhYWFhYWFhYWFhYWFhYWA")
	if err != nil {
		t.Fatal(err)
	}
	first, err := provider.ListTrash(ctx, owner, domain.TrashListRequest{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" {
		t.Fatal("trash page did not create a cursor")
	}
	if _, err := provider.ListTrash(ctx, otherOwner, domain.TrashListRequest{Limit: 1, Cursor: first.NextCursor}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("cross-owner trash cursor error = %v", err)
	}
}

func TestNamespaceTrashFailureBeforePublicationLeavesSourceVisible(t *testing.T) {
	provider, live, _ := boundaryProvider(t)
	ctx := context.Background()
	owner := live.UserID()
	path := domain.MustParseUserPath("/source")
	entry, err := provider.CreateDirectory(ctx, live, domain.CreateDirectoryRequest{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.MoveToTrash(ctx, owner, domain.TrashRequest{Path: path, ExpectedVersion: "stale", TrashID: "stale", IdempotencyKey: "stale-trash"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale trash version error = %v", err)
	}
	if visible, err := provider.Stat(ctx, live, path); err != nil || visible.Version != entry.Version {
		t.Fatalf("source after denied trash = %+v, %v", visible, err)
	}

	provider.InjectFault(OperationMove, FaultPartialOperation)
	operation, err := provider.MoveToTrash(ctx, owner, domain.TrashRequest{Path: path, ExpectedVersion: entry.Version, TrashID: "partial", IdempotencyKey: "partial-trash"})
	if err != nil || operation.State != domain.OperationFailed {
		t.Fatalf("partial trash operation = %+v, %v", operation, err)
	}
	if _, err := provider.Stat(ctx, live, path); err != nil {
		t.Fatalf("partial trash lost source: %v", err)
	}
	if page, err := provider.ListTrash(ctx, owner, domain.TrashListRequest{}); err != nil || len(page.Items) != 0 {
		t.Fatalf("partial trash published metadata = %+v, %v", page, err)
	}
}
