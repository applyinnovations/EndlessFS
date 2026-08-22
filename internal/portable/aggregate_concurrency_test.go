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
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
)

func TestReplicaFileCursorKeepsCurrentAggregateSnapshotAcrossMutation(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2041, 1, 1, 3, 4, 5, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(198, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	first := openEngine(t, backend, clock, 199, nil)
	second := openEngine(t, backend, clock, 200, nil)
	user, _ := domain.ParseUserID("WFhYWFhYWFhYWFhYWFhYWA")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	folder := domain.MustParseUserPath("/folder")
	if _, err := first.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: folder}); err != nil {
		t.Fatal(err)
	}
	uploadPortableFile(t, server.Client(), first.Files(), scope, domain.MustParseUserPath("/folder/a.bin"), []byte("four"))
	uploadPortableFile(t, server.Client(), first.Files(), scope, domain.MustParseUserPath("/folder/b.bin"), []byte("sixsix"))
	page, err := first.Files().List(context.Background(), scope, domain.ListRequest{Directory: folder, PageSize: 1})
	if err != nil || page.Current.Size != 10 || page.Current.FileCount != 2 || len(page.Entries) != 1 || page.NextCursor == "" {
		t.Fatalf("first replica List() = %+v, %v", page, err)
	}
	uploadPortableFile(t, server.Client(), second.Files(), scope, domain.MustParseUserPath("/folder/c.bin"), []byte("new"))
	next, err := second.Files().List(context.Background(), scope, domain.ListRequest{Directory: folder, PageSize: 1, Cursor: page.NextCursor})
	if err != nil || next.Current != page.Current || next.Current.Size != 10 || next.Current.FileCount != 2 || len(next.Entries) != 1 {
		t.Fatalf("second replica cursor List() = %+v, %v; want current %+v", next, err, page.Current)
	}
	fresh, err := second.Files().List(context.Background(), scope, domain.ListRequest{Directory: folder})
	if err != nil || fresh.Current.Size != 13 || fresh.Current.FileCount != 3 || len(fresh.Entries) != 3 {
		t.Fatalf("fresh second replica List() = %+v, %v", fresh, err)
	}
}

