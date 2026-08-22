package portable

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type migrationPreparationFaultBackend struct {
	objectstore.Backend
	failAt int
	calls  int
}

type migrationMarkPutFaultBackend struct {
	objectstore.Backend
	err error
}

func (backend *migrationMarkPutFaultBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	if condition.Mode == objectstore.PutMatch && strings.HasPrefix(key.String(), storageformat.MigrationDirectoryMarkPrefix(schemaMigration003To004.checkpointID)) {
		return "", backend.err
	}
	return backend.Backend.Put(ctx, key, body, condition)
}

func (backend *migrationPreparationFaultBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	backend.calls++
	if backend.calls == backend.failAt {
		return "", domain.NewError(domain.ErrorUnavailable, "injected migration preparation failure")
	}
	return backend.Backend.Put(ctx, key, body, condition)
}

func TestMigrationSchema001UploadValidationRejectsEveryInvalidFieldFamily(t *testing.T) {
	userID := "WVhXWVhXWVhXWVhXWVhXWQ"
	uploadID := "upload-1"
	valid := schema001UploadRecord{
		SchemaVersion: 1,
		UploadID:      uploadID,
		UserID:        userID,
		Area:          areaName(domain.AreaLive),
		RequestedPath: "/requested.txt",
		ResolvedPath:  "/resolved.txt",
		StagingKey:    storageformat.StagingKey(userID, uploadID, "upload").String(),
		BackendKind:   "memory",
		LeaseKey:      storageformat.LeaseKey("memory", uploadID).String(),
		Size:          1,
		MediaType:     "text/plain",
		Conflict:      domain.ConflictFail,
		State:         storageformat.UploadActive,
		CreatedAt:     time.Date(2042, 1, 2, 3, 4, 5, 0, time.UTC),
		ExpiresAt:     time.Date(2042, 1, 2, 3, 14, 5, 0, time.UTC),
	}
	key := storageformat.OperationKey(userID, uploadID)
	if err := validateSchema001UploadRecord(key, valid); err != nil {
		t.Fatalf("valid schema-001 upload rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*schema001UploadRecord)
	}{
		{name: "identity", mutate: func(record *schema001UploadRecord) { record.UserID = "invalid" }},
		{name: "area", mutate: func(record *schema001UploadRecord) { record.Area = "archive" }},
		{name: "path", mutate: func(record *schema001UploadRecord) { record.RequestedPath = "/" }},
		{name: "constraints", mutate: func(record *schema001UploadRecord) { record.MediaType = "TEXT/PLAIN" }},
		{name: "backend", mutate: func(record *schema001UploadRecord) { record.BackendKind = "INVALID" }},
		{name: "storage-keys", mutate: func(record *schema001UploadRecord) { record.StagingKey = "invalid" }},
		{name: "state", mutate: func(record *schema001UploadRecord) { record.State = "unknown" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := valid
			test.mutate(&record)
			if err := validateSchema001UploadRecord(key, record); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("invalid %s error = %v; want invalid", test.name, err)
			}
		})
	}
}

func TestMigrationGraphValidationRejectsEveryStructuralAmbiguity(t *testing.T) {
	root := storageformat.RootDirectoryID
	tests := []struct {
		name      string
		directory string
		parent    string
		configure func(*migrationWalk)
		wantErr   bool
	}{
		{name: "missing-root", directory: "missing", wantErr: true},
		{name: "root-as-child", directory: root, parent: "parent", configure: func(walk *migrationWalk) { walk.group.roots[root] = struct{}{} }, wantErr: true},
		{name: "multiple-parents", directory: "child", parent: "second", configure: func(walk *migrationWalk) {
			walk.group.roots["child"] = struct{}{}
			walk.parents["child"] = "first"
		}, wantErr: true},
		{name: "cycle", directory: "child", configure: func(walk *migrationWalk) {
			walk.group.roots["child"] = struct{}{}
			walk.state["child"] = 1
		}, wantErr: true},
		{name: "completed", directory: "child", configure: func(walk *migrationWalk) {
			walk.group.roots["child"] = struct{}{}
			walk.state["child"] = 2
			walk.totals["child"] = migrationAggregate{bytes: 4, files: 1}
		}},
		{name: "completed-without-total", directory: "child", configure: func(walk *migrationWalk) {
			walk.group.roots["child"] = struct{}{}
			walk.state["child"] = 2
			walk.totals = nil
		}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			walk := &migrationWalk{
				group: migrationScope{roots: make(map[string]struct{})},
				state: make(map[string]uint8), totals: make(map[string]migrationAggregate), parents: make(map[string]string),
			}
			if test.configure != nil {
				test.configure(walk)
			}
			got, err := walk.directory(t.Context(), test.directory, test.parent)
			if test.wantErr && !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("directory error = %v; want invalid", err)
			}
			if !test.wantErr && (err != nil || got != (migrationAggregate{bytes: 4, files: 1})) {
				t.Fatalf("completed directory = %+v, %v", got, err)
			}
		})
	}
}

func TestMigrationDirectoryMarksResumeWithoutRewalkingCompletedSubtree(t *testing.T) {
	backend, engine, scope, _, _ := emptyPhysicalMigrationRoot(t)
	ctx := context.Background()
	plan := aggregateMigrationPlan{writeProviderFingerprints: true}
	if err := engine.migrateAllDirectoryAggregatesPhase(ctx, schemaMigration003To004, plan, migrationPhaseTransform); err != nil {
		t.Fatal(err)
	}
	key := storageformat.MigrationDirectoryMarkKey(schemaMigration003To004.checkpointID, migrationPhaseTransform, scope.UserID().String(), areaName(scope.Area()), storageformat.RootDirectoryID)
	before, err := backend.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	indexReads := 0
	hooks := &hookedBackend{Backend: backend}
	hooks.get = func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
		if strings.Contains(key.String(), "/index-nodes/") || strings.Contains(key.String(), "/pages/") {
			indexReads++
		}
		return backend.Get(ctx, key)
	}
	engine.backend = hooks
	if err := engine.migrateAllDirectoryAggregatesPhase(ctx, schemaMigration003To004, plan, migrationPhaseTransform); err != nil {
		t.Fatal(err)
	}
	after, err := backend.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if indexReads != 0 || before.Version != after.Version {
		t.Fatalf("completed subtree resume read %d entry nodes and changed mark %q -> %q", indexReads, before.Version, after.Version)
	}
}

func TestMigrationDirectoryMarksFailClosedAndCleanUp(t *testing.T) {
	backend, engine, scope, _, _ := emptyPhysicalMigrationRoot(t)
	ctx := context.Background()
	plan := aggregateMigrationPlan{writeProviderFingerprints: true}
	if err := engine.migrateAllDirectoryAggregatesPhase(ctx, schemaMigration003To004, plan, migrationPhaseVerify); err != nil {
		t.Fatal(err)
	}
	key := storageformat.MigrationDirectoryMarkKey(schemaMigration003To004.checkpointID, migrationPhaseVerify, scope.UserID().String(), areaName(scope.Area()), storageformat.RootDirectoryID)
	object, err := backend.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, key, []byte("{}"), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
		t.Fatal(err)
	}
	if err := engine.migrateAllDirectoryAggregatesPhase(ctx, schemaMigration003To004, plan, migrationPhaseVerify); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt migration mark error = %v; want invalid", err)
	}
	if err := engine.cleanupMigrationDirectoryMarks(ctx, schemaMigration003To004.checkpointID); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Get(ctx, key); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("migration mark after cleanup error = %v; want not found", err)
	}
}

func TestMigrationManifestValidationRejectsMalformedCanonicalState(t *testing.T) {
	valid := storageformat.DirectoryManifest{
		SchemaVersion: 1,
		DirectoryID:   "directory",
		ManifestID:    "manifest",
		PageIDs:       []string{"page"},
		CreatedAt:     time.Date(2042, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	if err := validateMigrationManifest(valid, "directory", "manifest"); err != nil {
		t.Fatalf("valid migration manifest rejected: %v", err)
	}
	invalid := valid
	invalid.EntryCount = -1
	if err := validateMigrationManifest(invalid, "directory", "manifest"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid migration manifest error = %v; want invalid", err)
	}
}

func TestMigrationCanonicalDecoderFailsClosed(t *testing.T) {
	var value struct {
		Value int `json:"value"`
	}
	if err := decodeCanonicalRecord([]byte("{"), &value); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("malformed canonical record error = %v; want invalid", err)
	}
	if err := decodeCanonicalRecord([]byte("{\"value\":1 }"), &value); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("non-canonical record error = %v; want invalid", err)
	}
	var unsupported struct {
		Channel chan int `json:"channel"`
	}
	if err := decodeCanonicalRecord([]byte("{\"channel\":null}"), &unsupported); err == nil {
		t.Fatal("unsupported canonical destination unexpectedly encoded")
	}
	if err := decodeCanonicalSuperblock(nil, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("nil superblock destination error = %v; want invalid", err)
	}
}

