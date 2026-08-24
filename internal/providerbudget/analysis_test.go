package providerbudget

import (
	"context"
	"testing"
)

func TestEconomicsModelReportsBytesFailuresSubsystemsAndCriticalPath(t *testing.T) {
	model, err := ParseModel(
		[]byte(`{"schemaVersion":1,"provider":"test","profile":"research","currency":"USD","sourceURL":"https://provider.example/pricing","effectiveDate":"2040-01-02","assumptions":["test"],"requests":{"object_get":{"billingClass":"read","unitRequests":1,"unitPricePicoUSD":10},"object_put":{"billingClass":"write","unitRequests":1,"unitPricePicoUSD":20}}}`),
		[]byte(`{"schemaVersion":1,"provider":"test","profile":"research","sourceURL":"https://provider.example/latency","researchedDate":"2040-01-03","methodology":"test","limitations":["test"],"supportingSourceURLs":["https://provider.example/guidance"],"requests":{"object_get":{"p50Micros":10,"p95Micros":20,"p99Micros":30},"object_put":{"p50Micros":40,"p95Micros":50,"p99Micros":60}}}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	events := []Event{
		{Role: RoleState, Kind: RequestObjectGet, Operation: "move", Subsystem: "namespace-resolution", RequestBytes: 3, ResponseBytes: 100},
		{Role: RoleState, Kind: RequestObjectPut, Operation: "move", Subsystem: "immutable-preparation", ParallelGroup: "prepare", RequestBytes: 50},
		{Role: RoleState, Kind: RequestObjectPut, Operation: "move", Subsystem: "immutable-preparation", ParallelGroup: "prepare", RequestBytes: 30, Failed: true},
	}

	totals, err := model.Estimate(events)
	if err != nil {
		t.Fatal(err)
	}
	if totals.RequestBytes != 83 || totals.ResponseBytes != 100 || totals.FailedRequests != 1 {
		t.Fatalf("byte/failure totals = %+v", totals)
	}
	preparation := totals.BySubsystem["immutable-preparation"]
	if preparation.Requests != 2 || preparation.RequestBytes != 80 || preparation.FailedRequests != 1 {
		t.Fatalf("preparation totals = %+v", preparation)
	}
	if totals.CriticalP50Micros != 50 || totals.CriticalP95Micros != 70 || totals.CriticalP99Micros != 90 {
		t.Fatalf("critical path = p50 %d, p95 %d, p99 %d", totals.CriticalP50Micros, totals.CriticalP95Micros, totals.CriticalP99Micros)
	}
}

func TestTraceContextPropagatesOperationSubsystemAndParallelGroup(t *testing.T) {
	ctx := WithTrace(context.Background(), Trace{Operation: "trash", Subsystem: "namespace-commit", ParallelGroup: "publish"})
	trace := TraceFromContext(ctx)
	if trace.Operation != "trash" || trace.Subsystem != "namespace-commit" || trace.ParallelGroup != "publish" {
		t.Fatalf("trace = %+v", trace)
	}
	if trace := TraceFromContext(context.Background()); trace != (Trace{}) {
		t.Fatalf("empty trace = %+v", trace)
	}
}
