package portable

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestSchema008DomainStoreHeadSnapshotAndProviderBoundaryMatrix(t *testing.T) {
	ctx := context.Background()
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:domain-boundaries"}
	invalidReference := consistencyDomainRef{Kind: "invalid", ID: "owner"}
	if _, err := newConsistencyDomainStore(objectmemory.New(), nil).loadHead(ctx, invalidReference); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid load reference error = %v", err)
	}
	if _, err := newConsistencyDomainStore(objectmemory.New(), nil).mutate(ctx, invalidReference, consistencyDomainMutation{ID: "mutation", Changes: []consistencyDomainChange{{Key: "key", Require: domainValueAbsent, Value: []byte("value")}}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid mutation reference error = %v", err)
	}
	if _, err := newConsistencyDomainStore(objectmemory.New(), nil).mutate(ctx, reference, consistencyDomainMutation{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid mutation error = %v", err)
	}

	t.Run("invalid-initial-head", func(t *testing.T) {
		memory := objectmemory.New()
		key := storageformat.DomainHeadKey(reference.Kind, reference.ID)
		head := storageformat.DomainHead{SchemaVersion: 2, DomainID: reference.ID, Kind: reference.Kind}
		body, err := storageformat.EncodeEnvelope(domainHeadSchema, key, 1, head)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := memory.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if _, err := newConsistencyDomainStore(memory, nil).loadHead(ctx, reference); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("initial head validation error = %v", err)
		}
	})

	t.Run("prepared-head-binding", func(t *testing.T) {
		store := newConsistencyDomainStore(objectmemory.New(), nil)
		misbound := consistencyDomainHeadSnapshot{exists: true, head: storageformat.DomainHead{SchemaVersion: 1, DomainID: "other", Kind: reference.Kind, Registered: true}}
		_, err := store.mutateAtHead(ctx, reference, consistencyDomainMutation{ID: "prepared", RetainUntil: time.Now().Add(time.Hour), Changes: []consistencyDomainChange{{Key: "key", Require: domainValueAbsent, Value: []byte("value")}}}, &misbound)
		if !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("misbound prepared head error = %v", err)
		}
	})

	t.Run("head-read-and-registration-write-failures", func(t *testing.T) {
		memory := objectmemory.New()
		hooks := &hookedBackend{Backend: memory, get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "read failed")
		}}
		store := newConsistencyDomainStore(hooks, nil)
		if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: "read-failure", Changes: []consistencyDomainChange{{Key: "key", Require: domainValueAbsent, Value: []byte("value")}}}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("head read failure = %v", err)
		}
		hooks.get = nil
		hooks.put = func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return "", domain.NewError(domain.ErrorUnavailable, "write failed")
		}
		if _, err := store.prepareRegistration(ctx, reference); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("registration write failure = %v", err)
		}
	})

	t.Run("snapshot-validation-and-transport", func(t *testing.T) {
		memory := objectmemory.New()
		store := newConsistencyDomainStore(memory, nil)
		if _, err := store.writeHeadSnapshot(ctx, reference, storageformat.DomainHead{}, time.Time{}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid snapshot error = %v", err)
		}
		hooks := &hookedBackend{Backend: memory, get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "snapshot unavailable")
		}}
		if _, err := newConsistencyDomainStore(hooks, nil).loadHeadSnapshot(ctx, reference, storageformat.Digest([]byte("snapshot"))); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("snapshot read error = %v", err)
		}

		wrong := storageformat.DomainSnapshot{SchemaVersion: 1, DomainID: "other", Kind: reference.Kind, Head: storageformat.DomainHead{SchemaVersion: 1, DomainID: "other", Kind: reference.Kind, Registered: true}, ExpiresAt: time.Now().Add(time.Hour)}
		body, err := storageformat.EncodeCanonical(wrong)
		if err != nil {
			t.Fatal(err)
		}
		digest := storageformat.Digest(body)
		if _, err := memory.Put(ctx, storageformat.DomainSnapshotKey(reference.Kind, reference.ID, digest), body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.loadHeadSnapshot(ctx, reference, digest); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("snapshot key binding error = %v", err)
		}
	})
}

func TestSchema008DomainStoreLookupListCompactionAndExpiryDenials(t *testing.T) {
	ctx := context.Background()
	memory := objectmemory.New()
	store := newConsistencyDomainStore(memory, nil)
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:domain-denials"}
	if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: "seed", Changes: []consistencyDomainChange{{Key: "prefix/value", Require: domainValueAbsent, Value: []byte("value")}}}); err != nil {
		t.Fatal(err)
	}
	head, err := store.loadHead(ctx, reference)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.list(ctx, reference, "prefix/", "", 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.listSnapshot(ctx, reference, storageformat.Digest([]byte("missing")), "prefix/", "", 1); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing list snapshot error = %v", err)
	}

	corruptRoot := storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("missing-page")), Level: 0, EntryCount: 1}
	head.head.Base = corruptRoot
	head.head.Deltas = nil
	if _, _, err := store.lookupAtHead(ctx, reference, head.head, "prefix/value"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("lookup page failure = %v", err)
	}
	if _, err := store.get(ctx, reference, "prefix/value"); err != nil {
		t.Fatal(err)
	}

	if !parseConsistencyDomainExpiryTime("invalid").IsZero() || !parseConsistencyDomainExpiryTime("20251340T250000.000000000Z.bad").IsZero() {
		t.Fatal("invalid expiry keys parsed")
	}
	validExpiry := consistencyDomainOutcomeExpiryKey(time.Now().UTC(), "mutation")
	if parseConsistencyDomainExpiryTime(validExpiry).IsZero() {
		t.Fatal("valid expiry key did not parse")
	}
	badExpiryRoot, err := newConsistencyDomainTreeSession(store, reference).buildTree(ctx, []storageformat.DomainEntry{{Key: "invalid", Value: []byte("mutation"), LogicalVersion: "version"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.pruneExpiredOutcomes(ctx, newConsistencyDomainTreeSession(store, reference), storageformat.DomainTreeRoot{}, badExpiryRoot, time.Now(), map[string]storageformat.DomainChange{}, map[string]storageformat.DomainChange{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid expiry tree error = %v", err)
	}

	hooks := &hookedBackend{Backend: memory}
	failingStore := newConsistencyDomainStore(hooks, nil)
	hooks.put = func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
		return "", domain.NewError(domain.ErrorUnavailable, "snapshot write failed")
	}
	validHead, err := store.loadHead(ctx, reference)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failingStore.writeHeadSnapshot(ctx, reference, validHead.head, time.Now().Add(time.Hour)); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("snapshot write error = %v", err)
	}
}