func TestEightReplicaConcurrentMultiFileCompletionConvergesRecursiveAggregates(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2041, 1, 2, 3, 4, 5, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(201, 1<<20)))); err != nil {
		t.Fatal(err)
	}

	const replicaCount = 8
	barrier := newAggregateBarrier(replicaCount)
	engines := make([]*portable.Engine, replicaCount)
	schedulers := make([]*aggregateOneShotScheduler, replicaCount)
	for index := range engines {
		schedulers[index] = &aggregateOneShotScheduler{step: portable.StepAdmissionAfterCandidate, barrier: barrier}
		engines[index] = openEngine(t, backend, clock, byte(202+index), schedulers[index])
	}
	user, _ := domain.ParseUserID("WVlZWVlZWVlZWVlZWVlZWQ")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	for _, path := range []string{"/batch", "/batch/nested"} {
		if _, err := engines[0].Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(path)}); err != nil {
			t.Fatal(err)
		}
	}

	type pendingUpload struct {
		capability domain.UploadCapability
		completion domain.CompleteUploadRequest
	}
	uploads := make([]pendingUpload, replicaCount)
	var wantTotal int64
	for index, engine := range engines {
		path := fmt.Sprintf("/batch/file-%d.bin", index)
		if index%2 == 1 {
			path = fmt.Sprintf("/batch/nested/file-%d.bin", index)
		}
		body := bytes.Repeat([]byte{byte('a' + index)}, index+1)
		wantTotal += int64(len(body))
		capability, err := engine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{
			Path: domain.MustParseUserPath(path), Size: int64(len(body)), MediaType: "application/octet-stream", Resumable: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		sendPortableUpload(t, server.Client(), capability, body, 0)
		uploads[index] = pendingUpload{
			capability: capability,
			completion: domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: domain.MustParseUserPath(path), Size: int64(len(body)), MediaType: "application/octet-stream"},
		}
	}
	for _, scheduler := range schedulers {
		scheduler.Enable()
	}

	start := make(chan struct{})
	errorsFound := make([]error, replicaCount)
	var wait sync.WaitGroup
	for index, engine := range engines {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			for attempt := 0; attempt < 64; attempt++ {
				_, errorsFound[index] = engine.Files().CompleteUpload(context.Background(), scope, uploads[index].completion)
				if errorsFound[index] == nil || !errors.Is(errorsFound[index], domain.ErrUnavailable) {
					return
				}
			}
		}()
	}
	close(start)
	wait.Wait()
	for index, err := range errorsFound {
		if err != nil {
			t.Errorf("replica %d CompleteUpload() error = %v", index, err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	if got := assertVisibleRecursiveAggregates(t, engines[0].Files(), scope, domain.MustParseUserPath("/")); got != wantTotal {
		t.Fatalf("visible recursive total = %d; want %d", got, wantTotal)
	}

	for index, engine := range engines {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := engine.Files().CompleteUpload(context.Background(), scope, uploads[index].completion); err != nil {
				t.Errorf("replica %d replayed CompleteUpload() error = %v", index, err)
			}
		}()
	}
	wait.Wait()
	if got := assertVisibleRecursiveAggregates(t, engines[1].Files(), scope, domain.MustParseUserPath("/")); got != wantTotal {
		t.Fatalf("replayed visible recursive total = %d; want %d", got, wantTotal)
	}
}

func TestEightReplicaSameTargetUploadRacesHaveOneAggregateWinner(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		name := "create"
		if replacement {
			name = "replace"
		}
		t.Run(name, func(t *testing.T) {
			backend := objectmemory.New()
			server := httptest.NewServer(backend)
			t.Cleanup(server.Close)
			clock := domain.NewFixedClock(time.Date(2041, 2, 3, 4, 5, 6, 0, time.UTC))
			if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(212, 1<<20)))); err != nil {
				t.Fatal(err)
			}
			const replicaCount = 8
			barrier := newAggregateBarrier(replicaCount)
			engines := make([]*portable.Engine, replicaCount)
			schedulers := make([]*aggregateOneShotScheduler, replicaCount)
			for index := range engines {
				schedulers[index] = &aggregateOneShotScheduler{step: portable.StepAdmissionAfterCandidate, barrier: barrier}
				engines[index] = openEngine(t, backend, clock, byte(213+index), schedulers[index])
			}
			user, _ := domain.ParseUserID("WlpaWlpaWlpaWlpaWlpaWg")
			scope, _ := domain.NewScope(user, domain.AreaLive)
			path := domain.MustParseUserPath("/same-target.bin")
			var expectedVersion domain.Version
			if replacement {
				uploadPortableFile(t, server.Client(), engines[0].Files(), scope, path, []byte("base"))
				entry, err := engines[0].Files().Stat(context.Background(), scope, path)
				if err != nil {
					t.Fatal(err)
				}
				expectedVersion = entry.Version
			}

			completions := make([]domain.CompleteUploadRequest, replicaCount)
			for index, engine := range engines {
				body := bytes.Repeat([]byte{byte('k' + index)}, index+5)
				request := domain.CreateUploadRequest{Path: path, Size: int64(len(body)), MediaType: "application/octet-stream"}
				if replacement {
					request.Conflict = domain.ConflictReplace
					request.ExpectedVersion = expectedVersion
				}
				capability, err := engine.Files().CreateUpload(context.Background(), scope, request)
				if err != nil {
					t.Fatal(err)
				}
				sendPortableUpload(t, server.Client(), capability, body, 0)
				completions[index] = domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: int64(len(body)), MediaType: "application/octet-stream"}
			}
			for _, scheduler := range schedulers {
				scheduler.Enable()
			}
			start := make(chan struct{})
			entries := make([]domain.Entry, replicaCount)
			errorsFound := make([]error, replicaCount)
			var wait sync.WaitGroup
			for index, engine := range engines {
				wait.Add(1)
				go func() {
					defer wait.Done()
					<-start
					for attempt := 0; attempt < 64; attempt++ {
						entries[index], errorsFound[index] = engine.Files().CompleteUpload(context.Background(), scope, completions[index])
						if errorsFound[index] == nil || !errors.Is(errorsFound[index], domain.ErrUnavailable) {
							return
						}
					}
				}()
			}
			close(start)
			wait.Wait()
			winners := 0
			var winningSize int64
			for index, err := range errorsFound {
				if err == nil {
					winners++
					winningSize = entries[index].Size
					continue
				}
				if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
					t.Errorf("replica %d CompleteUpload() error = %v", index, err)
				}
			}
			if winners != 1 {
				t.Fatalf("successful same-target completions = %d; want 1 (errors %v)", winners, errorsFound)
			}
			if got := assertVisibleRecursiveAggregates(t, engines[7].Files(), scope, domain.MustParseUserPath("/")); got != winningSize {
				t.Fatalf("same-target recursive total = %d; want winner size %d", got, winningSize)
			}
		})
	}
}

