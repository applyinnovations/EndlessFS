package portable_test

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
)

type failNthBackend struct {
	backend *objectmemory.Backend
	failAt  int
	calls   int
}

func (backend *failNthBackend) arm(call int) { backend.failAt, backend.calls = call, 0 }
func (backend *failNthBackend) disable()     { backend.failAt = 0 }
func (backend *failNthBackend) fault() error {
	backend.calls++
	if backend.failAt > 0 && backend.calls == backend.failAt {
		return domain.NewError(domain.ErrorUnavailable, "injected object transport interruption")
	}
	return nil
}
func (backend *failNthBackend) Head(ctx context.Context, key objectstore.Key) (objectstore.ObjectInfo, error) {
	if err := backend.fault(); err != nil {
		return objectstore.ObjectInfo{}, err
	}
	return backend.backend.Head(ctx, key)
}
func (backend *failNthBackend) Verify(ctx context.Context, key objectstore.Key, expected objectstore.ExpectedIntegrity) (objectstore.ObjectInfo, error) {
	if err := backend.fault(); err != nil {
		return objectstore.ObjectInfo{}, err
	}
	return backend.backend.Verify(ctx, key, expected)
}
func (backend *failNthBackend) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	if err := backend.fault(); err != nil {
		return objectstore.Object{}, err
	}
	return backend.backend.Get(ctx, key)
}
func (backend *failNthBackend) Open(ctx context.Context, key objectstore.Key) (objectstore.ObjectReader, error) {
	if err := backend.fault(); err != nil {
		return objectstore.ObjectReader{}, err
	}
	return backend.backend.Open(ctx, key)
}
func (backend *failNthBackend) List(ctx context.Context, request objectstore.ListRequest) (objectstore.ListPage, error) {
	if err := backend.fault(); err != nil {
		return objectstore.ListPage{}, err
	}
	return backend.backend.List(ctx, request)
}
func (backend *failNthBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	if err := backend.fault(); err != nil {
		return "", err
	}
	return backend.backend.Put(ctx, key, body, condition)
}
func (backend *failNthBackend) Delete(ctx context.Context, key objectstore.Key, condition objectstore.DeleteCondition) error {
	if err := backend.fault(); err != nil {
		return err
	}
	return backend.backend.Delete(ctx, key, condition)
}
func (backend *failNthBackend) Copy(ctx context.Context, source, destination objectstore.Key, condition objectstore.CopyCondition) (objectstore.CopyResult, error) {
	if err := backend.fault(); err != nil {
		return objectstore.CopyResult{}, err
	}
	return backend.backend.Copy(ctx, source, destination, condition)
}
func (backend *failNthBackend) BackendKind() string { return backend.backend.BackendKind() }
func (backend *failNthBackend) BeginUpload(ctx context.Context, request objectstore.UploadRequest) (objectstore.UploadHandle, error) {
	if err := backend.fault(); err != nil {
		return objectstore.UploadHandle{}, err
	}
	return backend.backend.BeginUpload(ctx, request)
}
func (backend *failNthBackend) ResumeUpload(ctx context.Context, lease []byte) (objectstore.UploadCapability, error) {
	if err := backend.fault(); err != nil {
		return objectstore.UploadCapability{}, err
	}
	return backend.backend.ResumeUpload(ctx, lease)
}
func (backend *failNthBackend) UploadProgress(ctx context.Context, lease []byte) (objectstore.UploadProgress, error) {
	if err := backend.fault(); err != nil {
		return objectstore.UploadProgress{}, err
	}
	return backend.backend.UploadProgress(ctx, lease)
}
func (backend *failNthBackend) AbortUpload(ctx context.Context, lease []byte) error {
	if err := backend.fault(); err != nil {
		return err
	}
	return backend.backend.AbortUpload(ctx, lease)
}
func (backend *failNthBackend) CreateDownload(ctx context.Context, request objectstore.DownloadRequest) (objectstore.DownloadCapability, error) {
	if err := backend.fault(); err != nil {
		return objectstore.DownloadCapability{}, err
	}
	return backend.backend.CreateDownload(ctx, request)
}

