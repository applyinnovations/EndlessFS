package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
)

const CeremonyLifetime = 5 * time.Minute

type RegistrationPolicy struct {
	AllowPublic bool
	AllowInvite bool
}

// PolicySource permits policy changes between ceremony start and verification.
type PolicySource interface {
	RegistrationPolicy() RegistrationPolicy
}

type MutablePolicy struct {
	mu     sync.RWMutex
	policy RegistrationPolicy
}

func NewMutablePolicy(policy RegistrationPolicy) *MutablePolicy {
	return &MutablePolicy{policy: policy}
}

func (p *MutablePolicy) RegistrationPolicy() RegistrationPolicy {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.policy
}

func (p *MutablePolicy) Set(policy RegistrationPolicy) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.policy = policy
}

type Service struct {
	repository        *Repository
	webAuthn          auth.WebAuthnEngine
	sessions          *auth.SessionManager
	ids               *domain.IDGenerator
	clock             domain.Clock
	policy            PolicySource
	bootstrapHash     string
	baseURL           string
	rateMu            sync.Mutex
	registrationRates map[string]registrationRate
}

func NewService(repository *Repository, webAuthn auth.WebAuthnEngine, sessions *auth.SessionManager, ids *domain.IDGenerator, clock domain.Clock, policy PolicySource, bootstrapToken secret.Value, baseURL string) (*Service, error) {
	if repository == nil || webAuthn == nil || sessions == nil || ids == nil || clock == nil || policy == nil || baseURL == "" {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid identity service configuration")
	}
	bootstrapHash := ""
	if raw := bootstrapToken.Reveal(); raw != "" {
		if !secret.ValidBearerToken(raw) {
			return nil, domain.NewError(domain.ErrorInvalid, "invalid bootstrap token")
		}
		bootstrapHash = secret.Hash(raw)
	}
	return &Service{
		repository: repository, webAuthn: webAuthn, sessions: sessions,
		ids: ids, clock: clock, policy: policy, bootstrapHash: bootstrapHash,
		baseURL:           baseURL,
		registrationRates: make(map[string]registrationRate),
	}, nil
}

type CeremonyStart struct {
	CeremonyID     string          `json:"ceremonyID"`
	BrowserBinding secret.Value    `json:"-"`
	Options        json.RawMessage `json:"publicKey"`
}

type RegistrationStartRequest struct {
	DisplayName string
	InviteToken secret.Value
	ClientKey   string
}

type registrationRate struct {
	windowStart time.Time
	attempts    int
}

type RegistrationComplete struct {
	UserID       domain.UserID      `json:"userID"`
	CredentialID string             `json:"credentialID"`
	Flow         model.CeremonyFlow `json:"flow"`
}

func (s *Service) StartBootstrap(ctx context.Context, bootstrapToken secret.Value, displayName string) (CeremonyStart, error) {
	if s.bootstrapHash == "" || !secret.MatchesHash(bootstrapToken.Reveal(), s.bootstrapHash) {
		return CeremonyStart{}, domain.NewError(domain.ErrorUnavailable, "bootstrap is unavailable")
	}
	if _, _, err := s.repository.Bootstrap(ctx); err == nil || !errors.Is(err, domain.ErrNotFound) {
		return CeremonyStart{}, domain.NewError(domain.ErrorUnavailable, "bootstrap is unavailable")
	}
	if _, _, err := s.repository.FirstAccountMarker(ctx); err == nil || !errors.Is(err, domain.ErrNotFound) {
		return CeremonyStart{}, domain.NewError(domain.ErrorUnavailable, "bootstrap is unavailable")
	}
	exists, err := s.repository.UserExists(ctx)
	if err != nil || exists {
		return CeremonyStart{}, domain.NewError(domain.ErrorUnavailable, "bootstrap is unavailable")
	}
	return s.startNewUserRegistration(ctx, model.CeremonyBootstrap, displayName, "")
}

