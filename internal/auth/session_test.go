package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
)

type sessionRepositoryStub struct {
	sessions      map[string]model.Session
	versions      map[string]state.Version
	accounts      map[domain.UserID]model.Account
	createErr     error
	deleteErr     error
	revokeErr     error
	revokedUserID domain.UserID
}

func newSessionRepositoryStub() *sessionRepositoryStub {
	return &sessionRepositoryStub{sessions: map[string]model.Session{}, versions: map[string]state.Version{}, accounts: map[domain.UserID]model.Account{}}
}

func (repository *sessionRepositoryStub) CreateSession(_ context.Context, token string, record model.Session) error {
	if repository.createErr != nil {
		return repository.createErr
	}
	repository.sessions[token] = record
	repository.versions[token] = "v1"
	return nil
}

func (repository *sessionRepositoryStub) Session(_ context.Context, token string) (model.Session, state.Version, error) {
	record, ok := repository.sessions[token]
	if !ok {
		return model.Session{}, "", domain.ErrNotFound
	}
	return record, repository.versions[token], nil
}

func (repository *sessionRepositoryStub) DeleteSession(_ context.Context, token string, _ state.Version) error {
	if repository.deleteErr != nil {
		return repository.deleteErr
	}
	if _, ok := repository.sessions[token]; !ok {
		return domain.ErrNotFound
	}
	delete(repository.sessions, token)
	return nil
}

func (repository *sessionRepositoryStub) RevokeUserSessions(_ context.Context, userID domain.UserID) error {
	repository.revokedUserID = userID
	return repository.revokeErr
}

func (repository *sessionRepositoryStub) Account(_ context.Context, userID domain.UserID) (model.Account, state.Version, error) {
	account, ok := repository.accounts[userID]
	if !ok {
		return model.Account{}, "", domain.ErrNotFound
	}
	return account, "v1", nil
}

func TestSessionManagerSecurityBoundaryAndCookieLifecycle(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC))
	userID := testUserID(t, 0x91)
	key := secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x51}, 32)))
	repository := newSessionRepositoryStub()
	repository.accounts[userID] = model.Account{SchemaVersion: model.SchemaVersion, UserID: userID, Status: model.AccountEnabled, CreatedAt: clock.Now(), UpdatedAt: clock.Now()}
	manager, err := NewSessionManager(repository, domain.NewIDGenerator(bytes.NewReader(sessionEntropy(4096))), clock, time.Hour, "https://drive.example.test", true, key)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := manager.Issue(ctx, domain.UserID{}, "credential"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("Issue invalid user = %v", err)
	}
	if _, err := manager.Issue(ctx, userID, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("Issue empty credential = %v", err)
	}
	issued, err := manager.Issue(ctx, userID, "credential-1")
	if err != nil {
		t.Fatal(err)
	}
	if issued.Record.SessionTokenHash == issued.Token.Reveal() || issued.Record.CSRFTokenHash == issued.CSRFToken.Reveal() {
		t.Fatal("session material was not hashed at rest")
	}
	current, err := manager.Authenticate(ctx, issued.Token.Reveal())
	if err != nil || current.Record.UserID != userID {
		t.Fatalf("Authenticate() = %+v, %v", current, err)
	}
	for name, mutate := range map[string]func(){
		"unknown": func() { delete(repository.sessions, issued.Token.Reveal()) },
		"wrong hash": func() {
			record := issued.Record
			record.SessionTokenHash = secret.Hash("other")
			repository.sessions[issued.Token.Reveal()] = record
		},
		"expired": func() {
			record := issued.Record
			record.ExpiresAt = clock.Now()
			repository.sessions[issued.Token.Reveal()] = record
		},
		"disabled": func() {
			repository.sessions[issued.Token.Reveal()] = issued.Record
			account := repository.accounts[userID]
			account.Status = model.AccountDisabled
			repository.accounts[userID] = account
		},
	} {
		t.Run(name, func(t *testing.T) {
			repository.sessions[issued.Token.Reveal()] = issued.Record
			repository.accounts[userID] = model.Account{SchemaVersion: model.SchemaVersion, UserID: userID, Status: model.AccountEnabled, CreatedAt: clock.Now(), UpdatedAt: clock.Now()}
			mutate()
			if _, err := manager.Authenticate(ctx, issued.Token.Reveal()); !errors.Is(err, domain.ErrUnauthenticated) {
				t.Fatalf("Authenticate() = %v", err)
			}
		})
	}
	if _, err := manager.Authenticate(ctx, "not-a-token"); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("invalid token = %v", err)
	}

	repository.sessions[issued.Token.Reveal()] = issued.Record
	repository.accounts[userID] = model.Account{SchemaVersion: model.SchemaVersion, UserID: userID, Status: model.AccountEnabled, CreatedAt: clock.Now(), UpdatedAt: clock.Now()}
	if err := manager.AuthorizeMutation(current, issued.CSRFToken.Reveal(), "https://other.example"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("wrong origin = %v", err)
	}
	if err := manager.AuthorizeMutation(current, "invalid", "https://drive.example.test"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("wrong CSRF = %v", err)
	}
	if err := manager.AuthorizeMutation(current, issued.CSRFToken.Reveal(), "https://drive.example.test"); err != nil {
		t.Fatalf("valid mutation = %v", err)
	}
	if protected := manager.Protect("value"); protected == "value" || !manager.MatchesProtected("value", protected) || manager.MatchesProtected("other", protected) {
		t.Fatal("protection helpers did not bind the keyed value")
	}

	repository.deleteErr = domain.ErrUnavailable
	if _, err := manager.Rotate(ctx, current, "credential-2"); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("failed rotation = %v", err)
	}
	repository.deleteErr = nil
	repository.sessions[issued.Token.Reveal()] = issued.Record
	rotated, err := manager.Rotate(ctx, current, "credential-2")
	if err != nil || rotated.Token.Reveal() == issued.Token.Reveal() {
		t.Fatalf("Rotate() = %+v, %v", rotated, err)
	}
	for _, harmless := range []error{domain.ErrNotFound, domain.ErrPreconditionFailed} {
		repository.deleteErr = harmless
		if err := manager.Logout(ctx, current); err != nil {
			t.Fatalf("Logout(%v) = %v", harmless, err)
		}
	}
	repository.deleteErr = domain.ErrUnavailable
	if err := manager.Logout(ctx, current); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("Logout unavailable = %v", err)
	}
	repository.revokeErr = domain.ErrUnavailable
	if err := manager.RevokeUser(ctx, userID); !errors.Is(err, domain.ErrUnavailable) || repository.revokedUserID != userID {
		t.Fatalf("RevokeUser() = %v, user=%v", err, repository.revokedUserID)
	}

	cookie := manager.Cookie(rotated)
	csrfCookie := manager.CSRFCookie(rotated)
	clear := manager.ClearCookie()
	clearCSRF := manager.ClearCSRFCookie()
	if cookie.Name != SecureSessionCookieName || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != 3 || csrfCookie.Name != SecureCSRFCookieName || csrfCookie.HttpOnly {
		t.Fatalf("secure cookies = %#v %#v", cookie, csrfCookie)
	}
	if clear.Value != "" || clear.MaxAge != -1 || !clear.HttpOnly || clearCSRF.Value != "" || clearCSRF.MaxAge != -1 || clearCSRF.HttpOnly {
		t.Fatalf("clear cookies = %#v %#v", clear, clearCSRF)
	}

	development, err := NewSessionManager(repository, domain.NewIDGenerator(strings.NewReader(strings.Repeat("x", 1024))), clock, time.Hour, "http://localhost:8080", false, key)
	if err != nil {
		t.Fatal(err)
	}
	if development.CookieName() != DevSessionCookieName || development.Cookie(rotated).Secure || development.CSRFCookie(rotated).Name != DevCSRFCookieName || development.ClearCSRFCookie().Name != DevCSRFCookieName {
		t.Fatal("development cookie policy is not loopback-compatible")
	}
}

