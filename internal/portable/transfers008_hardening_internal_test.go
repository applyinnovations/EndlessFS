package portable

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
	resumeErr   error
	progressErr error
	abortErr    error
	abortOK     bool
	downloadErr error
}

func (backend *transferFailureBackend) BackendKind() string { return backend.transfers.BackendKind() }
func (backend *transferFailureBackend) BeginUpload(ctx context.Context, request objectstore.UploadRequest) (objectstore.UploadHandle, error) {
	if backend.beginErr != nil {
		return objectstore.UploadHandle{}, backend.beginErr
	}
	return backend.transfers.BeginUpload(ctx, request)
}
func (backend *transferFailureBackend) ResumeUpload(ctx context.Context, lease []byte) (objectstore.UploadCapability, error) {
	if backend.resumeErr != nil {
		return objectstore.UploadCapability{}, backend.resumeErr
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

func TestSchema008TerminalUploadsRemoveRuntimeLeasesAndReplay(t *testing.T) {
	ctx := context.Background()
	newFixture := func(t *testing.T) (*objectmemory.Backend, *Engine, *httptest.Server, domain.Scope) {
		t.Helper()
		backend := objectmemory.New()
		clock := domain.NewFixedClock(time.Date(2062, 2, 3, 4, 5, 6, 0, time.UTC))
		server := httptest.NewServer(backend)
		t.Cleanup(server.Close)
		if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x62}, 1<<20)))); err != nil {
			t.Fatal(err)
		}
		return backend, openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat(t.Name(), 1<<14))), server, namespaceTestScope(t, domain.AreaLive)
	}
	assertLeaseMissing := func(t *testing.T, backend *objectmemory.Backend, uploadID domain.UploadID) {
		t.Helper()
		key := storageformat.LeaseKey(backend.BackendKind(), string(uploadID))
		if _, err := backend.Get(ctx, key); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("runtime lease %s remains: %v", key, err)
		}
	}

	t.Run("complete", func(t *testing.T) {
		backend, engine, server, scope := newFixture(t)
		body := []byte("completed upload")
		path := domain.MustParseUserPath("/complete.bin")
		capability, err := engine.Files().CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: path, Size: int64(len(body)), MediaType: "application/octet-stream"})
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequestWithContext(ctx, capability.Method, capability.URL, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for name, value := range capability.Headers {
			request.Header.Set(name, value)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("upload status = %d", response.StatusCode)
		}
		completion := domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: int64(len(body)), MediaType: "application/octet-stream"}
		first, err := engine.Files().CompleteUpload(ctx, scope, completion)
		if err != nil {
			t.Fatal(err)
		}
		assertLeaseMissing(t, backend, capability.UploadID)
		replayed, err := engine.Files().CompleteUpload(ctx, scope, completion)
		if err != nil || replayed.Version != first.Version {
			t.Fatalf("completion replay = %+v, %v; want version %q", replayed, err, first.Version)
		}
		assertLeaseMissing(t, backend, capability.UploadID)
	})

	t.Run("abort", func(t *testing.T) {
		backend, engine, _, scope := newFixture(t)
		capability, err := engine.Files().CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/abort.bin"), Size: 1, MediaType: "application/octet-stream"})
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.Files().AbortUpload(ctx, scope, capability.UploadID); err != nil {
			t.Fatal(err)
		}
		assertLeaseMissing(t, backend, capability.UploadID)
		if err := engine.Files().AbortUpload(ctx, scope, capability.UploadID); err != nil {
			t.Fatalf("abort replay = %v", err)
		}
		assertLeaseMissing(t, backend, capability.UploadID)
	})
}

