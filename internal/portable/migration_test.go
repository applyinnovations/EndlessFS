package portable_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"net/http/httptest"
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

type legacyDirectoryRoot struct {
	SchemaVersion int                        `json:"schemaVersion"`
	DirectoryID   string                     `json:"directoryID"`
	ManifestID    string                     `json:"manifestID"`
	Pending       *legacyDirectoryTransition `json:"pending,omitempty"`
}

type legacyDirectoryTransition struct {
	OperationID    string `json:"operationID"`
	Fence          uint64 `json:"fence"`
	PreManifestID  string `json:"preManifestID,omitempty"`
	PostManifestID string `json:"postManifestID"`
}

type legacyDirectoryManifest struct {
	SchemaVersion int       `json:"schemaVersion"`
	DirectoryID   string    `json:"directoryID"`
	ManifestID    string    `json:"manifestID"`
	PageIDs       []string  `json:"pageIDs"`
	EntryCount    int       `json:"entryCount"`
	CreatedAt     time.Time `json:"createdAt"`
}

func TestStartupAutomaticallyMigratesLegacyRecursiveByteAggregates(t *testing.T) {
	clock := domain.NewFixedClock(time.Date(2042, 6, 7, 8, 9, 10, 0, time.UTC))
	legacyObjects, user := legacyAggregateFixture(t, clock)
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
	if err != nil || len(lookup.Entries) != 1 || lookup.Entries[0].Kind != domain.EntryDirectory || lookup.Entries[0].Size != 5 {
		t.Fatalf("migrated legacy trash lookup = %+v, %v; want directory size 5", lookup, err)
	}
	gate, err := engine.GateStatus(context.Background())
	if err != nil || gate.Mode != storageformat.GateOpen || gate.Epoch != 2 {
		t.Fatalf("migrated gate = %+v, %v; want open epoch 2", gate, err)
	}
	assertRecursiveFeatureActivated(t, backend.Export())
	assertLegacyWriterCannotDecodeMigratedGate(t, backend.Export())
	uploadPortableFile(t, server.Client(), engine.Files(), live, domain.MustParseUserPath("/photos/third.txt"), []byte("new"))
	if got := assertVisibleRecursiveAggregates(t, engine.Files(), live, domain.MustParseUserPath("/")); got != 15 {
		t.Fatalf("post-migration live aggregate = %d; want 15", got)
	}
}

func TestStartupAutomaticallyMigratesLegacySplitBackend(t *testing.T) {
	clock := domain.NewFixedClock(time.Date(2042, 6, 8, 8, 9, 10, 0, time.UTC))
	stateBackend := objectmemory.New()
	fileBackend := objectmemory.New()
	server := newPortableDataServer(t, fileBackend, clock, 183)
	engine, err := portable.Open(context.Background(), legacySplitMigrationOptions(stateBackend, fileBackend, clock, 184, nil))
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
	legacyState := downgradeRecursiveByteFeature(t, stateBackend.Export())
	legacyFiles := fileBackend.Export()
	migratedState := objectmemory.New()
	migratedFiles := objectmemory.New()
	if err := migratedState.Import(legacyState); err != nil {
		t.Fatal(err)
	}
	if err := migratedFiles.Import(legacyFiles); err != nil {
		t.Fatal(err)
	}
	migrated, err := portable.Open(context.Background(), legacySplitMigrationOptions(migratedState, migratedFiles, clock, 185, nil))
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

func TestStartupRecursiveByteMigrationResumesAfterEveryDurableBoundary(t *testing.T) {
	steps := []string{
		portable.StepMigrationAfterDetection,
		portable.StepMigrationAfterGateClosed,
		portable.StepMigrationAfterDirectoryPrerequisites,
		portable.StepMigrationAfterDirectoryRoot,
		portable.StepMigrationAfterDirectories,
		portable.StepMigrationAfterWriterSet,
		portable.StepMigrationAfterSuperblock,
		portable.StepMigrationAfterGateBinding,
		portable.StepMigrationAfterCheckpoint,
	}
	for index, step := range steps {
		t.Run(step, func(t *testing.T) {
			clock := domain.NewFixedClock(time.Date(2042, 7, 8, 9, 10, 11, 0, time.UTC))
			legacyObjects, user := legacyAggregateFixture(t, clock)
			backend := objectmemory.New()
			if err := backend.Import(legacyObjects); err != nil {
				t.Fatal(err)
			}
			crasher := &stepFailure{step: step}
			if _, err := portable.Open(context.Background(), legacyMigrationOptions(backend, clock, byte(190+index), crasher)); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("interrupted Open() error = %v", err)
			}
			engine, err := portable.Open(context.Background(), legacyMigrationOptions(backend, clock, byte(210+index), nil))
			if err != nil {
				t.Fatalf("resumed Open() error = %v", err)
			}
			live, _ := domain.NewScope(user, domain.AreaLive)
			trash, _ := domain.NewScope(user, domain.AreaTrash)
			if got := assertVisibleRecursiveAggregates(t, engine.Files(), live, domain.MustParseUserPath("/")); got != 12 {
				t.Fatalf("resumed live aggregate = %d; want 12", got)
			}
			if got := assertVisibleRecursiveAggregates(t, engine.Files(), trash, domain.MustParseUserPath("/")); got != 5 {
				t.Fatalf("resumed trash aggregate = %d; want 5", got)
			}
			assertRecursiveFeatureActivated(t, backend.Export())
		})
	}
}

