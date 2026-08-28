package portable

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func validSchema010ReceiptForTest(t *testing.T) schema010ConservationReceipt {
	t.Helper()
	logical := state.MustKey(state.NamespacePreferences, "owner-a", "theme")
	payload := []byte(`{"themeID":"endlessfs-dark"}`)
	target, reference, recordType, value, err := migrateStateEntry009(logical, payload)
	if err != nil {
		t.Fatal(err)
	}
	return schema010ConservationReceipt{
		SchemaVersion: schema010ConservationSchema,
		SourceRootKey: storageformat.StateIndexRootKey(stateNamespace(logical)).String(), SourceRootDigest: storageformat.Digest([]byte("root")),
		SourceLogicalKey: logical.String(), SourceLogicalVersion: "legacy-version",
		SourceVersionKey: storageformat.StateVersionKey(stateNamespace(logical), logical.String(), "legacy-version").String(), SourceVersionDigest: storageformat.Digest([]byte("version")),
		TargetDomainKind: reference.Kind, TargetDomainID: reference.ID, TargetKey: target.String(),
		TargetRecordType: recordType, TargetValue: value, TargetValueDigest: storageformat.Digest(value),
		Disposition: schema010DispositionRecover,
	}
}

func validSchema010ConservationForTest() schema010Conservation {
	empty := sha256.Sum256(nil)
	return schema010Conservation{
		SchemaVersion: schema010ConservationSchema, FreezeEpoch: 7,
		SourceCatalog: storageformat.DomainCatalogHead{SchemaVersion: 1, Revision: 1, FreezeEpoch: 7},
		Commitment:    hex.EncodeToString(empty[:]),
	}
}

func TestSchema010ConservationInventoryMatchesIdenticalCanonicalRoots(t *testing.T) {
	proof := validSchema010ConservationForTest()
	proof.Roots = []schema010ConservationRoot{{
		Namespace:         string(state.NamespacePreferences),
		RootKey:           storageformat.StateIndexRootKey(string(state.NamespacePreferences)).String(),
		RootDigest:        storageformat.Digest([]byte("root")),
		EntryCount:        1,
		ReceiptCommitment: storageformat.Digest([]byte("receipts")),
	}}
	proof.SourceEntryCount = 1
	proof.RecoveredCount = 1

	if !sameSchema010ConservationInventory(proof, proof) {
		t.Fatal("identical conservation inventories did not match")
	}
}

func TestSchema010ConservationRecordsFailClosedForEveryBindingClass(t *testing.T) {
	valid := validSchema010ReceiptForTest(t)
	if _, _, body, err := validateSchema010Receipt(valid); err != nil || len(body) == 0 {
		t.Fatalf("valid receipt = %q, %v", body, err)
	}
	for index, mutate := range []func(*schema010ConservationReceipt){
		func(value *schema010ConservationReceipt) { value.SchemaVersion = 0 },
		func(value *schema010ConservationReceipt) { value.SourceLogicalKey = "invalid" },
		func(value *schema010ConservationReceipt) { value.TargetDomainKind = "unknown" },
		func(value *schema010ConservationReceipt) { value.Disposition = "unknown" },
		func(value *schema010ConservationReceipt) {
			value.TargetValueDigest = storageformat.Digest([]byte("other"))
		},
		func(value *schema010ConservationReceipt) { value.TargetKey += "-other" },
		func(value *schema010ConservationReceipt) { value.TargetRecordType = storageformat.StateRecordAccount },
	} {
		receipt := valid
		mutate(&receipt)
		if _, _, _, err := validateSchema010Receipt(receipt); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("invalid receipt %d error = %v", index, err)
		}
	}
	malformedTarget := valid
	malformedTarget.TargetValue = []byte("{")
	malformedTarget.TargetValueDigest = storageformat.Digest(malformedTarget.TargetValue)
	if payload := mustSchema010SourcePayload(malformedTarget); payload != nil {
		t.Fatalf("malformed typed target decoded as %q", payload)
	}

	validProof := validSchema010ConservationForTest()
	if body, err := validateSchema010Conservation(validProof); err != nil || len(body) == 0 {
		t.Fatalf("valid conservation proof = %q, %v", body, err)
	}
	root := schema010ConservationRoot{
		Namespace:  string(state.NamespacePreferences),
		RootKey:    storageformat.StateIndexRootKey(string(state.NamespacePreferences)).String(),
		RootDigest: storageformat.Digest([]byte("root")), EntryCount: 1,
		ReceiptCommitment: storageformat.Digest([]byte("receipts")),
	}
	for index, proof := range []schema010Conservation{
		{},
		func() schema010Conservation { value := validProof; value.SourceEntryCount = 1; return value }(),
		func() schema010Conservation {
			value := validProof
			value.Roots = []schema010ConservationRoot{{}}
			return value
		}(),
		func() schema010Conservation {
			value := validProof
			value.Roots = []schema010ConservationRoot{root, root}
			return value
		}(),
		func() schema010Conservation {
			value := validProof
			value.Roots = []schema010ConservationRoot{{
				Namespace: root.Namespace, RootKey: root.RootKey, RootDigest: root.RootDigest,
				EntryCount: math.MaxUint64, ReceiptCommitment: root.ReceiptCommitment,
			}, {Namespace: string(state.NamespaceRoles), RootKey: storageformat.StateIndexRootKey(string(state.NamespaceRoles)).String(), RootDigest: root.RootDigest, EntryCount: 1, ReceiptCommitment: root.ReceiptCommitment}}
			value.SourceEntryCount = math.MaxUint64
			value.RecoveredCount = math.MaxUint64
			return value
		}(),
		func() schema010Conservation {
			value := validProof
			value.Commitment = storageformat.Digest([]byte("wrong"))
			return value
		}(),
	} {
		if _, err := validateSchema010Conservation(proof); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("invalid conservation proof %d error = %v", index, err)
		}
	}
	if sameSchema010ConservationInventory(validProof, func() schema010Conservation { value := validProof; value.FreezeEpoch++; return value }()) {
		t.Fatal("different conservation inventories matched")
	}
}

type schema010GetFailureBackend struct {
	objectstore.Backend
	key  objectstore.Key
	err  error
	body []byte
}

type migrationScriptBackend struct {
	objectstore.Backend
	get  func(context.Context, objectstore.Key) (objectstore.Object, error)
	list func(context.Context, objectstore.ListRequest) (objectstore.ListPage, error)
	put  func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error)
}

func (backend *migrationScriptBackend) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	if backend.get != nil {
		return backend.get(ctx, key)
	}
	return backend.Backend.Get(ctx, key)
}

func (backend *migrationScriptBackend) List(ctx context.Context, request objectstore.ListRequest) (objectstore.ListPage, error) {
	if backend.list != nil {
		return backend.list(ctx, request)
	}
	return backend.Backend.List(ctx, request)
}

func (backend *migrationScriptBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	if backend.put != nil {
		return backend.put(ctx, key, body, condition)
	}
	return backend.Backend.Put(ctx, key, body, condition)
}

func (backend *schema010GetFailureBackend) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	if key == backend.key {
		if backend.err != nil {
			return objectstore.Object{}, backend.err
		}
		return objectstore.Object{Key: key, Body: append([]byte(nil), backend.body...), Version: "fixture"}, nil
	}
	return backend.Backend.Get(ctx, key)
}

func TestSchema010AuthorityVerifierDeniesMissingUnavailableAndMalformedProofs(t *testing.T) {
	ctx := context.Background()
	key := storageformat.Schema010MigrationConservationKey()
	for _, test := range []struct {
		name    string
		backend objectstore.Backend
		want    error
	}{
		{name: "missing", backend: objectmemory.New(), want: domain.ErrPreconditionFailed},
		{name: "unavailable", backend: &schema010GetFailureBackend{Backend: objectmemory.New(), key: key, err: domain.NewError(domain.ErrorUnavailable, "injected")}, want: domain.ErrUnavailable},
		{name: "malformed", backend: &schema010GetFailureBackend{Backend: objectmemory.New(), key: key, body: []byte(`{}`)}, want: domain.ErrInvalid},
		{name: "non-canonical", backend: &schema010GetFailureBackend{Backend: objectmemory.New(), key: key, body: []byte(`{"schemaVersion":1}`)}, want: domain.ErrInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := &Engine{backend: test.backend}
			if err := engine.verifySchema010Authority(ctx, schemaMigration009To010); !errors.Is(err, test.want) {
				t.Fatalf("authority verification error = %v; want %v", err, test.want)
			}
		})
	}
	if err := (&Engine{backend: objectmemory.New()}).verifySchema010Authority(ctx, schemaMigration008To009); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("wrong authority transition error = %v", err)
	}
}

func TestSchema010ReceiptConstructionRejectsMalformedIndexEntries(t *testing.T) {
	engine := &Engine{backend: objectmemory.New()}
	root := schema010ConservationRoot{Namespace: string(state.NamespacePreferences)}
	for index, entry := range []storageformat.StateIndexEntry{
		{},
		{LogicalKey: "invalid", LogicalVersion: "version"},
		{LogicalKey: state.MustKey(state.NamespaceRoles, "admins").String(), LogicalVersion: "version"},
	} {
		if _, err := engine.schema010ReceiptForEntry(context.Background(), root, entry, 1); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("invalid index entry %d error = %v", index, err)
		}
	}
	if err := engine.prepareSchema010Target(context.Background(), consistencyDomainRef{Kind: "unknown", ID: "invalid"}, 1); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid recovery target error = %v", err)
	}
}

func TestSchema009And010DurableArtifactIOFailsClosed(t *testing.T) {
	ctx := context.Background()
	unavailable := domain.NewError(domain.ErrorUnavailable, "injected artifact transport failure")
	validStage := schema009MigrationStage{
		SchemaVersion: schema009MigrationStageSchema, SourceIdentity: "source",
		DomainKind: storageformat.DomainIdentity, DomainID: "owner:fixture",
		Tree: "base", Key: "users/fixture", Value: []byte("value"), LogicalVersion: "version",
	}
	stageReference, stageBody, err := validateSchema009MigrationStage(validStage)
	if err != nil {
		t.Fatal(err)
	}
	stageKey := storageformat.Schema009MigrationStageKey(schema008DomainIdentity(stageReference), validStage.SourceIdentity)
	for _, test := range []struct {
		name    string
		backend objectstore.Backend
		call    func(*Engine) error
		want    error
	}{
		{
			name: "schema009-read-unavailable", want: domain.ErrUnavailable,
			backend: &migrationScriptBackend{Backend: objectmemory.New(), get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
				return objectstore.Object{}, unavailable
			}},
			call: func(engine *Engine) error { _, _, err := engine.readSchema009StagingComplete(ctx); return err },
		},
		{
			name: "schema009-read-malformed", want: domain.ErrInvalid,
			backend: &migrationScriptBackend{Backend: objectmemory.New(), get: func(_ context.Context, key objectstore.Key) (objectstore.Object, error) {
				return objectstore.Object{Key: key, Body: []byte(`{}`)}, nil
			}},
			call: func(engine *Engine) error { _, _, err := engine.readSchema009StagingComplete(ctx); return err },
		},
		{
			name: "schema009-stage-put-unavailable", want: domain.ErrUnavailable,
			backend: &migrationScriptBackend{Backend: objectmemory.New(), put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
				return "", unavailable
			}},
			call: func(engine *Engine) error { return engine.writeSchema009MigrationStage(ctx, validStage) },
		},
		{
			name: "schema009-stage-winner-unavailable", want: domain.ErrInvalid,
			backend: &migrationScriptBackend{Backend: objectmemory.New(), put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
				return "", domain.ErrConflict
			}, get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
				return objectstore.Object{}, unavailable
			}},
			call: func(engine *Engine) error { return engine.writeSchema009MigrationStage(ctx, validStage) },
		},
		{
			name: "schema009-stage-winner-differs", want: domain.ErrInvalid,
			backend: &migrationScriptBackend{Backend: objectmemory.New(), put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
				return "", domain.ErrConflict
			}, get: func(_ context.Context, key objectstore.Key) (objectstore.Object, error) {
				return objectstore.Object{Key: key, Body: []byte(`{}`)}, nil
			}},
			call: func(engine *Engine) error { return engine.writeSchema009MigrationStage(ctx, validStage) },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(&Engine{backend: test.backend}); !errors.Is(err, test.want) {
				t.Fatalf("artifact operation error = %v; want %v", err, test.want)
			}
		})
	}

	receipt := validSchema010ReceiptForTest(t)
	_, _, receiptBody, err := validateSchema010Receipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receiptKey := storageformat.Schema010MigrationReceiptKey(schema008DomainIdentity(consistencyDomainRef{Kind: receipt.TargetDomainKind, ID: receipt.TargetDomainID}), receipt.TargetKey)
	_ = stageBody
	_ = stageKey
	for _, test := range []struct {
		name    string
		backend objectstore.Backend
		want    error
	}{
		{name: "put-unavailable", want: domain.ErrUnavailable, backend: &migrationScriptBackend{Backend: objectmemory.New(), put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return "", unavailable
		}}},
		{name: "winner-unavailable", want: domain.ErrUnavailable, backend: &migrationScriptBackend{Backend: objectmemory.New(), put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return "", domain.ErrConflict
		}, get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, unavailable
		}}},
		{name: "winner-malformed", want: domain.ErrInvalid, backend: &migrationScriptBackend{Backend: objectmemory.New(), put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return "", domain.ErrConflict
		}, get: func(_ context.Context, key objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{Key: key, Body: []byte(`{}`)}, nil
		}}},
		{name: "winner-differs", want: domain.ErrInvalid, backend: &migrationScriptBackend{Backend: objectmemory.New(), put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return "", domain.ErrConflict
		}, get: func(_ context.Context, key objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{Key: key, Body: append([]byte(nil), receiptBody...), Version: "winner"}, nil
		}}},
	} {
		t.Run("schema010-receipt-"+test.name, func(t *testing.T) {
			candidate := receipt
			if test.name == "winner-differs" {
				candidate.SourceRootDigest = storageformat.Digest([]byte("other"))
			}
			_, _, err := (&Engine{backend: test.backend}).writeSchema010Receipt(ctx, candidate, receiptBody)
			if !errors.Is(err, test.want) {
				t.Fatalf("receipt artifact error = %v; want %v (key %s)", err, test.want, receiptKey.String())
			}
		})
	}
}

