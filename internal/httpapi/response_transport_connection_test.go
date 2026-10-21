package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/telemetry"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type bufferedResponseProbe struct {
	http.ResponseWriter
	written  int
	writeErr error
}

func (probe *bufferedResponseProbe) Write(body []byte) (int, error) {
	probe.written, probe.writeErr = probe.ResponseWriter.Write(body)
	return probe.written, probe.writeErr
}
func (probe *bufferedResponseProbe) Unwrap() http.ResponseWriter { return probe.ResponseWriter }

type bufferedHandlerObservation struct {
	deadlineErr        error
	contextBeforeWrite error
	contextAfterWrite  error
	requestHasDeadline bool
	written            int
	writeErr           error
}

// This uses a real HTTP/1 socket and the ordinary 30-second server setting.
// The handler injects an expired socket deadline instead of sleeping: the
// small JSON body enters net/http's response buffer successfully, then its
// implicit post-handler flush would fail. The socket deadline does not itself
// cancel the handler context. The explicit control-boundary flush now surfaces
// that failure before completion telemetry ends.
func TestIntegrationResponseTransportObservesRealHTTP1BufferedFlush(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "positive"
		if expired {
			name = "expired-write-deadline"
		}
		t.Run(name, func(t *testing.T) {
			logs := &responseTransportLog{records: make(chan slog.Record, 8)}
			observer := telemetry.New(nil, slog.New(logs))
			defer observer.Close()
			observer.SetVersion("buffered-http1-test")
			exporter := tracetest.NewInMemoryExporter()
			provider := telemetry.TraceProvider(exporter, observer, "buffered-http1-test", 1)
			defer func() { _ = provider.Shutdown(context.Background()) }()
			observed := make(chan bufferedHandlerObservation, 1)
			closed := make(chan struct{}, 1)
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, r *http.Request) {
				observation := bufferedHandlerObservation{}
				if expired {
					observation.deadlineErr = http.NewResponseController(w).SetWriteDeadline(time.Unix(1, 0))
				}
				_, observation.requestHasDeadline = r.Context().Deadline()
				observation.contextBeforeWrite = r.Context().Err()
				probe := &bufferedResponseProbe{ResponseWriter: w}
				writeJSON(probe, http.StatusOK, struct {
					OK bool `json:"ok"`
				}{OK: true})
				observation.written, observation.writeErr = probe.written, probe.writeErr
				observation.contextAfterWrite = r.Context().Err()
				observed <- observation
			})
			handler := requestIDMiddleware(telemetryMiddleware(requestLogMiddleware(mux, slog.New(logs)), mux))
			server := httptest.NewUnstartedServer(handler)
			server.EnableHTTP2 = false
			server.Config.WriteTimeout = 30 * time.Second
			server.Config.BaseContext = func(net.Listener) context.Context { return telemetry.Context(context.Background(), observer) }
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateClosed {
					select {
					case closed <- struct{}{}:
					default:
					}
				}
			}
			server.Start()
			defer server.Close()
			transport := &http.Transport{DisableKeepAlives: true, ForceAttemptHTTP2: false}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
			request, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/config", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("X-Request-ID", "buffered-request-42")
			response, clientErr := client.Do(request)
			if expired {
				if response != nil || !errors.Is(clientErr, io.EOF) {
					if response != nil {
						_ = response.Body.Close()
					}
					t.Fatal("expired real socket did not produce the expected client EOF")
				}
			} else {
				if clientErr != nil {
					t.Fatal("valid local HTTP response failed")
				}
				body, readErr := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if readErr != nil || response.ProtoMajor != 1 || response.StatusCode != http.StatusOK || string(body) != "{\"ok\":true}\n" {
					t.Fatal("positive local HTTP/1 response changed")
				}
				t.Logf("positive_status=%d automatic_content_length=%d transfer_encoding=%v", response.StatusCode, response.ContentLength, response.TransferEncoding)
			}
			observation := <-observed
			if observation.deadlineErr != nil || observation.requestHasDeadline || observation.contextBeforeWrite != nil || observation.contextAfterWrite != nil || observation.written != len("{\"ok\":true}\n") || observation.writeErr != nil {
				t.Fatal("fixture did not retain live request context and successful small writeJSON buffer")
			}
			select {
			case <-closed:
			case <-time.After(5 * time.Second):
				t.Fatal("real HTTP/1 connection did not complete")
			}
			if err := provider.ForceFlush(context.Background()); err != nil {
				t.Fatal(err)
			}
			spans := exporter.GetSpans()
			if len(spans) != 1 || spans[0].Name != "http.request" {
				t.Fatal("real HTTP request span missing")
			}
			spanResult := ""
			for _, attr := range spans[0].Attributes {
				if attr.Key == "result" {
					spanResult = attr.Value.AsString()
				}
			}
			message, record := responseTransportRecord(t, logs)
			if message != "request_completed" || record["status"] != int64(http.StatusOK) || record["requestID"] != "buffered-request-42" {
				t.Fatal("intended status or safe request completion correlation missing")
			}
			var metrics bytes.Buffer
			observer.WritePrometheus(&metrics)
			successCount := strings.Contains(metrics.String(), `endlessfs_operations_total{operation="http.request",role="application",result="success"} 1`)
			t.Logf("client_eof=%t intended_status=200 handler_write_succeeded=true handler_context_live=true request_result=%v span_result=%s metric_http_success=%t", errors.Is(clientErr, io.EOF), record["result"], spanResult, successCount)
			want := "success"
			if expired {
				want = "timeout"
			}
			if record["result"] != want || spanResult != want || expired && successCount {
				t.Fatalf("real socket outcome hidden after buffered write: request_result=%v span_result=%s expected=%s", record["result"], spanResult, want)
			}
		})
	}
}