func (s *Service) StartRegistration(ctx context.Context, request RegistrationStartRequest) (CeremonyStart, error) {
	policy := s.policy.RegistrationPolicy()
	if request.InviteToken.Reveal() == "" {
		if !policy.AllowPublic {
			return CeremonyStart{}, domain.NewError(domain.ErrorUnavailable, "registration is unavailable")
		}
		if !s.allowPublicRegistrationAttempt(request.ClientKey) {
			return CeremonyStart{}, domain.NewError(domain.ErrorRateLimited, "public registration rate limit exceeded")
		}
		return s.startNewUserRegistration(ctx, model.CeremonyPublic, request.DisplayName, "")
	}
	if !policy.AllowInvite {
		return CeremonyStart{}, domain.NewError(domain.ErrorUnavailable, "registration is unavailable")
	}
	invite, _, err := s.repository.InviteByToken(ctx, request.InviteToken.Reveal())
	if err != nil || !s.inviteUsable(invite) {
		return CeremonyStart{}, domain.NewError(domain.ErrorUnavailable, "registration is unavailable")
	}
	return s.startNewUserRegistration(ctx, model.CeremonyInvite, request.DisplayName, invite.TokenHash)
}

func (s *Service) allowPublicRegistrationAttempt(clientKey string) bool {
	if host, _, err := net.SplitHostPort(clientKey); err == nil {
		clientKey = host
	}
	if clientKey == "" {
		clientKey = "local"
	}
	now := s.clock.Now()
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	rate := s.registrationRates[clientKey]
	if rate.windowStart.IsZero() || now.Sub(rate.windowStart) >= time.Minute {
		rate = registrationRate{windowStart: now}
	}
	if rate.attempts >= 10 {
		return false
	}
	rate.attempts++
	s.registrationRates[clientKey] = rate
	return true
}

func (s *Service) StartRecovery(ctx context.Context, recoveryToken secret.Value) (CeremonyStart, error) {
	if !secret.ValidScopedBearerToken(recoveryToken.Reveal()) {
		return CeremonyStart{}, domain.NewError(domain.ErrorUnavailable, "recovery is unavailable")
	}
	recovery, _, err := s.repository.RecoveryByToken(ctx, recoveryToken.Reveal())
	if err != nil || !s.recoveryUsable(recovery) {
		return CeremonyStart{}, domain.NewError(domain.ErrorUnavailable, "recovery is unavailable")
	}
	profile, _, err := s.repository.Profile(ctx, recovery.TargetUserID)
	if err != nil {
		return CeremonyStart{}, domain.NewError(domain.ErrorUnavailable, "recovery is unavailable")
	}
	return s.startExistingUserRegistration(ctx, model.CeremonyRecovery, profile, recovery.TokenHash)
}

func (s *Service) StartAddPasskey(ctx context.Context, current auth.AuthenticatedSession, labelValue string) (CeremonyStart, error) {
	if s.clock.Now().Sub(current.Record.CreatedAt) > CeremonyLifetime {
		return CeremonyStart{}, domain.NewError(domain.ErrorPreconditionFailed, "recent authentication is required")
	}
	profile, _, err := s.repository.Profile(ctx, current.Record.UserID)
	if err != nil {
		return CeremonyStart{}, err
	}
	var label domain.CredentialLabel
	if labelValue != "" {
		label, err = domain.ParseCredentialLabel(labelValue)
		if err != nil {
			return CeremonyStart{}, err
		}
	}
	start, err := s.startExistingUserRegistration(ctx, model.CeremonyAddPasskey, profile, "")
	if err != nil || labelValue == "" {
		return start, err
	}
	ceremony, version, err := s.repository.Ceremony(ctx, start.CeremonyID)
	if err != nil {
		return CeremonyStart{}, err
	}
	ceremony.CredentialLabel = &label
	if _, err := s.repository.UpdateCeremony(ctx, ceremony, version); err != nil {
		return CeremonyStart{}, err
	}
	return start, nil
}

