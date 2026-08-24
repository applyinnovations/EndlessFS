package architecturelab

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/preview"
	"github.com/applyinnovations/endlessfs/internal/provider"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/state"
)

type testRecordDomain struct {
	engine *recordDomain
	ledger *providerbudget.Ledger
}

func TestProductionWorkloadCatalogCoversEveryApplicationProviderContractMethod(t *testing.T) {
	workloads := make(map[string]bool)
	for _, workload := range ProductionWorkloads() {
		workloads[workload.ID] = true
	}
	contracts := []struct {
		name    string
		typeOf  reflect.Type
		methods map[string]string
	}{
		{name: "state.Store", typeOf: reflect.TypeOf((*state.Store)(nil)).Elem(), methods: map[string]string{
			"Get": "state/get", "List": "state/list", "Create": "state/create", "CompareAndSwap": "state/compare-and-swap", "Delete": "state/delete",
		}},
		{name: "provider.Storage", typeOf: reflect.TypeOf((*provider.Storage)(nil)).Elem(), methods: map[string]string{
			"List": "namespace/list", "LookupChildren": "namespace/lookup-children", "Stat": "namespace/stat", "CreateDirectory": "namespace/create-directory",
			"CreateUpload": "transfer/create-upload", "UploadStatus": "transfer/upload-status", "CompleteUpload": "transfer/complete-upload", "AbortUpload": "transfer/abort-upload", "CreateDownload": "transfer/create-download",
			"Copy": "namespace/copy", "Move": "namespace/move", "Delete": "namespace/delete", "GetOperation": "namespace/get-operation",
		}},
		{name: "provider.DuplicateStorage", typeOf: reflect.TypeOf((*provider.DuplicateStorage)(nil)).Elem(), methods: map[string]string{
			"ListDuplicateGroups": "duplicates/list-groups", "ListDuplicateOccurrences": "duplicates/list-occurrences", "SetDuplicateGroupIgnored": "duplicates/set-group-ignored",
			"CompareDuplicateDirectories": "duplicates/compare-directories", "ListDuplicateDirectoryOverlaps": "duplicates/list-directory-overlaps", "SetDuplicateDirectoryIgnored": "duplicates/set-directory-ignored",
			"PreviewDuplicateReconciliation": "duplicates/preview-reconciliation", "ValidateDuplicateReconciliation": "duplicates/validate-reconciliation",
		}},
		{name: "preview.Store", typeOf: reflect.TypeOf((*preview.Store)(nil)).Elem(), methods: map[string]string{
			"Validate": "preview/validate", "Check": "preview/check", "Claim": "preview/claim", "Release": "preview/release", "Commit": "preview/commit",
			"Latest": "preview/latest", "Read": "preview/read", "CreateDownload": "preview/create-download",
		}},
	}
	for _, contract := range contracts {
		if contract.typeOf.NumMethod() < len(contract.methods) {
			t.Fatalf("%s method map has more entries than its interface", contract.name)
		}
		for index := 0; index < contract.typeOf.NumMethod(); index++ {
			method := contract.typeOf.Method(index)
			// Ready/DataOrigin and BackendKind are local capability queries and
			// therefore deliberately have no provider-request workload.
			if method.Name == "Ready" || method.Name == "DataOrigin" || method.Name == "BackendKind" {
				continue
			}
			workload, found := contract.methods[method.Name]
			if !found {
				t.Fatalf("%s.%s has no production provider workload", contract.name, method.Name)
			}
			if !workloads[workload] {
				t.Fatalf("%s.%s maps to unknown workload %q", contract.name, method.Name, workload)
			}
		}
	}
}

func testContext() context.Context { return context.Background() }

func openTestRecordDomain(t *testing.T, id string) testRecordDomain {
	t.Helper()
	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
	engine, err := openRecordDomain(testContext(), backend, id)
	if err != nil {
		t.Fatal(err)
	}
	return testRecordDomain{engine: engine, ledger: ledger}
}

