package identity

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
)

type CreatedInvite struct {
	InviteID string       `json:"inviteID"`
	Link     secret.Value `json:"-"`
	Record   model.Invite `json:"-"`
}

type InviteSummary struct {
	InviteID        string         `json:"inviteID"`
	CreatedByUserID domain.UserID  `json:"createdByUserID"`
	CreatedAt       time.Time      `json:"createdAt"`
	ExpiresAt       *time.Time     `json:"expiresAt,omitempty"`
	UsedAt          *time.Time     `json:"usedAt,omitempty"`
	UsedByUserID    *domain.UserID `json:"usedByUserID,omitempty"`
	RevokedAt       *time.Time     `json:"revokedAt,omitempty"`
}

type CreatedRecovery struct {
	RecoveryID string         `json:"recoveryID"`
	Link       secret.Value   `json:"-"`
	Record     model.Recovery `json:"-"`
}

type AdminUserSummary struct {
	UserID      domain.UserID       `json:"userID"`
	DisplayName domain.DisplayName  `json:"displayName"`
	Status      model.AccountStatus `json:"status"`
	Admin       bool                `json:"admin"`
	CreatedAt   time.Time           `json:"createdAt"`
}

type AdminUserPage struct {
	Users      []AdminUserSummary `json:"users"`
	NextCursor string             `json:"nextCursor,omitempty"`
}

func (s *Service) CreateInvite(ctx context.Context, actor auth.AuthenticatedSession, expiresAt *time.Time, idempotencyKey string) (CreatedInvite, error) {
	if err := s.requireAdmin(ctx, actor.Record.UserID); err != nil {
		return CreatedInvite{}, err
	}
	if !s.policy.RegistrationPolicy().AllowInvite {
		return CreatedInvite{}, domain.NewError(domain.ErrorUnavailable, "invite registration is disabled")
	}
	now := s.clock.Now()
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return CreatedInvite{}, err
	}
	if expiresAt != nil {
		value := expiresAt.UTC()
		expiresAt = &value
		if !now.Before(value) {
			return CreatedInvite{}, domain.NewError(domain.ErrorInvalid, "invite expiry must be in the future")
		}
	}
	inviteID, err := s.ids.OpaqueID()
	if err != nil {
		return CreatedInvite{}, err
	}
	token, err := s.ids.BearerToken()
	if err != nil {
		return CreatedInvite{}, err
	}
	record := model.Invite{
		SchemaVersion: model.SchemaVersion, InviteID: inviteID, TokenHash: secret.Hash(token),
		CreatedByUserID: actor.Record.UserID, CreatedAt: now, ExpiresAt: expiresAt,
		MaxUses: 1,
	}
	fingerprint := secret.Hash("invite\x00" + optionalTimeFingerprint(expiresAt))
	claimed, err := s.claimIdempotency(ctx, actor.Record.UserID, idempotencyKey, model.IdempotencyInvite, fingerprint, inviteID, &record)
	if err != nil {
		return CreatedInvite{}, err
	}
	if !claimed {
		return CreatedInvite{}, domain.NewError(domain.ErrorConflict, "idempotent invite already exists; raw link is no longer available")
	}
	if err := s.createOrConfirmInvite(ctx, record); err != nil {
		return CreatedInvite{}, err
	}
	if err := s.commitIdempotency(ctx, actor.Record.UserID, idempotencyKey); err != nil {
		return CreatedInvite{}, err
	}
	return CreatedInvite{InviteID: inviteID, Link: secret.Value(s.baseURL + "/register/invite/" + token), Record: record}, nil
}

func (s *Service) Invites(ctx context.Context, actor auth.AuthenticatedSession) ([]InviteSummary, error) {
	if err := s.requireAdmin(ctx, actor.Record.UserID); err != nil {
		return nil, err
	}
	records, err := s.repository.Invites(ctx)
	if err != nil {
		return nil, err
	}
	summaries := make([]InviteSummary, len(records))
	for index, record := range records {
		summaries[index] = InviteSummary{
			InviteID: record.InviteID, CreatedByUserID: record.CreatedByUserID,
			CreatedAt: record.CreatedAt, ExpiresAt: record.ExpiresAt,
			UsedAt: record.UsedAt, UsedByUserID: record.UsedByUserID, RevokedAt: record.RevokedAt,
		}
	}
	sort.Slice(summaries, func(left, right int) bool { return summaries[left].CreatedAt.Before(summaries[right].CreatedAt) })
	return summaries, nil
}

