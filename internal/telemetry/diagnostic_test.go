package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type diagnosticLogHandler struct{ records chan slog.Record }

func (*diagnosticLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (handler *diagnosticLogHandler) Handle(_ context.Context, record slog.Record) error {
	handler.records <- record.Clone()
	return nil
}
func (handler *diagnosticLogHandler) WithAttrs([]slog.Attr) slog.Handler { return handler }
func (handler *diagnosticLogHandler) WithGroup(string) slog.Handler      { return handler }

func nextDiagnosticLog(t *testing.T, handler *diagnosticLogHandler) map[string]any {
	t.Helper()
	select {
	case record := <-handler.records:
		if record.Message != "operation_completed" {
			t.Fatalf("unexpected lifecycle event %q", record.Message)
		}
		values := map[string]any{}
		record.Attrs(func(value slog.Attr) bool { values[value.Key] = value.Value.Any(); return true })
		return values
	case <-time.After(5 * time.Second):
		t.Fatal("diagnostic lifecycle event was not delivered")
		return nil
	}
}

func TestDiagnosticPreservesDomainAndCancellationSemantics(t *testing.T) {
	if Diagnostic(nil, ProjectionBuild, PackCapacity) != nil {
		t.Fatal("diagnostics changed a successful operation")
	}
	for _, test := range []struct {
		cause  error
		result Result
	}{
		{domain.ErrInvalid, Invalid},
		{context.DeadlineExceeded, Timeout},
		{context.Canceled, Canceled},
	} {
		original := domain.WrapError(domain.ErrorInvalid, "sentinel-private-filename", test.cause)
		annotated := fmt.Errorf("caller: %w", Diagnostic(original, NamespaceRead, CorruptMetadata))
		if !errors.Is(annotated, test.cause) || domain.KindOf(annotated) != domain.ErrorInvalid {
			t.Fatal("diagnostic wrapping changed error identity or public domain classification")
		}
		var recovered *domain.Error
		if !errors.As(annotated, &recovered) || recovered != original {
			t.Fatal("diagnostic wrapping lost the original typed domain error")
		}
		if result := Classify(annotated); result != test.result {
			t.Errorf("wrapped context/domain result=%v, want %v", result, test.result)
		}
	}
}

func TestDiagnosticLifecycleIsUsefulBoundedAndRedacted(t *testing.T) {
	handler := &diagnosticLogHandler{records: make(chan slog.Record, 1)}
	observer := New(nil, slog.New(handler))
	defer observer.Close()
	observer.SetVersion("v0.8.0-corrective.12908dd")
	traceID, err := trace.TraceIDFromHex("f639a73ef118d02f3de71db54f2e9165")
	if err != nil {
		t.Fatal(err)
	}
	spanID, err := trace.SpanIDFromHex("0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	ctx := trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID}))
	ctx = WithRequestID(Context(ctx, observer), "evkrwXFtnjgltCIY")
	_, activity := Start(ctx, UploadSizes, Application)
	private := strings.Repeat("sentinel-user/path/file.txt provider-key credential capability=https://private/?secret body ", 1024)
	activity.End(Diagnostic(domain.WrapError(domain.ErrorInvalid, private, errors.New(private)), ProjectionBuild, PackCapacity))
	values := nextDiagnosticLog(t, handler)
	for key, want := range map[string]any{
		"operation": "upload.plan.sizes", "role": "application", "result": "invalid",
		"stage": "projection_build", "reason": "pack_capacity", "build": "v0.8.0-corrective.12908dd",
		"traceID": traceID.String(), "requestID": "evkrwXFtnjgltCIY",
	} {
		if values[key] != want {
			t.Errorf("log %s=%v, want %v", key, values[key], want)
		}
	}
	if seconds, ok := values["durationSeconds"].(float64); !ok || seconds < 0 {
		t.Fatal("operation duration is missing or invalid")
	}
	allowed := map[string]bool{"operation": true, "role": true, "result": true, "stage": true, "reason": true, "build": true, "traceID": true, "requestID": true, "durationSeconds": true}
	for key := range values {
		if !allowed[key] {
			t.Errorf("unreviewed lifecycle field %s", key)
		}
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("sentinel")) || len(encoded) > 1024 {
		t.Fatal("raw error data or unbounded error content entered lifecycle logs")
	}
	var metrics bytes.Buffer
	observer.WritePrometheus(&metrics)
	for _, forbidden := range []string{"sentinel", "evkrwXFtnjgltCIY", traceID.String(), "requestID=", "traceID="} {
		if strings.Contains(metrics.String(), forbidden) {
			t.Errorf("correlation or private content entered metrics: %s", forbidden)
		}
	}
}

func TestDiagnosticStagesReasonsAndCorrelationStayClosed(t *testing.T) {
	handler := &diagnosticLogHandler{records: make(chan slog.Record, 1)}
	observer := New(nil, slog.New(handler))
	defer observer.Close()
	observer.SetVersion("sentinel/private-build")
	allowedStages := map[any]bool{"unknown": true, "request_validation": true, "namespace_read": true, "projection_build": true, "projection_publish": true, "target_lookup": true, "snapshot_check": true, "namespace_publish": true, "response_write": true, "metadata_admission": true, "metadata_materialize": true}
	allowedReasons := map[any]bool{"unknown": true, "validation": true, "corrupt_metadata": true, "pack_capacity": true, "pack_collision": true, "snapshot_stale": true, "provider_failure": true, "canceled": true, "internal": true, "deadline": true, "transport_failure": true}
	var baseline map[string]bool
	for value := range 256 {
		ctx := WithRequestID(Context(context.Background(), observer), fmt.Sprintf("correlation-%d", value))
		_, activity := Start(ctx, UploadSizes, Application)
		activity.End(Diagnostic(domain.ErrInvalid, Stage(value), Reason(value)))
		values := nextDiagnosticLog(t, handler)
		if !allowedStages[values["stage"]] || !allowedReasons[values["reason"]] {
			t.Fatalf("unknown diagnostic enum escaped its closed vocabulary: stage=%v reason=%v", values["stage"], values["reason"])
		}
		if values["build"] != "unknown" {
			t.Fatal("unvalidated build identity entered logs")
		}
		var output bytes.Buffer
		observer.WritePrometheus(&output)
		series := map[string]bool{}
		for _, line := range strings.Split(output.String(), "\n") {
			if index := strings.IndexByte(line, '}'); index >= 0 {
				series[line[:index+1]] = true
			}
		}
		if value == 0 {
			baseline = series
		} else if !reflect.DeepEqual(series, baseline) {
			t.Fatal("diagnostic enums or distinct request correlations expanded metric cardinality")
		}
	}
	for _, invalid := range []string{"", "sentinel/private", "sentinel?capability=secret", "sentinel\nforged", strings.Repeat("s", 65), "sentinelé"} {
		_, activity := Start(WithRequestID(Context(context.Background(), observer), invalid), UploadSizes, Application)
		activity.End(domain.ErrInvalid)
		values := nextDiagnosticLog(t, handler)
		if value := values["requestID"]; value != nil && value != "" {
			t.Errorf("unsafe correlation ID was retained: %q", value)
		}
	}
	for _, valid := range []string{"r", "request._-012345", strings.Repeat("a", 64)} {
		_, activity := Start(WithRequestID(Context(context.Background(), observer), valid), UploadSizes, Application)
		activity.End(nil)
		if values := nextDiagnosticLog(t, handler); values["requestID"] != valid {
			t.Error("valid bounded request correlation was lost")
		}
	}
}

func TestDiagnosticTraceAllowlistIncludesSafeCausesButRejectsCorrelationAndArbitraryCauses(t *testing.T) {
	exporter := &recordingExporter{}
	observer := New(nil, nil)
	provider := TraceProvider(exporter, observer, "v0.8.0", 1)
	defer func() { _ = provider.Shutdown(context.Background()) }()
	ctx, activity := Start(WithRequestID(Context(context.Background(), observer), "request-log-only"), HTTP, Application)
	activity.Route("POST /api/v1/uploads/plan/sizes")
	activity.End(Diagnostic(domain.WrapError(domain.ErrorInvalid, "sentinel-private", errors.New("sentinel-provider-key")), ProjectionBuild, PackCollision))
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	if len(exporter.spans) != 1 {
		t.Fatalf("safe six-attribute HTTP diagnostic was dropped: spans=%d dropped=%d", len(exporter.spans), observer.ExportDropped.Load())
	}
	span := exporter.spans[0]
	values := map[string]string{}
	for _, value := range span.Attributes() {
		values[string(value.Key)] = value.Value.AsString()
	}
	if values["stage"] != "projection_build" || values["reason"] != "pack_collision" || values["route"] != "POST /api/v1/uploads/plan/sizes" {
		t.Fatal("trace lost stage, reason, or HTTP route")
	}
	if values["requestID"] != "" || strings.Contains(fmt.Sprint(values), "sentinel") {
		t.Fatal("private values entered trace attributes")
	}
	for _, unsafe := range []attribute.KeyValue{
		attribute.String("stage", "sentinel-private-path"), attribute.String("reason", "sentinel-provider-error"),
		attribute.String("requestID", "request-log-only"), attribute.Int("stage", 1), attribute.Int("reason", 1),
	} {
		attrs := make([]attribute.KeyValue, 0, len(span.Attributes())+1)
		for _, value := range span.Attributes() {
			if value.Key != unsafe.Key {
				attrs = append(attrs, value)
			}
		}
		attrs = append(attrs, unsafe)
		if safeSpan(alteredSpan{ReadOnlySpan: span, attrs: attrs}) {
			t.Errorf("unsafe diagnostic span attribute accepted: %s", unsafe.Key)
		}
	}
	for _, key := range []string{"stage", "reason"} {
		// Validate every retained attribute, not only the last value for a key:
		// an earlier private value must never be masked by a later safe duplicate.
		attrs := append([]attribute.KeyValue{attribute.String(key, "sentinel-private-error")}, span.Attributes()...)
		if safeSpan(alteredSpan{ReadOnlySpan: span, attrs: attrs}) {
			t.Errorf("private duplicate %s attribute was hidden by a later safe value", key)
		}
	}
	RecordProblem(ctx, nil) // Successful/non-HTTP callers need no special guard.
}

func TestDiagnosticRecordingRemainsIndependentOfBlockedCollectors(t *testing.T) {
	writer := &blockedLogWriter{entered: make(chan struct{}), release: make(chan struct{})}
	observer := New(nil, slog.New(slog.NewJSONHandler(writer, nil)))
	defer observer.Close()
	defer close(writer.release)
	exporter := &recordingExporter{blocked: true, started: make(chan struct{})}
	provider := TraceProvider(exporter, observer, "test", 1)
	defer func() { _ = provider.Shutdown(context.Background()) }()
	ctx := WithRequestID(Context(context.Background(), observer), "blocked-collector")
	for range exportBatchSize {
		_, activity := Start(ctx, UploadSizes, Application)
		activity.End(Diagnostic(domain.ErrInvalid, ProjectionBuild, PackCapacity))
	}
	for _, started := range []<-chan struct{}{writer.entered, exporter.started} {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("test collector did not enter its deterministic blocked state")
		}
	}
	// Neither blocked sink may affect local completion or require raw errors.
	for range 1000 {
		_, activity := Start(ctx, UploadSizes, Application)
		activity.End(Diagnostic(domain.ErrInvalid, ProjectionBuild, PackCapacity))
	}
	var metrics bytes.Buffer
	observer.WritePrometheus(&metrics)
	if !strings.Contains(metrics.String(), `endlessfs_operations_total{operation="upload.plan.sizes",role="application",result="invalid"} 1064`) {
		t.Fatal("collector backpressure changed local diagnostic completion accounting")
	}
	if observer.LogDropped.Load() == 0 || observer.ExportDropped.Load() == 0 {
		t.Fatal("bounded collector queues failed to report dropped samples")
	}
	RecordProblem(context.Background(), Diagnostic(domain.ErrInvalid, RequestValidation, Validation))
}

func TestDiagnosticProblemContextIsIsolatedAcrossConcurrentHTTPRequests(t *testing.T) {
	handler := &diagnosticLogHandler{records: make(chan slog.Record, 64)}
	observer := New(nil, slog.New(handler))
	defer observer.Close()
	var requests sync.WaitGroup
	for index := range 16 {
		requests.Go(func() {
			ctx := WithRequestID(Context(context.Background(), observer), fmt.Sprintf("isolated-%d", index))
			ctx, request := Start(ctx, HTTP, Application)
			ctx, child := Start(ctx, UploadSizes, Application)
			stage, reason := ProjectionBuild, PackCollision
			if index%2 == 1 {
				stage, reason = RequestValidation, Validation
			}
			RecordProblem(ctx, Diagnostic(domain.ErrInvalid, stage, reason))
			child.End(nil)
			request.EndResult(Invalid)
		})
	}
	requests.Wait()
	seen := map[string]bool{}
	for range 32 {
		values := nextDiagnosticLog(t, handler)
		if values["operation"] != "http.request" {
			if values["result"] != "success" {
				t.Fatal("HTTP problem annotation changed successful nested operation accounting")
			}
			continue
		}
		id, ok := values["requestID"].(string)
		if !ok || seen[id] {
			t.Fatal("concurrent request diagnostic correlation is missing or duplicated")
		}
		seen[id] = true
		var index int
		if _, err := fmt.Sscanf(id, "isolated-%d", &index); err != nil {
			t.Fatal(err)
		}
		stage, reason := "projection_build", "pack_collision"
		if index%2 == 1 {
			stage, reason = "request_validation", "validation"
		}
		if values["stage"] != stage || values["reason"] != reason || values["result"] != "invalid" {
			t.Fatal("diagnostic leaked across requests or was lost through a nested context")
		}
	}
	if len(seen) != 16 {
		t.Fatal("concurrent HTTP diagnostics are incomplete")
	}
}
