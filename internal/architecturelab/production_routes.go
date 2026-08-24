package architecturelab

// ProductionRoute records the provider-boundary composition of every HTTP
// use case that can issue a provider request. The dependency lists point into
// ProductionWorkloads; they are deliberately architectural plans rather than
// guessed request ceilings. Pure local routes are listed separately below.
type ProductionRoute struct {
	Pattern             string
	CurrentDependencies []string
	AfterDependencies   []string
	Cardinality         string
}

func ProductionProviderRoutes() []ProductionRoute {
	auth := []string{"session/authenticate"}
	route := func(pattern, cardinality string, current, after []string) ProductionRoute {
		return ProductionRoute{Pattern: pattern, Cardinality: cardinality, CurrentDependencies: append([]string(nil), current...), AfterDependencies: append([]string(nil), after...)}
	}
	protected := func(pattern, workload string) ProductionRoute {
		return route(pattern, "bounded", append(append([]string(nil), auth...), workload), []string{"session/authenticate", workload})
	}
	result := []ProductionRoute{
		route("POST /api/v1/bootstrap/options", "one ceremony", []string{"control/read", "control/create"}, []string{"control/read", "control/create"}),
		route("POST /api/v1/bootstrap/verify", "one bootstrap registration", []string{"control/read", "control/atomic-multi-record"}, []string{"control/read", "control/atomic-multi-record"}),
		route("POST /api/v1/registration/options", "one ceremony", []string{"control/read", "control/create"}, []string{"control/read", "control/create"}),
		route("POST /api/v1/registration/verify", "one invited/public registration", []string{"control/read", "control/atomic-multi-record"}, []string{"control/read", "control/atomic-multi-record"}),
		route("POST /api/v1/authentication/options", "one ceremony", []string{"control/create"}, []string{"control/create"}),
		route("POST /api/v1/authentication/verify", "one credential and ceremony", []string{"control/read", "control/update", "session/issue"}, []string{"control/read", "control/update", "session/issue"}),
		route("POST /api/v1/logout", "one session", []string{"session/authenticate", "session/logout"}, []string{"session/authenticate", "session/logout"}),
		protected("GET /api/v1/me", "control/read"),
		protected("PATCH /api/v1/me", "control/update"),
		protected("GET /api/v1/me/passkeys", "control/list"),
		route("POST /api/v1/me/passkeys/options", "one ceremony", []string{"session/authenticate", "control/read", "control/create"}, []string{"session/authenticate", "control/read", "control/create"}),
		route("POST /api/v1/me/passkeys/verify", "one owner mutation and session rotation", []string{"session/authenticate", "control/atomic-multi-record", "session/rotate"}, []string{"session/authenticate", "control/atomic-multi-record", "session/rotate"}),
		route("DELETE /api/v1/me/passkeys/{credentialID}", "one owner-domain atomic mutation", []string{"session/authenticate", "control/atomic-multi-record"}, []string{"session/authenticate", "control/atomic-multi-record"}),
		route("GET /api/v1/admin/invites", "one bounded capability-domain page", []string{"session/authenticate", "control/read", "control/list"}, []string{"session/authenticate", "control/read", "control/list"}),
		route("POST /api/v1/admin/invites", "one capability", []string{"session/authenticate", "control/read", "control/atomic-multi-record"}, []string{"session/authenticate", "control/read", "control/atomic-multi-record"}),
		route("DELETE /api/v1/admin/invites/{inviteID}", "one capability", []string{"session/authenticate", "control/read", "control/update"}, []string{"session/authenticate", "control/read", "control/update"}),
		route("GET /api/v1/admin/users", "one bounded administrative projection page", []string{"session/authenticate", "control/read", "control/list"}, []string{"session/authenticate", "control/read", "maintenance/derived-view-rebuild"}),
		route("POST /api/v1/admin/users/{userID}/disable", "administration plus owner auth generation", []string{"session/authenticate", "control/atomic-multi-record", "session/revoke-user"}, []string{"session/authenticate", "control/atomic-multi-record", "session/revoke-user"}),
		route("POST /api/v1/admin/users/{userID}/enable", "one owner control mutation", []string{"session/authenticate", "control/read", "control/update"}, []string{"session/authenticate", "control/read", "control/update"}),
		route("POST /api/v1/admin/users/{userID}/admin", "administration plus owner auth generation", []string{"session/authenticate", "control/atomic-multi-record", "session/revoke-user"}, []string{"session/authenticate", "control/atomic-multi-record", "session/revoke-user"}),
		route("DELETE /api/v1/admin/users/{userID}/admin", "administration plus owner auth generation", []string{"session/authenticate", "control/atomic-multi-record", "session/revoke-user"}, []string{"session/authenticate", "control/atomic-multi-record", "session/revoke-user"}),
		route("POST /api/v1/admin/users/{userID}/recoveries", "one capability", []string{"session/authenticate", "control/read", "control/atomic-multi-record"}, []string{"session/authenticate", "control/read", "control/atomic-multi-record"}),
		route("POST /api/v1/recovery/options", "one recovery capability and ceremony", []string{"control/read", "control/create"}, []string{"control/read", "control/create"}),
		route("POST /api/v1/recovery/verify", "one recovery registration", []string{"control/read", "control/atomic-multi-record"}, []string{"control/read", "control/atomic-multi-record"}),

		protected("GET /api/v1/files", "namespace/list"),
		route("GET /api/v1/files/storage-map", "one root plus at most eight child pages", []string{"session/authenticate", "namespace/list"}, []string{"session/authenticate", "namespace/list"}),
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
		route("POST /api/v1/uploads/batch", "one to 100 upload capabilities", []string{"session/authenticate", "transfer/create-upload"}, []string{"session/authenticate", "transfer/create-upload"}),
		protected("GET /api/v1/uploads/{uploadID}", "transfer/upload-status"),
		protected("POST /api/v1/uploads/{uploadID}/complete", "transfer/complete-upload"),
		protected("DELETE /api/v1/uploads/{uploadID}", "transfer/abort-upload"),
		protected("POST /api/v1/downloads", "transfer/create-download"),
		route("POST /api/v1/files/copy", "one to 100 selected roots", []string{"session/authenticate", "namespace/copy"}, []string{"session/authenticate", "namespace/copy"}),
		route("POST /api/v1/files/move", "one to 100 selected roots", []string{"session/authenticate", "namespace/move"}, []string{"session/authenticate", "namespace/move"}),
		route("POST /api/v1/files/trash", "one to 100 selected roots", []string{"session/authenticate", "namespace/move"}, []string{"session/authenticate", "namespace/move"}),
		protected("GET /api/v1/operations/{operationID}", "namespace/get-operation"),
		route("GET /api/v1/trash", "one bounded metadata page plus batched child lookup", []string{"session/authenticate", "control/list", "namespace/lookup-children"}, []string{"session/authenticate", "namespace/list"}),
		route("POST /api/v1/trash/{trashID}/restore", "one trash root", []string{"session/authenticate", "namespace/stat", "namespace/move"}, []string{"session/authenticate", "namespace/move"}),
		route("DELETE /api/v1/trash/{trashID}", "one trash root", []string{"session/authenticate", "namespace/stat", "namespace/delete"}, []string{"session/authenticate", "namespace/delete"}),
		route("POST /api/v1/trash/empty", "all selected trash roots in bounded batches", []string{"session/authenticate", "control/list", "namespace/delete"}, []string{"session/authenticate", "namespace/delete"}),
		route("GET /api/v1/shares", "one bounded owner-domain page", []string{"session/authenticate", "control/list"}, []string{"session/authenticate", "control/list"}),
		route("POST /api/v1/shares", "one namespace proof and share mutation", []string{"session/authenticate", "namespace/stat", "control/create"}, []string{"session/authenticate", "namespace/stat", "control/create"}),
		route("DELETE /api/v1/shares/{shareID}", "one share mutation", []string{"session/authenticate", "control/read", "control/update"}, []string{"session/authenticate", "control/update"}),
		route("GET /api/v1/public/shares/{token}", "share authority plus namespace page and root revalidation", []string{"control/read", "namespace/stat", "namespace/list"}, []string{"control/read", "namespace/stat", "namespace/list"}),
		route("GET /api/v1/public/shares/{token}/stat", "share authority plus target and root revalidation", []string{"control/read", "namespace/stat"}, []string{"control/read", "namespace/stat"}),
		route("POST /api/v1/public/shares/{token}/downloads", "share authority plus target and capability", []string{"control/read", "namespace/stat", "transfer/create-download"}, []string{"control/read", "namespace/stat", "transfer/create-download"}),

		route("POST /api/v1/previews/resolve", "one to 100 items", []string{"session/authenticate", "namespace/stat", "preview/latest", "preview/create-download"}, []string{"session/authenticate", "namespace/stat", "preview/latest", "preview/create-download"}),
		route("POST /api/v1/previews/generations", "one item and one durable operation", []string{"session/authenticate", "namespace/stat", "control/atomic-multi-record", "preview/claim", "preview/commit"}, []string{"session/authenticate", "namespace/stat", "control/atomic-multi-record", "preview/claim", "preview/commit"}),
		route("GET /api/v1/previews/operations/{operationID}", "one operation", []string{"session/authenticate", "control/read", "preview/latest", "preview/create-download"}, []string{"session/authenticate", "control/read", "preview/latest", "preview/create-download"}),
		protected("GET /api/v1/me/preferences/theme", "control/read"),
		protected("PUT /api/v1/me/preferences/theme", "control/update"),
	}
	return append([]ProductionRoute(nil), result...)
}

func LocalOnlyRoutes() []string {
	return []string{
		"GET /healthz", "GET /readyz", "GET /api/v1/config", "GET /", "GET /s/{token}",
		"GET /api/v1/themes", "GET /assets/themes/{digest}/{asset}",
	}
}
