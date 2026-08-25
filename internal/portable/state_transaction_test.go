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
