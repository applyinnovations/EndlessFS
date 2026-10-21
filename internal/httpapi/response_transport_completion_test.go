package httpapi

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/telemetry"
)

type completionPlainWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (writer *completionPlainWriter) Header() http.Header    { return writer.header }
func (writer *completionPlainWriter) WriteHeader(status int) { writer.status = status }
func (writer *completionPlainWriter) Write(body []byte) (int, error) {
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	return writer.body.Write(body)
}

type completionUnwrapper struct{ http.ResponseWriter }

func (writer completionUnwrapper) Unwrap() http.ResponseWriter { return writer.ResponseWriter }

func TestResponseTransportCompletionFlushesExactlyOnceBeforeBothObservations(t *testing.T) {
	for _, test := range []struct {
		name   string
		cause  error
		result string
	}{
		{"success", nil, "success"},
		{"deadline", os.ErrDeadlineExceeded, "timeout"},
		{"canceled", context.Canceled, "canceled"},
		{"failure", errors.New("sentinel-private-path capability=secret"), "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			logs := &responseTransportLog{records: make(chan slog.Record, 4)}
			observer := telemetry.New(nil, slog.New(logs))
			defer observer.Close()
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(completionUnwrapper{w}, http.StatusOK, struct {
					OK bool `json:"ok"`
				}{true})
			})
			writer := &responseTransportWriter{header: make(http.Header), flushErr: test.cause}
			request := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil).WithContext(telemetry.Context(context.Background(), observer))
			requestIDMiddleware(telemetryMiddleware(requestLogMiddleware(mux, slog.New(logs)), mux)).ServeHTTP(completionUnwrapper{writer}, request)
			if writer.flushCalls != 1 || writer.status != http.StatusOK || string(writer.body) != "{\"ok\":true}\n" {
				t.Fatal("normal completion did not preserve body/status and exactly one final flush")
			}
			message, values := responseTransportRecord(t, logs)
			if message != "request_completed" || values["result"] != test.result {
				t.Fatal("request was logged before its buffered flush outcome")
			}
			if test.cause != nil {
				message, values = responseTransportRecord(t, logs)
				if message != "operation_completed" || values["result"] != test.result || values["stage"] != "response_write" {
					t.Fatal("HTTP activity ended before its buffered flush outcome")
				}
			}
		})
	}
}

func TestResponseTransportCompletionIgnoresUnsupportedFlushAndDoesNotInventHijacker(t *testing.T) {
	writer := &completionPlainWriter{header: make(http.Header)}
	observer := telemetry.New(nil, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, _ *http.Request) {
		if _, ok := w.(http.Hijacker); ok {
			t.Fatal("unsupported Hijacker interface advertised")
		}
		if _, _, err := http.NewResponseController(w).Hijack(); !errors.Is(err, http.ErrNotSupported) {
			t.Fatal("unsupported hijack behavior changed")
		}
		writeJSON(w, http.StatusOK, struct {
			OK bool `json:"ok"`
		}{true})
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil).WithContext(telemetry.Context(context.Background(), observer))
	telemetryMiddleware(mux, mux).ServeHTTP(completionUnwrapper{writer}, request)
	if writer.status != http.StatusOK || writer.body.String() != "{\"ok\":true}\n" {
		t.Fatal("unsupported flush changed public response")
	}
	var output bytes.Buffer
	observer.WritePrometheus(&output)
	if !bytes.Contains(output.Bytes(), []byte(`operation="http.request",role="application",result="success"} 1`)) {
		t.Fatal("unsupported optional flush was misclassified as transport failure")
	}
}

func TestResponseTransportCompletionSkipsSuccessfulHijackAndPanic(t *testing.T) {
	for _, name := range []string{"hijack", "panic", "failed-hijack"} {
		t.Run(name, func(t *testing.T) {
			observer := telemetry.New(nil, nil)
			writer := &responseTransportWriter{header: make(http.Header)}
			cause := errors.New("safe-hijack-failure")
			if name == "failed-hijack" {
				writer.hijackErr = cause
			}
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, _ *http.Request) {
				if name == "panic" {
					panic(cause)
				}
				if _, ok := w.(http.Hijacker); !ok {
					t.Fatal("existing Hijacker capability lost")
				}
				connection, buffer, err := http.NewResponseController(completionUnwrapper{w}).Hijack()
				if connection != nil || buffer != nil || (name == "hijack" && err != nil) || (name == "failed-hijack" && err != cause) {
					t.Fatal("hijack result changed")
				}
				if name == "failed-hijack" {
					writeJSON(w, http.StatusOK, struct {
						OK bool `json:"ok"`
					}{true})
				}
			})
			request := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil).WithContext(telemetry.Context(context.Background(), observer))
			func() {
				defer func() {
					value := recover()
					if name == "panic" {
						if value != cause {
							t.Fatal("panic propagation changed")
						}
					} else if value != nil {
						t.Fatal("hijack completion panicked")
					}
				}()
				telemetryMiddleware(mux, mux).ServeHTTP(completionUnwrapper{writer}, request)
			}()
			wantFlush := 0
			if name == "failed-hijack" {
				wantFlush = 1
			}
			if writer.flushCalls != wantFlush {
				t.Fatal("panic/hijack finalization flush guard failed")
			}
		})
	}
}