func (s *Service) startNewUserRegistration(ctx context.Context, flow model.CeremonyFlow, displayNameValue, bearerHash string) (CeremonyStart, error) {
	displayName, err := domain.ParseDisplayName(displayNameValue)
	if err != nil {
		return CeremonyStart{}, err
	}
	userID, err := s.ids.UserID()
	if err != nil {
		return CeremonyStart{}, err
	}
	return s.startRegistrationCeremony(ctx, flow, auth.User{ID: userID, DisplayName: displayName}, bearerHash)
}

func (s *Service) startExistingUserRegistration(ctx context.Context, flow model.CeremonyFlow, profile model.Profile, bearerHash string) (CeremonyStart, error) {
	credentials, err := s.repository.Credentials(ctx, profile.UserID)
	if err != nil {
		return CeremonyStart{}, err
	}
	return s.startRegistrationCeremony(ctx, flow, auth.User{ID: profile.UserID, DisplayName: profile.DisplayName, Credentials: credentials}, bearerHash)
}

func (s *Service) startRegistrationCeremony(ctx context.Context, flow model.CeremonyFlow, user auth.User, bearerHash string) (CeremonyStart, error) {
	challenge, err := s.ids.BearerToken()
	if err != nil {
		return CeremonyStart{}, err
	}
	challengeBytes, _ := base64.RawURLEncoding.DecodeString(challenge)
	options, librarySession, err := s.webAuthn.BeginRegistration(user, challengeBytes)
	if err != nil {
		return CeremonyStart{}, err
	}
	ceremonyID, binding, err := s.newCeremonySecrets()
	if err != nil {
		return CeremonyStart{}, err
	}
	ceremonyID, err = domain.ScopeOpaqueID(user.ID, ceremonyID)
	if err != nil {
		return CeremonyStart{}, err
	}
	now := s.clock.Now()
	userID := user.ID
	displayName := user.DisplayName
	record := model.Ceremony{
		SchemaVersion: model.SchemaVersion, CeremonyID: ceremonyID,
		Type: model.CeremonyRegistration, Flow: flow,
		BrowserBindingHash: s.sessions.Protect(binding), UserID: &userID, DisplayName: &displayName,
		BearerTokenHash: bearerHash, LibrarySession: librarySession,
		CreatedAt: now, ExpiresAt: now.Add(CeremonyLifetime),
	}
	if err := s.repository.CreateCeremony(ctx, record); err != nil {
		return CeremonyStart{}, err
	}
	return CeremonyStart{CeremonyID: ceremonyID, BrowserBinding: secret.Value(binding), Options: options}, nil
}

func (s *Service) StartAuthentication(ctx context.Context) (CeremonyStart, error) {
	challenge, err := s.ids.BearerToken()
	if err != nil {
		return CeremonyStart{}, err
	}
	challengeBytes, _ := base64.RawURLEncoding.DecodeString(challenge)
	options, librarySession, err := s.webAuthn.BeginAuthentication(challengeBytes)
	if err != nil {
		return CeremonyStart{}, err
	}
	ceremonyID, binding, err := s.newCeremonySecrets()
	if err != nil {
		return CeremonyStart{}, err
	}
	now := s.clock.Now()
	record := model.Ceremony{
		SchemaVersion: model.SchemaVersion, CeremonyID: ceremonyID,
		Type: model.CeremonyAuthentication, Flow: model.CeremonyAuthenticationFlow,
		BrowserBindingHash: s.sessions.Protect(binding), LibrarySession: librarySession,
		CreatedAt: now, ExpiresAt: now.Add(CeremonyLifetime),
	}
	if err := s.repository.CreateCeremony(ctx, record); err != nil {
		return CeremonyStart{}, err
	}
	return CeremonyStart{CeremonyID: ceremonyID, BrowserBinding: secret.Value(binding), Options: options}, nil
}

