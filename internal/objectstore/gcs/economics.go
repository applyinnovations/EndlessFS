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

//go:embed economics/budgets-schema-009-regional-standard-flat-2026-08.json
var regionalStandardFlatSchema009Budgets []byte

//go:embed economics/budgets-schema-010-regional-standard-flat-2026-08.json
var regionalStandardFlatSchema010Budgets []byte

//go:embed economics/budgets-smart-upload-regional-standard-flat-2026-08.json
var regionalStandardFlatSmartUploadBudgets []byte

//go:embed economics/budgets-provider-workflows-regional-standard-flat-2026-09.json
var regionalStandardFlatProviderWorkflowBudgets []byte

//go:embed economics/budgets-schema-011-regional-standard-flat-2026-09.json
var regionalStandardFlatSchema011Budgets []byte

// RegionalStandardFlatEconomics returns the reviewed provider model used by
// deterministic request-budget tests. It performs no network access.
func RegionalStandardFlatEconomics() (providerbudget.Model, error) {
	return providerbudget.ParseModel(regionalStandardFlatPricing, regionalStandardFlatLatency)
}

// RegionalStandardFlatBudgetRatchet returns the append-only operation ceilings
// for this provider profile. Later epochs may only retain or tighten them.
func RegionalStandardFlatBudgetRatchet() (providerbudget.RatchetLedger, error) {
	ledger, err := providerbudget.ParseRatchetLedger(regionalStandardFlatBudgets)
	if err != nil {
		return providerbudget.RatchetLedger{}, err
	}
	for _, delta := range [][]byte{
		regionalStandardFlatSchema009Budgets,
		regionalStandardFlatSchema010Budgets,
		regionalStandardFlatSmartUploadBudgets,
		regionalStandardFlatProviderWorkflowBudgets,
		regionalStandardFlatSchema011Budgets,
	} {
		ledger, err = providerbudget.AppendRatchetDelta(ledger, delta)
		if err != nil {
			return providerbudget.RatchetLedger{}, err
		}
	}
	return ledger, nil
}