func TestEightReplicaSameUploadCompletionIsIdempotentAndAggregatedOnce(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2041, 2, 4, 4, 5, 6, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(224, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	const replicaCount = 8
	barrier := newAggregateBarrier(replicaCount)
	engines := make([]*portable.Engine, replicaCount)
	schedulers := make([]*aggregateOneShotScheduler, replicaCount)
	for index := range engines {
		schedulers[index] = &aggregateOneShotScheduler{step: portable.StepAdmissionAfterCandidate, barrier: barrier}
		engines[index] = openEngine(t, backend, clock, byte(225+index), schedulers[index])
	}
	user, _ := domain.ParseUserID("Wl5eWl5eWl5eWl5eWl5eWg")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	path := domain.MustParseUserPath("/same-upload.bin")
	body := []byte("exactly once")
	capability, err := engines[0].Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: path, Size: int64(len(body)), MediaType: "application/octet-stream"})
	if err != nil {
		t.Fatal(err)
	}
	sendPortableUpload(t, server.Client(), capability, body, 0)
	for _, scheduler := range schedulers {
		scheduler.Enable()
	}
	completion := domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: int64(len(body)), MediaType: "application/octet-stream"}
	start := make(chan struct{})
	entries := make([]domain.Entry, replicaCount)
	errorsFound := make([]error, replicaCount)
	var wait sync.WaitGroup
	for index, engine := range engines {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			for attempt := 0; attempt < 64; attempt++ {
				entries[index], errorsFound[index] = engine.Files().CompleteUpload(context.Background(), scope, completion)
				if errorsFound[index] == nil || !errors.Is(errorsFound[index], domain.ErrUnavailable) {
					return
				}
			}
		}()
	}
	close(start)
	wait.Wait()
	for index, err := range errorsFound {
		if err != nil {
			t.Errorf("replica %d CompleteUpload() error = %v", index, err)
			continue
		}
		if entries[index].Size != int64(len(body)) || entries[index].FileCount != 1 {
			t.Errorf("replica %d completion = %+v", index, entries[index])
		}
	}
	if got := assertVisibleRecursiveAggregates(t, engines[7].Files(), scope, domain.MustParseUserPath("/")); got != int64(len(body)) {
		t.Fatalf("same-upload recursive aggregate = %d; want %d", got, len(body))
	}
}