func TestMigrationStringSignaturesAreOrderSensitive(t *testing.T) {
	if equalStrings([]string{"a", "b"}, []string{"a", "c"}) {
		t.Fatal("different migration feature signatures compare equal")
	}
	if equalStrings([]string{"a"}, []string{"a", "b"}) {
		t.Fatal("different-length migration feature signatures compare equal")
	}
}

func TestMigrationDirectoryRootDecoderAcceptsEveryEpochAndRejectsCorruption(t *testing.T) {
	user, _ := domain.ParseUserID("WVhXWVhXWVhXWVhXWVhXWQ")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	directoryID := "directory"
	key := storageformat.DirectoryRootKey(user.String(), areaName(scope.Area()), directoryID)

	tests := []struct {
		name      string
		body      func(*testing.T) []byte
		wantErr   error
		wantBytes bool
		current   bool
	}{
		{name: "missing", wantErr: domain.ErrNotFound},
		{name: "current", wantBytes: true, current: true, body: func(t *testing.T) []byte {
			return migrationEnvelope(t, directoryRootSchema, key, storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: "manifest", RecursiveBytes: 4, RecursiveFileCount: 1})
		}},
		{name: "current-invalid", wantErr: domain.ErrInvalid, body: func(t *testing.T) []byte {
			return migrationEnvelope(t, directoryRootSchema, key, storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: "manifest", RecursiveFileCount: -1})
		}},
		{name: "byte-only", wantBytes: true, body: func(t *testing.T) []byte {
			return migrationEnvelope(t, directoryRootSchema, key, schema002DirectoryRoot{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: "manifest", RecursiveBytes: 4})
		}},
		{name: "byte-only-invalid", wantErr: domain.ErrInvalid, body: func(t *testing.T) []byte {
			return migrationEnvelope(t, directoryRootSchema, key, schema002DirectoryRoot{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: "manifest", RecursiveBytes: -1})
		}},
		{name: "legacy", body: func(t *testing.T) []byte {
			return migrationEnvelope(t, directoryRootSchema, key, schema001DirectoryRoot{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: "manifest"})
		}},
		{name: "legacy-invalid", wantErr: domain.ErrInvalid, body: func(t *testing.T) []byte {
			return migrationEnvelope(t, directoryRootSchema, key, schema001DirectoryRoot{SchemaVersion: 1, DirectoryID: directoryID})
		}},
		{name: "malformed", wantErr: domain.ErrInvalid, body: func(*testing.T) []byte { return []byte("{}") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := objectmemory.New()
			if test.body != nil {
				migrationPut(t, backend, key, test.body(t))
			}
			engine := &Engine{backend: backend}
			root, err := engine.readMigrationDirectoryRoot(context.Background(), scope, directoryID)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("root decode error = %v; want %v", err, test.wantErr)
			}
			if err == nil && (root.hasRecursiveBytes != test.wantBytes || root.current != test.current) {
				t.Fatalf("root flags = bytes:%t current:%t", root.hasRecursiveBytes, root.current)
			}
		})
	}
}

func TestMigrationDirectoryManifestDecoderAcceptsEveryEpochAndRejectsCorruption(t *testing.T) {
	user, _ := domain.ParseUserID("WVhXWVhXWVhXWVhXWVhXWQ")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	directoryID, manifestID := "directory", "manifest"
	key := storageformat.DirectoryManifestKey(user.String(), areaName(scope.Area()), directoryID, manifestID)
	createdAt := time.Date(2042, 1, 2, 3, 4, 5, 0, time.UTC)

	tests := []struct {
		name      string
		body      func(*testing.T) []byte
		wantErr   error
		wantBytes bool
		current   bool
	}{
		{name: "missing", wantErr: domain.ErrNotFound},
		{name: "current", wantBytes: true, current: true, body: func(t *testing.T) []byte {
			return migrationEnvelope(t, directoryManifestSchema, key, storageformat.DirectoryManifest{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: manifestID, PageIDs: []string{"page"}, CreatedAt: createdAt})
		}},
		{name: "current-invalid", wantErr: domain.ErrInvalid, body: func(t *testing.T) []byte {
			return migrationEnvelope(t, directoryManifestSchema, key, storageformat.DirectoryManifest{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: manifestID, CreatedAt: createdAt})
		}},
		{name: "byte-only", wantBytes: true, body: func(t *testing.T) []byte {
			return migrationEnvelope(t, directoryManifestSchema, key, schema002DirectoryManifest{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: manifestID, PageIDs: []string{"page"}, CreatedAt: createdAt})
		}},
		{name: "byte-only-invalid", wantErr: domain.ErrInvalid, body: func(t *testing.T) []byte {
			return migrationEnvelope(t, directoryManifestSchema, key, schema002DirectoryManifest{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: manifestID, CreatedAt: createdAt})
		}},
		{name: "legacy", body: func(t *testing.T) []byte {
			return migrationEnvelope(t, directoryManifestSchema, key, schema001DirectoryManifest{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: manifestID, PageIDs: []string{"page"}, CreatedAt: createdAt})
		}},
		{name: "legacy-invalid", wantErr: domain.ErrInvalid, body: func(t *testing.T) []byte {
			return migrationEnvelope(t, directoryManifestSchema, key, schema001DirectoryManifest{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: manifestID, CreatedAt: createdAt})
		}},
		{name: "malformed", wantErr: domain.ErrInvalid, body: func(*testing.T) []byte { return []byte("{}") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := objectmemory.New()
			if test.body != nil {
				migrationPut(t, backend, key, test.body(t))
			}
			engine := &Engine{backend: backend}
			manifest, err := engine.readMigrationDirectoryManifest(context.Background(), scope, directoryID, manifestID)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("manifest decode error = %v; want %v", err, test.wantErr)
			}
			if err == nil && (manifest.hasRecursiveBytes != test.wantBytes || manifest.current != test.current) {
				t.Fatalf("manifest flags = bytes:%t current:%t", manifest.hasRecursiveBytes, manifest.current)
			}
		})
	}
}

func migrationEnvelope(t *testing.T, schema string, key objectstore.Key, value any) []byte {
	t.Helper()
	body, err := storageformat.EncodeEnvelope(schema, key, 1, value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func migrationPut(t *testing.T, backend objectstore.Backend, key objectstore.Key, body []byte) {
	t.Helper()
	if _, err := backend.Put(context.Background(), key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationActivationAndCompletionRejectInconsistentControlRecords(t *testing.T) {
	t.Run("completed-edge-is-idempotent", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		object, err := backend.Get(context.Background(), storageformat.SuperblockKey())
		if err != nil {
			t.Fatal(err)
		}
		var superblock storageformat.Superblock
		if err := decodeCanonicalSuperblock(object.Body, &superblock); err != nil {
			t.Fatal(err)
		}
		if err := engine.runAggregateSchemaMigration(context.Background(), schemaMigration002To003, object, superblock, aggregateMigrationPlan{writeFileCounts: true}); err != nil {
			t.Fatalf("idempotent completed migration: %v", err)
		}
		closed, err := engine.closeStorageMigrationGate(context.Background(), schemaMigration002To003, aggregateMigrationPlan{writeFileCounts: true})
		if err != nil || closed {
			t.Fatalf("completed migration gate close = %t, %v; want false, nil", closed, err)
		}
	})

	t.Run("writer-identity", func(t *testing.T) {
		_, engine := currentMigrationEngine(t)
		engine.writer.ConfigurationDigest = "different"
		if err := engine.verifyMigrationWriterSet(context.Background(), schemaMigration002To003); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("writer identity error = %v; want precondition failed", err)
		}
		if err := engine.activateMigrationWriterSet(context.Background(), schemaMigration002To003); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("writer activation identity error = %v; want precondition failed", err)
		}
	})

	t.Run("writer-schema", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		rewriteMigrationWriter(t, backend, nil)
		if err := engine.verifyMigrationWriterSet(context.Background(), schemaMigration002To003); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("writer schema error = %v; want precondition failed", err)
		}
		if err := engine.activateMigrationWriterSet(context.Background(), schemaMigration002To003); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("writer activation schema error = %v; want precondition failed", err)
		}
	})

	t.Run("malformed-writer", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		replaceMigrationBody(t, backend, storageformat.WriterSetKey(), []byte("{}"))
		if _, _, _, err := engine.readStoredWriterSet(context.Background()); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("malformed writer error = %v; want invalid", err)
		}
	})

	t.Run("superblock-format", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		object, err := backend.Get(context.Background(), storageformat.SuperblockKey())
		if err != nil {
			t.Fatal(err)
		}
		var superblock storageformat.Superblock
		if err := decodeCanonicalSuperblock(object.Body, &superblock); err != nil {
			t.Fatal(err)
		}
		superblock.FormatID = "unknown"
		if err := engine.activateMigrationSuperblock(context.Background(), schemaMigration002To003, object, superblock); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("superblock format error = %v; want precondition failed", err)
		}
	})

	t.Run("superblock-schema", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		object, err := backend.Get(context.Background(), storageformat.SuperblockKey())
		if err != nil {
			t.Fatal(err)
		}
		var superblock storageformat.Superblock
		if err := decodeCanonicalSuperblock(object.Body, &superblock); err != nil {
			t.Fatal(err)
		}
		superblock.RequiredFeatures = nil
		if err := engine.activateMigrationSuperblock(context.Background(), schemaMigration002To003, object, superblock); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("superblock schema error = %v; want precondition failed", err)
		}
	})

	t.Run("gate-mode", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		rewriteMigrationGate(t, backend, func(gate *storageformat.WriteGate) {
			gate.Mode = storageformat.GateClosing
			gate.CheckpointID = "other-maintenance"
		})
		if err := engine.bindMigrationGateToTarget(context.Background(), schemaMigration002To003); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("gate mode error = %v; want precondition failed", err)
		}
	})

	t.Run("gate-schema", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		rewriteMigrationGate(t, backend, func(gate *storageformat.WriteGate) {
			gate.Mode = storageformat.GateClosed
			gate.CheckpointID = schemaMigration002To003.checkpointID
			gate.WriterFeatures = []string{"unknown-feature"}
		})
		if err := engine.bindMigrationGateToTarget(context.Background(), schemaMigration002To003); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("gate schema error = %v; want precondition failed", err)
		}
	})

	t.Run("completion-corruption", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		replaceMigrationBody(t, backend, storageformat.SuperblockKey(), []byte("{}"))
		if _, err := engine.storageMigrationComplete(context.Background(), schemaMigration002To003); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("completion corruption error = %v; want invalid", err)
		}
	})

	t.Run("completion-writer-mismatch", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		rewriteMigrationWriter(t, backend, nil)
		complete, err := engine.storageMigrationComplete(context.Background(), schemaMigration002To003)
		if err != nil || complete {
			t.Fatalf("completion with old writer = %t, %v; want false, nil", complete, err)
		}
	})
}

