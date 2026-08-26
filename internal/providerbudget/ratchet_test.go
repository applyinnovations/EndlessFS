package providerbudget

import (
	"strings"
	"testing"
)

func TestRatchetLedgerOnlyTightensAndCannotDropPathways(t *testing.T) {
	valid := []byte(`{"schemaVersion":1,"provider":"gcs","profile":"regional","epochs":[{"id":"001","budgets":[{"name":"move","provider":"gcs","profile":"regional","maximum":{"requests":2,"costPicoUSD":2,"p50Micros":2,"p95Micros":3,"p99Micros":4},"roles":{"state":{"requests":2,"costPicoUSD":2,"p50Micros":2,"p95Micros":3,"p99Micros":4}}}]},{"id":"002","budgets":[{"name":"move","provider":"gcs","profile":"regional","maximum":{"requests":1,"costPicoUSD":2,"p50Micros":2,"p95Micros":3,"p99Micros":4},"roles":{"state":{"requests":1,"costPicoUSD":2,"p50Micros":2,"p95Micros":3,"p99Micros":4}}}]}]}`)
	ledger, err := ParseRatchetLedger(valid)
	if err != nil {
		t.Fatal(err)
	}
	if budget, ok := ledger.Latest("move"); !ok || budget.Maximum.Requests != 1 {
		t.Fatalf("Latest(move) = %+v, %t", budget, ok)
	}
	for name, body := range map[string][]byte{
		"loosened": []byte(`{"schemaVersion":1,"provider":"gcs","profile":"regional","epochs":[{"id":"001","budgets":[{"name":"move","provider":"gcs","profile":"regional","maximum":{"requests":1},"roles":{"state":{"requests":1}}}]},{"id":"002","budgets":[{"name":"move","provider":"gcs","profile":"regional","maximum":{"requests":2},"roles":{"state":{"requests":2}}}]}]}`),
		"removed":  []byte(`{"schemaVersion":1,"provider":"gcs","profile":"regional","epochs":[{"id":"001","budgets":[{"name":"move","provider":"gcs","profile":"regional","maximum":{"requests":1},"roles":{"state":{"requests":1}}}]},{"id":"002","budgets":[{"name":"copy","provider":"gcs","profile":"regional","maximum":{"requests":1},"roles":{"state":{"requests":1}}}]}]}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRatchetLedger(body); err == nil {
				t.Fatal("invalid ratchet unexpectedly parsed")
			}
		})
	}
}

func TestRatchetLedgerCanIntroduceButNeverLoosenCriticalPathLimits(t *testing.T) {
	valid := []byte(`{"schemaVersion":1,"provider":"gcs","profile":"regional","epochs":[{"id":"001","budgets":[{"name":"batch","provider":"gcs","profile":"regional","maximum":{"requests":2,"p50Micros":20,"p95Micros":30,"p99Micros":40},"roles":{"state":{"requests":2}}}]},{"id":"002","budgets":[{"name":"batch","provider":"gcs","profile":"regional","maximum":{"requests":2,"p50Micros":20,"p95Micros":30,"p99Micros":40,"criticalP50Micros":5,"criticalP95Micros":8,"criticalP99Micros":13},"roles":{"state":{"requests":2}}}]}]}`)
	if _, err := ParseRatchetLedger(valid); err != nil {
		t.Fatal(err)
	}
	loosened := []byte(`{"schemaVersion":1,"provider":"gcs","profile":"regional","epochs":[{"id":"001","budgets":[{"name":"batch","provider":"gcs","profile":"regional","maximum":{"requests":2,"p50Micros":20,"p95Micros":30,"p99Micros":40,"criticalP50Micros":5,"criticalP95Micros":8,"criticalP99Micros":13},"roles":{"state":{"requests":2}}}]},{"id":"002","budgets":[{"name":"batch","provider":"gcs","profile":"regional","maximum":{"requests":2,"p50Micros":20,"p95Micros":30,"p99Micros":40,"criticalP50Micros":6,"criticalP95Micros":8,"criticalP99Micros":13},"roles":{"state":{"requests":2}}}]}]}`)
	if _, err := ParseRatchetLedger(loosened); err == nil {
		t.Fatal("critical-path ratchet loosened")
	}
}

func TestRatchetCheckExactReportsMissingCalibrationAndRoleDrift(t *testing.T) {
	model, err := ParseModel(
		[]byte(`{"schemaVersion":1,"provider":"gcs","profile":"regional","currency":"USD","sourceURL":"https://provider.example/pricing","effectiveDate":"2040-01-02","assumptions":["test"],"requests":{"object_get":{"billingClass":"read","unitRequests":1,"unitPricePicoUSD":1}}}`),
		[]byte(`{"schemaVersion":1,"provider":"gcs","profile":"regional","sourceURL":"https://provider.example/latency","researchedDate":"2040-01-03","methodology":"test","limitations":["test"],"supportingSourceURLs":["https://provider.example/guidance"],"requests":{"object_get":{"p50Micros":1,"p95Micros":2,"p99Micros":3}}}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := ParseRatchetLedger([]byte(`{"schemaVersion":1,"provider":"gcs","profile":"regional","epochs":[{"id":"001","budgets":[{"name":"read","provider":"gcs","profile":"regional","maximum":{"requests":1,"costPicoUSD":1,"p50Micros":1,"p95Micros":2,"p99Micros":3,"criticalP50Micros":1,"criticalP95Micros":2,"criticalP99Micros":3},"roles":{"state":{"requests":1,"costPicoUSD":1,"p50Micros":1,"p95Micros":2,"p99Micros":3}}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	events := []Event{{Role: RoleState, Kind: RequestObjectGet}}
	if _, err := ledger.CheckExact("missing", model, []Role{RoleState}, events); err == nil || !strings.Contains(err.Error(), `"name":"missing"`) {
		t.Fatalf("missing calibration error = %v", err)
	}
	if _, err := ledger.CheckExact("read", model, []Role{RoleState, RoleFile}, events); err == nil || !strings.Contains(err.Error(), "role contract changed") {
		t.Fatalf("role drift error = %v", err)
	}
	if _, err := ledger.CheckExact("read", model, []Role{RoleState}, events); err != nil {
		t.Fatal(err)
	}
}

func TestRatchetDeltaInheritsUnchangedBudgetsAndTightensExistingOnes(t *testing.T) {
	base, err := ParseRatchetLedger([]byte(`{"schemaVersion":1,"provider":"gcs","profile":"regional","epochs":[{"id":"001","budgets":[{"name":"move","provider":"gcs","profile":"regional","maximum":{"requests":2,"costPicoUSD":2,"p50Micros":2,"p95Micros":3,"p99Micros":4},"roles":{"state":{"requests":2,"costPicoUSD":2,"p50Micros":2,"p95Micros":3,"p99Micros":4}}},{"name":"read","provider":"gcs","profile":"regional","maximum":{"requests":1,"costPicoUSD":1,"p50Micros":1,"p95Micros":2,"p99Micros":3},"roles":{"state":{"requests":1,"costPicoUSD":1,"p50Micros":1,"p95Micros":2,"p99Micros":3}}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := AppendRatchetDelta(base, []byte(`{"schemaVersion":1,"provider":"gcs","profile":"regional","id":"002","budgets":[{"name":"move","provider":"gcs","profile":"regional","maximum":{"requests":1,"costPicoUSD":2,"p50Micros":2,"p95Micros":3,"p99Micros":4},"roles":{"state":{"requests":1,"costPicoUSD":2,"p50Micros":2,"p95Micros":3,"p99Micros":4}}},{"name":"copy","provider":"gcs","profile":"regional","maximum":{"requests":1},"roles":{"state":{"requests":1}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Epochs) != 2 || len(ledger.Epochs[1].Budgets) != 3 {
		t.Fatalf("materialized delta = %+v", ledger.Epochs)
	}
	if budget, found := ledger.Latest("read"); !found || budget.Maximum.Requests != 1 {
		t.Fatalf("inherited read budget = %+v, %t", budget, found)
	}
	if budget, found := ledger.Latest("move"); !found || budget.Maximum.Requests != 1 {
		t.Fatalf("tightened move budget = %+v, %t", budget, found)
	}
	if budget, found := ledger.Latest("copy"); !found || budget.Maximum.Requests != 1 {
		t.Fatalf("new copy budget = %+v, %t", budget, found)
	}
}

func TestRatchetDeltaFailsClosedOnLooseningRoleDriftAndMalformedInput(t *testing.T) {
	base, err := ParseRatchetLedger([]byte(`{"schemaVersion":1,"provider":"gcs","profile":"regional","epochs":[{"id":"001","budgets":[{"name":"move","provider":"gcs","profile":"regional","maximum":{"requests":1},"roles":{"state":{"requests":1}}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"loosen":      `{"schemaVersion":1,"provider":"gcs","profile":"regional","id":"002","budgets":[{"name":"move","provider":"gcs","profile":"regional","maximum":{"requests":2},"roles":{"state":{"requests":2}}}]}`,
		"role drift":  `{"schemaVersion":1,"provider":"gcs","profile":"regional","id":"002","budgets":[{"name":"move","provider":"gcs","profile":"regional","maximum":{"requests":1},"roles":{"state":{"requests":1},"file":{"requests":0}}}]}`,
		"duplicate":   `{"schemaVersion":1,"provider":"gcs","profile":"regional","id":"002","budgets":[{"name":"copy","provider":"gcs","profile":"regional","maximum":{"requests":1},"roles":{"state":{"requests":1}}},{"name":"copy","provider":"gcs","profile":"regional","maximum":{"requests":1},"roles":{"state":{"requests":1}}}]}`,
		"stale epoch": `{"schemaVersion":1,"provider":"gcs","profile":"regional","id":"001","budgets":[{"name":"copy","provider":"gcs","profile":"regional","maximum":{"requests":1},"roles":{"state":{"requests":1}}}]}`,
		"unknown":     `{"schemaVersion":1,"provider":"gcs","profile":"regional","id":"002","budgets":[{"name":"copy","provider":"gcs","profile":"regional","maximum":{"requests":1},"roles":{"state":{"requests":1}}}],"extra":true}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := AppendRatchetDelta(base, []byte(body)); err == nil {
				t.Fatal("invalid ratchet delta unexpectedly appended")
			}
		})
	}
}
