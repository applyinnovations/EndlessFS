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

func TestPortableRecursiveAggregatesTrackEveryFileMutation(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2040, 1, 3, 3, 4, 5, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(74, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	engine := openEngine(t, backend, clock, 75, nil)
	user, _ := domain.ParseUserID("V1dXV1dXV1dXV1dXV1dXVw")
	live, _ := domain.NewScope(user, domain.AreaLive)
	trash, _ := domain.NewScope(user, domain.AreaTrash)

	for _, path := range []string{"/alpha", "/alpha/bravo"} {
		if _, err := engine.Files().CreateDirectory(context.Background(), live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(path)}); err != nil {
			t.Fatal(err)
		}
	}
	assertAggregates := func(scope domain.Scope, expected map[string][2]int64) {
		t.Helper()
		for path, aggregate := range expected {
			entry, err := engine.Files().Stat(context.Background(), scope, domain.MustParseUserPath(path))
			if err != nil || entry.Size != aggregate[0] || entry.FileCount != aggregate[1] {
				t.Fatalf("Stat(%s) = %+v, %v; want recursive size/count %d/%d", path, entry, err, aggregate[0], aggregate[1])
			}
		}
	}
	assertAggregates(live, map[string][2]int64{"/": {0, 0}, "/alpha": {0, 0}, "/alpha/bravo": {0, 0}})

	uploadPortableFile(t, server.Client(), engine.Files(), live, domain.MustParseUserPath("/alpha/first.txt"), []byte("first"))
	uploadPortableFile(t, server.Client(), engine.Files(), live, domain.MustParseUserPath("/alpha/bravo/second.txt"), []byte("second!"))
	assertAggregates(live, map[string][2]int64{"/": {12, 2}, "/alpha": {12, 2}, "/alpha/bravo": {7, 1}})
	first, err := engine.Files().Stat(context.Background(), live, domain.MustParseUserPath("/alpha/first.txt"))
	if err != nil {
		t.Fatal(err)
	}
	replacement := []byte("first-new")
	replacementCapability, err := engine.Files().CreateUpload(context.Background(), live, domain.CreateUploadRequest{
		Path: domain.MustParseUserPath("/alpha/first.txt"), Size: int64(len(replacement)), MediaType: "text/plain",
		Conflict: domain.ConflictReplace, ExpectedVersion: first.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	replacementRequest, _ := http.NewRequest(replacementCapability.Method, replacementCapability.URL, bytes.NewReader(replacement))
	for name, value := range replacementCapability.Headers {
		replacementRequest.Header.Set(name, value)
	}
	replacementResponse, err := server.Client().Do(replacementRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = replacementResponse.Body.Close()
	if _, err := engine.Files().CompleteUpload(context.Background(), live, domain.CompleteUploadRequest{
		UploadID: replacementCapability.UploadID, Path: domain.MustParseUserPath("/alpha/first.txt"), Size: int64(len(replacement)), MediaType: "text/plain",
	}); err != nil {
		t.Fatal(err)
	}
	assertAggregates(live, map[string][2]int64{"/": {16, 2}, "/alpha": {16, 2}, "/alpha/bravo": {7, 1}})
	page, err := engine.Files().List(context.Background(), live, domain.ListRequest{Directory: domain.MustParseUserPath("/alpha")})
	if err != nil {
		t.Fatal(err)
	}
	if bravo, found := findEntry(page.Entries, "bravo"); !found || bravo.Size != 7 || bravo.FileCount != 1 || page.Current.FileCount != 2 {
		t.Fatalf("List(/alpha) = %+v; bravo = %+v, %t; want recursive size/count 7/1 and current count 2", page.Current, bravo, found)
	}

	copyOperation, err := engine.Files().Copy(context.Background(), live, live, domain.CopyRequest{
		Source: domain.MustParseUserPath("/alpha"), Destination: domain.MustParseUserPath("/copy"), IdempotencyKey: "aggregate-copy-1",
	})
	if err != nil || copyOperation.State != domain.OperationSucceeded {
		t.Fatalf("Copy() = %+v, %v", copyOperation, err)
	}
	assertAggregates(live, map[string][2]int64{"/": {32, 4}, "/alpha": {16, 2}, "/copy": {16, 2}, "/copy/bravo": {7, 1}})

	trashOperation, err := engine.Files().Move(context.Background(), live, trash, domain.MoveRequest{
		Source: domain.MustParseUserPath("/copy/bravo"), Destination: domain.MustParseUserPath("/trashed"), IdempotencyKey: "aggregate-trash-1",
	})
	if err != nil || trashOperation.State != domain.OperationSucceeded {
		t.Fatalf("trash Move() = %+v, %v", trashOperation, err)
	}
	assertAggregates(live, map[string][2]int64{"/": {25, 3}, "/copy": {9, 1}})
	assertAggregates(trash, map[string][2]int64{"/": {7, 1}, "/trashed": {7, 1}})

	restoreOperation, err := engine.Files().Move(context.Background(), trash, live, domain.MoveRequest{
		Source: domain.MustParseUserPath("/trashed"), Destination: domain.MustParseUserPath("/restored"), IdempotencyKey: "aggregate-restore-1",
	})
	if err != nil || restoreOperation.State != domain.OperationSucceeded {
		t.Fatalf("restore Move() = %+v, %v", restoreOperation, err)
	}
	assertAggregates(live, map[string][2]int64{"/": {32, 4}, "/restored": {7, 1}})
	assertAggregates(trash, map[string][2]int64{"/": {0, 0}})

	deleteOperation, err := engine.Files().Delete(context.Background(), live, domain.DeleteRequest{
		Path: domain.MustParseUserPath("/restored"), IdempotencyKey: "aggregate-delete-1",
	})
	if err != nil || deleteOperation.State != domain.OperationSucceeded {
		t.Fatalf("Delete() = %+v, %v", deleteOperation, err)
	}
	assertAggregates(live, map[string][2]int64{"/": {25, 3}, "/alpha": {16, 2}, "/copy": {9, 1}})
}

func findEntry(entries []domain.Entry, name string) (domain.Entry, bool) {
	for _, entry := range entries {
		if entry.Name == name {
			return entry, true
		}
	}
	return domain.Entry{}, false
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
	if source, err := second.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/source")); err != nil || source.Size != 5 || source.FileCount != 1 {
		t.Fatalf("pre-commit source aggregate = %+v, %v", source, err)
	}
	if destination, err := second.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/destination")); err != nil || destination.Size != 0 || destination.FileCount != 0 {
		t.Fatalf("pre-commit destination aggregate = %+v, %v", destination, err)
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
	if source, err := second.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/source")); err != nil || source.Size != 0 || source.FileCount != 0 {
		t.Fatalf("post-commit source aggregate = %+v, %v", source, err)
	}
	if destination, err := second.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/destination")); err != nil || destination.Size != 5 || destination.FileCount != 1 {
		t.Fatalf("post-commit destination aggregate = %+v, %v", destination, err)
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
