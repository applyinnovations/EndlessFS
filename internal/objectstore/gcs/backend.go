// Package gcs implements the Google Cloud Storage atomic object transport.
// Filesystem, state, admission, and canonical-format semantics remain in the
// portable engine.
package gcs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strconv"
	"strings"

	"cloud.google.com/go/storage"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

const versionPrefix = "gcs-v1."

// Backend is a thin conditional-object adapter over one GCS bucket.
type Backend struct {
	client   *storage.Client
	bucket   *storage.BucketHandle
	name     string
	owned    bool
	transfer *transferConfiguration
}

// NewWithTransfers binds an injected client and explicit signing/lease
// dependencies. It is used by credential-free protocol tests and by process
// construction that has already selected its workload-identity credentials.
func NewWithTransfers(client *storage.Client, bucket string, options TransferOptions) (*Backend, error) {
	backend, err := New(client, bucket)
	if err != nil {
		return nil, err
	}
	configuration, err := newTransferConfiguration(options)
	if err != nil {
		return nil, err
	}
	backend.transfer = configuration
	return backend, nil
}

// EnableWorkloadIdentityTransfers enables V4 signing through the credentials
// already discovered by the official client. The client library uses IAM
// signBlob when the workload identity has no local private key.
func (b *Backend) EnableWorkloadIdentityTransfers(leaseKey []byte, signingAccount string) error {
	configuration, err := newTransferConfiguration(TransferOptions{LeaseKey: leaseKey, GoogleAccessID: signingAccount})
	if err != nil {
		return err
	}
	b.transfer = configuration
	return nil
}

// Open creates a production client using Application Default Credentials.
// ADC supports attached Google service accounts and workload identity
// federation; endpoint overrides and unauthenticated modes are deliberately
// absent from this runtime configuration surface.
func Open(ctx context.Context, bucket string) (*Backend, error) {
	if err := validateBucket(bucket); err != nil {
		return nil, err
	}
	client, err := storage.NewClient(ctx, storage.WithJSONReads())
	if err != nil {
		return nil, domain.WrapError(domain.ErrorUnavailable, "GCS client initialization failed", err)
	}
	backend, err := New(client, bucket)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	backend.owned = true
	return backend, nil
}

// New binds an injected client to a bucket. Tests use this boundary with an
// in-process protocol server and no credentials.
func New(client *storage.Client, bucket string) (*Backend, error) {
	if client == nil {
		return nil, domain.NewError(domain.ErrorInvalid, "GCS client is required")
	}
	if err := validateBucket(bucket); err != nil {
		return nil, err
	}
	return &Backend{client: client, bucket: client.Bucket(bucket), name: bucket}, nil
}

func validateBucket(bucket string) error {
	if len(bucket) < 3 || len(bucket) > 222 || strings.ContainsAny(bucket, "/\\\x00\r\n") {
		return domain.NewError(domain.ErrorInvalid, "invalid GCS bucket")
	}
	return nil
}

// Close releases a client created by Open. Injected clients remain owned by
// their caller.
func (b *Backend) Close() error {
	if b == nil || !b.owned {
		return nil
	}
	b.owned = false
	return b.client.Close()
}

func (b *Backend) Head(ctx context.Context, key objectstore.Key) (objectstore.ObjectInfo, error) {
	if err := objectstore.ContextError(ctx); err != nil {
		return objectstore.ObjectInfo{}, err
	}
	if !key.Valid() {
		return objectstore.ObjectInfo{}, domain.NewError(domain.ErrorInvalid, "invalid object key")
	}
	attrs, err := b.bucket.Object(key.String()).Attrs(ctx)
	if err != nil {
		return objectstore.ObjectInfo{}, classify("GCS object metadata read failed", err)
	}
	if attrs.Generation <= 0 || attrs.Size < 0 {
		return objectstore.ObjectInfo{}, domain.NewError(domain.ErrorInternal, "GCS returned invalid object metadata")
	}
	return objectstore.ObjectInfo{Key: key, Version: encodeVersion(attrs.Generation), Size: attrs.Size}, nil
}

