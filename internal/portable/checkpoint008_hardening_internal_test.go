package portable

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type checkpointAbortFailureBackend struct {
	*objectmemory.Backend
	failure error
}

func (backend *checkpointAbortFailureBackend) AbortUpload(context.Context, []byte) error {
	return backend.failure
}

func TestSchema008CheckpointClosureRejectsMisboundDomainsAndControlValues(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	if err := engine.validateSchema008CheckpointClosure(ctx, 1); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("unfrozen checkpoint closure error = %v", err)
	}
	controlReference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:checkpoint-control"}
	if err := engine.validateConsistencyDomainClosure(ctx, controlReference, storageformat.DomainHead{}); err != nil {
		t.Fatalf("empty control closure = %v", err)
	}
	invalidNamespace := consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "invalid-owner"}
	if err := engine.validateConsistencyDomainClosure(ctx, invalidNamespace, storageformat.DomainHead{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid namespace owner closure error = %v", err)
	}

	for name, key := range map[string]string{
		"upload":             "upload/bad",
		"upload-idempotency": "upload-idempotency/bad",
	} {
		t.Run(name, func(t *testing.T) {
			head := storageformat.DomainHead{Deltas: []storageformat.DomainDelta{{Changes: []storageformat.DomainChange{{Key: key, Value: []byte("{}"), LogicalVersion: "version"}}}}}
			if err := engine.validateKnownControlDomainValues(ctx, consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: namespaceTestScope(t, domain.AreaLive).UserID().String()}, head); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("invalid control value error = %v", err)
			}
		})
	}
	if err := engine.validateKnownControlDomainValues(ctx, consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:unrelated"}, storageformat.DomainHead{}); err != nil {
		t.Fatalf("unrelated control domain closure = %v", err)
	}
}

func TestSchema008CheckpointClosureRejectsEveryTreeAndNestedBindingFailure(t *testing.T) {
	ctx := context.Background()
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:checkpoint-tree-errors"}

	t.Run("missing-authority-root", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		head := storageformat.DomainHead{Base: storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("missing-root")), Level: 0, EntryCount: 1}}
		if err := engine.validateConsistencyDomainClosure(ctx, reference, head); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing authority root error = %v", err)
		}
	})

	t.Run("later-authority-page", func(t *testing.T) {
		backend := objectmemory.New()
		engine := openNamespaceTestEngine(t, backend)
		session := newConsistencyDomainTreeSession(engine.stateDomainStore(), reference)
		entries := make([]storageformat.DomainEntry, domainPageMaximumItems+1)
		for index := range entries {
			entries[index] = storageformat.DomainEntry{Key: fmt.Sprintf("valid-%04d", index), Value: []byte("value"), LogicalVersion: "version"}
		}
		root, err := session.buildTree(ctx, entries)
		if err != nil {
			t.Fatal(err)
		}
		rootObject, err := backend.Get(ctx, storageformat.DomainPageKey(reference.Kind, reference.ID, root.Digest))
		if err != nil {
			t.Fatal(err)
		}
		var rootPage storageformat.DomainPage
		if err := decodeCanonicalValue(rootObject.Body, &rootPage); err != nil || len(rootPage.Children) < 2 {
			t.Fatalf("branch page = %+v, %v", rootPage, err)
		}
		missing := storageformat.DomainPageKey(reference.Kind, reference.ID, rootPage.Children[len(rootPage.Children)-1].Digest)
		object, err := backend.Get(ctx, missing)
		if err != nil {
			t.Fatal(err)
		}
		if err := backend.Delete(ctx, missing, objectstore.DeleteCondition{Version: object.Version}); err != nil {
			t.Fatal(err)
		}
		if err := engine.validateConsistencyDomainClosure(ctx, reference, storageformat.DomainHead{Base: root}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("later authority page error = %v", err)
		}
	})

	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	namespaceReference := namespaceReference(owner)
	validFile := func(t *testing.T, name string) []byte {
		t.Helper()
		entry := validNamespaceTestFile(t, openNamespaceTestEngine(t, objectmemory.New()), name, 1)
		body, err := encodeNamespaceEntry(entry)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	for name, body := range map[string][]byte{
		"malformed-root": []byte("not canonical"),
		"named-root":     validFile(t, "named-root"),
	} {
		t.Run(name, func(t *testing.T) {
			engine := openNamespaceTestEngine(t, objectmemory.New())
			head := storageformat.DomainHead{Deltas: []storageformat.DomainDelta{{Changes: []storageformat.DomainChange{{Key: namespaceRootKey(domain.AreaLive), Value: body, LogicalVersion: "version"}}}}}
			if err := engine.validateConsistencyDomainClosure(ctx, namespaceReference, head); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("namespace root error = %v", err)
			}
		})
	}

	t.Run("invalid-delta-result", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		head := storageformat.DomainHead{Deltas: []storageformat.DomainDelta{{Result: []byte("not canonical")}}}
		if err := engine.validateConsistencyDomainClosure(ctx, namespaceReference, head); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid delta result error = %v", err)
		}
	})

	t.Run("invalid-upload-idempotency-target", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		keyDigest := storageformat.Digest([]byte("invalid-target"))
		idempotencyBody, err := storageformat.EncodeCanonical(storageformat.PortableUploadIdempotency{SchemaVersion: 1, OwnerID: owner.String(), KeyDigest: keyDigest, Fingerprint: storageformat.Digest([]byte("request")), UploadID: "bad-target"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.stateDomainStore().mutate(ctx, namespaceReference, consistencyDomainMutation{ID: "invalid-upload-target", Changes: []consistencyDomainChange{
			{Key: uploadRecordKey("bad-target"), Require: domainValueAbsent, Value: []byte("not canonical")},
			{Key: "upload-idempotency/" + keyDigest, Require: domainValueAbsent, Value: idempotencyBody},
		}}); err != nil {
			t.Fatal(err)
		}
		snapshot, err := engine.stateDomainStore().loadHead(ctx, namespaceReference)
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.validateKnownControlDomainValues(ctx, namespaceReference, snapshot.head); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid upload target error = %v", err)
		}
	})

	t.Run("unknown-control-key", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		control := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:bad-key"}
		if _, err := engine.stateDomainStore().mutate(ctx, control, consistencyDomainMutation{ID: "invalid-control-key", Changes: []consistencyDomainChange{{Key: "%", Require: domainValueAbsent, Value: []byte("value")}}}); err != nil {
			t.Fatal(err)
		}
		snapshot, err := engine.stateDomainStore().loadHead(ctx, control)
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.validateKnownControlDomainValues(ctx, control, snapshot.head); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("unknown control key error = %v", err)
		}
	})

	t.Run("misbound-namespace-child", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		session := newConsistencyDomainTreeSession(engine.stateDomainStore(), namespaceReference)
		file := validNamespaceTestFile(t, engine, "actual", 1)
		body, err := encodeNamespaceEntry(file)
		if err != nil {
			t.Fatal(err)
		}
		children, err := session.buildTree(ctx, []storageformat.DomainEntry{{Key: "different", Value: body, LogicalVersion: file.Entry.LogicalVersion}})
		if err != nil {
			t.Fatal(err)
		}
		root := storageformat.NamespaceEntry{SchemaVersion: 1, NodeID: "root", Entry: storageformat.DirectoryEntry{Kind: domain.EntryDirectory, DirectoryID: "root"}, Children: children, EntryCount: 1}
		if err := engine.validateNamespaceEntryClosure(ctx, session, owner, namespaceRootPath(), root); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("misbound child error = %v", err)
		}
	})

	t.Run("nested-invalid-outcome", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		session := newConsistencyDomainTreeSession(engine.stateDomainStore(), namespaceReference)
		outcome := storageformat.DomainOutcome{MutationID: "nested", Fingerprint: storageformat.Digest([]byte("nested")), Revision: 1, RetainUntil: engine.clock.Now().Add(time.Hour), Result: []byte("not canonical")}
		body, err := storageformat.EncodeCanonical(outcome)
		if err != nil {
			t.Fatal(err)
		}
		root, err := session.buildTree(ctx, []storageformat.DomainEntry{{Key: outcome.MutationID, Value: body, LogicalVersion: "version"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.validateNamespaceOutcomeClosure(ctx, session, root); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("nested invalid outcome error = %v", err)
		}
	})

	t.Run("missing-batch-item-root", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		session := newConsistencyDomainTreeSession(engine.stateDomainStore(), namespaceReference)
		now := engine.clock.Now().UTC()
		operation := domain.Operation{ID: "missing-items", State: domain.OperationSucceeded, StartedAt: now, UpdatedAt: now}
		result := storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: storageformat.Digest([]byte("missing-items")), Batch: &storageformat.NamespaceBatch{Operation: operation, Items: storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("missing-items")), Level: 0, EntryCount: 1}, ItemCount: 1}}
		body, err := storageformat.EncodeCanonical(result)
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.validateNamespaceMutationResultClosure(ctx, session, body); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing batch root error = %v", err)
		}
	})
}

