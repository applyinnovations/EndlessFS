package portable

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestConsistencyDomainMutationUsesOneReadAndOneConditionalPublication(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
	store := newConsistencyDomainStore(backend, nil)
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner-a"}
	mutation := consistencyDomainMutation{ID: "mutation-a", Result: []byte(`{"ok":true}`), Changes: []consistencyDomainChange{{Key: "preferences/theme", Require: domainValueAbsent, Value: []byte(`{"theme":"dark"}`)}}}
	if err := store.ensureRegistered(ctx, reference); err != nil {
		t.Fatal(err)
	}
	ledger.Reset()

	outcome, err := store.mutate(ctx, reference, mutation)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Revision != 1 || outcome.MutationID != mutation.ID || outcome.Replayed {
		t.Fatalf("outcome = %+v", outcome)
	}
	events := ledger.Events()
	if len(events) != 2 || events[0].Kind != providerbudget.RequestObjectGet || events[1].Kind != providerbudget.RequestObjectPut {
		t.Fatalf("mutation requests = %+v, want head GET and conditional head PUT", events)
	}

	ledger.Reset()
	value, err := store.get(ctx, reference, "preferences/theme")
	if err != nil {
		t.Fatal(err)
	}
	if string(value.Data) != `{"theme":"dark"}` || value.LogicalVersion == "" || value.Revision != 1 {
		t.Fatalf("value = %+v", value)
	}
	if events := ledger.Events(); len(events) != 1 || events[0].Kind != providerbudget.RequestObjectGet {
		t.Fatalf("inline read requests = %+v", events)
	}

	replay, err := store.mutate(ctx, reference, mutation)
	if err != nil || !replay.Replayed || replay.Revision != outcome.Revision {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
}

func TestConsistencyDomainLogicalVersionsAreDerivedAndStableAcrossRetry(t *testing.T) {
	ctx := context.Background()
	store := newConsistencyDomainStore(objectmemory.New(), nil)
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner-versions"}
	mutation := consistencyDomainMutation{ID: "create", Changes: []consistencyDomainChange{{Key: "record", Require: domainValueAbsent, Value: []byte("same")}}}
	if _, err := store.mutate(ctx, reference, mutation); err != nil {
		t.Fatal(err)
	}
	first, err := store.get(ctx, reference, "record")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.mutate(ctx, reference, mutation); err != nil {
		t.Fatal(err)
	}
	replayed, err := store.get(ctx, reference, "record")
	if err != nil || replayed.LogicalVersion != first.LogicalVersion {
		t.Fatalf("retry changed derived version: first=%+v retry=%+v err=%v", first, replayed, err)
	}
	if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: "replace", Changes: []consistencyDomainChange{{Key: "record", Require: domainValuePresent, ExpectedVersion: first.LogicalVersion, Value: []byte("same")}}}); err != nil {
		t.Fatal(err)
	}
	second, err := store.get(ctx, reference, "record")
	if err != nil || second.LogicalVersion == first.LogicalVersion {
		t.Fatalf("distinct committed intent reused logical version: first=%+v second=%+v err=%v", first, second, err)
	}
}

func TestConsistencyDomainDeniedPreconditionCreatesNoDurableGarbage(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	base := objectmemory.New()
	store := newConsistencyDomainStore(budgettest.Wrap(providerbudget.RoleState, base, ledger), nil)
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner-denial"}
	if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: "missing", Changes: []consistencyDomainChange{{Key: "record", Require: domainValuePresent, Value: []byte("value")}}}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing precondition error = %v", err)
	}
	if events := ledger.Events(); len(events) != 1 || events[0].Kind != providerbudget.RequestObjectGet {
		t.Fatalf("denied mutation requests = %+v, want one read and no writes", events)
	}
	if objects := base.Export(); len(objects) != 0 {
		t.Fatalf("denied mutation left durable objects: %+v", objects)
	}
}

