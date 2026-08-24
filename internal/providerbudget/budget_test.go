package providerbudget

import (
	"strings"
	"testing"
)

func TestBudgetChecksIndependentCountCostAndLatencyLimits(t *testing.T) {
	model, err := ParseModel(
		[]byte(`{"schemaVersion":1,"provider":"test","profile":"p","currency":"USD","sourceURL":"https://provider.example/pricing","effectiveDate":"2040-01-02","assumptions":["test"],"requests":{"object_get":{"billingClass":"class-b","unitRequests":1,"unitPricePicoUSD":10}}}`),
		[]byte(`{"schemaVersion":1,"provider":"test","profile":"p","sourceURL":"https://provider.example/latency","researchedDate":"2040-01-03","methodology":"test","limitations":["test"],"supportingSourceURLs":["https://provider.example/guidance"],"requests":{"object_get":{"p50Micros":1,"p95Micros":2,"p99Micros":3}}}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	events := []Event{{Role: RoleState, Kind: RequestObjectGet}, {Role: RoleState, Kind: RequestObjectGet}}
	valid := Budget{Name: "read", Provider: "test", Profile: "p", Maximum: Limits{Requests: 2, CostPicoUSD: 20, P50Micros: 2, P95Micros: 4, P99Micros: 6, CriticalP50Micros: 2, CriticalP95Micros: 4, CriticalP99Micros: 6}, Roles: map[Role]Limits{RoleState: {Requests: 2, CostPicoUSD: 20, P50Micros: 2, P95Micros: 4, P99Micros: 6}}}
	if _, err := valid.Check(model, events); err != nil {
		t.Fatal(err)
	}
	if _, err := valid.CheckRatchet(model, events); err != nil {
		t.Fatal(err)
	}
	if _, err := valid.CheckRatchet(model, events[:1]); err == nil || !strings.Contains(err.Error(), "append a tighter ratchet epoch") {
		t.Fatalf("CheckRatchet() improvement error = %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*Budget)
		want   string
	}{
		{name: "count", mutate: func(b *Budget) { b.Maximum.Requests = 1 }, want: "request count"},
		{name: "cost", mutate: func(b *Budget) { b.Maximum.CostPicoUSD = 19 }, want: "cost"},
		{name: "p50", mutate: func(b *Budget) { b.Maximum.P50Micros = 1 }, want: "p50"},
		{name: "p95", mutate: func(b *Budget) { b.Maximum.P95Micros = 3 }, want: "p95"},
		{name: "p99", mutate: func(b *Budget) { b.Maximum.P99Micros = 5 }, want: "p99"},
		{name: "critical-p50", mutate: func(b *Budget) { b.Maximum.CriticalP50Micros = 1 }, want: "critical-path p50"},
		{name: "critical-p95", mutate: func(b *Budget) { b.Maximum.CriticalP95Micros = 3 }, want: "critical-path p95"},
		{name: "critical-p99", mutate: func(b *Budget) { b.Maximum.CriticalP99Micros = 5 }, want: "critical-path p99"},
		{name: "role", mutate: func(b *Budget) { limit := b.Roles[RoleState]; limit.Requests = 1; b.Roles[RoleState] = limit }, want: "state request count"},
	} {
		t.Run(test.name, func(t *testing.T) {
			budget := valid
			budget.Roles = map[Role]Limits{RoleState: valid.Roles[RoleState]}
			test.mutate(&budget)
			if _, err := budget.Check(model, events); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Check() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestBudgetForbidsUnbudgetedRoles(t *testing.T) {
	model, err := ParseModel(
		[]byte(`{"schemaVersion":1,"provider":"test","profile":"p","currency":"USD","sourceURL":"https://provider.example/pricing","effectiveDate":"2040-01-02","assumptions":["test"],"requests":{"object_get":{"billingClass":"class-b","unitRequests":1,"unitPricePicoUSD":0}}}`),
		[]byte(`{"schemaVersion":1,"provider":"test","profile":"p","sourceURL":"https://provider.example/latency","researchedDate":"2040-01-03","methodology":"test","limitations":["test"],"supportingSourceURLs":["https://provider.example/guidance"],"requests":{"object_get":{"p50Micros":0,"p95Micros":0,"p99Micros":0}}}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	budget := Budget{Name: "metadata-only", Provider: "test", Profile: "p", Maximum: Limits{Requests: 1}, Roles: map[Role]Limits{RoleState: {Requests: 1}}}
	if _, err := budget.Check(model, []Event{{Role: RoleFile, Kind: RequestObjectGet}}); err == nil || !strings.Contains(err.Error(), "file role is not budgeted") {
		t.Fatalf("Check() error = %v", err)
	}
}