func TestSchema008CheckpointReachabilityWalkerFailsClosedAtEveryNestedBoundary(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	reference := namespaceReference(owner)
	session := newConsistencyDomainTreeSession(engine.stateDomainStore(), reference)
	root, err := session.buildTree(ctx, []storageformat.DomainEntry{{Key: "entry", Value: []byte("value"), LogicalVersion: "version"}})
	if err != nil {
		t.Fatal(err)
	}

	newWalker := func(t *testing.T) *checkpointReachabilityWalker {
		t.Helper()
		collector, err := newCheckpointReachabilityCollector()
		if err != nil {
			t.Fatal(err)
		}
		visited, err := newCheckpointVisitSet()
		if err != nil {
			_ = collector.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = collector.Close(); _ = visited.Close() })
		return &checkpointReachabilityWalker{engine: engine, collector: collector, visited: visited}
	}

	t.Run("closed-visited-set", func(t *testing.T) {
		walker := newWalker(t)
		if err := walker.visited.Close(); err != nil {
			t.Fatal(err)
		}
		if err := walker.walkTree(ctx, session, root, "closed-visited", nil); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("closed visited-set error = %v", err)
		}
	})

	t.Run("missing-page", func(t *testing.T) {
		walker := newWalker(t)
		missing := storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("missing-walk-page")), Level: 0, EntryCount: 1}
		if err := walker.walkTree(ctx, session, missing, "missing", nil); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing walk page error = %v", err)
		}
	})

	t.Run("closed-collector", func(t *testing.T) {
		walker := newWalker(t)
		if err := walker.collector.Close(); err != nil {
			t.Fatal(err)
		}
		if err := walker.walkTree(ctx, session, root, "closed-collector", nil); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("closed collector error = %v", err)
		}
	})

	t.Run("visitor-and-revisit", func(t *testing.T) {
		walker := newWalker(t)
		failure := domain.NewError(domain.ErrorUnavailable, "visitor failed")
		if err := walker.walkTree(ctx, session, root, "visitor", func(storageformat.DomainEntry) error { return failure }); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("visitor error = %v", err)
		}
		if err := walker.walkTree(ctx, session, root, "visitor", func(storageformat.DomainEntry) error { return errors.New("must not revisit") }); err != nil {
			t.Fatalf("revisited tree error = %v", err)
		}
	})

	t.Run("namespace-entry-denials", func(t *testing.T) {
		walker := newWalker(t)
		file := validNamespaceTestFile(t, engine, "file", 1)
		if err := walker.walkNamespaceEntry(ctx, session, owner, file); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("file root error = %v", err)
		}
		malformedChildren, err := session.buildTree(ctx, []storageformat.DomainEntry{{Key: "bad", Value: []byte("not canonical"), LogicalVersion: "version"}})
		if err != nil {
			t.Fatal(err)
		}
		directory := storageformat.NamespaceEntry{SchemaVersion: 1, NodeID: "directory", Entry: storageformat.DirectoryEntry{Kind: domain.EntryDirectory, DirectoryID: "directory"}, Children: malformedChildren, EntryCount: 1}
		if err := walker.walkNamespaceEntry(ctx, session, owner, directory); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("malformed child error = %v", err)
		}
	})

	t.Run("mutation-result-denials", func(t *testing.T) {
		walker := newWalker(t)
		if err := walker.walkNamespaceMutationResult(ctx, session, []byte("not canonical")); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("malformed mutation result error = %v", err)
		}
		now := engine.clock.Now().UTC()
		operation := domain.Operation{ID: "walk-missing-items", State: domain.OperationSucceeded, StartedAt: now, UpdatedAt: now}
		body, err := storageformat.EncodeCanonical(storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: storageformat.Digest([]byte("walk-missing-items")), Batch: &storageformat.NamespaceBatch{Operation: operation, Items: storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("walk-missing-items")), Level: 0, EntryCount: 1}, ItemCount: 1}})
		if err != nil {
			t.Fatal(err)
		}
		if err := walker.walkNamespaceMutationResult(ctx, session, body); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing mutation batch page error = %v", err)
		}
	})
}

