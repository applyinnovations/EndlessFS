package memory

import (
	"context"
	"errors"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

// The mock models item receipts under its in-memory admission lock. Production
// metadata coalescing and native I/O limits are qualified in the portable engine.
func (p *Provider) AdmitUploads(ctx context.Context, scope domain.Scope, requested []domain.CreateUploadRequest) ([]domain.UploadAdmissionResult, error) {
	if err := validateContextScope(ctx, scope); err != nil {
		return nil, err
	}
	if len(requested) < 1 || len(requested) > domain.MaxUploadLifecycleItems {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid admission page size")
	}
	seen := make(map[string]bool, len(requested))
	for _, request := range requested {
		if len(request.IdempotencyKey) < 16 || validateIdempotencyKey(request.IdempotencyKey) != nil || seen[request.IdempotencyKey] {
			return nil, domain.NewError(domain.ErrorInvalid, "invalid or repeated admission key")
		}
		seen[request.IdempotencyKey] = true
	}
	p.uploadBatchMu.Lock()
	defer p.uploadBatchMu.Unlock()
	results := make([]domain.UploadAdmissionResult, len(requested))
	for i, request := range requested {
		result := &results[i]
		result.Index, result.ID = i, request.IdempotencyKey
		p.mu.Lock()
		_, existed := p.uploadIdempotency[idempotencyKey(scope.UserID(), OperationCreateUpload, request.IdempotencyKey)]
		p.mu.Unlock()
		capability, err := p.createUpload(ctx, scope, request)
		if err != nil {
			result.ErrorKind = domain.KindOf(err)
			result.Outcome, result.NextAction = "rejected", "attention"
			result.Retryable = errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrRateLimited) || errors.Is(err, domain.ErrInternal)
			if result.Retryable {
				result.Outcome, result.NextAction = "retryable", "retry"
			}
			continue
		}
		status, err := p.UploadStatus(ctx, scope, capability.UploadID)
		if err != nil {
			return nil, err
		}
		result.UploadID, result.Status = capability.UploadID, &status
		switch status.State {
		case domain.UploadStateCompleted:
			result.Outcome, result.NextAction = "completed", "done"
		case domain.UploadStateExpired, domain.UploadStateAborted:
			result.Outcome, result.NextAction = string(status.State), "restart"
		default:
			result.Outcome, result.NextAction = "accepted", "transfer"
			if existed {
				result.Outcome = "already_accepted"
			}
			if status.DataComplete {
				result.NextAction = "complete"
			} else {
				result.Capability = &capability
			}
		}
	}
	return results, ctx.Err()
}
