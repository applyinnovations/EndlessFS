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
		"endlessfs-portable-v1/schema-004",
		"endlessfs-portable-v1/schema-005",
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
		wantBinding := storageGateFeatureBound
		if index == 0 {
			wantBinding = storageGateLegacyUnbound
		}
		if schema.gateBinding != wantBinding {
			t.Fatalf("storage schema %q gate binding = %q; want %q", schema.id, schema.gateBinding, wantBinding)
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

func TestStorageSchemaGateDetectionUsesEpochBindingRepresentation(t *testing.T) {
	current := []string{
		"directory-content-digests-v1",
		"directory-manifests",
		"duplicate-catalog-v1",
		"fenced-operations",
		"generated-previews-v1",
		"metadata-only-checkpoints-v1",
		"paged-operation-steps-v1",
		"persistent-directory-indexes-v1",
		"persistent-state-indexes-v1",
		"portable-checkpoints",
		"preview-integrity-crc32c-v1",
		"provider-content-fingerprints-v1",
		"recursive-byte-aggregates-v1",
		"recursive-file-count-aggregates-v1",
		"resumable-operation-preparation-v1",
	}
	legacy, found := detectWriteGateSchema(nil, current)
	if !found || legacy.id != storageSchema001 {
		t.Fatalf("legacy unbound gate detected as %+v, %t; want schema 001", legacy, found)
	}
	if _, found := detectWriteGateSchema([]string{"directory-manifests"}, current); found {
		t.Fatal("partially feature-bound legacy gate was accepted")
	}
	for _, schemaID := range []storageSchemaID{storageSchema002, storageSchema003, storageSchema004, "endlessfs-portable-v1/schema-005"} {
		features, _ := schemaFeatures(schemaID, current)
		detected, found := detectWriteGateSchema(features, current)
		if !found || detected.id != schemaID {
			t.Fatalf("feature-bound gate %q detected as %+v, %t", schemaID, detected, found)
		}
	}
}

func TestStorageSchemaReleaseLedgerDefinesDerivedValidityRanges(t *testing.T) {
	want := map[storageSchemaID][]StorageSchemaReleaseRange{
		"endlessfs-portable-v1/schema-001": {{First: "v0.1.0", Before: "v0.1.5"}},
		"endlessfs-portable-v1/schema-002": nil,
		"endlessfs-portable-v1/schema-003": {{First: "v0.1.5", Before: "v0.2.0"}},
		"endlessfs-portable-v1/schema-004": nil,
		"endlessfs-portable-v1/schema-005": {{First: "v0.2.0"}},
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
		{release: "v0.1.14", want: storageSchema003, found: true},
		{release: "v0.1.15", want: storageSchema003, found: true},
		{release: "v0.1.999", want: storageSchema003, found: true},
		{release: "v0.2.0", want: "endlessfs-portable-v1/schema-005", found: true},
		{release: "v0.2.999", want: "endlessfs-portable-v1/schema-005", found: true},
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
			want: []storageMigrationID{"schema-001-to-002", "schema-002-to-003", "schema-003-to-004", "schema-004-to-005"},
		},
		{
			from: "endlessfs-portable-v1/schema-002",
			want: []storageMigrationID{"schema-002-to-003", "schema-003-to-004", "schema-004-to-005"},
		},
		{from: "endlessfs-portable-v1/schema-003", want: []storageMigrationID{"schema-003-to-004", "schema-004-to-005"}},
		{from: "endlessfs-portable-v1/schema-004", want: []storageMigrationID{"schema-004-to-005"}},
		{from: "endlessfs-portable-v1/schema-005"},
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

func TestStorageSchemaLedgerRejectsEveryMalformedReleaseShape(t *testing.T) {
	for _, release := range []string{
		"1.2.3",
		"v1.2",
		"v1..3",
		"v1.02.3",
		"v1.x.3",
		"v1.-2.3",
	} {
		if _, err := parseReleaseVersion(release); err == nil {
			t.Errorf("parseReleaseVersion(%q) succeeded", release)
		}
	}
}

func TestStorageSchemaReleaseOrderingComparesEveryVersionComponent(t *testing.T) {
	tests := []struct {
		left, right releaseVersion
		want        bool
	}{
		{left: releaseVersion{major: 1}, right: releaseVersion{major: 2}, want: true},
		{left: releaseVersion{major: 2}, right: releaseVersion{major: 1}},
		{left: releaseVersion{major: 1, minor: 1}, right: releaseVersion{major: 1, minor: 2}, want: true},
		{left: releaseVersion{major: 1, minor: 2}, right: releaseVersion{major: 1, minor: 1}},
		{left: releaseVersion{major: 1, minor: 2, patch: 3}, right: releaseVersion{major: 1, minor: 2, patch: 4}, want: true},
	}
	for _, test := range tests {
		if got := test.left.less(test.right); got != test.want {
			t.Errorf("%+v.less(%+v) = %t; want %t", test.left, test.right, got, test.want)
		}
	}
}

func TestStorageSchemaHelpersFailClosedForUnknownOrBrokenLedgerState(t *testing.T) {
	unknown := storageSchemaID("endlessfs-portable-v1/schema-999")
	if _, found := schemaDefinition(unknown); found {
		t.Fatal("unknown schema has a definition")
	}
	if _, found := schemaIndex(unknown); found {
		t.Fatal("unknown schema has an index")
	}
	if _, found := schemaFeatures(unknown, nil); found {
		t.Fatal("unknown schema has features")
	}
	if _, found := detectStorageSchema([]string{"unknown-feature"}, nil); found {
		t.Fatal("unknown storage feature signature was accepted")
	}
	if _, found := detectWriteGateSchema([]string{"unknown-feature"}, nil); found {
		t.Fatal("unknown gate feature signature was accepted")
	}
	if schemaAtLeast([]string{"unknown-feature"}, storageSchema001, nil) {
		t.Fatal("unknown storage feature signature satisfied a minimum")
	}
	currentFeatures, found := schemaFeatures("endlessfs-portable-v1/schema-005", nil)
	if !found {
		t.Fatal("current schema has no features")
	}
	if schemaAtLeast(currentFeatures, unknown, nil) {
		t.Fatal("unknown storage minimum was accepted")
	}
	if writeGateSchemaAtLeast([]string{"unknown-feature"}, storageSchema001, nil) {
		t.Fatal("unknown gate feature signature satisfied a minimum")
	}
	if writeGateSchemaAtLeast(currentFeatures, unknown, nil) {
		t.Fatal("unknown gate minimum was accepted")
	}
	if _, found := migrationForCheckpoint("unknown-checkpoint"); found {
		t.Fatal("unknown migration checkpoint was accepted")
	}

	original := storageSchemaLedger[1].migrationFromPrevious
	t.Cleanup(func() { storageSchemaLedger[1].migrationFromPrevious = original })
	storageSchemaLedger[1].migrationFromPrevious = nil
	if _, err := storageMigrationPath(storageSchema001); err == nil {
		t.Fatal("broken adjacent migration edge was accepted")
	}
}
