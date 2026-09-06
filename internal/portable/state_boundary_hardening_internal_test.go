package portable

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestSchema009StateCodecAndRoutingRejectUnsupportedKeysAtEveryBoundary(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	unsupported := state.MustKey(state.NamespaceOperations, "unknown", "owner", "operation")
	for name, call := range map[string]func() error{
		"encode-invalid": func() error { _, err := encodeStateValue009(state.Key{}, []byte("value")); return err },
		"encode-unsupported": func() error {
			_, err := encodeStateValue009(unsupported, []byte("value"))
			return err
		},
		"decode-invalid": func() error { _, err := decodeStateValue009(state.Key{}, []byte("value")); return err },
		"decode-unsupported": func() error {
			_, err := decodeStateValue009(unsupported, []byte("value"))
			return err
		},
		"get":    func() error { _, err := engine.Get(ctx, unsupported); return err },
		"create": func() error { _, err := engine.Create(ctx, unsupported, []byte("value")); return err },
		"compare-and-swap": func() error {
			_, err := engine.CompareAndSwap(ctx, unsupported, "version", []byte("value"))
			return err
		},
		"delete": func() error { return engine.Delete(ctx, unsupported, "version") },
		"mutate": func() error {
			_, err := engine.Mutate(ctx, state.Mutation{ID: "unsupported", Changes: []state.Change{{Key: unsupported, Requirement: state.RequirementAbsent, Data: []byte("value")}}})
			return err
		},
		"new-domain-mutation": func() error {
			_, _, err := engine.newStateDomainMutation(unsupported, "", []byte("value"), false)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if route := stateRouteForPath008("unsupported", nil); route.exact || route.reference != (consistencyDomainRef{}) {
		t.Fatalf("unsupported schema-008 route = %+v", route)
	}
}

func TestConsistencyDomainPreparedSnapshotsOutcomesAndLocksFailClosed(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2059, 1, 2, 3, 4, 5, 0, time.UTC))
	backend := objectmemory.New()
	store := newConsistencyDomainStore(backend, nil, clock)
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:state-boundaries"}
	mutation := consistencyDomainMutation{ID: "prepared", Changes: []consistencyDomainChange{{Key: "value", Require: domainValueAbsent, Value: []byte("value")}}}
	misbound := consistencyDomainHeadSnapshot{exists: true, head: storageformat.DomainHead{SchemaVersion: 1, Registered: true, Kind: reference.Kind, DomainID: "other"}}
	if _, err := store.mutatePrepared(ctx, reference, mutation, &misbound, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("misbound prepared snapshot error = %v", err)
	}

	badSnapshotBody := []byte("invalid")
	badSnapshotDigest := storageformat.Digest(badSnapshotBody)
	if _, err := backend.Put(ctx, storageformat.DomainSnapshotKey(reference.Kind, reference.ID, badSnapshotDigest), badSnapshotBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.loadHeadSnapshot(ctx, reference, badSnapshotDigest); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt domain snapshot error = %v", err)
	}
	otherSnapshot := storageformat.DomainSnapshot{SchemaVersion: 1, DomainID: "other", Kind: reference.Kind, Head: storageformat.DomainHead{SchemaVersion: 1, Registered: true, DomainID: "other", Kind: reference.Kind}, ExpiresAt: clock.Now().Add(time.Hour)}
	otherBody, err := storageformat.EncodeCanonical(otherSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	otherDigest := storageformat.Digest(otherBody)
	if _, err := backend.Put(ctx, storageformat.DomainSnapshotKey(reference.Kind, reference.ID, otherDigest), otherBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.loadHeadSnapshot(ctx, reference, otherDigest); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("misbound domain snapshot error = %v", err)
	}

	session := newConsistencyDomainTreeSession(store, reference)
	malformedRoot, err := session.buildTree(ctx, []storageformat.DomainEntry{{Key: "mutation", Value: []byte("invalid"), LogicalVersion: "version"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.lookupOutcomeAtHead(ctx, reference, storageformat.DomainHead{Outcomes: malformedRoot}, "mutation"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("malformed outcome error = %v", err)
	}
	recorded := storageformat.DomainOutcome{MutationID: "other", Fingerprint: storageformat.Digest([]byte("other")), Revision: 1, RetainUntil: clock.Now().Add(time.Hour)}
	recordedBody, err := storageformat.EncodeCanonical(recorded)
	if err != nil {
		t.Fatal(err)
	}
	misboundRoot, err := session.buildTree(ctx, []storageformat.DomainEntry{{Key: "mutation", Value: recordedBody, LogicalVersion: "version"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.lookupOutcomeAtHead(ctx, reference, storageformat.DomainHead{Outcomes: misboundRoot}, "mutation"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("misbound outcome error = %v", err)
	}
	expiredHead := storageformat.DomainHead{Deltas: []storageformat.DomainDelta{{MutationID: "expired", RetainUntil: clock.Now().Add(-time.Second)}}}
	if _, found, err := store.lookupOutcomeAtHead(ctx, reference, expiredHead, "expired"); err != nil || found {
		t.Fatalf("expired outcome found=%v error=%v", found, err)
	}

	lock := storageformat.TransitionLock009{SchemaVersion: 1, TransitionID: "transition", Fingerprint: storageformat.Digest([]byte("transition")), Kind: reference.Kind, DomainID: reference.ID}
	lockBody, err := storageformat.EncodeCanonical(lock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: "install-lock", Changes: []consistencyDomainChange{{Key: transitionLockKey009, Require: domainValueAbsent, Value: lockBody}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.get(ctx, reference, "value"); !errors.Is(err, errTransitionPending009) {
		t.Fatalf("transition-locked get error = %v", err)
	}
	if _, _, _, err := store.list(ctx, reference, "value", "", 1, clock.Now().Add(time.Hour)); !errors.Is(err, errTransitionPending009) {
		t.Fatalf("transition-locked list error = %v", err)
	}
	if _, err := store.get(ctx, reference, "value"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("read through transition lock error = %v", err)
	}
	if _, _, _, err := store.list(ctx, reference, "value", "", 1, clock.Now().Add(time.Hour)); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("list through transition lock error = %v", err)
	}
}

func TestConsistencyDomainProviderFailuresRemainFailClosed(t *testing.T) {
	ctx := context.Background()
	failure := domain.NewError(domain.ErrorUnavailable, "injected domain provider failure")
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:provider-failures"}
	mutation := consistencyDomainMutation{ID: "mutation", Changes: []consistencyDomainChange{{Key: "value", Require: domainValueAbsent, Value: []byte("value")}}}

	t.Run("initial-head-read", func(t *testing.T) {
		memory := objectmemory.New()
		backend := &hookedBackend{Backend: memory, get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, failure
		}}
		if _, err := newConsistencyDomainStore(backend, nil).mutate(ctx, reference, mutation); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("initial head read error = %v", err)
		}
	})

	t.Run("registration-head-write", func(t *testing.T) {
		memory := objectmemory.New()
		backend := &hookedBackend{Backend: memory, put: func(_ context.Context, key objectstore.Key, _ []byte, _ objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if key == storageformat.DomainHeadKey(reference.Kind, reference.ID) {
				return "", failure
			}
			return "", domain.NewError(domain.ErrorInternal, "unexpected write")
		}}
		if _, err := newConsistencyDomainStore(backend, nil).mutate(ctx, reference, mutation); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("registration head write error = %v", err)
		}
	})

	t.Run("snapshot-write", func(t *testing.T) {
		memory := objectmemory.New()
		backend := &hookedBackend{Backend: memory, put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return "", failure
		}}
		store := newConsistencyDomainStore(backend, nil)
		head := storageformat.DomainHead{SchemaVersion: 1, Registered: true, DomainID: reference.ID, Kind: reference.Kind}
		if _, err := store.writeHeadSnapshot(ctx, reference, head, store.clock.Now().Add(time.Hour)); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("snapshot write error = %v", err)
		}
	})

	t.Run("base-tree-read", func(t *testing.T) {
		store := newConsistencyDomainStore(objectmemory.New(), nil)
		head := storageformat.DomainHead{SchemaVersion: 1, Registered: true, DomainID: reference.ID, Kind: reference.Kind, Base: storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("missing-base")), Level: 0, EntryCount: 1}}
		if err := store.validateMutationAtHead(ctx, reference, head, mutation.Changes); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("base tree read error = %v", err)
		}
	})
}

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

