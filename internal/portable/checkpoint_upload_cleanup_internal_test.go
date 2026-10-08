package portable

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestCheckpointTerminalUploadCleanupPreservesFrozenAuthority(t *testing.T) {
	for _, terminal := range []storageformat.UploadState{storageformat.UploadCompleted, storageformat.UploadAborted} {
		t.Run(string(terminal), func(t *testing.T) {
			ctx := context.Background()
			engine := openNamespaceTestEngine(t, objectmemory.New())
			owner := namespaceTestScope(t, domain.AreaLive).UserID()
			record := checkpointUploadRecord(engine, owner, "terminal-cleanup", engine.clock.Now().Add(time.Hour))
			record.State, record.CleanupPending = terminal, true
			seedCheckpointUploadRecord(t, engine, owner, record)
			_, before, err := engine.Files().portableUpload(ctx, owner, record.UploadID)
			if err != nil {
				t.Fatal(err)
			}
			if err := engine.CloseWrites(ctx, "pending-terminal-cleanup"); err != nil {
				t.Fatalf("checkpoint closure with pending terminal cleanup: %v", err)
			}
			current, after, err := engine.Files().portableUpload(ctx, owner, record.UploadID)
			if err != nil || !current.CleanupPending || string(before.Data) != string(after.Data) || before.LogicalVersion != after.LogicalVersion {
				t.Fatalf("frozen upload authority changed: %+v, %v", current, err)
			}
			if err := engine.Files().cleanupPortableUpload(ctx, owner, record.UploadID, nil); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("ordinary cleanup bypassed checkpoint freeze: %v", err)
			}
			if err := engine.drainExpiredSchema008Uploads(ctx); err != nil {
				t.Fatalf("repeated frozen cleanup: %v", err)
			}
			current, after, err = engine.Files().portableUpload(ctx, owner, record.UploadID)
			if err != nil || !current.CleanupPending || string(before.Data) != string(after.Data) || before.LogicalVersion != after.LogicalVersion {
				t.Fatalf("repeated cleanup changed frozen authority: %+v, %v", current, err)
			}
		})
	}
}

func TestCheckpointTerminalCleanupProviderFailureDoesNotPublishOrStrandFreeze(t *testing.T) {
	ctx := context.Background()
	memory := objectmemory.New()
	hooks := &hookedBackend{Backend: memory}
	engine := openNamespaceTestEngine(t, hooks)
	engine.fileBackend = memory
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	record := checkpointUploadRecord(engine, owner, "failed-cleanup", engine.clock.Now().Add(time.Hour))
	record.State, record.CleanupPending = storageformat.UploadCompleted, true
	seedCheckpointUploadRecord(t, engine, owner, record)
	leaseKey := storageformat.LeaseKey(memory.BackendKind(), record.UploadID)
	if _, err := memory.Put(ctx, leaseKey, []byte("transient provider lease"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	failure := domain.NewError(domain.ErrorUnavailable, "injected provider cleanup failure")
	hooks.delete = func(context.Context, objectstore.Key, objectstore.DeleteCondition) error { return failure }
	if err := engine.CloseWrites(ctx, "failed-terminal-cleanup"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("provider cleanup failure was ignored: %v", err)
	}
	gate, err := engine.GateStatus(ctx)
	if err != nil || gate.Mode != storageformat.GateOpen {
		t.Fatalf("failed cleanup stranded the write gate: %+v, %v", gate, err)
	}
	snapshot, err := engine.stateDomainStore().loadHead(ctx, namespaceReference(owner))
	if err != nil || snapshot.head.Frozen {
		t.Fatalf("failed cleanup stranded a frozen domain: %+v, %v", snapshot.head, err)
	}
	current, _, err := engine.Files().portableUpload(ctx, owner, record.UploadID)
	if err != nil || !current.CleanupPending {
		t.Fatalf("failed provider cleanup published completion: %+v, %v", current, err)
	}
	hooks.delete = nil
	if err := engine.CloseWrites(ctx, "failed-terminal-cleanup"); err != nil {
		t.Fatalf("retry after provider recovery: %v", err)
	}
}

func TestCheckpointTerminalCleanupRejectsMisboundBlobBeforeProviderEffects(t *testing.T) {
	ctx := context.Background()
	memory := objectmemory.New()
	deletes := 0
	hooks := &hookedBackend{Backend: memory}
	hooks.delete = func(context.Context, objectstore.Key, objectstore.DeleteCondition) error {
		deletes++
		return nil
	}
	engine := openNamespaceTestEngine(t, hooks)
	engine.fileBackend = memory
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	record := checkpointUploadRecord(engine, owner, "misbound-cleanup", engine.clock.Now().Add(time.Hour))
	record.State, record.CleanupPending, record.BlobID = storageformat.UploadCompleted, true, "another-upload"
	seedCheckpointUploadRecord(t, engine, owner, record)
	if err := engine.CloseWrites(ctx, "misbound-terminal-cleanup"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("misbound terminal cleanup was accepted: %v", err)
	}
	if deletes != 0 {
		t.Fatal("misbound terminal authority caused provider cleanup")
	}
	gate, err := engine.GateStatus(ctx)
	if err != nil || gate.Mode != storageformat.GateClosing {
		t.Fatalf("corrupt cleanup authority did not fail closed: %+v, %v", gate, err)
	}
}
