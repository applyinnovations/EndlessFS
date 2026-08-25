package portable

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func seedNamespaceBatchFiles(t *testing.T, store *namespaceStore, scope domain.Scope, count int) []domain.Entry {
	t.Helper()
	ctx := context.Background()
	view, err := store.loadView(ctx, scope.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	edits := make([]namespaceDirectoryEdit, count)
	entries := make([]domain.Entry, count)
	for index := 0; index < count; index++ {
		name := fmt.Sprintf("file-%05d.bin", index)
		entry := storageformat.NamespaceEntry{SchemaVersion: 1, NodeID: fmt.Sprintf("batch-node-%05d", index), Entry: storageformat.DirectoryEntry{
			Name: name, NameDigest: storageformat.NameDigest(name), Kind: domain.EntryFile, BlobID: fmt.Sprintf("batch-blob-%05d", index),
			Size: 1, MediaType: "application/octet-stream", MD5: testProviderMD5, CRC32C: testProviderCRC32C,
			ModifiedAt: store.engine.clock.Now().UTC(),
		}}
		entry.Entry.LogicalVersion, err = directoryEntryVersion(entry.Entry)
		if err != nil {
			t.Fatal(err)
		}
		edits[index] = namespaceDirectoryEdit{after: &entry}
		path := domain.MustParseUserPath("/" + name)
		entries[index] = namespaceDomainEntry(path, entry)
	}
	root := view.roots[scope.Area()]
	updated, err := store.applyDirectoryEdits(ctx, view, root, edits, store.engine.clock.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	operation := domain.Operation{ID: "batch-seed-operation", State: domain.OperationSucceeded, StartedAt: store.engine.clock.Now(), UpdatedAt: store.engine.clock.Now()}
	if _, err := store.commit(ctx, view, "batch-seed", namespaceRequestFingerprint("batch-seed", fmt.Sprint(count)), map[string]storageformat.NamespaceEntry{namespaceFrameKey(scope.Area(), namespaceRootPath()): updated}, storageformat.NamespaceMutationResult{Operation: &operation}); err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestNamespaceBatchTrashPublishesTenThousandEdgesThroughOneHead(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	base := objectmemory.New()
	backend := budgettest.Wrap(providerbudget.RoleState, base, ledger)
	engine := openNamespaceTestEngine(t, backend)
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	entries := seedNamespaceBatchFiles(t, store, live, maximumNamespaceBatchItems)
	requests := make([]domain.TrashRequest, len(entries))
	for index, entry := range entries {
		requests[index] = domain.TrashRequest{Path: entry.Path, ExpectedVersion: entry.Version, TrashID: fmt.Sprintf("trash-%05d", index)}
	}

	ledger.Reset()
	result, err := engine.Files().BatchMoveToTrash(ctx, live.UserID(), requests, "batch-trash-10000")
	if err != nil || len(result.Items) != maximumNamespaceBatchItems || result.Operation.State != domain.OperationSucceeded {
		t.Fatalf("BatchMoveToTrash() = %d items, %+v, %v", len(result.Items), result.Operation, err)
	}
	events := ledger.Events()
	economics, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := economics.Estimate(events)
	if err != nil {
		t.Fatal(err)
	}
	ratchet, err := gcs.RegionalStandardFlatBudgetRatchet()
	if err != nil {
		t.Fatal(err)
	}
	if report, err := ratchet.CheckExact("trash-batch-10000-schema-009", economics, []providerbudget.Role{providerbudget.RoleState}, events); err != nil {
		t.Errorf("10,000-item trash provider budget: %v; observed=%+v", err, report.Totals)
	}
	t.Logf("measured 10,000-item atomic trash provider budget: %+v", metrics)
	headKey := storageformat.DomainHeadKey(storageformat.DomainNamespace, live.UserID().String()).String()
	headPuts := 0
	parallelPagePuts := 0
	serialPagePuts := 0
	for _, event := range events {
		if event.Role != providerbudget.RoleState || event.Kind == providerbudget.RequestObjectList || event.Kind == providerbudget.RequestObjectCopy || event.Kind == providerbudget.RequestObjectDelete || event.Kind == providerbudget.RequestObjectOpen {
			t.Fatalf("batch trash used forbidden provider work: %+v", event)
		}
		if event.Kind == providerbudget.RequestObjectPut && event.Target == headKey && !event.Failed {
			headPuts++
		}
		if event.Kind == providerbudget.RequestObjectPut && strings.Contains(event.Target, "/pages/") && !event.Failed {
			switch event.ParallelGroup {
			case "immutable-domain-pages":
				parallelPagePuts++
			case "":
				// A rewrite level can contain one page. There is no independent
				// work to parallelize in that level, so it is deliberately serial.
				serialPagePuts++
			default:
				t.Fatalf("immutable page write used an unknown parallel group: %+v", event)
			}
		}
	}
	if headPuts != 1 {
		t.Fatalf("batch trash head publications = %d, want one; requests=%d", headPuts, len(events))
	}
	if len(events) != 125 {
		t.Fatalf("10,000-item trash requests = %d, want measured schema-009 calibration 125", len(events))
	}
	if parallelPagePuts < 2 || serialPagePuts == 0 {
		t.Fatalf("10,000-item trash wrote %d immutable pages; want a parallelizable page batch", parallelPagePuts)
	}
	t.Logf("measured 10,000-item atomic trash requests: %d", len(events))
	root, err := store.stat(ctx, live, namespaceRootPath())
	if err != nil || root.FileCount != 0 || root.Size != 0 {
		t.Fatalf("live root after batch = %+v, %v", root, err)
	}
	trash := namespaceTestScope(t, domain.AreaTrash)
	trashRoot, err := store.stat(ctx, trash, namespaceRootPath())
	if err != nil || trashRoot.FileCount != maximumNamespaceBatchItems || trashRoot.Size != maximumNamespaceBatchItems {
		t.Fatalf("trash root after batch = %+v, %v", trashRoot, err)
	}

	trashIDs := make([]string, len(result.Items))
	for index := range result.Items {
		trashIDs[index] = result.Items[index].TrashID
	}
	ledger.Reset()
	deleted, err := engine.Files().BatchDeleteFromTrash(ctx, live.UserID(), trashIDs, "batch-delete-10000")
	if err != nil || len(deleted.Items) != maximumNamespaceBatchItems {
		t.Fatalf("BatchDeleteFromTrash() = %d items, %v", len(deleted.Items), err)
	}
	if report, err := ratchet.CheckExact("empty-trash-10000-schema-009", economics, []providerbudget.Role{providerbudget.RoleState}, ledger.Events()); err != nil {
		t.Errorf("10,000-item permanent-delete provider budget: %v; observed=%+v", err, report.Totals)
	}
}

func TestProviderBudgetNamespaceCopyAndMoveTenThousandRoots(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	engine := openNamespaceTestEngine(t, budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger))
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	entries := seedNamespaceBatchFiles(t, store, live, maximumNamespaceBatchItems)
	economics, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	ratchet, err := gcs.RegionalStandardFlatBudgetRatchet()
	if err != nil {
		t.Fatal(err)
	}
	requests := make([]domain.CopyRequest, len(entries))
	for index, entry := range entries {
		requests[index] = domain.CopyRequest{
			Source: entry.Path, Destination: domain.MustParseUserPath(fmt.Sprintf("/copy-%05d.bin", index)),
			ExpectedSource: entry.Version, Conflict: domain.ConflictFail,
		}
	}
	ledger.Reset()
	if result, err := engine.Files().BatchCopyMove(ctx, live.UserID(), requests, false, "batch-copy-10000"); err != nil || len(result.Items) != maximumNamespaceBatchItems {
		t.Fatalf("BatchCopyMove(copy) = %d items, %v", len(result.Items), err)
	}
	if report, err := ratchet.CheckExact("batch-copy-10000-schema-009", economics, []providerbudget.Role{providerbudget.RoleState}, ledger.Events()); err != nil {
		t.Errorf("10,000-item copy provider budget: %v; observed=%+v", err, report.Totals)
	}
	for index, entry := range entries {
		requests[index] = domain.CopyRequest{
			Source: entry.Path, Destination: domain.MustParseUserPath(fmt.Sprintf("/moved-%05d.bin", index)),
			ExpectedSource: entry.Version, Conflict: domain.ConflictFail,
		}
	}
	ledger.Reset()
	if result, err := engine.Files().BatchCopyMove(ctx, live.UserID(), requests, true, "batch-move-10000"); err != nil || len(result.Items) != maximumNamespaceBatchItems {
		t.Fatalf("BatchCopyMove(move) = %d items, %v", len(result.Items), err)
	}
	if report, err := ratchet.CheckExact("batch-move-10000-schema-009", economics, []providerbudget.Role{providerbudget.RoleState}, ledger.Events()); err != nil {
		t.Errorf("10,000-item move provider budget: %v; observed=%+v", err, report.Totals)
	}
}

func TestNamespaceBatchPreconditionFailurePublishesNothing(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
	engine := openNamespaceTestEngine(t, backend)
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	entries := seedNamespaceBatchFiles(t, store, live, 3)
	requests := make([]domain.TrashRequest, len(entries))
	for index, entry := range entries {
		requests[index] = domain.TrashRequest{Path: entry.Path, ExpectedVersion: entry.Version, TrashID: fmt.Sprintf("atomic-%d", index)}
	}
	requests[2].ExpectedVersion = "stale"
	ledger.Reset()
	if _, err := engine.Files().BatchMoveToTrash(ctx, live.UserID(), requests, "batch-atomic-denial"); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("BatchMoveToTrash(stale) error = %v", err)
	}
	for _, event := range ledger.Events() {
		if event.Kind == providerbudget.RequestObjectPut || event.Kind == providerbudget.RequestObjectDelete || event.Kind == providerbudget.RequestObjectCopy {
			t.Fatalf("denied batch wrote provider state: %+v", event)
		}
	}
	for _, entry := range entries {
		if _, err := store.stat(ctx, live, entry.Path); err != nil {
			t.Fatalf("denied batch removed %s: %v", entry.Path.String(), err)
		}
	}
}

func TestNamespaceBatchLostSuccessReplaysSamePagedOutcome(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	engine := openNamespaceTestEngine(t, backend)
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	entries := seedNamespaceBatchFiles(t, store, live, 3)
	requests := make([]domain.TrashRequest, len(entries))
	for index, entry := range entries {
		requests[index] = domain.TrashRequest{Path: entry.Path, ExpectedVersion: entry.Version, TrashID: fmt.Sprintf("lost-%d", index)}
	}
	failure := &internalStepFailure{step: StepDomainAfterHeadCommit}
	engine.scheduler = failure
	first, err := engine.Files().BatchMoveToTrash(ctx, live.UserID(), requests, "batch-lost-success")
	if err != nil || len(first.Items) != len(requests) {
		t.Fatalf("lost-success batch = %+v, %v", first, err)
	}
	engine.scheduler = nil
	replayed, err := engine.Files().BatchMoveToTrash(ctx, live.UserID(), requests, "batch-lost-success")
	if err != nil || replayed.Operation.ID != first.Operation.ID || len(replayed.Items) != len(first.Items) {
		t.Fatalf("replayed batch = %+v, %v; first=%+v", replayed, err, first)
	}
}

type internalStepFailure struct {
	step string
	done bool
}

func (failure *internalStepFailure) Step(_ context.Context, step string) error {
	if step == failure.step && !failure.done {
		failure.done = true
		return domain.NewError(domain.ErrorUnavailable, "injected replica loss")
	}
	return nil
}

func TestNamespaceCursorContinuationDoesNotExtendSnapshotLifetime(t *testing.T) {
	clock := domain.NewFixedClock(time.Date(2052, 1, 2, 3, 4, 5, 0, time.UTC))
	engine := openInternalTestEngine(t, objectmemory.New(), clock, strings.NewReader(strings.Repeat("cursor-lifetime-seed-0123456789", 1<<15)))
	engine.cursorTTL = time.Minute
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	seedNamespaceBatchFiles(t, store, live, 5)
	first, err := store.list(context.Background(), live, domain.ListRequest{Directory: namespaceRootPath(), PageSize: 1})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	firstCursor, err := store.decodeListCursor(first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(50 * time.Second)
	second, err := store.list(context.Background(), live, domain.ListRequest{Directory: namespaceRootPath(), PageSize: 1, Cursor: first.NextCursor})
	if err != nil || second.NextCursor == "" {
		t.Fatalf("second page = %+v, %v", second, err)
	}
	secondCursor, err := store.decodeListCursor(second.NextCursor)
	if err != nil || !secondCursor.ExpiresAt.Equal(firstCursor.ExpiresAt) {
		t.Fatalf("continuation expiry = %v, %v; first=%v", secondCursor.ExpiresAt, err, firstCursor.ExpiresAt)
	}
	clock.Advance(11 * time.Second)
	if _, err := store.list(context.Background(), live, domain.ListRequest{Directory: namespaceRootPath(), PageSize: 1, Cursor: second.NextCursor}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("extended cursor error = %v", err)
	}
}
