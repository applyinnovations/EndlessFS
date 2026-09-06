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
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type checksumOmittingListBackend struct {
	objectstore.Backend
	omitFingerprints bool
	headRequests     int
	bodyOpens        int
}

func (backend *checksumOmittingListBackend) List(ctx context.Context, request objectstore.ListRequest) (objectstore.ListPage, error) {
	page, err := backend.Backend.List(ctx, request)
	if err == nil && backend.omitFingerprints {
		for index := range page.Objects {
			page.Objects[index].Fingerprint = objectstore.ContentFingerprint{}
		}
	}
	return page, err
}

func (backend *checksumOmittingListBackend) Head(ctx context.Context, key objectstore.Key) (objectstore.ObjectInfo, error) {
	backend.headRequests++
	return backend.Backend.Head(ctx, key)
}

func (backend *checksumOmittingListBackend) Open(ctx context.Context, key objectstore.Key) (objectstore.ObjectReader, error) {
	backend.bodyOpens++
	return backend.Backend.Open(ctx, key)
}

func TestCheckpointMetadataAttestationDenials(t *testing.T) {
	ctx := context.Background()
	body := []byte("authoritative")
	objectKey := storageformat.BlobKey("owner", "blob")

	t.Run("metadata-head-interruption", func(t *testing.T) {
		backend := &hookedBackend{
			Backend: objectmemory.New(),
			head: func(context.Context, objectstore.Key) (objectstore.ObjectInfo, error) {
				return objectstore.ObjectInfo{}, domain.NewError(domain.ErrorUnavailable, "checkpoint metadata interrupted")
			},
		}
		if _, _, err := streamCheckpointObject(ctx, backend, objectstore.ObjectInfo{Key: objectKey, Size: int64(len(body))}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("streamCheckpointObject() error = %v; want unavailable", err)
		}
	})

	t.Run("metadata-size-changed", func(t *testing.T) {
		backend := &hookedBackend{
			Backend: objectmemory.New(),
			head: func(context.Context, objectstore.Key) (objectstore.ObjectInfo, error) {
				return objectstore.ObjectInfo{Key: objectKey, Size: int64(len(body)) + 1, Fingerprint: objectstore.FingerprintFor(body)}, nil
			},
		}
		if _, _, err := streamCheckpointObject(ctx, backend, objectstore.ObjectInfo{Key: objectKey, Size: int64(len(body))}); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("streamCheckpointObject() error = %v; want precondition failed", err)
		}
	})

	t.Run("metadata-missing-fingerprint", func(t *testing.T) {
		backend := &hookedBackend{
			Backend: objectmemory.New(),
			head: func(context.Context, objectstore.Key) (objectstore.ObjectInfo, error) {
				return objectstore.ObjectInfo{Key: objectKey, Size: int64(len(body))}, nil
			},
		}
		if _, _, err := streamCheckpointObject(ctx, backend, objectstore.ObjectInfo{Key: objectKey, Size: int64(len(body))}); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("streamCheckpointObject() error = %v; want precondition failed", err)
		}
	})

	t.Run("metadata-version-changed", func(t *testing.T) {
		backend := &hookedBackend{
			Backend: objectmemory.New(),
			head: func(context.Context, objectstore.Key) (objectstore.ObjectInfo, error) {
				return objectstore.ObjectInfo{Key: objectKey, Size: int64(len(body)), Version: "changed", Fingerprint: objectstore.FingerprintFor(body)}, nil
			},
		}
		if _, _, err := streamCheckpointObject(ctx, backend, objectstore.ObjectInfo{Key: objectKey, Size: int64(len(body)), Version: "expected"}); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("streamCheckpointObject() error = %v; want precondition failed", err)
		}
	})
}

func TestCheckpointHydratesChecksumsOmittedFromProviderListingsWithoutReadingBodies(t *testing.T) {
	backend := &checksumOmittingListBackend{Backend: objectmemory.New()}
	clock := domain.NewFixedClock(time.Date(2044, 2, 3, 4, 5, 6, 0, time.UTC))
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("metadata-checksums", 1<<14)))
	backend.omitFingerprints = true

	if _, err := engine.CreateCheckpoint(context.Background(), "metadata-checksums"); err != nil {
		t.Fatal(err)
	}
	if backend.headRequests == 0 {
		t.Fatal("checkpoint did not hydrate checksum-less listing entries with metadata heads")
	}
	if backend.bodyOpens != 0 {
		t.Fatalf("checkpoint opened %d object bodies", backend.bodyOpens)
	}
}

func TestMigrationCheckpointReopenRejectsDifferentStoredCheckpoint(t *testing.T) {
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2044, 2, 3, 4, 5, 6, 0, time.UTC))
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("checkpoint-mismatch", 1<<14)))
	checkpoint, err := engine.CreateCheckpoint(context.Background(), "checkpoint-mismatch")
	if err != nil {
		t.Fatal(err)
	}
	checkpoint.InventoryDigest = storageformat.Digest([]byte("different inventory"))
	if err := engine.openWritesAfterCreatedCheckpoint(context.Background(), checkpoint); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("openWritesAfterCreatedCheckpoint() error = %v; want precondition failed", err)
	}
	missing := checkpoint
	missing.CheckpointID = "missing-checkpoint"
	if err := engine.openWritesAfterCreatedCheckpoint(context.Background(), missing); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("openWritesAfterCreatedCheckpoint(missing) error = %v; want not found", err)
	}
}

