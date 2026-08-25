package gcs

import (
	_ "embed"

	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

// These fixtures intentionally model published marginal list pricing without
// free-tier allowances or negotiated discounts. The latency fixture is a
// conservative same-region Standard Storage model whose provenance and
// limitations are recorded in the fixture itself.
//
//go:embed economics/pricing-regional-standard-flat-2026-08.json
var regionalStandardFlatPricing []byte

//go:embed economics/latency-regional-standard-flat-2026-08.json
var regionalStandardFlatLatency []byte

//go:embed economics/budgets-regional-standard-flat-2026-08.json
var regionalStandardFlatBudgets []byte

// RegionalStandardFlatEconomics returns the reviewed provider model used by
// deterministic request-budget tests. It performs no network access.
func RegionalStandardFlatEconomics() (providerbudget.Model, error) {
	return providerbudget.ParseModel(regionalStandardFlatPricing, regionalStandardFlatLatency)
}

// RegionalStandardFlatBudgetRatchet returns the append-only operation ceilings
// for this provider profile. Later epochs may only retain or tighten them.
func RegionalStandardFlatBudgetRatchet() (providerbudget.RatchetLedger, error) {
	return providerbudget.ParseRatchetLedger(regionalStandardFlatBudgets)
}
