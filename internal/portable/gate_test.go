package portable_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestReplicaDropAfterAdmissionIsFencedRecoveredAndClosed(t *testing.T) {
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2036, 2, 3, 4, 5, 6, 0, time.UTC))
	crasher := &stepFailure{step: portable.StepStateAfterAdmitted}
	first := openEngine(t, backend, clock, 11, crasher)
	second := openEngine(t, backend, clock, 12, nil)
	key := state.MustKey(state.NamespaceAccounts, "crash-recovery")
	if _, err := first.Create(context.Background(), key, []byte("intended")); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("crashed Create() error = %v", err)
	}
	if _, err := second.Get(context.Background(), key); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("mutation became visible before recovery: %v", err)
	}
	clock.Advance(2 * time.Minute)
	if _, err := second.CreateCheckpoint(context.Background(), "checkpoint-1"); err != nil {
		t.Fatalf("CreateCheckpoint() error = %v", err)
	}
	value, err := second.Get(context.Background(), key)
	if err != nil || string(value.Data) != "intended" {
		t.Fatalf("recovered Get() = %+v, %v", value, err)
	}
	status, err := second.GateStatus(context.Background())
	if err != nil || status.Mode != storageformat.GateClosed {
		t.Fatalf("GateStatus() = %+v, %v", status, err)
	}
	if _, err := first.Create(context.Background(), state.MustKey(state.NamespaceAccounts, "blocked"), []byte("x")); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("post-close Create() error = %v", err)
	}
	if err := second.OpenWrites(context.Background(), "checkpoint-1"); err != nil {
		t.Fatalf("OpenWrites() error = %v", err)
	}
}

func TestCandidateCannotAdmitAfterGateStartsClosing(t *testing.T) {
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2036, 2, 3, 4, 5, 6, 0, time.UTC))
	candidateReached := make(chan struct{})
	releaseCandidate := make(chan struct{})
	scheduler := portable.SchedulerFunc(func(_ context.Context, step string) error {
		if step == portable.StepAdmissionAfterCandidate {
			close(candidateReached)
			<-releaseCandidate
		}
		return nil
	})
	first := openEngine(t, backend, clock, 13, scheduler)
	second := openEngine(t, backend, clock, 14, nil)
	key := state.MustKey(state.NamespaceAccounts, "candidate-race")
	result := make(chan error, 1)
	go func() {
		_, err := first.Create(context.Background(), key, []byte("must-not-commit"))
		result <- err
	}()
	<-candidateReached
	if err := second.CloseWrites(context.Background(), "checkpoint-2"); err != nil {
		t.Fatalf("CloseWrites() error = %v", err)
	}
	close(releaseCandidate)
	if err := <-result; !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("racing Create() error = %v", err)
	}
	if _, err := second.Get(context.Background(), key); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cancelled candidate wrote state: %v", err)
	}
}

type stepFailure struct {
	mu   sync.Mutex
	step string
	done bool
}

func (f *stepFailure) Step(_ context.Context, step string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if step == f.step && !f.done {
		f.done = true
		return domain.NewError(domain.ErrorUnavailable, "injected replica loss")
	}
	return nil
}

func openEngine(t *testing.T, backend *objectmemory.Backend, clock *domain.FixedClock, seed byte, scheduler portable.Scheduler) *portable.Engine {
	t.Helper()
	engine, err := portable.Open(context.Background(), portable.Options{
		Backend: backend, Clock: clock,
		IDs: domain.NewIDGenerator(bytes.NewReader(deterministic(seed, 1<<20))),
		Writer: portable.WriterConfiguration{
			WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1",
			KeyringIdentifiers: []string{"session-v1"},
		},
		LeaseTTL: time.Minute, Scheduler: scheduler,
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}
