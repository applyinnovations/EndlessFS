package portable

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type deleteBackendFunc func(context.Context, objectstore.Key, objectstore.DeleteCondition) error

func (function deleteBackendFunc) Delete(ctx context.Context, key objectstore.Key, condition objectstore.DeleteCondition) error {
	return function(ctx, key, condition)
}

func TestMutationReferenceAndLegacyIntentValidation(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2047, 1, 2, 3, 4, 6, 0, time.UTC))
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("mutation-reference", 1<<16)))
	key := objectstore.MustKey("endlessfs/v1/test/reference")
	body := []byte("reference")
	if _, err := backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	reference := storageformat.MutationObjectReference{Key: key.String(), BodyDigest: storageformat.Digest(body)}
	if err := engine.ensureMutationPrerequisiteReferences(ctx, []storageformat.MutationObjectReference{reference}); err != nil {
		t.Fatal(err)
	}
	for name, references := range map[string][]storageformat.MutationObjectReference{
		"order":   {reference, reference},
		"key":     {{Key: "INVALID", BodyDigest: reference.BodyDigest}},
		"missing": {{Key: "endlessfs/v1/test/missing", BodyDigest: reference.BodyDigest}},
		"digest":  {{Key: key.String(), BodyDigest: "wrong"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := engine.ensureMutationPrerequisiteReferences(ctx, references); err == nil {
				t.Fatal("invalid prerequisite reference was accepted")
			}
		})
	}

	operationKey := storageformat.OperationKey("user", "operation")
	initial := storageformat.FileOperation{
		SchemaVersion: 1, OperationID: "operation", UserID: "user", Kind: "copy",
		State: storageformat.FileOperationRunning, Attempt: 1, Fence: 1, ReplicaAttemptID: "first",
		ExpiresAt: clock.Now().Add(time.Minute), StartedAt: clock.Now(), UpdatedAt: clock.Now(),
		Roots: []storageformat.FileOperationRoot{{Key: "root", PendingBody: []byte("pending"), FinalBody: []byte("final")}},
	}
	current := initial
	current.State = storageformat.FileOperationCommitted
	current.Attempt = 2
	current.Fence = 2
	current.ReplicaAttemptID = "second"
	current.ExpiresAt = clock.Now().Add(2 * time.Minute)
	current.UpdatedAt = clock.Now().Add(time.Minute)
	currentBody := encodeInternalEnvelope(t, fileOperationSchema, operationKey, 2, current)
	initialBody := encodeInternalEnvelope(t, fileOperationSchema, operationKey, 1, initial)
	if !sameFileOperationIntent(currentBody, initialBody, operationKey) {
		t.Fatal("legacy operation with only recoverable fields changed did not match")
	}
	current.Kind = "move"
	if sameFileOperationIntent(encodeInternalEnvelope(t, fileOperationSchema, operationKey, 3, current), initialBody, operationKey) {
		t.Fatal("different legacy operation intent matched")
	}
}

