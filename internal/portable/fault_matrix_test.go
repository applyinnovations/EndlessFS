package portable_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type failNthBackend struct {
	mu               sync.Mutex
	backend          *objectmemory.Backend
	failAt           int
	calls            int
	failureOperation string
}

func (backend *failNthBackend) arm(call int) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.failAt, backend.calls, backend.failureOperation = call, 0, ""
}
func (backend *failNthBackend) disable() {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.failAt = 0
}
func (backend *failNthBackend) fault(operation string) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.calls++
	if backend.failAt > 0 && backend.calls == backend.failAt {
		backend.failureOperation = operation
		return domain.NewError(domain.ErrorUnavailable, "injected object transport interruption")
	}
	return nil
}

func TestFailNthBackendCountsConcurrentProviderCallsSafely(t *testing.T) {
	const total = 64
	backend := &failNthBackend{}
	backend.arm(17)
	errorsSeen := make(chan error, total)
	var wait sync.WaitGroup
	for call := range total {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsSeen <- backend.fault(fmt.Sprintf("concurrent-%d", call))
		}()
	}
	wait.Wait()
	close(errorsSeen)
	failures := 0
	for err := range errorsSeen {
		if err != nil {
			failures++
		}
	}
	if backend.calls != total || failures != 1 {
		t.Fatalf("concurrent fault count = %d calls, %d failures; want %d, 1", backend.calls, failures, total)
	}
}

func (backend *failNthBackend) Head(ctx context.Context, key objectstore.Key) (objectstore.ObjectInfo, error) {
	if err := backend.fault("HEAD " + key.String()); err != nil {
		return objectstore.ObjectInfo{}, err
	}
	return backend.backend.Head(ctx, key)
}

func (backend *failNthBackend) Verify(ctx context.Context, key objectstore.Key, expected objectstore.ExpectedIntegrity) (objectstore.ObjectInfo, error) {
	if err := backend.fault("VERIFY " + key.String()); err != nil {
		return objectstore.ObjectInfo{}, err
	}
	return backend.backend.Verify(ctx, key, expected)
}

func (backend *failNthBackend) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	if err := backend.fault("GET " + key.String()); err != nil {
		return objectstore.Object{}, err
	}
	return backend.backend.Get(ctx, key)
}

func (backend *failNthBackend) Open(ctx context.Context, key objectstore.Key) (objectstore.ObjectReader, error) {
	if err := backend.fault("OPEN " + key.String()); err != nil {
		return objectstore.ObjectReader{}, err
	}
	return backend.backend.Open(ctx, key)
}

func (backend *failNthBackend) List(ctx context.Context, request objectstore.ListRequest) (objectstore.ListPage, error) {
	if err := backend.fault("LIST " + request.Prefix); err != nil {
		return objectstore.ListPage{}, err
	}
	return backend.backend.List(ctx, request)
}

func (backend *failNthBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	if err := backend.fault("PUT " + key.String()); err != nil {
		return "", err
	}
	return backend.backend.Put(ctx, key, body, condition)
}

func (backend *failNthBackend) Delete(ctx context.Context, key objectstore.Key, condition objectstore.DeleteCondition) error {
	if err := backend.fault("DELETE " + key.String()); err != nil {
		return err
	}
	return backend.backend.Delete(ctx, key, condition)
}

func (backend *failNthBackend) Copy(ctx context.Context, source, destination objectstore.Key, condition objectstore.CopyCondition) (objectstore.CopyResult, error) {
	if err := backend.fault("COPY " + source.String() + " -> " + destination.String()); err != nil {
		return objectstore.CopyResult{}, err
	}
	return backend.backend.Copy(ctx, source, destination, condition)
}

func (backend *failNthBackend) BackendKind() string { return backend.backend.BackendKind() }

func (backend *failNthBackend) BeginUpload(ctx context.Context, request objectstore.UploadRequest) (objectstore.UploadHandle, error) {
	if err := backend.fault("BEGIN-UPLOAD " + request.Key.String()); err != nil {
		return objectstore.UploadHandle{}, err
	}
	return backend.backend.BeginUpload(ctx, request)
}

func (backend *failNthBackend) ResumeUpload(ctx context.Context, lease []byte) (objectstore.UploadCapability, error) {
	if err := backend.fault("RESUME-UPLOAD"); err != nil {
		return objectstore.UploadCapability{}, err
	}
	return backend.backend.ResumeUpload(ctx, lease)
}

func (backend *failNthBackend) UploadProgress(ctx context.Context, lease []byte) (objectstore.UploadProgress, error) {
	if err := backend.fault("UPLOAD-PROGRESS"); err != nil {
		return objectstore.UploadProgress{}, err
	}
	return backend.backend.UploadProgress(ctx, lease)
}

func (backend *failNthBackend) AbortUpload(ctx context.Context, lease []byte) error {
	if err := backend.fault("ABORT-UPLOAD"); err != nil {
		return err
	}
	return backend.backend.AbortUpload(ctx, lease)
}

func (backend *failNthBackend) CreateDownload(ctx context.Context, request objectstore.DownloadRequest) (objectstore.DownloadCapability, error) {
	if err := backend.fault("CREATE-DOWNLOAD " + request.Key.String()); err != nil {
		return objectstore.DownloadCapability{}, err
	}
	return backend.backend.CreateDownload(ctx, request)
}

