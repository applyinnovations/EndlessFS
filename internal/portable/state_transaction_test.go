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

func openTransactionalEngine(t *testing.T, backend *objectmemory.Backend, seed byte, scheduler portable.Scheduler) *portable.Engine {
	t.Helper()
	engine, err := portable.Open(context.Background(), portable.Options{
		Backend: backend, Clock: domain.NewFixedClock(time.Date(2049, 1, 2, 3, 4, 5, 0, time.UTC)),
		IDs:      domain.NewIDGenerator(bytes.NewReader(deterministic(seed, 1<<20))),
		Writer:   portable.WriterConfiguration{WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1", KeyringIdentifiers: []string{"session-v1"}},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x63}, 32), Scheduler: scheduler,
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func transactionalFixture(t *testing.T, engine *portable.Engine) (state.Key, state.Key, state.Version, state.Version) {
	t.Helper()
	admin := state.MustKey(state.NamespaceRoles, "admins")
	owner := state.MustKey(state.NamespaceAccounts, "dGVzdC1vd25lci0wMDE")
	adminVersion, err := engine.Create(context.Background(), admin, []byte(`{"value":"old-admin"}`))
	if err != nil {
		t.Fatal(err)
	}
	ownerVersion, err := engine.Create(context.Background(), owner, []byte(`{"value":"old-owner"}`))
	if err != nil {
		t.Fatal(err)
	}
	return admin, owner, adminVersion, ownerVersion
}

func TestPortableCrossDomainTransitionRecoversLostDecisionAndEightReplicaReplay(t *testing.T) {
	backend := objectmemory.New()
	failure := &stepFailure{}
	engine := openTransactionalEngine(t, backend, 201, failure)
	admin, owner, adminVersion, ownerVersion := transactionalFixture(t, engine)
	mutation := state.Mutation{ID: "identity-admin-transition-001", Result: []byte(`{"committed":true}`), Changes: []state.Change{
		{Key: admin, Requirement: state.RequirementPresent, ExpectedVersion: adminVersion, Data: []byte(`{"value":"new-admin"}`)},
		{Key: owner, Requirement: state.RequirementPresent, ExpectedVersion: ownerVersion, Data: []byte(`{"value":"new-owner"}`)},
	}}
	failure.step = portable.StepTransitionAfterDecision
	first, err := engine.Transact(context.Background(), mutation)
	if err != nil || string(first.Result) != `{"committed":true}` {
		t.Fatalf("lost-decision Transact() = %+v, %v", first, err)
	}

	const replicas = 8
	results := make(chan struct {
		outcome state.MutationOutcome
		err     error
	}, replicas)
	var start sync.WaitGroup
	start.Add(1)
	for index := 0; index < replicas; index++ {
		replica := openTransactionalEngine(t, backend, byte(202+index), nil)
		go func() {
			start.Wait()
			outcome, err := replica.Transact(context.Background(), mutation)
			results <- struct {
				outcome state.MutationOutcome
				err     error
			}{outcome, err}
		}()
	}
	start.Done()
	for range replicas {
		result := <-results
		if result.err != nil || result.outcome.ID != mutation.ID || !result.outcome.Replayed {
			t.Fatalf("replica replay = %+v, %v", result.outcome, result.err)
		}
	}
	adminValue, err := engine.Get(context.Background(), admin)
	if err != nil || string(adminValue.Data) != `{"value":"new-admin"}` {
		t.Fatalf("admin value = %s, %v", adminValue.Data, err)
	}
	ownerValue, err := engine.Get(context.Background(), owner)
	if err != nil || string(ownerValue.Data) != `{"value":"new-owner"}` {
		t.Fatalf("owner value = %s, %v", ownerValue.Data, err)
	}

	conflict := mutation
	conflict.Changes = append([]state.Change(nil), mutation.Changes...)
	conflict.Changes[0].Data = []byte(`{"value":"conflict"}`)
	if _, err := engine.Transact(context.Background(), conflict); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("transition ID reuse error = %v", err)
	}
}

func TestPortableCrossDomainTransitionRestartsAfterEveryDurableBoundary(t *testing.T) {
	steps := []string{
		portable.StepTransitionAfterPlan,
		portable.StepTransitionAfterParticipantPrepared,
		portable.StepTransitionBeforeDecision,
		portable.StepTransitionAfterDecision,
		portable.StepTransitionAfterParticipantFinalized,
	}
	for index, step := range steps {
		t.Run(step, func(t *testing.T) {
			backend := objectmemory.New()
			failure := &stepFailure{step: step}
			engine := openTransactionalEngine(t, backend, byte(220+index), failure)
			admin, owner, adminVersion, ownerVersion := transactionalFixture(t, engine)
			mutation := state.Mutation{ID: "restart-transition-" + string(rune('a'+index)), Changes: []state.Change{
				{Key: admin, Requirement: state.RequirementPresent, ExpectedVersion: adminVersion, Data: []byte(`{"value":"new-admin"}`)},
				{Key: owner, Requirement: state.RequirementPresent, ExpectedVersion: ownerVersion, Data: []byte(`{"value":"new-owner"}`)},
			}}
			_, firstErr := engine.Transact(context.Background(), mutation)
			// A lost response after the decision may be recovered in the same
			// call. Every earlier/later injected process boundary may return a
			// retryable error, but the restarted replica must converge either way.
			if firstErr != nil && !errors.Is(firstErr, domain.ErrUnavailable) {
				t.Fatalf("first transition error = %v", firstErr)
			}
			restarted := openTransactionalEngine(t, backend, byte(230+index), nil)
			outcome, err := restarted.Transact(context.Background(), mutation)
			if err != nil || outcome.ID != mutation.ID {
				t.Fatalf("restarted transition = %+v, %v", outcome, err)
			}
			for key, want := range map[state.Key]string{admin: `{"value":"new-admin"}`, owner: `{"value":"new-owner"}`} {
				value, err := restarted.Get(context.Background(), key)
				if err != nil || string(value.Data) != want {
					t.Fatalf("Get(%s) = %s, %v; want %s", key.String(), value.Data, err, want)
				}
			}
		})
	}
}

func TestPortableCrossDomainTransitionAbortsWithoutPartialState(t *testing.T) {
	backend := objectmemory.New()
	engine := openTransactionalEngine(t, backend, 241, nil)
	admin, owner, adminVersion, ownerVersion := transactionalFixture(t, engine)
	if _, err := engine.CompareAndSwap(context.Background(), owner, ownerVersion, []byte(`{"value":"raced-owner"}`)); err != nil {
		t.Fatal(err)
	}
	mutation := state.Mutation{ID: "aborted-transition-001", Changes: []state.Change{
		{Key: admin, Requirement: state.RequirementPresent, ExpectedVersion: adminVersion, Data: []byte(`{"value":"must-not-publish"}`)},
		{Key: owner, Requirement: state.RequirementPresent, ExpectedVersion: ownerVersion, Data: []byte(`{"value":"must-not-publish"}`)},
	}}
	if _, err := engine.Transact(context.Background(), mutation); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale transition error = %v", err)
	}
	adminValue, _ := engine.Get(context.Background(), admin)
	ownerValue, _ := engine.Get(context.Background(), owner)
	if string(adminValue.Data) != `{"value":"old-admin"}` || string(ownerValue.Data) != `{"value":"raced-owner"}` {
		t.Fatalf("aborted transition was partial: admin=%s owner=%s", adminValue.Data, ownerValue.Data)
	}
	// An aborted lock cannot strand either domain.
	if _, err := engine.CompareAndSwap(context.Background(), admin, adminValue.Version, []byte(`{"value":"after-abort"}`)); err != nil {
		t.Fatalf("admin domain remained locked: %v", err)
	}
}

func TestCheckpointHelpsPendingTransitionBeforeFreezingDomains(t *testing.T) {
	backend := objectmemory.New()
	failure := &stepFailure{step: portable.StepTransitionAfterParticipantPrepared}
	engine := openTransactionalEngine(t, backend, 242, failure)
	admin, owner, adminVersion, ownerVersion := transactionalFixture(t, engine)
	mutation := state.Mutation{ID: "checkpoint-transition-001", Result: []byte(`{"checkpoint":true}`), Changes: []state.Change{
		{Key: admin, Requirement: state.RequirementPresent, ExpectedVersion: adminVersion, Data: []byte(`{"value":"new-admin"}`)},
		{Key: owner, Requirement: state.RequirementPresent, ExpectedVersion: ownerVersion, Data: []byte(`{"value":"new-owner"}`)},
	}}
	if _, err := engine.Transact(context.Background(), mutation); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("interrupted transition error = %v", err)
	}
	checkpoint, err := engine.CreateCheckpoint(context.Background(), "pending-transition-checkpoint")
	if err != nil {
		t.Fatalf("checkpoint did not help pending transition: %v", err)
	}
	if err := engine.VerifyCheckpoint(context.Background(), checkpoint.CheckpointID); err != nil {
		t.Fatal(err)
	}
	if err := engine.OpenWrites(context.Background(), checkpoint.CheckpointID); err != nil {
		t.Fatal(err)
	}
	replayed, err := engine.Transact(context.Background(), mutation)
	if err != nil || !replayed.Replayed || string(replayed.Result) != `{"checkpoint":true}` {
		t.Fatalf("transition replay after checkpoint = %+v, %v", replayed, err)
	}
}

func TestCheckpointCollectsExpiredFinalizedTransitionJournals(t *testing.T) {
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2050, 3, 4, 5, 6, 7, 0, time.UTC))
	engine, err := portable.Open(context.Background(), portable.Options{
		Backend: backend, Clock: clock, IDs: domain.NewIDGenerator(bytes.NewReader(deterministic(243, 1<<20))),
		Writer:   portable.WriterConfiguration{WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1", KeyringIdentifiers: []string{"session-v1"}},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x63}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	admin, owner, adminVersion, ownerVersion := transactionalFixture(t, engine)
	mutation := state.Mutation{ID: "expired-transition-001", RetainUntil: clock.Now().Add(time.Minute), Changes: []state.Change{
		{Key: admin, Requirement: state.RequirementPresent, ExpectedVersion: adminVersion, Data: []byte(`{"value":"new-admin"}`)},
		{Key: owner, Requirement: state.RequirementPresent, ExpectedVersion: ownerVersion, Data: []byte(`{"value":"new-owner"}`)},
	}}
	if _, err := engine.Transact(context.Background(), mutation); err != nil {
		t.Fatal(err)
	}
	for _, key := range []objectstore.Key{storageformat.TransitionPlanKey(mutation.ID), storageformat.TransitionDecisionKey(mutation.ID)} {
		if _, err := backend.Head(context.Background(), key); err != nil {
			t.Fatalf("transition journal before retention expiry: %v", err)
		}
	}
	clock.Advance(2 * time.Minute)
	if _, err := engine.CreateCheckpoint(context.Background(), "expired-transition-checkpoint"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []objectstore.Key{storageformat.TransitionPlanKey(mutation.ID), storageformat.TransitionDecisionKey(mutation.ID)} {
		if _, err := backend.Head(context.Background(), key); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("expired transition journal %s error = %v; want not found", key, err)
		}
	}
}