func currentMigrationEngine(t *testing.T) (*objectmemory.Backend, *Engine) {
	t.Helper()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2042, 1, 2, 3, 4, 5, 0, time.UTC))
	return backend, openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("m", 1<<16)))
}

func replaceMigrationBody(t *testing.T, backend *objectmemory.Backend, key objectstore.Key, body []byte) {
	t.Helper()
	object, err := backend.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(context.Background(), key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
		t.Fatal(err)
	}
}

func rewriteMigrationWriter(t *testing.T, backend *objectmemory.Backend, features []string) {
	t.Helper()
	key := storageformat.WriterSetKey()
	object, err := backend.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var envelope storageformat.Envelope
	var writer storageformat.WriterSet
	if err := storageformat.DecodeEnvelope(object.Body, key, writerSetSchema, &envelope, &writer); err != nil {
		t.Fatal(err)
	}
	writer.RequiredFeatures = append([]string(nil), features...)
	body := migrationEnvelope(t, writerSetSchema, key, writer)
	replaceMigrationBody(t, backend, key, body)
}

func rewriteMigrationGate(t *testing.T, backend *objectmemory.Backend, mutate func(*storageformat.WriteGate)) {
	t.Helper()
	key := storageformat.WriteGateKey()
	object, err := backend.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var envelope storageformat.Envelope
	var gate storageformat.WriteGate
	if err := storageformat.DecodeEnvelope(object.Body, key, writeGateSchema, &envelope, &gate); err != nil {
		t.Fatal(err)
	}
	mutate(&gate)
	body := migrationEnvelope(t, writeGateSchema, key, gate)
	replaceMigrationBody(t, backend, key, body)
}

type closingGateContentionBackend struct {
	objectstore.Backend
	once bool
}

func (backend *closingGateContentionBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	if key == storageformat.WriteGateKey() && strings.Contains(string(body), `"mode":"closed"`) && !backend.once {
		backend.once = true
		return "", domain.NewError(domain.ErrorPreconditionFailed, "injected closing-gate contention")
	}
	return backend.Backend.Put(ctx, key, body, condition)
}

func TestMigrationSchemaChainFailsClosedForEveryControlPlaneDisagreement(t *testing.T) {
	t.Run("malformed-superblock", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		replaceMigrationBody(t, backend, storageformat.SuperblockKey(), []byte("{}"))
		if err := engine.migrateStorageSchemaChain(context.Background()); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("malformed superblock error = %v; want invalid", err)
		}
	})

	t.Run("incompatible-superblock", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		rewriteMigrationSuperblock(t, backend, func(superblock *storageformat.Superblock) { superblock.FormatID = "unknown" })
		if err := engine.migrateStorageSchemaChain(context.Background()); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("incompatible superblock error = %v; want precondition failed", err)
		}
	})

	t.Run("malformed-gate", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		replaceMigrationBody(t, backend, storageformat.WriteGateKey(), []byte("{}"))
		if err := engine.migrateStorageSchemaChain(context.Background()); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("malformed gate error = %v; want invalid", err)
		}
	})

	t.Run("unregistered-schema", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		rewriteMigrationSuperblock(t, backend, func(superblock *storageformat.Superblock) { superblock.RequiredFeatures = []string{"unknown-feature"} })
		if err := engine.migrateStorageSchemaChain(context.Background()); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("unregistered schema error = %v; want precondition failed", err)
		}
	})

	t.Run("pending-writer-corruption", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		replaceMigrationBody(t, backend, storageformat.WriterSetKey(), []byte("{}"))
		if err := engine.migrateStorageSchemaChain(context.Background()); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("pending writer corruption error = %v; want invalid", err)
		}
	})

	t.Run("pending-gate-corruption", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		if _, err := engine.storageMigrationPending(context.Background()); err != nil {
			t.Fatal(err)
		}
		replaceMigrationBody(t, backend, storageformat.WriteGateKey(), []byte("{}"))
		if _, err := engine.storageMigrationPending(context.Background()); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("pending gate corruption error = %v; want invalid", err)
		}
	})

	t.Run("broken-ledger-path", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		rewriteMigrationSuperblock(t, backend, func(superblock *storageformat.Superblock) { superblock.RequiredFeatures = nil })
		original := storageSchemaLedger[1].migrationFromPrevious
		storageSchemaLedger[1].migrationFromPrevious = nil
		t.Cleanup(func() { storageSchemaLedger[1].migrationFromPrevious = original })
		if err := engine.migrateStorageSchemaChain(context.Background()); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("broken ledger path error = %v; want precondition failed", err)
		}
	})

	t.Run("resumed-edge-error", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		rewriteMigrationGate(t, backend, func(gate *storageformat.WriteGate) {
			gate.Mode = storageformat.GateClosed
			gate.CheckpointID = schemaMigration002To003.checkpointID
		})
		engine.scheduler = SchedulerFunc(func(context.Context, string) error {
			return domain.NewError(domain.ErrorUnavailable, "injected resumed migration interruption")
		})
		if err := engine.migrateStorageSchemaChain(context.Background()); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("resumed edge error = %v; want unavailable", err)
		}
	})

	t.Run("non-convergent-ledger", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		rewriteMigrationSuperblock(t, backend, func(superblock *storageformat.Superblock) { superblock.RequiredFeatures = nil })
		original := schemaMigration001To002.run
		schemaMigration001To002.run = func(*Engine, context.Context, storageMigration, objectstore.Object, storageformat.Superblock) error {
			return nil
		}
		t.Cleanup(func() { schemaMigration001To002.run = original })
		if err := engine.migrateStorageSchemaChain(context.Background()); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("non-convergent ledger error = %v; want unavailable", err)
		}
	})
}

func rewriteMigrationSuperblock(t *testing.T, backend *objectmemory.Backend, mutate func(*storageformat.Superblock)) {
	t.Helper()
	key := storageformat.SuperblockKey()
	object, err := backend.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var superblock storageformat.Superblock
	if err := decodeCanonicalSuperblock(object.Body, &superblock); err != nil {
		t.Fatal(err)
	}
	mutate(&superblock)
	body, err := storageformat.EncodeCanonical(superblock)
	if err != nil {
		t.Fatal(err)
	}
	replaceMigrationBody(t, backend, key, body)
}

