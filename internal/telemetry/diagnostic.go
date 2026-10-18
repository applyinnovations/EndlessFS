package telemetry

import (
	"context"
	"errors"
	"net"
	"os"
	"regexp"
)

type Stage uint8

const (
	StageUnknown Stage = iota
	RequestValidation
	NamespaceRead
	ProjectionBuild
	ProjectionPublish
	TargetLookup
	SnapshotCheck
	NamespacePublish
	ResponseWrite
	MetadataAdmission
)

type Reason uint8

const (
	ReasonUnknown Reason = iota
	Validation
	CorruptMetadata
	PackCapacity
	PackCollision
	SnapshotStale
	ProviderFailure
	ContextCanceled
	InternalFailure
	ContextDeadline
	TransportFailure
)

var stageNames = [...]string{"unknown", "request_validation", "namespace_read", "projection_build", "projection_publish", "target_lookup", "snapshot_check", "namespace_publish", "response_write", "metadata_admission"}
var reasonNames = [...]string{"unknown", "validation", "corrupt_metadata", "pack_capacity", "pack_collision", "snapshot_stale", "provider_failure", "canceled", "internal", "deadline", "transport_failure"}

type diagnosticError struct {
	cause  error
	stage  Stage
	reason Reason
}

func (err *diagnosticError) Error() string { return err.cause.Error() }
func (err *diagnosticError) Unwrap() error { return err.cause }

// Diagnostic carries only reviewed codes into observation. Its cause keeps
// ordinary errors.Is/As semantics and is never queued or serialized by telemetry.
func Diagnostic(err error, stage Stage, reason Reason) error {
	if err == nil {
		return nil
	}
	var existing *diagnosticError
	if errors.As(err, &existing) {
		return err
	}
	if int(stage) >= len(stageNames) {
		stage = StageUnknown
	}
	if int(reason) >= len(reasonNames) {
		reason = ReasonUnknown
	}
	return &diagnosticError{cause: err, stage: stage, reason: reason}
}

type requestCorrelationKey struct{}
type httpActivityKey struct{}

var requestCorrelationPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func WithRequestID(ctx context.Context, id string) context.Context {
	if !requestCorrelationPattern.MatchString(id) {
		id = ""
	}
	return context.WithValue(ctx, requestCorrelationKey{}, id)
}

func (activity *Activity) diagnosis(err error, problem bool) {
	if activity == nil || err == nil {
		return
	}
	var diagnostic *diagnosticError
	stage, reason := StageUnknown, ReasonUnknown
	if errors.As(err, &diagnostic) {
		stage, reason = diagnostic.stage, diagnostic.reason
	}
	activity.diagnosticMu.Lock()
	defer activity.diagnosticMu.Unlock()
	if activity.transportFailed || activity.problemRecorded && !problem {
		return
	}
	activity.stage, activity.reason = stage, reason
	if problem {
		activity.problemRecorded = true
		activity.problemResult = Classify(err)
	}
}

// RecordProblem reaches the enclosing HTTP activity through nested operations.
// Retain only bounded codes and class, never the private error or request body.
func RecordProblem(ctx context.Context, err error) {
	activity, _ := ctx.Value(httpActivityKey{}).(*Activity)
	activity.diagnosis(err, true)
}

// RecordResponseWrite observes an error surfaced by a response write or the
// control request boundary's explicit buffered-body flush.
// It preserves intended HTTP status while classifying transport completion. No
// raw error is retained, and a later public problem cannot mask a failed write.
// Later net/http terminal chunks/trailers and client receipt remain outside
// this boundary.
func RecordResponseWrite(ctx context.Context, err error) Result {
	if err == nil {
		return Success
	}
	result, reason := Unavailable, TransportFailure
	var timeout net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		result, reason = Timeout, ContextDeadline
	case errors.Is(err, context.Canceled):
		result, reason = Canceled, ContextCanceled
	case errors.As(err, &timeout) && timeout.Timeout():
		result, reason = Timeout, ContextDeadline
	}
	if ctx != nil {
		activity, _ := ctx.Value(httpActivityKey{}).(*Activity)
		if activity != nil {
			activity.diagnosticMu.Lock()
			if !activity.transportFailed {
				activity.stage, activity.reason = ResponseWrite, reason
				activity.problemRecorded, activity.problemResult = true, result
				activity.transportFailed = true
			}
			activity.diagnosticMu.Unlock()
		}
	}
	return result
}
