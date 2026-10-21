package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

type opaqueResponseTimeout struct{ timeout bool }

func (opaqueResponseTimeout) Error() string     { panic("raw transport Error must never be inspected") }
func (err opaqueResponseTimeout) Timeout() bool { return err.timeout }
func (opaqueResponseTimeout) Temporary() bool   { return false }

func TestResponseWriteTelemetryClassifiesWithoutInspectingPrivateErrorText(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		result Result
		reason string
	}{
		{"context-deadline", fmt.Errorf("sentinel-capability: %w", context.DeadlineExceeded), Timeout, "deadline"},
		{"os-deadline", os.ErrDeadlineExceeded, Timeout, "deadline"},
		{"net-timeout", &net.OpError{Op: "sentinel-private", Net: "sentinel-network", Err: opaqueResponseTimeout{true}}, Timeout, "deadline"},
		{"cancel", fmt.Errorf("sentinel-user: %w", context.Canceled), Canceled, "canceled"},
		{"other", opaqueResponseTimeout{false}, Unavailable, "transport_failure"},
	} {
		t.Run(test.name, func(t *testing.T) {
			logs := &diagnosticLogHandler{records: make(chan slog.Record, 1)}
			observer := New(nil, slog.New(logs))
			defer observer.Close()
			ctx, activity := Start(Context(context.Background(), observer), HTTP, Application)
			if got := RecordResponseWrite(ctx, test.err); got != test.result {
				t.Fatalf("transport class=%v want=%v", got, test.result)
			}
			RecordProblem(ctx, Diagnostic(domain.ErrInvalid, RequestValidation, Validation))
			activity.EndResult(Success)
			values := nextDiagnosticLog(t, logs)
			if values["stage"] != "response_write" || values["reason"] != test.reason || values["result"] != resultNames[test.result] || strings.Contains(fmt.Sprint(values), "sentinel") {
				t.Fatal("transport diagnostics unsafe, unhelpful or masked by public problem")
			}
			if RecordResponseWrite(context.Background(), test.err) != test.result || RecordResponseWrite(context.TODO(), test.err) != test.result {
				t.Fatal("disabled observation changed classification")
			}
		})
	}
	if RecordResponseWrite(context.Background(), nil) != Success {
		t.Fatal("successful write changed")
	}
}

func TestResponseWriteTelemetryKeepsMetricsClosedAcrossDistinctPrivateErrors(t *testing.T) {
	observer := New(nil, nil)
	for index := range 100 {
		ctx, activity := Start(WithRequestID(Context(context.Background(), observer), fmt.Sprintf("request-%d", index)), HTTP, Application)
		RecordResponseWrite(ctx, fmt.Errorf("sentinel-user-%d/path/file capability=secret-%d", index, index))
		activity.EndResult(Success)
	}
	var output bytes.Buffer
	observer.WritePrometheus(&output)
	if !strings.Contains(output.String(), `endlessfs_operations_total{operation="http.request",role="application",result="unavailable"} 100`) {
		t.Fatal("transport accounting incomplete")
	}
	for _, forbidden := range []string{"sentinel", "requestID=", "request-", "stage=", "reason="} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatalf("unbounded/private transport metric %q", forbidden)
		}
	}
}

func TestResponseWriteTelemetrySafeTraceUsesOnlyClosedTransportCodes(t *testing.T) {
	exporter := &recordingExporter{}
	observer := New(nil, nil)
	provider := TraceProvider(exporter, observer, "transport-test", 1)
	defer func() { _ = provider.Shutdown(context.Background()) }()
	ctx, activity := Start(Context(context.Background(), observer), HTTP, Application)
	activity.Route("POST /api/v1/uploads/plan/sizes")
	RecordResponseWrite(ctx, opaqueResponseTimeout{true})
	activity.EndResult(Success)
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	if len(exporter.spans) != 1 || observer.ExportDropped.Load() != 0 {
		t.Fatal("closed transport trace rejected")
	}
	values := map[string]string{}
	for _, attr := range exporter.spans[0].Attributes() {
		values[string(attr.Key)] = attr.Value.AsString()
	}
	if values["stage"] != "response_write" || values["reason"] != "deadline" || values["result"] != "timeout" || len(values) != 6 {
		t.Fatal("transport trace lost closed outcome or retained extra fields")
	}
}