func openFaultEngine(t *testing.T, backend objectstore.Backend, clock *domain.FixedClock, seed byte) *portable.Engine {
	t.Helper()
	engine, err := portable.Open(context.Background(), portable.Options{
		Backend: backend, Clock: clock, IDs: domain.NewIDGenerator(bytes.NewReader(deterministic(seed, 1<<20))),
		Writer: portable.WriterConfiguration{
			WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1",
			KeyringIdentifiers: []string{"session-v1"},
		},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x63}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestObjectFailureAtEveryRecursiveCopyBoundaryRecoversWithoutPartialVisibility(t *testing.T) {
	t.Parallel()
	fixtureBackend := objectmemory.New()
	fixtureServer := httptest.NewServer(fixtureBackend)
	t.Cleanup(fixtureServer.Close)
	fixtureClock := domain.NewFixedClock(time.Date(2041, 1, 2, 3, 4, 5, 0, time.UTC))
	if err := fixtureBackend.ConfigureDataPlane(fixtureServer.URL, fixtureClock, domain.NewIDGenerator(bytes.NewReader(deterministic(101, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	fixtureEngine := openEngine(t, fixtureBackend, fixtureClock, 102, nil)
	user, _ := domain.ParseUserID("VFRUVFRUVFRUVFRUVFRUVA")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	if _, err := fixtureEngine.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/source")}); err != nil {
		t.Fatal(err)
	}
	uploadPortableFile(t, fixtureServer.Client(), fixtureEngine.Files(), scope, domain.MustParseUserPath("/source/value.txt"), []byte("fault matrix"))
	fixture := fixtureBackend.Export()
	countBackend := objectmemory.New()
	if err := countBackend.Import(fixture); err != nil {
		t.Fatal(err)
	}
	countFaults := &failNthBackend{backend: countBackend}
	countEngine := openFaultEngine(t, countFaults, domain.NewFixedClock(fixtureClock.Now()), 103)
	countFaults.arm(0)
	if _, err := countEngine.Files().Copy(context.Background(), scope, scope, domain.CopyRequest{
		Source: domain.MustParseUserPath("/source"), Destination: domain.MustParseUserPath("/destination"),
		IdempotencyKey: "fault-copy",
	}); err != nil {
		t.Fatal(err)
	}
	lastBoundary := countFaults.calls + 3

	consecutiveSuccesses := 0
	for failAt := 1; failAt <= lastBoundary && consecutiveSuccesses < 3; failAt++ {
		backend := objectmemory.New()
		if err := backend.Import(fixture); err != nil {
			t.Fatal(err)
		}
		clock := domain.NewFixedClock(fixtureClock.Now())
		faults := &failNthBackend{backend: backend}
		engine := openFaultEngine(t, faults, clock, byte(103+failAt%100))
		faults.arm(failAt)
		_, copyErr := engine.Files().Copy(context.Background(), scope, scope, domain.CopyRequest{
			Source: domain.MustParseUserPath("/source"), Destination: domain.MustParseUserPath("/destination"),
			IdempotencyKey: "fault-copy",
		})
		calls := faults.calls
		faults.disable()
		if copyErr == nil && failAt > calls {
			consecutiveSuccesses++
		} else {
			consecutiveSuccesses = 0
		}
		if copyErr != nil && !errors.Is(copyErr, domain.ErrUnavailable) {
			t.Fatalf("failure %d returned unsafe error: %v", failAt, copyErr)
		}
		clock.Advance(2 * time.Minute)
		if _, err := engine.CreateCheckpoint(context.Background(), fmt.Sprintf("copy-fault-%d", failAt)); err != nil {
			t.Fatalf("failure %d recovery: %v", failAt, err)
		}
		if _, err := engine.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/source/value.txt")); err != nil {
			t.Fatalf("failure %d lost source: %v", failAt, err)
		}
		if _, err := engine.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/destination")); err == nil {
			if _, err := engine.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/destination/value.txt")); err != nil {
				t.Fatalf("failure %d exposed partial destination: %v", failAt, err)
			}
		} else if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("failure %d destination status: %v", failAt, err)
		}
	}
	if consecutiveSuccesses < 3 {
		t.Fatal("fault matrix did not pass every recursive-copy object boundary")
	}
}

func TestObjectFailureAtEveryCheckpointBoundaryIsRetrySafe(t *testing.T) {
	fixtureBackend := objectmemory.New()
	fixtureClock := domain.NewFixedClock(time.Date(2041, 2, 3, 4, 5, 6, 0, time.UTC))
	fixtureEngine := openEngine(t, fixtureBackend, fixtureClock, 111, nil)
	if _, err := fixtureEngine.Create(context.Background(), state.MustKey(state.NamespaceAccounts, "checkpoint-fault"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	fixture := fixtureBackend.Export()

	consecutiveSuccesses := 0
	for failAt := 1; failAt <= 100 && consecutiveSuccesses < 3; failAt++ {
		backend := objectmemory.New()
		if err := backend.Import(fixture); err != nil {
			t.Fatal(err)
		}
		clock := domain.NewFixedClock(fixtureClock.Now())
		faults := &failNthBackend{backend: backend}
		engine := openFaultEngine(t, faults, clock, byte(112+failAt%100))
		checkpointID := fmt.Sprintf("checkpoint-fault-%d", failAt)
		faults.arm(failAt)
		_, firstErr := engine.CreateCheckpoint(context.Background(), checkpointID)
		calls := faults.calls
		faults.disable()
		if firstErr == nil && failAt > calls {
			consecutiveSuccesses++
		} else {
			consecutiveSuccesses = 0
		}
		if firstErr != nil && !errors.Is(firstErr, domain.ErrUnavailable) {
			t.Fatalf("failure %d returned unsafe error: %v", failAt, firstErr)
		}
		if _, err := engine.CreateCheckpoint(context.Background(), checkpointID); err != nil {
			t.Fatalf("failure %d retry: %v", failAt, err)
		}
		if err := engine.VerifyCheckpoint(context.Background(), checkpointID); err != nil {
			t.Fatalf("failure %d verification: %v", failAt, err)
		}
	}
	if consecutiveSuccesses < 3 {
		t.Fatal("fault matrix did not pass every checkpoint object boundary")
	}
}

func TestObjectFailureAtEveryUploadInitiationBoundaryDrainsSafely(t *testing.T) {
	user, _ := domain.ParseUserID("WlpaWlpaWlpaWlpaWlpaWg")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	consecutiveSuccesses := 0
	for failAt := 1; failAt <= 100 && consecutiveSuccesses < 3; failAt++ {
		backend := objectmemory.New()
		server := httptest.NewServer(backend)
		clock := domain.NewFixedClock(time.Date(2041, 4, 5, 6, 7, 8, 0, time.UTC))
		if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(byte(130+failAt%100), 1<<20)))); err != nil {
			server.Close()
			t.Fatal(err)
		}
		faults := &failNthBackend{backend: backend}
		engine := openFaultEngine(t, faults, clock, byte(140+failAt%100))
		faults.arm(failAt)
		_, uploadErr := engine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{
			Path: domain.MustParseUserPath("/fault.bin"), Size: 8, MediaType: "application/octet-stream",
			Resumable: true, IdempotencyKey: "fault-upload",
		})
		calls := faults.calls
		faults.disable()
		if uploadErr == nil && failAt > calls {
			consecutiveSuccesses++
		} else {
			consecutiveSuccesses = 0
		}
		if uploadErr != nil && !errors.Is(uploadErr, domain.ErrUnavailable) {
			server.Close()
			t.Fatalf("failure %d returned unsafe error: %v", failAt, uploadErr)
		}
		clock.Advance(11 * time.Minute)
		checkpointID := fmt.Sprintf("upload-init-fault-%d", failAt)
		if _, err := engine.CreateCheckpoint(context.Background(), checkpointID); err != nil {
			server.Close()
			t.Fatalf("failure %d recovery: %v", failAt, err)
		}
		if err := engine.VerifyCheckpoint(context.Background(), checkpointID); err != nil {
			server.Close()
			t.Fatalf("failure %d verification: %v", failAt, err)
		}
		server.Close()
	}
	if consecutiveSuccesses < 3 {
		t.Fatal("fault matrix did not pass every upload-initiation object boundary")
	}
}