func TestSchema009And010ArtifactListingsRejectMalformedProviderResults(t *testing.T) {
	ctx := context.Background()
	unavailable := domain.NewError(domain.ErrorUnavailable, "injected list failure")
	stagePrefix := storageformat.Schema009MigrationStagePrefix()
	receiptPrefix := storageformat.Schema010MigrationReceiptPrefix()
	validProof := validSchema010ConservationForTest()
	for _, test := range []struct {
		name string
		page objectstore.ListPage
		err  error
		call func(*Engine) error
		want error
	}{
		{name: "schema009-list-unavailable", err: unavailable, call: func(engine *Engine) error { _, err := engine.installSchema009StagedDomains(ctx, 1); return err }, want: domain.ErrUnavailable},
		{name: "schema009-outside-prefix", page: objectstore.ListPage{Objects: []objectstore.ObjectInfo{{Key: objectstore.MustKey("endlessfs/v1/unrelated.json")}}}, call: func(engine *Engine) error { _, err := engine.installSchema009StagedDomains(ctx, 1); return err }, want: domain.ErrInvalid},
		{name: "schema009-missing-group", page: objectstore.ListPage{Objects: []objectstore.ObjectInfo{{Key: objectstore.MustKey(stagePrefix + "orphan")}}}, call: func(engine *Engine) error { _, err := engine.installSchema009StagedDomains(ctx, 1); return err }, want: domain.ErrInvalid},
		{name: "schema010-list-unavailable", err: unavailable, call: func(engine *Engine) error { _, err := engine.installSchema010Receipts(ctx, validProof); return err }, want: domain.ErrUnavailable},
		{name: "schema010-outside-prefix", page: objectstore.ListPage{Objects: []objectstore.ObjectInfo{{Key: objectstore.MustKey("endlessfs/v1/unrelated.json")}}}, call: func(engine *Engine) error { _, err := engine.installSchema010Receipts(ctx, validProof); return err }, want: domain.ErrInvalid},
		{name: "schema010-missing-group", page: objectstore.ListPage{Objects: []objectstore.ObjectInfo{{Key: objectstore.MustKey(receiptPrefix + "orphan")}}}, call: func(engine *Engine) error { _, err := engine.installSchema010Receipts(ctx, validProof); return err }, want: domain.ErrInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := objectmemory.New()
			engine := &Engine{backend: &migrationScriptBackend{Backend: base, list: func(context.Context, objectstore.ListRequest) (objectstore.ListPage, error) {
				return test.page, test.err
			}}}
			if err := test.call(engine); !errors.Is(err, test.want) {
				t.Fatalf("malformed listing error = %v; want %v", err, test.want)
			}
		})
	}
}

func schema010ProofForReceiptTest(receipt schema010ConservationReceipt) schema010Conservation {
	proof := validSchema010ConservationForTest()
	root := schema010ConservationRoot{
		Namespace:         stateNamespace(state.MustKey(state.NamespacePreferences, "owner-a", "theme")),
		RootKey:           receipt.SourceRootKey,
		RootDigest:        receipt.SourceRootDigest,
		EntryCount:        1,
		ReceiptCommitment: storageformat.Digest([]byte("receipt-commitment")),
	}
	commitment := sha256.New()
	writeSchema010Commitment(commitment, root.RootKey, root.RootDigest, root.ReceiptCommitment)
	proof.Roots = []schema010ConservationRoot{root}
	proof.SourceEntryCount = 1
	proof.RecoveredCount = 1
	proof.Commitment = hex.EncodeToString(commitment.Sum(nil))
	return proof
}

func TestSchema009And010ArtifactListingsDenyInvalidObjectsAndPreservePagination(t *testing.T) {
	ctx := context.Background()
	unavailable := domain.NewError(domain.ErrorUnavailable, "injected artifact read failure")
	stage := schema009MigrationStage{
		SchemaVersion: schema009MigrationStageSchema, SourceIdentity: "source",
		DomainKind: storageformat.DomainIdentity, DomainID: "owner:fixture",
		Tree: "base", Key: "preferences/owner-a/theme", Value: []byte("value"), LogicalVersion: "version",
	}
	stageReference, stageBody, err := validateSchema009MigrationStage(stage)
	if err != nil {
		t.Fatal(err)
	}
	stageKey := storageformat.Schema009MigrationStageKey(schema008DomainIdentity(stageReference), stage.SourceIdentity)
	receipt := validSchema010ReceiptForTest(t)
	receiptReference, _, receiptBody, err := validateSchema010Receipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receiptKey := storageformat.Schema010MigrationReceiptKey(schema008DomainIdentity(receiptReference), receipt.TargetKey)
	proof := schema010ProofForReceiptTest(receipt)

	for _, schema := range []string{"009", "010"} {
		for _, failure := range []string{"get-unavailable", "malformed-body", "misbound-key"} {
			t.Run(schema+"-"+failure, func(t *testing.T) {
				base := objectmemory.New()
				if schema == "010" {
					putMigrationCatalogHeadForTest(t, base, proof.SourceCatalog)
				}
				listedKey, body := stageKey, stageBody
				if schema == "010" {
					listedKey, body = receiptKey, receiptBody
				}
				if failure == "misbound-key" {
					listedKey = objectstore.MustKey(listedKey.String() + "x")
				}
				backend := &migrationScriptBackend{Backend: base}
				backend.list = func(context.Context, objectstore.ListRequest) (objectstore.ListPage, error) {
					return objectstore.ListPage{Objects: []objectstore.ObjectInfo{{Key: listedKey}}}, nil
				}
				backend.get = func(_ context.Context, key objectstore.Key) (objectstore.Object, error) {
					if key != listedKey {
						return base.Get(ctx, key)
					}
					if failure == "get-unavailable" {
						return objectstore.Object{}, unavailable
					}
					if failure == "malformed-body" {
						return objectstore.Object{Key: key, Body: []byte(`{}`)}, nil
					}
					return objectstore.Object{Key: key, Body: body}, nil
				}
				engine := &Engine{backend: backend}
				var callErr error
				if schema == "009" {
					_, callErr = engine.installSchema009StagedDomains(ctx, proof.FreezeEpoch)
				} else {
					_, callErr = engine.installSchema010Receipts(ctx, proof)
				}
				want := domain.ErrInvalid
				if failure == "get-unavailable" {
					want = domain.ErrUnavailable
				}
				if !errors.Is(callErr, want) {
					t.Fatalf("artifact installation error = %v; want %v", callErr, want)
				}
			})
		}
		t.Run(schema+"-pagination", func(t *testing.T) {
			base := objectmemory.New()
			if schema == "010" {
				putMigrationCatalogHeadForTest(t, base, validSchema010ConservationForTest().SourceCatalog)
			}
			pages := 0
			backend := &migrationScriptBackend{Backend: base, list: func(_ context.Context, request objectstore.ListRequest) (objectstore.ListPage, error) {
				pages++
				if pages == 1 {
					return objectstore.ListPage{NextCursor: "next"}, nil
				}
				if request.Cursor != "next" {
					t.Fatalf("second page cursor = %q", request.Cursor)
				}
				return objectstore.ListPage{}, nil
			}}
			engine := &Engine{backend: backend}
			if schema == "009" {
				if _, err := engine.installSchema009StagedDomains(ctx, 1); err != nil {
					t.Fatal(err)
				}
			} else if _, err := engine.installSchema010Receipts(ctx, validSchema010ConservationForTest()); err != nil {
				t.Fatal(err)
			}
			if pages != 2 {
				t.Fatalf("listed pages = %d; want 2", pages)
			}
		})
	}
}

func putMigrationDomainHeadForTest(t *testing.T, backend objectstore.Backend, head storageformat.DomainHead) {
	t.Helper()
	key := storageformat.DomainHeadKey(head.Kind, head.DomainID)
	body, err := storageformat.EncodeEnvelope(domainHeadSchema, key, 1, head)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(context.Background(), key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
}

func putMigrationCatalogHeadForTest(t *testing.T, backend objectstore.Backend, head storageformat.DomainCatalogHead) {
	t.Helper()
	key := storageformat.DomainCatalogHeadKey()
	body, err := storageformat.EncodeEnvelope(domainCatalogHeadSchema, key, 1, head)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(context.Background(), key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
}

func migrationRecoveredRootForTest(t *testing.T, engine *Engine, reference consistencyDomainRef) storageformat.DomainTreeRoot {
	t.Helper()
	runs := newSchema008MigrationRuns(context.Background(), newConsistencyDomainTreeSession(engine.stateDomainStore(), reference))
	if err := runs.Add(storageformat.DomainEntry{Key: "preferences/owner/theme", Value: []byte("value"), LogicalVersion: "version"}); err != nil {
		t.Fatal(err)
	}
	root, err := runs.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func migrationRootForEntriesTest(t *testing.T, engine *Engine, reference consistencyDomainRef, entries ...storageformat.DomainEntry) storageformat.DomainTreeRoot {
	t.Helper()
	runs := newSchema008MigrationRuns(context.Background(), newConsistencyDomainTreeSession(engine.stateDomainStore(), reference))
	for _, entry := range entries {
		if err := runs.Add(entry); err != nil {
			t.Fatal(err)
		}
	}
	root, err := runs.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSchema009And010DomainInstallationDeniesUnsafeStatesAndContention(t *testing.T) {
	ctx := context.Background()
	reference := consistencyDomainRef{Kind: storageformat.DomainIdentity, ID: "owner:fixture"}
	for _, schema := range []string{"009", "010"} {
		t.Run(schema+"-unfrozen-existing", func(t *testing.T) {
			backend := objectmemory.New()
			putMigrationDomainHeadForTest(t, backend, storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true, Revision: 1, BaseRevision: 1})
			engine := &Engine{backend: backend}
			var err error
			if schema == "009" {
				err = engine.installSchema009Domain(ctx, reference, storageformat.DomainTreeRoot{}, storageformat.DomainTreeRoot{}, storageformat.DomainTreeRoot{}, 7)
			} else {
				_, err = engine.installSchema010Domain(ctx, reference, storageformat.DomainTreeRoot{}, 7, true)
			}
			if !errors.Is(err, domain.ErrPreconditionFailed) {
				t.Fatalf("unfrozen target error = %v", err)
			}
		})
	}

	t.Run("schema010-new-and-existing-domain-classification", func(t *testing.T) {
		backend := objectmemory.New()
		engine := &Engine{backend: backend}
		recovered := migrationRecoveredRootForTest(t, engine, reference)
		if created, err := engine.installSchema010Domain(ctx, reference, recovered, 7, false); err != nil || !created {
			t.Fatalf("new recovered domain = %t, %v", created, err)
		}
		if created, err := engine.installSchema010Domain(ctx, reference, recovered, 7, false); err != nil || !created {
			t.Fatalf("idempotent recovered domain = %t, %v", created, err)
		}
		if _, err := engine.installSchema010Domain(ctx, reference, storageformat.DomainTreeRoot{}, 7, true); err != nil {
			t.Fatalf("catalogued empty recovery = %v", err)
		}
		if _, err := engine.installSchema010Domain(ctx, reference, storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("different")), EntryCount: 1}, 7, false); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("uncatalogued conflicting domain error = %v", err)
		}
	})

	t.Run("schema010-missing-source-domain", func(t *testing.T) {
		engine := &Engine{backend: objectmemory.New()}
		if _, err := engine.installSchema010Domain(ctx, reference, storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("root")), EntryCount: 1}, 7, true); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("missing catalogued domain error = %v", err)
		}
		if created, err := engine.installSchema010Domain(ctx, reference, storageformat.DomainTreeRoot{}, 7, false); err != nil || created {
			t.Fatalf("empty uncatalogued domain = %t, %v", created, err)
		}
	})

	for _, schema := range []string{"009", "010"} {
		for _, providerErr := range []error{domain.NewError(domain.ErrorUnavailable, "injected put failure"), domain.ErrConflict} {
			name := schema + "-put-failure"
			if errors.Is(providerErr, domain.ErrConflict) {
				name = schema + "-persistent-contention"
			}
			t.Run(name, func(t *testing.T) {
				base := objectmemory.New()
				engineForRoot := &Engine{backend: base}
				recovered := migrationRecoveredRootForTest(t, engineForRoot, reference)
				backend := &migrationScriptBackend{Backend: base, put: func(_ context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
					if key == storageformat.DomainHeadKey(reference.Kind, reference.ID) {
						return "", providerErr
					}
					return base.Put(ctx, key, body, condition)
				}}
				engine := &Engine{backend: backend}
				var err error
				if schema == "009" {
					err = engine.installSchema009Domain(ctx, reference, recovered, storageformat.DomainTreeRoot{}, storageformat.DomainTreeRoot{}, 7)
				} else {
					_, err = engine.installSchema010Domain(ctx, reference, recovered, 7, false)
				}
				want := domain.ErrUnavailable
				if !errors.Is(providerErr, domain.ErrConflict) {
					want = providerErr
				}
				if !errors.Is(err, want) {
					t.Fatalf("domain installation error = %v; want %v", err, want)
				}
			})
		}
	}
}

