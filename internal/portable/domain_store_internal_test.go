package portable

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestConsistencyDomainMutationPublishesOneConditionalHead(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
	store := newConsistencyDomainStore(backend, nil)
	domainRef := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner-a"}

	outcome, err := store.mutate(ctx, domainRef, consistencyDomainMutation{
		ID: "mutation-a", Result: []byte(`{"ok":true}`),
		Changes: []consistencyDomainChange{{Key: "preferences/theme", Require: domainValueAbsent, Value: []byte(`{"theme":"dark"}`), LogicalVersion: "version-a"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Revision != 1 || outcome.MutationID != "mutation-a" || outcome.Replayed {
		t.Fatalf("outcome = %+v", outcome)
	}
	events := ledger.Events()
	if len(events) != 4 {
		t.Fatalf("mutation provider requests = %d, want head GET + claim PUT + head PUT + claim finalization PUT; events=%+v", len(events), events)
	}
	if events[0].Kind != providerbudget.RequestObjectGet || events[1].Kind != providerbudget.RequestObjectPut || events[2].Kind != providerbudget.RequestObjectPut || events[3].Kind != providerbudget.RequestObjectPut {
		t.Fatalf("mutation request sequence = %+v", events)
	}
	for _, event := range events {
		if event.Role != providerbudget.RoleState {
			t.Fatalf("mutation contacted non-state provider role: %+v", event)
		}
	}

	ledger.Reset()
	value, err := store.get(ctx, domainRef, "preferences/theme")
	if err != nil {
		t.Fatal(err)
	}
	if string(value.Data) != `{"theme":"dark"}` || value.LogicalVersion != "version-a" || value.Revision != 1 {
		t.Fatalf("value = %+v", value)
	}
	if events := ledger.Events(); len(events) != 1 || events[0].Kind != providerbudget.RequestObjectGet {
		t.Fatalf("inline-delta read requests = %+v, want one head GET", events)
	}
}

func TestConsistencyDomainMutationDeniesStaleAndConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	stores := make([]*consistencyDomainStore, 8)
	for index := range stores {
		stores[index] = newConsistencyDomainStore(backend, nil)
	}
	domainRef := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner-a"}
	if _, err := stores[0].mutate(ctx, domainRef, consistencyDomainMutation{
		ID:      "seed",
		Changes: []consistencyDomainChange{{Key: "profile", Require: domainValueAbsent, Value: []byte("seed"), LogicalVersion: "version-seed"}},
	}); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, len(stores))
	for index, store := range stores {
		go func(index int, store *consistencyDomainStore) {
			<-start
			_, err := store.mutate(ctx, domainRef, consistencyDomainMutation{
				ID:      "writer-" + string(rune('a'+index)),
				Changes: []consistencyDomainChange{{Key: "profile", Require: domainValuePresent, ExpectedVersion: "version-seed", Value: []byte{byte('a' + index)}, LogicalVersion: "version-" + string(rune('a'+index))}},
			})
			results <- err
		}(index, store)
	}
	close(start)
	var succeeded, denied int
	for range stores {
		err := <-results
		if err == nil {
			succeeded++
		} else if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPreconditionFailed) {
			denied++
		} else {
			t.Fatalf("concurrent mutation error = %v", err)
		}
	}
	if succeeded != 1 || denied != len(stores)-1 {
		t.Fatalf("concurrent writers succeeded=%d denied=%d", succeeded, denied)
	}
}

func TestConsistencyDomainLostClaimFinalizationRecoversCommittedOutcome(t *testing.T) {
	ctx := context.Background()
	base := objectmemory.New()
	backend := &failAfterClaimFinalizationBackend{Backend: base}
	store := newConsistencyDomainStore(backend, nil)
	domainRef := consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "owner-a"}
	mutation := consistencyDomainMutation{
		ID: "move-project", Result: []byte(`{"moved":true}`),
		Changes: []consistencyDomainChange{{Key: "edge/live/project", Require: domainValueAbsent, Value: []byte("project"), LogicalVersion: "version-project"}},
	}
	backend.failNext = true
	if _, err := store.mutate(ctx, domainRef, mutation); !errors.Is(err, errInjectedClaimFinalization) {
		t.Fatalf("lost finalization result = %v", err)
	}

	outcome, err := store.mutate(ctx, domainRef, mutation)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Replayed || outcome.Revision != 1 || string(outcome.Result) != `{"moved":true}` {
		t.Fatalf("recovered outcome = %+v", outcome)
	}
	value, err := store.get(ctx, domainRef, "edge/live/project")
	if err != nil || string(value.Data) != "project" {
		t.Fatalf("committed value = %+v, %v", value, err)
	}
}