func TestSchema008CheckpointClosureBindsEveryNamespaceAndControlAuthorityValue(t *testing.T) {
	ctx := context.Background()
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	reference := namespaceReference(owner)
	now := time.Date(2063, 4, 5, 6, 7, 8, 0, time.UTC)
	upload := storageformat.PortableUploadRecord{
		SchemaVersion: 1, UploadID: "upload", OwnerID: owner.String(), Area: "live", RequestedPath: "/file.bin", ResolvedPath: "/file.bin",
		BlobID: "upload", Size: 4, MediaType: "application/octet-stream", Conflict: domain.ConflictFail,
		State: storageformat.UploadCompleted, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	uploadBody, err := storageformat.EncodeCanonical(upload)
	if err != nil {
		t.Fatal(err)
	}
	keyDigest := storageformat.Digest([]byte("request-key"))
	idempotency := storageformat.PortableUploadIdempotency{SchemaVersion: 1, OwnerID: owner.String(), KeyDigest: keyDigest, Fingerprint: storageformat.Digest([]byte("request")), UploadID: upload.UploadID}
	idempotencyBody, err := storageformat.EncodeCanonical(idempotency)
	if err != nil {
		t.Fatal(err)
	}

	headFor := func(t *testing.T, reference consistencyDomainRef, changes []consistencyDomainChange) (*Engine, storageformat.DomainHead) {
		t.Helper()
		engine := openNamespaceTestEngine(t, objectmemory.New())
		if _, err := engine.stateDomainStore().mutate(ctx, reference, consistencyDomainMutation{ID: "seed", Changes: changes}); err != nil {
			t.Fatal(err)
		}
		snapshot, err := engine.stateDomainStore().loadHead(ctx, reference)
		if err != nil {
			t.Fatal(err)
		}
		return engine, snapshot.head
	}

	t.Run("valid-upload-idempotency-edge", func(t *testing.T) {
		engine, head := headFor(t, reference, []consistencyDomainChange{
			{Key: uploadRecordKey(upload.UploadID), Require: domainValueAbsent, Value: uploadBody},
			{Key: "upload-idempotency/" + keyDigest, Require: domainValueAbsent, Value: idempotencyBody},
		})
		if err := engine.validateKnownControlDomainValues(ctx, reference, head); err != nil {
			t.Fatal(err)
		}
	})

	for name, changes := range map[string][]consistencyDomainChange{
		"missing-upload-target": {{Key: "upload-idempotency/" + keyDigest, Require: domainValueAbsent, Value: idempotencyBody}},
		"misbound-idempotency-key": {
			{Key: uploadRecordKey(upload.UploadID), Require: domainValueAbsent, Value: uploadBody},
			{Key: "upload-idempotency/" + storageformat.Digest([]byte("other")), Require: domainValueAbsent, Value: idempotencyBody},
		},
		"unknown-namespace-value": {{Key: "unknown/value", Require: domainValueAbsent, Value: []byte("opaque")}},
	} {
		t.Run(name, func(t *testing.T) {
			engine, head := headFor(t, reference, changes)
			if err := engine.validateKnownControlDomainValues(ctx, reference, head); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("closure error = %v", err)
			}
		})
	}

	controlReference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:" + owner.String()}
	validStateKey := state.MustKey(state.NamespaceAccounts, owner.String())
	engine, head := headFor(t, controlReference, []consistencyDomainChange{{Key: validStateKey.String(), Require: domainValueAbsent, Value: []byte("opaque application state")}})
	if err := engine.validateKnownControlDomainValues(ctx, controlReference, head); err != nil {
		t.Fatalf("valid routed state = %v", err)
	}

	otherOwner, err := domain.ParseUserID("WFhYWFhYWFhYWFhYWFhYWA")
	if err != nil {
		t.Fatal(err)
	}
	misrouted := state.MustKey(state.NamespaceAccounts, otherOwner.String())
	engine, head = headFor(t, controlReference, []consistencyDomainChange{{Key: misrouted.String(), Require: domainValueAbsent, Value: []byte("wrong owner")}})
	if err := engine.validateKnownControlDomainValues(ctx, controlReference, head); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("cross-domain state error = %v", err)
	}

	groupID := storageformat.Digest([]byte("group"))
	ignoreBody, _ := storageformat.EncodeCanonical(domain.DuplicateIgnore{GroupID: groupID, Ignored: true, Revision: 1})
	pairID := storageformat.Digest([]byte("pair"))
	pairBody, _ := storageformat.EncodeCanonical(storageformat.DuplicateDirectoryPreference{SchemaVersion: 1, PairID: pairID, LeftIdentity: "a", RightIdentity: "b", Ignored: true, Revision: 1})
	engine, head = headFor(t, controlReference, []consistencyDomainChange{
		{Key: duplicateIgnoreKey008(groupID), Require: domainValueAbsent, Value: ignoreBody},
		{Key: duplicateDirectoryIgnoreKey008(pairID), Require: domainValueAbsent, Value: pairBody},
	})
	if err := engine.validateKnownControlDomainValues(ctx, controlReference, head); err != nil {
		t.Fatalf("valid duplicate preferences = %v", err)
	}
	badIgnore, _ := storageformat.EncodeCanonical(domain.DuplicateIgnore{GroupID: storageformat.Digest([]byte("other")), Ignored: true, Revision: 1})
	engine, head = headFor(t, controlReference, []consistencyDomainChange{{Key: duplicateIgnoreKey008(groupID), Require: domainValueAbsent, Value: badIgnore}})
	if err := engine.validateKnownControlDomainValues(ctx, controlReference, head); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("misbound duplicate preference error = %v", err)
	}
}

func TestSchema009CheckpointClosureBindsTypedValuesToInvariantDomains(t *testing.T) {
	ctx := context.Background()
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	reference := consistencyDomainRef{Kind: storageformat.DomainIdentity, ID: "owner:" + owner.String()}
	key := state.MustKey(state.NamespaceAccounts, owner.String())

	headFor := func(t *testing.T, body []byte) (*Engine, storageformat.DomainHead) {
		t.Helper()
		engine := openNamespaceTestEngine(t, objectmemory.New())
		if _, err := engine.stateDomainStore().mutate(ctx, reference, consistencyDomainMutation{ID: "seed", Changes: []consistencyDomainChange{{Key: key.String(), Require: domainValueAbsent, Value: body}}}); err != nil {
			t.Fatal(err)
		}
		snapshot, err := engine.stateDomainStore().loadHead(ctx, reference)
		if err != nil {
			t.Fatal(err)
		}
		return engine, snapshot.head
	}

	account, err := storageformat.EncodeStateRecord009(storageformat.StateRecordAccount, []byte("opaque account payload"))
	if err != nil {
		t.Fatal(err)
	}
	engine, head := headFor(t, account)
	if err := engine.validateKnownControlDomainValuesForSchema(ctx, reference, head, true); err != nil {
		t.Fatalf("valid schema-009 authority = %v", err)
	}
	if err := engine.validateKnownControlDomainValues(ctx, reference, head); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("schema-009 authority accepted by schema-008 routing = %v", err)
	}

	for name, body := range map[string][]byte{
		"untyped": []byte("opaque account payload"),
		"wrong-type": func() []byte {
			value, encodeErr := storageformat.EncodeStateRecord009(storageformat.StateRecordSession, []byte("opaque account payload"))
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			return value
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			engine, head := headFor(t, body)
			if err := engine.validateKnownControlDomainValuesForSchema(ctx, reference, head, true); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("invalid schema-009 authority error = %v", err)
			}
		})
	}
}

