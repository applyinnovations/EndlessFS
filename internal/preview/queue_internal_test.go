package preview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/state"
)

func queueTestService(t *testing.T) (*Service, Binding, ItemRequest, *domain.FixedClock) {
	t.Helper()
	clock := domain.NewFixedClock(time.Date(2036, 2, 3, 4, 5, 6, 0, time.UTC))
	s := &Service{state: state.NewMemoryStore(), clock: clock, ids: domain.NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte("deterministic queue lease identifiers"), 100))), options: Options{MaxConcurrency: 1, OperationTimeout: time.Minute, OperationRetention: 24 * time.Hour, StartupTimeout: time.Second}}
	binding := internalBinding(t)
	item := ItemRequest{Path: domain.MustParseUserPath("/queue.png"), Version: "source-version", Variant: binding.Variant}
	return s, binding, item, clock
}

type queueFaultStore struct {
	state.AtomicStore
	get    func(context.Context, state.Key) (state.Value, error)
	list   func(context.Context, state.Prefix, state.PageRequest) (state.Page, error)
	mutate func(context.Context, state.Mutation) (state.MutationOutcome, error)
}

func (s queueFaultStore) Get(ctx context.Context, key state.Key) (state.Value, error) {
	if s.get != nil {
		return s.get(ctx, key)
	}
	return s.AtomicStore.Get(ctx, key)
}
func (s queueFaultStore) List(ctx context.Context, prefix state.Prefix, page state.PageRequest) (state.Page, error) {
	if s.list != nil {
		return s.list(ctx, prefix, page)
	}
	return s.AtomicStore.List(ctx, prefix, page)
}
func (s queueFaultStore) Mutate(ctx context.Context, m state.Mutation) (state.MutationOutcome, error) {
	if s.mutate != nil {
		return s.mutate(ctx, m)
	}
	return s.AtomicStore.Mutate(ctx, m)
}

func TestThumbnailQueuePortableStateSurvivesEngineReopen(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprint(split), func(t *testing.T) {
			s, binding, item, clock := queueTestService(t)
			backend := objectmemory.New()
			options := portable.Options{Backend: backend, Clock: clock, IDs: s.ids, LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x51}, 32), Writer: portable.WriterConfiguration{WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "queue-test", KeyringIdentifiers: []string{"session-v1"}}}
			if split {
				options.FileBackend = objectmemory.New()
			}
			first, err := portable.Open(t.Context(), options)
			if err != nil {
				t.Fatal(err)
			}
			s.state = first
			if _, err := s.enqueueThumbnail(t.Context(), binding, item); err != nil {
				t.Fatal(err)
			}
			second, err := portable.Open(t.Context(), options)
			if err != nil {
				t.Fatal(err)
			}
			s.state = second
			claim, found, err := s.claimThumbnail(t.Context())
			if err != nil || !found {
				t.Fatalf("reopened queue: %v %v", found, err)
			}
			clock.Advance(time.Minute)
			third, err := portable.Open(t.Context(), options)
			if err != nil {
				t.Fatal(err)
			}
			s.state = third
			recovered, found, err := s.claimThumbnail(t.Context())
			if err != nil || !found || recovered.job.Epoch != claim.job.Epoch+1 {
				t.Fatalf("reopened expired claim: %+v %v %v", recovered, found, err)
			}
			if err := s.finishThumbnail(t.Context(), recovered, ItemResult{State: StateReady}, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestThumbnailQueueLostAdmissionAndClaimAcknowledgementsRecover(t *testing.T) {
	s, binding, item, clock := queueTestService(t)
	base := s.state
	lost := true
	s.state = queueFaultStore{AtomicStore: base, mutate: func(ctx context.Context, m state.Mutation) (state.MutationOutcome, error) {
		out, err := base.Mutate(ctx, m)
		if err == nil && lost {
			lost = false
			return out, domain.ErrUnavailable
		}
		return out, err
	}}
	if _, err := s.enqueueThumbnail(t.Context(), binding, item); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("lost admission: %v", err)
	}
	if got, err := s.enqueueThumbnail(t.Context(), binding, item); err != nil || got != StateQueued {
		t.Fatalf("replay admission: %s %v", got, err)
	}
	lost = true
	if _, _, err := s.claimThumbnail(t.Context()); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("lost claim: %v", err)
	}
	clock.Advance(time.Minute)
	claim, found, err := s.claimThumbnail(t.Context())
	if err != nil || !found || claim.job.Epoch != 2 {
		t.Fatalf("recover lost claim: %+v %v %v", claim, found, err)
	}
}

