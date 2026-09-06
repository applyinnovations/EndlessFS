package portable

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type runtimeObjectCall struct {
	method string
	key    string
}

func TestSchema008RuntimeMutationsCannotReachRetiredBackendOrObjectBytes(t *testing.T) {
	ctx := context.Background()
	base := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2059, 1, 2, 3, 4, 5, 0, time.UTC))
	engine := openInternalTestEngine(t, base, clock, strings.NewReader(strings.Repeat("runtime-008-boundary", 1<<15)))

	var mu sync.Mutex
	var calls []runtimeObjectCall
	record := func(method string, key objectstore.Key) {
		mu.Lock()
		calls = append(calls, runtimeObjectCall{method: method, key: key.String()})
		mu.Unlock()
	}
	hooks := &hookedBackend{Backend: base}
	hooks.get = func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
		record("get", key)
		return base.Get(ctx, key)
	}
	hooks.head = func(ctx context.Context, key objectstore.Key) (objectstore.ObjectInfo, error) {
		record("head", key)
		return base.Head(ctx, key)
	}
	hooks.put = func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
		record("put", key)
		return base.Put(ctx, key, body, condition)
	}
	hooks.list = func(ctx context.Context, request objectstore.ListRequest) (objectstore.ListPage, error) {
		record("list", objectstore.MustKey(strings.TrimSuffix(request.Prefix, "/")+"/probe"))
		return base.List(ctx, request)
	}
	hooks.open = func(ctx context.Context, key objectstore.Key) (objectstore.ObjectReader, error) {
		record("open", key)
		return base.Open(ctx, key)
	}
	hooks.delete = func(ctx context.Context, key objectstore.Key, condition objectstore.DeleteCondition) error {
		record("delete", key)
		return base.Delete(ctx, key, condition)
	}
	hooks.copy = func(ctx context.Context, source, destination objectstore.Key, condition objectstore.CopyCondition) (objectstore.CopyResult, error) {
		record("copy", destination)
		return base.Copy(ctx, source, destination, condition)
	}
	engine.backend, engine.fileBackend = hooks, hooks

	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	seed := seedNamespaceBatchFiles(t, store, live, 1)[0]
	mu.Lock()
	calls = nil
	mu.Unlock()

	folder, err := engine.Files().CreateDirectory(ctx, live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/folder")})
	if err != nil || folder.Kind != domain.EntryDirectory {
		t.Fatalf("CreateDirectory() = %+v, %v", folder, err)
	}
	move, err := engine.Files().Move(ctx, live, live, domain.MoveRequest{Source: seed.Path, Destination: domain.MustParseUserPath("/folder/file.bin"), ExpectedSource: seed.Version, IdempotencyKey: "boundary-move"})
	if err != nil || move.State != domain.OperationSucceeded {
		t.Fatalf("Move() = %+v, %v", move, err)
	}
	moved, err := engine.Files().Stat(ctx, live, domain.MustParseUserPath("/folder/file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	copyResult, err := engine.Files().Copy(ctx, live, live, domain.CopyRequest{Source: moved.Path, Destination: domain.MustParseUserPath("/copy.bin"), ExpectedSource: moved.Version, IdempotencyKey: "boundary-copy"})
	if err != nil || copyResult.State != domain.OperationSucceeded {
		t.Fatalf("Copy() = %+v, %v", copyResult, err)
	}
	copied, err := engine.Files().Stat(ctx, live, domain.MustParseUserPath("/copy.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if operation, err := engine.Files().MoveToTrash(ctx, live.UserID(), domain.TrashRequest{Path: copied.Path, ExpectedVersion: copied.Version, TrashID: "boundary-trash", IdempotencyKey: "boundary-trash"}); err != nil || operation.State != domain.OperationSucceeded {
		t.Fatalf("MoveToTrash() = %+v, %v", operation, err)
	}
	if operation, err := engine.Files().RestoreFromTrash(ctx, live.UserID(), "boundary-trash", domain.ConflictFail, "boundary-restore"); err != nil || operation.State != domain.OperationSucceeded {
		t.Fatalf("RestoreFromTrash() = %+v, %v", operation, err)
	}
	if operation, err := engine.Files().Delete(ctx, live, domain.DeleteRequest{Path: moved.Path, ExpectedVersion: moved.Version, IdempotencyKey: "boundary-delete"}); err != nil || operation.State != domain.OperationSucceeded {
		t.Fatalf("Delete() = %+v, %v", operation, err)
	}

	legacyPrefixes := []string{
		storageformat.StateRecordsPrefix(), storageformat.StateVersionsPrefix(), storageformat.StateIndexRootPrefix(),
		storageformat.OperationPrefix(), storageformat.FileOperationStepsPrefix(), storageformat.FileOperationPreparationsPrefix(),
		storageformat.OperationStagingPrefix(), storageformat.IdempotencyPrefix(), storageformat.DuplicateRecordsPrefix(),
		storageformat.FilesystemPrefix(), "endlessfs/v1/admissions/",
	}
	mu.Lock()
	observed := append([]runtimeObjectCall(nil), calls...)
	mu.Unlock()
	if len(observed) == 0 {
		t.Fatal("runtime boundary observed no provider calls")
	}
	for _, call := range observed {
		if call.method == "open" || call.method == "copy" || call.method == "delete" || call.method == "list" {
			t.Fatalf("schema-008 metadata mutation used forbidden provider method: %+v", call)
		}
		for _, prefix := range legacyPrefixes {
			if strings.HasPrefix(call.key, prefix) {
				t.Fatalf("schema-008 runtime reached retired authority %q: %+v", prefix, call)
			}
		}
	}
}