func TestIntegrationResponseTransportFlushPreservesHTTP1ResponseBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, method string
		status       int
		body         string
		length       bool
		trailer      bool
		empty        bool
	}{
		{"body", http.MethodGet, http.StatusOK, "{\"ok\":true}\n", false, false, false},
		{"head", http.MethodHead, http.StatusOK, "", false, false, false},
		{"head-explicit-length", http.MethodHead, http.StatusOK, "", true, false, false},
		{"no-content", http.MethodGet, http.StatusNoContent, "", false, false, true},
		{"not-modified", http.MethodGet, http.StatusNotModified, "", false, false, true},
		{"explicit-length", http.MethodGet, http.StatusOK, "{\"ok\":true}\n", true, false, false},
		{"trailers", http.MethodGet, http.StatusOK, "{\"ok\":true}\n", false, true, false},
		{"empty", http.MethodGet, http.StatusOK, "", false, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			observer := telemetry.New(nil, nil)
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Cache-Control", "no-store")
				if test.length {
					w.Header().Set("Content-Length", "12")
				}
				if test.trailer {
					w.Header().Set("Trailer", "X-Checksum")
				}
				if test.empty {
					w.WriteHeader(test.status)
				} else {
					writeJSON(completionUnwrapper{w}, test.status, struct {
						OK bool `json:"ok"`
					}{true})
				}
				if test.trailer {
					w.Header().Set("X-Checksum", "safe-synthetic-checksum")
				}
			})
			handler := requestIDMiddleware(telemetryMiddleware(securityHeaders(mux, false, "", ""), mux))
			server := httptest.NewUnstartedServer(handler)
			server.Config.WriteTimeout = 30 * time.Second
			server.Config.BaseContext = func(net.Listener) context.Context { return telemetry.Context(context.Background(), observer) }
			server.Start()
			defer server.Close()
			client := server.Client()
			client.Timeout = 5 * time.Second
			request, err := http.NewRequest(test.method, server.URL+"/api/v1/config", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("X-Request-ID", "boundary-request-42")
			response, err := client.Do(request)
			if err != nil {
				t.Fatal("valid boundary response failed")
			}
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr != nil || response.ProtoMajor != 1 || response.StatusCode != test.status || string(body) != test.body {
				t.Fatal("body/status/HTTP1 response semantics changed")
			}
			assertSecurityHeaders(t, response.Header)
			if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Request-ID") != "boundary-request-42" {
				t.Fatal("flush changed cache/security correlation headers")
			}
			if test.length && response.ContentLength != 12 {
				t.Fatal("explicit content length changed")
			}
			if test.trailer && response.Trailer.Get("X-Checksum") != "safe-synthetic-checksum" {
				t.Fatal("declared trailer lost after explicit buffered-body flush")
			}
			if (test.method == http.MethodHead || test.status == http.StatusNoContent || test.status == http.StatusNotModified) && len(response.TransferEncoding) != 0 {
				t.Fatal("bodyless response acquired chunked body framing")
			}
			t.Logf("status=%d body_bytes=%d content_length=%d transfer_encoding=%v", response.StatusCode, len(body), response.ContentLength, response.TransferEncoding)
		})
	}
}

