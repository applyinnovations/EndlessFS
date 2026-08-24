package architecturelab

import (
	"fmt"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

func TestPagedBatchTrashPublishesOneAtomicSelection(t *testing.T) {
	ctx := testContext()
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{1, 100, 10_000} {
		t.Run(fmt.Sprintf("items-%d", count), func(t *testing.T) {
			ledger := providerbudget.NewLedger()
			backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
			engine, err := openBatchNamespace(ctx, backend, fmt.Sprintf("batch-%d", count))
			if err != nil {
				t.Fatal(err)
			}
			names := make([]string, count)
			for index := range names {
				names[index] = fmt.Sprintf("file-%05d", index)
			}
			if err := engine.Seed(ctx, names); err != nil {
				t.Fatal(err)
			}
			ledger.Reset()
			if _, err := engine.Trash(ctx, "trash-selection", names); err != nil {
				t.Fatal(err)
			}
			totals, err := model.Estimate(ledger.Events())
			if err != nil {
				t.Fatal(err)
			}
			if totals.Requests >= int64(count)*5 && count > 1 {
				t.Fatalf("batch requests=%d scale per item for %d items", totals.Requests, count)
			}
			if found, err := engine.Exists(ctx, AreaLive, names[count-1]); err != nil || found {
				t.Fatalf("live last item = %t, %v", found, err)
			}
			if found, err := engine.Exists(ctx, AreaTrash, names[count-1]); err != nil || !found {
				t.Fatalf("trash last item = %t, %v", found, err)
			}
			t.Logf("items=%d requests=%d costPicoUSD=%d serialP95=%d criticalP95=%d requestBytes=%d responseBytes=%d", count, totals.Requests, totals.CostPicoUSD, totals.P95Micros, totals.CriticalP95Micros, totals.RequestBytes, totals.ResponseBytes)
		})
	}
}

func TestPagedBatchMutationShapesPublishOneAtomicSelection(t *testing.T) {
	ctx := testContext()
	tests := []struct {
		name        string
		kind        MutationKind
		from        Area
		to          Area
		copy        bool
		destination string
	}{
		{name: "copy", kind: MutationCopy, from: AreaLive, to: AreaLive, copy: true, destination: "copy"},
		{name: "move", kind: MutationMove, from: AreaLive, to: AreaLive, destination: "moved"},
		{name: "trash", kind: "batch-trash", from: AreaLive, to: AreaTrash, destination: "trash"},
		{name: "restore", kind: "batch-restore", from: AreaTrash, to: AreaLive, destination: "restored"},
		{name: "delete", kind: MutationDelete, from: AreaLive},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := objectmemory.New()
			engine, err := openBatchNamespace(ctx, backend, "shape-"+test.name)
			if err != nil {
				t.Fatal(err)
			}
			if err := engine.SeedArea(ctx, test.from, []string{"source"}); err != nil {
				t.Fatal(err)
			}
			changes := []batchSelection{{Source: "source", Destination: test.destination}}
			if _, err := engine.ApplySelection(ctx, "selection", test.kind, test.from, test.to, changes, test.copy); err != nil {
				t.Fatal(err)
			}
			if found, err := engine.Exists(ctx, test.from, "source"); err != nil || found != test.copy {
				t.Fatalf("source found=%t, want %t, err=%v", found, test.copy, err)
			}
			if test.destination != "" {
				if found, err := engine.Exists(ctx, test.to, test.destination); err != nil || !found {
					t.Fatalf("destination found=%t, err=%v", found, err)
				}
			}
		})
	}
}

func TestPagedBatchMutationEconomics(t *testing.T) {
	ctx := testContext()
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		kind MutationKind
		from Area
		to   Area
		copy bool
	}{
		{name: "copy", kind: MutationCopy, from: AreaLive, to: AreaLive, copy: true},
		{name: "move", kind: MutationMove, from: AreaLive, to: AreaLive},
		{name: "trash", kind: "batch-trash", from: AreaLive, to: AreaTrash},
		{name: "restore", kind: "batch-restore", from: AreaTrash, to: AreaLive},
		{name: "delete", kind: MutationDelete, from: AreaLive},
	}
	for _, test := range tests {
		for _, count := range []int{1, 100, 10_000} {
			t.Run(fmt.Sprintf("%s-%d", test.name, count), func(t *testing.T) {
				ledger := providerbudget.NewLedger()
				backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
				engine, err := openBatchNamespace(ctx, backend, fmt.Sprintf("economics-%s-%d", test.name, count))
				if err != nil {
					t.Fatal(err)
				}
				names := make([]string, count)
				selection := make([]batchSelection, len(names))
				for index := range names {
					names[index] = fmt.Sprintf("file-%05d", index)
					destination := ""
					if test.kind != MutationDelete {
						destination = fmt.Sprintf("destination-%05d", index)
					}
					selection[index] = batchSelection{Source: names[index], Destination: destination}
				}
				if err := engine.SeedArea(ctx, test.from, names); err != nil {
					t.Fatal(err)
				}
				ledger.Reset()
				if _, err := engine.ApplySelection(ctx, "selection", test.kind, test.from, test.to, selection, test.copy); err != nil {
					t.Fatal(err)
				}
				totals, err := model.Estimate(ledger.Events())
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("after/batch/%s-%d requests=%d costPicoUSD=%d serialP95=%d criticalP95=%d requestBytes=%d responseBytes=%d", test.name, count, totals.Requests, totals.CostPicoUSD, totals.P95Micros, totals.CriticalP95Micros, totals.RequestBytes, totals.ResponseBytes)
			})
		}
	}
}