func (s *Service) newCeremonySecrets() (ceremonyID, binding string, err error) {
	ceremonyID, err = s.ids.OpaqueID()
	if err != nil {
		return "", "", err
	}
	binding, err = s.ids.BearerToken()
	return ceremonyID, binding, err
}

func (s *Service) VerifyRegistration(ctx context.Context, ceremonyID string, browserBinding secret.Value, response []byte) (RegistrationComplete, error) {
	ceremony, ceremonyVersion, err := s.registrationCeremony(ctx, ceremonyID, browserBinding)
	if err != nil {
		return RegistrationComplete{}, err
	}
	if ceremony.ConsumedAt != nil {
		return s.resumeRegistration(ctx, ceremony)
	}
	if err := s.verifyRegistrationPolicy(ctx, ceremony); err != nil {
		return RegistrationComplete{}, err
	}
	user, err := s.ceremonyUser(ctx, ceremony)
	if err != nil {
		return RegistrationComplete{}, err
	}
	result, err := s.webAuthn.FinishRegistration(user, ceremony.LibrarySession, response)
	if err != nil {
		return RegistrationComplete{}, err
	}
	now := s.clock.Now()
	result.Credential.SchemaVersion = model.SchemaVersion
	result.Credential.UserID = user.ID
	result.Credential.CreatedAt = now
	result.Credential.LastUsedAt = now
	if ceremony.CredentialLabel != nil {
		result.Credential.Label = ceremony.CredentialLabel
	}
	operationID, err := s.ids.OpaqueID()
	if err != nil {
		return RegistrationComplete{}, err
	}
	operation := model.RegistrationOperation{
		SchemaVersion: model.SchemaVersion, OperationID: operationID,
		Flow: ceremony.Flow, Status: model.OperationClaimed,
		UserID: user.ID, DisplayName: user.DisplayName,
		Credential: result.Credential, CreatedAt: now,
	}
	if err := s.commitRegistrationAtomic(ctx, ceremony, ceremonyVersion, operation); err != nil {
		return RegistrationComplete{}, err
	}
	return RegistrationComplete{UserID: operation.UserID, CredentialID: operation.Credential.CredentialID, Flow: operation.Flow}, nil
}

func (s *Service) VerifyAuthentication(ctx context.Context, ceremonyID string, browserBinding secret.Value, response []byte) (auth.IssuedSession, error) {
	ceremony, version, err := s.authenticationCeremony(ctx, ceremonyID, browserBinding)
	if err != nil {
		return auth.IssuedSession{}, err
	}
	result, err := s.webAuthn.FinishAuthentication(ceremony.LibrarySession, response, s.resolveDiscoverable(ctx))
	if err != nil {
		return auth.IssuedSession{}, err
	}
	now := s.clock.Now()
	operationID, err := s.ids.OpaqueID()
	if err != nil {
		return auth.IssuedSession{}, err
	}
	ceremony.ConsumedAt = &now
	ceremony.OperationID = operationID
	if _, err := s.repository.UpdateCeremony(ctx, ceremony, version); err != nil {
		return auth.IssuedSession{}, domain.NewError(domain.ErrorConflict, "ceremony was already consumed")
	}
	account, _, err := s.repository.Account(ctx, result.UserID)
	if err != nil || account.Status != model.AccountEnabled {
		return auth.IssuedSession{}, domain.NewError(domain.ErrorUnauthenticated, "authentication failed")
	}
	stored, credentialVersion, err := s.repository.Credential(ctx, result.UserID, result.Credential.CredentialID)
	if err != nil || stored.UserID != result.UserID {
		return auth.IssuedSession{}, domain.NewError(domain.ErrorUnauthenticated, "authentication failed")
	}
	stored.SignCount = result.Credential.SignCount
	stored.BackupEligible = result.Credential.BackupEligible
	stored.BackupState = result.Credential.BackupState
	stored.LastUsedAt = now
	if _, err := s.repository.UpdateCredential(ctx, stored, credentialVersion); err != nil {
		return auth.IssuedSession{}, domain.NewError(domain.ErrorConflict, "credential changed concurrently")
	}
	return s.sessions.Issue(ctx, result.UserID, stored.CredentialID)
}

