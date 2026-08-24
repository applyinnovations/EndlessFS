package architecturelab

import (
	"context"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

func TestPrototypeDuplicateWorkflowProviderEconomics(t *testing.T) {
	ctx := context.Background()
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	base := objectmemory.New()
	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleState, base, ledger)
	view, err := openDerivedView(ctx, backend, "duplicates", 7, []byte(`{"groups":[{"id":"same"}],"occurrences":["/left","/right"],"overlaps":["/right"]}`))
	if err != nil {
		t.Fatal(err)
	}
	control, err := openRecordDomain(ctx, backend, "duplicate-preferences")
	if err != nil {
		t.Fatal(err)
	}
	batch, err := openBatchNamespace(ctx, backend, "duplicate-reconcile")
	if err != nil {
		t.Fatal(err)
	}
	if err := batch.Seed(ctx, []string{"same.txt"}); err != nil {
		t.Fatal(err)
	}
	measure := func(name string, run func() error) {
		t.Helper()
		ledger.Reset()
		if err := run(); err != nil {
			t.Fatal(err)
		}
		logCurrentEconomics(t, name, model, ledger)
	}
	for _, name := range []string{"duplicates/list-groups", "duplicates/list-occurrences", "duplicates/compare-directories", "duplicates/list-directory-overlaps"} {
		name := name
		measure(name, func() error {
			_, err := view.Read(ctx, 7)
			return err
		})
	}
	measure("duplicates/set-group-ignored", func() error {
		_, err := control.Mutate(ctx, RecordMutation{ID: "ignore-group", Key: "group/same", Value: []byte(`{"ignored":true}`)})
		return err
	})
	measure("duplicates/set-directory-ignored", func() error {
		_, err := control.Mutate(ctx, RecordMutation{ID: "ignore-pair", Key: "pair/left-right", Value: []byte(`{"ignored":true}`)})
		return err
	})
	var planKey objectstore.Key
	measure("duplicates/preview-reconciliation", func() error {
		if _, err := view.Read(ctx, 7); err != nil {
			return err
		}
		var err error
		planKey, err = view.CreatePlan(ctx, "plan-one", []byte(`{"sourceRevision":7,"remove":["same.txt"]}`))
		return err
	})
	measure("duplicates/validate-reconciliation", func() error {
		if _, err := backend.Get(derivedTrace(ctx, "validate-reconciliation", "reconciliation-plan", ""), planKey); err != nil {
			return err
		}
		_, _, err := batch.load(ctx, "validate-reconciliation")
		return err
	})
	measure("duplicates/apply-reconciliation", func() error {
		if _, err := backend.Get(derivedTrace(ctx, "apply-reconciliation", "reconciliation-plan", ""), planKey); err != nil {
			return err
		}
		_, err := batch.Trash(ctx, "apply-plan-one", []string{"same.txt"})
		return err
	})
}