func TestMigrationGateClosureRejectsEveryUnsafeControlState(t *testing.T) {
	t.Run("completion-read-error", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		replaceMigrationBody(t, backend, storageformat.SuperblockKey(), []byte("{}"))
		if _, err := engine.closeStorageMigrationGate(context.Background(), schemaMigration002To003, aggregateMigrationPlan{}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("completion read error = %v; want invalid", err)
		}
	})

	t.Run("target-gate-before-control-activation", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		rewriteMigrationSuperblock(t, backend, func(superblock *storageformat.Superblock) { superblock.RequiredFeatures = nil })
		if _, err := engine.closeStorageMigrationGate(context.Background(), schemaMigration002To003, aggregateMigrationPlan{}); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("premature target gate error = %v; want precondition failed", err)
		}
	})

	t.Run("later-edge-owns-gate", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		rewriteMigrationGate(t, backend, func(gate *storageformat.WriteGate) {
			gate.Mode = storageformat.GateClosed
			gate.CheckpointID = schemaMigration002To003.checkpointID
		})
		closed, err := engine.closeStorageMigrationGate(context.Background(), schemaMigration001To002, aggregateMigrationPlan{})
		if err != nil || closed {
			t.Fatalf("later-owned gate close = %t, %v; want false, nil", closed, err)
		}
	})

	t.Run("closing-gate-contention-is-retried", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		features, _ := schemaFeatures(storageSchema002, engine.writer.RequiredFeatures)
		rewriteMigrationSuperblock(t, backend, func(superblock *storageformat.Superblock) {
			superblock.RequiredFeatures = append([]string(nil), features...)
		})
		rewriteMigrationWriter(t, backend, features)
		rewriteMigrationGate(t, backend, func(gate *storageformat.WriteGate) { gate.WriterFeatures = append([]string(nil), features...) })
		contended := &closingGateContentionBackend{Backend: backend}
		engine.backend = contended
		closed, err := engine.closeStorageMigrationGate(context.Background(), schemaMigration002To003, aggregateMigrationPlan{writeFileCounts: true})
		if err != nil || !closed || !contended.once {
			t.Fatalf("contended closing gate = %t, %v, injected=%t; want true, nil, true", closed, err, contended.once)
		}
	})

	t.Run("earlier-edge-defers-to-later-complete-binding", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		rewriteMigrationGate(t, backend, func(gate *storageformat.WriteGate) {
			gate.Mode = storageformat.GateClosed
			gate.CheckpointID = schemaMigration002To003.checkpointID
		})
		if err := engine.bindMigrationGateToTarget(context.Background(), schemaMigration001To002); err != nil {
			t.Fatalf("earlier edge binding with later owner = %v; want nil", err)
		}
	})

	t.Run("aggregate-run-defers-to-later-edge", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		rewriteMigrationSuperblock(t, backend, func(superblock *storageformat.Superblock) { superblock.RequiredFeatures = nil })
		rewriteMigrationGate(t, backend, func(gate *storageformat.WriteGate) {
			gate.Mode = storageformat.GateClosed
			gate.CheckpointID = schemaMigration002To003.checkpointID
		})
		superblockObject, err := backend.Get(context.Background(), storageformat.SuperblockKey())
		if err != nil {
			t.Fatal(err)
		}
		var superblock storageformat.Superblock
		if err := decodeCanonicalSuperblock(superblockObject.Body, &superblock); err != nil {
			t.Fatal(err)
		}
		if err := engine.runAggregateSchemaMigration(context.Background(), schemaMigration001To002, superblockObject, superblock, aggregateMigrationPlan{}); err != nil {
			t.Fatalf("aggregate run with later gate owner = %v; want nil", err)
		}
	})

	t.Run("unknown-maintenance-owns-gate", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		rewriteMigrationGate(t, backend, func(gate *storageformat.WriteGate) {
			gate.Mode = storageformat.GateClosed
			gate.CheckpointID = "unknown-maintenance"
		})
		if _, err := engine.closeStorageMigrationGate(context.Background(), schemaMigration002To003, aggregateMigrationPlan{}); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("unknown maintenance gate error = %v; want conflict", err)
		}
	})

	t.Run("unknown-gate-schema", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		rewriteMigrationGate(t, backend, func(gate *storageformat.WriteGate) {
			gate.Mode = storageformat.GateClosed
			gate.CheckpointID = schemaMigration002To003.checkpointID
			gate.WriterFeatures = []string{"unknown-feature"}
		})
		if _, err := engine.closeStorageMigrationGate(context.Background(), schemaMigration002To003, aggregateMigrationPlan{}); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("unknown gate schema error = %v; want precondition failed", err)
		}
	})
}

func TestMigrationDirectoryDiscoveryRejectsMalformedAndDisconnectedScopes(t *testing.T) {
	user, _ := domain.ParseUserID("WVhXWVhXWVhXWVhXWVhXWQ")
	orphanID := "AAAAAAAAAAAAAAAAAAAAAA"

	t.Run("malformed-root-key", func(t *testing.T) {
		backend := objectmemory.New()
		segments := strings.Split(storageformat.DirectoryRootKey(user.String(), "live", storageformat.RootDirectoryID).String(), "/")
		segments[3] = "invalid"
		migrationPut(t, backend, objectstore.MustKey(strings.Join(segments, "/")), []byte("{}"))
		engine := &Engine{backend: backend}
		if err := engine.migrateAllDirectoryAggregates(context.Background(), schemaMigration002To003, aggregateMigrationPlan{writeFileCounts: true}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("malformed root key error = %v; want invalid", err)
		}
	})

	t.Run("missing-canonical-root", func(t *testing.T) {
		backend := objectmemory.New()
		migrationPut(t, backend, storageformat.DirectoryRootKey(user.String(), "live", orphanID), []byte("{}"))
		engine := &Engine{backend: backend}
		if err := engine.migrateAllDirectoryAggregates(context.Background(), schemaMigration002To003, aggregateMigrationPlan{writeFileCounts: true}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("missing canonical root error = %v; want invalid", err)
		}
	})

	t.Run("unreachable-directory-root", func(t *testing.T) {
		backend, engine, scope, _, _ := emptyPhysicalMigrationRoot(t)
		migrationPut(t, backend, storageformat.DirectoryRootKey(scope.UserID().String(), areaName(scope.Area()), orphanID), []byte("{}"))
		if err := engine.migrateAllDirectoryAggregates(context.Background(), schemaMigration002To003, aggregateMigrationPlan{writeFileCounts: true}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("unreachable directory root error = %v; want invalid", err)
		}
	})
}