func (s *Service) RevokeInvite(ctx context.Context, actor auth.AuthenticatedSession, inviteID string) error {
	if err := s.requireAdmin(ctx, actor.Record.UserID); err != nil {
		return err
	}
	for attempts := 0; attempts < 8; attempts++ {
		record, version, err := s.repository.InviteByID(ctx, inviteID)
		if err != nil {
			return err
		}
		if record.RevokedAt != nil {
			return nil
		}
		now := s.clock.Now()
		record.RevokedAt = &now
		if _, err := s.repository.UpdateInvite(ctx, record, version); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
	}
	return domain.NewError(domain.ErrorConflict, "invite changed concurrently")
}

func (s *Service) CreateRecovery(ctx context.Context, actor auth.AuthenticatedSession, target domain.UserID, ttl time.Duration, idempotencyKey string) (CreatedRecovery, error) {
	if err := s.requireAdmin(ctx, actor.Record.UserID); err != nil {
		return CreatedRecovery{}, err
	}
	if ttl <= 0 || ttl > time.Hour {
		return CreatedRecovery{}, domain.NewError(domain.ErrorInvalid, "recovery lifetime must be at most one hour")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return CreatedRecovery{}, err
	}
	if _, _, err := s.repository.Account(ctx, target); err != nil {
		return CreatedRecovery{}, domain.NewError(domain.ErrorNotFound, "user not found")
	}
	recoveryID, err := s.ids.OpaqueID()
	if err != nil {
		return CreatedRecovery{}, err
	}
	rawToken, err := s.ids.BearerToken()
	if err != nil {
		return CreatedRecovery{}, err
	}
	token, err := secret.ScopeBearerToken(target, rawToken)
	if err != nil {
		return CreatedRecovery{}, err
	}
	now := s.clock.Now()
	record := model.Recovery{
		SchemaVersion: model.SchemaVersion, RecoveryID: recoveryID, TokenHash: secret.Hash(token),
		TargetUserID: target, CreatedByUserID: actor.Record.UserID,
		CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
	fingerprint := secret.Hash("recovery\x00" + target.String() + "\x00" + ttl.String())
	claimed, err := s.claimIdempotency(ctx, actor.Record.UserID, idempotencyKey, model.IdempotencyRecovery, fingerprint, recoveryID, &record)
	if err != nil {
		return CreatedRecovery{}, err
	}
	if !claimed {
		return CreatedRecovery{}, domain.NewError(domain.ErrorConflict, "idempotent recovery already exists; raw link is no longer available")
	}
	if err := s.createOrConfirmRecovery(ctx, record); err != nil {
		return CreatedRecovery{}, err
	}
	if err := s.commitIdempotency(ctx, actor.Record.UserID, idempotencyKey); err != nil {
		return CreatedRecovery{}, err
	}
	return CreatedRecovery{RecoveryID: recoveryID, Link: secret.Value(s.baseURL + "/recover/" + token), Record: record}, nil
}

func validateIdempotencyKey(value string) error {
	if len(value) < 16 || len(value) > 128 || strings.ContainsAny(value, "\r\n\x00") {
		return domain.NewError(domain.ErrorInvalid, "a valid idempotency key is required")
	}
	return nil
}

func optionalTimeFingerprint(value *time.Time) string {
	if value == nil {
		return "none"
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func (s *Service) claimIdempotency(ctx context.Context, owner domain.UserID, rawKey string, kind model.IdempotencyKind, fingerprint, resourceID string, resource any) (bool, error) {
	resourceJSON, err := state.EncodeJSON(resource)
	if err != nil {
		return false, err
	}
	record := model.IdempotencyRecord{
		SchemaVersion: model.SchemaVersion, OwnerUserID: owner, KeyHash: secret.Hash(rawKey),
		Kind: kind, Fingerprint: fingerprint, ResourceID: resourceID, Resource: resourceJSON,
		Status: model.OperationClaimed, CreatedAt: s.clock.Now(),
	}
	if err := s.repository.CreateIdempotency(ctx, record); err == nil {
		return true, nil
	} else if !errors.Is(err, domain.ErrConflict) {
		return false, err
	}
	existing, _, err := s.repository.Idempotency(ctx, owner, record.KeyHash)
	if err != nil || existing.Kind != kind || existing.Fingerprint != fingerprint {
		return false, domain.NewError(domain.ErrorConflict, "idempotency key was reused with a different request")
	}
	if err := s.resumeIdempotencyResource(ctx, existing); err != nil {
		return false, err
	}
	if err := s.commitIdempotency(ctx, owner, rawKey); err != nil {
		return false, err
	}
	return false, nil
}

func (s *Service) resumeIdempotencyResource(ctx context.Context, record model.IdempotencyRecord) error {
	switch record.Kind {
	case model.IdempotencyInvite:
		var invite model.Invite
		if err := state.DecodeJSON(record.Resource, &invite); err != nil {
			return err
		}
		return s.createOrConfirmInvite(ctx, invite)
	case model.IdempotencyRecovery:
		var recovery model.Recovery
		if err := state.DecodeJSON(record.Resource, &recovery); err != nil {
			return err
		}
		return s.createOrConfirmRecovery(ctx, recovery)
	default:
		return domain.NewError(domain.ErrorInternal, "unknown idempotency resource")
	}
}

func (s *Service) createOrConfirmInvite(ctx context.Context, record model.Invite) error {
	if err := s.repository.CreateInvite(ctx, record); err == nil {
		return nil
	} else if !errors.Is(err, domain.ErrConflict) {
		return err
	}
	existing, _, err := s.repository.InviteByHash(ctx, record.TokenHash)
	if err != nil || existing.InviteID != record.InviteID {
		return domain.NewError(domain.ErrorConflict, "invite materialization conflict")
	}
	return nil
}

func (s *Service) createOrConfirmRecovery(ctx context.Context, record model.Recovery) error {
	if err := s.repository.CreateRecovery(ctx, record); err == nil {
		return nil
	} else if !errors.Is(err, domain.ErrConflict) {
		return err
	}
	existing, _, err := s.repository.RecoveryByHash(ctx, record.TargetUserID, record.TokenHash)
	if err != nil || existing.RecoveryID != record.RecoveryID {
		return domain.NewError(domain.ErrorConflict, "recovery materialization conflict")
	}
	return nil
}

func (s *Service) commitIdempotency(ctx context.Context, owner domain.UserID, rawKey string) error {
	record, version, err := s.repository.Idempotency(ctx, owner, secret.Hash(rawKey))
	if err != nil {
		return err
	}
	if record.Status == model.OperationCommitted {
		return nil
	}
	now := s.clock.Now()
	record.Status = model.OperationCommitted
	record.CommittedAt = &now
	_, err = s.repository.UpdateIdempotency(ctx, record, version)
	return err
}

func (s *Service) AdminUsers(ctx context.Context, actor auth.AuthenticatedSession) ([]AdminUserSummary, error) {
	page, err := s.AdminUsersPage(ctx, actor, 1000, "")
	return page.Users, err
}

func (s *Service) AdminUsersPage(ctx context.Context, actor auth.AuthenticatedSession, limit int, cursor string) (AdminUserPage, error) {
	if err := s.requireAdmin(ctx, actor.Record.UserID); err != nil {
		return AdminUserPage{}, err
	}
	if limit == 0 {
		limit = 200
	}
	if limit < 1 || limit > 1000 {
		return AdminUserPage{}, domain.NewError(domain.ErrorInvalid, "page limit must be between 1 and 1000")
	}
	offset, err := s.adminCursorOffset(cursor)
	if err != nil {
		return AdminUserPage{}, err
	}
	profiles, err := s.repository.Profiles(ctx)
	if err != nil {
		return AdminUserPage{}, err
	}
	result := make([]AdminUserSummary, 0, len(profiles))
	for _, profile := range profiles {
		account, _, err := s.repository.Account(ctx, profile.UserID)
		if err != nil {
			return AdminUserPage{}, err
		}
		admin, err := s.isAdmin(ctx, profile.UserID)
		if err != nil {
			return AdminUserPage{}, err
		}
		result = append(result, AdminUserSummary{
			UserID: profile.UserID, DisplayName: profile.DisplayName,
			Status: account.Status, Admin: admin, CreatedAt: account.CreatedAt,
		})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].UserID.String() < result[right].UserID.String() })
	if offset > len(result) {
		return AdminUserPage{}, domain.NewError(domain.ErrorInvalid, "invalid admin user cursor")
	}
	end := min(offset+limit, len(result))
	page := AdminUserPage{Users: append([]AdminUserSummary(nil), result[offset:end]...)}
	if end < len(result) {
		payload := strconv.Itoa(end)
		protected := s.sessions.Protect("admin-users\x00" + payload)
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(payload + "." + protected))
	}
	return page, nil
}

