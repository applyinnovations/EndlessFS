package drive

import (
	"context"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/provider"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
)

func (s *Service) AdmitUploads(ctx context.Context, userID domain.UserID, requests []domain.CreateUploadRequest) (outcome []domain.UploadAdmissionResult, err error) {
	ctx, activity := telemetry.Start(ctx, telemetry.UploadAdmission, telemetry.Application)
	defer telemetry.Finish(activity, &err)
	if len(requests) < 1 || len(requests) > domain.MaxUploadLifecycleItems {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid admission page size")
	}
	seen := make(map[string]bool, len(requests))
	for _, request := range requests {
		if err := validateIdempotencyKey(request.IdempotencyKey); err != nil {
			return nil, err
		}
		if seen[request.IdempotencyKey] {
			return nil, domain.NewError(domain.ErrorInvalid, "admission repeats an item key")
		}
		seen[request.IdempotencyKey] = true
	}
	scope, err := liveScope(userID)
	if err != nil {
		return nil, err
	}
	storage, ok := s.storage.(provider.UploadAdmissionStorage)
	if !ok {
		return nil, domain.NewError(domain.ErrorUnavailable, "independent upload admission is unavailable")
	}
	return storage.AdmitUploads(ctx, scope, requests)
}