func TestSchema008UploadIdempotencyAndTransferBoundaryFailures(t *testing.T) {
	ctx := context.Background()
	live := namespaceTestScope(t, domain.AreaLive)
	t.Run("corrupt-and-missing-idempotency-target", func(t *testing.T) {
		for name, body := range map[string][]byte{
			"corrupt": []byte("bad"),
			"missing-target": func() []byte {
				record := storageformat.PortableUploadIdempotency{SchemaVersion: 1, OwnerID: live.UserID().String(), KeyDigest: storageformat.Digest([]byte("key")), Fingerprint: storageformat.Digest([]byte("fingerprint")), UploadID: "missing"}
				encoded, _ := storageformat.EncodeCanonical(record)
				return encoded
			}(),
		} {
			t.Run(name, func(t *testing.T) {
				engine := openNamespaceTestEngine(t, objectmemory.New())
				store := engine.stateDomainStore()
				if _, err := store.mutate(ctx, uploadDomainReference(live.UserID()), consistencyDomainMutation{ID: "seed-" + name, Changes: []consistencyDomainChange{{Key: uploadIdempotencyKey("key"), Require: domainValueAbsent, Value: body}}}); err != nil {
					t.Fatal(err)
				}
				view, err := newNamespaceStore(engine).loadView(ctx, live.UserID(), "")
				if err != nil {
					t.Fatal(err)
				}
				_, found, err := engine.Files().portableUploadByIdempotencyAtView(ctx, view, live.UserID(), "key", storageformat.Digest([]byte("fingerprint")))
				if !found || err == nil {
					t.Fatalf("idempotency lookup found=%v error=%v", found, err)
				}
				_, found, err = engine.Files().portableUploadByIdempotency(ctx, live.UserID(), "key", storageformat.Digest([]byte("fingerprint")))
				if !found || err == nil {
					t.Fatalf("non-view idempotency lookup found=%v error=%v", found, err)
				}
			})
		}
	})

	t.Run("missing-transfer-and-lease", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		files := engine.Files()
		active := checkpointUploadRecord(engine, live.UserID(), "active-transfer", engine.clock.Now().Add(time.Hour))
		if _, err := files.resumePortableUpload(ctx, active); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing lease resume error = %v", err)
		}
		leaseKey := storageformat.LeaseKey(memory.BackendKind(), active.UploadID)
		if _, err := memory.Put(ctx, leaseKey, []byte(active.UploadID), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		engine.fileBackend = &hookedBackend{Backend: memory}
		if _, _, err := files.runtimeUploadLease(ctx, active.UploadID); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("runtime lease without transfers error = %v", err)
		}
	})

	t.Run("create-input-denials", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		files := engine.Files()
		base := domain.CreateUploadRequest{Path: domain.MustParseUserPath("/file.bin"), Size: 1, MediaType: "application/octet-stream"}
		invalidMedia := base
		invalidMedia.MediaType = "invalid media type"
		if _, err := files.createUpload008(ctx, live, invalidMedia); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid media error = %v", err)
		}
		invalidConflict := base
		invalidConflict.Conflict = "invalid"
		if _, err := files.createUpload008(ctx, live, invalidConflict); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid conflict error = %v", err)
		}
		invalidKey := base
		invalidKey.IdempotencyKey = strings.Repeat("x", 129)
		if _, err := files.createUpload008(ctx, live, invalidKey); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid idempotency key error = %v", err)
		}
	})
}

