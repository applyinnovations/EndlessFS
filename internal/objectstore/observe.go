package objectstore

import (
	"context"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
	"io"
	"sync"
)

// Observe is transparent: one delegated call per operation and no new provider
// traffic. Optional direct-transfer support is preserved rather than invented.
func Observe(backend Backend, role telemetry.Role) Backend {
	observed := &observedBackend{Backend: backend, role: role}
	if transfers, ok := backend.(DirectTransferBackend); ok {
		return &observedTransfers{observedBackend: observed, transfers: transfers}
	}
	return observed
}

type observedBackend struct {
	Backend
	role telemetry.Role
}

func (backend *observedBackend) Head(ctx context.Context, key Key) (ObjectInfo, error) {
	ctx, activity := telemetry.Start(ctx, telemetry.ProviderHead, backend.role)
	value, err := backend.Backend.Head(ctx, key)
	if err == nil {
		activity.Bytes(0, 0)
	}
	activity.End(err)
	return value, err
}

func (backend *observedBackend) Verify(ctx context.Context, key Key, integrity ExpectedIntegrity) (ObjectInfo, error) {
	ctx, activity := telemetry.Start(ctx, telemetry.ProviderVerify, backend.role)
	value, err := backend.Backend.Verify(ctx, key, integrity)
	if err == nil {
		activity.Bytes(0, 0)
	}
	activity.End(err)
	return value, err
}

func (backend *observedBackend) Get(ctx context.Context, key Key) (Object, error) {
	ctx, activity := telemetry.Start(ctx, telemetry.ProviderGet, backend.role)
	value, err := backend.Backend.Get(ctx, key)
	if err == nil {
		activity.Bytes(int64(len(value.Body)), 0)
	}
	activity.End(err)
	return value, err
}

func (backend *observedBackend) List(ctx context.Context, request ListRequest) (ListPage, error) {
	ctx, activity := telemetry.Start(ctx, telemetry.ProviderList, backend.role)
	value, err := backend.Backend.List(ctx, request)
	if err == nil {
		activity.Bytes(0, 0)
	}
	activity.End(err)
	return value, err
}

func (backend *observedBackend) Put(ctx context.Context, key Key, body []byte, condition PutCondition) (NativeVersion, error) {
	ctx, activity := telemetry.Start(ctx, telemetry.ProviderPut, backend.role)
	value, err := backend.Backend.Put(ctx, key, body, condition)
	if err == nil {
		activity.Bytes(0, int64(len(body)))
	}
	activity.End(err)
	return value, err
}

func (backend *observedBackend) Copy(ctx context.Context, source, destination Key, condition CopyCondition) (CopyResult, error) {
	ctx, activity := telemetry.Start(ctx, telemetry.ProviderCopy, backend.role)
	value, err := backend.Backend.Copy(ctx, source, destination, condition)
	if err == nil {
		activity.Bytes(0, 0)
	}
	activity.End(err)
	return value, err
}

func (backend *observedBackend) Delete(ctx context.Context, key Key, condition DeleteCondition) error {
	ctx, activity := telemetry.Start(ctx, telemetry.ProviderDelete, backend.role)
	err := backend.Backend.Delete(ctx, key, condition)
	activity.End(err)
	return err
}
func (backend *observedBackend) Open(ctx context.Context, key Key) (ObjectReader, error) {
	ctx, activity := telemetry.Start(ctx, telemetry.ProviderOpen, backend.role)
	value, err := backend.Backend.Open(ctx, key)
	if err != nil {
		activity.End(err)
		return value, err
	}
	if activity != nil {
		value.Body = &observedReader{ReadCloser: value.Body, activity: activity}
	}
	return value, nil
}

type observedReader struct {
	io.ReadCloser
	activity *telemetry.Activity
	once     sync.Once
}

func (reader *observedReader) Read(body []byte) (int, error) {
	count, err := reader.ReadCloser.Read(body)
	reader.activity.Bytes(int64(count), 0)
	if err != nil && err != io.EOF {
		reader.once.Do(func() { reader.activity.End(err) })
	}
	return count, err
}
func (reader *observedReader) Close() error {
	err := reader.ReadCloser.Close()
	reader.once.Do(func() { reader.activity.End(err) })
	return err
}

type observedTransfers struct {
	*observedBackend
	transfers DirectTransferBackend
}

func (backend *observedTransfers) BackendKind() string { return backend.transfers.BackendKind() }

func (backend *observedTransfers) BeginUpload(ctx context.Context, request UploadRequest) (UploadHandle, error) {
	ctx, activity := telemetry.Start(ctx, telemetry.ProviderUploadBegin, backend.role)
	value, err := backend.transfers.BeginUpload(ctx, request)
	activity.End(err)
	return value, err
}

func (backend *observedTransfers) ResumeUpload(ctx context.Context, lease []byte) (UploadCapability, error) {
	ctx, activity := telemetry.Start(ctx, telemetry.ProviderUploadResume, backend.role)
	value, err := backend.transfers.ResumeUpload(ctx, lease)
	activity.End(err)
	return value, err
}

func (backend *observedTransfers) UploadProgress(ctx context.Context, lease []byte) (UploadProgress, error) {
	ctx, activity := telemetry.Start(ctx, telemetry.ProviderUploadStatus, backend.role)
	value, err := backend.transfers.UploadProgress(ctx, lease)
	activity.End(err)
	return value, err
}

func (backend *observedTransfers) CreateDownload(ctx context.Context, request DownloadRequest) (DownloadCapability, error) {
	ctx, activity := telemetry.Start(ctx, telemetry.ProviderDownload, backend.role)
	value, err := backend.transfers.CreateDownload(ctx, request)
	activity.End(err)
	return value, err
}
func (backend *observedTransfers) AbortUpload(ctx context.Context, lease []byte) error {
	ctx, activity := telemetry.Start(ctx, telemetry.ProviderUploadAbort, backend.role)
	err := backend.transfers.AbortUpload(ctx, lease)
	activity.End(err)
	return err
}
