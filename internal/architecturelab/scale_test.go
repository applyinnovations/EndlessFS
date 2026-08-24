package architecturelab

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

func TestEmbeddedNamespaceWideDirectorySlopeFollowsTreeHeight(t *testing.T) {
	skipArchitectureScaleUnderRace(t)
	ctx := context.Background()
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	for _, width := range []int{1, maxTreePageItems, maxTreePageItems + 1, maxTreePageItems * maxTreePageItems} {
		t.Run(fmt.Sprintf("width-%d", width), func(t *testing.T) {
			base := objectmemory.New()
			ledger := providerbudget.NewLedger()
			backend := budgettest.Wrap(providerbudget.RoleState, base, ledger)
			candidate, err := openEmbeddedGraph(ctx, backend, Options{DomainID: fmt.Sprintf("wide-%d", width)})
			if err != nil {
				t.Fatal(err)
			}
			engine := candidate.(*embeddedGraphEngine)
			for index := 0; index < width; index++ {
				name := fmt.Sprintf("file-%06d", index)
				if _, err := engine.Mutate(ctx, Mutation{ID: "seed-" + name, Kind: MutationCreateFile, ToArea: AreaLive, Destination: "/" + name, NodeID: name, Size: 1, BlobIdentity: "blob-" + name}); err != nil {
					t.Fatalf("seed %d: %v", index, err)
				}
			}
			head, _, err := engine.loadHead(ctx, "inspect")
			if err != nil {
				t.Fatal(err)
			}
			rootPage, err := engine.tree.readPage(ctx, "inspect", "inspect-tree", head.Live.DirectoryRef)
			if err != nil {
				t.Fatal(err)
			}
			ledger.Reset()
			source := fmt.Sprintf("/file-%06d", width-1)
			if _, err := engine.Mutate(ctx, Mutation{ID: "rename", Kind: MutationMove, FromArea: AreaLive, ToArea: AreaLive, Source: source, Destination: "/renamed"}); err != nil {
				t.Fatal(err)
			}
			totals, err := model.Estimate(ledger.Events())
			if err != nil {
				t.Fatal(err)
			}
			directoryCalls := totals.BySubsystem["embedded-directory-index"].Requests
			// A same-directory rename reads and rewrites only one path through the
			// persistent tree. The assertion is derived from the observed page
			// height, so it protects the logarithmic shape without inventing a
			// product request ceiling.
			if directoryCalls > int64(2*(rootPage.Level+1)) {
				t.Fatalf("directory calls=%d exceed one read/write tree path at level=%d; events=%+v", directoryCalls, rootPage.Level, ledger.Events())
			}
			t.Logf("width=%d level=%d requests=%d directoryRequests=%d requestBytes=%d responseBytes=%d", width, rootPage.Level, totals.Requests, directoryCalls, totals.RequestBytes, totals.ResponseBytes)
		})
	}
}

