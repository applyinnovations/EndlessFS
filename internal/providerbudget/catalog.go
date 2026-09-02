package providerbudget

// ProductionWorkload is the completeness boundary for provider economics.
// Budgets names the deterministic executable ratchets that cover the workload;
// a composed HTTP route may depend on several workloads without inventing an
// arbitrary route-level request ceiling.
type ProductionWorkload struct {
	ID       string
	Category string
	Budgets  []string
}

// ProductionRoute records every HTTP use case that can cross an object-store
// boundary. Local-only routes are listed separately so a newly registered
// route must be deliberately classified on one side of the boundary.
type ProductionRoute struct {
	Pattern     string
	Cardinality string
	Workloads   []string
}

// BudgetExecution composes an exact provider ratchet into a production-scale
// workflow. Executions captures logical repetition; Parallelism records the
// actual bounded worker pool used for critical-path estimates without changing
// aggregate request count or marginal cost.
type BudgetExecution struct {
	Budget      string
	Executions  int64
	Parallelism int64
}

// ProductionScaleScenario is the cardinality audit for workflows where a UI
// state or one user intent can represent many logical items. The component
// budgets remain the append-only ceilings; this catalog prevents client/API
// composition from silently multiplying a cheap primitive per rendered item.
type ProductionScaleScenario struct {
	ID              string
	Category        string
	LogicalItems    int64
	BrowserRequests int64
	RequestBasis    string
	Executions      []BudgetExecution
}

// ProviderRequestWave is an evidence-backed request shape for a production
// efficiency target. Count is provider wire operations, Parallelism is the
// bounded concurrency that the architecture must prove, and byte counts make
// packed-segment latency explicit rather than treating metadata as free.
type ProviderRequestWave struct {
	Role              Role
	Kind              RequestKind
	Count             int64
	Parallelism       int64
	RequestBytesEach  int64
	ResponseBytesEach int64
}

// ProductionScaleTarget is deliberately separate from an observed ratchet.
// Ratchets prevent the implementation from getting worse; targets describe
// the lower-cost architecture the implementation is required to converge on.
// BaselineScenario is empty only when the target adds a missing scale proof.
type ProductionScaleTarget struct {
	ID                     string
	BaselineScenario       string
	Category               string
	LogicalItems           int64
	MaximumBrowserRequests int64
	FeasibilityBasis       string
	RequestWaves           []ProviderRequestWave
}

func execution(budget string, executions, parallelism int64) BudgetExecution {
	return BudgetExecution{Budget: budget, Executions: executions, Parallelism: parallelism}
}

func targetWave(role Role, kind RequestKind, count, parallelism, requestBytesEach, responseBytesEach int64) ProviderRequestWave {
	return ProviderRequestWave{
		Role: role, Kind: kind, Count: count, Parallelism: parallelism,
		RequestBytesEach: requestBytesEach, ResponseBytesEach: responseBytesEach,
	}
}

const (
	targetTransactionSegmentBytes = int64(4 << 20)
	targetHeadBytes               = int64(64 << 10)
	targetPreviewBytes            = int64(256 << 10)
)