func TestObjectFailureAtEveryUploadCompletionBoundaryReconcilesOnce(t *testing.T) {
	t.Parallel()
	user, _ := domain.ParseUserID("W1tbW1tbW1tbW1tbW1tbWw")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	content := []byte("complete")
	countBackend := objectmemory.New()
	countServer := httptest.NewServer(countBackend)
	countClock := domain.NewFixedClock(time.Date(2041, 5, 6, 7, 8, 9, 0, time.UTC))
	if err := countBackend.ConfigureDataPlane(countServer.URL, countClock, domain.NewIDGenerator(bytes.NewReader(deterministic(150, 1<<20)))); err != nil {
		countServer.Close()
		t.Fatal(err)
	}
	countFaults := &failNthBackend{backend: countBackend}
	countEngine := openFaultEngine(t, countFaults, countClock, 160)
	countPath := domain.MustParseUserPath("/complete.bin")
	countCapability, err := countEngine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: countPath, Size: int64(len(content)), MediaType: "application/octet-stream"})
	if err != nil {
		countServer.Close()
		t.Fatal(err)
	}
	countRequest, _ := http.NewRequest(countCapability.Method, countCapability.URL, bytes.NewReader(content))
	for name, value := range countCapability.Headers {
		countRequest.Header.Set(name, value)
	}
	countResponse, err := countServer.Client().Do(countRequest)
	if err != nil {
		countServer.Close()
		t.Fatal(err)
	}
	_ = countResponse.Body.Close()
	countFaults.arm(0)
	if _, err := countEngine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: countCapability.UploadID, Path: countPath, Size: int64(len(content)), MediaType: "application/octet-stream"}); err != nil {
		countServer.Close()
		t.Fatal(err)
	}
	lastBoundary := countFaults.calls + 3
	countServer.Close()
	consecutiveSuccesses := 0
	for failAt := 1; failAt <= lastBoundary && consecutiveSuccesses < 3; failAt++ {
		backend := objectmemory.New()
		server := httptest.NewServer(backend)
		clock := domain.NewFixedClock(time.Date(2041, 5, 6, 7, 8, 9, 0, time.UTC))
		if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(byte(150+failAt%100), 1<<20)))); err != nil {
			server.Close()
			t.Fatal(err)
		}
		faults := &failNthBackend{backend: backend}
		engine := openFaultEngine(t, faults, clock, byte(160+failAt%90))
		path := domain.MustParseUserPath("/complete.bin")
		capability, err := engine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: path, Size: int64(len(content)), MediaType: "application/octet-stream"})
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		request, _ := http.NewRequest(capability.Method, capability.URL, bytes.NewReader(content))
		for name, value := range capability.Headers {
			request.Header.Set(name, value)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		_ = response.Body.Close()
		completion := domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: int64(len(content)), MediaType: "application/octet-stream"}
		faults.arm(failAt)
		_, completionErr := engine.Files().CompleteUpload(context.Background(), scope, completion)
		calls := faults.calls
		faults.disable()
		if completionErr == nil && failAt > calls {
			consecutiveSuccesses++
		} else {
			consecutiveSuccesses = 0
		}
		if completionErr != nil {
			if !errors.Is(completionErr, domain.ErrUnavailable) {
				server.Close()
				t.Fatalf("failure %d returned unsafe error: %v", failAt, completionErr)
			}
			if _, err := engine.Files().CompleteUpload(context.Background(), scope, completion); err != nil {
				server.Close()
				t.Fatalf("failure %d completion retry: %v", failAt, err)
			}
		}
		entry, err := engine.Files().Stat(context.Background(), scope, path)
		if err != nil || entry.Size != int64(len(content)) {
			server.Close()
			t.Fatalf("failure %d final entry = %+v, %v", failAt, entry, err)
		}
		leaseKey := storageformat.LeaseKey(backend.BackendKind(), string(capability.UploadID))
		if _, err := backend.Get(context.Background(), leaseKey); !errors.Is(err, domain.ErrNotFound) {
			server.Close()
			t.Fatalf("failure %d left runtime upload lease %s: %v", failAt, leaseKey, err)
		}
		server.Close()
	}
	if consecutiveSuccesses < 3 {
		t.Fatal("fault matrix did not pass every upload-completion object boundary")
	}
}

func TestObjectFailureAtEveryUploadAbortBoundaryReconcilesAndRemovesLease(t *testing.T) {
	t.Parallel()
	user, _ := domain.ParseUserID("W1tbW1tbW1tbW1tbW1tbWw")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	countBackend := objectmemory.New()
	countServer := httptest.NewServer(countBackend)
	countClock := domain.NewFixedClock(time.Date(2041, 5, 7, 7, 8, 9, 0, time.UTC))
	if err := countBackend.ConfigureDataPlane(countServer.URL, countClock, domain.NewIDGenerator(bytes.NewReader(deterministic(151, 1<<20)))); err != nil {
		countServer.Close()
		t.Fatal(err)
	}
	countFaults := &failNthBackend{backend: countBackend}
	countEngine := openFaultEngine(t, countFaults, countClock, 161)
	countCapability, err := countEngine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/abort.bin"), Size: 1, MediaType: "application/octet-stream"})
	if err != nil {
		countServer.Close()
		t.Fatal(err)
	}
	countFaults.arm(0)
	if err := countEngine.Files().AbortUpload(context.Background(), scope, countCapability.UploadID); err != nil {
		countServer.Close()
		t.Fatal(err)
	}
	lastBoundary := countFaults.calls + 3
	countServer.Close()

	consecutiveSuccesses := 0
	for failAt := 1; failAt <= lastBoundary && consecutiveSuccesses < 3; failAt++ {
		backend := objectmemory.New()
		server := httptest.NewServer(backend)
		clock := domain.NewFixedClock(time.Date(2041, 5, 7, 7, 8, 9, 0, time.UTC))
		if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(byte(151+failAt%100), 1<<20)))); err != nil {
			server.Close()
			t.Fatal(err)
		}
		faults := &failNthBackend{backend: backend}
		engine := openFaultEngine(t, faults, clock, byte(161+failAt%90))
		capability, err := engine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/abort.bin"), Size: 1, MediaType: "application/octet-stream"})
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		faults.arm(failAt)
		abortErr := engine.Files().AbortUpload(context.Background(), scope, capability.UploadID)
		calls := faults.calls
		faults.disable()
		if abortErr == nil && failAt > calls {
			consecutiveSuccesses++
		} else {
			consecutiveSuccesses = 0
		}
		if abortErr != nil {
			if !errors.Is(abortErr, domain.ErrUnavailable) {
				server.Close()
				t.Fatalf("failure %d returned unsafe error: %v", failAt, abortErr)
			}
			if err := engine.Files().AbortUpload(context.Background(), scope, capability.UploadID); err != nil {
				server.Close()
				t.Fatalf("failure %d abort retry: %v", failAt, err)
			}
		}
		status, err := engine.Files().UploadStatus(context.Background(), scope, capability.UploadID)
		if err != nil || status.State != domain.UploadStateAborted {
			server.Close()
			t.Fatalf("failure %d final status = %+v, %v", failAt, status, err)
		}
		leaseKey := storageformat.LeaseKey(backend.BackendKind(), string(capability.UploadID))
		if _, err := backend.Get(context.Background(), leaseKey); !errors.Is(err, domain.ErrNotFound) {
			server.Close()
			t.Fatalf("failure %d left runtime upload lease %s: %v", failAt, leaseKey, err)
		}
		server.Close()
	}
	if consecutiveSuccesses < 3 {
		t.Fatal("fault matrix did not pass every upload-abort object boundary")
	}
}