func migrationCatalogFixtureForTest(t *testing.T, backend objectstore.Backend, reference consistencyDomainRef, freezeEpoch uint64) storageformat.DomainCatalogHead {
	t.Helper()
	engine := &Engine{backend: backend}
	runs := newSchema008MigrationRuns(context.Background(), newDomainCatalogTreeSession(engine.stateDomainStore()))
	entry := storageformat.DomainCatalogEntry{
		DomainID: reference.ID,
		Kind:     reference.Kind,
		HeadKey:  storageformat.DomainHeadKey(reference.Kind, reference.ID).String(),
	}
	body, err := storageformat.EncodeCanonical(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := runs.Add(storageformat.DomainEntry{Key: catalogEntryKey(reference), Value: body, LogicalVersion: storageformat.Digest(body)}); err != nil {
		t.Fatal(err)
	}
	root, err := runs.Finish()
	if err != nil {
		t.Fatal(err)
	}
	head := storageformat.DomainCatalogHead{SchemaVersion: 1, Revision: 1, FreezeEpoch: freezeEpoch, Root: root}
	putMigrationCatalogHeadForTest(t, backend, head)
	return head
}

func TestSchema009RetirementIsIdempotentAndFailsClosedAtEveryProviderBoundary(t *testing.T) {
	ctx := context.Background()
	reference := consistencyDomainRef{Kind: storageformat.DomainIdentity, ID: "owner:retirement"}
	freezeEpoch := uint64(7)
	target := storageformat.DomainTreeRoot{}

	t.Run("published-target-other-epoch", func(t *testing.T) {
		backend := objectmemory.New()
		source := storageformat.DomainCatalogHead{SchemaVersion: 1, Revision: 1, FreezeEpoch: freezeEpoch + 1, Root: target}
		putMigrationCatalogHeadForTest(t, backend, source)
		if err := (&Engine{backend: backend}).retireSchema008DomainHeads009(ctx, source, target, freezeEpoch); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("published target epoch error = %v", err)
		}
	})

	for _, test := range []struct {
		name       string
		head       *storageformat.DomainHead
		getErr     error
		putErr     error
		targetRoot storageformat.DomainTreeRoot
		want       error
	}{
		{name: "missing-head"},
		{name: "head-unavailable", getErr: domain.NewError(domain.ErrorUnavailable, "head unavailable"), want: domain.ErrUnavailable},
		{name: "unregistered-head", head: &storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind}},
		{name: "unfrozen-head", head: &storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true, Revision: 1, BaseRevision: 1}, want: domain.ErrPreconditionFailed},
		{name: "retire-success", head: &storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true, Revision: 1, BaseRevision: 1, Frozen: true, FreezeEpoch: freezeEpoch}},
		{name: "retire-provider-failure", head: &storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true, Revision: 1, BaseRevision: 1, Frozen: true, FreezeEpoch: freezeEpoch}, putErr: domain.NewError(domain.ErrorUnavailable, "retirement unavailable"), want: domain.ErrUnavailable},
		{name: "retire-contention", head: &storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true, Revision: 1, BaseRevision: 1, Frozen: true, FreezeEpoch: freezeEpoch}, putErr: domain.ErrConflict, want: domain.ErrUnavailable},
		{name: "target-lookup-unavailable", targetRoot: storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("missing-target")), Level: 0, EntryCount: 1}, want: domain.ErrNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := objectmemory.New()
			source := migrationCatalogFixtureForTest(t, base, reference, freezeEpoch)
			if test.head != nil {
				putMigrationDomainHeadForTest(t, base, *test.head)
			}
			headKey := storageformat.DomainHeadKey(reference.Kind, reference.ID)
			backend := &migrationScriptBackend{Backend: base}
			if test.getErr != nil {
				backend.get = func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
					if key == headKey {
						return objectstore.Object{}, test.getErr
					}
					return base.Get(ctx, key)
				}
			}
			if test.putErr != nil {
				backend.put = func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
					if key == headKey {
						return "", test.putErr
					}
					return base.Put(ctx, key, body, condition)
				}
			}
			err := (&Engine{backend: backend}).retireSchema008DomainHeads009(ctx, source, test.targetRoot, freezeEpoch)
			if test.want == nil {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, test.want) {
				t.Fatalf("retirement error = %v; want %v", err, test.want)
			}
		})
	}
}

func TestSchema010TargetPreparationAndDomainMergeSurviveProviderFailures(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2046, 8, 27, 12, 0, 0, 0, time.UTC))
	reference := consistencyDomainRef{Kind: storageformat.DomainIdentity, ID: "owner:prepare"}
	freezeEpoch := uint64(7)
	validDelta := storageformat.DomainDelta{
		MutationID: "mutation", Fingerprint: storageformat.Digest([]byte("mutation")), Revision: 1,
		RetainUntil: clock.Now().Add(time.Hour),
		Changes:     []storageformat.DomainChange{{Key: "preferences/owner/theme", Value: []byte("value"), LogicalVersion: "version"}},
	}
	seedFrozenDelta := func(t *testing.T, backend objectstore.Backend) {
		t.Helper()
		putMigrationDomainHeadForTest(t, backend, storageformat.DomainHead{
			SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true,
			Revision: 1, Frozen: true, FreezeEpoch: freezeEpoch, Deltas: []storageformat.DomainDelta{validDelta},
		})
	}

	t.Run("wrong-freeze-epoch", func(t *testing.T) {
		backend := objectmemory.New()
		putMigrationDomainHeadForTest(t, backend, storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true, Revision: 1, BaseRevision: 1, Frozen: true, FreezeEpoch: freezeEpoch + 1})
		if err := (&Engine{backend: backend, clock: clock}).prepareSchema010Target(ctx, reference, freezeEpoch); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("wrong target freeze epoch error = %v", err)
		}
	})

	t.Run("unfreeze-provider-failure", func(t *testing.T) {
		base := objectmemory.New()
		seedFrozenDelta(t, base)
		headKey := storageformat.DomainHeadKey(reference.Kind, reference.ID)
		backend := &migrationScriptBackend{Backend: base, put: func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if key == headKey {
				return "", domain.NewError(domain.ErrorUnavailable, "unfreeze unavailable")
			}
			return base.Put(ctx, key, body, condition)
		}}
		if err := (&Engine{backend: backend, clock: clock}).prepareSchema010Target(ctx, reference, freezeEpoch); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("unfreeze failure = %v", err)
		}
	})

	t.Run("compaction-failure", func(t *testing.T) {
		backend := objectmemory.New()
		seedFrozenDelta(t, backend)
		engine := &Engine{backend: backend, clock: clock, scheduler: SchedulerFunc(func(_ context.Context, step string) error {
			if step == "consistency-domain:before-compaction-commit" {
				return domain.NewError(domain.ErrorUnavailable, "compaction interrupted")
			}
			return nil
		})}
		if err := engine.prepareSchema010Target(ctx, reference, freezeEpoch); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("compaction failure = %v", err)
		}
	})

	t.Run("freeze-provider-failure", func(t *testing.T) {
		base := objectmemory.New()
		seedFrozenDelta(t, base)
		headKey := storageformat.DomainHeadKey(reference.Kind, reference.ID)
		headPuts := 0
		backend := &migrationScriptBackend{Backend: base, put: func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if key == headKey {
				headPuts++
				if headPuts == 3 {
					return "", domain.NewError(domain.ErrorUnavailable, "refreeze unavailable")
				}
			}
			return base.Put(ctx, key, body, condition)
		}}
		if err := (&Engine{backend: backend, clock: clock}).prepareSchema010Target(ctx, reference, freezeEpoch); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("refreeze failure = %v (head puts %d)", err, headPuts)
		}
	})

	t.Run("successful-compaction-and-refreeze", func(t *testing.T) {
		backend := objectmemory.New()
		seedFrozenDelta(t, backend)
		if err := (&Engine{backend: backend, clock: clock}).prepareSchema010Target(ctx, reference, freezeEpoch); err != nil {
			t.Fatal(err)
		}
		snapshot, err := (&Engine{backend: backend, clock: clock}).stateDomainStore().loadHead(ctx, reference)
		if err != nil || !snapshot.head.Frozen || snapshot.head.FreezeEpoch != freezeEpoch || len(snapshot.head.Deltas) != 0 {
			t.Fatalf("prepared target = %+v, %v", snapshot.head, err)
		}
	})

	for _, persistent := range []bool{false, true} {
		name := "transient-unfreeze-epoch-race"
		if persistent {
			name = "persistent-unfreeze-epoch-race"
		}
		t.Run(name, func(t *testing.T) {
			base := objectmemory.New()
			seedFrozenDelta(t, base)
			headKey := storageformat.DomainHeadKey(reference.Kind, reference.ID)
			changed := storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true, Revision: 1, Frozen: true, FreezeEpoch: freezeEpoch + 1, Deltas: []storageformat.DomainDelta{validDelta}}
			changedBody, err := storageformat.EncodeEnvelope(domainHeadSchema, headKey, 1, changed)
			if err != nil {
				t.Fatal(err)
			}
			headGets := 0
			backend := &migrationScriptBackend{Backend: base, get: func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
				if key == headKey {
					headGets++
					if headGets%2 == 0 && (persistent || headGets == 2) {
						return objectstore.Object{Key: key, Body: changedBody, Version: "racing"}, nil
					}
				}
				return base.Get(ctx, key)
			}}
			err = (&Engine{backend: backend, clock: clock}).prepareSchema010Target(ctx, reference, freezeEpoch)
			if persistent {
				if !errors.Is(err, domain.ErrUnavailable) {
					t.Fatalf("persistent epoch race error = %v", err)
				}
			} else if err != nil {
				t.Fatalf("transient epoch race error = %v", err)
			}
		})
	}

	t.Run("existing-domain-subset-and-merge", func(t *testing.T) {
		for _, providerErr := range []error{nil, domain.NewError(domain.ErrorUnavailable, "merge unavailable")} {
			base := objectmemory.New()
			engineForRoots := &Engine{backend: base, clock: clock}
			first := storageformat.DomainEntry{Key: "preferences/owner/a", Value: []byte("a"), LogicalVersion: "a-v1"}
			second := storageformat.DomainEntry{Key: "preferences/owner/b", Value: []byte("b"), LogicalVersion: "b-v1"}
			baseRoot := migrationRootForEntriesTest(t, engineForRoots, reference, first)
			recovered := migrationRootForEntriesTest(t, engineForRoots, reference, second)
			putMigrationDomainHeadForTest(t, base, storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true, Revision: 1, BaseRevision: 1, Frozen: true, FreezeEpoch: freezeEpoch, Base: baseRoot})
			backend := objectstore.Backend(base)
			if providerErr != nil {
				headKey := storageformat.DomainHeadKey(reference.Kind, reference.ID)
				backend = &migrationScriptBackend{Backend: base, put: func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
					if key == headKey {
						return "", providerErr
					}
					return base.Put(ctx, key, body, condition)
				}}
			}
			_, err := (&Engine{backend: backend, clock: clock}).installSchema010Domain(ctx, reference, recovered, freezeEpoch, true)
			if providerErr == nil && err != nil {
				t.Fatal(err)
			}
			if providerErr != nil && !errors.Is(err, providerErr) {
				t.Fatalf("existing merge provider error = %v; want %v", err, providerErr)
			}
		}

		backend := objectmemory.New()
		engine := &Engine{backend: backend, clock: clock}
		first := storageformat.DomainEntry{Key: "preferences/owner/a", Value: []byte("a"), LogicalVersion: "a-v1"}
		second := storageformat.DomainEntry{Key: "preferences/owner/b", Value: []byte("b"), LogicalVersion: "b-v1"}
		baseRoot := migrationRootForEntriesTest(t, engine, reference, first, second)
		recovered := migrationRootForEntriesTest(t, engine, reference, first)
		putMigrationDomainHeadForTest(t, backend, storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true, Revision: 1, BaseRevision: 1, Frozen: true, FreezeEpoch: freezeEpoch, Base: baseRoot})
		if created, err := engine.installSchema010Domain(ctx, reference, recovered, freezeEpoch, true); err != nil || created {
			t.Fatalf("already-contained recovered subset = %t, %v", created, err)
		}
	})

	t.Run("unregistered-head-is-conditionally-replaced", func(t *testing.T) {
		backend := objectmemory.New()
		engine := &Engine{backend: backend, clock: clock}
		recovered := migrationRecoveredRootForTest(t, engine, reference)
		putMigrationDomainHeadForTest(t, backend, storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind})
		if created, err := engine.installSchema010Domain(ctx, reference, recovered, freezeEpoch, false); err != nil || !created {
			t.Fatalf("replace unregistered head = %t, %v", created, err)
		}
	})

	t.Run("new-domain-envelope-limit", func(t *testing.T) {
		backend := objectmemory.New()
		engine := &Engine{backend: backend, clock: clock}
		huge := consistencyDomainRef{Kind: storageformat.DomainIdentity, ID: strings.Repeat("x", storageformat.MaxCanonicalBytes)}
		recovered := migrationRecoveredRootForTest(t, engine, reference)
		if _, err := engine.installSchema010Domain(ctx, huge, recovered, freezeEpoch, false); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("oversized target head error = %v", err)
		}
	})
}

