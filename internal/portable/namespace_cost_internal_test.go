package portable

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
)

type namespaceCostBackend struct {
	*objectmemory.Backend

	mu      sync.Mutex
	gets    int
	puts    int
	copies  int
	deletes int
}

func namespaceCostRandom(seed byte, size int) []byte {
	value := make([]byte, size)
	state := uint64(seed) + 0x9e3779b97f4a7c15
	for index := range value {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		value[index] = byte(state >> 29)
	}
	return value
}

func (backend *namespaceCostBackend) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	backend.mu.Lock()
	backend.gets++
	backend.mu.Unlock()
	return backend.Backend.Get(ctx, key)
}

func (backend *namespaceCostBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	backend.mu.Lock()
	backend.puts++
	backend.mu.Unlock()
	return backend.Backend.Put(ctx, key, body, condition)
}

func (backend *namespaceCostBackend) Copy(ctx context.Context, source, destination objectstore.Key, condition objectstore.CopyCondition) (objectstore.CopyResult, error) {
	if strings.Contains(destination.String(), "/blobs/") {
		backend.mu.Lock()
		backend.copies++
		backend.mu.Unlock()
	}
	return backend.Backend.Copy(ctx, source, destination, condition)
}

func (backend *namespaceCostBackend) Delete(ctx context.Context, key objectstore.Key, condition objectstore.DeleteCondition) error {
	backend.mu.Lock()
	backend.deletes++
	backend.mu.Unlock()
	return backend.Backend.Delete(ctx, key, condition)
}

func (backend *namespaceCostBackend) resetCosts() {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.gets, backend.puts, backend.copies, backend.deletes = 0, 0, 0, 0
}

func (backend *namespaceCostBackend) costs() (gets, puts, copies, deletes int) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.gets, backend.puts, backend.copies, backend.deletes
}

func TestNamespaceFolderMoveCostDoesNotScaleWithDescendants(t *testing.T) {
	measure := func(t *testing.T, descendants int) (int, int, int, int) {
		t.Helper()
		ctx := context.Background()
		backend := &namespaceCostBackend{Backend: objectmemory.New()}
		server := httptest.NewServer(backend.Backend)
		t.Cleanup(server.Close)
		clock := domain.NewFixedClock(time.Date(2048, 1, 2, 3, 4, 5, 0, time.UTC))
		if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(namespaceCostRandom(0x61, 1<<20)))); err != nil {
			t.Fatal(err)
		}
		engine := openInternalTestEngine(t, backend, clock, strings.NewReader(string(namespaceCostRandom(byte(descendants), 1<<20))))
		user, _ := domain.ParseUserID("Y29zdC1tb3ZlLXVzZXItMDAwMA")
		live, _ := domain.NewScope(user, domain.AreaLive)
		trash, _ := domain.NewScope(user, domain.AreaTrash)
		if _, err := engine.Files().CreateDirectory(ctx, live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/tree")}); err != nil {
			t.Fatal(err)
		}
		for index := range descendants {
			path := domain.MustParseUserPath(fmt.Sprintf("/tree/file-%06d.bin", index))
			body := []byte{byte(index), byte(index >> 8)}
			capability, err := engine.Files().CreateUpload(ctx, live, domain.CreateUploadRequest{Path: path, Size: int64(len(body)), MediaType: "application/octet-stream"})
			if err != nil {
				t.Fatal(err)
			}
			request, _ := http.NewRequestWithContext(ctx, capability.Method, capability.URL, bytes.NewReader(body))
			for name, value := range capability.Headers {
				request.Header.Set(name, value)
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				t.Fatalf("upload status = %d", response.StatusCode)
			}
			if _, err := engine.Files().CompleteUpload(ctx, live, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: int64(len(body)), MediaType: "application/octet-stream"}); err != nil {
				t.Fatal(err)
			}
		}

		backend.resetCosts()
		operation, err := engine.Files().Move(ctx, live, trash, domain.MoveRequest{
			Source: domain.MustParseUserPath("/tree"), Destination: domain.MustParseUserPath("/tree"),
			IdempotencyKey: fmt.Sprintf("move-%d", descendants),
		})
		if err != nil || operation.State != domain.OperationSucceeded {
			t.Fatalf("Move() = %+v, %v", operation, err)
		}
		trashGets, trashPuts, copies, _ := backend.costs()
		if copies != 0 {
			t.Fatalf("folder move copied %d file objects", copies)
		}
		backend.resetCosts()
		operation, err = engine.Files().Move(ctx, trash, live, domain.MoveRequest{
			Source: domain.MustParseUserPath("/tree"), Destination: domain.MustParseUserPath("/tree"),
			IdempotencyKey: fmt.Sprintf("restore-%d", descendants),
		})
		if err != nil || operation.State != domain.OperationSucceeded {
			t.Fatalf("restore Move() = %+v, %v", operation, err)
		}
		restoreGets, restorePuts, copies, _ := backend.costs()
		if copies != 0 {
			t.Fatalf("folder restore copied %d file objects", copies)
		}
		root, err := engine.Files().Stat(ctx, live, domain.MustParseUserPath("/tree"))
		if err != nil || root.FileCount != int64(descendants) || root.Size != int64(descendants*2) {
			t.Fatalf("restored tree aggregate = %+v, %v; want %d files/%d bytes", root, err, descendants, descendants*2)
		}
		return trashGets, trashPuts, restoreGets, restorePuts
	}

	smallTrashGets, smallTrashPuts, smallRestoreGets, smallRestorePuts := measure(t, 1)
	largeTrashGets, largeTrashPuts, largeRestoreGets, largeRestorePuts := measure(t, 128)
	if largeTrashGets > smallTrashGets+4 || largeTrashPuts > smallTrashPuts+4 || largeRestoreGets > smallRestoreGets+4 || largeRestorePuts > smallRestorePuts+4 {
		t.Fatalf("folder namespace cost scaled with descendants: trash 1=%d/%d 128=%d/%d, restore 1=%d/%d 128=%d/%d gets/puts", smallTrashGets, smallTrashPuts, largeTrashGets, largeTrashPuts, smallRestoreGets, smallRestorePuts, largeRestoreGets, largeRestorePuts)
	}
}

