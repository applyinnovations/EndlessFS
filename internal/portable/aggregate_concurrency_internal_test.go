package portable

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestDirectoryTrailMismatchDistinguishesRacesFromStableCorruption(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2046, 1, 2, 3, 4, 5, 0, time.UTC))
	user, _ := domain.ParseUserID("YmJiYmJiYmJiYmJiYmJiYg")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	newFixture := func(t *testing.T) (*objectmemory.Backend, *Engine, directoryTrailNode, storageformat.DirectoryEntry, directorySnapshot) {
		t.Helper()
		memory := objectmemory.New()
		engine := openInternalTestEngine(t, memory, clock, strings.NewReader(strings.Repeat(t.Name(), 1<<14)))
		if _, err := engine.Files().CreateDirectory(ctx, scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/child")}); err != nil {
			t.Fatal(err)
		}
		trail, err := engine.Files().resolveDirectoryTrail(ctx, scope, domain.MustParseUserPath("/child"))
		if err != nil {
			t.Fatal(err)
		}
		entry, found := findDirectoryEntry(trail[0].snapshot.entries, "child")
		if !found {
			t.Fatal("child entry is missing")
		}
		return memory, engine, trail[0], entry, trail[1].snapshot
	}

	t.Run("stable-mismatch", func(t *testing.T) {
		_, engine, parent, entry, child := newFixture(t)
		if _, err := engine.Files().resolveDirectoryTrail(ctx, scope, domain.MustParseUserPath("/missing")); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("resolveDirectoryTrail(missing) error = %v", err)
		}
		if err := engine.Files().classifyDirectoryTrailMismatch(ctx, parent, entry, child); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("classifyDirectoryTrailMismatch() error = %v", err)
		}
	})
	t.Run("parent-changed", func(t *testing.T) {
		_, engine, parent, entry, child := newFixture(t)
		parent.snapshot.manifestID = "changed"
		if err := engine.Files().classifyDirectoryTrailMismatch(ctx, parent, entry, child); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("classifyDirectoryTrailMismatch() error = %v", err)
		}
	})
	t.Run("child-changed", func(t *testing.T) {
		_, engine, parent, entry, child := newFixture(t)
		child.manifestID = "changed"
		if err := engine.Files().classifyDirectoryTrailMismatch(ctx, parent, entry, child); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("classifyDirectoryTrailMismatch() error = %v", err)
		}
	})
	t.Run("parent-read-failure", func(t *testing.T) {
		memory, engine, parent, entry, child := newFixture(t)
		parentKey := storageformat.DirectoryRootKey(user.String(), "live", parent.directoryID)
		hooks := &hookedBackend{Backend: memory, get: func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
			if key == parentKey {
				return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "parent read failed")
			}
			return memory.Get(ctx, key)
		}}
		engine.backend = hooks
		if err := engine.Files().classifyDirectoryTrailMismatch(ctx, parent, entry, child); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("classifyDirectoryTrailMismatch() error = %v", err)
		}
	})
	t.Run("child-read-failure", func(t *testing.T) {
		memory, engine, parent, entry, child := newFixture(t)
		childKey := storageformat.DirectoryRootKey(user.String(), "live", entry.DirectoryID)
		hooks := &hookedBackend{Backend: memory, get: func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
			if key == childKey {
				return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "child read failed")
			}
			return memory.Get(ctx, key)
		}}
		engine.backend = hooks
		if err := engine.Files().classifyDirectoryTrailMismatch(ctx, parent, entry, child); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("classifyDirectoryTrailMismatch() error = %v", err)
		}
	})
	t.Run("missing-child-root", func(t *testing.T) {
		memory, engine, parent, entry, child := newFixture(t)
		childKey := storageformat.DirectoryRootKey(user.String(), "live", entry.DirectoryID)
		object, err := memory.Get(ctx, childKey)
		if err != nil {
			t.Fatal(err)
		}
		if err := memory.Delete(ctx, childKey, objectstore.DeleteCondition{Version: object.Version}); err != nil {
			t.Fatal(err)
		}
		if err := engine.Files().classifyDirectoryTrailMismatch(ctx, parent, entry, child); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("classifyDirectoryTrailMismatch() error = %v", err)
		}
	})
}

