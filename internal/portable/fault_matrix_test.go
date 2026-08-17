package portable_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/state"
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

func (backend *failNthBackend) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	if err := backend.fault(); err != nil {
		return objectstore.Object{}, err
	}
	return backend.backend.Get(ctx, key)
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

	consecutiveSuccesses := 0
	for failAt := 1; failAt <= 160 && consecutiveSuccesses < 3; failAt++ {
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
	user, _ := domain.ParseUserID("W1tbW1tbW1tbW1tbW1tbWw")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	content := []byte("complete")
	consecutiveSuccesses := 0
	for failAt := 1; failAt <= 120 && consecutiveSuccesses < 3; failAt++ {
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
		server.Close()
	}
	if consecutiveSuccesses < 3 {
		t.Fatal("fault matrix did not pass every upload-completion object boundary")
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
	if _, err := engine.Files().List(context.Background(), first, domain.ListRequest{Directory: domain.MustParseUserPath("/"), PageSize: 1001}); !errors.Is(err, domain.ErrInvalid) {
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
