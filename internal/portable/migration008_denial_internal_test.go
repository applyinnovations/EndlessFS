package portable

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func newSchema008MigrationTestEngine(t *testing.T, backend objectstore.Backend) *Engine {
	t.Helper()
	return openInternalTestEngine(t, backend, domain.NewFixedClock(time.Date(2060, 1, 2, 3, 4, 5, 0, time.UTC)), strings.NewReader(strings.Repeat(t.Name(), 1<<14)))
}

func validSchema008MigrationStage(reference consistencyDomainRef, source string) schema008MigrationStage {
	return schema008MigrationStage{
		SchemaVersion:  1,
		SourceKey:      source,
		DomainKind:     reference.Kind,
		DomainID:       reference.ID,
		Key:            "logical/key",
		Value:          []byte("value"),
		LogicalVersion: "logical-version",
	}
}

func TestSchema008MigrationStageValidationAndMarkerRecoveryMatrix(t *testing.T) {
	ctx := context.Background()
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:migration-stage"}
	valid := validSchema008MigrationStage(reference, "endlessfs/v1/test/source")
	mutations := map[string]func(*schema008MigrationStage){
		"schema":          func(stage *schema008MigrationStage) { stage.SchemaVersion = 0 },
		"source":          func(stage *schema008MigrationStage) { stage.SourceKey = "" },
		"kind":            func(stage *schema008MigrationStage) { stage.DomainKind = "invalid" },
		"domain":          func(stage *schema008MigrationStage) { stage.DomainID = "" },
		"key":             func(stage *schema008MigrationStage) { stage.Key = "" },
		"logical-version": func(stage *schema008MigrationStage) { stage.LogicalVersion = "" },
		"tree":            func(stage *schema008MigrationStage) { stage.Tree = "invalid" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			stage := valid
			mutate(&stage)
			if _, _, err := validateSchema008MigrationStage(stage); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("stage validation error = %v", err)
			}
		})
	}
	oversized := valid
	oversized.Value = make([]byte, storageformat.MaxCanonicalBytes)
	if _, _, err := validateSchema008MigrationStage(oversized); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized stage error = %v", err)
	}

	backend := objectmemory.New()
	engine := newSchema008MigrationTestEngine(t, backend)
	stageKey, err := engine.writeSchema008MigrationStage(ctx, valid)
	if err != nil {
		t.Fatal(err)
	}
	if repeated, err := engine.writeSchema008MigrationStage(ctx, valid); err != nil || repeated != stageKey {
		t.Fatalf("idempotent stage = %s, %v", repeated.String(), err)
	}
	source := objectstore.MustKey(valid.SourceKey)
	if staged, err := engine.schema008MigrationSourceStaged(ctx, source); err != nil || staged {
		t.Fatalf("unmarked source staged=%v error=%v", staged, err)
	}
	if err := engine.markSchema008MigrationSource(ctx, source); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty source marker error = %v", err)
	}
	if err := engine.markSchema008MigrationSource(ctx, source, stageKey); err != nil {
		t.Fatal(err)
	}
	if err := engine.markSchema008MigrationSource(ctx, source, stageKey); err != nil {
		t.Fatalf("idempotent source marker = %v", err)
	}
	if staged, err := engine.schema008MigrationSourceStaged(ctx, source); err != nil || !staged {
		t.Fatalf("marked source staged=%v error=%v", staged, err)
	}

	t.Run("marker-missing-stage", func(t *testing.T) {
		base := objectmemory.New()
		candidate := newSchema008MigrationTestEngine(t, base)
		missingSource := objectstore.MustKey("endlessfs/v1/test/missing-stage-source")
		marker := schema008MigrationSourceMarker{SchemaVersion: 1, SourceKey: missingSource.String(), StageKeys: []string{"endlessfs/v1/test/missing-stage"}}
		body, err := storageformat.EncodeCanonical(marker)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := base.Put(ctx, storageformat.Schema008MigrationSourceMarkerKey(missingSource.String()), body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if _, err := candidate.schema008MigrationSourceStaged(ctx, missingSource); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing stage error = %v", err)
		}
	})

	t.Run("marker-invalid", func(t *testing.T) {
		base := objectmemory.New()
		candidate := newSchema008MigrationTestEngine(t, base)
		invalidSource := objectstore.MustKey("endlessfs/v1/test/invalid-marker-source")
		if _, err := base.Put(ctx, storageformat.Schema008MigrationSourceMarkerKey(invalidSource.String()), []byte("{}"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if _, err := candidate.schema008MigrationSourceStaged(ctx, invalidSource); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid marker error = %v", err)
		}
	})

	t.Run("stage-winner-differs", func(t *testing.T) {
		base := objectmemory.New()
		candidate := newSchema008MigrationTestEngine(t, base)
		key := storageformat.Schema008MigrationStageKey(schema008DomainIdentity(reference), valid.SourceKey)
		if _, err := base.Put(ctx, key, []byte("different"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if _, err := candidate.writeSchema008MigrationStage(ctx, valid); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("different stage winner error = %v", err)
		}
	})

	t.Run("marker-winner-differs", func(t *testing.T) {
		base := objectmemory.New()
		candidate := newSchema008MigrationTestEngine(t, base)
		key := storageformat.Schema008MigrationSourceMarkerKey(source.String())
		if _, err := base.Put(ctx, key, []byte("different"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := candidate.markSchema008MigrationSource(ctx, source, stageKey); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("different marker winner error = %v", err)
		}
	})

	t.Run("stage-provider-failure", func(t *testing.T) {
		base := objectmemory.New()
		hooks := &hookedBackend{Backend: base, put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return "", domain.NewError(domain.ErrorUnavailable, "put denied")
		}}
		candidate := newSchema008MigrationTestEngine(t, base)
		candidate.backend = hooks
		if _, err := candidate.writeSchema008MigrationStage(ctx, valid); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("stage put failure = %v", err)
		}
	})

	t.Run("marker-provider-failure", func(t *testing.T) {
		base := objectmemory.New()
		hooks := &hookedBackend{Backend: base, put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return "", domain.NewError(domain.ErrorUnavailable, "put denied")
		}}
		candidate := newSchema008MigrationTestEngine(t, base)
		candidate.backend = hooks
		if err := candidate.markSchema008MigrationSource(ctx, source, stageKey); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("marker put failure = %v", err)
		}
	})

	t.Run("source-marker-read-failure", func(t *testing.T) {
		base := objectmemory.New()
		hooks := &hookedBackend{Backend: base, get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "get denied")
		}}
		candidate := newSchema008MigrationTestEngine(t, base)
		candidate.backend = hooks
		if _, err := candidate.schema008MigrationSourceStaged(ctx, source); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("source marker read failure = %v", err)
		}
	})
}

func TestSchema008MigrationStagingPropagatesSourceProviderFailures(t *testing.T) {
	ctx := context.Background()
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	stateLogical := state.MustKey(state.NamespaceAccounts, owner.String())
	stateSource := canonicalStateKey(stateLogical)
	uploadSource := storageformat.OperationKey(owner.String(), "upload")
	idempotencySource := storageformat.IdempotencyKey(owner.String(), "key")
	operationSource := storageformat.OperationKey(owner.String(), "operation")
	now := time.Date(2060, 1, 2, 3, 4, 5, 0, time.UTC)

	for name, test := range map[string]struct {
		prefixSource objectstore.Key
		body         []byte
		stage        func(*Engine) error
	}{
		"state":       {prefixSource: stateSource, body: encodeInternalEnvelope(t, stateRecordSchema, stateSource, 1, storageformat.StateRecord{SchemaVersion: 1, LogicalKey: stateLogical.String(), Data: []byte("state")}), stage: func(engine *Engine) error { return engine.stageSchema007State008(ctx) }},
		"upload":      {prefixSource: uploadSource, body: encodeInternalEnvelope(t, uploadRecordSchema, uploadSource, 1, storageformat.UploadRecord{SchemaVersion: 1, UploadID: "upload", CompletionOperationID: "completion", UserID: owner.String(), Area: "live", RequestedPath: "/file", ResolvedPath: "/file", Size: 1, MediaType: "application/octet-stream", Conflict: domain.ConflictFail, State: storageformat.UploadCompleted, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}), stage: func(engine *Engine) error { return engine.stageSchema007Uploads008(ctx) }},
		"idempotency": {prefixSource: idempotencySource, body: encodeInternalEnvelope(t, idempotencySchema, idempotencySource, 1, storageformat.IdempotencyRecord{SchemaVersion: 1, UserID: owner.String(), Kind: "upload", KeyDigest: storageformat.Digest([]byte("key")), Fingerprint: storageformat.Digest([]byte("fingerprint")), OperationID: "upload"}), stage: func(engine *Engine) error { return engine.stageSchema007UploadIdempotency008(ctx) }},
		"operation":   {prefixSource: operationSource, body: encodeInternalEnvelope(t, fileOperationSchema, operationSource, 1, storageformat.FileOperation{SchemaVersion: 1, OperationID: "operation", UserID: owner.String(), Kind: operationCopy, State: storageformat.FileOperationSucceeded, Attempt: 1, Fence: 1, ReplicaAttemptID: "replica", ExpiresAt: now.Add(time.Hour), StartedAt: now, UpdatedAt: now, Roots: []storageformat.FileOperationRoot{{Key: "root", FinalBody: []byte("body")}}}), stage: func(engine *Engine) error { return engine.stageSchema007Operations008(ctx) }},
	} {
		t.Run(name, func(t *testing.T) {
			base := objectmemory.New()
			if _, err := base.Put(ctx, test.prefixSource, test.body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
				t.Fatal(err)
			}
			hooks := &hookedBackend{Backend: base, get: func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
				if key == test.prefixSource {
					return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "source get denied")
				}
				return base.Get(ctx, key)
			}}
			engine := newSchema008MigrationTestEngine(t, hooks)
			if err := test.stage(engine); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("source failure = %v", err)
			}
		})
	}
}