func TestFailedPartialAbortedAndReplayedUploadsDoNotSkewAggregates(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2041, 3, 4, 5, 6, 7, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(231, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	engine := openEngine(t, backend, clock, 232, nil)
	user, _ := domain.ParseUserID("W1tbW1tbW1tbW1tbW1tbWw")
	scope, _ := domain.NewScope(user, domain.AreaLive)

	partialPath := domain.MustParseUserPath("/partial.bin")
	partial, err := engine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: partialPath, Size: 10, MediaType: "application/octet-stream", Resumable: true})
	if err != nil {
		t.Fatal(err)
	}
	sendPortableUpload(t, server.Client(), partial, []byte("part"), 0)
	if _, err := engine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: partial.UploadID, Path: partialPath, Size: 10, MediaType: "application/octet-stream"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("partial CompleteUpload() error = %v", err)
	}

	failedPath := domain.MustParseUserPath("/failed.bin")
	failed, err := engine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: failedPath, Size: 6, MediaType: "application/octet-stream", Resumable: true})
	if err != nil {
		t.Fatal(err)
	}
	backend.InjectTransferFault(objectmemory.TransferUploadData, objectmemory.TransferFaultInterrupted)
	sendPortableUploadExpectStatus(t, server.Client(), failed, []byte("broken"), 0, http.StatusServiceUnavailable)
	if _, err := engine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: failed.UploadID, Path: failedPath, Size: 6, MediaType: "application/octet-stream"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("failed CompleteUpload() error = %v", err)
	}

	abortedPath := domain.MustParseUserPath("/aborted.bin")
	abortedBody := []byte("abort")
	aborted, err := engine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: abortedPath, Size: int64(len(abortedBody)), MediaType: "application/octet-stream"})
	if err != nil {
		t.Fatal(err)
	}
	sendPortableUpload(t, server.Client(), aborted, abortedBody, 0)
	if err := engine.Files().AbortUpload(context.Background(), scope, aborted.UploadID); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: aborted.UploadID, Path: abortedPath, Size: int64(len(abortedBody)), MediaType: "application/octet-stream"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("aborted CompleteUpload() error = %v", err)
	}

	checksumPath := domain.MustParseUserPath("/checksum.bin")
	checksumBody := []byte("valid")
	checksumUpload, err := engine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: checksumPath, Size: int64(len(checksumBody)), MediaType: "application/octet-stream"})
	if err != nil {
		t.Fatal(err)
	}
	sendPortableUpload(t, server.Client(), checksumUpload, checksumBody, 0)
	completion := domain.CompleteUploadRequest{UploadID: checksumUpload.UploadID, Path: checksumPath, Size: int64(len(checksumBody)), MediaType: "application/octet-stream"}
	if got := assertVisibleRecursiveAggregates(t, engine.Files(), scope, domain.MustParseUserPath("/")); got != 0 {
		t.Fatalf("aggregate before valid completion = %d; want 0", got)
	}
	if _, err := engine.Files().CompleteUpload(context.Background(), scope, completion); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Files().CompleteUpload(context.Background(), scope, completion); err != nil {
		t.Fatalf("replayed CompleteUpload() error = %v", err)
	}
	if got := assertVisibleRecursiveAggregates(t, engine.Files(), scope, domain.MustParseUserPath("/")); got != int64(len(checksumBody)) {
		t.Fatalf("aggregate after valid replayed completion = %d; want %d", got, len(checksumBody))
	}
}

func TestConcurrentReplicaUploadCompletionAndAbortNeverSkewAggregate(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2041, 3, 5, 5, 6, 7, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(235, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	barrier := newAggregateBarrier(2)
	completionScheduler := &aggregateOneShotScheduler{step: portable.StepAdmissionAfterCandidate, barrier: barrier}
	abortScheduler := &aggregateOneShotScheduler{step: portable.StepAdmissionAfterCandidate, barrier: barrier}
	completionEngine := openEngine(t, backend, clock, 236, completionScheduler)
	abortEngine := openEngine(t, backend, clock, 237, abortScheduler)
	user, _ := domain.ParseUserID("XFxgXFxgXFxgXFxgXFxgXA")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	path := domain.MustParseUserPath("/completion-abort.bin")
	body := []byte("racing")
	capability, err := completionEngine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: path, Size: int64(len(body)), MediaType: "application/octet-stream"})
	if err != nil {
		t.Fatal(err)
	}
	sendPortableUpload(t, server.Client(), capability, body, 0)
	completionScheduler.Enable()
	abortScheduler.Enable()

	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, completeErr := completionEngine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: int64(len(body)), MediaType: "application/octet-stream"})
		results <- completeErr
	}()
	go func() {
		<-start
		results <- abortEngine.Files().AbortUpload(context.Background(), scope, capability.UploadID)
	}()
	close(start)
	for range 2 {
		err := <-results
		if err != nil && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrPreconditionFailed) && !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("completion/abort race error = %v", err)
		}
	}
	got := assertVisibleRecursiveAggregates(t, completionEngine.Files(), scope, domain.MustParseUserPath("/"))
	if got != 0 && got != int64(len(body)) {
		t.Fatalf("completion/abort recursive aggregate = %d; want 0 or %d", got, len(body))
	}
	entry, statErr := completionEngine.Files().Stat(context.Background(), scope, path)
	if got == 0 && !errors.Is(statErr, domain.ErrNotFound) {
		t.Fatalf("aborted winner left visible entry %+v, %v", entry, statErr)
	}
	if got == int64(len(body)) && (statErr != nil || entry.Size != got || entry.FileCount != 1) {
		t.Fatalf("completion winner entry = %+v, %v; aggregate %d", entry, statErr, got)
	}
}

