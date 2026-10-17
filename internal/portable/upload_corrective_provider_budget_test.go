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
)

func TestDirectoryPreparationProviderBudgetSharesWholeFolderSnapshot(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	engine := openNamespaceTestEngine(t, budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger))
	live := namespaceTestScope(t, domain.AreaLive)
	paths := []domain.UserPath{domain.MustParseUserPath("/folder")}
	for index := range 43 {
		paths = append(paths, domain.MustParseUserPath(fmt.Sprintf("/folder/dir-%02d", index)))
	}
	ledger.Reset()
	items, err := engine.Files().PrepareDirectories(ctx, live, paths)
	if err != nil || len(items) != len(paths) {
		t.Fatalf("create directories: %+v %v", items, err)
	}
	created := ledger.Events()
	ledger.Reset()
	for _, path := range paths {
		if _, err := engine.Files().Stat(ctx, live, path); err != nil {
			t.Fatal(err)
		}
	}
	scalar := ledger.Events()
	ledger.Reset()
	items, err = engine.Files().PrepareDirectories(ctx, live, paths)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if !item.Ready {
			t.Fatalf("existing directory: %+v", item)
		}
	}
	warm := ledger.Events()
	if len(warm) >= len(scalar) {
		t.Fatalf("batch failed to share reads: scalar=%d batch=%d", len(scalar), len(warm))
	}
	for _, event := range warm {
		if event.Kind != providerbudget.RequestObjectGet {
			t.Fatalf("replay mutated state: %+v", event)
		}
	}
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	ratchet, err := gcs.RegionalStandardFlatBudgetRatchet()
	if err != nil {
		t.Fatal(err)
	}
	for name, events := range map[string][]providerbudget.Event{"directory-prepare-new-44-schema-012": created, "directory-prepare-existing-44-schema-012": warm} {
		if report, err := checkGrowthBudget(t, ratchet, name, model, []providerbudget.Role{providerbudget.RoleState, providerbudget.RoleFile}, events); err != nil {
			t.Errorf("%s: %v; observed=%+v", name, err, report.Totals)
		}
	}
	t.Logf("folder preparation: paths=44 scalar-stats=%d batch-existing=%d batch-new=%d", len(scalar), len(warm), len(created))
}

func TestUploadPlanningProviderBudgetTenThousandExistingAndPlanningItems(t *testing.T) {
	parallelCorrectiveHeavyFixture(t)
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	engine := openNamespaceTestEngine(t, budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger))
	live := namespaceTestScope(t, domain.AreaLive)
	seedNamespaceBatchFiles(t, newNamespaceStore(engine), live, 10_000)
	ledger.Reset()
	plan, err := engine.Files().PlanUploadSizes(ctx, live.UserID(), uploadSizePlanFixture(10_000))
	if err != nil {
		t.Fatal(err)
	}
	cold := ledger.Events()
	ledger.Reset()
	if _, err := engine.Files().PlanUploadSizes(ctx, live.UserID(), uploadSizePlanFixture(1)); err != nil {
		t.Fatal(err)
	}
	oneSize := len(ledger.Events())
	ledger.Reset()
	if _, err := engine.Files().PlanUploadSizes(ctx, live.UserID(), uploadSizePlanFixture(10_000)); err != nil {
		t.Fatal(err)
	}
	warm := ledger.Events()
	if len(warm) != oneSize {
		t.Fatalf("warm size calls grow with cardinality: one=%d many=%d", oneSize, len(warm))
	}
	ledger.Reset()
	if _, err := engine.Files().PlanUploadFingerprints(ctx, live.UserID(), uploadFingerprintPlanFixture(1, plan.Token)); err != nil {
		t.Fatal(err)
	}
	oneExact := len(ledger.Events())
	ledger.Reset()
	if _, err := engine.Files().PlanUploadFingerprints(ctx, live.UserID(), uploadFingerprintPlanFixture(10_000, plan.Token)); err != nil {
		t.Fatal(err)
	}
	exact := ledger.Events()
	if len(exact) != oneExact {
		t.Fatalf("warm exact calls grow with cardinality: one=%d many=%d", oneExact, len(exact))
	}
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	ratchet, err := gcs.RegionalStandardFlatBudgetRatchet()
	if err != nil {
		t.Fatal(err)
	}
	for name, events := range map[string][]providerbudget.Event{"upload-plan-index-cold-10000-corrective": cold, "upload-plan-sizes-10000-corrective": warm, "upload-plan-fingerprints-10000-corrective": exact} {
		for _, event := range events {
			if event.Role != providerbudget.RoleState {
				t.Fatalf("file provider used for metadata planning: %+v", event)
			}
		}
		if report, err := checkGrowthBudget(t, ratchet, name, model, []providerbudget.Role{providerbudget.RoleState, providerbudget.RoleFile}, events); err != nil {
			t.Errorf("%s: %v; observed=%+v", name, err, report.Totals)
		}
	}
	t.Logf("planning existing=10000 queries=1/10000: cold=%d warm-size=%d warm-exact=%d", len(cold), len(warm), len(exact))
}
