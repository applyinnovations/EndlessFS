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
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
)

func TestPortableRecursiveOperationsAreDurableIdempotentAndIsolated(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2040, 1, 2, 3, 4, 5, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(50, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	engine := openEngine(t, backend, clock, 51, nil)
	user, _ := domain.ParseUserID("UVFRUVFRUVFRUVFRUVFRUQ")
	live, _ := domain.NewScope(user, domain.AreaLive)
	trash, _ := domain.NewScope(user, domain.AreaTrash)
	if _, err := engine.Files().CreateDirectory(context.Background(), live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/tree")}); err != nil {
		t.Fatal(err)
	}
	uploadPortableFile(t, server.Client(), engine.Files(), live, domain.MustParseUserPath("/tree/file.txt"), []byte("tree"))
	request := domain.CopyRequest{Source: domain.MustParseUserPath("/tree"), Destination: domain.MustParseUserPath("/copy"), IdempotencyKey: "copy-1"}
	operation, err := engine.Files().Copy(context.Background(), live, live, request)
	if err != nil || operation.State != domain.OperationSucceeded {
		t.Fatalf("Copy() = %+v, %v", operation, err)
	}
	replayed, err := engine.Files().Copy(context.Background(), live, live, request)
	if err != nil || replayed.ID != operation.ID {
		t.Fatalf("replayed Copy() = %+v, %v", replayed, err)
	}
	if _, err := engine.Files().Stat(context.Background(), live, domain.MustParseUserPath("/copy/file.txt")); err != nil {
		t.Fatalf("copied descendant missing: %v", err)
	}
	moved, err := engine.Files().Move(context.Background(), live, trash, domain.MoveRequest{Source: domain.MustParseUserPath("/copy"), Destination: domain.MustParseUserPath("/trashed"), IdempotencyKey: "move-1"})
	if err != nil || moved.State != domain.OperationSucceeded {
		t.Fatalf("Move() = %+v, %v", moved, err)
	}
	if _, err := engine.Files().Stat(context.Background(), live, domain.MustParseUserPath("/copy")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("moved source remains: %v", err)
	}
	if _, err := engine.Files().Stat(context.Background(), trash, domain.MustParseUserPath("/trashed/file.txt")); err != nil {
		t.Fatalf("moved descendant missing: %v", err)
	}
	deleted, err := engine.Files().Delete(context.Background(), trash, domain.DeleteRequest{Path: domain.MustParseUserPath("/trashed"), IdempotencyKey: "delete-1"})
	if err != nil || deleted.State != domain.OperationSucceeded {
		t.Fatalf("Delete() = %+v, %v", deleted, err)
	}
}

func TestReplicaDropAfterRootPrepareRecoversAtOneCommitPoint(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2040, 2, 3, 4, 5, 6, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(52, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	crasher := &stepFailure{step: portable.StepOperationAfterPrepared}
	first := openEngine(t, backend, clock, 53, crasher)
	second := openEngine(t, backend, clock, 54, nil)
	user, _ := domain.ParseUserID("UlJSUlJSUlJSUlJSUlJSUg")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	for _, path := range []string{"/source", "/destination"} {
		if _, err := first.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(path)}); err != nil {
			t.Fatal(err)
		}
	}
	uploadPortableFile(t, server.Client(), first.Files(), scope, domain.MustParseUserPath("/source/value.txt"), []byte("value"))
	_, err := first.Files().Move(context.Background(), scope, scope, domain.MoveRequest{Source: domain.MustParseUserPath("/source/value.txt"), Destination: domain.MustParseUserPath("/destination/value.txt")})
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("crashed Move() error = %v", err)
	}
	if _, err := second.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/source/value.txt")); err != nil {
		t.Fatalf("pre-commit source was not visible: %v", err)
	}
	if _, err := second.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/destination/value.txt")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("pre-commit destination became visible: %v", err)
	}
	clock.Advance(2 * time.Minute)
	if _, err := second.CreateCheckpoint(context.Background(), "prepared-recovery"); err != nil {
		t.Fatalf("CreateCheckpoint() recovery error = %v", err)
	}
	if _, err := second.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/source/value.txt")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("recovered source remains: %v", err)
	}
	if _, err := second.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/destination/value.txt")); err != nil {
		t.Fatalf("recovered destination missing: %v", err)
	}
}

func TestSupersededReplicaCannotCommitWithTakeoverFence(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2040, 2, 4, 4, 5, 6, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(55, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	firstPause := newStepPause(portable.StepOperationAfterPrepared)
	secondPause := newStepPause(portable.StepOperationAfterPrepared)
	first := openEngine(t, backend, clock, 56, firstPause)
	second := openEngine(t, backend, clock, 57, secondPause)
	user, _ := domain.ParseUserID("U1NTU1NTU1NTU1NTU1NTUw")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	for _, path := range []string{"/source", "/destination"} {
		if _, err := first.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(path)}); err != nil {
			t.Fatal(err)
		}
	}
	uploadPortableFile(t, server.Client(), first.Files(), scope, domain.MustParseUserPath("/source/value.txt"), []byte("value"))

	firstResult := make(chan error, 1)
	go func() {
		_, err := first.Files().Move(context.Background(), scope, scope, domain.MoveRequest{
			Source: domain.MustParseUserPath("/source/value.txt"), Destination: domain.MustParseUserPath("/destination/value.txt"),
		})
		firstResult <- err
	}()
	<-firstPause.reached
	clock.Advance(2 * time.Minute)
	checkpointResult := make(chan error, 1)
	go func() {
		_, err := second.CreateCheckpoint(context.Background(), "stale-owner")
		checkpointResult <- err
	}()
	<-secondPause.reached
	close(firstPause.release)
	if err := <-firstResult; !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("superseded Move() error = %v", err)
	}
	if _, err := second.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/source/value.txt")); err != nil {
		t.Fatalf("superseded worker changed pre-commit view: %v", err)
	}
	if _, err := second.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/destination/value.txt")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("superseded worker published destination: %v", err)
	}
	close(secondPause.release)
	if err := <-checkpointResult; err != nil {
		t.Fatalf("takeover checkpoint error = %v", err)
	}
	if _, err := second.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/source/value.txt")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("recovered source remains: %v", err)
	}
	if _, err := second.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/destination/value.txt")); err != nil {
		t.Fatalf("recovered destination missing: %v", err)
	}
}