// ProductionScaleTargets records the required next architecture rather than
// blessing today's measurements. The common mutation shape is an authentication
// read, a bounded head read, one immutable 4 MiB metadata-pack read and write,
// and one conditional head publication. The 1 MiB control-plane request limit
// makes that segment a conservative envelope for the compact intent; per-item
// success records are reconstructed rather than copied into an outcome tree.
// The common packed-read shape is three serial authority/head reads plus at
// most two concurrent 4 MiB projection segments. Workloads larger than these byte
// envelopes scale by encoded segment count, never by one provider transaction
// per logical item.
func ProductionScaleTargets() []ProductionScaleTarget {
	packedRead := func() []ProviderRequestWave {
		return []ProviderRequestWave{
			targetWave(RoleState, RequestObjectGet, 3, 1, 0, targetHeadBytes),
			targetWave(RoleState, RequestObjectGet, 2, 2, 0, targetTransactionSegmentBytes),
		}
	}
	mutation := func(segmentCount int64) []ProviderRequestWave {
		return []ProviderRequestWave{
			targetWave(RoleState, RequestObjectGet, 2, 1, 0, targetHeadBytes),
			targetWave(RoleState, RequestObjectGet, 1, 1, 0, targetTransactionSegmentBytes),
			targetWave(RoleState, RequestObjectPut, segmentCount, segmentCount, targetTransactionSegmentBytes, 0),
			targetWave(RoleState, RequestObjectPut, 1, 1, targetHeadBytes, 0),
		}
	}
	replay := func() []ProviderRequestWave {
		return []ProviderRequestWave{
			targetWave(RoleState, RequestObjectGet, 3, 1, 0, targetHeadBytes),
			targetWave(RoleState, RequestObjectGet, 1, 1, 0, targetTransactionSegmentBytes),
		}
	}
	denial := func() []ProviderRequestWave {
		return []ProviderRequestWave{
			targetWave(RoleState, RequestObjectGet, 2, 1, 0, targetHeadBytes),
			targetWave(RoleState, RequestObjectGet, 1, 1, 0, targetTransactionSegmentBytes),
		}
	}
	transferAdmission := func() []ProviderRequestWave {
		return []ProviderRequestWave{
			targetWave(RoleFile, RequestUploadBegin, 10_000, 100, 0, 0),
			targetWave(RoleState, RequestObjectGet, 2, 1, 0, targetHeadBytes),
			// Ten lease checkpoints are irreducible if crash orphaning is to stay
			// at most 1,000 sessions; the packed admission facts and head CAS are
			// the remaining two publications.
			targetWave(RoleState, RequestObjectPut, 10, 1, targetTransactionSegmentBytes, 0),
			targetWave(RoleState, RequestObjectPut, 1, 1, targetTransactionSegmentBytes, 0),
			targetWave(RoleState, RequestObjectPut, 1, 1, targetHeadBytes, 0),
		}
	}
	transferCompletion := func() []ProviderRequestWave {
		return []ProviderRequestWave{
			targetWave(RoleFile, RequestObjectVerify, 10_000, 100, 0, 0),
			targetWave(RoleState, RequestObjectGet, 2, 1, 0, targetHeadBytes),
			targetWave(RoleState, RequestObjectGet, 1, 1, 0, targetTransactionSegmentBytes),
			targetWave(RoleState, RequestObjectPut, 9, 1, targetTransactionSegmentBytes, 0),
			targetWave(RoleState, RequestObjectPut, 1, 1, targetTransactionSegmentBytes, 0),
			targetWave(RoleState, RequestObjectPut, 1, 1, targetHeadBytes, 0),
		}
	}
	transferCancellation := func() []ProviderRequestWave {
		return []ProviderRequestWave{
			targetWave(RoleFile, RequestUploadAbort, 10_000, 100, 0, 0),
			targetWave(RoleState, RequestObjectGet, 2, 1, 0, targetHeadBytes),
			targetWave(RoleState, RequestObjectGet, 2, 1, 0, targetTransactionSegmentBytes),
			targetWave(RoleState, RequestObjectPut, 9, 1, targetTransactionSegmentBytes, 0),
			targetWave(RoleState, RequestObjectPut, 1, 1, targetHeadBytes, 0),
		}
	}
	return []ProductionScaleTarget{
		{ID: "restored-transfer-ledger-needs-source", BaselineScenario: "restored-transfer-ledger-needs-source", Category: "startup", LogicalItems: 10_000, MaximumBrowserRequests: 0, FeasibilityBasis: "Dormant device-local history has no provider authority to reconcile."},
		{ID: "browse-live-directory", BaselineScenario: "browse-live-directory", Category: "namespace-read", LogicalItems: 10_000, MaximumBrowserRequests: 1, FeasibilityBasis: "One progressively decoded response reads one authenticated snapshot and at most two parallel packed projection segments totaling 8 MiB; the measured verbose 10,000-row envelope is 6.05 MB.", RequestWaves: packedRead()},
		{ID: "browse-trash", BaselineScenario: "browse-trash", Category: "namespace-read", LogicalItems: 10_000, MaximumBrowserRequests: 1, FeasibilityBasis: "Trash is another projection of the same owner namespace and uses the same progressively decoded packed-read envelope.", RequestWaves: packedRead()},
		{ID: "copy-selection", BaselineScenario: "copy-selection", Category: "namespace-mutation", LogicalItems: 10_000, MaximumBrowserRequests: 1, FeasibilityBasis: "The accepted request is at most 1 MiB; one 4 MiB compact-intent segment and one head CAS publish it without moving object bytes.", RequestWaves: mutation(1)},
		{ID: "move-selection", BaselineScenario: "move-selection", Category: "namespace-mutation", LogicalItems: 10_000, MaximumBrowserRequests: 1, FeasibilityBasis: "The accepted request is at most 1 MiB; one 4 MiB compact-intent segment and one head CAS publish it without moving object bytes.", RequestWaves: mutation(1)},
		{ID: "trash-selection", BaselineScenario: "trash-selection", Category: "namespace-mutation", LogicalItems: 10_000, MaximumBrowserRequests: 1, FeasibilityBasis: "Trash is a compact edge-state transition in one segment, not 10,000 tree rewrites or retained success rows.", RequestWaves: mutation(1)},
		{ID: "restore-selection", BaselineScenario: "restore-selection", Category: "namespace-mutation", LogicalItems: 10_000, MaximumBrowserRequests: 1, FeasibilityBasis: "Original destinations are bound into the Trash snapshot proof and published as one compact transaction segment.", RequestWaves: mutation(1)},
		{ID: "permanent-delete-selection", BaselineScenario: "permanent-delete-selection", Category: "namespace-mutation", LogicalItems: 10_000, MaximumBrowserRequests: 1, FeasibilityBasis: "Deletion records compact route identities in one segment and reconstructs successful item results on replay.", RequestWaves: mutation(1)},
		{ID: "trash-selection-replay", BaselineScenario: "trash-selection-replay", Category: "recovery", LogicalItems: 10_000, MaximumBrowserRequests: 1, FeasibilityBasis: "The request deterministically reconstructs item results; replay reads only authority, head, and an optional compact exception segment.", RequestWaves: replay()},
		{ID: "restore-selection-replay", BaselineScenario: "restore-selection-replay", Category: "recovery", LogicalItems: 10_000, MaximumBrowserRequests: 1, FeasibilityBasis: "The request deterministically reconstructs item results; replay reads only authority, head, and an optional compact exception segment.", RequestWaves: replay()},
		{ID: "permanent-delete-selection-replay", BaselineScenario: "permanent-delete-selection-replay", Category: "recovery", LogicalItems: 10_000, MaximumBrowserRequests: 1, FeasibilityBasis: "The durable transaction descriptor is sufficient; no paged per-item outcome tree is read.", RequestWaves: replay()},
		{ID: "trash-selection-denied", BaselineScenario: "trash-selection-denied", Category: "denial", LogicalItems: 10_000, MaximumBrowserRequests: 1, FeasibilityBasis: "A mismatched signed namespace revision denies before transaction segments are written.", RequestWaves: denial()},
		{ID: "smart-upload-size-planning", BaselineScenario: "smart-upload-size-planning", Category: "transfer-planning", LogicalItems: 10_000, MaximumBrowserRequests: 1, FeasibilityBasis: "One packed size/fingerprint projection query covers 10,000 local sizes.", RequestWaves: packedRead()},
		{ID: "smart-upload-all-size-candidates", BaselineScenario: "smart-upload-all-size-candidates", Category: "transfer-planning", LogicalItems: 10_000, MaximumBrowserRequests: 2, FeasibilityBasis: "Only candidates make a second packed projection query after client hashing.", RequestWaves: append(packedRead(), packedRead()...)},
		{ID: "upload-admission", BaselineScenario: "upload-admission", Category: "transfer-active", LogicalItems: 10_000, MaximumBrowserRequests: 1, FeasibilityBasis: "GCS requires one resumable-session initiation per real object; ten 1,000-lease checkpoints bound orphaning, while one packed admission object and one head CAS preserve atomic authority.", RequestWaves: transferAdmission()},
		{ID: "upload-completion", BaselineScenario: "upload-completion", Category: "transfer-active", LogicalItems: 10_000, MaximumBrowserRequests: 1, FeasibilityBasis: "One provider checksum/metadata verification per completed object is retained; nine intermediate checkpoints plus the atomic packed namespace publication bound restart rework to at most 1,000 items.", RequestWaves: transferCompletion()},
		{ID: "upload-cancellation", BaselineScenario: "upload-cancellation", Category: "transfer-active", LogicalItems: 10_000, MaximumBrowserRequests: 1, FeasibilityBasis: "GCS exposes one abort per active resumable session; nine intermediate checkpoints plus one compact abort-bitmap head CAS bound restart rework to at most 1,000 items without rewriting admission records.", RequestWaves: transferCancellation()},
		{ID: "visible-grid-preview-resolution", BaselineScenario: "visible-grid-preview-resolution", Category: "preview", LogicalItems: 10_000, MaximumBrowserRequests: 1, FeasibilityBasis: "One batched control-plane resolution reads a single visible-window projection segment plus 32 unavoidable direct thumbnail reads.", RequestWaves: append([]ProviderRequestWave{
			targetWave(RoleState, RequestObjectGet, 3, 1, 0, targetHeadBytes),
			targetWave(RolePreviewState, RequestObjectGet, 1, 1, 0, targetTransactionSegmentBytes),
		}, targetWave(RolePreviewArtifact, RequestDataDownload, 32, 8, 0, targetPreviewBytes))},
		{ID: "checkpoint-garbage-collection", BaselineScenario: "checkpoint-garbage-collection", Category: "maintenance", LogicalItems: 128, MaximumBrowserRequests: 0, FeasibilityBasis: "Each garbage object requires one billed-as-free delete; two manifest reads and one cursor/head publication bound orchestration.", RequestWaves: []ProviderRequestWave{
			targetWave(RoleState, RequestObjectGet, 2, 1, 0, targetHeadBytes),
			targetWave(RoleState, RequestObjectDelete, 128, 100, 0, 0),
			targetWave(RoleState, RequestObjectPut, 1, 1, targetHeadBytes, 0),
		}},
		{ID: "domain-compaction", BaselineScenario: "domain-compaction", Category: "maintenance", LogicalItems: 300, MaximumBrowserRequests: 0, FeasibilityBasis: "The calibrated 300-item compacted base fits one 4 MiB segment followed by one conditional head replacement.", RequestWaves: mutation(1)},
		{ID: "move-subtree-million-descendants", Category: "namespace-mutation", LogicalItems: 1_000_000, MaximumBrowserRequests: 1, FeasibilityBasis: "A directory move changes one parent edge and preserves the immutable child root; descendant cardinality performs no provider work.", RequestWaves: []ProviderRequestWave{
			targetWave(RoleState, RequestObjectGet, 3, 1, 0, targetHeadBytes),
			targetWave(RoleState, RequestObjectPut, 1, 1, targetHeadBytes, 0),
		}},
	}
}