func TestEightReplicasConcurrentlyMigrateOneLegacyAggregateTree(t *testing.T) {
	clock := domain.NewFixedClock(time.Date(2042, 8, 9, 10, 11, 12, 0, time.UTC))
	legacyObjects, user := legacyAggregateFixture(t, clock)
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
			scheduler := &aggregateOneShotScheduler{step: portable.StepMigrationAfterDetection, barrier: barrier, enabled: true}
			engines[index], errorsFound[index] = portable.Open(context.Background(), legacyMigrationOptions(backend, clock, byte(231+index), scheduler))
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
	if err != nil || gate.Mode != storageformat.GateOpen || gate.Epoch != 2 {
		t.Fatalf("concurrently migrated gate = %+v, %v", gate, err)
	}
	assertRecursiveFeatureActivated(t, backend.Export())
}

func TestStartupRecursiveByteMigrationWaitsForAndDrainsActiveUpload(t *testing.T) {
	clock := domain.NewFixedClock(time.Date(2042, 9, 10, 11, 12, 13, 0, time.UTC))
	legacyObjects, user := legacyAggregateFixtureWithActiveUpload(t, clock)
	backend := objectmemory.New()
	if err := backend.Import(legacyObjects); err != nil {
		t.Fatal(err)
	}
	if _, err := portable.Open(context.Background(), legacyMigrationOptions(backend, clock, 242, nil)); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("Open(with active upload) error = %v", err)
	}
	assertRecursiveFeatureInactive(t, backend.Export())
	clock.Advance(11 * time.Minute)
	engine, err := portable.Open(context.Background(), legacyMigrationOptions(backend, clock, 243, nil))
	if err != nil {
		t.Fatalf("Open(after upload expiry) error = %v", err)
	}
	live, _ := domain.NewScope(user, domain.AreaLive)
	if got := assertVisibleRecursiveAggregates(t, engine.Files(), live, domain.MustParseUserPath("/")); got != 12 {
		t.Fatalf("post-drain aggregate = %d; want 12", got)
	}
	assertRecursiveFeatureActivated(t, backend.Export())
}

