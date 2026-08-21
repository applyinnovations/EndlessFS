package portable

import (
	"fmt"
	"reflect"
	"testing"
)

func TestStorageSchemaLedgerIsLinearAndAppendOnly(t *testing.T) {
	wantIDs := []storageSchemaID{
		"endlessfs-portable-v1/schema-001",
		"endlessfs-portable-v1/schema-002",
		"endlessfs-portable-v1/schema-003",
	}
	if len(storageSchemaLedger) != len(wantIDs) {
		t.Fatalf("storage schema ledger length = %d; want %d", len(storageSchemaLedger), len(wantIDs))
	}
	checkpoints := make(map[string]struct{})
	featureSignatures := make(map[string]struct{})
	for index, schema := range storageSchemaLedger {
		if schema.id != wantIDs[index] {
			t.Fatalf("storage schema ledger[%d] = %q; want %q", index, schema.id, wantIDs[index])
		}
		signature := fmt.Sprint(schema.features)
		if _, duplicate := featureSignatures[signature]; duplicate {
			t.Fatalf("storage schema %q reuses feature signature %s", schema.id, signature)
		}
		featureSignatures[signature] = struct{}{}
		for featureIndex := 1; featureIndex < len(schema.features); featureIndex++ {
			if schema.features[featureIndex-1] >= schema.features[featureIndex] {
				t.Fatalf("storage schema %q features are not unique and sorted: %v", schema.id, schema.features)
			}
		}
		if index == 0 {
			if schema.migrationFromPrevious != nil {
				t.Fatalf("first schema %q unexpectedly has a predecessor migration", schema.id)
			}
			continue
		}
		migration := schema.migrationFromPrevious
		if migration == nil {
			t.Fatalf("schema %q has no migration from its predecessor", schema.id)
		}
		if migration.from != storageSchemaLedger[index-1].id || migration.to != schema.id {
			t.Fatalf("schema %q incoming migration = %q -> %q; want %q -> %q", schema.id, migration.from, migration.to, storageSchemaLedger[index-1].id, schema.id)
		}
		if migration.id == "" || migration.checkpointID == "" || migration.run == nil {
			t.Fatalf("schema %q migration is incomplete: %+v", schema.id, migration)
		}
		if _, duplicate := checkpoints[migration.checkpointID]; duplicate {
			t.Fatalf("migration checkpoint %q is reused", migration.checkpointID)
		}
		checkpoints[migration.checkpointID] = struct{}{}
	}
}

func TestStorageSchemaReleaseLedgerDefinesDerivedValidityRanges(t *testing.T) {
	want := map[storageSchemaID][]StorageSchemaReleaseRange{
		"endlessfs-portable-v1/schema-001": {{First: "v0.1.0", Before: "v0.1.5"}},
		"endlessfs-portable-v1/schema-002": nil,
		"endlessfs-portable-v1/schema-003": {{First: "v0.1.5"}},
	}
	for _, schema := range storageSchemaLedger {
		got := releaseRangesForSchema(schema.id)
		if !reflect.DeepEqual(got, want[schema.id]) {
			t.Fatalf("schema %q release ranges = %+v; want %+v", schema.id, got, want[schema.id])
		}
	}
}

func TestStorageSchemaReleaseLedgerIsCanonicalOrderedAndReferencesKnownSchemas(t *testing.T) {
	var previous releaseVersion
	for index, boundary := range storageSchemaReleaseLedger {
		version, err := parseReleaseVersion(boundary.first)
		if err != nil {
			t.Fatalf("release boundary %d: %v", index, err)
		}
		if index > 0 && !previous.less(version) {
			t.Fatalf("release boundary %d is not later than its predecessor", index)
		}
		if _, found := schemaDefinition(boundary.schema); !found {
			t.Fatalf("release boundary %d references unknown schema %q", index, boundary.schema)
		}
		previous = version
	}
}

func TestStorageSchemaLedgerResolvesReleaseValidityRanges(t *testing.T) {
	tests := []struct {
		release string
		want    storageSchemaID
		found   bool
	}{
		{release: "v0.1.0", want: storageSchema001, found: true},
		{release: "v0.1.4", want: storageSchema001, found: true},
		{release: "v0.1.5", want: storageSchema003, found: true},
		{release: "v0.1.999", want: storageSchema003, found: true},
		{release: "v0.0.9"},
		{release: "0.1.7"},
		{release: "v0.1"},
		{release: "v0.01.7"},
	}
	for _, test := range tests {
		t.Run(test.release, func(t *testing.T) {
			got, found := StorageSchemaForRelease(test.release)
			if found != test.found || got != test.want.String() {
				t.Fatalf("StorageSchemaForRelease(%q) = %q, %t; want %q, %t", test.release, got, found, test.want, test.found)
			}
		})
	}
}

func TestStorageSchemaHistoryIsDefensiveLedgerSnapshot(t *testing.T) {
	history := StorageSchemaHistory()
	if len(history) != len(storageSchemaLedger) {
		t.Fatalf("history length = %d; want %d", len(history), len(storageSchemaLedger))
	}
	history[0].ID = "mutated"
	history[0].Features = append(history[0].Features, "mutated")
	history[0].Releases[0].First = "v9.9.9"
	again := StorageSchemaHistory()
	if again[0].ID != storageSchema001.String() || again[0].Releases[0].First != "v0.1.0" {
		t.Fatalf("storage schema history exposed mutable ledger state: %+v", again[0])
	}
}

func TestStorageSchemaLedgerBuildsEveryRemainingMigrationPath(t *testing.T) {
	tests := []struct {
		from storageSchemaID
		want []storageMigrationID
	}{
		{
			from: "endlessfs-portable-v1/schema-001",
			want: []storageMigrationID{"schema-001-to-002", "schema-002-to-003"},
		},
		{
			from: "endlessfs-portable-v1/schema-002",
			want: []storageMigrationID{"schema-002-to-003"},
		},
		{from: "endlessfs-portable-v1/schema-003"},
	}
	for _, test := range tests {
		t.Run(fmt.Sprint(test.from), func(t *testing.T) {
			path, err := storageMigrationPath(test.from)
			if err != nil {
				t.Fatal(err)
			}
			var got []storageMigrationID
			for index := range path {
				got = append(got, path[index].id)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("migration path from %q = %v; want %v", test.from, got, test.want)
			}
		})
	}
}

func TestStorageSchemaLedgerRejectsUnknownEntryPoint(t *testing.T) {
	if _, err := storageMigrationPath("endlessfs-portable-v1/schema-999"); err == nil {
		t.Fatal("unknown storage schema unexpectedly has a migration path")
	}
}