func TestSchema008MigrationStagesSchema007StateAndRejectsCorruptRestartMarkers(t *testing.T) {
	ctx := context.Background()
	owner := "WVhXWVhXWVhXWVhXWVhXWQ"
	logical := state.MustKey(state.NamespaceAccounts, owner)
	sourceKey := canonicalStateKey(logical)
	record := storageformat.StateRecord{SchemaVersion: 1, LogicalKey: logical.String(), Data: []byte("account")}

	newFixture := func(t *testing.T) (*objectmemory.Backend, *Engine) {
		t.Helper()
		backend := objectmemory.New()
		engine := newSchema008MigrationTestEngine(t, backend)
		body, err := storageformat.EncodeEnvelope(stateRecordSchema, sourceKey, 1, record)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Put(ctx, sourceKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		return backend, engine
	}

	t.Run("success-and-restart", func(t *testing.T) {
		backend, engine := newFixture(t)
		if err := engine.stageSchema007State008(ctx); err != nil {
			t.Fatal(err)
		}
		if err := engine.stageSchema007State008(ctx); err != nil {
			t.Fatalf("restart staging = %v", err)
		}
		marker, err := backend.Get(ctx, storageformat.Schema008MigrationSourceMarkerKey(sourceKey.String()))
		if err != nil || len(marker.Body) == 0 {
			t.Fatalf("source marker = %+v, %v", marker, err)
		}
	})

	t.Run("malformed-source", func(t *testing.T) {
		backend, engine := newFixture(t)
		replaceObjectBody(t, backend, sourceKey, []byte("not-json"))
		if err := engine.stageSchema007State008(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("malformed source error = %v", err)
		}
	})

	t.Run("source-binding", func(t *testing.T) {
		backend, engine := newFixture(t)
		object, err := backend.Get(ctx, sourceKey)
		if err != nil {
			t.Fatal(err)
		}
		wrong := record
		wrong.LogicalKey = state.MustKey(state.NamespaceAccounts, "different").String()
		body, err := storageformat.EncodeEnvelope(stateRecordSchema, sourceKey, 2, wrong)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Put(ctx, sourceKey, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
			t.Fatal(err)
		}
		if err := engine.stageSchema007State008(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("misbound source error = %v", err)
		}
	})

	t.Run("corrupt-marker", func(t *testing.T) {
		backend, engine := newFixture(t)
		markerKey := storageformat.Schema008MigrationSourceMarkerKey(sourceKey.String())
		if _, err := backend.Put(ctx, markerKey, []byte("{}"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := engine.stageSchema007State008(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("corrupt restart marker error = %v", err)
		}
	})

	t.Run("marker-stage-key", func(t *testing.T) {
		backend, engine := newFixture(t)
		markerKey := storageformat.Schema008MigrationSourceMarkerKey(sourceKey.String())
		marker := schema008MigrationSourceMarker{SchemaVersion: 1, SourceKey: sourceKey.String(), StageKeys: []string{"INVALID"}}
		body, _ := storageformat.EncodeCanonical(marker)
		if _, err := backend.Put(ctx, markerKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := engine.stageSchema007State008(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid restart stage key error = %v", err)
		}
	})

	t.Run("stage-write-failure", func(t *testing.T) {
		backend, engine := newFixture(t)
		hooks := &hookedBackend{Backend: backend, put: func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if strings.HasPrefix(key.String(), storageformat.Schema008MigrationStagePrefix()) {
				return "", domain.NewError(domain.ErrorUnavailable, "stage write denied")
			}
			return backend.Put(ctx, key, body, condition)
		}}
		engine.backend = hooks
		if err := engine.stageSchema007State008(ctx); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("state stage write failure = %v", err)
		}
	})
}

func TestSchema008MigrationStagesTerminalUploadsAndAuthenticatesIdempotencyKeys(t *testing.T) {
	ctx := context.Background()
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	now := time.Date(2060, 1, 2, 3, 4, 5, 0, time.UTC)

	putEnvelope := func(t *testing.T, backend objectstore.Backend, key objectstore.Key, schema string, value any) {
		t.Helper()
		body, err := storageformat.EncodeEnvelope(schema, key, 1, value)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}
	validUpload := func(stateValue storageformat.UploadState) storageformat.UploadRecord {
		return storageformat.UploadRecord{
			SchemaVersion: 1, UploadID: "upload", CompletionOperationID: "completion", UserID: owner.String(), Area: "live",
			RequestedPath: "/file.bin", ResolvedPath: "/file.bin", Size: 4, MediaType: "application/octet-stream",
			Conflict: domain.ConflictFail, State: stateValue, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}
	}

	for _, stateValue := range []storageformat.UploadState{storageformat.UploadCompleted, storageformat.UploadAborted} {
		t.Run("terminal-"+string(stateValue), func(t *testing.T) {
			backend := objectmemory.New()
			engine := newSchema008MigrationTestEngine(t, backend)
			upload := validUpload(stateValue)
			source := storageformat.OperationKey(owner.String(), upload.UploadID)
			putEnvelope(t, backend, source, uploadRecordSchema, upload)
			if err := engine.stageSchema007Uploads008(ctx); err != nil {
				t.Fatal(err)
			}
			if err := engine.stageSchema007Uploads008(ctx); err != nil {
				t.Fatalf("restart staging = %v", err)
			}
			stageKey := storageformat.Schema008MigrationStageKey(schema008DomainIdentity(namespaceReference(owner)), source.String())
			object, err := backend.Get(ctx, stageKey)
			if err != nil {
				t.Fatal(err)
			}
			var stage schema008MigrationStage
			if err := decodeCanonicalValue(object.Body, &stage); err != nil {
				t.Fatal(err)
			}
			var migrated storageformat.PortableUploadRecord
			if err := decodeCanonicalValue(stage.Value, &migrated); err != nil || migrated.State != stateValue || migrated.OwnerID != owner.String() || migrated.BlobID != upload.UploadID {
				t.Fatalf("migrated upload = %+v, %v", migrated, err)
			}
		})
	}

	for name, mutate := range map[string]func(*storageformat.UploadRecord, *objectstore.Key){
		"active": func(upload *storageformat.UploadRecord, _ *objectstore.Key) {
			upload.State = storageformat.UploadActive
		},
		"completion": func(upload *storageformat.UploadRecord, _ *objectstore.Key) { upload.CompletionOperationID = "" },
		"portable":   func(upload *storageformat.UploadRecord, _ *objectstore.Key) { upload.Area = "archive" },
		"key-binding": func(upload *storageformat.UploadRecord, key *objectstore.Key) {
			*key = storageformat.OperationKey(owner.String(), "other")
		},
	} {
		t.Run("invalid-upload-"+name, func(t *testing.T) {
			backend := objectmemory.New()
			engine := newSchema008MigrationTestEngine(t, backend)
			upload := validUpload(storageformat.UploadCompleted)
			key := storageformat.OperationKey(owner.String(), upload.UploadID)
			mutate(&upload, &key)
			putEnvelope(t, backend, key, uploadRecordSchema, upload)
			if err := engine.stageSchema007Uploads008(ctx); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("migration error = %v", err)
			}
		})
	}

	t.Run("upload-idempotency", func(t *testing.T) {
		backend := objectmemory.New()
		engine := newSchema008MigrationTestEngine(t, backend)
		originalKey := "client-key"
		record := storageformat.IdempotencyRecord{SchemaVersion: 1, UserID: owner.String(), Kind: "upload", KeyDigest: storageformat.Digest([]byte(originalKey)), Fingerprint: storageformat.Digest([]byte("fingerprint")), OperationID: "upload"}
		upload := validUpload(storageformat.UploadCompleted)
		putEnvelope(t, backend, storageformat.OperationKey(owner.String(), upload.UploadID), uploadRecordSchema, upload)
		if err := engine.stageSchema007Uploads008(ctx); err != nil {
			t.Fatal(err)
		}
		source := storageformat.IdempotencyKey(owner.String(), originalKey)
		putEnvelope(t, backend, source, idempotencySchema, record)
		if err := engine.stageSchema007UploadIdempotency008(ctx); err != nil {
			t.Fatal(err)
		}
		if err := engine.stageSchema007UploadIdempotency008(ctx); err != nil {
			t.Fatalf("restart staging = %v", err)
		}
	})

	t.Run("idempotency-missing-upload", func(t *testing.T) {
		backend := objectmemory.New()
		engine := newSchema008MigrationTestEngine(t, backend)
		originalKey := "orphan-key"
		record := storageformat.IdempotencyRecord{SchemaVersion: 1, UserID: owner.String(), Kind: "upload", KeyDigest: storageformat.Digest([]byte(originalKey)), Fingerprint: storageformat.Digest([]byte("fingerprint")), OperationID: "missing"}
		putEnvelope(t, backend, storageformat.IdempotencyKey(owner.String(), originalKey), idempotencySchema, record)
		if err := engine.stageSchema007UploadIdempotency008(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("orphan idempotency error = %v", err)
		}
	})

	t.Run("idempotency-unsupported-kind", func(t *testing.T) {
		backend := objectmemory.New()
		engine := newSchema008MigrationTestEngine(t, backend)
		originalKey := "unsupported-key"
		record := storageformat.IdempotencyRecord{SchemaVersion: 1, UserID: owner.String(), Kind: "unknown", KeyDigest: storageformat.Digest([]byte(originalKey)), Fingerprint: storageformat.Digest([]byte("fingerprint")), OperationID: "operation"}
		putEnvelope(t, backend, storageformat.IdempotencyKey(owner.String(), originalKey), idempotencySchema, record)
		if err := engine.stageSchema007UploadIdempotency008(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("unsupported idempotency error = %v", err)
		}
	})

	t.Run("idempotency-corrupt-upload-stage", func(t *testing.T) {
		backend := objectmemory.New()
		engine := newSchema008MigrationTestEngine(t, backend)
		upload := validUpload(storageformat.UploadCompleted)
		uploadSource := storageformat.OperationKey(owner.String(), upload.UploadID)
		putEnvelope(t, backend, uploadSource, uploadRecordSchema, upload)
		if err := engine.stageSchema007Uploads008(ctx); err != nil {
			t.Fatal(err)
		}
		uploadStageKey := storageformat.Schema008MigrationStageKey(schema008DomainIdentity(namespaceReference(owner)), uploadSource.String())
		replaceObjectBody(t, backend, uploadStageKey, []byte("not-json"))
		originalKey := "corrupt-target"
		record := storageformat.IdempotencyRecord{SchemaVersion: 1, UserID: owner.String(), Kind: "upload", KeyDigest: storageformat.Digest([]byte(originalKey)), Fingerprint: storageformat.Digest([]byte("fingerprint")), OperationID: upload.UploadID}
		putEnvelope(t, backend, storageformat.IdempotencyKey(owner.String(), originalKey), idempotencySchema, record)
		if err := engine.stageSchema007UploadIdempotency008(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("corrupt upload target error = %v", err)
		}
	})

	for name, mutate := range map[string]func(*schema008MigrationStage){
		"domain":         func(stage *schema008MigrationStage) { stage.DomainID = "different-owner" },
		"tree":           func(stage *schema008MigrationStage) { stage.Tree = "outcomes" },
		"migration-only": func(stage *schema008MigrationStage) { stage.MigrationOnly = true },
	} {
		t.Run("idempotency-upload-stage-"+name, func(t *testing.T) {
			backend := objectmemory.New()
			engine := newSchema008MigrationTestEngine(t, backend)
			upload := validUpload(storageformat.UploadCompleted)
			uploadSource := storageformat.OperationKey(owner.String(), upload.UploadID)
			putEnvelope(t, backend, uploadSource, uploadRecordSchema, upload)
			if err := engine.stageSchema007Uploads008(ctx); err != nil {
				t.Fatal(err)
			}
			uploadStageKey := storageformat.Schema008MigrationStageKey(schema008DomainIdentity(uploadDomainReference(owner)), uploadSource.String())
			object, err := backend.Get(ctx, uploadStageKey)
			if err != nil {
				t.Fatal(err)
			}
			var stage schema008MigrationStage
			if err := decodeCanonicalValue(object.Body, &stage); err != nil {
				t.Fatal(err)
			}
			mutate(&stage)
			body, err := storageformat.EncodeCanonical(stage)
			if err != nil {
				t.Fatal(err)
			}
			replaceObjectBody(t, backend, uploadStageKey, body)
			originalKey := "misbound-target-" + name
			record := storageformat.IdempotencyRecord{SchemaVersion: 1, UserID: owner.String(), Kind: "upload", KeyDigest: storageformat.Digest([]byte(originalKey)), Fingerprint: storageformat.Digest([]byte("fingerprint")), OperationID: upload.UploadID}
			putEnvelope(t, backend, storageformat.IdempotencyKey(owner.String(), originalKey), idempotencySchema, record)
			if err := engine.stageSchema007UploadIdempotency008(ctx); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("misbound upload target error = %v", err)
			}
		})
	}

	t.Run("idempotency-key-body-mismatch", func(t *testing.T) {
		backend := objectmemory.New()
		engine := newSchema008MigrationTestEngine(t, backend)
		record := storageformat.IdempotencyRecord{SchemaVersion: 1, UserID: owner.String(), Kind: "upload", KeyDigest: storageformat.Digest([]byte("body-key")), Fingerprint: storageformat.Digest([]byte("fingerprint")), OperationID: "upload"}
		putEnvelope(t, backend, storageformat.IdempotencyKey(owner.String(), "object-key"), idempotencySchema, record)
		if err := engine.stageSchema007UploadIdempotency008(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("misbound idempotency error = %v", err)
		}
	})

	t.Run("idempotency-invalid-schema", func(t *testing.T) {
		backend := objectmemory.New()
		engine := newSchema008MigrationTestEngine(t, backend)
		originalKey := "invalid-schema"
		record := storageformat.IdempotencyRecord{UserID: owner.String(), Kind: "upload", KeyDigest: storageformat.Digest([]byte(originalKey)), Fingerprint: storageformat.Digest([]byte("fingerprint")), OperationID: "upload"}
		key := storageformat.IdempotencyKey(owner.String(), originalKey)
		putEnvelope(t, backend, key, idempotencySchema, record)
		if err := engine.stageSchema007UploadIdempotency008(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid idempotency schema error = %v", err)
		}
	})
}

