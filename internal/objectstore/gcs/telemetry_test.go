package gcs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/api/option"
)

func TestContractGCSWireTelemetryPreservesEveryRequestAndResponseClass(t *testing.T) {
	requests := []struct{ method, target, header, value, operation string }{
		{"GET", "/storage/v1/b/b/o/key", "", "", "head"},
		{"GET", "/storage/v1/b/b/o/key?alt=media", "", "", "read"},
		{"GET", "/storage/v1/b/b/o", "", "", "list"},
		{"POST", "/upload/storage/v1/b/b/o", "", "", "put"},
		{"DELETE", "/storage/v1/b/b/o/key", "", "", "delete"},
		{"POST", "/storage/v1/b/b/o/key/rewriteTo/b/b/o/new", "", "", "copy"},
		{"POST", "/bucket/key", "x-goog-resumable", "start", "upload.begin"},
		{"PUT", "/upload/session", "Content-Range", "bytes */3", "upload.status"},
		{"DELETE", "/upload/session", "", "", "upload.abort"},
		{"PUT", "/upload/session", "Content-Range", "bytes 0-2/3", "upload.data"},
		{"GET", "/bucket/key?X-Goog-Signature=sentinel", "", "", "download.data"},
		{"PATCH", "/unknown", "", "", "unknown"},
	}
	results := []struct {
		status int
		result string
	}{{200, "success"}, {401, "denied"}, {403, "denied"}, {404, "not_found"}, {409, "conflict"}, {412, "conflict"}, {408, "timeout"}, {504, "timeout"}, {429, "unavailable"}, {503, "unavailable"}, {400, "invalid"}}
	for _, shape := range requests {
		for _, outcome := range results {
			t.Run(fmt.Sprintf("%s/%d", shape.operation, outcome.status), func(t *testing.T) {
				observer := telemetry.New(nil, nil)
				ctx, parent := telemetry.Start(telemetry.Context(context.Background(), observer), telemetry.ProviderGet, telemetry.Files)
				defer parent.End(nil)
				request, err := http.NewRequestWithContext(ctx, shape.method, "https://storage.example"+shape.target, strings.NewReader("abc"))
				if err != nil {
					t.Fatal(err)
				}
				if shape.header != "" {
					request.Header.Set(shape.header, shape.value)
				}
				calls := 0
				original := &http.Response{StatusCode: outcome.status, Header: http.Header{"X-Test": {"same"}}, Body: io.NopCloser(strings.NewReader("xyz"))}
				transport := observedTransport{base: roundTripperFunc(func(actual *http.Request) (*http.Response, error) {
					calls++
					if actual.Method != request.Method || actual.URL.String() != request.URL.String() || actual.Body != request.Body || actual.ContentLength != request.ContentLength || actual.Header.Get(shape.header) != shape.value {
						t.Fatal("wire request changed")
					}
					return original, nil
				})}
				response, err := transport.RoundTrip(request)
				if err != nil || response != original || calls != 1 {
					t.Fatalf("response/calls changed: %v/%d", err, calls)
				}
				body, err := io.ReadAll(response.Body)
				if err != nil || string(body) != "xyz" || response.Header.Get("X-Test") != "same" {
					t.Fatal("response changed")
				}
				if err := response.Body.Close(); err != nil {
					t.Fatal(err)
				}
				if err := response.Body.Close(); err != nil {
					t.Fatal(err)
				}
				var metrics bytes.Buffer
				observer.WritePrometheus(&metrics)
				labels := fmt.Sprintf(`operation="provider.http.%s",role="files"`, shape.operation)
				for _, want := range []string{fmt.Sprintf(`endlessfs_operations_total{%s,result="%s"} 1`, labels, outcome.result), fmt.Sprintf(`endlessfs_operations_active{%s} 0`, labels), fmt.Sprintf(`endlessfs_operation_bytes_total{%s,direction="read"} 3`, labels), fmt.Sprintf(`endlessfs_operation_bytes_total{%s,direction="written"} 3`, labels)} {
					if !strings.Contains(metrics.String(), want) {
						t.Errorf("missing %s", want)
					}
				}
				if strings.Contains(metrics.String(), "sentinel") {
					t.Fatal("capability leaked")
				}
			})
		}
	}
}

type telemetryFaultBody struct{ readError, closeError error }

func (body telemetryFaultBody) Read([]byte) (int, error) { return 0, body.readError }
func (body telemetryFaultBody) Close() error             { return body.closeError }