func sessionEntropy(size int) []byte {
	value := make([]byte, size)
	for index := range value {
		value[index] = byte(index*29 + 7)
	}
	return value
}

func TestSessionManagerRejectsInvalidConstructionAndEntropyFailures(t *testing.T) {
	clock := domain.NewFixedClock(time.Now().UTC())
	repository := newSessionRepositoryStub()
	ids := domain.NewIDGenerator(strings.NewReader(strings.Repeat("x", 1024)))
	key := secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	for name, build := range map[string]func() (*SessionManager, error){
		"repository": func() (*SessionManager, error) {
			return NewSessionManager(nil, ids, clock, time.Hour, "origin", true, key)
		},
		"ids": func() (*SessionManager, error) {
			return NewSessionManager(repository, nil, clock, time.Hour, "origin", true, key)
		},
		"clock": func() (*SessionManager, error) {
			return NewSessionManager(repository, ids, nil, time.Hour, "origin", true, key)
		},
		"ttl zero": func() (*SessionManager, error) {
			return NewSessionManager(repository, ids, clock, 0, "origin", true, key)
		},
		"ttl long": func() (*SessionManager, error) {
			return NewSessionManager(repository, ids, clock, 8*24*time.Hour, "origin", true, key)
		},
		"origin": func() (*SessionManager, error) {
			return NewSessionManager(repository, ids, clock, time.Hour, "", true, key)
		},
		"key": func() (*SessionManager, error) {
			return NewSessionManager(repository, ids, clock, time.Hour, "origin", true, "weak")
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := build(); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("NewSessionManager() = %v", err)
			}
		})
	}
	manager, err := NewSessionManager(repository, domain.NewIDGenerator(strings.NewReader("short")), clock, time.Hour, "origin", true, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Issue(context.Background(), testUserID(t, 0x92), "credential"); err == nil {
		t.Fatal("Issue succeeded with a failing entropy source")
	}
	repository.createErr = domain.ErrUnavailable
	manager, _ = NewSessionManager(repository, domain.NewIDGenerator(strings.NewReader(strings.Repeat("x", 1024))), clock, time.Hour, "origin", true, key)
	if _, err := manager.Issue(context.Background(), testUserID(t, 0x93), "credential"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("Issue repository error = %v", err)
	}
}
