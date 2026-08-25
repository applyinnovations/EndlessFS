// Package budgettest provides request-count instrumentation for deterministic
// storage-economics tests. It is a thin object-store adapter and is not wired
// into the production application.
package budgettest

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

type Backend struct {
	role    providerbudget.Role
	backend objectstore.Backend
	ledger  *providerbudget.Ledger
}

func Wrap(role providerbudget.Role, backend objectstore.Backend, ledger *providerbudget.Ledger) *Backend {
	return &Backend{role: role, backend: backend, ledger: ledger}
}

func (backend *Backend) Head(ctx context.Context, key objectstore.Key) (objectstore.ObjectInfo, error) {
	result, err := backend.backend.Head(ctx, key)
	backend.record(providerbudget.RequestObjectHead, key.String(), 0, 0, err)
	return result, err
}

func (backend *Backend) Verify(ctx context.Context, key objectstore.Key, expected objectstore.ExpectedIntegrity) (objectstore.ObjectInfo, error) {
	result, err := backend.backend.Verify(ctx, key, expected)
	backend.record(providerbudget.RequestObjectVerify, key.String(), 0, 0, err)
	return result, err
}

func (backend *Backend) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	result, err := backend.backend.Get(ctx, key)
	backend.record(providerbudget.RequestObjectGet, key.String(), 0, int64(len(result.Body)), err)
	return result, err
}

func (backend *Backend) Open(ctx context.Context, key objectstore.Key) (objectstore.ObjectReader, error) {
	result, err := backend.backend.Open(ctx, key)
	if err != nil {
		backend.record(providerbudget.RequestObjectOpen, key.String(), 0, 0, err)
		return result, err
	}
	result.Body = &recordingReadCloser{ReadCloser: result.Body, record: func(bytes int64, streamErr error) {
		backend.record(providerbudget.RequestObjectOpen, key.String(), 0, bytes, streamErr)
	}}
	return result, nil
}

func (backend *Backend) List(ctx context.Context, request objectstore.ListRequest) (objectstore.ListPage, error) {
	result, err := backend.backend.List(ctx, request)
	backend.record(providerbudget.RequestObjectList, request.Prefix, 0, 0, err)
	return result, err
}

func (backend *Backend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	result, err := backend.backend.Put(ctx, key, body, condition)
	backend.record(providerbudget.RequestObjectPut, key.String(), int64(len(body)), 0, err)
	return result, err
}

func (backend *Backend) Delete(ctx context.Context, key objectstore.Key, condition objectstore.DeleteCondition) error {
	err := backend.backend.Delete(ctx, key, condition)
	backend.record(providerbudget.RequestObjectDelete, key.String(), 0, 0, err)
	return err
}

func (backend *Backend) Copy(ctx context.Context, source, destination objectstore.Key, condition objectstore.CopyCondition) (objectstore.CopyResult, error) {
	result, err := backend.backend.Copy(ctx, source, destination, condition)
	backend.record(providerbudget.RequestObjectCopy, source.String()+" -> "+destination.String(), 0, 0, err)
	return result, err
}

func (backend *Backend) record(kind providerbudget.RequestKind, target string, requestBytes, responseBytes int64, err error) {
	backend.ledger.Record(providerbudget.Event{Role: backend.role, Kind: kind, Target: target, RequestBytes: requestBytes, ResponseBytes: responseBytes, Failed: err != nil})
}

func (backend *Backend) transferBackend() (objectstore.DirectTransferBackend, error) {
	transfers, ok := backend.backend.(objectstore.DirectTransferBackend)
	if !ok {
		return nil, errors.New("instrumented backend has no direct-transfer capability")
	}
	return transfers, nil
}

func (backend *Backend) BackendKind() string {
	transfers, err := backend.transferBackend()
	if err != nil {
		return ""
	}
	return transfers.BackendKind()
}

func (backend *Backend) BeginUpload(ctx context.Context, request objectstore.UploadRequest) (objectstore.UploadHandle, error) {
	transfers, err := backend.transferBackend()
	if err != nil {
		return objectstore.UploadHandle{}, err
	}
	result, err := transfers.BeginUpload(ctx, request)
	backend.record(providerbudget.RequestUploadBegin, request.Key.String(), 0, 0, err)
	return result, err
}

func (backend *Backend) ResumeUpload(ctx context.Context, lease []byte) (objectstore.UploadCapability, error) {
	transfers, err := backend.transferBackend()
	if err != nil {
		return objectstore.UploadCapability{}, err
	}
	result, err := transfers.ResumeUpload(ctx, lease)
	backend.record(providerbudget.RequestUploadResume, "", 0, 0, err)
	return result, err
}

func (backend *Backend) UploadProgress(ctx context.Context, lease []byte) (objectstore.UploadProgress, error) {
	transfers, err := backend.transferBackend()
	if err != nil {
		return objectstore.UploadProgress{}, err
	}
	result, err := transfers.UploadProgress(ctx, lease)
	backend.record(providerbudget.RequestUploadProgress, "", 0, 0, err)
	return result, err
}

func (backend *Backend) AbortUpload(ctx context.Context, lease []byte) error {
	transfers, err := backend.transferBackend()
	if err != nil {
		return err
	}
	err = transfers.AbortUpload(ctx, lease)
	backend.record(providerbudget.RequestUploadAbort, "", 0, 0, err)
	return err
}

func (backend *Backend) CreateDownload(ctx context.Context, request objectstore.DownloadRequest) (objectstore.DownloadCapability, error) {
	transfers, err := backend.transferBackend()
	if err != nil {
		return objectstore.DownloadCapability{}, err
	}
	result, err := transfers.CreateDownload(ctx, request)
	backend.record(providerbudget.RequestDownloadSign, request.Key.String(), 0, 0, err)
	return result, err
}

type recordingReadCloser struct {
	io.ReadCloser
	mu       sync.Mutex
	read     int64
	readErr  error
	recorded bool
	record   func(int64, error)
}

func (reader *recordingReadCloser) Read(buffer []byte) (int, error) {
	count, err := reader.ReadCloser.Read(buffer)
	reader.mu.Lock()
	reader.read += int64(count)
	if err != nil && !errors.Is(err, io.EOF) && reader.readErr == nil {
		reader.readErr = err
	}
	reader.mu.Unlock()
	return count, err
}

func (reader *recordingReadCloser) Close() error {
	closeErr := reader.ReadCloser.Close()
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.readErr == nil && closeErr != nil {
		reader.readErr = closeErr
	}
	if !reader.recorded {
		reader.recorded = true
		reader.record(reader.read, reader.readErr)
	}
	return closeErr
}
