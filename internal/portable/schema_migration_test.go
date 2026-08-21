package portable_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const migrationCandidateReleaseEnvironment = "ENDLESSFS_MIGRATION_CANDIDATE_RELEASE"

type storageSchemaFixture struct {
	SchemaVersion int               `json:"schemaVersion"`
	SourceRelease string            `json:"sourceRelease"`
	SourceCommit  string            `json:"sourceCommit"`
	CreatedAt     time.Time         `json:"createdAt"`
	UserID        string            `json:"userID"`
	StateObjects  map[string][]byte `json:"stateObjects"`
	FileObjects   map[string][]byte `json:"fileObjects"`
}

type storageSchemaFixtureEntry struct {
	schemaID  string
	file      string
	digest    string
	producer  string
	commit    string
	wantEpoch uint64
	wantSize  int64
	wantFiles int64
}

var storageSchemaFixtures = []storageSchemaFixtureEntry{
	{
		schemaID: "endlessfs-portable-v1/schema-001",
		file:     "pre-aggregate-v0.1.4.json", digest: "24111f7739207b53fad5c4e1cf0ca106982b40fce33850f045d7430150260258",
		producer: "v0.1.4", commit: "edb67f8e345694001b9614604c5baded9bde5d86",
		wantEpoch: 3, wantSize: 26, wantFiles: 2,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-002",
		file:     "schema-002-recursive-bytes.json", digest: "c7fc6a6924e62f99e9fdd99a35343385c17088d36bcac5f47b6abfe8776ee854",
		producer: "schema-002", commit: "b70f6361497d45f20049279bb5381a4fbb1005f1",
		wantEpoch: 2, wantSize: 10, wantFiles: 2,
	},
	{
		schemaID: "endlessfs-portable-v1/schema-003",
		file:     "recursive-aggregates-v0.1.7.json", digest: "0e2ce0a0853cba6e29730346b69e3c829240f617b1f277949f394b9a54786a51",
		producer: "v0.1.7", commit: "1548dafa30ea3fbf0340b3b32381e885a110ef5e",
		wantEpoch: 1, wantSize: 26, wantFiles: 2,
	},
}

var historicalReleases = []string{"v0.1.0", "v0.1.1", "v0.1.2", "v0.1.3", "v0.1.4", "v0.1.5", "v0.1.6", "v0.1.7"}

