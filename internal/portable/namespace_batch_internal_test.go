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
	return seedNamespaceBatchFilesWithMutation(t, store, scope, count, "batch-seed")
}

func seedNamespaceBatchFilesWithMutation(t *testing.T, store *namespaceStore, scope domain.Scope, count int, mutation string) []domain.Entry {
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
	operation := domain.Operation{ID: domain.OperationID(mutation + "-operation"), State: domain.OperationSucceeded, StartedAt: store.engine.clock.Now(), UpdatedAt: store.engine.clock.Now()}
	if _, err := store.commit(ctx, view, mutation, namespaceRequestFingerprint(mutation, fmt.Sprint(count)), map[string]storageformat.NamespaceEntry{namespaceFrameKey(scope.Area(), namespaceRootPath()): updated}, storageformat.NamespaceMutationResult{Operation: &operation}); err != nil {
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
	if report, err := ratchet.CheckExact("trash-batch-10000-schema-011", economics, []providerbudget.Role{providerbudget.RoleState}, events); err != nil {
		t.Errorf("10,000-item trash provider budget: %v; observed=%+v", err, report.Totals)
	}
	t.Logf("measured 10,000-item atomic trash provider budget: %+v", metrics)
	headKey := storageformat.DomainHeadKey(storageformat.DomainNamespace, live.UserID().String()).String()
	headPuts := 0
	packPuts := 0
	for _, event := range events {
		if event.Role != providerbudget.RoleState || event.Kind == providerbudget.RequestObjectList || event.Kind == providerbudget.RequestObjectCopy || event.Kind == providerbudget.RequestObjectDelete || event.Kind == providerbudget.RequestObjectOpen {
			t.Fatalf("batch trash used forbidden provider work: %+v", event)
		}
		if event.Kind == providerbudget.RequestObjectPut && event.Target == headKey && !event.Failed {
			headPuts++
		}
		if event.Kind == providerbudget.RequestObjectPut && strings.Contains(event.Target, "/packs/") && !event.Failed {
			packPuts++
		}
	}
	if headPuts != 1 {
		t.Fatalf("batch trash head publications = %d, want one; requests=%d", headPuts, len(events))
	}
	if len(events) != 4 {
		t.Fatalf("10,000-item trash requests = %d, want schema-011 packed publication count 4", len(events))
	}
	if packPuts != 1 {
		t.Fatalf("10,000-item trash wrote %d immutable page packs; want one bounded pack", packPuts)
	}
	t.Logf("measured 10,000-item atomic trash requests: %d", len(events))
	ledger.Reset()
	replayedTrash, err := engine.Files().BatchMoveToTrash(ctx, live.UserID(), requests, "batch-trash-10000")
	if err != nil || replayedTrash.Operation.ID != result.Operation.ID || len(replayedTrash.Items) != maximumNamespaceBatchItems {
		t.Fatalf("BatchMoveToTrash(replay) = %d items, %+v, %v", len(replayedTrash.Items), replayedTrash.Operation, err)
	}
	if report, err := ratchet.CheckExact("trash-batch-10000-replay-schema-011", economics, []providerbudget.Role{providerbudget.RoleState}, ledger.Events()); err != nil {
		t.Errorf("10,000-item trash replay provider budget: %v; observed=%+v", err, report.Totals)
	}
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
	if report, err := ratchet.CheckExact("empty-trash-10000-schema-011", economics, []providerbudget.Role{providerbudget.RoleState}, ledger.Events()); err != nil {
		t.Errorf("10,000-item permanent-delete provider budget: %v; observed=%+v", err, report.Totals)
	}
	ledger.Reset()
	replayedDelete, err := engine.Files().BatchDeleteFromTrash(ctx, live.UserID(), trashIDs, "batch-delete-10000")
	if err != nil || replayedDelete.Operation.ID != deleted.Operation.ID || len(replayedDelete.Items) != maximumNamespaceBatchItems {
		t.Fatalf("BatchDeleteFromTrash(replay) = %d items, %+v, %v", len(replayedDelete.Items), replayedDelete.Operation, err)
	}
	if report, err := ratchet.CheckExact("empty-trash-10000-replay-schema-011", economics, []providerbudget.Role{providerbudget.RoleState}, ledger.Events()); err != nil {
		t.Errorf("10,000-item permanent-delete replay provider budget: %v; observed=%+v", err, report.Totals)
	}
}

func TestNamespaceBatchRestorePublishesTenThousandEdgesThroughOneHead(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
	engine := openNamespaceTestEngine(t, backend)
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	entries := seedNamespaceBatchFiles(t, store, live, maximumNamespaceBatchItems)
	requests := make([]domain.TrashRequest, len(entries))
	trashIDs := make([]string, len(entries))
	for index, entry := range entries {
		trashIDs[index] = fmt.Sprintf("restore-%05d", index)
		requests[index] = domain.TrashRequest{Path: entry.Path, ExpectedVersion: entry.Version, TrashID: trashIDs[index]}
	}
	if _, err := engine.Files().BatchMoveToTrash(ctx, live.UserID(), requests, "prepare-batch-restore-10000"); err != nil {
		t.Fatal(err)
	}

	ledger.Reset()
	restored, err := engine.Files().BatchRestoreFromTrash(ctx, live.UserID(), trashIDs, domain.ConflictFail, "batch-restore-10000")
	if err != nil || len(restored.Items) != maximumNamespaceBatchItems || restored.Operation.State != domain.OperationSucceeded {
		t.Fatalf("BatchRestoreFromTrash() = %d items, %+v, %v", len(restored.Items), restored.Operation, err)
	}
	for index, item := range restored.Items {
		if item.Source.String() != "/"+trashIDs[index] || item.Destination != entries[index].Path || item.TrashID != trashIDs[index] || item.State != domain.OperationSucceeded {
			t.Fatalf("restored item %d = %+v", index, item)
		}
	}
	successEvents := ledger.Events()
	economics, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	ratchet, err := gcs.RegionalStandardFlatBudgetRatchet()
	if err != nil {
		t.Fatal(err)
	}
	if report, err := ratchet.CheckExact("restore-batch-10000-schema-011", economics, []providerbudget.Role{providerbudget.RoleState}, successEvents); err != nil {
		t.Errorf("10,000-item restore provider budget: %v; observed=%+v", err, report.Totals)
	}
	ledger.Reset()
	replayed, err := engine.Files().BatchRestoreFromTrash(ctx, live.UserID(), trashIDs, domain.ConflictFail, "batch-restore-10000")
	if err != nil || replayed.Operation.ID != restored.Operation.ID || len(replayed.Items) != maximumNamespaceBatchItems {
		t.Fatalf("BatchRestoreFromTrash(replay) = %d items, %+v, %v", len(replayed.Items), replayed.Operation, err)
	}
	if report, err := ratchet.CheckExact("restore-batch-10000-replay-schema-011", economics, []providerbudget.Role{providerbudget.RoleState}, ledger.Events()); err != nil {
		t.Errorf("10,000-item restore replay provider budget: %v; observed=%+v", err, report.Totals)
	}
	headKey := storageformat.DomainHeadKey(storageformat.DomainNamespace, live.UserID().String()).String()
	headPuts := 0
	for _, event := range successEvents {
		if event.Kind == providerbudget.RequestObjectPut && event.Target == headKey && !event.Failed {
			headPuts++
		}
		if event.Role != providerbudget.RoleState || event.Kind == providerbudget.RequestObjectList || event.Kind == providerbudget.RequestObjectCopy || event.Kind == providerbudget.RequestObjectDelete || event.Kind == providerbudget.RequestObjectOpen {
			t.Fatalf("batch restore used forbidden provider work: %+v", event)
		}
	}
	if headPuts != 1 {
		t.Fatalf("batch restore head publications = %d, want one; requests=%d", headPuts, len(successEvents))
	}
	liveRoot, err := store.stat(ctx, live, namespaceRootPath())
	if err != nil || liveRoot.FileCount != maximumNamespaceBatchItems || liveRoot.Size != maximumNamespaceBatchItems {
		t.Fatalf("live root after restore = %+v, %v", liveRoot, err)
	}
	trash := namespaceTestScope(t, domain.AreaTrash)
	trashRoot, err := store.stat(ctx, trash, namespaceRootPath())
	if err != nil || trashRoot.FileCount != 0 || trashRoot.Size != 0 {
		t.Fatalf("trash root after restore = %+v, %v", trashRoot, err)
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
	if report, err := ratchet.CheckExact("batch-copy-10000-schema-011", economics, []providerbudget.Role{providerbudget.RoleState}, ledger.Events()); err != nil {
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
	for index, event := range ledger.Events() {
		t.Logf("batch move provider event %d: %s %s", index+1, event.Kind, event.Target)
	}
	if report, err := ratchet.CheckExact("batch-move-10000-schema-011", economics, []providerbudget.Role{providerbudget.RoleState}, ledger.Events()); err != nil {
		t.Errorf("10,000-item move provider budget: %v; observed=%+v", err, report.Totals)
	}
}

func TestProviderBudgetNamespaceListTenThousandEntries(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	engine := openNamespaceTestEngine(t, budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger))
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	seedNamespaceBatchFiles(t, store, live, 10_000)
	economics, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	ratchet, err := gcs.RegionalStandardFlatBudgetRatchet()
	if err != nil {
		t.Fatal(err)
	}
	ledger.Reset()
	page, err := engine.Files().List(ctx, live, domain.ListRequest{Directory: namespaceRootPath(), PageSize: 10_000, Sort: domain.SortName})
	if err != nil || len(page.Entries) != 10_000 || page.NextCursor != "" {
		t.Fatalf("List(10000) = %d entries, cursor=%q, %v", len(page.Entries), page.NextCursor, err)
	}
	if report, err := ratchet.CheckExact("namespace-list-page-10000-schema-011", economics, []providerbudget.Role{providerbudget.RoleState}, ledger.Events()); err != nil {
		t.Errorf("10,000-entry list provider budget: %v; observed=%+v", err, report.Totals)
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

func TestProviderBudgetNamespaceBatchTenThousandItemDenialPublishesNothing(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	engine := openNamespaceTestEngine(t, budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger))
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	entries := seedNamespaceBatchFiles(t, store, live, maximumNamespaceBatchItems)
	requests := make([]domain.TrashRequest, len(entries))
	for index, entry := range entries {
		requests[index] = domain.TrashRequest{Path: entry.Path, ExpectedVersion: entry.Version, TrashID: fmt.Sprintf("denied-%05d", index)}
	}
	requests[len(requests)-1].ExpectedVersion = "stale"
	ledger.Reset()
	if _, err := engine.Files().BatchMoveToTrash(ctx, live.UserID(), requests, "batch-denied-10000"); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("BatchMoveToTrash(stale final item) error = %v", err)
	}
	for _, event := range ledger.Events() {
		if event.Kind == providerbudget.RequestObjectPut || event.Kind == providerbudget.RequestObjectDelete || event.Kind == providerbudget.RequestObjectCopy {
			t.Fatalf("denied 10,000-item batch wrote provider state: %+v", event)
		}
	}
	economics, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	ratchet, err := gcs.RegionalStandardFlatBudgetRatchet()
	if err != nil {
		t.Fatal(err)
	}
	if report, err := ratchet.CheckExact("trash-batch-10000-denied-schema-011", economics, []providerbudget.Role{providerbudget.RoleState}, ledger.Events()); err != nil {
		t.Errorf("10,000-item denied trash provider budget: %v; observed=%+v", err, report.Totals)
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

func TestNamespaceBatchRestoreConflictPublishesNothing(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	engine := openNamespaceTestEngine(t, budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger))
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	entries := seedNamespaceBatchFiles(t, store, live, 2)
	requests := []domain.TrashRequest{
		{Path: entries[0].Path, ExpectedVersion: entries[0].Version, TrashID: "restore-conflict-0"},
		{Path: entries[1].Path, ExpectedVersion: entries[1].Version, TrashID: "restore-conflict-1"},
	}
	if _, err := engine.Files().BatchMoveToTrash(ctx, live.UserID(), requests, "prepare-restore-conflict"); err != nil {
		t.Fatal(err)
	}
	seedNamespaceBatchFilesWithMutation(t, store, live, 2, "batch-restore-conflicting-targets")
	ledger.Reset()
	if _, err := engine.Files().BatchRestoreFromTrash(ctx, live.UserID(), []string{"restore-conflict-0", "restore-conflict-1"}, domain.ConflictFail, "restore-conflict"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("BatchRestoreFromTrash(conflict) error = %v", err)
	}
	for _, event := range ledger.Events() {
		if event.Kind == providerbudget.RequestObjectPut || event.Kind == providerbudget.RequestObjectDelete || event.Kind == providerbudget.RequestObjectCopy {
			t.Fatalf("denied restore batch wrote provider state: %+v", event)
		}
	}
	trash := namespaceTestScope(t, domain.AreaTrash)
	for _, trashID := range []string{"restore-conflict-0", "restore-conflict-1"} {
		if _, err := store.stat(ctx, trash, domain.MustParseUserPath("/"+trashID)); err != nil {
			t.Fatalf("denied restore removed %s: %v", trashID, err)
		}
	}
}

func TestNamespaceBatchRestoreLostSuccessReplaysSameOutcome(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	entries := seedNamespaceBatchFiles(t, store, live, 3)
	requests := make([]domain.TrashRequest, len(entries))
	trashIDs := make([]string, len(entries))
	for index, entry := range entries {
		trashIDs[index] = fmt.Sprintf("restore-lost-%d", index)
		requests[index] = domain.TrashRequest{Path: entry.Path, ExpectedVersion: entry.Version, TrashID: trashIDs[index]}
	}
	if _, err := engine.Files().BatchMoveToTrash(ctx, live.UserID(), requests, "prepare-restore-lost"); err != nil {
		t.Fatal(err)
	}
	failure := &internalStepFailure{step: StepDomainAfterHeadCommit}
	engine.scheduler = failure
	first, err := engine.Files().BatchRestoreFromTrash(ctx, live.UserID(), trashIDs, domain.ConflictFail, "restore-lost-success")
	if err != nil || len(first.Items) != len(trashIDs) {
		t.Fatalf("lost-success restore = %+v, %v", first, err)
	}
	engine.scheduler = nil
	replayed, err := engine.Files().BatchRestoreFromTrash(ctx, live.UserID(), trashIDs, domain.ConflictFail, "restore-lost-success")
	if err != nil || replayed.Operation.ID != first.Operation.ID || len(replayed.Items) != len(first.Items) {
		t.Fatalf("replayed restore = %+v, %v; first=%+v", replayed, err, first)
	}
}

func TestNamespaceBatchRestoreRejectsInvalidRequestsBeforeProviderAccess(t *testing.T) {
	ledger := providerbudget.NewLedger()
	engine := openNamespaceTestEngine(t, budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger))
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	tests := []struct {
		name     string
		owner    domain.UserID
		trashIDs []string
		conflict domain.ConflictMode
	}{
		{name: "invalid owner", trashIDs: []string{"trash-1"}, conflict: domain.ConflictFail},
		{name: "invalid conflict", owner: owner, trashIDs: []string{"trash-1"}, conflict: domain.ConflictMode("overwrite")},
		{name: "duplicate identity", owner: owner, trashIDs: []string{"trash-1", "trash-1"}, conflict: domain.ConflictFail},
		{name: "non-opaque identity", owner: owner, trashIDs: []string{"nested/trash-1"}, conflict: domain.ConflictFail},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ledger.Reset()
			if _, err := engine.Files().BatchRestoreFromTrash(context.Background(), test.owner, test.trashIDs, test.conflict, "invalid-restore"); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("BatchRestoreFromTrash() error = %v, want invalid", err)
			}
			if events := ledger.Events(); len(events) != 0 {
				t.Fatalf("invalid restore reached provider: %+v", events)
			}
		})
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