func TestSchema008CheckpointClosurePreservesProviderFailureClassification(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	engine := openNamespaceTestEngine(t, backend)
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	reference := namespaceReference(owner)
	now := time.Date(2063, 4, 5, 6, 7, 8, 0, time.UTC)
	upload := storageformat.PortableUploadRecord{
		SchemaVersion: 1, UploadID: "upload", OwnerID: owner.String(), Area: "live", RequestedPath: "/file.bin", ResolvedPath: "/file.bin",
		BlobID: "upload", Size: 4, MediaType: "application/octet-stream", Conflict: domain.ConflictFail,
		State: storageformat.UploadCompleted, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	uploadBody, err := storageformat.EncodeCanonical(upload)
	if err != nil {
		t.Fatal(err)
	}
	keyDigest := storageformat.Digest([]byte("request-key"))
	idempotency := storageformat.PortableUploadIdempotency{SchemaVersion: 1, OwnerID: owner.String(), KeyDigest: keyDigest, Fingerprint: storageformat.Digest([]byte("request")), UploadID: upload.UploadID}
	idempotencyBody, err := storageformat.EncodeCanonical(idempotency)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.stateDomainStore().mutate(ctx, reference, consistencyDomainMutation{ID: "seed", Changes: []consistencyDomainChange{
		{Key: uploadRecordKey(upload.UploadID), Require: domainValueAbsent, Value: uploadBody},
		{Key: "upload-idempotency/" + keyDigest, Require: domainValueAbsent, Value: idempotencyBody},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := engine.stateDomainStore().compact(ctx, reference); err != nil {
		t.Fatal(err)
	}
	head, err := engine.stateDomainStore().loadHead(ctx, reference)
	if err != nil {
		t.Fatal(err)
	}
	hooks := &hookedBackend{Backend: backend, get: func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
		if strings.Contains(key.String(), "/pages/") || strings.Contains(key.String(), "/packs/") {
			return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "domain page temporarily unavailable")
		}
		return backend.Get(ctx, key)
	}}
	engine.backend = hooks
	if err := engine.validateKnownControlDomainValues(ctx, reference, head.head); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("checkpoint closure provider failure = %v", err)
	}
}

func TestSchema008CheckpointAuthorityKeyAllowlistExcludesRetiredAndProjectionState(t *testing.T) {
	owner := "WVhXWVhXWVhXWVhXWVhXWQ"
	reference := consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: owner}
	allowed := []objectstore.Key{
		storageformat.SuperblockKey(),
		storageformat.WriterSetKey(),
		storageformat.WriteGateKey(),
		storageformat.DomainCatalogHeadKey(),
		storageformat.DomainHeadKey(reference.Kind, reference.ID),
		storageformat.DomainPageKey(reference.Kind, reference.ID, storageformat.Digest([]byte("page"))),
	}
	for _, key := range allowed {
		if !isSchema008AuthorityStateKey(key.String()) {
			t.Fatalf("current authority key rejected: %s", key)
		}
	}
	excluded := []string{
		"endlessfs/v1/users/" + owner + "/live/dirs/root/root.json",
		"endlessfs/v1/state/accounts/" + owner + ".json",
		"endlessfs/v1/duplicate-projections/" + owner + "/head.json",
		"endlessfs/v1/domains/unknown/" + owner + "/head.json",
		"endlessfs/v1/domains/namespace//head.json",
		"endlessfs/v1/domains/namespace/" + owner + "/pages/not-json",
	}
	for _, key := range excluded {
		if isSchema008AuthorityStateKey(key) {
			t.Fatalf("non-authority key accepted: %s", key)
		}
	}
}

func TestSchema009NamespaceDeltaOpaqueResultClassification(t *testing.T) {
	if schema009NamespaceDeltaHasOpaqueResult(storageformat.DomainDelta{}) {
		t.Fatal("empty namespace delta was classified as opaque")
	}
	if !schema009NamespaceDeltaHasOpaqueResult(storageformat.DomainDelta{Changes: []storageformat.DomainChange{{Key: transitionLockKey009}}}) {
		t.Fatal("transition-lock delta was not classified as opaque")
	}
	key := state.MustKey(state.NamespaceTrash, "owner", "record")
	if !schema009NamespaceDeltaHasOpaqueResult(storageformat.DomainDelta{Changes: []storageformat.DomainChange{{Key: key.String()}}}) {
		t.Fatal("state delta was not classified as opaque")
	}
	if schema009NamespaceDeltaHasOpaqueResult(storageformat.DomainDelta{Changes: []storageformat.DomainChange{{Key: "namespace/live"}}}) {
		t.Fatal("namespace-only delta was classified as opaque")
	}
}

func TestSchema009TransitionReachabilityRejectsProviderAndBindingGaps(t *testing.T) {
	ctx := context.Background()
	newCollector := func(t *testing.T) *checkpointReachabilityCollector {
		t.Helper()
		collector, err := newCheckpointReachabilityCollector()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = collector.Close() })
		return collector
	}

	t.Run("list", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		engine.backend = &hookedBackend{Backend: memory, list: func(context.Context, objectstore.ListRequest) (objectstore.ListPage, error) {
			return objectstore.ListPage{}, domain.NewError(domain.ErrorUnavailable, "transition list failed")
		}}
		if err := engine.collectTransitionReachability009(ctx, newCollector(t)); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("transition list error = %v", err)
		}
	})

	t.Run("decision-read", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		key := storageformat.TransitionDecisionKey("read-failure")
		if _, err := memory.Put(ctx, key, []byte("body"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		engine.backend = &hookedBackend{Backend: memory, get: func(callCtx context.Context, requested objectstore.Key) (objectstore.Object, error) {
			if requested == key {
				return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "transition decision read failed")
			}
			return memory.Get(callCtx, requested)
		}}
		if err := engine.collectTransitionReachability009(ctx, newCollector(t)); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("transition decision read error = %v", err)
		}
	})

	t.Run("invalid-decision", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		key := storageformat.TransitionDecisionKey("invalid-decision")
		if _, err := memory.Put(ctx, key, []byte("invalid"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := engine.collectTransitionReachability009(ctx, newCollector(t)); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid transition decision error = %v", err)
		}
	})

	t.Run("missing-plan", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		decision := storageformat.TransitionDecision009{SchemaVersion: 1, TransitionID: "missing-plan", Fingerprint: storageformat.Digest([]byte("missing-plan")), Committed: true, DecidedAt: engine.clock.Now().UTC()}
		key := storageformat.TransitionDecisionKey(decision.TransitionID)
		body := encodeInternalEnvelope(t, transitionDecisionSchema009, key, 1, decision)
		if _, err := memory.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := engine.collectTransitionReachability009(ctx, newCollector(t)); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("decision without plan error = %v", err)
		}
	})
}