func TestEmbeddedNamespaceSubtreeMoveDoesNotVisitDescendants(t *testing.T) {
	skipArchitectureScaleUnderRace(t)
	ctx := context.Background()
	for _, descendants := range []int{0, 512} {
		t.Run(fmt.Sprintf("descendants-%d", descendants), func(t *testing.T) {
			ledger := providerbudget.NewLedger()
			backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
			candidate, err := openEmbeddedGraph(ctx, backend, Options{DomainID: fmt.Sprintf("subtree-%d", descendants)})
			if err != nil {
				t.Fatal(err)
			}
			engine := candidate.(*embeddedGraphEngine)
			if _, err := engine.Mutate(ctx, Mutation{ID: "mkdir", Kind: MutationCreateDirectory, ToArea: AreaLive, Destination: "/project", NodeID: "project"}); err != nil {
				t.Fatal(err)
			}
			for index := 0; index < descendants; index++ {
				name := fmt.Sprintf("file-%06d", index)
				if _, err := engine.Mutate(ctx, Mutation{ID: "seed-" + name, Kind: MutationCreateFile, ToArea: AreaLive, Destination: "/project/" + name, NodeID: name, Size: 1, BlobIdentity: "blob-" + name}); err != nil {
					t.Fatal(err)
				}
			}
			head, _, err := engine.loadHead(ctx, "inspect")
			if err != nil {
				t.Fatal(err)
			}
			livePage, err := engine.tree.readPage(ctx, "inspect", "inspect-tree", head.Live.DirectoryRef)
			if err != nil {
				t.Fatal(err)
			}
			trashPage, err := engine.tree.readPage(ctx, "inspect", "inspect-tree", head.Trash.DirectoryRef)
			if err != nil {
				t.Fatal(err)
			}
			ledger.Reset()
			if _, err := engine.Mutate(ctx, Mutation{ID: "move-project", Kind: MutationMove, FromArea: AreaLive, ToArea: AreaTrash, Source: "/project", Destination: "/project"}); err != nil {
				t.Fatal(err)
			}
			directoryCalls := int64(0)
			for _, event := range ledger.Events() {
				if event.Subsystem == "embedded-directory-index" {
					directoryCalls++
				}
			}
			pathBound := int64(2 * (livePage.Level + trashPage.Level + 2))
			if directoryCalls > pathBound {
				t.Fatalf("directory calls=%d exceed source/destination tree paths=%d", directoryCalls, pathBound)
			}
			t.Logf("descendants=%d providerRequests=%d directoryRequests=%d", descendants, len(ledger.Events()), directoryCalls)
		})
	}
}

func TestHybridForegroundPublishesDeltaWithoutRewritingBasePages(t *testing.T) {
	skipArchitectureScaleUnderRace(t)
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
	candidate, err := openHybrid(ctx, backend, Options{DomainID: "hybrid-wide"})
	if err != nil {
		t.Fatal(err)
	}
	engine := candidate.(*hybridEngine)
	for index := 0; index < maxTreePageItems*maxTreePageItems; index++ {
		name := fmt.Sprintf("file-%06d", index)
		if _, err := engine.Mutate(ctx, Mutation{ID: "seed-" + name, Kind: MutationCreateFile, ToArea: AreaLive, Destination: "/" + name, NodeID: name, Size: 1, BlobIdentity: "blob-" + name}); err != nil {
			t.Fatalf("seed %d: %v", index, err)
		}
	}
	if err := engine.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	ledger.Reset()
	if _, err := engine.Mutate(ctx, Mutation{ID: "rename", Kind: MutationMove, FromArea: AreaLive, ToArea: AreaLive, Source: "/file-004095", Destination: "/renamed"}); err != nil {
		t.Fatal(err)
	}
	for _, event := range ledger.Events() {
		if event.Kind == providerbudget.RequestObjectPut && strings.Contains(event.Target, "/pages/") {
			t.Fatalf("foreground hybrid mutation rewrote a compacted base page: %+v", event)
		}
	}
	t.Logf("wide hybrid foreground requests=%d", len(ledger.Events()))
}