func TestMigrationWinnerValidationRejectsEveryInconsistentResult(t *testing.T) {
	user, _ := domain.ParseUserID("WVhXWVhXWVhXWVhXWVhXWQ")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	directoryID := "directory"
	createdAt := time.Date(2042, 1, 2, 3, 4, 5, 0, time.UTC)
	want := migrationAggregate{bytes: 4, files: 1}

	t.Run("missing-root", func(t *testing.T) {
		engine := &Engine{backend: objectmemory.New()}
		if _, err := engine.migrationWinnerMatchesTarget(context.Background(), scope, directoryID, want, schemaMigration002To003); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing winner root error = %v; want not found", err)
		}
	})

	t.Run("missing-manifest", func(t *testing.T) {
		backend := objectmemory.New()
		putMigrationRoot(t, backend, scope, directoryID, "manifest", storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: "manifest", RecursiveBytes: 4, RecursiveFileCount: 1})
		engine := &Engine{backend: backend}
		if _, err := engine.migrationWinnerMatchesTarget(context.Background(), scope, directoryID, want, schemaMigration002To003); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing winner manifest error = %v; want not found", err)
		}
	})

	t.Run("mixed-epoch", func(t *testing.T) {
		backend := objectmemory.New()
		putMigrationRoot(t, backend, scope, directoryID, "manifest", storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: "manifest", RecursiveBytes: 4, RecursiveFileCount: 1})
		putMigrationManifest(t, backend, scope, directoryID, "manifest", schema002DirectoryManifest{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: "manifest", PageIDs: []string{"page"}, RecursiveBytes: 4, CreatedAt: createdAt})
		engine := &Engine{backend: backend}
		if _, err := engine.migrationWinnerMatchesTarget(context.Background(), scope, directoryID, want, schemaMigration002To003); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("mixed winner error = %v; want invalid", err)
		}
	})

	t.Run("aggregate-mismatch", func(t *testing.T) {
		backend := objectmemory.New()
		putMigrationRoot(t, backend, scope, directoryID, "manifest", schema001DirectoryRoot{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: "manifest"})
		putMigrationManifest(t, backend, scope, directoryID, "manifest", schema001DirectoryManifest{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: "manifest", PageIDs: []string{"page"}, CreatedAt: createdAt})
		engine := &Engine{backend: backend}
		matched, err := engine.migrationWinnerMatchesTarget(context.Background(), scope, directoryID, want, schemaMigration001To002)
		if err != nil || matched {
			t.Fatalf("legacy winner match = %t, %v; want false, nil", matched, err)
		}
	})

	t.Run("schema-002-target", func(t *testing.T) {
		backend := objectmemory.New()
		putMigrationRoot(t, backend, scope, directoryID, "manifest", schema002DirectoryRoot{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: "manifest", RecursiveBytes: 4})
		putMigrationManifest(t, backend, scope, directoryID, "manifest", schema002DirectoryManifest{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: "manifest", PageIDs: []string{"page"}, RecursiveBytes: 4, CreatedAt: createdAt})
		engine := &Engine{backend: backend}
		matched, err := engine.migrationWinnerMatchesTarget(context.Background(), scope, directoryID, want, schemaMigration001To002)
		if err != nil || !matched {
			t.Fatalf("schema-002 winner match = %t, %v; want true, nil", matched, err)
		}
	})

	t.Run("unknown-target", func(t *testing.T) {
		backend := objectmemory.New()
		putMigrationRoot(t, backend, scope, directoryID, "manifest", schema002DirectoryRoot{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: "manifest", RecursiveBytes: 4})
		putMigrationManifest(t, backend, scope, directoryID, "manifest", schema002DirectoryManifest{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: "manifest", PageIDs: []string{"page"}, RecursiveBytes: 4, CreatedAt: createdAt})
		engine := &Engine{backend: backend}
		transition := storageMigration{to: storageSchemaID("unknown")}
		if _, err := engine.migrationWinnerMatchesTarget(context.Background(), scope, directoryID, want, transition); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("unknown winner target error = %v; want precondition failed", err)
		}
	})

	t.Run("schema-004-content-mismatch", func(t *testing.T) {
		_, engine, currentScope, root, _ := emptyPhysicalMigrationRoot(t)
		want := migrationAggregate{bytes: root.recursiveBytes, files: root.recursiveFileCount, directories: 1, accumulator: root.contentAccumulator}
		matched, err := engine.migrationWinnerMatchesTarget(t.Context(), currentScope, storageformat.RootDirectoryID, want, schemaMigration003To004)
		if err != nil || matched {
			t.Fatalf("schema-004 content mismatch = %t, %v", matched, err)
		}
	})

	t.Run("schema-004-index-read-failure", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		entry := withCurrentTestFingerprint(migrationFileEntry(t, "file", 1))
		prepared, err := engine.Files().prepareDirectory(t.Context(), scope, directoryID, []storageformat.DirectoryEntry{entry}, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, prerequisite := range prepared.prerequisites {
			migrationPut(t, backend, objectstore.MustKey(prerequisite.Key), prerequisite.Body)
		}
		migrationPut(t, backend, storageformat.DirectoryRootKey(scope.UserID().String(), areaName(scope.Area()), directoryID), prepared.rootBody)
		root, err := engine.readMigrationDirectoryRoot(t.Context(), scope, directoryID)
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := engine.readMigrationDirectoryManifest(t.Context(), scope, directoryID, root.manifestID)
		if err != nil {
			t.Fatal(err)
		}
		indexKey := storageformat.DirectoryIndexNodeKey(scope.UserID().String(), areaName(scope.Area()), directoryID, manifest.manifest.IndexRootID)
		index, err := backend.Get(t.Context(), indexKey)
		if err != nil {
			t.Fatal(err)
		}
		if err := backend.Delete(t.Context(), indexKey, objectstore.DeleteCondition{Version: index.Version}); err != nil {
			t.Fatal(err)
		}
		want := migrationAggregate{bytes: root.recursiveBytes, files: root.recursiveFileCount, directories: 1, accumulator: root.contentAccumulator, digest: root.contentDigest}
		if _, err := engine.migrationWinnerMatchesTarget(t.Context(), scope, directoryID, want, schemaMigration003To004); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing winner index error = %v", err)
		}
	})

	t.Run("schema-004-content-index-read-failure", func(t *testing.T) {
		backend, engine := currentMigrationEngine(t)
		entry := withCurrentTestFingerprint(migrationFileEntry(t, "file", 1))
		prepared, err := engine.Files().prepareDirectory(t.Context(), scope, directoryID, []storageformat.DirectoryEntry{entry}, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, prerequisite := range prepared.prerequisites {
			migrationPut(t, backend, objectstore.MustKey(prerequisite.Key), prerequisite.Body)
		}
		migrationPut(t, backend, storageformat.DirectoryRootKey(scope.UserID().String(), areaName(scope.Area()), directoryID), prepared.rootBody)
		root, err := engine.readMigrationDirectoryRoot(t.Context(), scope, directoryID)
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := engine.readMigrationDirectoryManifest(t.Context(), scope, directoryID, root.manifestID)
		if err != nil {
			t.Fatal(err)
		}
		indexKey := storageformat.DirectoryContentIndexNodeKey(scope.UserID().String(), areaName(scope.Area()), directoryID, manifest.manifest.ContentIndexRootID)
		index, err := backend.Get(t.Context(), indexKey)
		if err != nil {
			t.Fatal(err)
		}
		if err := backend.Delete(t.Context(), indexKey, objectstore.DeleteCondition{Version: index.Version}); err != nil {
			t.Fatal(err)
		}
		want := migrationAggregate{bytes: root.recursiveBytes, files: root.recursiveFileCount, directories: 1, accumulator: root.contentAccumulator, digest: root.contentDigest}
		if _, err := engine.migrationWinnerMatchesTarget(t.Context(), scope, directoryID, want, schemaMigration003To004); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing winner content index error = %v", err)
		}
	})

	t.Run("schema-004-rejects-sha-entry", func(t *testing.T) {
		backend := objectmemory.New()
		entry := migrationFileEntry(t, "file", 1)
		entry.SHA256 = storageformat.Digest([]byte("legacy"))
		entry.LogicalVersion, _ = directoryEntryVersion(entry)
		pageID, manifestID := "page", "manifest"
		pageKey := storageformat.DirectoryPageKey(scope.UserID().String(), areaName(scope.Area()), directoryID, pageID)
		migrationPut(t, backend, pageKey, migrationEnvelope(t, directoryPageSchema, pageKey, storageformat.DirectoryPage{SchemaVersion: 1, DirectoryID: directoryID, PageID: pageID, Entries: []storageformat.DirectoryEntry{entry}}))
		putMigrationRoot(t, backend, scope, directoryID, manifestID, storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: manifestID, RecursiveBytes: 1, RecursiveFileCount: 1, ContentAccumulator: "accumulator", ContentDigest: "digest"})
		putMigrationManifest(t, backend, scope, directoryID, manifestID, storageformat.DirectoryManifest{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: manifestID, PageIDs: []string{pageID}, EntryCount: 1, RecursiveBytes: 1, RecursiveFileCount: 1, ContentAccumulator: "accumulator", ContentDigest: "digest", CreatedAt: createdAt})
		engine := &Engine{backend: backend}
		want := migrationAggregate{bytes: 1, files: 1, directories: 1, accumulator: "accumulator", digest: "digest"}
		matched, err := engine.migrationWinnerMatchesTarget(t.Context(), scope, directoryID, want, schemaMigration003To004)
		if err != nil || matched {
			t.Fatalf("SHA winner match = %t, %v", matched, err)
		}
	})
}

func putMigrationRoot(t *testing.T, backend objectstore.Backend, scope domain.Scope, directoryID, manifestID string, value any) {
	t.Helper()
	key := storageformat.DirectoryRootKey(scope.UserID().String(), areaName(scope.Area()), directoryID)
	migrationPut(t, backend, key, migrationEnvelope(t, directoryRootSchema, key, value))
}

func putMigrationManifest(t *testing.T, backend objectstore.Backend, scope domain.Scope, directoryID, manifestID string, value any) {
	t.Helper()
	key := storageformat.DirectoryManifestKey(scope.UserID().String(), areaName(scope.Area()), directoryID, manifestID)
	migrationPut(t, backend, key, migrationEnvelope(t, directoryManifestSchema, key, value))
}

func TestMigrationDirectoryWalkRejectsMixedEpochsAndAggregateContradictions(t *testing.T) {
	tests := []struct {
		name       string
		root       func(string) any
		manifest   func(string, []string, time.Time) any
		transition storageMigration
		plan       aggregateMigrationPlan
	}{
		{
			name: "mixed-root-and-manifest",
			root: func(manifestID string) any {
				return schema002DirectoryRoot{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, ManifestID: manifestID}
			},
		},
		{
			name: "current-aggregate-mismatch",
			root: func(manifestID string) any {
				return storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, ManifestID: manifestID, RecursiveBytes: 1, RecursiveFileCount: 1}
			},
			manifest: func(manifestID string, pages []string, createdAt time.Time) any {
				return storageformat.DirectoryManifest{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, ManifestID: manifestID, PageIDs: pages, RecursiveBytes: 1, RecursiveFileCount: 1, CreatedAt: createdAt}
			},
		},
		{
			name: "byte-only-aggregate-mismatch",
			root: func(manifestID string) any {
				return schema002DirectoryRoot{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, ManifestID: manifestID, RecursiveBytes: 1}
			},
			manifest: func(manifestID string, pages []string, createdAt time.Time) any {
				return schema002DirectoryManifest{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, ManifestID: manifestID, PageIDs: pages, RecursiveBytes: 1, CreatedAt: createdAt}
			},
			transition: schemaMigration001To002,
		},
		{
			name: "schema-002-edge-with-schema-001-directory",
			root: func(manifestID string) any {
				return schema001DirectoryRoot{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, ManifestID: manifestID}
			},
			manifest: func(manifestID string, pages []string, createdAt time.Time) any {
				return schema001DirectoryManifest{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, ManifestID: manifestID, PageIDs: pages, CreatedAt: createdAt}
			},
			transition: schemaMigration002To003,
			plan:       aggregateMigrationPlan{writeFileCounts: true},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend, engine, scope, root, manifest := emptyPhysicalMigrationRoot(t)
			if test.root != nil {
				replaceMigrationBody(t, backend, root.object.Key, migrationEnvelope(t, directoryRootSchema, root.object.Key, test.root(root.manifestID)))
			}
			if test.manifest != nil {
				key := storageformat.DirectoryManifestKey(scope.UserID().String(), areaName(scope.Area()), storageformat.RootDirectoryID, root.manifestID)
				replaceMigrationBody(t, backend, key, migrationEnvelope(t, directoryManifestSchema, key, test.manifest(root.manifestID, manifest.manifest.PageIDs, manifest.manifest.CreatedAt)))
			}
			walk := &migrationWalk{
				engine: engine, group: migrationScope{scope: scope, roots: map[string]struct{}{storageformat.RootDirectoryID: {}}},
				transition: test.transition, plan: test.plan,
				state: make(map[string]uint8), totals: make(map[string]migrationAggregate), parents: make(map[string]string),
			}
			if _, err := walk.directory(context.Background(), storageformat.RootDirectoryID, ""); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("contradictory directory error = %v; want invalid", err)
			}
		})
	}
}