func TestObjectFailureAtEveryStateCASAndDeleteBoundaryIsLinearizable(t *testing.T) {
	for _, action := range []string{"cas", "delete"} {
		t.Run(action, func(t *testing.T) {
			fixtureBackend := objectmemory.New()
			fixtureClock := domain.NewFixedClock(time.Date(2041, 6, 7, 8, 9, 10, 0, time.UTC))
			fixtureEngine := openEngine(t, fixtureBackend, fixtureClock, 171, nil)
			key := state.MustKey(state.NamespaceAccounts, "state-fault")
			version, err := fixtureEngine.Create(context.Background(), key, []byte("before"))
			if err != nil {
				t.Fatal(err)
			}
			fixture := fixtureBackend.Export()
			consecutiveSuccesses := 0
			for failAt := 1; failAt <= 70 && consecutiveSuccesses < 3; failAt++ {
				backend := objectmemory.New()
				if err := backend.Import(fixture); err != nil {
					t.Fatal(err)
				}
				clock := domain.NewFixedClock(fixtureClock.Now())
				faults := &failNthBackend{backend: backend}
				engine := openFaultEngine(t, faults, clock, byte(172+failAt%80))
				faults.arm(failAt)
				var mutationErr error
				if action == "cas" {
					_, mutationErr = engine.CompareAndSwap(context.Background(), key, version, []byte("after"))
				} else {
					mutationErr = engine.Delete(context.Background(), key, version)
				}
				calls := faults.calls
				faults.disable()
				if mutationErr == nil && failAt > calls {
					consecutiveSuccesses++
				} else {
					consecutiveSuccesses = 0
				}
				if mutationErr != nil && !errors.Is(mutationErr, domain.ErrUnavailable) {
					t.Fatalf("failure %d returned unsafe error: %v", failAt, mutationErr)
				}
				clock.Advance(2 * time.Minute)
				checkpointID := fmt.Sprintf("state-%s-fault-%d", action, failAt)
				if _, err := engine.CreateCheckpoint(context.Background(), checkpointID); err != nil {
					t.Fatalf("failure %d recovery: %v", failAt, err)
				}
				value, getErr := engine.Get(context.Background(), key)
				if action == "cas" {
					if getErr != nil || (string(value.Data) != "before" && string(value.Data) != "after") {
						t.Fatalf("failure %d CAS result = %+v, %v", failAt, value, getErr)
					}
				} else if getErr != nil && !errors.Is(getErr, domain.ErrNotFound) {
					t.Fatalf("failure %d delete result = %+v, %v", failAt, value, getErr)
				}
			}
			if consecutiveSuccesses < 3 {
				t.Fatalf("fault matrix did not pass every state %s object boundary", action)
			}
		})
	}
}

func TestObjectFailureAtEveryDirectoryCreateBoundaryIsAtomic(t *testing.T) {
	user, _ := domain.ParseUserID("XV1dXV1dXV1dXV1dXV1dXQ")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	consecutiveSuccesses := 0
	for failAt := 1; failAt <= 80 && consecutiveSuccesses < 3; failAt++ {
		backend := objectmemory.New()
		clock := domain.NewFixedClock(time.Date(2041, 7, 8, 9, 10, 11, 0, time.UTC))
		faults := &failNthBackend{backend: backend}
		engine := openFaultEngine(t, faults, clock, byte(180+failAt%70))
		path := domain.MustParseUserPath("/atomic-directory")
		faults.arm(failAt)
		_, createErr := engine.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: path})
		calls := faults.calls
		faults.disable()
		if createErr == nil && failAt > calls {
			consecutiveSuccesses++
		} else {
			consecutiveSuccesses = 0
		}
		if createErr != nil && !errors.Is(createErr, domain.ErrUnavailable) {
			t.Fatalf("failure %d returned unsafe error: %v", failAt, createErr)
		}
		if _, err := engine.Files().Stat(context.Background(), scope, path); err != nil && !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("failure %d exposed invalid directory state: %v", failAt, err)
		}
	}
	if consecutiveSuccesses < 3 {
		t.Fatal("fault matrix did not pass every directory-create object boundary")
	}
}

func TestObjectFailureAtEveryDirectoryReadBoundaryFailsClosed(t *testing.T) {
	fixtureBackend := objectmemory.New()
	fixtureClock := domain.NewFixedClock(time.Date(2041, 8, 9, 10, 11, 12, 0, time.UTC))
	fixtureEngine := openEngine(t, fixtureBackend, fixtureClock, 191, nil)
	user, _ := domain.ParseUserID("Xl5eXl5eXl5eXl5eXl5eXg")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	for _, path := range []string{"/folder", "/folder/child"} {
		if _, err := fixtureEngine.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(path)}); err != nil {
			t.Fatal(err)
		}
	}
	fixture := fixtureBackend.Export()
	for _, action := range []string{"list", "stat"} {
		t.Run(action, func(t *testing.T) {
			consecutiveSuccesses := 0
			for failAt := 1; failAt <= 40 && consecutiveSuccesses < 3; failAt++ {
				backend := objectmemory.New()
				if err := backend.Import(fixture); err != nil {
					t.Fatal(err)
				}
				faults := &failNthBackend{backend: backend}
				engine := openFaultEngine(t, faults, domain.NewFixedClock(fixtureClock.Now()), byte(192+failAt%60))
				faults.arm(failAt)
				var readErr error
				if action == "list" {
					_, readErr = engine.Files().List(context.Background(), scope, domain.ListRequest{Directory: domain.MustParseUserPath("/folder")})
				} else {
					_, readErr = engine.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/folder/child"))
				}
				calls := faults.calls
				if readErr == nil && failAt > calls {
					consecutiveSuccesses++
				} else {
					consecutiveSuccesses = 0
				}
				if readErr != nil && !errors.Is(readErr, domain.ErrUnavailable) {
					t.Fatalf("failure %d returned unsafe error: %v", failAt, readErr)
				}
			}
			if consecutiveSuccesses < 3 {
				t.Fatalf("fault matrix did not pass every directory %s boundary", action)
			}
		})
	}
}

func TestObjectFailureAtEveryDeleteBoundaryRecoversAtomically(t *testing.T) {
	fixtureBackend := objectmemory.New()
	fixtureClock := domain.NewFixedClock(time.Date(2041, 9, 10, 11, 12, 13, 0, time.UTC))
	fixtureEngine := openEngine(t, fixtureBackend, fixtureClock, 201, nil)
	user, _ := domain.ParseUserID("X19fX19fX19fX19fX19fXw")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	path := domain.MustParseUserPath("/victim")
	if _, err := fixtureEngine.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: path}); err != nil {
		t.Fatal(err)
	}
	fixture := fixtureBackend.Export()
	consecutiveSuccesses := 0
	for failAt := 1; failAt <= 100 && consecutiveSuccesses < 3; failAt++ {
		backend := objectmemory.New()
		if err := backend.Import(fixture); err != nil {
			t.Fatal(err)
		}
		clock := domain.NewFixedClock(fixtureClock.Now())
		faults := &failNthBackend{backend: backend}
		engine := openFaultEngine(t, faults, clock, byte(202+failAt%50))
		faults.arm(failAt)
		_, deleteErr := engine.Files().Delete(context.Background(), scope, domain.DeleteRequest{Path: path, IdempotencyKey: "delete-fault"})
		calls := faults.calls
		faults.disable()
		if deleteErr == nil && failAt > calls {
			consecutiveSuccesses++
		} else {
			consecutiveSuccesses = 0
		}
		if deleteErr != nil && !errors.Is(deleteErr, domain.ErrUnavailable) {
			t.Fatalf("failure %d returned unsafe error: %v", failAt, deleteErr)
		}
		clock.Advance(2 * time.Minute)
		if _, err := engine.CreateCheckpoint(context.Background(), fmt.Sprintf("delete-fault-%d", failAt)); err != nil {
			t.Fatalf("failure %d recovery: %v", failAt, err)
		}
		if _, err := engine.Files().Stat(context.Background(), scope, path); err != nil && !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("failure %d exposed invalid delete state: %v", failAt, err)
		}
	}
	if consecutiveSuccesses < 3 {
		t.Fatal("fault matrix did not pass every delete object boundary")
	}
}