func TestMigrationEveryRegisteredStorageSchemaOpensAndMutatesWithCurrentCode(t *testing.T) {
	history := portable.StorageSchemaHistory()
	if len(storageSchemaFixtures) != len(history) {
		t.Fatalf("storage-schema fixture count = %d; ledger entries = %d", len(storageSchemaFixtures), len(history))
	}
	fixtureSchemas := make(map[string]struct{}, len(storageSchemaFixtures))
	for familyIndex, family := range storageSchemaFixtures {
		if _, duplicate := fixtureSchemas[family.schemaID]; duplicate {
			t.Fatalf("storage schema %s has more than one canonical fixture", family.schemaID)
		}
		fixtureSchemas[family.schemaID] = struct{}{}
		if family.schemaID != history[familyIndex].ID {
			t.Fatalf("fixture[%d] schema = %s; ledger schema = %s", familyIndex, family.schemaID, history[familyIndex].ID)
		}
		t.Run(family.schemaID, func(t *testing.T) {
			for topologyIndex, split := range []bool{false, true} {
				topology := "single"
				if split {
					topology = "split"
				}
				t.Run(topology, func(t *testing.T) {
					fixture := loadStorageSchemaFixture(t, family)
					stateBackend := objectmemory.New()
					fileBackend := stateBackend
					if split {
						fileBackend = objectmemory.New()
					}
					if err := stateBackend.Import(fixture.StateObjects); err != nil {
						t.Fatal(err)
					}
					if err := fileBackend.Import(fixture.FileObjects); err != nil {
						t.Fatal(err)
					}
					clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
					seed := byte(40 + familyIndex*4 + topologyIndex)
					server := newPortableDataServer(t, fileBackend, clock, seed)
					options := schemaMigrationOptions(stateBackend, clock, seed+20, nil)
					if split {
						options = schemaSplitMigrationOptions(stateBackend, fileBackend, clock, seed+20, nil)
					}
					engine, err := portable.Open(context.Background(), options)
					if err != nil {
						t.Fatalf("Open(%s %s-backend schema fixture produced by %s) error = %v", family.schemaID, topology, family.producer, err)
					}
					user, err := domain.ParseUserID(fixture.UserID)
					if err != nil {
						t.Fatal(err)
					}
					live, _ := domain.NewScope(user, domain.AreaLive)
					root, err := engine.Files().Stat(context.Background(), live, domain.MustParseUserPath("/"))
					if err != nil || root.Size != family.wantSize || root.FileCount != family.wantFiles {
						t.Fatalf("upgraded %s %s-backend root = %+v, %v; want %d bytes/%d files", family.schemaID, topology, root, err, family.wantSize, family.wantFiles)
					}
					gate, err := engine.GateStatus(context.Background())
					if err != nil || gate.Mode != storageformat.GateOpen || gate.Epoch != family.wantEpoch {
						t.Fatalf("upgraded %s %s-backend gate = %+v, %v; want open epoch %d", family.schemaID, topology, gate, err, family.wantEpoch)
					}
					assertAllUploadRecordsUseCurrentSchema(t, stateBackend.Export())
					uploadPortableFile(t, server.Client(), engine.Files(), live, domain.MustParseUserPath("/projects/after-upgrade.txt"), []byte("ok"))
					after, err := engine.Files().Stat(context.Background(), live, domain.MustParseUserPath("/"))
					if err != nil || after.Size != family.wantSize+2 || after.FileCount != family.wantFiles+1 {
						t.Fatalf("post-upgrade %s %s-backend mutation = %+v, %v", family.schemaID, topology, after, err)
					}
				})
			}
		})
	}
	for _, entry := range history {
		if _, found := fixtureSchemas[entry.ID]; !found {
			t.Fatalf("ledger schema %s has no immutable migration fixture", entry.ID)
		}
	}
}

func TestEveryHistoricalReleaseMapsToRegisteredStorageSchemaFixture(t *testing.T) {
	fixtureSchemas := make(map[string]struct{}, len(storageSchemaFixtures))
	for _, family := range storageSchemaFixtures {
		fixtureSchemas[family.schemaID] = struct{}{}
	}
	for _, release := range historicalReleases {
		schemaID, found := portable.StorageSchemaForRelease(release)
		if !found {
			t.Fatalf("historical release %s is outside every ledger validity range", release)
		}
		if _, registered := fixtureSchemas[schemaID]; !registered {
			t.Fatalf("historical release %s maps to schema %s without an immutable fixture", release, schemaID)
		}
	}
	if candidate := os.Getenv(migrationCandidateReleaseEnvironment); candidate != "" {
		schemaID, found := portable.StorageSchemaForRelease(candidate)
		if !found {
			t.Fatalf("release candidate %s is outside every declared storage-schema validity range", candidate)
		}
		if _, registered := fixtureSchemas[schemaID]; !registered {
			t.Fatalf("release candidate %s maps to schema %s without an immutable fixture", candidate, schemaID)
		}
	}
}

