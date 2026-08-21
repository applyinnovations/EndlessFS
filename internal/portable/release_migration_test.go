package portable_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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

type releasedStorageFixture struct {
	SchemaVersion int               `json:"schemaVersion"`
	SourceRelease string            `json:"sourceRelease"`
	SourceCommit  string            `json:"sourceCommit"`
	CreatedAt     time.Time         `json:"createdAt"`
	UserID        string            `json:"userID"`
	StateObjects  map[string][]byte `json:"stateObjects"`
	FileObjects   map[string][]byte `json:"fileObjects"`
}

type releasedFixtureFamily struct {
	file      string
	digest    string
	producer  string
	commit    string
	releases  []string
	wantEpoch uint64
	wantSize  int64
	wantFiles int64
}

var releasedFixtureFamilies = []releasedFixtureFamily{
	{
		file: "pre-aggregate-v0.1.4.json", digest: "24111f7739207b53fad5c4e1cf0ca106982b40fce33850f045d7430150260258",
		producer: "v0.1.4", commit: "edb67f8e345694001b9614604c5baded9bde5d86",
		releases: []string{"v0.1.0", "v0.1.1", "v0.1.2", "v0.1.3", "v0.1.4"}, wantEpoch: 2, wantSize: 26, wantFiles: 2,
	},
	{
		file: "recursive-aggregates-v0.1.7.json", digest: "0e2ce0a0853cba6e29730346b69e3c829240f617b1f277949f394b9a54786a51",
		producer: "v0.1.7", commit: "1548dafa30ea3fbf0340b3b32381e885a110ef5e",
		releases: []string{"v0.1.5", "v0.1.6", "v0.1.7"}, wantEpoch: 1, wantSize: 26, wantFiles: 2,
	},
}

var releasedVersions = []string{"v0.1.0", "v0.1.1", "v0.1.2", "v0.1.3", "v0.1.4", "v0.1.5", "v0.1.6", "v0.1.7"}

func TestEveryReleasedStorageFormatOpensAndMutatesWithCurrentCode(t *testing.T) {
	seen := make(map[string]struct{})
	for familyIndex, family := range releasedFixtureFamilies {
		for _, release := range family.releases {
			if _, duplicate := seen[release]; duplicate {
				t.Fatalf("release %s occurs in more than one migration fixture family", release)
			}
			seen[release] = struct{}{}
			t.Run(release, func(t *testing.T) {
				for topologyIndex, split := range []bool{false, true} {
					topology := "single"
					if split {
						topology = "split"
					}
					t.Run(topology, func(t *testing.T) {
						fixture := loadReleasedStorageFixture(t, family)
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
						options := legacyMigrationOptions(stateBackend, clock, seed+20, nil)
						if split {
							options = legacySplitMigrationOptions(stateBackend, fileBackend, clock, seed+20, nil)
						}
						engine, err := portable.Open(context.Background(), options)
						if err != nil {
							t.Fatalf("Open(%s %s-backend release fixture produced by %s) error = %v", release, topology, family.producer, err)
						}
						user, err := domain.ParseUserID(fixture.UserID)
						if err != nil {
							t.Fatal(err)
						}
						live, _ := domain.NewScope(user, domain.AreaLive)
						root, err := engine.Files().Stat(context.Background(), live, domain.MustParseUserPath("/"))
						if err != nil || root.Size != family.wantSize || root.FileCount != family.wantFiles {
							t.Fatalf("upgraded %s %s-backend root = %+v, %v; want %d bytes/%d files", release, topology, root, err, family.wantSize, family.wantFiles)
						}
						gate, err := engine.GateStatus(context.Background())
						if err != nil || gate.Mode != storageformat.GateOpen || gate.Epoch != family.wantEpoch {
							t.Fatalf("upgraded %s %s-backend gate = %+v, %v; want open epoch %d", release, topology, gate, err, family.wantEpoch)
						}
						assertAllUploadRecordsUseCurrentSchema(t, stateBackend.Export())
						uploadPortableFile(t, server.Client(), engine.Files(), live, domain.MustParseUserPath("/projects/after-upgrade.txt"), []byte("ok"))
						after, err := engine.Files().Stat(context.Background(), live, domain.MustParseUserPath("/"))
						if err != nil || after.Size != family.wantSize+2 || after.FileCount != family.wantFiles+1 {
							t.Fatalf("post-upgrade %s %s-backend mutation = %+v, %v", release, topology, after, err)
						}
					})
				}
			})
		}
	}
	got := make([]string, 0, len(seen))
	for release := range seen {
		got = append(got, release)
	}
	sort.Strings(got)
	if fmt.Sprint(got) != fmt.Sprint(releasedVersions) {
		t.Fatalf("migration release coverage = %v; want %v", got, releasedVersions)
	}
	if candidate := os.Getenv(migrationCandidateReleaseEnvironment); candidate != "" {
		if _, covered := seen[candidate]; !covered {
			t.Fatalf("release candidate %s has no registered historical migration fixture", candidate)
		}
	}
}