func TestSchema008MigrationStagesOnlyBoundTerminalOperationOutcomes(t *testing.T) {
	ctx := context.Background()
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	now := time.Date(2060, 1, 2, 3, 4, 5, 0, time.UTC)
	putOperation := func(t *testing.T, backend objectstore.Backend, key objectstore.Key, operation storageformat.FileOperation) {
		t.Helper()
		body, err := storageformat.EncodeEnvelope(fileOperationSchema, key, 1, operation)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}
	valid := func(kind string, stateValue storageformat.FileOperationState) storageformat.FileOperation {
		return storageformat.FileOperation{
			SchemaVersion: 1, OperationID: "operation", UserID: owner.String(), Kind: kind, State: stateValue,
			Attempt: 1, Fence: 1, ReplicaAttemptID: "replica", ExpiresAt: now.Add(time.Hour), StartedAt: now, UpdatedAt: now,
			IntentFingerprint: storageformat.Digest([]byte("intent")), Roots: []storageformat.FileOperationRoot{{Key: "legacy/root", FinalBody: []byte("final")}},
		}
	}

	for _, kind := range []string{operationCopy, operationMove, operationDelete, operationCreateDirectory, "upload-complete"} {
		t.Run("terminal-"+kind, func(t *testing.T) {
			backend := objectmemory.New()
			engine := newSchema008MigrationTestEngine(t, backend)
			operation := valid(kind, storageformat.FileOperationSucceeded)
			key := storageformat.OperationKey(owner.String(), operation.OperationID)
			putOperation(t, backend, key, operation)
			if err := engine.stageSchema007Operations008(ctx); err != nil {
				t.Fatal(err)
			}
			if err := engine.stageSchema007Operations008(ctx); err != nil {
				t.Fatalf("restart staging = %v", err)
			}
		})
	}

	for name, mutate := range map[string]func(*storageformat.FileOperation, *objectstore.Key){
		"running": func(operation *storageformat.FileOperation, _ *objectstore.Key) {
			operation.State = storageformat.FileOperationRunning
		},
		"unknown-kind": func(operation *storageformat.FileOperation, _ *objectstore.Key) { operation.Kind = "unknown" },
		"key-binding": func(_ *storageformat.FileOperation, key *objectstore.Key) {
			*key = storageformat.OperationKey(owner.String(), "other")
		},
		"owner": func(operation *storageformat.FileOperation, key *objectstore.Key) {
			operation.UserID = "invalid"
			*key = storageformat.OperationKey(operation.UserID, operation.OperationID)
		},
	} {
		t.Run("invalid-"+name, func(t *testing.T) {
			backend := objectmemory.New()
			engine := newSchema008MigrationTestEngine(t, backend)
			operation := valid(operationCopy, storageformat.FileOperationSucceeded)
			key := storageformat.OperationKey(owner.String(), operation.OperationID)
			mutate(&operation, &key)
			putOperation(t, backend, key, operation)
			if err := engine.stageSchema007Operations008(ctx); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("migration error = %v", err)
			}
		})
	}

	t.Run("file-operation-idempotency", func(t *testing.T) {
		backend := objectmemory.New()
		engine := newSchema008MigrationTestEngine(t, backend)
		operation := valid(operationMove, storageformat.FileOperationSucceeded)
		putOperation(t, backend, storageformat.OperationKey(owner.String(), operation.OperationID), operation)
		originalKey := "move-key"
		binding := storageformat.IdempotencyRecord{SchemaVersion: 1, UserID: owner.String(), Kind: operation.Kind, KeyDigest: storageformat.Digest([]byte(originalKey)), Fingerprint: operation.IntentFingerprint, OperationID: operation.OperationID}
		key := storageformat.IdempotencyKey(owner.String(), originalKey)
		body, err := storageformat.EncodeEnvelope(idempotencySchema, key, 1, binding)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := engine.stageSchema007UploadIdempotency008(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("file-operation-idempotency-binding", func(t *testing.T) {
		backend := objectmemory.New()
		engine := newSchema008MigrationTestEngine(t, backend)
		operation := valid(operationCopy, storageformat.FileOperationSucceeded)
		putOperation(t, backend, storageformat.OperationKey(owner.String(), operation.OperationID), operation)
		originalKey := "copy-key"
		binding := storageformat.IdempotencyRecord{SchemaVersion: 1, UserID: owner.String(), Kind: operation.Kind, KeyDigest: storageformat.Digest([]byte(originalKey)), Fingerprint: storageformat.Digest([]byte("different")), OperationID: operation.OperationID}
		key := storageformat.IdempotencyKey(owner.String(), originalKey)
		body, _ := storageformat.EncodeEnvelope(idempotencySchema, key, 1, binding)
		if _, err := backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := engine.stageSchema007UploadIdempotency008(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("misbound operation idempotency error = %v", err)
		}
	})

	t.Run("file-operation-idempotency-target-missing", func(t *testing.T) {
		backend := objectmemory.New()
		engine := newSchema008MigrationTestEngine(t, backend)
		originalKey := "orphan-copy"
		binding := storageformat.IdempotencyRecord{SchemaVersion: 1, UserID: owner.String(), Kind: operationCopy, KeyDigest: storageformat.Digest([]byte(originalKey)), Fingerprint: storageformat.Digest([]byte("fingerprint")), OperationID: "missing"}
		key := storageformat.IdempotencyKey(owner.String(), originalKey)
		body, _ := storageformat.EncodeEnvelope(idempotencySchema, key, 1, binding)
		if _, err := backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := engine.stageSchema007UploadIdempotency008(ctx); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing operation target error = %v", err)
		}
	})
}

func TestSchema008MigrationRejectsMalformedLegacyOperationObjects(t *testing.T) {
	ctx := context.Background()
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	for name, stage := range map[string]func(*Engine) error{
		"upload":    func(engine *Engine) error { return engine.stageSchema007Uploads008(ctx) },
		"operation": func(engine *Engine) error { return engine.stageSchema007Operations008(ctx) },
	} {
		t.Run(name, func(t *testing.T) {
			backend := objectmemory.New()
			engine := newSchema008MigrationTestEngine(t, backend)
			key := storageformat.OperationKey(owner.String(), name)
			if _, err := backend.Put(ctx, key, []byte("not-json"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
				t.Fatal(err)
			}
			if err := stage(engine); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("malformed operation error = %v", err)
			}
		})
	}
	backend := objectmemory.New()
	engine := newSchema008MigrationTestEngine(t, backend)
	key := storageformat.IdempotencyKey(owner.String(), "malformed")
	if _, err := backend.Put(ctx, key, []byte("not-json"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if err := engine.stageSchema007UploadIdempotency008(ctx); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("malformed idempotency error = %v", err)
	}
}

func TestSchema008MigrationInstallStagesAreIdempotentAndFailClosed(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	engine := newSchema008MigrationTestEngine(t, backend)
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:install"}
	stages := []schema008MigrationStage{
		validSchema008MigrationStage(reference, "source/base"),
		validSchema008MigrationStage(reference, "source/outcome"),
		validSchema008MigrationStage(reference, "source/expiry"),
		validSchema008MigrationStage(reference, "source/migration-only"),
	}
	stages[1].Tree, stages[1].Key = "outcomes", "outcome"
	stages[2].Tree, stages[2].Key = "outcome-expiry", "expiry"
	stages[3].MigrationOnly = true
	for _, stage := range stages {
		if _, err := engine.writeSchema008MigrationStage(ctx, stage); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.installSchema008StagedDomains(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine.installSchema008StagedDomains(ctx); err != nil {
		t.Fatalf("idempotent install = %v", err)
	}
	head, err := engine.stateDomainStore().loadHead(ctx, reference)
	if err != nil || head.head.Revision != 1 || head.head.Base.EntryCount != 1 || head.head.Outcomes.EntryCount != 1 || head.head.OutcomeExpiry.EntryCount != 1 {
		t.Fatalf("installed head = %+v, %v", head.head, err)
	}

	t.Run("empty", func(t *testing.T) {
		empty := newSchema008MigrationTestEngine(t, objectmemory.New())
		if err := empty.installSchema008StagedDomains(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("malformed-stage", func(t *testing.T) {
		base := objectmemory.New()
		candidate := newSchema008MigrationTestEngine(t, base)
		key := storageformat.Schema008MigrationStageKey(schema008DomainIdentity(reference), "malformed")
		if _, err := base.Put(ctx, key, []byte("not-json"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := candidate.installSchema008StagedDomains(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("malformed install stage error = %v", err)
		}
	})

	t.Run("stage-key-binding", func(t *testing.T) {
		base := objectmemory.New()
		candidate := newSchema008MigrationTestEngine(t, base)
		stage := validSchema008MigrationStage(reference, "actual-source")
		body, _ := storageformat.EncodeCanonical(stage)
		wrongKey := storageformat.Schema008MigrationStageKey(schema008DomainIdentity(reference), "other-source")
		if _, err := base.Put(ctx, wrongKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := candidate.installSchema008StagedDomains(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("misbound install stage error = %v", err)
		}
	})

	t.Run("already-mutated-domain", func(t *testing.T) {
		base := objectmemory.New()
		candidate := newSchema008MigrationTestEngine(t, base)
		store := candidate.stateDomainStore()
		if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: "existing", Changes: []consistencyDomainChange{{Key: "existing", Require: domainValueAbsent, Value: []byte("existing")}}}); err != nil {
			t.Fatal(err)
		}
		if err := candidate.installSchema008Domain(ctx, reference, storageformat.DomainTreeRoot{}, storageformat.DomainTreeRoot{}, storageformat.DomainTreeRoot{}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("mutated domain install error = %v", err)
		}
	})

	t.Run("invalid-domain-root", func(t *testing.T) {
		candidate := newSchema008MigrationTestEngine(t, objectmemory.New())
		invalidRoot := storageformat.DomainTreeRoot{Digest: "not-a-canonical-root"}
		if err := candidate.installSchema008Domain(ctx, reference, invalidRoot, storageformat.DomainTreeRoot{}, storageformat.DomainTreeRoot{}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid installed root error = %v", err)
		}
	})

	t.Run("multiple-domains-and-migration-only-group", func(t *testing.T) {
		base := objectmemory.New()
		candidate := newSchema008MigrationTestEngine(t, base)
		second := consistencyDomainRef{Kind: storageformat.DomainAdmin, ID: "administration"}
		third := consistencyDomainRef{Kind: storageformat.DomainShare, ID: "share:migration-only"}
		secondStage := validSchema008MigrationStage(second, "second/source")
		thirdStage := validSchema008MigrationStage(third, "third/source")
		thirdStage.MigrationOnly = true
		if _, err := candidate.writeSchema008MigrationStage(ctx, secondStage); err != nil {
			t.Fatal(err)
		}
		if _, err := candidate.writeSchema008MigrationStage(ctx, thirdStage); err != nil {
			t.Fatal(err)
		}
		if err := candidate.installSchema008StagedDomains(ctx); err != nil {
			t.Fatal(err)
		}
		installed, err := candidate.stateDomainStore().loadHead(ctx, second)
		if err != nil || installed.head.Revision != 1 || installed.head.Base.EntryCount != 1 {
			t.Fatalf("second installed domain = %+v, %v", installed.head, err)
		}
		if _, err := base.Head(ctx, storageformat.DomainHeadKey(third.Kind, third.ID)); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("migration-only domain head = %v", err)
		}
	})

	t.Run("provider-list-failure", func(t *testing.T) {
		base := objectmemory.New()
		hooks := &hookedBackend{Backend: base, list: func(context.Context, objectstore.ListRequest) (objectstore.ListPage, error) {
			return objectstore.ListPage{}, domain.NewError(domain.ErrorUnavailable, "list denied")
		}}
		candidate := newSchema008MigrationTestEngine(t, hooks)
		if err := candidate.installSchema008StagedDomains(ctx); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("list failure = %v", err)
		}
	})

	t.Run("provider-stage-read-failure", func(t *testing.T) {
		base := objectmemory.New()
		writer := newSchema008MigrationTestEngine(t, base)
		stage := validSchema008MigrationStage(reference, "read-failure")
		key, err := writer.writeSchema008MigrationStage(ctx, stage)
		if err != nil {
			t.Fatal(err)
		}
		hooks := &hookedBackend{Backend: base, get: func(ctx context.Context, candidate objectstore.Key) (objectstore.Object, error) {
			if candidate == key {
				return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "get denied")
			}
			return base.Get(ctx, candidate)
		}}
		candidate := newSchema008MigrationTestEngine(t, hooks)
		if err := candidate.installSchema008StagedDomains(ctx); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("stage read failure = %v", err)
		}
	})

	t.Run("tree-publication-failure", func(t *testing.T) {
		base := objectmemory.New()
		writer := newSchema008MigrationTestEngine(t, base)
		for index := 0; index < domainPageMaximumItems; index++ {
			stage := validSchema008MigrationStage(reference, "tree-failure/"+storageformat.Digest([]byte{byte(index), byte(index >> 8)}))
			stage.Key = storageformat.Digest([]byte("key/" + stage.SourceKey))
			if _, err := writer.writeSchema008MigrationStage(ctx, stage); err != nil {
				t.Fatal(err)
			}
		}
		hooks := &hookedBackend{Backend: base, put: func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if strings.Contains(key.String(), "/domains/") && strings.Contains(key.String(), "/pages/") {
				return "", domain.NewError(domain.ErrorUnavailable, "domain page write denied")
			}
			return base.Put(ctx, key, body, condition)
		}}
		candidate := newSchema008MigrationTestEngine(t, hooks)
		if err := candidate.installSchema008StagedDomains(ctx); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("tree publication failure = %v", err)
		}
	})
}

func TestSchema008MigrationRunAccumulatorMergesBoundedChunks(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:migration-runs"}
	runs := newSchema008MigrationRuns(ctx, newConsistencyDomainTreeSession(newConsistencyDomainStore(backend, nil), reference))
	count := domainPageMaximumItems*namespaceProjectionMergeFanIn + 1
	for index := 0; index < count; index++ {
		key := strings.Repeat("0", 8-len(string(rune(index%10)))) + string(rune('a'+index%26))
		key = key + "/" + storageformat.Digest([]byte{byte(index >> 8), byte(index)})
		if err := runs.Add(storageformat.DomainEntry{Key: key, Value: []byte(key), LogicalVersion: storageformat.Digest([]byte(key))}); err != nil {
			t.Fatal(err)
		}
	}
	root, err := runs.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if root.EntryCount != uint64(count) {
		t.Fatalf("merged migration run count = %d", root.EntryCount)
	}

	t.Run("merge-failure", func(t *testing.T) {
		candidateBackend := objectmemory.New()
		candidateSession := newConsistencyDomainTreeSession(newConsistencyDomainStore(candidateBackend, nil), reference)
		candidateRoot, err := candidateSession.buildTree(ctx, []storageformat.DomainEntry{{Key: "same", Value: []byte("value"), LogicalVersion: "version"}})
		if err != nil {
			t.Fatal(err)
		}
		candidateRuns := newSchema008MigrationRuns(ctx, candidateSession)
		candidateRuns.levels = [][]storageformat.DomainTreeRoot{make([]storageformat.DomainTreeRoot, namespaceProjectionMergeFanIn-1)}
		for index := range candidateRuns.levels[0] {
			candidateRuns.levels[0][index] = candidateRoot
		}
		if err := candidateRuns.addRun(0, candidateRoot); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("overlapping run merge error = %v", err)
		}
	})
}

func TestSchema008MigrationVisitsLegacyDirectoryPagesWithExactBinding(t *testing.T) {
	ctx := context.Background()
	scope := namespaceTestScope(t, domain.AreaLive)
	directoryID := "legacy-directory"
	pageID := "page-1"
	pageKey := storageformat.DirectoryPageKey(scope.UserID().String(), areaName(scope.Area()), directoryID, pageID)
	entry := storageformat.DirectoryEntry{Name: "file.bin", Kind: domain.EntryFile, BlobID: "blob", LogicalVersion: "version"}
	manifest := storageformat.DirectoryManifest{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: "manifest", PageIDs: []string{pageID}, EntryCount: 1}

	newFixture := func(t *testing.T, page storageformat.DirectoryPage) (*objectmemory.Backend, *Engine) {
		t.Helper()
		backend := objectmemory.New()
		engine := newSchema008MigrationTestEngine(t, backend)
		body, err := storageformat.EncodeEnvelope(directoryPageSchema, pageKey, 1, page)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Put(ctx, pageKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		return backend, engine
	}

	_, engine := newFixture(t, storageformat.DirectoryPage{SchemaVersion: 1, DirectoryID: directoryID, PageID: pageID, Entries: []storageformat.DirectoryEntry{entry}})
	visited := 0
	if err := engine.visitSchema007DirectoryEntries(ctx, scope, directoryID, manifest, func(value storageformat.DirectoryEntry) error {
		visited++
		if value.Name != entry.Name {
			t.Fatalf("entry = %+v", value)
		}
		return nil
	}); err != nil || visited != 1 {
		t.Fatalf("legacy page visit count=%d error=%v", visited, err)
	}
	callbackErr := domain.NewError(domain.ErrorUnavailable, "stop")
	if err := engine.visitSchema007DirectoryEntries(ctx, scope, directoryID, manifest, func(storageformat.DirectoryEntry) error { return callbackErr }); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("callback error = %v", err)
	}

	for name, mutate := range map[string]func(*storageformat.DirectoryPage, *storageformat.DirectoryManifest){
		"page-schema": func(page *storageformat.DirectoryPage, _ *storageformat.DirectoryManifest) { page.SchemaVersion = 0 },
		"directory-binding": func(page *storageformat.DirectoryPage, _ *storageformat.DirectoryManifest) {
			page.DirectoryID = "other"
		},
		"page-binding":   func(page *storageformat.DirectoryPage, _ *storageformat.DirectoryManifest) { page.PageID = "other" },
		"manifest-count": func(_ *storageformat.DirectoryPage, value *storageformat.DirectoryManifest) { value.EntryCount = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			page := storageformat.DirectoryPage{SchemaVersion: 1, DirectoryID: directoryID, PageID: pageID, Entries: []storageformat.DirectoryEntry{entry}}
			candidateManifest := manifest
			mutate(&page, &candidateManifest)
			_, candidate := newFixture(t, page)
			if err := candidate.visitSchema007DirectoryEntries(ctx, scope, directoryID, candidateManifest, func(storageformat.DirectoryEntry) error { return nil }); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("legacy page error = %v", err)
			}
		})
	}

	empty := newSchema008MigrationTestEngine(t, objectmemory.New())
	if err := empty.visitSchema007DirectoryEntries(ctx, scope, directoryID, manifest, func(storageformat.DirectoryEntry) error { return nil }); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing legacy page error = %v", err)
	}
}