func TestThumbnailQueueCompletionRetriesConcurrentPriorityRefresh(t *testing.T) {
	s, binding, item, clock := queueTestService(t)
	if _, err := s.enqueueThumbnail(t.Context(), binding, item); err != nil {
		t.Fatal(err)
	}
	claim, found, err := s.claimThumbnail(t.Context())
	if err != nil || !found {
		t.Fatal(err)
	}
	base := s.state
	refresh := true
	s.state = queueFaultStore{AtomicStore: base, mutate: func(ctx context.Context, m state.Mutation) (state.MutationOutcome, error) {
		if strings.HasPrefix(m.ID, "thumbnail-finish") && refresh {
			refresh = false
			clock.Advance(time.Second)
			if _, err := s.enqueueThumbnail(ctx, binding, item); err != nil {
				return state.MutationOutcome{}, err
			}
		}
		return base.Mutate(ctx, m)
	}}
	if err := s.finishThumbnail(t.Context(), claim, ItemResult{State: StateReady}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.loadThumbnail(t.Context(), claim.job.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("completion lost after refresh: %v", err)
	}
}

func TestThumbnailQueueRepeatedCrashIsTerminalAndExpires(t *testing.T) {
	s, binding, item, clock := queueTestService(t)
	if _, err := s.enqueueThumbnail(t.Context(), binding, item); err != nil {
		t.Fatal(err)
	}
	for range queueAttempts {
		if _, found, err := s.claimThumbnail(t.Context()); err != nil || !found {
			t.Fatalf("claim: %v %v", found, err)
		}
		clock.Advance(time.Minute)
	}
	if _, found, err := s.claimThumbnail(t.Context()); err != nil || found {
		t.Fatalf("crash attempts unbounded: %v %v", found, err)
	}
	if removed, err := s.cleanupThumbnail(t.Context()); err != nil || removed {
		t.Fatalf("failure removed before retention: %v %v", removed, err)
	}
	clock.Advance(s.options.OperationRetention)
	if removed, err := s.cleanupThumbnail(t.Context()); err != nil || !removed {
		t.Fatalf("failure did not expire: %v %v", removed, err)
	}
	if _, _, err := s.loadThumbnail(t.Context(), thumbnailID(binding)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestThumbnailQueueRefreshAfterFailureRetention(t *testing.T) {
	s, binding, item, clock := queueTestService(t)
	if _, err := s.enqueueThumbnail(t.Context(), binding, item); err != nil {
		t.Fatal(err)
	}
	claim, found, err := s.claimThumbnail(t.Context())
	if err != nil || !found {
		t.Fatal(err)
	}
	if err := s.finishThumbnail(t.Context(), claim, ItemResult{State: StateFailed}, nil); err != nil {
		t.Fatal(err)
	}
	clock.Advance(s.options.OperationRetention)
	if got, err := s.enqueueThumbnail(t.Context(), binding, item); err != nil || got != StateQueued {
		t.Fatalf("expired failure did not accept fresh request: %s %v", got, err)
	}
	if _, found, err := s.claimThumbnail(t.Context()); err != nil || !found {
		t.Fatalf("refreshed claim: %v %v", found, err)
	}
}

func TestThumbnailQueueLeaseRecoveryDoesNotRequireAnotherRequest(t *testing.T) {
	first, binding, item, clock := queueTestService(t)
	if _, err := first.enqueueThumbnail(t.Context(), binding, item); err != nil {
		t.Fatal(err)
	}
	claim, found, err := first.claimThumbnail(t.Context())
	if err != nil || !found {
		t.Fatalf("first claim: %v %v", found, err)
	}
	// Priority refresh preserves the original worker's exact lease.
	clock.Advance(30 * time.Second)
	if _, err := first.enqueueThumbnail(t.Context(), binding, item); err != nil {
		t.Fatal(err)
	}
	refreshed, _, err := first.loadThumbnail(t.Context(), claim.job.ID)
	if err != nil || refreshed.LeaseExpiresAt != claim.job.LeaseExpiresAt || refreshed.Epoch != claim.job.Epoch {
		t.Fatalf("request extended a lease: %+v %v", refreshed, err)
	}
	restarted := &Service{state: first.state, clock: first.clock, ids: first.ids, options: first.options}
	if _, found, err := restarted.claimThumbnail(t.Context()); err != nil || found {
		t.Fatalf("live claim stolen: %v %v", found, err)
	}
	clock.Advance(30 * time.Second)
	replacement, found, err := restarted.claimThumbnail(t.Context())
	if err != nil || !found || replacement.job.Epoch != claim.job.Epoch+1 {
		t.Fatalf("restart recovery: %+v %v %v", replacement, found, err)
	}
	if err := first.finishThumbnail(t.Context(), claim, ItemResult{State: StateReady}, nil); err != nil {
		t.Fatal(err)
	}
	current, _, err := restarted.loadThumbnail(t.Context(), claim.job.ID)
	if err != nil || current.Epoch != replacement.job.Epoch || current.LeaseID != replacement.job.LeaseID {
		t.Fatalf("stale completion changed new claim: %+v %v", current, err)
	}
	if err := restarted.finishThumbnail(t.Context(), replacement, ItemResult{State: StateReady}, nil); err != nil {
		t.Fatal(err)
	}
	if err := first.finishThumbnail(t.Context(), claim, ItemResult{State: StateReady}, nil); err != nil {
		t.Fatalf("late completion after removal: %v", err)
	}
}

func TestThumbnailQueueTwoClaimsHaveOneWinner(t *testing.T) {
	s, binding, item, _ := queueTestService(t)
	if _, err := s.enqueueThumbnail(t.Context(), binding, item); err != nil {
		t.Fatal(err)
	}
	other := &Service{state: s.state, clock: s.clock, ids: s.ids, options: s.options}
	var group sync.WaitGroup
	results := make(chan bool, 2)
	failures := make(chan error, 2)
	for _, worker := range []*Service{s, other} {
		group.Go(func() { _, found, err := worker.claimThumbnail(t.Context()); results <- found; failures <- err })
	}
	group.Wait()
	close(results)
	close(failures)
	winners := 0
	for result := range results {
		if result {
			winners++
		}
	}
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("claim winners=%d", winners)
	}
}

func TestThumbnailQueueRetryBackoffAndTerminalFailures(t *testing.T) {
	s, binding, item, clock := queueTestService(t)
	if _, err := s.enqueueThumbnail(t.Context(), binding, item); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= queueAttempts; attempt++ {
		claim, found, err := s.claimThumbnail(t.Context())
		if err != nil || !found {
			t.Fatalf("attempt %d claim: %v %v", attempt, found, err)
		}
		if err := s.finishThumbnail(t.Context(), claim, ItemResult{}, domain.ErrUnavailable); err != nil {
			t.Fatal(err)
		}
		job, _, err := s.loadThumbnail(t.Context(), claim.job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if attempt < queueAttempts {
			if job.State != StateQueued || !job.AvailableAt.After(clock.Now()) {
				t.Fatalf("no retry backoff: %+v", job)
			}
			if _, found, err := s.claimThumbnail(t.Context()); err != nil || found {
				t.Fatalf("backoff skipped: %v %v", found, err)
			}
		} else if job.State != StateFailed {
			t.Fatalf("unbounded retries: %+v", job)
		}
		clock.Advance(time.Minute)
	}
	if got, err := s.enqueueThumbnail(t.Context(), binding, item); err != nil || got != StateFailed {
		t.Fatalf("failed job silently restarted: %s %v", got, err)
	}
	for _, kind := range []string{"recent", "oldest"} {
		page, err := s.state.List(t.Context(), state.MustPrefix(state.NamespaceOperations, "admin", "thumbnail-"+kind), state.PageRequest{Limit: 64})
		if err != nil || len(page.Items) != 0 {
			t.Fatalf("terminal job blocks %s: %v %v", kind, len(page.Items), err)
		}
	}
}

func TestThumbnailQueueIdentityIncludesTenantAndVariant(t *testing.T) {
	s, binding, item, _ := queueTestService(t)
	for index := range 3 {
		b, i := binding, item
		if index == 1 {
			var err error
			b.Owner, err = domain.ParseUserID("RERERERERERERERERERERA")
			if err != nil {
				t.Fatal(err)
			}
		}
		if index == 2 {
			b.Variant = 512
			i.Variant = 512
		}
		if _, err := s.enqueueThumbnail(t.Context(), b, i); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.state.List(t.Context(), state.MustPrefix(state.NamespaceOperations, "admin", "thumbnail-job"), state.PageRequest{Limit: 64})
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("tenant/variant collision: %d %v", len(page.Items), err)
	}
}

func TestThumbnailQueueDeniesCorruptRecordsAndCanceledClaims(t *testing.T) {
	s, binding, item, _ := queueTestService(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := s.claimThumbnail(ctx); err == nil {
		t.Fatal("canceled worker claimed work")
	}
	key := thumbnailKey("job", thumbnailID(binding))
	if _, err := s.state.Create(t.Context(), key, []byte(`{"schemaVersion":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.enqueueThumbnail(t.Context(), binding, item); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid state accepted: %v", err)
	}
}

func TestThumbnailBatchAdmissionUsesOnePublicationPerRefresh(t *testing.T) {
	s, binding, item, clock := queueTestService(t)
	base := s.state
	publications := 0
	s.state = queueFaultStore{AtomicStore: base, mutate: func(ctx context.Context, m state.Mutation) (state.MutationOutcome, error) {
		publications++
		return base.Mutate(ctx, m)
	}}
	requests := make([]thumbnailRequest, 64)
	for i := range requests {
		b, entry := binding, item
		b.ContentID = domain.ContentID(fmt.Sprintf("content-%d", i))
		entry.Path = domain.MustParseUserPath(fmt.Sprintf("/photo-%d.png", i))
		requests[i] = thumbnailRequest{binding: b, item: entry}
	}
	for range 100 {
		states, err := s.enqueueThumbnails(t.Context(), requests)
		if err != nil || len(states) != 64 {
			t.Fatalf("batch admission: %d %v", len(states), err)
		}
		clock.Advance(time.Millisecond)
	}
	page, err := base.List(t.Context(), state.MustPrefix(state.NamespaceOperations, "admin", "thumbnail-job"), state.PageRequest{Limit: 1000})
	if err != nil || len(page.Items) != 64 || publications != 100 {
		t.Fatalf("100 batches: jobs=%d publications=%d err=%v", len(page.Items), publications, err)
	}
}
