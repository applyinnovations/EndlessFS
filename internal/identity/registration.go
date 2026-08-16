package identity

import (
	"context"
	"errors"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/model"
)

func (s *Service) inviteUsable(invite model.Invite) bool {
	return invite.Uses == 0 && invite.RevokedAt == nil && (invite.ExpiresAt == nil || s.clock.Now().Before(*invite.ExpiresAt))
}

func (s *Service) recoveryUsable(recovery model.Recovery) bool {
	return recovery.UsedAt == nil && recovery.RevokedAt == nil && s.clock.Now().Before(recovery.ExpiresAt)
}

func (s *Service) verifyRegistrationPolicy(ctx context.Context, ceremony model.Ceremony) error {
	switch ceremony.Flow {
	case model.CeremonyBootstrap:
		if s.bootstrapHash == "" {
			return domain.NewError(domain.ErrorUnavailable, "bootstrap is unavailable")
		}
		if _, _, err := s.repository.Bootstrap(ctx); err == nil || !errors.Is(err, domain.ErrNotFound) {
			return domain.NewError(domain.ErrorUnavailable, "bootstrap is unavailable")
		}
		if _, _, err := s.repository.FirstAccountMarker(ctx); err == nil || !errors.Is(err, domain.ErrNotFound) {
			return domain.NewError(domain.ErrorUnavailable, "bootstrap is unavailable")
		}
		exists, err := s.repository.UserExists(ctx)
		if err != nil || exists {
			return domain.NewError(domain.ErrorUnavailable, "bootstrap is unavailable")
		}
	case model.CeremonyPublic:
		if !s.policy.RegistrationPolicy().AllowPublic {
			return domain.NewError(domain.ErrorUnavailable, "registration is unavailable")
		}
	case model.CeremonyInvite:
		if !s.policy.RegistrationPolicy().AllowInvite {
			return domain.NewError(domain.ErrorUnavailable, "registration is unavailable")
		}
		invite, _, err := s.repository.InviteByHash(ctx, ceremony.BearerTokenHash)
		if err != nil || !s.inviteUsable(invite) {
			return domain.NewError(domain.ErrorUnavailable, "registration is unavailable")
		}
	case model.CeremonyRecovery:
		recovery, _, err := s.repository.RecoveryByHash(ctx, ceremony.BearerTokenHash)
		if err != nil || !s.recoveryUsable(recovery) || ceremony.UserID == nil || recovery.TargetUserID != *ceremony.UserID {
			return domain.NewError(domain.ErrorUnavailable, "recovery is unavailable")
		}
	case model.CeremonyAddPasskey:
		account, _, err := s.repository.Account(ctx, *ceremony.UserID)
		if err != nil || account.Status != model.AccountEnabled {
			return domain.NewError(domain.ErrorUnauthorized, "account is unavailable")
		}
	default:
		return domain.NewError(domain.ErrorInvalid, "unsupported registration flow")
	}
	return nil
}

func (s *Service) claimRegistration(ctx context.Context, ceremony model.Ceremony, operation model.RegistrationOperation) error {
	now := s.clock.Now()
	if ceremony.Flow == model.CeremonyBootstrap || ceremony.Flow == model.CeremonyPublic || ceremony.Flow == model.CeremonyInvite {
		marker := model.FirstAccountMarker{
			SchemaVersion: model.SchemaVersion, Flow: ceremony.Flow,
			OperationID: operation.OperationID, UserID: operation.UserID, CreatedAt: now,
		}
		if err := s.repository.CreateFirstAccountMarker(ctx, marker); err != nil && !errors.Is(err, domain.ErrConflict) {
			return err
		}
		existing, _, err := s.repository.FirstAccountMarker(ctx)
		if err != nil {
			return err
		}
		if ceremony.Flow == model.CeremonyBootstrap && existing.OperationID != operation.OperationID {
			return domain.NewError(domain.ErrorConflict, "bootstrap was already claimed")
		}
	}
	switch ceremony.Flow {
	case model.CeremonyBootstrap:
		claim := model.BootstrapState{
			SchemaVersion: model.SchemaVersion, Status: model.OperationClaimed, Operation: operation,
		}
		if err := s.repository.CreateBootstrap(ctx, claim); err != nil {
			return domain.NewError(domain.ErrorConflict, "bootstrap was already claimed")
		}
	case model.CeremonyInvite:
		invite, version, err := s.repository.InviteByHash(ctx, ceremony.BearerTokenHash)
		if err != nil || !s.inviteUsable(invite) {
			return domain.NewError(domain.ErrorUnavailable, "registration is unavailable")
		}
		invite.Uses = 1
		invite.UsedAt = &now
		invite.UsedByUserID = &operation.UserID
		invite.OperationID = operation.OperationID
		if _, err := s.repository.UpdateInvite(ctx, invite, version); err != nil {
			return domain.NewError(domain.ErrorConflict, "invite was already consumed")
		}
	case model.CeremonyRecovery:
		recovery, version, err := s.repository.RecoveryByHash(ctx, ceremony.BearerTokenHash)
		if err != nil || !s.recoveryUsable(recovery) || recovery.TargetUserID != operation.UserID {
			return domain.NewError(domain.ErrorUnavailable, "recovery is unavailable")
		}
		recovery.UsedAt = &now
		recovery.OperationID = operation.OperationID
		if _, err := s.repository.UpdateRecovery(ctx, recovery, version); err != nil {
			return domain.NewError(domain.ErrorConflict, "recovery was already consumed")
		}
	case model.CeremonyPublic, model.CeremonyAddPasskey:
		// The consumed ceremony record is the conditional claim.
	default:
		return domain.NewError(domain.ErrorInvalid, "unsupported registration flow")
	}
	return nil
}