func TestProductionWorkloadCatalogIsComplete(t *testing.T) {
	want := []string{
		"state/get", "state/list", "state/create", "state/compare-and-swap", "state/delete",
		"namespace/list", "namespace/lookup-children", "namespace/stat", "namespace/create-directory",
		"transfer/create-upload", "transfer/upload-status", "transfer/complete-upload", "transfer/abort-upload", "transfer/create-download",
		"namespace/copy", "namespace/move", "namespace/delete", "namespace/get-operation",
		"duplicates/list-groups", "duplicates/list-occurrences", "duplicates/set-group-ignored",
		"duplicates/compare-directories", "duplicates/list-directory-overlaps", "duplicates/set-directory-ignored",
		"duplicates/preview-reconciliation", "duplicates/validate-reconciliation", "duplicates/apply-reconciliation",
		"preview/validate", "preview/check", "preview/claim", "preview/release", "preview/commit", "preview/latest", "preview/read", "preview/create-download",
		"data-plane/upload", "data-plane/download",
		"session/issue", "session/authenticate", "session/rotate", "session/logout", "session/revoke-user",
		"control/read", "control/list", "control/create", "control/update", "control/delete", "control/atomic-multi-record",
		"maintenance/startup", "maintenance/checkpoint", "maintenance/compaction", "maintenance/recovery", "maintenance/garbage-collection", "maintenance/derived-view-rebuild", "maintenance/migration",
	}
	got := make([]string, 0, len(ProductionWorkloads()))
	seen := make(map[string]bool)
	for _, workload := range ProductionWorkloads() {
		if workload.ID == "" || workload.Category == "" || workload.CurrentEvidence == "" || workload.PrototypeEvidence == "" {
			t.Fatalf("incomplete workload: %+v", workload)
		}
		if seen[workload.ID] {
			t.Fatalf("duplicate workload %q", workload.ID)
		}
		seen[workload.ID] = true
		got = append(got, workload.ID)
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("workload count=%d, want %d\ngot=%v\nwant=%v", len(got), len(want), got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("workload catalog mismatch at %d: got %q, want %q\ngot=%v\nwant=%v", index, got[index], want[index], got, want)
		}
	}
}

func TestProductionProviderRouteCatalogCoversEveryProviderBackedHTTPUseCase(t *testing.T) {
	workloads := make(map[string]bool)
	for _, workload := range ProductionWorkloads() {
		workloads[workload.ID] = true
	}
	routes := ProductionProviderRoutes()
	if len(routes) != 61 {
		t.Fatalf("provider-backed routes=%d, want 61", len(routes))
	}
	seen := make(map[string]bool)
	for _, route := range routes {
		if route.Pattern == "" || route.Cardinality == "" || len(route.CurrentDependencies) == 0 || len(route.AfterDependencies) == 0 {
			t.Fatalf("incomplete provider-backed route: %+v", route)
		}
		if seen[route.Pattern] {
			t.Fatalf("duplicate route %q", route.Pattern)
		}
		seen[route.Pattern] = true
		for _, dependency := range append(append([]string(nil), route.CurrentDependencies...), route.AfterDependencies...) {
			if !workloads[dependency] {
				t.Fatalf("route %q references unknown provider workload %q", route.Pattern, dependency)
			}
		}
	}
	if got := len(LocalOnlyRoutes()); got != 7 {
		t.Fatalf("local-only routes=%d, want 7", got)
	}
}

func TestRecordDomainUsesOneBoundedHeadAndDurableClaims(t *testing.T) {
	domain := openTestRecordDomain(t, "owner-control")
	domain.ledger.Reset()
	outcome, err := domain.engine.Mutate(testContext(), RecordMutation{ID: "create-profile", Key: "profile", Value: []byte(`{"displayName":"Ada"}`)})
	if err != nil || !outcome.Committed {
		t.Fatalf("Mutate() = %+v, %v", outcome, err)
	}
	events := domain.ledger.Events()
	if len(events) != 4 {
		t.Fatalf("record-domain mutation requests=%d, want 4; events=%+v", len(events), events)
	}
	for _, event := range events {
		if event.Subsystem == "" {
			t.Fatalf("unattributed record-domain event: %+v", event)
		}
	}
	domain.ledger.Reset()
	value, found, err := domain.engine.Get(testContext(), "profile")
	if err != nil || !found || string(value) != `{"displayName":"Ada"}` {
		t.Fatalf("Get() = %q, %t, %v", value, found, err)
	}
	if got := len(domain.ledger.Events()); got != 1 {
		t.Fatalf("record-domain read requests=%d, want 1", got)
	}
}

