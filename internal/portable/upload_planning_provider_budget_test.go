package portable

import (
	"context"
	"fmt"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func uploadSizePlanFixture(count int) domain.UploadSizePlanRequest {
	items := make([]domain.UploadSizePlanItem, count)
	for index := range items {
		items[index] = domain.UploadSizePlanItem{ID: fmt.Sprintf("size-%04d", index), Path: domain.MustParseUserPath(fmt.Sprintf("/incoming-%04d.bin", index)), Size: 1}
	}
	return domain.UploadSizePlanRequest{Items: items}
}

func uploadFingerprintPlanFixture(count int, token string) domain.UploadFingerprintPlanRequest {
	items := make([]domain.UploadFingerprintPlanItem, count)
	for index := range items {
		items[index] = domain.UploadFingerprintPlanItem{ID: fmt.Sprintf("exact-%04d", index), Path: domain.MustParseUserPath(fmt.Sprintf("/exact-%04d.bin", index)), Size: 1, MD5: testProviderMD5, CRC32C: testProviderCRC32C}
	}
	return domain.UploadFingerprintPlanRequest{Token: token, Items: items}
}

func assertMetadataOnlyPlanningEvents(t *testing.T, events []providerbudget.Event) providerbudget.Totals {
	t.Helper()
	for _, event := range events {
		if event.Role != providerbudget.RoleState || event.Kind != providerbudget.RequestObjectGet {
			t.Fatalf("upload planning issued non-metadata provider work: %+v", event)
		}
	}
	economics, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := economics.Estimate(events)
	if err != nil {
		t.Fatal(err)
	}
	return metrics
}

func assertDerivedPlanningEvents(t *testing.T, events []providerbudget.Event) providerbudget.Totals {
	t.Helper()
	for _, event := range events {
		if event.Role != providerbudget.RoleState || event.Kind != providerbudget.RequestObjectGet && event.Kind != providerbudget.RequestObjectPut {
			t.Fatalf("upload planning projection issued forbidden provider work: %+v", event)
		}
	}
	economics, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := economics.Estimate(events)
	if err != nil {
		t.Fatal(err)
	}
	return metrics
}

func TestUploadPlanningWarmRequestBudgetIsIndependentOfBatchCardinality(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
	engine := openNamespaceTestEngine(t, backend)
	live := namespaceTestScope(t, domain.AreaLive)
	seedNamespaceBatchFiles(t, newNamespaceStore(engine), live, 256)
	ledger.Reset()
	if _, err := engine.Files().PlanUploadSizes(ctx, live.UserID(), uploadSizePlanFixture(1)); err != nil {
		t.Fatal(err)
	}
	coldMetrics := assertDerivedPlanningEvents(t, ledger.Events())
	coldEvents := ledger.Events()

	ledger.Reset()
	oneSize, err := engine.Files().PlanUploadSizes(ctx, live.UserID(), uploadSizePlanFixture(1))
	if err != nil {
		t.Fatal(err)
	}
	oneSizeEvents := ledger.Events()
	ledger.Reset()
	manySize, err := engine.Files().PlanUploadSizes(ctx, live.UserID(), uploadSizePlanFixture(1000))
	if err != nil {
		t.Fatal(err)
	}
	manySizeEvents := ledger.Events()
	if len(oneSizeEvents) != len(manySizeEvents) {
		t.Fatalf("warm size-plan requests scale with item count: one=%d thousand=%d", len(oneSizeEvents), len(manySizeEvents))
	}
	sizeMetrics := assertMetadataOnlyPlanningEvents(t, manySizeEvents)

	ledger.Reset()
	if _, err := engine.Files().PlanUploadFingerprints(ctx, live.UserID(), uploadFingerprintPlanFixture(1, oneSize.Token)); err != nil {
		t.Fatal(err)
	}
	oneExactEvents := ledger.Events()
	ledger.Reset()
	if _, err := engine.Files().PlanUploadFingerprints(ctx, live.UserID(), uploadFingerprintPlanFixture(1000, manySize.Token)); err != nil {
		t.Fatal(err)
	}
	manyExactEvents := ledger.Events()
	if len(oneExactEvents) != len(manyExactEvents) {
		t.Fatalf("warm fingerprint-plan requests scale with item count: one=%d thousand=%d", len(oneExactEvents), len(manyExactEvents))
	}
	exactMetrics := assertMetadataOnlyPlanningEvents(t, manyExactEvents)

	store := newNamespaceStore(engine)
	if _, err := store.publishFileWithChanges(ctx, live, domain.MustParseUserPath("/newly-published.bin"), domain.ConflictFail, "", "planning-incremental-source", namespaceRequestFingerprint("planning-incremental-source"), storageformat.DirectoryEntry{
		Kind: domain.EntryFile, BlobID: "planning-incremental-blob", Size: 1, MediaType: "application/octet-stream", MD5: testProviderMD5, CRC32C: testProviderCRC32C, ModifiedAt: engine.clock.Now().UTC(),
	}, nil); err != nil {
		t.Fatal(err)
	}
	ledger.Reset()
	updated, err := engine.Files().PlanUploadSizes(ctx, live.UserID(), domain.UploadSizePlanRequest{Items: []domain.UploadSizePlanItem{{ID: "incremental", Path: domain.MustParseUserPath("/another.bin"), Size: 1}}})
	if err != nil || len(updated.Items) != 1 || !updated.Items[0].FingerprintRequired {
		t.Fatalf("incremental upload plan = %+v, %v", updated, err)
	}
	incrementalMetrics := assertDerivedPlanningEvents(t, ledger.Events())
	incrementalEvents := ledger.Events()
	economics, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	ratchet, err := gcs.RegionalStandardFlatBudgetRatchet()
	if err != nil {
		t.Fatal(err)
	}
	for name, events := range map[string][]providerbudget.Event{
		"upload-plan-index-cold-256-schema-010":        coldEvents,
		"upload-plan-index-incremental-one-schema-010": incrementalEvents,
		"upload-plan-sizes-1000-schema-010":            manySizeEvents,
		"upload-plan-fingerprints-1000-schema-010":     manyExactEvents,
	} {
		if report, err := ratchet.CheckExact(name, economics, []providerbudget.Role{providerbudget.RoleState, providerbudget.RoleFile}, events); err != nil {
			t.Errorf("%s: %v; observed=%+v", name, err, report.Totals)
		}
	}
	t.Logf("upload planning budget: cold-256=%+v incremental-one=%+v sizes-1000=%+v fingerprints-1000=%+v", coldMetrics, incrementalMetrics, sizeMetrics, exactMetrics)
}