func TestSchema009And010CatalogPublicationRejectsChangedSourcesAndContention(t *testing.T) {
	ctx := context.Background()
	source := storageformat.DomainCatalogHead{SchemaVersion: 1, Revision: 1, FreezeEpoch: 7}
	target := storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("target")), EntryCount: 1}
	for _, schema := range []string{"009", "010"} {
		t.Run(schema+"-changed-source", func(t *testing.T) {
			backend := objectmemory.New()
			putMigrationCatalogHeadForTest(t, backend, source)
			engine := &Engine{backend: backend}
			var err error
			if schema == "009" {
				err = engine.publishSchema009Catalog(ctx, target, 8)
			} else {
				changed := source
				changed.Root = target
				err = engine.publishSchema010Catalog(ctx, changed, target, 7)
			}
			if !errors.Is(err, domain.ErrPreconditionFailed) {
				t.Fatalf("changed catalog source error = %v", err)
			}
		})
		for _, providerErr := range []error{domain.NewError(domain.ErrorUnavailable, "injected publish failure"), domain.ErrConflict} {
			name := schema + "-publish-failure"
			if errors.Is(providerErr, domain.ErrConflict) {
				name = schema + "-publish-contention"
			}
			t.Run(name, func(t *testing.T) {
				base := objectmemory.New()
				putMigrationCatalogHeadForTest(t, base, source)
				backend := &migrationScriptBackend{Backend: base, put: func(_ context.Context, key objectstore.Key, _ []byte, _ objectstore.PutCondition) (objectstore.NativeVersion, error) {
					if key == storageformat.DomainCatalogHeadKey() {
						return "", providerErr
					}
					return "", domain.NewError(domain.ErrorInternal, "unexpected put")
				}}
				engine := &Engine{backend: backend}
				var err error
				if schema == "009" {
					err = engine.publishSchema009Catalog(ctx, target, 7)
				} else {
					err = engine.publishSchema010Catalog(ctx, source, target, 7)
				}
				want := domain.ErrUnavailable
				if !errors.Is(providerErr, domain.ErrConflict) {
					want = providerErr
				}
				if !errors.Is(err, want) {
					t.Fatalf("catalog publication error = %v; want %v", err, want)
				}
			})
		}
	}
}

func TestSchema009DomainStagingAndInstallationDenyMalformedOrDivergentAuthority(t *testing.T) {
	ctx := context.Background()
	reference := consistencyDomainRef{Kind: storageformat.DomainIdentity, ID: "owner:stage"}
	freezeEpoch := uint64(7)

	for _, test := range []struct {
		name  string
		entry *storageformat.DomainEntry
		head  *storageformat.DomainHead
		want  error
	}{
		{name: "missing-head", want: domain.ErrPreconditionFailed},
		{name: "unfrozen-head", head: &storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true, Revision: 1, BaseRevision: 1}, want: domain.ErrPreconditionFailed},
		{name: "malformed-state-payload", entry: &storageformat.DomainEntry{Key: state.MustKey(state.NamespaceCredentials, "credential-hash").String(), Value: []byte(`{}`), LogicalVersion: "version"}, want: domain.ErrInvalid},
		{name: "non-state-identity-key", entry: &storageformat.DomainEntry{Key: "not-state", Value: []byte("value"), LogicalVersion: "version"}, want: domain.ErrInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := objectmemory.New()
			engine := &Engine{backend: backend}
			if test.entry != nil {
				root := migrationRootForEntriesTest(t, engine, reference, *test.entry)
				putMigrationDomainHeadForTest(t, backend, storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true, Revision: 1, BaseRevision: 1, Frozen: true, FreezeEpoch: freezeEpoch, Base: root})
			} else if test.head != nil {
				putMigrationDomainHeadForTest(t, backend, *test.head)
			}
			if err := engine.stageSchema008Domain009(ctx, reference, freezeEpoch); !errors.Is(err, test.want) {
				t.Fatalf("domain staging error = %v; want %v", err, test.want)
			}
		})
	}

	if err := (&Engine{backend: objectmemory.New()}).writeSchema009MigrationStage(ctx, schema009MigrationStage{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid direct stage error = %v", err)
	}

	t.Run("existing-domain-revision-and-match", func(t *testing.T) {
		backend := objectmemory.New()
		engine := &Engine{backend: backend}
		oldRoot := migrationRootForEntriesTest(t, engine, reference, storageformat.DomainEntry{Key: "preferences/owner/a", Value: []byte("a"), LogicalVersion: "a-v1"})
		newRoot := migrationRootForEntriesTest(t, engine, reference, storageformat.DomainEntry{Key: "preferences/owner/b", Value: []byte("b"), LogicalVersion: "b-v1"})
		putMigrationDomainHeadForTest(t, backend, storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true, Revision: 1, BaseRevision: 1, Frozen: true, FreezeEpoch: freezeEpoch, Base: oldRoot})
		if err := engine.installSchema009Domain(ctx, reference, newRoot, storageformat.DomainTreeRoot{}, storageformat.DomainTreeRoot{}, freezeEpoch); err != nil {
			t.Fatal(err)
		}
		snapshot, err := engine.stateDomainStore().loadHead(ctx, reference)
		if err != nil || snapshot.head.Revision != 2 || snapshot.head.Base != newRoot {
			t.Fatalf("installed existing domain = %+v, %v", snapshot.head, err)
		}
	})

	t.Run("domain-envelope-limit", func(t *testing.T) {
		huge := consistencyDomainRef{Kind: storageformat.DomainIdentity, ID: strings.Repeat("x", storageformat.MaxCanonicalBytes)}
		if err := (&Engine{backend: objectmemory.New()}).installSchema009Domain(ctx, huge, storageformat.DomainTreeRoot{}, storageformat.DomainTreeRoot{}, storageformat.DomainTreeRoot{}, freezeEpoch); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("oversized schema-009 domain head error = %v", err)
		}
	})

	for _, test := range []struct {
		name string
		get  func(objectstore.Key) (objectstore.Object, error)
		want error
	}{
		{name: "winner-unavailable", want: domain.ErrInvalid, get: func(objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "marker unavailable")
		}},
		{name: "winner-differs", want: domain.ErrInvalid, get: func(key objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{Key: key, Body: []byte(`{}`)}, nil
		}},
	} {
		t.Run("staging-marker-"+test.name, func(t *testing.T) {
			base := objectmemory.New()
			putMigrationCatalogHeadForTest(t, base, storageformat.DomainCatalogHead{SchemaVersion: 1, Revision: 1, FreezeEpoch: freezeEpoch})
			markerKey := storageformat.Schema009MigrationStageCompleteKey()
			markerGets := 0
			backend := &migrationScriptBackend{Backend: base}
			backend.get = func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
				if key == markerKey {
					markerGets++
					if markerGets == 1 {
						return objectstore.Object{}, domain.ErrNotFound
					}
					return test.get(key)
				}
				return base.Get(ctx, key)
			}
			backend.put = func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
				if key == markerKey {
					return "", domain.ErrConflict
				}
				return base.Put(ctx, key, body, condition)
			}
			if _, err := (&Engine{backend: backend}).stageSchema008Domains009(ctx, freezeEpoch); !errors.Is(err, test.want) {
				t.Fatalf("staging marker winner error = %v; want %v", err, test.want)
			}
		})
	}
}

func TestSchema009CatalogStagingHandlesMissingStaleAndInterruptedDomainClosure(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2046, 8, 27, 13, 0, 0, 0, time.UTC))
	reference := consistencyDomainRef{Kind: storageformat.DomainIdentity, ID: "owner:closure"}
	freezeEpoch := uint64(7)
	delta := storageformat.DomainDelta{
		MutationID: "mutation", Fingerprint: storageformat.Digest([]byte("mutation")), Revision: 1,
		RetainUntil: clock.Now().Add(time.Hour),
		Changes:     []storageformat.DomainChange{{Key: "preferences/owner/theme", Value: []byte(`{"themeID":"endlessfs-dark"}`), LogicalVersion: "version"}},
	}

	for _, test := range []struct {
		name      string
		head      *storageformat.DomainHead
		scheduler Scheduler
		putFailAt int
		want      error
	}{
		{name: "missing-catalogued-domain", want: domain.ErrPreconditionFailed},
		{name: "source-frozen-at-other-epoch", head: &storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true, Revision: 1, BaseRevision: 1, Frozen: true, FreezeEpoch: freezeEpoch + 1}, want: domain.ErrPreconditionFailed},
		{name: "unfreeze-failure", head: &storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true, Revision: 1, Frozen: true, FreezeEpoch: freezeEpoch, Deltas: []storageformat.DomainDelta{delta}}, putFailAt: 1, want: domain.ErrUnavailable},
		{name: "compaction-failure", head: &storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true, Revision: 1, Deltas: []storageformat.DomainDelta{delta}}, scheduler: SchedulerFunc(func(_ context.Context, step string) error {
			if step == "consistency-domain:before-compaction-commit" {
				return domain.NewError(domain.ErrorUnavailable, "closure compaction interrupted")
			}
			return nil
		}), want: domain.ErrUnavailable},
		{name: "freeze-failure", head: &storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true, Revision: 1, Deltas: []storageformat.DomainDelta{delta}}, putFailAt: 2, want: domain.ErrUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := objectmemory.New()
			migrationCatalogFixtureForTest(t, base, reference, freezeEpoch)
			if test.head != nil {
				putMigrationDomainHeadForTest(t, base, *test.head)
			}
			headKey := storageformat.DomainHeadKey(reference.Kind, reference.ID)
			headPuts := 0
			backend := objectstore.Backend(base)
			if test.putFailAt != 0 {
				backend = &migrationScriptBackend{Backend: base, put: func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
					if key == headKey {
						headPuts++
						if headPuts == test.putFailAt {
							return "", domain.NewError(domain.ErrorUnavailable, "domain closure unavailable")
						}
					}
					return base.Put(ctx, key, body, condition)
				}}
			}
			engine := &Engine{backend: backend, clock: clock, scheduler: test.scheduler}
			if _, err := engine.stageSchema008Domains009(ctx, freezeEpoch); !errors.Is(err, test.want) {
				t.Fatalf("catalog staging error = %v; want %v (head puts %d)", err, test.want, headPuts)
			}
		})
	}

	t.Run("catalog-freeze-epoch-changed-before-staging", func(t *testing.T) {
		base := objectmemory.New()
		head := storageformat.DomainCatalogHead{SchemaVersion: 1, Revision: 1, FreezeEpoch: freezeEpoch}
		putMigrationCatalogHeadForTest(t, base, head)
		changed := head
		changed.Revision++
		changed.FreezeEpoch++
		changedBody, err := storageformat.EncodeEnvelope(domainCatalogHeadSchema, storageformat.DomainCatalogHeadKey(), 2, changed)
		if err != nil {
			t.Fatal(err)
		}
		catalogGets := 0
		backend := &migrationScriptBackend{Backend: base, get: func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
			if key == storageformat.DomainCatalogHeadKey() {
				catalogGets++
				if catalogGets > 1 {
					return objectstore.Object{Key: key, Body: changedBody, Version: "changed"}, nil
				}
			}
			return base.Get(ctx, key)
		}}
		if _, err := (&Engine{backend: backend, clock: clock}).stageSchema008Domains009(ctx, freezeEpoch); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("changed catalog freeze epoch error = %v", err)
		}
	})
}

