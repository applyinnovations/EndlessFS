package portable

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
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

func TestPreparingFileOperationSealsBoundedRunsBeforeVisibility(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2047, 1, 2, 3, 4, 5, 0, time.UTC))
	engine := &Engine{backend: backend, clock: clock, leaseTTL: time.Minute}
	store := &FileStore{engine: engine}
	operation := storageformat.FileOperation{
		SchemaVersion: 2, OperationID: "preparation-seal", UserID: "WVhXWVhXWVhXWVhXWVhXWQ", Kind: operationMove,
		State: storageformat.FileOperationPreparing, Attempt: 1, Fence: 1, ReplicaAttemptID: "attempt-one",
		ExpiresAt: clock.Now().Add(time.Minute), StartedAt: clock.Now(), UpdatedAt: clock.Now(),
		Preparation: &storageformat.FileOperationPreparation{SchemaVersion: 1, RunSetID: "stable-seal-set", Phase: "seal"},
	}
	collector, err := newOperationPreparationRunCollector(store, operation, 0)
	if err != nil {
		t.Fatal(err)
	}
	target := objectstore.MustKey("endlessfs/v1/test/prepared-root")
	root := storageformat.FileOperationRoot{Key: target.String(), PendingBody: []byte("pending"), FinalBody: []byte("final")}
	if err := collector.Add(ctx, storageformat.FileOperationPreparationItem{SortKey: "root\x00" + root.Key, Kind: storageformat.FileOperationPreparationRoot, Root: &root}); err != nil {
		t.Fatal(err)
	}
	operation.Preparation.RunCount, err = collector.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key := storageformat.OperationKey(operation.UserID, operation.OperationID)
	body, err := storageformat.EncodeEnvelope(fileOperationSchema, key, 1, operation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if err := store.executeFileOperation(ctx, key); err != nil {
		t.Fatal(err)
	}
	userID, parseErr := domain.ParseUserID(operation.UserID)
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	stored, err := store.readFileOperation(ctx, userID, operation.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != storageformat.FileOperationSucceeded || stored.Preparation != nil || stored.StepPageCount != 1 {
		t.Fatalf("sealed operation = state:%q preparation:%+v pages:%d", stored.State, stored.Preparation, stored.StepPageCount)
	}
	object, err := backend.Get(ctx, target)
	if err != nil || strings.Compare(string(object.Body), "final") != 0 {
		t.Fatalf("prepared root = %q, %v; want final", object.Body, err)
	}
}

func TestCloneTreeStreamEmitsWithoutSubtreeCollections(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2047, 3, 4, 5, 6, 7, 0, time.UTC))
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("stream-clone-entropy", 1<<16)))
	user, _ := domain.ParseUserID("WVhXWVhXWVhXWVhXWVhXWQ")
	from, _ := domain.NewScope(user, domain.AreaLive)
	to, _ := domain.NewScope(user, domain.AreaTrash)
	entries := make([]storageformat.DirectoryEntry, maxDirectoryIndexItems*4+1)
	for index := range entries {
		name := fmt.Sprintf("file-%05d.bin", index)
		entries[index] = withCurrentTestFingerprint(storageformat.DirectoryEntry{
			Name: name, NameDigest: storageformat.NameDigest(name), Kind: domain.EntryFile,
			BlobID: fmt.Sprintf("blob-%05d", index), Size: int64(index + 1), MediaType: "application/octet-stream", ModifiedAt: clock.Now(),
		})
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].NameDigest < entries[right].NameDigest })
	prepared, err := engine.Files().prepareDirectory(ctx, from, storageformat.RootDirectoryID, entries, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, prerequisite := range prepared.prerequisites {
		if _, err := backend.Put(ctx, objectstore.MustKey(prerequisite.Key), prerequisite.Body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}
	rootKey := storageformat.DirectoryRootKey(user.String(), "live", storageformat.RootDirectoryID)
	if _, err := backend.Put(ctx, rootKey, prepared.rootBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	source := storageformat.DirectoryEntry{
		Name: "source", NameDigest: storageformat.NameDigest("source"), Kind: domain.EntryDirectory,
		DirectoryID: storageformat.RootDirectoryID, Size: prepared.recursiveBytes, FileCount: prepared.recursiveFileCount,
		ContentDigest: prepared.contentDigest, ModifiedAt: clock.Now(),
	}
	source.LogicalVersion, err = directoryEntryVersion(source)
	if err != nil {
		t.Fatal(err)
	}
	objects, copies, occurrences := 0, 0, 0
	result, err := engine.Files().cloneTreeStream(ctx, "stable-clone-operation", from, to, source, true,
		func(storageformat.MutationObject) error { objects++; return nil },
		func(storageformat.MutationCopy) error { copies++; return nil },
		func(relativeCatalogEntry) error { occurrences++; return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.entry.DirectoryID == source.DirectoryID || result.entry.Size != source.Size || result.entry.FileCount != source.FileCount || result.entry.ContentDigest != source.ContentDigest {
		t.Fatalf("stream clone result = %+v; source = %+v", result.entry, source)
	}
	if copies != len(entries) || occurrences != len(entries)+1 || objects <= len(entries)/maxDirectoryIndexItems {
		t.Fatalf("stream clone emissions = objects:%d copies:%d occurrences:%d", objects, copies, occurrences)
	}
}
