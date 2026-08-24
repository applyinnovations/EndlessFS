package architecturelab

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

func TestDomainCatalogMovesCheckpointTaxOffOrdinaryMutations(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
	catalog, err := openDomainCatalog(ctx, backend, "all-users")
	if err != nil {
		t.Fatal(err)
	}
	var selected *embeddedGraphEngine
	for index := 0; index < 128; index++ {
		engine, err := catalog.Register(ctx, fmt.Sprintf("user-%03d", index))
		if err != nil {
			t.Fatalf("register %d: %v", index, err)
		}
		if index == 64 {
			selected = engine
		}
	}
	ledger.Reset()
	if _, err := selected.Mutate(ctx, Mutation{ID: "mkdir", Kind: MutationCreateDirectory, ToArea: AreaLive, Destination: "/project", NodeID: "project"}); err != nil {
		t.Fatal(err)
	}
	for _, event := range ledger.Events() {
		if event.Subsystem == "catalog-head" || event.Subsystem == "catalog-domain-index" || event.Subsystem == "catalog-commit" {
			t.Fatalf("ordinary mutation paid domain-catalog tax: %+v", event)
		}
	}
	checkpoint, err := catalog.Freeze(ctx, "checkpoint-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoint.Domains) != 128 {
		t.Fatalf("checkpoint domains=%d, want 128", len(checkpoint.Domains))
	}
	if _, err := selected.Mutate(ctx, Mutation{ID: "after-freeze", Kind: MutationCreateDirectory, ToArea: AreaLive, Destination: "/denied", NodeID: "denied"}); err == nil {
		t.Fatal("registered domain mutated after checkpoint freeze")
	}
	if _, err := catalog.Register(ctx, "late-user"); err == nil {
		t.Fatal("new domain registered after catalog freeze")
	}
}

func TestDomainCatalogCheckpointContainsCompleteNestedClosure(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	catalog, err := openDomainCatalog(ctx, backend, "nested-closure")
	if err != nil {
		t.Fatal(err)
	}
	engine, err := catalog.Register(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []Mutation{
		{ID: "project", Kind: MutationCreateDirectory, ToArea: AreaLive, Destination: "/project", NodeID: "project"},
		{ID: "nested", Kind: MutationCreateDirectory, ToArea: AreaLive, Destination: "/project/nested", NodeID: "nested"},
		{ID: "file", Kind: MutationCreateFile, ToArea: AreaLive, Destination: "/project/nested/file", NodeID: "file", Size: 7, BlobIdentity: "blob"},
	} {
		if _, err := engine.Mutate(ctx, mutation); err != nil {
			t.Fatal(err)
		}
	}
	checkpoint, err := catalog.CreateCheckpoint(ctx, "nested-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	refs := checkpoint.DomainRefs["owner"]
	if len(refs) <= 3 {
		t.Fatalf("nested checkpoint closure has %d refs, want descendant pages as well as immediate roots: %v", len(refs), refs)
	}
	if len(checkpoint.DomainRefs["__catalog__"]) == 0 {
		t.Fatal("checkpoint omitted the catalog-tree closure")
	}
}

func TestDomainCatalogGarbageCollectionKeepsRetainedClosure(t *testing.T) {
	ctx := context.Background()
	base := objectmemory.New()
	catalog, err := openDomainCatalog(ctx, base, "collectible")
	if err != nil {
		t.Fatal(err)
	}
	engine, err := catalog.Register(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Mutate(ctx, Mutation{ID: "file", Kind: MutationCreateFile, ToArea: AreaLive, Destination: "/file", NodeID: "file", Size: 7, BlobIdentity: "blob"}); err != nil {
		t.Fatal(err)
	}
	garbageBody := []byte(`{"schemaVersion":1,"level":0,"values":[]}`)
	garbageKey := candidateKey("embedded", "owner", "pages/"+digest(garbageBody)+".json")
	if _, err := base.Put(ctx, garbageKey, garbageBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := catalog.CreateCheckpoint(ctx, "retained")
	if err != nil {
		t.Fatal(err)
	}
	result, err := catalog.CollectGarbage(ctx, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted < 1 {
		t.Fatalf("deleted=%d, want the synthetic unreachable page", result.Deleted)
	}
	if _, err := base.Head(ctx, garbageKey); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unreachable page still exists: %v", err)
	}
	for _, refs := range checkpoint.DomainRefs {
		for _, ref := range refs {
			key, err := objectstore.ParseKey(ref)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := base.Head(ctx, key); err != nil {
				t.Fatalf("retained closure object %s was collected: %v", ref, err)
			}
		}
	}
}