func (s *Service) adminCursorOffset(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, domain.NewError(domain.ErrorInvalid, "invalid admin user cursor")
	}
	parts := strings.Split(string(decoded), ".")
	if len(parts) != 2 || !s.sessions.MatchesProtected("admin-users\x00"+parts[0], parts[1]) {
		return 0, domain.NewError(domain.ErrorInvalid, "invalid admin user cursor")
	}
	offset, err := strconv.Atoi(parts[0])
	if err != nil || offset < 0 {
		return 0, domain.NewError(domain.ErrorInvalid, "invalid admin user cursor")
	}
	return offset, nil
}

func (s *Service) DisableUser(ctx context.Context, actor auth.AuthenticatedSession, target domain.UserID) error {
	for attempts := 0; attempts < 8; attempts++ {
		roles, rolesVersion, actorAccount, actorVersion, err := s.adminAuthoritySnapshot(ctx, actor.Record.UserID)
		if err != nil {
			return err
		}
		targetAccount, targetVersion, err := s.repository.Account(ctx, target)
		if err != nil {
			return err
		}
		wasAdmin := containsUserID(roles.UserIDs, target)
		if targetAccount.Status == model.AccountDisabled && !wasAdmin {
			return nil
		}
		if wasAdmin {
			enabledAdmins, countErr := s.enabledAdminCount(ctx, roles)
			if countErr != nil {
				return countErr
			}
			if enabledAdmins <= 1 {
				return domain.NewError(domain.ErrorPreconditionFailed, "the final enabled administrator cannot be removed")
			}
			roles.UserIDs = withoutUserID(roles.UserIDs, target)
		}
		if targetAccount.Status != model.AccountDisabled {
			targetAccount.Status = model.AccountDisabled
			targetAccount.UpdatedAt = s.clock.Now()
			if err := advanceAuthEpoch(&targetAccount); err != nil {
				return err
			}
		}
		if err := s.commitAdminAccountMutation(ctx, "disable", actor.Record.UserID, target, roles, rolesVersion, actorAccount, actorVersion, targetAccount, targetVersion); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrPreconditionFailed) && !errors.Is(err, domain.ErrConflict) {
			return err
		}
	}
	return domain.NewError(domain.ErrorConflict, "account changed concurrently")
}