func TestSchema010ReceiptRecoveryRejectsInvalidLegacyVersionsAndRecognizesInstalledValues(t *testing.T) {
	ctx := context.Background()
	freezeEpoch := uint64(7)
	logical := state.MustKey(state.NamespacePreferences, "owner-receipt", "theme")
	entry := storageformat.StateIndexEntry{LogicalKey: logical.String(), LogicalVersion: "legacy-v1"}
	root := schema010ConservationRoot{Namespace: stateNamespace(logical), RootKey: storageformat.StateIndexRootKey(stateNamespace(logical)).String(), RootDigest: storageformat.Digest([]byte("root")), EntryCount: 1}
	versionKey := storageformat.StateVersionKey(stateNamespace(logical), logical.String(), entry.LogicalVersion)

	t.Run("malformed-state-version-envelope", func(t *testing.T) {
		backend := objectmemory.New()
		if _, err := backend.Put(ctx, versionKey, []byte(`{}`), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if _, err := (&Engine{backend: backend}).schema010ReceiptForEntry(ctx, root, entry, freezeEpoch); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("malformed state-version error = %v", err)
		}
	})

	t.Run("invalid-legacy-migration-binding", func(t *testing.T) {
		backend := objectmemory.New()
		legacyLogical := state.MustKey(state.NamespaceCredentials, "credential-hash")
		legacyEntry := storageformat.StateIndexEntry{LogicalKey: legacyLogical.String(), LogicalVersion: entry.LogicalVersion}
		legacyRoot := schema010ConservationRoot{Namespace: stateNamespace(legacyLogical), RootKey: storageformat.StateIndexRootKey(stateNamespace(legacyLogical)).String(), RootDigest: root.RootDigest, EntryCount: 1}
		object, err := stateVersionObject(legacyLogical, state.Version(legacyEntry.LogicalVersion), []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Put(ctx, objectstore.MustKey(object.Key), object.Body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if _, err := (&Engine{backend: backend}).schema010ReceiptForEntry(ctx, legacyRoot, legacyEntry, freezeEpoch); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid legacy payload error = %v", err)
		}
	})

	t.Run("already-installed-value", func(t *testing.T) {
		backend := objectmemory.New()
		engine := &Engine{backend: backend}
		payload := []byte(`{"themeID":"endlessfs-dark"}`)
		object, err := stateVersionObject(logical, state.Version(entry.LogicalVersion), payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Put(ctx, objectstore.MustKey(object.Key), object.Body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		target, reference, _, targetValue, err := migrateStateEntry009(logical, payload)
		if err != nil {
			t.Fatal(err)
		}
		targetRoot := migrationRootForEntriesTest(t, engine, reference, storageformat.DomainEntry{Key: target.String(), Value: targetValue, LogicalVersion: entry.LogicalVersion})
		putMigrationDomainHeadForTest(t, backend, storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true, Revision: 1, BaseRevision: 1, Frozen: true, FreezeEpoch: freezeEpoch, Base: targetRoot})
		receipt, err := engine.schema010ReceiptForEntry(ctx, root, entry, freezeEpoch)
		if err != nil || receipt.Disposition != schema010DispositionAlreadyPresent {
			t.Fatalf("already-installed receipt = %+v, %v", receipt, err)
		}
	})
}

func TestSchema010ProofAndTargetVerificationDenySemanticBindingFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("canonical-but-invalid-proof", func(t *testing.T) {
		backend := objectmemory.New()
		body, err := storageformat.EncodeCanonical(schema010Conservation{SchemaVersion: schema010ConservationSchema})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Put(ctx, storageformat.Schema010MigrationConservationKey(), body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := (&Engine{backend: backend}).readSchema010Conservation(ctx); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("semantically invalid proof error = %v", err)
		}
	})
	if err := (&Engine{backend: objectmemory.New()}).verifySchema010Conservation(ctx, schema010Conservation{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid direct verification proof error = %v", err)
	}
	if _, err := (&Engine{backend: objectmemory.New()}).installSchema010Receipts(ctx, schema010Conservation{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid receipt installation proof error = %v", err)
	}

	objects, proof := schema010InstalledVerificationFixture(t)
	baseline := cloneMigrationBackendForTest(t, objects)
	page, err := baseline.List(ctx, objectstore.ListRequest{Prefix: storageformat.Schema010MigrationReceiptPrefix(), Limit: 1000})
	if err != nil || len(page.Objects) != 1 {
		t.Fatalf("verification fixture receipts = %+v, %v", page, err)
	}
	receiptKey := page.Objects[0].Key
	receiptObject, err := baseline.Get(ctx, receiptKey)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("misbound-receipt-key", func(t *testing.T) {
		backend := cloneMigrationBackendForTest(t, objects)
		deleteMigrationObjectForTest(t, backend, receiptKey)
		misbound := objectstore.MustKey(receiptKey.String() + "x")
		if _, err := backend.Put(ctx, misbound, receiptObject.Body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := (&Engine{backend: backend}).verifySchema010ReceiptTargets(ctx, proof); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("misbound target receipt error = %v", err)
		}
	})

	t.Run("receipt-pagination", func(t *testing.T) {
		backend := cloneMigrationBackendForTest(t, objects)
		lists := 0
		wrapped := &migrationScriptBackend{Backend: backend, list: func(ctx context.Context, request objectstore.ListRequest) (objectstore.ListPage, error) {
			if request.Prefix != storageformat.Schema010MigrationReceiptPrefix() {
				return backend.List(ctx, request)
			}
			lists++
			if lists == 1 {
				return objectstore.ListPage{Objects: []objectstore.ObjectInfo{{Key: receiptKey}}, NextCursor: "next"}, nil
			}
			if request.Cursor != "next" {
				t.Fatalf("target verification cursor = %q", request.Cursor)
			}
			return objectstore.ListPage{}, nil
		}}
		if err := (&Engine{backend: wrapped}).verifySchema010ReceiptTargets(ctx, proof); err != nil {
			t.Fatal(err)
		}
		if lists != 2 {
			t.Fatalf("target verification pages = %d; want 2", lists)
		}
	})
}

func TestSchema010StagingCountsAlreadyInstalledValuesAndRejectsIndexCountDrift(t *testing.T) {
	ctx := context.Background()
	freezeEpoch := uint64(7)
	payload := []byte(`{"themeID":"endlessfs-dark"}`)
	logical := state.MustKey(state.NamespacePreferences, "owner-count", "theme")
	version := "legacy-count-v1"
	target, reference, _, targetValue, err := migrateStateEntry009(logical, payload)
	if err != nil {
		t.Fatal(err)
	}

	setup := func(t *testing.T, alreadyInstalled bool) (*objectmemory.Backend, *Engine) {
		t.Helper()
		backend := objectmemory.New()
		engine := &Engine{backend: backend}
		migrationCatalogFixtureForTest(t, backend, reference, freezeEpoch)
		base := storageformat.DomainTreeRoot{}
		if alreadyInstalled {
			base = migrationRootForEntriesTest(t, engine, reference, storageformat.DomainEntry{Key: target.String(), Value: targetValue, LogicalVersion: version})
		}
		putMigrationDomainHeadForTest(t, backend, storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true, Revision: 1, BaseRevision: 1, Frozen: true, FreezeEpoch: freezeEpoch, Base: base})
		putSchema007IndexedState(t, engine, logical, version, payload)
		return backend, engine
	}

	t.Run("already-installed", func(t *testing.T) {
		_, engine := setup(t, true)
		proof, err := engine.stageSchema009IndexedState010(ctx, freezeEpoch)
		if err != nil || proof.SourceEntryCount != 1 || proof.PresentCount != 1 || proof.RecoveredCount != 0 {
			t.Fatalf("already-installed proof = %+v, %v", proof, err)
		}
	})

	t.Run("root-entry-count-drift", func(t *testing.T) {
		backend, engine := setup(t, false)
		rootKey := storageformat.StateIndexRootKey(stateNamespace(logical))
		object, err := backend.Get(ctx, rootKey)
		if err != nil {
			t.Fatal(err)
		}
		var envelope storageformat.Envelope
		var root storageformat.StateIndexRoot
		if err := storageformat.DecodeEnvelope(object.Body, rootKey, stateIndexRootSchema, &envelope, &root); err != nil {
			t.Fatal(err)
		}
		root.EntryCount++
		body, err := storageformat.EncodeEnvelope(stateIndexRootSchema, rootKey, envelope.Revision+1, root)
		if err != nil {
			t.Fatal(err)
		}
		replaceMigrationObjectForTest(t, backend, rootKey, body)
		if _, err := engine.stageSchema009IndexedState010(ctx, freezeEpoch); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("state-index count drift error = %v", err)
		}
	})
}

func TestSchema009And010GroupedInstallationStopsBeforePublishingPartialDomains(t *testing.T) {
	ctx := context.Background()
	unavailable := domain.NewError(domain.ErrorUnavailable, "first group installation unavailable")

	t.Run("schema009", func(t *testing.T) {
		base := objectmemory.New()
		stages := make([]schema009MigrationStage, 0, 2)
		for _, owner := range []string{"owner-a", "owner-b"} {
			stages = append(stages, schema009MigrationStage{SchemaVersion: schema009MigrationStageSchema, SourceIdentity: "source-" + owner, DomainKind: storageformat.DomainIdentity, DomainID: owner, Tree: "base", Key: "preferences/" + owner + "/theme", Value: []byte("value"), LogicalVersion: "version"})
		}
		bodies := make(map[objectstore.Key][]byte, len(stages))
		infos := make([]objectstore.ObjectInfo, 0, len(stages))
		for _, stage := range stages {
			reference, body, err := validateSchema009MigrationStage(stage)
			if err != nil {
				t.Fatal(err)
			}
			key := storageformat.Schema009MigrationStageKey(schema008DomainIdentity(reference), stage.SourceIdentity)
			bodies[key] = body
			infos = append(infos, objectstore.ObjectInfo{Key: key})
		}
		sort.Slice(infos, func(i, j int) bool { return infos[i].Key.String() < infos[j].Key.String() })
		backend := &migrationScriptBackend{Backend: base}
		backend.list = func(context.Context, objectstore.ListRequest) (objectstore.ListPage, error) {
			return objectstore.ListPage{Objects: infos}, nil
		}
		backend.get = func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
			if body, found := bodies[key]; found {
				return objectstore.Object{Key: key, Body: body}, nil
			}
			return base.Get(ctx, key)
		}
		backend.put = func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if strings.Contains(key.String(), "/domains/") && strings.HasSuffix(key.String(), "/head.json") {
				return "", unavailable
			}
			return base.Put(ctx, key, body, condition)
		}
		if _, err := (&Engine{backend: backend}).installSchema009StagedDomains(ctx, 7); !errors.Is(err, unavailable) {
			t.Fatalf("grouped schema-009 installation error = %v", err)
		}
	})

	t.Run("schema010", func(t *testing.T) {
		base := objectmemory.New()
		proof := validSchema010ConservationForTest()
		putMigrationCatalogHeadForTest(t, base, proof.SourceCatalog)
		receipts := make([]schema010ConservationReceipt, 0, 2)
		for _, owner := range []string{"owner-a", "owner-b"} {
			logical := state.MustKey(state.NamespacePreferences, owner, "theme")
			payload := []byte(`{"themeID":"endlessfs-dark"}`)
			target, reference, recordType, value, err := migrateStateEntry009(logical, payload)
			if err != nil {
				t.Fatal(err)
			}
			receipts = append(receipts, schema010ConservationReceipt{SchemaVersion: schema010ConservationSchema, SourceRootKey: storageformat.StateIndexRootKey(stateNamespace(logical)).String(), SourceRootDigest: storageformat.Digest([]byte("root")), SourceLogicalKey: logical.String(), SourceLogicalVersion: "version", SourceVersionKey: storageformat.StateVersionKey(stateNamespace(logical), logical.String(), "version").String(), SourceVersionDigest: storageformat.Digest([]byte("version")), TargetDomainKind: reference.Kind, TargetDomainID: reference.ID, TargetKey: target.String(), TargetRecordType: recordType, TargetValue: value, TargetValueDigest: storageformat.Digest(value), Disposition: schema010DispositionRecover})
		}
		bodies := make(map[objectstore.Key][]byte, len(receipts))
		infos := make([]objectstore.ObjectInfo, 0, len(receipts))
		for _, receipt := range receipts {
			reference, _, body, err := validateSchema010Receipt(receipt)
			if err != nil {
				t.Fatal(err)
			}
			key := storageformat.Schema010MigrationReceiptKey(schema008DomainIdentity(reference), receipt.TargetKey)
			bodies[key] = body
			infos = append(infos, objectstore.ObjectInfo{Key: key})
		}
		sort.Slice(infos, func(i, j int) bool { return infos[i].Key.String() < infos[j].Key.String() })
		root := schema010ConservationRoot{Namespace: string(state.NamespacePreferences), RootKey: storageformat.StateIndexRootKey(string(state.NamespacePreferences)).String(), RootDigest: storageformat.Digest([]byte("root")), EntryCount: 2, ReceiptCommitment: storageformat.Digest([]byte("receipts"))}
		proof.Roots = []schema010ConservationRoot{root}
		proof.SourceEntryCount, proof.RecoveredCount = 2, 2
		refreshSchema010ProofCommitmentForTest(&proof)
		backend := &migrationScriptBackend{Backend: base}
		backend.list = func(context.Context, objectstore.ListRequest) (objectstore.ListPage, error) {
			return objectstore.ListPage{Objects: infos}, nil
		}
		backend.get = func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
			if body, found := bodies[key]; found {
				return objectstore.Object{Key: key, Body: body}, nil
			}
			return base.Get(ctx, key)
		}
		backend.put = func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if strings.Contains(key.String(), "/domains/") && strings.HasSuffix(key.String(), "/head.json") {
				return "", unavailable
			}
			return base.Put(ctx, key, body, condition)
		}
		if _, err := (&Engine{backend: backend}).installSchema010Receipts(ctx, proof); !errors.Is(err, unavailable) {
			t.Fatalf("grouped schema-010 installation error = %v", err)
		}
	})
}

