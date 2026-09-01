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

func execution(budget string, executions, parallelism int64) BudgetExecution {
	return BudgetExecution{Budget: budget, Executions: executions, Parallelism: parallelism}
}

// ProductionScaleScenarios records the largest deterministic workloads in the
// production surface. A zero-execution scenario is intentional proof that
// dormant client state does not contact the provider.
func ProductionScaleScenarios() []ProductionScaleScenario {
	return []ProductionScaleScenario{
		{ID: "restored-transfer-ledger-needs-source", Category: "startup", LogicalItems: 10_000, BrowserRequests: 0, RequestBasis: "Dormant history is local IndexedDB state; provider reconciliation is deferred until a source file is reacquired."},
		{ID: "browse-live-directory", Category: "namespace-read", LogicalItems: 10_000, BrowserRequests: 10, RequestBasis: "Ten sequential 1,000-entry pages.", Executions: []BudgetExecution{execution("session-authenticate-schema-009", 10, 1), execution("namespace-list-page-1000-schema-010", 10, 1)}},
		{ID: "browse-trash", Category: "namespace-read", LogicalItems: 10_000, BrowserRequests: 10, RequestBasis: "Ten sequential 1,000-entry pages.", Executions: []BudgetExecution{execution("session-authenticate-schema-009", 10, 1), execution("namespace-list-page-1000-schema-010", 10, 1)}},
		{ID: "copy-selection", Category: "namespace-mutation", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "One atomic owner-namespace batch.", Executions: []BudgetExecution{execution("session-authenticate-schema-009", 1, 1), execution("batch-copy-10000-schema-009", 1, 1)}},
		{ID: "move-selection", Category: "namespace-mutation", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "One atomic owner-namespace batch.", Executions: []BudgetExecution{execution("session-authenticate-schema-009", 1, 1), execution("batch-move-10000-schema-009", 1, 1)}},
		{ID: "trash-selection", Category: "namespace-mutation", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "One atomic owner-namespace batch.", Executions: []BudgetExecution{execution("session-authenticate-schema-009", 1, 1), execution("trash-batch-10000-schema-009", 1, 1)}},
		{ID: "restore-selection", Category: "namespace-mutation", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "One atomic owner-namespace batch.", Executions: []BudgetExecution{execution("session-authenticate-schema-009", 1, 1), execution("restore-batch-10000-schema-010", 1, 1)}},
		{ID: "permanent-delete-selection", Category: "namespace-mutation", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "One atomic owner-namespace batch.", Executions: []BudgetExecution{execution("session-authenticate-schema-009", 1, 1), execution("empty-trash-10000-schema-009", 1, 1)}},
		{ID: "trash-selection-replay", Category: "recovery", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "Retry reads the durable paged outcome and performs no publication.", Executions: []BudgetExecution{execution("session-authenticate-schema-009", 1, 1), execution("trash-batch-10000-replay-schema-010", 1, 1)}},
		{ID: "restore-selection-replay", Category: "recovery", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "Retry reads the durable paged outcome and performs no publication.", Executions: []BudgetExecution{execution("session-authenticate-schema-009", 1, 1), execution("restore-batch-10000-replay-schema-010", 1, 1)}},
		{ID: "permanent-delete-selection-replay", Category: "recovery", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "Retry reads the durable paged outcome and performs no publication.", Executions: []BudgetExecution{execution("session-authenticate-schema-009", 1, 1), execution("empty-trash-10000-replay-schema-010", 1, 1)}},
		{ID: "trash-selection-denied", Category: "denial", LogicalItems: 10_000, BrowserRequests: 1, RequestBasis: "A stale final item validates the closed snapshot and writes nothing.", Executions: []BudgetExecution{execution("session-authenticate-schema-009", 1, 1), execution("trash-batch-10000-denied-schema-010", 1, 1)}},
		{ID: "smart-upload-size-planning", Category: "transfer-planning", LogicalItems: 10_000, BrowserRequests: 10, RequestBasis: "Ten metadata-only requests of 1,000 local sizes.", Executions: []BudgetExecution{execution("session-authenticate-schema-009", 10, 1), execution("upload-plan-sizes-1000-schema-010", 10, 1)}},
		{ID: "smart-upload-all-size-candidates", Category: "transfer-planning", LogicalItems: 10_000, BrowserRequests: 20, RequestBasis: "Size planning followed by fingerprints only for every size candidate.", Executions: []BudgetExecution{execution("session-authenticate-schema-009", 20, 1), execution("upload-plan-sizes-1000-schema-010", 10, 1), execution("upload-plan-fingerprints-1000-schema-010", 10, 1)}},
		{ID: "upload-admission", Category: "transfer-active", LogicalItems: 10_000, BrowserRequests: 100, RequestBasis: "One hundred batches of 100; each item necessarily creates one provider upload session.", Executions: []BudgetExecution{execution("session-authenticate-schema-009", 100, 1), execution("file-create-upload-batch-100-schema-009", 100, 1)}},
		{ID: "upload-completion", Category: "transfer-active", LogicalItems: 10_000, BrowserRequests: 10_000, RequestBasis: "Only transfers whose object data completed call completion; the configured worker pool bounds concurrency.", Executions: []BudgetExecution{execution("session-authenticate-schema-009", 10_000, 100), execution("file-complete-upload-schema-009", 10_000, 100)}},
		{ID: "upload-cancellation", Category: "transfer-active", LogicalItems: 10_000, BrowserRequests: 10_000, RequestBasis: "Only live provider sessions are aborted; the configured control worker pool prevents an unbounded request surge.", Executions: []BudgetExecution{execution("session-authenticate-schema-009", 10_000, 100), execution("file-abort-upload-schema-009", 10_000, 100)}},
		{ID: "visible-grid-preview-resolution", Category: "preview", LogicalItems: 10_000, BrowserRequests: 32, RequestBasis: "The logical directory is virtualized; only the bounded visible/overscan window resolves previews.", Executions: []BudgetExecution{execution("session-authenticate-schema-009", 32, 2), execution("namespace-stat-schema-009", 32, 2), execution("preview-latest", 32, 2), execution("preview-create-download", 32, 2)}},
		{ID: "checkpoint-garbage-collection", Category: "maintenance", LogicalItems: 128, BrowserRequests: 0, RequestBasis: "One quiescent maintenance run over the calibrated garbage set.", Executions: []BudgetExecution{execution("maintenance-checkpoint-garbage-128-schema-009", 1, 1)}},
		{ID: "domain-compaction", Category: "maintenance", LogicalItems: 300, BrowserRequests: 0, RequestBasis: "One bounded compaction run over the calibrated domain history.", Executions: []BudgetExecution{execution("maintenance-domain-compaction-300-schema-009", 1, 1)}},
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
		workload("state/get", "control", "state-get-schema-009", "state-get-missing-schema-009"),
		workload("state/list", "control", "state-list-empty-schema-009"),
		workload("state/create", "control", "state-create-schema-009"),
		workload("state/compare-and-swap", "control", "state-compare-and-swap-schema-009"),
		workload("state/delete", "control", "state-delete-schema-009"),
		workload("state/mutate", "control", "state-mutate-two-records-schema-009"),
		workload("state/transact", "control", "state-transact-two-domains-schema-009"),

		workload("namespace/list", "namespace-read", "namespace-list-page-schema-009", "namespace-list-page-1000-schema-010"),
		workload("namespace/lookup-children", "namespace-read", "namespace-lookup-children-schema-009"),
		workload("namespace/stat", "namespace-read", "namespace-stat-schema-009"),
		workload("namespace/create-directory", "namespace-mutation", "file-create-directory-schema-009"),
		workload("namespace/copy", "namespace-mutation", "direct-copy-one-file-schema-009", "batch-copy-10000-schema-009"),
		workload("namespace/move", "namespace-mutation", "direct-move-one-file-schema-009", "batch-move-10000-schema-009"),
		workload("namespace/batch-copy-move", "namespace-mutation", "batch-copy-10000-schema-009", "batch-move-10000-schema-009"),
		workload("namespace/trash", "namespace-mutation", "trash-one-file-schema-009", "trash-batch-10000-schema-009", "trash-batch-10000-replay-schema-010", "trash-batch-10000-denied-schema-010"),
		workload("namespace/restore", "namespace-mutation", "restore-one-file-schema-009", "restore-batch-10000-schema-010", "restore-batch-10000-replay-schema-010"),
		workload("namespace/delete", "namespace-mutation", "direct-delete-one-file-schema-009"),
		workload("namespace/delete-trash", "namespace-mutation", "permanent-delete-one-file-schema-009", "empty-trash-10000-schema-009", "empty-trash-10000-replay-schema-010"),
		workload("namespace/get-operation", "namespace-read", "namespace-get-operation-schema-009"),

		workload("transfer/create-upload", "transfer", "file-create-upload-cold-schema-009", "file-create-upload-warm-schema-009"),
		workload("transfer/create-upload-batch", "transfer", "file-create-upload-batch-100-schema-009"),
		workload("transfer/plan-upload-sizes", "derived-read", "upload-plan-index-cold-256-schema-010", "upload-plan-index-incremental-one-schema-010", "upload-plan-sizes-1000-schema-010"),
		workload("transfer/plan-upload-fingerprints", "derived-read", "upload-plan-fingerprints-1000-schema-010"),
		workload("transfer/upload-status", "transfer", "file-upload-status-active-schema-009"),
		workload("transfer/complete-upload", "transfer", "file-complete-upload-schema-009"),
		workload("transfer/abort-upload", "transfer", "file-abort-upload-schema-009"),
		workload("transfer/create-download", "transfer", "file-create-download-schema-009"),

		workload("duplicates/list-groups", "derived-read", "duplicates-list-groups-schema-009"),
		workload("duplicates/list-occurrences", "derived-read", "duplicates-list-occurrences-schema-009"),
		workload("duplicates/set-group-ignored", "control", "duplicates-set-group-ignored-schema-009"),
		workload("duplicates/compare-directories", "derived-read", "duplicates-compare-directories-schema-009"),
		workload("duplicates/list-directory-overlaps", "derived-read", "duplicates-list-directory-overlaps-schema-009"),
		workload("duplicates/set-directory-ignored", "control", "duplicates-set-directory-ignored-schema-009"),
		workload("duplicates/preview-reconciliation", "derived-read", "duplicates-preview-reconciliation-schema-009"),
		workload("duplicates/validate-reconciliation", "derived-read", "duplicates-validate-reconciliation-schema-009"),
		workload("duplicates/apply-reconciliation", "namespace-mutation", "duplicates-apply-reconciliation-schema-009"),

		workload("preview/validate", "preview", "preview-validate"),
		workload("preview/check", "preview", "preview-check"),
		workload("preview/claim", "preview", "preview-claim-new"),
		workload("preview/release", "preview", "preview-release"),
		workload("preview/commit", "preview", "preview-commit"),
		workload("preview/latest", "preview", "preview-latest"),
		workload("preview/read", "preview", "preview-read"),
		workload("preview/create-download", "preview", "preview-create-download"),

		workload("data-plane/upload", "data-plane", "file-data-upload-four-bytes"),
		workload("data-plane/download", "data-plane", "file-data-download-four-bytes"),

		workload("session/issue", "session", "session-issue-schema-009"),
		workload("session/authenticate", "session", "session-authenticate-schema-009"),
		workload("session/rotate", "session", "session-rotate-schema-009"),
		workload("session/logout", "session", "session-logout-schema-009"),
		workload("session/revoke-user", "session", "session-revoke-user-schema-009"),

		workload("identity/bootstrap-options", "identity", "identity-bootstrap-options-schema-009"),
		workload("identity/bootstrap-verify", "identity", "identity-bootstrap-verify-schema-009"),
		workload("identity/registration-options", "identity", "identity-registration-options-schema-009", "identity-invited-registration-options-schema-009"),
		workload("identity/registration-verify", "identity", "identity-registration-verify-schema-009", "identity-invited-registration-verify-schema-009"),
		workload("identity/authentication-options", "identity", "identity-authentication-options-schema-009"),
		workload("identity/authentication-verify", "identity", "identity-authentication-verify-schema-009"),
		workload("identity/current-user", "identity", "identity-current-user-schema-009"),
		workload("identity/update-profile", "identity", "identity-update-profile-schema-009"),
		workload("identity/list-passkeys", "identity", "identity-list-passkeys-schema-009"),
		workload("identity/add-passkey-options", "identity", "identity-add-passkey-options-schema-009"),
		workload("identity/add-passkey-verify", "identity", "identity-add-passkey-verify-schema-009"),
		workload("identity/remove-passkey", "identity", "identity-remove-passkey-schema-009"),
		workload("identity/list-invites", "identity", "identity-list-invites-schema-009"),
		workload("identity/create-invite", "identity", "identity-create-invite-schema-009"),
		workload("identity/revoke-invite", "identity", "identity-revoke-invite-schema-009"),
		workload("identity/list-admin-users", "identity", "identity-list-admin-users-schema-009"),
		workload("identity/disable-user", "identity", "identity-disable-user-schema-009"),
		workload("identity/enable-user", "identity", "identity-enable-user-schema-009"),
		workload("identity/grant-admin", "identity", "identity-grant-admin-schema-009"),
		workload("identity/revoke-admin", "identity", "identity-revoke-admin-schema-009"),
		workload("identity/create-recovery", "identity", "identity-create-recovery-schema-009"),
		workload("identity/recovery-options", "identity", "identity-recovery-options-schema-009"),
		workload("identity/recovery-verify", "identity", "identity-recovery-verify-schema-009"),

		workload("share/create", "share", "share-create-schema-009"),
		workload("share/list", "share", "share-list-schema-009"),
		workload("share/revoke", "share", "share-revoke-schema-009"),
		workload("share/public-list", "share", "share-public-list-schema-009"),
		workload("share/public-stat", "share", "share-public-stat-schema-009"),
		workload("share/public-download", "share", "share-public-download-schema-009"),

		workload("theme/get", "preference", "theme-get-default-schema-009"),
		workload("theme/set", "preference", "theme-set-create-schema-009", "theme-set-update-schema-009"),

		workload("maintenance/startup", "maintenance", "maintenance-startup-warm-schema-009"),
		workload("maintenance/gate-status", "maintenance", "maintenance-gate-status-schema-009"),
		workload("maintenance/checkpoint", "maintenance", "maintenance-checkpoint-minimal-schema-009", "maintenance-checkpoint-garbage-128-schema-009"),
		workload("maintenance/verify-checkpoint", "maintenance", "maintenance-verify-checkpoint-minimal-schema-009"),
		workload("maintenance/visit-checkpoint", "maintenance", "maintenance-visit-checkpoint-minimal-schema-009"),
		workload("maintenance/open-writes", "maintenance", "maintenance-open-writes-schema-009"),
		workload("maintenance/compaction", "maintenance", "maintenance-domain-compaction-300-schema-009"),
		workload("maintenance/recovery", "maintenance", "maintenance-transition-recovery-schema-009"),
		workload("maintenance/derived-view-rebuild", "maintenance", "maintenance-derived-view-rebuild-schema-009"),
		workload("maintenance/migration", "maintenance", "maintenance-migration-008-to-010-minimal-fixture"),
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
		route("POST /api/v1/uploads/batch", "one to 100 upload capabilities", "session/authenticate", "transfer/create-upload-batch"),
		route("POST /api/v1/uploads/plan/sizes", "one to 1000 local metadata items", "session/authenticate", "transfer/plan-upload-sizes"),
		route("POST /api/v1/uploads/plan/fingerprints", "one to 1000 local fingerprints", "session/authenticate", "transfer/plan-upload-fingerprints"),
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