func (s *Service) resumeRegistration(ctx context.Context, ceremony model.Ceremony) (RegistrationComplete, error) {
	if ceremony.OperationID == "" {
		return RegistrationComplete{}, domain.NewError(domain.ErrorConflict, "registration ceremony was consumed")
	}
	operation, _, err := s.repository.RegistrationOperation(ctx, ceremony.OperationID)
	if err != nil || operation.UserID != *ceremony.UserID || operation.Flow != ceremony.Flow {
		return RegistrationComplete{}, domain.NewError(domain.ErrorConflict, "registration operation is unavailable")
	}
	if err := s.registrationClaimOwned(ctx, ceremony, operation); err != nil {
		return RegistrationComplete{}, err
	}
	if err := s.materializeRegistration(ctx, operation); err != nil {
		return RegistrationComplete{}, err
	}
	return RegistrationComplete{UserID: operation.UserID, CredentialID: operation.Credential.CredentialID, Flow: operation.Flow}, nil
}

func (s *Service) registrationClaimOwned(ctx context.Context, ceremony model.Ceremony, operation model.RegistrationOperation) error {
	switch ceremony.Flow {
	case model.CeremonyBootstrap:
		marker, _, markerErr := s.repository.FirstAccountMarker(ctx)
		bootstrap, _, err := s.repository.Bootstrap(ctx)
		if markerErr != nil || marker.OperationID != operation.OperationID || err != nil || bootstrap.Operation.OperationID != operation.OperationID {
			return domain.NewError(domain.ErrorConflict, "bootstrap claim is unavailable")
		}
	case model.CeremonyInvite:
		invite, _, err := s.repository.InviteByHash(ctx, ceremony.BearerTokenHash)
		if err != nil || invite.OperationID != operation.OperationID || invite.UsedByUserID == nil || *invite.UsedByUserID != operation.UserID {
			return domain.NewError(domain.ErrorConflict, "invite claim is unavailable")
		}
	case model.CeremonyRecovery:
		recovery, _, err := s.repository.RecoveryByHash(ctx, ceremony.BearerTokenHash)
		if err != nil || recovery.OperationID != operation.OperationID || recovery.TargetUserID != operation.UserID {
			return domain.NewError(domain.ErrorConflict, "recovery claim is unavailable")
		}
	case model.CeremonyPublic, model.CeremonyAddPasskey:
		return nil
	default:
		return domain.NewError(domain.ErrorInvalid, "unsupported registration flow")
	}
	return nil
}

func (s *Service) materializeRegistration(ctx context.Context, operation model.RegistrationOperation) error {
	if operation.Status == model.OperationCommitted {
		return nil
	}
	if err := s.createOrConfirmCredential(ctx, operation.Credential); err != nil {
		return err
	}
	switch operation.Flow {
	case model.CeremonyBootstrap, model.CeremonyPublic, model.CeremonyInvite:
		if err := s.createOrConfirmProfile(ctx, model.Profile{UserID: operation.UserID, DisplayName: operation.DisplayName}); err != nil {
			return err
		}
		if err := s.createOrConfirmAccount(ctx, operation); err != nil {
			return err
		}
		if operation.Flow == model.CeremonyBootstrap {
			if err := s.createOrConfirmFirstAdmin(ctx, operation.UserID); err != nil {
				return err
			}
		}
		if err := s.enableMaterializedAccount(ctx, operation.UserID); err != nil {
			return err
		}
	case model.CeremonyRecovery:
		if err := s.sessions.RevokeUser(ctx, operation.UserID); err != nil {
			return err
		}
	case model.CeremonyAddPasskey:
		// Credential creation is the only persistent change.
	default:
		return domain.NewError(domain.ErrorInvalid, "unsupported registration operation")
	}
	return s.commitRegistration(ctx, operation)
}

