package portable

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestConsistencyDomainSnapshotDecisionRejectsChangedReadAuthority(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"delta", "delta-compaction", "materialized"} {
		t.Run(mode, func(t *testing.T) {
			clock := domain.NewFixedClock(time.Date(2072, 1, 2, 3, 4, 5, 0, time.UTC))
			store := newConsistencyDomainStore(objectmemory.New(), nil, clock)
			reference := consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "snapshot-decision"}
			count := 1
			if mode == "delta-compaction" {
				count = consistencyDomainDeltaWindow
			}
			for index := 0; index < count; index++ {
				if _, err := store.mutate(ctx, reference, consistencyDomainMutation{
					ID: fmt.Sprintf("seed-%d", index), Changes: []consistencyDomainChange{{Key: "authority", Require: domainValueAny, Value: []byte("active")}},
				}); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, session := materializedDomainView(t, store, reference, "publish-decision")
			revision := snapshot.head.Revision
			mutation := consistencyDomainMutation{
				ID: "publish-decision", ExpectedRevision: &revision,
				Changes: []consistencyDomainChange{{Key: "decision", Require: domainValueAbsent, Value: []byte("aborted")}},
			}
			// The competing write changes authority the decision read, while the
			// decision's own key remains absent and passes its key precondition.
			if _, err := store.mutate(ctx, reference, consistencyDomainMutation{
				ID: "complete-authority", Changes: []consistencyDomainChange{{Key: "authority", Require: domainValuePresent, Value: []byte("completed")}},
			}); err != nil {
				t.Fatal(err)
			}
			var err error
			if mode == "materialized" {
				// A caller may already have refreshed the view but must not carry
				// a decision validated at an older revision into that new view.
				snapshot, session = materializedDomainView(t, store, reference, mutation.ID)
				_, err = store.mutateMaterializedPrepared(ctx, reference, mutation, &snapshot, session)
			} else {
				_, err = store.mutatePrepared(ctx, reference, mutation, &snapshot, session)
			}
			if !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("stale decision error = %v", err)
			}
			if _, err := store.get(ctx, reference, "decision"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("stale decision was published: %v", err)
			}
			value, err := store.get(ctx, reference, "authority")
			if err != nil || string(value.Data) != "completed" {
				t.Fatalf("winning authority = %+v, %v", value, err)
			}
		})
	}
}

func TestConsistencyDomainSnapshotDecisionPreservesCompactionAndReplay(t *testing.T) {
	ctx := context.Background()
	for _, materialized := range []bool{false, true} {
		t.Run(fmt.Sprintf("materialized=%t", materialized), func(t *testing.T) {
			clock := domain.NewFixedClock(time.Date(2072, 1, 2, 3, 4, 5, 0, time.UTC))
			store := newConsistencyDomainStore(objectmemory.New(), nil, clock)
			reference := consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "snapshot-replay"}
			if _, err := store.mutate(ctx, reference, consistencyDomainMutation{
				ID: "seed", Changes: []consistencyDomainChange{{Key: "authority", Require: domainValueAbsent, Value: []byte("active")}},
			}); err != nil {
				t.Fatal(err)
			}
			snapshot, _ := materializedDomainView(t, store, reference, "decision")
			revision := snapshot.head.Revision
			if err := store.compactSnapshot(ctx, reference, snapshot); err != nil {
				t.Fatal(err)
			}
			mutation := consistencyDomainMutation{
				ID: "decision", ExpectedRevision: &revision, Result: []byte("committed"),
				Changes: []consistencyDomainChange{{Key: "decision", Require: domainValueAbsent, Value: []byte("aborted")}},
			}
			apply := func() (consistencyDomainOutcome, error) {
				snapshot, session := materializedDomainView(t, store, reference, mutation.ID)
				if materialized {
					return store.mutateMaterializedPrepared(ctx, reference, mutation, &snapshot, session)
				}
				return store.mutatePrepared(ctx, reference, mutation, &snapshot, session)
			}
			outcome, err := apply()
			if err != nil || outcome.Replayed || outcome.Revision != revision+1 {
				t.Fatalf("decision after compaction = %+v, %v", outcome, err)
			}
			// The same committed intent replays even though its original read
			// revision is now stale. Revalidation does not change durable IDs.
			replay, err := apply()
			if err != nil || !replay.Replayed || replay.Revision != outcome.Revision || replay.Fingerprint != outcome.Fingerprint || string(replay.Result) != "committed" {
				t.Fatalf("replay = %+v, %v", replay, err)
			}
			mutation.ExpectedRevision = nil
			replay, err = apply()
			if err != nil || !replay.Replayed || replay.Fingerprint != outcome.Fingerprint {
				t.Fatalf("replay without transient precondition = %+v, %v", replay, err)
			}
		})
	}
}