func TestPortableInitializationPropagatesWriterSetPublicationFailure(t *testing.T) {
	ctx := context.Background()
	memory := objectmemory.New()
	failure := domain.NewError(domain.ErrorUnavailable, "writer-set publication failed")
	backend := &hookedBackend{Backend: memory, put: func(callCtx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
		if key == storageformat.WriterSetKey() {
			return "", failure
		}
		return memory.Put(callCtx, key, body, condition)
	}}
	_, err := Open(ctx, Options{
		Backend: backend, Clock: domain.NewFixedClock(time.Date(2066, 1, 2, 3, 4, 5, 0, time.UTC)), IDs: domain.NewIDGenerator(strings.NewReader(strings.Repeat("writer-set-failure", 1<<12))),
		Writer: WriterConfiguration{WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{"key"}}, LeaseTTL: time.Minute, CursorKey: []byte("0123456789abcdef0123456789abcdef"),
	})
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("writer-set publication error = %v", err)
	}
}

func TestConsistencyDomainPaginationExpiryAndCompactionFailureBoundaries(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2060, 1, 2, 3, 4, 5, 0, time.UTC))
	backend := objectmemory.New()
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:domain-maintenance"}
	store := newConsistencyDomainStore(backend, nil, clock)
	head := storageformat.DomainHead{Deltas: []storageformat.DomainDelta{{Changes: []storageformat.DomainChange{
		{Key: "prefix-a", Value: []byte("a"), LogicalVersion: "a"},
		{Key: "prefix-b", Value: []byte("b"), LogicalVersion: "b"},
	}}}}
	entries, err := store.listAtHead(ctx, reference, head, "prefix-", "", 1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("bounded domain list = %+v, %v", entries, err)
	}

	retainedUntil := clock.Now().Add(time.Second)
	if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: "expires-before-compaction", RetainUntil: retainedUntil, Changes: []consistencyDomainChange{{Key: "record", Require: domainValueAbsent, Value: []byte("value")}}}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Second)
	if err := store.compact(ctx, reference); err != nil {
		t.Fatal(err)
	}

	failing := newConsistencyDomainStore(&hookedBackend{Backend: backend, get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
		return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "domain read failed")
	}}, nil, clock)
	if err := failing.compact(ctx, reference); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("compaction read error = %v", err)
	}

	scheduled := newConsistencyDomainStore(backend, SchedulerFunc(func(_ context.Context, step string) error {
		if step == "consistency-domain:before-compaction-commit" {
			return domain.NewError(domain.ErrorUnavailable, "compaction interrupted")
		}
		return nil
	}), clock)
	if _, err := scheduled.mutate(ctx, reference, consistencyDomainMutation{ID: "pending-compaction", Changes: []consistencyDomainChange{{Key: "second", Require: domainValueAbsent, Value: []byte("second")}}}); err != nil {
		t.Fatal(err)
	}
	if err := scheduled.compact(ctx, reference); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("scheduled compaction error = %v", err)
	}

	invalidExpiry, err := newConsistencyDomainTreeSession(store, reference).buildTree(ctx, []storageformat.DomainEntry{{Key: "invalid-expiry", Value: []byte("mutation"), LogicalVersion: "version"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.pruneExpiredOutcomes(ctx, newConsistencyDomainTreeSession(store, reference), storageformat.DomainTreeRoot{}, invalidExpiry, clock.Now(), map[string]storageformat.DomainChange{}, map[string]storageformat.DomainChange{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid expiry index error = %v", err)
	}

	if parseConsistencyDomainExpiryTime("short") != (time.Time{}) || parseConsistencyDomainExpiryTime(strings.Repeat("x", len("20060102T150405.000000000Z"))+".id") != (time.Time{}) {
		t.Fatal("invalid expiry time parsed")
	}
}