func TestConsistencyDomainSameMutationConcurrentCallersObserveOneOutcome(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	reference := consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "owner-concurrent-retry"}
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	scheduler := SchedulerFunc(func(_ context.Context, step string) error {
		if step == "consistency-domain:before-head-commit" {
			ready <- struct{}{}
			<-release
		}
		return nil
	})
	mutation := consistencyDomainMutation{ID: "same", Result: []byte("committed"), Changes: []consistencyDomainChange{{Key: "edge", Require: domainValueAbsent, Value: []byte("node")}}}
	results := make(chan struct {
		outcome consistencyDomainOutcome
		err     error
	}, 2)
	for range 2 {
		go func() {
			outcome, err := newConsistencyDomainStore(backend, scheduler).mutate(ctx, reference, mutation)
			results <- struct {
				outcome consistencyDomainOutcome
				err     error
			}{outcome, err}
		}()
	}
	<-ready
	<-ready
	close(release)
	var replayed int
	for range 2 {
		result := <-results
		if result.err != nil || result.outcome.Revision != 1 || string(result.outcome.Result) != "committed" {
			t.Fatalf("concurrent result = %+v, %v", result.outcome, result.err)
		}
		if result.outcome.Replayed {
			replayed++
		}
	}
	if replayed != 1 {
		t.Fatalf("replayed outcomes = %d, want exactly one losing caller reconciled", replayed)
	}
}

func TestConsistencyDomainLostHeadCommitResponseRecoversInSameCall(t *testing.T) {
	ctx := context.Background()
	base := objectmemory.New()
	reference := consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "owner-lost-success"}
	backend := &loseSuccessfulHeadPutBackend{Backend: base, target: storageformat.DomainHeadKey(reference.Kind, reference.ID)}
	store := newConsistencyDomainStore(backend, nil)
	if err := store.ensureRegistered(ctx, reference); err != nil {
		t.Fatal(err)
	}
	backend.armed = true
	mutation := consistencyDomainMutation{ID: "move", Result: []byte("done"), Changes: []consistencyDomainChange{{Key: "edge", Require: domainValueAbsent, Value: []byte("node")}}}

	outcome, err := store.mutate(ctx, reference, mutation)
	if err != nil || !outcome.Replayed || outcome.Revision != 1 || string(outcome.Result) != "done" {
		t.Fatalf("lost-success outcome = %+v, %v", outcome, err)
	}
	value, err := store.get(ctx, reference, "edge")
	if err != nil || string(value.Data) != "node" {
		t.Fatalf("committed value = %+v, %v", value, err)
	}
}

func TestConsistencyDomainCompactionPersistsValuesAndOutcomesWithoutClaimReads(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2054, 2, 3, 4, 5, 6, 0, time.UTC))
	ledger := providerbudget.NewLedger()
	base := objectmemory.New()
	store := newConsistencyDomainStore(budgettest.Wrap(providerbudget.RoleState, base, ledger), nil, clock)
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner-wide"}
	mutations := make([]consistencyDomainMutation, 300)
	for index := range mutations {
		key := domainTestKey(index)
		mutations[index] = consistencyDomainMutation{ID: "create-" + key, Result: []byte(key), Changes: []consistencyDomainChange{{Key: key, Require: domainValueAbsent, Value: []byte(key)}}}
		if _, err := store.mutate(ctx, reference, mutations[index]); err != nil {
			t.Fatalf("seed %d: %v", index, err)
		}
		// Keep expiry keys distinct without making the exact economics fixture
		// depend on the host wall clock's precision.
		clock.Advance(time.Microsecond)
	}
	ledger.Reset()
	if err := store.compact(ctx, reference); err != nil {
		t.Fatal(err)
	}
	economics, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	ratchet, err := gcs.RegionalStandardFlatBudgetRatchet()
	if err != nil {
		t.Fatal(err)
	}
	if report, err := ratchet.CheckExact("maintenance-domain-compaction-300-schema-009", economics, []providerbudget.Role{providerbudget.RoleState}, ledger.Events()); err != nil {
		t.Errorf("domain compaction provider budget: %v; observed=%+v", err, report.Totals)
	}
	snapshot, err := store.loadHead(ctx, reference)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.head.Base.EntryCount != 300 || snapshot.head.Outcomes.EntryCount != 300 || len(snapshot.head.Deltas) != 0 {
		t.Fatalf("compacted head = %+v", snapshot.head)
	}
	for _, event := range ledger.Events() {
		if event.Kind == providerbudget.RequestObjectList || event.Kind == providerbudget.RequestObjectDelete || bytes.Contains([]byte(event.Target), []byte("/claims/")) {
			t.Fatalf("compaction performed forbidden discovery/claim work: %+v", event)
		}
	}
	replayed, err := store.mutate(ctx, reference, mutations[299])
	if err != nil || !replayed.Replayed || replayed.Revision != 300 || string(replayed.Result) != domainTestKey(299) {
		t.Fatalf("compacted replay = %+v, %v", replayed, err)
	}
	value, err := store.get(ctx, reference, domainTestKey(299))
	if err != nil || string(value.Data) != domainTestKey(299) {
		t.Fatalf("compacted value = %+v, %v", value, err)
	}
}