func TestSchema009CheckpointDrainAndTypedNamespaceStateBoundaries(t *testing.T) {
	ctx := context.Background()
	owner := namespaceTestScope(t, domain.AreaLive).UserID()

	t.Run("terminal-cleanup", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		record := checkpointUploadRecord(engine, owner, "checkpoint-cleanup", engine.clock.Now().Add(time.Hour))
		record.State, record.CleanupPending = storageformat.UploadCompleted, true
		seedCheckpointUploadRecord(t, engine, owner, record)
		if err := engine.drainExpiredSchema008Uploads(ctx); err != nil {
			t.Fatal(err)
		}
		current, _, err := engine.Files().portableUpload(ctx, owner, record.UploadID)
		if err != nil || current.CleanupPending {
			t.Fatalf("drained terminal upload = %+v, %v", current, err)
		}
	})

	t.Run("typed-state", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		reference := namespaceReference(owner)
		for name, change := range map[string]storageformat.DomainChange{
			"unknown-key":  {Key: "unknown", Value: []byte("value"), LogicalVersion: "version"},
			"wrong-route":  {Key: state.MustKey(state.NamespaceAccounts, owner.String()).String(), Value: []byte("value"), LogicalVersion: "version"},
			"invalid-type": {Key: state.MustKey(state.NamespaceTrash, owner.String(), "record").String(), Value: []byte("invalid"), LogicalVersion: "version"},
		} {
			t.Run(name, func(t *testing.T) {
				head := storageformat.DomainHead{SchemaVersion: 1, Registered: true, DomainID: reference.ID, Kind: reference.Kind, Revision: 1, Deltas: []storageformat.DomainDelta{{MutationID: "mutation", Fingerprint: storageformat.Digest([]byte("mutation")), Revision: 1, RetainUntil: engine.clock.Now().Add(time.Hour), Changes: []storageformat.DomainChange{change}}}}
				if err := engine.validateKnownControlDomainValuesForSchema(ctx, reference, head, true); !errors.Is(err, domain.ErrInvalid) {
					t.Fatalf("typed namespace state error = %v", err)
				}
			})
		}
	})
}

func TestSchema008CheckpointRejectsRegisteredHeadMissingFromCatalog(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	engine := openNamespaceTestEngine(t, backend)
	legitimate := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:legitimate"}
	if err := engine.stateDomainStore().ensureRegistered(ctx, legitimate); err != nil {
		t.Fatal(err)
	}

	orphan := consistencyDomainRef{Kind: storageformat.DomainShare, ID: "orphan-registered-head"}
	head := storageformat.DomainHead{SchemaVersion: 1, DomainID: orphan.ID, Kind: orphan.Kind, Registered: true}
	key := storageformat.DomainHeadKey(orphan.Kind, orphan.ID)
	body, err := storageformat.EncodeEnvelope(domainHeadSchema, key, 1, head)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}

	stream, err := newSchema008CheckpointMetadataStream(ctx, engine)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for {
		_, _, found, nextErr := stream.next(ctx)
		if errors.Is(nextErr, domain.ErrPreconditionFailed) {
			return
		}
		if nextErr != nil {
			t.Fatalf("checkpoint metadata stream error = %v", nextErr)
		}
		if !found {
			t.Fatal("checkpoint accepted a registered head missing from the domain catalog")
		}
	}
}

func TestSchema008CheckpointExcludesInertPreRegistrationHead(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	engine := openNamespaceTestEngine(t, backend)
	legitimate := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:legitimate-inert"}
	if err := engine.stateDomainStore().ensureRegistered(ctx, legitimate); err != nil {
		t.Fatal(err)
	}

	inert := consistencyDomainRef{Kind: storageformat.DomainShare, ID: "crashed-before-registration"}
	head := storageformat.DomainHead{SchemaVersion: 1, DomainID: inert.ID, Kind: inert.Kind}
	key := storageformat.DomainHeadKey(inert.Kind, inert.ID)
	body, err := storageformat.EncodeEnvelope(domainHeadSchema, key, 1, head)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}

	stream, err := newSchema008CheckpointMetadataStream(ctx, engine)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for {
		info, _, found, nextErr := stream.next(ctx)
		if nextErr != nil {
			t.Fatalf("checkpoint rejected inert pre-registration head: %v", nextErr)
		}
		if !found {
			return
		}
		if info.Key == key {
			t.Fatal("checkpoint included inert pre-registration head")
		}
	}
}

func TestSchema008CheckpointNamespaceEntryAndBlobMetadataDenialMatrix(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	engine := openNamespaceTestEngine(t, backend)
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	session := newConsistencyDomainTreeSession(engine.stateDomainStore(), namespaceReference(owner))
	now := engine.clock.Now().UTC()
	file := storageformat.NamespaceEntry{SchemaVersion: 1, NodeID: "file-node", Entry: storageformat.DirectoryEntry{Name: "file.bin", NameDigest: storageformat.NameDigest("file.bin"), Kind: domain.EntryFile, BlobID: "blob", Size: 4, MediaType: "application/octet-stream", MD5: testProviderMD5, CRC32C: testProviderCRC32C, ModifiedAt: now}}
	file.Entry.LogicalVersion, _ = directoryEntryVersion(file.Entry)
	fileBody, err := encodeNamespaceEntry(file)
	if err != nil {
		t.Fatal(err)
	}
	children, err := session.buildTree(ctx, []storageformat.DomainEntry{{Key: file.Entry.Name, Value: fileBody, LogicalVersion: file.Entry.LogicalVersion}})
	if err != nil {
		t.Fatal(err)
	}
	root := storageformat.NamespaceEntry{SchemaVersion: 1, NodeID: "root-live", Entry: storageformat.DirectoryEntry{Kind: domain.EntryDirectory, DirectoryID: "root-live", Size: file.Entry.Size, FileCount: 1, ModifiedAt: now}, Children: children, EntryCount: 1}

	if err := engine.validateNamespaceEntryClosure(ctx, session, owner, namespaceRootPath(), file); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("file closure root error = %v", err)
	}
	if err := engine.validateNamespaceEntryClosure(ctx, session, owner, namespaceRootPath(), root); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing blob closure error = %v", err)
	}
	blobKey := storageformat.BlobKey(owner.String(), file.Entry.BlobID)
	if _, err := backend.Put(ctx, blobKey, []byte("data"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if err := engine.validateNamespaceEntryClosure(ctx, session, owner, namespaceRootPath(), root); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("mismatched blob fingerprint error = %v", err)
	}
	fingerprint := objectstore.FingerprintFor([]byte("data"))
	file.Entry.MD5, file.Entry.CRC32C = fingerprint.MD5, fingerprint.CRC32C
	file.Entry.LogicalVersion, _ = directoryEntryVersion(file.Entry)
	fileBody, _ = encodeNamespaceEntry(file)
	validSession := newConsistencyDomainTreeSession(engine.stateDomainStore(), namespaceReference(owner))
	root.Children, err = validSession.buildTree(ctx, []storageformat.DomainEntry{{Key: file.Entry.Name, Value: fileBody, LogicalVersion: file.Entry.LogicalVersion}})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.validateNamespaceEntryClosure(ctx, validSession, owner, namespaceRootPath(), root); err != nil {
		t.Fatalf("valid namespace closure = %v", err)
	}
	root.EntryCount = 2
	if err := engine.validateNamespaceEntryClosure(ctx, validSession, owner, namespaceRootPath(), root); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("directory count mismatch error = %v", err)
	}
}

