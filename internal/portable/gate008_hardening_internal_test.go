package portable

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func rewriteCurrentGate(t *testing.T, backend objectstore.Backend, mutate func(*storageformat.WriteGate)) {
	t.Helper()
	ctx := context.Background()
	key := storageformat.WriteGateKey()
	object, err := backend.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	var envelope storageformat.Envelope
	var gate storageformat.WriteGate
	if err := storageformat.DecodeEnvelope(object.Body, key, writeGateSchema, &envelope, &gate); err != nil {
		t.Fatal(err)
	}
	mutate(&gate)
	body, err := storageformat.EncodeEnvelope(writeGateSchema, key, envelope.Revision+1, gate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
		t.Fatal(err)
	}
}

func TestSchema008DomainCatalogBindingTraversalAndEncodingFailures(t *testing.T) {
	ctx := context.Background()
	memory := objectmemory.New()
	catalog := newDomainCatalog(memory, nil)
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:catalog-boundary"}
	session := newDomainCatalogTreeSession(catalog.store)

	misbound := storageformat.DomainCatalogEntry{DomainID: reference.ID, Kind: reference.Kind, HeadKey: storageformat.DomainHeadKey(reference.Kind, "different").String()}
	body, err := storageformat.EncodeCanonical(misbound)
	if err != nil {
		t.Fatal(err)
	}
	root, err := session.buildTree(ctx, []storageformat.DomainEntry{{Key: catalogEntryKey(reference), Value: body, LogicalVersion: "version"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := catalog.entryAt(ctx, storageformat.DomainCatalogHead{Root: root}, reference); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("misbound catalog entry error = %v", err)
	}

	valid := storageformat.DomainCatalogEntry{DomainID: reference.ID, Kind: reference.Kind, HeadKey: storageformat.DomainHeadKey(reference.Kind, reference.ID).String()}
	body, err = storageformat.EncodeCanonical(valid)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := session.buildTree(ctx, []storageformat.DomainEntry{{Key: catalogEntryKey(reference), Value: body, LogicalVersion: "version"}})
	if err != nil {
		t.Fatal(err)
	}
	branch := storageformat.DomainPage{SchemaVersion: 1, DomainID: "__catalog__", Kind: storageformat.DomainAdmin, Level: 1, Children: []storageformat.DomainPageChild{
		{FirstKey: catalogEntryKey(reference), LastKey: catalogEntryKey(reference), Digest: leaf.Digest, Level: leaf.Level, EntryCount: leaf.EntryCount, ByteCount: leaf.ByteCount},
		{FirstKey: "z", LastKey: "z", Digest: storageformat.Digest([]byte("missing-catalog-child")), Level: 0, EntryCount: 1},
	}}
	branchReference, err := session.writePage(ctx, branch)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.visitEntries(ctx, storageformat.DomainCatalogHead{Root: branchReference.root}, func(storageformat.DomainCatalogEntry) error { return nil }); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("later catalog page error = %v", err)
	}

	oversized := storageformat.DomainCatalogHead{SchemaVersion: 1, Revision: 1, Root: storageformat.DomainTreeRoot{Digest: strings.Repeat("x", storageformat.MaxCanonicalBytes), Level: 0, EntryCount: 1}}
	if err := catalog.publish(ctx, domainCatalogSnapshot{}, oversized); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized catalog publication error = %v", err)
	}
}