func TestStateMutationErrorBoundaries(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2047, 1, 2, 3, 4, 8, 0, time.UTC))
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("state-errors", 1<<16)))
	key := state.MustKey(state.NamespaceAccounts, "errors")
	version, err := engine.Create(ctx, key, []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CompareAndSwap(ctx, key, "", []byte("second")); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty CAS version error = %v", err)
	}
	engine.ids = domain.NewIDGenerator(strings.NewReader("short"))
	if _, err := engine.CompareAndSwap(ctx, key, version, []byte("second")); err == nil {
		t.Fatal("CAS accepted unavailable secure randomness")
	}
	if _, err := engine.Create(ctx, state.MustKey(state.NamespaceAccounts, "new"), []byte("new")); err == nil {
		t.Fatal("Create accepted unavailable secure randomness")
	}
	if _, err := engine.encodeStateListCursor(stateListCursor{}); err == nil {
		t.Fatal("state cursor accepted unavailable secure randomness")
	}
	engine.ids = domain.NewIDGenerator(strings.NewReader(strings.Repeat("state-errors-restored", 1<<14)))
	if _, err := stateVersionObject(key, "oversized", make([]byte, storageformat.MaxCanonicalBytes)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized state object error = %v", err)
	}
	badRecordKey := storageformat.StateVersionKey(string(state.NamespaceAccounts), key.String(), "bad-record")
	badRecordBody := encodeInternalEnvelope(t, stateVersionSchema, badRecordKey, 1, storageformat.StateVersionRecord{SchemaVersion: 1, LogicalKey: key.String(), LogicalVersion: "other", Data: []byte("x")})
	if _, err := backend.Put(ctx, badRecordKey, badRecordBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.readIndexedStateValue(ctx, storageformat.StateIndexEntry{LogicalKey: key.String(), LogicalVersion: "bad-record"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("mismatched indexed record error = %v", err)
	}
	invalidCursorBody := []byte(`{"schemaVersion":3}`)
	nonce := make([]byte, engine.cursorAEAD.NonceSize())
	sealed := engine.cursorAEAD.Seal(append([]byte(nil), nonce...), nonce, invalidCursorBody, []byte("endlessfs-state-cursor-v3"))
	if _, err := engine.decodeStateListCursor(base64.RawURLEncoding.EncodeToString(sealed)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid decrypted state cursor error = %v", err)
	}
}

func TestRetentionPruningAndTerminalOperationValidation(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2047, 1, 2, 3, 4, 9, 0, time.UTC))
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("retention-pruning", 1<<16)))

	orphanKey := storageformat.IdempotencyKey("user", "orphan")
	orphan := storageformat.IdempotencyRecord{SchemaVersion: 1, UserID: "user", OperationID: "missing", Kind: "copy", KeyDigest: "key", Fingerprint: "fingerprint"}
	if _, err := backend.Put(ctx, orphanKey, encodeInternalEnvelope(t, idempotencySchema, orphanKey, 1, orphan), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	expiredKey := storageformat.OperationKey("user", "expired")
	expired := storageformat.FileOperation{
		SchemaVersion: 1, OperationID: "expired", UserID: "user", Kind: "copy", State: storageformat.FileOperationSucceeded,
		Attempt: 1, Fence: 1, ReplicaAttemptID: "owner", ExpiresAt: clock.Now().Add(-31 * 24 * time.Hour), StartedAt: clock.Now().Add(-31 * 24 * time.Hour), UpdatedAt: clock.Now().Add(-31 * 24 * time.Hour),
		Roots: []storageformat.FileOperationRoot{{Key: "root"}},
	}
	if _, err := backend.Put(ctx, expiredKey, encodeInternalEnvelope(t, fileOperationSchema, expiredKey, 1, expired), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if err := engine.pruneExpiredOperationRecords(ctx); err != nil {
		t.Fatal(err)
	}
	for _, key := range []objectstore.Key{orphanKey, expiredKey} {
		if _, err := backend.Head(ctx, key); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("expired maintenance object %s remains: %v", key.String(), err)
		}
	}

	malformed := objectstore.Object{Key: expiredKey, Body: []byte("not-json")}
	if _, err := terminalOperationExpired(malformed, clock.Now()); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("malformed terminal operation error = %v", err)
	}
	unknown := objectstore.Object{Key: expiredKey, Body: encodeInternalEnvelope(t, "unknown-v1", expiredKey, 1, map[string]int{"schemaVersion": 1})}
	if _, err := terminalOperationExpired(unknown, clock.Now()); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("unknown terminal operation error = %v", err)
	}
	invalidFile := expired
	invalidFile.SchemaVersion = 0
	if _, err := terminalOperationExpired(objectstore.Object{Key: expiredKey, Body: encodeInternalEnvelope(t, fileOperationSchema, expiredKey, 1, invalidFile)}, clock.Now()); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid file operation error = %v", err)
	}
	uploadKey := storageformat.OperationKey("user", "upload")
	invalidUpload := storageformat.UploadRecord{SchemaVersion: 1, UserID: "user", UploadID: "other", State: storageformat.UploadCompleted, CreatedAt: clock.Now()}
	if _, err := terminalOperationExpired(objectstore.Object{Key: uploadKey, Body: encodeInternalEnvelope(t, uploadRecordSchema, uploadKey, 1, invalidUpload)}, clock.Now()); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid upload operation error = %v", err)
	}
	for _, expected := range []error{domain.ErrNotFound, domain.ErrPreconditionFailed} {
		if err := deleteMaintenanceObject(ctx, deleteBackendFunc(func(context.Context, objectstore.Key, objectstore.DeleteCondition) error { return expected }), expiredKeyObject(expiredKey)); err != nil {
			t.Fatalf("superseded maintenance delete error = %v", err)
		}
	}
}