func TestMutableDirectoryRecoveryFailsClosedOnUnexpectedOperationRead(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2046, 1, 10, 3, 4, 5, 0, time.UTC))
	user, _ := domain.ParseUserID("aGhoaGhoaGhoaGhoaGhoaA")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	memory := objectmemory.New()
	engine := openInternalTestEngine(t, memory, clock, strings.NewReader(strings.Repeat(t.Name(), 1<<14)))
	crashed := false
	engine.scheduler = SchedulerFunc(func(_ context.Context, step string) error {
		if step == StepCreateDirectoryAfterCommitted && !crashed {
			crashed = true
			return domain.NewError(domain.ErrorUnavailable, "simulated loss after commit")
		}
		return nil
	})
	if _, err := engine.Files().CreateDirectory(ctx, scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/stuck")}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("interrupted CreateDirectory() error = %v", err)
	}
	clock.Advance(2 * time.Minute)

	rootKey := storageformat.DirectoryRootKey(user.String(), "live", storageformat.RootDirectoryID)
	rootObject, err := memory.Get(ctx, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	var rootEnvelope storageformat.Envelope
	var root storageformat.DirectoryRoot
	if err := storageformat.DecodeEnvelope(rootObject.Body, rootKey, directoryRootSchema, &rootEnvelope, &root); err != nil {
		t.Fatal(err)
	}
	if root.Pending == nil {
		t.Fatal("interrupted create did not leave a pending root")
	}
	operationKey := storageformat.OperationKey(user.String(), root.Pending.OperationID)
	operationReads := 0
	hooks := &hookedBackend{Backend: memory, get: func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
		if key == operationKey {
			operationReads++
			if operationReads == 2 {
				return objectstore.Object{}, domain.NewError(domain.ErrorUnauthorized, "operation read denied")
			}
		}
		return memory.Get(ctx, key)
	}}
	engine.backend = hooks
	if _, err := engine.Files().resolveMutableDirectoryMetadataTrail(ctx, scope, domain.MustParseUserPath("/")); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("resolveMutableDirectoryMetadataTrail() error = %v", err)
	}
}

func TestFileOperationFailureRollbackFaultsRemainRecoverable(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2046, 2, 3, 4, 5, 6, 0, time.UTC))
	user, _ := domain.ParseUserID("Y2NjY2NjY2NjY2NjY2NjYw")
	newFixture := func(t *testing.T) (*objectmemory.Backend, *hookedBackend, *Engine, objectstore.Object, storageformat.Envelope, storageformat.FileOperation, objectstore.Key) {
		t.Helper()
		memory := objectmemory.New()
		hooks := &hookedBackend{Backend: memory}
		engine := openInternalTestEngine(t, hooks, clock, strings.NewReader(strings.Repeat(t.Name(), 1<<14)))
		operationID := "rollback-operation"
		operationKey := storageformat.OperationKey(user.String(), operationID)
		rootKey := storageformat.DirectoryRootKey(user.String(), "live", storageformat.RootDirectoryID)
		pendingBody := []byte("pending")
		if _, err := memory.Put(ctx, rootKey, pendingBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		operation := storageformat.FileOperation{
			SchemaVersion: 1, OperationID: operationID, UserID: user.String(), Kind: "move", State: storageformat.FileOperationRunning,
			Attempt: 1, Fence: 1, ReplicaAttemptID: "replica", ExpiresAt: clock.Now().Add(time.Minute), StartedAt: clock.Now(), UpdatedAt: clock.Now(),
			Roots: []storageformat.FileOperationRoot{{Key: rootKey.String(), PreExisted: true, PendingBody: pendingBody, RollbackBody: []byte("rollback")}},
		}
		body := encodeInternalEnvelope(t, fileOperationSchema, operationKey, 1, operation)
		if _, err := memory.Put(ctx, operationKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		object, envelope, stored, err := engine.Files().readFileOperationObject(ctx, operationKey)
		if err != nil {
			t.Fatal(err)
		}
		return memory, hooks, engine, object, envelope, stored, rootKey
	}

	t.Run("root-read", func(t *testing.T) {
		_, hooks, engine, object, envelope, operation, rootKey := newFixture(t)
		hooks.get = func(context.Context, objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "read interrupted")
		}
		if err := engine.Files().failFileOperation(ctx, object, envelope, operation, domain.ErrorPreconditionFailed, "failed"); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("failFileOperation(%s) error = %v", rootKey, err)
		}
	})
	t.Run("rollback-write", func(t *testing.T) {
		memory, hooks, engine, object, envelope, operation, rootKey := newFixture(t)
		hooks.put = func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if key == rootKey {
				return "", domain.NewError(domain.ErrorUnavailable, "rollback interrupted")
			}
			return memory.Put(ctx, key, body, condition)
		}
		if err := engine.Files().failFileOperation(ctx, object, envelope, operation, domain.ErrorPreconditionFailed, "failed"); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("failFileOperation() error = %v", err)
		}
	})
	t.Run("failure-state-cas", func(t *testing.T) {
		memory, hooks, engine, object, envelope, operation, rootKey := newFixture(t)
		operation.Roots[0].PendingBody = []byte("not-current")
		hooks.put = func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if key == object.Key {
				return "", domain.NewError(domain.ErrorPreconditionFailed, "operation changed")
			}
			return memory.Put(ctx, key, body, condition)
		}
		if err := engine.Files().failFileOperation(ctx, object, envelope, operation, domain.ErrorPreconditionFailed, "failed"); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("failFileOperation(%s) error = %v", rootKey, err)
		}
	})
	t.Run("invalid-operation-key", func(t *testing.T) {
		engine := &Engine{backend: objectmemory.New(), clock: clock}
		operation := storageformat.FileOperation{State: storageformat.FileOperationRunning, UpdatedAt: clock.Now()}
		if err := engine.Files().failFileOperation(ctx, objectstore.Object{}, storageformat.Envelope{}, operation, domain.ErrorPreconditionFailed, "failed"); err == nil {
			t.Fatal("failFileOperation(invalid key) unexpectedly succeeded")
		}
	})
}