func TestIntegrationResponseTransportFlushSkipsRealHTTP1HijackAndPanic(t *testing.T) {
	for _, name := range []string{"hijack", "panic"} {
		t.Run(name, func(t *testing.T) {
			logs := &responseTransportLog{records: make(chan slog.Record, 4)}
			observer := telemetry.New(nil, slog.New(logs))
			defer observer.Close()
			completed := make(chan error, 1)
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, _ *http.Request) {
				if name == "panic" {
					writeJSON(w, http.StatusOK, struct {
						OK bool `json:"ok"`
					}{true})
					panic("sentinel-private-panic")
				}
				connection, buffer, err := http.NewResponseController(completionUnwrapper{w}).Hijack()
				if err != nil {
					completed <- err
					return
				}
				_, err = buffer.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
				if err == nil {
					err = buffer.Flush()
				}
				_ = connection.Close()
				completed <- err
			})
			handler := requestIDMiddleware(telemetryMiddleware(requestLogMiddleware(mux, slog.New(logs)), mux))
			server := httptest.NewUnstartedServer(handler)
			server.Config.WriteTimeout = 30 * time.Second
			server.Config.ErrorLog = log.New(io.Discard, "", 0)
			server.Config.BaseContext = func(net.Listener) context.Context { return telemetry.Context(context.Background(), observer) }
			server.Start()
			defer server.Close()
			client := server.Client()
			client.Timeout = 5 * time.Second
			response, err := client.Get(server.URL + "/api/v1/config")
			if name == "panic" {
				if response != nil || !errors.Is(err, io.EOF) {
					if response != nil {
						_ = response.Body.Close()
					}
					t.Fatal("panic unwind flushed buffered response or changed EOF behavior")
				}
				message, values := responseTransportRecord(t, logs)
				if message != "operation_completed" || values["result"] != "panic" {
					t.Fatal("panic was masked by completion flush")
				}
				return
			}
			if err != nil {
				t.Fatal("hijacked valid HTTP response failed")
			}
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr != nil || response.StatusCode != http.StatusOK || string(body) != "ok" {
				t.Fatal("completion flush changed hijacked caller-owned response")
			}
			select {
			case err := <-completed:
				if err != nil {
					t.Fatal("hijacked response did not complete")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("hijacked response hung")
			}
			message, values := responseTransportRecord(t, logs)
			if message != "request_completed" || values["result"] != "success" {
				t.Fatal("successful hijack was misclassified")
			}
		})
	}
}

func TestIntegrationResponseTransportMeasuresHTTP1FlushFraming(t *testing.T) {
	type wireResponse struct {
		wire     []byte
		response *http.Response
		body     []byte
	}
	capture := func(observed bool) wireResponse {
		observer := telemetry.New(nil, nil)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Date", "Mon, 02 Jan 2006 15:04:05 GMT")
			writeJSON(w, http.StatusOK, struct {
				OK bool `json:"ok"`
			}{true})
		})
		var handler http.Handler = securityHeaders(mux, false, "", "")
		if observed {
			handler = telemetryMiddleware(handler, mux)
		}
		handler = requestIDMiddleware(handler)
		server := httptest.NewUnstartedServer(handler)
		server.Config.WriteTimeout = 30 * time.Second
		server.Config.BaseContext = func(net.Listener) context.Context { return telemetry.Context(context.Background(), observer) }
		server.Start()
		defer server.Close()
		connection, err := net.DialTimeout("tcp", server.Listener.Addr().String(), 5*time.Second)
		if err != nil {
			t.Fatal("local wire probe could not connect")
		}
		defer connection.Close()
		if err = connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		_, err = io.WriteString(connection, "GET /api/v1/config HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\nX-Request-ID: framing-request-42\r\n\r\n")
		if err != nil {
			t.Fatal("local wire probe request failed")
		}
		wire, err := io.ReadAll(connection)
		if err != nil {
			t.Fatal("local wire probe response failed")
		}
		response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(wire)), &http.Request{Method: http.MethodGet})
		if err != nil {
			t.Fatal("local wire response malformed")
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal("local wire body incomplete")
		}
		return wireResponse{wire, response, body}
	}
	baseline, candidate := capture(false), capture(true)
	if string(baseline.body) != "{\"ok\":true}\n" || !bytes.Equal(baseline.body, candidate.body) || baseline.response.StatusCode != candidate.response.StatusCode {
		t.Fatal("framing observation changed payload/status")
	}
	if baseline.response.ContentLength != 12 || len(baseline.response.TransferEncoding) != 0 || candidate.response.ContentLength != -1 || !reflect.DeepEqual(candidate.response.TransferEncoding, []string{"chunked"}) {
		t.Fatal("expected measured automatic content length to chunked transition missing")
	}
	baselineHeaders, candidateHeaders := baseline.response.Header.Clone(), candidate.response.Header.Clone()
	baselineHeaders.Del("Content-Length")
	candidateHeaders.Del("Content-Length")
	if !reflect.DeepEqual(baselineHeaders, candidateHeaders) {
		t.Fatal("flush changed application/security/cache headers beyond accepted framing")
	}
	baselineBodyOffset := bytes.Index(baseline.wire, []byte("\r\n\r\n")) + 4
	candidateBodyOffset := bytes.Index(candidate.wire, []byte("\r\n\r\n")) + 4
	headerDelta := candidateBodyOffset - baselineBodyOffset
	framingDelta := len(candidate.wire[candidateBodyOffset:]) - len(baseline.wire[baselineBodyOffset:])
	totalDelta := len(candidate.wire) - len(baseline.wire)
	if headerDelta != 8 || framingDelta != 10 || totalDelta != 18 {
		t.Fatalf("unexpected framing delta header=%d body=%d total=%d", headerDelta, framingDelta, totalDelta)
	}
	t.Logf("actual_http1_wire_bytes baseline=%d flushed=%d header_delta=%d chunk_framing_delta=%d total_delta=%d payload_bytes=%d", len(baseline.wire), len(candidate.wire), headerDelta, framingDelta, totalDelta, len(candidate.body))
}
