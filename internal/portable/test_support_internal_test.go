package portable

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const (
	testProviderMD5    = "AAAAAAAAAAAAAAAAAAAAAA"
	testProviderCRC32C = "AAAAAA"
)

func withCurrentTestFingerprint(entry storageformat.DirectoryEntry) storageformat.DirectoryEntry {
	entry.MD5 = testProviderMD5
	entry.CRC32C = testProviderCRC32C
	entry.SHA256 = ""
	entry.LogicalVersion, _ = directoryEntryVersion(entry)
	return entry
}

type hookedBackend struct {
	objectstore.Backend
	head   func(context.Context, objectstore.Key) (objectstore.ObjectInfo, error)
	get    func(context.Context, objectstore.Key) (objectstore.Object, error)
	open   func(context.Context, objectstore.Key) (objectstore.ObjectReader, error)
	list   func(context.Context, objectstore.ListRequest) (objectstore.ListPage, error)
	put    func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error)
	delete func(context.Context, objectstore.Key, objectstore.DeleteCondition) error
	copy   func(context.Context, objectstore.Key, objectstore.Key, objectstore.CopyCondition) (objectstore.CopyResult, error)
}

func (backend *hookedBackend) Head(ctx context.Context, key objectstore.Key) (objectstore.ObjectInfo, error) {
	if backend.head != nil {
		return backend.head(ctx, key)
	}
	return backend.Backend.Head(ctx, key)
}

func (backend *hookedBackend) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	if backend.get != nil {
		return backend.get(ctx, key)
	}
	return backend.Backend.Get(ctx, key)
}

func (backend *hookedBackend) Open(ctx context.Context, key objectstore.Key) (objectstore.ObjectReader, error) {
	if backend.open != nil {
		return backend.open(ctx, key)
	}
	return backend.Backend.Open(ctx, key)
}

func (backend *hookedBackend) List(ctx context.Context, request objectstore.ListRequest) (objectstore.ListPage, error) {
	if backend.list != nil {
		return backend.list(ctx, request)
	}
	return backend.Backend.List(ctx, request)
}

func (backend *hookedBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	if backend.put != nil {
		return backend.put(ctx, key, body, condition)
	}
	return backend.Backend.Put(ctx, key, body, condition)
}

func (backend *hookedBackend) Delete(ctx context.Context, key objectstore.Key, condition objectstore.DeleteCondition) error {
	if backend.delete != nil {
		return backend.delete(ctx, key, condition)
	}
	return backend.Backend.Delete(ctx, key, condition)
}

func (backend *hookedBackend) Copy(ctx context.Context, source, destination objectstore.Key, condition objectstore.CopyCondition) (objectstore.CopyResult, error) {
	if backend.copy != nil {
		return backend.copy(ctx, source, destination, condition)
	}
	return backend.Backend.Copy(ctx, source, destination, condition)
}

func openInternalTestEngine(t *testing.T, backend objectstore.Backend, clock *domain.FixedClock, random *strings.Reader) *Engine {
	t.Helper()
	engine, err := Open(context.Background(), Options{
		Backend: backend, Clock: clock, IDs: domain.NewIDGenerator(random),
		Writer:   WriterConfiguration{WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{"key"}},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x44}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}
