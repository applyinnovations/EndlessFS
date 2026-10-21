package portable

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
)

func TestUploadPlanningProviderBudgetColdProductionRecoveryScale(t *testing.T) {
	parallelCorrectiveHeavyFixture(t)
	ctx := telemetry.Context(context.Background(), telemetry.New(nil, nil))
	ledger := providerbudget.NewLedger()
	backend := objectstore.Observe(budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger), telemetry.State)
	engine := openNamespaceTestEngine(t, backend)
	live := namespaceTestScope(t, domain.AreaLive)
	seedNamespaceBatchFiles(t, newNamespaceStore(engine), live, 7_966)
	ledger.Reset()
	result, err := engine.Files().PlanUploadSizes(ctx, live.UserID(), uploadSizePlanFixture(2_638))
	if err != nil {
		t.Fatalf("cold planning for 7,966 completed / 2,638 remaining files failed: %v; pack-size-bound=%t; provider-calls=%d", err, errors.Is(err, storageformat.ErrDomainPagePackTooLarge), len(ledger.Events()))
	}
	if len(result.Items) != 2_638 || result.Token == "" {
		t.Fatalf("incomplete cold size plan: items=%d token-present=%t", len(result.Items), result.Token != "")
	}
	t.Logf("cold planning: existing=7966 planned=2638 provider-calls=%d", len(ledger.Events()))
}

func TestUploadPlanningColdLongMetadataRecoveryScale(t *testing.T) {
	parallelCorrectiveHeavyFixture(t)
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	live := namespaceTestScope(t, domain.AreaLive)
	store := newNamespaceStore(engine)
	view, err := store.loadView(ctx, live.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	edits := make([]namespaceDirectoryEdit, 7_966)
	for index := range edits {
		name := fmt.Sprintf("file-%05d-%s.bin", index, strings.Repeat("synthetic", 24))
		entry := storageformat.NamespaceEntry{SchemaVersion: 1, NodeID: fmt.Sprintf("scale-node-%05d", index), Entry: storageformat.DirectoryEntry{
			Name: name, NameDigest: storageformat.NameDigest(name), Kind: domain.EntryFile, BlobID: fmt.Sprintf("scale-blob-%05d", index),
			Size: 1, MediaType: "application/octet-stream", MD5: testProviderMD5, CRC32C: testProviderCRC32C, ModifiedAt: engine.clock.Now().UTC(),
		}}
		entry.Entry.LogicalVersion, err = directoryEntryVersion(entry.Entry)
		if err != nil {
			t.Fatal(err)
		}
		edits[index] = namespaceDirectoryEdit{after: &entry}
	}
	updated, err := store.applyDirectoryEdits(ctx, view, view.roots[domain.AreaLive], edits, engine.clock.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	operation := domain.Operation{ID: "scale-seed-operation", State: domain.OperationSucceeded, StartedAt: engine.clock.Now(), UpdatedAt: engine.clock.Now()}
	if _, err := store.commit(ctx, view, "scale-seed", namespaceRequestFingerprint("scale-seed"), map[string]storageformat.NamespaceEntry{namespaceFrameKey(domain.AreaLive, namespaceRootPath()): updated}, storageformat.NamespaceMutationResult{Operation: &operation}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Files().PlanUploadSizes(ctx, live.UserID(), uploadSizePlanFixture(2_638)); err != nil {
		t.Fatalf("cold long-metadata planning failed: %v; pack-size-bound=%t", err, errors.Is(err, storageformat.ErrDomainPagePackTooLarge))
	}
}

func TestUploadPlanningRebuiltProjectionDoesNotAliasPriorPack(t *testing.T) {
	parallelCorrectiveHeavyFixture(t)
	ctx := context.Background()
	backend := objectmemory.New()
	engine := openNamespaceTestEngine(t, backend)
	live := namespaceTestScope(t, domain.AreaLive)
	store := newNamespaceStore(engine)
	seedNamespaceBatchFiles(t, store, live, 256)
	if _, err := engine.Files().PlanUploadSizes(ctx, live.UserID(), uploadSizePlanFixture(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.publishFileWithChanges(ctx, live, domain.MustParseUserPath("/zz-newly-published.bin"), domain.ConflictFail, "", "planning-rebuild-source", namespaceRequestFingerprint("planning-rebuild-source"), storageformat.DirectoryEntry{
		Kind: domain.EntryFile, BlobID: "planning-rebuild-blob", Size: 1, MediaType: "application/octet-stream", MD5: testProviderMD5, CRC32C: testProviderCRC32C, ModifiedAt: engine.clock.Now().UTC(),
	}, nil); err != nil {
		t.Fatal(err)
	}
	key := storageformat.ScopedProjectionHeadKey(live.UserID().String(), storageformat.ProjectionDuplicates, uploadPlanningProjectionID008(live.UserID()))
	head, err := backend.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Delete(ctx, key, objectstore.DeleteCondition{Version: head.Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Files().PlanUploadSizes(ctx, live.UserID(), uploadSizePlanFixture(1)); err != nil {
		t.Fatalf("projection rebuild after namespace changed failed: %v", err)
	}
}
