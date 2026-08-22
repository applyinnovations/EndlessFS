package portable_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type schema001DirectoryRoot struct {
	SchemaVersion int                           `json:"schemaVersion"`
	DirectoryID   string                        `json:"directoryID"`
	ManifestID    string                        `json:"manifestID"`
	Pending       *schema001DirectoryTransition `json:"pending,omitempty"`
}

type schema001DirectoryTransition struct {
	OperationID    string `json:"operationID"`
	Fence          uint64 `json:"fence"`
	PreManifestID  string `json:"preManifestID,omitempty"`
	PostManifestID string `json:"postManifestID"`
}

type schema001DirectoryManifest struct {
	SchemaVersion int       `json:"schemaVersion"`
	DirectoryID   string    `json:"directoryID"`
	ManifestID    string    `json:"manifestID"`
	PageIDs       []string  `json:"pageIDs"`
	EntryCount    int       `json:"entryCount"`
	CreatedAt     time.Time `json:"createdAt"`
}

type schema002DirectoryRoot struct {
	SchemaVersion  int    `json:"schemaVersion"`
	DirectoryID    string `json:"directoryID"`
	ManifestID     string `json:"manifestID"`
	RecursiveBytes int64  `json:"recursiveBytes"`
}

type schema002DirectoryManifest struct {
	SchemaVersion  int       `json:"schemaVersion"`
	DirectoryID    string    `json:"directoryID"`
	ManifestID     string    `json:"manifestID"`
	PageIDs        []string  `json:"pageIDs"`
	EntryCount     int       `json:"entryCount"`
	RecursiveBytes int64     `json:"recursiveBytes"`
	CreatedAt      time.Time `json:"createdAt"`
}

func TestStartupMigratesSchema001ThroughCurrent(t *testing.T) {
	clock := domain.NewFixedClock(time.Date(2042, 6, 7, 8, 9, 10, 0, time.UTC))
	legacyObjects, user := schema001AggregateFixture(t, clock)
	backend := objectmemory.New()
	server := newPortableDataServer(t, backend, clock, 180)
	if err := backend.Import(legacyObjects); err != nil {
		t.Fatal(err)
	}

	engine, err := portable.Open(context.Background(), portable.Options{
		Backend: backend, Clock: clock, IDs: domain.NewIDGenerator(bytes.NewReader(deterministic(181, 1<<20))),
		Writer: portable.WriterConfiguration{
			WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1",
			KeyringIdentifiers: []string{"session-v1"},
		},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x63}, 32),
	})
	if err != nil {
		t.Fatalf("Open(legacy bucket) error = %v", err)
	}
	live, _ := domain.NewScope(user, domain.AreaLive)
	trash, _ := domain.NewScope(user, domain.AreaTrash)
	if got := assertVisibleRecursiveAggregates(t, engine.Files(), live, domain.MustParseUserPath("/")); got != 12 {
		t.Fatalf("migrated live aggregate = %d; want 12", got)
	}
	if got := assertVisibleRecursiveAggregates(t, engine.Files(), trash, domain.MustParseUserPath("/")); got != 5 {
		t.Fatalf("migrated trash aggregate = %d; want 5", got)
	}
	lookup, err := engine.Files().LookupChildren(context.Background(), trash, domain.ChildLookupRequest{Directory: domain.MustParseUserPath("/"), Names: []string{"old"}})
	if err != nil || len(lookup.Entries) != 1 || lookup.Entries[0].Kind != domain.EntryDirectory || lookup.Entries[0].Size != 5 || lookup.Entries[0].FileCount != 1 || lookup.Current.FileCount != 1 {
		t.Fatalf("migrated legacy trash lookup = %+v, %v; want directory size/count 5/1", lookup, err)
	}
	gate, err := engine.GateStatus(context.Background())
	if err != nil || gate.Mode != storageformat.GateOpen || gate.Epoch != 4 {
		t.Fatalf("migrated gate = %+v, %v; want open epoch 4 after three schema migrations", gate, err)
	}
	assertRecursiveFeatureActivated(t, backend.Export())
	assertLegacyWriterCannotDecodeMigratedGate(t, backend.Export())
	duplicateRecords := 0
	for key := range backend.Export() {
		if strings.HasPrefix(key, storageformat.DuplicateRecordsPrefix()) {
			duplicateRecords++
		}
	}
	if duplicateRecords == 0 {
		t.Fatal("schema migration did not construct the duplicate catalog")
	}
	uploadPortableFile(t, server.Client(), engine.Files(), live, domain.MustParseUserPath("/photos/third.txt"), []byte("new"))
	if got := assertVisibleRecursiveAggregates(t, engine.Files(), live, domain.MustParseUserPath("/")); got != 15 {
		t.Fatalf("post-migration live aggregate = %d; want 15", got)
	}
}

