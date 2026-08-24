package architecturelab

import (
	"context"
	"fmt"
	"testing"

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
