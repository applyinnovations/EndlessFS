package portable

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
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

func TestStateCompatibilityObjectsAndVersionPruning(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2047, 1, 2, 3, 4, 5, 0, time.UTC))
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("state-pruning", 1<<16)))

	legacyKey := state.MustKey(state.NamespaceAccounts, "legacy")
	legacyObjectKey := canonicalStateKey(legacyKey)
	legacyBody := encodeInternalEnvelope(t, stateRecordSchema, legacyObjectKey, 1, storageformat.StateRecord{
		SchemaVersion: 1, LogicalKey: legacyKey.String(), Data: []byte("legacy"),
	})
	legacyVersion, err := canonicalLogicalVersion(legacyBody)
	if err != nil {
		t.Fatal(err)
	}
	legacyNative, err := backend.Put(ctx, legacyObjectKey, legacyBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
	if err != nil {
		t.Fatal(err)
	}
	legacyObject := objectstore.Object{Key: legacyObjectKey, Body: legacyBody, Version: legacyNative}
	if record, _, err := decodeStateObject(legacyObject, legacyKey); err != nil || string(record.Data) != "legacy" {
		t.Fatalf("decodeStateObject() = %+v, %v", record, err)
	}
	if _, _, err := decodeStateObject(objectstore.Object{Key: legacyObjectKey, Body: []byte("not-json")}, legacyKey); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("malformed state object error = %v", err)
	}
	otherKey := state.MustKey(state.NamespaceAccounts, "other")
	if _, _, err := decodeStateObject(legacyObject, otherKey); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("mismatched state object error = %v", err)
	}

	currentLegacy, err := stateVersionObject(legacyKey, state.Version(legacyVersion), []byte("legacy"))
	if err != nil {
		t.Fatal(err)
	}
	staleLegacy, err := stateVersionObject(legacyKey, "stale-legacy", []byte("stale"))
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range []storageformat.MutationObject{currentLegacy, staleLegacy} {
		if _, err := backend.Put(ctx, objectstore.MustKey(object.Key), object.Body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}

	indexedKey := state.MustKey(state.NamespaceAccounts, "indexed")
	indexedVersion, err := engine.Create(ctx, indexedKey, []byte("current"))
	if err != nil {
		t.Fatal(err)
	}
	staleIndexed, err := stateVersionObject(indexedKey, "stale-indexed", []byte("stale"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, objectstore.MustKey(staleIndexed.Key), staleIndexed.Body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.readIndexedStateValue(ctx, storageformat.StateIndexEntry{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid indexed value error = %v", err)
	}
	missingEntry := storageformat.StateIndexEntry{LogicalKey: indexedKey.String(), LogicalVersion: "missing-version"}
	if _, err := engine.readIndexedStateValue(ctx, missingEntry); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing indexed value error = %v", err)
	}

	if err := engine.pruneStateVersions(ctx); err != nil {
		t.Fatal(err)
	}
	for _, key := range []objectstore.Key{objectstore.MustKey(currentLegacy.Key), storageformat.StateVersionKey(string(state.NamespaceAccounts), indexedKey.String(), string(indexedVersion))} {
		if _, err := backend.Head(ctx, key); err != nil {
			t.Fatalf("current state version %s was pruned: %v", key.String(), err)
		}
	}
	for _, key := range []objectstore.Key{objectstore.MustKey(staleLegacy.Key), objectstore.MustKey(staleIndexed.Key)} {
		if _, err := backend.Head(ctx, key); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("stale state version %s remains: %v", key.String(), err)
		}
	}
	if page, err := engine.List(ctx, state.MustPrefix(state.NamespaceAccounts), state.PageRequest{}); err != nil || len(page.Items) != 1 {
		t.Fatalf("default state page = %+v, %v", page, err)
	}
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

func TestPagedOperationHelpersValidateImmutableChains(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2047, 1, 2, 3, 4, 7, 0, time.UTC))
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("operation-pages", 1<<16)))
	files := engine.Files()

	if err := files.persistFileOperationStepPages(ctx, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("nil operation page error = %v", err)
	}
	if err := files.persistFileOperationStepPages(ctx, &storageformat.FileOperation{UserID: "user", OperationID: "operation", ReplicaAttemptID: "attempt"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("rootless operation page error = %v", err)
	}

	operation := storageformat.FileOperation{
		SchemaVersion: 1, UserID: "user", OperationID: "operation", ReplicaAttemptID: "attempt",
		Roots:         []storageformat.FileOperationRoot{{Key: "a", PendingBody: []byte("pending"), FinalBody: []byte("final")}},
		Prerequisites: []storageformat.MutationObject{{Key: "endlessfs/v1/test/prerequisite", Body: []byte("prerequisite")}},
		Copies:        []storageformat.MutationCopy{{SourceKey: "source", DestinationKey: "destination", Size: 1, MD5: testProviderMD5, CRC32C: testProviderCRC32C}},
	}
	if err := files.persistFileOperationStepPages(ctx, &operation); err != nil {
		t.Fatal(err)
	}
	if operation.StepPageCount != 3 || operation.StepDigest == "" || !operation.StepsStaged {
		t.Fatalf("paged operation = %+v", operation)
	}
	visited := 0
	if err := files.forEachFileOperationStepPage(ctx, operation, func(storageformat.FileOperationStepPage) error { visited++; return nil }); err != nil || visited != 3 {
		t.Fatalf("step pages visited = %d, %v", visited, err)
	}
	visitErr := domain.NewError(domain.ErrorUnavailable, "stop")
	if err := files.forEachFileOperationStepPage(ctx, operation, func(storageformat.FileOperationStepPage) error { return visitErr }); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("visitor error = %v", err)
	}
	tampered := operation
	tampered.StepDigest = "wrong"
	if err := files.forEachFileOperationStepPage(ctx, tampered, func(storageformat.FileOperationStepPage) error { return nil }); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("chain digest error = %v", err)
	}
	legacy := storageformat.FileOperation{UserID: "user", OperationID: "legacy", Roots: []storageformat.FileOperationRoot{{Key: "root"}}, Prerequisites: []storageformat.MutationObject{{Key: "key", Body: []byte("body")}}}
	if err := files.forEachFileOperationStepPage(ctx, legacy, func(page storageformat.FileOperationStepPage) error {
		if len(page.Roots) != 1 || len(page.Prerequisites) != 1 {
			t.Fatalf("legacy page = %+v", page)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	legacy.Roots = nil
	if err := files.forEachFileOperationStepPage(ctx, legacy, func(storageformat.FileOperationStepPage) error { return nil }); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid legacy steps error = %v", err)
	}

	immutableKey := objectstore.MustKey("endlessfs/v1/test/immutable")
	if err := files.ensureImmutableOperationObject(ctx, immutableKey, []byte("same")); err != nil {
		t.Fatal(err)
	}
	if err := files.ensureImmutableOperationObject(ctx, immutableKey, []byte("same")); err != nil {
		t.Fatalf("immutable replay error = %v", err)
	}
	if err := files.ensureImmutableOperationObject(ctx, immutableKey, []byte("different")); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("immutable collision error = %v", err)
	}
	if fileOperationStepPageKey(storageformat.FileOperation{UserID: "user", OperationID: "operation", StepSetID: "steps"}, 2) == stagedFileOperationStepPageKey(operation, 2) {
		t.Fatal("durable and staged operation page keys unexpectedly match")
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
	if _, err := engine.newStateVersion(key, make([]byte, storageformat.MaxCanonicalBytes)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized state version error = %v", err)
	}
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

func coverageRandomReader(seed uint64, size int) *strings.Reader {
	value := make([]byte, size)
	stateValue := seed + 0x9e3779b97f4a7c15
	for index := range value {
		stateValue ^= stateValue << 13
		stateValue ^= stateValue >> 7
		stateValue ^= stateValue << 17
		value[index] = byte(stateValue >> 29)
	}
	return strings.NewReader(string(value))
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
	if _, err := files.startFileOperation(ctx, storageformat.FileOperation{UserID: "invalid"}, nil, "", ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid stored operation owner error = %v", err)
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

func TestDirectoryContentMutationValidationBoundaries(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2047, 1, 2, 3, 4, 12, 0, time.UTC))
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("directory-content", 1<<16)))
	files := engine.Files()
	user, _ := domain.ParseUserID("a2tra2tra2tra2tra2traw")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	file := withCurrentTestFingerprint(storageformat.DirectoryEntry{Name: "file.bin", NameDigest: storageformat.NameDigest("file.bin"), Kind: domain.EntryFile, BlobID: "blob", Size: 1, MediaType: "application/octet-stream", ModifiedAt: clock.Now()})
	other := file
	other.Name = "other.bin"
	other.NameDigest = storageformat.NameDigest(other.Name)
	other.LogicalVersion, _ = directoryEntryVersion(other)
	emptyAccumulator, emptyDigest, err := directoryContentIdentity(nil)
	if err != nil {
		t.Fatal(err)
	}
	emptyNode := directoryTrailNode{scope: scope, path: domain.MustParseUserPath("/"), directoryID: storageformat.RootDirectoryID, snapshot: directorySnapshot{
		manifest: storageformat.DirectoryManifest{SchemaVersion: 2, DirectoryID: storageformat.RootDirectoryID}, contentAccumulator: emptyAccumulator, contentDigest: emptyDigest,
	}}

	if _, err := directoryContentContribution(withEntry(file, func(entry *storageformat.DirectoryEntry) {
		entry.Name = strings.Repeat("x", storageformat.MaxCanonicalBytes)
	})); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized content contribution error = %v", err)
	}
	if _, _, err := updateDirectoryContentIdentityAtCount("invalid", nil, nil, 0); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid content accumulator error = %v", err)
	}
	if _, _, err := updateDirectoryContentIdentityAtCount(emptyAccumulator, []storageformat.DirectoryEntry{{Kind: domain.EntryFile}}, nil, 0); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid removed content error = %v", err)
	}
	if _, _, err := updateDirectoryContentIdentityAtCount(emptyAccumulator, nil, []storageformat.DirectoryEntry{{Kind: domain.EntryFile}}, 1); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid added content error = %v", err)
	}
	if err := applyDirectoryEntryChangeWithContent(map[string]directoryUpdate{}, nil, nil, nil, nil, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty directory change error = %v", err)
	}
	if err := applyDirectoryEntryChangeWithContent(map[string]directoryUpdate{}, []directoryTrailNode{emptyNode}, &file, &other, nil, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("renamed directory change error = %v", err)
	}
	if err := applyDirectoryEntryChangeWithContent(map[string]directoryUpdate{}, []directoryTrailNode{emptyNode}, nil, &file, nil, []relativeDirectoryContentFile{{entry: storageformat.DirectoryEntry{Kind: domain.EntryDirectory}}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("non-file content change error = %v", err)
	}
	badBefore := file
	badBefore.Size = -1
	if err := applyDirectoryEntryChangeWithContent(map[string]directoryUpdate{}, []directoryTrailNode{emptyNode}, &badBefore, nil, nil, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid before aggregate error = %v", err)
	}
	badAfter := file
	badAfter.Size = -1
	if err := applyDirectoryEntryChangeWithContent(map[string]directoryUpdate{}, []directoryTrailNode{emptyNode}, nil, &badAfter, nil, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid after aggregate error = %v", err)
	}
	overflowNode := emptyNode
	overflowNode.snapshot.recursiveBytes = math.MaxInt64
	if err := applyDirectoryEntryChangeWithContent(map[string]directoryUpdate{}, []directoryTrailNode{overflowNode}, nil, &file, nil, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("recursive byte overflow error = %v", err)
	}
	overflowFilesNode := emptyNode
	overflowFilesNode.snapshot.recursiveFileCount = math.MaxInt64
	if err := applyDirectoryEntryChangeWithContent(map[string]directoryUpdate{}, []directoryTrailNode{overflowFilesNode}, nil, &file, nil, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("recursive file-count overflow error = %v", err)
	}
	if err := applyDirectoryEntryChangeWithContent(map[string]directoryUpdate{}, []directoryTrailNode{emptyNode}, &file, nil, nil, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("entry-count underflow error = %v", err)
	}
	invalidAccumulatorNode := emptyNode
	invalidAccumulatorNode.snapshot.manifest.EntryCount = 1
	invalidAccumulatorNode.snapshot.contentAccumulator = "invalid"
	if err := applyDirectoryEntryChangeWithContent(map[string]directoryUpdate{}, []directoryTrailNode{invalidAccumulatorNode}, &file, nil, nil, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("composed accumulator error = %v", err)
	}
	updates := make(map[string]directoryUpdate)
	if err := applyDirectoryEntryChangeWithContent(updates, []directoryTrailNode{emptyNode}, nil, &file, nil, []relativeDirectoryContentFile{{entry: file}}); err != nil {
		t.Fatal(err)
	}
	changedFile := file
	changedFile.Size = 2
	changedFile.LogicalVersion, _ = directoryEntryVersion(changedFile)
	if err := applyDirectoryEntryChangeWithContent(updates, []directoryTrailNode{emptyNode}, &changedFile, nil, []relativeDirectoryContentFile{{entry: changedFile}}, nil); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("composed entry mismatch error = %v", err)
	}
	badContent := file
	badContent.MD5 = "invalid"
	if err := applyDirectoryEntryChangeWithContent(map[string]directoryUpdate{}, []directoryTrailNode{emptyNode}, nil, &file, nil, []relativeDirectoryContentFile{{entry: badContent}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid content occurrence error = %v", err)
	}
	childNode := emptyNode
	childNode.path = domain.MustParseUserPath("/child")
	childNode.directoryID = "child"
	childNode.entry = storageformat.DirectoryEntry{Kind: domain.EntryFile, DirectoryID: "child"}
	if err := applyDirectoryEntryChangeWithContent(map[string]directoryUpdate{}, []directoryTrailNode{emptyNode, childNode}, nil, &file, nil, []relativeDirectoryContentFile{{entry: file}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid aggregate ancestor error = %v", err)
	}

	changes := make(map[string]directoryContentIndexMutation)
	if err := applyDirectoryContentChanges(changes, []string{"folder"}, nil, []relativeDirectoryContentFile{{entry: file}}); err != nil || len(changes) != 1 {
		t.Fatalf("new content change = %+v, %v", changes, err)
	}
	if err := applyDirectoryContentChanges(changes, []string{"folder"}, []relativeDirectoryContentFile{{entry: file}}, nil); err != nil || len(changes) != 0 {
		t.Fatalf("collapsed content change = %+v, %v", changes, err)
	}
	if err := applyDirectoryContentChanges(changes, []string{"folder"}, []relativeDirectoryContentFile{{segments: []string{"."}, entry: file}}, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid before content path error = %v", err)
	}
	if err := applyDirectoryContentChanges(changes, []string{"folder"}, nil, []relativeDirectoryContentFile{{segments: []string{"."}, entry: file}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid after content path error = %v", err)
	}
	if err := applyDirectoryContentChanges(changes, []string{"folder"}, nil, []relativeDirectoryContentFile{{entry: badContent}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid content index entry error = %v", err)
	}
	if _, err := files.prepareDirectoryMutation(ctx, directoryUpdate{}, 1); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid prepared mutation error = %v", err)
	}
	if _, err := files.prepareDirectoryWithIndexAggregates(scope, storageformat.RootDirectoryID, -1, 0, 0, 1, storageformat.DirectoryIndexChild{}, nil, storageformat.DirectoryContentIndexChild{}, nil, emptyAccumulator, emptyDigest); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid directory aggregates error = %v", err)
	}
	if _, err := files.prepareDirectoryWithIndexAggregates(scope, storageformat.RootDirectoryID, 0, 0, 0, 1, storageformat.DirectoryIndexChild{}, nil, storageformat.DirectoryContentIndexChild{}, nil, "invalid", emptyDigest); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid prepared accumulator error = %v", err)
	}
	if _, err := files.prepareDirectoryWithIndexAggregates(scope, storageformat.RootDirectoryID, 0, 0, 0, 1, storageformat.DirectoryIndexChild{}, nil, storageformat.DirectoryContentIndexChild{}, nil, emptyAccumulator, "wrong"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("prepared content digest mismatch error = %v", err)
	}
	engine.ids = domain.NewIDGenerator(strings.NewReader("short"))
	if _, err := files.prepareDirectoryWithIndexAggregates(scope, storageformat.RootDirectoryID, 0, 0, 0, 1, storageformat.DirectoryIndexChild{}, nil, storageformat.DirectoryContentIndexChild{}, nil, emptyAccumulator, emptyDigest); err == nil {
		t.Fatal("prepared directory accepted unavailable secure randomness")
	}
	if _, err := files.encodeListCursor(listCursor{DirectoryPath: strings.Repeat("x", storageformat.MaxCanonicalBytes)}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized file cursor error = %v", err)
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
	t.Run("immutable-get-fails", func(t *testing.T) {
		backend := objectmemory.New()
		hooks := &hookedBackend{Backend: backend, put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return "", domain.NewError(domain.ErrorConflict, "exists")
		}, get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "get denied")
		}}
		engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("immutable-get", 1<<15)))
		engine.backend = hooks
		if err := engine.Files().ensureImmutableOperationObject(ctx, objectstore.MustKey("endlessfs/v1/test/immutable-get"), []byte("body")); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("immutable get error = %v", err)
		}
	})
	backend := objectmemory.New()
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("start-operation", 1<<15)))
	user, _ := domain.ParseUserID("bGxsbGxsbGxsbGxsbGxsbA")
	operation := storageformat.FileOperation{SchemaVersion: 1, UserID: user.String(), OperationID: "start", Kind: "copy", State: storageformat.FileOperationRunning, Roots: []storageformat.FileOperationRoot{{Key: "root"}}}
	operationKey := storageformat.OperationKey(operation.UserID, operation.OperationID)
	operationBody := encodeInternalEnvelope(t, fileOperationSchema, operationKey, 1, operation)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := engine.Files().startFileOperation(canceled, operation, operationBody, "", ""); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("legacy start cancellation error = %v", err)
	}
	if _, err := engine.Files().startFileOperation(canceled, operation, operationBody, "idempotency", "fingerprint"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("idempotent start cancellation error = %v", err)
	}
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
	engine.backend = backend
	largeKindOperation := operation
	largeKindOperation.Kind = strings.Repeat("x", storageformat.MaxCanonicalBytes)
	if _, err := engine.Files().startFileOperation(ctx, largeKindOperation, operationBody, "idempotency", "fingerprint"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized idempotency binding error = %v", err)
	}
}

