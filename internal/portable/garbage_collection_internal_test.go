package portable

import (
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

func TestClosedGateGarbageCollectionRetainsOnlyReachableImmutableState(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2049, 1, 2, 3, 4, 5, 0, time.UTC))
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("garbage-collection-entropy", 1<<17)))
	user, _ := domain.ParseUserID("a2tra2tra2tra2tra2traw")
	scope, _ := domain.NewScope(user, domain.AreaLive)

	putBlob := func(blobID string, body []byte) storageformat.DirectoryEntry {
		key := storageformat.BlobKey(user.String(), blobID)
		if _, err := backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		fingerprint := objectstore.FingerprintFor(body)
		entry := storageformat.DirectoryEntry{
			Name: "value.bin", NameDigest: storageformat.NameDigest("value.bin"), Kind: domain.EntryFile,
			BlobID: blobID, Size: int64(len(body)), MediaType: "application/octet-stream",
			MD5: fingerprint.MD5, CRC32C: fingerprint.CRC32C, ModifiedAt: clock.Now(),
		}
		entry.LogicalVersion, _ = directoryEntryVersion(entry)
		return entry
	}
	oldEntry := putBlob("old-blob", []byte("old"))
	initial, err := engine.Files().prepareDirectory(ctx, scope, storageformat.RootDirectoryID, []storageformat.DirectoryEntry{oldEntry}, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range initial.prerequisites {
		if _, err := backend.Put(ctx, objectstore.MustKey(object.Key), object.Body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}
	rootKey := storageformat.DirectoryRootKey(user.String(), "live", storageformat.RootDirectoryID)
	if _, err := backend.Put(ctx, rootKey, initial.rootBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := engine.Files().readDirectory(ctx, scope, storageformat.RootDirectoryID, true)
	if err != nil {
		t.Fatal(err)
	}
	newEntry := putBlob("new-blob", []byte("new-value"))
	updated, err := engine.Files().prepareDirectoryUpdate(ctx, scope, storageformat.RootDirectoryID, snapshot, []storageformat.DirectoryEntry{newEntry}, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range updated.prerequisites {
		if _, err := backend.Put(ctx, objectstore.MustKey(object.Key), object.Body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := backend.Put(ctx, rootKey, updated.rootBody, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: snapshot.object.Version}); err != nil {
		t.Fatal(err)
	}

	logical := state.MustKey(state.NamespacePreferences, "gc-value")
	oldVersion, err := engine.Create(ctx, logical, []byte("old"))
	if err != nil {
		t.Fatal(err)
	}
	newVersion, err := engine.CompareAndSwap(ctx, logical, oldVersion, []byte("new"))
	if err != nil {
		t.Fatal(err)
	}
	oldManifestKey := storageformat.DirectoryManifestKey(user.String(), "live", storageformat.RootDirectoryID, initial.manifestID)
	oldBlobKey := storageformat.BlobKey(user.String(), "old-blob")
	oldStateVersionKey := storageformat.StateVersionKey(string(state.NamespacePreferences), logical.String(), string(oldVersion))

	if err := engine.CloseWrites(ctx, "collect-unreachable-state"); err != nil {
		t.Fatal(err)
	}
	for name, key := range map[string]objectstore.Key{"old manifest": oldManifestKey, "old blob": oldBlobKey, "old state version": oldStateVersionKey} {
		if _, err := backend.Get(ctx, key); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("%s remains after collection: %v", name, err)
		}
	}
	for name, key := range map[string]objectstore.Key{
		"current manifest":      storageformat.DirectoryManifestKey(user.String(), "live", storageformat.RootDirectoryID, updated.manifestID),
		"current blob":          storageformat.BlobKey(user.String(), "new-blob"),
		"current state version": storageformat.StateVersionKey(string(state.NamespacePreferences), logical.String(), string(newVersion)),
	} {
		if _, err := backend.Get(ctx, key); err != nil {
			t.Fatalf("%s was collected: %v", name, err)
		}
	}
	marks, err := listAllFrom(ctx, backend, storageformat.GarbageCollectionMarkPrefix("collect-unreachable-state"))
	if err != nil || len(marks) != 0 {
		t.Fatalf("garbage collection marks remain: %d, %v", len(marks), err)
	}
	if _, err := backend.Get(ctx, storageformat.GarbageCollectionSessionKey("collect-unreachable-state")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("garbage collection session remains: %v", err)
	}
}

func TestClosedGateMaintenancePrunesExpiredOperationRetentionPairs(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2049, 2, 3, 4, 5, 6, 0, time.UTC))
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("operation-retention-entropy", 1<<17)))
	user, _ := domain.ParseUserID("bGxsbGxsbGxsbGxsbGxsbA")
	operationID := "retained-operation"
	operationKey := storageformat.OperationKey(user.String(), operationID)
	old := clock.Now().Add(-terminalOperationRetention - time.Hour)
	operation := storageformat.FileOperation{
		SchemaVersion: 1, OperationID: operationID, UserID: user.String(), Kind: operationDelete,
		State: storageformat.FileOperationSucceeded, Attempt: 1, Fence: 1, ReplicaAttemptID: "finished-attempt",
		ExpiresAt: old.Add(time.Minute), StartedAt: old, UpdatedAt: old,
		StepPageCount: 1, StepSetID: "retained-step-set", StepDigest: storageformat.Digest([]byte("retained-step")), StepsStaged: true,
	}
	operationBody, err := storageformat.EncodeEnvelope(fileOperationSchema, operationKey, 1, operation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, operationKey, operationBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	idempotencyKey := storageformat.IdempotencyKey(user.String(), "retained-request-key")
	idempotencyBody, err := storageformat.EncodeEnvelope(idempotencySchema, idempotencyKey, 1, storageformat.IdempotencyRecord{
		SchemaVersion: 1, UserID: user.String(), Kind: operationDelete,
		KeyDigest: storageformat.Digest([]byte("retained-request-key")), Fingerprint: "request-fingerprint", OperationID: operationID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, idempotencyKey, idempotencyBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if err := engine.CloseWrites(ctx, "prune-expired-operation-retention"); err != nil {
		t.Fatal(err)
	}
	for name, key := range map[string]objectstore.Key{"operation": operationKey, "idempotency binding": idempotencyKey} {
		if _, err := backend.Get(ctx, key); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("expired %s remains: %v", name, err)
		}
	}
}

func TestGarbageCollectionMarksBindExactClosedGateVersion(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2049, 3, 4, 5, 6, 7, 0, time.UTC))
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("garbage-mark-binding-entropy", 1<<17)))
	session := storageformat.GarbageCollectionSession{
		SchemaVersion: 1, CheckpointID: "gate-bound-marks", GateEpoch: 7, GateVersion: "exact-gate-version",
		Phase: garbageCollectionMarking, UpdatedAt: clock.Now(),
	}
	target := storageformat.SuperblockKey()
	if err := engine.ensureGarbageCollectionMark(ctx, session, garbageCollectionStateRole, target); err != nil {
		t.Fatal(err)
	}
	markKey := storageformat.GarbageCollectionMarkKey(session.CheckpointID, garbageCollectionStateRole, target.String())
	object, err := backend.Get(ctx, markKey)
	if err != nil {
		t.Fatal(err)
	}
	var envelope storageformat.Envelope
	var mark storageformat.GarbageCollectionMark
	if err := storageformat.DecodeEnvelope(object.Body, markKey, garbageCollectionMarkSchema, &envelope, &mark); err != nil {
		t.Fatal(err)
	}
	if mark.GateVersion != session.GateVersion {
		t.Fatalf("mark gate version = %q; want %q", mark.GateVersion, session.GateVersion)
	}
	stale := session
	stale.GateVersion = "different-gate-version"
	if _, err := engine.garbageCollectionMarked(ctx, stale, garbageCollectionStateRole, target); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("stale gate mark error = %v; want invalid", err)
	}
}
