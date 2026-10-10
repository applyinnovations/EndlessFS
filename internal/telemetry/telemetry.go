// Package telemetry defines non-authoritative, closed-label operational signals.
// It accepts enums and registered route templates, never arbitrary attributes.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type Operation uint8

const (
	HTTP Operation = iota
	ProviderHead
	ProviderVerify
	ProviderGet
	ProviderOpen
	ProviderList
	ProviderPut
	ProviderDelete
	ProviderCopy
	ProviderUploadBegin
	ProviderUploadResume
	ProviderUploadStatus
	ProviderUploadAbort
	ProviderDownload
	UploadSizes
	UploadFingerprints
	UploadAdmission
	UploadStatus
	UploadCompletion
	UploadCancellation
	MetadataPackDecode
	MetadataLeaseDecode
	DomainCommit
	DomainRecovery
	DomainRetry
	Migration
	PreviewQueue
	PreviewSource
	PreviewGenerate
	PreviewWorker
	PreviewPersist
	WireHead
	WireRead
	WireList
	WirePut
	WireDelete
	WireCopy
	WireUploadBegin
	WireUploadStatus
	WireUploadAbort
	WireUploadData
	WireDownloadData
	WireUnknown
	operationCount
)

type Role uint8

const (
	Application Role = iota
	State
	Files
	Previews
	Shared
	roleCount
)

type Result uint8

const (
	Success Result = iota
	Invalid
	Denied
	NotFound
	Conflict
	Unavailable
	Timeout
	Canceled
	Internal
	Panicked
	resultCount
)

var operationNames = [...]string{
	"http.request",
	"provider.head",
	"provider.verify",
	"provider.get",
	"provider.open",
	"provider.list",
	"provider.put",
	"provider.delete",
	"provider.copy",
	"provider.upload.begin",
	"provider.upload.resume",
	"provider.upload.status",
	"provider.upload.abort",
	"provider.download",
	"upload.plan.sizes",
	"upload.plan.fingerprints",
	"upload.admission",
	"upload.status",
	"upload.completion",
	"upload.cancellation",
	"metadata.pack.decode",
	"metadata.lease.decode",
	"domain.commit",
	"domain.recovery",
	"domain.retry",
	"migration",
	"preview.queue",
	"preview.source",
	"preview.generate",
	"preview.worker",
	"preview.persist",
	"provider.http.head",
	"provider.http.read",
	"provider.http.list",
	"provider.http.put",
	"provider.http.delete",
	"provider.http.copy",
	"provider.http.upload.begin",
	"provider.http.upload.status",
	"provider.http.upload.abort",
	"provider.http.upload.data",
	"provider.http.download.data",
	"provider.http.unknown",
}
var roleNames = [...]string{
	"application",
	"state",
	"files",
	"previews",
	"shared",
}
var resultNames = [...]string{
	"success",
	"invalid",
	"denied",
	"not_found",
	"conflict",
	"unavailable",
	"timeout",
	"canceled",
	"internal",
	"panic",
}
var durationBounds = [...]float64{.001, .005, .025, .1, .5, 2, 10, 60}
var routes = [...]string{
	"unmatched",
	"POST /api/v1/previews/resolve",
	"POST /api/v1/previews/generations",
	"GET /api/v1/previews/operations/{operationID}",
	"GET /api/v1/themes",
	"GET /api/v1/me/preferences/theme",
	"PUT /api/v1/me/preferences/theme",
	"GET /assets/themes/{digest}/{asset}",
	"GET /healthz",
	"GET /readyz",
	"GET /api/v1/config",
	"GET /",
	"POST /api/v1/bootstrap/options",
	"POST /api/v1/bootstrap/verify",
	"POST /api/v1/registration/options",
	"POST /api/v1/registration/verify",
	"POST /api/v1/authentication/options",
	"POST /api/v1/authentication/verify",
	"POST /api/v1/logout",
	"GET /api/v1/me",
	"PATCH /api/v1/me",
	"GET /api/v1/me/passkeys",
	"POST /api/v1/me/passkeys/options",
	"POST /api/v1/me/passkeys/verify",
	"DELETE /api/v1/me/passkeys/{credentialID}",
	"GET /api/v1/admin/invites",
	"POST /api/v1/admin/invites",
	"DELETE /api/v1/admin/invites/{inviteID}",
	"GET /api/v1/admin/users",
	"POST /api/v1/admin/users/{userID}/disable",
	"POST /api/v1/admin/users/{userID}/enable",
	"POST /api/v1/admin/users/{userID}/admin",
	"DELETE /api/v1/admin/users/{userID}/admin",
	"POST /api/v1/admin/users/{userID}/recoveries",
	"POST /api/v1/recovery/options",
	"POST /api/v1/recovery/verify",
	"GET /api/v1/files",
	"GET /api/v1/files/storage-map",
	"GET /api/v1/files/stat",
	"GET /api/v1/duplicates/groups",
	"GET /api/v1/duplicates/groups/{groupID}/occurrences",
	"PUT /api/v1/duplicates/groups/{groupID}/ignore",
	"POST /api/v1/duplicates/directories/compare",
	"POST /api/v1/duplicates/directories/overlaps",
	"PUT /api/v1/duplicates/directories/ignore",
	"POST /api/v1/duplicates/directories/reconciliation-preview",
	"POST /api/v1/duplicates/directories/reconcile",
	"POST /api/v1/directories",
	"POST /api/v1/uploads",
	"POST /api/v1/uploads/batch",
	"POST /api/v1/uploads/batch/complete",
	"DELETE /api/v1/uploads/batch",
	"POST /api/v1/uploads/plan/sizes",
	"POST /api/v1/uploads/plan/fingerprints",
	"GET /api/v1/uploads/{uploadID}",
	"POST /api/v1/uploads/{uploadID}/complete",
	"DELETE /api/v1/uploads/{uploadID}",
	"POST /api/v1/downloads",
	"POST /api/v1/files/copy",
	"POST /api/v1/files/move",
	"POST /api/v1/files/trash",
	"GET /api/v1/operations/{operationID}",
	"GET /api/v1/trash",
	"POST /api/v1/trash/restore",
	"POST /api/v1/trash/delete",
	"POST /api/v1/trash/{trashID}/restore",
	"DELETE /api/v1/trash/{trashID}",
	"POST /api/v1/trash/empty",
	"GET /api/v1/shares",
	"POST /api/v1/shares",
	"DELETE /api/v1/shares/{shareID}",
	"GET /api/v1/public/shares/{token}",
	"GET /api/v1/public/shares/{token}/stat",
	"POST /api/v1/public/shares/{token}/downloads",
	"GET /s/{token}",
	"GET /metrics",
	"GET /debug/pprof/",
}