func TestReleasedStorageMigrationResumesAfterHistoricalUploadUpgrade(t *testing.T) {
	family := releasedFixtureFamilies[0]
	fixture := loadReleasedStorageFixture(t, family)
	initialCurrent, initialLegacy := countUploadRecordSchemas(t, fixture.StateObjects)
	if initialCurrent != 0 || initialLegacy < 2 {
		t.Fatalf("historical fixture upload schemas = %d current/%d legacy; want 0/at least 2", initialCurrent, initialLegacy)
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
	crasher := &stepFailure{step: portable.StepMigrationAfterUploadRecord}
	if _, err := portable.Open(context.Background(), legacySplitMigrationOptions(stateBackend, fileBackend, clock, 70, crasher)); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("interrupted release migration error = %v; want unavailable", err)
	}
	current, legacy := countUploadRecordSchemas(t, stateBackend.Export())
	if current != 1 || legacy != initialLegacy-1 {
		t.Fatalf("interrupted release migration upload schemas = %d current/%d legacy; want 1/%d", current, legacy, initialLegacy-1)
	}
	engine, err := portable.Open(context.Background(), legacySplitMigrationOptions(stateBackend, fileBackend, clock, 71, nil))
	if err != nil {
		t.Fatalf("resumed release migration error = %v", err)
	}
	user, _ := domain.ParseUserID(fixture.UserID)
	live, _ := domain.NewScope(user, domain.AreaLive)
	root, err := engine.Files().Stat(context.Background(), live, domain.MustParseUserPath("/"))
	if err != nil || root.Size != family.wantSize || root.FileCount != family.wantFiles {
		t.Fatalf("resumed release migration root = %+v, %v", root, err)
	}
}

func TestEightReplicasConcurrentlyMigrateReleasedStorage(t *testing.T) {
	family := releasedFixtureFamilies[0]
	fixture := loadReleasedStorageFixture(t, family)
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
			scheduler := &aggregateOneShotScheduler{step: portable.StepMigrationAfterDetection, barrier: barrier, enabled: true}
			engines[index], errorsFound[index] = portable.Open(context.Background(), legacySplitMigrationOptions(stateBackend, fileBackend, clock, byte(80+index), scheduler))
		}()
	}
	wait.Wait()
	for index, err := range errorsFound {
		if err != nil {
			t.Errorf("release migration replica %d error = %v", index, err)
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

func TestReleasedStorageMigrationRejectsCorruptHistoricalUpload(t *testing.T) {
	family := releasedFixtureFamilies[0]
	fixture := loadReleasedStorageFixture(t, family)
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
	if _, err := portable.Open(context.Background(), legacySplitMigrationOptions(stateBackend, fileBackend, clock, 72, nil)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt historical upload migration error = %v; want invalid", err)
	}
	assertRecursiveFeatureInactive(t, stateBackend.Export())
}

func loadReleasedStorageFixture(t *testing.T, family releasedFixtureFamily) releasedStorageFixture {
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
	var fixture releasedStorageFixture
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

type releasedLegacyUploadRecord struct {
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
		var record releasedLegacyUploadRecord
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
	current, legacy := countUploadRecordSchemas(t, objects)
	if current < 2 || legacy != 0 {
		t.Fatalf("release migration fixture exposed %d current/%d legacy upload records; want at least 2/0", current, legacy)
	}
}

func countUploadRecordSchemas(t *testing.T, objects map[string][]byte) (int, int) {
	t.Helper()
	currentCount := 0
	legacyCount := 0
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
		var legacyEnvelope storageformat.Envelope
		var legacyRecord releasedLegacyUploadRecord
		if err := storageformat.DecodeEnvelope(body, key, "upload-record-v1", &legacyEnvelope, &legacyRecord); err != nil {
			t.Fatalf("upload record %s is neither the current nor registered historical schema: %v", keyValue, err)
		}
		legacyCount++
	}
	return currentCount, legacyCount
}