func TestSchema008UploadHelpersFailClosedBeforeProviderPublication(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	engine := openNamespaceTestEngine(t, backend)
	files := engine.Files()
	live := namespaceTestScope(t, domain.AreaLive)

	if _, _, _, _, err := files.portableUploadSnapshot(ctx, live.UserID(), "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing upload snapshot error = %v", err)
	}
	expired := checkpointUploadRecord(engine, live.UserID(), "expired-initialize", engine.clock.Now().Add(-time.Second))
	expired.State = storageformat.UploadInitializing
	if _, err := files.initializePortableUpload(ctx, expired, true); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expired initialization error = %v", err)
	}
	invalidOwner := expired
	invalidOwner.UploadID, invalidOwner.BlobID, invalidOwner.OwnerID = "invalid-owner", "invalid-owner", "invalid"
	invalidOwner.CreatedAt, invalidOwner.ExpiresAt = engine.clock.Now(), engine.clock.Now().Add(time.Hour)
	if _, err := files.initializePortableUpload(ctx, invalidOwner, true); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid upload owner error = %v", err)
	}

	base := domain.CreateUploadRequest{Path: domain.MustParseUserPath("/batch.bin"), Size: 1, MediaType: "application/octet-stream", IdempotencyKey: "batch-key"}
	for name, request := range map[string]domain.CreateUploadRequest{
		"root":        {Path: domain.MustParseUserPath("/"), Size: 1, MediaType: base.MediaType, IdempotencyKey: base.IdempotencyKey},
		"size":        {Path: base.Path, Size: -1, MediaType: base.MediaType, IdempotencyKey: base.IdempotencyKey},
		"media-type":  {Path: base.Path, Size: 1, MediaType: "invalid media type", IdempotencyKey: base.IdempotencyKey},
		"conflict":    {Path: base.Path, Size: 1, MediaType: base.MediaType, Conflict: "invalid", IdempotencyKey: base.IdempotencyKey},
		"missing-key": {Path: base.Path, Size: 1, MediaType: base.MediaType},
	} {
		t.Run("normalize-"+name, func(t *testing.T) {
			if _, _, err := normalizePortableUploadRequest(live, request); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("normalize error = %v", err)
			}
		})
	}
	if normalized, fingerprint, err := normalizePortableUploadRequest(live, base); err != nil || normalized.MediaType != base.MediaType || fingerprint == "" {
		t.Fatalf("normalized upload = %+v, %q, %v", normalized, fingerprint, err)
	}
	if _, err := files.createUploadBatch008(ctx, live, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty upload batch error = %v", err)
	}
	tooMany := make([]domain.CreateUploadRequest, 101)
	if _, err := files.createUploadBatch008(ctx, live, tooMany); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized upload batch error = %v", err)
	}
	if _, err := files.createUploadBatch008(ctx, live, []domain.CreateUploadRequest{base, base}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("repeated batch key error = %v", err)
	}

	initializing := checkpointUploadRecord(engine, live.UserID(), "status-initializing", engine.clock.Now().Add(time.Hour))
	initializing.State = storageformat.UploadInitializing
	seedCheckpointUploadRecord(t, engine, live.UserID(), initializing)
	status, err := files.uploadStatus008(ctx, live, domain.UploadID(initializing.UploadID))
	if err != nil || status.State != domain.UploadStateActive {
		t.Fatalf("initializing upload status = %+v, %v", status, err)
	}

	aborted := checkpointUploadRecord(engine, live.UserID(), "complete-aborted", engine.clock.Now().Add(time.Hour))
	aborted.State = storageformat.UploadAborted
	seedCheckpointUploadRecord(t, engine, live.UserID(), aborted)
	if _, err := files.completeUpload008(ctx, live, domain.CompleteUploadRequest{UploadID: domain.UploadID(aborted.UploadID), Path: domain.MustParseUserPath(aborted.RequestedPath), Size: aborted.Size, MediaType: aborted.MediaType}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("completion of aborted upload error = %v", err)
	}

	engine.fileBackend = metadataOnlyBackend{Backend: backend}
	if _, _, err := files.ensurePortableUploadLease(ctx, initializing, true); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("upload lease without transfer backend error = %v", err)
	}
	if err := files.deleteRuntimeUploadLease(ctx, initializing.UploadID); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("lease delete without transfer backend error = %v", err)
	}
}