func expiredKeyObject(key objectstore.Key) objectstore.Object {
	return objectstore.Object{Key: key, Version: "version"}
}

func TestOperationPrerequisiteReferencePromotionMatrix(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2047, 1, 2, 3, 4, 10, 0, time.UTC))
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("operation-reference", 1<<16)))
	files := engine.Files()
	body := []byte("prerequisite")
	digest := storageformat.Digest(body)
	target := objectstore.MustKey("endlessfs/v1/test/reference-present")
	if _, err := backend.Put(ctx, target, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	present := storageformat.MutationObjectReference{Key: target.String(), BodyDigest: digest}
	if err := files.ensureOperationPrerequisiteReferences(ctx, storageformat.FileOperation{}, []storageformat.MutationObjectReference{present}); err != nil {
		t.Fatal(err)
	}
	present.BodyDigest = "wrong"
	if err := files.ensureOperationPrerequisiteReferences(ctx, storageformat.FileOperation{}, []storageformat.MutationObjectReference{present}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("present digest mismatch error = %v", err)
	}
	missing := storageformat.MutationObjectReference{Key: "endlessfs/v1/test/reference-missing", BodyDigest: digest}
	if err := files.ensureOperationPrerequisiteReferences(ctx, storageformat.FileOperation{}, []storageformat.MutationObjectReference{missing}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unstaged missing prerequisite error = %v", err)
	}
	operation := storageformat.FileOperation{UserID: "user", OperationID: "operation", StepsStaged: true}
	missing.StagingKey = "INVALID"
	if err := files.ensureOperationPrerequisiteReferences(ctx, operation, []storageformat.MutationObjectReference{missing}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid staging reference error = %v", err)
	}
	staging := storageformat.OperationStagingKey(operation.UserID, operation.OperationID, "prerequisite-"+storageformat.Digest([]byte(missing.Key)))
	missing.StagingKey = staging.String()
	if _, err := backend.Put(ctx, staging, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if err := files.ensureOperationPrerequisiteReferences(ctx, operation, []storageformat.MutationObjectReference{missing}); err != nil {
		t.Fatalf("staged prerequisite promotion error = %v", err)
	}
	if object, err := backend.Get(ctx, objectstore.MustKey(missing.Key)); err != nil || string(object.Body) != string(body) {
		t.Fatalf("promoted prerequisite = %+v, %v", object, err)
	}
	badMissing := storageformat.MutationObjectReference{Key: "endlessfs/v1/test/reference-bad-staged", BodyDigest: "wrong"}
	badStaging := storageformat.OperationStagingKey(operation.UserID, operation.OperationID, "prerequisite-"+storageformat.Digest([]byte(badMissing.Key)))
	badMissing.StagingKey = badStaging.String()
	if _, err := backend.Put(ctx, badStaging, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if err := files.ensureOperationPrerequisiteReferences(ctx, operation, []storageformat.MutationObjectReference{badMissing}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("staged digest mismatch error = %v", err)
	}
}

func TestCheckpointMetadataOnlyValidationBoundaries(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2047, 1, 2, 3, 4, 11, 0, time.UTC))
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("checkpoint-metadata", 1<<16)))
	if got := saturatingInventoryBytes(math.MaxInt64, 1); got != math.MaxInt64 {
		t.Fatalf("saturating inventory bytes = %d", got)
	}
	if err := engine.validateCheckpointPageSet(ctx, "missing-pages", 1); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("missing page-set error = %v", err)
	}
	unexpectedID := "unexpected-pages"
	unexpectedKey := storageformat.CheckpointInventoryPageKey(unexpectedID, 1)
	if _, err := backend.Put(ctx, unexpectedKey, []byte("page"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if err := engine.validateCheckpointPageSet(ctx, unexpectedID, 1); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("unexpected page-set error = %v", err)
	}
	objectKey := objectstore.MustKey("endlessfs/v1/test/checkpoint-object")
	body := []byte("metadata-only")
	version, err := backend.Put(ctx, objectKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
	if err != nil {
		t.Fatal(err)
	}
	expected := objectstore.ObjectInfo{Key: objectKey, Version: version, Size: int64(len(body))}
	if object, _, err := streamCheckpointObject(ctx, backend, expected); err != nil || object.MD5 == "" || object.CRC32C == "" {
		t.Fatalf("streamCheckpointObject() = %+v, %v", object, err)
	}
	expected.Size++
	if _, _, err := streamCheckpointObject(ctx, backend, expected); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("changed checkpoint metadata error = %v", err)
	}
	missingInfo := objectstore.ObjectInfo{Key: objectstore.MustKey("endlessfs/v1/test/checkpoint-missing"), Size: 1}
	if _, _, err := streamCheckpointObject(ctx, backend, missingInfo); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing checkpoint metadata error = %v", err)
	}
	visitorErr := domain.NewError(domain.ErrorUnavailable, "stop")
	legacy := storageformat.Checkpoint{SchemaVersion: 1, Objects: []storageformat.CheckpointObject{{Key: objectKey.String()}}}
	if err := engine.visitCheckpointInventory(ctx, legacy, func(storageformat.CheckpointInventoryEntry) error { return visitorErr }); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("legacy checkpoint visitor error = %v", err)
	}
	if err := engine.visitCheckpointInventory(ctx, legacy, nil); err != nil {
		t.Fatalf("legacy checkpoint without visitor error = %v", err)
	}
	checkpoint := storageformat.Checkpoint{CheckpointID: "summary", GateEpoch: 2, WriterSetID: engine.writer.WriterSetID, InventoryPageCount: 1, StateObjectCount: 2, FileObjectCount: 3, InventoryDigest: "digest"}
	if err := engine.verifyCheckpointV2Summary(checkpoint, storageformat.WriteGate{Mode: storageformat.GateOpen}, checkpointInventorySummary{}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("invalid checkpoint summary error = %v", err)
	}
	if err := engine.verifyCheckpointV3(ctx, checkpoint, storageformat.WriteGate{Mode: storageformat.GateOpen}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("invalid v3 checkpoint gate error = %v", err)
	}

	if err := engine.retireLegacyCheckpoint(ctx, "absent"); err != nil {
		t.Fatalf("absent legacy checkpoint error = %v", err)
	}
	for name, schema := range map[string]string{"current": checkpointSchemaV3, "legacy": checkpointSchema, "incompatible": "other-checkpoint-v1"} {
		key := storageformat.CheckpointKey(name)
		if _, err := backend.Put(ctx, key, encodeInternalEnvelope(t, schema, key, 1, map[string]int{"schemaVersion": 1}), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		err := engine.retireLegacyCheckpoint(ctx, name)
		if name == "incompatible" {
			if !errors.Is(err, domain.ErrPreconditionFailed) {
				t.Fatalf("incompatible checkpoint retirement error = %v", err)
			}
		} else if err != nil {
			t.Fatalf("%s checkpoint retirement error = %v", name, err)
		}
	}
}

