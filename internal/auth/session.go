package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
)

const (
	SecureSessionCookieName = "__Host-endlessfs_session"
	DevSessionCookieName    = "endlessfs_session_dev"
	SecureCSRFCookieName    = "__Host-endlessfs_csrf"
	DevCSRFCookieName       = "endlessfs_csrf_dev"
)

type SessionRepository interface {
	CreateSession(context.Context, string, model.Session) error
	Session(context.Context, string) (model.Session, state.Version, error)
	DeleteSession(context.Context, string, state.Version) error
	RotateSessionAtomic(context.Context, string, state.Version, string, model.Session) error
	RevokeUserSessions(context.Context, domain.UserID) error
	Account(context.Context, domain.UserID) (model.Account, state.Version, error)
}

type SessionManager struct {
	repository    SessionRepository
	ids           *domain.IDGenerator
	clock         domain.Clock
	ttl           time.Duration
	allowedOrigin string
	secure        bool
	protectionKey secret.Value
}

type IssuedSession struct {
	Token     secret.Value
	CSRFToken secret.Value
	Record    model.Session
}

type AuthenticatedSession struct {
	RawToken secret.Value
	Record   model.Session
	Version  state.Version
}

func NewSessionManager(repository SessionRepository, ids *domain.IDGenerator, clock domain.Clock, ttl time.Duration, allowedOrigin string, secure bool, protectionKey secret.Value) (*SessionManager, error) {
	if repository == nil || ids == nil || clock == nil || ttl <= 0 || ttl > 7*24*time.Hour || allowedOrigin == "" || !secret.ValidBearerToken(protectionKey.Reveal()) {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid session manager configuration")
	}
	return &SessionManager{repository: repository, ids: ids, clock: clock, ttl: ttl, allowedOrigin: allowedOrigin, secure: secure, protectionKey: protectionKey}, nil
}

func (m *SessionManager) Issue(ctx context.Context, userID domain.UserID, credentialID string) (IssuedSession, error) {
	issued, err := m.prepare(ctx, userID, credentialID)
	if err != nil {
		return IssuedSession{}, err
	}
	if err := m.repository.CreateSession(ctx, issued.Token.Reveal(), issued.Record); err != nil {
		return IssuedSession{}, err
	}
	return issued, nil
}

func (m *SessionManager) prepare(ctx context.Context, userID domain.UserID, credentialID string) (IssuedSession, error) {
	if !userID.Valid() || credentialID == "" {
		return IssuedSession{}, domain.NewError(domain.ErrorInvalid, "session owner and credential are required")
	}
	account, _, err := m.repository.Account(ctx, userID)
	if err != nil || account.Status != model.AccountEnabled {
		return IssuedSession{}, domain.NewError(domain.ErrorUnauthenticated, "account is unavailable")
	}
	authEpoch := effectiveAuthEpoch(account.AuthEpoch)
	rawSecret, err := m.ids.BearerToken()
	if err != nil {
		return IssuedSession{}, err
	}
	rawToken, err := secret.ScopeBearerToken(userID, rawSecret)
	if err != nil {
		return IssuedSession{}, err
	}
	csrfToken, err := m.ids.BearerToken()
	if err != nil {
		return IssuedSession{}, err
	}
	now := m.clock.Now()
	record := model.Session{
		SchemaVersion:         model.SchemaVersion,
		SessionTokenHash:      secret.KeyedHash(m.protectionKey, rawToken),
		UserID:                userID,
		AuthEpoch:             authEpoch,
		CSRFTokenHash:         secret.KeyedHash(m.protectionKey, csrfToken),
		CreatedAt:             now,
		ExpiresAt:             now.Add(m.ttl),
		AuthnCredentialIDHash: secret.Hash(credentialID),
	}
	return IssuedSession{Token: secret.Value(rawToken), CSRFToken: secret.Value(csrfToken), Record: record}, nil
}

// PrepareForOperation derives replayable session material from a committed
// authentication operation. The operation ID is public entropy; secrecy and
// unlinkability come from the server-held protection key. Raw secrets remain
// ephemeral and are never written to application state.
func (m *SessionManager) PrepareForOperation(userID domain.UserID, credentialID, operationID string, createdAt time.Time, authEpoch uint64) (IssuedSession, error) {
	if !userID.Valid() || credentialID == "" || operationID == "" || createdAt.IsZero() || createdAt.Location() != time.UTC || authEpoch == 0 {
		return IssuedSession{}, domain.NewError(domain.ErrorInvalid, "invalid deterministic session material")
	}
	rawSecret := secret.KeyedHash(m.protectionKey, "endlessfs-session-operation-v1\x00"+userID.String()+"\x00"+operationID)
	rawToken, err := secret.ScopeBearerToken(userID, rawSecret)
	if err != nil {
		return IssuedSession{}, err
	}
	csrfToken := secret.KeyedHash(m.protectionKey, "endlessfs-csrf-operation-v1\x00"+userID.String()+"\x00"+operationID)
	record := model.Session{
		SchemaVersion: model.SchemaVersion, SessionTokenHash: secret.KeyedHash(m.protectionKey, rawToken),
		UserID: userID, AuthEpoch: authEpoch, CSRFTokenHash: secret.KeyedHash(m.protectionKey, csrfToken),
		CreatedAt: createdAt, ExpiresAt: createdAt.Add(m.ttl), AuthnCredentialIDHash: secret.Hash(credentialID),
	}
	return IssuedSession{Token: secret.Value(rawToken), CSRFToken: secret.Value(csrfToken), Record: record}, nil
}