func TestStartupMigratesSchema001SplitBackendThroughCurrent(t *testing.T) {
	clock := domain.NewFixedClock(time.Date(2042, 6, 8, 8, 9, 10, 0, time.UTC))
	stateBackend := objectmemory.New()
	fileBackend := objectmemory.New()
	server := newPortableDataServer(t, fileBackend, clock, 183)
	engine, err := portable.Open(context.Background(), schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 184, nil))
	if err != nil {
		t.Fatal(err)
	}
	user, _ := domain.ParseUserID("WlpYWlpYWlpYWlpYWlpYWg")
	live, _ := domain.NewScope(user, domain.AreaLive)
	for _, path := range []string{"/split", "/split/nested"} {
		if _, err := engine.Files().CreateDirectory(context.Background(), live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(path)}); err != nil {
			t.Fatal(err)
		}
	}
	uploadPortableFile(t, server.Client(), engine.Files(), live, domain.MustParseUserPath("/split/first.txt"), []byte("four"))
	uploadPortableFile(t, server.Client(), engine.Files(), live, domain.MustParseUserPath("/split/nested/second.txt"), []byte("sixsix"))
	legacyState := encodeSchema001Fixture(t, stateBackend.Export())
	legacyFiles := fileBackend.Export()
	migratedState := objectmemory.New()
	migratedFiles := objectmemory.New()
	if err := migratedState.Import(legacyState); err != nil {
		t.Fatal(err)
	}
	if err := migratedFiles.Import(legacyFiles); err != nil {
		t.Fatal(err)
	}
	migrated, err := portable.Open(context.Background(), schemaSplitMigrationOptions(migratedState, migratedFiles, clock, 185, nil))
	if err != nil {
		t.Fatalf("Open(legacy split bucket) error = %v", err)
	}
	if got := assertVisibleRecursiveAggregates(t, migrated.Files(), live, domain.MustParseUserPath("/")); got != 10 {
		t.Fatalf("migrated split aggregate = %d; want 10", got)
	}
	if len(migratedFiles.Export()) != len(legacyFiles) {
		t.Fatalf("file backend object count changed during metadata migration: %d to %d", len(legacyFiles), len(migratedFiles.Export()))
	}
	assertRecursiveFeatureActivated(t, migratedState.Export())
}