func TestCheckpointRecordAndMetadataOnlyDenials(t *testing.T) {
	ctx := context.Background()
	const checkpointID = "checkpoint-record-denials"
	checkpointKey := storageformat.CheckpointKey(checkpointID)

	readStored := func(t *testing.T, body []byte) error {
		t.Helper()
		backend := objectmemory.New()
		if _, err := backend.Put(ctx, checkpointKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		_, err := (&Engine{backend: backend}).readCheckpoint(ctx, checkpointID)
		return err
	}

	t.Run("unsupported-envelope-schema", func(t *testing.T) {
		body, err := storageformat.EncodeEnvelope("checkpoint-unknown", checkpointKey, 1, storageformat.Checkpoint{})
		if err != nil {
			t.Fatal(err)
		}
		if err := readStored(t, body); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("readCheckpoint() error = %v; want precondition failed", err)
		}
	})

	t.Run("envelope-bound-to-different-key", func(t *testing.T) {
		otherKey := storageformat.CheckpointKey("different-checkpoint")
		body, err := storageformat.EncodeEnvelope(checkpointSchemaV2, otherKey, 1, storageformat.Checkpoint{})
		if err != nil {
			t.Fatal(err)
		}
		if err := readStored(t, body); err == nil {
			t.Fatal("readCheckpoint() accepted an envelope bound to a different key")
		}
	})

	t.Run("invalid-v1-shape", func(t *testing.T) {
		checkpoint := storageformat.Checkpoint{
			SchemaVersion: 1, CheckpointID: checkpointID, KeyFormatVersion: storageformat.KeyFormatVersion,
			WriterProtocolVersion: storageformat.WriterProtocolVersion, InventoryDigest: "digest",
		}
		body, err := storageformat.EncodeEnvelope(checkpointSchema, checkpointKey, 1, checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		if err := readStored(t, body); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("readCheckpoint() error = %v; want precondition failed", err)
		}
	})

	t.Run("visitor-is-required", func(t *testing.T) {
		engine := &Engine{backend: objectmemory.New()}
		if err := engine.VisitCheckpointObjects(ctx, checkpointID, nil); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("VisitCheckpointObjects() error = %v; want invalid", err)
		}
		if err := engine.VisitCheckpointObjects(ctx, checkpointID, func(storageformat.CheckpointObject) error { return nil }); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("VisitCheckpointObjects(missing) error = %v; want not found", err)
		}
	})

	t.Run("legacy-checkpoint-cannot-verify", func(t *testing.T) {
		backend := objectmemory.New()
		checkpoint := storageformat.Checkpoint{
			SchemaVersion: 1, CheckpointID: checkpointID, KeyFormatVersion: storageformat.KeyFormatVersion,
			WriterProtocolVersion: storageformat.WriterProtocolVersion, WriterSetID: "writer", GateEpoch: 1,
			InventoryDigest: "legacy", Objects: []storageformat.CheckpointObject{{Key: storageformat.SuperblockKey().String(), Size: 1, SHA256: "legacy"}},
		}
		body, err := storageformat.EncodeEnvelope(checkpointSchema, checkpointKey, 1, checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Put(ctx, checkpointKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		gate := storageformat.WriteGate{SchemaVersion: 1, Epoch: 1, Mode: storageformat.GateClosed, CheckpointID: checkpointID}
		gateBody, err := storageformat.EncodeEnvelope(writeGateSchema, storageformat.WriteGateKey(), 1, gate)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Put(ctx, storageformat.WriteGateKey(), gateBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		engine := &Engine{backend: backend}
		if err := engine.VerifyCheckpoint(ctx, checkpointID); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("VerifyCheckpoint(legacy) error = %v; want precondition failed", err)
		}
	})

	t.Run("paged-visitor-and-content-change", func(t *testing.T) {
		backend := objectmemory.New()
		clock := domain.NewFixedClock(time.Date(2044, 2, 3, 4, 5, 6, 0, time.UTC))
		engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat(t.Name(), 1<<14)))
		key := objectstore.MustKey("endlessfs/v1/test/mutable-record.json")
		original := []byte("original")
		if _, err := backend.Put(ctx, key, original, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		checkpoint, err := engine.CreateCheckpoint(ctx, checkpointID)
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.VisitCheckpointObjects(ctx, checkpointID, func(storageformat.CheckpointObject) error {
			return domain.NewError(domain.ErrorUnavailable, "checkpoint visitor interrupted")
		}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("VisitCheckpointObjects() error = %v; want unavailable", err)
		}
		mismatchedSummary := checkpoint
		mismatchedSummary.StateObjectCount++
		if err := engine.visitCheckpointInventory(ctx, mismatchedSummary, nil); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("visitCheckpointInventory(summary) error = %v; want precondition failed", err)
		}
		info, err := backend.Head(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if err := backend.Delete(ctx, key, objectstore.DeleteCondition{Version: info.Version}); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Put(ctx, key, []byte("changed!"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := engine.verifyCheckpointV3(ctx, checkpoint, storageformat.WriteGate{Mode: storageformat.GateClosed, CheckpointID: checkpointID, Epoch: checkpoint.GateEpoch}); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("verifyCheckpointV3() error = %v; want precondition failed", err)
		}
	})
}
