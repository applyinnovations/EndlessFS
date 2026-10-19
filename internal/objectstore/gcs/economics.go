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

//go:embed economics/budgets-cleanup-migration-regional-standard-flat-2026-10.json
var regionalStandardFlatCleanupMigrationBudgets []byte

//go:embed economics/budgets-geometric-scale-regional-standard-flat-2026-10.json
var regionalStandardFlatGeometricBudgets []byte

//go:embed economics/budgets-scale-retirement-regional-standard-flat-2026-10.json
var regionalStandardFlatScaleRetirement []byte

//go:embed economics/budgets-upload-recovery-regional-standard-flat-2026-10.json
var regionalStandardFlatUploadRecovery []byte

//go:embed economics/budgets-schema-012-regional-standard-flat-2026-10.json
var regionalStandardFlatSchema012Budgets []byte

//go:embed economics/budgets-upload-pipeline-regional-standard-flat-2026-10.json
var regionalStandardFlatUploadPipelineBudgets []byte

//go:embed economics/budgets-upload-lifecycle-regional-standard-flat-2026-10.json
var regionalStandardFlatUploadLifecycleBudgets []byte

//go:embed economics/budgets-upload-admission-regional-standard-flat-2026-10.json
var regionalStandardFlatUploadAdmissionBudgets []byte

//go:embed economics/budgets-upload-corrective-regional-standard-flat-2026-10.json
var regionalStandardFlatUploadCorrectiveBudgets []byte

//go:embed economics/budgets-prepared-mutation-regional-standard-flat-2026-10.json
var regionalStandardFlatPreparedMutationBudgets []byte

//go:embed economics/budgets-derived-view-corrective-regional-standard-flat-2026-10.json
var regionalStandardFlatDerivedViewCorrectiveBudgets []byte

//go:embed economics/budgets-maintenance-corrective-regional-standard-flat-2026-10.json
var regionalStandardFlatMaintenanceCorrectiveBudgets []byte

//go:embed economics/budgets-immutable-pack-cache-regional-standard-flat-2026-10.json
var regionalStandardFlatImmutablePackCacheBudgets []byte

// Reuse of one validated checkpoint operation removes repeated GETs; later
// independent reachability reads retain their actual provider-presence checks.
//
//go:embed economics/budgets-checkpoint-session-regional-standard-flat-2026-10.json
var regionalStandardFlatCheckpointSessionBudgets []byte

// RegionalStandardFlatEconomics returns the reviewed provider model used by
// deterministic request-budget tests. It performs no network access.
func RegionalStandardFlatEconomics() (providerbudget.Model, error) {
	return providerbudget.ParseModel(regionalStandardFlatPricing, regionalStandardFlatLatency)
}

// RegionalStandardFlatBudgetRatchet returns the append-only operation ceilings
// for this provider profile. Active ceilings only tighten; reviewed qualification
// retirement keeps the original measured budgets in historical epochs.
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
		regionalStandardFlatCleanupMigrationBudgets,
		regionalStandardFlatGeometricBudgets,
		regionalStandardFlatScaleRetirement,
		regionalStandardFlatUploadRecovery,
		regionalStandardFlatSchema012Budgets,
		regionalStandardFlatUploadPipelineBudgets,
		regionalStandardFlatUploadLifecycleBudgets,
		regionalStandardFlatUploadAdmissionBudgets,
		regionalStandardFlatUploadCorrectiveBudgets,
		regionalStandardFlatPreparedMutationBudgets,
		regionalStandardFlatDerivedViewCorrectiveBudgets,
		regionalStandardFlatMaintenanceCorrectiveBudgets,
		regionalStandardFlatImmutablePackCacheBudgets,
		regionalStandardFlatCheckpointSessionBudgets,
	} {
		ledger, err = providerbudget.AppendRatchetDelta(ledger, delta)
		if err != nil {
			return providerbudget.RatchetLedger{}, err
		}
	}
	return ledger, nil
}
