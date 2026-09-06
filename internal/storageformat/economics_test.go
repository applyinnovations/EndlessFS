package storageformat

import "testing"

func TestClassifyEconomicsTargetAttributesCanonicalSubsystems(t *testing.T) {
	tests := map[string]string{
		"endlessfs/v1/control/write-gate.json":                                 "write-gate",
		"endlessfs/v1/control/writers.json":                                    "control",
		"endlessfs/v1/admissions/1/x.json":                                     "admission",
		"endlessfs/v1/state-indexes/preferences/root.json":                     "state-index",
		"endlessfs/v1/state-versions/preferences/key/version.json":             "state-value",
		"endlessfs/v1/state/preferences/key.json":                              "state-compatibility",
		"endlessfs/v1/fs/user/live/dirs/root/directory.json":                   "directory-root",
		"endlessfs/v1/fs/user/live/dirs/root/manifests/id.json":                "directory-manifest",
		"endlessfs/v1/fs/user/live/dirs/root/index/id.json":                    "directory-name-index",
		"endlessfs/v1/fs/user/live/dirs/root/sort-index/modified/id.json":      "directory-sort-index",
		"endlessfs/v1/fs/user/live/dirs/root/content-index/id.json":            "directory-content-index",
		"endlessfs/v1/fs/user/live/dirs/root/pages/id.json":                    "directory-page",
		"endlessfs/v1/fs/user/live/unknown.json":                               "filesystem",
		"endlessfs/v1/duplicates/user/file/occurrences/group/id.json":          "duplicate-occurrence",
		"endlessfs/v1/duplicates/user/file/summaries/group/00.json":            "duplicate-summary",
		"endlessfs/v1/duplicates/user/similarity/00/hash/live/id.json":         "duplicate-similarity",
		"endlessfs/v1/duplicates/user/file/ignores/group.json":                 "duplicate-ignore",
		"endlessfs/v1/duplicates/user/file/unknown/group.json":                 "duplicate",
		"endlessfs/v1/operations/user/id.json":                                 "operation",
		"endlessfs/v1/operation-preparation/user/id/run/0000/0000/0000.json":   "operation-preparation",
		"endlessfs/v1/operation-steps/user/id/set/0000.json":                   "operation-step",
		"endlessfs/v1/operation-staging/user/id/object.json":                   "operation-staging",
		"endlessfs/v1/idempotency/user/key.json":                               "idempotency",
		"endlessfs/v1/checkpoints/id.json":                                     "checkpoint",
		"endlessfs/v1/checkpoint-inventory/id/pages/0000.json":                 "checkpoint-inventory",
		"endlessfs/v1/migrations/id/root.json":                                 "migration",
		"endlessfs/v1/migration-lock.json":                                     "migration",
		"endlessfs/v1/uploads/user/id.json":                                    "upload-state",
		"endlessfs/v1/fs/user/blobs/blob":                                      "file-blob",
		"endlessfs/v1/fs/user/blobs/source -> endlessfs/v1/fs/user/blobs/dest": "file-blob",
	}
	for target, want := range tests {
		if got := ClassifyEconomicsTarget(target); got != want {
			t.Errorf("ClassifyEconomicsTarget(%q) = %q, want %q", target, got, want)
		}
	}
	if got := ClassifyEconomicsTarget("unexpected"); got != "other" {
		t.Fatalf("unexpected target = %q", got)
	}
}