func TestSchema008UploadProviderCoordinationFailureMatrix(t *testing.T) {
	ctx := context.Background()
	failure := domain.NewError(domain.ErrorUnavailable, "injected transfer failure")
	newFixture := func(t *testing.T) (*objectmemory.Backend, *Engine, storageformat.PortableUploadRecord) {
		t.Helper()
		backend := objectmemory.New()
		clock := domain.NewFixedClock(time.Date(2064, 1, 2, 3, 4, 5, 0, time.UTC))
		server := httptest.NewServer(backend)
		t.Cleanup(server.Close)
		if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x64}, 1<<20)))); err != nil {
			t.Fatal(err)
		}
		engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat(t.Name(), 1<<14)))
		return backend, engine, checkpointUploadRecord(engine, namespaceTestScope(t, domain.AreaLive).UserID(), "provider-coordination", clock.Now().Add(time.Hour))
	}

	t.Run("begin", func(t *testing.T) {
		backend, engine, record := newFixture(t)
		engine.fileBackend = &transferFailureBackend{Backend: backend, transfers: backend, beginErr: failure}
		if _, _, err := engine.Files().ensurePortableUploadLease(ctx, record, true); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("begin failure error = %v", err)
		}
	})

	t.Run("lease-read", func(t *testing.T) {
		backend, engine, record := newFixture(t)
		leaseKey := storageformat.LeaseKey(backend.BackendKind(), record.UploadID)
		engine.backend = &hookedBackend{Backend: backend, get: func(callCtx context.Context, key objectstore.Key) (objectstore.Object, error) {
			if key == leaseKey {
				return objectstore.Object{}, failure
			}
			return backend.Get(callCtx, key)
		}}
		if _, _, err := engine.Files().ensurePortableUploadLease(ctx, record, false); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("lease read failure error = %v", err)
		}
	})

	t.Run("lease-publication", func(t *testing.T) {
		backend, engine, record := newFixture(t)
		leaseKey := storageformat.LeaseKey(backend.BackendKind(), record.UploadID)
		engine.backend = &hookedBackend{Backend: backend, put: func(callCtx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if key == leaseKey {
				return "", failure
			}
			return backend.Put(callCtx, key, body, condition)
		}}
		if _, _, err := engine.Files().ensurePortableUploadLease(ctx, record, true); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("lease publication failure error = %v", err)
		}
	})

	t.Run("lease-race-winner-read", func(t *testing.T) {
		backend, engine, record := newFixture(t)
		leaseKey := storageformat.LeaseKey(backend.BackendKind(), record.UploadID)
		engine.backend = &hookedBackend{Backend: backend,
			put: func(callCtx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
				if key == leaseKey {
					return "", domain.NewError(domain.ErrorConflict, "lease winner")
				}
				return backend.Put(callCtx, key, body, condition)
			},
			get: func(callCtx context.Context, key objectstore.Key) (objectstore.Object, error) {
				if key == leaseKey {
					return objectstore.Object{}, failure
				}
				return backend.Get(callCtx, key)
			},
		}
		if _, _, err := engine.Files().ensurePortableUploadLease(ctx, record, true); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("lease winner read failure error = %v", err)
		}
	})

	t.Run("empty-and-unresumable-lease", func(t *testing.T) {
		backend, engine, record := newFixture(t)
		leaseKey := storageformat.LeaseKey(backend.BackendKind(), record.UploadID)
		if _, err := backend.Put(ctx, leaseKey, []byte{}, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := engine.Files().ensurePortableUploadLease(ctx, record, false); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("empty lease error = %v", err)
		}

		backend, engine, record = newFixture(t)
		leaseKey = storageformat.LeaseKey(backend.BackendKind(), record.UploadID)
		if _, err := backend.Put(ctx, leaseKey, []byte("lease"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		engine.fileBackend = &transferFailureBackend{Backend: backend, transfers: backend, resumeErr: failure}
		if _, _, err := engine.Files().ensurePortableUploadLease(ctx, record, false); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("resume lease error = %v", err)
		}
		if _, err := engine.Files().resumePortableUpload(ctx, record); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("resume active upload error = %v", err)
		}
	})
}

