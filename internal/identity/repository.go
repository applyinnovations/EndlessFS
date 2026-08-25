// Package identity persists and coordinates EndlessFS account metadata over
// the provider-neutral conditional state store.
package identity

import (
	"context"
	"encoding/base64"
	"errors"
	"math"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
)

type Repository struct {
	store state.Store
}

func NewRepository(store state.Store) *Repository {
	return &Repository{store: store}
}

func (r *Repository) UserExists(ctx context.Context) (bool, error) {
	page, err := r.store.List(ctx, state.MustPrefix(state.NamespaceUsers), state.PageRequest{Limit: 1})
	if err != nil {
		return false, err
	}
	return len(page.Items) != 0, nil
}

func (r *Repository) CreateProfile(ctx context.Context, record model.Profile) error {
	return createRecord(ctx, r.store, state.MustKey(state.NamespaceUsers, record.UserID.String()), &record)
}

func (r *Repository) Profile(ctx context.Context, userID domain.UserID) (model.Profile, state.Version, error) {
	return getRecord[model.Profile](ctx, r.store, state.MustKey(state.NamespaceUsers, userID.String()))
}

func (r *Repository) UpdateProfile(ctx context.Context, record model.Profile, version state.Version) (state.Version, error) {
	return swapRecord(ctx, r.store, state.MustKey(state.NamespaceUsers, record.UserID.String()), version, &record)
}

func (r *Repository) Profiles(ctx context.Context) ([]model.Profile, error) {
	return listRecords[model.Profile](ctx, r.store, state.MustPrefix(state.NamespaceUsers))
}

func (r *Repository) CreateAccount(ctx context.Context, record model.Account) error {
	return createRecord(ctx, r.store, state.MustKey(state.NamespaceAccounts, record.UserID.String()), &record)
}

func (r *Repository) Account(ctx context.Context, userID domain.UserID) (model.Account, state.Version, error) {
	return getRecord[model.Account](ctx, r.store, state.MustKey(state.NamespaceAccounts, userID.String()))
}

func (r *Repository) UpdateAccount(ctx context.Context, record model.Account, version state.Version) (state.Version, error) {
	return swapRecord(ctx, r.store, state.MustKey(state.NamespaceAccounts, record.UserID.String()), version, &record)
}

func (r *Repository) Accounts(ctx context.Context) ([]model.Account, error) {
	return listRecords[model.Account](ctx, r.store, state.MustPrefix(state.NamespaceAccounts))
}

func credentialKey(userID domain.UserID, credentialID string) state.Key {
	return state.MustKey(state.NamespaceCredentials, userID.String(), secret.Hash(credentialID))
}

func (r *Repository) CreateCredential(ctx context.Context, record model.Credential) error {
	return createRecord(ctx, r.store, credentialKey(record.UserID, record.CredentialID), &record)
}

func (r *Repository) Credential(ctx context.Context, userID domain.UserID, credentialID string) (model.Credential, state.Version, error) {
	return getRecord[model.Credential](ctx, r.store, credentialKey(userID, credentialID))
}

func (r *Repository) CredentialByRawID(ctx context.Context, userID domain.UserID, rawID []byte) (model.Credential, state.Version, error) {
	return r.Credential(ctx, userID, base64.RawURLEncoding.EncodeToString(rawID))
}

func (r *Repository) UpdateCredential(ctx context.Context, record model.Credential, version state.Version) (state.Version, error) {
	return swapRecord(ctx, r.store, credentialKey(record.UserID, record.CredentialID), version, &record)
}

func (r *Repository) DeleteCredential(ctx context.Context, userID domain.UserID, credentialID string, version state.Version) error {
	return r.store.Delete(ctx, credentialKey(userID, credentialID), version)
}

func (r *Repository) Credentials(ctx context.Context, userID domain.UserID) ([]model.Credential, error) {
	index, _, err := r.CredentialIndex(ctx, userID)
	if err != nil {
		return nil, err
	}
	owned := make([]model.Credential, 0, len(index.CredentialIDs))
	for _, credentialID := range index.CredentialIDs {
		credential, _, err := r.Credential(ctx, userID, credentialID)
		if err != nil || credential.UserID != userID {
			return nil, domain.NewError(domain.ErrorInternal, "credential index is inconsistent")
		}
		owned = append(owned, credential)
	}
	return owned, nil
}

func credentialIndexKey(userID domain.UserID) state.Key {
	return state.MustKey(state.NamespaceCredentials, userID.String(), "index")
}