func TestMigrationEveryLedgerEdgeResumesAfterEveryDurableBoundary(t *testing.T) {
	boundaries := []string{
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
	history := portable.StorageSchemaHistory()
	for edgeIndex, entry := range history[:len(history)-1] {
		family := storageSchemaFixtures[edgeIndex]
		for boundaryIndex, boundary := range boundaries {
			t.Run(entry.MigrationID+"/"+boundary, func(t *testing.T) {
				fixture := loadStorageSchemaFixture(t, family)
				stateBackend := objectmemory.New()
				fileBackend := objectmemory.New()
				if err := stateBackend.Import(fixture.StateObjects); err != nil {
					t.Fatal(err)
				}
				if err := fileBackend.Import(fixture.FileObjects); err != nil {
					t.Fatal(err)
				}
				clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
				crasher := &stepFailure{step: portable.MigrationStepName(entry.MigrationID, boundary)}
				seed := byte(100 + edgeIndex*32 + boundaryIndex)
				if _, err := portable.Open(context.Background(), schemaSplitMigrationOptions(stateBackend, fileBackend, clock, seed, crasher)); !errors.Is(err, domain.ErrUnavailable) {
					t.Fatalf("interrupted %s at %s error = %v; want unavailable", entry.MigrationID, boundary, err)
				}
				engine, err := portable.Open(context.Background(), schemaSplitMigrationOptions(stateBackend, fileBackend, clock, seed+64, nil))
				if err != nil {
					t.Fatalf("resume %s after %s error = %v", entry.MigrationID, boundary, err)
				}
				user, err := domain.ParseUserID(fixture.UserID)
				if err != nil {
					t.Fatal(err)
				}
				live, _ := domain.NewScope(user, domain.AreaLive)
				root, err := engine.Files().Stat(context.Background(), live, domain.MustParseUserPath("/"))
				if err != nil || root.Size != family.wantSize || root.FileCount != family.wantFiles {
					t.Fatalf("resumed %s root = %+v, %v; want %d bytes/%d files", entry.MigrationID, root, err, family.wantSize, family.wantFiles)
				}
				gate, err := engine.GateStatus(context.Background())
				if err != nil || gate.Mode != storageformat.GateOpen || gate.Epoch != family.wantEpoch {
					t.Fatalf("resumed %s gate = %+v, %v; want open epoch %d", entry.MigrationID, gate, err, family.wantEpoch)
				}
			})
		}
	}
}

func TestMigrationOldestSchemaTraversesLedgerEdgesInOrder(t *testing.T) {
	history := portable.StorageSchemaHistory()
	family := storageSchemaFixtures[0]
	fixture := loadStorageSchemaFixture(t, family)
	stateBackend := objectmemory.New()
	fileBackend := objectmemory.New()
	if err := stateBackend.Import(fixture.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Import(fixture.FileObjects); err != nil {
		t.Fatal(err)
	}
	want := make([]string, 0, len(history)-1)
	for _, entry := range history[:len(history)-1] {
		want = append(want, portable.MigrationStepName(entry.MigrationID, portable.StepMigrationAfterDetection))
	}
	got := make([]string, 0, len(want))
	scheduler := portable.SchedulerFunc(func(_ context.Context, step string) error {
		for _, expected := range want {
			if step == expected {
				got = append(got, step)
				break
			}
		}
		return nil
	})
	clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
	if _, err := portable.Open(context.Background(), schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 180, scheduler)); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("observed migration-edge starts = %v; want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("observed migration-edge starts = %v; want %v", got, want)
		}
	}
}

func TestSchema001MigrationResumesAfterUploadRecordUpgrade(t *testing.T) {
	family := storageSchemaFixtures[0]
	fixture := loadStorageSchemaFixture(t, family)
	initialCurrent, initialSchema001 := countUploadRecordSchemas(t, fixture.StateObjects)
	if initialCurrent != 0 || initialSchema001 < 2 {
		t.Fatalf("schema-001 fixture upload schemas = %d current/%d schema-001; want 0/at least 2", initialCurrent, initialSchema001)
	}
	stateBackend := objectmemory.New()
	fileBackend := objectmemory.New()
	if err := stateBackend.Import(fixture.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Import(fixture.FileObjects); err != nil {
		t.Fatal(err)
	}
	clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
	crasher := &stepFailure{step: portable.MigrationStepName("schema-001-to-002", portable.StepMigrationAfterUploadRecord)}
	if _, err := portable.Open(context.Background(), schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 70, crasher)); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("interrupted schema-001 migration error = %v; want unavailable", err)
	}
	current, schema001 := countUploadRecordSchemas(t, stateBackend.Export())
	if current != 1 || schema001 != initialSchema001-1 {
		t.Fatalf("interrupted schema-001 migration upload schemas = %d current/%d schema-001; want 1/%d", current, schema001, initialSchema001-1)
	}
	engine, err := portable.Open(context.Background(), schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 71, nil))
	if err != nil {
		t.Fatalf("resumed schema-001 migration error = %v", err)
	}
	user, _ := domain.ParseUserID(fixture.UserID)
	live, _ := domain.NewScope(user, domain.AreaLive)
	root, err := engine.Files().Stat(context.Background(), live, domain.MustParseUserPath("/"))
	if err != nil || root.Size != family.wantSize || root.FileCount != family.wantFiles {
		t.Fatalf("resumed schema-001 migration root = %+v, %v", root, err)
	}
}

