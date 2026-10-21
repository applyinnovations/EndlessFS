package storageformat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestDomainPagePackAdmissionTelemetrySeparatesWaitingFromDecodeAndCancellation(t *testing.T) {
	if cap(domainPagePackDecodeSlot) != 1 {
		t.Fatal("process-wide expansion memory admission is no longer single-slot")
	}
	pack := testDomainPagePack(t)
	wire, err := EncodeDomainPagePack(pack)
	if err != nil {
		t.Fatal(err)
	}
	for _, canceled := range []bool{false, true} {
		observer := telemetry.New(nil, nil)
		base, cancel := context.WithCancel(telemetry.Context(context.Background(), observer))
		waiting := &packAdmissionWaitingContext{Context: base, ready: make(chan struct{})}
		domainPagePackDecodeSlot <- struct{}{}
		result := make(chan error, 1)
		go func() {
			_, err := DecodeDomainPagePack(waiting, wire, pack.DomainID, pack.Kind, pack.PackID)
			result <- err
		}()
		<-waiting.ready
		var active bytes.Buffer
		observer.WritePrometheus(&active)
		if !strings.Contains(active.String(), `endlessfs_operations_active{operation="metadata.pack.admission",role="state"} 1`) {
			<-domainPagePackDecodeSlot
			cancel()
			<-result
			t.Fatal("queued admission is invisible in telemetry")
		}
		if strings.Contains(active.String(), `operation="metadata.pack.decode",role="state",result=`) {
			t.Fatal("queued work decoded before expansion admission")
		}
		if canceled {
			cancel()
		} else {
			<-domainPagePackDecodeSlot
		}
		err := <-result
		if canceled {
			if !errors.Is(err, context.Canceled) || !errors.Is(err, domain.ErrUnavailable) {
				t.Fatal("admission cancellation semantics changed")
			}
			if len(domainPagePackDecodeSlot) != 1 {
				t.Fatal("canceled waiter consumed another operation's slot")
			}
			<-domainPagePackDecodeSlot
		} else if err != nil {
			t.Fatal(err)
		}
		cancel()
		var output bytes.Buffer
		observer.WritePrometheus(&output)
		resultName := "success"
		if canceled {
			resultName = "canceled"
		}
		if !strings.Contains(output.String(), `endlessfs_operations_total{operation="metadata.pack.admission",role="state",result="`+resultName+`"} 1`) || !strings.Contains(output.String(), `endlessfs_operations_active{operation="metadata.pack.admission",role="state"} 0`) {
			t.Fatal("admission completion/classification missing")
		}
		if canceled && strings.Contains(output.String(), `operation="metadata.pack.decode",role="state",result=`) {
			t.Fatal("canceled admission recorded decode work")
		}
		for _, secret := range []string{pack.DomainID, pack.PackID, "requestID=", "traceID=", "stage=", "reason="} {
			if strings.Contains(output.String(), secret) {
				t.Fatalf("private/cardinality admission metric %q", secret)
			}
		}
		if _, err := DecodeDomainPagePack(context.Background(), wire, pack.DomainID, pack.Kind, pack.PackID); err != nil {
			t.Fatal("admission telemetry leaked slot")
		}
	}
}

// The explicit signal is the injected deadline/cancellation schedule. No wall
// clock sleep or provider work determines which side of admission is canceled.
type admissionSignalContext struct {
	context.Context
	done chan struct{}
	err  error
}

func (ctx *admissionSignalContext) Done() <-chan struct{} { return ctx.done }
func (ctx *admissionSignalContext) Err() error {
	select {
	case <-ctx.done:
		return ctx.err
	default:
		return nil
	}
}

