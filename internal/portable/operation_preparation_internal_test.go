package portable

import (
	"context"
	"fmt"
	"testing"

	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestOperationPreparationRunsMergeWithBoundedMemoryAndIdempotentReplay(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	store := &FileStore{engine: &Engine{backend: backend}}
	operation := storageformat.FileOperation{UserID: "WVhXWVhXWVhXWVhXWVhXWQ", OperationID: "preparation-operation", Preparation: &storageformat.FileOperationPreparation{SchemaVersion: 1, RunSetID: "stable-run-set"}}

	write := func() uint64 {
		collector, err := newOperationPreparationRunCollector(store, operation, 0)
		if err != nil {
			t.Fatal(err)
		}
		for index := maxOperationPreparationPageItems*operationPreparationMergeFanIn + 17; index >= 0; index-- {
			if err := collector.Add(ctx, storageformat.FileOperationPreparationItem{
				SortKey: fmt.Sprintf("root\x00%08d", index), Kind: storageformat.FileOperationPreparationRoot,
				Root: &storageformat.FileOperationRoot{Key: fmt.Sprintf("endlessfs/v1/test/%08d", index)},
			}); err != nil {
				t.Fatal(err)
			}
		}
		count, err := collector.Close(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if collector.maxBuffered > maxOperationPreparationPageItems {
			t.Fatalf("collector buffered %d items; want at most %d", collector.maxBuffered, maxOperationPreparationPageItems)
		}
		return count
	}

	runCount := write()
	if replayed := write(); replayed != runCount {
		t.Fatalf("replayed run count = %d; want %d", replayed, runCount)
	}
	generation, err := store.mergeOperationPreparationRuns(ctx, operation, 0, runCount)
	if err != nil {
		t.Fatal(err)
	}
	previous := ""
	items := 0
	if err := store.forEachOperationPreparationRunItem(ctx, operation, generation, 0, func(item storageformat.FileOperationPreparationItem) error {
		if item.SortKey <= previous {
			t.Fatalf("merged preparation order %q after %q", item.SortKey, previous)
		}
		previous = item.SortKey
		items++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := maxOperationPreparationPageItems*operationPreparationMergeFanIn + 18
	if items != want {
		t.Fatalf("merged items = %d; want %d", items, want)
	}
	if replayGeneration, err := store.mergeOperationPreparationRuns(ctx, operation, 0, runCount); err != nil || replayGeneration != generation {
		t.Fatalf("replayed merge = generation %d, error %v; want %d", replayGeneration, err, generation)
	}
}
