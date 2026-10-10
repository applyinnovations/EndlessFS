package otlpexport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/telemetry"
	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestOfficialOTLPExporterSendsSafeProtobufAndDoesNotRetryRejection(t *testing.T) {
	var attempts atomic.Uint64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.URL.Path != "/v1/traces" || r.Header.Get("Authorization") != "" {
			t.Error("unexpected endpoint or environment credentials")
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Error(err)
		}
		var request collector.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &request); err != nil {
			t.Error(err)
		}
		if len(request.ResourceSpans) != 1 || len(request.ResourceSpans[0].ScopeSpans) != 1 {
			t.Error("missing OTLP spans")
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=sentinel-secret")
	exporter, err := New(context.Background(), server.URL+"/v1/traces")
	if err != nil {
		t.Fatal(err)
	}
	observer := telemetry.New(nil, nil)
	provider := telemetry.TraceProvider(exporter, observer, "test", 1)
	defer func() { _ = provider.Shutdown(context.Background()) }()
	_, activity := telemetry.Start(telemetry.Context(context.Background(), observer), telemetry.UploadCompletion, telemetry.Application)
	activity.End(nil)
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 1 || observer.ExportFailed.Load() != 1 {
		t.Fatalf("attempts=%d failures=%d", attempts.Load(), observer.ExportFailed.Load())
	}
}
