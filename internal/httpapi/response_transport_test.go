package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
)

type responseTransportWriter struct {
	header     http.Header
	status     int
	body       []byte
	written    int
	writeErr   error
	short      bool
	deadline   time.Time
	flushed    bool
	flushCalls int
	flushErr   error
	hijacked   bool
	hijackErr  error
}

func (writer *responseTransportWriter) Header() http.Header    { return writer.header }
func (writer *responseTransportWriter) WriteHeader(status int) { writer.status = status }
func (writer *responseTransportWriter) Write(body []byte) (int, error) {
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	writer.body = append(writer.body, body...)
	if writer.writeErr != nil || writer.short {
		return writer.written, writer.writeErr
	}
	return len(body), nil
}
func (writer *responseTransportWriter) SetWriteDeadline(deadline time.Time) error {
	writer.deadline = deadline
	return nil
}
func (writer *responseTransportWriter) FlushError() error {
	writer.flushed = true
	writer.flushCalls++
	return writer.flushErr
}
func (writer *responseTransportWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	writer.hijacked = true
	return nil, nil, writer.hijackErr
}

type responseTransportLog struct{ records chan slog.Record }

func (*responseTransportLog) Enabled(context.Context, slog.Level) bool { return true }
func (handler *responseTransportLog) Handle(_ context.Context, record slog.Record) error {
	handler.records <- record.Clone()
	return nil
}
func (handler *responseTransportLog) WithAttrs([]slog.Attr) slog.Handler { return handler }
func (handler *responseTransportLog) WithGroup(string) slog.Handler      { return handler }
func responseTransportRecord(t *testing.T, logs *responseTransportLog) (string, map[string]any) {
	t.Helper()
	select {
	case record := <-logs.records:
		values := map[string]any{}
		record.Attrs(func(attr slog.Attr) bool { values[attr.Key] = attr.Value.Any(); return true })
		return record.Message, values
	case <-time.After(5 * time.Second):
		t.Fatal("transport completion event missing")
		return "", nil
	}
}

func TestIntegrationResponseTransportFailurePreservesIntendedStatusAndRecordsSafeOutcome(t *testing.T) {
	const private = "sentinel-user/private/file.txt storage-key credential capability=https://private/?secret body"
	for _, test := range []struct {
		name   string
		cause  error
		result string
		short  bool
	}{
		{"success", nil, "success", false},
		{"timeout", fmt.Errorf(private+": %w", os.ErrDeadlineExceeded), "timeout", false},
		{"canceled", fmt.Errorf(private+": %w", context.Canceled), "canceled", false},
		{"write-error", errors.New(private), "unavailable", false},
		{"short-write", nil, "unavailable", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, logging := range []bool{false, true} {
				logs := &responseTransportLog{records: make(chan slog.Record, 8)}
				observer := telemetry.New(nil, slog.New(logs))
				defer observer.Close()
				observer.SetVersion("transport-review.6db3cd38")
				mux := http.NewServeMux()
				mux.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, _ *http.Request) {
					writeJSON(w, http.StatusOK, map[string]string{"private": private})
				})
				var next http.Handler = mux
				if logging {
					next = requestLogMiddleware(next, slog.New(logs))
				}
				handler := requestIDMiddleware(telemetryMiddleware(next, mux))
				request := httptest.NewRequest(http.MethodGet, "/api/v1/config?capability=sentinel-secret", nil)
				request.Header.Set("X-Request-ID", "transport-request-42")
				request.Header.Set("Authorization", "Bearer sentinel-secret")
				request.Header.Set("traceparent", "00-f639a73ef118d02f3de71db54f2e9165-0123456789abcdef-01")
				request = request.WithContext(telemetry.Context(request.Context(), observer))
				writer := &responseTransportWriter{header: make(http.Header), written: 3, writeErr: test.cause, short: test.short}
				handler.ServeHTTP(writer, request)
				if writer.status != http.StatusOK || !bytes.Contains(writer.body, []byte("sentinel-user")) || writer.Header().Get("Content-Type") != "application/json" {
					t.Fatal("transport observation changed intended public response")
				}
				var output bytes.Buffer
				observer.WritePrometheus(&output)
				want := `endlessfs_operations_total{operation="http.request",role="application",result="` + test.result + `"} 1`
				if !strings.Contains(output.String(), want) {
					t.Fatalf("surfaced write outcome missing (logging=%v): want %s", logging, want)
				}
				for _, secret := range []string{"sentinel", "transport-request-42", "f639a73ef118d02f3de71db54f2e9165", "stage=", "reason="} {
					if strings.Contains(output.String(), secret) {
						t.Fatalf("unbounded/private metric label %q", secret)
					}
				}
				expected := 0
				if logging {
					expected++
				}
				if test.cause != nil || test.short {
					expected++
				}
				seen := map[string]bool{}
				for range expected {
					message, values := responseTransportRecord(t, logs)
					seen[message] = true
					if strings.Contains(fmt.Sprint(values), "sentinel") {
						t.Fatal("raw transport error/request/body entered logs")
					}
					if values["requestID"] != "transport-request-42" || values["traceID"] != "f639a73ef118d02f3de71db54f2e9165" {
						t.Fatal("safe log correlation missing")
					}
					if values["result"] != test.result {
						t.Fatalf("transport log result=%v want %s", values["result"], test.result)
					}
					if (test.cause != nil || test.short) && (values["stage"] != "response_write" || values["reason"] != map[string]string{"timeout": "deadline", "canceled": "canceled", "unavailable": "transport_failure"}[test.result]) {
						t.Fatal("safe transport stage/reason missing")
					}
					if message == "request_completed" && values["status"] != int64(http.StatusOK) {
						t.Fatal("request log lost intended status")
					}
					if message == "operation_completed" && values["build"] != "transport-review.6db3cd38" {
						t.Fatal("transport operation lost build identity")
					}
				}
				if logging && !seen["request_completed"] || (test.cause != nil || test.short) && !seen["operation_completed"] {
					t.Fatal("transport completion accounting missing")
				}
			}
		})
	}
}

