package portable_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
)

type uploadProtocolFaultBackend struct {
	*failNthBackend
	failPuts     bool
	failAbort    bool
	failDelete   bool
	beginUploads int
}

func (backend *uploadProtocolFaultBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	if backend.failPuts {
		return "", domain.NewError(domain.ErrorUnavailable, "injected durable-state write failure")
	}
	return backend.failNthBackend.Put(ctx, key, body, condition)
}

func (backend *uploadProtocolFaultBackend) BeginUpload(ctx context.Context, request objectstore.UploadRequest) (objectstore.UploadHandle, error) {
	backend.beginUploads++
	return backend.failNthBackend.BeginUpload(ctx, request)
}

func (backend *uploadProtocolFaultBackend) AbortUpload(ctx context.Context, lease []byte) error {
	if backend.failAbort {
		backend.failAbort = false
		return domain.NewError(domain.ErrorUnavailable, "injected upload cleanup failure")
	}
	return backend.failNthBackend.AbortUpload(ctx, lease)
}

func (backend *uploadProtocolFaultBackend) Delete(ctx context.Context, key objectstore.Key, condition objectstore.DeleteCondition) error {
	if backend.failDelete {
		backend.failDelete = false
		return domain.NewError(domain.ErrorUnavailable, "injected lease cleanup failure")
	}
	return backend.failNthBackend.Delete(ctx, key, condition)
}

func newUploadProtocolFixture(t *testing.T) (*uploadProtocolFaultBackend, *objectmemory.Backend, *httptest.Server, *domain.FixedClock, domain.Scope) {
	t.Helper()
	base := objectmemory.New()
	server := httptest.NewServer(base)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2051, 2, 3, 4, 5, 6, 0, time.UTC))
	if err := base.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(211, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	backend := &uploadProtocolFaultBackend{failNthBackend: &failNthBackend{backend: base}}
	owner, _ := domain.ParseUserID("zMzMzMzMzMzMzMzMzMzMzA")
	scope, _ := domain.NewScope(owner, domain.AreaLive)
	return backend, base, server, clock, scope
}

func TestUploadIntentIsDurableBeforeProviderInitiation(t *testing.T) {
	backend, _, _, clock, scope := newUploadProtocolFixture(t)
	engine := openFaultEngine(t, backend, clock, 212)
	backend.failPuts = true
	_, err := engine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{
		Path: domain.MustParseUserPath("/intent-first.bin"), Size: 4, MediaType: "application/octet-stream", IdempotencyKey: "intent-first-upload",
	})
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("CreateUpload() error = %v", err)
	}
	if backend.beginUploads != 0 {
		t.Fatalf("provider upload initiations before durable intent = %d, want 0", backend.beginUploads)
	}
}

func TestTerminalUploadCommitDoesNotFailBecauseCleanupIsTemporarilyUnavailable(t *testing.T) {
	backend, _, server, clock, scope := newUploadProtocolFixture(t)
	engine := openFaultEngine(t, backend, clock, 213)
	abortCapability, err := engine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/abort.bin"), Size: 1, MediaType: "application/octet-stream"})
	if err != nil {
		t.Fatal(err)
	}
	backend.failAbort = true
	if err := engine.Files().AbortUpload(context.Background(), scope, abortCapability.UploadID); err != nil {
		t.Fatalf("logical abort failed because cleanup failed: %v", err)
	}
	if status, err := engine.Files().UploadStatus(context.Background(), scope, abortCapability.UploadID); err != nil || status.State != domain.UploadStateAborted {
		t.Fatalf("aborted status = %+v, %v", status, err)
	}
	if err := engine.Files().AbortUpload(context.Background(), scope, abortCapability.UploadID); err != nil {
		t.Fatalf("abort cleanup retry = %v", err)
	}

	body := []byte("done")
	path := domain.MustParseUserPath("/complete.bin")
	completeCapability, err := engine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: path, Size: int64(len(body)), MediaType: "application/octet-stream"})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(completeCapability.Method, completeCapability.URL, bytes.NewReader(body))
	for name, value := range completeCapability.Headers {
		request.Header.Set(name, value)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	backend.failDelete = true
	entry, err := engine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: completeCapability.UploadID, Path: path, Size: int64(len(body)), MediaType: "application/octet-stream"})
	if err != nil || entry.Path != path {
		t.Fatalf("logical completion failed because cleanup failed: %+v, %v", entry, err)
	}
	if _, err := engine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: completeCapability.UploadID, Path: path, Size: int64(len(body)), MediaType: "application/octet-stream"}); err != nil {
		t.Fatalf("completion cleanup retry = %v", err)
	}
}