type distribution struct {
	Count   uint64
	Seconds float64
	Buckets [len(durationBounds)]uint64
}
type measurement struct {
	Active        int64
	Read, Written uint64
	Results       [resultCount]distribution
}
type WorkerResources struct {
	CPUSeconds float64
	PeakBytes  int64
	ExitCode   int
	Signaled   bool
}

// Observer owns a fixed-size registry. Its tracer is local, not the global SDK
// provider used by opaque third-party instrumentation.
type Observer struct {
	mu            sync.Mutex
	metrics       [operationCount][roleCount]measurement
	http          [len(routes)][resultCount]distribution
	migration     [len(migrationStages)][roleCount]migrationProgress
	tracer        trace.Tracer
	version       string
	logger        *slog.Logger
	events        chan lifecycleEvent
	stopEvents    chan struct{}
	stopOnce      sync.Once
	workerCPU     float64
	workerPeak    int64
	workerExits   [4]uint64
	ExportFailed  atomic.Uint64
	ExportDropped atomic.Uint64
	Exported      atomic.Uint64
	LogDropped    atomic.Uint64
}
type lifecycleEvent struct {
	operation Operation
	role      Role
	result    Result
	seconds   float64
	traceID   string
}
type Activity struct {
	observer  *Observer
	operation Operation
	role      Role
	route     int
	started   time.Time
	span      trace.Span
	ctx       context.Context
	ended     atomic.Bool
}
type contextKey struct{}
type roleKey struct{}

func New(tracer trace.Tracer, logger *slog.Logger) *Observer {
	observer := &Observer{tracer: tracer, logger: logger}
	if logger != nil {
		observer.events = make(chan lifecycleEvent, 64)
		observer.stopEvents = make(chan struct{})
		go observer.logEvents()
	}
	return observer
}

