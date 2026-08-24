package architecturelab

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
)

type commitFaultBackend struct {
	objectstore.Backend
	mu          sync.Mutex
	failBefore  bool
	loseSuccess bool
}

type readBarrierBackend struct {
	objectstore.Backend
	mu      sync.Mutex
	enabled bool
	reads   int
	release chan struct{}
}

func (backend *readBarrierBackend) enable() {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.enabled, backend.reads, backend.release = true, 0, make(chan struct{})
}

func (backend *readBarrierBackend) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	object, err := backend.Backend.Get(ctx, key)
	if err != nil || !(strings.HasSuffix(key.String(), "/head.json") || strings.HasSuffix(key.String(), "/root.json")) {
		return object, err
	}
	backend.mu.Lock()
	if !backend.enabled {
		backend.mu.Unlock()
		return object, nil
	}
	backend.reads++
	release := backend.release
	if backend.reads == 2 {
		backend.enabled = false
		close(release)
	}
	backend.mu.Unlock()
	select {
	case <-release:
		return object, nil
	case <-ctx.Done():
		return objectstore.Object{}, ctx.Err()
	}
}

func (backend *commitFaultBackend) armBefore() {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.failBefore = true
}

func (backend *commitFaultBackend) armLostSuccess() {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.loseSuccess = true
}

func (backend *commitFaultBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	commit := strings.HasSuffix(key.String(), "/head.json") || strings.HasSuffix(key.String(), "/root.json")
	backend.mu.Lock()
	failBefore := commit && backend.failBefore
	loseSuccess := commit && backend.loseSuccess
	if failBefore {
		backend.failBefore = false
	}
	if loseSuccess {
		backend.loseSuccess = false
	}
	backend.mu.Unlock()
	if failBefore {
		return "", domain.NewError(domain.ErrorUnavailable, "injected failure before commit")
	}
	version, err := backend.Backend.Put(ctx, key, body, condition)
	if err == nil && loseSuccess {
		return "", domain.NewError(domain.ErrorUnavailable, "injected lost success")
	}
	return version, err
}

func TestCandidatesRecoverBeforeCommitAndLostSuccess(t *testing.T) {
	ctx := context.Background()
	for _, factory := range CandidateFactories() {
		t.Run(factory.Name, func(t *testing.T) {
			backend := &commitFaultBackend{Backend: objectmemory.New()}
			engine, err := factory.Open(ctx, backend, Options{DomainID: "recovery-user"})
			if err != nil {
				t.Fatal(err)
			}
			mutation := Mutation{ID: "mkdir-recovery", Kind: MutationCreateDirectory, ToArea: AreaLive, Destination: "/recovery", NodeID: "recovery"}

			backend.armBefore()
			if _, err := engine.Mutate(ctx, mutation); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("failure before commit = %v", err)
			}
			snapshot, err := engine.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := snapshot.Lookup(AreaLive, "/recovery"); ok {
				t.Fatal("failed mutation became visible")
			}

			backend.armLostSuccess()
			if _, err := engine.Mutate(ctx, mutation); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("lost success = %v", err)
			}
			outcome, err := engine.Mutate(ctx, mutation)
			if err != nil || !outcome.Replayed || !outcome.Committed {
				t.Fatalf("lost-success replay = %+v, %v", outcome, err)
			}
			snapshot, err = engine.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := snapshot.Lookup(AreaLive, "/recovery"); !ok {
				t.Fatal("committed lost success was not visible")
			}
		})
	}
}

