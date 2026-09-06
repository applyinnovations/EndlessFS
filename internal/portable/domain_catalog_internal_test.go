package portable

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestDomainCatalogRegistrationIsPaidOnceAndOrdinaryMutationDoesNotReadCatalog(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
	store := newConsistencyDomainStore(backend, nil)
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner-catalog-cost"}
	if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: "first", Changes: []consistencyDomainChange{{Key: "a", Require: domainValueAbsent, Value: []byte("a")}}}); err != nil {
		t.Fatal(err)
	}
	ledger.Reset()
	if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: "second", Changes: []consistencyDomainChange{{Key: "b", Require: domainValueAbsent, Value: []byte("b")}}}); err != nil {
		t.Fatal(err)
	}
	for _, request := range ledger.Events() {
		if strings.Contains(request.Target, "/domains/catalog/") {
			t.Fatalf("warm mutation paid catalog request: %+v", request)
		}
	}
}

func TestFrozenCatalogRejectsNewDomainWithoutPublishingValues(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	catalog := newDomainCatalog(backend, nil)
	if entries, err := catalog.freezeDomains(ctx, 7); err != nil || len(entries) != 0 {
		t.Fatalf("freeze empty catalog = %+v, %v", entries, err)
	}
	reference := consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "owner-created-too-late"}
	store := newConsistencyDomainStore(backend, nil)
	if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: "late", Changes: []consistencyDomainChange{{Key: "edge", Require: domainValueAbsent, Value: []byte("node")}}}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("late domain mutation error = %v", err)
	}
	snapshot, err := store.loadHead(ctx, reference)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.exists || snapshot.head.Registered || snapshot.head.Revision != 0 || len(snapshot.head.Deltas) != 0 {
		t.Fatalf("late domain published authority: %+v", snapshot.head)
	}
}

func TestCatalogFreezeIncludesAndFreezesEveryRegisteredDomain(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	store := newConsistencyDomainStore(backend, nil)
	references := []consistencyDomainRef{
		{Kind: storageformat.DomainNamespace, ID: "owner-a"},
		{Kind: storageformat.DomainCapability, ID: "invite-a"},
	}
	for index, reference := range references {
		if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: domainTestKey(index), Changes: []consistencyDomainChange{{Key: "value", Require: domainValueAbsent, Value: []byte("value")}}}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := newDomainCatalog(backend, nil).freezeDomains(ctx, 9)
	if err != nil || len(entries) != len(references) {
		t.Fatalf("freeze entries = %+v, %v", entries, err)
	}
	for _, reference := range references {
		snapshot, err := store.loadHead(ctx, reference)
		if err != nil || !snapshot.head.Frozen || snapshot.head.FreezeEpoch != 9 {
			t.Fatalf("frozen %v = %+v, %v", reference, snapshot.head, err)
		}
		if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: "denied", Changes: []consistencyDomainChange{{Key: "other", Require: domainValueAbsent, Value: []byte("other")}}}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("mutation through frozen %v = %v", reference, err)
		}
	}
	if err := newDomainCatalog(backend, nil).unfreeze(ctx, 9); err != nil {
		t.Fatal(err)
	}
	for index, reference := range references {
		if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: "after-" + domainTestKey(index), Changes: []consistencyDomainChange{{Key: "after", Require: domainValueAbsent, Value: []byte("after")}}}); err != nil {
			t.Fatalf("mutation after unfreeze %v: %v", reference, err)
		}
	}
}
