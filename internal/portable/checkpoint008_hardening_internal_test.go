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