func (s *Service) EnableUser(ctx context.Context, actor auth.AuthenticatedSession, target domain.UserID) error {
	for attempts := 0; attempts < 8; attempts++ {
		roles, rolesVersion, actorAccount, actorVersion, err := s.adminAuthoritySnapshot(ctx, actor.Record.UserID)
		if err != nil {
			return err
		}
		account, version, err := s.repository.Account(ctx, target)
		if err != nil {
			return err
		}
		if account.Status == model.AccountEnabled {
			return nil
		}
		account.Status = model.AccountEnabled
		account.UpdatedAt = s.clock.Now()
		if err := s.commitAdminAccountMutation(ctx, "enable", actor.Record.UserID, target, roles, rolesVersion, actorAccount, actorVersion, account, version); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrPreconditionFailed) && !errors.Is(err, domain.ErrConflict) {
			return err
		}
	}
	return domain.NewError(domain.ErrorConflict, "account changed concurrently")
}

func (s *Service) GrantAdmin(ctx context.Context, actor auth.AuthenticatedSession, target domain.UserID) error {
	for attempts := 0; attempts < 8; attempts++ {
		roles, rolesVersion, actorAccount, actorVersion, err := s.adminAuthoritySnapshot(ctx, actor.Record.UserID)
		if err != nil {
			return err
		}
		account, accountVersion, err := s.repository.Account(ctx, target)
		if err != nil || account.Status != model.AccountEnabled {
			return domain.NewError(domain.ErrorPreconditionFailed, "only enabled users can become administrators")
		}
		if containsUserID(roles.UserIDs, target) {
			return nil
		}
		roles.UserIDs = append(roles.UserIDs, target)
		if err := advanceAuthEpoch(&account); err != nil {
			return err
		}
		if err := s.commitAdminAccountMutation(ctx, "grant", actor.Record.UserID, target, roles, rolesVersion, actorAccount, actorVersion, account, accountVersion); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrPreconditionFailed) && !errors.Is(err, domain.ErrConflict) {
			return err
		}
	}
	return domain.NewError(domain.ErrorConflict, "administrator roles changed concurrently")
}

func (s *Service) RevokeAdmin(ctx context.Context, actor auth.AuthenticatedSession, target domain.UserID) error {
	for attempts := 0; attempts < 8; attempts++ {
		roles, rolesVersion, actorAccount, actorVersion, err := s.adminAuthoritySnapshot(ctx, actor.Record.UserID)
		if err != nil {
			return err
		}
		if !containsUserID(roles.UserIDs, target) {
			return nil
		}
		enabledAdmins, err := s.enabledAdminCount(ctx, roles)
		if err != nil {
			return err
		}
		if enabledAdmins <= 1 {
			return domain.NewError(domain.ErrorPreconditionFailed, "the final enabled administrator cannot be removed")
		}
		account, accountVersion, err := s.repository.Account(ctx, target)
		if err != nil {
			return err
		}
		roles.UserIDs = withoutUserID(roles.UserIDs, target)
		if err := advanceAuthEpoch(&account); err != nil {
			return err
		}
		if err := s.commitAdminAccountMutation(ctx, "revoke", actor.Record.UserID, target, roles, rolesVersion, actorAccount, actorVersion, account, accountVersion); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrPreconditionFailed) && !errors.Is(err, domain.ErrConflict) {
			return err
		}
	}
	return domain.NewError(domain.ErrorConflict, "administrator roles changed concurrently")
}

