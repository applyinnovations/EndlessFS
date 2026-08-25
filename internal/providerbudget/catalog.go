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

		workload("namespace/list", "namespace-read", "namespace-list-page-schema-009"),
		workload("namespace/lookup-children", "namespace-read", "namespace-lookup-children-schema-009"),
		workload("namespace/stat", "namespace-read", "namespace-stat-schema-009"),
		workload("namespace/create-directory", "namespace-mutation", "file-create-directory-schema-009"),
		workload("namespace/copy", "namespace-mutation", "direct-copy-one-file-schema-009", "batch-copy-10000-schema-009"),
		workload("namespace/move", "namespace-mutation", "direct-move-one-file-schema-009", "batch-move-10000-schema-009", "trash-one-file-schema-009", "trash-batch-10000-schema-009", "restore-one-file-schema-009"),
		workload("namespace/delete", "namespace-mutation", "direct-delete-one-file-schema-009", "permanent-delete-one-file-schema-009", "empty-trash-10000-schema-009"),
		workload("namespace/get-operation", "namespace-read", "namespace-get-operation-schema-009"),

		workload("transfer/create-upload", "transfer", "file-create-upload-cold-schema-009", "file-create-upload-warm-schema-009", "file-create-upload-batch-100-schema-009"),
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

		workload("control/read", "control", "control-read-schema-009"),
		workload("control/list", "control", "control-list-schema-009"),
		workload("control/create", "control", "control-create-schema-009"),
		workload("control/update", "control", "control-update-schema-009"),
		workload("control/delete", "control", "control-delete-schema-009"),
		workload("control/atomic-multi-record", "control", "control-atomic-multi-record-schema-009"),

		workload("maintenance/startup", "maintenance", "maintenance-startup-warm-schema-009"),
		workload("maintenance/checkpoint", "maintenance", "maintenance-checkpoint-schema-009"),
		workload("maintenance/compaction", "maintenance", "maintenance-compaction-schema-009"),
		workload("maintenance/recovery", "maintenance", "maintenance-recovery-schema-009"),
		workload("maintenance/garbage-collection", "maintenance", "maintenance-garbage-collection-schema-009"),
		workload("maintenance/derived-view-rebuild", "maintenance", "maintenance-derived-view-rebuild-schema-009"),
		workload("maintenance/migration", "maintenance", "maintenance-migration-008-to-009"),
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
		route("POST /api/v1/bootstrap/options", "one ceremony", "control/read", "control/create"),
		route("POST /api/v1/bootstrap/verify", "one bootstrap registration", "control/read", "control/atomic-multi-record"),
		route("POST /api/v1/registration/options", "one ceremony", "control/read", "control/create"),
		route("POST /api/v1/registration/verify", "one registration", "control/read", "control/atomic-multi-record"),
		route("POST /api/v1/authentication/options", "one ceremony", "control/create"),
		route("POST /api/v1/authentication/verify", "one credential and ceremony", "control/read", "control/update", "session/issue"),
		route("POST /api/v1/logout", "one session", "session/authenticate", "session/logout"),
		protected("GET /api/v1/me", "control/read"),
		protected("PATCH /api/v1/me", "control/update"),
		protected("GET /api/v1/me/passkeys", "control/list"),
		route("POST /api/v1/me/passkeys/options", "one ceremony", "session/authenticate", "control/read", "control/create"),
		route("POST /api/v1/me/passkeys/verify", "owner mutation and session rotation", "session/authenticate", "control/atomic-multi-record", "session/rotate"),
		route("DELETE /api/v1/me/passkeys/{credentialID}", "one owner mutation", "session/authenticate", "control/atomic-multi-record"),
		route("GET /api/v1/admin/invites", "one capability page", "session/authenticate", "control/read", "control/list"),
		route("POST /api/v1/admin/invites", "one capability", "session/authenticate", "control/read", "control/atomic-multi-record"),
		route("DELETE /api/v1/admin/invites/{inviteID}", "one capability", "session/authenticate", "control/read", "control/update"),
		route("GET /api/v1/admin/users", "one administrative page", "session/authenticate", "control/read", "control/list", "maintenance/derived-view-rebuild"),
		route("POST /api/v1/admin/users/{userID}/disable", "administration plus owner epoch", "session/authenticate", "control/atomic-multi-record", "session/revoke-user"),
		route("POST /api/v1/admin/users/{userID}/enable", "one owner mutation", "session/authenticate", "control/read", "control/update"),
		route("POST /api/v1/admin/users/{userID}/admin", "administration plus owner epoch", "session/authenticate", "control/atomic-multi-record", "session/revoke-user"),
		route("DELETE /api/v1/admin/users/{userID}/admin", "administration plus owner epoch", "session/authenticate", "control/atomic-multi-record", "session/revoke-user"),
		route("POST /api/v1/admin/users/{userID}/recoveries", "one capability", "session/authenticate", "control/read", "control/atomic-multi-record"),
		route("POST /api/v1/recovery/options", "one recovery ceremony", "control/read", "control/create"),
		route("POST /api/v1/recovery/verify", "one recovery registration", "control/read", "control/atomic-multi-record"),

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
		route("POST /api/v1/uploads/batch", "one to 100 upload capabilities", "session/authenticate", "transfer/create-upload"),
		protected("GET /api/v1/uploads/{uploadID}", "transfer/upload-status"),
		protected("POST /api/v1/uploads/{uploadID}/complete", "transfer/complete-upload"),
		protected("DELETE /api/v1/uploads/{uploadID}", "transfer/abort-upload"),
		protected("POST /api/v1/downloads", "transfer/create-download"),
		route("POST /api/v1/files/copy", "one to 10000 selected roots", "session/authenticate", "namespace/copy"),
		route("POST /api/v1/files/move", "one to 10000 selected roots", "session/authenticate", "namespace/move"),
		route("POST /api/v1/files/trash", "one to 10000 selected roots", "session/authenticate", "namespace/move"),
		protected("GET /api/v1/operations/{operationID}", "namespace/get-operation"),
		route("GET /api/v1/trash", "one metadata page", "session/authenticate", "namespace/list"),
		route("POST /api/v1/trash/{trashID}/restore", "one trash root", "session/authenticate", "namespace/move"),
		route("DELETE /api/v1/trash/{trashID}", "one trash root", "session/authenticate", "namespace/delete"),
		route("POST /api/v1/trash/empty", "up to 10000 trash roots", "session/authenticate", "namespace/delete"),
		route("GET /api/v1/shares", "one owner page", "session/authenticate", "control/list"),
		route("POST /api/v1/shares", "one namespace proof and share mutation", "session/authenticate", "namespace/stat", "control/create"),
		route("DELETE /api/v1/shares/{shareID}", "one share mutation", "session/authenticate", "control/update"),
		route("GET /api/v1/public/shares/{token}", "share authority plus namespace page", "control/read", "namespace/stat", "namespace/list"),
		route("GET /api/v1/public/shares/{token}/stat", "share authority plus namespace target", "control/read", "namespace/stat"),
		route("POST /api/v1/public/shares/{token}/downloads", "share authority plus capability", "control/read", "namespace/stat", "transfer/create-download"),

		route("POST /api/v1/previews/resolve", "one to 64 items", "session/authenticate", "namespace/stat", "preview/latest", "preview/create-download"),
		route("POST /api/v1/previews/generations", "one durable operation", "session/authenticate", "namespace/stat", "control/atomic-multi-record", "preview/claim", "preview/commit"),
		route("GET /api/v1/previews/operations/{operationID}", "one operation", "session/authenticate", "control/read", "preview/latest", "preview/create-download"),
		protected("GET /api/v1/me/preferences/theme", "control/read"),
		protected("PUT /api/v1/me/preferences/theme", "control/update"),
	}
	return append([]ProductionRoute(nil), routes...)
}

func LocalOnlyRoutes() []string {
	return []string{
		"GET /healthz", "GET /readyz", "GET /api/v1/config", "GET /", "GET /s/{token}",
		"GET /api/v1/themes", "GET /assets/themes/{digest}/{asset}",
	}
}
