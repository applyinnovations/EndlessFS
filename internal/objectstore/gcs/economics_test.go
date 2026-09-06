package gcs

import (
	"testing"

	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

func TestGCSEconomicsFixture(t *testing.T) {
	model, err := RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	if model.Provider() != "gcs" || model.Profile() != "regional-standard-flat" {
		t.Fatalf("economics identity = %s/%s", model.Provider(), model.Profile())
	}
	totals, err := model.Estimate([]providerbudget.Event{
		{Role: providerbudget.RoleState, Kind: providerbudget.RequestObjectGet},
		{Role: providerbudget.RoleState, Kind: providerbudget.RequestObjectPut},
		{Role: providerbudget.RoleState, Kind: providerbudget.RequestObjectDelete},
	})
	if err != nil {
		t.Fatal(err)
	}
	if totals.Requests != 3 || totals.CostPicoUSD != 5_400_000 || totals.P95Micros != 205_000 {
		t.Fatalf("GCS economics totals = %+v", totals)
	}
}

func TestGCSBudgetRatchetFixture(t *testing.T) {
	ledger, err := RegionalStandardFlatBudgetRatchet()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"direct-move-one-file",
		"namespace-list-page-1000-schema-010",
		"restore-batch-10000-schema-010",
	} {
		if _, ok := ledger.Latest(name); !ok {
			t.Fatalf("%s budget is missing", name)
		}
	}
}