func TestUploadCompletionRetryDenialBranches(t *testing.T) {
	store := (&Engine{}).Files()
	if _, err := store.retryUploadCompletion(context.Background(), domain.Scope{}, domain.CompleteUploadRequest{}, "", nil, objectstore.Object{}, storageformat.Envelope{}, storageformat.UploadRecord{}, 1); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("retryUploadCompletion(exhausted) error = %v", err)
	}

	emptyIDs := &Engine{ids: domain.NewIDGenerator(bytes.NewReader(nil))}
	if err := emptyIDs.Files().rotateUploadCompletionOperation(context.Background(), objectstore.Object{}, storageformat.Envelope{}, storageformat.UploadRecord{}); !errors.Is(err, io.EOF) {
		t.Fatalf("rotateUploadCompletionOperation(randomness) error = %v", err)
	}
	if _, err := emptyIDs.Files().retryUploadCompletion(context.Background(), domain.Scope{}, domain.CompleteUploadRequest{}, "", nil, objectstore.Object{}, storageformat.Envelope{}, storageformat.UploadRecord{}, 2); !errors.Is(err, io.EOF) {
		t.Fatalf("retryUploadCompletion(randomness) error = %v", err)
	}
	invalidObject := objectstore.Object{}
	invalidKeyEngine := &Engine{ids: domain.NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x41}, 64)))}
	if err := invalidKeyEngine.Files().rotateUploadCompletionOperation(context.Background(), invalidObject, storageformat.Envelope{}, storageformat.UploadRecord{}); err == nil {
		t.Fatal("rotateUploadCompletionOperation(invalid key) unexpectedly succeeded")
	}
}

