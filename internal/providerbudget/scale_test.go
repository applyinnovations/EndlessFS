package providerbudget_test

import (
	"encoding/json"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

type scaleEvidence struct {
	ID                 string `json:"id"`
	Category           string `json:"category"`
	LogicalItems       int64  `json:"logicalItems"`
	BrowserRequests    int64  `json:"browserRequests"`
	ProviderRequests   int64  `json:"providerRequests"`
	CostPicoUSD        int64  `json:"costPicoUSD"`
	AggregateP95Micros int64  `json:"aggregateP95Micros"`
	CriticalP95Micros  int64  `json:"criticalP95Micros"`
	RequestBasis       string `json:"requestBasis"`
}

func TestProviderBudgetProductionScaleScenarios(t *testing.T) {
	ratchet, err := gcs.RegionalStandardFlatBudgetRatchet()
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	evidence := make([]scaleEvidence, 0, len(providerbudget.ProductionScaleScenarios()))
	for _, scenario := range providerbudget.ProductionScaleScenarios() {
		if scenario.ID == "" || scenario.Category == "" || scenario.LogicalItems < 1 || scenario.BrowserRequests < 0 || scenario.RequestBasis == "" || seen[scenario.ID] {
			t.Fatalf("invalid production scale scenario: %+v", scenario)
		}
		seen[scenario.ID] = true
		item := scaleEvidence{ID: scenario.ID, Category: scenario.Category, LogicalItems: scenario.LogicalItems, BrowserRequests: scenario.BrowserRequests, RequestBasis: scenario.RequestBasis}
		for _, execution := range scenario.Executions {
			if execution.Budget == "" || execution.Executions < 1 || execution.Parallelism < 1 || execution.Parallelism > execution.Executions {
				t.Fatalf("invalid execution in %q: %+v", scenario.ID, execution)
			}
			budget, found := ratchet.Latest(execution.Budget)
			if !found {
				t.Fatalf("scale scenario %q references missing budget %q", scenario.ID, execution.Budget)
			}
			item.ProviderRequests += budget.Maximum.Requests * execution.Executions
			item.CostPicoUSD += budget.Maximum.CostPicoUSD * execution.Executions
			item.AggregateP95Micros += budget.Maximum.P95Micros * execution.Executions
			critical := budget.Maximum.CriticalP95Micros
			if critical == 0 {
				critical = budget.Maximum.P95Micros
			}
			waves := (execution.Executions + execution.Parallelism - 1) / execution.Parallelism
			item.CriticalP95Micros += critical * waves
		}
		if len(scenario.Executions) == 0 && (scenario.ID != "restored-transfer-ledger-needs-source" || scenario.BrowserRequests != 0) {
			t.Fatalf("only dormant restored transfer history may have a zero-provider scale budget: %+v", scenario)
		}
		evidence = append(evidence, item)
	}
	if len(evidence) < 18 {
		t.Fatalf("production scale audit has only %d scenarios", len(evidence))
	}
	body, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("provider-scale-v1 %s", body)
}