func TestSchema008MigrationAuthenticatesLegacyDuplicateOccurrences(t *testing.T) {
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	groupID := storageformat.Digest([]byte("group"))
	occurrence := storageformat.DuplicateOccurrence{GroupID: groupID, Kind: domain.DuplicateFile, Area: "live", Path: "/file.bin", Version: "version", Size: 4, FileCount: 1}
	key := storageformat.DuplicateOccurrenceKey(owner.String(), string(occurrence.Kind), occurrence.GroupID, occurrence.Area, occurrence.Path)
	root := storageformat.DuplicateOccurrenceRoot{SchemaVersion: 1, UserID: owner.String(), Current: &occurrence}
	encode := func(t *testing.T, key objectstore.Key, value storageformat.DuplicateOccurrenceRoot) []byte {
		t.Helper()
		body, err := storageformat.EncodeEnvelope(duplicateOccurrenceSchema, key, 1, value)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	decoded, err := decodeSchema007OperationOccurrence008(key.String(), encode(t, key, root), owner)
	if err != nil || decoded == nil || decoded.Path != occurrence.Path {
		t.Fatalf("decoded occurrence = %+v, %v", decoded, err)
	}
	empty := root
	empty.Current = nil
	if decoded, err := decodeSchema007OperationOccurrence008(key.String(), encode(t, key, empty), owner); err != nil || decoded != nil {
		t.Fatalf("empty occurrence = %+v, %v", decoded, err)
	}
	if _, err := decodeSchema007OperationOccurrence008("INVALID", nil, owner); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid key error = %v", err)
	}
	if _, err := decodeSchema007OperationOccurrence008(key.String(), []byte("not-json"), owner); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid body error = %v", err)
	}
	for name, mutate := range map[string]func(*storageformat.DuplicateOccurrenceRoot){
		"schema": func(value *storageformat.DuplicateOccurrenceRoot) { value.SchemaVersion = 0 },
		"owner":  func(value *storageformat.DuplicateOccurrenceRoot) { value.UserID = "other" },
		"pending": func(value *storageformat.DuplicateOccurrenceRoot) {
			value.Pending = &storageformat.DuplicateOccurrenceTransition{}
		},
		"binding": func(value *storageformat.DuplicateOccurrenceRoot) { value.Current.Path = "/other.bin" },
	} {
		t.Run(name, func(t *testing.T) {
			value := root
			copyOccurrence := occurrence
			value.Current = &copyOccurrence
			mutate(&value)
			if _, err := decodeSchema007OperationOccurrence008(key.String(), encode(t, key, value), owner); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("occurrence error = %v", err)
			}
		})
	}
}