func TestConcurrentReplicaFolderMutationsKeepRecursiveAggregatesAtomic(t *testing.T) {
	for _, test := range []struct {
		name       string
		trashFirst bool
		run        func(*portable.Engine, *portable.Engine, domain.Scope, domain.Scope) (domain.Operation, domain.Operation)
	}{
		{
			name: "rename-versus-delete",
			run: func(first, second *portable.Engine, live, _ domain.Scope) (domain.Operation, domain.Operation) {
				return runConcurrentOperations(
					func() (domain.Operation, error) {
						return first.Files().Move(context.Background(), live, live, domain.MoveRequest{Source: domain.MustParseUserPath("/tree"), Destination: domain.MustParseUserPath("/renamed"), IdempotencyKey: "aggregate-rename-race"})
					},
					func() (domain.Operation, error) {
						return second.Files().Delete(context.Background(), live, domain.DeleteRequest{Path: domain.MustParseUserPath("/tree"), IdempotencyKey: "aggregate-delete-race"})
					},
				)
			},
		},
		{
			name: "trash-versus-delete",
			run: func(first, second *portable.Engine, live, trash domain.Scope) (domain.Operation, domain.Operation) {
				return runConcurrentOperations(
					func() (domain.Operation, error) {
						return first.Files().Move(context.Background(), live, trash, domain.MoveRequest{Source: domain.MustParseUserPath("/tree"), Destination: domain.MustParseUserPath("/trashed"), IdempotencyKey: "aggregate-trash-race"})
					},
					func() (domain.Operation, error) {
						return second.Files().Delete(context.Background(), live, domain.DeleteRequest{Path: domain.MustParseUserPath("/tree"), IdempotencyKey: "aggregate-delete-race"})
					},
				)
			},
		},
		{
			name:       "restore-versus-permanent-delete",
			trashFirst: true,
			run: func(first, second *portable.Engine, live, trash domain.Scope) (domain.Operation, domain.Operation) {
				return runConcurrentOperations(
					func() (domain.Operation, error) {
						return first.Files().Move(context.Background(), trash, live, domain.MoveRequest{Source: domain.MustParseUserPath("/tree"), Destination: domain.MustParseUserPath("/restored"), IdempotencyKey: "aggregate-restore-race"})
					},
					func() (domain.Operation, error) {
						return second.Files().Delete(context.Background(), trash, domain.DeleteRequest{Path: domain.MustParseUserPath("/tree"), IdempotencyKey: "aggregate-permanent-delete-race"})
					},
				)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := objectmemory.New()
			server := httptest.NewServer(backend)
			t.Cleanup(server.Close)
			clock := domain.NewFixedClock(time.Date(2041, 4, 5, 6, 7, 8, 0, time.UTC))
			if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(241, 1<<20)))); err != nil {
				t.Fatal(err)
			}
			barrier := newAggregateBarrier(2)
			firstScheduler := &aggregateOneShotScheduler{step: portable.StepAdmissionAfterCandidate, barrier: barrier}
			secondScheduler := &aggregateOneShotScheduler{step: portable.StepAdmissionAfterCandidate, barrier: barrier}
			first := openEngine(t, backend, clock, 242, firstScheduler)
			second := openEngine(t, backend, clock, 243, secondScheduler)
			user, _ := domain.ParseUserID("XFxcXFxcXFxcXFxcXFxcXA")
			live, _ := domain.NewScope(user, domain.AreaLive)
			trash, _ := domain.NewScope(user, domain.AreaTrash)
			seedAggregateTree(t, server.Client(), first, live)
			if test.trashFirst {
				operation, err := first.Files().Move(context.Background(), live, trash, domain.MoveRequest{Source: domain.MustParseUserPath("/tree"), Destination: domain.MustParseUserPath("/tree"), IdempotencyKey: "aggregate-race-setup-trash"})
				if err != nil || operation.State != domain.OperationSucceeded {
					t.Fatalf("setup trash Move() = %+v, %v", operation, err)
				}
			}
			firstScheduler.Enable()
			secondScheduler.Enable()

			firstResult, secondResult := test.run(first, second, live, trash)
			succeeded := 0
			failed := 0
			for _, operation := range []domain.Operation{firstResult, secondResult} {
				switch operation.State {
				case domain.OperationSucceeded:
					succeeded++
				case domain.OperationFailed:
					failed++
				default:
					t.Fatalf("contested operation = %+v", operation)
				}
			}
			if succeeded != 1 || failed != 1 {
				t.Fatalf("contested operations = %+v and %+v; want one success and one failed CAS loser", firstResult, secondResult)
			}
			liveTotal := assertVisibleRecursiveAggregates(t, first.Files(), live, domain.MustParseUserPath("/"))
			trashTotal := assertVisibleRecursiveAggregates(t, second.Files(), trash, domain.MustParseUserPath("/"))
			if liveTotal+trashTotal != 0 && liveTotal+trashTotal != 12 {
				t.Fatalf("combined aggregate after contested folder mutation = %d; want 0 or 12", liveTotal+trashTotal)
			}
		})
	}
}

