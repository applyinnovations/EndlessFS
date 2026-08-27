package portable_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestMigrationRecoversFromEveryObjectTransportInterruption(t *testing.T) {
	t.Parallel()
	predecessors := []storageSchemaFixtureEntry{storageSchemaFixtures[0]}
	for _, candidate := range storageSchemaFixtures {
		if candidate.schemaID == "endlessfs-portable-v1/schema-007" && candidate.profile == "application-preview-gcs" {
			predecessors = append(predecessors, candidate)
			break
		}
	}
	if len(predecessors) != 2 {
		t.Fatal("schema-007 feature-complete predecessor fixture is missing")
	}

	for _, family := range predecessors {
		family := family
		fixture := loadStorageSchemaFixture(t, family)
		wantGateEpoch := expectedCurrentGateEpoch(t, portable.StorageSchemaHistory(), family.schemaID, fixture)
		for _, target := range []string{"state", "file"} {
			target := target
			t.Run(family.schemaID+"/"+family.profile+"/"+target, func(t *testing.T) {
				t.Parallel()
				baselineState := objectmemory.New()
				baselineFile := objectmemory.New()
				if err := baselineState.Import(fixture.StateObjects); err != nil {
					t.Fatal(err)
				}
				if err := baselineFile.Import(fixture.FileObjects); err != nil {
					t.Fatal(err)
				}
				baselineClock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
				baselineFaults := &failNthBackend{}
				baselineOptions := schemaSplitMigrationOptions(baselineState, baselineFile, baselineClock, 19, nil)
				baselineOptions.Writer = currentWriterForSchemaFixture(t, fixture)
				if target == "state" {
					baselineFaults.backend = baselineState
					baselineOptions.Backend = baselineFaults
				} else {
					baselineFaults.backend = baselineFile
					baselineOptions.FileBackend = baselineFaults
				}
				baselineFaults.arm(0)
				if _, err := portable.Open(context.Background(), baselineOptions); err != nil {
					t.Fatal(err)
				}
				boundaryCalls := baselineFaults.calls
				consecutiveCompleted := 0
				for failAt := 1; failAt <= boundaryCalls+3 && consecutiveCompleted < 3; failAt++ {
					stateBackend := objectmemory.New()
					fileBackend := objectmemory.New()
					if err := stateBackend.Import(fixture.StateObjects); err != nil {
						t.Fatal(err)
					}
					if err := fileBackend.Import(fixture.FileObjects); err != nil {
						t.Fatal(err)
					}

					clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
					faults := &failNthBackend{}
					options := schemaSplitMigrationOptions(stateBackend, fileBackend, clock, byte(20+failAt%200), nil)
					options.Writer = currentWriterForSchemaFixture(t, fixture)
					switch target {
					case "state":
						faults.backend = stateBackend
						options.Backend = faults
					case "file":
						faults.backend = fileBackend
						options.FileBackend = faults
					default:
						t.Fatalf("unknown fault target %q", target)
					}
					faults.arm(failAt)

					_, migrationErr := portable.Open(context.Background(), options)
					calls := faults.calls
					faults.disable()
					if migrationErr == nil && failAt > calls {
						consecutiveCompleted++
					} else {
						consecutiveCompleted = 0
					}
					if migrationErr != nil && !errors.Is(migrationErr, domain.ErrUnavailable) {
						t.Fatalf("%s interruption %d at %s returned %v; want unavailable", target, failAt, faults.failureOperation, migrationErr)
					}

					resumeOptions := schemaSplitMigrationOptions(stateBackend, fileBackend, clock, byte(120+failAt%100), nil)
					resumeOptions.Writer = currentWriterForSchemaFixture(t, fixture)
					engine, err := portable.Open(context.Background(), resumeOptions)
					if err != nil {
						t.Fatalf("resume after %s interruption %d at %s: %v", target, failAt, faults.failureOperation, err)
					}
					gate, err := engine.GateStatus(context.Background())
					if err != nil || gate.Mode != storageformat.GateOpen || gate.Epoch != wantGateEpoch {
						t.Fatalf("resume after %s interruption %d gate = %+v, %v", target, failAt, gate, err)
					}
				}
				if consecutiveCompleted < 3 {
					t.Fatal(fmt.Errorf("%s migration fault matrix did not traverse every object operation", target))
				}
			})
		}
	}
}

type migrationContentionBackend struct {
	objectstore.Backend
	match     func(objectstore.Key, []byte, objectstore.PutCondition) bool
	remaining int
}