func TestEightReplicasConcurrentlyMigrateSchema002(t *testing.T) {
	clock := domain.NewFixedClock(time.Date(2042, 6, 9, 8, 9, 10, 0, time.UTC))
	current := objectmemory.New()
	server := newPortableDataServer(t, current, clock, 186)
	engine := openEngine(t, current, clock, 187, nil)
	user, _ := domain.ParseUserID("W1tbW1tbW1tbW1tbW1tbWw")
	live, _ := domain.NewScope(user, domain.AreaLive)
	trash, _ := domain.NewScope(user, domain.AreaTrash)
	for _, path := range []string{"/existing", "/existing/empty", "/existing/nested"} {
		if _, err := engine.Files().CreateDirectory(context.Background(), live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(path)}); err != nil {
			t.Fatal(err)
		}
	}
	uploadPortableFile(t, server.Client(), engine.Files(), live, domain.MustParseUserPath("/existing/zero.bin"), nil)
	uploadPortableFile(t, server.Client(), engine.Files(), live, domain.MustParseUserPath("/existing/nested/data.bin"), []byte("data"))
	if _, err := engine.Files().CreateDirectory(context.Background(), trash, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/old")}); err != nil {
		t.Fatal(err)
	}
	uploadPortableFile(t, server.Client(), engine.Files(), trash, domain.MustParseUserPath("/old/deleted.bin"), []byte("xx"))
	predecessor := objectmemory.New()
	if err := predecessor.Import(encodeSchema002Fixture(t, current.Export())); err != nil {
		t.Fatal(err)
	}

	const replicas = 8
	barrier := newAggregateBarrier(replicas)
	engines := make([]*portable.Engine, replicas)
	errorsFound := make([]error, replicas)
	var wait sync.WaitGroup
	for index := range replicas {
		wait.Add(1)
		go func() {
			defer wait.Done()
			scheduler := &aggregateOneShotScheduler{step: portable.MigrationStepName("schema-002-to-003", portable.StepMigrationAfterDetection), barrier: barrier, enabled: true}
			engines[index], errorsFound[index] = portable.Open(context.Background(), schemaMigrationOptions(predecessor, clock, byte(188+index), scheduler))
		}()
	}
	wait.Wait()
	for index, err := range errorsFound {
		if err != nil {
			t.Errorf("replica %d Open(byte-only predecessor) error = %v", index, err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	root, err := engines[7].Files().Stat(context.Background(), live, domain.MustParseUserPath("/"))
	if err != nil || root.Size != 4 || root.FileCount != 2 {
		t.Fatalf("migrated byte-only root = %+v, %v; want 4 bytes/2 files", root, err)
	}
	empty, err := engines[0].Files().Stat(context.Background(), live, domain.MustParseUserPath("/existing/empty"))
	if err != nil || empty.Size != 0 || empty.FileCount != 0 {
		t.Fatalf("migrated empty directory = %+v, %v", empty, err)
	}
	trashRoot, err := engines[3].Files().Stat(context.Background(), trash, domain.MustParseUserPath("/"))
	if err != nil || trashRoot.Size != 2 || trashRoot.FileCount != 1 {
		t.Fatalf("migrated byte-only trash root = %+v, %v; want 2 bytes/1 file", trashRoot, err)
	}
	gate, err := engines[1].GateStatus(context.Background())
	if err != nil || gate.Mode != storageformat.GateOpen || gate.Epoch != 3 {
		t.Fatalf("migrated byte-only gate = %+v, %v", gate, err)
	}
	assertRecursiveFeatureActivated(t, predecessor.Export())
}

func TestEightReplicasConcurrentlyMigrateSchema001AggregateTree(t *testing.T) {
	clock := domain.NewFixedClock(time.Date(2042, 8, 9, 10, 11, 12, 0, time.UTC))
	legacyObjects, user := schema001AggregateFixture(t, clock)
	backend := objectmemory.New()
	if err := backend.Import(legacyObjects); err != nil {
		t.Fatal(err)
	}
	const replicas = 8
	barrier := newAggregateBarrier(replicas)
	engines := make([]*portable.Engine, replicas)
	errorsFound := make([]error, replicas)
	var wait sync.WaitGroup
	for index := range replicas {
		wait.Add(1)
		go func() {
			defer wait.Done()
			scheduler := &aggregateOneShotScheduler{step: portable.MigrationStepName("schema-001-to-002", portable.StepMigrationAfterDetection), barrier: barrier, enabled: true}
			engines[index], errorsFound[index] = portable.Open(context.Background(), schemaMigrationOptions(backend, clock, byte(231+index), scheduler))
		}()
	}
	wait.Wait()
	for index, err := range errorsFound {
		if err != nil {
			t.Errorf("replica %d Open() error = %v", index, err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	live, _ := domain.NewScope(user, domain.AreaLive)
	if got := assertVisibleRecursiveAggregates(t, engines[7].Files(), live, domain.MustParseUserPath("/")); got != 12 {
		t.Fatalf("concurrently migrated aggregate = %d; want 12", got)
	}
	gate, err := engines[0].GateStatus(context.Background())
	if err != nil || gate.Mode != storageformat.GateOpen || gate.Epoch != 4 {
		t.Fatalf("concurrently migrated gate = %+v, %v", gate, err)
	}
	assertRecursiveFeatureActivated(t, backend.Export())
}

func TestSchema001MigrationWaitsForAndDrainsActiveUpload(t *testing.T) {
	clock := domain.NewFixedClock(time.Date(2042, 9, 10, 11, 12, 13, 0, time.UTC))
	legacyObjects, user := schema001AggregateFixtureWithActiveUpload(t, clock)
	backend := objectmemory.New()
	if err := backend.Import(legacyObjects); err != nil {
		t.Fatal(err)
	}
	if _, err := portable.Open(context.Background(), schemaMigrationOptions(backend, clock, 242, nil)); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("Open(with active upload) error = %v", err)
	}
	assertRecursiveFeatureInactive(t, backend.Export())
	clock.Advance(11 * time.Minute)
	engine, err := portable.Open(context.Background(), schemaMigrationOptions(backend, clock, 243, nil))
	if err != nil {
		t.Fatalf("Open(after upload expiry) error = %v", err)
	}
	live, _ := domain.NewScope(user, domain.AreaLive)
	if got := assertVisibleRecursiveAggregates(t, engine.Files(), live, domain.MustParseUserPath("/")); got != 12 {
		t.Fatalf("post-drain aggregate = %d; want 12", got)
	}
	assertRecursiveFeatureActivated(t, backend.Export())
}

func TestSchema001MigrationRejectsCorruptTrees(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, map[string][]byte)
	}{
		{name: "missing-child-root", mutate: removeSchema001ChildRoot},
		{name: "recursive-overflow", mutate: overflowSchema001Directory},
		{name: "directory-cycle", mutate: cycleSchema001Directory},
	} {
		t.Run(test.name, func(t *testing.T) {
			clock := domain.NewFixedClock(time.Date(2042, 10, 11, 12, 13, 14, 0, time.UTC))
			legacyObjects, _ := schema001AggregateFixture(t, clock)
			test.mutate(t, legacyObjects)
			backend := objectmemory.New()
			if err := backend.Import(legacyObjects); err != nil {
				t.Fatal(err)
			}
			if _, err := portable.Open(context.Background(), schemaMigrationOptions(backend, clock, 251, nil)); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("Open(corrupt legacy tree) error = %v", err)
			}
			assertRecursiveFeatureInactive(t, backend.Export())
			gate := decodeStoredGate(t, backend.Export())
			if gate.Mode == storageformat.GateOpen {
				t.Fatalf("corrupt migration reopened gate: %+v", gate)
			}
		})
	}
}

func schema001AggregateFixture(t *testing.T, clock *domain.FixedClock) (map[string][]byte, domain.UserID) {
	return schema001AggregateFixtureState(t, clock, false)
}

func schema001AggregateFixtureWithActiveUpload(t *testing.T, clock *domain.FixedClock) (map[string][]byte, domain.UserID) {
	return schema001AggregateFixtureState(t, clock, true)
}

func schema001AggregateFixtureState(t *testing.T, clock *domain.FixedClock, activeUpload bool) (map[string][]byte, domain.UserID) {
	t.Helper()
	backend := objectmemory.New()
	server := newPortableDataServer(t, backend, clock, 171)
	engine := openEngine(t, backend, clock, 172, nil)
	user, _ := domain.ParseUserID("WVhXWVhXWVhXWVhXWVhXWQ")
	live, _ := domain.NewScope(user, domain.AreaLive)
	trash, _ := domain.NewScope(user, domain.AreaTrash)
	for _, path := range []string{"/photos", "/photos/nested"} {
		if _, err := engine.Files().CreateDirectory(context.Background(), live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(path)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := engine.Files().CreateDirectory(context.Background(), trash, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/old")}); err != nil {
		t.Fatal(err)
	}
	uploadPortableFile(t, server.Client(), engine.Files(), live, domain.MustParseUserPath("/photos/first.txt"), []byte("first"))
	uploadPortableFile(t, server.Client(), engine.Files(), live, domain.MustParseUserPath("/photos/nested/second.txt"), []byte("second!"))
	uploadPortableFile(t, server.Client(), engine.Files(), trash, domain.MustParseUserPath("/old/deleted.txt"), []byte("trash"))
	if activeUpload {
		if _, err := engine.Files().CreateUpload(context.Background(), live, domain.CreateUploadRequest{
			Path: domain.MustParseUserPath("/photos/incomplete.txt"), Size: 99, MediaType: "text/plain", Resumable: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return encodeSchema001Fixture(t, backend.Export()), user
}

func encodeSchema001Fixture(t *testing.T, objects map[string][]byte) map[string][]byte {
	t.Helper()
	downgradeDirectoryIndexes(t, objects)
	for key, body := range objects {
		if strings.HasPrefix(key, storageformat.DuplicateRecordsPrefix()) {
			delete(objects, key)
			continue
		}
		parsed := storageformatKey(t, key)
		switch {
		case key == storageformat.SuperblockKey().String():
			var superblock storageformat.Superblock
			if err := state.DecodeJSONWithLimit(body, &superblock, storageformat.MaxCanonicalBytes); err != nil {
				t.Fatal(err)
			}
			superblock.RequiredFeatures = withoutRecursiveFeature(superblock.RequiredFeatures)
			objects[key] = mustCanonical(t, superblock)
		case key == storageformat.WriterSetKey().String():
			var envelope storageformat.Envelope
			var writer storageformat.WriterSet
			if err := storageformat.DecodeEnvelope(body, parsed, "writer-set-v1", &envelope, &writer); err != nil {
				t.Fatal(err)
			}
			writer.RequiredFeatures = withoutRecursiveFeature(writer.RequiredFeatures)
			objects[key] = mustEnvelope(t, "writer-set-v1", parsed, envelope.Revision, writer)
		case key == storageformat.WriteGateKey().String():
			var envelope storageformat.Envelope
			var gate storageformat.WriteGate
			if err := storageformat.DecodeEnvelope(body, parsed, "write-gate-v1", &envelope, &gate); err != nil {
				t.Fatal(err)
			}
			gate.WriterFeatures = nil
			objects[key] = mustEnvelope(t, "write-gate-v1", parsed, envelope.Revision, gate)
		case strings.HasSuffix(key, "/directory.json") && strings.Contains(key, "/dirs/"):
			var envelope storageformat.Envelope
			var root storageformat.DirectoryRoot
			if err := storageformat.DecodeEnvelope(body, parsed, "directory-root-v1", &envelope, &root); err != nil {
				t.Fatal(err)
			}
			if root.Pending != nil {
				t.Fatal("fixture directory root is unexpectedly pending")
			}
			legacy := schema001DirectoryRoot{SchemaVersion: root.SchemaVersion, DirectoryID: root.DirectoryID, ManifestID: root.ManifestID}
			objects[key] = mustEnvelope(t, "directory-root-v1", parsed, envelope.Revision, legacy)
		case strings.Contains(key, "/manifests/") && strings.HasSuffix(key, ".json"):
			var envelope storageformat.Envelope
			var manifest storageformat.DirectoryManifest
			if err := storageformat.DecodeEnvelope(body, parsed, "directory-manifest-v1", &envelope, &manifest); err != nil {
				t.Fatal(err)
			}
			legacy := schema001DirectoryManifest{
				SchemaVersion: manifest.SchemaVersion, DirectoryID: manifest.DirectoryID, ManifestID: manifest.ManifestID,
				PageIDs: append([]string(nil), manifest.PageIDs...), EntryCount: manifest.EntryCount, CreatedAt: manifest.CreatedAt,
			}
			objects[key] = mustEnvelope(t, "directory-manifest-v1", parsed, envelope.Revision, legacy)
		case strings.Contains(key, "/pages/") && strings.HasSuffix(key, ".json"):
			var envelope storageformat.Envelope
			var page storageformat.DirectoryPage
			if err := storageformat.DecodeEnvelope(body, parsed, "directory-page-v1", &envelope, &page); err != nil {
				t.Fatal(err)
			}
			for index := range page.Entries {
				if page.Entries[index].Kind != domain.EntryDirectory {
					continue
				}
				page.Entries[index].Size = 0
				page.Entries[index].FileCount = 0
				page.Entries[index].LogicalVersion = entryLogicalVersion(t, page.Entries[index])
			}
			objects[key] = mustEnvelope(t, "directory-page-v1", parsed, envelope.Revision, page)
		}
	}
	return objects
}

func encodeSchema002Fixture(t *testing.T, objects map[string][]byte) map[string][]byte {
	t.Helper()
	downgradeDirectoryIndexes(t, objects)
	withoutCount := func(features []string) []string {
		result := make([]string, 0, len(features))
		for _, feature := range features {
			if feature != storageformat.FeatureRecursiveFileCounts && feature != storageformat.FeatureProviderFingerprints && feature != storageformat.FeatureDuplicateCatalog && feature != storageformat.FeatureDirectoryDigests && feature != storageformat.FeatureMetadataCheckpoints && feature != storageformat.FeaturePagedOperations && feature != storageformat.FeatureDirectoryIndexes && feature != storageformat.FeatureStateIndexes {
				result = append(result, feature)
			}
		}
		return result
	}
	for key, body := range objects {
		if strings.HasPrefix(key, storageformat.DuplicateRecordsPrefix()) {
			delete(objects, key)
			continue
		}
		parsed := storageformatKey(t, key)
		switch {
		case key == storageformat.SuperblockKey().String():
			var superblock storageformat.Superblock
			if err := state.DecodeJSONWithLimit(body, &superblock, storageformat.MaxCanonicalBytes); err != nil {
				t.Fatal(err)
			}
			superblock.RequiredFeatures = withoutCount(superblock.RequiredFeatures)
			objects[key] = mustCanonical(t, superblock)
		case key == storageformat.WriterSetKey().String():
			var envelope storageformat.Envelope
			var writer storageformat.WriterSet
			if err := storageformat.DecodeEnvelope(body, parsed, "writer-set-v1", &envelope, &writer); err != nil {
				t.Fatal(err)
			}
			writer.RequiredFeatures = withoutCount(writer.RequiredFeatures)
			objects[key] = mustEnvelope(t, "writer-set-v1", parsed, envelope.Revision, writer)
		case key == storageformat.WriteGateKey().String():
			var envelope storageformat.Envelope
			var gate storageformat.WriteGate
			if err := storageformat.DecodeEnvelope(body, parsed, "write-gate-v1", &envelope, &gate); err != nil {
				t.Fatal(err)
			}
			gate.WriterFeatures = withoutCount(gate.WriterFeatures)
			objects[key] = mustEnvelope(t, "write-gate-v1", parsed, envelope.Revision, gate)
		case strings.HasSuffix(key, "/directory.json") && strings.Contains(key, "/dirs/"):
			var envelope storageformat.Envelope
			var root storageformat.DirectoryRoot
			if err := storageformat.DecodeEnvelope(body, parsed, "directory-root-v1", &envelope, &root); err != nil {
				t.Fatal(err)
			}
			if root.Pending != nil {
				t.Fatal("fixture directory root is unexpectedly pending")
			}
			legacy := schema002DirectoryRoot{SchemaVersion: root.SchemaVersion, DirectoryID: root.DirectoryID, ManifestID: root.ManifestID, RecursiveBytes: root.RecursiveBytes}
			objects[key] = mustEnvelope(t, "directory-root-v1", parsed, envelope.Revision, legacy)
		case strings.Contains(key, "/manifests/") && strings.HasSuffix(key, ".json"):
			var envelope storageformat.Envelope
			var manifest storageformat.DirectoryManifest
			if err := storageformat.DecodeEnvelope(body, parsed, "directory-manifest-v1", &envelope, &manifest); err != nil {
				t.Fatal(err)
			}
			legacy := schema002DirectoryManifest{
				SchemaVersion: manifest.SchemaVersion, DirectoryID: manifest.DirectoryID, ManifestID: manifest.ManifestID,
				PageIDs: append([]string(nil), manifest.PageIDs...), EntryCount: manifest.EntryCount,
				RecursiveBytes: manifest.RecursiveBytes, CreatedAt: manifest.CreatedAt,
			}
			objects[key] = mustEnvelope(t, "directory-manifest-v1", parsed, envelope.Revision, legacy)
		case strings.Contains(key, "/pages/") && strings.HasSuffix(key, ".json"):
			var envelope storageformat.Envelope
			var page storageformat.DirectoryPage
			if err := storageformat.DecodeEnvelope(body, parsed, "directory-page-v1", &envelope, &page); err != nil {
				t.Fatal(err)
			}
			for index := range page.Entries {
				if page.Entries[index].Kind == domain.EntryDirectory {
					page.Entries[index].FileCount = 0
					page.Entries[index].LogicalVersion = entryLogicalVersion(t, page.Entries[index])
				}
			}
			objects[key] = mustEnvelope(t, "directory-page-v1", parsed, envelope.Revision, page)
		}
	}
	return objects
}

func downgradeDirectoryIndexes(t *testing.T, objects map[string][]byte) {
	t.Helper()
	nodes := make(map[string]storageformat.DirectoryIndexNode)
	for key, body := range objects {
		if !strings.Contains(key, "/index/") || !strings.HasSuffix(key, ".json") {
			continue
		}
		parsed := storageformatKey(t, key)
		var envelope storageformat.Envelope
		var node storageformat.DirectoryIndexNode
		if err := storageformat.DecodeEnvelope(body, parsed, "directory-index-node-v1", &envelope, &node); err != nil {
			t.Fatal(err)
		}
		nodes[node.DirectoryID+"\x00"+node.NodeID] = node
	}
	var collect func(string, string) []storageformat.DirectoryEntry
	collect = func(directoryID, nodeID string) []storageformat.DirectoryEntry {
		node, ok := nodes[directoryID+"\x00"+nodeID]
		if !ok {
			t.Fatalf("missing directory index node %q while constructing predecessor fixture", nodeID)
		}
		if node.Leaf {
			return append([]storageformat.DirectoryEntry(nil), node.Entries...)
		}
		var entries []storageformat.DirectoryEntry
		for _, child := range node.Children {
			entries = append(entries, collect(directoryID, child.NodeID)...)
		}
		return entries
	}
	for key, body := range objects {
		if !strings.Contains(key, "/manifests/") || !strings.HasSuffix(key, ".json") {
			continue
		}
		parsed := storageformatKey(t, key)
		var envelope storageformat.Envelope
		var manifest storageformat.DirectoryManifest
		if err := storageformat.DecodeEnvelope(body, parsed, "directory-manifest-v1", &envelope, &manifest); err != nil {
			t.Fatal(err)
		}
		if manifest.SchemaVersion != 2 {
			continue
		}
		var entries []storageformat.DirectoryEntry
		if manifest.EntryCount > 0 {
			entries = collect(manifest.DirectoryID, manifest.IndexRootID)
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].NameDigest == entries[j].NameDigest {
				return entries[i].Name < entries[j].Name
			}
			return entries[i].NameDigest < entries[j].NameDigest
		})
		if len(entries) > 200 {
			t.Fatal("predecessor test fixture exceeds the legacy directory page bound")
		}
		manifestPrefix := strings.Split(key, "/manifests/")[0]
		pageID := manifest.ManifestID
		pageKey := objectstore.MustKey(manifestPrefix + "/pages/" + strings.Split(key, "/manifests/")[1])
		page := storageformat.DirectoryPage{SchemaVersion: 1, DirectoryID: manifest.DirectoryID, PageID: pageID, Entries: entries}
		objects[pageKey.String()] = mustEnvelope(t, "directory-page-v1", pageKey, 1, page)
		manifest.SchemaVersion = 1
		manifest.PageIDs = []string{pageID}
		manifest.IndexRootID = ""
		manifest.IndexRootDigest = ""
		objects[key] = mustEnvelope(t, "directory-manifest-v1", parsed, envelope.Revision, manifest)
	}
	for key := range objects {
		if strings.Contains(key, "/index/") && strings.HasSuffix(key, ".json") {
			delete(objects, key)
		}
	}
}

func assertRecursiveFeatureActivated(t *testing.T, objects map[string][]byte) {
	t.Helper()
	var superblock storageformat.Superblock
	if err := state.DecodeJSONWithLimit(objects[storageformat.SuperblockKey().String()], &superblock, storageformat.MaxCanonicalBytes); err != nil {
		t.Fatal(err)
	}
	if !containsFeature(superblock.RequiredFeatures, storageformat.FeatureRecursiveBytes) {
		t.Fatalf("superblock features = %v", superblock.RequiredFeatures)
	}
	if !containsFeature(superblock.RequiredFeatures, storageformat.FeatureRecursiveFileCounts) {
		t.Fatalf("superblock features lack recursive file counts = %v", superblock.RequiredFeatures)
	}
	if !containsFeature(superblock.RequiredFeatures, storageformat.FeatureProviderFingerprints) || !containsFeature(superblock.RequiredFeatures, storageformat.FeatureDuplicateCatalog) {
		t.Fatalf("superblock features lack provider duplicate catalog support = %v", superblock.RequiredFeatures)
	}
	key := storageformat.WriterSetKey()
	var envelope storageformat.Envelope
	var writer storageformat.WriterSet
	if err := storageformat.DecodeEnvelope(objects[key.String()], key, "writer-set-v1", &envelope, &writer); err != nil {
		t.Fatal(err)
	}
	if !containsFeature(writer.RequiredFeatures, storageformat.FeatureRecursiveBytes) {
		t.Fatalf("writer features = %v", writer.RequiredFeatures)
	}
	if !containsFeature(writer.RequiredFeatures, storageformat.FeatureRecursiveFileCounts) {
		t.Fatalf("writer features lack recursive file counts = %v", writer.RequiredFeatures)
	}
	if !containsFeature(writer.RequiredFeatures, storageformat.FeatureProviderFingerprints) || !containsFeature(writer.RequiredFeatures, storageformat.FeatureDuplicateCatalog) {
		t.Fatalf("writer features lack provider duplicate catalog support = %v", writer.RequiredFeatures)
	}
}

func assertRecursiveFeatureInactive(t *testing.T, objects map[string][]byte) {
	t.Helper()
	var superblock storageformat.Superblock
	if err := state.DecodeJSONWithLimit(objects[storageformat.SuperblockKey().String()], &superblock, storageformat.MaxCanonicalBytes); err != nil {
		t.Fatal(err)
	}
	if containsFeature(superblock.RequiredFeatures, storageformat.FeatureRecursiveBytes) {
		t.Fatalf("recursive-byte feature activated after failed migration: %v", superblock.RequiredFeatures)
	}
	if containsFeature(superblock.RequiredFeatures, storageformat.FeatureRecursiveFileCounts) {
		t.Fatalf("recursive-file-count feature activated after failed migration: %v", superblock.RequiredFeatures)
	}
}

func assertLegacyWriterCannotDecodeMigratedGate(t *testing.T, objects map[string][]byte) {
	t.Helper()
	type legacyGate struct {
		SchemaVersion int                    `json:"schemaVersion"`
		Epoch         uint64                 `json:"epoch"`
		Mode          storageformat.GateMode `json:"mode"`
		CheckpointID  string                 `json:"checkpointID,omitempty"`
	}
	key := storageformat.WriteGateKey()
	var envelope storageformat.Envelope
	var gate legacyGate
	if err := storageformat.DecodeEnvelope(objects[key.String()], key, "write-gate-v1", &envelope, &gate); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("legacy writer decoded feature-bound gate: %+v, %v", gate, err)
	}
}

func schemaMigrationOptions(backend *objectmemory.Backend, clock *domain.FixedClock, seed byte, scheduler portable.Scheduler) portable.Options {
	return portable.Options{
		Backend: backend, Clock: clock, IDs: domain.NewIDGenerator(bytes.NewReader(deterministic(seed, 1<<20))),
		Writer: portable.WriterConfiguration{
			WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1",
			KeyringIdentifiers: []string{"session-v1"},
		},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x63}, 32), Scheduler: scheduler,
	}
}

func schemaSplitMigrationOptions(stateBackend, fileBackend *objectmemory.Backend, clock *domain.FixedClock, seed byte, scheduler portable.Scheduler) portable.Options {
	options := schemaMigrationOptions(stateBackend, clock, seed, scheduler)
	options.FileBackend = fileBackend
	return options
}

func removeSchema001ChildRoot(t *testing.T, objects map[string][]byte) {
	t.Helper()
	for key := range objects {
		_, _, directoryID, matched, err := storageformat.ParseDirectoryRootKey(storageformatKey(t, key))
		if err != nil {
			t.Fatal(err)
		}
		if matched && directoryID != storageformat.RootDirectoryID {
			delete(objects, key)
			return
		}
	}
	t.Fatal("legacy fixture has no child root")
}

func overflowSchema001Directory(t *testing.T, objects map[string][]byte) {
	t.Helper()
	mutateSchemaFixturePage(t, objects, func(page *storageformat.DirectoryPage) bool {
		for index := range page.Entries {
			if page.Entries[index].Kind == domain.EntryFile {
				page.Entries[index].Size = math.MaxInt64
				page.Entries[index].LogicalVersion = entryLogicalVersion(t, page.Entries[index])
				return true
			}
		}
		return false
	})
}

func cycleSchema001Directory(t *testing.T, objects map[string][]byte) {
	t.Helper()
	mutateSchemaFixturePage(t, objects, func(page *storageformat.DirectoryPage) bool {
		if page.DirectoryID == storageformat.RootDirectoryID {
			return false
		}
		for index := range page.Entries {
			if page.Entries[index].Kind == domain.EntryDirectory {
				page.Entries[index].DirectoryID = page.DirectoryID
				page.Entries[index].LogicalVersion = entryLogicalVersion(t, page.Entries[index])
				return true
			}
		}
		return false
	})
}

func mutateSchemaFixturePage(t *testing.T, objects map[string][]byte, mutate func(*storageformat.DirectoryPage) bool) {
	t.Helper()
	mutated := false
	for key, body := range objects {
		if !strings.Contains(key, "/pages/") || !strings.HasSuffix(key, ".json") {
			continue
		}
		parsed := storageformatKey(t, key)
		var envelope storageformat.Envelope
		var page storageformat.DirectoryPage
		if err := storageformat.DecodeEnvelope(body, parsed, "directory-page-v1", &envelope, &page); err != nil {
			t.Fatal(err)
		}
		if mutate(&page) {
			objects[key] = mustEnvelope(t, "directory-page-v1", parsed, envelope.Revision, page)
			mutated = true
		}
	}
	if !mutated {
		t.Fatal("legacy fixture has no matching page")
	}
}

func decodeStoredGate(t *testing.T, objects map[string][]byte) storageformat.WriteGate {
	t.Helper()
	key := storageformat.WriteGateKey()
	var envelope storageformat.Envelope
	var gate storageformat.WriteGate
	if err := storageformat.DecodeEnvelope(objects[key.String()], key, "write-gate-v1", &envelope, &gate); err != nil {
		t.Fatal(err)
	}
	return gate
}

func withoutRecursiveFeature(features []string) []string {
	result := make([]string, 0, len(features))
	for _, feature := range features {
		if feature != storageformat.FeatureRecursiveBytes && feature != storageformat.FeatureRecursiveFileCounts && feature != storageformat.FeatureProviderFingerprints && feature != storageformat.FeatureDuplicateCatalog && feature != storageformat.FeatureDirectoryDigests && feature != storageformat.FeatureMetadataCheckpoints && feature != storageformat.FeaturePagedOperations && feature != storageformat.FeatureDirectoryIndexes && feature != storageformat.FeatureStateIndexes {
			result = append(result, feature)
		}
	}
	return result
}

func containsFeature(features []string, wanted string) bool {
	for _, feature := range features {
		if feature == wanted {
			return true
		}
	}
	return false
}

func newPortableDataServer(t *testing.T, backend *objectmemory.Backend, clock *domain.FixedClock, seed byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(seed, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	return server
}

func storageformatKey(t *testing.T, value string) objectstore.Key {
	t.Helper()
	key, err := objectstore.ParseKey(value)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func mustCanonical(t *testing.T, value any) []byte {
	t.Helper()
	body, err := storageformat.EncodeCanonical(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func mustEnvelope(t *testing.T, schema string, key objectstore.Key, revision uint64, value any) []byte {
	t.Helper()
	body, err := storageformat.EncodeEnvelope(schema, key, revision, value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func entryLogicalVersion(t *testing.T, entry storageformat.DirectoryEntry) string {
	t.Helper()
	entry.LogicalVersion = ""
	return storageformat.Digest(mustCanonical(t, entry))
}
