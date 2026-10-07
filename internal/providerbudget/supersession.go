package providerbudget

// BudgetSupersessions preserves historical measured inputs while identifying
// the current executable geometric/boundary workload. It never edits a ledger.
func BudgetSupersessions() map[string]string {
	return map[string]string{
		"trash-batch-10000-schema-011":                "trash-batch-1024-schema-011",
		"trash-batch-10000-replay-schema-011":         "trash-batch-1024-replay-schema-011",
		"trash-batch-10000-denied-schema-011":         "trash-batch-1024-denied-schema-011",
		"restore-batch-10000-schema-011":              "restore-batch-1024-schema-011",
		"restore-batch-10000-replay-schema-011":       "restore-batch-1024-replay-schema-011",
		"empty-trash-10000-schema-011":                "empty-trash-1024-schema-011",
		"empty-trash-10000-replay-schema-011":         "empty-trash-1024-replay-schema-011",
		"batch-copy-10000-schema-011":                 "batch-copy-1024-schema-011",
		"batch-move-10000-schema-011":                 "batch-move-1024-schema-011",
		"namespace-list-page-10000-schema-011":        "namespace-list-page-1024-schema-011",
		"upload-plan-sizes-10000-schema-011":          "upload-plan-sizes-1024-schema-011",
		"upload-plan-fingerprints-10000-schema-011":   "upload-plan-fingerprints-1024-schema-011",
		"file-create-upload-batch-10000-schema-011":   "file-create-upload-batch-2001-schema-011",
		"file-complete-upload-batch-10000-schema-011": "file-complete-upload-batch-2001-schema-011",
		"file-abort-upload-batch-10000-schema-011":    "file-abort-upload-batch-2001-schema-011",
	}
}