// ProductionScaleScenarios records the largest deterministic workloads in the
// production surface. A zero-execution scenario is intentional proof that
// dormant client state does not contact the provider.
func ProductionScaleScenarios() []ProductionScaleScenario {
	return []ProductionScaleScenario{
		{ID: "restored-transfer-ledger-needs-source", Category: "startup", LogicalItems: 10_000, BrowserRequests: 0, RequestBasis: "Dormant history is local IndexedDB state; provider reconciliation is deferred until a source file is reacquired."},
		{ID: "browse-live-directory", Category: "namespace-read", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "One progressively decoded 10,000-entry response.", Executions: []BudgetExecution{execution("session-authenticate-schema-011", 1, 1), execution("namespace-list-page-10000-schema-011", 1, 1)}},
		{ID: "browse-trash", Category: "namespace-read", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "One progressively decoded 10,000-entry response.", Executions: []BudgetExecution{execution("session-authenticate-schema-011", 1, 1), execution("namespace-list-page-10000-schema-011", 1, 1)}},
		{ID: "copy-selection", Category: "namespace-mutation", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "One atomic packed owner-namespace batch.", Executions: []BudgetExecution{execution("session-authenticate-schema-011", 1, 1), execution("batch-copy-10000-schema-011", 1, 1)}},
		{ID: "move-selection", Category: "namespace-mutation", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "One atomic packed owner-namespace batch.", Executions: []BudgetExecution{execution("session-authenticate-schema-011", 1, 1), execution("batch-move-10000-schema-011", 1, 1)}},
		{ID: "trash-selection", Category: "namespace-mutation", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "One atomic packed owner-namespace batch.", Executions: []BudgetExecution{execution("session-authenticate-schema-011", 1, 1), execution("trash-batch-10000-schema-011", 1, 1)}},
		{ID: "restore-selection", Category: "namespace-mutation", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "One atomic packed owner-namespace batch.", Executions: []BudgetExecution{execution("session-authenticate-schema-011", 1, 1), execution("restore-batch-10000-schema-011", 1, 1)}},
		{ID: "permanent-delete-selection", Category: "namespace-mutation", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "One atomic packed owner-namespace batch.", Executions: []BudgetExecution{execution("session-authenticate-schema-011", 1, 1), execution("empty-trash-10000-schema-011", 1, 1)}},
		{ID: "trash-selection-replay", Category: "recovery", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "Retry reconstructs results from the compact transaction descriptor.", Executions: []BudgetExecution{execution("session-authenticate-schema-011", 1, 1), execution("trash-batch-10000-replay-schema-011", 1, 1)}},
		{ID: "restore-selection-replay", Category: "recovery", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "Retry reconstructs results from the compact transaction descriptor.", Executions: []BudgetExecution{execution("session-authenticate-schema-011", 1, 1), execution("restore-batch-10000-replay-schema-011", 1, 1)}},
		{ID: "permanent-delete-selection-replay", Category: "recovery", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "Retry reconstructs results from the compact transaction descriptor.", Executions: []BudgetExecution{execution("session-authenticate-schema-011", 1, 1), execution("empty-trash-10000-replay-schema-011", 1, 1)}},
		{ID: "trash-selection-denied", Category: "denial", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "A mismatched signed revision denies before publication.", Executions: []BudgetExecution{execution("session-authenticate-schema-011", 1, 1), execution("trash-batch-10000-denied-schema-011", 1, 1)}},
		{ID: "smart-upload-size-planning", Category: "transfer-planning", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "One packed metadata-only query covers 10,000 local sizes.", Executions: []BudgetExecution{execution("session-authenticate-schema-011", 1, 1), execution("upload-plan-sizes-10000-schema-011", 1, 1)}},
		{ID: "smart-upload-all-size-candidates", Category: "transfer-planning", LogicalItems: 10_000, BrowserRequests: 2, RequestBasis: "One size request followed by one fingerprint request only for candidates.", Executions: []BudgetExecution{execution("session-authenticate-schema-011", 2, 2), execution("upload-plan-sizes-10000-schema-011", 1, 1), execution("upload-plan-fingerprints-10000-schema-011", 1, 1)}},
		{ID: "upload-admission", Category: "transfer-active", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "One progressive batch response; each real object necessarily creates one provider upload session.", Executions: []BudgetExecution{execution("session-authenticate-schema-011", 1, 1), execution("file-create-upload-batch-10000-schema-011", 1, 1)}},
		{ID: "upload-completion", Category: "transfer-active", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "One progressive completion request records bounded durable progress.", Executions: []BudgetExecution{execution("session-authenticate-schema-011", 1, 1), execution("file-complete-upload-batch-10000-schema-011", 1, 1)}},
		{ID: "upload-cancellation", Category: "transfer-active", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "One progressive cancellation request records bounded durable progress.", Executions: []BudgetExecution{execution("session-authenticate-schema-011", 1, 1), execution("file-abort-upload-batch-10000-schema-011", 1, 1)}},
		{ID: "visible-grid-preview-resolution", Category: "preview", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "One control-plane resolution plus 32 direct visible-window thumbnail reads.", Executions: []BudgetExecution{execution("session-authenticate-schema-011", 1, 1), execution("namespace-stat-schema-011", 1, 1), execution("preview-ready-window-32-schema-011", 1, 1)}},
		{ID: "checkpoint-garbage-collection", Category: "maintenance", LogicalItems: 128, BrowserRequests: 0, RequestBasis: "One authenticated checkpoint-bound garbage plan and bounded conditional sweep.", Executions: []BudgetExecution{execution("maintenance-checkpoint-garbage-128-schema-011", 1, 1)}},
		{ID: "domain-compaction", Category: "maintenance", LogicalItems: 300, BrowserRequests: 0, RequestBasis: "One packed compaction run over the calibrated domain history.", Executions: []BudgetExecution{execution("maintenance-domain-compaction-300-schema-011", 1, 1)}},
	}
}