func TestSchema010InstallationRejectsChangedCatalogAndCorruptMergeTrees(t *testing.T) {
	ctx := context.Background()
	freezeEpoch := uint64(7)
	reference := consistencyDomainRef{Kind: storageformat.DomainIdentity, ID: "owner:merge-boundary"}
	entryA := storageformat.DomainEntry{Key: "preferences/owner/a", Value: []byte("a"), LogicalVersion: "a-v1"}
	entryB := storageformat.DomainEntry{Key: "preferences/owner/b", Value: []byte("b"), LogicalVersion: "b-v1"}

	t.Run("published-catalog-cannot-mask-missing-receipts", func(t *testing.T) {
		backend := objectmemory.New()
		proof := conservationProofWithOneRootForTest()
		changed := proof.SourceCatalog
		changed.Root = storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("changed-catalog")), Level: 0, EntryCount: 1}
		putMigrationCatalogHeadForTest(t, backend, changed)
		if _, err := (&Engine{backend: backend}).installSchema010Receipts(ctx, proof); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("changed catalog installation error = %v", err)
		}
	})

	t.Run("recovered-root-unavailable", func(t *testing.T) {
		engine := &Engine{backend: objectmemory.New()}
		missing := storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("missing-recovered")), Level: 0, EntryCount: 1}
		if _, err := engine.schema010DomainContainsRecoveredRoot(ctx, engine.stateDomainStore(), reference, storageformat.DomainHead{}, missing); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing recovered root error = %v", err)
		}
	})

	t.Run("current-root-unavailable", func(t *testing.T) {
		backend := objectmemory.New()
		engine := &Engine{backend: backend}
		recovered := migrationRootForEntriesTest(t, engine, reference, entryA)
		missingHead := storageformat.DomainHead{Base: storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("missing-current")), Level: 0, EntryCount: 1}}
		if _, err := engine.schema010DomainContainsRecoveredRoot(ctx, engine.stateDomainStore(), reference, missingHead, recovered); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing current root error = %v", err)
		}
	})

	t.Run("installed-domain-current-root-unavailable", func(t *testing.T) {
		backend := objectmemory.New()
		engine := &Engine{backend: backend}
		recovered := migrationRootForEntriesTest(t, engine, reference, entryA)
		missing := storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("missing-installed-current")), Level: 0, EntryCount: 1}
		putMigrationDomainHeadForTest(t, backend, storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true, Revision: 1, BaseRevision: 1, Frozen: true, FreezeEpoch: freezeEpoch, Base: missing})
		if _, err := engine.installSchema010Domain(ctx, reference, recovered, freezeEpoch, true); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("installed domain missing current root error = %v", err)
		}
	})

	for _, providerErr := range []error{nil, domain.ErrConflict} {
		name := "conflicting-roots"
		if providerErr != nil {
			name = "existing-merge-contention"
		}
		t.Run(name, func(t *testing.T) {
			base := objectmemory.New()
			engineForRoots := &Engine{backend: base}
			baseRoot := migrationRootForEntriesTest(t, engineForRoots, reference, entryA)
			recoveredEntry := entryA
			if providerErr != nil {
				recoveredEntry = entryB
			} else {
				recoveredEntry.Value = []byte("changed")
				recoveredEntry.LogicalVersion = "changed-v1"
			}
			recovered := migrationRootForEntriesTest(t, engineForRoots, reference, recoveredEntry)
			putMigrationDomainHeadForTest(t, base, storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true, Revision: 1, BaseRevision: 1, Frozen: true, FreezeEpoch: freezeEpoch, Base: baseRoot})
			backend := objectstore.Backend(base)
			if providerErr != nil {
				headKey := storageformat.DomainHeadKey(reference.Kind, reference.ID)
				backend = &migrationScriptBackend{Backend: base, put: func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
					if key == headKey {
						return "", providerErr
					}
					return base.Put(ctx, key, body, condition)
				}}
			}
			_, err := (&Engine{backend: backend}).installSchema010Domain(ctx, reference, recovered, freezeEpoch, true)
			if providerErr == nil {
				if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrInvalid) {
					t.Fatalf("conflicting recovered roots error = %v", err)
				}
			} else if !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("existing merge contention error = %v", err)
			}
		})
	}
}

func TestSchema009And010InstallationPropagatesRecoveredDomainPageWriteFailures(t *testing.T) {
	ctx := context.Background()
	const artifactCount = domainPageMaximumItems
	value := []byte("value")
	pageFailure := domain.NewError(domain.ErrorUnavailable, "recovered domain page unavailable")

	t.Run("schema009", func(t *testing.T) {
		base := objectmemory.New()
		reference := consistencyDomainRef{Kind: storageformat.DomainIdentity, ID: "owner:oversized-page"}
		bodies := make(map[objectstore.Key][]byte, artifactCount)
		infos := make([]objectstore.ObjectInfo, 0, artifactCount)
		for index := range artifactCount {
			stage := schema009MigrationStage{SchemaVersion: schema009MigrationStageSchema, SourceIdentity: fmt.Sprintf("source-%03d", index), DomainKind: reference.Kind, DomainID: reference.ID, Tree: "base", Key: fmt.Sprintf("preferences/owner/theme-%03d", index), Value: value, LogicalVersion: "version"}
			_, body, err := validateSchema009MigrationStage(stage)
			if err != nil {
				t.Fatal(err)
			}
			key := storageformat.Schema009MigrationStageKey(schema008DomainIdentity(reference), stage.SourceIdentity)
			bodies[key] = body
			infos = append(infos, objectstore.ObjectInfo{Key: key})
		}
		sort.Slice(infos, func(i, j int) bool { return infos[i].Key.String() < infos[j].Key.String() })
		backend := &migrationScriptBackend{Backend: base, list: func(context.Context, objectstore.ListRequest) (objectstore.ListPage, error) {
			return objectstore.ListPage{Objects: infos}, nil
		}, get: func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
			if body, found := bodies[key]; found {
				return objectstore.Object{Key: key, Body: body}, nil
			}
			return base.Get(ctx, key)
		}, put: func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if strings.Contains(key.String(), "/domains/") && strings.Contains(key.String(), "/pages/") {
				return "", pageFailure
			}
			return base.Put(ctx, key, body, condition)
		}}
		if _, err := (&Engine{backend: backend}).installSchema009StagedDomains(ctx, 7); !errors.Is(err, pageFailure) {
			t.Fatalf("schema-009 domain page failure = %v", err)
		}
	})

	t.Run("schema010", func(t *testing.T) {
		base := objectmemory.New()
		proof := validSchema010ConservationForTest()
		putMigrationCatalogHeadForTest(t, base, proof.SourceCatalog)
		bodies := make(map[objectstore.Key][]byte, artifactCount)
		infos := make([]objectstore.ObjectInfo, 0, artifactCount)
		for index := range artifactCount {
			logical := state.MustKey(state.NamespacePreferences, "owner-oversized-page", fmt.Sprintf("theme-%03d", index))
			payload := []byte(`{"themeID":"endlessfs-dark"}`)
			target, reference, recordType, targetValue, err := migrateStateEntry009(logical, payload)
			if err != nil {
				t.Fatal(err)
			}
			receipt := schema010ConservationReceipt{SchemaVersion: schema010ConservationSchema, SourceRootKey: storageformat.StateIndexRootKey(stateNamespace(logical)).String(), SourceRootDigest: storageformat.Digest([]byte("root")), SourceLogicalKey: logical.String(), SourceLogicalVersion: "version", SourceVersionKey: storageformat.StateVersionKey(stateNamespace(logical), logical.String(), "version").String(), SourceVersionDigest: storageformat.Digest([]byte("version")), TargetDomainKind: reference.Kind, TargetDomainID: reference.ID, TargetKey: target.String(), TargetRecordType: recordType, TargetValue: targetValue, TargetValueDigest: storageformat.Digest(targetValue), Disposition: schema010DispositionRecover}
			_, _, body, err := validateSchema010Receipt(receipt)
			if err != nil {
				t.Fatal(err)
			}
			key := storageformat.Schema010MigrationReceiptKey(schema008DomainIdentity(reference), receipt.TargetKey)
			bodies[key] = body
			infos = append(infos, objectstore.ObjectInfo{Key: key})
		}
		sort.Slice(infos, func(i, j int) bool { return infos[i].Key.String() < infos[j].Key.String() })
		root := schema010ConservationRoot{Namespace: string(state.NamespacePreferences), RootKey: storageformat.StateIndexRootKey(string(state.NamespacePreferences)).String(), RootDigest: storageformat.Digest([]byte("root")), EntryCount: artifactCount, ReceiptCommitment: storageformat.Digest([]byte("receipts"))}
		proof.Roots = []schema010ConservationRoot{root}
		proof.SourceEntryCount, proof.RecoveredCount = artifactCount, artifactCount
		refreshSchema010ProofCommitmentForTest(&proof)
		backend := &migrationScriptBackend{Backend: base, list: func(context.Context, objectstore.ListRequest) (objectstore.ListPage, error) {
			return objectstore.ListPage{Objects: infos}, nil
		}, get: func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
			if body, found := bodies[key]; found {
				return objectstore.Object{Key: key, Body: body}, nil
			}
			return base.Get(ctx, key)
		}, put: func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if strings.Contains(key.String(), "/domains/") && strings.Contains(key.String(), "/pages/") {
				return "", pageFailure
			}
			return base.Put(ctx, key, body, condition)
		}}
		if _, err := (&Engine{backend: backend}).installSchema010Receipts(ctx, proof); !errors.Is(err, pageFailure) {
			t.Fatalf("schema-010 recovered domain page failure = %v", err)
		}
	})
}

func TestSchemaLegacyDirectoryWalkRejectsRecursiveByteOverflow(t *testing.T) {
	backend, engine, scope, root, manifest := emptyPhysicalMigrationRoot(t)
	entries := []storageformat.DirectoryEntry{
		migrationFileEntry(t, "one", math.MaxInt64),
		migrationFileEntry(t, "two", 1),
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].NameDigest < entries[j].NameDigest })
	pageID := "overflow-page"
	pageKey := storageformat.DirectoryPageKey(scope.UserID().String(), areaName(scope.Area()), storageformat.RootDirectoryID, pageID)
	migrationPut(t, backend, pageKey, migrationEnvelope(t, directoryPageSchema, pageKey, storageformat.DirectoryPage{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, PageID: pageID, Entries: entries}))
	manifestKey := storageformat.DirectoryManifestKey(scope.UserID().String(), areaName(scope.Area()), storageformat.RootDirectoryID, root.manifestID)
	replaceMigrationBody(t, backend, root.object.Key, migrationEnvelope(t, directoryRootSchema, root.object.Key, schema001DirectoryRoot{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, ManifestID: root.manifestID}))
	replaceMigrationBody(t, backend, manifestKey, migrationEnvelope(t, directoryManifestSchema, manifestKey, schema001DirectoryManifest{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, ManifestID: root.manifestID, PageIDs: []string{pageID}, EntryCount: len(entries), CreatedAt: manifest.manifest.CreatedAt}))
	walk := &migrationWalk{engine: engine, group: migrationScope{scope: scope, roots: map[string]struct{}{storageformat.RootDirectoryID: {}}}, transition: schemaMigration001To002, state: make(map[string]uint8), totals: make(map[string]migrationAggregate), parents: make(map[string]string)}
	if _, err := walk.directory(context.Background(), storageformat.RootDirectoryID, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("recursive byte overflow error = %v", err)
	}
}

func schema010InstalledVerificationFixture(t *testing.T) (map[string][]byte, schema010Conservation) {
	t.Helper()
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2046, 8, 27, 9, 10, 11, 0, time.UTC))
	engine, err := Open(ctx, internalMigration010Options(backend, clock, 0xa1))
	if err != nil {
		t.Fatal(err)
	}
	configureMigrationSourceSchema(t, backend, engine, storageSchema009)
	key := state.MustKey(state.NamespacePreferences, "owner-verification", "theme")
	putSchema007IndexedState(t, engine, key, "legacy-verification-v1", []byte(`{"themeID":"endlessfs-dark"}`))
	interrupted := internalMigration010Options(backend, clock, 0xa2)
	interrupted.Scheduler = SchedulerFunc(func(_ context.Context, step string) error {
		if step == MigrationStepName(string(storageMigration009To010), StepMigrationAfterDirectoryRoot) {
			return domain.NewError(domain.ErrorUnavailable, "capture installed schema-010 verification fixture")
		}
		return nil
	})
	if _, err := Open(ctx, interrupted); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("capture installed recovery state: %v", err)
	}
	proof, found, err := (&Engine{backend: backend}).readSchema010Conservation(ctx)
	if err != nil || !found || proof.SourceEntryCount != 1 || len(proof.Roots) != 1 {
		t.Fatalf("installed conservation proof = %+v, %t, %v", proof, found, err)
	}
	return backend.Export(), proof
}

func cloneMigrationBackendForTest(t *testing.T, objects map[string][]byte) *objectmemory.Backend {
	t.Helper()
	backend := objectmemory.New()
	if err := backend.Import(objects); err != nil {
		t.Fatal(err)
	}
	return backend
}

func replaceMigrationObjectForTest(t *testing.T, backend objectstore.Backend, key objectstore.Key, body []byte) {
	t.Helper()
	object, err := backend.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(context.Background(), key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
		t.Fatal(err)
	}
}