func (s *Service) registrationCeremony(ctx context.Context, ceremonyID string, binding secret.Value) (model.Ceremony, state.Version, error) {
	ceremony, version, err := s.repository.Ceremony(ctx, ceremonyID)
	if err != nil || ceremony.Type != model.CeremonyRegistration || !s.sessions.MatchesProtected(binding.Reveal(), ceremony.BrowserBindingHash) {
		return model.Ceremony{}, "", domain.NewError(domain.ErrorUnauthenticated, "invalid registration ceremony")
	}
	if ceremony.ConsumedAt == nil && !s.clock.Now().Before(ceremony.ExpiresAt) {
		return model.Ceremony{}, "", domain.NewError(domain.ErrorUnauthenticated, "invalid registration ceremony")
	}
	return ceremony, version, nil
}

func (s *Service) authenticationCeremony(ctx context.Context, ceremonyID string, binding secret.Value) (model.Ceremony, state.Version, error) {
	ceremony, version, err := s.repository.Ceremony(ctx, ceremonyID)
	if err != nil || ceremony.Type != model.CeremonyAuthentication || ceremony.ConsumedAt != nil || !s.sessions.MatchesProtected(binding.Reveal(), ceremony.BrowserBindingHash) {
		return model.Ceremony{}, "", domain.NewError(domain.ErrorUnauthenticated, "invalid authentication ceremony")
	}
	if !s.clock.Now().Before(ceremony.ExpiresAt) {
		return model.Ceremony{}, "", domain.NewError(domain.ErrorUnauthenticated, "invalid authentication ceremony")
	}
	return ceremony, version, nil
}

func (s *Service) ceremonyUser(ctx context.Context, ceremony model.Ceremony) (auth.User, error) {
	if ceremony.UserID == nil || ceremony.DisplayName == nil {
		return auth.User{}, domain.NewError(domain.ErrorInvalid, "registration ceremony has no user")
	}
	credentials := []model.Credential(nil)
	if ceremony.Flow == model.CeremonyAddPasskey || ceremony.Flow == model.CeremonyRecovery {
		var err error
		credentials, err = s.repository.Credentials(ctx, *ceremony.UserID)
		if err != nil {
			return auth.User{}, err
		}
	}
	return auth.User{ID: *ceremony.UserID, DisplayName: *ceremony.DisplayName, Credentials: credentials}, nil
}

func (s *Service) resolveDiscoverable(ctx context.Context) auth.UserResolver {
	return func(rawCredentialID, userHandle []byte) (auth.User, error) {
		userID, err := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(userHandle))
		if err != nil {
			return auth.User{}, domain.NewError(domain.ErrorUnauthenticated, "authentication failed")
		}
		credential, _, err := s.repository.CredentialByRawID(ctx, userID, rawCredentialID)
		if err != nil || credential.UserID != userID {
			return auth.User{}, domain.NewError(domain.ErrorUnauthenticated, "authentication failed")
		}
		account, _, err := s.repository.Account(ctx, userID)
		if err != nil || account.Status != model.AccountEnabled {
			return auth.User{}, domain.NewError(domain.ErrorUnauthenticated, "authentication failed")
		}
		profile, _, err := s.repository.Profile(ctx, userID)
		if err != nil {
			return auth.User{}, domain.NewError(domain.ErrorUnauthenticated, "authentication failed")
		}
		credentials, err := s.repository.Credentials(ctx, userID)
		if err != nil {
			return auth.User{}, domain.NewError(domain.ErrorUnauthenticated, "authentication failed")
		}
		return auth.User{ID: userID, DisplayName: profile.DisplayName, Credentials: credentials}, nil
	}
}