func TestConsistencyDomainOutcomeRetentionUsesBoundedExpiryIndex(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2054, 1, 2, 3, 4, 5, 0, time.UTC))
	store := newConsistencyDomainStore(objectmemory.New(), nil, clock)
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner-retention"}
	mutation := consistencyDomainMutation{ID: "retained", Changes: []consistencyDomainChange{{Key: "record", Require: domainValueAbsent, Value: []byte("first")}}}
	first, err := store.mutate(ctx, reference, mutation)
	if err != nil {
		t.Fatal(err)
	}
	if first.RetainUntil != clock.Now().Add(terminalOperationRetention) {
		t.Fatalf("retention = %v, want %v", first.RetainUntil, clock.Now().Add(terminalOperationRetention))
	}
	if err := store.compact(ctx, reference); err != nil {
		t.Fatal(err)
	}
	compacted, err := store.loadHead(ctx, reference)
	if err != nil || compacted.head.Outcomes.EntryCount != 1 || compacted.head.OutcomeExpiry.EntryCount != 1 {
		t.Fatalf("compacted retention roots = %+v, %v", compacted.head, err)
	}
	clock.Advance(terminalOperationRetention - time.Second)
	if replay, err := store.mutate(ctx, reference, mutation); err != nil || !replay.Replayed {
		t.Fatalf("retained replay = %+v, %v", replay, err)
	}
	clock.Advance(2 * time.Second)
	if _, found, err := store.lookupOutcomeAtHead(ctx, reference, compacted.head, mutation.ID); err != nil || found {
		t.Fatalf("expired lookup found=%v error=%v", found, err)
	}
	if err := store.compact(ctx, reference); err != nil {
		t.Fatal(err)
	}
	pruned, err := store.loadHead(ctx, reference)
	if err != nil || pruned.head.Outcomes.Digest != "" || pruned.head.OutcomeExpiry.Digest != "" {
		t.Fatalf("pruned retention roots = %+v, %v", pruned.head, err)
	}
	value, err := store.get(ctx, reference, "record")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: mutation.ID, Changes: []consistencyDomainChange{{Key: "record", Require: domainValuePresent, ExpectedVersion: value.LogicalVersion, Value: []byte("second")}}}); err != nil {
		t.Fatalf("reuse after canonical replay retention = %v", err)
	}
}

