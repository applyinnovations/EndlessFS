package portable

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
)

func newNamespaceFaultFixture(t *testing.T) (*Engine, *namespaceStore, domain.Scope, domain.Entry) {
	t.Helper()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	entry := seedNamespaceBatchFiles(t, store, live, 1)[0]
	return engine, store, live, entry
}

func assertNamespaceMissing(t *testing.T, store *namespaceStore, scope domain.Scope, path domain.UserPath) {
	t.Helper()
	if _, err := store.stat(context.Background(), scope, path); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Stat(%s) error = %v; want not found", path.String(), err)
	}
}

func TestEveryNamespaceMutationPublishesNothingBeforeHeadCommit(t *testing.T) {
	ctx := context.Background()
	fail := func(engine *Engine) { engine.scheduler = &internalStepFailure{step: StepDomainBeforeHeadCommit} }

	t.Run("create-directory", func(t *testing.T) {
		engine, store, live, _ := newNamespaceFaultFixture(t)
		path := domain.MustParseUserPath("/new-directory")
		fail(engine)
		if _, err := engine.Files().CreateDirectory(ctx, live, domain.CreateDirectoryRequest{Path: path}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("CreateDirectory() error = %v", err)
		}
		assertNamespaceMissing(t, store, live, path)
	})

	t.Run("copy", func(t *testing.T) {
		engine, store, live, source := newNamespaceFaultFixture(t)
		destination := domain.MustParseUserPath("/copy.bin")
		fail(engine)
		if _, err := engine.Files().Copy(ctx, live, live, domain.CopyRequest{Source: source.Path, Destination: destination, ExpectedSource: source.Version, IdempotencyKey: "fault-copy"}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("Copy() error = %v", err)
		}
		if _, err := store.stat(ctx, live, source.Path); err != nil {
			t.Fatalf("copy removed source: %v", err)
		}
		assertNamespaceMissing(t, store, live, destination)
	})

	t.Run("move", func(t *testing.T) {
		engine, store, live, source := newNamespaceFaultFixture(t)
		destination := domain.MustParseUserPath("/moved.bin")
		fail(engine)
		if _, err := engine.Files().Move(ctx, live, live, domain.MoveRequest{Source: source.Path, Destination: destination, ExpectedSource: source.Version, IdempotencyKey: "fault-move"}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("Move() error = %v", err)
		}
		if _, err := store.stat(ctx, live, source.Path); err != nil {
			t.Fatalf("failed move removed source: %v", err)
		}
		assertNamespaceMissing(t, store, live, destination)
	})

	t.Run("delete", func(t *testing.T) {
		engine, store, live, source := newNamespaceFaultFixture(t)
		fail(engine)
		if _, err := engine.Files().Delete(ctx, live, domain.DeleteRequest{Path: source.Path, ExpectedVersion: source.Version, IdempotencyKey: "fault-delete"}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("Delete() error = %v", err)
		}
		if _, err := store.stat(ctx, live, source.Path); err != nil {
			t.Fatalf("failed delete removed source: %v", err)
		}
	})

	t.Run("trash", func(t *testing.T) {
		engine, store, live, source := newNamespaceFaultFixture(t)
		trash, _ := domain.NewScope(live.UserID(), domain.AreaTrash)
		fail(engine)
		if _, err := engine.Files().MoveToTrash(ctx, live.UserID(), domain.TrashRequest{Path: source.Path, ExpectedVersion: source.Version, TrashID: "fault-trash", IdempotencyKey: "fault-trash"}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("MoveToTrash() error = %v", err)
		}
		if _, err := store.stat(ctx, live, source.Path); err != nil {
			t.Fatalf("failed trash removed source: %v", err)
		}
		assertNamespaceMissing(t, store, trash, domain.MustParseUserPath("/fault-trash"))
	})

	t.Run("restore", func(t *testing.T) {
		engine, store, live, source := newNamespaceFaultFixture(t)
		trash, _ := domain.NewScope(live.UserID(), domain.AreaTrash)
		if _, err := engine.Files().MoveToTrash(ctx, live.UserID(), domain.TrashRequest{Path: source.Path, ExpectedVersion: source.Version, TrashID: "restore-me", IdempotencyKey: "seed-trash"}); err != nil {
			t.Fatal(err)
		}
		fail(engine)
		if _, err := engine.Files().RestoreFromTrash(ctx, live.UserID(), "restore-me", domain.ConflictFail, "fault-restore"); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("RestoreFromTrash() error = %v", err)
		}
		assertNamespaceMissing(t, store, live, source.Path)
		if _, err := store.stat(ctx, trash, domain.MustParseUserPath("/restore-me")); err != nil {
			t.Fatalf("failed restore removed trash authority: %v", err)
		}
	})
}

