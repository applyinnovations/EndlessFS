package telemetry

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// ExportQueueSize is a memory/safety bound, not a provider-performance budget.
const ExportQueueSize = 256
const exportBatchSize = 64
const instrumentationName = "github.com/applyinnovations/endlessfs"

// TraceProvider uses a process-local SDK provider and fixed bounded processor.
// It never enables opaque provider-SDK instrumentation or environment detectors.
func TraceProvider(exporter sdktrace.SpanExporter, observer *Observer, version string, sampleRatio float64) *sdktrace.TracerProvider {
	processor := &traceProcessor{exporter: exporter, observer: observer, queue: make(chan sdktrace.ReadOnlySpan, ExportQueueSize), flush: make(chan chan struct{}), done: make(chan struct{})}
	processor.ctx, processor.cancel = context.WithCancel(context.Background())
	go processor.run()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", "endlessfs"), attribute.String("service.version", safeVersion(version)))),
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(sampleRatio)),
		sdktrace.WithRawSpanLimits(sdktrace.SpanLimits{AttributeValueLengthLimit: 128, AttributeCountLimit: 4, EventCountLimit: 0, LinkCountLimit: 0, AttributePerEventCountLimit: 0, AttributePerLinkCountLimit: 0}),
		sdktrace.WithSpanProcessor(processor),
	)
	observer.tracer = provider.Tracer(instrumentationName)
	return provider
}

func safeVersion(value string) string {
	if len(value) == 0 || len(value) > 80 {
		return "unknown"
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '.' || character == '+' || character == '-') {
			return "unknown"
		}
	}
	return value
}

type traceProcessor struct {
	exporter sdktrace.SpanExporter
	observer *Observer
	queue    chan sdktrace.ReadOnlySpan
	flush    chan chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.RWMutex
	closed   bool
}

func (*traceProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (processor *traceProcessor) OnEnd(span sdktrace.ReadOnlySpan) {
	if !span.SpanContext().IsSampled() {
		return
	}
	processor.mu.RLock()
	defer processor.mu.RUnlock()
	if processor.closed {
		processor.observer.ExportDropped.Add(1)
		return
	}
	// Privacy is enforced before retaining a span, not only at the network edge.
	if !safeSpan(span) {
		processor.observer.ExportDropped.Add(1)
		return
	}
	select {
	case processor.queue <- span:
	default:
		processor.observer.ExportDropped.Add(1)
	}
}
func safeSpan(span sdktrace.ReadOnlySpan) bool {
	scope := span.InstrumentationScope()
	if scope.Name != instrumentationName || scope.Version != "" || scope.SchemaURL != "" || scope.Attributes.Len() != 0 || span.SpanContext().TraceState().Len() != 0 || span.Parent().TraceState().Len() != 0 {
		return false
	}
	if span.Resource().SchemaURL() != "" {
		return false
	}
	resourceAttributes := span.Resource().Attributes()
	if len(resourceAttributes) != 2 {
		return false
	}
	for _, value := range resourceAttributes {
		if value.Value.Type() != attribute.STRING {
			return false
		}
		switch string(value.Key) {
		case "service.name":
			if value.Value.AsString() != "endlessfs" {
				return false
			}
		case "service.version":
			if safeVersion(value.Value.AsString()) != value.Value.AsString() {
				return false
			}
		default:
			return false
		}
	}
	operation, role, result, route := "", "", "", ""
	for _, value := range span.Attributes() {
		if value.Value.Type() != attribute.STRING {
			return false
		}
		switch string(value.Key) {
		case "operation":
			operation = value.Value.AsString()
		case "role":
			role = value.Value.AsString()
		case "result":
			result = value.Value.AsString()
		case "route":
			route = value.Value.AsString()
		default:
			return false
		}
	}
	if !contains(operationNames[:], operation) || !contains(roleNames[:], role) || !contains(resultNames[:], result) || span.Name() != operation {
		return false
	}
	if route != "" && (operation != operationNames[HTTP] || !contains(routes[:], route)) {
		return false
	}
	if span.Status().Description != "" && !contains(resultNames[:], span.Status().Description) {
		return false
	}
	return len(span.Events()) == 0 && len(span.Links()) == 0
}
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func (processor *traceProcessor) run() {
	defer close(processor.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	batch := make([]sdktrace.ReadOnlySpan, 0, exportBatchSize)
	export := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(processor.ctx, time.Second)
		err := processor.exporter.ExportSpans(ctx, batch)
		cancel()
		if err != nil {
			processor.observer.ExportFailed.Add(uint64(len(batch)))
		} else {
			processor.observer.Exported.Add(uint64(len(batch)))
		}
		clear(batch)
		batch = batch[:0]
	}
	drain := func() {
		for {
			select {
			case span := <-processor.queue:
				batch = append(batch, span)
				if len(batch) == exportBatchSize {
					export()
				}
			default:
				export()
				return
			}
		}
	}
	for {
		select {
		case <-processor.ctx.Done():
			processor.observer.ExportDropped.Add(uint64(len(batch) + len(processor.queue)))
			return
		case span := <-processor.queue:
			batch = append(batch, span)
			if len(batch) == exportBatchSize {
				export()
			}
		case <-ticker.C:
			export()
		case acknowledgment := <-processor.flush:
			drain()
			close(acknowledgment)
		}
	}
}
func (processor *traceProcessor) ForceFlush(ctx context.Context) error {
	acknowledgment := make(chan struct{})
	select {
	case <-processor.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case processor.flush <- acknowledgment:
	}
	select {
	case <-acknowledgment:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-processor.done:
		return nil
	}
}
func (processor *traceProcessor) Shutdown(ctx context.Context) error {
	processor.mu.Lock()
	processor.closed = true
	processor.cancel()
	processor.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-processor.done:
		return processor.exporter.Shutdown(ctx)
	}
}
