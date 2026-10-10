package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type alteredSpan struct {
	sdktrace.ReadOnlySpan
	attrs       []attribute.KeyValue
	scope       *instrumentation.Scope
	res         *resource.Resource
	name        string
	status      *sdktrace.Status
	spanContext *trace.SpanContext
}

func (span alteredSpan) Attributes() []attribute.KeyValue {
	if span.attrs != nil {
		return span.attrs
	}
	return span.ReadOnlySpan.Attributes()
}
func (span alteredSpan) InstrumentationScope() instrumentation.Scope {
	if span.scope != nil {
		return *span.scope
	}
	return span.ReadOnlySpan.InstrumentationScope()
}
func (span alteredSpan) Resource() *resource.Resource {
	if span.res != nil {
		return span.res
	}
	return span.ReadOnlySpan.Resource()
}
func (span alteredSpan) Name() string {
	if span.name != "" {
		return span.name
	}
	return span.ReadOnlySpan.Name()
}
func (span alteredSpan) Status() sdktrace.Status {
	if span.status != nil {
		return *span.status
	}
	return span.ReadOnlySpan.Status()
}
func (span alteredSpan) SpanContext() trace.SpanContext {
	if span.spanContext != nil {
		return *span.spanContext
	}
	return span.ReadOnlySpan.SpanContext()
}

func TestTraceExportRejectsEveryPrivateMetadataSurface(t *testing.T) {
	exporter := &recordingExporter{}
	observer := New(nil, nil)
	provider := TraceProvider(exporter, observer, "test", 1)
	defer func() { _ = provider.Shutdown(context.Background()) }()
	_, activity := Start(Context(context.Background(), observer), HTTP, Application)
	activity.Route("GET /api/v1/config")
	activity.End(nil)
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	exporter.mu.Lock()
	original := exporter.spans[0]
	exporter.mu.Unlock()
	unsafeState, err := trace.ParseTraceState("vendor=sentinel-private-user")
	if err != nil {
		t.Fatal(err)
	}
	spanContext := original.SpanContext().WithTraceState(unsafeState)
	tests := []alteredSpan{
		{ReadOnlySpan: original, scope: &instrumentation.Scope{Name: "sentinel-user-file"}},
		{ReadOnlySpan: original, scope: &instrumentation.Scope{Name: instrumentationName, Version: "sentinel-secret"}},
		{ReadOnlySpan: original, scope: &instrumentation.Scope{Name: instrumentationName, SchemaURL: "https://sentinel-private"}},
		{ReadOnlySpan: original, res: resource.NewWithAttributes("https://sentinel-private", attribute.String("service.name", "endlessfs"), attribute.String("service.version", "test"))},
		{ReadOnlySpan: original, res: resource.NewSchemaless(attribute.String("service.name", "endlessfs"), attribute.String("service.version", "test"), attribute.String("path", "sentinel-file"))},
		{ReadOnlySpan: original, res: resource.NewSchemaless(attribute.Int("service.name", 1), attribute.String("service.version", "test"))},
		{ReadOnlySpan: original, res: resource.NewSchemaless(attribute.String("service.name", "sentinel-user"), attribute.String("service.version", "test"))},
		{ReadOnlySpan: original, res: resource.NewSchemaless(attribute.String("service.name", "endlessfs"), attribute.String("service.version", "sentinel/private"))},
		{ReadOnlySpan: original, name: "sentinel-user-filename"},
		{ReadOnlySpan: original, status: &sdktrace.Status{Code: codes.Error, Description: "sentinel-secret-error"}},
		{ReadOnlySpan: original, spanContext: &spanContext},
		{ReadOnlySpan: original, attrs: []attribute.KeyValue{attribute.Int("operation", 1)}},
		{ReadOnlySpan: original, attrs: []attribute.KeyValue{attribute.String("operation", "http.request"), attribute.String("role", "application"), attribute.String("result", "success"), attribute.String("route", "/sentinel-private-file")}},
	}
	for index, span := range tests {
		if safeSpan(span) {
			t.Errorf("unsafe span %d admitted", index)
		}
	}
	if !safeSpan(original) {
		t.Fatal("valid trace was denied")
	}
}