// SetVersion is called once during process setup, before collection starts.
func (observer *Observer) SetVersion(version string) { observer.version = safeVersion(version) }
func (observer *Observer) Close() {
	if observer != nil && observer.stopEvents != nil {
		observer.stopOnce.Do(func() { close(observer.stopEvents) })
	}
}
func (observer *Observer) logEvents() {
	for {
		select {
		case <-observer.stopEvents:
			return
		case event := <-observer.events:
			observer.logger.Info("operation_completed", "operation", operationNames[event.operation], "role", roleNames[event.role], "result", resultNames[event.result], "durationSeconds", event.seconds, "traceID", event.traceID)
		}
	}
}
func Context(ctx context.Context, observer *Observer) context.Context {
	return context.WithValue(ctx, contextKey{}, observer)
}
func From(ctx context.Context) *Observer {
	observer, _ := ctx.Value(contextKey{}).(*Observer)
	return observer
}
func Start(ctx context.Context, operation Operation, role Role) (context.Context, *Activity) {
	observer := From(ctx)
	if observer == nil || operation >= operationCount || role >= roleCount {
		return ctx, nil
	}
	activity := &Activity{observer: observer, operation: operation, role: role, started: time.Now()}
	if observer.tracer != nil {
		kind := trace.SpanKindInternal
		if operation == HTTP {
			kind = trace.SpanKindServer
		} else if operation >= ProviderHead && operation <= ProviderDownload || operation >= WireHead && operation <= WireUnknown {
			kind = trace.SpanKindClient
		}
		ctx, activity.span = observer.tracer.Start(ctx, operationNames[operation], trace.WithSpanKind(kind), trace.WithAttributes(attribute.String("operation", operationNames[operation]), attribute.String("role", roleNames[role])))
	}
	activity.ctx = ctx
	ctx = context.WithValue(ctx, roleKey{}, role)
	observer.mu.Lock()
	observer.metrics[operation][role].Active++
	observer.mu.Unlock()
	return ctx, activity
}
func ContextRole(ctx context.Context) Role { role, _ := ctx.Value(roleKey{}).(Role); return role }
func (activity *Activity) Route(pattern string) {
	if activity == nil || activity.operation != HTTP {
		return
	}
	for index, route := range routes {
		if route == pattern {
			activity.route = index
			break
		}
	}
	if activity.span != nil {
		activity.span.SetAttributes(attribute.String("route", routes[activity.route]))
	}
}
func (activity *Activity) Bytes(read, written int64) {
	if activity == nil {
		return
	}
	activity.observer.mu.Lock()
	defer activity.observer.mu.Unlock()
	measurement := &activity.observer.metrics[activity.operation][activity.role]
	if read > 0 {
		measurement.Read += uint64(read)
	}
	if written > 0 {
		measurement.Written += uint64(written)
	}
}
func Classify(err error) Result {
	switch {
	case err == nil:
		return Success
	case errors.Is(err, context.DeadlineExceeded):
		return Timeout
	case errors.Is(err, context.Canceled):
		return Canceled
	case errors.Is(err, domain.ErrInvalid):
		return Invalid
	case errors.Is(err, domain.ErrUnauthenticated), errors.Is(err, domain.ErrUnauthorized):
		return Denied
	case errors.Is(err, domain.ErrNotFound):
		return NotFound
	case errors.Is(err, domain.ErrConflict), errors.Is(err, domain.ErrPreconditionFailed):
		return Conflict
	case errors.Is(err, domain.ErrUnavailable), errors.Is(err, domain.ErrRateLimited):
		return Unavailable
	default:
		return Internal
	}
}
func (activity *Activity) End(err error) { activity.EndResult(Classify(err)) }

