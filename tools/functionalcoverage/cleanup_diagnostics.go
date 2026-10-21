package main

import (
	"errors"
	"os"
	"syscall"
)

type nativeCleanupError struct {
	stage, reason string
	cause         error
}

func (e *nativeCleanupError) Error() string { return "native_cleanup_observation_inadequate" }
func (e *nativeCleanupError) Unwrap() error { return e.cause }
func nativeScanFailure(stage string, cause error) error {
	reason := "invalid"
	switch {
	case errors.Is(cause, os.ErrPermission):
		reason = "permission_denied"
	case errors.Is(cause, os.ErrNotExist):
		reason = "not_found"
	case errors.Is(cause, syscall.ESRCH):
		reason = "gone"
	case errors.Is(cause, syscall.EIO):
		reason = "io_error"
	case cause != nil && !errors.Is(cause, errContract):
		reason = "other"
	}
	return &nativeCleanupError{closedCleanupStage(stage), reason, cause}
}
func closedCleanupStage(stage string) string {
	switch stage {
	case "", "proc_inventory", "proc_bound", "process_metadata", "environment_open", "environment_read", "environment_close", "environment_bound", "scope_identity", "scope_marker", "owned_stat", "owned_identity", "owned_executable", "owned_bound", "orphan_reap", "orphan_bound", "pidfd_cleanup":
		return stage
	default:
		return "unknown"
	}
}
func closedCleanupReason(reason string) string {
	switch reason {
	case "", "permission_denied", "not_found", "gone", "io_error", "invalid", "other":
		return reason
	default:
		return "unknown"
	}
}
func setCleanupFailure(r *cleanupObservation, err error) {
	var detail *nativeCleanupError
	if errors.As(err, &detail) {
		r.ScanStage, r.ScanReason = closedCleanupStage(detail.stage), closedCleanupReason(detail.reason)
	} else {
		r.ScanStage, r.ScanReason = "unknown", "other"
	}
}
