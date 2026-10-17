package memory

import (
	"context"
	"errors"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/integrity"
)

// The mock models independently reported outcomes under its in-memory lock.
// Production coalescing/provider economics belong to the portable engine.
func (p *Provider) UploadActions(ctx context.Context, scope domain.Scope, requested []domain.UploadActionItem) ([]domain.UploadActionResult, error) {
	if err := validateContextScope(ctx, scope); err != nil {
		return nil, err
	}
	if len(requested) < 1 || len(requested) > domain.MaxUploadLifecycleItems {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid upload action page size")
	}
	seen := make(map[domain.UploadID]bool)
	for _, item := range requested {
		if item.UploadID == "" || len(item.UploadID) > 256 || seen[item.UploadID] {
			return nil, domain.NewError(domain.ErrorInvalid, "invalid or repeated upload action ID")
		}
		seen[item.UploadID] = true
	}
	p.uploadBatchMu.Lock()
	defer p.uploadBatchMu.Unlock()
	results := make([]domain.UploadActionResult, len(requested))
	for index, item := range requested {
		result := &results[index]
		result.Index, result.UploadID, result.Action = index, item.UploadID, item.Action
		fail := func(err error) {
			result.ErrorKind = domain.KindOf(err)
			result.Retryable = errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrRateLimited) || errors.Is(err, domain.ErrInternal)
			result.Outcome, result.NextAction = "rejected", "attention"
			if result.Retryable {
				result.Outcome, result.NextAction = "retryable", "retry"
			}
		}
		if item.Action != "complete" && item.Action != "cancel" || item.Action == "cancel" && item.CRC32C != "" {
			fail(domain.ErrInvalid)
			continue
		}
		if item.Action == "complete" {
			if _, ok := integrity.ParseCRC32C(item.CRC32C); !ok {
				fail(domain.ErrInvalid)
				continue
			}
		}
		p.mu.Lock()
		session, found := p.uploads[item.UploadID]
		if !found || session.scope != scope {
			p.mu.Unlock()
			fail(domain.ErrNotFound)
			continue
		}
		copy := *session
		p.mu.Unlock()
		status, err := p.UploadStatus(ctx, scope, item.UploadID)
		if err != nil {
			fail(err)
			continue
		}
		result.Status = &status
		if copy.state == domain.UploadStateCompleted {
			if item.Action == "complete" && copy.completedCRC32C != item.CRC32C {
				fail(domain.ErrConflict)
				continue
			}
			result.Entry, result.Outcome, result.NextAction = copy.completedEntry, "already_applied", "done"
			if item.Action == "cancel" {
				result.Outcome = "already_completed"
			}
			continue
		}
		if copy.state == domain.UploadStateAborted {
			if item.Action == "cancel" {
				result.Outcome, result.NextAction = "already_applied", "done"
			} else {
				fail(domain.ErrConflict)
				result.NextAction = "restart"
			}
			continue
		}
		if item.Action == "complete" {
			if status.State == domain.UploadStateExpired {
				fail(domain.ErrConflict)
				result.NextAction = "restart"
				continue
			}
			if !copy.materialized || integrity.CRC32C(copy.data) != item.CRC32C {
				fail(domain.ErrPreconditionFailed)
				continue
			}
			entry, completeErr := p.completeUpload(ctx, scope, domain.CompleteUploadRequest{UploadID: item.UploadID, Path: copy.requestedPath, Size: copy.size, MediaType: copy.mediaType, CRC32C: item.CRC32C})
			if completeErr != nil {
				fail(completeErr)
				continue
			}
			result.Entry = &entry
			result.Status.State, result.Status.DataComplete, result.Status.ConfirmedOffset = domain.UploadStateCompleted, true, copy.size
		} else {
			if err := p.abortUpload(ctx, scope, item.UploadID); err != nil {
				fail(err)
				continue
			}
			result.Status.State = domain.UploadStateAborted
		}
		result.Outcome, result.NextAction = "applied", "done"
	}
	return results, ctx.Err()
}