// Finish is called directly by defer with a named return error. It preserves
// panic behavior while recording its coarse class rather than raw panic data.
func Finish(activity *Activity, err *error) {
	if value := recover(); value != nil {
		activity.EndResult(Panicked)
		panic(value)
	}
	activity.End(*err)
}
func observeDistribution(value *distribution, seconds float64) {
	value.Count++
	value.Seconds += seconds
	for index, bound := range durationBounds {
		if seconds <= bound {
			value.Buckets[index]++
		}
	}
}
func (activity *Activity) EndResult(result Result) {
	if activity == nil || !activity.ended.CompareAndSwap(false, true) {
		return
	}
	if result >= resultCount {
		result = Internal
	}
	elapsed := time.Since(activity.started).Seconds()
	observer := activity.observer
	observer.mu.Lock()
	measurement := &observer.metrics[activity.operation][activity.role]
	measurement.Active--
	observeDistribution(&measurement.Results[result], elapsed)
	if activity.operation == HTTP {
		observeDistribution(&observer.http[activity.route][result], elapsed)
	}
	observer.mu.Unlock()
	if activity.span != nil {
		activity.span.SetAttributes(attribute.String("result", resultNames[result]))
		if result != Success {
			activity.span.SetStatus(codes.Error, resultNames[result])
		}
		activity.span.End()
	}
	// Lifecycle errors are safe and useful; provider successes remain metrics/
	// traces rather than creating one log record per storage call.
	if observer.logger != nil && (activity.operation >= UploadSizes && activity.operation <= PreviewPersist || result != Success) {
		event := lifecycleEvent{activity.operation, activity.role, result, elapsed, TraceID(activity.ctx)}
		select {
		case <-observer.stopEvents:
			observer.LogDropped.Add(1)
		case observer.events <- event:
		default:
			observer.LogDropped.Add(1)
		}
	}
}
func TraceID(ctx context.Context) string {
	span := trace.SpanContextFromContext(ctx)
	if !span.IsValid() {
		return ""
	}
	return span.TraceID().String()
}
func (observer *Observer) Worker(resources WorkerResources) {
	if observer == nil {
		return
	}
	outcome := 0
	if resources.Signaled {
		outcome = 1
	} else if resources.ExitCode != 0 {
		outcome = 2
	}
	if resources.PeakBytes < 0 {
		outcome = 3
		resources.PeakBytes = 0
	}
	observer.mu.Lock()
	observer.workerCPU += resources.CPUSeconds
	if resources.PeakBytes > observer.workerPeak {
		observer.workerPeak = resources.PeakBytes
	}
	observer.workerExits[outcome]++
	observer.mu.Unlock()
}
func (observer *Observer) WritePrometheus(output io.Writer) {
	if observer == nil {
		return
	}
	observer.mu.Lock()
	metrics, httpMetrics, cpu, peak, exits := observer.metrics, observer.http, observer.workerCPU, observer.workerPeak, observer.workerExits
	migration := observer.migration
	observer.mu.Unlock()
	fmt.Fprintf(output, "endlessfs_build_info{version=%q} 1\n", safeVersion(observer.version))
	fmt.Fprintln(output, "# TYPE endlessfs_operations_total counter")
	fmt.Fprintln(output, "# TYPE endlessfs_operations_active gauge")
	fmt.Fprintln(output, "# TYPE endlessfs_operation_duration_seconds histogram")
	fmt.Fprintln(output, "# TYPE endlessfs_operation_bytes_total counter")
	for operation, roles := range metrics {
		for role, measurement := range roles {
			labels := fmt.Sprintf("operation=%q,role=%q", operationNames[operation], roleNames[role])
			fmt.Fprintf(output, "endlessfs_operations_active{%s} %d\n", labels, measurement.Active)
			fmt.Fprintf(output, "endlessfs_operation_bytes_total{%s,direction=\"read\"} %d\n", labels, measurement.Read)
			fmt.Fprintf(output, "endlessfs_operation_bytes_total{%s,direction=\"written\"} %d\n", labels, measurement.Written)
			for result, value := range measurement.Results {
				if value.Count == 0 {
					continue
				}
				outcome := fmt.Sprintf("%s,result=%q", labels, resultNames[result])
				fmt.Fprintf(output, "endlessfs_operations_total{%s} %d\n", outcome, value.Count)
				writeHistogram(output, "endlessfs_operation_duration_seconds", outcome, value)
			}
		}
	}
	fmt.Fprintln(output, "# TYPE endlessfs_http_duration_seconds histogram")
	writeMigration(output, migration)
	for index, results := range httpMetrics {
		for result, value := range results {
			if value.Count > 0 {
				writeHistogram(output, "endlessfs_http_duration_seconds", fmt.Sprintf("route=%q,result=%q", routes[index], resultNames[result]), value)
			}
		}
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	fmt.Fprintf(output, "endlessfs_go_heap_alloc_bytes %d\nendlessfs_go_heap_inuse_bytes %d\nendlessfs_go_heap_sys_bytes %d\nendlessfs_go_allocated_bytes_total %d\nendlessfs_go_gc_cycles_total %d\nendlessfs_go_gc_pause_seconds_total %g\nendlessfs_go_goroutines %d\n", memory.HeapAlloc, memory.HeapInuse, memory.HeapSys, memory.TotalAlloc, memory.NumGC, float64(memory.PauseTotalNs)/1e9, runtime.NumGoroutine())
	fmt.Fprintf(output, "endlessfs_preview_child_cpu_seconds_total %g\nendlessfs_preview_child_peak_resident_bytes %d\n", cpu, peak)
	for index, name := range []string{"success", "signaled", "failed", "unknown"} {
		fmt.Fprintf(output, "endlessfs_preview_child_exits_total{result=%q} %d\n", name, exits[index])
	}
	fmt.Fprintf(output, "endlessfs_trace_export_failed_total %d\nendlessfs_trace_dropped_total %d\nendlessfs_trace_exported_total %d\n", observer.ExportFailed.Load(), observer.ExportDropped.Load(), observer.Exported.Load())
	fmt.Fprintf(output, "endlessfs_telemetry_log_dropped_total %d\n", observer.LogDropped.Load())
}
func writeHistogram(output io.Writer, name, labels string, value distribution) {
	for index, bound := range durationBounds {
		fmt.Fprintf(output, "%s_bucket{%s,le=%q} %d\n", name, labels, fmt.Sprint(bound), value.Buckets[index])
	}
	fmt.Fprintf(output, "%s_bucket{%s,le=\"+Inf\"} %d\n%s_count{%s} %d\n%s_sum{%s} %g\n", name, labels, value.Count, name, labels, value.Count, name, labels, value.Seconds)
}