func TestSchema008GateTransitionStateAndFailureMatrix(t *testing.T) {
	ctx := context.Background()
	t.Run("input-and-status-provider-failure", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		if err := engine.CloseWrites(ctx, ""); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("empty checkpoint close error = %v", err)
		}
		hooks := &hookedBackend{Backend: memory, get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "gate unavailable")
		}}
		engine.backend = hooks
		if _, err := engine.GateStatus(ctx); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("gate status error = %v", err)
		}
	})

	for name, mode := range map[string]storageformat.GateMode{
		"open": storageformat.GateOpen, "closing": storageformat.GateClosing,
	} {
		t.Run("finish-rejects-"+name, func(t *testing.T) {
			memory := objectmemory.New()
			engine := openNamespaceTestEngine(t, memory)
			rewriteCurrentGate(t, memory, func(gate *storageformat.WriteGate) {
				gate.Mode, gate.CheckpointID = mode, "other"
				if mode == storageformat.GateOpen {
					gate.CheckpointID = ""
				}
			})
			if err := engine.finishClosingWrites(ctx, "checkpoint"); !errors.Is(err, domain.ErrPreconditionFailed) {
				t.Fatalf("finish error = %v", err)
			}
		})
	}

	t.Run("already-closed-and-conflicting-close", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		rewriteCurrentGate(t, memory, func(gate *storageformat.WriteGate) {
			gate.Mode, gate.CheckpointID = storageformat.GateClosed, "checkpoint"
		})
		if err := engine.finishClosingWrites(ctx, "checkpoint"); err != nil {
			t.Fatal(err)
		}
		if err := engine.CloseWrites(ctx, "checkpoint"); err != nil {
			t.Fatal(err)
		}
		if err := engine.CloseWrites(ctx, "different"); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("different close error = %v", err)
		}
	})

	t.Run("closing-for-different-checkpoint", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		rewriteCurrentGate(t, memory, func(gate *storageformat.WriteGate) {
			gate.Mode, gate.CheckpointID = storageformat.GateClosing, "other"
		})
		if err := engine.CloseWrites(ctx, "checkpoint"); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("close error = %v", err)
		}
	})

	t.Run("closing-gate-changes-after-domain-freeze", func(t *testing.T) {
		memory := objectmemory.New()
		hooks := &hookedBackend{Backend: memory}
		engine := openNamespaceTestEngine(t, hooks)
		rewriteCurrentGate(t, memory, func(gate *storageformat.WriteGate) {
			gate.Mode, gate.CheckpointID = storageformat.GateClosing, "checkpoint"
		})
		seenCatalog := false
		hooks.put = func(callCtx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if key == storageformat.DomainCatalogHeadKey() {
				seenCatalog = true
				version, err := memory.Put(callCtx, key, body, condition)
				if err == nil {
					rewriteCurrentGate(t, memory, func(gate *storageformat.WriteGate) {
						gate.Mode, gate.CheckpointID = storageformat.GateOpen, ""
					})
				}
				return version, err
			}
			return memory.Put(callCtx, key, body, condition)
		}
		if err := engine.finishClosingWrites(ctx, "checkpoint"); !errors.Is(err, domain.ErrPreconditionFailed) || !seenCatalog {
			t.Fatalf("finish after gate change error = %v, saw catalog=%v", err, seenCatalog)
		}
	})

	t.Run("final-gate-cas-lost-to-equivalent-winner", func(t *testing.T) {
		memory := objectmemory.New()
		hooks := &hookedBackend{Backend: memory}
		engine := openNamespaceTestEngine(t, hooks)
		rewriteCurrentGate(t, memory, func(gate *storageformat.WriteGate) {
			gate.Mode, gate.CheckpointID = storageformat.GateClosing, "checkpoint"
		})
		lost := false
		hooks.put = func(callCtx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if key == storageformat.WriteGateKey() && !lost {
				lost = true
				rewriteCurrentGate(t, memory, func(gate *storageformat.WriteGate) {
					gate.Mode, gate.CheckpointID = storageformat.GateClosed, "checkpoint"
				})
				return "", domain.NewError(domain.ErrorPreconditionFailed, "lost gate CAS")
			}
			return memory.Put(callCtx, key, body, condition)
		}
		if err := engine.finishClosingWrites(ctx, "checkpoint"); err != nil || !lost {
			t.Fatalf("equivalent winner error = %v, lost=%v", err, lost)
		}
	})
}