func TestSchema008CheckpointOutcomeAndBatchClosureDenialMatrix(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	session := newConsistencyDomainTreeSession(engine.stateDomainStore(), namespaceReference(owner))
	if err := engine.validateNamespaceMutationResultClosure(ctx, session, []byte("{}")); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid mutation result closure error = %v", err)
	}
	operation := domain.Operation{ID: "operation", State: domain.OperationSucceeded, StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	fingerprint := storageformat.Digest([]byte("fingerprint"))
	simpleBody, err := storageformat.EncodeCanonical(storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: fingerprint, Operation: &operation})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.validateNamespaceMutationResultClosure(ctx, session, simpleBody); err != nil {
		t.Fatalf("simple mutation result closure = %v", err)
	}

	invalidItems, err := session.buildTree(ctx, []storageformat.DomainEntry{{Key: "0000000000000000", Value: []byte("{}"), LogicalVersion: "version"}})
	if err != nil {
		t.Fatal(err)
	}
	invalidBatch, err := storageformat.EncodeCanonical(storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: fingerprint, Batch: &storageformat.NamespaceBatch{Operation: operation, Items: invalidItems, ItemCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.validateNamespaceMutationResultClosure(ctx, session, invalidBatch); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid batch item closure error = %v", err)
	}

	stored := storageformat.NamespaceBatchItem{Index: 0, Source: "/source", Destination: "/destination", OperationID: operation.ID, State: domain.OperationSucceeded}
	itemBody, err := storageformat.EncodeCanonical(stored)
	if err != nil {
		t.Fatal(err)
	}
	validItems, err := session.buildTree(ctx, []storageformat.DomainEntry{{Key: "0000000000000000", Value: itemBody, LogicalVersion: "version"}})
	if err != nil {
		t.Fatal(err)
	}
	countMismatch, err := storageformat.EncodeCanonical(storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: fingerprint, Batch: &storageformat.NamespaceBatch{Operation: operation, Items: validItems, ItemCount: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.validateNamespaceMutationResultClosure(ctx, session, countMismatch); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("batch count closure error = %v", err)
	}
	validBatch, err := storageformat.EncodeCanonical(storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: fingerprint, Batch: &storageformat.NamespaceBatch{Operation: operation, Items: validItems, ItemCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.validateNamespaceMutationResultClosure(ctx, session, validBatch); err != nil {
		t.Fatalf("valid batch closure = %v", err)
	}

	outcomeRoot, err := session.buildTree(ctx, []storageformat.DomainEntry{{Key: "outcome", Value: []byte("{}"), LogicalVersion: "version"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.validateNamespaceOutcomeClosure(ctx, session, outcomeRoot); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid outcome closure error = %v", err)
	}
	outcome := storageformat.DomainOutcome{MutationID: "outcome", Fingerprint: fingerprint, Revision: 1, RetainUntil: time.Now().UTC().Add(time.Hour), Result: simpleBody}
	outcomeBody, err := storageformat.EncodeCanonical(outcome)
	if err != nil {
		t.Fatal(err)
	}
	outcomeRoot, err = session.buildTree(ctx, []storageformat.DomainEntry{{Key: outcome.MutationID, Value: outcomeBody, LogicalVersion: "version"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.validateNamespaceOutcomeClosure(ctx, session, outcomeRoot); err != nil {
		t.Fatalf("valid outcome closure = %v", err)
	}
}

func seedCheckpointUploadRecord(t *testing.T, engine *Engine, owner domain.UserID, record storageformat.PortableUploadRecord) {
	t.Helper()
	body, err := storageformat.EncodeCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.stateDomainStore().mutate(context.Background(), namespaceReference(owner), consistencyDomainMutation{ID: "seed-upload-" + record.UploadID, Changes: []consistencyDomainChange{{Key: uploadRecordKey(record.UploadID), Require: domainValueAbsent, Value: body}}}); err != nil {
		t.Fatal(err)
	}
}

func checkpointUploadRecord(engine *Engine, owner domain.UserID, uploadID string, expiresAt time.Time) storageformat.PortableUploadRecord {
	createdAt := engine.clock.Now().UTC()
	if !expiresAt.After(createdAt) {
		createdAt = expiresAt.Add(-time.Hour).UTC()
	}
	return storageformat.PortableUploadRecord{
		SchemaVersion: 1, UploadID: uploadID, OwnerID: owner.String(), Area: "live", RequestedPath: "/" + uploadID + ".bin", ResolvedPath: "/" + uploadID + ".bin",
		BlobID: uploadID, Size: 4, MediaType: "application/octet-stream", Conflict: domain.ConflictFail,
		State: storageformat.UploadActive, CreatedAt: createdAt, ExpiresAt: expiresAt.UTC(),
	}
}

func TestSchema008CheckpointUploadDrainFailClosedMatrix(t *testing.T) {
	ctx := context.Background()
	owner := namespaceTestScope(t, domain.AreaLive).UserID()

	t.Run("active-capability-blocks-and-rolls-back-close", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		seedCheckpointUploadRecord(t, engine, owner, checkpointUploadRecord(engine, owner, "active", engine.clock.Now().Add(time.Hour)))
		if err := engine.CloseWrites(ctx, "active-upload-checkpoint"); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("close with active upload error = %v", err)
		}
		gate, err := engine.GateStatus(ctx)
		if err != nil || gate.Mode != storageformat.GateOpen {
			t.Fatalf("rolled-back gate = %+v, %v", gate, err)
		}
	})

	t.Run("invalid-record", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		if _, err := engine.stateDomainStore().mutate(ctx, namespaceReference(owner), consistencyDomainMutation{ID: "invalid-upload", Changes: []consistencyDomainChange{{Key: "upload/invalid", Require: domainValueAbsent, Value: []byte("bad")}}}); err != nil {
			t.Fatal(err)
		}
		if err := engine.drainExpiredSchema008Uploads(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid upload drain error = %v", err)
		}
	})

	t.Run("expired-without-lease", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		seedCheckpointUploadRecord(t, engine, owner, checkpointUploadRecord(engine, owner, "expired-no-lease", engine.clock.Now().Add(-time.Minute)))
		if err := engine.drainExpiredSchema008Uploads(ctx); err != nil {
			t.Fatalf("expired missing lease drain = %v", err)
		}
	})

	t.Run("expired-without-transfer-support", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		record := checkpointUploadRecord(engine, owner, "expired-no-transfer", engine.clock.Now().Add(-time.Minute))
		seedCheckpointUploadRecord(t, engine, owner, record)
		leaseKey := storageformat.LeaseKey(memory.BackendKind(), record.UploadID)
		if _, err := memory.Put(ctx, leaseKey, []byte(record.UploadID), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		engine.fileBackend = &hookedBackend{Backend: memory}
		if err := engine.drainExpiredSchema008Uploads(ctx); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("missing transfer support error = %v", err)
		}
	})

	t.Run("lease-read-and-delete-provider-failures", func(t *testing.T) {
		for _, operation := range []string{"get", "delete"} {
			t.Run(operation, func(t *testing.T) {
				memory := objectmemory.New()
				hooks := &hookedBackend{Backend: memory}
				engine := openNamespaceTestEngine(t, hooks)
				engine.fileBackend = memory
				record := checkpointUploadRecord(engine, owner, "expired-"+operation, engine.clock.Now().Add(-time.Minute))
				seedCheckpointUploadRecord(t, engine, owner, record)
				leaseKey := storageformat.LeaseKey(memory.BackendKind(), record.UploadID)
				if _, err := memory.Put(ctx, leaseKey, []byte("unknown-upload"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
					t.Fatal(err)
				}
				if operation == "get" {
					hooks.get = func(callCtx context.Context, key objectstore.Key) (objectstore.Object, error) {
						if key == leaseKey {
							return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "lease read failed")
						}
						return memory.Get(callCtx, key)
					}
				} else {
					hooks.delete = func(context.Context, objectstore.Key, objectstore.DeleteCondition) error {
						return domain.NewError(domain.ErrorUnavailable, "lease delete failed")
					}
				}
				if err := engine.drainExpiredSchema008Uploads(ctx); !errors.Is(err, domain.ErrUnavailable) {
					t.Fatalf("%s failure error = %v", operation, err)
				}
			})
		}
	})
}

func TestSchema008CheckpointUploadDrainAndClosureTraversalBoundaries(t *testing.T) {
	ctx := context.Background()
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	reference := namespaceReference(owner)

	t.Run("domain-head-read", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		if err := engine.stateDomainStore().ensureRegistered(ctx, reference); err != nil {
			t.Fatal(err)
		}
		hooks := &hookedBackend{Backend: memory, get: func(callCtx context.Context, key objectstore.Key) (objectstore.Object, error) {
			if key == storageformat.DomainHeadKey(reference.Kind, reference.ID) {
				return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "namespace head unavailable")
			}
			return memory.Get(callCtx, key)
		}}
		engine.backend = hooks
		if err := engine.drainExpiredSchema008Uploads(ctx); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("upload-drain head error = %v", err)
		}
	})

	t.Run("upload-list-page", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		record := checkpointUploadRecord(engine, owner, "missing-upload-page", engine.clock.Now().Add(-time.Minute))
		seedCheckpointUploadRecord(t, engine, owner, record)
		if err := engine.stateDomainStore().compact(ctx, reference); err != nil {
			t.Fatal(err)
		}
		snapshot, err := engine.stateDomainStore().loadHead(ctx, reference)
		if err != nil {
			t.Fatal(err)
		}
		key := domainTreeStorageKey(reference, snapshot.head.Base)
		object, err := memory.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if err := memory.Delete(ctx, key, objectstore.DeleteCondition{Version: object.Version}); err != nil {
			t.Fatal(err)
		}
		if err := engine.drainExpiredSchema008Uploads(ctx); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("upload-drain list error = %v", err)
		}
	})

	t.Run("provider-abort", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		record := checkpointUploadRecord(engine, owner, "abort-failure", engine.clock.Now().Add(-time.Minute))
		seedCheckpointUploadRecord(t, engine, owner, record)
		failure := domain.NewError(domain.ErrorUnavailable, "provider abort unavailable")
		transfers := &checkpointAbortFailureBackend{Backend: memory, failure: failure}
		engine.fileBackend = transfers
		leaseKey := storageformat.LeaseKey(transfers.BackendKind(), record.UploadID)
		if _, err := memory.Put(ctx, leaseKey, []byte("opaque lease"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := engine.drainExpiredSchema008Uploads(ctx); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("provider abort error = %v", err)
		}
	})

	t.Run("registered-but-unfrozen-domain", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		if err := engine.stateDomainStore().ensureRegistered(ctx, reference); err != nil {
			t.Fatal(err)
		}
		if _, err := newDomainCatalog(engine.backend, engine.scheduler).freeze(ctx, 9); err != nil {
			t.Fatal(err)
		}
		if err := engine.validateSchema008CheckpointClosure(ctx, 9); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("unfrozen domain closure error = %v", err)
		}
	})

	t.Run("known-control-invalid-owner", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		if err := engine.validateKnownControlDomainValues(ctx, consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "invalid-owner"}, storageformat.DomainHead{}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("known-control owner error = %v", err)
		}
	})

	t.Run("multi-page-control-values", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		session := newConsistencyDomainTreeSession(engine.stateDomainStore(), reference)
		entries := make([]storageformat.DomainEntry, domainPageMaximumItems+1)
		for index := range entries {
			uploadID := fmt.Sprintf("completed-%04d", index)
			record := checkpointUploadRecord(engine, owner, uploadID, engine.clock.Now().Add(time.Hour))
			record.State = storageformat.UploadCompleted
			body, err := storageformat.EncodeCanonical(record)
			if err != nil {
				t.Fatal(err)
			}
			entries[index] = storageformat.DomainEntry{Key: uploadRecordKey(uploadID), Value: body, LogicalVersion: storageformat.Digest(body)}
		}
		root, err := session.buildTree(ctx, entries)
		if err != nil {
			t.Fatal(err)
		}
		head := storageformat.DomainHead{Base: root}
		if err := engine.validateKnownControlDomainValues(ctx, reference, head); err != nil {
			t.Fatalf("multi-page control closure = %v", err)
		}
	})
}