func TestCandidatesHaveOneConditionalWinnerAcrossReplicas(t *testing.T) {
	ctx := context.Background()
	for _, factory := range CandidateFactories() {
		t.Run(factory.Name, func(t *testing.T) {
			backend := &readBarrierBackend{Backend: objectmemory.New()}
			first, err := factory.Open(ctx, backend, Options{DomainID: "concurrent-user"})
			if err != nil {
				t.Fatal(err)
			}
			second, err := factory.Open(ctx, backend, Options{DomainID: "concurrent-user"})
			if err != nil {
				t.Fatal(err)
			}
			mutations := []Mutation{
				{ID: "mkdir-a", Kind: MutationCreateDirectory, ToArea: AreaLive, Destination: "/a", NodeID: "a"},
				{ID: "mkdir-b", Kind: MutationCreateDirectory, ToArea: AreaLive, Destination: "/b", NodeID: "b"},
			}
			engines := []Engine{first, second}
			type result struct {
				index int
				err   error
			}
			results := make(chan result, 2)
			backend.enable()
			for index := range engines {
				index := index
				go func() {
					_, err := engines[index].Mutate(ctx, mutations[index])
					results <- result{index: index, err: err}
				}()
			}
			winner, loser := -1, -1
			for range 2 {
				result := <-results
				if result.err == nil {
					if winner != -1 {
						t.Fatal("both concurrent commits succeeded")
					}
					winner = result.index
				} else if errors.Is(result.err, domain.ErrPreconditionFailed) || errors.Is(result.err, domain.ErrConflict) {
					loser = result.index
				} else {
					t.Fatalf("concurrent mutation %d: %v", result.index, result.err)
				}
			}
			if winner == -1 || loser == -1 {
				t.Fatalf("winner=%d loser=%d", winner, loser)
			}
			if _, err := engines[loser].Mutate(ctx, mutations[loser]); err != nil {
				t.Fatalf("rebased loser: %v", err)
			}
			snapshot, err := first.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := snapshot.Lookup(AreaLive, "/a"); !ok {
				t.Fatal("/a is missing after rebase")
			}
			if _, ok := snapshot.Lookup(AreaLive, "/b"); !ok {
				t.Fatal("/b is missing after rebase")
			}
		})
	}
}

func TestClaimedCandidatePermanentlyBindsAttemptedIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	backend := &commitFaultBackend{Backend: objectmemory.New()}
	engine, err := openClaimedEmbeddedGraph(ctx, backend, Options{DomainID: "claimed-reuse"})
	if err != nil {
		t.Fatal(err)
	}
	backend.armBefore()
	first := Mutation{ID: "same-key", Kind: MutationCreateDirectory, ToArea: AreaLive, Destination: "/first", NodeID: "first"}
	if _, err := engine.Mutate(ctx, first); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("injected first attempt = %v", err)
	}
	second := Mutation{ID: "same-key", Kind: MutationCreateDirectory, ToArea: AreaLive, Destination: "/second", NodeID: "second"}
	if _, err := engine.Mutate(ctx, second); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("different intent reused prepared claim: %v", err)
	}
	if _, err := engine.Mutate(ctx, first); err != nil {
		t.Fatalf("original intent could not resume: %v", err)
	}
}

func TestHybridCompactionIsCrashResumable(t *testing.T) {
	ctx := context.Background()
	backend := &commitFaultBackend{Backend: objectmemory.New()}
	candidate, err := openHybrid(ctx, backend, Options{DomainID: "hybrid-compaction"})
	if err != nil {
		t.Fatal(err)
	}
	engine := candidate.(*hybridEngine)
	for index := 0; index < 8; index++ {
		name := fmt.Sprintf("item-%d", index)
		if _, err := engine.Mutate(ctx, Mutation{ID: "create-" + name, Kind: MutationCreateFile, ToArea: AreaLive, Destination: "/" + name, NodeID: name, Size: 1, BlobIdentity: "blob-" + name}); err != nil {
			t.Fatal(err)
		}
	}
	backend.armBefore()
	if err := engine.Compact(ctx); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("compaction failure before commit = %v", err)
	}
	before, err := engine.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Nodes) != 10 {
		t.Fatalf("failed compaction changed visible nodes: %d", len(before.Nodes))
	}
	if err := engine.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := engine.Snapshot(ctx)
	if err != nil || after.Revision != before.Revision || len(after.Nodes) != len(before.Nodes) {
		t.Fatalf("resumed compaction snapshot = revision %d nodes %d, %v; want revision %d nodes %d", after.Revision, len(after.Nodes), err, before.Revision, len(before.Nodes))
	}
}