func TestSchema008CancelClosingGateRetriesOnlyConditionalRaces(t *testing.T) {
	ctx := context.Background()
	t.Run("open-and-misbound", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		if err := engine.cancelClosingWriteGate(ctx, "checkpoint"); err != nil {
			t.Fatal(err)
		}
		rewriteCurrentGate(t, memory, func(gate *storageformat.WriteGate) {
			gate.Mode, gate.CheckpointID = storageformat.GateClosing, "other"
		})
		if err := engine.cancelClosingWriteGate(ctx, "checkpoint"); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("misbound cancellation error = %v", err)
		}
	})

	t.Run("conditional-retry", func(t *testing.T) {
		memory := objectmemory.New()
		hooks := &hookedBackend{Backend: memory}
		engine := openNamespaceTestEngine(t, hooks)
		rewriteCurrentGate(t, memory, func(gate *storageformat.WriteGate) {
			gate.Mode, gate.CheckpointID = storageformat.GateClosing, "checkpoint"
		})
		attempts := 0
		hooks.put = func(callCtx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if key == storageformat.WriteGateKey() {
				attempts++
				if attempts == 1 {
					return "", domain.NewError(domain.ErrorConflict, "retry")
				}
			}
			return memory.Put(callCtx, key, body, condition)
		}
		if err := engine.cancelClosingWriteGate(ctx, "checkpoint"); err != nil || attempts != 2 {
			t.Fatalf("cancel error = %v attempts=%d", err, attempts)
		}
	})

	t.Run("provider-failure", func(t *testing.T) {
		memory := objectmemory.New()
		hooks := &hookedBackend{Backend: memory}
		engine := openNamespaceTestEngine(t, hooks)
		rewriteCurrentGate(t, memory, func(gate *storageformat.WriteGate) {
			gate.Mode, gate.CheckpointID = storageformat.GateClosing, "checkpoint"
		})
		hooks.put = func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return "", domain.NewError(domain.ErrorUnavailable, "write failed")
		}
		if err := engine.cancelClosingWriteGate(ctx, "checkpoint"); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("cancel provider error = %v", err)
		}
	})
}

func TestSchema008DomainCatalogCorruptionAndFreezeDenials(t *testing.T) {
	ctx := context.Background()
	if _, err := newDomainCatalog(objectmemory.New(), nil).freeze(ctx, 0); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("zero freeze error = %v", err)
	}
	if err := newDomainCatalog(objectmemory.New(), nil).unfreeze(ctx, 0); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("zero unfreeze error = %v", err)
	}
	if err := newDomainCatalog(objectmemory.New(), nil).visitEntries(ctx, storageformat.DomainCatalogHead{}, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("nil visitor error = %v", err)
	}

	memory := objectmemory.New()
	engine := openNamespaceTestEngine(t, memory)
	catalog := newDomainCatalog(memory, nil)
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:catalog-hardening"}
	if _, err := engine.stateDomainStore().mutate(ctx, reference, consistencyDomainMutation{ID: "catalog-seed", Changes: []consistencyDomainChange{{Key: "seed", Require: domainValueAbsent, Value: []byte("value")}}}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.register(ctx, reference); err != nil {
		t.Fatal(err)
	}
	if err := catalog.register(ctx, reference); err != nil {
		t.Fatalf("idempotent register error = %v", err)
	}
	if _, err := catalog.freeze(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.freeze(ctx, 8); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("different freeze error = %v", err)
	}
	if err := catalog.register(ctx, consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:frozen"}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("frozen registration error = %v", err)
	}
	if err := catalog.unfreeze(ctx, 8); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("different unfreeze error = %v", err)
	}
	if err := catalog.unfreeze(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if err := catalog.unfreeze(ctx, 7); err != nil {
		t.Fatalf("idempotent unfreeze error = %v", err)
	}
	key := storageformat.DomainCatalogHeadKey()
	object, err := memory.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Put(ctx, key, []byte("corrupt"), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.load(ctx); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt catalog error = %v", err)
	}
}

