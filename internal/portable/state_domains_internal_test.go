package portable

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestSchema008StateRoutingRemainsFrozenForMigration(t *testing.T) {
	ownerA := "WVhXWVhXWVhXWVhXWVhXWQ"
	ownerB := "aGhoaGhoaGhoaGhoaGhoaA"
	cases := []struct {
		key  state.Key
		kind storageformat.ConsistencyDomainKind
		id   string
	}{
		{state.MustKey(state.NamespaceUsers, ownerA), storageformat.DomainOwnerControl, "owner:" + ownerA},
		{state.MustKey(state.NamespaceAccounts, ownerB), storageformat.DomainOwnerControl, "owner:" + ownerB},
		{state.MustKey(state.NamespacePreferences, ownerA, "theme"), storageformat.DomainOwnerControl, "owner:" + ownerA},
		{state.MustKey(state.NamespaceTrash, ownerA, "trash-id"), storageformat.DomainOwnerControl, "owner:" + ownerA},
		{state.MustKey(state.NamespaceUploads, ownerA, "upload-id"), storageformat.DomainOwnerControl, "owner:" + ownerA},
		{state.MustKey(state.NamespaceCredentials, "user-index", ownerB), storageformat.DomainOwnerControl, "owner:" + ownerB},
		{state.MustKey(state.NamespaceIdempotency, ownerA, "request"), storageformat.DomainOwnerControl, "owner:" + ownerA},
		{state.MustKey(state.NamespaceIdempotency, "preview", ownerB, "request"), storageformat.DomainOwnerControl, "owner:" + ownerB},
		{state.MustKey(state.NamespaceIdempotency, "drive", ownerA, "request"), storageformat.DomainOwnerControl, "owner:" + ownerA},
		{state.MustKey(state.NamespaceOperations, "preview", ownerB, "operation"), storageformat.DomainOwnerControl, "owner:" + ownerB},
		{state.MustKey(state.NamespaceOperations, "preview-index", ownerA, "operation"), storageformat.DomainOwnerControl, "owner:" + ownerA},
		{state.MustKey(state.NamespaceOperations, "batch", ownerB, "operation"), storageformat.DomainOwnerControl, "owner:" + ownerB},
		{state.MustKey(state.NamespaceBootstrap, "state"), storageformat.DomainAdmin, "administration"},
		{state.MustKey(state.NamespaceRoles, "admins"), storageformat.DomainAdmin, "administration"},
	}
	for _, test := range cases {
		reference, err := stateDomainReferenceForKey008(test.key)
		if err != nil || reference.Kind != test.kind || reference.ID != test.id {
			t.Errorf("route %q = %+v, %v; want %s/%s", test.key.String(), reference, err, test.kind, test.id)
		}
	}

	first, err := stateDomainReferenceForKey008(state.MustKey(state.NamespaceSessions, "aaa-token"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := stateDomainReferenceForKey008(state.MustKey(state.NamespaceSessions, "bbb-token"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Kind != storageformat.DomainCapability || second.Kind != storageformat.DomainCapability || first.ID == second.ID {
		t.Fatalf("session shards = %+v and %+v", first, second)
	}
	for _, key := range []state.Key{
		state.MustKey(state.NamespaceCredentials, "credential-id"),
		state.MustKey(state.NamespaceCeremonies, "ceremony-id"),
		state.MustKey(state.NamespaceInvites, "invite-id"),
		state.MustKey(state.NamespaceRecoveries, "recovery-id"),
		state.MustKey(state.NamespaceOperations, "operation-id"),
	} {
		reference, err := stateDomainReferenceForKey008(key)
		if err != nil || reference.Kind != storageformat.DomainCapability || !strings.HasPrefix(reference.ID, "state:") {
			t.Fatalf("capability route %q = %+v, %v", key.String(), reference, err)
		}
	}
	share, err := stateDomainReferenceForKey008(state.MustKey(state.NamespaceShares, "share-token"))
	if err != nil || share.Kind != storageformat.DomainShare || !strings.HasPrefix(share.ID, "state:") {
		t.Fatalf("share route = %+v, %v", share, err)
	}
	namespace, parts, err := decodedStatePath(state.MustPrefix(state.NamespaceAccounts, ownerA).String(), true)
	route := stateRouteForPath008(namespace, parts)
	if err != nil || !route.exact || route.reference.ID != "owner:"+ownerA {
		t.Fatalf("exact schema-008 state prefix route = %+v, %v", route, err)
	}
	if reference, exact, err := stateDomainReferenceForPrefix(state.MustPrefix(state.NamespaceAccounts)); err != nil || exact || reference != (consistencyDomainRef{}) {
		t.Fatalf("cross-domain state prefix route = %+v, %v, %v", reference, exact, err)
	}
	if _, err := stateDomainReferenceForKey(state.Key{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid state key route error = %v", err)
	}
	if _, _, err := stateDomainReferenceForPrefix(state.Prefix{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid state prefix route error = %v", err)
	}
	for _, invalid := range []string{"", "accounts/%", "accounts/"} {
		if _, _, err := decodedStatePath(invalid, false); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid state path %q error = %v", invalid, err)
		}
	}
}

func TestUnrelatedOwnerStateMutationsDoNotContendOnOneHead(t *testing.T) {
	ctx := context.Background()
	base := objectmemory.New()
	engine := openNamespaceTestEngine(t, base)
	ownerA := state.MustKey(state.NamespaceAccounts, "WVhXWVhXWVhXWVhXWVhXWQ")
	ownerB := state.MustKey(state.NamespaceAccounts, "aGhoaGhoaGhoaGhoaGhoaA")
	if _, err := engine.Create(ctx, ownerA, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Create(ctx, ownerB, []byte("b")); err != nil {
		t.Fatal(err)
	}
	referenceA, _ := stateDomainReferenceForKey(ownerA)
	referenceB, _ := stateDomainReferenceForKey(ownerB)
	if referenceA == referenceB {
		t.Fatal("unrelated owners share one consistency-domain head")
	}
	objects := base.Export()
	if len(objects[storageformat.DomainHeadKey(referenceA.Kind, referenceA.ID).String()]) == 0 || len(objects[storageformat.DomainHeadKey(referenceB.Kind, referenceB.ID).String()]) == 0 {
		t.Fatalf("owner heads are absent: %+v", objects)
	}
}

func TestWarmStateMutationPublishesOneConditionalHead(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
	engine := openNamespaceTestEngine(t, backend)
	key := state.MustKey(state.NamespacePreferences, "WVhXWVhXWVhXWVhXWVhXWQ", "theme")
	version, err := engine.Create(ctx, key, []byte("dark"))
	if err != nil {
		t.Fatal(err)
	}
	ledger.Reset()
	if _, err := engine.CompareAndSwap(ctx, key, version, []byte("light")); err != nil {
		t.Fatal(err)
	}
	events := ledger.Events()
	if len(events) != 2 || events[0].Kind != providerbudget.RequestObjectGet || events[1].Kind != providerbudget.RequestObjectPut {
		t.Fatalf("warm state mutation = %+v; want head GET plus one conditional publication", events)
	}
	for _, event := range events {
		if event.Kind == providerbudget.RequestObjectList || event.Kind == providerbudget.RequestObjectDelete || event.Kind == providerbudget.RequestObjectOpen || event.Kind == providerbudget.RequestObjectCopy {
			t.Fatalf("state mutation used an unrelated provider operation: %+v", event)
		}
	}
}

func TestStateMutationDenialWritesNothingInSelectedDomain(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
	engine := openNamespaceTestEngine(t, backend)
	key := state.MustKey(state.NamespaceAccounts, "WVhXWVhXWVhXWVhXWVhXWQ")
	if _, err := engine.Create(ctx, key, []byte("one")); err != nil {
		t.Fatal(err)
	}
	ledger.Reset()
	if _, err := engine.CompareAndSwap(ctx, key, "stale", []byte("two")); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale CAS error = %v", err)
	}
	for _, event := range ledger.Events() {
		if event.Kind == providerbudget.RequestObjectPut || event.Kind == providerbudget.RequestObjectDelete || event.Kind == providerbudget.RequestObjectCopy || event.Kind == providerbudget.RequestObjectOpen {
			t.Fatalf("denied state mutation wrote provider state: %+v", event)
		}
	}
}

func TestStateMutationNeverTouchesFileProvider(t *testing.T) {
	ctx := context.Background()
	stateLedger, fileLedger := providerbudget.NewLedger(), providerbudget.NewLedger()
	engine := openInternalTestEngine(t,
		budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), stateLedger),
		domain.NewFixedClock(time.Date(2052, 1, 2, 3, 4, 5, 0, time.UTC)),
		strings.NewReader(strings.Repeat("state-domain-test-entropy-", 1<<14)),
	)
	engine.fileBackend = budgettest.Wrap(providerbudget.RoleFile, objectmemory.New(), fileLedger)
	if _, err := engine.Create(ctx, state.MustKey(state.NamespaceInvites, "capability-token"), []byte("invite")); err != nil {
		t.Fatal(err)
	}
	if len(stateLedger.Events()) == 0 || len(fileLedger.Events()) != 0 {
		t.Fatalf("state=%+v file=%+v", stateLedger.Events(), fileLedger.Events())
	}
	for _, event := range stateLedger.Events() {
		if event.Kind == providerbudget.RequestObjectOpen {
			t.Fatalf("state mutation streamed an object: %+v", event)
		}
	}
}

func TestCrossDomainStateListBuildsImmutableBoundedSnapshot(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	owners := []string{
		"WVhXWVhXWVhXWVhXWVhXWQ",
		"aGhoaGhoaGhoaGhoaGhoaA",
		"aWlpaWlpaWlpaWlpaWlpaQ",
	}
	versions := make(map[string]state.Version)
	for _, owner := range owners {
		key := state.MustKey(state.NamespaceUsers, owner)
		version, err := engine.Create(ctx, key, []byte(owner))
		if err != nil {
			t.Fatal(err)
		}
		versions[owner] = version
	}
	prefix := state.MustPrefix(state.NamespaceUsers)
	first, err := engine.List(ctx, prefix, state.PageRequest{Limit: 1})
	if err != nil || len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatalf("first cross-domain page = %+v, %v", first, err)
	}
	if _, err := engine.CompareAndSwap(ctx, state.MustKey(state.NamespaceUsers, owners[1]), versions[owners[1]], []byte("changed")); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Create(ctx, state.MustKey(state.NamespaceUsers, "bG1ubG1ubG1ubG1ubG1ubQ"), []byte("later")); err != nil {
		t.Fatal(err)
	}
	seen := append([]state.Item(nil), first.Items...)
	cursor := first.NextCursor
	for cursor != "" {
		page, err := engine.List(ctx, prefix, state.PageRequest{Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, page.Items...)
		cursor = page.NextCursor
	}
	if len(seen) != len(owners) {
		t.Fatalf("snapshot items = %d, want %d: %+v", len(seen), len(owners), seen)
	}
	for _, item := range seen {
		if string(item.Value.Data) == "changed" || string(item.Value.Data) == "later" {
			t.Fatalf("cross-domain cursor observed post-snapshot data: %+v", item)
		}
	}
}

func TestSchema008StateCursorMutationAndStoredKeyDenialMatrix(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	owner := "WVhXWVhXWVhXWVhXWVhXWQ"
	exactPrefix := state.MustPrefix(state.NamespaceAccounts, owner)
	compositePrefix := state.MustPrefix(state.NamespaceAccounts)
	expires := engine.clock.Now().Add(time.Hour)

	compositeOnExact, err := engine.encodeStateListCursor(stateListCursor{SchemaVersion: 4, Prefix: exactPrefix.String(), Limit: 1, Namespace: string(state.NamespaceAccounts), Revision: 1, Snapshot: "snapshot", Composite: true, After: exactPrefix.String() + "value", ExpiresAt: expires})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.List(ctx, exactPrefix, state.PageRequest{Limit: 1, Cursor: compositeOnExact}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("composite cursor on exact route error = %v", err)
	}
	nonCompositeOnComposite, err := engine.encodeStateListCursor(stateListCursor{SchemaVersion: 4, Prefix: compositePrefix.String(), Limit: 1, Namespace: string(state.NamespaceAccounts), Revision: 1, Snapshot: "snapshot", After: compositePrefix.String() + "value", ExpiresAt: expires})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.List(ctx, compositePrefix, state.PageRequest{Limit: 1, Cursor: nonCompositeOnComposite}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("non-composite cursor on cross-domain route error = %v", err)
	}

	nonce := bytes.Repeat([]byte{2}, engine.cursorAEAD.NonceSize())
	sealed := engine.cursorAEAD.Seal(append([]byte(nil), nonce...), nonce, []byte("bad"), []byte("endlessfs-state-cursor-v4"))
	if _, err := engine.decodeStateListCursor(base64.RawURLEncoding.EncodeToString(sealed)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid cursor body error = %v", err)
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := engine.decodeStateListCursor(base64.RawURLEncoding.EncodeToString(sealed)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("tampered cursor error = %v", err)
	}

	if _, err := parseExistingStateKey("accounts/%"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid stored key error = %v", err)
	}
	if _, err := parseExistingStateKey("unknown"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("unknown stored namespace error = %v", err)
	}
	if _, err := engine.List(ctx, exactPrefix, state.PageRequest{Limit: 1001}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid page limit error = %v", err)
	}
	if _, err := engine.CompareAndSwap(ctx, state.MustKey(state.NamespaceAccounts, owner), "", []byte("value")); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty CAS version error = %v", err)
	}
	if err := engine.Delete(ctx, state.MustKey(state.NamespaceAccounts, owner), ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty delete version error = %v", err)
	}

	engine.ids = domain.NewIDGenerator(bytes.NewReader(nil))
	if _, _, err := engine.newStateDomainMutation(state.MustKey(state.NamespaceAccounts, owner), "", []byte("value"), false); err == nil {
		t.Fatal("state mutation succeeded without entropy")
	}
	if _, err := engine.encodeStateListCursor(stateListCursor{SchemaVersion: 4}); err == nil {
		t.Fatal("state cursor succeeded without entropy")
	}
}

func TestSchema008StatePublicBoundaryProviderAndCursorFailures(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	engine := openNamespaceTestEngine(t, backend)
	owner := "WVhXWVhXWVhXWVhXWVhXWQ"
	prefix := state.MustPrefix(state.NamespacePreferences, owner)
	firstKey := state.MustKey(state.NamespacePreferences, owner, "first")
	secondKey := state.MustKey(state.NamespacePreferences, owner, "second")

	for name, call := range map[string]func() error{
		"get-invalid-key":     func() error { _, err := engine.Get(ctx, state.Key{}); return err },
		"list-invalid-prefix": func() error { _, err := engine.List(ctx, state.Prefix{}, state.PageRequest{}); return err },
		"create-invalid-key":  func() error { _, err := engine.Create(ctx, state.Key{}, []byte("value")); return err },
		"create-oversized":    func() error { _, err := engine.Create(ctx, firstKey, make([]byte, state.MaxRecordBytes+1)); return err },
		"cas-invalid-key": func() error {
			_, err := engine.CompareAndSwap(ctx, state.Key{}, "version", []byte("value"))
			return err
		},
		"cas-oversized": func() error {
			_, err := engine.CompareAndSwap(ctx, firstKey, "version", make([]byte, state.MaxRecordBytes+1))
			return err
		},
		"delete-invalid-key": func() error { return engine.Delete(ctx, state.Key{}, "version") },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	firstVersion, err := engine.Create(ctx, firstKey, []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Create(ctx, secondKey, []byte("second")); err != nil {
		t.Fatal(err)
	}
	if page, err := engine.List(ctx, prefix, state.PageRequest{}); err != nil || len(page.Items) != 2 {
		t.Fatalf("default state page = %+v, %v", page, err)
	}
	page, err := engine.List(ctx, prefix, state.PageRequest{Limit: 1})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("state cursor page = %+v, %v", page, err)
	}
	cursor, err := engine.decodeStateListCursor(page.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	cursor.Revision++
	wrongRevision, err := engine.encodeStateListCursor(cursor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.List(ctx, prefix, state.PageRequest{Limit: 1, Cursor: wrongRevision}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("changed state snapshot revision error = %v", err)
	}

	// A persisted logical key must still pass the public state-key grammar; a
	// canonical tree alone cannot make malformed key material authoritative.
	reference, err := stateDomainReferenceForKey(firstKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.stateDomainStore().mutate(ctx, reference, consistencyDomainMutation{ID: "malformed-state-key", Changes: []consistencyDomainChange{{Key: prefix.String() + "%", Require: domainValueAbsent, Value: []byte("bad")}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.List(ctx, prefix, state.PageRequest{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("malformed persisted state key error = %v", err)
	}
	if _, err := engine.stateDomainStore().mutate(ctx, reference, consistencyDomainMutation{ID: "remove-malformed-state-key", Changes: []consistencyDomainChange{{Key: prefix.String() + "%", Require: domainValueAny, Delete: true}}}); err != nil {
		t.Fatal(err)
	}

	failure := domain.NewError(domain.ErrorUnavailable, "state provider unavailable")
	engine.backend = &hookedBackend{Backend: backend, get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
		return objectstore.Object{}, failure
	}}
	for name, call := range map[string]func() error{
		"get":  func() error { _, err := engine.Get(ctx, firstKey); return err },
		"list": func() error { _, err := engine.List(ctx, prefix, state.PageRequest{}); return err },
		"create": func() error {
			_, err := engine.Create(ctx, state.MustKey(state.NamespacePreferences, owner, "third"), []byte("third"))
			return err
		},
		"cas": func() error {
			_, err := engine.CompareAndSwap(ctx, firstKey, firstVersion, []byte("changed"))
			return err
		},
		"delete": func() error { return engine.Delete(ctx, firstKey, firstVersion) },
	} {
		t.Run("provider-"+name, func(t *testing.T) {
			if err := call(); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	engine.backend = backend
	engine.ids = domain.NewIDGenerator(bytes.NewReader(nil))
	if err := engine.Delete(ctx, firstKey, firstVersion); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("delete without mutation entropy error = %v", err)
	}
	if _, err := engine.List(ctx, prefix, state.PageRequest{Limit: 1}); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("cursor without entropy error = %v", err)
	}
}
