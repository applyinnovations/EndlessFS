package drive

import (
	"context"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/provider"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
)

func (s *Service) PrepareDirectories(ctx context.Context, owner domain.UserID, paths []domain.UserPath) (outcome []domain.DirectoryPreparationResult, err error) {
	ctx, activity := telemetry.Start(ctx, telemetry.DirectoryPreparation, telemetry.Application)
	defer telemetry.Finish(activity, &err)
	scope, err := liveScope(owner)
	if err != nil {
		return nil, err
	}
	if err := domain.ValidateDirectoryPreparationPaths(paths); err != nil {
		return nil, err
	}
	storage, ok := s.storage.(provider.DirectoryPreparationStorage)
	if !ok {
		return nil, domain.NewError(domain.ErrorUnavailable, "directory preparation is unavailable")
	}
	return storage.PrepareDirectories(ctx, scope, paths)
}