func TestHybridColdReadPathsSharePagedBaseReads(t *testing.T) {
	ctx := testContext()
	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
	candidate, err := openHybrid(ctx, backend, Options{DomainID: "read-paths"})
	if err != nil {
		t.Fatal(err)
	}
	engine := candidate.(*hybridEngine)
	for index, name := range []string{"alpha", "beta", "gamma"} {
		if _, err := engine.Mutate(ctx, Mutation{ID: "create-" + name, Kind: MutationCreateFile, ToArea: AreaLive, Destination: "/" + name, NodeID: name, Size: int64(index + 1), BlobIdentity: "blob-" + name}); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.Compact(ctx); err != nil {
		t.Fatal(err)
	}

	ledger.Reset()
	entry, found, err := engine.Stat(ctx, AreaLive, "/beta")
	if err != nil || !found || entry.NodeID != "beta" {
		t.Fatalf("Stat() = %+v, %t, %v", entry, found, err)
	}
	if got := len(ledger.Events()); got != 2 {
		t.Fatalf("cold one-level stat requests=%d, want head plus one page; events=%+v", got, ledger.Events())
	}

	ledger.Reset()
	entries, err := engine.List(ctx, AreaLive, "/", 100)
	if err != nil || len(entries) != 3 {
		t.Fatalf("List() = %+v, %v", entries, err)
	}
	if got := len(ledger.Events()); got != 2 {
		t.Fatalf("cold one-page list requests=%d, want head plus one page; events=%+v", got, ledger.Events())
	}

	ledger.Reset()
	children, err := engine.LookupChildren(ctx, AreaLive, "/", []string{"alpha", "gamma"})
	if err != nil || len(children) != 2 || children[0].NodeID != "alpha" || children[1].NodeID != "gamma" {
		t.Fatalf("LookupChildren() = %+v, %v", children, err)
	}
	if got := len(ledger.Events()); got != 2 {
		t.Fatalf("cold batched lookup requests=%d, want shared head and page; events=%+v", got, ledger.Events())
	}
}

func TestHybridDirectoryCopyIsLazyAndCopyOnWrite(t *testing.T) {
	ctx := testContext()
	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
	candidate, err := openHybrid(ctx, backend, Options{DomainID: "lazy-copy"})
	if err != nil {
		t.Fatal(err)
	}
	engine := candidate.(*hybridEngine)
	for _, mutation := range []Mutation{
		{ID: "source", Kind: MutationCreateDirectory, ToArea: AreaLive, Destination: "/source", NodeID: "source"},
		{ID: "sub", Kind: MutationCreateDirectory, ToArea: AreaLive, Destination: "/source/sub", NodeID: "sub"},
		{ID: "file", Kind: MutationCreateFile, ToArea: AreaLive, Destination: "/source/sub/file", NodeID: "file", Size: 7, BlobIdentity: "blob"},
	} {
		if _, err := engine.Mutate(ctx, mutation); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	ledger.Reset()
	if _, err := engine.Mutate(ctx, Mutation{ID: "copy-tree", Kind: MutationCopy, FromArea: AreaLive, ToArea: AreaLive, Source: "/source", Destination: "/clone", NodeID: "clone-root"}); err != nil {
		t.Fatal(err)
	}
	if got := len(ledger.Events()); got != 5 {
		t.Fatalf("lazy directory copy requests=%d, want 5 independent of descendants; events=%+v", got, ledger.Events())
	}
	if _, err := engine.Mutate(ctx, Mutation{ID: "rename-clone-file", Kind: MutationMove, FromArea: AreaLive, ToArea: AreaLive, Source: "/clone/sub/file", Destination: "/clone/sub/renamed"}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := engine.Stat(ctx, AreaLive, "/source/sub/file"); err != nil || !found {
		t.Fatalf("source changed after clone mutation: found=%t err=%v", found, err)
	}
	if _, found, err := engine.Stat(ctx, AreaLive, "/clone/sub/file"); err != nil || found {
		t.Fatalf("old clone path = found %t, err %v", found, err)
	}
	if _, found, err := engine.Stat(ctx, AreaLive, "/clone/sub/renamed"); err != nil || !found {
		t.Fatalf("renamed clone path = found %t, err %v", found, err)
	}
}