func (b *Backend) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	if err := objectstore.ContextError(ctx); err != nil {
		return objectstore.Object{}, err
	}
	if !key.Valid() {
		return objectstore.Object{}, domain.NewError(domain.ErrorInvalid, "invalid object key")
	}
	info, err := b.Head(ctx, key)
	if err != nil {
		return objectstore.Object{}, err
	}
	generation, err := decodeVersion(info.Version)
	if err != nil {
		return objectstore.Object{}, err
	}
	reader, err := b.bucket.Object(key.String()).Generation(generation).NewReader(ctx)
	if err != nil {
		return objectstore.Object{}, classify("GCS object read failed", err)
	}
	body, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		return objectstore.Object{}, classify("GCS object body read failed", readErr)
	}
	if closeErr != nil {
		return objectstore.Object{}, classify("GCS object body close failed", closeErr)
	}
	if int64(len(body)) != info.Size {
		return objectstore.Object{}, domain.NewError(domain.ErrorInternal, "GCS object size mismatch")
	}
	return objectstore.Object{Key: key, Body: body, Version: info.Version, Size: info.Size}, nil
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
	values := make([]*storage.ObjectAttrs, 0, limit)
	pager := iterator.NewPager(b.bucket.Objects(ctx, &storage.Query{Prefix: request.Prefix}), limit, request.Cursor)
	next, err := pager.NextPage(&values)
	if err != nil {
		return objectstore.ListPage{}, classify("GCS object list failed", err)
	}
	page := objectstore.ListPage{Objects: make([]objectstore.ObjectInfo, 0, len(values)), NextCursor: next}
	for _, attrs := range values {
		key, parseErr := objectstore.ParseKey(attrs.Name)
		if parseErr != nil || attrs.Generation <= 0 || attrs.Size < 0 {
			return objectstore.ListPage{}, domain.NewError(domain.ErrorInternal, "GCS returned invalid object listing")
		}
		page.Objects = append(page.Objects, objectstore.ObjectInfo{Key: key, Version: encodeVersion(attrs.Generation), Size: attrs.Size})
	}
	return page, nil
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
	handle, err := b.conditionedObject(key, condition)
	if err != nil {
		return "", err
	}
	writeCtx, cancelWrite := context.WithCancel(ctx)
	defer cancelWrite()
	writer := handle.NewWriter(writeCtx)
	writer.ChunkSize = 0
	writer.ContentType = "application/octet-stream"
	// Private server-written objects must not become reusable through a shared
	// browser cache. This provider metadata is deliberately non-authoritative.
	writer.CacheControl = "no-store"
	writer.CRC32C = crc32.Checksum(body, crc32.MakeTable(crc32.Castagnoli))
	writer.SendCRC32C = true
	if _, err = io.Copy(writer, bytes.NewReader(body)); err != nil {
		cancelWrite()
		return "", classifyPut(condition, err)
	}
	if err = writer.Close(); err != nil {
		return "", classifyPut(condition, err)
	}
	attrs := writer.Attrs()
	if attrs == nil || attrs.Generation <= 0 {
		return "", domain.NewError(domain.ErrorInternal, "GCS upload returned no generation")
	}
	return encodeVersion(attrs.Generation), nil
}

func (b *Backend) Delete(ctx context.Context, key objectstore.Key, condition objectstore.DeleteCondition) error {
	if err := objectstore.ContextError(ctx); err != nil {
		return err
	}
	if !key.Valid() || condition.Version == "" {
		return domain.NewError(domain.ErrorInvalid, "invalid object delete")
	}
	generation, err := decodeVersion(condition.Version)
	if err != nil {
		return err
	}
	err = b.bucket.Object(key.String()).If(storage.Conditions{GenerationMatch: generation}).Retryer(storage.WithPolicy(storage.RetryNever)).Delete(ctx)
	return classify("GCS object delete failed", err)
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
	sourceGeneration, err := decodeVersion(condition.SourceVersion)
	if err != nil {
		return objectstore.CopyResult{}, err
	}
	destinationHandle, err := b.conditionedObject(destination, condition.Destination)
	if err != nil {
		return objectstore.CopyResult{}, err
	}
	sourceHandle := b.bucket.Object(source.String()).If(storage.Conditions{GenerationMatch: sourceGeneration}).Retryer(storage.WithPolicy(storage.RetryNever))
	destinationHandle = destinationHandle.Retryer(storage.WithPolicy(storage.RetryNever))
	attrs, err := destinationHandle.CopierFrom(sourceHandle).Run(ctx)
	if err != nil {
		return objectstore.CopyResult{}, b.classifyCopy(ctx, source, sourceGeneration, condition.Destination, err)
	}
	if attrs.Generation <= 0 || attrs.Size < 0 {
		return objectstore.CopyResult{}, domain.NewError(domain.ErrorInternal, "GCS copy returned invalid metadata")
	}
	return objectstore.CopyResult{Version: encodeVersion(attrs.Generation), Size: attrs.Size}, nil
}