func TestReplicaDropAfterCommitOrFinalizationRecoversPostCommitView(t *testing.T) {
	for index, step := range []string{portable.StepOperationAfterCommitted, portable.StepOperationAfterFinalized} {
		t.Run(step, func(t *testing.T) {
			backend := objectmemory.New()
			server := httptest.NewServer(backend)
			t.Cleanup(server.Close)
			clock := domain.NewFixedClock(time.Date(2040, 2, 5+index, 4, 5, 6, 0, time.UTC))
			if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(byte(58+index), 1<<20)))); err != nil {
				t.Fatal(err)
			}
			crasher := &stepFailure{step: step}
			first := openEngine(t, backend, clock, byte(60+index*2), crasher)
			second := openEngine(t, backend, clock, byte(61+index*2), nil)
			user, _ := domain.ParseUserID("U1NTU1NTU1NTU1NTU1NTUw")
			scope, _ := domain.NewScope(user, domain.AreaLive)
			for _, path := range []string{"/source", "/destination"} {
				if _, err := first.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(path)}); err != nil {
					t.Fatal(err)
				}
			}
			uploadPortableFile(t, server.Client(), first.Files(), scope, domain.MustParseUserPath("/source/value.txt"), []byte("value"))
			if _, err := first.Files().Move(context.Background(), scope, scope, domain.MoveRequest{Source: domain.MustParseUserPath("/source/value.txt"), Destination: domain.MustParseUserPath("/destination/value.txt")}); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("crashed Move() error = %v", err)
			}
			if _, err := second.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/source/value.txt")); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("committed source remains: %v", err)
			}
			if _, err := second.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/destination/value.txt")); err != nil {
				t.Fatalf("committed destination missing: %v", err)
			}
			clock.Advance(2 * time.Minute)
			if _, err := second.CreateCheckpoint(context.Background(), fmt.Sprintf("post-commit-%d", index)); err != nil {
				t.Fatalf("checkpoint recovery error = %v", err)
			}
		})
	}
}

func TestConcurrentRootChangeFailsOperationWithoutRollingBackWinner(t *testing.T) {
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2040, 3, 4, 5, 6, 7, 0, time.UTC))
	winner := openEngine(t, backend, clock, 70, nil)
	user, _ := domain.ParseUserID("VFRUVFRUVFRUVFRUVFRUVA")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	if _, err := winner.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/source")}); err != nil {
		t.Fatal(err)
	}
	fired := false
	competitor := portable.SchedulerFunc(func(ctx context.Context, step string) error {
		if step != portable.StepAdmissionAfterCandidate || fired {
			return nil
		}
		fired = true
		_, err := winner.Files().CreateDirectory(ctx, scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/winner")})
		return err
	})
	stale := openEngine(t, backend, clock, 71, competitor)
	operation, err := stale.Files().Copy(context.Background(), scope, scope, domain.CopyRequest{
		Source: domain.MustParseUserPath("/source"), Destination: domain.MustParseUserPath("/copy"), IdempotencyKey: "concurrent-root-copy-1",
	})
	if err != nil || operation.State != domain.OperationFailed || operation.ErrorKind != domain.ErrorPreconditionFailed {
		t.Fatalf("Copy() = %+v, %v", operation, err)
	}
	if _, err := winner.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/winner")); err != nil {
		t.Fatalf("winning mutation was rolled back: %v", err)
	}
	if _, err := winner.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/source")); err != nil {
		t.Fatalf("source disappeared: %v", err)
	}
	if _, err := winner.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/copy")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("failed operation published destination: %v", err)
	}
	stored, err := winner.Files().GetOperation(context.Background(), user, operation.ID)
	if err != nil || stored.State != domain.OperationFailed {
		t.Fatalf("GetOperation() = %+v, %v", stored, err)
	}
}

type stepPause struct {
	step    string
	reached chan struct{}
	release chan struct{}
}

func newStepPause(step string) *stepPause {
	return &stepPause{step: step, reached: make(chan struct{}), release: make(chan struct{})}
}

func (pause *stepPause) Step(_ context.Context, step string) error {
	if step == pause.step {
		close(pause.reached)
		<-pause.release
	}
	return nil
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
	request, _ := http.NewRequest(capability.Method, capability.URL, bytes.NewReader(body))
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if _, err := files.CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: int64(len(body)), MediaType: "text/plain"}); err != nil {
		t.Fatal(err)
	}
}