func TestMutationCopyConvergesWhenConcurrentWinnerRemovesSource(t *testing.T) {
	ctx := context.Background()
	source := objectstore.MustKey("copy/source")
	destination := objectstore.MustKey("copy/destination")
	intent := storageformat.MutationCopy{SourceKey: source.String(), DestinationKey: destination.String(), Size: 1}

	t.Run("source-missing-after-destination-read", func(t *testing.T) {
		memory := objectmemory.New()
		if _, err := memory.Put(ctx, destination, []byte("x"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		destinationReads := 0
		hooks := &hookedBackend{Backend: memory, head: func(ctx context.Context, key objectstore.Key) (objectstore.ObjectInfo, error) {
			if key == destination {
				destinationReads++
				if destinationReads == 1 {
					return objectstore.ObjectInfo{}, domain.NewError(domain.ErrorNotFound, "not visible on first read")
				}
			}
			return memory.Head(ctx, key)
		}}
		if err := (&Engine{fileBackend: hooks}).ensureMutationCopies(ctx, []storageformat.MutationCopy{intent}); err != nil {
			t.Fatalf("ensureMutationCopies() error = %v", err)
		}
	})
	t.Run("source-and-destination-missing", func(t *testing.T) {
		memory := objectmemory.New()
		if err := (&Engine{fileBackend: memory}).ensureMutationCopies(ctx, []storageformat.MutationCopy{intent}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("ensureMutationCopies() error = %v", err)
		}
	})
	t.Run("source-removed-after-read-without-winner", func(t *testing.T) {
		memory := objectmemory.New()
		if _, err := memory.Put(ctx, source, []byte("x"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		hooks := &hookedBackend{Backend: memory}
		hooks.copy = func(ctx context.Context, from, _ objectstore.Key, condition objectstore.CopyCondition) (objectstore.CopyResult, error) {
			if err := memory.Delete(ctx, from, objectstore.DeleteCondition{Version: condition.SourceVersion}); err != nil {
				return objectstore.CopyResult{}, err
			}
			return objectstore.CopyResult{}, domain.NewError(domain.ErrorNotFound, "source removed before copy")
		}
		if err := (&Engine{fileBackend: hooks}).ensureMutationCopies(ctx, []storageformat.MutationCopy{intent}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("ensureMutationCopies() error = %v; want source not found", err)
		}
	})
	t.Run("copy-response-lost-after-winner", func(t *testing.T) {
		memory := objectmemory.New()
		if _, err := memory.Put(ctx, source, []byte("x"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		hooks := &hookedBackend{Backend: memory}
		hooks.copy = func(ctx context.Context, from, to objectstore.Key, condition objectstore.CopyCondition) (objectstore.CopyResult, error) {
			if _, err := memory.Copy(ctx, from, to, condition); err != nil {
				return objectstore.CopyResult{}, err
			}
			return objectstore.CopyResult{}, domain.NewError(domain.ErrorNotFound, "source disappeared after copy")
		}
		if err := (&Engine{fileBackend: hooks}).ensureMutationCopies(ctx, []storageformat.MutationCopy{intent}); err != nil {
			t.Fatalf("ensureMutationCopies() error = %v", err)
		}
	})
}

func TestConcurrentOperationRootRechecksHandleDenialBranches(t *testing.T) {
	ctx := context.Background()
	key := storageformat.DirectoryRootKey("owner", "live", storageformat.RootDirectoryID)
	root := storageformat.FileOperationRoot{Key: key.String(), PendingBody: []byte("pending"), FinalBody: []byte("final")}
	original := domain.NewError(domain.ErrorPreconditionFailed, "original")

	t.Run("accept-original", func(t *testing.T) {
		memory := objectmemory.New()
		if _, err := memory.Put(ctx, key, []byte("other"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		store := (&Engine{backend: memory}).Files()
		if err := store.acceptConcurrentOperationRoot(ctx, key, root, original); err != original {
			t.Fatalf("acceptConcurrentOperationRoot() error = %v", err)
		}
	})
	t.Run("accept-concurrent-winner", func(t *testing.T) {
		memory := objectmemory.New()
		if _, err := memory.Put(ctx, key, root.PendingBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := (&Engine{backend: memory}).Files().acceptConcurrentOperationRoot(ctx, key, root, original); err != nil {
			t.Fatalf("acceptConcurrentOperationRoot() error = %v", err)
		}
	})
	t.Run("accept-read-failure", func(t *testing.T) {
		hooks := &hookedBackend{Backend: objectmemory.New(), get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "read interrupted")
		}}
		if err := (&Engine{backend: hooks}).Files().acceptConcurrentOperationRoot(ctx, key, root, original); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("acceptConcurrentOperationRoot() error = %v", err)
		}
	})
	for _, test := range []struct {
		name    string
		getErr  error
		wantErr error
	}{
		{name: "finalize-read-failure", getErr: domain.NewError(domain.ErrorUnavailable, "read interrupted"), wantErr: domain.ErrUnavailable},
		{name: "finalize-missing", getErr: domain.NewError(domain.ErrorNotFound, "missing"), wantErr: domain.ErrPreconditionFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			memory := objectmemory.New()
			if _, err := memory.Put(ctx, key, root.PendingBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
				t.Fatal(err)
			}
			reads := 0
			hooks := &hookedBackend{Backend: memory}
			hooks.get = func(ctx context.Context, requested objectstore.Key) (objectstore.Object, error) {
				reads++
				if reads == 1 {
					return memory.Get(ctx, requested)
				}
				return objectstore.Object{}, test.getErr
			}
			hooks.put = func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
				return "", domain.NewError(domain.ErrorPreconditionFailed, "concurrent finalization")
			}
			if err := (&Engine{backend: hooks}).Files().finalizeOperationRoot(ctx, root); !errors.Is(err, test.wantErr) {
				t.Fatalf("finalizeOperationRoot() error = %v", err)
			}
		})
	}
	t.Run("finalize-concurrent-winner", func(t *testing.T) {
		memory := objectmemory.New()
		if _, err := memory.Put(ctx, key, root.PendingBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		hooks := &hookedBackend{Backend: memory}
		hooks.put = func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if _, err := memory.Put(ctx, key, body, condition); err != nil {
				return "", err
			}
			return "", domain.NewError(domain.ErrorPreconditionFailed, "concurrent finalization")
		}
		if err := (&Engine{backend: hooks}).Files().finalizeOperationRoot(ctx, root); err != nil {
			t.Fatalf("finalizeOperationRoot() error = %v", err)
		}
	})
}

func TestFinishUploadContentionAndDenialBranches(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2046, 3, 4, 5, 6, 7, 0, time.UTC))
	user, _ := domain.ParseUserID("ZGRkZGRkZGRkZGRkZGRkZA")
	newFixture := func(t *testing.T) (*objectmemory.Backend, *hookedBackend, *Engine, objectstore.Object, storageformat.Envelope, storageformat.UploadRecord) {
		t.Helper()
		memory := objectmemory.New()
		hooks := &hookedBackend{Backend: memory}
		engine := openInternalTestEngine(t, hooks, clock, strings.NewReader(strings.Repeat(t.Name(), 1<<14)))
		uploadID := "finish-upload"
		key := storageformat.OperationKey(user.String(), uploadID)
		record := storageformat.UploadRecord{
			SchemaVersion: 1, UploadID: uploadID, CompletionOperationID: "completion", UserID: user.String(), Area: "live",
			RequestedPath: "/file.bin", ResolvedPath: "/file.bin", StagingKey: storageformat.StagingKey(user.String(), uploadID, "upload").String(),
			BackendKind: memory.BackendKind(), LeaseKey: storageformat.LeaseKey(memory.BackendKind(), uploadID).String(),
			Size: 1, MediaType: "application/octet-stream", Conflict: domain.ConflictFail, State: storageformat.UploadActive,
			CreatedAt: clock.Now(), ExpiresAt: clock.Now().Add(time.Minute),
		}
		body := encodeInternalEnvelope(t, uploadRecordSchema, key, 1, record)
		if _, err := memory.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		object, envelope, stored, err := engine.Files().readUploadRecord(ctx, user, uploadID)
		if err != nil {
			t.Fatal(err)
		}
		return memory, hooks, engine, object, envelope, stored
	}

	t.Run("invalid-user", func(t *testing.T) {
		if err := (&Engine{}).Files().finishUpload(ctx, objectstore.Object{}, storageformat.Envelope{}, storageformat.UploadRecord{}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("finishUpload() error = %v", err)
		}
	})
	t.Run("invalid-key", func(t *testing.T) {
		_, _, engine, _, envelope, record := newFixture(t)
		if err := engine.Files().finishUpload(ctx, objectstore.Object{}, envelope, record); err == nil {
			t.Fatal("finishUpload(invalid key) unexpectedly succeeded")
		}
	})
	t.Run("reread-failure", func(t *testing.T) {
		_, hooks, engine, object, envelope, record := newFixture(t)
		target := object.Key
		hooks.put = func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return "", domain.NewError(domain.ErrorPreconditionFailed, "stale")
		}
		hooks.get = func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
			if key == target {
				return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "reread interrupted")
			}
			return hooks.Backend.Get(ctx, key)
		}
		if err := engine.Files().finishUpload(ctx, object, envelope, record); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("finishUpload() error = %v", err)
		}
	})
	t.Run("aborted-winner", func(t *testing.T) {
		memory, hooks, engine, object, envelope, record := newFixture(t)
		target := object.Key
		aborted := record
		aborted.State = storageformat.UploadAborted
		body := encodeInternalEnvelope(t, uploadRecordSchema, target, envelope.Revision+1, aborted)
		if _, err := memory.Put(ctx, target, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
			t.Fatal(err)
		}
		hooks.put = func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return memory.Put(ctx, key, body, condition)
		}
		if err := engine.Files().finishUpload(ctx, object, envelope, record); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("finishUpload() error = %v", err)
		}
	})
	t.Run("exhausted", func(t *testing.T) {
		memory, hooks, engine, object, envelope, record := newFixture(t)
		target := object.Key
		hooks.put = func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if key == target {
				return "", domain.NewError(domain.ErrorPreconditionFailed, "stale")
			}
			return memory.Put(ctx, key, body, condition)
		}
		if err := engine.Files().finishUpload(ctx, object, envelope, record); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("finishUpload() error = %v", err)
		}
	})
}