func TestPersistentPageFanoutSensitivity(t *testing.T) {
	skipArchitectureScaleUnderRace(t)
	ctx := context.Background()
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	const entries = 4096
	measurements := make([]Measurement, 0, 4)
	for _, fanout := range []int{16, 64, 256, 1024} {
		base := objectmemory.New()
		ledger := providerbudget.NewLedger()
		backend := budgettest.Wrap(providerbudget.RoleState, base, ledger)
		tree := immutableTree{backend: backend, domainID: fmt.Sprintf("fanout-%d", fanout), candidate: "fanout", pageItems: fanout}
		root, err := tree.empty(ctx, "seed", "fanout-seed")
		if err != nil {
			t.Fatal(err)
		}
		for index := 0; index < entries; index++ {
			entry := graphEntry{NodeID: fmt.Sprintf("node-%06d", index), Kind: NodeFile, Size: int64(index + 1), FileCount: 1, BlobIdentity: fmt.Sprintf("blob-%06d", index), ContentVersion: fmt.Sprintf("version-%06d", index)}
			root, _, err = tree.upsert(ctx, "seed", "fanout-seed", root, fmt.Sprintf("entry-%06d", index), entry)
			if err != nil {
				t.Fatalf("fanout=%d seed=%d: %v", fanout, index, err)
			}
		}
		page, err := tree.readPage(ctx, "inspect", "fanout-inspect", root)
		if err != nil {
			t.Fatal(err)
		}
		ledger.Reset()
		replacement := graphEntry{NodeID: "node-004095", Kind: NodeFile, Size: 9999, FileCount: 1, BlobIdentity: "blob-004095", ContentVersion: "replacement"}
		session := newTreeSession(tree)
		body, _ := encode(replacement)
		if _, err := session.apply(ctx, "fanout-update", "fanout-index", root, []treeEdit{{Key: "entry-004095", Value: body, Requirement: treePresent}}); err != nil {
			t.Fatal(err)
		}
		totals, err := model.Estimate(ledger.Events())
		if err != nil {
			t.Fatal(err)
		}
		measurement := MeasurementFromTotals(fmt.Sprintf("fanout-%d", fanout), "page-fanout-update", totals, 0, 0)
		measurements = append(measurements, measurement)
		t.Logf("fanout=%d rootLevel=%d requests=%d costPicoUSD=%d p95=%d requestBytes=%d responseBytes=%d", fanout, page.Level, totals.Requests, totals.CostPicoUSD, totals.CriticalP95Micros, totals.RequestBytes, totals.ResponseBytes)
	}
	frontier := ParetoFrontier(measurements)
	if len(frontier) < 2 {
		t.Fatalf("fanout sensitivity unexpectedly produced a single universal winner: %+v", frontier)
	}
	t.Logf("fanout frontier=%v", candidateNames(frontier))
}

func TestHybridDeltaWindowSensitivity(t *testing.T) {
	skipArchitectureScaleUnderRace(t)
	ctx := context.Background()
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	const operations = 256
	measurements := make([]Measurement, 0, 4)
	for _, window := range []int{1, 8, 32, 128} {
		ledger := providerbudget.NewLedger()
		base := objectmemory.New()
		backend := budgettest.Wrap(providerbudget.RoleState, base, ledger)
		candidate, err := openHybrid(ctx, backend, Options{DomainID: fmt.Sprintf("window-%d", window)})
		if err != nil {
			t.Fatal(err)
		}
		engine := candidate.(*hybridEngine)
		engine.deltaLimit = window
		ledger.Reset()
		for index := 0; index < operations; index++ {
			name := fmt.Sprintf("file-%06d", index)
			if _, err := engine.Mutate(ctx, Mutation{ID: "create-" + name, Kind: MutationCreateFile, ToArea: AreaLive, Destination: "/" + name, NodeID: name, Size: 1, BlobIdentity: "blob-" + name}); err != nil {
				t.Fatalf("window=%d operation=%d: %v", window, index, err)
			}
		}
		totals, err := model.Estimate(ledger.Events())
		if err != nil {
			t.Fatal(err)
		}
		headObject, err := base.Get(ctx, engine.headKey)
		if err != nil {
			t.Fatal(err)
		}
		measurement := MeasurementFromTotals(fmt.Sprintf("window-%d", window), "delta-window-256-mutations", totals, 0, int64(len(headObject.Body)))
		measurements = append(measurements, measurement)
		t.Logf("window=%d requests=%d costPicoUSD=%d p95Serial=%d requestBytes=%d responseBytes=%d finalHeadBytes=%d", window, totals.Requests, totals.CostPicoUSD, totals.P95Micros, totals.RequestBytes, totals.ResponseBytes, len(headObject.Body))
	}
	frontier := ParetoFrontier(measurements)
	if len(frontier) < 2 {
		t.Fatalf("delta-window sensitivity unexpectedly produced a single universal winner: %+v", frontier)
	}
	t.Logf("delta-window frontier=%v", candidateNames(frontier))
}

func skipArchitectureScaleUnderRace(t *testing.T) {
	t.Helper()
	if architectureRaceEnabled {
		t.Skip("deterministic scale curve runs in the ordinary gate; race schedules run in recovery and catalog tests")
	}
}
