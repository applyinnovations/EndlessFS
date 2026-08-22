package portable

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestCheckpointWorkRestartRejectsMissingAndMisplacedObjects(t *testing.T) {
	ctx := context.Background()
	checkpointID := "checkpoint-restart-denial"
	const gateEpoch = 9
	workKey := bytes.Repeat([]byte{0x45}, 32)
	body := []byte("authoritative")
	integrity := objectstore.IntegrityFor(body)
	objectKey := storageformat.BlobKey("owner", "blob")

	newWork := func(fileData bool) storageformat.CheckpointWork {
		return storageformat.CheckpointWork{
			SchemaVersion: 1,
			CheckpointID:  checkpointID,
			GateEpoch:     gateEpoch,
			FileData:      fileData,
			Object: storageformat.CheckpointObject{
				Key: objectKey.String(), Size: int64(len(body)), SHA256: storageformat.Digest(body),
			},
			CRC32C: integrity.Checksum.Value,
		}
	}

	t.Run("missing-authoritative-object", func(t *testing.T) {
		backend := objectmemory.New()
		engine := &Engine{backend: backend, fileBackend: backend, checkpointWorkKey: workKey}
		writeCheckpointWorkForTest(t, engine, newWork(false))
		if _, err := engine.prepareCheckpointInventory(ctx, checkpointID, gateEpoch); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("prepareCheckpointInventory() error = %v; want precondition failed", err)
		}
	})

	t.Run("wrong-backend-role", func(t *testing.T) {
		stateBackend := objectmemory.New()
		fileBackend := objectmemory.New()
		if _, err := fileBackend.Put(ctx, objectKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		engine := &Engine{
			backend: stateBackend, fileBackend: fileBackend, separateFileBackend: true,
			checkpointWorkKey: workKey,
		}
		writeCheckpointWorkForTest(t, engine, newWork(false))
		if _, err := engine.prepareCheckpointInventory(ctx, checkpointID, gateEpoch); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("prepareCheckpointInventory() error = %v; want precondition failed", err)
		}
	})

	t.Run("missing-authentication-key", func(t *testing.T) {
		if _, err := (&Engine{}).checkpointWorkProof(newWork(false)); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("checkpointWorkProof() error = %v; want invalid", err)
		}
	})

	t.Run("malformed-work-envelope", func(t *testing.T) {
		backend := objectmemory.New()
		key := storageformat.CheckpointWorkKey(checkpointID, objectKey.String())
		if _, err := backend.Put(ctx, key, []byte("{"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		engine := &Engine{backend: backend, checkpointWorkKey: workKey}
		if _, _, err := engine.readCheckpointWorkEntry(ctx, checkpointID, gateEpoch, objectKey.String()); err == nil {
			t.Fatal("readCheckpointWorkEntry() accepted a malformed work envelope")
		}
	})

	t.Run("invalid-work-schema", func(t *testing.T) {
		backend := objectmemory.New()
		work := newWork(false)
		work.SchemaVersion = 2
		key := storageformat.CheckpointWorkKey(checkpointID, objectKey.String())
		encoded, err := storageformat.EncodeEnvelope(checkpointWorkSchema, key, 1, work)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Put(ctx, key, encoded, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		engine := &Engine{backend: backend, checkpointWorkKey: workKey}
		if _, _, err := engine.readCheckpointWorkEntry(ctx, checkpointID, gateEpoch, objectKey.String()); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("readCheckpointWorkEntry() error = %v; want precondition failed", err)
		}
	})

	t.Run("invalid-work-authentication", func(t *testing.T) {
		backend := objectmemory.New()
		work := newWork(false)
		work.Proof = "forged-proof"
		key := storageformat.CheckpointWorkKey(checkpointID, objectKey.String())
		encoded, err := storageformat.EncodeEnvelope(checkpointWorkSchema, key, 1, work)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Put(ctx, key, encoded, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		engine := &Engine{backend: backend, checkpointWorkKey: workKey}
		if _, _, err := engine.readCheckpointWorkEntry(ctx, checkpointID, gateEpoch, objectKey.String()); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("readCheckpointWorkEntry() error = %v; want precondition failed", err)
		}
	})

	t.Run("duplicate-work-list-entry", func(t *testing.T) {
		memory := objectmemory.New()
		engine := &Engine{backend: memory, checkpointWorkKey: workKey}
		writeCheckpointWorkForTest(t, engine, newWork(false))
		key := storageformat.CheckpointWorkKey(checkpointID, objectKey.String())
		object, err := memory.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		backend := &hookedBackend{
			Backend: memory,
			list: func(context.Context, objectstore.ListRequest) (objectstore.ListPage, error) {
				info := objectstore.ObjectInfo{Key: key, Size: int64(len(object.Body)), Version: object.Version}
				return objectstore.ListPage{Objects: []objectstore.ObjectInfo{info, info}}, nil
			},
		}
		engine.backend = backend
		if err := walkObjectInfos(ctx, backend, storageformat.CheckpointWorkPrefix(checkpointID), func(objectstore.ObjectInfo) error { return nil }); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("walkObjectInfos() error = %v; want precondition failed", err)
		}
	})

	t.Run("work-list-interruption", func(t *testing.T) {
		backend := &hookedBackend{
			Backend: objectmemory.New(),
			list: func(context.Context, objectstore.ListRequest) (objectstore.ListPage, error) {
				return objectstore.ListPage{}, domain.NewError(domain.ErrorUnavailable, "checkpoint work list interrupted")
			},
		}
		engine := &Engine{backend: backend, checkpointWorkKey: workKey}
		if _, err := engine.prepareCheckpointInventory(ctx, checkpointID, gateEpoch); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("prepareCheckpointInventory() error = %v; want unavailable", err)
		}
	})

	t.Run("work-body-interruption", func(t *testing.T) {
		memory := objectmemory.New()
		key := storageformat.CheckpointWorkKey(checkpointID, objectKey.String())
		if _, err := memory.Put(ctx, key, []byte("placeholder"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		backend := &hookedBackend{
			Backend: memory,
			get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
				return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "checkpoint work body interrupted")
			},
		}
		engine := &Engine{backend: backend, checkpointWorkKey: workKey}
		if _, _, err := engine.readCheckpointWorkEntry(ctx, checkpointID, gateEpoch, objectKey.String()); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("readCheckpointWorkEntry() error = %v; want unavailable", err)
		}
	})

	t.Run("resume-integrity-interruption", func(t *testing.T) {
		memory := objectmemory.New()
		if _, err := memory.Put(ctx, objectKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		backend := &checkpointVerifyUnavailableBackend{Backend: memory}
		engine := &Engine{backend: backend, fileBackend: backend, checkpointWorkKey: workKey}
		writeCheckpointWorkForTest(t, engine, newWork(true))
		if _, err := engine.checkpointInventoryRole(ctx, backend, true, checkpointID, gateEpoch); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("checkpointInventoryRole() error = %v; want unavailable", err)
		}
	})

	t.Run("resume-metadata-mismatch", func(t *testing.T) {
		backend := objectmemory.New()
		if _, err := backend.Put(ctx, objectKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		work := newWork(true)
		work.Object.Size++
		engine := &Engine{backend: backend, fileBackend: backend, checkpointWorkKey: workKey}
		writeCheckpointWorkForTest(t, engine, work)
		if _, err := engine.checkpointInventoryRole(ctx, backend, true, checkpointID, gateEpoch); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("checkpointInventoryRole() error = %v; want precondition failed", err)
		}
	})

	t.Run("authoritative-size-changed", func(t *testing.T) {
		memory := objectmemory.New()
		if _, err := memory.Put(ctx, objectKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		backend := &hookedBackend{
			Backend: memory,
			list: func(context.Context, objectstore.ListRequest) (objectstore.ListPage, error) {
				return objectstore.ListPage{Objects: []objectstore.ObjectInfo{{Key: objectKey, Size: int64(len(body) + 1)}}}, nil
			},
		}
		engine := &Engine{backend: backend, fileBackend: backend, checkpointWorkKey: workKey}
		if _, err := engine.checkpointInventoryRole(ctx, backend, true, checkpointID, gateEpoch); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("checkpointInventoryRole() error = %v; want precondition failed", err)
		}
	})

	t.Run("file-backend-inventory-interruption", func(t *testing.T) {
		stateBackend := objectmemory.New()
		fileBackend := &hookedBackend{
			Backend: objectmemory.New(),
			list: func(context.Context, objectstore.ListRequest) (objectstore.ListPage, error) {
				return objectstore.ListPage{}, domain.NewError(domain.ErrorUnavailable, "file inventory interrupted")
			},
		}
		engine := &Engine{
			backend: stateBackend, fileBackend: fileBackend, separateFileBackend: true,
			checkpointWorkKey: workKey,
		}
		if _, err := engine.prepareCheckpointInventory(ctx, checkpointID, gateEpoch); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("prepareCheckpointInventory() error = %v; want unavailable", err)
		}
	})

	t.Run("negative-inventory-size", func(t *testing.T) {
		backend := &hookedBackend{
			Backend: objectmemory.New(),
			list: func(context.Context, objectstore.ListRequest) (objectstore.ListPage, error) {
				return objectstore.ListPage{Objects: []objectstore.ObjectInfo{{Key: objectKey, Size: -1}}}, nil
			},
		}
		engine := &Engine{backend: backend, fileBackend: backend, checkpointWorkKey: workKey}
		if _, _, err := engine.checkpointRoleTotals(ctx, backend, true); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("checkpointRoleTotals() error = %v; want precondition failed", err)
		}
	})

	t.Run("bounded-observability-values", func(t *testing.T) {
		if boundedProgressCount(math.MaxUint64) != int(^uint(0)>>1) {
			t.Fatal("boundedProgressCount() did not saturate at the platform int maximum")
		}
		if saturatingInventoryBytes(1, -1) != math.MaxInt64 || saturatingInventoryBytes(math.MaxInt64, 1) != math.MaxInt64 {
			t.Fatal("saturatingInventoryBytes() did not saturate invalid or overflowing totals")
		}
	})

	t.Run("stream-open-interruption", func(t *testing.T) {
		backend := &hookedBackend{
			Backend: objectmemory.New(),
			open: func(context.Context, objectstore.Key) (objectstore.ObjectReader, error) {
				return objectstore.ObjectReader{}, domain.NewError(domain.ErrorUnavailable, "checkpoint open interrupted")
			},
		}
		if _, _, err := streamCheckpointObject(ctx, backend, objectstore.ObjectInfo{Key: objectKey, Size: int64(len(body))}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("streamCheckpointObject() error = %v; want unavailable", err)
		}
	})

	t.Run("stream-read-interruption", func(t *testing.T) {
		backend := &hookedBackend{
			Backend: objectmemory.New(),
			open: func(context.Context, objectstore.Key) (objectstore.ObjectReader, error) {
				return objectstore.ObjectReader{Key: objectKey, Size: int64(len(body)), Body: &checkpointFaultReader{readErr: domain.NewError(domain.ErrorUnavailable, "checkpoint read interrupted")}}, nil
			},
		}
		if _, _, err := streamCheckpointObject(ctx, backend, objectstore.ObjectInfo{Key: objectKey, Size: int64(len(body))}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("streamCheckpointObject() error = %v; want unavailable", err)
		}
	})

	t.Run("stream-close-interruption", func(t *testing.T) {
		backend := &hookedBackend{
			Backend: objectmemory.New(),
			open: func(context.Context, objectstore.Key) (objectstore.ObjectReader, error) {
				return objectstore.ObjectReader{Key: objectKey, Size: int64(len(body)), Body: &checkpointFaultReader{Reader: bytes.NewReader(body), closeErr: domain.NewError(domain.ErrorUnavailable, "checkpoint close interrupted")}}, nil
			},
		}
		if _, _, err := streamCheckpointObject(ctx, backend, objectstore.ObjectInfo{Key: objectKey, Size: int64(len(body))}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("streamCheckpointObject() error = %v; want unavailable", err)
		}
	})

	t.Run("stream-version-changed", func(t *testing.T) {
		backend := &hookedBackend{
			Backend: objectmemory.New(),
			open: func(context.Context, objectstore.Key) (objectstore.ObjectReader, error) {
				return objectstore.ObjectReader{Key: objectKey, Size: int64(len(body)), Version: "changed", Body: io.NopCloser(bytes.NewReader(body))}, nil
			},
		}
		if _, _, err := streamCheckpointObject(ctx, backend, objectstore.ObjectInfo{Key: objectKey, Size: int64(len(body)), Version: "expected"}); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("streamCheckpointObject() error = %v; want precondition failed", err)
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

	t.Run("legacy-visitor-interruption", func(t *testing.T) {
		engine := &Engine{}
		checkpoint := storageformat.Checkpoint{SchemaVersion: 1, Objects: []storageformat.CheckpointObject{{Key: objectKey.String()}}}
		if err := engine.visitCheckpointInventory(ctx, checkpoint, func(storageformat.CheckpointInventoryEntry) error {
			return domain.NewError(domain.ErrorUnavailable, "visitor interrupted")
		}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("visitCheckpointInventory() error = %v; want unavailable", err)
		}
	})

	t.Run("inventory-count-changed", func(t *testing.T) {
		calls := 0
		stateKey := storageformat.StateKey("area", "record")
		backend := &hookedBackend{
			Backend: objectmemory.New(),
			list: func(context.Context, objectstore.ListRequest) (objectstore.ListPage, error) {
				calls++
				if calls == 1 {
					return objectstore.ListPage{Objects: []objectstore.ObjectInfo{{Key: stateKey, Size: 1}}}, nil
				}
				return objectstore.ListPage{}, nil
			},
		}
		engine := &Engine{backend: backend, checkpointWorkKey: workKey}
		if _, err := engine.checkpointInventoryRole(ctx, backend, false, checkpointID, gateEpoch); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("checkpointInventoryRole() error = %v; want precondition failed", err)
		}
	})

	t.Run("missing-inventory-page", func(t *testing.T) {
		engine := &Engine{backend: objectmemory.New()}
		if err := engine.validateCheckpointPageSet(ctx, checkpointID, 1); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("validateCheckpointPageSet() error = %v; want precondition failed", err)
		}
	})

	t.Run("work-write-without-authentication-key", func(t *testing.T) {
		if err := (&Engine{}).writeCheckpointWork(ctx, newWork(true)); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("writeCheckpointWork() error = %v; want invalid", err)
		}
	})

	t.Run("work-write-interruption", func(t *testing.T) {
		backend := &hookedBackend{
			Backend: objectmemory.New(),
			put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
				return "", domain.NewError(domain.ErrorUnavailable, "checkpoint work write interrupted")
			},
		}
		engine := &Engine{backend: backend, checkpointWorkKey: workKey}
		if err := engine.writeCheckpointWork(ctx, newWork(true)); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("writeCheckpointWork() error = %v; want unavailable", err)
		}
	})

	t.Run("work-write-conflict", func(t *testing.T) {
		memory := objectmemory.New()
		backend := &hookedBackend{
			Backend: memory,
			put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
				return "", domain.NewError(domain.ErrorConflict, "checkpoint work write raced")
			},
		}
		engine := &Engine{backend: backend, checkpointWorkKey: workKey}
		if err := engine.writeCheckpointWork(ctx, newWork(true)); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("writeCheckpointWork() error = %v; want precondition failed", err)
		}
	})

	t.Run("checkpoint-summary-mismatch", func(t *testing.T) {
		engine := &Engine{writer: storageformat.WriterSet{WriterSetID: "writer"}}
		checkpoint := storageformat.Checkpoint{CheckpointID: checkpointID, WriterSetID: "writer", GateEpoch: gateEpoch, InventoryPageCount: 1, StateObjectCount: 1, InventoryDigest: "digest"}
		gate := storageformat.WriteGate{Mode: storageformat.GateClosed, CheckpointID: checkpointID, Epoch: gateEpoch}
		if err := engine.verifyCheckpointV2Summary(checkpoint, gate, checkpointInventorySummary{}); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("verifyCheckpointV2Summary() error = %v; want precondition failed", err)
		}
	})

	t.Run("inventory-page-write-interruption", func(t *testing.T) {
		memory := objectmemory.New()
		engine := &Engine{backend: memory, checkpointWorkKey: workKey}
		writeCheckpointWorkForTest(t, engine, newWork(true))
		engine.backend = &hookedBackend{
			Backend: memory,
			put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
				return "", domain.NewError(domain.ErrorUnavailable, "checkpoint page write interrupted")
			},
		}
		if _, err := engine.buildCheckpointInventoryPages(ctx, checkpointID, gateEpoch, 0, 1); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("buildCheckpointInventoryPages() error = %v; want unavailable", err)
		}
	})

	t.Run("inventory-page-write-conflict", func(t *testing.T) {
		memory := objectmemory.New()
		engine := &Engine{backend: memory, checkpointWorkKey: workKey}
		writeCheckpointWorkForTest(t, engine, newWork(true))
		engine.backend = &hookedBackend{
			Backend: memory,
			put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
				return "", domain.NewError(domain.ErrorConflict, "checkpoint page write raced")
			},
		}
		if _, err := engine.buildCheckpointInventoryPages(ctx, checkpointID, gateEpoch, 0, 1); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("buildCheckpointInventoryPages() error = %v; want precondition failed", err)
		}
	})
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