func TestSchema008MigrationAuthenticatesExplicitAndRecoveredTrashMetadata(t *testing.T) {
	ctx := context.Background()
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	trashID := "trashed"
	logical := state.MustKey(state.NamespaceTrash, owner.String(), trashID)
	reference, err := stateDomainReferenceForKey(logical)
	if err != nil {
		t.Fatal(err)
	}
	record := schema007TrashRecord{
		SchemaVersion: 1, TrashID: trashID, OwnerUserID: owner, OriginalPath: domain.MustParseUserPath("/original"),
		TrashedPath: domain.MustParseUserPath("/" + trashID), Kind: domain.EntryDirectory,
		TrashedAt: time.Date(2060, 1, 2, 3, 4, 5, 0, time.UTC), OriginalVersion: "original-version",
	}
	body, err := storageformat.EncodeCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	putStage := func(t *testing.T, source string, mutate func(*schema008MigrationStage)) (*Engine, objectstore.Key) {
		t.Helper()
		backend := objectmemory.New()
		engine := newSchema008MigrationTestEngine(t, backend)
		stage := schema008MigrationStage{SchemaVersion: 1, SourceKey: source, DomainKind: reference.Kind, DomainID: reference.ID, Key: logical.String(), Value: body, LogicalVersion: "version", MigrationOnly: true}
		if mutate != nil {
			mutate(&stage)
		}
		stageBody, err := storageformat.EncodeCanonical(stage)
		if err != nil {
			t.Fatal(err)
		}
		stageKey := storageformat.Schema008MigrationStageKey(schema008DomainIdentity(reference), source)
		if _, err := backend.Put(ctx, stageKey, stageBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		return engine, stageKey
	}

	explicitSource := canonicalStateKey(logical).String()
	engine, stageKey := putStage(t, explicitSource, nil)
	metadata, err := engine.readSchema007TrashMetadataStage008(ctx, stageKey, explicitSource, logical, owner, trashID, domain.EntryDirectory)
	if err != nil || metadata == nil || metadata.OriginalPath != record.OriginalPath.String() {
		t.Fatalf("explicit metadata = %+v, %v", metadata, err)
	}
	if metadata, err := engine.schema007ExplicitTrashMetadata008(ctx, owner, trashID, domain.EntryDirectory); err != nil || metadata == nil {
		t.Fatalf("explicit lookup = %+v, %v", metadata, err)
	}
	if metadata, err := engine.schema007TrashMetadata008(ctx, owner, trashID, domain.EntryDirectory); err != nil || metadata == nil {
		t.Fatalf("preferred explicit lookup = %+v, %v", metadata, err)
	}

	recoveredSource := schema007RecoveredTrashSource(owner, trashID)
	engine, _ = putStage(t, recoveredSource, nil)
	if metadata, err := engine.schema007TrashMetadata008(ctx, owner, trashID, domain.EntryDirectory); err != nil || metadata == nil {
		t.Fatalf("recovered lookup = %+v, %v", metadata, err)
	}

	emptyEngine := newSchema008MigrationTestEngine(t, objectmemory.New())
	if metadata, err := emptyEngine.readSchema007TrashMetadataStage008(ctx, stageKey, explicitSource, logical, owner, trashID, domain.EntryDirectory); err != nil || metadata != nil {
		t.Fatalf("missing metadata = %+v, %v", metadata, err)
	}
	for name, mutate := range map[string]func(*schema008MigrationStage){
		"migration-only": func(stage *schema008MigrationStage) { stage.MigrationOnly = false },
		"source":         func(stage *schema008MigrationStage) { stage.SourceKey = "other" },
		"key":            func(stage *schema008MigrationStage) { stage.Key = "other" },
		"record":         func(stage *schema008MigrationStage) { stage.Value = []byte("not-json") },
	} {
		t.Run(name, func(t *testing.T) {
			candidate, candidateKey := putStage(t, explicitSource, mutate)
			if _, err := candidate.readSchema007TrashMetadataStage008(ctx, candidateKey, explicitSource, logical, owner, trashID, domain.EntryDirectory); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("trash metadata error = %v", err)
			}
		})
	}
}

