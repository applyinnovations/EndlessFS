package providerbudget_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	"github.com/applyinnovations/endlessfs/internal/preview"
	"github.com/applyinnovations/endlessfs/internal/provider"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/state"
)

func TestProviderBudgetCatalogCoversApplicationContractsAndRatchets(t *testing.T) {
	workloads := make(map[string]providerbudget.ProductionWorkload)
	referencedBudgets := make(map[string]bool)
	for _, workload := range providerbudget.ProductionWorkloads() {
		if workload.ID == "" || workload.Category == "" || len(workload.Budgets) == 0 {
			t.Fatalf("incomplete production workload: %+v", workload)
		}
		if _, exists := workloads[workload.ID]; exists {
			t.Fatalf("duplicate production workload %q", workload.ID)
		}
		workloads[workload.ID] = workload
		for _, name := range workload.Budgets {
			referencedBudgets[name] = true
		}
	}
	ratchet, err := gcs.RegionalStandardFlatBudgetRatchet()
	if err != nil {
		t.Fatal(err)
	}
	for _, workload := range workloads {
		for _, name := range workload.Budgets {
			if _, found := ratchet.Latest(name); !found {
				t.Errorf("production workload %q has no GCS ratchet %q", workload.ID, name)
			}
		}
	}
	for _, budget := range ratchet.Epochs[len(ratchet.Epochs)-1].Budgets {
		current := strings.HasSuffix(budget.Name, "schema-009") || strings.Contains(budget.Name, "008-to-009") || budget.Name == "preview-validate"
		if current && !referencedBudgets[budget.Name] {
			t.Errorf("current provider ratchet %q is absent from the production workload catalog", budget.Name)
		}
	}

	contracts := []struct {
		name    string
		typeOf  reflect.Type
		methods map[string]string
		local   map[string]bool
	}{
		{name: "state.Store", typeOf: reflect.TypeOf((*state.Store)(nil)).Elem(), methods: map[string]string{"Get": "state/get", "List": "state/list", "Create": "state/create", "CompareAndSwap": "state/compare-and-swap", "Delete": "state/delete"}},
		{name: "state.AtomicStore", typeOf: reflect.TypeOf((*state.AtomicStore)(nil)).Elem(), methods: map[string]string{
			"Get": "state/get", "List": "state/list", "Create": "state/create", "CompareAndSwap": "state/compare-and-swap", "Delete": "state/delete", "Mutate": "state/mutate",
		}},
		{name: "state.TransactionalStore", typeOf: reflect.TypeOf((*state.TransactionalStore)(nil)).Elem(), methods: map[string]string{
			"Get": "state/get", "List": "state/list", "Create": "state/create", "CompareAndSwap": "state/compare-and-swap", "Delete": "state/delete", "Mutate": "state/mutate", "Transact": "state/transact",
		}},
		{name: "provider.Storage", typeOf: reflect.TypeOf((*provider.Storage)(nil)).Elem(), methods: map[string]string{
			"List": "namespace/list", "LookupChildren": "namespace/lookup-children", "Stat": "namespace/stat", "CreateDirectory": "namespace/create-directory",
			"CreateUpload": "transfer/create-upload", "UploadStatus": "transfer/upload-status", "CompleteUpload": "transfer/complete-upload", "AbortUpload": "transfer/abort-upload", "CreateDownload": "transfer/create-download",
			"Copy": "namespace/copy", "Move": "namespace/move", "Delete": "namespace/delete", "GetOperation": "namespace/get-operation",
		}, local: map[string]bool{"Ready": true, "DataOrigin": true, "BackendKind": true}},
		{name: "provider.TrashStorage", typeOf: reflect.TypeOf((*provider.TrashStorage)(nil)).Elem(), methods: map[string]string{
			"MoveToTrash": "namespace/trash", "ListTrash": "namespace/list", "RestoreFromTrash": "namespace/restore", "DeleteFromTrash": "namespace/delete-trash",
		}},
		{name: "provider.BatchStorage", typeOf: reflect.TypeOf((*provider.BatchStorage)(nil)).Elem(), methods: map[string]string{
			"BatchCopyMove": "namespace/batch-copy-move", "BatchMoveToTrash": "namespace/trash", "BatchDeleteFromTrash": "namespace/delete-trash", "GetBatchOperation": "namespace/get-operation",
		}},
		{name: "provider.UploadBatchStorage", typeOf: reflect.TypeOf((*provider.UploadBatchStorage)(nil)).Elem(), methods: map[string]string{
			"CreateUploadBatch": "transfer/create-upload-batch",
		}},
		{name: "provider.DuplicateStorage", typeOf: reflect.TypeOf((*provider.DuplicateStorage)(nil)).Elem(), methods: map[string]string{
			"ListDuplicateGroups": "duplicates/list-groups", "ListDuplicateOccurrences": "duplicates/list-occurrences", "SetDuplicateGroupIgnored": "duplicates/set-group-ignored",
			"CompareDuplicateDirectories": "duplicates/compare-directories", "ListDuplicateDirectoryOverlaps": "duplicates/list-directory-overlaps", "SetDuplicateDirectoryIgnored": "duplicates/set-directory-ignored",
			"PreviewDuplicateReconciliation": "duplicates/preview-reconciliation", "ValidateDuplicateReconciliation": "duplicates/validate-reconciliation", "ApplyDuplicateReconciliation": "duplicates/apply-reconciliation",
		}},
		{name: "preview.Store", typeOf: reflect.TypeOf((*preview.Store)(nil)).Elem(), methods: map[string]string{
			"Validate": "preview/validate", "Check": "preview/check", "Claim": "preview/claim", "Release": "preview/release", "Commit": "preview/commit", "Latest": "preview/latest", "Read": "preview/read", "CreateDownload": "preview/create-download",
		}, local: map[string]bool{"Ready": true, "DataOrigin": true, "BackendKind": true}},
	}
	for _, contract := range contracts {
		for index := 0; index < contract.typeOf.NumMethod(); index++ {
			method := contract.typeOf.Method(index).Name
			if contract.local[method] {
				continue
			}
			workload, found := contract.methods[method]
			if !found {
				t.Errorf("%s.%s has no provider workload classification", contract.name, method)
				continue
			}
			if _, found := workloads[workload]; !found {
				t.Errorf("%s.%s maps to unknown workload %q", contract.name, method, workload)
			}
		}
	}
}

