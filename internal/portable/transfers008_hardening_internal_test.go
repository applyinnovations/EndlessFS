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
