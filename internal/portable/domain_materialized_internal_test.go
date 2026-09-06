package portable

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func materializedDomainView(t *testing.T, store *consistencyDomainStore, reference consistencyDomainRef, mutationID string) (consistencyDomainHeadSnapshot, *consistencyDomainTreeSession) {
	t.Helper()
	snapshot, err := store.loadHead(context.Background(), reference)
	if err != nil {
		t.Fatal(err)
	}
	session := newConsistencyDomainTreeSession(store, reference)
	headBody, err := storageformat.EncodeCanonical(snapshot.head)
	if err != nil {
		t.Fatal(err)
	}
	session.enablePackedWrites(storageformat.Digest(headBody))
	if err := session.bindPackedMutation(storageformat.Digest([]byte("materialized-test\x00" + mutationID))); err != nil {
		t.Fatal(err)
	}
	return snapshot, session
}

func TestMaterializedConsistencyDomainMutationPublishesReplaysAndRejectsReuse(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2071, 2, 3, 4, 5, 6, 0, time.UTC))
	store := newConsistencyDomainStore(objectmemory.New(), nil, clock)
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "materialized-owner"}
	if err := store.ensureRegistered(ctx, reference); err != nil {
		t.Fatal(err)
	}
	mutation := consistencyDomainMutation{
		ID: "materialized-create", Result: []byte("committed"), RetainUntil: clock.Now().Add(time.Hour),
		Changes: []consistencyDomainChange{
			{Key: "alpha", Require: domainValueAbsent, Value: []byte("one")},
			{Key: "beta", Require: domainValueAbsent, Value: []byte("two")},
		},
	}
	snapshot, session := materializedDomainView(t, store, reference, mutation.ID)
	outcome, err := store.mutateMaterializedPrepared(ctx, reference, mutation, &snapshot, session)
	if err != nil || outcome.Replayed || outcome.Revision != 1 || string(outcome.Result) != "committed" || outcome.RetainUntil != mutation.RetainUntil {
		t.Fatalf("outcome = %+v, %v", outcome, err)
	}
	for key, want := range map[string]string{"alpha": "one", "beta": "two"} {
		value, err := store.get(ctx, reference, key)
		if err != nil || string(value.Data) != want {
			t.Fatalf("%s = %+v, %v", key, value, err)
		}
	}
	snapshot, session = materializedDomainView(t, store, reference, mutation.ID)
	replayed, err := store.mutateMaterializedPrepared(ctx, reference, mutation, &snapshot, session)
	if err != nil || !replayed.Replayed || replayed.Revision != outcome.Revision {
		t.Fatalf("replay = %+v, %v", replayed, err)
	}
	changed := mutation
	changed.Changes = append([]consistencyDomainChange(nil), mutation.Changes...)
	changed.Changes[0].Value = []byte("different")
	snapshot, session = materializedDomainView(t, store, reference, mutation.ID)
	if _, err := store.mutateMaterializedPrepared(ctx, reference, changed, &snapshot, session); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("changed replay = %v", err)
	}
}

