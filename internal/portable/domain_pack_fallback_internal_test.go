package portable

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestMaterializedPackWireFallbackPreservesPublicationBoundaries(t *testing.T) {
	// Independent canonical values fit their page bounds but their noise exceeds
	// the compressed pack bound. The upload regression covers expanded overflow.
	mutation := consistencyDomainMutation{ID: "large-wire-mutation", Result: []byte("committed")}
	reader := &deterministicScaleReader{state: 0x93847561}
	for index := range 8 {
		value := make([]byte, 600_000)
		_, _ = reader.Read(value)
		mutation.Changes = append(mutation.Changes, consistencyDomainChange{Key: fmt.Sprintf("item-%02d", index), Require: domainValueAbsent, Value: value})
	}
	for _, mode := range []string{"lost-success", "concurrent-writer", "freeze", "cancel", "prepared-child-pages"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "pack-fallback"}
			base := objectmemory.New()
			backend := &loseSuccessfulHeadPutBackend{Backend: base, target: storageformat.DomainHeadKey(reference.Kind, reference.ID)}
			clock := domain.NewFixedClock(time.Date(2072, 5, 6, 7, 8, 9, 0, time.UTC))
			store := newConsistencyDomainStore(backend, nil, clock)
			if err := store.ensureRegistered(ctx, reference); err != nil {
				t.Fatal(err)
			}
			other := newConsistencyDomainStore(base, nil, clock)
			boundaryReached := false
			store.scheduler = SchedulerFunc(func(ctx context.Context, step string) error {
				if step != StepDomainBeforeHeadCommit || boundaryReached {
					return nil
				}
				boundaryReached = true
				// All immutable fallback pages are present, but none is visible.
				if _, err := other.get(ctx, reference, mutation.Changes[0].Key); !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("unpublished fallback became visible: %v", err)
				}
				switch mode {
				case "lost-success":
					backend.armed = true
				case "concurrent-writer":
					_, err := other.mutate(ctx, reference, consistencyDomainMutation{ID: "winner", Changes: []consistencyDomainChange{{Key: "winner", Require: domainValueAbsent, Value: []byte("winner")}}})
					return err
				case "freeze":
					return other.freeze(ctx, reference, 7)
				case "cancel":
					cancel()
				}
				return nil
			})
			snapshot, session := materializedDomainView(t, store, reference, mutation.ID)
			candidate := mutation
			candidate.Changes = append([]consistencyDomainChange(nil), mutation.Changes...)
			var childRoot storageformat.DomainTreeRoot
			if mode == "prepared-child-pages" {
				// A caller may embed this descriptor in an opaque mutation value.
				// Dropping its session would publish a dangling packed reference.
				child, err := session.writePage(ctx, storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Entries: []storageformat.DomainEntry{{Key: "child", Value: []byte("child"), LogicalVersion: "child-version"}}})
				if err != nil {
					t.Fatal(err)
				}
				childRoot = child.root
				body, err := storageformat.EncodeCanonical(childRoot)
				if err != nil {
					t.Fatal(err)
				}
				candidate.Changes = append(candidate.Changes, consistencyDomainChange{Key: "opaque-child", Require: domainValueAbsent, Value: body})
			}
			outcome, err := store.mutateMaterializedPrepared(ctx, reference, candidate, &snapshot, session)
			if mode == "prepared-child-pages" {
				if !boundaryReached || err != nil {
					t.Fatalf("pre-staged references did not publish safely through the original conditional boundary: %v", err)
				}
				value, err := other.get(ctx, reference, "opaque-child")
				if err != nil {
					t.Fatal(err)
				}
				var restored storageformat.DomainTreeRoot
				if err := decodeCanonicalValue(value.Data, &restored); err != nil || restored != childRoot {
					t.Fatalf("opaque staged reference changed: %+v %v", restored, err)
				}
				page, err := newConsistencyDomainTreeSession(other, reference).readPage(ctx, domainPageRef{root: restored})
				if err != nil || len(page.Entries) != 1 || string(page.Entries[0].Value) != "child" {
					t.Fatalf("committed opaque child is unreachable or changed: %+v %v", page, err)
				}
				for _, change := range candidate.Changes {
					actual, err := other.get(ctx, reference, change.Key)
					if err != nil || !bytes.Equal(actual.Data, change.Value) {
						t.Fatalf("repacked authoritative value changed: %v", err)
					}
				}
				return
			}
			if !boundaryReached {
				t.Fatalf("fallback did not reach conditional publication: %v", err)
			}
			if mode == "lost-success" {
				if err != nil || !outcome.Replayed || outcome.Revision != 1 {
					t.Fatalf("lost-success fallback = %+v, %v", outcome, err)
				}
				for _, change := range mutation.Changes {
					value, err := other.get(ctx, reference, change.Key)
					if err != nil || !bytes.Equal(value.Data, change.Value) {
						t.Fatalf("fallback authority changed for %s: %v", change.Key, err)
					}
				}
				fresh, freshSession := materializedDomainView(t, store, reference, mutation.ID)
				replay, err := store.mutateMaterializedPrepared(ctx, reference, mutation, &fresh, freshSession)
				if err != nil || !replay.Replayed || replay.Fingerprint != outcome.Fingerprint || replay.Revision != outcome.Revision {
					t.Fatalf("fallback replay = %+v, %v", replay, err)
				}
				return
			}
			if err == nil || mode == "cancel" && !errors.Is(err, domain.ErrUnavailable) || mode != "cancel" && !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
				t.Fatalf("%s failed to deny stale fallback: %v", mode, err)
			}
			for _, change := range mutation.Changes {
				if _, err := other.get(context.Background(), reference, change.Key); !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("failed fallback published %s: %v", change.Key, err)
				}
			}
		})
	}
}

func TestMaterializedPackCorruptionDoesNotTriggerFallback(t *testing.T) {
	ctx := context.Background()
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "invalid-pack"}
	backend := objectmemory.New()
	store := newConsistencyDomainStore(backend, nil, domain.NewFixedClock(time.Date(2072, 5, 6, 7, 8, 9, 0, time.UTC)))
	if err := store.ensureRegistered(ctx, reference); err != nil {
		t.Fatal(err)
	}
	before, err := backend.List(ctx, objectstore.ListRequest{Prefix: storageformat.DomainPrefix()})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, session := materializedDomainView(t, store, reference, "invalid-pack")
	if _, err := session.ensurePackID("seed"); err != nil {
		t.Fatal(err)
	}
	session.pendingPack["invalid"] = storageformat.DomainPage{}
	_, err = store.mutateMaterializedPrepared(ctx, reference, consistencyDomainMutation{ID: "invalid-pack", Changes: []consistencyDomainChange{{Key: "new", Require: domainValueAbsent, Value: []byte("new")}}}, &snapshot, session)
	if !errors.Is(err, domain.ErrInvalid) || errors.Is(err, storageformat.ErrDomainPagePackTooLarge) {
		t.Fatalf("corrupt pack = %v", err)
	}
	if _, err := store.get(ctx, reference, "new"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("corrupt preparation published: %v", err)
	}
	if objects, err := backend.List(ctx, objectstore.ListRequest{Prefix: storageformat.DomainPrefix()}); err != nil || len(objects.Objects) != len(before.Objects) {
		t.Fatalf("corrupt preparation wrote fallback pages: %+v, %v", objects, err)
	}
}