func (r *Repository) CredentialIndex(ctx context.Context, userID domain.UserID) (model.CredentialIndex, state.Version, error) {
	return getRecord[model.CredentialIndex](ctx, r.store, credentialIndexKey(userID))
}

func (r *Repository) CreateCredentialIndex(ctx context.Context, record model.CredentialIndex) error {
	return createRecord(ctx, r.store, credentialIndexKey(record.UserID), &record)
}

func (r *Repository) UpdateCredentialIndex(ctx context.Context, record model.CredentialIndex, version state.Version) (state.Version, error) {
	return swapRecord(ctx, r.store, credentialIndexKey(record.UserID), version, &record)
}

func (r *Repository) CreateCeremony(ctx context.Context, record model.Ceremony) error {
	key, owner, err := ceremonyKey(record.CeremonyID)
	if err != nil || record.UserID != nil && owner != *record.UserID || record.UserID == nil && owner.Valid() {
		return domain.NewError(domain.ErrorInvalid, "ceremony owner binding mismatch")
	}
	return createRecord(ctx, r.store, key, &record)
}

func (r *Repository) Ceremony(ctx context.Context, ceremonyID string) (model.Ceremony, state.Version, error) {
	key, _, err := ceremonyKey(ceremonyID)
	if err != nil {
		return model.Ceremony{}, "", err
	}
	return getRecord[model.Ceremony](ctx, r.store, key)
}

func (r *Repository) UpdateCeremony(ctx context.Context, record model.Ceremony, version state.Version) (state.Version, error) {
	key, owner, err := ceremonyKey(record.CeremonyID)
	if err != nil || record.UserID != nil && owner != *record.UserID || record.UserID == nil && owner.Valid() {
		return "", domain.NewError(domain.ErrorInvalid, "ceremony owner binding mismatch")
	}
	return swapRecord(ctx, r.store, key, version, &record)
}