type telemetryClosingTransport struct{ closed bool }

func (transport *telemetryClosingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, context.Canceled
}
func (transport *telemetryClosingTransport) CloseIdleConnections() { transport.closed = true }

func TestContractGCSWireTelemetryPreservesTransportAndStreamFailures(t *testing.T) {
	for _, failure := range []string{"transport", "read", "close", "disabled"} {
		t.Run(failure, func(t *testing.T) {
			observer := telemetry.New(nil, nil)
			ctx := telemetry.Context(context.Background(), observer)
			if failure == "disabled" {
				ctx = context.Background()
			}
			request, _ := http.NewRequestWithContext(ctx, "GET", "https://storage.example/storage/v1/b/b/o/key", nil)
			fault := errors.New("sentinel-private-error")
			body := telemetryFaultBody{readError: io.EOF}
			if failure == "read" {
				body.readError = fault
			}
			if failure == "close" {
				body.closeError = fault
			}
			original := &http.Response{StatusCode: 200, Body: body}
			transport := observedTransport{base: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				if failure == "transport" {
					return nil, fault
				}
				return original, nil
			})}
			response, err := transport.RoundTrip(request)
			if failure == "transport" {
				if !errors.Is(err, fault) || response != nil {
					t.Fatal("transport failure changed")
				}
			} else {
				if err != nil || response != original {
					t.Fatal("response changed")
				}
				_, readErr := io.ReadAll(response.Body)
				closeErr := response.Body.Close()
				_ = response.Body.Close()
				if failure == "read" && !errors.Is(readErr, fault) || failure == "close" && !errors.Is(closeErr, fault) {
					t.Fatal("stream failure changed")
				}
				if failure == "disabled" && response.Body != body {
					t.Fatal("disabled observation wrapped stream")
				}
			}
			var metrics bytes.Buffer
			observer.WritePrometheus(&metrics)
			want := `endlessfs_operations_total{operation="provider.http.head",role="application",result="internal"} 1`
			if failure != "disabled" && !strings.Contains(metrics.String(), want) {
				t.Errorf("missing %s", want)
			}
			if strings.Contains(metrics.String(), "sentinel") {
				t.Fatal("error leaked")
			}
		})
	}
	closing := &telemetryClosingTransport{}
	observedTransport{base: closing}.CloseIdleConnections()
	if !closing.closed {
		t.Fatal("idle connection cleanup not delegated")
	}
	observedTransport{base: roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, context.Canceled })}.CloseIdleConnections()
}

func TestContractGCSWireTelemetryRecordsActualSDKRetryWithoutKeysOrURLs(t *testing.T) {
	var attempts atomic.Uint64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			http.Error(w, "sentinel-provider-secret", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"sentinel-provider-key","generation":"1","size":"0","crc32c":"AAAAAA=="}`))
	}))
	defer server.Close()
	clientHTTP := server.Client()
	clientHTTP.Transport = observedTransport{base: clientHTTP.Transport}
	client, err := storage.NewClient(context.Background(), option.WithEndpoint(server.URL+"/storage/v1/"), option.WithHTTPClient(clientHTTP), option.WithoutAuthentication(), storage.WithDisabledClientMetrics(), storage.WithJSONReads())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetRetry(storage.WithBackoff(gax.Backoff{Initial: time.Nanosecond, Max: time.Nanosecond, Multiplier: 1}), storage.WithPolicy(storage.RetryAlways))
	backend, err := New(client, "bucket")
	if err != nil {
		t.Fatal(err)
	}
	observer := telemetry.New(nil, nil)
	observed := objectstore.Observe(backend, telemetry.State)
	if _, err := observed.Head(telemetry.Context(context.Background(), observer), objectstore.MustKey("sentinel-provider-key")); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("SDK attempts=%d", attempts.Load())
	}
	var metrics bytes.Buffer
	observer.WritePrometheus(&metrics)
	for _, want := range []string{
		`endlessfs_operations_total{operation="provider.head",role="state",result="success"} 1`,
		`endlessfs_operations_total{operation="provider.http.head",role="state",result="success"} 1`,
		`endlessfs_operations_total{operation="provider.http.head",role="state",result="unavailable"} 1`,
	} {
		if !strings.Contains(metrics.String(), want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(metrics.String(), "sentinel") {
		t.Fatal("provider keys/URLs/errors entered metrics")
	}
}