func TestMigrationDirectoryWalkReachesLegacyAggregateDenials(t *testing.T) {
	type legacyKind int
	const (
		legacySchema001 legacyKind = iota
		legacySchema002
	)
	setup := func(t *testing.T, kind legacyKind, entries []storageformat.DirectoryEntry, recursiveBytes int64) (*objectmemory.Backend, *Engine, domain.Scope, migrationDirectoryRoot) {
		t.Helper()
		backend, engine, scope, root, manifest := emptyPhysicalMigrationRoot(t)
		pageID := "legacy-page"
		pageKey := storageformat.DirectoryPageKey(scope.UserID().String(), areaName(scope.Area()), storageformat.RootDirectoryID, pageID)
		migrationPut(t, backend, pageKey, migrationEnvelope(t, directoryPageSchema, pageKey, storageformat.DirectoryPage{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, PageID: pageID, Entries: entries}))
		manifestKey := storageformat.DirectoryManifestKey(scope.UserID().String(), areaName(scope.Area()), storageformat.RootDirectoryID, root.manifestID)
		switch kind {
		case legacySchema001:
			replaceMigrationBody(t, backend, root.object.Key, migrationEnvelope(t, directoryRootSchema, root.object.Key, schema001DirectoryRoot{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, ManifestID: root.manifestID}))
			replaceMigrationBody(t, backend, manifestKey, migrationEnvelope(t, directoryManifestSchema, manifestKey, schema001DirectoryManifest{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, ManifestID: root.manifestID, PageIDs: []string{pageID}, EntryCount: len(entries), CreatedAt: manifest.manifest.CreatedAt}))
		case legacySchema002:
			replaceMigrationBody(t, backend, root.object.Key, migrationEnvelope(t, directoryRootSchema, root.object.Key, schema002DirectoryRoot{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, ManifestID: root.manifestID, RecursiveBytes: recursiveBytes}))
			replaceMigrationBody(t, backend, manifestKey, migrationEnvelope(t, directoryManifestSchema, manifestKey, schema002DirectoryManifest{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, ManifestID: root.manifestID, PageIDs: []string{pageID}, EntryCount: len(entries), RecursiveBytes: recursiveBytes, CreatedAt: manifest.manifest.CreatedAt}))
		}
		return backend, engine, scope, root
	}
	walk := func(engine *Engine, scope domain.Scope, transition storageMigration, plan aggregateMigrationPlan, totals map[string]migrationAggregate) error {
		state := make(map[string]uint8)
		roots := map[string]struct{}{storageformat.RootDirectoryID: {}}
		for directoryID := range totals {
			state[directoryID] = 2
			roots[directoryID] = struct{}{}
		}
		candidate := &migrationWalk{
			engine: engine, group: migrationScope{scope: scope, roots: roots},
			transition: transition, plan: plan, state: state, totals: totals, parents: make(map[string]string),
		}
		_, err := candidate.directory(t.Context(), storageformat.RootDirectoryID, "")
		return err
	}

	t.Run("schema-002-byte-mismatch", func(t *testing.T) {
		_, engine, scope, _ := setup(t, legacySchema002, nil, 1)
		if err := walk(engine, scope, schemaMigration001To002, aggregateMigrationPlan{}, nil); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("byte mismatch error = %v", err)
		}
	})
	t.Run("schema-002-edge-sees-schema-001", func(t *testing.T) {
		_, engine, scope, _ := setup(t, legacySchema001, nil, 0)
		if err := walk(engine, scope, schemaMigration002To003, aggregateMigrationPlan{writeFileCounts: true}, nil); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("wrong source epoch error = %v", err)
		}
	})
	t.Run("directory-count-overflow", func(t *testing.T) {
		child := migrationDirectoryEntry(t, "child", "AAAAAAAAAAAAAAAAAAAAAA", 0, 0)
		backend, engine, scope, root := setup(t, legacySchema001, []storageformat.DirectoryEntry{child}, 0)
		_ = backend
		_ = root
		totals := map[string]migrationAggregate{child.DirectoryID: {directories: math.MaxInt64}}
		if err := walk(engine, scope, schemaMigration001To002, aggregateMigrationPlan{}, totals); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("directory-count overflow error = %v", err)
		}
	})
	t.Run("file-count-overflow", func(t *testing.T) {
		left := migrationDirectoryEntry(t, "left", "AAAAAAAAAAAAAAAAAAAAAA", 0, math.MaxInt64)
		right := migrationDirectoryEntry(t, "right", "BBBBBBBBBBBBBBBBBBBBBB", 0, 1)
		entries := []storageformat.DirectoryEntry{left, right}
		sort.Slice(entries, func(i, j int) bool { return entries[i].NameDigest < entries[j].NameDigest })
		_, engine, scope, _ := setup(t, legacySchema001, entries, 0)
		totals := map[string]migrationAggregate{
			left.DirectoryID:  {files: math.MaxInt64, directories: 1},
			right.DirectoryID: {files: 1, directories: 1},
		}
		if err := walk(engine, scope, schemaMigration001To002, aggregateMigrationPlan{}, totals); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("file-count overflow error = %v", err)
		}
	})
	t.Run("current-content-mismatch", func(t *testing.T) {
		backend, engine, scope, root, _ := emptyPhysicalMigrationRoot(t)
		key := root.object.Key
		replaceMigrationBody(t, backend, key, migrationEnvelope(t, directoryRootSchema, key, storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, ManifestID: root.manifestID, RecursiveBytes: root.recursiveBytes, RecursiveFileCount: root.recursiveFileCount, ContentAccumulator: root.contentAccumulator, ContentDigest: storageformat.Digest([]byte("wrong"))}))
		if err := walk(engine, scope, schemaMigration003To004, aggregateMigrationPlan{writeProviderFingerprints: true}, nil); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("content mismatch error = %v", err)
		}
	})
	t.Run("current-aggregate-mismatch", func(t *testing.T) {
		backend, engine, scope, root, _ := emptyPhysicalMigrationRoot(t)
		key := root.object.Key
		replaceMigrationBody(t, backend, key, migrationEnvelope(t, directoryRootSchema, key, storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, ManifestID: root.manifestID, RecursiveBytes: 1, RecursiveFileCount: root.recursiveFileCount, ContentAccumulator: root.contentAccumulator, ContentDigest: root.contentDigest}))
		if err := walk(engine, scope, schemaMigration002To003, aggregateMigrationPlan{writeFileCounts: true}, nil); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("current aggregate mismatch error = %v", err)
		}
	})
}

func TestMigrationDirectoryWalkRejectsStaleChildAggregates(t *testing.T) {
	for _, test := range []struct {
		name       string
		entryBytes int64
		entryFiles int64
	}{
		{name: "bytes", entryBytes: 0, entryFiles: 1},
		{name: "file-count", entryBytes: 1, entryFiles: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend, engine := currentMigrationEngine(t)
			user, _ := domain.ParseUserID("WVhXWVhXWVhXWVhXWVhXWQ")
			scope, _ := domain.NewScope(user, domain.AreaLive)
			childID := "AAAAAAAAAAAAAAAAAAAAAA"
			entry := migrationDirectoryEntry(t, "child", childID, test.entryBytes, test.entryFiles)
			var contentEntries []storageformat.DirectoryContentIndexEntry
			if test.entryFiles == 1 {
				file := withCurrentTestFingerprint(migrationFileEntry(t, "file", test.entryBytes))
				content, contentErr := directoryContentIndexEntry(domain.MustParseUserPath("/child/file"), file)
				if contentErr != nil {
					t.Fatal(contentErr)
				}
				contentEntries = []storageformat.DirectoryContentIndexEntry{content}
			}
			prepared, err := engine.Files().prepareDirectoryWithContentEntries(scope, storageformat.RootDirectoryID, []storageformat.DirectoryEntry{entry}, contentEntries, 1)
			if err != nil {
				t.Fatal(err)
			}
			for _, prerequisite := range prepared.prerequisites {
				migrationPut(t, backend, objectstore.MustKey(prerequisite.Key), prerequisite.Body)
			}
			migrationPut(t, backend, storageformat.DirectoryRootKey(user.String(), areaName(scope.Area()), storageformat.RootDirectoryID), prepared.rootBody)

			walk := &migrationWalk{
				engine: engine,
				group: migrationScope{scope: scope, roots: map[string]struct{}{
					storageformat.RootDirectoryID: {},
					childID:                       {},
				}},
				transition: schemaMigration002To003,
				plan:       aggregateMigrationPlan{writeFileCounts: true},
				state:      map[string]uint8{childID: 2},
				totals:     map[string]migrationAggregate{childID: {bytes: 1, files: 1}},
				parents:    make(map[string]string),
			}
			if _, err := walk.directory(context.Background(), storageformat.RootDirectoryID, ""); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("stale child %s aggregate error = %v; want invalid", test.name, err)
			}
		})
	}
}