func TestObjectFailureAtEveryMoveBoundaryRecoversWithoutSplitVisibility(t *testing.T) {
	t.Parallel()
	fixtureBackend := objectmemory.New()
	fixtureServer := httptest.NewServer(fixtureBackend)
	t.Cleanup(fixtureServer.Close)
	fixtureClock := domain.NewFixedClock(time.Date(2041, 10, 11, 12, 13, 14, 0, time.UTC))
	if err := fixtureBackend.ConfigureDataPlane(fixtureServer.URL, fixtureClock, domain.NewIDGenerator(bytes.NewReader(deterministic(211, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	fixtureEngine := openEngine(t, fixtureBackend, fixtureClock, 212, nil)
	user, _ := domain.ParseUserID("YGBgYGBgYGBgYGBgYGBgYA")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	if _, err := fixtureEngine.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/source")}); err != nil {
		t.Fatal(err)
	}
	uploadPortableFile(t, fixtureServer.Client(), fixtureEngine.Files(), scope, domain.MustParseUserPath("/source/value.txt"), []byte("move fault matrix"))
	fixture := fixtureBackend.Export()
	countBackend := objectmemory.New()
	if err := countBackend.Import(fixture); err != nil {
		t.Fatal(err)
	}
	countFaults := &failNthBackend{backend: countBackend}
	countEngine := openFaultEngine(t, countFaults, domain.NewFixedClock(fixtureClock.Now()), 213)
	countFaults.arm(0)
	if _, err := countEngine.Files().Move(context.Background(), scope, scope, domain.MoveRequest{
		Source: domain.MustParseUserPath("/source"), Destination: domain.MustParseUserPath("/destination"),
		IdempotencyKey: "fault-move",
	}); err != nil {
		t.Fatal(err)
	}
	lastBoundary := countFaults.calls + 3

	consecutiveSuccesses := 0
	for failAt := 1; failAt <= lastBoundary && consecutiveSuccesses < 3; failAt++ {
		backend := objectmemory.New()
		if err := backend.Import(fixture); err != nil {
			t.Fatal(err)
		}
		clock := domain.NewFixedClock(fixtureClock.Now())
		faults := &failNthBackend{backend: backend}
		engine := openFaultEngine(t, faults, clock, byte(213+failAt%40))
		faults.arm(failAt)
		_, moveErr := engine.Files().Move(context.Background(), scope, scope, domain.MoveRequest{
			Source: domain.MustParseUserPath("/source"), Destination: domain.MustParseUserPath("/destination"),
			IdempotencyKey: "fault-move",
		})
		calls := faults.calls
		faults.disable()
		if moveErr == nil && failAt > calls {
			consecutiveSuccesses++
		} else {
			consecutiveSuccesses = 0
		}
		if moveErr != nil && !errors.Is(moveErr, domain.ErrUnavailable) {
			t.Fatalf("failure %d returned unsafe error: %v", failAt, moveErr)
		}
		clock.Advance(2 * time.Minute)
		if _, err := engine.CreateCheckpoint(context.Background(), fmt.Sprintf("move-fault-%d", failAt)); err != nil {
			t.Fatalf("failure %d recovery: %v", failAt, err)
		}
		sourceEntry, sourceErr := engine.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/source/value.txt"))
		destinationEntry, destinationErr := engine.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/destination/value.txt"))
		if sourceErr == nil && destinationErr == nil {
			t.Fatalf("failure %d duplicated moved file: source=%+v destination=%+v", failAt, sourceEntry, destinationEntry)
		}
		if sourceErr != nil && !errors.Is(sourceErr, domain.ErrNotFound) {
			t.Fatalf("failure %d source status: %v", failAt, sourceErr)
		}
		if destinationErr != nil && !errors.Is(destinationErr, domain.ErrNotFound) {
			t.Fatalf("failure %d destination status: %v", failAt, destinationErr)
		}
		if sourceErr != nil && destinationErr != nil {
			t.Fatalf("failure %d lost moved file", failAt)
		}
	}
	if consecutiveSuccesses < 3 {
		t.Fatal("fault matrix did not pass every recursive-move object boundary")
	}
}

func TestObjectFailureAtEveryStateReadListAndCreateBoundaryFailsClosed(t *testing.T) {
	fixtureBackend := objectmemory.New()
	fixtureClock := domain.NewFixedClock(time.Date(2041, 11, 12, 13, 14, 15, 0, time.UTC))
	fixtureEngine := openEngine(t, fixtureBackend, fixtureClock, 221, nil)
	existing := state.MustKey(state.NamespaceAccounts, "read-fault")
	if _, err := fixtureEngine.Create(context.Background(), existing, []byte("existing")); err != nil {
		t.Fatal(err)
	}
	fixture := fixtureBackend.Export()
	for _, action := range []string{"get", "list", "create"} {
		t.Run(action, func(t *testing.T) {
			consecutiveSuccesses := 0
			for failAt := 1; failAt <= 80 && consecutiveSuccesses < 3; failAt++ {
				backend := objectmemory.New()
				if err := backend.Import(fixture); err != nil {
					t.Fatal(err)
				}
				faults := &failNthBackend{backend: backend}
				engine := openFaultEngine(t, faults, domain.NewFixedClock(fixtureClock.Now()), byte(222+failAt%30))
				faults.arm(failAt)
				var actionErr error
				switch action {
				case "get":
					_, actionErr = engine.Get(context.Background(), existing)
				case "list":
					_, actionErr = engine.List(context.Background(), state.MustPrefix(state.NamespaceAccounts), state.PageRequest{Limit: 1})
				case "create":
					_, actionErr = engine.Create(context.Background(), state.MustKey(state.NamespaceAccounts, fmt.Sprintf("create-fault-%d", failAt)), []byte("new"))
				}
				calls := faults.calls
				faults.disable()
				if actionErr == nil && failAt > calls {
					consecutiveSuccesses++
				} else {
					consecutiveSuccesses = 0
				}
				if actionErr != nil && !errors.Is(actionErr, domain.ErrUnavailable) {
					t.Fatalf("failure %d returned unsafe error: %v", failAt, actionErr)
				}
				if value, err := engine.Get(context.Background(), existing); err != nil || string(value.Data) != "existing" {
					t.Fatalf("failure %d damaged existing state: %+v, %v", failAt, value, err)
				}
			}
			if consecutiveSuccesses < 3 {
				t.Fatalf("fault matrix did not pass every state %s boundary", action)
			}
		})
	}
}

func TestObjectFailureAtEveryEmptyGateTransitionBoundaryIsRetrySafe(t *testing.T) {
	for _, action := range []string{"close", "open"} {
		t.Run(action, func(t *testing.T) {
			consecutiveSuccesses := 0
			for failAt := 1; failAt <= 100 && consecutiveSuccesses < 3; failAt++ {
				backend := objectmemory.New()
				clock := domain.NewFixedClock(time.Date(2041, 12, 13, 14, 15, 16, 0, time.UTC))
				faults := &failNthBackend{backend: backend}
				engine := openFaultEngine(t, faults, clock, byte(231+failAt%20))
				checkpointID := fmt.Sprintf("gate-fault-%d", failAt)
				if action == "open" {
					if _, err := engine.CreateCheckpoint(context.Background(), checkpointID); err != nil {
						t.Fatal(err)
					}
				}
				faults.arm(failAt)
				var transitionErr error
				if action == "close" {
					transitionErr = engine.CloseWrites(context.Background(), checkpointID)
				} else {
					transitionErr = engine.OpenWrites(context.Background(), checkpointID)
				}
				calls := faults.calls
				faults.disable()
				if transitionErr == nil && failAt > calls {
					consecutiveSuccesses++
				} else {
					consecutiveSuccesses = 0
				}
				if transitionErr != nil && !errors.Is(transitionErr, domain.ErrUnavailable) {
					t.Fatalf("failure %d returned unsafe error: %v", failAt, transitionErr)
				}
				if transitionErr != nil {
					if action == "close" {
						if err := engine.CloseWrites(context.Background(), checkpointID); err != nil {
							t.Fatalf("failure %d close retry: %v", failAt, err)
						}
					} else if err := engine.OpenWrites(context.Background(), checkpointID); err != nil {
						t.Fatalf("failure %d open retry: %v", failAt, err)
					}
				}
				gate, err := engine.GateStatus(context.Background())
				if err != nil || (action == "close" && gate.Mode != "closed") || (action == "open" && gate.Mode != "open") {
					t.Fatalf("failure %d final gate = %+v, %v", failAt, gate, err)
				}
			}
			if consecutiveSuccesses < 3 {
				t.Fatalf("fault matrix did not pass every gate %s boundary", action)
			}
		})
	}
}

func TestPortablePublicMethodsRejectInvalidAndCrossOwnerRequests(t *testing.T) {
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2041, 3, 4, 5, 6, 7, 0, time.UTC))
	engine := openEngine(t, backend, clock, 121, nil)
	firstUser, _ := domain.ParseUserID("WFhYWFhYWFhYWFhYWFhYWA")
	secondUser, _ := domain.ParseUserID("WVlZWVlZWVlZWVlZWVlZWQ")
	first, _ := domain.NewScope(firstUser, domain.AreaLive)
	second, _ := domain.NewScope(secondUser, domain.AreaLive)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := engine.Files().CreateDirectory(canceled, first, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/x")}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled CreateDirectory() error = %v", err)
	}
	if _, err := engine.Files().Copy(context.Background(), first, second, domain.CopyRequest{Source: domain.MustParseUserPath("/a"), Destination: domain.MustParseUserPath("/b")}); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("cross-owner Copy() error = %v", err)
	}
	if _, err := engine.Files().Move(context.Background(), first, first, domain.MoveRequest{Source: domain.MustParseUserPath("/"), Destination: domain.MustParseUserPath("/b")}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("root Move() error = %v", err)
	}
	if _, err := engine.Files().Delete(context.Background(), first, domain.DeleteRequest{Path: domain.MustParseUserPath("/")}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("root Delete() error = %v", err)
	}
	if _, err := engine.Files().CreateUpload(context.Background(), first, domain.CreateUploadRequest{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid CreateUpload() error = %v", err)
	}
	if _, err := engine.Files().GetOperation(context.Background(), firstUser, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid GetOperation() error = %v", err)
	}
	if _, err := engine.Create(context.Background(), state.Key{}, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid state Create() error = %v", err)
	}
	if err := engine.Delete(context.Background(), state.MustKey(state.NamespaceAccounts, "missing"), ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid state Delete() error = %v", err)
	}
	if _, err := engine.List(context.Background(), state.Prefix{}, state.PageRequest{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid state List() error = %v", err)
	}
	if _, err := engine.Files().CreateDownload(context.Background(), first, domain.CreateDownloadRequest{Path: domain.MustParseUserPath("/missing")}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing CreateDownload() error = %v", err)
	}
	if _, err := engine.Files().List(context.Background(), first, domain.ListRequest{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid file List() error = %v", err)
	}
	if _, err := engine.Files().List(context.Background(), first, domain.ListRequest{Directory: domain.MustParseUserPath("/"), PageSize: 10001}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized file List() error = %v", err)
	}
	if _, err := engine.Files().List(context.Background(), first, domain.ListRequest{Directory: domain.MustParseUserPath("/"), Sort: "unknown"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid sort error = %v", err)
	}
	if _, err := engine.Files().List(context.Background(), first, domain.ListRequest{Directory: domain.MustParseUserPath("/"), Cursor: "%"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid cursor error = %v", err)
	}
	if _, err := engine.Files().Stat(context.Background(), first, domain.UserPath{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid Stat() error = %v", err)
	}
	if root, err := engine.Files().Stat(context.Background(), first, domain.MustParseUserPath("/")); err != nil || root.Kind != domain.EntryDirectory {
		t.Fatalf("root Stat() = %+v, %v", root, err)
	}
	if _, err := engine.Files().CreateDirectory(context.Background(), first, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/")}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("root CreateDirectory() error = %v", err)
	}
	if _, err := engine.Files().CreateDirectory(context.Background(), first, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/source"), Conflict: "unknown"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid directory conflict error = %v", err)
	}
	source, err := engine.Files().CreateDirectory(context.Background(), first, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/source")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Files().CreateDirectory(context.Background(), first, domain.CreateDirectoryRequest{Path: source.Path}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate directory error = %v", err)
	}
	if _, err := engine.Files().CreateDirectory(context.Background(), first, domain.CreateDirectoryRequest{Path: source.Path, Conflict: domain.ConflictReplace, ExpectedVersion: "stale"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale directory replacement error = %v", err)
	}
	if _, err := engine.Files().CreateDirectory(context.Background(), first, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/source/child")}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Files().Copy(context.Background(), first, first, domain.CopyRequest{Source: source.Path, Destination: source.Path}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("identical Copy() error = %v", err)
	}
	if _, err := engine.Files().Copy(context.Background(), first, first, domain.CopyRequest{Source: source.Path, Destination: domain.MustParseUserPath("/source/child/copy")}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("descendant Copy() error = %v", err)
	}
	if _, err := engine.Files().Copy(context.Background(), first, first, domain.CopyRequest{Source: domain.MustParseUserPath("/missing"), Destination: domain.MustParseUserPath("/copy")}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing Copy() error = %v", err)
	}
	if _, err := engine.Files().Copy(context.Background(), first, first, domain.CopyRequest{Source: source.Path, Destination: domain.MustParseUserPath("/copy"), ExpectedSource: "stale"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale-source Copy() error = %v", err)
	}
	if _, err := engine.Files().Delete(context.Background(), first, domain.DeleteRequest{Path: domain.MustParseUserPath("/missing")}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing Delete() error = %v", err)
	}
	if _, err := engine.Files().Delete(context.Background(), first, domain.DeleteRequest{Path: source.Path, ExpectedVersion: "stale"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale Delete() error = %v", err)
	}
	if _, err := engine.Files().GetOperation(canceled, firstUser, "operation"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled GetOperation() error = %v", err)
	}
	if _, err := engine.Files().GetOperation(context.Background(), firstUser, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing GetOperation() error = %v", err)
	}
	if _, err := engine.Files().UploadStatus(context.Background(), first, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty UploadStatus() error = %v", err)
	}
	if _, err := engine.Files().UploadStatus(context.Background(), first, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing UploadStatus() error = %v", err)
	}
	if _, err := engine.Files().CompleteUpload(context.Background(), first, domain.CompleteUploadRequest{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid CompleteUpload() error = %v", err)
	}
	if _, err := engine.Files().CompleteUpload(context.Background(), first, domain.CompleteUploadRequest{UploadID: "missing", Path: domain.MustParseUserPath("/missing"), Size: 1, MediaType: "text/plain"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing CompleteUpload() error = %v", err)
	}
	if err := engine.Files().AbortUpload(context.Background(), first, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty AbortUpload() error = %v", err)
	}
	if err := engine.Files().AbortUpload(context.Background(), first, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing AbortUpload() error = %v", err)
	}
	if _, err := engine.Files().CreateDownload(context.Background(), first, domain.CreateDownloadRequest{Path: domain.MustParseUserPath("/")}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("root CreateDownload() error = %v", err)
	}
	if _, err := engine.Files().CreateDownload(context.Background(), first, domain.CreateDownloadRequest{Path: source.Path, Disposition: "open"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid disposition error = %v", err)
	}
	if _, err := engine.Files().CreateDownload(context.Background(), first, domain.CreateDownloadRequest{Path: source.Path}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("directory download error = %v", err)
	}
}

func TestObjectFailureAtEveryNamespaceBatchBoundaryIsAtomicAndReplayable(t *testing.T) {
	ctx := context.Background()
	fixtureBackend := objectmemory.New()
	fixtureClock := domain.NewFixedClock(time.Date(2042, 2, 3, 4, 5, 6, 0, time.UTC))
	fixtureEngine := openEngine(t, fixtureBackend, fixtureClock, 231, nil)
	owner, _ := domain.ParseUserID("cXFxcXFxcXFxcXFxcXFxcQ")
	scope, _ := domain.NewScope(owner, domain.AreaLive)
	requests := make([]domain.CopyRequest, 3)
	for index := range requests {
		sourcePath := domain.MustParseUserPath(fmt.Sprintf("/batch-source-%d", index))
		entry, err := fixtureEngine.Files().CreateDirectory(ctx, scope, domain.CreateDirectoryRequest{Path: sourcePath})
		if err != nil {
			t.Fatal(err)
		}
		requests[index] = domain.CopyRequest{
			Source: sourcePath, Destination: domain.MustParseUserPath(fmt.Sprintf("/batch-target-%d", index)),
			ExpectedSource: entry.Version,
		}
	}
	fixture := fixtureBackend.Export()
	countBackend := objectmemory.New()
	if err := countBackend.Import(fixture); err != nil {
		t.Fatal(err)
	}
	countFaults := &failNthBackend{backend: countBackend}
	countEngine := openFaultEngine(t, countFaults, domain.NewFixedClock(fixtureClock.Now()), 232)
	countFaults.arm(0)
	if _, err := countEngine.Files().BatchCopyMove(ctx, owner, requests, false, "batch-fault-matrix"); err != nil {
		t.Fatal(err)
	}
	lastBoundary := countFaults.calls + 3

	consecutiveSuccesses := 0
	for failAt := 1; failAt <= lastBoundary && consecutiveSuccesses < 3; failAt++ {
		backend := objectmemory.New()
		if err := backend.Import(fixture); err != nil {
			t.Fatal(err)
		}
		faults := &failNthBackend{backend: backend}
		engine := openFaultEngine(t, faults, domain.NewFixedClock(fixtureClock.Now()), byte(233+failAt%20))
		faults.arm(failAt)
		result, mutationErr := engine.Files().BatchCopyMove(ctx, owner, requests, false, "batch-fault-matrix")
		calls := faults.calls
		faults.disable()
		if mutationErr == nil && failAt > calls {
			consecutiveSuccesses++
		} else {
			consecutiveSuccesses = 0
		}
		if mutationErr != nil {
			if !errors.Is(mutationErr, domain.ErrUnavailable) {
				t.Fatalf("failure %d returned unsafe error: %v", failAt, mutationErr)
			}
			result, mutationErr = engine.Files().BatchCopyMove(ctx, owner, requests, false, "batch-fault-matrix")
		}
		if mutationErr != nil || len(result.Items) != len(requests) || result.Operation.State != domain.OperationSucceeded {
			t.Fatalf("failure %d retry result = %+v, %v", failAt, result, mutationErr)
		}
		for _, request := range requests {
			if _, err := engine.Files().Stat(ctx, scope, request.Source); err != nil {
				t.Fatalf("failure %d lost source %s: %v", failAt, request.Source, err)
			}
			if _, err := engine.Files().Stat(ctx, scope, request.Destination); err != nil {
				t.Fatalf("failure %d omitted destination %s: %v", failAt, request.Destination, err)
			}
		}
	}
	if consecutiveSuccesses < 3 {
		t.Fatal("fault matrix did not pass every namespace-batch object boundary")
	}
}

func TestObjectFailureAtEveryTrashMutationBoundaryIsAtomicAndReplayable(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2042, 3, 4, 5, 6, 7, 0, time.UTC))
	owner, _ := domain.ParseUserID("dHJhc2gtZmF1bHQtYXVkaXQ")
	live, _ := domain.NewScope(owner, domain.AreaLive)

	run := func(t *testing.T, fixture map[string][]byte, mutate func(*portable.Engine) error, assertBefore, assertAfter func(*portable.Engine)) {
		t.Helper()
		countBackend := objectmemory.New()
		if err := countBackend.Import(fixture); err != nil {
			t.Fatal(err)
		}
		counter := &failNthBackend{backend: countBackend}
		countEngine := openFaultEngine(t, counter, domain.NewFixedClock(clock.Now()), 241)
		counter.arm(0)
		if err := mutate(countEngine); err != nil {
			t.Fatal(err)
		}
		lastBoundary := counter.calls + 3

		consecutiveSuccesses := 0
		for failAt := 1; failAt <= lastBoundary && consecutiveSuccesses < 3; failAt++ {
			backend := objectmemory.New()
			if err := backend.Import(fixture); err != nil {
				t.Fatal(err)
			}
			faults := &failNthBackend{backend: backend}
			engine := openFaultEngine(t, faults, domain.NewFixedClock(clock.Now()), byte(242+failAt%10))
			faults.arm(failAt)
			mutationErr := mutate(engine)
			calls := faults.calls
			faults.disable()
			if mutationErr == nil && failAt > calls {
				consecutiveSuccesses++
			} else {
				consecutiveSuccesses = 0
			}
			if mutationErr != nil {
				if !errors.Is(mutationErr, domain.ErrUnavailable) {
					t.Fatalf("failure %d returned unsafe error: %v", failAt, mutationErr)
				}
				assertBefore(engine)
				mutationErr = mutate(engine)
			}
			if mutationErr != nil {
				t.Fatalf("failure %d retry: %v", failAt, mutationErr)
			}
			assertAfter(engine)
		}
		if consecutiveSuccesses < 3 {
			t.Fatal("fault matrix did not pass every trash mutation object boundary")
		}
	}

	t.Run("batch-move-to-trash", func(t *testing.T) {
		fixtureBackend := objectmemory.New()
		fixtureEngine := openFaultEngine(t, fixtureBackend, clock, 251)
		requests := make([]domain.TrashRequest, 2)
		for index := range requests {
			path := domain.MustParseUserPath(fmt.Sprintf("/trash-source-%d", index))
			entry, err := fixtureEngine.Files().CreateDirectory(ctx, live, domain.CreateDirectoryRequest{Path: path})
			if err != nil {
				t.Fatal(err)
			}
			requests[index] = domain.TrashRequest{Path: path, ExpectedVersion: entry.Version, TrashID: fmt.Sprintf("fault-trash-%d", index)}
		}
		assertBefore := func(engine *portable.Engine) {
			for _, request := range requests {
				if _, err := engine.Files().Stat(ctx, live, request.Path); err != nil {
					t.Fatalf("failed trash removed %s: %v", request.Path, err)
				}
			}
			page, err := engine.Files().ListTrash(ctx, owner, domain.TrashListRequest{})
			if err != nil || len(page.Items) != 0 {
				t.Fatalf("failed trash published items: %+v, %v", page, err)
			}
		}
		assertAfter := func(engine *portable.Engine) {
			for _, request := range requests {
				if _, err := engine.Files().Stat(ctx, live, request.Path); !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("completed trash retained %s: %v", request.Path, err)
				}
			}
			page, err := engine.Files().ListTrash(ctx, owner, domain.TrashListRequest{})
			if err != nil || len(page.Items) != len(requests) {
				t.Fatalf("completed trash page: %+v, %v", page, err)
			}
		}
		run(t, fixtureBackend.Export(), func(engine *portable.Engine) error {
			result, err := engine.Files().BatchMoveToTrash(ctx, owner, requests, "trash-fault-matrix")
			if err == nil && (len(result.Items) != len(requests) || result.Operation.State != domain.OperationSucceeded) {
				return fmt.Errorf("invalid batch trash result: %+v", result)
			}
			return err
		}, assertBefore, assertAfter)
	})

	t.Run("restore-from-trash", func(t *testing.T) {
		fixtureBackend := objectmemory.New()
		fixtureEngine := openFaultEngine(t, fixtureBackend, clock, 252)
		path := domain.MustParseUserPath("/restore-source")
		entry, err := fixtureEngine.Files().CreateDirectory(ctx, live, domain.CreateDirectoryRequest{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixtureEngine.Files().MoveToTrash(ctx, owner, domain.TrashRequest{Path: path, ExpectedVersion: entry.Version, TrashID: "restore-fault", IdempotencyKey: "seed-restore-fault"}); err != nil {
			t.Fatal(err)
		}
		assertBefore := func(engine *portable.Engine) {
			if _, err := engine.Files().Stat(ctx, live, path); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("failed restore published live source: %v", err)
			}
			page, err := engine.Files().ListTrash(ctx, owner, domain.TrashListRequest{})
			if err != nil || len(page.Items) != 1 {
				t.Fatalf("failed restore removed trash source: %+v, %v", page, err)
			}
		}
		assertAfter := func(engine *portable.Engine) {
			if _, err := engine.Files().Stat(ctx, live, path); err != nil {
				t.Fatalf("completed restore omitted live source: %v", err)
			}
			page, err := engine.Files().ListTrash(ctx, owner, domain.TrashListRequest{})
			if err != nil || len(page.Items) != 0 {
				t.Fatalf("completed restore retained trash source: %+v, %v", page, err)
			}
		}
		run(t, fixtureBackend.Export(), func(engine *portable.Engine) error {
			operation, err := engine.Files().RestoreFromTrash(ctx, owner, "restore-fault", domain.ConflictFail, "restore-fault-matrix")
			if err == nil && operation.State != domain.OperationSucceeded {
				return fmt.Errorf("invalid restore result: %+v", operation)
			}
			return err
		}, assertBefore, assertAfter)
	})

	t.Run("batch-delete-from-trash", func(t *testing.T) {
		fixtureBackend := objectmemory.New()
		fixtureEngine := openFaultEngine(t, fixtureBackend, clock, 253)
		trashIDs := []string{"delete-fault-0", "delete-fault-1"}
		for index, trashID := range trashIDs {
			path := domain.MustParseUserPath(fmt.Sprintf("/delete-source-%d", index))
			entry, err := fixtureEngine.Files().CreateDirectory(ctx, live, domain.CreateDirectoryRequest{Path: path})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fixtureEngine.Files().MoveToTrash(ctx, owner, domain.TrashRequest{Path: path, ExpectedVersion: entry.Version, TrashID: trashID, IdempotencyKey: "seed-" + trashID}); err != nil {
				t.Fatal(err)
			}
		}
		assertBefore := func(engine *portable.Engine) {
			page, err := engine.Files().ListTrash(ctx, owner, domain.TrashListRequest{})
			if err != nil || len(page.Items) != len(trashIDs) {
				t.Fatalf("failed delete removed trash authority: %+v, %v", page, err)
			}
		}
		assertAfter := func(engine *portable.Engine) {
			page, err := engine.Files().ListTrash(ctx, owner, domain.TrashListRequest{})
			if err != nil || len(page.Items) != 0 {
				t.Fatalf("completed delete retained trash authority: %+v, %v", page, err)
			}
		}
		run(t, fixtureBackend.Export(), func(engine *portable.Engine) error {
			result, err := engine.Files().BatchDeleteFromTrash(ctx, owner, trashIDs, "delete-trash-fault-matrix")
			if err == nil && (len(result.Items) != len(trashIDs) || result.Operation.State != domain.OperationSucceeded) {
				return fmt.Errorf("invalid batch delete result: %+v", result)
			}
			return err
		}, assertBefore, assertAfter)
	})
}
