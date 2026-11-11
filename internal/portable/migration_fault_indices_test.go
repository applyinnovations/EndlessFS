package portable_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type migrationFaultTrace struct {
	Phase, Operation string
}

// migrationFaultIndices selects the first provider call of every (migration
// edge, operation, object family) class, plus three calls past the end.
// Interrupting every individual call repeats the same code path for each
// record in a family and made this matrix the slowest test in the repository;
// one interruption per class keeps the crash-safety guarantee for each
// distinct operation the migration performs.
func migrationFaultIndices(trace []migrationFaultTrace) []int {
	selected := make(map[int]bool)
	seen := make(map[string]bool)
	for index, event := range trace {
		phaseParts := strings.Split(event.Phase, ":")
		// Group by migration edge; every step boundary within an edge is
		// already interrupted by the ledger-edge durable-boundary test.
		if len(phaseParts) > 2 {
			event.Phase = strings.Join(phaseParts[:2], ":")
		}
		parts := strings.Fields(event.Operation)
		if len(parts) == 0 {
			panic("empty migration transport operation")
		}
		switch parts[0] {
		case "PUT", "DELETE", "COPY", "ABORT-UPLOAD", "BEGIN-UPLOAD", "RESUME-UPLOAD", "UPLOAD-PROGRESS", "CREATE-DOWNLOAD",
			"GET", "HEAD", "LIST", "OPEN", "VERIFY":
		default:
			panic("unclassified migration transport operation: " + event.Operation)
		}
		family := event.Operation
		if len(parts) > 1 {
			family = migrationObjectFamily(parts[1])
		}
		key := event.Phase + "/" + parts[0] + "/" + family
		if !seen[key] {
			seen[key] = true
			selected[index+1] = true
		}
	}
	for beyond := 1; beyond <= 3; beyond++ {
		selected[len(trace)+beyond] = true
	}
	indices := make([]int, 0, len(selected))
	for index := range selected {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	return indices
}

func migrationObjectFamily(key string) string {
	if strings.HasPrefix(key, "endlessfs/v1/domains/") {
		parts := strings.Split(strings.TrimPrefix(key, "endlessfs/v1/domains/"), "/")
		if strings.HasSuffix(key, "/head.json") {
			return "domain/" + parts[0] + "/head"
		}
		for _, representation := range []string{"pages", "packs"} {
			if strings.Contains(key, "/"+representation+"/") {
				return "domain/" + parts[0] + "/" + representation
			}
		}
	}
	family := storageformat.ClassifyEconomicsTarget(key)
	if family == "other" {
		// Records under one unclassified prefix (for example GC marks) share
		// a code path; keep top-level control objects distinct.
		if index := strings.LastIndex(key, "/"); strings.Count(key, "/") > 2 {
			return "unclassified/" + key[:index]
		}
		return "unclassified/" + key
	}
	return family
}

func TestMigrationFaultIndicesCoverEveryOperationClass(t *testing.T) {
	trace := []migrationFaultTrace{
		{"first", "GET endlessfs/v1/domains/catalog/pages/a.json"},
		{"first", "GET endlessfs/v1/domains/catalog/pages/b.json"},
		{"first", "GET endlessfs/v1/domains/catalog/pages/c.json"},
		{"first", "PUT head"}, {"first", "DELETE lease"},
		{"first", "COPY source destination"}, {"first", "ABORT-UPLOAD"},
		{"next", "GET endlessfs/v1/domains/catalog/pages/d.json"},
		{"next", "HEAD endlessfs/v1/fs/owner/blobs/blob"},
	}
	indices := migrationFaultIndices(trace)
	want := []int{1, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	if len(indices) != len(want) {
		t.Fatalf("selected boundaries = %v; want %v", indices, want)
	}
	for index := range want {
		if indices[index] != want[index] {
			t.Fatalf("selected boundaries = %v; want %v", indices, want)
		}
	}
}