func TestMigrationDirectoryPreparationRejectsInvalidAndOverflowingEntries(t *testing.T) {
	_, engine := currentMigrationEngine(t)
	user, _ := domain.ParseUserID("WVhXWVhXWVhXWVhXWVhXWQ")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	root := migrationDirectoryRoot{envelope: storageformat.Envelope{Revision: 1, LogicalVersion: "version"}}
	createdAt := time.Date(2042, 1, 2, 3, 4, 5, 0, time.UTC)

	invalid := storageformat.DirectoryEntry{Name: "invalid"}
	if _, err := engine.prepareMigratedDirectory(context.Background(), scope, storageformat.RootDirectoryID, []storageformat.DirectoryEntry{invalid}, root, createdAt, schemaMigration001To002, aggregateMigrationPlan{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid entry preparation error = %v; want invalid", err)
	}

	byteOverflow := []storageformat.DirectoryEntry{
		migrationFileEntry(t, "one", math.MaxInt64),
		migrationFileEntry(t, "two", 1),
	}
	sort.Slice(byteOverflow, func(i, j int) bool { return byteOverflow[i].NameDigest < byteOverflow[j].NameDigest })
	if _, err := engine.prepareMigratedDirectory(context.Background(), scope, storageformat.RootDirectoryID, byteOverflow, root, createdAt, schemaMigration001To002, aggregateMigrationPlan{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("byte overflow preparation error = %v; want invalid", err)
	}

	countOverflow := []storageformat.DirectoryEntry{
		migrationDirectoryEntry(t, "one", "directory-one", 0, math.MaxInt64),
		migrationDirectoryEntry(t, "two", "directory-two", 0, 1),
	}
	sort.Slice(countOverflow, func(i, j int) bool { return countOverflow[i].NameDigest < countOverflow[j].NameDigest })
	if _, err := engine.prepareMigratedDirectory(context.Background(), scope, storageformat.RootDirectoryID, countOverflow, root, createdAt, schemaMigration002To003, aggregateMigrationPlan{writeFileCounts: true}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("file-count overflow preparation error = %v; want invalid", err)
	}
}

func TestMigrationDirectoryMarkReadWriteDenialAndUpdatePaths(t *testing.T) {
	type markFixture struct {
		backend  *objectmemory.Backend
		engine   *Engine
		scope    domain.Scope
		root     migrationDirectoryRoot
		manifest migrationDirectoryManifest
		walk     *migrationWalk
		total    migrationAggregate
		key      objectstore.Key
	}
	setup := func(t *testing.T) markFixture {
		t.Helper()
		backend, engine, scope, root, manifest := emptyPhysicalMigrationRoot(t)
		walk := &migrationWalk{
			engine: engine, group: migrationScope{scope: scope}, transition: schemaMigration003To004,
			plan: aggregateMigrationPlan{writeProviderFingerprints: true}, phase: migrationPhaseTransform,
		}
		total := migrationAggregate{
			bytes: root.recursiveBytes, files: root.recursiveFileCount, directories: 1,
			accumulator: root.contentAccumulator, digest: root.contentDigest,
		}
		key := storageformat.MigrationDirectoryMarkKey(schemaMigration003To004.checkpointID, migrationPhaseTransform, scope.UserID().String(), areaName(scope.Area()), storageformat.RootDirectoryID)
		return markFixture{backend: backend, engine: engine, scope: scope, root: root, manifest: manifest, walk: walk, total: total, key: key}
	}
	write := func(t *testing.T, fixture markFixture) {
		t.Helper()
		if err := fixture.walk.writeCompletedDirectoryMark(t.Context(), storageformat.RootDirectoryID, "", "", fixture.total); err != nil {
			t.Fatal(err)
		}
	}
	mutate := func(t *testing.T, fixture markFixture, change func(*storageformat.MigrationDirectoryMark)) {
		t.Helper()
		object, err := fixture.backend.Get(t.Context(), fixture.key)
		if err != nil {
			t.Fatal(err)
		}
		var envelope storageformat.Envelope
		var mark storageformat.MigrationDirectoryMark
		if err := storageformat.DecodeEnvelope(object.Body, fixture.key, migrationDirectoryMarkSchema, &envelope, &mark); err != nil {
			t.Fatal(err)
		}
		change(&mark)
		body, err := storageformat.EncodeEnvelope(migrationDirectoryMarkSchema, fixture.key, envelope.Revision+1, mark)
		if err != nil {
			t.Fatal(err)
		}
		replaceMigrationBody(t, fixture.backend, fixture.key, body)
	}
	deleteObject := func(t *testing.T, backend *objectmemory.Backend, key objectstore.Key) {
		t.Helper()
		object, err := backend.Get(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		if err := backend.Delete(t.Context(), key, objectstore.DeleteCondition{Version: object.Version}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("complete-in-memory", func(t *testing.T) {
		fixture := setup(t)
		fixture.walk.phase = ""
		fixture.walk.state = make(map[string]uint8)
		fixture.walk.totals = make(map[string]migrationAggregate)
		if err := fixture.walk.completeDirectory(t.Context(), storageformat.RootDirectoryID, "", "", fixture.total); err != nil || fixture.walk.totals[storageformat.RootDirectoryID] != fixture.total {
			t.Fatalf("complete directory = %+v, %v", fixture.walk.totals, err)
		}
	})
	t.Run("invalid-total", func(t *testing.T) {
		fixture := setup(t)
		fixture.total.directories = 0
		if err := fixture.walk.writeCompletedDirectoryMark(t.Context(), storageformat.RootDirectoryID, "", "", fixture.total); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid total error = %v", err)
		}
	})
	for _, test := range []struct {
		name   string
		change func(*markFixture)
	}{
		{name: "missing-root", change: func(fixture *markFixture) {
			deleteObject(t, fixture.backend, storageformat.DirectoryRootKey(fixture.scope.UserID().String(), areaName(fixture.scope.Area()), storageformat.RootDirectoryID))
		}},
		{name: "missing-manifest", change: func(fixture *markFixture) {
			deleteObject(t, fixture.backend, storageformat.DirectoryManifestKey(fixture.scope.UserID().String(), areaName(fixture.scope.Area()), storageformat.RootDirectoryID, fixture.root.manifestID))
		}},
		{name: "byte-aggregate", change: func(fixture *markFixture) { fixture.total.bytes++ }},
		{name: "file-aggregate", change: func(fixture *markFixture) { fixture.total.files++ }},
		{name: "content-identity", change: func(fixture *markFixture) { fixture.total.digest = storageformat.Digest([]byte("different")) }},
	} {
		t.Run("write-"+test.name, func(t *testing.T) {
			fixture := setup(t)
			test.change(&fixture)
			if err := fixture.walk.writeCompletedDirectoryMark(t.Context(), storageformat.RootDirectoryID, "", "", fixture.total); err == nil {
				t.Fatal("invalid mark write succeeded")
			}
		})
	}
	t.Run("update-existing", func(t *testing.T) {
		fixture := setup(t)
		write(t, fixture)
		fixture.total.directories = 2
		if err := fixture.walk.writeCompletedDirectoryMark(t.Context(), storageformat.RootDirectoryID, "", "", fixture.total); err != nil {
			t.Fatal(err)
		}
		got, found, err := fixture.walk.readCompletedDirectoryMark(t.Context(), storageformat.RootDirectoryID, "", "")
		if err != nil || !found || got.directories != 2 {
			t.Fatalf("updated mark = %+v, %t, %v", got, found, err)
		}
	})
	t.Run("write-corrupt-existing", func(t *testing.T) {
		fixture := setup(t)
		write(t, fixture)
		replaceMigrationBody(t, fixture.backend, fixture.key, []byte("{}"))
		if err := fixture.walk.writeCompletedDirectoryMark(t.Context(), storageformat.RootDirectoryID, "", "", fixture.total); err == nil {
			t.Fatal("corrupt existing mark was accepted")
		}
	})
	t.Run("write-conflicting-existing", func(t *testing.T) {
		fixture := setup(t)
		write(t, fixture)
		mutate(t, fixture, func(mark *storageformat.MigrationDirectoryMark) { mark.ParentEntryName = "other" })
		if err := fixture.walk.writeCompletedDirectoryMark(t.Context(), storageformat.RootDirectoryID, "", "", fixture.total); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("conflicting mark error = %v", err)
		}
	})
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "transport", err: domain.NewError(domain.ErrorUnavailable, "injected mark update failure")},
		{name: "contention", err: domain.NewError(domain.ErrorPreconditionFailed, "injected mark contention")},
	} {
		t.Run("write-"+test.name, func(t *testing.T) {
			fixture := setup(t)
			write(t, fixture)
			fixture.engine.backend = &migrationMarkPutFaultBackend{Backend: fixture.backend, err: test.err}
			fixture.total.directories = 2
			err := fixture.walk.writeCompletedDirectoryMark(t.Context(), storageformat.RootDirectoryID, "", "", fixture.total)
			if test.name == "transport" && !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("mark transport error = %v", err)
			}
			if test.name == "contention" && !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("mark contention error = %v", err)
			}
		})
	}
	for _, test := range []struct {
		name   string
		change func(*storageformat.MigrationDirectoryMark)
	}{
		{name: "invalid", change: func(mark *storageformat.MigrationDirectoryMark) { mark.DirectoryCount = 0 }},
		{name: "changed-version", change: func(mark *storageformat.MigrationDirectoryMark) {
			mark.RootLogicalVersion = storageformat.Digest([]byte("changed"))
		}},
		{name: "stale-aggregate", change: func(mark *storageformat.MigrationDirectoryMark) { mark.RecursiveBytes++ }},
	} {
		t.Run("read-"+test.name, func(t *testing.T) {
			fixture := setup(t)
			write(t, fixture)
			mutate(t, fixture, test.change)
			_, found, err := fixture.walk.readCompletedDirectoryMark(t.Context(), storageformat.RootDirectoryID, "", "")
			if test.name == "changed-version" {
				if err != nil || found {
					t.Fatalf("changed-version mark = %t, %v", found, err)
				}
			} else if err == nil {
				t.Fatalf("invalid mark was accepted (found=%t)", found)
			}
		})
	}
	t.Run("read-corrupt", func(t *testing.T) {
		fixture := setup(t)
		write(t, fixture)
		replaceMigrationBody(t, fixture.backend, fixture.key, []byte("{}"))
		if _, _, err := fixture.walk.readCompletedDirectoryMark(t.Context(), storageformat.RootDirectoryID, "", ""); err == nil {
			t.Fatal("corrupt mark was accepted")
		}
	})
	t.Run("read-missing-root", func(t *testing.T) {
		fixture := setup(t)
		write(t, fixture)
		deleteObject(t, fixture.backend, storageformat.DirectoryRootKey(fixture.scope.UserID().String(), areaName(fixture.scope.Area()), storageformat.RootDirectoryID))
		if _, _, err := fixture.walk.readCompletedDirectoryMark(t.Context(), storageformat.RootDirectoryID, "", ""); err == nil {
			t.Fatal("mark with missing root was accepted")
		}
	})
	t.Run("read-missing-manifest", func(t *testing.T) {
		fixture := setup(t)
		write(t, fixture)
		deleteObject(t, fixture.backend, storageformat.DirectoryManifestKey(fixture.scope.UserID().String(), areaName(fixture.scope.Area()), storageformat.RootDirectoryID, fixture.root.manifestID))
		if _, _, err := fixture.walk.readCompletedDirectoryMark(t.Context(), storageformat.RootDirectoryID, "", ""); err == nil {
			t.Fatal("mark with missing manifest was accepted")
		}
	})
}

