package architecturelab

import (
	"context"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

func TestStorageMapAndTrashPageEconomicsBeforeAndAfter(t *testing.T) {
	ctx := context.Background()
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	current := openCurrentProviderHarness(t, "read-workflow-before")
	if _, err := current.engine.Files().CreateDirectory(ctx, current.live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/project")}); err != nil {
		t.Fatal(err)
	}
	current.upload(t, "/project/file.txt", "payload")
	current.upload(t, "/trash-file.txt", "payload")
	trashed, err := current.service.Trash(ctx, current.user, []domain.UserPath{domain.MustParseUserPath("/trash-file.txt")}, "read-trash-seed-0001")
	if err != nil || len(trashed.Items) != 1 {
		t.Fatalf("Trash()=%+v, %v", trashed, err)
	}
	current.ledger.Reset()
	if _, err := current.service.StorageMap(ctx, current.user, domain.MustParseUserPath("/")); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "before/read/storage-map-one-expanded-directory", model, current.ledger)
	if _, err := current.service.TrashPage(ctx, current.user, 100, ""); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "before/read/trash-page-one", model, current.ledger)

	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
	candidate, err := openHybrid(ctx, backend, Options{DomainID: "read-workflow-after"})
	if err != nil {
		t.Fatal(err)
	}
	engine := candidate.(*hybridEngine)
	for _, mutation := range []Mutation{
		{ID: "project", Kind: MutationCreateDirectory, ToArea: AreaLive, Destination: "/project", NodeID: "project"},
		{ID: "project-file", Kind: MutationCreateFile, ToArea: AreaLive, Destination: "/project/file.txt", NodeID: "project-file", Size: 7, BlobIdentity: "project-blob"},
		{ID: "trash-file", Kind: MutationCreateFile, ToArea: AreaTrash, Destination: "/trash-id", NodeID: "trash-file", Size: 7, BlobIdentity: "trash-blob"},
	} {
		if _, err := engine.Mutate(ctx, mutation); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	ledger.Reset()
	listed, err := engine.ListDirectories(ctx, AreaLive, []string{"/", "/project"}, 100)
	if err != nil || len(listed) != 2 {
		t.Fatalf("ListDirectories()=%+v, %v", listed, err)
	}
	logCurrentEconomics(t, "after/read/storage-map-one-expanded-directory", model, ledger)
	if _, err := engine.List(ctx, AreaTrash, "/", 100); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "after/read/trash-page-one", model, ledger)
}