func TestDirectoryManifestPreparationAndReplacementBoundaries(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2047, 1, 2, 3, 4, 15, 0, time.UTC))
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(coverageRandomReader(101, 1<<17))); err != nil {
		t.Fatal(err)
	}
	engine := openInternalTestEngine(t, backend, clock, coverageRandomReader(102, 1<<17))
	files := engine.Files()
	user, _ := domain.ParseUserID("bW1tbW1tbW1tbW1tbW1tbQ")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	file := withCurrentTestFingerprint(storageformat.DirectoryEntry{Name: "replace.bin", NameDigest: storageformat.NameDigest("replace.bin"), Kind: domain.EntryFile, BlobID: "blob", Size: 3, MediaType: "application/octet-stream", ModifiedAt: clock.Now()})
	capability, err := files.CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/replace.bin"), Size: 3, MediaType: "application/octet-stream", IdempotencyKey: "replace-upload"})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(capability.Method, capability.URL, bytes.NewReader([]byte("abc")))
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	uploaded, err := files.CompleteUpload(ctx, scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: domain.MustParseUserPath("/replace.bin"), Size: 3, MediaType: "application/octet-stream"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := files.resolveDirectoryMetadataTrail(ctx, scope, domain.MustParseUserPath("/replace.bin")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("file used as directory error = %v", err)
	}
	replaced, err := files.CreateDirectory(ctx, scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/replace.bin"), Conflict: domain.ConflictReplace, ExpectedVersion: uploaded.Version})
	if err != nil || replaced.Kind != domain.EntryDirectory || replaced.Size != 0 {
		t.Fatalf("file-to-directory replacement = %+v, %v", replaced, err)
	}

	legacyManifest := storageformat.DirectoryManifest{SchemaVersion: 1, DirectoryID: "legacy", ManifestID: "manifest", EntryCount: 1, PageIDs: []string{"missing"}}
	if _, err := files.readManifestPageEntries(ctx, scope, "legacy", legacyManifest); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing legacy page error = %v", err)
	}
	pageKey := storageformat.DirectoryPageKey(user.String(), "live", "legacy", "malformed")
	if _, err := backend.Put(ctx, pageKey, []byte("not-json"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	legacyManifest.PageIDs = []string{"malformed"}
	if _, err := files.readManifestPageEntries(ctx, scope, "legacy", legacyManifest); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("malformed legacy page error = %v", err)
	}
	invalidPageKey := storageformat.DirectoryPageKey(user.String(), "live", "legacy", "invalid")
	invalidPage := storageformat.DirectoryPage{SchemaVersion: 0, DirectoryID: "legacy", PageID: "invalid"}
	if _, err := backend.Put(ctx, invalidPageKey, encodeInternalEnvelope(t, directoryPageSchema, invalidPageKey, 1, invalidPage), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	legacyManifest.PageIDs = []string{"invalid"}
	if _, err := files.readManifestPageEntries(ctx, scope, "legacy", legacyManifest); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid legacy page error = %v", err)
	}
	legacyManifest.PageIDs = nil
	if _, err := files.readManifestPageEntries(ctx, scope, "legacy", legacyManifest); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("legacy page count error = %v", err)
	}
	badEntry := file
	badEntry.LogicalVersion = "wrong"
	badEntriesKey := storageformat.DirectoryPageKey(user.String(), "live", "legacy", "bad-entries")
	badEntriesPage := storageformat.DirectoryPage{SchemaVersion: 1, DirectoryID: "legacy", PageID: "bad-entries", Entries: []storageformat.DirectoryEntry{badEntry}}
	if _, err := backend.Put(ctx, badEntriesKey, encodeInternalEnvelope(t, directoryPageSchema, badEntriesKey, 1, badEntriesPage), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	legacyManifest.PageIDs = []string{"bad-entries"}
	if _, err := files.readManifestPageEntries(ctx, scope, "legacy", legacyManifest); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid legacy entries error = %v", err)
	}
	if _, err := files.readManifestPageEntries(ctx, scope, "invalid-v2", storageformat.DirectoryManifest{SchemaVersion: 2, DirectoryID: "invalid-v2", EntryCount: 1}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid v2 directory index error = %v", err)
	}

	if _, err := files.prepareDirectoryWithContentEntries(scope, "directory", []storageformat.DirectoryEntry{{}}, nil, 1); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid prepared entries error = %v", err)
	}
	legacyFile := storageformat.DirectoryEntry{Name: "legacy.bin", NameDigest: storageformat.NameDigest("legacy.bin"), Kind: domain.EntryFile, BlobID: "legacy", Size: 1, MediaType: "application/octet-stream", ModifiedAt: clock.Now()}
	legacyFile.LogicalVersion, _ = directoryEntryVersion(legacyFile)
	if _, err := files.prepareDirectoryWithContentEntries(scope, "directory", []storageformat.DirectoryEntry{legacyFile}, nil, 1); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("missing content fingerprint error = %v", err)
	}
	if _, err := files.prepareDirectoryWithContentEntries(scope, "directory", []storageformat.DirectoryEntry{file}, []storageformat.DirectoryContentIndexEntry{{}}, 1); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid content index build error = %v", err)
	}
	emptyAccumulator, emptyDigest, _ := directoryContentIdentity(nil)
	badEmptyUpdate := directoryUpdate{
		scope: scope, directoryID: storageformat.RootDirectoryID, changes: map[string]directoryEntryMutation{"x": {}},
		entryCount: 0, snapshot: directorySnapshot{exists: true, recursiveBytes: 1, manifest: storageformat.DirectoryManifest{}, contentAccumulator: emptyAccumulator, contentDigest: emptyDigest},
	}
	if _, err := files.prepareDirectoryMutation(ctx, badEmptyUpdate, 1); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid empty mutation source error = %v", err)
	}
	maxEntry := file
	maxEntry.Size = math.MaxInt64
	oneEntry := file
	oneEntry.Name = "one.bin"
	oneEntry.NameDigest = storageformat.NameDigest(oneEntry.Name)
	oneEntry.Size = 1
	oneEntry.LogicalVersion, _ = directoryEntryVersion(oneEntry)
	if _, err := files.prepareDirectoryWithIndex(scope, "directory", []storageformat.DirectoryEntry{maxEntry, oneEntry}, 1, storageformat.DirectoryIndexChild{}, nil, storageformat.DirectoryContentIndexChild{}, nil, emptyAccumulator, emptyDigest); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("prepared recursive byte overflow error = %v", err)
	}
	maxDirectory := storageformat.DirectoryEntry{Name: "max", NameDigest: storageformat.NameDigest("max"), Kind: domain.EntryDirectory, DirectoryID: "max", FileCount: math.MaxInt64, ContentDigest: emptyDigest, ModifiedAt: clock.Now()}
	maxDirectory.LogicalVersion, _ = directoryEntryVersion(maxDirectory)
	oneDirectory := maxDirectory
	oneDirectory.Name, oneDirectory.NameDigest, oneDirectory.DirectoryID, oneDirectory.FileCount = "one", storageformat.NameDigest("one"), "one", 1
	oneDirectory.LogicalVersion, _ = directoryEntryVersion(oneDirectory)
	if _, err := files.prepareDirectoryWithIndex(scope, "directory", []storageformat.DirectoryEntry{maxDirectory, oneDirectory}, 1, storageformat.DirectoryIndexChild{}, nil, storageformat.DirectoryContentIndexChild{}, nil, emptyAccumulator, emptyDigest); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("prepared recursive file-count overflow error = %v", err)
	}

	engine.ids = domain.NewIDGenerator(strings.NewReader(strings.Repeat("cursor-setup", 1<<15)))
	longName := strings.Repeat("x", 250) + ".bin"
	longEntry := file
	longEntry.Name, longEntry.NameDigest = longName, storageformat.NameDigest(longName)
	longEntry.LogicalVersion, _ = directoryEntryVersion(longEntry)
	longPrepared, err := files.prepareDirectory(ctx, scope, "rename", []storageformat.DirectoryEntry{longEntry}, 1)
	if err != nil {
		t.Fatal(err)
	}
	var longManifest storageformat.DirectoryManifest
	for _, prerequisite := range longPrepared.prerequisites {
		if strings.Contains(prerequisite.Key, "/manifests/") {
			key := objectstore.MustKey(prerequisite.Key)
			var envelope storageformat.Envelope
			if err := storageformat.DecodeEnvelope(prerequisite.Body, key, directoryManifestSchema, &envelope, &longManifest); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := backend.Put(ctx, objectstore.MustKey(prerequisite.Key), prerequisite.Body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}
	requested := domain.MustParseUserPath("/" + longName)
	renamed, _, err := files.resolveIndexedDirectoryDestination(ctx, scope, "rename", longManifest, requested, domain.ConflictRename, "")
	if err != nil || renamed == requested || len(renamed.Name()) > 255 {
		t.Fatalf("indexed long rename = %q, %v", renamed.String(), err)
	}
	if _, _, err := files.resolveIndexedDirectoryDestination(ctx, scope, "rename", longManifest, requested, "invalid", ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid indexed conflict mode error = %v", err)
	}
	if _, _, err := files.startPreparingCreateDirectoryReplacement(ctx, scope, domain.MustParseUserPath("/invalid"), domain.CreateDirectoryRequest{}, nil, file); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid recursive replacement source error = %v", err)
	}
	validDirectory := storageformat.DirectoryEntry{Name: "directory", NameDigest: storageformat.NameDigest("directory"), Kind: domain.EntryDirectory, DirectoryID: "directory", ContentDigest: emptyDigest, ModifiedAt: clock.Now()}
	validDirectory.LogicalVersion, _ = directoryEntryVersion(validDirectory)
	parentTrail := []directoryTrailNode{{scope: scope, path: domain.MustParseUserPath("/"), directoryID: storageformat.RootDirectoryID}}
	engine.ids = domain.NewIDGenerator(strings.NewReader("short"))
	if _, _, err := files.startPreparingCreateDirectoryReplacement(ctx, scope, domain.MustParseUserPath("/directory"), domain.CreateDirectoryRequest{}, parentTrail, validDirectory); err == nil {
		t.Fatal("recursive replacement accepted unavailable operation ID randomness")
	}
	engine.ids = domain.NewIDGenerator(strings.NewReader(strings.Repeat("x", 16)))
	if _, _, err := files.startPreparingCreateDirectoryReplacement(ctx, scope, domain.MustParseUserPath("/directory"), domain.CreateDirectoryRequest{}, parentTrail, validDirectory); err == nil {
		t.Fatal("recursive replacement accepted unavailable owner ID randomness")
	}
	gateHooks := &hookedBackend{Backend: backend, get: func(_ context.Context, key objectstore.Key) (objectstore.Object, error) {
		if key == storageformat.WriteGateKey() {
			return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "gate denied")
		}
		return backend.Get(ctx, key)
	}}
	engine.backend = gateHooks
	engine.ids = domain.NewIDGenerator(coverageRandomReader(103, 1<<15))
	if _, _, err := files.startPreparingCreateDirectoryReplacement(ctx, scope, domain.MustParseUserPath("/directory"), domain.CreateDirectoryRequest{}, parentTrail, validDirectory); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("recursive replacement gate error = %v", err)
	}
	engine.backend = backend

	underflowNode := directoryTrailNode{scope: scope, path: domain.MustParseUserPath("/"), directoryID: storageformat.RootDirectoryID, snapshot: directorySnapshot{
		manifest: storageformat.DirectoryManifest{EntryCount: 0}, recursiveBytes: file.Size, recursiveFileCount: 1,
	}}
	underflowNode.snapshot.contentAccumulator, underflowNode.snapshot.contentDigest, _ = directoryContentIdentity([]storageformat.DirectoryEntry{file})
	if err := applyDirectoryEntryChangeWithContent(map[string]directoryUpdate{}, []directoryTrailNode{underflowNode}, &file, nil, []relativeDirectoryContentFile{{entry: file}}, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("directory entry-count underflow error = %v", err)
	}
	composed := make(map[string]directoryContentIndexMutation)
	if err := applyDirectoryContentChanges(composed, []string{"folder"}, nil, []relativeDirectoryContentFile{{entry: file}}); err != nil {
		t.Fatal(err)
	}
	for key, change := range composed {
		mutated := *change.after
		mutated.Size++
		change.after = &mutated
		composed[key] = change
	}
	if err := applyDirectoryContentChanges(composed, []string{"folder"}, []relativeDirectoryContentFile{{entry: file}}, nil); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("composed content occurrence mismatch error = %v", err)
	}
	if _, err := encodeListCursor(listCursor{DirectoryPath: strings.Repeat("x", storageformat.MaxCanonicalBytes)}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized portable list cursor error = %v", err)
	}
	engine.ids = domain.NewIDGenerator(strings.NewReader("short"))
	if _, err := files.encodeListCursor(listCursor{SchemaVersion: 3}); err == nil {
		t.Fatal("file cursor accepted unavailable secure randomness")
	}
	if _, err := files.decodeListCursor(base64.RawURLEncoding.EncodeToString(make([]byte, engine.cursorAEAD.NonceSize()+17))); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid authenticated file cursor error = %v", err)
	}
	engine.ids = domain.NewIDGenerator(coverageRandomReader(104, 1<<16))
	view, err := files.resolveDirectoryMetadataView(ctx, scope, domain.MustParseUserPath("/replace.bin"))
	if err != nil {
		t.Fatal(err)
	}
	_, gateEnvelope, gate, err := engine.readGate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	encodeCursor := func(path, directoryID, manifestID, parentID, parentManifest string) string {
		t.Helper()
		value, encodeErr := files.encodeListCursor(listCursor{
			SchemaVersion: 3, UserID: user.String(), Area: "live", DirectoryPath: path, DirectoryID: directoryID, ManifestID: manifestID,
			ParentID: parentID, ParentManifest: parentManifest, PageSize: 1, Sort: domain.SortName, AfterName: "after",
			GateEpoch: gate.Epoch, GateVersion: gateEnvelope.LogicalVersion, ExpiresAt: clock.Now().Add(time.Minute),
		})
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		return value
	}
	missingCursor := encodeCursor("/missing", view.directoryID, view.snapshot.manifestID, storageformat.RootDirectoryID, "parent")
	if _, err := files.List(ctx, scope, domain.ListRequest{Directory: domain.MustParseUserPath("/missing"), PageSize: 1, Sort: domain.SortName, Cursor: missingCursor}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing cursor directory error = %v", err)
	}
	replacedCursor := encodeCursor("/replace.bin", "different", view.snapshot.manifestID, storageformat.RootDirectoryID, "parent")
	if _, err := files.List(ctx, scope, domain.ListRequest{Directory: domain.MustParseUserPath("/replace.bin"), PageSize: 1, Sort: domain.SortName, Cursor: replacedCursor}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("replaced cursor directory error = %v", err)
	}
	historicalCursor := encodeCursor("/replace.bin", view.directoryID, view.snapshot.manifestID, storageformat.RootDirectoryID, "missing-parent-manifest")
	if _, err := files.List(ctx, scope, domain.ListRequest{Directory: domain.MustParseUserPath("/replace.bin"), PageSize: 1, Sort: domain.SortName, Cursor: historicalCursor}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing historical cursor parent error = %v", err)
	}
	if _, err := files.CreateDirectory(ctx, scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/second")}); err != nil {
		t.Fatal(err)
	}
	engine.ids = domain.NewIDGenerator(strings.NewReader("short"))
	for _, sortField := range []domain.SortField{domain.SortName, domain.SortSize} {
		if _, err := files.List(ctx, scope, domain.ListRequest{Directory: domain.MustParseUserPath("/"), PageSize: 1, Sort: sortField}); err == nil {
			t.Fatalf("%s list cursor accepted unavailable secure randomness", sortField)
		}
	}
	engine.ids = domain.NewIDGenerator(coverageRandomReader(105, 1<<15))
	engine.backend = &hookedBackend{Backend: backend, get: func(_ context.Context, key objectstore.Key) (objectstore.Object, error) {
		if strings.Contains(key.String(), "/sort-index/") {
			return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "sort index denied")
		}
		return backend.Get(ctx, key)
	}}
	if _, err := files.List(ctx, scope, domain.ListRequest{Directory: domain.MustParseUserPath("/"), PageSize: 1, Sort: domain.SortSize}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("sort-index read error = %v", err)
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