func TestCheckpointRecordAndStreamingDenials(t *testing.T) {
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

	t.Run("legacy-gate-and-inventory-mismatch", func(t *testing.T) {
		engine := &Engine{writer: storageformat.WriterSet{WriterSetID: "writer"}}
		checkpoint := storageformat.Checkpoint{CheckpointID: checkpointID, WriterSetID: "writer", GateEpoch: 2, InventoryDigest: "different"}
		if err := engine.verifyCheckpointInventory(checkpoint, storageformat.WriteGate{}, nil); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("verifyCheckpointInventory(gate) error = %v; want precondition failed", err)
		}
		gate := storageformat.WriteGate{Mode: storageformat.GateClosed, CheckpointID: checkpointID, Epoch: 2}
		if err := engine.verifyCheckpointInventory(checkpoint, gate, nil); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("verifyCheckpointInventory(inventory) error = %v; want precondition failed", err)
		}
	})

	t.Run("legacy-inventory-list-interruption", func(t *testing.T) {
		unavailable := func(context.Context, objectstore.ListRequest) (objectstore.ListPage, error) {
			return objectstore.ListPage{}, domain.NewError(domain.ErrorUnavailable, "legacy inventory list interrupted")
		}
		engine := &Engine{backend: &hookedBackend{Backend: objectmemory.New(), list: unavailable}}
		if _, err := engine.authoritativeInventory(ctx); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("authoritativeInventory(state) error = %v; want unavailable", err)
		}
		engine = &Engine{
			backend: objectmemory.New(), fileBackend: &hookedBackend{Backend: objectmemory.New(), list: unavailable},
			separateFileBackend: true,
		}
		if _, err := engine.authoritativeInventory(ctx); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("authoritativeInventory(file) error = %v; want unavailable", err)
		}
	})

	t.Run("paged-visitor-and-content-change", func(t *testing.T) {
		backend := objectmemory.New()
		clock := domain.NewFixedClock(time.Date(2044, 2, 3, 4, 5, 6, 0, time.UTC))
		engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat(t.Name(), 1<<14)))
		key := storageformat.BlobKey("owner", "mutable-record")
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
		if err := engine.verifyCheckpointV2(ctx, checkpoint, storageformat.WriteGate{Mode: storageformat.GateClosed, CheckpointID: checkpointID, Epoch: checkpoint.GateEpoch}); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("verifyCheckpointV2() error = %v; want precondition failed", err)
		}
	})
}

func writeCheckpointWorkForTest(t *testing.T, engine *Engine, work storageformat.CheckpointWork) {
	t.Helper()
	proof, err := engine.checkpointWorkProof(work)
	if err != nil {
		t.Fatal(err)
	}
	work.Proof = proof
	key := storageformat.CheckpointWorkKey(work.CheckpointID, work.Object.Key)
	body, err := storageformat.EncodeEnvelope(checkpointWorkSchema, key, 1, work)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.backend.Put(context.Background(), key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
}

type checkpointVerifyUnavailableBackend struct {
	objectstore.Backend
}

func (*checkpointVerifyUnavailableBackend) Verify(context.Context, objectstore.Key, objectstore.ExpectedIntegrity) (objectstore.ObjectInfo, error) {
	return objectstore.ObjectInfo{}, domain.NewError(domain.ErrorUnavailable, "checkpoint integrity verification interrupted")
}

type checkpointFaultReader struct {
	io.Reader
	readErr  error
	closeErr error
}

func (reader *checkpointFaultReader) Read(buffer []byte) (int, error) {
	if reader.readErr != nil {
		return 0, reader.readErr
	}
	return reader.Reader.Read(buffer)
}

func (reader *checkpointFaultReader) Close() error { return reader.closeErr }
