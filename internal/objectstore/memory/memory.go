// Package memory implements a strongly consistent in-memory object backend.
package memory

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

type record struct {
	body         []byte
	version      objectstore.NativeVersion
	size         int64
	materialized bool
}

type snapshot struct {
	prefix  string
	limit   int
	objects []objectstore.ObjectInfo
	index   int
}

type Backend struct {
	mu        sync.Mutex
	records   map[string]record
	snapshots map[string]*snapshot
	versions  uint64
	cursors   uint64

	clock          domain.Clock
	ids            *domain.IDGenerator
	dataPlaneURL   string
	uploads        map[string]*uploadSession
	uploadTokens   map[[32]byte]string
	downloads      map[[32]byte]downloadSession
	transferFaults map[string][]string
	uploadBytes    int64
	downloadBytes  int64
}

func New() *Backend {
	return &Backend{
		records: make(map[string]record), snapshots: make(map[string]*snapshot),
		clock: domain.SystemClock{}, ids: domain.SystemIDGenerator(), uploads: make(map[string]*uploadSession),
		uploadTokens: make(map[[32]byte]string), downloads: make(map[[32]byte]downloadSession), transferFaults: make(map[string][]string),
	}
}

func (b *Backend) Head(ctx context.Context, key objectstore.Key) (objectstore.ObjectInfo, error) {
	if err := objectstore.ContextError(ctx); err != nil {
		return objectstore.ObjectInfo{}, err
	}
	if !key.Valid() {
		return objectstore.ObjectInfo{}, domain.NewError(domain.ErrorInvalid, "invalid object key")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	record, found := b.records[key.String()]
	if !found {
		return objectstore.ObjectInfo{}, domain.NewError(domain.ErrorNotFound, "object not found")
	}
	return objectstore.ObjectInfo{Key: key, Version: record.version, Size: record.size}, nil
}

func (b *Backend) Verify(ctx context.Context, key objectstore.Key, expected objectstore.ExpectedIntegrity) (objectstore.ObjectInfo, error) {
	if err := objectstore.ContextError(ctx); err != nil {
		return objectstore.ObjectInfo{}, err
	}
	if !key.Valid() {
		return objectstore.ObjectInfo{}, domain.NewError(domain.ErrorInvalid, "invalid object key")
	}
	if err := expected.Validate(); err != nil {
		return objectstore.ObjectInfo{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	record, found := b.records[key.String()]
	if !found {
		return objectstore.ObjectInfo{}, domain.NewError(domain.ErrorNotFound, "object not found")
	}
	if !record.materialized {
		return objectstore.ObjectInfo{}, domain.NewError(domain.ErrorUnavailable, "object integrity is not materialized")
	}
	actual := objectstore.IntegrityFor(record.body)
	if record.size != expected.Size || actual.Checksum != expected.Checksum {
		return objectstore.ObjectInfo{}, domain.NewError(domain.ErrorPreconditionFailed, "object integrity does not match")
	}
	return objectstore.ObjectInfo{Key: key, Version: record.version, Size: record.size}, nil
}

func (b *Backend) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	reader, err := b.Open(ctx, key)
	if err != nil {
		return objectstore.Object{}, err
	}
	body, readErr := io.ReadAll(reader.Body)
	closeErr := reader.Body.Close()
	if readErr != nil {
		return objectstore.Object{}, readErr
	}
	if closeErr != nil {
		return objectstore.Object{}, closeErr
	}
	return objectstore.Object{Key: key, Body: body, Version: reader.Version, Size: reader.Size}, nil
}

func (b *Backend) Open(ctx context.Context, key objectstore.Key) (objectstore.ObjectReader, error) {
	if err := objectstore.ContextError(ctx); err != nil {
		return objectstore.ObjectReader{}, err
	}
	if !key.Valid() {
		return objectstore.ObjectReader{}, domain.NewError(domain.ErrorInvalid, "invalid object key")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	record, found := b.records[key.String()]
	if !found {
		return objectstore.ObjectReader{}, domain.NewError(domain.ErrorNotFound, "object not found")
	}
	body := append([]byte(nil), record.body...)
	if !record.materialized && record.size > int64(len(body)) {
		return objectstore.ObjectReader{}, domain.NewError(domain.ErrorUnavailable, "logical object body is not materialized")
	}
	return objectstore.ObjectReader{Key: key, Body: io.NopCloser(bytes.NewReader(body)), Version: record.version, Size: record.size}, nil
}

func (b *Backend) List(ctx context.Context, request objectstore.ListRequest) (objectstore.ListPage, error) {
	if err := objectstore.ContextError(ctx); err != nil {
		return objectstore.ListPage{}, err
	}
	if err := objectstore.ValidatePrefix(request.Prefix); err != nil {
		return objectstore.ListPage{}, err
	}
	limit := request.Limit
	if limit == 0 {
		limit = 1000
	}
	if limit < 1 || limit > 1000 {
		return objectstore.ListPage{}, domain.NewError(domain.ErrorInvalid, "invalid object list limit")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if request.Cursor != "" {
		value, found := b.snapshots[request.Cursor]
		if !found || value.prefix != request.Prefix || value.limit != limit {
			return objectstore.ListPage{}, domain.NewError(domain.ErrorInvalid, "invalid object list cursor")
		}
		return b.page(request.Cursor, value), nil
	}
	keys := make([]string, 0)
	for key := range b.records {
		if strings.HasPrefix(key, request.Prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	objects := make([]objectstore.ObjectInfo, 0, len(keys))
	for _, value := range keys {
		record := b.records[value]
		objects = append(objects, objectstore.ObjectInfo{Key: objectstore.MustKey(value), Version: record.version, Size: record.size})
	}
	if len(objects) <= limit {
		return objectstore.ListPage{Objects: objects}, nil
	}
	b.cursors++
	cursor := string(objectstore.VersionString("c", b.cursors))
	value := &snapshot{prefix: request.Prefix, limit: limit, objects: objects}
	b.snapshots[cursor] = value
	return b.page(cursor, value), nil
}

func (b *Backend) page(cursor string, value *snapshot) objectstore.ListPage {
	end := min(value.index+value.limit, len(value.objects))
	objects := append([]objectstore.ObjectInfo(nil), value.objects[value.index:end]...)
	value.index = end
	if end == len(value.objects) {
		delete(b.snapshots, cursor)
		cursor = ""
	}
	return objectstore.ListPage{Objects: objects, NextCursor: cursor}
}

func (b *Backend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	if err := objectstore.ContextError(ctx); err != nil {
		return "", err
	}
	if !key.Valid() {
		return "", domain.NewError(domain.ErrorInvalid, "invalid object key")
	}
	if err := condition.Validate(); err != nil {
		return "", err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	current, exists := b.records[key.String()]
	switch condition.Mode {
	case objectstore.PutCreateOnly:
		if exists {
			return "", domain.NewError(domain.ErrorConflict, "object already exists")
		}
	case objectstore.PutMatch:
		if !exists {
			return "", domain.NewError(domain.ErrorNotFound, "object not found")
		}
		if current.version != condition.Version {
			return "", domain.NewError(domain.ErrorPreconditionFailed, "stale object version")
		}
	}
	b.versions++
	version := objectstore.VersionString("m", b.versions)
	b.records[key.String()] = record{body: append([]byte(nil), body...), version: version, size: int64(len(body)), materialized: true}
	return version, nil
}

func (b *Backend) Delete(ctx context.Context, key objectstore.Key, condition objectstore.DeleteCondition) error {
	if err := objectstore.ContextError(ctx); err != nil {
		return err
	}
	if !key.Valid() || condition.Version == "" {
		return domain.NewError(domain.ErrorInvalid, "invalid object delete")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	current, exists := b.records[key.String()]
	if !exists {
		return domain.NewError(domain.ErrorNotFound, "object not found")
	}
	if current.version != condition.Version {
		return domain.NewError(domain.ErrorPreconditionFailed, "stale object version")
	}
	delete(b.records, key.String())
	return nil
}

func (b *Backend) Copy(ctx context.Context, source, destination objectstore.Key, condition objectstore.CopyCondition) (objectstore.CopyResult, error) {
	if err := objectstore.ContextError(ctx); err != nil {
		return objectstore.CopyResult{}, err
	}
	if !source.Valid() || !destination.Valid() || condition.SourceVersion == "" {
		return objectstore.CopyResult{}, domain.NewError(domain.ErrorInvalid, "invalid object copy")
	}
	if err := condition.Destination.Validate(); err != nil {
		return objectstore.CopyResult{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	sourceRecord, exists := b.records[source.String()]
	if !exists {
		return objectstore.CopyResult{}, domain.NewError(domain.ErrorNotFound, "source object not found")
	}
	if sourceRecord.version != condition.SourceVersion {
		return objectstore.CopyResult{}, domain.NewError(domain.ErrorPreconditionFailed, "stale source object version")
	}
	destinationRecord, destinationExists := b.records[destination.String()]
	switch condition.Destination.Mode {
	case objectstore.PutCreateOnly:
		if destinationExists {
			return objectstore.CopyResult{}, domain.NewError(domain.ErrorConflict, "destination object already exists")
		}
	case objectstore.PutMatch:
		if !destinationExists {
			return objectstore.CopyResult{}, domain.NewError(domain.ErrorNotFound, "destination object not found")
		}
		if destinationRecord.version != condition.Destination.Version {
			return objectstore.CopyResult{}, domain.NewError(domain.ErrorPreconditionFailed, "stale destination object version")
		}
	}
	b.versions++
	version := objectstore.VersionString("m", b.versions)
	body := append([]byte(nil), sourceRecord.body...)
	b.records[destination.String()] = record{body: body, version: version, size: sourceRecord.size, materialized: sourceRecord.materialized}
	return objectstore.CopyResult{Version: version, Size: sourceRecord.size}, nil
}

// Export returns a copy of all key/body pairs for portability tests.
func (b *Backend) Export() map[string][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := make(map[string][]byte, len(b.records))
	for key, value := range b.records {
		result[key] = append([]byte(nil), value.body...)
	}
	return result
}

// Import creates key/body pairs with new native versions.
func (b *Backend) Import(objects map[string][]byte) error {
	for key, body := range objects {
		parsed, err := objectstore.ParseKey(key)
		if err != nil {
			return err
		}
		if _, err := b.Put(context.Background(), parsed, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			return err
		}
	}
	return nil
}