func TestSchema008GateAndCatalogProviderFailureBranches(t *testing.T) {
	ctx := context.Background()
	t.Run("close-gate-put", func(t *testing.T) {
		memory := objectmemory.New()
		hooks := &hookedBackend{Backend: memory}
		engine := openNamespaceTestEngine(t, hooks)
		hooks.put = func(_ context.Context, key objectstore.Key, _ []byte, _ objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if key == storageformat.WriteGateKey() {
				return "", domain.NewError(domain.ErrorUnavailable, "gate write failed")
			}
			return "", domain.NewError(domain.ErrorInternal, "unexpected write")
		}
		if err := engine.CloseWrites(ctx, "checkpoint"); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("close gate write error = %v", err)
		}
	})

	t.Run("finish-conflict-winner-read", func(t *testing.T) {
		memory := objectmemory.New()
		hooks := &hookedBackend{Backend: memory}
		engine := openNamespaceTestEngine(t, hooks)
		rewriteCurrentGate(t, memory, func(gate *storageformat.WriteGate) {
			gate.Mode, gate.CheckpointID = storageformat.GateClosing, "checkpoint"
		})
		gatePut := false
		hooks.put = func(callCtx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if key == storageformat.WriteGateKey() {
				gatePut = true
				return "", domain.NewError(domain.ErrorConflict, "lost")
			}
			return memory.Put(callCtx, key, body, condition)
		}
		hooks.get = func(callCtx context.Context, key objectstore.Key) (objectstore.Object, error) {
			if gatePut && key == storageformat.WriteGateKey() {
				return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "winner read failed")
			}
			return memory.Get(callCtx, key)
		}
		if err := engine.finishClosingWrites(ctx, "checkpoint"); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("winner read error = %v", err)
		}
	})

	t.Run("cancel-read", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		engine.backend = &hookedBackend{Backend: memory, get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "cancel read failed")
		}}
		if err := engine.cancelClosingWriteGate(ctx, "checkpoint"); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("cancel read error = %v", err)
		}
	})

	t.Run("open-resume-precondition", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		if _, err := engine.CreateCheckpoint(ctx, "checkpoint"); err != nil {
			t.Fatal(err)
		}
		if err := engine.openClosedWriteGate(ctx, "checkpoint"); err != nil {
			t.Fatal(err)
		}
		rewriteCurrentGate(t, memory, func(gate *storageformat.WriteGate) { gate.Epoch++ })
		if err := engine.OpenWrites(ctx, "checkpoint"); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("resume precondition error = %v", err)
		}
	})

	t.Run("catalog-head-and-entry-corruption", func(t *testing.T) {
		memory := objectmemory.New()
		catalog := newDomainCatalog(memory, nil)
		key := storageformat.DomainCatalogHeadKey()
		invalidHead := storageformat.DomainCatalogHead{SchemaVersion: 2}
		body, err := storageformat.EncodeEnvelope(domainCatalogHeadSchema, key, 1, invalidHead)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := memory.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.load(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid catalog head error = %v", err)
		}

		memory = objectmemory.New()
		catalog = newDomainCatalog(memory, nil)
		reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:entry-corrupt"}
		session := newDomainCatalogTreeSession(catalog.store)
		root, err := session.buildTree(ctx, []storageformat.DomainEntry{{Key: catalogEntryKey(reference), Value: []byte("bad"), LogicalVersion: "version"}})
		if err != nil {
			t.Fatal(err)
		}
		head := storageformat.DomainCatalogHead{SchemaVersion: 1, Root: root}
		if _, _, err := catalog.entryAt(ctx, head, reference); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid entry error = %v", err)
		}
		if err := catalog.visitEntries(ctx, head, func(storageformat.DomainCatalogEntry) error { return nil }); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid visited entry error = %v", err)
		}

		validEntry := storageformat.DomainCatalogEntry{DomainID: reference.ID, Kind: reference.Kind, HeadKey: storageformat.DomainHeadKey(reference.Kind, reference.ID).String()}
		validBody, err := storageformat.EncodeCanonical(validEntry)
		if err != nil {
			t.Fatal(err)
		}
		root, err = session.buildTree(ctx, []storageformat.DomainEntry{{Key: catalogEntryKey(reference), Value: validBody, LogicalVersion: "version"}})
		if err != nil {
			t.Fatal(err)
		}
		head.Root = root
		visitorFailure := domain.NewError(domain.ErrorUnavailable, "visitor failed")
		if err := catalog.visitEntries(ctx, head, func(storageformat.DomainCatalogEntry) error { return visitorFailure }); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("visitor error = %v", err)
		}
	})
}
