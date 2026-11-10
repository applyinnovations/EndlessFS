package providerbudget_test

import (
	"reflect"
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
		current := strings.HasSuffix(budget.Name, "schema-011") || strings.HasSuffix(budget.Name, "schema-012") || strings.Contains(budget.Name, "008-to-011") || strings.Contains(budget.Name, "008-to-012") || strings.HasPrefix(budget.Name, "preview-") || strings.HasPrefix(budget.Name, "file-data-")
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
			"CreateUpload": "transfer/create-upload", "UploadStatus": "transfer/upload-status", "ResumeUpload": "transfer/resume-upload", "CompleteUpload": "transfer/complete-upload", "AbortUpload": "transfer/abort-upload", "CreateDownload": "transfer/create-download",
			"Copy": "namespace/copy", "Move": "namespace/move", "Delete": "namespace/delete", "GetOperation": "namespace/get-operation",
		}, local: map[string]bool{"Ready": true, "DataOrigin": true, "BackendKind": true}},
		{name: "provider.TrashStorage", typeOf: reflect.TypeOf((*provider.TrashStorage)(nil)).Elem(), methods: map[string]string{
			"MoveToTrash": "namespace/trash", "ListTrash": "namespace/list", "RestoreFromTrash": "namespace/restore", "DeleteFromTrash": "namespace/delete-trash",
		}},
		{name: "provider.BatchStorage", typeOf: reflect.TypeOf((*provider.BatchStorage)(nil)).Elem(), methods: map[string]string{
			"BatchCopyMove": "namespace/batch-copy-move", "BatchMoveToTrash": "namespace/trash", "BatchRestoreFromTrash": "namespace/restore", "BatchDeleteFromTrash": "namespace/delete-trash", "GetBatchOperation": "namespace/get-operation",
		}},
		{name: "provider.UploadBatchStorage", typeOf: reflect.TypeOf((*provider.UploadBatchStorage)(nil)).Elem(), methods: map[string]string{
			"CreateUploadBatch": "transfer/create-upload-batch",
		}},
		{name: "provider.UploadAdmissionStorage", typeOf: reflect.TypeOf((*provider.UploadAdmissionStorage)(nil)).Elem(), methods: map[string]string{"AdmitUploads": "transfer/upload-admission"}},
		{name: "provider.DirectoryPreparationStorage", typeOf: reflect.TypeOf((*provider.DirectoryPreparationStorage)(nil)).Elem(), methods: map[string]string{"PrepareDirectories": "namespace/prepare-directories"}},
		{name: "provider.UploadLifecycleStorage", typeOf: reflect.TypeOf((*provider.UploadLifecycleStorage)(nil)).Elem(), methods: map[string]string{"UploadLifecycle": "transfer/upload-lifecycle-resume"}},
		{name: "provider.UploadActionStorage", typeOf: reflect.TypeOf((*provider.UploadActionStorage)(nil)).Elem(), methods: map[string]string{"UploadActions": "transfer/upload-actions"}},
		{name: "provider.DuplicateStorage", typeOf: reflect.TypeOf((*provider.DuplicateStorage)(nil)).Elem(), methods: map[string]string{
			"ListDuplicateGroups": "duplicates/list-groups", "ListDuplicateOccurrences": "duplicates/list-occurrences", "SetDuplicateGroupIgnored": "duplicates/set-group-ignored",
			"CompareDuplicateDirectories": "duplicates/compare-directories", "ListDuplicateDirectoryOverlaps": "duplicates/list-directory-overlaps", "SetDuplicateDirectoryIgnored": "duplicates/set-directory-ignored",
			"PreviewDuplicateReconciliation": "duplicates/preview-reconciliation", "ValidateDuplicateReconciliation": "duplicates/validate-reconciliation", "ApplyDuplicateReconciliation": "duplicates/apply-reconciliation",
		}},
		{name: "preview.Store", typeOf: reflect.TypeOf((*preview.Store)(nil)).Elem(), methods: map[string]string{
			"Validate": "preview/validate", "Check": "preview/check", "Claim": "preview/claim", "Release": "preview/release", "Commit": "preview/commit", "Latest": "preview/latest", "Read": "preview/read", "CreateDownload": "preview/create-download",
			"ResolveReady": "preview/resolve-ready", "RecordReady": "preview/record-ready", "CreateKnownDownload": "preview/create-known-download",
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
