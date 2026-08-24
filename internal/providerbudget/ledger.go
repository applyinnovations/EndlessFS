package providerbudget

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"

	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

type Ledger struct {
	mu     sync.Mutex
	events []Event
}

func NewLedger() *Ledger { return &Ledger{} }

func (ledger *Ledger) Record(event Event) {
	if ledger == nil {
		return
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	ledger.events = append(ledger.events, event)
}

func (ledger *Ledger) Events() []Event {
	if ledger == nil {
		return nil
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	return append([]Event(nil), ledger.events...)
}

func (ledger *Ledger) Reset() {
	if ledger == nil {
		return
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	ledger.events = nil
}

type InstrumentedBackend struct {
	role    Role
	backend objectstore.Backend
	ledger  *Ledger
}

func InstrumentBackend(role Role, backend objectstore.Backend, ledger *Ledger) *InstrumentedBackend {
	return &InstrumentedBackend{role: role, backend: backend, ledger: ledger}
}

func (backend *InstrumentedBackend) Head(ctx context.Context, key objectstore.Key) (objectstore.ObjectInfo, error) {
	result, err := backend.backend.Head(ctx, key)
	backend.record(RequestObjectHead, key.String(), 0, 0, err)
	return result, err
}

func (backend *InstrumentedBackend) Verify(ctx context.Context, key objectstore.Key, expected objectstore.ExpectedIntegrity) (objectstore.ObjectInfo, error) {
	result, err := backend.backend.Verify(ctx, key, expected)
	backend.record(RequestObjectVerify, key.String(), 0, 0, err)
	return result, err
}

func (backend *InstrumentedBackend) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	result, err := backend.backend.Get(ctx, key)
	backend.record(RequestObjectGet, key.String(), 0, int64(len(result.Body)), err)
	return result, err
}

func (backend *InstrumentedBackend) Open(ctx context.Context, key objectstore.Key) (objectstore.ObjectReader, error) {
	result, err := backend.backend.Open(ctx, key)
	if err != nil {
		backend.record(RequestObjectOpen, key.String(), 0, 0, err)
		return result, err
	}
	result.Body = &recordingReadCloser{ReadCloser: result.Body, record: func(bytes int64, streamErr error) {
		backend.record(RequestObjectOpen, key.String(), 0, bytes, streamErr)
	}}
	return result, nil
}

func (backend *InstrumentedBackend) List(ctx context.Context, request objectstore.ListRequest) (objectstore.ListPage, error) {
	result, err := backend.backend.List(ctx, request)
	backend.record(RequestObjectList, request.Prefix, 0, 0, err)
	return result, err
}

func (backend *InstrumentedBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	result, err := backend.backend.Put(ctx, key, body, condition)
	backend.record(RequestObjectPut, key.String(), int64(len(body)), 0, err)
	return result, err
}

func (backend *InstrumentedBackend) Delete(ctx context.Context, key objectstore.Key, condition objectstore.DeleteCondition) error {
	err := backend.backend.Delete(ctx, key, condition)
	backend.record(RequestObjectDelete, key.String(), 0, 0, err)
	return err
}

func (backend *InstrumentedBackend) Copy(ctx context.Context, source, destination objectstore.Key, condition objectstore.CopyCondition) (objectstore.CopyResult, error) {
	result, err := backend.backend.Copy(ctx, source, destination, condition)
	backend.record(RequestObjectCopy, source.String()+" -> "+destination.String(), 0, 0, err)
	return result, err
}

func (backend *InstrumentedBackend) record(kind RequestKind, target string, requestBytes, responseBytes int64, err error) {
	backend.ledger.Record(Event{Role: backend.role, Kind: kind, Target: target, RequestBytes: requestBytes, ResponseBytes: responseBytes, Failed: err != nil})
}

func (backend *InstrumentedBackend) transferBackend() (objectstore.DirectTransferBackend, error) {
	transfers, ok := backend.backend.(objectstore.DirectTransferBackend)
	if !ok {
		return nil, errors.New("instrumented backend has no direct-transfer capability")
	}
	return transfers, nil
}

func (backend *InstrumentedBackend) BackendKind() string {
	transfers, err := backend.transferBackend()
	if err != nil {
		return ""
	}
	return transfers.BackendKind()
}

func (backend *InstrumentedBackend) BeginUpload(ctx context.Context, request objectstore.UploadRequest) (objectstore.UploadHandle, error) {
	transfers, err := backend.transferBackend()
	if err != nil {
		return objectstore.UploadHandle{}, err
	}
	result, err := transfers.BeginUpload(ctx, request)
	backend.record(RequestUploadBegin, request.Key.String(), 0, 0, err)
	return result, err
}

func (backend *InstrumentedBackend) ResumeUpload(ctx context.Context, lease []byte) (objectstore.UploadCapability, error) {
	transfers, err := backend.transferBackend()
	if err != nil {
		return objectstore.UploadCapability{}, err
	}
	result, err := transfers.ResumeUpload(ctx, lease)
	backend.record(RequestUploadResume, "", 0, 0, err)
	return result, err
}

func (backend *InstrumentedBackend) UploadProgress(ctx context.Context, lease []byte) (objectstore.UploadProgress, error) {
	transfers, err := backend.transferBackend()
	if err != nil {
		return objectstore.UploadProgress{}, err
	}
	result, err := transfers.UploadProgress(ctx, lease)
	backend.record(RequestUploadProgress, "", 0, 0, err)
	return result, err
}

func (backend *InstrumentedBackend) AbortUpload(ctx context.Context, lease []byte) error {
	transfers, err := backend.transferBackend()
	if err != nil {
		return err
	}
	err = transfers.AbortUpload(ctx, lease)
	backend.record(RequestUploadAbort, "", 0, 0, err)
	return err
}

func (backend *InstrumentedBackend) CreateDownload(ctx context.Context, request objectstore.DownloadRequest) (objectstore.DownloadCapability, error) {
	transfers, err := backend.transferBackend()
	if err != nil {
		return objectstore.DownloadCapability{}, err
	}
	result, err := transfers.CreateDownload(ctx, request)
	backend.record(RequestDownloadSign, request.Key.String(), 0, 0, err)
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

type RequestClassifier func(*http.Request) (RequestKind, error)

type instrumentedRoundTripper struct {
	role       Role
	base       http.RoundTripper
	ledger     *Ledger
	classifier RequestClassifier
}

func InstrumentRoundTripper(role Role, base http.RoundTripper, ledger *Ledger, classifier RequestClassifier) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &instrumentedRoundTripper{role: role, base: base, ledger: ledger, classifier: classifier}
}

func (transport *instrumentedRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	kind, err := transport.classifier(request)
	if err != nil {
		return nil, err
	}
	var requestCounter *countingReadCloser
	// Preserve http.NoBody exactly. The transport uses that sentinel to emit an
	// explicit zero Content-Length for provider protocols such as resumable
	// upload cancellation; wrapping it changes wire semantics.
	if request.Body != nil && request.Body != http.NoBody {
		requestCounter = &countingReadCloser{ReadCloser: request.Body}
		request.Body = requestCounter
	}
	response, requestErr := transport.base.RoundTrip(request)
	requestBytes := int64(0)
	if requestCounter != nil {
		requestBytes = requestCounter.BytesRead()
	} else if request.ContentLength > 0 {
		requestBytes = request.ContentLength
	}
	if requestErr != nil {
		transport.ledger.Record(Event{Role: transport.role, Kind: kind, RequestBytes: requestBytes, Failed: true})
		return nil, requestErr
	}
	if response.Body == nil {
		transport.ledger.Record(Event{Role: transport.role, Kind: kind, RequestBytes: requestBytes, StatusCode: response.StatusCode, Failed: response.StatusCode >= http.StatusBadRequest})
		return response, nil
	}
	response.Body = &recordingReadCloser{ReadCloser: response.Body, record: func(responseBytes int64, streamErr error) {
		transport.ledger.Record(Event{Role: transport.role, Kind: kind, RequestBytes: requestBytes, ResponseBytes: responseBytes, StatusCode: response.StatusCode, Failed: streamErr != nil || response.StatusCode >= http.StatusBadRequest})
	}}
	return response, nil
}

type countingReadCloser struct {
	io.ReadCloser
	mu   sync.Mutex
	read int64
}

func (reader *countingReadCloser) Read(buffer []byte) (int, error) {
	count, err := reader.ReadCloser.Read(buffer)
	reader.mu.Lock()
	reader.read += int64(count)
	reader.mu.Unlock()
	return count, err
}

func (reader *countingReadCloser) BytesRead() int64 {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.read
}
