package portable

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type unavailableCollector struct {
	entered chan struct{}
	once    sync.Once
}

func (collector *unavailableCollector) ExportSpans(ctx context.Context, _ []sdktrace.ReadOnlySpan) error {
	collector.once.Do(func() { close(collector.entered) })
	<-ctx.Done()
	return ctx.Err()
}
func (*unavailableCollector) Shutdown(context.Context) error { return nil }

func TestIntegrationUnavailableTelemetryCannotChangePortablePublicationOrConflicts(t *testing.T) {
	collector := &unavailableCollector{entered: make(chan struct{})}
	observer := telemetry.New(nil, nil)
	provider := telemetry.TraceProvider(collector, observer, "test", 1)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = provider.Shutdown(ctx)
	}()
	ctx := telemetry.Context(context.Background(), observer)
	// Deterministically stall the first export before constructing real authority.
	for range 64 {
		_, activity := telemetry.Start(ctx, telemetry.DomainCommit, telemetry.State)
		activity.End(nil)
	}
	select {
	case <-collector.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("collector did not enter")
	}
	engine, err := Open(ctx, Options{
		Backend: objectstore.Observe(objectmemory.New(), telemetry.State), FileBackend: objectstore.Observe(objectmemory.New(), telemetry.Files),
		Clock: domain.NewFixedClock(time.Date(2050, 1, 1, 0, 0, 0, 0, time.UTC)), IDs: domain.NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte("telemetry-independent-entropy"), 1<<16))),
		Writer: WriterConfiguration{WriterSetID: "telemetry-test", ConfigurationDigest: "telemetry-test-v1", KeyringIdentifiers: []string{"test"}}, LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x75}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	key := state.MustKey(state.NamespacePreferences, "telemetry-independent-owner")
	version, err := engine.Create(ctx, key, []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	next, err := engine.CompareAndSwap(ctx, key, version, []byte("second"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CompareAndSwap(ctx, key, version, []byte("stale")); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatal("collector changed stale-writer denial")
	}
	record, err := engine.Get(ctx, key)
	if err != nil || record.Version != next || !bytes.Equal(record.Data, []byte("second")) {
		t.Fatalf("publication changed: %+v %v", record, err)
	}
	// Force overflow while the real engine remains usable.
	for range 400 {
		_, activity := telemetry.Start(ctx, telemetry.UploadStatus, telemetry.Application)
		activity.End(nil)
	}
	if observer.ExportDropped.Load() == 0 {
		t.Fatal("overflow not measured")
	}
	if _, err := engine.Get(ctx, key); err != nil {
		t.Fatal("queue overflow changed authority availability")
	}
}