func TestGatePruningFailureMatrix(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2047, 1, 2, 3, 4, 13, 0, time.UTC))
	newEngine := func(t *testing.T) (*objectmemory.Backend, *Engine) {
		t.Helper()
		backend := objectmemory.New()
		return backend, openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat(t.Name(), 1<<15)))
	}
	putVersion := func(t *testing.T, backend *objectmemory.Backend, key objectstore.Key, record storageformat.StateVersionRecord) {
		t.Helper()
		if _, err := backend.Put(ctx, key, encodeInternalEnvelope(t, stateVersionSchema, key, 1, record), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("malformed-version-is-garbage", func(t *testing.T) {
		backend, engine := newEngine(t)
		key := storageformat.StateVersionKey("accounts", "accounts/YQ", "malformed")
		if _, err := backend.Put(ctx, key, []byte("not-json"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := engine.pruneStateVersions(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Head(ctx, key); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("malformed version remains: %v", err)
		}
	})
	t.Run("version-get-fails", func(t *testing.T) {
		backend, engine := newEngine(t)
		key := storageformat.StateVersionKey("accounts", "accounts/YQ", "get-error")
		putVersion(t, backend, key, storageformat.StateVersionRecord{SchemaVersion: 1, LogicalKey: "accounts/YQ", LogicalVersion: "get-error"})
		hooks := &hookedBackend{Backend: backend, get: func(_ context.Context, candidate objectstore.Key) (objectstore.Object, error) {
			if candidate == key {
				return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "get denied")
			}
			return backend.Get(ctx, candidate)
		}}
		engine.backend = hooks
		if err := engine.pruneStateVersions(ctx); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("version get error = %v", err)
		}
	})
	t.Run("invalid-version-record", func(t *testing.T) {
		backend, engine := newEngine(t)
		key := storageformat.StateVersionKey("accounts", "accounts/YQ", "invalid")
		putVersion(t, backend, key, storageformat.StateVersionRecord{LogicalKey: "accounts/YQ", LogicalVersion: "invalid"})
		if err := engine.pruneStateVersions(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid version record error = %v", err)
		}
	})
	t.Run("version-key-mismatch", func(t *testing.T) {
		backend, engine := newEngine(t)
		key := storageformat.StateVersionKey("accounts", "accounts/YQ", "wrong-key")
		putVersion(t, backend, key, storageformat.StateVersionRecord{SchemaVersion: 1, LogicalKey: "accounts/YQ", LogicalVersion: "different"})
		if err := engine.pruneStateVersions(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("version key mismatch error = %v", err)
		}
	})
	t.Run("current-read-fails", func(t *testing.T) {
		backend, engine := newEngine(t)
		logical := state.MustKey(state.NamespaceAccounts, "current-error")
		key := storageformat.StateVersionKey("accounts", logical.String(), "version")
		putVersion(t, backend, key, storageformat.StateVersionRecord{SchemaVersion: 1, LogicalKey: logical.String(), LogicalVersion: "version"})
		currentKey := canonicalStateKey(logical)
		hooks := &hookedBackend{Backend: backend, get: func(_ context.Context, candidate objectstore.Key) (objectstore.Object, error) {
			if candidate == currentKey {
				return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "current denied")
			}
			return backend.Get(ctx, candidate)
		}}
		engine.backend = hooks
		if err := engine.pruneStateVersions(ctx); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("current state read error = %v", err)
		}
	})
	t.Run("index-read-fails", func(t *testing.T) {
		backend, engine := newEngine(t)
		logical := state.MustKey(state.NamespaceAccounts, "index-error")
		key := storageformat.StateVersionKey("accounts", logical.String(), "version")
		putVersion(t, backend, key, storageformat.StateVersionRecord{SchemaVersion: 1, LogicalKey: logical.String(), LogicalVersion: "version"})
		rootKey := storageformat.StateIndexRootKey("accounts")
		hooks := &hookedBackend{Backend: backend, get: func(_ context.Context, candidate objectstore.Key) (objectstore.Object, error) {
			if candidate == rootKey {
				return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "index denied")
			}
			return backend.Get(ctx, candidate)
		}}
		engine.backend = hooks
		if err := engine.pruneStateVersions(ctx); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("state index read error = %v", err)
		}
	})
	t.Run("garbage-delete-fails", func(t *testing.T) {
		backend, engine := newEngine(t)
		key := storageformat.StateVersionKey("accounts", "accounts/YQ", "garbage")
		if _, err := backend.Put(ctx, key, []byte("not-json"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		hooks := &hookedBackend{Backend: backend, delete: func(context.Context, objectstore.Key, objectstore.DeleteCondition) error {
			return domain.NewError(domain.ErrorUnavailable, "delete denied")
		}}
		engine.backend = hooks
		if err := engine.pruneStateVersions(ctx); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("garbage delete error = %v", err)
		}
	})

	t.Run("listed-idempotency-disappears", func(t *testing.T) {
		backend, engine := newEngine(t)
		key := storageformat.IdempotencyKey("user", "disappears")
		if _, err := backend.Put(ctx, key, []byte("listed"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		hooks := &hookedBackend{Backend: backend, get: func(_ context.Context, candidate objectstore.Key) (objectstore.Object, error) {
			if candidate == key {
				return objectstore.Object{}, domain.NewError(domain.ErrorNotFound, "gone")
			}
			return backend.Get(ctx, candidate)
		}}
		engine.backend = hooks
		if err := engine.pruneExpiredOperationRecords(ctx); err != nil {
			t.Fatal(err)
		}
	})
	for name, body := range map[string][]byte{
		"malformed-idempotency": []byte("not-json"),
		"invalid-idempotency":   encodeInternalEnvelope(t, idempotencySchema, storageformat.IdempotencyKey("user", "invalid"), 1, storageformat.IdempotencyRecord{}),
	} {
		t.Run(name, func(t *testing.T) {
			backend, engine := newEngine(t)
			key := storageformat.IdempotencyKey("user", name)
			if name == "invalid-idempotency" {
				body = encodeInternalEnvelope(t, idempotencySchema, key, 1, storageformat.IdempotencyRecord{})
			}
			if _, err := backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
				t.Fatal(err)
			}
			if err := engine.pruneExpiredOperationRecords(ctx); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("prune error = %v", err)
			}
		})
	}

	_, engine := newEngine(t)
	deleteIntent := storageformat.MutationIntent{Action: storageformat.MutationDelete, TargetKey: "endlessfs/v1/test/already-deleted", ExpectedLogicalVersion: "version"}
	deleteBody, _ := storageformat.EncodeCanonical(deleteIntent)
	if err := engine.recoverMutation(ctx, storageformat.Admission{Mutation: &deleteIntent, IntentDigest: storageformat.Digest(deleteBody)}); err != nil {
		t.Fatalf("terminal delete recovery error = %v", err)
	}
	unknownIntent := storageformat.MutationIntent{Action: "unknown", TargetKey: "endlessfs/v1/test/unknown"}
	unknownBody, _ := storageformat.EncodeCanonical(unknownIntent)
	if err := engine.recoverMutation(ctx, storageformat.Admission{Mutation: &unknownIntent, IntentDigest: storageformat.Digest(unknownBody)}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("unknown recovery action error = %v", err)
	}
	if err := engine.ensureMutationCopies(ctx, []storageformat.MutationCopy{{SourceKey: "source", DestinationKey: "destination", Size: 1, MD5: "invalid", CRC32C: testProviderCRC32C}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid mutation copy fingerprint error = %v", err)
	}
	badFileEnvelope := objectstore.Object{Key: storageformat.OperationKey("user", "bad-file"), Body: encodeInternalEnvelope(t, fileOperationSchema, storageformat.OperationKey("user", "bad-file"), 1, map[string]string{"schemaVersion": "bad"})}
	if _, err := terminalOperationExpired(badFileEnvelope, clock.Now()); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("malformed file operation envelope error = %v", err)
	}
	badUploadEnvelope := objectstore.Object{Key: storageformat.OperationKey("user", "bad-upload"), Body: encodeInternalEnvelope(t, uploadRecordSchema, storageformat.OperationKey("user", "bad-upload"), 1, map[string]string{"schemaVersion": "bad"})}
	if _, err := terminalOperationExpired(badUploadEnvelope, clock.Now()); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("malformed upload operation envelope error = %v", err)
	}
	drainBackend, drainEngine := newEngine(t)
	drainKey := storageformat.OperationKey("user", "skip-file")
	drainOperation := storageformat.FileOperation{SchemaVersion: 1, OperationID: "skip-file", UserID: "user", Kind: "copy", State: storageformat.FileOperationRunning, UpdatedAt: clock.Now()}
	if _, err := drainBackend.Put(ctx, drainKey, encodeInternalEnvelope(t, fileOperationSchema, drainKey, 1, drainOperation), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if err := drainEngine.drainOperationRecords(ctx, false, false); err != nil {
		t.Fatalf("nonrecovering file-operation drain error = %v", err)
	}
	workBackend, workEngine := newEngine(t)
	workKey := storageformat.CheckpointWorkKey("artifact-error", "endlessfs/v1/test/object")
	if _, err := workBackend.Put(ctx, workKey, []byte("work"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	workEngine.backend = &hookedBackend{Backend: workBackend, delete: func(_ context.Context, key objectstore.Key, condition objectstore.DeleteCondition) error {
		if key == workKey {
			return domain.NewError(domain.ErrorUnavailable, "work delete denied")
		}
		return workBackend.Delete(ctx, key, condition)
	}}
	if err := workEngine.pruneOperationArtifacts(ctx); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("checkpoint work pruning error = %v", err)
	}
	disappearingBackend, disappearingEngine := newEngine(t)
	disappearingKey := storageformat.OperationKey("user", "disappearing")
	if _, err := disappearingBackend.Put(ctx, disappearingKey, []byte("listed"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	disappearingEngine.backend = &hookedBackend{Backend: disappearingBackend, get: func(_ context.Context, key objectstore.Key) (objectstore.Object, error) {
		if key == disappearingKey {
			return objectstore.Object{}, domain.NewError(domain.ErrorNotFound, "gone")
		}
		return disappearingBackend.Get(ctx, key)
	}}
	if err := disappearingEngine.pruneExpiredOperationRecords(ctx); err != nil {
		t.Fatalf("disappearing operation pruning error = %v", err)
	}
	invalidTerminalBackend, invalidTerminalEngine := newEngine(t)
	invalidTerminalKey := storageformat.OperationKey("user", "invalid-terminal")
	if _, err := invalidTerminalBackend.Put(ctx, invalidTerminalKey, encodeInternalEnvelope(t, "unknown-v1", invalidTerminalKey, 1, map[string]int{"schemaVersion": 1}), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if err := invalidTerminalEngine.pruneExpiredOperationRecords(ctx); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid terminal operation pruning error = %v", err)
	}
	uploadBackend, uploadEngine := newEngine(t)
	uploadEngine.fileBackend = metadataOnlyBackend{Backend: uploadBackend}
	uploadKey := storageformat.OperationKey("user", "active-upload")
	upload := storageformat.UploadRecord{SchemaVersion: 1, UserID: "user", UploadID: "active-upload", State: storageformat.UploadActive, CreatedAt: clock.Now().Add(-time.Hour), ExpiresAt: clock.Now().Add(-time.Minute)}
	if _, err := uploadBackend.Put(ctx, uploadKey, encodeInternalEnvelope(t, uploadRecordSchema, uploadKey, 1, upload), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if err := uploadEngine.drainOperationRecords(ctx, false, true); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("metadata-only active upload drain error = %v", err)
	}
}

func TestOperationStepPageDenialMatrix(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2047, 1, 2, 3, 4, 14, 0, time.UTC))
	newFixture := func(t *testing.T, page storageformat.FileOperationStepPage, raw []byte) (*objectmemory.Backend, *FileStore, storageformat.FileOperation) {
		t.Helper()
		backend := objectmemory.New()
		engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat(t.Name(), 1<<15)))
		operation := storageformat.FileOperation{UserID: "user", OperationID: "operation", StepSetID: "steps", StepPageCount: 1}
		key := fileOperationStepPageKey(operation, 0)
		body := raw
		if body == nil {
			page.SchemaVersion, page.UserID, page.OperationID, page.StepSetID, page.Index = 1, operation.UserID, operation.OperationID, operation.StepSetID, 0
			body = encodeInternalEnvelope(t, fileOperationStepPageSchema, key, 1, page)
		}
		if _, err := backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		operation.StepDigest = storageformat.Digest(body)
		return backend, engine.Files(), operation
	}
	cases := map[string]storageformat.FileOperationStepPage{
		"empty":        {},
		"root-order":   {Roots: []storageformat.FileOperationRoot{{Key: "b"}, {Key: "a"}}},
		"prerequisite": {Prerequisites: []storageformat.MutationObjectReference{{Key: "key"}}},
		"copy-order":   {Copies: []storageformat.MutationCopy{{DestinationKey: ""}}},
	}
	for name, page := range cases {
		t.Run(name, func(t *testing.T) {
			_, files, operation := newFixture(t, page, nil)
			if err := files.forEachFileOperationStepPage(ctx, operation, func(storageformat.FileOperationStepPage) error { return nil }); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("invalid step page error = %v", err)
			}
		})
	}
	t.Run("malformed", func(t *testing.T) {
		_, files, operation := newFixture(t, storageformat.FileOperationStepPage{}, []byte("not-json"))
		if err := files.forEachFileOperationStepPage(ctx, operation, func(storageformat.FileOperationStepPage) error { return nil }); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("malformed step page error = %v", err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		backend := objectmemory.New()
		engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("missing-page", 1<<15)))
		operation := storageformat.FileOperation{UserID: "user", OperationID: "operation", StepSetID: "steps", StepPageCount: 1, StepDigest: "digest"}
		if err := engine.Files().forEachFileOperationStepPage(ctx, operation, func(storageformat.FileOperationStepPage) error { return nil }); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing step page error = %v", err)
		}
	})
	backend := objectmemory.New()
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("start-operation", 1<<15)))
	if err := engine.Files().forEachFileOperationStepPage(ctx, storageformat.FileOperation{StepPageCount: 1, StepSetID: "steps", StepDigest: "digest", Roots: []storageformat.FileOperationRoot{{Key: "root"}}}, func(storageformat.FileOperationStepPage) error { return nil }); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("mixed paged operation error = %v", err)
	}
	if err := engine.Files().ensureOperationPrerequisiteReferences(ctx, storageformat.FileOperation{}, []storageformat.MutationObjectReference{{Key: "INVALID", BodyDigest: "digest"}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid operation prerequisite key error = %v", err)
	}
	target := objectstore.MustKey("endlessfs/v1/test/promotion-collision")
	staging := storageformat.OperationStagingKey("user", "collision", "prerequisite-"+storageformat.Digest([]byte(target.String())))
	stagedBody := []byte("expected")
	if _, err := backend.Put(ctx, staging, stagedBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	collisionHooks := &hookedBackend{Backend: backend}
	collisionHooks.copy = func(context.Context, objectstore.Key, objectstore.Key, objectstore.CopyCondition) (objectstore.CopyResult, error) {
		return objectstore.CopyResult{}, domain.NewError(domain.ErrorConflict, "collision")
	}
	targetGets := 0
	collisionHooks.get = func(_ context.Context, key objectstore.Key) (objectstore.Object, error) {
		if key == target {
			targetGets++
			if targetGets == 1 {
				return objectstore.Object{}, domain.NewError(domain.ErrorNotFound, "not promoted")
			}
			return objectstore.Object{Key: key, Body: []byte("different"), Version: "winner"}, nil
		}
		return backend.Get(ctx, key)
	}
	engine.backend = collisionHooks
	ref := storageformat.MutationObjectReference{Key: target.String(), BodyDigest: storageformat.Digest(stagedBody), StagingKey: staging.String()}
	if err := engine.Files().ensureOperationPrerequisiteReferences(ctx, storageformat.FileOperation{UserID: "user", OperationID: "collision", StepsStaged: true}, []storageformat.MutationObjectReference{ref}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("prerequisite promotion collision error = %v", err)
	}
}

func TestCheckpointRetirementAndListingDenials(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2047, 1, 2, 3, 4, 16, 0, time.UTC))
	t.Run("artifact-delete-is-superseded", func(t *testing.T) {
		backend := objectmemory.New()
		engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("retire-artifact", 1<<15)))
		checkpointID := "artifacts"
		workKey := storageformat.CheckpointWorkKey(checkpointID, "endlessfs/v1/test/object")
		if _, err := backend.Put(ctx, workKey, []byte("work"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		hooks := &hookedBackend{Backend: backend, delete: func(context.Context, objectstore.Key, objectstore.DeleteCondition) error {
			return domain.NewError(domain.ErrorPreconditionFailed, "superseded")
		}}
		engine.backend = hooks
		if err := engine.retireLegacyCheckpoint(ctx, checkpointID); err != nil {
			t.Fatalf("superseded artifact retirement error = %v", err)
		}
	})
	t.Run("root-read-fails", func(t *testing.T) {
		backend := objectmemory.New()
		engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("retire-read", 1<<15)))
		hooks := &hookedBackend{Backend: backend, get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "read denied")
		}}
		engine.backend = hooks
		if err := engine.retireLegacyCheckpoint(ctx, "read-error"); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("checkpoint root read error = %v", err)
		}
	})
	t.Run("malformed-root", func(t *testing.T) {
		backend := objectmemory.New()
		engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("retire-malformed", 1<<15)))
		key := storageformat.CheckpointKey("malformed")
		if _, err := backend.Put(ctx, key, []byte("not-json"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := engine.retireLegacyCheckpoint(ctx, "malformed"); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("malformed checkpoint retirement error = %v", err)
		}
	})
	t.Run("legacy-delete-is-superseded", func(t *testing.T) {
		backend := objectmemory.New()
		engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("retire-legacy", 1<<15)))
		key := storageformat.CheckpointKey("legacy-superseded")
		if _, err := backend.Put(ctx, key, encodeInternalEnvelope(t, checkpointSchema, key, 1, map[string]int{"schemaVersion": 1}), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		hooks := &hookedBackend{Backend: backend, delete: func(context.Context, objectstore.Key, objectstore.DeleteCondition) error {
			return domain.NewError(domain.ErrorNotFound, "gone")
		}}
		engine.backend = hooks
		if err := engine.retireLegacyCheckpoint(ctx, "legacy-superseded"); err != nil {
			t.Fatalf("superseded legacy root retirement error = %v", err)
		}
	})
	backend := objectmemory.New()
	hooks := &hookedBackend{Backend: backend, list: func(context.Context, objectstore.ListRequest) (objectstore.ListPage, error) {
		return objectstore.ListPage{Objects: []objectstore.ObjectInfo{{Key: objectstore.MustKey("outside/prefix")}}}, nil
	}}
	if err := walkObjectInfos(ctx, hooks, "endlessfs/v1/", func(objectstore.ObjectInfo) error { return nil }); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("non-canonical listing error = %v", err)
	}
}
