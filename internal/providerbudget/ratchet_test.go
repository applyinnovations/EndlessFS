package providerbudget

import "testing"

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