func TestSchema008CheckpointCollectabilityMetadataFailureMatrix(t *testing.T) {
	ctx := context.Background()
	memory := objectmemory.New()
	engine := openNamespaceTestEngine(t, memory)
	stream := &schema008CheckpointMetadataStream{metadata: newCheckpointMetadataStream(engine, true)}
	if collectable, err := stream.collectableMetadata(ctx, objectstore.ObjectInfo{Key: objectstore.MustKey("endlessfs/v1/unrecognized")}, false); err != nil || collectable {
		t.Fatalf("unrecognized metadata collectable=%v error=%v", collectable, err)
	}

	reference := consistencyDomainRef{Kind: storageformat.DomainShare, ID: "collectable-head"}
	key := storageformat.DomainHeadKey(reference.Kind, reference.ID)
	hooks := &hookedBackend{Backend: memory, get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
		return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "head unavailable")
	}}
	failingEngine := *engine
	failingEngine.backend = hooks
	failing := &schema008CheckpointMetadataStream{metadata: newCheckpointMetadataStream(&failingEngine, true)}
	if _, err := failing.collectableMetadata(ctx, objectstore.ObjectInfo{Key: key}, false); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("collectable head read error = %v", err)
	}

	if _, err := memory.Put(ctx, key, []byte("not canonical"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.collectableMetadata(ctx, objectstore.ObjectInfo{Key: key}, false); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("collectable corrupt head error = %v", err)
	}

	misboundKey := storageformat.DomainHeadKey(storageformat.DomainShare, "misbound-key")
	misbound := storageformat.DomainHead{SchemaVersion: 1, DomainID: "another-domain", Kind: storageformat.DomainShare}
	body, err := storageformat.EncodeEnvelope(domainHeadSchema, misboundKey, 1, misbound)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Put(ctx, misboundKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if collectable, err := stream.collectableMetadata(ctx, objectstore.ObjectInfo{Key: misboundKey}, false); err != nil || collectable {
		t.Fatalf("misbound inert head collectable=%v error=%v", collectable, err)
	}
}

func TestSchema008CheckpointOutcomeAndPreferenceCorruptionDenials(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	reference := namespaceReference(owner)
	session := newConsistencyDomainTreeSession(engine.stateDomainStore(), reference)
	root, err := session.buildTree(ctx, []storageformat.DomainEntry{{Key: "mutation", Value: []byte("bad"), LogicalVersion: "version"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.validateNamespaceOutcomeClosure(ctx, session, root); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid outcome error = %v", err)
	}

	controlReference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:" + owner.String()}
	pairID := storageformat.Digest([]byte("pair-corrupt"))
	misbound, err := storageformat.EncodeCanonical(storageformat.DuplicateDirectoryPreference{SchemaVersion: 1, PairID: storageformat.Digest([]byte("other")), LeftIdentity: "left", RightIdentity: "right", Ignored: true, Revision: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.stateDomainStore().mutate(ctx, controlReference, consistencyDomainMutation{ID: "bad-pair", Changes: []consistencyDomainChange{{Key: duplicateDirectoryIgnoreKey008(pairID), Require: domainValueAbsent, Value: misbound}}}); err != nil {
		t.Fatal(err)
	}
	head, err := engine.stateDomainStore().loadHead(ctx, controlReference)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.validateKnownControlDomainValues(ctx, controlReference, head.head); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("misbound duplicate pair error = %v", err)
	}
}

func TestSchema008CheckpointEnumerationAndReachabilityFailureBoundaries(t *testing.T) {
	ctx := context.Background()

	t.Run("backend-role-mismatch-and-file-exclusion", func(t *testing.T) {
		engine := &Engine{separateFileBackend: true}
		blobKey := storageformat.BlobKey("owner", "blob").String()
		if _, err := engine.checkpointRoleIncludes(blobKey, false, true); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("wrong backend role error = %v", err)
		}
		engine.separateFileBackend = false
		if included, err := engine.checkpointRoleIncludes(blobKey, false, true); err != nil || included {
			t.Fatalf("state-side file object included=%v error=%v", included, err)
		}
	})

	t.Run("paginated-object-list", func(t *testing.T) {
		calls := 0
		backend := &hookedBackend{Backend: objectmemory.New(), list: func(_ context.Context, request objectstore.ListRequest) (objectstore.ListPage, error) {
			calls++
			switch calls {
			case 1:
				if request.Cursor != "" {
					t.Fatalf("first cursor = %q", request.Cursor)
				}
				return objectstore.ListPage{Objects: []objectstore.ObjectInfo{{Key: objectstore.MustKey("endlessfs/v1/a")}}, NextCursor: "next-page"}, nil
			case 2:
				if request.Cursor != "next-page" {
					t.Fatalf("second cursor = %q", request.Cursor)
				}
				return objectstore.ListPage{Objects: []objectstore.ObjectInfo{{Key: objectstore.MustKey("endlessfs/v1/b")}}}, nil
			default:
				t.Fatalf("unexpected list call %d", calls)
				return objectstore.ListPage{}, nil
			}
		}}
		var keys []string
		if err := walkObjectInfos(ctx, backend, "endlessfs/v1/", func(info objectstore.ObjectInfo) error {
			keys = append(keys, info.Key.String())
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if strings.Join(keys, ",") != "endlessfs/v1/a,endlessfs/v1/b" {
			t.Fatalf("visited keys = %v", keys)
		}
	})

	t.Run("duplicate-object-across-backend-roles", func(t *testing.T) {
		info := objectstore.ObjectInfo{Key: objectstore.MustKey("endlessfs/v1/duplicate")}
		stream := &checkpointMetadataStream{
			stateInfo: info, stateFound: true, stateReady: true,
			fileInfo: info, fileFound: true, fileReady: true,
		}
		if _, _, _, err := stream.next(ctx); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("duplicate backend object error = %v", err)
		}
	})

	t.Run("unknown-domain-pages-are-not-collectable", func(t *testing.T) {
		if isSchema008CollectableAuthorityGarbageKey("endlessfs/v1/domains/unknown/domain/pages/page.json") {
			t.Fatal("unknown domain page was treated as collectable authority garbage")
		}
	})

	t.Run("missing-domain-catalog", func(t *testing.T) {
		engine := openNamespaceTestEngine(t, objectmemory.New())
		if _, err := engine.collectSchema008CheckpointReachability(ctx); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("missing catalog error = %v", err)
		}
	})

	t.Run("nested-tree-page-disappears", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		reference := consistencyDomainRef{Kind: storageformat.DomainShare, ID: "checkpoint-tree-boundary"}
		writer := newConsistencyDomainTreeSession(engine.stateDomainStore(), reference)
		entries := make([]storageformat.DomainEntry, domainPageMaximumItems+1)
		for index := range entries {
			value := []byte(fmt.Sprintf("value-%04d", index))
			entries[index] = storageformat.DomainEntry{Key: fmt.Sprintf("entry-%04d", index), Value: value, LogicalVersion: storageformat.Digest(value)}
		}
		root, err := writer.buildTree(ctx, entries)
		if err != nil {
			t.Fatal(err)
		}
		validCollector, err := newCheckpointReachabilityCollector()
		if err != nil {
			t.Fatal(err)
		}
		validVisited, err := newCheckpointVisitSet()
		if err != nil {
			_ = validCollector.Close()
			t.Fatal(err)
		}
		validWalker := &checkpointReachabilityWalker{engine: engine, collector: validCollector, visited: validVisited}
		if err := validWalker.walkTree(ctx, newConsistencyDomainTreeSession(engine.stateDomainStore(), reference), root, "nested-success", nil); err != nil {
			t.Fatalf("valid nested tree walk = %v", err)
		}
		if err := validVisited.Close(); err != nil {
			t.Fatal(err)
		}
		if err := validCollector.Close(); err != nil {
			t.Fatal(err)
		}
		reader := newConsistencyDomainTreeSession(engine.stateDomainStore(), reference)
		branch, err := reader.readPage(ctx, domainPageRef{root: root})
		if err != nil {
			t.Fatal(err)
		}
		if len(branch.Children) < 2 {
			t.Fatalf("branch children = %d, want at least 2", len(branch.Children))
		}
		missing := reader.pageKey(branch.Children[1].Digest)
		object, err := memory.Get(ctx, missing)
		if err != nil {
			t.Fatal(err)
		}
		if err := memory.Delete(ctx, missing, objectstore.DeleteCondition{Version: object.Version}); err != nil {
			t.Fatal(err)
		}
		collector, err := newCheckpointReachabilityCollector()
		if err != nil {
			t.Fatal(err)
		}
		defer collector.Close()
		visited, err := newCheckpointVisitSet()
		if err != nil {
			t.Fatal(err)
		}
		defer visited.Close()
		walker := &checkpointReachabilityWalker{engine: engine, collector: collector, visited: visited}
		if err := walker.walkTree(ctx, reader, root, "nested-boundary", nil); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing nested page error = %v", err)
		}
	})
}