func TestNamespaceMoveLostSuccessAndEightReplicaReplayConverge(t *testing.T) {
	ctx := context.Background()
	engine, store, live, source := newNamespaceFaultFixture(t)
	destination := domain.MustParseUserPath("/lost-success.bin")
	engine.scheduler = &internalStepFailure{step: StepDomainAfterHeadCommit}
	first, err := engine.Files().Move(ctx, live, live, domain.MoveRequest{Source: source.Path, Destination: destination, ExpectedSource: source.Version, IdempotencyKey: "lost-success-move"})
	if err != nil || first.State != domain.OperationSucceeded {
		t.Fatalf("lost-success Move() = %+v, %v", first, err)
	}
	engine.scheduler = nil
	replay, err := engine.Files().Move(ctx, live, live, domain.MoveRequest{Source: source.Path, Destination: destination, ExpectedSource: source.Version, IdempotencyKey: "lost-success-move"})
	if err != nil || replay.ID != first.ID {
		t.Fatalf("Move() replay = %+v, %v; first=%+v", replay, err, first)
	}
	if _, err := store.stat(ctx, live, destination); err != nil {
		t.Fatal(err)
	}
	moved, err := store.stat(ctx, live, destination)
	if err != nil {
		t.Fatal(err)
	}
	assertNamespaceMissing(t, store, live, source.Path)

	// Eight separately constructed replicas now race the same copy intent.
	// Every caller must return the one durable outcome rather than surfacing a
	// provider generation race or publishing multiple namespace versions.
	const replicas = 8
	barrier := &namespaceCommitBarrier{target: replicas, release: make(chan struct{})}
	scheduler := SchedulerFunc(func(_ context.Context, step string) error {
		if step == StepDomainBeforeHeadCommit {
			barrier.wait()
		}
		return nil
	})
	engines := make([]*Engine, replicas)
	for index := range engines {
		engines[index] = openInternalTestEngine(t, engine.backend, engine.clock.(*domain.FixedClock), strings.NewReader(strings.Repeat(string(rune('a'+index)), 1<<15)))
		engines[index].scheduler = scheduler
	}
	copyDestination := domain.MustParseUserPath("/replica-copy.bin")
	results := make(chan struct {
		operation domain.Operation
		err       error
	}, replicas)
	for _, replica := range engines {
		go func(replica *Engine) {
			operation, err := replica.Files().Copy(ctx, live, live, domain.CopyRequest{Source: destination, Destination: copyDestination, ExpectedSource: moved.Version, IdempotencyKey: "eight-replica-copy"})
			results <- struct {
				operation domain.Operation
				err       error
			}{operation, err}
		}(replica)
	}
	var operationID domain.OperationID
	for range replicas {
		result := <-results
		if result.err != nil || result.operation.State != domain.OperationSucceeded {
			t.Fatalf("replica copy = %+v, %v", result.operation, result.err)
		}
		if operationID == "" {
			operationID = result.operation.ID
		} else if result.operation.ID != operationID {
			t.Fatalf("replicas returned distinct operations: %s and %s", operationID, result.operation.ID)
		}
	}
}

type namespaceCommitBarrier struct {
	target  int
	release chan struct{}
	mu      sync.Mutex
	arrived int
}

func (barrier *namespaceCommitBarrier) wait() {
	barrier.mu.Lock()
	barrier.arrived++
	if barrier.arrived == barrier.target {
		close(barrier.release)
	}
	barrier.mu.Unlock()
	<-barrier.release
}
