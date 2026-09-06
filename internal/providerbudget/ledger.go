package providerbudget

import (
	"errors"
	"io"
	"net/http"
	"sync"
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
	trace := TraceFromContext(request.Context())
	event := Event{Role: transport.role, Kind: kind, Operation: trace.Operation, Subsystem: trace.Subsystem, ParallelGroup: trace.ParallelGroup}
	if requestCounter != nil {
		requestBytes = requestCounter.BytesRead()
	} else if request.ContentLength > 0 {
		requestBytes = request.ContentLength
	}
	if requestErr != nil {
		event.RequestBytes, event.Failed = requestBytes, true
		transport.ledger.Record(event)
		return nil, requestErr
	}
	if response.Body == nil {
		event.RequestBytes, event.StatusCode, event.Failed = requestBytes, response.StatusCode, response.StatusCode >= http.StatusBadRequest
		transport.ledger.Record(event)
		return response, nil
	}
	response.Body = &recordingReadCloser{ReadCloser: response.Body, record: func(responseBytes int64, streamErr error) {
		event.RequestBytes, event.ResponseBytes, event.StatusCode, event.Failed = requestBytes, responseBytes, response.StatusCode, streamErr != nil || response.StatusCode >= http.StatusBadRequest
		transport.ledger.Record(event)
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