func TestProviderBudgetCatalogRatchetsAreBackedByExecutableTests(t *testing.T) {
	literals := providerBudgetTestLiterals(t)
	for _, workload := range providerbudget.ProductionWorkloads() {
		for _, budget := range workload.Budgets {
			if !literals[budget] {
				t.Errorf("production budget %q for workload %q has no executable test reference", budget, workload.ID)
			}
		}
	}
}

func providerBudgetTestLiterals(t *testing.T) map[string]bool {
	t.Helper()
	_, current, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(current), "..")
	result := make(map[string]bool)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, "_test.go") || path == current {
			return nil
		}
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			if value, err := strconv.Unquote(literal.Value); err == nil {
				result[value] = true
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestProviderBudgetCatalogClassifiesEveryRegisteredHTTPRoute(t *testing.T) {
	classified := make(map[string]bool)
	workloads := make(map[string]bool)
	for _, workload := range providerbudget.ProductionWorkloads() {
		workloads[workload.ID] = true
	}
	for _, route := range providerbudget.ProductionProviderRoutes() {
		if route.Pattern == "" || route.Cardinality == "" || len(route.Workloads) == 0 || classified[route.Pattern] {
			t.Fatalf("invalid provider route classification: %+v", route)
		}
		classified[route.Pattern] = true
		for _, workload := range route.Workloads {
			if !workloads[workload] {
				t.Errorf("route %q maps to unknown workload %q", route.Pattern, workload)
			}
		}
	}
	for _, pattern := range providerbudget.LocalOnlyRoutes() {
		if classified[pattern] {
			t.Fatalf("route %q is both provider-backed and local-only", pattern)
		}
		classified[pattern] = true
	}
	actual := registeredHTTPRoutes(t)
	want := make([]string, 0, len(classified))
	for pattern := range classified {
		want = append(want, pattern)
	}
	sort.Strings(want)
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("HTTP route economics classification is stale\nregistered=%v\nclassified=%v", actual, want)
	}
}

func registeredHTTPRoutes(t *testing.T) []string {
	t.Helper()
	_, current, _, _ := runtime.Caller(0)
	directory := filepath.Join(filepath.Dir(current), "..", "httpapi")
	files, err := filepath.Glob(filepath.Join(directory, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	routes := make(map[string]bool)
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			literal, literalOK := call.Args[0].(*ast.BasicLit)
			if !ok || selector.Sel.Name != "HandleFunc" && selector.Sel.Name != "Handle" || !literalOK || literal.Kind != token.STRING {
				return true
			}
			value, unquoteErr := strconv.Unquote(literal.Value)
			if unquoteErr == nil {
				if strings.Contains(value, " ") {
					routes[value] = true
				} else if selector.Sel.Name == "Handle" && value == "/" {
					routes["GET /"] = true
				}
			}
			return true
		})
	}
	result := make([]string, 0, len(routes))
	for pattern := range routes {
		result = append(result, pattern)
	}
	sort.Strings(result)
	return result
}