func TestEightReplicasConcurrentlyMigrateSchema001Fixture(t *testing.T) {
	family := storageSchemaFixtures[0]
	fixture := loadStorageSchemaFixture(t, family)
	stateBackend := objectmemory.New()
	fileBackend := objectmemory.New()
	if err := stateBackend.Import(fixture.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Import(fixture.FileObjects); err != nil {
		t.Fatal(err)
	}
	clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
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
			engines[index], errorsFound[index] = portable.Open(context.Background(), schemaSplitMigrationOptions(stateBackend, fileBackend, clock, byte(80+index), scheduler))
		}()
	}
	wait.Wait()
	for index, err := range errorsFound {
		if err != nil {
			t.Errorf("schema-001 migration replica %d error = %v", index, err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	assertAllUploadRecordsUseCurrentSchema(t, stateBackend.Export())
	user, _ := domain.ParseUserID(fixture.UserID)
	live, _ := domain.NewScope(user, domain.AreaLive)
	root, err := engines[replicas-1].Files().Stat(context.Background(), live, domain.MustParseUserPath("/"))
	if err != nil || root.Size != family.wantSize || root.FileCount != family.wantFiles {
		t.Fatalf("concurrently migrated release root = %+v, %v", root, err)
	}
}

func TestSchema001MigrationRejectsCorruptUploadRecord(t *testing.T) {
	family := storageSchemaFixtures[0]
	fixture := loadStorageSchemaFixture(t, family)
	corruptHistoricalUploadRecord(t, fixture.StateObjects)
	stateBackend := objectmemory.New()
	fileBackend := objectmemory.New()
	if err := stateBackend.Import(fixture.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Import(fixture.FileObjects); err != nil {
		t.Fatal(err)
	}
	clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
	if _, err := portable.Open(context.Background(), schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 72, nil)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt historical upload migration error = %v; want invalid", err)
	}
	assertRecursiveFeatureInactive(t, stateBackend.Export())
}

func TestSchema002MigrationRejectsInconsistentPersistedAggregate(t *testing.T) {
	family := storageSchemaFixtures[1]
	fixture := loadStorageSchemaFixture(t, family)
	mutateSchemaFixturePage(t, fixture.StateObjects, func(page *storageformat.DirectoryPage) bool {
		for index := range page.Entries {
			if page.Entries[index].Kind != domain.EntryFile {
				continue
			}
			page.Entries[index].Size++
			page.Entries[index].LogicalVersion = entryLogicalVersion(t, page.Entries[index])
			return true
		}
		return false
	})
	stateBackend := objectmemory.New()
	fileBackend := objectmemory.New()
	if err := stateBackend.Import(fixture.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Import(fixture.FileObjects); err != nil {
		t.Fatal(err)
	}
	clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
	if _, err := portable.Open(context.Background(), schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 73, nil)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("schema-002 inconsistent aggregate migration error = %v; want invalid", err)
	}
}

func loadStorageSchemaFixture(t *testing.T, family storageSchemaFixtureEntry) storageSchemaFixture {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "migrations", family.file))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	if got := hex.EncodeToString(digest[:]); got != family.digest {
		t.Fatalf("historical fixture %s digest = %s; want immutable digest %s", family.file, got, family.digest)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var fixture storageSchemaFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("historical fixture trailing JSON error = %v; want EOF", err)
	}
	if fixture.SchemaVersion != 1 || fixture.SourceRelease != family.producer || fixture.SourceCommit != family.commit || fixture.CreatedAt.IsZero() || fixture.UserID == "" || len(fixture.StateObjects) == 0 || len(fixture.FileObjects) == 0 {
		t.Fatalf("historical fixture metadata is invalid: %+v", fixture)
	}
	return fixture
}