func deleteMigrationObjectForTest(t *testing.T, backend objectstore.Backend, key objectstore.Key) {
	t.Helper()
	object, err := backend.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Delete(context.Background(), key, objectstore.DeleteCondition{Version: object.Version}); err != nil {
		t.Fatal(err)
	}
}

func rewriteMigrationCatalogForTest(t *testing.T, backend objectstore.Backend, mutate func(*storageformat.DomainCatalogHead)) {
	t.Helper()
	key := storageformat.DomainCatalogHeadKey()
	object, err := backend.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var envelope storageformat.Envelope
	var head storageformat.DomainCatalogHead
	if err := storageformat.DecodeEnvelope(object.Body, key, domainCatalogHeadSchema, &envelope, &head); err != nil {
		t.Fatal(err)
	}
	mutate(&head)
	body, err := storageformat.EncodeEnvelope(domainCatalogHeadSchema, key, envelope.Revision+1, head)
	if err != nil {
		t.Fatal(err)
	}
	replaceMigrationObjectForTest(t, backend, key, body)
}

func rewriteMigrationDomainForTest(t *testing.T, backend objectstore.Backend, reference consistencyDomainRef, mutate func(*storageformat.DomainHead)) {
	t.Helper()
	key := storageformat.DomainHeadKey(reference.Kind, reference.ID)
	object, err := backend.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var envelope storageformat.Envelope
	var head storageformat.DomainHead
	if err := storageformat.DecodeEnvelope(object.Body, key, domainHeadSchema, &envelope, &head); err != nil {
		t.Fatal(err)
	}
	mutate(&head)
	body, err := storageformat.EncodeEnvelope(domainHeadSchema, key, envelope.Revision+1, head)
	if err != nil {
		t.Fatal(err)
	}
	replaceMigrationObjectForTest(t, backend, key, body)
}

func TestSchema010IndependentVerificationDeniesEachAuthorityArtifactFailure(t *testing.T) {
	ctx := context.Background()
	objects, proof := schema010InstalledVerificationFixture(t)
	var receipt schema010ConservationReceipt
	var receiptKey objectstore.Key
	baseline := cloneMigrationBackendForTest(t, objects)
	page, err := baseline.List(ctx, objectstore.ListRequest{Prefix: storageformat.Schema010MigrationReceiptPrefix(), Limit: 1000})
	if err != nil || len(page.Objects) != 1 {
		t.Fatalf("verification fixture receipts = %+v, %v", page, err)
	}
	receiptKey = page.Objects[0].Key
	receiptObject, err := baseline.Get(ctx, receiptKey)
	if err != nil || decodeCanonicalValue(receiptObject.Body, &receipt) != nil {
		t.Fatalf("verification fixture receipt = %+v, %v", receiptObject, err)
	}
	reference := consistencyDomainRef{Kind: receipt.TargetDomainKind, ID: receipt.TargetDomainID}

	tests := []struct {
		name   string
		mutate func(*testing.T, *objectmemory.Backend) objectstore.Backend
		want   error
	}{
		{name: "catalog-unavailable", want: domain.ErrUnavailable, mutate: func(_ *testing.T, backend *objectmemory.Backend) objectstore.Backend {
			return &migrationScriptBackend{Backend: backend, get: func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
				if key == storageformat.DomainCatalogHeadKey() {
					return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "catalog unavailable")
				}
				return backend.Get(ctx, key)
			}}
		}},
		{name: "catalog-freeze-epoch", want: domain.ErrPreconditionFailed, mutate: func(t *testing.T, backend *objectmemory.Backend) objectstore.Backend {
			rewriteMigrationCatalogForTest(t, backend, func(head *storageformat.DomainCatalogHead) { head.FreezeEpoch++ })
			return backend
		}},
		{name: "receipt-list-unavailable", want: domain.ErrUnavailable, mutate: func(_ *testing.T, backend *objectmemory.Backend) objectstore.Backend {
			return &migrationScriptBackend{Backend: backend, list: func(ctx context.Context, request objectstore.ListRequest) (objectstore.ListPage, error) {
				if request.Prefix == storageformat.Schema010MigrationReceiptPrefix() {
					return objectstore.ListPage{}, domain.NewError(domain.ErrorUnavailable, "receipt list unavailable")
				}
				return backend.List(ctx, request)
			}}
		}},
		{name: "receipt-get-unavailable", want: domain.ErrUnavailable, mutate: func(_ *testing.T, backend *objectmemory.Backend) objectstore.Backend {
			return &migrationScriptBackend{Backend: backend, get: func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
				if key == receiptKey {
					return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "receipt unavailable")
				}
				return backend.Get(ctx, key)
			}}
		}},
		{name: "receipt-malformed", want: domain.ErrInvalid, mutate: func(t *testing.T, backend *objectmemory.Backend) objectstore.Backend {
			replaceMigrationObjectForTest(t, backend, receiptKey, []byte(`{}`))
			return backend
		}},
		{name: "receipt-noncanonical", want: domain.ErrInvalid, mutate: func(t *testing.T, backend *objectmemory.Backend) objectstore.Backend {
			noncanonical := append(append([]byte(nil), receiptObject.Body[:len(receiptObject.Body)-1]...), ' ', '}')
			replaceMigrationObjectForTest(t, backend, receiptKey, noncanonical)
			return backend
		}},
		{name: "target-catalog-entry-missing", want: domain.ErrPreconditionFailed, mutate: func(t *testing.T, backend *objectmemory.Backend) objectstore.Backend {
			rewriteMigrationCatalogForTest(t, backend, func(head *storageformat.DomainCatalogHead) { head.Root = storageformat.DomainTreeRoot{} })
			return backend
		}},
		{name: "target-head-missing", want: domain.ErrPreconditionFailed, mutate: func(t *testing.T, backend *objectmemory.Backend) objectstore.Backend {
			deleteMigrationObjectForTest(t, backend, storageformat.DomainHeadKey(reference.Kind, reference.ID))
			return backend
		}},
		{name: "target-value-missing", want: domain.ErrPreconditionFailed, mutate: func(t *testing.T, backend *objectmemory.Backend) objectstore.Backend {
			rewriteMigrationDomainForTest(t, backend, reference, func(head *storageformat.DomainHead) {
				head.Revision++
				head.BaseRevision = head.Revision
				head.Base = storageformat.DomainTreeRoot{}
			})
			return backend
		}},
		{name: "target-receipt-count", want: domain.ErrPreconditionFailed, mutate: func(t *testing.T, backend *objectmemory.Backend) objectstore.Backend {
			deleteMigrationObjectForTest(t, backend, receiptKey)
			return backend
		}},
		{name: "source-root-missing", want: domain.ErrNotFound, mutate: func(t *testing.T, backend *objectmemory.Backend) objectstore.Backend {
			deleteMigrationObjectForTest(t, backend, objectstore.MustKey(proof.Roots[0].RootKey))
			return backend
		}},
		{name: "source-root-changed", want: domain.ErrPreconditionFailed, mutate: func(t *testing.T, backend *objectmemory.Backend) objectstore.Backend {
			key := objectstore.MustKey(proof.Roots[0].RootKey)
			object, getErr := backend.Get(ctx, key)
			if getErr != nil {
				t.Fatal(getErr)
			}
			replaceMigrationObjectForTest(t, backend, key, append(object.Body, ' '))
			return backend
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := cloneMigrationBackendForTest(t, objects)
			wrapped := test.mutate(t, backend)
			if err := (&Engine{backend: wrapped}).verifySchema010Conservation(ctx, proof); !errors.Is(err, test.want) {
				t.Fatalf("independent verification error = %v; want %v", err, test.want)
			}
		})
	}
}

func refreshSchema010ProofCommitmentForTest(proof *schema010Conservation) {
	commitment := sha256.New()
	for _, root := range proof.Roots {
		writeSchema010Commitment(commitment, root.RootKey, root.RootDigest, root.ReceiptCommitment)
	}
	proof.Commitment = hex.EncodeToString(commitment.Sum(nil))
}

func TestSchema010IndependentVerificationRechecksSourceArtifactsAfterTargetVerification(t *testing.T) {
	ctx := context.Background()
	objects, proof := schema010InstalledVerificationFixture(t)
	baseline := cloneMigrationBackendForTest(t, objects)
	page, err := baseline.List(ctx, objectstore.ListRequest{Prefix: storageformat.Schema010MigrationReceiptPrefix(), Limit: 1000})
	if err != nil || len(page.Objects) != 1 {
		t.Fatalf("verification fixture receipts = %+v, %v", page, err)
	}
	receiptKey := page.Objects[0].Key
	receiptObject, err := baseline.Get(ctx, receiptKey)
	if err != nil {
		t.Fatal(err)
	}
	var receipt schema010ConservationReceipt
	if err := decodeCanonicalValue(receiptObject.Body, &receipt); err != nil {
		t.Fatal(err)
	}

	t.Run("source-root-envelope", func(t *testing.T) {
		backend := cloneMigrationBackendForTest(t, objects)
		candidate := proof
		candidate.Roots = append([]schema010ConservationRoot(nil), proof.Roots...)
		rootKey := objectstore.MustKey(candidate.Roots[0].RootKey)
		replaceMigrationObjectForTest(t, backend, rootKey, []byte(`{}`))
		candidate.Roots[0].RootDigest = storageformat.Digest([]byte(`{}`))
		refreshSchema010ProofCommitmentForTest(&candidate)
		if err := (&Engine{backend: backend}).verifySchema010Conservation(ctx, candidate); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("malformed source root error = %v", err)
		}
	})

	t.Run("source-index-node-missing", func(t *testing.T) {
		backend := cloneMigrationBackendForTest(t, objects)
		indexPage, err := backend.List(ctx, objectstore.ListRequest{Prefix: storageformat.StateIndexRootPrefix(), Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		var nodeKey objectstore.Key
		for _, info := range indexPage.Objects {
			if strings.Contains(info.Key.String(), "/nodes/") {
				nodeKey = info.Key
				break
			}
		}
		if nodeKey.String() == "" {
			t.Fatal("verification fixture has no state-index node")
		}
		deleteMigrationObjectForTest(t, backend, nodeKey)
		if err := (&Engine{backend: backend}).verifySchema010Conservation(ctx, proof); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing source index node error = %v", err)
		}
	})

	t.Run("source-receipt-commitment-mismatch", func(t *testing.T) {
		backend := cloneMigrationBackendForTest(t, objects)
		candidate := proof
		candidate.Roots = append([]schema010ConservationRoot(nil), proof.Roots...)
		candidate.Roots[0].ReceiptCommitment = storageformat.Digest([]byte("changed-receipt-commitment"))
		refreshSchema010ProofCommitmentForTest(&candidate)
		if err := (&Engine{backend: backend}).verifySchema010Conservation(ctx, candidate); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("source receipt commitment mismatch error = %v", err)
		}
	})

	for _, test := range []struct {
		name string
		body func() ([]byte, error)
		want error
	}{
		{name: "receipt-unavailable", want: domain.ErrUnavailable},
		{name: "receipt-malformed", want: domain.ErrInvalid, body: func() ([]byte, error) { return []byte(`{`), nil }},
		{name: "receipt-invalid-disposition", want: domain.ErrInvalid, body: func() ([]byte, error) {
			changed := receipt
			changed.Disposition = "invalid"
			return storageformat.EncodeCanonical(changed)
		}},
		{name: "receipt-changed", want: domain.ErrPreconditionFailed, body: func() ([]byte, error) {
			changed := receipt
			changed.SourceRootDigest = storageformat.Digest([]byte("changed"))
			return storageformat.EncodeCanonical(changed)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := cloneMigrationBackendForTest(t, objects)
			gets := 0
			wrapped := &migrationScriptBackend{Backend: backend, get: func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
				if key != receiptKey {
					return backend.Get(ctx, key)
				}
				gets++
				if gets == 1 {
					return backend.Get(ctx, key)
				}
				if test.body == nil {
					return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "source receipt unavailable")
				}
				body, err := test.body()
				return objectstore.Object{Key: key, Body: body, Version: "changed"}, err
			}}
			if err := (&Engine{backend: wrapped}).verifySchema010Conservation(ctx, proof); !errors.Is(err, test.want) {
				t.Fatalf("second-phase source receipt error = %v; want %v", err, test.want)
			}
			if gets != 2 {
				t.Fatalf("receipt reads = %d; want 2", gets)
			}
		})
	}
}

func TestSchema008And009CompletedMigrationsShortCircuitWithoutRewritingAuthority(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2046, 8, 27, 14, 0, 0, 0, time.UTC))
	engine, err := Open(ctx, internalMigration010Options(backend, clock, 0xc1))
	if err != nil {
		t.Fatal(err)
	}
	object, err := backend.Get(ctx, storageformat.SuperblockKey())
	if err != nil {
		t.Fatal(err)
	}
	var superblock storageformat.Superblock
	if err := decodeCanonicalSuperblock(object.Body, &superblock); err != nil {
		t.Fatal(err)
	}
	if err := engine.runStorageMigration007To008(ctx, schemaMigration007To008, object, superblock); err != nil {
		t.Fatalf("completed schema-008 migration = %v", err)
	}
	if err := engine.runStorageMigration008To009(ctx, schemaMigration008To009, object, superblock); err != nil {
		t.Fatalf("completed schema-009 migration = %v", err)
	}
}

