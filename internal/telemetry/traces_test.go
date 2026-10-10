package telemetry

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type recordingExporter struct {
	mu      sync.Mutex
	spans   []sdktrace.ReadOnlySpan
	started chan struct{}
	blocked bool
	once    sync.Once
	failure bool
}

func (exporter *recordingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if exporter.blocked {
		exporter.once.Do(func() { close(exporter.started) })
		<-ctx.Done()
		return ctx.Err()
	}
	if exporter.failure {
		return errors.New("sentinel-secret-capability-url")
	}
	exporter.mu.Lock()
	exporter.spans = append(exporter.spans, spans...)
	exporter.mu.Unlock()
	return nil
}
func (*recordingExporter) Shutdown(context.Context) error { return nil }

func TestTraceHierarchyPrivacyAndGlobalSDKIsolation(t *testing.T) {
	exporter := &recordingExporter{}
	observer := New(nil, nil)
	provider := TraceProvider(exporter, observer, "v0.7.3", 1)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	if otel.GetTracerProvider() == provider {
		t.Fatal("application installed global opaque SDK instrumentation")
	}
	ctx, root := Start(Context(context.Background(), observer), HTTP, Application)
	root.Route("GET /api/v1/public/shares/{token}")
	_, child := Start(ctx, ProviderGet, State)
	child.End(domain.WrapError(domain.ErrorUnauthorized, "safe", errors.New("sentinel-secret-path-and-key")))
	root.End(nil)
	// A future developer accidentally setting a raw attribute fails closed.
	_, unsafe := provider.Tracer(instrumentationName).Start(ctx, "provider.get")
	unsafe.SetAttributes(attribute.String("operation", "provider.get"), attribute.String("role", "state"), attribute.String("result", "success"), attribute.String("path", "sentinel-private-file"))
	unsafe.End()
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	if len(exporter.spans) != 2 || observer.ExportDropped.Load() != 1 {
		t.Fatalf("safe spans=%d dropped=%d", len(exporter.spans), observer.ExportDropped.Load())
	}
	var parent, descendant sdktrace.ReadOnlySpan
	for _, span := range exporter.spans {
		if span.Name() == "http.request" {
			parent = span
		} else {
			descendant = span
		}
		for _, value := range span.Attributes() {
			if bytes.Contains([]byte(value.Value.AsString()), []byte("sentinel")) {
				t.Fatal("unsafe span attribute exported")
			}
		}
	}
	if descendant.Parent().SpanID() != parent.SpanContext().SpanID() || descendant.SpanContext().TraceID() != parent.SpanContext().TraceID() {
		t.Fatal("phase hierarchy was lost")
	}
}

func TestBlockedExporterDropsSamplesWithoutBackpressuringOperations(t *testing.T) {
	exporter := &recordingExporter{blocked: true, started: make(chan struct{})}
	observer := New(nil, nil)
	provider := TraceProvider(exporter, observer, "test", 1)
	ctx := Context(context.Background(), observer)
	for range exportBatchSize {
		_, activity := Start(ctx, DomainCommit, State)
		activity.End(nil)
	}
	select {
	case <-exporter.started:
	case <-time.After(5 * time.Second):
		t.Fatal("export did not start")
	}
	// The exporter is now definitely blocked. Recording still completes inline.
	for range 1000 {
		_, activity := Start(ctx, DomainCommit, State)
		activity.End(nil)
	}
	if observer.ExportDropped.Load() == 0 {
		t.Fatal("full queue did not report loss")
	}
	observer.mu.Lock()
	completed := observer.metrics[DomainCommit][State].Results[Success].Count
	observer.mu.Unlock()
	if completed != 1064 {
		t.Fatalf("collector changed local completion accounting: %d", completed)
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := provider.Shutdown(shutdown); err != nil {
		t.Fatalf("export shutdown did not respect cancellation: %v", err)
	}
}

func TestExportFailureIsMeasuredWithoutLeakingError(t *testing.T) {
	observer := New(nil, nil)
	provider := TraceProvider(&recordingExporter{failure: true}, observer, "private/path", 1)
	defer func() { _ = provider.Shutdown(context.Background()) }()
	_, activity := Start(Context(context.Background(), observer), UploadCompletion, Application)
	activity.End(nil)
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if observer.ExportFailed.Load() != 1 {
		t.Fatal("failed sample was not measured")
	}
	var output bytes.Buffer
	observer.WritePrometheus(&output)
	if bytes.Contains(output.Bytes(), []byte("sentinel")) {
		t.Fatal("export error entered metrics")
	}
	if safeVersion("private/path") != "unknown" {
		t.Fatal("unsafe release identity retained")
	}
}

func BenchmarkOperationalTelemetry(b *testing.B) {
	for _, mode := range []string{"disabled", "metrics", "sampled-traces"} {
		b.Run(mode, func(b *testing.B) {
			ctx := context.Background()
			if mode != "disabled" {
				observer := New(nil, nil)
				ctx = Context(ctx, observer)
				if mode == "sampled-traces" {
					provider := TraceProvider(&recordingExporter{failure: true}, observer, "bench", .1)
					b.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_, activity := Start(ctx, ProviderGet, State)
				activity.Bytes(4096, 0)
				activity.End(nil)
			}
		})
	}
}
