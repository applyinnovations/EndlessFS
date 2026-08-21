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
		if _, err := engine.authoritativeInventoryResumable(ctx, checkpointID, gateEpoch); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("authoritativeInventoryResumable() error = %v; want precondition failed", err)
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
		if _, err := engine.authoritativeInventoryResumable(ctx, checkpointID, gateEpoch); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("authoritativeInventoryResumable() error = %v; want precondition failed", err)
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
		if _, err := engine.readCheckpointWork(ctx, checkpointID, gateEpoch); err == nil {
			t.Fatal("readCheckpointWork() accepted a malformed work envelope")
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
		if _, err := engine.readCheckpointWork(ctx, checkpointID, gateEpoch); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("readCheckpointWork() error = %v; want precondition failed", err)
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
		if _, err := engine.readCheckpointWork(ctx, checkpointID, gateEpoch); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("readCheckpointWork() error = %v; want precondition failed", err)
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
		if _, err := engine.readCheckpointWork(ctx, checkpointID, gateEpoch); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("readCheckpointWork() error = %v; want precondition failed", err)
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
		if _, err := engine.readCheckpointWork(ctx, checkpointID, gateEpoch); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("readCheckpointWork() error = %v; want unavailable", err)
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
		if _, err := engine.readCheckpointWork(ctx, checkpointID, gateEpoch); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("readCheckpointWork() error = %v; want unavailable", err)
		}
	})

	t.Run("resume-integrity-interruption", func(t *testing.T) {
		memory := objectmemory.New()
		if _, err := memory.Put(ctx, objectKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		work := newWork(false)
		entries := map[string]checkpointWorkEntry{
			objectKey.String(): {object: work.Object, crc32c: work.CRC32C},
		}
		engine := &Engine{backend: memory, fileBackend: memory, checkpointWorkKey: workKey}
		backend := &checkpointVerifyUnavailableBackend{Backend: memory}
		if _, _, err := engine.authoritativeInventoryFromWork(ctx, backend, false, checkpointID, gateEpoch, entries); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("authoritativeInventoryFromWork() error = %v; want unavailable", err)
		}
	})

	t.Run("resume-metadata-mismatch", func(t *testing.T) {
		backend := objectmemory.New()
		if _, err := backend.Put(ctx, objectKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		work := newWork(false)
		work.Object.Size++
		entries := map[string]checkpointWorkEntry{
			objectKey.String(): {object: work.Object, crc32c: work.CRC32C},
		}
		engine := &Engine{backend: backend, fileBackend: backend, checkpointWorkKey: workKey}
		if _, _, err := engine.authoritativeInventoryFromWork(ctx, backend, false, checkpointID, gateEpoch, entries); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("authoritativeInventoryFromWork() error = %v; want precondition failed", err)
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
		if _, _, err := engine.authoritativeInventoryFromWork(ctx, backend, false, checkpointID, gateEpoch, nil); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("authoritativeInventoryFromWork() error = %v; want precondition failed", err)
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
		if _, err := engine.authoritativeInventoryResumable(ctx, checkpointID, gateEpoch); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("authoritativeInventoryResumable() error = %v; want unavailable", err)
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
		if _, _, err := engine.authoritativeInventoryFromWork(ctx, backend, false, checkpointID, gateEpoch, nil); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("authoritativeInventoryFromWork() error = %v; want precondition failed", err)
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