type disappearingMigrationObjectBackend struct {
	objectstore.Backend
	key  objectstore.Key
	once bool
}

type disappearingMigrationWinnerBackend struct {
	objectstore.Backend
	once bool
}

func (backend *disappearingMigrationWinnerBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	_, _, _, root, _ := storageformat.ParseDirectoryRootKey(key)
	if root && condition.Mode == objectstore.PutMatch && !backend.once {
		backend.once = true
		object, err := backend.Backend.Get(ctx, key)
		if err != nil {
			return "", err
		}
		if err := backend.Backend.Delete(ctx, key, objectstore.DeleteCondition{Version: object.Version}); err != nil {
			return "", err
		}
		return "", domain.NewError(domain.ErrorPreconditionFailed, "injected migration winner disappearance")
	}
	return backend.Backend.Put(ctx, key, body, condition)
}

type failingMigrationSuperblockRereadBackend struct {
	objectstore.Backend
	contended bool
	failed    bool
	corrupt   bool
}

func (backend *failingMigrationSuperblockRereadBackend) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	if key == storageformat.SuperblockKey() && backend.contended && !backend.failed {
		backend.failed = true
		if backend.corrupt {
			object, err := backend.Backend.Get(ctx, key)
			if err != nil {
				return objectstore.Object{}, err
			}
			object.Body = []byte("{}")
			return object, nil
		}
		return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "injected migration superblock reread failure")
	}
	return backend.Backend.Get(ctx, key)
}

func (backend *failingMigrationSuperblockRereadBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	if key == storageformat.SuperblockKey() && condition.Mode == objectstore.PutMatch && !backend.contended {
		backend.contended = true
		return "", domain.NewError(domain.ErrorPreconditionFailed, "injected migration superblock CAS loss")
	}
	return backend.Backend.Put(ctx, key, body, condition)
}

func (backend *disappearingMigrationObjectBackend) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	if key == backend.key && !backend.once {
		backend.once = true
		object, err := backend.Backend.Get(ctx, key)
		if err != nil {
			return objectstore.Object{}, err
		}
		if err := backend.Backend.Delete(ctx, key, objectstore.DeleteCondition{Version: object.Version}); err != nil {
			return objectstore.Object{}, err
		}
		return objectstore.Object{}, domain.NewError(domain.ErrorNotFound, "listed migration object disappeared")
	}
	return backend.Backend.Get(ctx, key)
}

func (backend *migrationContentionBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	if backend.remaining > 0 && backend.match(key, body, condition) {
		backend.remaining--
		return "", domain.NewError(domain.ErrorPreconditionFailed, "injected migration CAS loss")
	}
	return backend.Backend.Put(ctx, key, body, condition)
}

func TestMigrationFailsClosedWhenCASContentionCannotConverge(t *testing.T) {
	tests := []struct {
		name      string
		fixture   storageSchemaFixtureEntry
		remaining int
		match     func(objectstore.Key, []byte, objectstore.PutCondition) bool
		want      error
	}{
		{
			name: "gate-close", fixture: storageSchemaFixtures[1], remaining: 16, want: domain.ErrUnavailable,
			match: func(key objectstore.Key, _ []byte, _ objectstore.PutCondition) bool {
				return key == storageformat.WriteGateKey()
			},
		},
		{
			name: "schema-001-upload", fixture: storageSchemaFixtures[0], remaining: 16, want: domain.ErrUnavailable,
			match: func(key objectstore.Key, _ []byte, _ objectstore.PutCondition) bool {
				return strings.HasPrefix(key.String(), storageformat.OperationPrefix())
			},
		},
		{
			name: "directory-root-winner-mismatch", fixture: storageSchemaFixtures[1], remaining: 1, want: domain.ErrInvalid,
			match: func(key objectstore.Key, _ []byte, _ objectstore.PutCondition) bool {
				_, _, _, matched, _ := storageformat.ParseDirectoryRootKey(key)
				return matched
			},
		},
		{
			name: "writer-set-activation", fixture: storageSchemaFixtures[1], remaining: 8, want: domain.ErrUnavailable,
			match: func(key objectstore.Key, _ []byte, condition objectstore.PutCondition) bool {
				return key == storageformat.WriterSetKey() && condition.Mode == objectstore.PutMatch
			},
		},
		{
			name: "superblock-activation", fixture: storageSchemaFixtures[1], remaining: 8, want: domain.ErrUnavailable,
			match: func(key objectstore.Key, _ []byte, condition objectstore.PutCondition) bool {
				return key == storageformat.SuperblockKey() && condition.Mode == objectstore.PutMatch
			},
		},
		{
			name: "gate-binding", fixture: storageSchemaFixtures[1], remaining: 8, want: domain.ErrUnavailable,
			match: func(key objectstore.Key, body []byte, _ objectstore.PutCondition) bool {
				return key == storageformat.WriteGateKey() && bytes.Contains(body, []byte(storageformat.FeatureRecursiveFileCounts))
			},
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := loadStorageSchemaFixture(t, test.fixture)
			stateBackend := objectmemory.New()
			fileBackend := objectmemory.New()
			if err := stateBackend.Import(fixture.StateObjects); err != nil {
				t.Fatal(err)
			}
			if err := fileBackend.Import(fixture.FileObjects); err != nil {
				t.Fatal(err)
			}
			contended := &migrationContentionBackend{Backend: stateBackend, match: test.match, remaining: test.remaining}
			clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
			options := schemaSplitMigrationOptions(stateBackend, fileBackend, clock, byte(210+index), nil)
			options.Backend = contended
			options.Writer = currentWriterForSchemaFixture(t, fixture)
			if _, err := portable.Open(context.Background(), options); !errors.Is(err, test.want) {
				t.Fatalf("contended migration error = %v; want %v", err, test.want)
			}
			if contended.remaining != 0 {
				t.Fatalf("injected contention remaining = %d; want 0", contended.remaining)
			}
		})
	}
}

