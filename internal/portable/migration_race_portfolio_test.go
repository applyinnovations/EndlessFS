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

// Preserve every mutating provider boundary and the first/last read of each
// record family in each migration phase. The ordinary gate keeps every read.
func migrationRaceIndices(trace []migrationFaultTrace) []int {
	selected := make(map[int]bool)
	first, last := make(map[string]int), make(map[string]int)
	for index, event := range trace {
		phaseParts := strings.Split(event.Phase, ":")
		if len(phaseParts) > 3 {
			event.Phase = strings.Join(phaseParts[:3], ":")
		}
		parts := strings.Fields(event.Operation)
		if len(parts) == 0 {
			panic("empty migration transport operation")
		}
		immutable := len(parts) > 1 && (strings.Contains(parts[1], "/pages/") || strings.Contains(parts[1], "/packs/") || strings.Contains(parts[1], "/nodes/") || strings.Contains(parts[1], "/manifests/") || strings.Contains(parts[1], "/state-versions/"))
		switch {
		case (parts[0] == "PUT" || parts[0] == "DELETE") && immutable:
			key := event.Phase + "/" + parts[0] + "/" + migrationReadFamily(parts[1])
			if _, found := first[key]; !found {
				first[key] = index + 1
			}
			last[key] = index + 1
		case parts[0] == "PUT" || parts[0] == "DELETE" || parts[0] == "COPY" || parts[0] == "ABORT-UPLOAD" || parts[0] == "BEGIN-UPLOAD" || parts[0] == "RESUME-UPLOAD" || parts[0] == "UPLOAD-PROGRESS" || parts[0] == "CREATE-DOWNLOAD":
			selected[index+1] = true
		case parts[0] == "GET" || parts[0] == "HEAD" || parts[0] == "LIST" || parts[0] == "OPEN" || parts[0] == "VERIFY":
			family := event.Operation
			if len(parts) > 1 {
				family = migrationReadFamily(parts[1])
			}
			key := event.Phase + "/" + parts[0] + "/" + family
			if _, found := first[key]; !found {
				first[key] = index + 1
			}
			last[key] = index + 1
		default:
			panic("unclassified migration transport operation: " + event.Operation)
		}
	}
	for key, start := range first {
		selected[start], selected[last[key]] = true, true
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

func migrationReadFamily(key string) string {
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
		return "unclassified-exact-key/" + key
	}
	return family
}

func TestMigrationRacePortfolioPreservesMutationAndReadClasses(t *testing.T) {
	trace := []migrationFaultTrace{
		{"first", "GET endlessfs/v1/domains/catalog/pages/a.json"},
		{"first", "GET endlessfs/v1/domains/catalog/pages/b.json"},
		{"first", "GET endlessfs/v1/domains/catalog/pages/c.json"},
		{"first", "PUT head"}, {"first", "DELETE lease"},
		{"first", "COPY source destination"}, {"first", "ABORT-UPLOAD"},
		{"next", "GET endlessfs/v1/domains/catalog/pages/d.json"},
		{"next", "HEAD endlessfs/v1/fs/owner/blobs/blob"},
	}
	indices := migrationRaceIndices(trace)
	want := []int{1, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	if len(indices) != len(want) {
		t.Fatalf("selected boundaries = %v; want %v", indices, want)
	}
	for index := range want {
		if indices[index] != want[index] {
			t.Fatalf("selected boundaries = %v; want %v", indices, want)
		}
	}
}