func TestLogicalCopyAndUploadPublicationUseNoFileBackendCopy(t *testing.T) {
	ctx := context.Background()
	stateBackend := objectmemory.New()
	fileBackend := &namespaceCostBackend{Backend: objectmemory.New()}
	server := httptest.NewServer(fileBackend.Backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2048, 2, 3, 4, 5, 6, 0, time.UTC))
	if err := fileBackend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(namespaceCostRandom(0x71, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	engine, err := Open(ctx, Options{
		Backend: stateBackend, FileBackend: fileBackend, Clock: clock,
		IDs:      domain.NewIDGenerator(bytes.NewReader(namespaceCostRandom(0x72, 1<<20))),
		Writer:   WriterConfiguration{WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{"key"}},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x73}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	user, _ := domain.ParseUserID("Y29zdC1jb3B5LXVzZXItMDAwMA")
	live, _ := domain.NewScope(user, domain.AreaLive)
	content := []byte("one immutable provider object")
	capability, err := engine.Files().CreateUpload(ctx, live, domain.CreateUploadRequest{
		Path: domain.MustParseUserPath("/source.bin"), Size: int64(len(content)), MediaType: "application/octet-stream",
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequestWithContext(ctx, capability.Method, capability.URL, bytes.NewReader(content))
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("upload status = %d", response.StatusCode)
	}
	if _, err := engine.Files().CompleteUpload(ctx, live, domain.CompleteUploadRequest{
		UploadID: capability.UploadID, Path: domain.MustParseUserPath("/source.bin"), Size: int64(len(content)), MediaType: "application/octet-stream",
	}); err != nil {
		t.Fatal(err)
	}
	_, _, publicationCopies, _ := fileBackend.costs()
	if publicationCopies != 0 {
		t.Fatalf("upload publication performed %d file-provider copies; want direct final-blob publication", publicationCopies)
	}

	fileBackend.resetCosts()
	operation, err := engine.Files().Copy(ctx, live, live, domain.CopyRequest{
		Source: domain.MustParseUserPath("/source.bin"), Destination: domain.MustParseUserPath("/copy.bin"), IdempotencyKey: "logical-copy",
	})
	if err != nil || operation.State != domain.OperationSucceeded {
		t.Fatalf("Copy() = %+v, %v", operation, err)
	}
	_, _, logicalCopies, _ := fileBackend.costs()
	if logicalCopies != 0 {
		t.Fatalf("logical copy performed %d file-provider copies; want shared immutable BlobID", logicalCopies)
	}
	store := newNamespaceStore(engine)
	source, err := store.resolveEntry(ctx, live, domain.MustParseUserPath("/source.bin"))
	if err != nil {
		t.Fatal(err)
	}
	copy, err := store.resolveEntry(ctx, live, domain.MustParseUserPath("/copy.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if source.Entry.BlobID == "" || copy.Entry.BlobID != source.Entry.BlobID {
		t.Fatalf("logical copy blob IDs = source %q, copy %q", source.Entry.BlobID, copy.Entry.BlobID)
	}
	groups, err := engine.Files().ListDuplicateGroups(ctx, user, domain.DuplicateGroupRequest{Kind: domain.DuplicateFile, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups.Groups) != 0 {
		t.Fatalf("logical reflink was reported as reclaimable physical duplication: %+v", groups.Groups)
	}
}