func conservationProofWithOneRootForTest() schema010Conservation {
	root := schema010ConservationRoot{
		Namespace:  string(state.NamespacePreferences),
		RootKey:    storageformat.StateIndexRootKey(string(state.NamespacePreferences)).String(),
		RootDigest: storageformat.Digest([]byte("root")), EntryCount: 1,
		ReceiptCommitment: storageformat.Digest([]byte("receipt")),
	}
	commitment := sha256.New()
	writeSchema010Commitment(commitment, root.RootKey, root.RootDigest, root.ReceiptCommitment)
	proof := validSchema010ConservationForTest()
	proof.Roots = []schema010ConservationRoot{root}
	proof.SourceEntryCount = 1
	proof.RecoveredCount = 1
	proof.Commitment = hex.EncodeToString(commitment.Sum(nil))
	return proof
}

func TestSchema010StagingAndInstallationDenyInvalidBoundaries(t *testing.T) {
	ctx := context.Background()
	proof := validSchema010ConservationForTest()
	body, err := validateSchema010Conservation(proof)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("existing-proof-other-epoch", func(t *testing.T) {
		backend := objectmemory.New()
		if _, err := backend.Put(ctx, storageformat.Schema010MigrationConservationKey(), body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if _, err := (&Engine{backend: backend}).stageSchema009IndexedState010(ctx, proof.FreezeEpoch+1); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("other-epoch proof error = %v", err)
		}
	})
	t.Run("source-catalog-not-frozen", func(t *testing.T) {
		backend := objectmemory.New()
		putMigrationCatalogHeadForTest(t, backend, storageformat.DomainCatalogHead{SchemaVersion: 1, Revision: 1, FreezeEpoch: proof.FreezeEpoch - 1})
		if _, err := (&Engine{backend: backend}).stageSchema009IndexedState010(ctx, proof.FreezeEpoch); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("unfrozen source catalog error = %v", err)
		}
	})
	t.Run("unknown-state-index-object", func(t *testing.T) {
		backend := objectmemory.New()
		putMigrationCatalogHeadForTest(t, backend, proof.SourceCatalog)
		key := objectstore.MustKey(storageformat.StateIndexRootPrefix() + "unknown.json")
		if _, err := backend.Put(ctx, key, []byte(`{}`), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if _, err := (&Engine{backend: backend}).stageSchema009IndexedState010(ctx, proof.FreezeEpoch); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("unknown state-index object error = %v", err)
		}
	})
	t.Run("invalid-state-index-root", func(t *testing.T) {
		backend := objectmemory.New()
		putMigrationCatalogHeadForTest(t, backend, proof.SourceCatalog)
		key := storageformat.StateIndexRootKey(string(state.NamespacePreferences))
		if _, err := backend.Put(ctx, key, []byte(`{}`), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if _, err := (&Engine{backend: backend}).stageSchema009IndexedState010(ctx, proof.FreezeEpoch); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid state-index root error = %v", err)
		}
	})
	t.Run("receipt-count-mismatch", func(t *testing.T) {
		backend := objectmemory.New()
		nonempty := conservationProofWithOneRootForTest()
		putMigrationCatalogHeadForTest(t, backend, nonempty.SourceCatalog)
		if _, err := (&Engine{backend: backend}).installSchema010Receipts(ctx, nonempty); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("receipt count mismatch error = %v", err)
		}
	})
	for _, test := range []struct {
		name string
		get  func(context.Context, objectstore.Key) (objectstore.Object, error)
		want error
	}{
		{name: "winner-unavailable", want: domain.ErrUnavailable, get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "winner unavailable")
		}},
		{name: "winner-malformed", want: domain.ErrInvalid, get: func(_ context.Context, key objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{Key: key, Body: []byte(`{}`)}, nil
		}},
		{name: "winner-differs", want: domain.ErrInvalid, get: func(_ context.Context, key objectstore.Key) (objectstore.Object, error) {
			other := proof
			other.FreezeEpoch++
			other.SourceCatalog.FreezeEpoch++
			otherBody, _ := validateSchema010Conservation(other)
			return objectstore.Object{Key: key, Body: otherBody}, nil
		}},
	} {
		t.Run("proof-"+test.name, func(t *testing.T) {
			base := objectmemory.New()
			putMigrationCatalogHeadForTest(t, base, proof.SourceCatalog)
			proofGets := 0
			backend := &migrationScriptBackend{Backend: base, put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
				return "", domain.ErrConflict
			}, get: func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
				if key == storageformat.Schema010MigrationConservationKey() {
					proofGets++
					if proofGets == 1 {
						return objectstore.Object{}, domain.ErrNotFound
					}
					return test.get(ctx, key)
				}
				return base.Get(ctx, key)
			}}
			if _, err := (&Engine{backend: backend}).stageSchema009IndexedState010(ctx, proof.FreezeEpoch); !errors.Is(err, test.want) {
				t.Fatalf("proof winner error = %v; want %v", err, test.want)
			}
		})
	}
}

func TestMigrationSchema009RecoversIndexedApplicationStateDroppedBySchema008(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2046, 8, 27, 1, 2, 3, 0, time.UTC))
	options := internalMigration010Options(backend, clock, 0x41)
	engine, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	configureMigrationSourceSchema(t, backend, engine, storageSchema009)

	preference := state.MustKey(state.NamespacePreferences, "dXNlci0wMDAx")
	want := []byte(`{"themeID":"endlessfs-dark"}`)
	putSchema007IndexedState(t, engine, preference, "legacy-preference-v1", want)

	reopened, err := Open(ctx, internalMigration010Options(backend, clock, 0x42))
	if err != nil {
		t.Fatalf("reopen production-shaped schema-009 state: %v", err)
	}
	got, err := reopened.Get(ctx, preference)
	if err != nil {
		t.Fatalf("recovered indexed preference: %v", err)
	}
	if !bytes.Equal(got.Data, want) || got.Version != state.Version("legacy-preference-v1") {
		t.Fatalf("recovered indexed preference = %+v; want version and bytes preserved", got)
	}
}

func TestMigrationSchema010FailsClosedForConflictingCurrentState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2046, 8, 27, 2, 3, 4, 0, time.UTC))
	engine, err := Open(ctx, internalMigration010Options(backend, clock, 0x51))
	if err != nil {
		t.Fatal(err)
	}
	preference := state.MustKey(state.NamespacePreferences, "owner-a")
	if _, err := engine.Create(ctx, preference, []byte(`{"themeID":"endlessfs-light"}`)); err != nil {
		t.Fatal(err)
	}
	configureMigrationSourceSchema(t, backend, engine, storageSchema009)
	putSchema007IndexedState(t, engine, preference, "legacy-preference-v1", []byte(`{"themeID":"endlessfs-dark"}`))

	if _, err := Open(ctx, internalMigration010Options(backend, clock, 0x52)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("conflicting indexed-state recovery error = %v; want conflict", err)
	}
	assertMigration010SourceSchemaRemainsActive(t, backend)
}

func TestMigrationSchema010FailsClosedWhenIndexedVersionIsMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2046, 8, 27, 3, 4, 5, 0, time.UTC))
	engine, err := Open(ctx, internalMigration010Options(backend, clock, 0x61))
	if err != nil {
		t.Fatal(err)
	}
	configureMigrationSourceSchema(t, backend, engine, storageSchema009)
	key := state.MustKey(state.NamespaceAccounts, "owner-a")
	putSchema007IndexedState(t, engine, key, "legacy-account-v1", []byte(`{"userID":"owner-a","status":"enabled"}`))
	versionKey := storageformat.StateVersionKey(stateNamespace(key), key.String(), "legacy-account-v1")
	object, err := backend.Get(ctx, versionKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Delete(ctx, versionKey, objectstore.DeleteCondition{Version: object.Version}); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(ctx, internalMigration010Options(backend, clock, 0x62)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing indexed version recovery error = %v; want not found", err)
	}
	assertMigration010SourceSchemaRemainsActive(t, backend)
}

func TestMigrationSchema010RejectsCorruptDurableConservationReceiptOnRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2046, 8, 27, 4, 5, 6, 0, time.UTC))
	engine, err := Open(ctx, internalMigration010Options(backend, clock, 0x71))
	if err != nil {
		t.Fatal(err)
	}
	configureMigrationSourceSchema(t, backend, engine, storageSchema009)
	key := state.MustKey(state.NamespacePreferences, "owner-a")
	putSchema007IndexedState(t, engine, key, "legacy-preference-v1", []byte(`{"themeID":"endlessfs-dark"}`))

	interrupted := internalMigration010Options(backend, clock, 0x72)
	interrupted.Scheduler = SchedulerFunc(func(_ context.Context, step string) error {
		if step == MigrationStepName(string(storageMigration009To010), StepMigrationAfterDirectoryPrerequisites) {
			return domain.NewError(domain.ErrorUnavailable, "injected schema-010 interruption")
		}
		return nil
	})
	if _, err := Open(ctx, interrupted); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("interrupted recovery error = %v; want unavailable", err)
	}
	var receiptKey objectstore.Key
	for keyValue := range backend.Export() {
		if strings.HasPrefix(keyValue, storageformat.Schema010MigrationReceiptPrefix()) {
			receiptKey = objectstore.MustKey(keyValue)
			break
		}
	}
	if !receiptKey.Valid() {
		t.Fatal("interrupted migration wrote no conservation receipt")
	}
	receipt, err := backend.Get(ctx, receiptKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, receiptKey, []byte(`{}`), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: receipt.Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, internalMigration010Options(backend, clock, 0x73)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt receipt restart error = %v; want invalid", err)
	}
	assertMigration010SourceSchemaRemainsActive(t, backend)
}

func TestMigrationSchema010ConcurrentReplicasConvergeWithIndexedState(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2046, 8, 27, 5, 6, 7, 0, time.UTC))
	engine, err := Open(ctx, internalMigration010Options(backend, clock, 0x81))
	if err != nil {
		t.Fatal(err)
	}
	configureMigrationSourceSchema(t, backend, engine, storageSchema009)
	key := state.MustKey(state.NamespacePreferences, "owner-a")
	want := []byte(`{"themeID":"endlessfs-dark"}`)
	putSchema007IndexedState(t, engine, key, "legacy-preference-v1", want)

	results := make(chan error, 8)
	for index := range 8 {
		go func() {
			_, err := Open(ctx, internalMigration010Options(backend, clock, byte(0x82+index)))
			results <- err
		}()
	}
	for range 8 {
		if err := <-results; err != nil {
			chain := []string{}
			for current := err; current != nil; current = errors.Unwrap(current) {
				chain = append(chain, current.Error())
			}
			t.Fatalf("concurrent schema-010 migration: %s", strings.Join(chain, ": "))
		}
	}
	reopened, err := Open(ctx, internalMigration010Options(backend, clock, 0x91))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get(ctx, key)
	if err != nil || !bytes.Equal(got.Data, want) || got.Version != "legacy-preference-v1" {
		t.Fatalf("converged indexed state = %+v, %v", got, err)
	}
}

func internalMigration010Options(backend objectstore.Backend, clock domain.Clock, seed byte) Options {
	return Options{
		Backend: backend,
		Clock:   clock,
		IDs:     domain.NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{seed}, 1<<20))),
		Writer: WriterConfiguration{
			WriterSetID:         "d3JpdGVyLXNldC0wMDAx",
			ConfigurationDigest: "config-v1",
			KeyringIdentifiers:  []string{"session-v1"},
		},
		LeaseTTL:  time.Minute,
		CursorKey: bytes.Repeat([]byte{0x63}, 32),
	}
}

func putSchema007IndexedState(t *testing.T, engine *Engine, key state.Key, version string, data []byte) {
	t.Helper()
	ctx := context.Background()
	value, err := stateVersionObject(key, state.Version(version), data)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ensureMutationPrerequisites(ctx, []storageformat.MutationObject{value}); err != nil {
		t.Fatal(err)
	}
	prepared, err := engine.prepareStateIndexMutation(ctx, key, version, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ensureMutationPrerequisites(ctx, prepared.prerequisites); err != nil {
		t.Fatal(err)
	}
	condition := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
	if prepared.snapshot.exists {
		condition = objectstore.PutCondition{Mode: objectstore.PutMatch, Version: prepared.snapshot.object.Version}
	}
	if _, err := engine.backend.Put(ctx, storageformat.StateIndexRootKey(stateNamespace(key)), prepared.rootBody, condition); err != nil {
		t.Fatal(err)
	}
	indexed, err := engine.stateIndexEntry(ctx, key)
	if err != nil || indexed.LogicalVersion != version {
		t.Fatalf("seed schema-007 indexed state = %+v, %v", indexed, err)
	}
}

func assertMigration010SourceSchemaRemainsActive(t *testing.T, backend objectstore.Backend) {
	t.Helper()
	object, err := backend.Get(context.Background(), storageformat.SuperblockKey())
	if err != nil {
		t.Fatal(err)
	}
	var superblock storageformat.Superblock
	if err := decodeCanonicalSuperblock(object.Body, &superblock); err != nil {
		t.Fatal(err)
	}
	detected, found := detectStorageSchema(superblock.RequiredFeatures, nil)
	if !found || detected.id != storageSchema009 {
		t.Fatalf("failed migration activated schema = %+v, %t; want schema-009", detected, found)
	}
}
