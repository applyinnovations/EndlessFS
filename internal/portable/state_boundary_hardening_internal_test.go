package portable

import (
	"context"
	"errors"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestSchema009StateOperationsHelpTransitionLocksAndRejectCorruptValues(t *testing.T) {
	ctx := context.Background()
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	key := state.MustKey(state.NamespaceAccounts, owner.String(), "record")
	prefix := state.MustPrefix(state.NamespaceAccounts, owner.String())

	t.Run("transition-lock", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		version, err := engine.Create(ctx, key, []byte(`{"schemaVersion":1,"userID":"`+owner.String()+`","disabled":false,"authEpoch":1}`))
		if err != nil {
			t.Fatal(err)
		}
		reference, err := stateDomainReferenceForKey(key)
		if err != nil {
			t.Fatal(err)
		}
		lock := storageformat.TransitionLock009{SchemaVersion: 1, TransitionID: "missing-plan", Fingerprint: storageformat.Digest([]byte("missing-plan")), Kind: reference.Kind, DomainID: reference.ID}
		body, err := storageformat.EncodeCanonical(lock)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.stateDomainStore().mutate(ctx, reference, consistencyDomainMutation{ID: "install-state-lock", Changes: []consistencyDomainChange{{Key: transitionLockKey009, Require: domainValueAbsent, Value: body}}}); err != nil {
			t.Fatal(err)
		}
		other := state.MustKey(state.NamespaceAccounts, owner.String(), "other")
		calls := map[string]func() error{
			"get": func() error { _, err := engine.Get(ctx, key); return err },
			"list": func() error {
				_, err := engine.List(ctx, prefix, state.PageRequest{Limit: 1})
				return err
			},
			"create": func() error { _, err := engine.Create(ctx, other, []byte(`{"schemaVersion":1}`)); return err },
			"compare-and-swap": func() error {
				_, err := engine.CompareAndSwap(ctx, key, version, []byte(`{"schemaVersion":1,"userID":"`+owner.String()+`","disabled":true,"authEpoch":2}`))
				return err
			},
			"delete": func() error { return engine.Delete(ctx, key, version) },
			"mutate": func() error {
				_, err := engine.Mutate(ctx, state.Mutation{ID: "locked-state-mutation", Changes: []state.Change{{Key: other, Requirement: state.RequirementAbsent, Data: []byte(`{"schemaVersion":1}`)}}})
				return err
			},
		}
		for name, call := range calls {
			t.Run(name, func(t *testing.T) {
				if err := call(); !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("locked state operation error = %v; want missing transition plan", err)
				}
			})
		}
	})

	t.Run("corrupt-value", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		reference, err := stateDomainReferenceForKey(key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.stateDomainStore().mutate(ctx, reference, consistencyDomainMutation{ID: "corrupt-state-value", Changes: []consistencyDomainChange{{Key: key.String(), Require: domainValueAbsent, Value: []byte("invalid")}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Get(ctx, key); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("corrupt state value error = %v", err)
		}
	})

	t.Run("corrupt-stored-key", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		reference, err := stateDomainReferenceForKey(key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.stateDomainStore().mutate(ctx, reference, consistencyDomainMutation{ID: "corrupt-state-key", Changes: []consistencyDomainChange{{Key: prefix.String() + "%", Require: domainValueAbsent, Value: []byte(`{"schemaVersion":1}`)}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.List(ctx, prefix, state.PageRequest{Limit: 1}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("corrupt stored state key error = %v", err)
		}
	})
}

func TestSchema009GetManySharesOneAuthenticatedDomainAndFailsClosed(t *testing.T) {
	ctx := context.Background()
	owner := namespaceTestScope(t, domain.AreaLive).UserID().String()
	first := state.MustKey(state.NamespaceAccounts, owner, "first")
	second := state.MustKey(state.NamespaceAccounts, owner, "second")
	otherOwner := state.MustKey(state.NamespaceAccounts, "other-owner", "record")
	unsupported := state.MustKey(state.NamespaceOperations, "unknown", owner, "operation")
	failure := domain.NewError(domain.ErrorUnavailable, "multi-read authority unavailable")

	for name, keys := range map[string][]state.Key{
		"empty":        nil,
		"too-large":    make([]state.Key, 1001),
		"invalid-key":  {{}},
		"unsupported":  {unsupported},
		"cross-domain": {first, otherOwner},
	} {
		t.Run(name, func(t *testing.T) {
			engine := openNamespaceTestEngine(t, objectmemory.New())
			if _, err := engine.GetMany(ctx, keys); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("GetMany(%s) error = %v", name, err)
			}
		})
	}

	t.Run("missing-domain", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		if _, err := engine.GetMany(ctx, []state.Key{first}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing domain error = %v", err)
		}
	})

	t.Run("head-read-failure", func(t *testing.T) {
		base := objectmemory.New()
		engine := openNamespaceTestEngine(t, base)
		engine.backend = &hookedBackend{Backend: base, get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, failure
		}}
		if _, err := engine.GetMany(ctx, []state.Key{first}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("head read error = %v", err)
		}
	})

	t.Run("member-not-found", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		want := []byte(`{"record":"first"}`)
		if _, err := engine.Create(ctx, first, want); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.GetMany(ctx, []state.Key{first, second}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing member error = %v", err)
		}
		values, err := engine.GetMany(ctx, []state.Key{first})
		if err != nil || len(values) != 1 || string(values[0].Data) != string(want) || values[0].Version == "" {
			t.Fatalf("authenticated multi-read = %+v, %v", values, err)
		}
	})

	t.Run("corrupt-member", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		reference, err := stateDomainReferenceForKey(first)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.stateDomainStore().mutate(ctx, reference, consistencyDomainMutation{
			ID: "corrupt-multi-read", Changes: []consistencyDomainChange{{Key: first.String(), Require: domainValueAbsent, Value: []byte("invalid")}},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.GetMany(ctx, []state.Key{first}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("corrupt member error = %v", err)
		}
	})

	t.Run("tree-read-failure", func(t *testing.T) {
		base := objectmemory.New()
		engine := openNamespaceTestEngine(t, base)
		if _, err := engine.Create(ctx, first, []byte(`{"record":"first"}`)); err != nil {
			t.Fatal(err)
		}
		reference, err := stateDomainReferenceForKey(first)
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.stateDomainStore().compact(ctx, reference); err != nil {
			t.Fatal(err)
		}
		headKey := storageformat.DomainHeadKey(reference.Kind, reference.ID)
		engine.backend = &hookedBackend{Backend: base, get: func(callCtx context.Context, key objectstore.Key) (objectstore.Object, error) {
			if key == headKey {
				return base.Get(callCtx, key)
			}
			return objectstore.Object{}, failure
		}}
		if _, err := engine.GetMany(ctx, []state.Key{first}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("tree read error = %v", err)
		}
	})
}