func TestSchema008MigrationRecoversHistoricalTrashMetadataWithoutObjectRelocation(t *testing.T) {
	ctx := context.Background()
	trash := namespaceTestScope(t, domain.AreaTrash)
	owner := trash.UserID()
	now := time.Date(2060, 1, 2, 3, 4, 5, 0, time.UTC)
	trashID, manifestID, pageID := "historical-trash", "manifest", "page"
	entry := storageformat.DirectoryEntry{
		Name: trashID, NameDigest: storageformat.NameDigest(trashID), Kind: domain.EntryFile, BlobID: "blob", Size: 4,
		MediaType: "application/octet-stream", MD5: testProviderMD5, CRC32C: testProviderCRC32C, ModifiedAt: now,
	}
	entry.LogicalVersion, _ = directoryEntryVersion(entry)
	newFixture := func(t *testing.T, entries []storageformat.DirectoryEntry) (*objectmemory.Backend, *Engine) {
		t.Helper()
		backend := objectmemory.New()
		pageKey := storageformat.DirectoryPageKey(owner.String(), areaName(trash.Area()), storageformat.RootDirectoryID, pageID)
		migrationPut(t, backend, pageKey, migrationEnvelope(t, directoryPageSchema, pageKey, storageformat.DirectoryPage{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, PageID: pageID, Entries: entries}))
		putMigrationRoot(t, backend, trash, storageformat.RootDirectoryID, manifestID, storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, ManifestID: manifestID, RecursiveBytes: int64(len(entries)) * 4, RecursiveFileCount: int64(len(entries)), ContentAccumulator: "accumulator", ContentDigest: storageformat.Digest([]byte("content"))})
		putMigrationManifest(t, backend, trash, storageformat.RootDirectoryID, manifestID, storageformat.DirectoryManifest{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, ManifestID: manifestID, PageIDs: []string{pageID}, EntryCount: len(entries), RecursiveBytes: int64(len(entries)) * 4, RecursiveFileCount: int64(len(entries)), ContentAccumulator: "accumulator", ContentDigest: storageformat.Digest([]byte("content")), CreatedAt: now})
		return backend, newSchema008MigrationTestEngine(t, backend)
	}

	backend, engine := newFixture(t, []storageformat.DirectoryEntry{entry})
	if err := engine.stageSchema007RecoveredTrashMetadata008(ctx); err != nil {
		t.Fatal(err)
	}
	source := schema007RecoveredTrashSource(owner, trashID)
	logical := state.MustKey(state.NamespaceTrash, owner.String(), trashID)
	reference, _ := stateDomainReferenceForKey(logical)
	stageKey := storageformat.Schema008MigrationStageKey(schema008DomainIdentity(reference), source)
	if _, err := backend.Get(ctx, stageKey); err != nil {
		t.Fatalf("recovered metadata stage = %v", err)
	}
	metadata, err := engine.schema007TrashMetadata008(ctx, owner, trashID, domain.EntryFile)
	if err != nil || metadata == nil || metadata.OriginalPath != "/"+trashID || metadata.OriginalVersion != domain.Version(entry.LogicalVersion) || !metadata.TrashedAt.Equal(now) {
		t.Fatalf("salvaged metadata = %+v, %v", metadata, err)
	}

	t.Run("authoritative-move-occurrences", func(t *testing.T) {
		candidateBackend, candidateEngine := newFixture(t, []storageformat.DirectoryEntry{entry})
		groupID := storageformat.Digest([]byte("trash-group"))
		live := storageformat.DuplicateOccurrence{GroupID: groupID, Kind: domain.DuplicateFile, Area: "live", Path: "/original/file.bin", Version: "original-version", Size: entry.Size, FileCount: 1}
		deeperLive := live
		deeperLive.Path = "/backup/original/file.bin"
		wrongGroup := live
		wrongGroup.GroupID = storageformat.Digest([]byte("wrong-group"))
		trashed := storageformat.DuplicateOccurrence{GroupID: groupID, Kind: domain.DuplicateFile, Area: "trash", Path: "/" + trashID, Version: entry.LogicalVersion, Size: entry.Size, FileCount: 1}
		nestedTrash := trashed
		nestedTrash.Path = "/nested/" + trashID
		missingTrash := trashed
		missingTrash.Path = "/not-present"
		rootFor := func(occurrence storageformat.DuplicateOccurrence, rollback bool) storageformat.FileOperationRoot {
			key := storageformat.DuplicateOccurrenceKey(owner.String(), string(occurrence.Kind), occurrence.GroupID, occurrence.Area, occurrence.Path)
			body := encodeInternalEnvelope(t, duplicateOccurrenceSchema, key, 1, storageformat.DuplicateOccurrenceRoot{SchemaVersion: 1, UserID: owner.String(), Current: &occurrence})
			result := storageformat.FileOperationRoot{Key: key.String()}
			if rollback {
				result.RollbackBody = body
			} else {
				result.FinalBody = body
			}
			return result
		}
		invalidOccurrenceKey := storageformat.DuplicateOccurrenceKey(owner.String(), string(domain.DuplicateFile), groupID, "live", "/invalid")
		operation := storageformat.FileOperation{
			SchemaVersion: 1, OperationID: "trash-move", UserID: owner.String(), Kind: operationMove, State: storageformat.FileOperationSucceeded,
			Attempt: 1, Fence: 1, ReplicaAttemptID: "replica", ExpiresAt: now.Add(time.Hour), StartedAt: now.Add(-time.Minute), UpdatedAt: now,
			Roots: []storageformat.FileOperationRoot{
				{Key: "unrelated/root", FinalBody: []byte("ignored")},
				{Key: invalidOccurrenceKey.String(), RollbackBody: []byte("invalid"), FinalBody: []byte("invalid")},
				rootFor(wrongGroup, true), rootFor(deeperLive, true), rootFor(live, true),
				rootFor(nestedTrash, false), rootFor(missingTrash, false), rootFor(trashed, false),
			},
		}
		operationKey := storageformat.OperationKey(owner.String(), operation.OperationID)
		migrationPut(t, candidateBackend, operationKey, migrationEnvelope(t, fileOperationSchema, operationKey, operation))
		if err := candidateEngine.stageSchema007RecoveredTrashMetadata008(ctx); err != nil {
			t.Fatal(err)
		}
		metadata, err := candidateEngine.schema007TrashMetadata008(ctx, owner, trashID, domain.EntryFile)
		if err != nil || metadata == nil || metadata.OriginalPath != live.Path || metadata.OriginalVersion != domain.Version(live.Version) || !metadata.TrashedAt.Equal(now) {
			t.Fatalf("recovered occurrence metadata = %+v, %v", metadata, err)
		}
	})

	t.Run("explicit-authority-suppresses-recovery", func(t *testing.T) {
		candidateBackend, candidateEngine := newFixture(t, []storageformat.DirectoryEntry{entry})
		logical := state.MustKey(state.NamespaceTrash, owner.String(), trashID)
		reference, err := stateDomainReferenceForKey(logical)
		if err != nil {
			t.Fatal(err)
		}
		record := schema007TrashRecord{SchemaVersion: 1, TrashID: trashID, OwnerUserID: owner, OriginalPath: domain.MustParseUserPath("/explicit"), TrashedPath: domain.MustParseUserPath("/" + trashID), Kind: domain.EntryFile, TrashedAt: now, OriginalVersion: "explicit-version"}
		body, err := storageformat.EncodeCanonical(record)
		if err != nil {
			t.Fatal(err)
		}
		source := canonicalStateKey(logical).String()
		if _, err := candidateEngine.writeSchema008MigrationStage(ctx, schema008MigrationStage{SchemaVersion: 1, SourceKey: source, DomainKind: reference.Kind, DomainID: reference.ID, Key: logical.String(), Value: body, LogicalVersion: "version", MigrationOnly: true}); err != nil {
			t.Fatal(err)
		}
		if err := candidateEngine.stageSchema007RecoveredTrashMetadata008(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := candidateBackend.Head(ctx, storageformat.Schema008MigrationStageKey(schema008DomainIdentity(reference), schema007RecoveredTrashSource(owner, trashID))); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("unexpected recovered stage error = %v", err)
		}
	})

	t.Run("malformed-operation-scan", func(t *testing.T) {
		candidateBackend, candidateEngine := newFixture(t, []storageformat.DirectoryEntry{entry})
		operationKey := storageformat.OperationKey(owner.String(), "malformed-operation")
		migrationPut(t, candidateBackend, operationKey, []byte("not-json"))
		if err := candidateEngine.stageSchema007RecoveredTrashMetadata008(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("malformed trash operation error = %v", err)
		}
	})

	t.Run("operation-page-provider-failure", func(t *testing.T) {
		candidateBackend, candidateEngine := newFixture(t, []storageformat.DirectoryEntry{entry})
		operation := storageformat.FileOperation{
			SchemaVersion: 1, OperationID: "paged-move", UserID: owner.String(), Kind: operationMove, State: storageformat.FileOperationSucceeded,
			Attempt: 1, Fence: 1, ReplicaAttemptID: "replica", ExpiresAt: now.Add(time.Hour), StartedAt: now.Add(-time.Minute), UpdatedAt: now,
			StepSetID: "step-set", StepPageCount: 1, StepDigest: storageformat.Digest([]byte("steps")),
		}
		operationKey := storageformat.OperationKey(owner.String(), operation.OperationID)
		migrationPut(t, candidateBackend, operationKey, migrationEnvelope(t, fileOperationSchema, operationKey, operation))
		stepKey := storageformat.FileOperationStepPageKey(owner.String(), operation.OperationID, operation.StepSetID, 0)
		hooks := &hookedBackend{Backend: candidateBackend, get: func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
			if key == stepKey {
				return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "step page unavailable")
			}
			return candidateBackend.Get(ctx, key)
		}}
		candidateEngine.backend = hooks
		if err := candidateEngine.stageSchema007RecoveredTrashMetadata008(ctx); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("step page provider error = %v", err)
		}
	})

	t.Run("zero-time-salvage", func(t *testing.T) {
		zeroEntry := entry
		zeroEntry.ModifiedAt = time.Time{}
		zeroEntry.LogicalVersion, _ = directoryEntryVersion(zeroEntry)
		_, candidateEngine := newFixture(t, []storageformat.DirectoryEntry{zeroEntry})
		if err := candidateEngine.stageSchema007RecoveredTrashMetadata008(ctx); err != nil {
			t.Fatal(err)
		}
		metadata, err := candidateEngine.schema007TrashMetadata008(ctx, owner, trashID, domain.EntryFile)
		if err != nil || metadata == nil || !metadata.TrashedAt.Equal(time.Unix(0, 0).UTC()) {
			t.Fatalf("zero-time salvage = %+v, %v", metadata, err)
		}
	})

	t.Run("ambiguous-live-occurrences", func(t *testing.T) {
		candidateBackend, candidateEngine := newFixture(t, []storageformat.DirectoryEntry{entry})
		groupID := storageformat.Digest([]byte("ambiguous-group"))
		occurrences := []storageformat.DuplicateOccurrence{
			{GroupID: groupID, Kind: domain.DuplicateFile, Area: "live", Path: "/source-a/file.bin", Version: "a", Size: entry.Size, FileCount: 1},
			{GroupID: groupID, Kind: domain.DuplicateFile, Area: "live", Path: "/source-b/file.bin", Version: "b", Size: entry.Size, FileCount: 1},
			{GroupID: groupID, Kind: domain.DuplicateFile, Area: "trash", Path: "/" + trashID, Version: entry.LogicalVersion, Size: entry.Size, FileCount: 1},
		}
		roots := make([]storageformat.FileOperationRoot, 0, len(occurrences))
		for index := range occurrences {
			occurrence := occurrences[index]
			key := storageformat.DuplicateOccurrenceKey(owner.String(), string(occurrence.Kind), occurrence.GroupID, occurrence.Area, occurrence.Path)
			body := encodeInternalEnvelope(t, duplicateOccurrenceSchema, key, 1, storageformat.DuplicateOccurrenceRoot{SchemaVersion: 1, UserID: owner.String(), Current: &occurrence})
			root := storageformat.FileOperationRoot{Key: key.String(), RollbackBody: body}
			if occurrence.Area == "trash" {
				root.RollbackBody, root.FinalBody = nil, body
			}
			roots = append(roots, root)
		}
		operation := storageformat.FileOperation{SchemaVersion: 1, OperationID: "ambiguous", UserID: owner.String(), Kind: operationMove, State: storageformat.FileOperationSucceeded, Attempt: 1, Fence: 1, ReplicaAttemptID: "replica", ExpiresAt: now.Add(time.Hour), StartedAt: now.Add(-time.Minute), UpdatedAt: now, Roots: roots}
		operationKey := storageformat.OperationKey(owner.String(), operation.OperationID)
		migrationPut(t, candidateBackend, operationKey, migrationEnvelope(t, fileOperationSchema, operationKey, operation))
		if err := candidateEngine.stageSchema007RecoveredTrashMetadata008(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("ambiguous live occurrence error = %v", err)
		}
	})

	t.Run("target-without-live-source-falls-back", func(t *testing.T) {
		candidateBackend, candidateEngine := newFixture(t, []storageformat.DirectoryEntry{entry})
		groupID := storageformat.Digest([]byte("target-only"))
		target := storageformat.DuplicateOccurrence{GroupID: groupID, Kind: domain.DuplicateFile, Area: "trash", Path: "/" + trashID, Version: entry.LogicalVersion, Size: entry.Size, FileCount: 1}
		key := storageformat.DuplicateOccurrenceKey(owner.String(), string(target.Kind), target.GroupID, target.Area, target.Path)
		body := encodeInternalEnvelope(t, duplicateOccurrenceSchema, key, 1, storageformat.DuplicateOccurrenceRoot{SchemaVersion: 1, UserID: owner.String(), Current: &target})
		operation := storageformat.FileOperation{SchemaVersion: 1, OperationID: "target-only", UserID: owner.String(), Kind: operationMove, State: storageformat.FileOperationSucceeded, Attempt: 1, Fence: 1, ReplicaAttemptID: "replica", ExpiresAt: now.Add(time.Hour), StartedAt: now.Add(-time.Minute), UpdatedAt: now, Roots: []storageformat.FileOperationRoot{{Key: key.String(), FinalBody: body}}}
		operationKey := storageformat.OperationKey(owner.String(), operation.OperationID)
		migrationPut(t, candidateBackend, operationKey, migrationEnvelope(t, fileOperationSchema, operationKey, operation))
		if err := candidateEngine.stageSchema007RecoveredTrashMetadata008(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("root-live-source-is-invalid", func(t *testing.T) {
		candidateBackend, candidateEngine := newFixture(t, []storageformat.DirectoryEntry{entry})
		groupID := storageformat.Digest([]byte("root-source"))
		live := storageformat.DuplicateOccurrence{GroupID: groupID, Kind: domain.DuplicateFile, Area: "live", Path: "/", Version: "root-version", Size: entry.Size, FileCount: 1}
		target := storageformat.DuplicateOccurrence{GroupID: groupID, Kind: domain.DuplicateFile, Area: "trash", Path: "/" + trashID, Version: entry.LogicalVersion, Size: entry.Size, FileCount: 1}
		roots := make([]storageformat.FileOperationRoot, 0, 2)
		for _, occurrence := range []storageformat.DuplicateOccurrence{live, target} {
			key := storageformat.DuplicateOccurrenceKey(owner.String(), string(occurrence.Kind), occurrence.GroupID, occurrence.Area, occurrence.Path)
			body := encodeInternalEnvelope(t, duplicateOccurrenceSchema, key, 1, storageformat.DuplicateOccurrenceRoot{SchemaVersion: 1, UserID: owner.String(), Current: &occurrence})
			root := storageformat.FileOperationRoot{Key: key.String(), RollbackBody: body}
			if occurrence.Area == "trash" {
				root.RollbackBody, root.FinalBody = nil, body
			}
			roots = append(roots, root)
		}
		operation := storageformat.FileOperation{SchemaVersion: 1, OperationID: "root-source", UserID: owner.String(), Kind: operationMove, State: storageformat.FileOperationSucceeded, Attempt: 1, Fence: 1, ReplicaAttemptID: "replica", ExpiresAt: now.Add(time.Hour), StartedAt: now.Add(-time.Minute), UpdatedAt: now, Roots: roots}
		operationKey := storageformat.OperationKey(owner.String(), operation.OperationID)
		migrationPut(t, candidateBackend, operationKey, migrationEnvelope(t, fileOperationSchema, operationKey, operation))
		if err := candidateEngine.stageSchema007RecoveredTrashMetadata008(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("root live source error = %v", err)
		}
	})

	t.Run("invalid-operation-owner", func(t *testing.T) {
		candidateBackend, candidateEngine := newFixture(t, []storageformat.DirectoryEntry{entry})
		operation := storageformat.FileOperation{SchemaVersion: 1, OperationID: "invalid-owner", UserID: "invalid", Kind: operationMove, State: storageformat.FileOperationSucceeded, Attempt: 1, Fence: 1, ReplicaAttemptID: "replica", ExpiresAt: now.Add(time.Hour), StartedAt: now.Add(-time.Minute), UpdatedAt: now, Roots: []storageformat.FileOperationRoot{{Key: "unrelated/root", FinalBody: []byte("ignored")}}}
		operationKey := storageformat.OperationKey(operation.UserID, operation.OperationID)
		migrationPut(t, candidateBackend, operationKey, migrationEnvelope(t, fileOperationSchema, operationKey, operation))
		if err := candidateEngine.stageSchema007RecoveredTrashMetadata008(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid operation owner error = %v", err)
		}
	})

	_, duplicateEngine := newFixture(t, []storageformat.DirectoryEntry{entry, entry})
	if err := duplicateEngine.stageSchema007RecoveredTrashMetadata008(ctx); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("duplicate trash names error = %v", err)
	}
}

func TestSchema008NamespaceMigrationCachesSubtreesAndRejectsCyclesWithBoundedReads(t *testing.T) {
	ctx := context.Background()
	live := namespaceTestScope(t, domain.AreaLive)
	owner := live.UserID()
	now := time.Date(2060, 1, 2, 3, 4, 5, 0, time.UTC)
	manifestID, pageID := "manifest", "page"
	putTree := func(t *testing.T, backend objectstore.Backend, entry storageformat.DirectoryEntry) {
		t.Helper()
		recursiveFiles := entry.FileCount
		if entry.Kind == domain.EntryFile {
			recursiveFiles = 1
		}
		pageKey := storageformat.DirectoryPageKey(owner.String(), areaName(live.Area()), storageformat.RootDirectoryID, pageID)
		migrationPut(t, backend, pageKey, migrationEnvelope(t, directoryPageSchema, pageKey, storageformat.DirectoryPage{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, PageID: pageID, Entries: []storageformat.DirectoryEntry{entry}}))
		putMigrationRoot(t, backend, live, storageformat.RootDirectoryID, manifestID, storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, ManifestID: manifestID, RecursiveBytes: entry.Size, RecursiveFileCount: recursiveFiles, ContentAccumulator: "accumulator", ContentDigest: storageformat.Digest([]byte("content"))})
		putMigrationManifest(t, backend, live, storageformat.RootDirectoryID, manifestID, storageformat.DirectoryManifest{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, ManifestID: manifestID, PageIDs: []string{pageID}, EntryCount: 1, RecursiveBytes: entry.Size, RecursiveFileCount: recursiveFiles, ContentAccumulator: "accumulator", ContentDigest: storageformat.Digest([]byte("content")), CreatedAt: now})
	}

	file := storageformat.DirectoryEntry{Name: "file.bin", NameDigest: storageformat.NameDigest("file.bin"), Kind: domain.EntryFile, BlobID: "blob", Size: 4, MD5: testProviderMD5, CRC32C: testProviderCRC32C, MediaType: "application/octet-stream", ModifiedAt: now}
	file.LogicalVersion, _ = directoryEntryVersion(file)
	backend := objectmemory.New()
	putTree(t, backend, file)
	engine := newSchema008MigrationTestEngine(t, backend)
	if err := engine.stageSchema007Namespaces008(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine.stageSchema007Namespaces008(ctx); err != nil {
		t.Fatalf("restart namespace staging = %v", err)
	}
	sourceID := owner.String() + "\x00live\x00/\x00live\x00" + storageformat.RootDirectoryID + "\x00" + manifestID
	cacheKey := storageformat.Schema008MigrationSubtreeKey(storageformat.Digest([]byte(sourceID)))
	if _, err := backend.Get(ctx, cacheKey); err != nil {
		t.Fatalf("subtree cache = %v", err)
	}
	replaceObjectBody(t, backend, cacheKey, []byte("not-json"))
	if _, err := engine.buildSchema008NamespaceSubtree(ctx, owner, domain.AreaLive, domain.AreaLive, namespaceRootPath(), storageformat.RootDirectoryID, manifestID, nil, make(map[string]struct{})); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt subtree cache error = %v", err)
	}

	cycleBackend := objectmemory.New()
	cycle := storageformat.DirectoryEntry{Name: "loop", NameDigest: storageformat.NameDigest("loop"), Kind: domain.EntryDirectory, DirectoryID: storageformat.RootDirectoryID, ManifestID: manifestID, Size: 0, FileCount: 0, ContentDigest: storageformat.Digest([]byte("content")), ModifiedAt: now}
	cycle.LogicalVersion, _ = directoryEntryVersion(cycle)
	putTree(t, cycleBackend, cycle)
	reads := 0
	hooks := &hookedBackend{Backend: cycleBackend, get: func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
		reads++
		return cycleBackend.Get(ctx, key)
	}}
	cycleEngine := newSchema008MigrationTestEngine(t, hooks)
	if _, err := cycleEngine.buildSchema008NamespaceSubtree(ctx, owner, domain.AreaLive, domain.AreaLive, namespaceRootPath(), storageformat.RootDirectoryID, manifestID, nil, make(map[string]struct{})); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("cycle error = %v", err)
	}
	if reads > 8 {
		t.Fatalf("cycle detection used %d provider reads, want <= 8", reads)
	}

	t.Run("invalid-child-storage-area", func(t *testing.T) {
		candidateBackend := objectmemory.New()
		child := storageformat.DirectoryEntry{Name: "child", NameDigest: storageformat.NameDigest("child"), Kind: domain.EntryDirectory, DirectoryID: "child", ManifestID: "child-manifest", StorageArea: "archive", ContentDigest: storageformat.Digest([]byte("child")), ModifiedAt: now}
		child.LogicalVersion, _ = directoryEntryVersion(child)
		putTree(t, candidateBackend, child)
		candidate := newSchema008MigrationTestEngine(t, candidateBackend)
		if _, err := candidate.buildSchema008NamespaceSubtree(ctx, owner, domain.AreaLive, domain.AreaLive, namespaceRootPath(), storageformat.RootDirectoryID, manifestID, nil, make(map[string]struct{})); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid child storage area error = %v", err)
		}
	})

	t.Run("invalid-file-authority", func(t *testing.T) {
		candidateBackend := objectmemory.New()
		invalid := storageformat.DirectoryEntry{Name: "file.bin", Kind: domain.EntryFile, ModifiedAt: now}
		putTree(t, candidateBackend, invalid)
		candidate := newSchema008MigrationTestEngine(t, candidateBackend)
		if _, err := candidate.buildSchema008NamespaceSubtree(ctx, owner, domain.AreaLive, domain.AreaLive, namespaceRootPath(), storageformat.RootDirectoryID, manifestID, nil, make(map[string]struct{})); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid file authority error = %v", err)
		}
	})

	t.Run("path-bound", func(t *testing.T) {
		candidateBackend := objectmemory.New()
		longName := strings.Repeat("n", 255)
		longFile := file
		longFile.Name, longFile.NameDigest = longName, storageformat.NameDigest(longName)
		longFile.LogicalVersion, _ = directoryEntryVersion(longFile)
		putTree(t, candidateBackend, longFile)
		segments := make([]string, 16)
		for index := range segments {
			segments[index] = strings.Repeat("p", 240)
		}
		logicalPath := domain.MustParseUserPath("/" + strings.Join(segments, "/"))
		candidate := newSchema008MigrationTestEngine(t, candidateBackend)
		if _, err := candidate.buildSchema008NamespaceSubtree(ctx, owner, domain.AreaLive, domain.AreaLive, logicalPath, storageformat.RootDirectoryID, manifestID, nil, make(map[string]struct{})); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("path bound error = %v", err)
		}
	})

	t.Run("cross-area-directory", func(t *testing.T) {
		candidateBackend := objectmemory.New()
		accumulator, digest, err := directoryContentIdentity(nil)
		if err != nil {
			t.Fatal(err)
		}
		childID, childManifest := "trash-child", "trash-child-manifest"
		child := storageformat.DirectoryEntry{Name: "child", NameDigest: storageformat.NameDigest("child"), Kind: domain.EntryDirectory, DirectoryID: childID, ManifestID: childManifest, StorageArea: "trash", ContentDigest: digest, ModifiedAt: now}
		child.LogicalVersion, _ = directoryEntryVersion(child)
		putTree(t, candidateBackend, child)
		trashScope, _ := domain.NewScope(owner, domain.AreaTrash)
		childPage := "trash-child-page"
		childPageKey := storageformat.DirectoryPageKey(owner.String(), "trash", childID, childPage)
		migrationPut(t, candidateBackend, childPageKey, migrationEnvelope(t, directoryPageSchema, childPageKey, storageformat.DirectoryPage{SchemaVersion: 1, DirectoryID: childID, PageID: childPage}))
		putMigrationRoot(t, candidateBackend, trashScope, childID, childManifest, storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: childID, ManifestID: childManifest, ContentAccumulator: accumulator, ContentDigest: digest})
		putMigrationManifest(t, candidateBackend, trashScope, childID, childManifest, storageformat.DirectoryManifest{SchemaVersion: 1, DirectoryID: childID, ManifestID: childManifest, PageIDs: []string{childPage}, ContentAccumulator: accumulator, ContentDigest: digest, CreatedAt: now})
		candidate := newSchema008MigrationTestEngine(t, candidateBackend)
		migrated, err := candidate.buildSchema008NamespaceSubtree(ctx, owner, domain.AreaLive, domain.AreaLive, namespaceRootPath(), storageformat.RootDirectoryID, manifestID, nil, make(map[string]struct{}))
		if err != nil || migrated.EntryCount != 1 {
			t.Fatalf("cross-area directory = %+v, %v", migrated, err)
		}
	})

	t.Run("invalid-directory-aggregate", func(t *testing.T) {
		candidateBackend := objectmemory.New()
		putTree(t, candidateBackend, file)
		manifestKey := storageformat.DirectoryManifestKey(owner.String(), "live", storageformat.RootDirectoryID, manifestID)
		invalid := storageformat.DirectoryManifest{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, ManifestID: manifestID, PageIDs: []string{pageID}, EntryCount: 1, RecursiveBytes: file.Size, RecursiveFileCount: 1, ContentDigest: storageformat.Digest([]byte("content")), CreatedAt: now}
		replaceObjectBody(t, candidateBackend, manifestKey, migrationEnvelope(t, directoryManifestSchema, manifestKey, invalid))
		candidate := newSchema008MigrationTestEngine(t, candidateBackend)
		if _, err := candidate.buildSchema008NamespaceSubtree(ctx, owner, domain.AreaLive, domain.AreaLive, namespaceRootPath(), storageformat.RootDirectoryID, manifestID, nil, make(map[string]struct{})); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid directory aggregate error = %v", err)
		}
	})

	t.Run("cache-conflict-winner-differs", func(t *testing.T) {
		candidateBackend := objectmemory.New()
		putTree(t, candidateBackend, file)
		cacheReads := 0
		hooks := &hookedBackend{Backend: candidateBackend}
		hooks.get = func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
			if key == cacheKey {
				cacheReads++
				if cacheReads == 1 {
					return objectstore.Object{}, domain.NewError(domain.ErrorNotFound, "cache absent")
				}
				return objectstore.Object{Key: key, Body: []byte("different")}, nil
			}
			return candidateBackend.Get(ctx, key)
		}
		hooks.put = func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if key == cacheKey {
				return "", domain.NewError(domain.ErrorConflict, "cache race")
			}
			return candidateBackend.Put(ctx, key, body, condition)
		}
		candidate := newSchema008MigrationTestEngine(t, hooks)
		if _, err := candidate.buildSchema008NamespaceSubtree(ctx, owner, domain.AreaLive, domain.AreaLive, namespaceRootPath(), storageformat.RootDirectoryID, manifestID, nil, make(map[string]struct{})); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("cache conflict error = %v", err)
		}
	})

	t.Run("oversized-parent-authority", func(t *testing.T) {
		candidateBackend := objectmemory.New()
		accumulator, digest, err := directoryContentIdentity(nil)
		if err != nil {
			t.Fatal(err)
		}
		emptyPage := "empty-page"
		emptyPageKey := storageformat.DirectoryPageKey(owner.String(), "live", storageformat.RootDirectoryID, emptyPage)
		migrationPut(t, candidateBackend, emptyPageKey, migrationEnvelope(t, directoryPageSchema, emptyPageKey, storageformat.DirectoryPage{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, PageID: emptyPage}))
		putMigrationRoot(t, candidateBackend, live, storageformat.RootDirectoryID, manifestID, storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, ManifestID: manifestID, ContentAccumulator: accumulator, ContentDigest: digest})
		putMigrationManifest(t, candidateBackend, live, storageformat.RootDirectoryID, manifestID, storageformat.DirectoryManifest{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, ManifestID: manifestID, PageIDs: []string{emptyPage}, ContentAccumulator: accumulator, ContentDigest: digest, CreatedAt: now})
		parent := storageformat.DirectoryEntry{Name: "child", Kind: domain.EntryDirectory, MediaType: strings.Repeat("x", storageformat.MaxCanonicalBytes), ModifiedAt: now}
		candidate := newSchema008MigrationTestEngine(t, candidateBackend)
		if _, err := candidate.buildSchema008NamespaceSubtree(ctx, owner, domain.AreaLive, domain.AreaLive, domain.MustParseUserPath("/child"), storageformat.RootDirectoryID, manifestID, &parent, make(map[string]struct{})); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("oversized parent authority error = %v", err)
		}
	})
}

