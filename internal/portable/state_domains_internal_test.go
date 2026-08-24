package portable

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestStateRoutingUsesOwnerAndShardedCapabilityDomains(t *testing.T) {
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
		{state.MustKey(state.NamespaceCredentials, "user-index", ownerB), storageformat.DomainOwnerControl, "owner:" + ownerB},
		{state.MustKey(state.NamespaceBootstrap, "state"), storageformat.DomainAdmin, "administration"},
		{state.MustKey(state.NamespaceRoles, "admins"), storageformat.DomainAdmin, "administration"},
	}
	for _, test := range cases {
		reference, err := stateDomainReferenceForKey(test.key)
		if err != nil || reference.Kind != test.kind || reference.ID != test.id {
			t.Errorf("route %q = %+v, %v; want %s/%s", test.key.String(), reference, err, test.kind, test.id)
		}
	}

	first, err := stateDomainReferenceForKey(state.MustKey(state.NamespaceSessions, "aaa-token"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := stateDomainReferenceForKey(state.MustKey(state.NamespaceSessions, "bbb-token"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Kind != storageformat.DomainCapability || second.Kind != storageformat.DomainCapability || first.ID == second.ID {
		t.Fatalf("session shards = %+v and %+v", first, second)
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