func ceremonyKey(ceremonyID string) (state.Key, domain.UserID, error) {
	if owner, _, err := domain.ParseScopedOpaqueID(ceremonyID); err == nil {
		return state.MustKey(state.NamespaceCeremonies, "owner", owner.String(), secret.Hash(ceremonyID)), owner, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(ceremonyID)
	if err != nil || len(decoded) < 16 || base64.RawURLEncoding.EncodeToString(decoded) != ceremonyID {
		return state.Key{}, domain.UserID{}, domain.NewError(domain.ErrorInvalid, "invalid ceremony ID")
	}
	return state.MustKey(state.NamespaceCeremonies, "capability", secret.Hash(ceremonyID)), domain.UserID{}, nil
}

func (r *Repository) CreateSession(ctx context.Context, rawToken string, record model.Session) error {
	owner, _, err := secret.ParseScopedBearerToken(rawToken)
	if err != nil || owner != record.UserID {
		return domain.NewError(domain.ErrorInvalid, "session token owner mismatch")
	}
	return createRecord(ctx, r.store, sessionKey(owner, rawToken), &record)
}

func (r *Repository) Session(ctx context.Context, rawToken string) (model.Session, state.Version, error) {
	owner, _, err := secret.ParseScopedBearerToken(rawToken)
	if err != nil {
		return model.Session{}, "", domain.NewError(domain.ErrorNotFound, "session not found")
	}
	return getRecord[model.Session](ctx, r.store, sessionKey(owner, rawToken))
}

func (r *Repository) DeleteSession(ctx context.Context, rawToken string, version state.Version) error {
	owner, _, err := secret.ParseScopedBearerToken(rawToken)
	if err != nil {
		return domain.NewError(domain.ErrorNotFound, "session not found")
	}
	return r.store.Delete(ctx, sessionKey(owner, rawToken), version)
}

func (r *Repository) RevokeUserSessions(ctx context.Context, userID domain.UserID) error {
	atomic, ok := r.store.(state.AtomicStore)
	if !ok {
		return domain.NewError(domain.ErrorUnavailable, "atomic identity state is required")
	}
	for attempts := 0; attempts < 8; attempts++ {
		account, version, err := r.Account(ctx, userID)
		if err != nil {
			return err
		}
		if account.AuthEpoch == math.MaxUint64 {
			return domain.NewError(domain.ErrorInvalid, "account authentication epoch is exhausted")
		}
		if account.AuthEpoch == 0 {
			account.AuthEpoch = 1
		}
		account.AuthEpoch++
		body, err := state.EncodeJSON(&account)
		if err != nil {
			return err
		}
		_, err = atomic.Mutate(ctx, state.Mutation{
			ID: "revoke-sessions-" + secret.Hash(userID.String()+"\x00"+string(version)),
			Changes: []state.Change{{Key: state.MustKey(state.NamespaceAccounts, userID.String()), Requirement: state.RequirementPresent, ExpectedVersion: version, Data: body}},
		})
		if err == nil {
			return nil
		}
		if !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
	}
	return domain.NewError(domain.ErrorConflict, "account changed concurrently")
}

func sessionKey(owner domain.UserID, rawToken string) state.Key {
	return state.MustKey(state.NamespaceSessions, owner.String(), secret.Hash(rawToken))
}

func (r *Repository) CreateInvite(ctx context.Context, record model.Invite) error {
	return createRecord(ctx, r.store, state.MustKey(state.NamespaceInvites, record.TokenHash), &record)
}

func (r *Repository) InviteByToken(ctx context.Context, rawToken string) (model.Invite, state.Version, error) {
	return r.InviteByHash(ctx, secret.Hash(rawToken))
}

func (r *Repository) InviteByHash(ctx context.Context, tokenHash string) (model.Invite, state.Version, error) {
	return getRecord[model.Invite](ctx, r.store, state.MustKey(state.NamespaceInvites, tokenHash))
}

func (r *Repository) Invites(ctx context.Context) ([]model.Invite, error) {
	return listRecords[model.Invite](ctx, r.store, state.MustPrefix(state.NamespaceInvites))
}

func (r *Repository) UpdateInvite(ctx context.Context, record model.Invite, version state.Version) (state.Version, error) {
	return swapRecord(ctx, r.store, state.MustKey(state.NamespaceInvites, record.TokenHash), version, &record)
}

func (r *Repository) InviteByID(ctx context.Context, inviteID string) (model.Invite, state.Version, error) {
	return findRecord[model.Invite](ctx, r.store, state.MustPrefix(state.NamespaceInvites), func(record model.Invite) bool { return record.InviteID == inviteID })
}

func (r *Repository) CreateRecovery(ctx context.Context, record model.Recovery) error {
	return createRecord(ctx, r.store, state.MustKey(state.NamespaceRecoveries, record.TargetUserID.String(), record.TokenHash), &record)
}

func (r *Repository) RecoveryByToken(ctx context.Context, rawToken string) (model.Recovery, state.Version, error) {
	owner, _, err := secret.ParseScopedBearerToken(rawToken)
	if err != nil {
		return model.Recovery{}, "", domain.NewError(domain.ErrorNotFound, "recovery not found")
	}
	return r.RecoveryByHash(ctx, owner, secret.Hash(rawToken))
}

func (r *Repository) RecoveryByHash(ctx context.Context, owner domain.UserID, tokenHash string) (model.Recovery, state.Version, error) {
	return getRecord[model.Recovery](ctx, r.store, state.MustKey(state.NamespaceRecoveries, owner.String(), tokenHash))
}

func (r *Repository) UpdateRecovery(ctx context.Context, record model.Recovery, version state.Version) (state.Version, error) {
	return swapRecord(ctx, r.store, state.MustKey(state.NamespaceRecoveries, record.TargetUserID.String(), record.TokenHash), version, &record)
}

func (r *Repository) AdminRoles(ctx context.Context) (model.AdminRoles, state.Version, error) {
	return getRecord[model.AdminRoles](ctx, r.store, state.MustKey(state.NamespaceRoles, "admins"))
}

func (r *Repository) CreateAdminRoles(ctx context.Context, record model.AdminRoles) error {
	return createRecord(ctx, r.store, state.MustKey(state.NamespaceRoles, "admins"), &record)
}

func (r *Repository) UpdateAdminRoles(ctx context.Context, record model.AdminRoles, version state.Version) (state.Version, error) {
	return swapRecord(ctx, r.store, state.MustKey(state.NamespaceRoles, "admins"), version, &record)
}

func (r *Repository) Bootstrap(ctx context.Context) (model.BootstrapState, state.Version, error) {
	return getRecord[model.BootstrapState](ctx, r.store, state.MustKey(state.NamespaceBootstrap, "state"))
}

func (r *Repository) CreateBootstrap(ctx context.Context, record model.BootstrapState) error {
	return createRecord(ctx, r.store, state.MustKey(state.NamespaceBootstrap, "state"), &record)
}

func (r *Repository) UpdateBootstrap(ctx context.Context, record model.BootstrapState, version state.Version) (state.Version, error) {
	return swapRecord(ctx, r.store, state.MustKey(state.NamespaceBootstrap, "state"), version, &record)
}

func (r *Repository) FirstAccountMarker(ctx context.Context) (model.FirstAccountMarker, state.Version, error) {
	return getRecord[model.FirstAccountMarker](ctx, r.store, state.MustKey(state.NamespaceBootstrap, "first-account"))
}

func (r *Repository) CreateFirstAccountMarker(ctx context.Context, record model.FirstAccountMarker) error {
	return createRecord(ctx, r.store, state.MustKey(state.NamespaceBootstrap, "first-account"), &record)
}

func (r *Repository) CreateRegistrationOperation(ctx context.Context, record model.RegistrationOperation) error {
	return createRecord(ctx, r.store, state.MustKey(state.NamespaceOperations, "identity", record.UserID.String(), record.OperationID), &record)
}

func (r *Repository) RegistrationOperation(ctx context.Context, owner domain.UserID, operationID string) (model.RegistrationOperation, state.Version, error) {
	return getRecord[model.RegistrationOperation](ctx, r.store, state.MustKey(state.NamespaceOperations, "identity", owner.String(), operationID))
}

func (r *Repository) UpdateRegistrationOperation(ctx context.Context, record model.RegistrationOperation, version state.Version) (state.Version, error) {
	return swapRecord(ctx, r.store, state.MustKey(state.NamespaceOperations, "identity", record.UserID.String(), record.OperationID), version, &record)
}

func idempotencyKey(owner domain.UserID, keyHash string) state.Key {
	return state.MustKey(state.NamespaceIdempotency, "identity", owner.String(), keyHash)
}

func (r *Repository) CreateIdempotency(ctx context.Context, record model.IdempotencyRecord) error {
	return createRecord(ctx, r.store, idempotencyKey(record.OwnerUserID, record.KeyHash), &record)
}

func (r *Repository) Idempotency(ctx context.Context, owner domain.UserID, keyHash string) (model.IdempotencyRecord, state.Version, error) {
	return getRecord[model.IdempotencyRecord](ctx, r.store, idempotencyKey(owner, keyHash))
}

func (r *Repository) UpdateIdempotency(ctx context.Context, record model.IdempotencyRecord, version state.Version) (state.Version, error) {
	return swapRecord(ctx, r.store, idempotencyKey(record.OwnerUserID, record.KeyHash), version, &record)
}

func createRecord(ctx context.Context, store state.Store, key state.Key, record any) error {
	data, err := state.EncodeJSON(record)
	if err != nil {
		return err
	}
	_, err = store.Create(ctx, key, data)
	return err
}

func getRecord[T any](ctx context.Context, store state.Store, key state.Key) (T, state.Version, error) {
	var record T
	value, err := store.Get(ctx, key)
	if err != nil {
		return record, "", err
	}
	if err := state.DecodeJSON(value.Data, &record); err != nil {
		return record, "", err
	}
	return record, value.Version, nil
}

func swapRecord(ctx context.Context, store state.Store, key state.Key, version state.Version, record any) (state.Version, error) {
	data, err := state.EncodeJSON(record)
	if err != nil {
		return "", err
	}
	return store.CompareAndSwap(ctx, key, version, data)
}

func listRecords[T any](ctx context.Context, store state.Store, prefix state.Prefix) ([]T, error) {
	var records []T
	request := state.PageRequest{Limit: 200}
	for {
		page, err := store.List(ctx, prefix, request)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			var record T
			if err := state.DecodeJSON(item.Value.Data, &record); err != nil {
				return nil, err
			}
			records = append(records, record)
		}
		if page.NextCursor == "" {
			return records, nil
		}
		request.Cursor = page.NextCursor
	}
}

func findRecord[T any](ctx context.Context, store state.Store, prefix state.Prefix, predicate func(T) bool) (T, state.Version, error) {
	var zero T
	request := state.PageRequest{Limit: 200}
	for {
		page, err := store.List(ctx, prefix, request)
		if err != nil {
			return zero, "", err
		}
		for _, item := range page.Items {
			var record T
			if err := state.DecodeJSON(item.Value.Data, &record); err != nil {
				return zero, "", err
			}
			if predicate(record) {
				return record, item.Value.Version, nil
			}
		}
		if page.NextCursor == "" {
			return zero, "", domain.NewError(domain.ErrorNotFound, "identity record not found")
		}
		request.Cursor = page.NextCursor
	}
}
