package portable

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type transferFailureBackend struct {
	objectstore.Backend
	transfers   objectstore.DirectTransferBackend
	beginErr    error
	beginHandle *objectstore.UploadHandle
	resumeErr   error
	resumeValue *objectstore.UploadCapability
	progressErr error
	abortErr    error
	abortOK     bool
	abortCalls  atomic.Int64
	downloadErr error
	verifyInfo  *objectstore.ObjectInfo
	verifyErr   error
}

func (backend *transferFailureBackend) BackendKind() string { return backend.transfers.BackendKind() }
func (backend *transferFailureBackend) Verify(ctx context.Context, key objectstore.Key, expected objectstore.ExpectedIntegrity) (objectstore.ObjectInfo, error) {
	if backend.verifyErr != nil {
		return objectstore.ObjectInfo{}, backend.verifyErr
	}
	if backend.verifyInfo != nil {
		return *backend.verifyInfo, nil
	}
	return backend.Backend.Verify(ctx, key, expected)
}
func (backend *transferFailureBackend) BeginUpload(ctx context.Context, request objectstore.UploadRequest) (objectstore.UploadHandle, error) {
	if backend.beginErr != nil {
		return objectstore.UploadHandle{}, backend.beginErr
	}
	if backend.beginHandle != nil {
		return *backend.beginHandle, nil
	}
	return backend.transfers.BeginUpload(ctx, request)
}
func (backend *transferFailureBackend) ResumeUpload(ctx context.Context, lease []byte) (objectstore.UploadCapability, error) {
	if backend.resumeErr != nil {
		return objectstore.UploadCapability{}, backend.resumeErr
	}
	if backend.resumeValue != nil {
		return *backend.resumeValue, nil
	}
	return backend.transfers.ResumeUpload(ctx, lease)
}
func (backend *transferFailureBackend) UploadProgress(ctx context.Context, lease []byte) (objectstore.UploadProgress, error) {
	if backend.progressErr != nil {
		return objectstore.UploadProgress{}, backend.progressErr
	}
	return backend.transfers.UploadProgress(ctx, lease)
}
func (backend *transferFailureBackend) AbortUpload(ctx context.Context, lease []byte) error {
	backend.abortCalls.Add(1)
	if backend.abortErr != nil {
		return backend.abortErr
	}
	if backend.abortOK {
		return nil
	}
	return backend.transfers.AbortUpload(ctx, lease)
}
func (backend *transferFailureBackend) CreateDownload(ctx context.Context, request objectstore.DownloadRequest) (objectstore.DownloadCapability, error) {
	if backend.downloadErr != nil {
		return objectstore.DownloadCapability{}, backend.downloadErr
	}
	return backend.transfers.CreateDownload(ctx, request)
}