func (b *Backend) classifyCopy(ctx context.Context, source objectstore.Key, sourceGeneration int64, destination objectstore.PutCondition, err error) error {
	mapped := classify("GCS object copy failed", err)
	if destination.Mode != objectstore.PutCreateOnly || !errors.Is(mapped, domain.ErrPreconditionFailed) {
		return mapped
	}
	// GCS uses HTTP 412 for both a stale source generation and a create-only
	// destination conflict. Resolve that ambiguity without weakening either
	// condition: an exact-generation metadata read can only identify the source
	// generation used by this copy.
	if _, sourceErr := b.bucket.Object(source.String()).Generation(sourceGeneration).Attrs(ctx); sourceErr != nil {
		if errors.Is(classify("GCS copy source verification failed", sourceErr), domain.ErrNotFound) {
			return domain.WrapError(domain.ErrorPreconditionFailed, "GCS copy source version does not match", err)
		}
		return classify("GCS copy source verification failed", sourceErr)
	}
	return domain.WrapError(domain.ErrorConflict, "GCS copy destination already exists", err)
}

func (b *Backend) conditionedObject(key objectstore.Key, condition objectstore.PutCondition) (*storage.ObjectHandle, error) {
	handle := b.bucket.Object(key.String())
	switch condition.Mode {
	case objectstore.PutCreateOnly:
		return handle.If(storage.Conditions{DoesNotExist: true}).Retryer(storage.WithPolicy(storage.RetryNever)), nil
	case objectstore.PutMatch:
		generation, err := decodeVersion(condition.Version)
		if err != nil {
			return nil, err
		}
		return handle.If(storage.Conditions{GenerationMatch: generation}).Retryer(storage.WithPolicy(storage.RetryNever)), nil
	default:
		return nil, domain.NewError(domain.ErrorInvalid, "invalid object put condition")
	}
}

func encodeVersion(generation int64) objectstore.NativeVersion {
	return objectstore.NativeVersion(versionPrefix + strconv.FormatInt(generation, 10))
}

func decodeVersion(version objectstore.NativeVersion) (int64, error) {
	text := string(version)
	if !strings.HasPrefix(text, versionPrefix) {
		return 0, domain.NewError(domain.ErrorPreconditionFailed, "GCS object version does not match")
	}
	generation, err := strconv.ParseInt(strings.TrimPrefix(text, versionPrefix), 10, 64)
	if err != nil || generation <= 0 || encodeVersion(generation) != version {
		return 0, domain.NewError(domain.ErrorPreconditionFailed, "GCS object version does not match")
	}
	return generation, nil
}

func classifyPut(condition objectstore.PutCondition, err error) error {
	mapped := classify("GCS object write failed", err)
	if condition.Mode == objectstore.PutCreateOnly && errors.Is(mapped, domain.ErrPreconditionFailed) {
		return domain.WrapError(domain.ErrorConflict, "GCS object already exists", err)
	}
	return mapped
}

func classify(message string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, storage.ErrObjectNotExist) {
		return domain.WrapError(domain.ErrorNotFound, message, err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return domain.WrapError(domain.ErrorUnavailable, message, err)
	}
	var apiError *googleapi.Error
	if errors.As(err, &apiError) {
		switch apiError.Code {
		case 400:
			return domain.WrapError(domain.ErrorInvalid, message, err)
		case 401:
			return domain.WrapError(domain.ErrorUnauthenticated, message, err)
		case 403:
			return domain.WrapError(domain.ErrorUnauthorized, message, err)
		case 404:
			return domain.WrapError(domain.ErrorNotFound, message, err)
		case 409:
			return domain.WrapError(domain.ErrorConflict, message, err)
		case 412:
			return domain.WrapError(domain.ErrorPreconditionFailed, message, err)
		case 429:
			return domain.WrapError(domain.ErrorRateLimited, message, err)
		case 408, 500, 502, 503, 504:
			return domain.WrapError(domain.ErrorUnavailable, message, err)
		}
	}
	return domain.WrapError(domain.ErrorInternal, message, fmt.Errorf("GCS transport: %w", err))
}
