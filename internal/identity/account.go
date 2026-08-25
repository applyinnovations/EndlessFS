package identity

import (
	"context"
	"errors"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/model"
)

type CurrentUser struct {
	UserID      domain.UserID      `json:"userID"`
	DisplayName domain.DisplayName `json:"displayName"`
	Roles       []string           `json:"roles"`
}

type PasskeySummary struct {
	CredentialID string                  `json:"credentialID"`
	Label        *domain.CredentialLabel `json:"label,omitempty"`
	CreatedAt    time.Time               `json:"createdAt"`
	LastUsedAt   time.Time               `json:"lastUsedAt"`
}

func (s *Service) CurrentUser(ctx context.Context, session auth.AuthenticatedSession) (CurrentUser, error) {
	profile, _, err := s.repository.Profile(ctx, session.Record.UserID)
	if err != nil {
		return CurrentUser{}, err
	}
	roles := []string(nil)
	if isAdmin, err := s.isAdmin(ctx, profile.UserID); err != nil {
		return CurrentUser{}, err
	} else if isAdmin {
		roles = []string{"admin"}
	}
	return CurrentUser{UserID: profile.UserID, DisplayName: profile.DisplayName, Roles: roles}, nil
}

func (s *Service) UpdateDisplayName(ctx context.Context, session auth.AuthenticatedSession, value string) (model.Profile, error) {
	displayName, err := domain.ParseDisplayName(value)
	if err != nil {
		return model.Profile{}, err
	}
	for attempts := 0; attempts < 8; attempts++ {
		profile, version, err := s.repository.Profile(ctx, session.Record.UserID)
		if err != nil {
			return model.Profile{}, err
		}
		profile.DisplayName = displayName
		if _, err := s.repository.UpdateProfile(ctx, profile, version); err == nil {
			return profile, nil
		} else if !errors.Is(err, domain.ErrPreconditionFailed) {
			return model.Profile{}, err
		}
	}
	return model.Profile{}, domain.NewError(domain.ErrorConflict, "profile changed concurrently")
}

func (s *Service) Passkeys(ctx context.Context, session auth.AuthenticatedSession) ([]PasskeySummary, error) {
	credentials, err := s.repository.Credentials(ctx, session.Record.UserID)
	if err != nil {
		return nil, err
	}
	result := make([]PasskeySummary, len(credentials))
	for index, credential := range credentials {
		result[index] = PasskeySummary{
			CredentialID: credential.CredentialID, Label: credential.Label,
			CreatedAt: credential.CreatedAt, LastUsedAt: credential.LastUsedAt,
		}
	}
	return result, nil
}

func (s *Service) RemovePasskey(ctx context.Context, session auth.AuthenticatedSession, credentialID string) error {
	now := s.clock.Now()
	if now.Before(session.Record.CreatedAt) || now.Sub(session.Record.CreatedAt) > CeremonyLifetime {
		return domain.NewError(domain.ErrorPreconditionFailed, "recent authentication is required")
	}
	return s.repository.RemoveCredentialAtomic(ctx, session.Record.UserID, credentialID)
}

func (s *Service) isAdmin(ctx context.Context, userID domain.UserID) (bool, error) {
	roles, _, err := s.repository.AdminRoles(ctx)
	if errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, adminID := range roles.UserIDs {
		if adminID == userID {
			return true, nil
		}
	}
	return false, nil
}