func TestStartupRecursiveByteMigrationRejectsCorruptLegacyTrees(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, map[string][]byte)
	}{
		{name: "missing-child-root", mutate: removeLegacyChildRoot},
		{name: "recursive-overflow", mutate: overflowLegacyDirectory},
		{name: "directory-cycle", mutate: cycleLegacyDirectory},
	} {
		t.Run(test.name, func(t *testing.T) {
			clock := domain.NewFixedClock(time.Date(2042, 10, 11, 12, 13, 14, 0, time.UTC))
			legacyObjects, _ := legacyAggregateFixture(t, clock)
			test.mutate(t, legacyObjects)
			backend := objectmemory.New()
			if err := backend.Import(legacyObjects); err != nil {
				t.Fatal(err)
			}
			if _, err := portable.Open(context.Background(), legacyMigrationOptions(backend, clock, 251, nil)); !errors.Is(err, domain.ErrInvalid) {
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

func legacyAggregateFixture(t *testing.T, clock *domain.FixedClock) (map[string][]byte, domain.UserID) {
	return legacyAggregateFixtureState(t, clock, false)
}

func legacyAggregateFixtureWithActiveUpload(t *testing.T, clock *domain.FixedClock) (map[string][]byte, domain.UserID) {
	return legacyAggregateFixtureState(t, clock, true)
}

func legacyAggregateFixtureState(t *testing.T, clock *domain.FixedClock, activeUpload bool) (map[string][]byte, domain.UserID) {
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
	return downgradeRecursiveByteFeature(t, backend.Export()), user
}

func downgradeRecursiveByteFeature(t *testing.T, objects map[string][]byte) map[string][]byte {
	t.Helper()
	for key, body := range objects {
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
			legacy := legacyDirectoryRoot{SchemaVersion: root.SchemaVersion, DirectoryID: root.DirectoryID, ManifestID: root.ManifestID}
			objects[key] = mustEnvelope(t, "directory-root-v1", parsed, envelope.Revision, legacy)
		case strings.Contains(key, "/manifests/") && strings.HasSuffix(key, ".json"):
			var envelope storageformat.Envelope
			var manifest storageformat.DirectoryManifest
			if err := storageformat.DecodeEnvelope(body, parsed, "directory-manifest-v1", &envelope, &manifest); err != nil {
				t.Fatal(err)
			}
			legacy := legacyDirectoryManifest{
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
				page.Entries[index].LogicalVersion = entryLogicalVersion(t, page.Entries[index])
			}
			objects[key] = mustEnvelope(t, "directory-page-v1", parsed, envelope.Revision, page)
		}
	}
	return objects
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
	key := storageformat.WriterSetKey()
	var envelope storageformat.Envelope
	var writer storageformat.WriterSet
	if err := storageformat.DecodeEnvelope(objects[key.String()], key, "writer-set-v1", &envelope, &writer); err != nil {
		t.Fatal(err)
	}
	if !containsFeature(writer.RequiredFeatures, storageformat.FeatureRecursiveBytes) {
		t.Fatalf("writer features = %v", writer.RequiredFeatures)
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

func legacyMigrationOptions(backend *objectmemory.Backend, clock *domain.FixedClock, seed byte, scheduler portable.Scheduler) portable.Options {
	return portable.Options{
		Backend: backend, Clock: clock, IDs: domain.NewIDGenerator(bytes.NewReader(deterministic(seed, 1<<20))),
		Writer: portable.WriterConfiguration{
			WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1",
			KeyringIdentifiers: []string{"session-v1"},
		},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x63}, 32), Scheduler: scheduler,
	}
}

func legacySplitMigrationOptions(stateBackend, fileBackend *objectmemory.Backend, clock *domain.FixedClock, seed byte, scheduler portable.Scheduler) portable.Options {
	options := legacyMigrationOptions(stateBackend, clock, seed, scheduler)
	options.FileBackend = fileBackend
	return options
}

func removeLegacyChildRoot(t *testing.T, objects map[string][]byte) {
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

func overflowLegacyDirectory(t *testing.T, objects map[string][]byte) {
	t.Helper()
	mutateLegacyPage(t, objects, func(page *storageformat.DirectoryPage) bool {
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

func cycleLegacyDirectory(t *testing.T, objects map[string][]byte) {
	t.Helper()
	mutateLegacyPage(t, objects, func(page *storageformat.DirectoryPage) bool {
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

func mutateLegacyPage(t *testing.T, objects map[string][]byte, mutate func(*storageformat.DirectoryPage) bool) {
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
		if feature != storageformat.FeatureRecursiveBytes {
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