func TestSchema008MigrationRejectsUnroutableAndMisboundPredecessorRecords(t *testing.T) {
	ctx := context.Background()
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	now := time.Date(2060, 1, 2, 3, 4, 5, 0, time.UTC)

	t.Run("invalid-stage-through-writer", func(t *testing.T) {
		engine := newSchema008MigrationTestEngine(t, objectmemory.New())
		if _, err := engine.writeSchema008MigrationStage(ctx, schema008MigrationStage{}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid stage write error = %v", err)
		}
	})

	t.Run("state-without-domain-route", func(t *testing.T) {
		backend := objectmemory.New()
		engine := newSchema008MigrationTestEngine(t, backend)
		logical := state.MustKey(state.NamespaceAccounts)
		source := canonicalStateKey(logical)
		migrationPut(t, backend, source, migrationEnvelope(t, stateRecordSchema, source, storageformat.StateRecord{SchemaVersion: 1, LogicalKey: logical.String(), Data: []byte("value")}))
		if err := engine.stageSchema007State008(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("unroutable state error = %v", err)
		}
	})

	t.Run("upload-envelope-authentication", func(t *testing.T) {
		backend := objectmemory.New()
		engine := newSchema008MigrationTestEngine(t, backend)
		key := storageformat.OperationKey(owner.String(), "upload-auth")
		record := storageformat.UploadRecord{SchemaVersion: 1, UploadID: "upload-auth", CompletionOperationID: "complete", UserID: owner.String(), Area: "live", RequestedPath: "/a", ResolvedPath: "/a", Size: 1, MediaType: "application/octet-stream", Conflict: domain.ConflictFail, State: storageformat.UploadCompleted, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		body := migrationEnvelope(t, uploadRecordSchema, key, record)
		body = bytes.Replace(body, []byte(`"resolvedPath":"/a"`), []byte(`"resolvedPath":"/b"`), 1)
		migrationPut(t, backend, key, body)
		if err := engine.stageSchema007Uploads008(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("unauthenticated upload envelope error = %v", err)
		}
	})

	t.Run("idempotency-portable-validation", func(t *testing.T) {
		backend := objectmemory.New()
		engine := newSchema008MigrationTestEngine(t, backend)
		originalKey := "bad-fingerprint"
		record := storageformat.IdempotencyRecord{SchemaVersion: 1, UserID: owner.String(), Kind: "upload", KeyDigest: storageformat.Digest([]byte(originalKey)), Fingerprint: "not-a-digest", OperationID: "upload"}
		key := storageformat.IdempotencyKey(owner.String(), originalKey)
		migrationPut(t, backend, key, migrationEnvelope(t, idempotencySchema, key, record))
		if err := engine.stageSchema007UploadIdempotency008(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid portable idempotency error = %v", err)
		}
	})

	t.Run("operation-scan-skips-other-record-kinds", func(t *testing.T) {
		backend := objectmemory.New()
		engine := newSchema008MigrationTestEngine(t, backend)
		key := storageformat.OperationKey(owner.String(), "upload-skip")
		record := storageformat.UploadRecord{SchemaVersion: 1, UploadID: "upload-skip", CompletionOperationID: "complete", UserID: owner.String(), Area: "live", RequestedPath: "/a", ResolvedPath: "/a", Size: 1, MediaType: "application/octet-stream", Conflict: domain.ConflictFail, State: storageformat.UploadCompleted, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		migrationPut(t, backend, key, migrationEnvelope(t, uploadRecordSchema, key, record))
		if err := engine.stageSchema007Operations008(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("operation-envelope-authentication", func(t *testing.T) {
		backend := objectmemory.New()
		engine := newSchema008MigrationTestEngine(t, backend)
		key := storageformat.OperationKey(owner.String(), "operation-auth")
		record := storageformat.FileOperation{SchemaVersion: 1, OperationID: "operation-auth", UserID: owner.String(), Kind: operationDelete, State: storageformat.FileOperationSucceeded, Attempt: 1, Fence: 1, ReplicaAttemptID: "replica", ExpiresAt: now.Add(time.Hour), StartedAt: now, UpdatedAt: now}
		body := migrationEnvelope(t, fileOperationSchema, key, record)
		body = bytes.Replace(body, []byte(`"attempt":1`), []byte(`"attempt":2`), 1)
		migrationPut(t, backend, key, body)
		if err := engine.stageSchema007Operations008(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("unauthenticated operation envelope error = %v", err)
		}
	})

	if operation := ptrDomainOperation(domain.Operation{ID: "operation"}); operation == nil || operation.ID != "operation" {
		t.Fatalf("operation pointer = %+v", operation)
	}
}

func TestSchema008MigrationRejectsMalformedNamespaceRootsAndProviderListings(t *testing.T) {
	ctx := context.Background()
	owner := namespaceTestScope(t, domain.AreaLive).UserID()

	for name, key := range map[string]objectstore.Key{
		"owner": storageformat.DirectoryRootKey("invalid", "live", storageformat.RootDirectoryID),
		"area":  storageformat.DirectoryRootKey(owner.String(), "archive", storageformat.RootDirectoryID),
	} {
		t.Run(name, func(t *testing.T) {
			backend := objectmemory.New()
			migrationPut(t, backend, key, []byte("unused"))
			engine := newSchema008MigrationTestEngine(t, backend)
			if err := engine.stageSchema007Namespaces008(ctx); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("invalid namespace root error = %v", err)
			}
		})
	}

	t.Run("stage-head-provider-failure", func(t *testing.T) {
		base := objectmemory.New()
		root := storageformat.DirectoryRootKey(owner.String(), "live", storageformat.RootDirectoryID)
		migrationPut(t, base, root, []byte("unused"))
		hooks := &hookedBackend{Backend: base, head: func(context.Context, objectstore.Key) (objectstore.ObjectInfo, error) {
			return objectstore.ObjectInfo{}, domain.NewError(domain.ErrorUnavailable, "head denied")
		}}
		if err := newSchema008MigrationTestEngine(t, hooks).stageSchema007Namespaces008(ctx); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("stage head failure = %v", err)
		}
	})

	t.Run("subtree-cache-provider-failure", func(t *testing.T) {
		base := objectmemory.New()
		hooks := &hookedBackend{Backend: base, get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "get denied")
		}}
		engine := newSchema008MigrationTestEngine(t, hooks)
		if _, err := engine.buildSchema008NamespaceSubtree(ctx, owner, domain.AreaLive, domain.AreaLive, namespaceRootPath(), storageformat.RootDirectoryID, "manifest", nil, make(map[string]struct{})); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("subtree cache failure = %v", err)
		}
	})

	t.Run("missing-root-when-manifest-unspecified", func(t *testing.T) {
		engine := newSchema008MigrationTestEngine(t, objectmemory.New())
		if _, err := engine.buildSchema008NamespaceSubtree(ctx, owner, domain.AreaLive, domain.AreaLive, namespaceRootPath(), storageformat.RootDirectoryID, "", nil, make(map[string]struct{})); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing subtree root error = %v", err)
		}
	})

	t.Run("missing-manifest", func(t *testing.T) {
		engine := newSchema008MigrationTestEngine(t, objectmemory.New())
		if _, err := engine.buildSchema008NamespaceSubtree(ctx, owner, domain.AreaLive, domain.AreaLive, namespaceRootPath(), storageformat.RootDirectoryID, "missing", nil, make(map[string]struct{})); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing subtree manifest error = %v", err)
		}
	})

	t.Run("invalid-listing-prefix", func(t *testing.T) {
		base := objectmemory.New()
		hooks := &hookedBackend{Backend: base, list: func(context.Context, objectstore.ListRequest) (objectstore.ListPage, error) {
			return objectstore.ListPage{Objects: []objectstore.ObjectInfo{{Key: objectstore.MustKey("outside/prefix.json")}}}, nil
		}}
		if err := newSchema008MigrationTestEngine(t, hooks).installSchema008StagedDomains(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid listing prefix error = %v", err)
		}
	})

	t.Run("invalid-listing-key", func(t *testing.T) {
		base := objectmemory.New()
		key := objectstore.MustKey(storageformat.Schema008MigrationStagePrefix() + "missing-group-separator.json")
		hooks := &hookedBackend{Backend: base, list: func(context.Context, objectstore.ListRequest) (objectstore.ListPage, error) {
			return objectstore.ListPage{Objects: []objectstore.ObjectInfo{{Key: key}}}, nil
		}}
		if err := newSchema008MigrationTestEngine(t, hooks).installSchema008StagedDomains(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid listing key error = %v", err)
		}
	})

	t.Run("pagination", func(t *testing.T) {
		base := objectmemory.New()
		calls := 0
		hooks := &hookedBackend{Backend: base, list: func(_ context.Context, request objectstore.ListRequest) (objectstore.ListPage, error) {
			calls++
			if calls == 1 {
				if request.Cursor != "" {
					t.Fatalf("first cursor = %q", request.Cursor)
				}
				return objectstore.ListPage{NextCursor: "next"}, nil
			}
			if request.Cursor != "next" {
				t.Fatalf("next cursor = %q", request.Cursor)
			}
			return objectstore.ListPage{}, nil
		}}
		if err := newSchema008MigrationTestEngine(t, hooks).installSchema008StagedDomains(ctx); err != nil {
			t.Fatal(err)
		}
		if calls != 2 {
			t.Fatalf("list calls = %d", calls)
		}
	})
}