func TestMigrationCASLossFailsClosedWhenTheWinnerCannotBeVerified(t *testing.T) {
	family := storageSchemaFixtures[1]
	for index, test := range []struct {
		name string
		wrap func(objectstore.Backend) objectstore.Backend
		want error
	}{
		{
			name: "directory-winner-disappears",
			wrap: func(backend objectstore.Backend) objectstore.Backend {
				return &disappearingMigrationWinnerBackend{Backend: backend}
			},
			want: domain.ErrNotFound,
		},
		{
			name: "superblock-reread-fails",
			wrap: func(backend objectstore.Backend) objectstore.Backend {
				return &failingMigrationSuperblockRereadBackend{Backend: backend}
			},
			want: domain.ErrUnavailable,
		},
		{
			name: "superblock-reread-is-corrupt",
			wrap: func(backend objectstore.Backend) objectstore.Backend {
				return &failingMigrationSuperblockRereadBackend{Backend: backend, corrupt: true}
			},
			want: domain.ErrInvalid,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			options := schemaSplitMigrationOptions(stateBackend, fileBackend, clock, byte(240+index), nil)
			options.Backend = test.wrap(stateBackend)
			options.Writer = currentWriterForSchemaFixture(t, fixture)
			if _, err := portable.Open(context.Background(), options); !errors.Is(err, test.want) {
				t.Fatalf("migration CAS winner validation error = %v; want %v", err, test.want)
			}
		})
	}
}

func TestMigrationSchema001UploadScanHandlesDisappearanceAndRejectsMalformedRecords(t *testing.T) {
	family := storageSchemaFixtures[0]
	for _, test := range []struct {
		name      string
		mutate    func(map[string][]byte, string)
		disappear bool
		wantErr   error
	}{
		{name: "disappeared-after-list", disappear: true},
		{name: "malformed-json", wantErr: domain.ErrInvalid, mutate: func(objects map[string][]byte, key string) { objects[key] = []byte("{") }},
		{name: "invalid-historical-envelope", wantErr: domain.ErrInvalid, mutate: func(objects map[string][]byte, key string) {
			objects[key] = bytes.Replace(objects[key], []byte(`"size":`), []byte(`"size" :`), 1)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := loadStorageSchemaFixture(t, family)
			var uploadKey string
			for key := range fixture.StateObjects {
				if strings.HasPrefix(key, storageformat.OperationPrefix()) {
					uploadKey = key
					break
				}
			}
			if uploadKey == "" {
				t.Fatal("schema-001 fixture has no upload record")
			}
			if test.mutate != nil {
				test.mutate(fixture.StateObjects, uploadKey)
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
			options := schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 232, nil)
			options.Writer = currentWriterForSchemaFixture(t, fixture)
			if test.disappear {
				options.Backend = &disappearingMigrationObjectBackend{Backend: stateBackend, key: storageformatKey(t, uploadKey)}
			}
			_, err := portable.Open(context.Background(), options)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("schema-001 upload scan error = %v; want %v", err, test.wantErr)
			}
		})
	}
}