func (m *SessionManager) Authenticate(ctx context.Context, rawToken string) (AuthenticatedSession, error) {
	owner, _, parseErr := secret.ParseScopedBearerToken(rawToken)
	if parseErr != nil {
		return AuthenticatedSession{}, domain.NewError(domain.ErrorUnauthenticated, "invalid session")
	}
	record, version, err := m.repository.Session(ctx, rawToken)
	if err != nil || record.UserID != owner || !secret.MatchesKeyedHash(m.protectionKey, rawToken, record.SessionTokenHash) {
		return AuthenticatedSession{}, domain.NewError(domain.ErrorUnauthenticated, "invalid session")
	}
	if !m.clock.Now().Before(record.ExpiresAt) {
		return AuthenticatedSession{}, domain.NewError(domain.ErrorUnauthenticated, "expired session")
	}
	account, _, err := m.repository.Account(ctx, record.UserID)
	if err != nil || account.Status != model.AccountEnabled || effectiveAuthEpoch(account.AuthEpoch) != effectiveAuthEpoch(record.AuthEpoch) {
		return AuthenticatedSession{}, domain.NewError(domain.ErrorUnauthenticated, "invalid session")
	}
	return AuthenticatedSession{RawToken: secret.Value(rawToken), Record: record, Version: version}, nil
}

func effectiveAuthEpoch(value uint64) uint64 {
	if value == 0 {
		return 1
	}
	return value
}

func (m *SessionManager) AuthorizeMutation(session AuthenticatedSession, csrfToken, origin string) error {
	if origin != m.allowedOrigin {
		return domain.NewError(domain.ErrorUnauthorized, "request origin is not allowed")
	}
	if !secret.ValidBearerToken(csrfToken) || !secret.MatchesKeyedHash(m.protectionKey, csrfToken, session.Record.CSRFTokenHash) {
		return domain.NewError(domain.ErrorUnauthorized, "invalid CSRF token")
	}
	return nil
}

func (m *SessionManager) Protect(value string) string {
	return secret.KeyedHash(m.protectionKey, value)
}

func (m *SessionManager) MatchesProtected(value, protected string) bool {
	return secret.MatchesKeyedHash(m.protectionKey, value, protected)
}

func (m *SessionManager) Rotate(ctx context.Context, current AuthenticatedSession, credentialID string) (IssuedSession, error) {
	issued, err := m.prepare(ctx, current.Record.UserID, credentialID)
	if err != nil {
		return IssuedSession{}, err
	}
	if err := m.repository.RotateSessionAtomic(ctx, current.RawToken.Reveal(), current.Version, issued.Token.Reveal(), issued.Record); err != nil {
		return IssuedSession{}, domain.NewError(domain.ErrorUnauthenticated, "session rotation failed")
	}
	return issued, nil
}

func (m *SessionManager) Logout(ctx context.Context, current AuthenticatedSession) error {
	err := m.repository.DeleteSession(ctx, current.RawToken.Reveal(), current.Version)
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrPreconditionFailed) {
		return nil
	}
	return err
}

func (m *SessionManager) RevokeUser(ctx context.Context, userID domain.UserID) error {
	return m.repository.RevokeUserSessions(ctx, userID)
}

func (m *SessionManager) CookieName() string {
	if m.secure {
		return SecureSessionCookieName
	}
	return DevSessionCookieName
}

func (m *SessionManager) Cookie(issued IssuedSession) *http.Cookie {
	// #nosec G124 -- Secure is mandatory in configured secure mode; the only false mode is validated loopback HTTP development.
	return &http.Cookie{
		Name: m.CookieName(), Value: issued.Token.Reveal(), Path: "/",
		Expires: issued.Record.ExpiresAt, MaxAge: int(issued.Record.ExpiresAt.Sub(m.clock.Now()).Seconds()),
		Secure: m.secure, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	}
}

func (m *SessionManager) CSRFCookie(issued IssuedSession) *http.Cookie {
	name := SecureCSRFCookieName
	if !m.secure {
		name = DevCSRFCookieName
	}
	// #nosec G124 -- The CSRF cookie must be JavaScript-readable; Secure is false only in validated loopback development.
	return &http.Cookie{
		Name: name, Value: issued.CSRFToken.Reveal(), Path: "/",
		Expires: issued.Record.ExpiresAt, MaxAge: int(issued.Record.ExpiresAt.Sub(m.clock.Now()).Seconds()),
		Secure: m.secure, HttpOnly: false, SameSite: http.SameSiteStrictMode,
	}
}

func (m *SessionManager) ClearCookie() *http.Cookie {
	// #nosec G124 -- Expiration preserves the same dynamic secure-mode attributes as the session cookie it clears.
	return &http.Cookie{
		Name: m.CookieName(), Value: "", Path: "/", Expires: time.Unix(1, 0).UTC(),
		MaxAge: -1, Secure: m.secure, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	}
}

func (m *SessionManager) ClearCSRFCookie() *http.Cookie {
	name := SecureCSRFCookieName
	if !m.secure {
		name = DevCSRFCookieName
	}
	// #nosec G124 -- Expiration preserves the intentionally JavaScript-readable CSRF cookie and secure-mode attributes.
	return &http.Cookie{
		Name: name, Value: "", Path: "/", Expires: time.Unix(1, 0).UTC(),
		MaxAge: -1, Secure: m.secure, HttpOnly: false, SameSite: http.SameSiteStrictMode,
	}
}
