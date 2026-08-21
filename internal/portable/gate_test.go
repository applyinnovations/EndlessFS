package portable_test

import (
	"bytes"
	"context"
	"errors"
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

func TestReplicaCompatibilityRejectsWriterConfigurationDrift(t *testing.T) {
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2036, 2, 4, 4, 5, 6, 0, time.UTC))
	_ = openEngine(t, backend, clock, 15, nil)
	tests := []portable.WriterConfiguration{
		{WriterSetID: "different-writer", ConfigurationDigest: "config-v1", KeyringIdentifiers: []string{"session-v1"}},
		{WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "different-config", KeyringIdentifiers: []string{"session-v1"}},
		{WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1", KeyringIdentifiers: []string{"different-keyring"}},
		{WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1", KeyringIdentifiers: []string{"session-v1"}, RequiredFeatures: []string{"future-feature"}},
	}
	for index, writer := range tests {
		_, err := portable.Open(context.Background(), portable.Options{
			Backend: backend, Clock: clock, IDs: domain.NewIDGenerator(bytes.NewReader(deterministic(byte(60+index), 1<<20))),
			Writer: writer, LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x63}, 32),
		})
		if !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Errorf("configuration %d error = %v", index, err)
		}
	}
}

func TestAdmissionRejectsWriteGateFeatureBindingDrift(t *testing.T) {
	for name, features := range map[string][]string{
		"missing":   nil,
		"different": {"different-feature"},
	} {
		t.Run(name, func(t *testing.T) {
			backend := objectmemory.New()
			clock := domain.NewFixedClock(time.Date(2036, 2, 5, 4, 5, 6, 0, time.UTC))
			engine := openEngine(t, backend, clock, 19, nil)
			key := storageformat.WriteGateKey()
			object, err := backend.Get(context.Background(), key)
			if err != nil {
				t.Fatal(err)
			}
			var envelope storageformat.Envelope
			var gate storageformat.WriteGate
			if err := storageformat.DecodeEnvelope(object.Body, key, "write-gate-v1", &envelope, &gate); err != nil {
				t.Fatal(err)
			}
			gate.WriterFeatures = features
			body, err := storageformat.EncodeEnvelope("write-gate-v1", key, envelope.Revision+1, gate)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := backend.Put(context.Background(), key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.Create(context.Background(), state.MustKey(state.NamespaceAccounts, "blocked"), []byte("value")); !errors.Is(err, domain.ErrPreconditionFailed) {
				t.Fatalf("Create() error = %v", err)
			}
		})
	}
}

func TestMigrationCurrentSchemaStartupRejectsLegacyUnboundGate(t *testing.T) {
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2036, 2, 6, 4, 5, 6, 0, time.UTC))
	_ = openEngine(t, backend, clock, 20, nil)
	key := storageformat.WriteGateKey()
	object, err := backend.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var envelope storageformat.Envelope
	var gate storageformat.WriteGate
	if err := storageformat.DecodeEnvelope(object.Body, key, "write-gate-v1", &envelope, &gate); err != nil {
		t.Fatal(err)
	}
	gate.WriterFeatures = nil
	body, err := storageformat.EncodeEnvelope("write-gate-v1", key, envelope.Revision+1, gate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(context.Background(), key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
		t.Fatal(err)
	}
	_, err = portable.Open(context.Background(), portable.Options{
		Backend: backend, Clock: clock,
		IDs: domain.NewIDGenerator(bytes.NewReader(deterministic(21, 1<<20))),
		Writer: portable.WriterConfiguration{
			WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1",
			KeyringIdentifiers: []string{"session-v1"},
		},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x63}, 32),
	})
	if !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("current-schema startup with legacy unbound gate error = %v; want precondition failed", err)
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
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x63}, 32), Scheduler: scheduler,
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}
