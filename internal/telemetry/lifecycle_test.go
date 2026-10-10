package telemetry

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

type blockedLogWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (writer *blockedLogWriter) Write(body []byte) (int, error) {
	writer.once.Do(func() { close(writer.entered) })
	<-writer.release
	return len(body), nil
}
func TestSlowLifecycleLogSinkCannotBackpressureOperations(t *testing.T) {
	writer := &blockedLogWriter{entered: make(chan struct{}), release: make(chan struct{})}
	observer := New(nil, slog.New(slog.NewJSONHandler(writer, nil)))
	defer observer.Close()
	defer close(writer.release)
	ctx := Context(context.Background(), observer)
	_, activity := Start(ctx, UploadAdmission, Application)
	activity.End(nil)
	select {
	case <-writer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("log sink not entered")
	}
	for range 1000 {
		_, activity := Start(ctx, UploadAdmission, Application)
		activity.End(errors.New("sentinel-private-url"))
	}
	if observer.LogDropped.Load() == 0 {
		t.Fatal("full log queue did not report loss")
	}
}
func TestResultTaxonomyAndProgressAreClosed(t *testing.T) {
	for _, test := range []struct {
		err    error
		result Result
	}{
		{nil, Success}, {domain.ErrInvalid, Invalid}, {domain.ErrUnauthorized, Denied}, {domain.ErrUnauthenticated, Denied}, {domain.ErrNotFound, NotFound},
		{domain.ErrConflict, Conflict}, {domain.ErrPreconditionFailed, Conflict}, {domain.ErrRateLimited, Unavailable}, {domain.ErrUnavailable, Unavailable},
		{context.DeadlineExceeded, Timeout}, {context.Canceled, Canceled}, {errors.New("sentinel-secret"), Internal},
	} {
		if Classify(test.err) != test.result {
			t.Errorf("incorrect result class: want %d", test.result)
		}
	}
	observer := New(nil, nil)
	observer.MigrationProgress("checkpoint-inventory", State, 64, 100, 10, 1024, 2048)
	observer.MigrationProgress("sentinel-private-id", State, 1, 1, 0, 1, 1)
	observer.MigrationProgress("started", Role(255), 1, 1, 0, 1, 1)
	observer.Worker(WorkerResources{CPUSeconds: .5, PeakBytes: 4096, ExitCode: 0})
	observer.Worker(WorkerResources{CPUSeconds: .5, PeakBytes: 8192, ExitCode: -1, Signaled: true})
	observer.Worker(WorkerResources{PeakBytes: 0, ExitCode: 2})
	observer.Worker(WorkerResources{PeakBytes: -1})
	var output bytes.Buffer
	observer.WritePrometheus(&output)
	for _, want := range []string{`endlessfs_migration_completed_objects{stage="checkpoint-inventory",role="state"} 64`, `endlessfs_preview_child_cpu_seconds_total 1`, `endlessfs_preview_child_peak_resident_bytes 8192`} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(output.String(), "sentinel") {
		t.Fatal("unknown stage retained")
	}
	_, activity := Start(Context(context.Background(), observer), DomainCommit, State)
	func() {
		var err error
		defer func() {
			if recover() == nil {
				t.Fatal("panic swallowed")
			}
		}()
		defer Finish(activity, &err)
		panic("sentinel-secret")
	}()
}
func BenchmarkConcurrentOperationalTelemetry(b *testing.B) {
	for _, logging := range []bool{false, true} {
		name := "metrics"
		var logger *slog.Logger
		if logging {
			name = "lifecycle"
			logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
		}
		b.Run(name, func(b *testing.B) {
			observer := New(nil, logger)
			defer observer.Close()
			ctx := Context(context.Background(), observer)
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					_, activity := Start(ctx, UploadStatus, Application)
					activity.End(nil)
				}
			})
		})
	}
}