func workload(id, category string, budgets ...string) ProductionWorkload {
	return ProductionWorkload{ID: id, Category: category, Budgets: append([]string(nil), budgets...)}
}

// ProductionWorkloads returns every production behavior that can issue an
// object-provider request. Cryptography, validation, HTML, and embedded assets
// are intentionally absent because they do not cross that boundary.
func ProductionWorkloads() []ProductionWorkload {
	workloads := []ProductionWorkload{
		workload("state/get", "control", "state-get-schema-011", "state-get-missing-schema-011"),
		workload("state/list", "control", "state-list-empty-schema-011"),
		workload("state/create", "control", "state-create-schema-011"),
		workload("state/compare-and-swap", "control", "state-compare-and-swap-schema-011"),
		workload("state/delete", "control", "state-delete-schema-011"),
		workload("state/mutate", "control", "state-mutate-two-records-schema-011"),
		workload("state/transact", "control", "state-transact-two-domains-schema-011"),

		workload("namespace/list", "namespace-read", "namespace-list-page-schema-011", "namespace-list-page-10000-schema-011"),
		workload("namespace/lookup-children", "namespace-read", "namespace-lookup-children-schema-011"),
		workload("namespace/stat", "namespace-read", "namespace-stat-schema-011"),
		workload("namespace/create-directory", "namespace-mutation", "file-create-directory-schema-011"),
		workload("namespace/copy", "namespace-mutation", "direct-copy-one-file-schema-011", "batch-copy-10000-schema-011"),
		workload("namespace/move", "namespace-mutation", "direct-move-one-file-schema-011", "batch-move-10000-schema-011"),
		workload("namespace/batch-copy-move", "namespace-mutation", "batch-copy-10000-schema-011", "batch-move-10000-schema-011"),
		workload("namespace/trash", "namespace-mutation", "trash-one-file-schema-011", "trash-batch-10000-schema-011", "trash-batch-10000-replay-schema-011", "trash-batch-10000-denied-schema-011"),
		workload("namespace/restore", "namespace-mutation", "restore-one-file-schema-011", "restore-batch-10000-schema-011", "restore-batch-10000-replay-schema-011"),
		workload("namespace/delete", "namespace-mutation", "direct-delete-one-file-schema-011"),
		workload("namespace/delete-trash", "namespace-mutation", "permanent-delete-one-file-schema-011", "empty-trash-10000-schema-011", "empty-trash-10000-replay-schema-011"),
		workload("namespace/get-operation", "namespace-read", "namespace-get-operation-schema-011"),

		workload("transfer/create-upload", "transfer", "file-create-upload-cold-schema-011", "file-create-upload-warm-schema-011"),
		workload("transfer/create-upload-batch", "transfer", "file-create-upload-batch-100-schema-011", "file-create-upload-batch-10000-schema-011"),
		workload("transfer/complete-upload-batch", "transfer", "file-complete-upload-batch-10000-schema-011"),
		workload("transfer/abort-upload-batch", "transfer", "file-abort-upload-batch-10000-schema-011"),
		workload("transfer/plan-upload-sizes", "derived-read", "upload-plan-index-cold-256-schema-011", "upload-plan-index-incremental-one-schema-011", "upload-plan-sizes-10000-schema-011"),
		workload("transfer/plan-upload-fingerprints", "derived-read", "upload-plan-fingerprints-10000-schema-011"),
		workload("transfer/upload-status", "transfer", "file-upload-status-active-schema-011"),
		workload("transfer/complete-upload", "transfer", "file-complete-upload-schema-011"),
		workload("transfer/abort-upload", "transfer", "file-abort-upload-schema-011"),
		workload("transfer/create-download", "transfer", "file-create-download-schema-011"),

		workload("duplicates/list-groups", "derived-read", "duplicates-list-groups-schema-011"),
		workload("duplicates/list-occurrences", "derived-read", "duplicates-list-occurrences-schema-011"),
		workload("duplicates/set-group-ignored", "control", "duplicates-set-group-ignored-schema-011"),
		workload("duplicates/compare-directories", "derived-read", "duplicates-compare-directories-schema-011"),
		workload("duplicates/list-directory-overlaps", "derived-read", "duplicates-list-directory-overlaps-schema-011"),
		workload("duplicates/set-directory-ignored", "control", "duplicates-set-directory-ignored-schema-011"),
		workload("duplicates/preview-reconciliation", "derived-read", "duplicates-preview-reconciliation-schema-011"),
		workload("duplicates/validate-reconciliation", "derived-read", "duplicates-validate-reconciliation-schema-011"),
		workload("duplicates/apply-reconciliation", "namespace-mutation", "duplicates-apply-reconciliation-schema-011"),

		workload("preview/validate", "preview", "preview-validate"),
		workload("preview/check", "preview", "preview-check"),
		workload("preview/claim", "preview", "preview-claim-new"),
		workload("preview/release", "preview", "preview-release"),
		workload("preview/commit", "preview", "preview-commit"),
		workload("preview/latest", "preview", "preview-latest"),
		workload("preview/read", "preview", "preview-read"),
		workload("preview/create-download", "preview", "preview-create-download"),
		workload("preview/resolve-ready", "preview", "preview-resolve-ready", "preview-ready-window-32-schema-011"),
		workload("preview/record-ready", "preview", "preview-record-ready"),
		workload("preview/create-known-download", "preview", "preview-create-known-download"),

		workload("data-plane/upload", "data-plane", "file-data-upload-four-bytes"),
		workload("data-plane/download", "data-plane", "file-data-download-four-bytes"),

		workload("session/issue", "session", "session-issue-schema-011"),
		workload("session/authenticate", "session", "session-authenticate-schema-011"),
		workload("session/rotate", "session", "session-rotate-schema-011"),
		workload("session/logout", "session", "session-logout-schema-011"),
		workload("session/revoke-user", "session", "session-revoke-user-schema-011"),

		workload("identity/bootstrap-options", "identity", "identity-bootstrap-options-schema-011"),
		workload("identity/bootstrap-verify", "identity", "identity-bootstrap-verify-schema-011"),
		workload("identity/registration-options", "identity", "identity-registration-options-schema-011", "identity-invited-registration-options-schema-011"),
		workload("identity/registration-verify", "identity", "identity-registration-verify-schema-011", "identity-invited-registration-verify-schema-011"),
		workload("identity/authentication-options", "identity", "identity-authentication-options-schema-011"),
		workload("identity/authentication-verify", "identity", "identity-authentication-verify-schema-011"),
		workload("identity/current-user", "identity", "identity-current-user-schema-011"),
		workload("identity/update-profile", "identity", "identity-update-profile-schema-011"),
		workload("identity/list-passkeys", "identity", "identity-list-passkeys-schema-011"),
		workload("identity/add-passkey-options", "identity", "identity-add-passkey-options-schema-011"),
		workload("identity/add-passkey-verify", "identity", "identity-add-passkey-verify-schema-011"),
		workload("identity/remove-passkey", "identity", "identity-remove-passkey-schema-011"),
		workload("identity/list-invites", "identity", "identity-list-invites-schema-011"),
		workload("identity/create-invite", "identity", "identity-create-invite-schema-011"),
		workload("identity/revoke-invite", "identity", "identity-revoke-invite-schema-011"),
		workload("identity/list-admin-users", "identity", "identity-list-admin-users-schema-011"),
		workload("identity/disable-user", "identity", "identity-disable-user-schema-011"),
		workload("identity/enable-user", "identity", "identity-enable-user-schema-011"),
		workload("identity/grant-admin", "identity", "identity-grant-admin-schema-011"),
		workload("identity/revoke-admin", "identity", "identity-revoke-admin-schema-011"),
		workload("identity/create-recovery", "identity", "identity-create-recovery-schema-011"),
		workload("identity/recovery-options", "identity", "identity-recovery-options-schema-011"),
		workload("identity/recovery-verify", "identity", "identity-recovery-verify-schema-011"),

		workload("share/create", "share", "share-create-schema-011"),
		workload("share/list", "share", "share-list-schema-011"),
		workload("share/revoke", "share", "share-revoke-schema-011"),
		workload("share/public-list", "share", "share-public-list-schema-011"),
		workload("share/public-stat", "share", "share-public-stat-schema-011"),
		workload("share/public-download", "share", "share-public-download-schema-011"),

		workload("theme/get", "preference", "theme-get-default-schema-011"),
		workload("theme/set", "preference", "theme-set-create-schema-011", "theme-set-update-schema-011"),

		workload("maintenance/startup", "maintenance", "maintenance-startup-warm-schema-011"),
		workload("maintenance/gate-status", "maintenance", "maintenance-gate-status-schema-011"),
		workload("maintenance/checkpoint", "maintenance", "maintenance-checkpoint-minimal-schema-011", "maintenance-checkpoint-garbage-128-schema-011"),
		workload("maintenance/verify-checkpoint", "maintenance", "maintenance-verify-checkpoint-minimal-schema-011"),
		workload("maintenance/visit-checkpoint", "maintenance", "maintenance-visit-checkpoint-minimal-schema-011"),
		workload("maintenance/open-writes", "maintenance", "maintenance-open-writes-schema-011"),
		workload("maintenance/compaction", "maintenance", "maintenance-domain-compaction-300-schema-011"),
		workload("maintenance/recovery", "maintenance", "maintenance-transition-recovery-schema-011"),
		workload("maintenance/derived-view-rebuild", "maintenance", "maintenance-derived-view-rebuild-schema-011"),
		workload("maintenance/migration", "maintenance", "maintenance-migration-008-to-011-minimal-fixture"),
	}
	return append([]ProductionWorkload(nil), workloads...)
}