func TestSchema008UploadAuthorityStateAndBindingDenialMatrix(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2061, 1, 2, 3, 4, 5, 0, time.UTC))
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x61}, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("schema008-upload-denials", 1<<15)))
	files := engine.Files()
	live := namespaceTestScope(t, domain.AreaLive)
	request := domain.CreateUploadRequest{Path: domain.MustParseUserPath("/upload.bin"), Size: 8, MediaType: "application/octet-stream", Resumable: true, IdempotencyKey: "upload-denial-key"}
	capability, err := files.CreateUpload(ctx, live, request)
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := files.portableUpload(ctx, live.UserID(), string(capability.UploadID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodePortableUploadRecord([]byte("{}"), live.UserID(), record.UploadID); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("malformed portable record error = %v", err)
	}
	body, err := storageformat.EncodeCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodePortableUploadRecord(body, namespaceTestScope(t, domain.AreaLive).UserID(), "different-upload"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("misbound portable record error = %v", err)
	}
	if _, _, err := files.portableUploadAtView(ctx, nil, live.UserID(), record.UploadID); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("nil upload view error = %v", err)
	}
	view, err := newNamespaceStore(engine).loadView(ctx, live.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := files.portableUploadAtView(ctx, view, live.UserID(), "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing upload in view error = %v", err)
	}
	if _, found, err := files.portableUploadByIdempotencyAtView(ctx, view, live.UserID(), "", ""); err != nil || found {
		t.Fatalf("empty idempotency lookup found=%v error=%v", found, err)
	}
	if _, _, err := files.portableUploadByIdempotencyAtView(ctx, nil, live.UserID(), "key", "fingerprint"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("misbound idempotency view error = %v", err)
	}
	if _, found, err := files.portableUploadByIdempotency(ctx, live.UserID(), "", ""); err != nil || found {
		t.Fatalf("empty idempotency lookup found=%v error=%v", found, err)
	}
	if _, found, err := files.portableUploadByIdempotency(ctx, live.UserID(), "missing", "fingerprint"); err != nil || found {
		t.Fatalf("missing idempotency lookup found=%v error=%v", found, err)
	}
	if _, found, err := files.portableUploadByIdempotency(ctx, live.UserID(), request.IdempotencyKey, "wrong"); !found || !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("changed idempotency intent found=%v error=%v", found, err)
	}

	if _, err := files.UploadStatus(ctx, live, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty upload status error = %v", err)
	}
	trash, _ := domain.NewScope(live.UserID(), domain.AreaTrash)
	if _, err := files.UploadStatus(ctx, trash, capability.UploadID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-area upload status error = %v", err)
	}
	status, err := files.UploadStatus(ctx, live, capability.UploadID)
	if err != nil || status.State != domain.UploadStateActive || status.Protocol != domain.UploadResumable {
		t.Fatalf("active status = %+v, %v", status, err)
	}

	if _, err := files.CompleteUpload(ctx, live, domain.CompleteUploadRequest{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty completion error = %v", err)
	}
	if _, err := files.CompleteUpload(ctx, trash, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: request.Path, Size: request.Size, MediaType: request.MediaType}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-area completion error = %v", err)
	}
	for name, completion := range map[string]domain.CompleteUploadRequest{
		"path":  {UploadID: capability.UploadID, Path: domain.MustParseUserPath("/other.bin"), Size: request.Size, MediaType: request.MediaType},
		"size":  {UploadID: capability.UploadID, Path: request.Path, Size: request.Size + 1, MediaType: request.MediaType},
		"media": {UploadID: capability.UploadID, Path: request.Path, Size: request.Size, MediaType: "text/plain"},
	} {
		t.Run("completion-"+name, func(t *testing.T) {
			if _, err := files.CompleteUpload(ctx, live, completion); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("completion error = %v", err)
			}
		})
	}
	if _, err := files.CompleteUpload(ctx, live, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: request.Path, Size: request.Size, MediaType: request.MediaType}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("incomplete provider upload error = %v", err)
	}

	if err := files.AbortUpload(ctx, live, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty abort error = %v", err)
	}
	if err := files.AbortUpload(ctx, trash, capability.UploadID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-area abort error = %v", err)
	}
	if err := files.AbortUpload(ctx, live, capability.UploadID); err != nil {
		t.Fatal(err)
	}
	if err := files.AbortUpload(ctx, live, capability.UploadID); err != nil {
		t.Fatalf("idempotent abort = %v", err)
	}
	status, err = files.UploadStatus(ctx, live, capability.UploadID)
	if err != nil || status.State != domain.UploadStateAborted {
		t.Fatalf("aborted status = %+v, %v", status, err)
	}
	abortedRecord, _, err := files.portableUpload(ctx, live.UserID(), string(capability.UploadID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := files.resumePortableUpload(ctx, abortedRecord); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("resume stale active record after abort error = %v", err)
	}

	completed := record
	completed.UploadID, completed.BlobID, completed.State = "completed-upload", "completed-upload", storageformat.UploadCompleted
	completedBody, err := storageformat.EncodeCanonical(completed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.stateDomainStore().mutate(ctx, uploadDomainReference(live.UserID()), consistencyDomainMutation{ID: "seed-completed-upload", Changes: []consistencyDomainChange{{Key: uploadRecordKey(completed.UploadID), Require: domainValueAbsent, Value: completedBody}}}); err != nil {
		t.Fatal(err)
	}
	if err := files.AbortUpload(ctx, live, domain.UploadID(completed.UploadID)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("completed abort error = %v", err)
	}
	completedStatus, err := files.UploadStatus(ctx, live, domain.UploadID(completed.UploadID))
	if err != nil || completedStatus.State != domain.UploadStateCompleted || completedStatus.ConfirmedOffset != completed.Size {
		t.Fatalf("completed status = %+v, %v", completedStatus, err)
	}

	expiredRequest := request
	expiredRequest.Path, expiredRequest.IdempotencyKey = domain.MustParseUserPath("/expired.bin"), "expired-upload"
	expiredCapability, err := files.CreateUpload(ctx, live, expiredRequest)
	if err != nil {
		t.Fatal(err)
	}
	expiredRecord, _, err := files.portableUpload(ctx, live.UserID(), string(expiredCapability.UploadID))
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(engine.uploadTTL + time.Second)
	expiredStatus, err := files.UploadStatus(ctx, live, expiredCapability.UploadID)
	if err != nil || expiredStatus.State != domain.UploadStateExpired {
		t.Fatalf("expired status = %+v, %v", expiredStatus, err)
	}
	if _, err := files.resumePortableUpload(ctx, expiredRecord); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expired resume error = %v", err)
	}

}