func TestConsistencyDomainFrozenHeadDeniesMutationWithoutCreatingClaim(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	base := objectmemory.New()
	backend := budgettest.Wrap(providerbudget.RoleState, base, ledger)
	store := newConsistencyDomainStore(backend, nil)
	domainRef := consistencyDomainRef{Kind: storageformat.DomainAdmin, ID: "global"}
	if _, err := store.mutate(ctx, domainRef, consistencyDomainMutation{
		ID:      "seed",
		Changes: []consistencyDomainChange{{Key: "roles", Require: domainValueAbsent, Value: []byte("admin"), LogicalVersion: "version-admin"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.freeze(ctx, domainRef, 7); err != nil {
		t.Fatal(err)
	}
	ledger.Reset()
	if _, err := store.mutate(ctx, domainRef, consistencyDomainMutation{
		ID:      "denied",
		Changes: []consistencyDomainChange{{Key: "roles", Require: domainValueAny, Value: []byte("other"), LogicalVersion: "version-other"}},
	}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("frozen mutation error = %v", err)
	}
	events := ledger.Events()
	if len(events) != 1 || events[0].Kind != providerbudget.RequestObjectGet {
		t.Fatalf("frozen mutation requests = %+v, want only the head read", events)
	}
	if _, err := base.Get(ctx, storageformat.DomainClaimKey(domainRef.Kind, domainRef.ID, "denied")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("denied mutation created a claim: %v", err)
	}
}

func TestConsistencyDomainCompactionPublishesAuthenticatedPersistentPages(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	base := objectmemory.New()
	backend := budgettest.Wrap(providerbudget.RoleState, base, ledger)
	store := newConsistencyDomainStore(backend, nil)
	domainRef := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner-wide"}
	for index := 0; index < 300; index++ {
		name := domainTestKey(index)
		if _, err := store.mutate(ctx, domainRef, consistencyDomainMutation{
			ID:      "create-" + name,
			Changes: []consistencyDomainChange{{Key: name, Require: domainValueAbsent, Value: []byte(name), LogicalVersion: "version-" + name}},
		}); err != nil {
			t.Fatalf("seed %d: %v", index, err)
		}
	}
	ledger.Reset()
	if err := store.compact(ctx, domainRef); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.loadHead(ctx, domainRef)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.head.Base.Digest == "" || snapshot.head.Base.EntryCount != 300 || len(snapshot.head.Deltas) != 0 {
		t.Fatalf("compacted head = %+v", snapshot.head)
	}
	for _, event := range ledger.Events() {
		if event.Kind == providerbudget.RequestObjectList || event.Kind == providerbudget.RequestObjectDelete {
			t.Fatalf("compaction scanned or deleted provider objects: %+v", event)
		}
	}

	ledger.Reset()
	value, err := store.get(ctx, domainRef, domainTestKey(299))
	if err != nil {
		t.Fatal(err)
	}
	if string(value.Data) != domainTestKey(299) || value.LogicalVersion != "version-"+domainTestKey(299) {
		t.Fatalf("compacted value = %+v", value)
	}
	events := ledger.Events()
	if len(events) > snapshot.head.Base.Level+2 {
		t.Fatalf("compacted point read requests=%d exceed head plus one page path at level=%d: %+v", len(events), snapshot.head.Base.Level, events)
	}

	objectsBeforePathCopy := len(base.Export())
	for _, mutation := range []consistencyDomainMutation{
		{ID: "update-existing", Changes: []consistencyDomainChange{{Key: domainTestKey(299), Require: domainValuePresent, ExpectedVersion: "version-" + domainTestKey(299), Value: []byte("updated"), LogicalVersion: "version-updated"}}},
		{ID: "delete-existing", Changes: []consistencyDomainChange{{Key: domainTestKey(0), Require: domainValuePresent, ExpectedVersion: "version-" + domainTestKey(0), Delete: true}}},
		{ID: "insert-new", Changes: []consistencyDomainChange{{Key: "zz-new", Require: domainValueAbsent, Value: []byte("new"), LogicalVersion: "version-new"}}},
	} {
		if _, err := store.mutate(ctx, domainRef, mutation); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.compact(ctx, domainRef); err != nil {
		t.Fatal(err)
	}
	pathCopied, err := store.loadHead(ctx, domainRef)
	if err != nil {
		t.Fatal(err)
	}
	if pathCopied.head.Base.EntryCount != 300 || pathCopied.head.Base.Digest == snapshot.head.Base.Digest || len(pathCopied.head.Deltas) != 0 {
		t.Fatalf("path-copied head = %+v", pathCopied.head)
	}
	if added := len(base.Export()) - objectsBeforePathCopy; added > 3+3*(snapshot.head.Base.Level+1) {
		t.Fatalf("three compacted point edits retained %d objects, exceeding three claims plus three changed page paths at level %d", added, snapshot.head.Base.Level)
	}
	if value, err := store.get(ctx, domainRef, domainTestKey(299)); err != nil || string(value.Data) != "updated" || value.LogicalVersion != "version-updated" {
		t.Fatalf("updated compacted value = %+v, %v", value, err)
	}
	if _, err := store.get(ctx, domainRef, domainTestKey(0)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted compacted value error = %v", err)
	}
	if value, err := store.get(ctx, domainRef, "zz-new"); err != nil || string(value.Data) != "new" {
		t.Fatalf("inserted compacted value = %+v, %v", value, err)
	}

	pageKey := storageformat.DomainPageKey(domainRef.Kind, domainRef.ID, pathCopied.head.Base.Digest)
	pageObject, err := base.Get(ctx, pageKey)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), pageObject.Body...)
	corrupt[len(corrupt)-1] ^= 1
	if _, err := base.Put(ctx, pageKey, corrupt, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: pageObject.Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.get(ctx, domainRef, domainTestKey(299)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt compacted page error = %v", err)
	}
}

func TestConsistencyDomainCompactionReconcilesPreparedClaimsBeforeRetiringDeltas(t *testing.T) {
	ctx := context.Background()
	base := objectmemory.New()
	backend := &failBeforeClaimFinalizationBackend{Backend: base, failNext: true}
	store := newConsistencyDomainStore(backend, nil)
	domainRef := consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "owner-recovery"}
	mutation := consistencyDomainMutation{
		ID: "mutation", Result: []byte("result"),
		Changes: []consistencyDomainChange{{Key: "path", Require: domainValueAbsent, Value: []byte("value"), LogicalVersion: "version"}},
	}
	if _, err := store.mutate(ctx, domainRef, mutation); !errors.Is(err, errInjectedClaimFinalization) {
		t.Fatalf("injected claim finalization error = %v", err)
	}
	if err := store.compact(ctx, domainRef); err != nil {
		t.Fatal(err)
	}
	outcome, err := store.mutate(ctx, domainRef, mutation)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Replayed || outcome.Revision != 1 || string(outcome.Result) != "result" {
		t.Fatalf("replayed compacted outcome = %+v", outcome)
	}
}

func TestConsistencyDomainFreezeWinsAgainstPausedStaleWriter(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	store := newConsistencyDomainStore(backend, nil)
	domainRef := consistencyDomainRef{Kind: storageformat.DomainAdmin, ID: "global"}
	if _, err := store.mutate(ctx, domainRef, consistencyDomainMutation{
		ID:      "seed",
		Changes: []consistencyDomainChange{{Key: "roles", Require: domainValueAbsent, Value: []byte("admin"), LogicalVersion: "version-admin"}},
	}); err != nil {
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
		_, err := writer.mutate(ctx, domainRef, consistencyDomainMutation{
			ID:      "stale-writer",
			Changes: []consistencyDomainChange{{Key: "roles", Require: domainValuePresent, ExpectedVersion: "version-admin", Value: []byte("changed"), LogicalVersion: "version-changed"}},
		})
		result <- err
	}()
	<-paused
	if err := store.freeze(ctx, domainRef, 9); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale writer error = %v", err)
	}
	value, err := store.get(ctx, domainRef, "roles")
	if err != nil || string(value.Data) != "admin" || value.LogicalVersion != "version-admin" {
		t.Fatalf("value after freeze race = %+v, %v", value, err)
	}
}

var errInjectedClaimFinalization = errors.New("injected claim finalization response loss")

type failAfterClaimFinalizationBackend struct {
	objectstore.Backend
	mu       sync.Mutex
	failNext bool
}

type failBeforeClaimFinalizationBackend struct {
	objectstore.Backend
	mu       sync.Mutex
	failNext bool
}

func (backend *failBeforeClaimFinalizationBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.failNext && condition.Mode == objectstore.PutMatch && key == storageformat.DomainClaimKey(storageformat.DomainNamespace, "owner-recovery", "mutation") {
		backend.failNext = false
		return "", errInjectedClaimFinalization
	}
	return backend.Backend.Put(ctx, key, body, condition)
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

func (backend *failAfterClaimFinalizationBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	version, err := backend.Backend.Put(ctx, key, body, condition)
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if err == nil && backend.failNext && condition.Mode == objectstore.PutMatch && key == storageformat.DomainClaimKey(storageformat.DomainNamespace, "owner-a", "move-project") {
		backend.failNext = false
		return "", errInjectedClaimFinalization
	}
	return version, err
}