func TestConsistencyDomainOutcomePruningIsBoundedPerCompaction(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2055, 1, 2, 3, 4, 5, 0, time.UTC))
	store := newConsistencyDomainStore(objectmemory.New(), nil, clock)
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner-retention-bound"}
	for index := 0; index < domainPageMaximumItems+17; index++ {
		key := domainTestKey(index)
		if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: key, Changes: []consistencyDomainChange{{Key: key, Require: domainValueAbsent, Value: []byte(key)}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.compact(ctx, reference); err != nil {
		t.Fatal(err)
	}
	clock.Advance(terminalOperationRetention + time.Second)
	if err := store.compact(ctx, reference); err != nil {
		t.Fatal(err)
	}
	first, err := store.loadHead(ctx, reference)
	if err != nil || first.head.Outcomes.EntryCount != 17 || first.head.OutcomeExpiry.EntryCount != 17 {
		t.Fatalf("first bounded prune = %+v, %v", first.head, err)
	}
	if err := store.compact(ctx, reference); err != nil {
		t.Fatal(err)
	}
	second, err := store.loadHead(ctx, reference)
	if err != nil || second.head.Outcomes.Digest != "" || second.head.OutcomeExpiry.Digest != "" {
		t.Fatalf("second bounded prune = %+v, %v", second.head, err)
	}
}

func TestConsistencyDomainAutomaticallyCompactsBeforeHeadSaturation(t *testing.T) {
	ctx := context.Background()
	store := newConsistencyDomainStore(objectmemory.New(), nil)
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner-saturation"}
	for index := 0; index < 10; index++ {
		value := bytes.Repeat([]byte{byte('a' + index)}, 180*1024)
		if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: domainTestKey(index), Changes: []consistencyDomainChange{{Key: domainTestKey(index), Require: domainValueAbsent, Value: value}}}); err != nil {
			t.Fatalf("mutation %d failed at bounded delta rollover: %v", index, err)
		}
	}
	snapshot, err := store.loadHead(ctx, reference)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.head.BaseRevision == 0 || snapshot.head.Base.EntryCount == 0 {
		t.Fatalf("head never compacted before saturation: %+v", snapshot.head)
	}
	for index := 0; index < 10; index++ {
		value, err := store.get(ctx, reference, domainTestKey(index))
		if err != nil || len(value.Data) != 180*1024 {
			t.Fatalf("value %d after rollover = %d bytes, %v", index, len(value.Data), err)
		}
	}
}

func TestConsistencyDomainFrozenHeadDeniesMutationAndCompaction(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	store := newConsistencyDomainStore(budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger), nil)
	reference := consistencyDomainRef{Kind: storageformat.DomainAdmin, ID: "global"}
	if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: "seed", Changes: []consistencyDomainChange{{Key: "roles", Require: domainValueAbsent, Value: []byte("admin")}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.freeze(ctx, reference, 7); err != nil {
		t.Fatal(err)
	}
	before, err := store.loadHead(ctx, reference)
	if err != nil {
		t.Fatal(err)
	}
	ledger.Reset()
	if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: "denied", Changes: []consistencyDomainChange{{Key: "roles", Require: domainValueAny, Value: []byte("other")}}}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("frozen mutation error = %v", err)
	}
	if err := store.compact(ctx, reference); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("frozen compaction error = %v", err)
	}
	after, err := store.loadHead(ctx, reference)
	if err != nil {
		t.Fatal(err)
	}
	if before.object.Version != after.object.Version || before.head.Revision != after.head.Revision || before.head.Base != after.head.Base {
		t.Fatalf("frozen maintenance changed authority: before=%+v after=%+v", before.head, after.head)
	}
	for _, event := range ledger.Events() {
		if event.Kind == providerbudget.RequestObjectPut || event.Kind == providerbudget.RequestObjectDelete {
			t.Fatalf("frozen operation wrote provider state: %+v", event)
		}
	}
}

func TestConsistencyDomainFreezeWinsAgainstPausedStaleWriter(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	store := newConsistencyDomainStore(backend, nil)
	reference := consistencyDomainRef{Kind: storageformat.DomainAdmin, ID: "global-freeze-race"}
	if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: "seed", Changes: []consistencyDomainChange{{Key: "roles", Require: domainValueAbsent, Value: []byte("admin")}}}); err != nil {
		t.Fatal(err)
	}
	seed, err := store.get(ctx, reference, "roles")
	if err != nil {
		t.Fatal(err)
	}
	paused := make(chan struct{})
	release := make(chan struct{})
	writer := newConsistencyDomainStore(backend, SchedulerFunc(func(_ context.Context, step string) error {
		if step == "consistency-domain:before-head-commit" {
			close(paused)
			<-release
		}
		return nil
	}))
	result := make(chan error, 1)
	go func() {
		_, err := writer.mutate(ctx, reference, consistencyDomainMutation{ID: "stale", Changes: []consistencyDomainChange{{Key: "roles", Require: domainValuePresent, ExpectedVersion: seed.LogicalVersion, Value: []byte("changed")}}})
		result <- err
	}()
	<-paused
	if err := store.freeze(ctx, reference, 9); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale writer error = %v", err)
	}
	value, err := store.get(ctx, reference, "roles")
	if err != nil || string(value.Data) != "admin" {
		t.Fatalf("value after freeze race = %+v, %v", value, err)
	}
}