func TestSchema008RuntimeUploadLeaseAndDownloadDenials(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	engine := openNamespaceTestEngine(t, backend)
	files := engine.Files()
	live := namespaceTestScope(t, domain.AreaLive)
	if _, _, err := files.runtimeUploadLease(ctx, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing runtime lease error = %v", err)
	}
	leaseKey := storageformat.LeaseKey(backend.BackendKind(), "empty")
	if _, err := backend.Put(ctx, leaseKey, []byte{}, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := files.runtimeUploadLease(ctx, "empty"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty runtime lease error = %v", err)
	}

	if _, err := files.CreateDownload(ctx, live, domain.CreateDownloadRequest{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty download error = %v", err)
	}
	if _, err := files.CreateDownload(ctx, live, domain.CreateDownloadRequest{Path: domain.MustParseUserPath("/missing"), Disposition: "invalid"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid disposition error = %v", err)
	}
	if _, err := files.CreateDownload(ctx, live, domain.CreateDownloadRequest{Path: domain.MustParseUserPath("/missing")}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing download error = %v", err)
	}
	directory, err := files.CreateDirectory(ctx, live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/directory")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := files.CreateDownload(ctx, live, domain.CreateDownloadRequest{Path: directory.Path, Version: directory.Version}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("directory download error = %v", err)
	}

	seed := seedNamespaceBatchFiles(t, newNamespaceStore(engine), live, 1)[0]
	if _, err := files.CreateDownload(ctx, live, domain.CreateDownloadRequest{Path: seed.Path}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("empty download version error = %v", err)
	}
	if _, err := files.CreateDownload(ctx, live, domain.CreateDownloadRequest{Path: seed.Path, Version: "stale"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale download version error = %v", err)
	}
	if _, err := files.CreateDownload(ctx, live, domain.CreateDownloadRequest{Path: seed.Path, Version: seed.Version}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing download blob error = %v", err)
	}
	blobKey := storageformat.BlobKey(live.UserID().String(), "batch-blob-00000")
	if _, err := backend.Put(ctx, blobKey, []byte("x"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	failure := domain.NewError(domain.ErrorUnavailable, "download capability failed")
	engine.fileBackend = &transferFailureBackend{Backend: backend, transfers: backend, downloadErr: failure}
	if _, err := files.CreateDownload(ctx, live, domain.CreateDownloadRequest{Path: seed.Path, Version: seed.Version}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("download capability error = %v", err)
	}
}

func TestSchema011UploadBatchAbortOverlayCorruptionFailsClosed(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	defer server.Close()
	clock := domain.NewFixedClock(time.Date(2066, 4, 5, 6, 7, 8, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x71}, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat(t.Name(), 1<<14)))
	live := namespaceTestScope(t, domain.AreaLive)
	capabilities, err := engine.Files().CreateUploadBatch(ctx, live, []domain.CreateUploadRequest{{
		Path: domain.MustParseUserPath("/abort-overlay-corruption.bin"), Size: 1,
		MediaType: "application/octet-stream", IdempotencyKey: "abort-overlay-corruption-item",
	}})
	if err != nil || len(capabilities) != 1 {
		t.Fatalf("create upload batch = %+v, %v", capabilities, err)
	}
	request := domain.AbortUploadBatchRequest{
		UploadIDs: []domain.UploadID{capabilities[0].UploadID}, BatchID: capabilities[0].BatchID,
		IdempotencyKey: "abort-overlay-corruption",
	}
	if err := engine.Files().AbortUploadBatch(ctx, live, request); err != nil {
		t.Fatal(err)
	}
	store := newNamespaceStore(engine)
	view, err := store.loadView(ctx, live.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := engine.Files().portableUploadBatchAbortAtView(ctx, view, live.UserID(), capabilities[0].BatchID); err != nil || !found {
		t.Fatalf("load abort overlay = found:%t error:%v", found, err)
	}
	cached := view.uploadAborts[capabilities[0].BatchID]
	if _, err := engine.stateDomainStore().mutatePrepared(ctx, uploadDomainReference(live.UserID()), consistencyDomainMutation{
		ID: "corrupt-upload-abort-overlay",
		Changes: []consistencyDomainChange{{
			Key: uploadBatchAbortKey(capabilities[0].BatchID), Require: domainValuePresent,
			ExpectedVersion: cached.value.LogicalVersion, Value: []byte(`{"schemaVersion":1}`),
		}},
	}, view.headSnapshot, view.session); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Files().UploadStatus(ctx, live, capabilities[0].UploadID); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt upload abort overlay status error = %v", err)
	}
}

func TestSchema008TerminalUploadCleanupProviderFailureMatrix(t *testing.T) {
	ctx := context.Background()
	failure := domain.NewError(domain.ErrorUnavailable, "injected cleanup failure")
	live := namespaceTestScope(t, domain.AreaLive)
	seedTerminal := func(t *testing.T, stateValue storageformat.UploadState) (*objectmemory.Backend, *Engine, storageformat.PortableUploadRecord) {
		t.Helper()
		backend := objectmemory.New()
		engine := openNamespaceTestEngine(t, backend)
		record := checkpointUploadRecord(engine, live.UserID(), "cleanup-"+string(stateValue), engine.clock.Now().Add(time.Hour))
		record.State, record.CleanupPending = stateValue, true
		seedCheckpointUploadRecord(t, engine, live.UserID(), record)
		return backend, engine, record
	}

	t.Run("misbound-known-lease", func(t *testing.T) {
		_, engine, record := seedTerminal(t, storageformat.UploadCompleted)
		known := objectstore.Object{Key: objectstore.MustKey("endlessfs/v1/leases/memory/other.json"), Body: []byte("lease"), Version: "version"}
		if err := engine.Files().cleanupPortableUpload(ctx, live.UserID(), record.UploadID, &known); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("misbound known lease error = %v", err)
		}
	})

	t.Run("transfer-backend", func(t *testing.T) {
		backend, engine, record := seedTerminal(t, storageformat.UploadCompleted)
		engine.fileBackend = metadataOnlyBackend{Backend: backend}
		known := objectstore.Object{Key: storageformat.LeaseKey(backend.BackendKind(), record.UploadID), Body: []byte("lease"), Version: "version"}
		if err := engine.Files().cleanupPortableUpload(ctx, live.UserID(), record.UploadID, &known); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("cleanup transfer backend error = %v", err)
		}
	})

	t.Run("lease-read", func(t *testing.T) {
		backend, engine, record := seedTerminal(t, storageformat.UploadCompleted)
		leaseKey := storageformat.LeaseKey(backend.BackendKind(), record.UploadID)
		engine.backend = &hookedBackend{Backend: backend, get: func(callCtx context.Context, key objectstore.Key) (objectstore.Object, error) {
			if key == leaseKey {
				return objectstore.Object{}, failure
			}
			return backend.Get(callCtx, key)
		}}
		if err := engine.Files().cleanupPortableUpload(ctx, live.UserID(), record.UploadID, nil); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("cleanup lease read error = %v", err)
		}
	})

	t.Run("abort", func(t *testing.T) {
		backend, engine, record := seedTerminal(t, storageformat.UploadAborted)
		leaseKey := storageformat.LeaseKey(backend.BackendKind(), record.UploadID)
		if _, err := backend.Put(ctx, leaseKey, []byte("lease"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		engine.fileBackend = &transferFailureBackend{Backend: backend, transfers: backend, abortErr: failure}
		if err := engine.Files().cleanupPortableUpload(ctx, live.UserID(), record.UploadID, nil); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("cleanup abort error = %v", err)
		}
	})
}

func TestSchema008RuntimeLeaseDeletionReconcilesProviderRaces(t *testing.T) {
	ctx := context.Background()
	for _, providerErr := range []error{
		domain.NewError(domain.ErrorNotFound, "lease already removed"),
		domain.NewError(domain.ErrorPreconditionFailed, "lease already replaced"),
	} {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		engine.backend = &hookedBackend{Backend: memory, delete: func(context.Context, objectstore.Key, objectstore.DeleteCondition) error {
			return providerErr
		}}
		object := objectstore.Object{Key: storageformat.LeaseKey(memory.BackendKind(), "conditional-delete"), Version: "stale-version"}
		if err := engine.Files().deleteKnownRuntimeUploadLease(ctx, object); err != nil {
			t.Fatalf("conditional lease deletion error = %v; want nil", err)
		}
	}

	memory := objectmemory.New()
	engine := openNamespaceTestEngine(t, memory)
	leaseKey := storageformat.LeaseKey(memory.BackendKind(), "transfer-support-race")
	if _, err := memory.Put(ctx, leaseKey, []byte("lease"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	engine.backend = &hookedBackend{Backend: memory, get: func(callCtx context.Context, key objectstore.Key) (objectstore.Object, error) {
		object, err := memory.Get(callCtx, key)
		if key == leaseKey && err == nil {
			engine.fileBackend = metadataOnlyBackend{Backend: memory}
		}
		return object, err
	}}
	if err := engine.Files().abortAndDeleteRuntimeUploadLease(ctx, "transfer-support-race"); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("transfer support race error = %v; want precondition failed", err)
	}
}

func TestSchema008UploadBatchBoundsHeadContentionAndPropagatesProviderErrors(t *testing.T) {
	ctx := context.Background()
	live := namespaceTestScope(t, domain.AreaLive)
	request := domain.CreateUploadRequest{
		Path: domain.MustParseUserPath("/batch-provider-boundary.bin"), Size: 1,
		MediaType: "application/octet-stream", IdempotencyKey: "batch-provider-boundary",
	}
	reference := uploadDomainReference(live.UserID())
	headKey := storageformat.DomainHeadKey(reference.Kind, reference.ID)

	t.Run("unexpected-provider-error", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		failure := domain.NewError(domain.ErrorInvalid, "injected batch head failure")
		engine.backend = &hookedBackend{Backend: memory, put: func(callCtx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if key == headKey && condition.Mode == objectstore.PutMatch {
				return "", failure
			}
			return memory.Put(callCtx, key, body, condition)
		}}
		if _, err := engine.Files().createUploadBatch008(ctx, live, []domain.CreateUploadRequest{request}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("unexpected batch provider error = %v; want invalid", err)
		}
	})

	t.Run("persistent-head-contention", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		attempts := 0
		engine.backend = &hookedBackend{Backend: memory, put: func(callCtx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if key == headKey && condition.Mode == objectstore.PutMatch {
				attempts++
				return "", domain.NewError(domain.ErrorConflict, "injected batch head contention")
			}
			return memory.Put(callCtx, key, body, condition)
		}}
		if _, err := engine.Files().createUploadBatch008(ctx, live, []domain.CreateUploadRequest{request}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("persistent batch contention error = %v; want unavailable", err)
		}
		if attempts != 8 {
			t.Fatalf("batch head attempts = %d; want 8", attempts)
		}
	})
}