type schema001FixtureUploadRecord struct {
	SchemaVersion   int                       `json:"schemaVersion"`
	UploadID        string                    `json:"uploadID"`
	UserID          string                    `json:"userID"`
	Area            string                    `json:"area"`
	RequestedPath   string                    `json:"requestedPath"`
	ResolvedPath    string                    `json:"resolvedPath"`
	StagingKey      string                    `json:"stagingKey"`
	BackendKind     string                    `json:"backendKind,omitempty"`
	LeaseKey        string                    `json:"leaseKey,omitempty"`
	Size            int64                     `json:"size"`
	MediaType       string                    `json:"mediaType"`
	Conflict        domain.ConflictMode       `json:"conflict"`
	ExpectedVersion domain.Version            `json:"expectedVersion,omitempty"`
	TargetExisted   bool                      `json:"targetExisted"`
	Resumable       bool                      `json:"resumable"`
	State           storageformat.UploadState `json:"state"`
	CreatedAt       time.Time                 `json:"createdAt"`
	ExpiresAt       time.Time                 `json:"expiresAt"`
}

func corruptHistoricalUploadRecord(t *testing.T, objects map[string][]byte) {
	t.Helper()
	for keyValue, body := range objects {
		key := storageformatKey(t, keyValue)
		var generic storageformat.Envelope
		if err := state.DecodeJSONWithLimit(body, &generic, storageformat.MaxCanonicalBytes); err != nil || generic.Schema != "upload-record-v1" {
			continue
		}
		var envelope storageformat.Envelope
		var record schema001FixtureUploadRecord
		if err := storageformat.DecodeEnvelope(body, key, "upload-record-v1", &envelope, &record); err != nil {
			t.Fatal(err)
		}
		record.BackendKind = "invalid_backend"
		objects[keyValue] = mustEnvelope(t, "upload-record-v1", key, envelope.Revision+1, record)
		return
	}
	t.Fatal("historical fixture has no upload record")
}

func assertAllUploadRecordsUseCurrentSchema(t *testing.T, objects map[string][]byte) {
	t.Helper()
	current, schema001 := countUploadRecordSchemas(t, objects)
	if current < 2 || schema001 != 0 {
		t.Fatalf("schema migration fixture exposed %d current/%d schema-001 upload records; want at least 2/0", current, schema001)
	}
}

func countUploadRecordSchemas(t *testing.T, objects map[string][]byte) (int, int) {
	t.Helper()
	currentCount := 0
	schema001Count := 0
	for keyValue, body := range objects {
		key := storageformatKey(t, keyValue)
		var generic storageformat.Envelope
		if err := state.DecodeJSONWithLimit(body, &generic, storageformat.MaxCanonicalBytes); err != nil || generic.Schema != "upload-record-v1" {
			continue
		}
		var envelope storageformat.Envelope
		var record storageformat.UploadRecord
		if err := storageformat.DecodeEnvelope(body, key, "upload-record-v1", &envelope, &record); err == nil {
			if record.CompletionOperationID == "" {
				t.Fatalf("current upload record %s lacks completion operation ID", keyValue)
			}
			currentCount++
			continue
		}
		var schema001Envelope storageformat.Envelope
		var schema001Record schema001FixtureUploadRecord
		if err := storageformat.DecodeEnvelope(body, key, "upload-record-v1", &schema001Envelope, &schema001Record); err != nil {
			t.Fatalf("upload record %s is neither the current nor registered historical schema: %v", keyValue, err)
		}
		schema001Count++
	}
	return currentCount, schema001Count
}