func TestFolderMutationsRecoverAtEveryAggregateCommitBoundary(t *testing.T) {
	for operationIndex, operationName := range []string{"copy", "move", "delete"} {
		for stepIndex, step := range []string{portable.StepOperationAfterPrepared, portable.StepOperationAfterCommitted, portable.StepOperationAfterFinalized} {
			t.Run(operationName+"/"+step, func(t *testing.T) {
				backend := objectmemory.New()
				server := httptest.NewServer(backend)
				t.Cleanup(server.Close)
				clock := domain.NewFixedClock(time.Date(2041, 5, 6+operationIndex, 7, 8, 9, 0, time.UTC))
				if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(byte(251+operationIndex*3+stepIndex), 1<<20)))); err != nil {
					t.Fatal(err)
				}
				crasher := &stepFailure{step: step}
				first := openEngine(t, backend, clock, byte(11+operationIndex*6+stepIndex*2), crasher)
				second := openEngine(t, backend, clock, byte(12+operationIndex*6+stepIndex*2), nil)
				user, _ := domain.ParseUserID("XV1dXV1dXV1dXV1dXV1dXQ")
				scope, _ := domain.NewScope(user, domain.AreaLive)
				seedAggregateTree(t, server.Client(), first, scope)

				var err error
				switch operationName {
				case "copy":
					_, err = first.Files().Copy(context.Background(), scope, scope, domain.CopyRequest{Source: domain.MustParseUserPath("/tree"), Destination: domain.MustParseUserPath("/copy"), IdempotencyKey: "aggregate-crash-copy"})
				case "move":
					_, err = first.Files().Move(context.Background(), scope, scope, domain.MoveRequest{Source: domain.MustParseUserPath("/tree"), Destination: domain.MustParseUserPath("/moved"), IdempotencyKey: "aggregate-crash-move"})
				case "delete":
					_, err = first.Files().Delete(context.Background(), scope, domain.DeleteRequest{Path: domain.MustParseUserPath("/tree"), IdempotencyKey: "aggregate-crash-delete"})
				}
				if !errors.Is(err, domain.ErrUnavailable) {
					t.Fatalf("interrupted %s error = %v", operationName, err)
				}
				wantBeforeRecovery := int64(12)
				if step != portable.StepOperationAfterPrepared {
					switch operationName {
					case "copy":
						wantBeforeRecovery = 24
					case "delete":
						wantBeforeRecovery = 0
					}
				}
				if got := assertVisibleRecursiveAggregates(t, second.Files(), scope, domain.MustParseUserPath("/")); got != wantBeforeRecovery {
					t.Fatalf("interrupted %s aggregate = %d; want %d", operationName, got, wantBeforeRecovery)
				}

				clock.Advance(2 * time.Minute)
				if _, err := second.CreateCheckpoint(context.Background(), fmt.Sprintf("aggregate-%d-%d", operationIndex, stepIndex)); err != nil {
					t.Fatalf("checkpoint recovery error = %v", err)
				}
				wantRecovered := int64(12)
				switch operationName {
				case "copy":
					wantRecovered = 24
				case "delete":
					wantRecovered = 0
				}
				if got := assertVisibleRecursiveAggregates(t, second.Files(), scope, domain.MustParseUserPath("/")); got != wantRecovered {
					t.Fatalf("recovered %s aggregate = %d; want %d", operationName, got, wantRecovered)
				}
			})
		}
	}
}

