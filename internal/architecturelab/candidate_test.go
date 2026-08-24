package architecturelab

import (
	"context"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

func TestCandidatesPreserveNamespaceSemanticsAndIdempotency(t *testing.T) {
	ctx := context.Background()
	for _, factory := range CandidateFactories() {
		t.Run(factory.Name, func(t *testing.T) {
			ledger := providerbudget.NewLedger()
			backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
			engine, err := factory.Open(ctx, backend, Options{DomainID: "test-user"})
			if err != nil {
				t.Fatal(err)
			}
			if got := engine.Name(); got != factory.Name {
				t.Fatalf("Name() = %q, want %q", got, factory.Name)
			}
			mutations := []Mutation{
				{ID: "mkdir-projects", Kind: MutationCreateDirectory, ToArea: AreaLive, Destination: "/Projects", NodeID: "projects"},
				{ID: "mkdir-project-a", Kind: MutationCreateDirectory, ToArea: AreaLive, Destination: "/Projects/A", NodeID: "project-a"},
				{ID: "upload-readme", Kind: MutationCreateFile, ToArea: AreaLive, Destination: "/Projects/A/readme.txt", NodeID: "readme", Size: 7, BlobIdentity: "blob-readme"},
				{ID: "upload-scratch", Kind: MutationCreateFile, ToArea: AreaLive, Destination: "/scratch.tmp", NodeID: "scratch", Size: 11, BlobIdentity: "blob-scratch"},
				{ID: "delete-scratch", Kind: MutationDelete, FromArea: AreaLive, Source: "/scratch.tmp"},
				{ID: "move-project", Kind: MutationMove, FromArea: AreaLive, ToArea: AreaLive, Source: "/Projects/A", Destination: "/A"},
				{ID: "trash-project", Kind: MutationMove, FromArea: AreaLive, ToArea: AreaTrash, Source: "/A", Destination: "/trash-a"},
				{ID: "restore-project", Kind: MutationMove, FromArea: AreaTrash, ToArea: AreaLive, Source: "/trash-a", Destination: "/A"},
			}
			for _, mutation := range mutations {
				ledger.Reset()
				outcome, err := engine.Mutate(ctx, mutation)
				if err != nil {
					t.Fatalf("Mutate(%s): %v", mutation.ID, err)
				}
				if outcome.MutationID != mutation.ID || !outcome.Committed {
					t.Fatalf("Mutate(%s) outcome = %+v", mutation.ID, outcome)
				}
				for _, event := range ledger.Events() {
					if event.Operation != string(mutation.Kind) || event.Subsystem == "" {
						t.Fatalf("Mutate(%s) unattributed event = %+v", mutation.ID, event)
					}
				}
			}

			beforeReplay, err := engine.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			ledger.Reset()
			replayed, err := engine.Mutate(ctx, mutations[len(mutations)-1])
			if err != nil || !replayed.Replayed {
				t.Fatalf("idempotent replay = %+v, %v", replayed, err)
			}
			if err := engine.Compact(ctx); err != nil {
				t.Fatalf("Compact(): %v", err)
			}
			afterReplay, err := engine.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if beforeReplay.Revision != afterReplay.Revision {
				t.Fatalf("replay revision changed from %d to %d", beforeReplay.Revision, afterReplay.Revision)
			}
			entry, ok := afterReplay.Lookup(AreaLive, "/A/readme.txt")
			if !ok || entry.Kind != NodeFile || entry.Size != 7 || entry.BlobIdentity != "blob-readme" {
				t.Fatalf("restored entry = %+v, %t", entry, ok)
			}
			if _, ok := afterReplay.Lookup(AreaLive, "/Projects/A"); ok {
				t.Fatal("old project path remained visible")
			}
			if _, ok := afterReplay.Lookup(AreaLive, "/scratch.tmp"); ok {
				t.Fatal("deleted scratch file remained visible")
			}
		})
	}
}

func TestCandidatesFreezeTotallyOrdersWithMutations(t *testing.T) {
	ctx := context.Background()
	for _, factory := range CandidateFactories() {
		t.Run(factory.Name, func(t *testing.T) {
			engine, err := factory.Open(ctx, objectmemory.New(), Options{DomainID: "freeze-user"})
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err := engine.Freeze(ctx, "checkpoint-1")
			if err != nil || checkpoint.Revision == 0 || checkpoint.Digest == "" {
				t.Fatalf("Freeze() = %+v, %v", checkpoint, err)
			}
			if _, err := engine.Mutate(ctx, Mutation{ID: "after-freeze", Kind: MutationCreateDirectory, ToArea: AreaLive, Destination: "/denied", NodeID: "denied"}); err == nil {
				t.Fatal("mutation after freeze unexpectedly succeeded")
			}
		})
	}
}
