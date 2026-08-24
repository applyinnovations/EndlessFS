package architecturelab

// ProductionWorkload is the completeness boundary for the architecture
// comparison. CurrentEvidence names the production implementation or calibrated
// provider-budget primitive used for the baseline. PrototypeEvidence names the
// executable selected-architecture component used for the comparison.
type ProductionWorkload struct {
	ID                string
	Category          string
	CurrentEvidence   string
	PrototypeEvidence string
	Entrypoints       []string
}

// ProductionWorkloads returns every production behavior that can cross an
// object-provider boundary. Pure HTML/assets, local theme resolution, health
// response formatting, CSRF validation, and WebAuthn cryptography are omitted
// because they issue no provider request by themselves.
func ProductionWorkloads() []ProductionWorkload {
	workloads := []ProductionWorkload{
		workload("state/get", "control", "portable StateStore.Get", "record-domain read"),
		workload("state/list", "control", "portable StateStore.List", "record-domain bounded list"),
		workload("state/create", "control", "portable StateStore.Create", "record-domain mutation"),
		workload("state/compare-and-swap", "control", "portable StateStore.CompareAndSwap", "record-domain mutation"),
		workload("state/delete", "control", "portable StateStore.Delete", "record-domain mutation"),

		workload("namespace/list", "namespace-read", "portable Storage.List", "paged-delta directory page"),
		workload("namespace/lookup-children", "namespace-read", "portable Storage.LookupChildren", "paged-delta batched lookup"),
		workload("namespace/stat", "namespace-read", "portable Storage.Stat", "paged-delta path lookup"),
		workload("namespace/create-directory", "namespace-mutation", "file-create-directory ratchet", "paged-delta mutation"),
		workload("namespace/copy", "namespace-mutation", "direct-copy-one-file ratchet", "paged-delta reference copy"),
		workload("namespace/move", "namespace-mutation", "direct-move-one-file ratchet", "paged-delta edge move"),
		workload("namespace/delete", "namespace-mutation", "direct-delete-one-file ratchet", "paged-delta tombstone"),
		workload("namespace/get-operation", "namespace-read", "portable Storage.GetOperation", "bounded recent outcome read"),

		workload("transfer/create-upload", "transfer", "file-create-upload ratchet", "session-independent upload record plus provider capability"),
		workload("transfer/upload-status", "transfer", "file-upload-status-active ratchet", "upload record read plus provider progress"),
		workload("transfer/complete-upload", "transfer", "file-complete-upload ratchet", "metadata verify plus paged-delta publication"),
		workload("transfer/abort-upload", "transfer", "file-abort-upload ratchet", "provider abort plus upload-record mutation"),
		workload("transfer/create-download", "transfer", "file-create-download ratchet", "paged-delta stat plus signed capability"),

		workload("duplicates/list-groups", "derived-read", "portable duplicate group index", "immutable derived-view page"),
		workload("duplicates/list-occurrences", "derived-read", "portable duplicate occurrence index", "immutable derived-view page"),
		workload("duplicates/set-group-ignored", "control", "portable duplicate ignore record", "owner-control mutation"),
		workload("duplicates/compare-directories", "derived-read", "portable duplicate directory summaries", "immutable derived-view comparison"),
		workload("duplicates/list-directory-overlaps", "derived-read", "portable duplicate overlap index", "immutable derived-view page"),
		workload("duplicates/set-directory-ignored", "control", "portable duplicate directory ignore record", "owner-control mutation"),
		workload("duplicates/preview-reconciliation", "derived-read", "portable reconciliation plan", "derived-view read plus immutable plan"),
		workload("duplicates/validate-reconciliation", "derived-read", "portable reconciliation validation", "immutable plan read plus namespace revision"),
		workload("duplicates/apply-reconciliation", "namespace-mutation", "drive reconciliation batch", "paged batch-delta publication"),

		workload("preview/validate", "preview", "durable preview startup validation", "unchanged independent preview-store validation"),
		workload("preview/check", "preview", "preview-check ratchet", "unchanged preview-store check"),
		workload("preview/claim", "preview", "preview-claim-new ratchet", "unchanged preview-store claim"),
		workload("preview/release", "preview", "preview-release ratchet", "unchanged preview-store release"),
		workload("preview/commit", "preview", "preview-commit ratchet", "unchanged preview-store commit"),
		workload("preview/latest", "preview", "preview-latest ratchet", "unchanged preview-store latest"),
		workload("preview/read", "preview", "preview-read ratchet", "unchanged preview-store read"),
		workload("preview/create-download", "preview", "preview-create-download ratchet", "unchanged preview-store capability"),

		workload("data-plane/upload", "data-plane", "file-data-upload ratchet", "unchanged direct provider upload"),
		workload("data-plane/download", "data-plane", "file-data-download ratchet", "unchanged direct provider download"),

		workload("session/issue", "session", "state session create", "direct conditional session create"),
		workload("session/authenticate", "session", "state session plus account reads", "parallel session and owner-auth-head reads"),
		workload("session/rotate", "session", "state session delete plus create", "direct old-session delete plus new-session create"),
		workload("session/logout", "session", "state session delete", "direct conditional session delete"),
		workload("session/revoke-user", "session", "global session list plus matching deletes", "owner auth-generation mutation"),

		workload("control/read", "control", "state record get", "record-domain read"),
		workload("control/list", "control", "state prefix list", "record-domain bounded list"),
		workload("control/create", "control", "state record create", "record-domain mutation"),
		workload("control/update", "control", "state record compare-and-swap", "record-domain mutation"),
		workload("control/delete", "control", "state record delete", "record-domain mutation"),
		workload("control/atomic-multi-record", "control", "sequential state transactions and recovery records", "one consistency-domain mutation"),

		workload("maintenance/startup", "maintenance", "portable Open and readiness", "domain-catalog and bounded-head validation"),
		workload("maintenance/checkpoint", "maintenance", "global write-gate checkpoint", "frozen domain-catalog checkpoint"),
		workload("maintenance/compaction", "maintenance", "manifest/index maintenance", "dirty-directory page compaction"),
		workload("maintenance/recovery", "maintenance", "admission and operation recovery", "claim/head reconciliation"),
		workload("maintenance/garbage-collection", "maintenance", "authoritative object inventory", "watermark-delayed unreachable immutable-object sweep"),
		workload("maintenance/derived-view-rebuild", "maintenance", "synchronous authoritative secondary indexes", "asynchronous checkpoint-bound view rebuild"),
		workload("maintenance/migration", "maintenance", "schema-ledger adjacent transformations", "future adjacent migration into selected schema"),
	}
	return append([]ProductionWorkload(nil), workloads...)
}

func workload(id, category, current, prototype string, entrypoints ...string) ProductionWorkload {
	return ProductionWorkload{ID: id, Category: category, CurrentEvidence: current, PrototypeEvidence: prototype, Entrypoints: append([]string(nil), entrypoints...)}
}