func TestMigrationLegacyScansAndAggregateWalksFailClosedAtStructuralBoundaries(t *testing.T) {
	ctx := context.Background()
	scope := namespaceTestScope(t, domain.AreaLive)
	transition := storageMigration{checkpointID: "aggregate-boundary-test"}

	t.Run("schema-001-upload-scan-skips-other-operation-kinds", func(t *testing.T) {
		backend := objectmemory.New()
		engine := newSchema008MigrationTestEngine(t, backend)
		key := storageformat.OperationKey(scope.UserID().String(), "file-operation")
		record := storageformat.FileOperation{SchemaVersion: 1, OperationID: "file-operation", UserID: scope.UserID().String(), Kind: operationDelete, State: storageformat.FileOperationSucceeded}
		migrationPut(t, backend, key, migrationEnvelope(t, fileOperationSchema, key, record))
		if err := engine.migrateSchema001UploadRecords(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("scope-without-area-root", func(t *testing.T) {
		backend := objectmemory.New()
		engine := newSchema008MigrationTestEngine(t, backend)
		key := storageformat.DirectoryRootKey(scope.UserID().String(), "live", storageformat.Digest([]byte("orphan-directory")))
		migrationPut(t, backend, key, []byte("unread"))
		if err := engine.migrateAllDirectoryAggregatesPhase(ctx, transition, aggregateMigrationPlan{}, migrationPhaseTransform); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("missing area root error = %v", err)
		}
	})

	t.Run("active-cycle", func(t *testing.T) {
		engine := newSchema008MigrationTestEngine(t, objectmemory.New())
		walk := migrationWalk{engine: engine, group: migrationScope{scope: scope}, transition: transition, phase: migrationPhaseTransform, active: map[string]struct{}{"cycle": {}}}
		if _, err := walk.directoryEntry(ctx, "cycle", "parent", "child"); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("active cycle error = %v", err)
		}
	})

	t.Run("missing-child-root", func(t *testing.T) {
		engine := newSchema008MigrationTestEngine(t, objectmemory.New())
		walk := migrationWalk{engine: engine, group: migrationScope{scope: scope}, transition: transition, phase: migrationPhaseTransform, active: make(map[string]struct{})}
		if _, err := walk.directoryEntry(ctx, "missing", "parent", "child"); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("missing child root error = %v", err)
		}
	})
}