func TestConsistencyDomainFreezeRetriesConditionalConflict(t *testing.T) {
	ctx := context.Background()
	base := objectmemory.New()
	reference := consistencyDomainRef{Kind: storageformat.DomainAdmin, ID: "global-freeze-retry"}
	store := newConsistencyDomainStore(base, nil)
	if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: "seed", Changes: []consistencyDomainChange{{Key: "roles", Require: domainValueAbsent, Value: []byte("admin")}}}); err != nil {
		t.Fatal(err)
	}
	backend := &rejectHeadPutOnceBackend{Backend: base, target: storageformat.DomainHeadKey(reference.Kind, reference.ID), armed: true}
	if err := newConsistencyDomainStore(backend, nil).freeze(ctx, reference, 11); err != nil {
		t.Fatalf("freeze did not reconcile conditional conflict: %v", err)
	}
	snapshot, err := store.loadHead(ctx, reference)
	if err != nil || !snapshot.head.Frozen || snapshot.head.FreezeEpoch != 11 {
		t.Fatalf("frozen head = %+v, %v", snapshot.head, err)
	}
}

func TestConsistencyDomainEightReplicaConflictHasOneWinner(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner-eight"}
	store := newConsistencyDomainStore(backend, nil)
	if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: "seed", Changes: []consistencyDomainChange{{Key: "record", Require: domainValueAbsent, Value: []byte("seed")}}}); err != nil {
		t.Fatal(err)
	}
	seed, err := store.get(ctx, reference, "record")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 8)
	for index := 0; index < 8; index++ {
		go func(index int) {
			<-start
			_, err := newConsistencyDomainStore(backend, nil).mutate(ctx, reference, consistencyDomainMutation{ID: domainTestKey(index), Changes: []consistencyDomainChange{{Key: "record", Require: domainValuePresent, ExpectedVersion: seed.LogicalVersion, Value: []byte{byte(index)}}}})
			results <- err
		}(index)
	}
	close(start)
	var success int
	for range 8 {
		if err := <-results; err == nil {
			success++
		} else if !errors.Is(err, domain.ErrPreconditionFailed) && !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("unexpected replica error = %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("successful replicas = %d, want one", success)
	}
}

type loseSuccessfulHeadPutBackend struct {
	objectstore.Backend
	mu     sync.Mutex
	target objectstore.Key
	armed  bool
}

type rejectHeadPutOnceBackend struct {
	objectstore.Backend
	mu     sync.Mutex
	target objectstore.Key
	armed  bool
}

func (backend *rejectHeadPutOnceBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	backend.mu.Lock()
	if backend.armed && key == backend.target && condition.Mode == objectstore.PutMatch {
		backend.armed = false
		backend.mu.Unlock()
		return "", domain.NewError(domain.ErrorConflict, "injected conditional conflict")
	}
	backend.mu.Unlock()
	return backend.Backend.Put(ctx, key, body, condition)
}

func (backend *loseSuccessfulHeadPutBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	version, err := backend.Backend.Put(ctx, key, body, condition)
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if err == nil && backend.armed && key == backend.target {
		backend.armed = false
		return "", errors.New("injected lost successful head response")
	}
	return version, err
}

func domainTestKey(index int) string {
	const digits = "0123456789"
	value := []byte("item-0000")
	for position := len(value) - 1; position >= len("item-"); position-- {
		value[position] = digits[index%10]
		index /= 10
	}
	return string(value)
}
