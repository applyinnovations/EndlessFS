package telemetry

import (
	"context"
	"errors"
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
)

var stageNames = [...]string{"unknown", "request_validation", "namespace_read", "projection_build", "projection_publish", "target_lookup", "snapshot_check", "namespace_publish"}
var reasonNames = [...]string{"unknown", "validation", "corrupt_metadata", "pack_capacity", "pack_collision", "snapshot_stale", "provider_failure", "canceled", "internal"}

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
	if activity.problemRecorded && !problem {
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