func TestMaterializedConsistencyDomainMutationFailureAndRecoveryMatrix(t *testing.T) {
	ctx := context.Background()
	reference := consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "materialized-failures"}
	mutation := consistencyDomainMutation{ID: "materialized", Changes: []consistencyDomainChange{{Key: "record", Require: domainValueAbsent, Value: []byte("value")}}}

	base := objectmemory.New()
	store := newConsistencyDomainStore(base, nil)
	if err := store.ensureRegistered(ctx, reference); err != nil {
		t.Fatal(err)
	}
	snapshot, session := materializedDomainView(t, store, reference, mutation.ID)
	for name, run := range map[string]func() error{
		"reference": func() error {
			_, err := store.mutateMaterializedPrepared(ctx, consistencyDomainRef{}, mutation, &snapshot, session)
			return err
		},
		"snapshot": func() error {
			_, err := store.mutateMaterializedPrepared(ctx, reference, mutation, nil, session)
			return err
		},
		"session": func() error {
			_, err := store.mutateMaterializedPrepared(ctx, reference, mutation, &snapshot, nil)
			return err
		},
		"misbound": func() error {
			candidate := snapshot
			candidate.head.DomainID = "other"
			_, err := store.mutateMaterializedPrepared(ctx, reference, mutation, &candidate, session)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	missing := mutation
	missing.ID = "requires-present"
	missing.Changes = []consistencyDomainChange{{Key: "missing", Require: domainValuePresent, Value: []byte("value")}}
	snapshot, session = materializedDomainView(t, store, reference, missing.ID)
	if _, err := store.mutateMaterializedPrepared(ctx, reference, missing, &snapshot, session); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing precondition = %v", err)
	}

	invalid := mutation
	invalid.ID = ""
	snapshot, session = materializedDomainView(t, store, reference, "invalid-normalization")
	if _, err := store.mutateMaterializedPrepared(ctx, reference, invalid, &snapshot, session); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid materialized mutation = %v", err)
	}

	if err := store.freeze(ctx, reference, 7); err != nil {
		t.Fatal(err)
	}
	frozen := mutation
	frozen.ID = "frozen"
	snapshot, session = materializedDomainView(t, store, reference, frozen.ID)
	if _, err := store.mutateMaterializedPrepared(ctx, reference, frozen, &snapshot, session); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("frozen mutation = %v", err)
	}

	for name, candidate := range map[string]consistencyDomainMutation{
		"empty-id":                 {Changes: mutation.Changes},
		"empty-changes":            {ID: "empty"},
		"empty-key":                {ID: "empty-key", Changes: []consistencyDomainChange{{Value: []byte("value")}}},
		"duplicate":                {ID: "duplicate", Changes: []consistencyDomainChange{{Key: "key"}, {Key: "key"}}},
		"expected-without-present": {ID: "expected", Changes: []consistencyDomainChange{{Key: "key", Require: domainValueAny, ExpectedVersion: "version"}}},
		"delete-value":             {ID: "delete", Changes: []consistencyDomainChange{{Key: "key", Delete: true, Value: []byte("value")}}},
		"oversized-value":          {ID: "oversized", Changes: []consistencyDomainChange{{Key: "key", Require: domainValueAny, Value: make([]byte, storageformat.MaxCanonicalBytes)}}},
	} {
		t.Run("normalize-"+name, func(t *testing.T) {
			if _, _, err := normalizeMaterializedConsistencyDomainMutation(candidate); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	lostBase := objectmemory.New()
	lostBackend := &loseSuccessfulHeadPutBackend{Backend: lostBase, target: storageformat.DomainHeadKey(reference.Kind, "materialized-lost-success")}
	lostStore := newConsistencyDomainStore(lostBackend, nil)
	lostReference := consistencyDomainRef{Kind: reference.Kind, ID: "materialized-lost-success"}
	if err := lostStore.ensureRegistered(ctx, lostReference); err != nil {
		t.Fatal(err)
	}
	lostBackend.armed = true
	snapshot, session = materializedDomainView(t, lostStore, lostReference, mutation.ID)
	recovered, err := lostStore.mutateMaterializedPrepared(ctx, lostReference, mutation, &snapshot, session)
	if err != nil || !recovered.Replayed {
		t.Fatalf("lost-success recovery = %+v, %v", recovered, err)
	}

	afterFailure := domain.NewError(domain.ErrorUnavailable, "after commit response lost")
	afterStore := newConsistencyDomainStore(objectmemory.New(), SchedulerFunc(func(_ context.Context, step string) error {
		if step == StepDomainAfterHeadCommit {
			return afterFailure
		}
		return nil
	}))
	afterReference := consistencyDomainRef{Kind: reference.Kind, ID: "materialized-after-step"}
	if err := afterStore.ensureRegistered(ctx, afterReference); err != nil {
		t.Fatal(err)
	}
	snapshot, session = materializedDomainView(t, afterStore, afterReference, mutation.ID)
	recovered, err = afterStore.mutateMaterializedPrepared(ctx, afterReference, mutation, &snapshot, session)
	if err != nil || !recovered.Replayed {
		t.Fatalf("after-commit recovery = %+v, %v", recovered, err)
	}
}

func TestMaterializedConsistencyDomainOutcomePruningFailsClosed(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2072, 3, 4, 5, 6, 7, 0, time.UTC)
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "materialized-pruning-denials"}
	store := newConsistencyDomainStore(objectmemory.New(), nil, domain.NewFixedClock(now))

	t.Run("missing-expiry-tree", func(t *testing.T) {
		session := newConsistencyDomainTreeSession(store, reference)
		missing := storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("missing-expiry-tree")), Level: 0, EntryCount: 1}
		if _, _, err := store.pruneExpiredOutcomes(ctx, session, storageformat.DomainTreeRoot{}, missing, now, map[string]storageformat.DomainChange{}, map[string]storageformat.DomainChange{}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing expiry tree error = %v", err)
		}
	})

	t.Run("misbound-expiry-record", func(t *testing.T) {
		session := newConsistencyDomainTreeSession(store, reference)
		retainUntil := now.Add(-time.Hour)
		expiryKey := consistencyDomainOutcomeExpiryKey(retainUntil, "expected-mutation")
		expiryRoot, err := session.buildTree(ctx, []storageformat.DomainEntry{{Key: expiryKey, Value: []byte("different-mutation"), LogicalVersion: "expiry-version"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.pruneExpiredOutcomes(ctx, session, storageformat.DomainTreeRoot{}, expiryRoot, now, map[string]storageformat.DomainChange{}, map[string]storageformat.DomainChange{}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("misbound expiry record error = %v", err)
		}
	})

	t.Run("missing-outcome-tree", func(t *testing.T) {
		session := newConsistencyDomainTreeSession(store, reference)
		missing := storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("missing-outcome-tree")), Level: 0, EntryCount: 1}
		outcomes := map[string]storageformat.DomainChange{"new-outcome": {Key: "new-outcome", Value: []byte("value"), LogicalVersion: "outcome-version"}}
		if _, _, err := store.pruneExpiredOutcomes(ctx, session, missing, storageformat.DomainTreeRoot{}, now, outcomes, map[string]storageformat.DomainChange{}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing outcome tree error = %v", err)
		}
	})
}