type aggregateBarrier struct {
	target  int
	release chan struct{}
	mu      sync.Mutex
	arrived int
}

func newAggregateBarrier(target int) *aggregateBarrier {
	return &aggregateBarrier{target: target, release: make(chan struct{})}
}
func (barrier *aggregateBarrier) Wait() {
	barrier.mu.Lock()
	barrier.arrived++
	if barrier.arrived == barrier.target {
		close(barrier.release)
	}
	barrier.mu.Unlock()
	<-barrier.release
}

type aggregateOneShotScheduler struct {
	step    string
	barrier *aggregateBarrier
	mu      sync.Mutex
	enabled bool
	used    bool
}

type stepFailure struct {
	mu   sync.Mutex
	step string
	done bool
}

func (failure *stepFailure) Step(_ context.Context, step string) error {
	failure.mu.Lock()
	defer failure.mu.Unlock()
	if step == failure.step && !failure.done {
		failure.done = true
		return domain.NewError(domain.ErrorUnavailable, "injected replica loss")
	}
	return nil
}

func (scheduler *aggregateOneShotScheduler) Enable() {
	scheduler.mu.Lock()
	scheduler.enabled = true
	scheduler.mu.Unlock()
}
func (scheduler *aggregateOneShotScheduler) Step(_ context.Context, step string) error {
	scheduler.mu.Lock()
	if step != scheduler.step || !scheduler.enabled || scheduler.used {
		scheduler.mu.Unlock()
		return nil
	}
	scheduler.used = true
	scheduler.mu.Unlock()
	scheduler.barrier.Wait()
	return nil
}

func assertVisibleRecursiveAggregates(t *testing.T, files interface {
	List(context.Context, domain.Scope, domain.ListRequest) (domain.ListPage, error)
	Stat(context.Context, domain.Scope, domain.UserPath) (domain.Entry, error)
}, scope domain.Scope, directory domain.UserPath) int64 {
	t.Helper()
	var total, fileCount int64
	cursor := ""
	for {
		page, err := files.List(context.Background(), scope, domain.ListRequest{Directory: directory, PageSize: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("List(%s): %v", directory, err)
		}
		for _, entry := range page.Entries {
			if entry.Kind == domain.EntryFile {
				total += entry.Size
				fileCount++
				continue
			}
			total += assertVisibleRecursiveAggregates(t, files, scope, entry.Path)
			child, err := files.Stat(context.Background(), scope, entry.Path)
			if err != nil {
				t.Fatal(err)
			}
			fileCount += child.FileCount
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	entry, err := files.Stat(context.Background(), scope, directory)
	if err != nil || entry.Size != total || entry.FileCount != fileCount {
		t.Errorf("aggregate %s = %+v, %v; visible=%d/%d", directory, entry, err, total, fileCount)
	}
	return total
}

func openEngine(t *testing.T, backend *objectmemory.Backend, clock *domain.FixedClock, seed byte, scheduler portable.Scheduler) *portable.Engine {
	t.Helper()
	engine, err := portable.Open(context.Background(), portable.Options{
		Backend: backend, Clock: clock, IDs: domain.NewIDGenerator(bytes.NewReader(deterministic(seed, 1<<20))),
		Writer:   portable.WriterConfiguration{WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1", KeyringIdentifiers: []string{"session-v1"}},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x63}, 32), Scheduler: scheduler,
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func uploadPortableFile(t *testing.T, client *http.Client, files interface {
	CreateUpload(context.Context, domain.Scope, domain.CreateUploadRequest) (domain.UploadCapability, error)
	CompleteUpload(context.Context, domain.Scope, domain.CompleteUploadRequest) (domain.Entry, error)
}, scope domain.Scope, path domain.UserPath, body []byte) {
	t.Helper()
	capability, err := files.CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: path, Size: int64(len(body)), MediaType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequestWithContext(context.Background(), capability.Method, capability.URL, bytes.NewReader(body))
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("upload status = %d", response.StatusCode)
	}
	if _, err := files.CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: int64(len(body)), MediaType: "text/plain"}); err != nil {
		t.Fatal(err)
	}
}