func TestSchema008UploadLookupBatchAndStatusFailureBoundaries(t *testing.T) {
	ctx := context.Background()
	failure := domain.NewError(domain.ErrorUnavailable, "injected upload boundary failure")
	live := namespaceTestScope(t, domain.AreaLive)

	t.Run("registered-missing-and-transition-locked", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		files := engine.Files()
		store := engine.stateDomainStore()
		reference := uploadDomainReference(live.UserID())
		if err := store.ensureRegistered(ctx, reference); err != nil {
			t.Fatal(err)
		}
		if _, _, _, _, err := files.portableUploadSnapshot(ctx, live.UserID(), "missing"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("registered missing upload error = %v", err)
		}
		lock := storageformat.TransitionLock009{SchemaVersion: 1, TransitionID: "upload-transition", Fingerprint: storageformat.Digest([]byte("upload-transition")), Kind: reference.Kind, DomainID: reference.ID}
		body, err := storageformat.EncodeCanonical(lock)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: "install-upload-lock", Changes: []consistencyDomainChange{{Key: transitionLockKey009, Require: domainValueAbsent, Value: body}}}); err != nil {
			t.Fatal(err)
		}
		if _, _, _, _, err := files.portableUploadSnapshot(ctx, live.UserID(), "missing"); !errors.Is(err, errTransitionPending009) {
			t.Fatalf("transition-locked upload error = %v", err)
		}
	})

	t.Run("tree-and-idempotency-provider-reads", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		files := engine.Files()
		record := checkpointUploadRecord(engine, live.UserID(), "lookup-provider", engine.clock.Now().Add(time.Hour))
		seedCheckpointUploadRecord(t, engine, live.UserID(), record)
		reference := uploadDomainReference(live.UserID())
		store := engine.stateDomainStore()
		if err := store.compact(ctx, reference); err != nil {
			t.Fatal(err)
		}
		snapshot, err := store.loadHead(ctx, reference)
		if err != nil {
			t.Fatal(err)
		}
		hooks := &hookedBackend{Backend: memory, get: func(callCtx context.Context, key objectstore.Key) (objectstore.Object, error) {
			if strings.HasPrefix(key.String(), storageformat.DomainPrefix()) {
				return objectstore.Object{}, failure
			}
			return memory.Get(callCtx, key)
		}}
		viewStore := newConsistencyDomainStore(hooks, nil, engine.clock)
		view := &namespaceView{reference: reference, head: snapshot.head, session: newConsistencyDomainTreeSession(viewStore, reference)}
		if _, _, err := files.portableUploadAtView(ctx, view, live.UserID(), record.UploadID); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("upload tree read error = %v", err)
		}
		engine.backend = hooks
		if _, _, err := files.portableUploadByIdempotency(ctx, live.UserID(), "key", "fingerprint"); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("idempotency provider read error = %v", err)
		}
	})

	t.Run("superseded-initialization-and-nonterminal-cleanup", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		record := checkpointUploadRecord(engine, live.UserID(), "superseded", engine.clock.Now().Add(time.Hour))
		record.State = storageformat.UploadInitializing
		stored := record
		stored.State = storageformat.UploadAborted
		seedCheckpointUploadRecord(t, engine, live.UserID(), stored)
		if _, err := engine.Files().initializePortableUpload(ctx, record, true); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("superseded initialization error = %v", err)
		}
		active := checkpointUploadRecord(engine, live.UserID(), "nonterminal-cleanup", engine.clock.Now().Add(time.Hour))
		active.CleanupPending = true
		seedCheckpointUploadRecord(t, engine, live.UserID(), active)
		if err := engine.Files().cleanupPortableUpload(ctx, live.UserID(), active.UploadID, nil); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("nonterminal cleanup error = %v", err)
		}
	})

	t.Run("status-progress", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		record := checkpointUploadRecord(engine, live.UserID(), "status-progress", engine.clock.Now().Add(time.Hour))
		seedCheckpointUploadRecord(t, engine, live.UserID(), record)
		if _, err := memory.Put(ctx, storageformat.LeaseKey(memory.BackendKind(), record.UploadID), []byte("lease"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		engine.fileBackend = &transferFailureBackend{Backend: memory, transfers: memory, progressErr: failure}
		if _, err := engine.Files().uploadStatus008(ctx, live, domain.UploadID(record.UploadID)); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("upload progress error = %v", err)
		}
	})

	t.Run("lease-read-and-conditional-delete", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		leaseKey := storageformat.LeaseKey(memory.BackendKind(), "lease-read")
		engine.backend = &hookedBackend{Backend: memory, get: func(callCtx context.Context, key objectstore.Key) (objectstore.Object, error) {
			if key == leaseKey {
				return objectstore.Object{}, failure
			}
			return memory.Get(callCtx, key)
		}}
		if err := engine.Files().deleteRuntimeUploadLease(ctx, "lease-read"); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("runtime lease read error = %v", err)
		}

		engine = openNamespaceTestEngine(t, memory)
		leaseKey = storageformat.LeaseKey(memory.BackendKind(), "lease-delete")
		if _, err := memory.Put(ctx, leaseKey, []byte("lease"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		engine.fileBackend = &transferFailureBackend{Backend: memory, transfers: memory, abortOK: true}
		engine.backend = &hookedBackend{Backend: memory, delete: func(callCtx context.Context, key objectstore.Key, condition objectstore.DeleteCondition) error {
			if key == leaseKey {
				return domain.NewError(domain.ErrorPreconditionFailed, "lease already replaced")
			}
			return memory.Delete(callCtx, key, condition)
		}}
		if err := engine.Files().abortAndDeleteRuntimeUploadLease(ctx, "lease-delete"); err != nil {
			t.Fatalf("conditional lease deletion error = %v", err)
		}
	})

	t.Run("resume-initializing-and-active", func(t *testing.T) {
		memory := objectmemory.New()
		clock := domain.NewFixedClock(time.Date(2065, 2, 3, 4, 5, 6, 0, time.UTC))
		server := httptest.NewServer(memory)
		defer server.Close()
		if err := memory.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x66}, 1<<20)))); err != nil {
			t.Fatal(err)
		}
		engine := openInternalTestEngine(t, memory, clock, strings.NewReader(strings.Repeat(t.Name(), 1<<14)))
		record := checkpointUploadRecord(engine, live.UserID(), "resume-initializing", clock.Now().Add(time.Hour))
		record.State = storageformat.UploadInitializing
		seedCheckpointUploadRecord(t, engine, live.UserID(), record)
		if _, err := engine.Files().resumePortableUpload(ctx, record); err != nil {
			t.Fatalf("resume initializing upload error = %v", err)
		}
		if _, err := engine.Files().initializePortableUpload(ctx, record, false); err != nil {
			t.Fatalf("help already-active initialization error = %v", err)
		}
	})

	t.Run("download-transfer-support", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		entry := publishNamespaceTestFile(t, newNamespaceStore(engine), live, "/download.bin", 1, "download-transfer")
		if _, err := memory.Put(ctx, storageformat.BlobKey(live.UserID().String(), "blob-download-transfer"), []byte{1}, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		engine.fileBackend = metadataOnlyBackend{Backend: memory}
		if _, err := engine.Files().createDownload008(ctx, live, domain.CreateDownloadRequest{Path: entry.Path, Version: entry.Version}); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("download without transfer support error = %v", err)
		}
	})

	t.Run("batch-destination-id-and-session-failures", func(t *testing.T) {
		request := domain.CreateUploadRequest{Path: domain.MustParseUserPath("/batch-boundary.bin"), Size: 1, MediaType: "application/octet-stream", IdempotencyKey: "batch-boundary-one"}
		engine := openNamespaceTestEngine(t, objectmemory.New())
		second := request
		second.IdempotencyKey = "batch-boundary-two"
		if _, err := engine.Files().createUploadBatch008(ctx, live, []domain.CreateUploadRequest{request, second}); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("duplicate batch destination error = %v", err)
		}
		if _, err := engine.Files().createUploadBatch008(ctx, live, []domain.CreateUploadRequest{{Path: domain.MustParseUserPath("/missing-parent/file.bin"), Size: 1, MediaType: "application/octet-stream", IdempotencyKey: "missing-parent"}}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("batch destination resolution error = %v", err)
		}

		engine = openNamespaceTestEngine(t, objectmemory.New())
		normalized, fingerprint, err := normalizePortableUploadRequest(live, request)
		if err != nil {
			t.Fatal(err)
		}
		record := checkpointUploadRecord(engine, live.UserID(), "partial-batch", engine.clock.Now().Add(time.Hour))
		record.RequestedPath, record.ResolvedPath, record.Size, record.MediaType, record.Conflict = normalized.Path.String(), normalized.Path.String(), normalized.Size, normalized.MediaType, normalized.Conflict
		recordBody, err := storageformat.EncodeCanonical(record)
		if err != nil {
			t.Fatal(err)
		}
		idempotencyBody, err := storageformat.EncodeCanonical(storageformat.PortableUploadIdempotency{SchemaVersion: 1, OwnerID: live.UserID().String(), KeyDigest: storageformat.Digest([]byte(request.IdempotencyKey)), Fingerprint: fingerprint, UploadID: record.UploadID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.stateDomainStore().mutate(ctx, uploadDomainReference(live.UserID()), consistencyDomainMutation{ID: "seed-partial-batch", Changes: []consistencyDomainChange{
			{Key: uploadRecordKey(record.UploadID), Require: domainValueAbsent, Value: recordBody},
			{Key: uploadIdempotencyKey(request.IdempotencyKey), Require: domainValueAbsent, Value: idempotencyBody},
		}}); err != nil {
			t.Fatal(err)
		}
		second.Path = domain.MustParseUserPath("/batch-boundary-two.bin")
		if _, err := engine.Files().createUploadBatch008(ctx, live, []domain.CreateUploadRequest{request, second}); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("partially overlapping batch error = %v", err)
		}

		engine = openNamespaceTestEngine(t, objectmemory.New())
		engine.backend = &hookedBackend{Backend: engine.backend, get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, failure
		}}
		if _, err := engine.Files().createUploadBatch008(ctx, live, []domain.CreateUploadRequest{request}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("batch view load error = %v", err)
		}

		engine = openNamespaceTestEngine(t, objectmemory.New())
		engine.ids = domain.NewIDGenerator(bytes.NewReader(nil))
		if _, err := engine.Files().createUploadBatch008(ctx, live, []domain.CreateUploadRequest{request}); err == nil {
			t.Fatal("upload batch accepted exhausted ID entropy")
		}

		memory := objectmemory.New()
		clock := domain.NewFixedClock(time.Date(2065, 1, 2, 3, 4, 5, 0, time.UTC))
		server := httptest.NewServer(memory)
		defer server.Close()
		if err := memory.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x65}, 1<<20)))); err != nil {
			t.Fatal(err)
		}
		engine = openInternalTestEngine(t, memory, clock, strings.NewReader(strings.Repeat(t.Name(), 1<<14)))
		sessionRecord := checkpointUploadRecord(engine, live.UserID(), "batch-session", clock.Now().Add(time.Hour))
		sessionRecord.Batch = &storageformat.PortableUploadBatchMember{BatchID: "batch", Index: 0, Count: 1}
		engine.fileBackend = &transferFailureBackend{Backend: memory, transfers: memory, beginErr: failure}
		if _, err := engine.Files().activatePortableUploadBatch(ctx, live.UserID(), []portableUploadBatchItem{{record: sessionRecord, fingerprint: "fingerprint"}}, "batch", true); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("batch session error = %v", err)
		}
	})
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