func (s *Service) createOrConfirmCredential(ctx context.Context, credential model.Credential) error {
	if err := s.repository.CreateCredential(ctx, credential); err != nil && !errors.Is(err, domain.ErrConflict) {
		return err
	}
	existing, _, err := s.repository.Credential(ctx, credential.CredentialID)
	if err != nil || existing.UserID != credential.UserID || existing.PublicKey != credential.PublicKey {
		return domain.NewError(domain.ErrorConflict, "credential is already registered")
	}
	return s.indexCredential(ctx, credential.UserID, credential.CredentialID)
}

func (s *Service) indexCredential(ctx context.Context, userID domain.UserID, credentialID string) error {
	for attempts := 0; attempts < 8; attempts++ {
		index, version, err := s.repository.CredentialIndex(ctx, userID)
		if errors.Is(err, domain.ErrNotFound) {
			index = model.CredentialIndex{SchemaVersion: model.SchemaVersion, UserID: userID, CredentialIDs: []string{credentialID}}
			if err := s.repository.CreateCredentialIndex(ctx, index); err == nil {
				return nil
			} else if errors.Is(err, domain.ErrConflict) {
				continue
			} else {
				return err
			}
		}
		if err != nil {
			return err
		}
		for _, existingID := range index.CredentialIDs {
			if existingID == credentialID {
				return nil
			}
		}
		index.CredentialIDs = append(index.CredentialIDs, credentialID)
		if _, err := s.repository.UpdateCredentialIndex(ctx, index, version); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
	}
	return domain.NewError(domain.ErrorConflict, "credential index changed concurrently")
}

func (s *Service) createOrConfirmProfile(ctx context.Context, profile model.Profile) error {
	if err := s.repository.CreateProfile(ctx, profile); err == nil {
		return nil
	} else if !errors.Is(err, domain.ErrConflict) {
		return err
	}
	existing, _, err := s.repository.Profile(ctx, profile.UserID)
	if err != nil || existing != profile {
		return domain.NewError(domain.ErrorConflict, "profile materialization conflict")
	}
	return nil
}

func (s *Service) createOrConfirmAccount(ctx context.Context, operation model.RegistrationOperation) error {
	account := model.Account{
		SchemaVersion: model.SchemaVersion, UserID: operation.UserID,
		Status: model.AccountDisabled, CreatedAt: operation.CreatedAt, UpdatedAt: operation.CreatedAt,
	}
	if err := s.repository.CreateAccount(ctx, account); err == nil {
		return nil
	} else if !errors.Is(err, domain.ErrConflict) {
		return err
	}
	existing, _, err := s.repository.Account(ctx, operation.UserID)
	if err != nil || existing.UserID != operation.UserID {
		return domain.NewError(domain.ErrorConflict, "account materialization conflict")
	}
	return nil
}

func (s *Service) createOrConfirmFirstAdmin(ctx context.Context, userID domain.UserID) error {
	record := model.AdminRoles{SchemaVersion: model.SchemaVersion, UserIDs: []domain.UserID{userID}}
	if err := s.repository.CreateAdminRoles(ctx, record); err == nil {
		return nil
	} else if !errors.Is(err, domain.ErrConflict) {
		return err
	}
	existing, _, err := s.repository.AdminRoles(ctx)
	if err != nil || len(existing.UserIDs) != 1 || existing.UserIDs[0] != userID {
		return domain.NewError(domain.ErrorConflict, "administrator materialization conflict")
	}
	return nil
}

func (s *Service) enableMaterializedAccount(ctx context.Context, userID domain.UserID) error {
	for attempts := 0; attempts < 4; attempts++ {
		account, version, err := s.repository.Account(ctx, userID)
		if err != nil {
			return err
		}
		if account.Status == model.AccountEnabled {
			return nil
		}
		account.Status = model.AccountEnabled
		account.UpdatedAt = s.clock.Now()
		if _, err := s.repository.UpdateAccount(ctx, account, version); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
	}
	return domain.NewError(domain.ErrorConflict, "account changed concurrently")
}

func (s *Service) commitRegistration(ctx context.Context, operation model.RegistrationOperation) error {
	current, version, err := s.repository.RegistrationOperation(ctx, operation.OperationID)
	if err != nil {
		return err
	}
	if current.Status == model.OperationCommitted {
		return nil
	}
	now := s.clock.Now()
	current.Status = model.OperationCommitted
	current.CommittedAt = &now
	if _, err := s.repository.UpdateRegistrationOperation(ctx, current, version); err != nil {
		return err
	}
	if current.Flow == model.CeremonyBootstrap {
		bootstrap, bootstrapVersion, err := s.repository.Bootstrap(ctx)
		if err != nil {
			return err
		}
		if bootstrap.Status == model.OperationCommitted {
			return nil
		}
		bootstrap.Status = model.OperationCommitted
		bootstrap.Operation = current
		bootstrap.CompletedAt = &now
		if _, err := s.repository.UpdateBootstrap(ctx, bootstrap, bootstrapVersion); err != nil {
			return err
		}
	}
	return nil
}