func TestResponseTransportRecorderPreservesWriterReturnsAndControllerUnwrap(t *testing.T) {
	cause := errors.New("sentinel-private-write-error")
	writer := &responseTransportWriter{header: make(http.Header), written: 2, writeErr: cause}
	recorder := &statusRecorder{ResponseWriter: writer}
	n, err := recorder.Write([]byte("abcd"))
	if n != 2 || err != cause || recorder.status != http.StatusOK {
		t.Fatal("recorder changed write return values/status")
	}
	recorder.WriteHeader(http.StatusServiceUnavailable)
	if writer.status != http.StatusOK {
		t.Fatal("recorder changed committed response status")
	}
	deadline := time.Unix(123, 0)
	controller := http.NewResponseController(recorder)
	if err := controller.SetWriteDeadline(deadline); err != nil || writer.deadline != deadline {
		t.Fatal("writer deadline capability lost through Unwrap")
	}
	if err := controller.Flush(); err != nil || !writer.flushed {
		t.Fatal("writer flush capability lost through Unwrap")
	}
	writer.hijackErr = cause
	if connection, buffer, err := controller.Hijack(); connection != nil || buffer != nil || err != cause || !writer.hijacked {
		t.Fatal("writer hijack capability/return values lost through Unwrap")
	}
	if err := controller.EnableFullDuplex(); !errors.Is(err, http.ErrNotSupported) {
		t.Fatal("unsupported writer capability was invented")
	}
}

func TestIntegrationResponseTransportOverridesProblemButPreservesProblemResponse(t *testing.T) {
	observer := telemetry.New(nil, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, r, telemetry.Diagnostic(domain.ErrInvalid, telemetry.RequestValidation, telemetry.Validation))
	})
	writer := &responseTransportWriter{header: make(http.Header), writeErr: os.ErrDeadlineExceeded}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil).WithContext(telemetry.Context(context.Background(), observer))
	telemetryMiddleware(mux, mux).ServeHTTP(writer, request)
	if writer.status != http.StatusBadRequest || !bytes.Contains(writer.body, []byte(`"status":400`)) {
		t.Fatal("transport failure changed public problem semantics")
	}
	var output bytes.Buffer
	observer.WritePrometheus(&output)
	if !strings.Contains(output.String(), `operation="http.request",role="application",result="timeout"} 1`) {
		t.Fatal("problem classification masked surfaced transport timeout")
	}
}

func TestResponseTransportShortWriteAndFirstFailurePreserveReturns(t *testing.T) {
	observer := telemetry.New(nil, nil)
	ctx, activity := telemetry.Start(telemetry.Context(context.Background(), observer), telemetry.HTTP, telemetry.Application)
	writer := &responseTransportWriter{header: make(http.Header), written: 2, short: true}
	recorder := &statusRecorder{ResponseWriter: writer, ctx: ctx}
	if n, err := recorder.Write([]byte("abcd")); n != 2 || err != nil {
		t.Fatal("short-write observation changed Writer return values")
	}
	writer.writeErr = os.ErrDeadlineExceeded
	if n, err := recorder.Write([]byte("abcd")); n != 2 || err != os.ErrDeadlineExceeded {
		t.Fatal("later write return values changed")
	}
	activity.EndResult(telemetry.Success)
	var output bytes.Buffer
	observer.WritePrometheus(&output)
	if !strings.Contains(output.String(), `operation="http.request",role="application",result="unavailable"} 1`) {
		t.Fatal("later write masked first transport failure")
	}
}

func TestHTTPResponseTransportFailureDoesNotMaskPanic(t *testing.T) {
	observer := telemetry.New(nil, nil)
	mux := http.NewServeMux()
	const sentinel = "sentinel-private-panic"
	mux.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("safe")); panic(sentinel) })
	request := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil).WithContext(telemetry.Context(context.Background(), observer))
	writer := &responseTransportWriter{header: make(http.Header), writeErr: os.ErrDeadlineExceeded}
	func() {
		defer func() {
			if recover() != sentinel {
				t.Fatal("transport telemetry changed panic propagation")
			}
		}()
		telemetryMiddleware(mux, mux).ServeHTTP(writer, request)
	}()
	var output bytes.Buffer
	observer.WritePrometheus(&output)
	if !strings.Contains(output.String(), `operation="http.request",role="application",result="panic"} 1`) || strings.Contains(output.String(), sentinel) {
		t.Fatal("transport outcome masked panic or leaked panic text")
	}
}

func TestResponseTransportRequestLogWorksWithoutTelemetryObserver(t *testing.T) {
	logs := &responseTransportLog{records: make(chan slog.Record, 1)}
	handler := requestIDMiddleware(requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("safe")) }), slog.New(logs)))
	writer := &responseTransportWriter{header: make(http.Header), writeErr: os.ErrDeadlineExceeded}
	handler.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/", nil))
	message, values := responseTransportRecord(t, logs)
	if message != "request_completed" || values["result"] != "timeout" || values["stage"] != "response_write" || values["reason"] != "deadline" || values["status"] != int64(http.StatusOK) {
		t.Fatal("request transport log depends on configured telemetry observer")
	}
}