func TestMigrationFingerprintAndStreamingPreparationDenials(t *testing.T) {
	backend, engine := currentMigrationEngine(t)
	user, _ := domain.ParseUserID("WVhXWVhXWVhXWVhXWVhXWQ")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	if _, err := engine.migrateDirectoryEntryFingerprint(t.Context(), scope, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("nil fingerprint entry error = %v", err)
	}
	missing := migrationFileEntry(t, "missing", 1)
	if _, err := engine.migrateDirectoryEntryFingerprint(t.Context(), scope, &missing); err == nil {
		t.Fatal("missing blob metadata was accepted")
	}
	blobKey := storageformat.BlobKey(user.String(), "blob-file")
	migrationPut(t, backend, blobKey, []byte("x"))
	mismatch := migrationFileEntry(t, "file", 2)
	if _, err := engine.migrateDirectoryEntryFingerprint(t.Context(), scope, &mismatch); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("size mismatch error = %v", err)
	}
	mismatch.Size = 1
	mismatch.MD5 = "incomplete"
	if _, err := engine.migrateDirectoryEntryFingerprint(t.Context(), scope, &mismatch); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stored fingerprint mismatch error = %v", err)
	}
	if err := engine.migrateAllDirectoryAggregatesPhase(t.Context(), schemaMigration003To004, aggregateMigrationPlan{}, "invalid"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid migration phase error = %v", err)
	}

	entry := withCurrentTestFingerprint(migrationFileEntry(t, "file", 1))
	root := migrationDirectoryRoot{envelope: storageformat.Envelope{Revision: 1, LogicalVersion: "version"}}
	for failAt := 1; failAt <= 3; failAt++ {
		stateBackend, candidate := currentMigrationEngine(t)
		faults := &migrationPreparationFaultBackend{Backend: stateBackend, failAt: failAt}
		candidate.backend = faults
		if _, err := candidate.prepareMigratedDirectory(t.Context(), scope, storageformat.RootDirectoryID, []storageformat.DirectoryEntry{entry}, root, time.Date(2042, 1, 2, 3, 4, 5, 0, time.UTC), schemaMigration003To004, aggregateMigrationPlan{writeProviderFingerprints: true}); err == nil {
			t.Fatalf("streaming preparation fault %d was ignored", failAt)
		}
	}
	missingDirectory := migrationDirectoryEntry(t, "child", "AAAAAAAAAAAAAAAAAAAAAA", 1, 1)
	if _, err := engine.prepareMigratedDirectory(t.Context(), scope, storageformat.RootDirectoryID, []storageformat.DirectoryEntry{missingDirectory}, root, time.Date(2042, 1, 2, 3, 4, 5, 0, time.UTC), schemaMigration003To004, aggregateMigrationPlan{writeProviderFingerprints: true}); err == nil {
		t.Fatal("missing child content index was accepted")
	}
	invalidContent := migrationDirectoryEntry(t, "invalid-content", "BBBBBBBBBBBBBBBBBBBBBB", 0, 0)
	invalidContent.ContentDigest = ""
	invalidContent.LogicalVersion, _ = directoryEntryVersion(invalidContent)
	if _, err := engine.prepareMigratedDirectory(t.Context(), scope, storageformat.RootDirectoryID, []storageformat.DirectoryEntry{invalidContent}, root, time.Date(2042, 1, 2, 3, 4, 5, 0, time.UTC), schemaMigration003To004, aggregateMigrationPlan{writeProviderFingerprints: true}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid content identity error = %v", err)
	}
}

func emptyPhysicalMigrationRoot(t *testing.T) (*objectmemory.Backend, *Engine, domain.Scope, migrationDirectoryRoot, migrationDirectoryManifest) {
	t.Helper()
	backend, engine := currentMigrationEngine(t)
	user, _ := domain.ParseUserID("WVhXWVhXWVhXWVhXWVhXWQ")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	prepared, err := engine.Files().prepareDirectory(context.Background(), scope, storageformat.RootDirectoryID, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, prerequisite := range prepared.prerequisites {
		migrationPut(t, backend, objectstore.MustKey(prerequisite.Key), prerequisite.Body)
	}
	rootKey := storageformat.DirectoryRootKey(user.String(), areaName(scope.Area()), storageformat.RootDirectoryID)
	migrationPut(t, backend, rootKey, prepared.rootBody)
	root, err := engine.readMigrationDirectoryRoot(context.Background(), scope, storageformat.RootDirectoryID)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := engine.readMigrationDirectoryManifest(context.Background(), scope, storageformat.RootDirectoryID, root.manifestID)
	if err != nil {
		t.Fatal(err)
	}
	return backend, engine, scope, root, manifest
}

func migrationFileEntry(t *testing.T, name string, size int64) storageformat.DirectoryEntry {
	t.Helper()
	entry := storageformat.DirectoryEntry{
		Name: name, NameDigest: storageformat.NameDigest(name), Kind: domain.EntryFile, BlobID: "blob-" + name,
		Size: size, MediaType: "application/octet-stream", ModifiedAt: time.Date(2042, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	entry.LogicalVersion, _ = directoryEntryVersion(entry)
	return entry
}

func migrationDirectoryEntry(t *testing.T, name, directoryID string, size, files int64) storageformat.DirectoryEntry {
	t.Helper()
	emptyDigest, err := directoryContentDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	entry := storageformat.DirectoryEntry{
		Name: name, NameDigest: storageformat.NameDigest(name), Kind: domain.EntryDirectory, DirectoryID: directoryID,
		Size: size, FileCount: files, ContentDigest: emptyDigest, ModifiedAt: time.Date(2042, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	entry.LogicalVersion, _ = directoryEntryVersion(entry)
	return entry
}