func TestDomainPagePackAdmissionTelemetryCanceledAndDeadlineBoundariesAreSafe(t *testing.T) {
	pack := testDomainPagePack(t)
	wire, err := EncodeDomainPagePack(pack)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		cause  error
		result string
		reason string
	}{
		{"canceled", context.Canceled, "canceled", "canceled"},
		{"deadline", context.DeadlineExceeded, "timeout", "deadline"},
	} {
		for _, queued := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/queued=%v", test.name, queued), func(t *testing.T) {
				logs := &admissionTelemetryLog{records: make(chan slog.Record, 1)}
				observer := telemetry.New(nil, slog.New(logs))
				defer observer.Close()
				observer.SetVersion("admission-test")
				exporter := tracetest.NewInMemoryExporter()
				provider := telemetry.TraceProvider(exporter, observer, "admission-test", 1)
				defer func() { _ = provider.Shutdown(context.Background()) }()
				base := telemetry.WithRequestID(telemetry.Context(context.Background(), observer), "admission-request-42")
				signal := &admissionSignalContext{Context: base, done: make(chan struct{}), err: test.cause}
				waiting := &packAdmissionWaitingContext{Context: signal, ready: make(chan struct{})}
				if queued {
					domainPagePackDecodeSlot <- struct{}{}
				} else {
					close(signal.done)
				}
				result := make(chan error, 1)
				go func() {
					_, err := DecodeDomainPagePack(waiting, wire, "sentinel-owner", pack.Kind, "sentinel-pack-key")
					result <- err
				}()
				if queued {
					<-waiting.ready
					close(signal.done)
				}
				err := <-result
				if queued {
					if len(domainPagePackDecodeSlot) != 1 {
						t.Fatal("canceled admission consumed occupied expansion slot")
					}
					<-domainPagePackDecodeSlot
				}
				if !errors.Is(err, test.cause) || !errors.Is(err, domain.ErrUnavailable) {
					t.Fatal("pre/deferred admission changed cancellation error identity")
				}
				if err := provider.ForceFlush(context.Background()); err != nil {
					t.Fatal(err)
				}
				spans := exporter.GetSpans()
				if len(spans) != 1 || spans[0].Name != "metadata.pack.admission" || spans[0].EndTime.Before(spans[0].StartTime) {
					t.Fatal("admission span missing or overlapping decode")
				}
				values := map[string]string{}
				for _, attr := range spans[0].Attributes {
					values[string(attr.Key)] = attr.Value.AsString()
				}
				if values["result"] != test.result || values["stage"] != "metadata_admission" || values["reason"] != test.reason || len(values) != 5 {
					t.Fatal("closed admission trace outcome missing")
				}
				select {
				case record := <-logs.records:
					fields := map[string]any{}
					record.Attrs(func(attr slog.Attr) bool { fields[attr.Key] = attr.Value.Any(); return true })
					if record.Message != "operation_completed" || fields["operation"] != "metadata.pack.admission" || fields["result"] != test.result || fields["stage"] != "metadata_admission" || fields["reason"] != test.reason || fields["requestID"] != "admission-request-42" || fields["build"] != "admission-test" || strings.Contains(fmt.Sprint(fields), "sentinel") {
						t.Fatal("safe admission log diagnostics/correlation missing")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("admission completion log missing")
				}
				var output bytes.Buffer
				observer.WritePrometheus(&output)
				if !strings.Contains(output.String(), `operation="metadata.pack.admission",role="state",direction="written"} 0`) || strings.Contains(output.String(), `operation="metadata.pack.decode",role="state",result=`) || strings.Contains(output.String(), "sentinel") || strings.Contains(output.String(), "admission-request-42") {
					t.Fatal("canceled admission allocated/decoded or retained private dimensions")
				}
			})
		}
	}
}

type admissionTelemetryLog struct{ records chan slog.Record }

func (*admissionTelemetryLog) Enabled(context.Context, slog.Level) bool { return true }
func (handler *admissionTelemetryLog) Handle(_ context.Context, record slog.Record) error {
	handler.records <- record.Clone()
	return nil
}
func (handler *admissionTelemetryLog) WithAttrs([]slog.Attr) slog.Handler { return handler }
func (handler *admissionTelemetryLog) WithGroup(string) slog.Handler      { return handler }

func TestDomainPagePackAdmissionTelemetrySuccessEndsBeforeStrictDecode(t *testing.T) {
	pack := testDomainPagePack(t)
	wire, err := EncodeDomainPagePack(pack)
	if err != nil {
		t.Fatal(err)
	}
	observer := telemetry.New(nil, nil)
	exporter := tracetest.NewInMemoryExporter()
	provider := telemetry.TraceProvider(exporter, observer, "admission-test", 1)
	defer func() { _ = provider.Shutdown(context.Background()) }()
	ctx := telemetry.Context(context.Background(), observer)
	decoded, err := DecodeDomainPagePack(ctx, wire, pack.DomainID, pack.Kind, pack.PackID)
	if err != nil || !reflect.DeepEqual(decoded, pack) {
		t.Fatal("admission observation changed validated decoded authority")
	}
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans := exporter.GetSpans()
	if len(spans) != 2 || spans[0].Name != "metadata.pack.admission" || spans[1].Name != "metadata.pack.decode" || spans[0].EndTime.After(spans[1].StartTime) {
		t.Fatal("admission span includes decode duration or decode observation changed")
	}
	for _, span := range spans {
		values := map[string]string{}
		for _, attr := range span.Attributes {
			values[string(attr.Key)] = attr.Value.AsString()
		}
		if values["result"] != "success" || len(values) != 3 {
			t.Fatal("success admission/decode trace gained private or unreviewed dimensions")
		}
	}
	var output bytes.Buffer
	observer.WritePrometheus(&output)
	if !strings.Contains(output.String(), `endlessfs_operations_total{operation="metadata.pack.decode",role="state",result="success"} 1`) || !strings.Contains(output.String(), `operation="metadata.pack.admission",role="state",direction="read"} 0`) {
		t.Fatal("existing decode or new zero-byte admission accounting changed")
	}
}