func runConcurrentOperations(first, second func() (domain.Operation, error)) (domain.Operation, domain.Operation) {
	start := make(chan struct{})
	results := make([]domain.Operation, 2)
	errorsFound := make([]error, 2)
	var wait sync.WaitGroup
	for index, operation := range []func() (domain.Operation, error){first, second} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			results[index], errorsFound[index] = operation()
		}()
	}
	close(start)
	wait.Wait()
	for _, err := range errorsFound {
		if err != nil {
			return domain.Operation{ErrorKind: domain.ErrorInternal, Error: err.Error()}, domain.Operation{ErrorKind: domain.ErrorInternal, Error: err.Error()}
		}
	}
	return results[0], results[1]
}

func seedAggregateTree(t *testing.T, client *http.Client, engine *portable.Engine, scope domain.Scope) {
	t.Helper()
	for _, path := range []string{"/tree", "/tree/nested"} {
		if _, err := engine.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(path)}); err != nil {
			t.Fatal(err)
		}
	}
	uploadPortableFile(t, client, engine.Files(), scope, domain.MustParseUserPath("/tree/first.bin"), []byte("first"))
	uploadPortableFile(t, client, engine.Files(), scope, domain.MustParseUserPath("/tree/nested/second.bin"), []byte("second!"))
}

func assertVisibleRecursiveAggregates(t *testing.T, files interface {
	List(context.Context, domain.Scope, domain.ListRequest) (domain.ListPage, error)
	Stat(context.Context, domain.Scope, domain.UserPath) (domain.Entry, error)
}, scope domain.Scope, directory domain.UserPath) int64 {
	t.Helper()
	var total int64
	var fileCount int64
	cursor := ""
	for {
		page, err := files.List(context.Background(), scope, domain.ListRequest{Directory: directory, PageSize: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("List(%s) error = %v", directory, err)
		}
		for _, entry := range page.Entries {
			if entry.Kind == domain.EntryFile {
				if entry.FileCount != 1 {
					t.Errorf("file entry %s count = %d; want 1", entry.Path, entry.FileCount)
				}
				total += entry.Size
				fileCount++
				continue
			}
			childTotal := assertVisibleRecursiveAggregates(t, files, scope, entry.Path)
			child, err := files.Stat(context.Background(), scope, entry.Path)
			if err != nil {
				t.Fatalf("Stat(%s) error = %v", entry.Path, err)
			}
			if entry.Size != childTotal || entry.FileCount != child.FileCount {
				t.Errorf("directory entry %s aggregate = %d; visible descendants = %d", entry.Path, entry.Size, childTotal)
			}
			total += childTotal
			fileCount += child.FileCount
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	entry, err := files.Stat(context.Background(), scope, directory)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", directory, err)
	}
	if entry.Size != total || entry.FileCount != fileCount {
		t.Errorf("persisted aggregates %s = %d bytes/%d files; visible descendants = %d bytes/%d files", directory, entry.Size, entry.FileCount, total, fileCount)
	}
	return total
}

func sendPortableUpload(t *testing.T, client *http.Client, capability domain.UploadCapability, body []byte, offset int64) {
	t.Helper()
	sendPortableUploadExpectStatus(t, client, capability, body, offset, http.StatusNoContent)
}

func sendPortableUploadExpectStatus(t *testing.T, client *http.Client, capability domain.UploadCapability, body []byte, offset int64, wantStatus int) {
	t.Helper()
	request, err := http.NewRequest(capability.Method, capability.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	if capability.Protocol == domain.UploadResumable {
		request.Header.Set("Upload-Offset", fmt.Sprintf("%d", offset))
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != wantStatus {
		t.Fatalf("upload status = %d; want %d", response.StatusCode, wantStatus)
	}
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
