package providerbudget

import (
	"strings"
	"testing"
)

func TestEconomicsModelAggregatesCountCostAndLatency(t *testing.T) {
	pricing := []byte(`{
		"schemaVersion":1,
		"provider":"test",
		"profile":"regional-standard",
		"currency":"USD",
		"sourceURL":"https://provider.example/pricing",
		"effectiveDate":"2040-01-02",
		"assumptions":["test pricing assumptions"],
		"requests":{
			"object_get":{"billingClass":"class-b","unitRequests":1000,"unitPricePicoUSD":400000000},
			"object_put":{"billingClass":"class-a","unitRequests":1000,"unitPricePicoUSD":5000000000},
			"object_delete":{"billingClass":"free","unitRequests":1,"unitPricePicoUSD":0}
		}
	}`)
	latency := []byte(`{
		"schemaVersion":1,
		"provider":"test",
		"profile":"regional-standard",
		"sourceURL":"https://provider.example/latency",
		"researchedDate":"2040-01-03",
		"methodology":"deterministic test model",
		"limitations":["test-only values"],
		"supportingSourceURLs":["https://provider.example/guidance"],
		"requests":{
			"object_get":{"p50Micros":10000,"p95Micros":20000,"p99Micros":40000},
			"object_put":{"p50Micros":30000,"p95Micros":50000,"p99Micros":90000},
			"object_delete":{"p50Micros":8000,"p95Micros":15000,"p99Micros":30000}
		}
	}`)
	model, err := ParseModel(pricing, latency)
	if err != nil {
		t.Fatal(err)
	}
	totals, err := model.Estimate([]Event{
		{Role: RoleState, Kind: RequestObjectGet},
		{Role: RoleState, Kind: RequestObjectGet},
		{Role: RoleState, Kind: RequestObjectPut, RequestBytes: 1024},
		{Role: RoleState, Kind: RequestObjectDelete},
	})
	if err != nil {
		t.Fatal(err)
	}
	if totals.Requests != 4 || totals.CostPicoUSD != 5_800_000 || totals.P50Micros != 58_000 || totals.P95Micros != 105_000 || totals.P99Micros != 200_000 {
		t.Fatalf("Estimate() = %+v", totals)
	}
	if got := totals.ByRole[RoleState]; got.Requests != totals.Requests || got.CostPicoUSD != totals.CostPicoUSD {
		t.Fatalf("state totals = %+v, want %+v", got, totals)
	}
}

func TestEconomicsModelFailsClosed(t *testing.T) {
	validPricing := `{"schemaVersion":1,"provider":"test","profile":"p","currency":"USD","sourceURL":"https://provider.example/pricing","effectiveDate":"2040-01-02","assumptions":["test"],"requests":{"object_get":{"billingClass":"class-b","unitRequests":1,"unitPricePicoUSD":1}}}`
	validLatency := `{"schemaVersion":1,"provider":"test","profile":"p","sourceURL":"https://provider.example/latency","researchedDate":"2040-01-03","methodology":"test","limitations":["test"],"supportingSourceURLs":["https://provider.example/guidance"],"requests":{"object_get":{"p50Micros":1,"p95Micros":2,"p99Micros":3}}}`
	for _, test := range []struct {
		name    string
		pricing string
		latency string
	}{
		{name: "unknown pricing field", pricing: strings.Replace(validPricing, `"currency":"USD"`, `"currency":"USD","surprise":true`, 1), latency: validLatency},
		{name: "provider mismatch", pricing: validPricing, latency: strings.Replace(validLatency, `"provider":"test"`, `"provider":"other"`, 1)},
		{name: "missing latency", pricing: validPricing, latency: strings.Replace(validLatency, `"object_get"`, `"object_put"`, 1)},
		{name: "invalid percentile order", pricing: validPricing, latency: strings.Replace(validLatency, `"p50Micros":1,"p95Micros":2`, `"p50Micros":3,"p95Micros":2`, 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseModel([]byte(test.pricing), []byte(test.latency)); err == nil {
				t.Fatal("ParseModel() succeeded")
			}
		})
	}
}

func TestEconomicsModelRejectsUnclassifiedRequest(t *testing.T) {
	pricing := []byte(`{"schemaVersion":1,"provider":"test","profile":"p","currency":"USD","sourceURL":"https://provider.example/pricing","effectiveDate":"2040-01-02","assumptions":["test"],"requests":{"object_get":{"billingClass":"class-b","unitRequests":1,"unitPricePicoUSD":1}}}`)
	latency := []byte(`{"schemaVersion":1,"provider":"test","profile":"p","sourceURL":"https://provider.example/latency","researchedDate":"2040-01-03","methodology":"test","limitations":["test"],"supportingSourceURLs":["https://provider.example/guidance"],"requests":{"object_get":{"p50Micros":1,"p95Micros":2,"p99Micros":3}}}`)
	model, err := ParseModel(pricing, latency)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.Estimate([]Event{{Role: RoleFile, Kind: RequestKind("unknown")}}); err == nil {
		t.Fatal("Estimate() accepted unknown request kind")
	}
}