func route(pattern, cardinality string, workloads ...string) ProductionRoute {
	return ProductionRoute{Pattern: pattern, Cardinality: cardinality, Workloads: append([]string(nil), workloads...)}
}

func protected(pattern, workload string) ProductionRoute {
	return route(pattern, "bounded", "session/authenticate", workload)
}

func ProductionProviderRoutes() []ProductionRoute {
	routes := []ProductionRoute{
		route("POST /api/v1/bootstrap/options", "one ceremony", "identity/bootstrap-options"),
		route("POST /api/v1/bootstrap/verify", "one bootstrap registration", "identity/bootstrap-verify"),
		route("POST /api/v1/registration/options", "one public or invited ceremony", "identity/registration-options"),
		route("POST /api/v1/registration/verify", "one public or invited registration", "identity/registration-verify"),
		route("POST /api/v1/authentication/options", "one ceremony", "identity/authentication-options"),
		route("POST /api/v1/authentication/verify", "one credential and ceremony", "identity/authentication-verify"),
		route("POST /api/v1/logout", "one session", "session/authenticate", "session/logout"),
		protected("GET /api/v1/me", "identity/current-user"),
		protected("PATCH /api/v1/me", "identity/update-profile"),
		protected("GET /api/v1/me/passkeys", "identity/list-passkeys"),
		route("POST /api/v1/me/passkeys/options", "one ceremony", "session/authenticate", "identity/add-passkey-options"),
		route("POST /api/v1/me/passkeys/verify", "one passkey registration", "session/authenticate", "identity/add-passkey-verify"),
		route("DELETE /api/v1/me/passkeys/{credentialID}", "one owner mutation", "session/authenticate", "identity/remove-passkey"),
		route("GET /api/v1/admin/invites", "one capability page", "session/authenticate", "identity/list-invites"),
		route("POST /api/v1/admin/invites", "one capability", "session/authenticate", "identity/create-invite"),
		route("DELETE /api/v1/admin/invites/{inviteID}", "one capability", "session/authenticate", "identity/revoke-invite"),
		route("GET /api/v1/admin/users", "one administrative page", "session/authenticate", "identity/list-admin-users"),
		route("POST /api/v1/admin/users/{userID}/disable", "administration plus owner epoch", "session/authenticate", "identity/disable-user"),
		route("POST /api/v1/admin/users/{userID}/enable", "one owner mutation", "session/authenticate", "identity/enable-user"),
		route("POST /api/v1/admin/users/{userID}/admin", "administration plus owner epoch", "session/authenticate", "identity/grant-admin"),
		route("DELETE /api/v1/admin/users/{userID}/admin", "administration plus owner epoch", "session/authenticate", "identity/revoke-admin"),
		route("POST /api/v1/admin/users/{userID}/recoveries", "one capability", "session/authenticate", "identity/create-recovery"),
		route("POST /api/v1/recovery/options", "one recovery ceremony", "identity/recovery-options"),
		route("POST /api/v1/recovery/verify", "one recovery registration", "identity/recovery-verify"),

		protected("GET /api/v1/files", "namespace/list"),
		route("GET /api/v1/files/storage-map", "root plus eight child pages", "session/authenticate", "namespace/list"),
		protected("GET /api/v1/files/stat", "namespace/stat"),
		protected("GET /api/v1/duplicates/groups", "duplicates/list-groups"),
		protected("GET /api/v1/duplicates/groups/{groupID}/occurrences", "duplicates/list-occurrences"),
		protected("PUT /api/v1/duplicates/groups/{groupID}/ignore", "duplicates/set-group-ignored"),
		protected("POST /api/v1/duplicates/directories/compare", "duplicates/compare-directories"),
		protected("POST /api/v1/duplicates/directories/overlaps", "duplicates/list-directory-overlaps"),
		protected("PUT /api/v1/duplicates/directories/ignore", "duplicates/set-directory-ignored"),
		protected("POST /api/v1/duplicates/directories/reconciliation-preview", "duplicates/preview-reconciliation"),
		protected("POST /api/v1/duplicates/directories/reconcile", "duplicates/apply-reconciliation"),
		protected("POST /api/v1/directories", "namespace/create-directory"),
		protected("POST /api/v1/uploads", "transfer/create-upload"),
		route("POST /api/v1/uploads/batch", "one to 10000 upload capabilities", "session/authenticate", "transfer/create-upload-batch"),
		route("POST /api/v1/uploads/batch/complete", "one to 10000 completed uploads", "session/authenticate", "transfer/complete-upload-batch"),
		route("DELETE /api/v1/uploads/batch", "one to 10000 active uploads", "session/authenticate", "transfer/abort-upload-batch"),
		route("POST /api/v1/uploads/plan/sizes", "one to 10000 local metadata items", "session/authenticate", "transfer/plan-upload-sizes"),
		route("POST /api/v1/uploads/plan/fingerprints", "one to 10000 local fingerprints", "session/authenticate", "transfer/plan-upload-fingerprints"),
		protected("GET /api/v1/uploads/{uploadID}", "transfer/upload-status"),
		protected("POST /api/v1/uploads/{uploadID}/complete", "transfer/complete-upload"),
		protected("DELETE /api/v1/uploads/{uploadID}", "transfer/abort-upload"),
		protected("POST /api/v1/downloads", "transfer/create-download"),
		route("POST /api/v1/files/copy", "one to 10000 selected roots", "session/authenticate", "namespace/copy"),
		route("POST /api/v1/files/move", "one to 10000 selected roots", "session/authenticate", "namespace/move"),
		route("POST /api/v1/files/trash", "one to 10000 selected roots", "session/authenticate", "namespace/trash"),
		protected("GET /api/v1/operations/{operationID}", "namespace/get-operation"),
		route("GET /api/v1/trash", "one metadata page", "session/authenticate", "namespace/list"),
		route("POST /api/v1/trash/restore", "one to 10000 trash roots", "session/authenticate", "namespace/restore"),
		route("POST /api/v1/trash/delete", "one to 10000 trash roots", "session/authenticate", "namespace/delete-trash"),
		route("POST /api/v1/trash/{trashID}/restore", "one trash root", "session/authenticate", "namespace/restore"),
		route("DELETE /api/v1/trash/{trashID}", "one trash root", "session/authenticate", "namespace/delete-trash"),
		route("POST /api/v1/trash/empty", "up to 10000 trash roots", "session/authenticate", "namespace/delete-trash"),
		route("GET /api/v1/shares", "one owner page", "session/authenticate", "share/list"),
		route("POST /api/v1/shares", "one namespace proof and share mutation", "session/authenticate", "share/create"),
		route("DELETE /api/v1/shares/{shareID}", "one share mutation", "session/authenticate", "share/revoke"),
		route("GET /api/v1/public/shares/{token}", "share authority plus namespace page", "share/public-list"),
		route("GET /api/v1/public/shares/{token}/stat", "share authority plus namespace target", "share/public-stat"),
		route("POST /api/v1/public/shares/{token}/downloads", "share authority plus capability", "share/public-download"),

		route("POST /api/v1/previews/resolve", "one to 64 items", "session/authenticate", "namespace/stat", "preview/latest", "preview/create-download"),
		route("POST /api/v1/previews/generations", "one durable operation", "session/authenticate", "namespace/stat", "state/mutate", "preview/claim", "preview/commit"),
		route("GET /api/v1/previews/operations/{operationID}", "one operation", "session/authenticate", "state/get", "preview/latest", "preview/create-download"),
		protected("GET /api/v1/me/preferences/theme", "theme/get"),
		protected("PUT /api/v1/me/preferences/theme", "theme/set"),
	}
	return append([]ProductionRoute(nil), routes...)
}

func LocalOnlyRoutes() []string {
	return []string{
		"GET /healthz", "GET /readyz", "GET /api/v1/config", "GET /", "GET /s/{token}",
		"GET /api/v1/themes", "GET /assets/themes/{digest}/{asset}",
	}
}