func (s *Service) adminAuthoritySnapshot(ctx context.Context, actor domain.UserID) (model.AdminRoles, state.Version, model.Account, state.Version, error) {
	account, accountVersion, err := s.repository.Account(ctx, actor)
	if err != nil || account.Status != model.AccountEnabled {
		return model.AdminRoles{}, "", model.Account{}, "", domain.NewError(domain.ErrorUnauthorized, "administrator access required")
	}
	roles, rolesVersion, err := s.repository.AdminRoles(ctx)
	if err != nil || !containsUserID(roles.UserIDs, actor) {
		return model.AdminRoles{}, "", model.Account{}, "", domain.NewError(domain.ErrorUnauthorized, "administrator access required")
	}
	return roles, rolesVersion, account, accountVersion, nil
}

func (s *Service) enabledAdminCount(ctx context.Context, roles model.AdminRoles) (int, error) {
	enabled := 0
	for _, userID := range roles.UserIDs {
		account, _, err := s.repository.Account(ctx, userID)
		if err != nil {
			return 0, domain.NewError(domain.ErrorInvalid, "administrator role references a missing account")
		}
		if account.Status == model.AccountEnabled {
			enabled++
		}
	}
	return enabled, nil
}

func withoutUserID(values []domain.UserID, target domain.UserID) []domain.UserID {
	updated := make([]domain.UserID, 0, len(values))
	for _, value := range values {
		if value != target {
			updated = append(updated, value)
		}
	}
	return updated
}

func advanceAuthEpoch(account *model.Account) error {
	if account.AuthEpoch == math.MaxUint64 {
		return domain.NewError(domain.ErrorInvalid, "account authentication epoch is exhausted")
	}
	if account.AuthEpoch == 0 {
		account.AuthEpoch = 1
	}
	account.AuthEpoch++
	return nil
}

func (s *Service) commitAdminAccountMutation(ctx context.Context, action string, actor, target domain.UserID, roles model.AdminRoles, rolesVersion state.Version, actorAccount model.Account, actorVersion state.Version, targetAccount model.Account, targetVersion state.Version) error {
	changes := make([]state.Change, 0, 3)
	rolesBody, err := state.EncodeJSON(&roles)
	if err != nil {
		return err
	}
	changes = append(changes, state.Change{Key: state.MustKey(state.NamespaceRoles, "admins"), Requirement: state.RequirementPresent, ExpectedVersion: rolesVersion, Data: rolesBody})
	targetBody, err := state.EncodeJSON(&targetAccount)
	if err != nil {
		return err
	}
	changes = append(changes, state.Change{Key: state.MustKey(state.NamespaceAccounts, target.String()), Requirement: state.RequirementPresent, ExpectedVersion: targetVersion, Data: targetBody})
	if actor != target {
		actorBody, encodeErr := state.EncodeJSON(&actorAccount)
		if encodeErr != nil {
			return encodeErr
		}
		changes = append(changes, state.Change{Key: state.MustKey(state.NamespaceAccounts, actor.String()), Requirement: state.RequirementPresent, ExpectedVersion: actorVersion, Data: actorBody})
	}
	store, err := s.repository.transactionalStore()
	if err != nil {
		return err
	}
	mutationID := "admin-" + action + ":" + secret.Hash(actor.String()+"\x00"+target.String()+"\x00"+string(rolesVersion)+"\x00"+string(targetVersion))
	_, err = store.Transact(ctx, state.Mutation{ID: mutationID, Changes: changes})
	return err
}

func (s *Service) requireAdmin(ctx context.Context, userID domain.UserID) error {
	account, _, err := s.repository.Account(ctx, userID)
	if err != nil || account.Status != model.AccountEnabled {
		return domain.NewError(domain.ErrorUnauthorized, "administrator access required")
	}
	admin, err := s.isAdmin(ctx, userID)
	if err != nil || !admin {
		return domain.NewError(domain.ErrorUnauthorized, "administrator access required")
	}
	return nil
}

func containsUserID(values []domain.UserID, target domain.UserID) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
