package storageformat

import "strings"

// ClassifyEconomicsTarget maps a test-only object target to the physical
// subsystem that caused the provider request. It exposes no virtual path or
// provider-native value and is deliberately diagnostic rather than
// authoritative.
func ClassifyEconomicsTarget(target string) string {
	if strings.Contains(target, " -> ") {
		target = strings.SplitN(target, " -> ", 2)[0]
	}
	for _, rule := range []struct {
		match string
		name  string
	}{
		{match: root + "control/write-gate.json", name: "write-gate"},
		{match: root + "control/", name: "control"},
		{match: root + "admissions/", name: "admission"},
		{match: root + "state-indexes/", name: "state-index"},
		{match: root + "state-versions/", name: "state-value"},
		{match: root + "state/", name: "state-compatibility"},
		{match: root + "operation-preparation/", name: "operation-preparation"},
		{match: root + "operation-steps/", name: "operation-step"},
		{match: root + "operation-staging/", name: "operation-staging"},
		{match: root + "operations/", name: "operation"},
		{match: root + "idempotency/", name: "idempotency"},
		{match: root + "checkpoint-inventory/", name: "checkpoint-inventory"},
		{match: root + "checkpoints/", name: "checkpoint"},
		{match: root + "migrations/", name: "migration"},
		{match: root + "migration-", name: "migration"},
		{match: root + "duplicates/", name: "duplicate"},
		{match: root + "uploads/", name: "upload-state"},
		{match: root + "fs/", name: "filesystem"},
	} {
		if strings.HasPrefix(target, rule.match) {
			switch rule.name {
			case "duplicate":
				return classifyDuplicateTarget(target)
			case "filesystem":
				return classifyFilesystemTarget(target)
			default:
				return rule.name
			}
		}
	}
	return "other"
}

func classifyDuplicateTarget(target string) string {
	switch {
	case strings.Contains(target, "/occurrences/"):
		return "duplicate-occurrence"
	case strings.Contains(target, "/summaries/"):
		return "duplicate-summary"
	case strings.Contains(target, "/similarity/"):
		return "duplicate-similarity"
	case strings.Contains(target, "/ignores/") || strings.Contains(target, "/directory-ignores/"):
		return "duplicate-ignore"
	default:
		return "duplicate"
	}
}

func classifyFilesystemTarget(target string) string {
	switch {
	case strings.Contains(target, "/blobs/"):
		return "file-blob"
	case strings.Contains(target, "/sort-index/"):
		return "directory-sort-index"
	case strings.Contains(target, "/content-index/"):
		return "directory-content-index"
	case strings.Contains(target, "/index/"):
		return "directory-name-index"
	case strings.Contains(target, "/manifests/"):
		return "directory-manifest"
	case strings.HasSuffix(target, "/directory.json"):
		return "directory-root"
	case strings.Contains(target, "/pages/"):
		return "directory-page"
	default:
		return "filesystem"
	}
}
