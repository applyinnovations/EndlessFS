package identity

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
)

var identityEpoch = time.Date(2026, 8, 16, 2, 0, 0, 0, time.UTC)

func TestIntegrationConcurrentBootstrapCreatesExactlyOneAdmin(t *testing.T) {
	env := newIdentityEnvironment(t, RegistrationPolicy{AllowInvite: true})
	starts := make([]CeremonyStart, 16)
	for index := range starts {
		start, err := env.service.StartBootstrap(context.Background(), env.bootstrapToken, "First Admin")
		if err != nil {
			t.Fatalf("StartBootstrap(%d): %v", index, err)
		}
		starts[index] = start
	}
	var successes int
	var winner int
	var mu sync.Mutex
	var wait sync.WaitGroup
	for index, start := range starts {
		wait.Add(1)
		go func(index int, start CeremonyStart) {
			defer wait.Done()
			_, err := env.service.VerifyRegistration(context.Background(), start.CeremonyID, start.BrowserBinding, fakeRegistrationResponse(byte(index+1)))
			if err == nil {
				mu.Lock()
				successes++
				winner = index
				mu.Unlock()
			}
		}(index, start)
	}
	wait.Wait()
	if successes != 1 {
		t.Fatalf("successful bootstraps = %d, want 1", successes)
	}
	profiles, err := env.repository.Profiles(context.Background())
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profiles = %d, %v", len(profiles), err)
	}
	account, _, _ := env.repository.Account(context.Background(), profiles[0].UserID)
	roles, _, _ := env.repository.AdminRoles(context.Background())
	bootstrap, _, _ := env.repository.Bootstrap(context.Background())
	if account.Status != model.AccountEnabled || len(roles.UserIDs) != 1 || roles.UserIDs[0] != profiles[0].UserID || bootstrap.Status != model.OperationCommitted {
		t.Fatalf("materialized bootstrap account=%+v roles=%+v bootstrap=%+v", account, roles, bootstrap)
	}
	if _, err := env.service.VerifyRegistration(context.Background(), starts[winner].CeremonyID, starts[winner].BrowserBinding, nil); err != nil {
		t.Fatalf("verified operation did not resume idempotently: %v", err)
	}
	if _, err := env.service.StartBootstrap(context.Background(), env.bootstrapToken, "Another"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("completed bootstrap error = %v", err)
	}
}

func TestIntegrationRegistrationPolicyMatrixAndVerificationRecheck(t *testing.T) {
	for _, allowPublic := range []bool{false, true} {
		for _, allowInvite := range []bool{false, true} {
			name := strings.Join([]string{boolName(allowPublic), boolName(allowInvite)}, "/")
			t.Run(name, func(t *testing.T) {
				env := newIdentityEnvironment(t, RegistrationPolicy{AllowPublic: allowPublic, AllowInvite: allowInvite})
				token := bearer(0x91)
				now := env.clock.Now()
				adminID := userID(t, 0x90)
				invite := model.Invite{
					SchemaVersion: model.SchemaVersion, InviteID: opaque(0x92), TokenHash: secret.Hash(token),
					CreatedByUserID: adminID, CreatedAt: now, MaxUses: 1,
				}
				if err := env.repository.CreateInvite(context.Background(), invite); err != nil {
					t.Fatal(err)
				}
				_, publicErr := env.service.StartRegistration(context.Background(), RegistrationStartRequest{DisplayName: "Public User"})
				if (publicErr == nil) != allowPublic {
					t.Fatalf("public start error = %v", publicErr)
				}
				_, inviteErr := env.service.StartRegistration(context.Background(), RegistrationStartRequest{DisplayName: "Invited User", InviteToken: secret.Value(token)})
				if (inviteErr == nil) != allowInvite {
					t.Fatalf("invite start error = %v", inviteErr)
				}
			})
		}
	}

	env := newIdentityEnvironment(t, RegistrationPolicy{AllowPublic: true, AllowInvite: true})
	start, err := env.service.StartRegistration(context.Background(), RegistrationStartRequest{DisplayName: "Policy Changed"})
	if err != nil {
		t.Fatal(err)
	}
	env.policy.Set(RegistrationPolicy{})
	if _, err := env.service.VerifyRegistration(context.Background(), start.CeremonyID, start.BrowserBinding, fakeRegistrationResponse(0xa1)); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("verification after policy close error = %v", err)
	}
}

func TestIntegrationCeremonyBindingExpiryReplayAndUsernamelessAuthentication(t *testing.T) {
	env := newIdentityEnvironment(t, RegistrationPolicy{AllowPublic: true})
	expiring, err := env.service.StartRegistration(context.Background(), RegistrationStartRequest{DisplayName: "Expiring"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.VerifyRegistration(context.Background(), expiring.CeremonyID, secret.Value(bearer(0x33)), fakeRegistrationResponse(0x34)); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("wrong browser binding error = %v", err)
	}
	env.clock.Advance(6 * time.Minute)
	if _, err := env.service.VerifyRegistration(context.Background(), expiring.CeremonyID, expiring.BrowserBinding, fakeRegistrationResponse(0x34)); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("expired registration error = %v", err)
	}
	env.clock.Advance(-6 * time.Minute)
	start, _ := env.service.StartRegistration(context.Background(), RegistrationStartRequest{DisplayName: "Login User"})
	complete, err := env.service.VerifyRegistration(context.Background(), start.CeremonyID, start.BrowserBinding, fakeRegistrationResponse(0x35))
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := env.service.VerifyRegistration(context.Background(), start.CeremonyID, start.BrowserBinding, fakeRegistrationResponse(0x35)); err != nil || replay.UserID != complete.UserID {
		t.Fatalf("verified registration replay did not resume: %+v, %v", replay, err)
	}
	profiles, _ := env.repository.Profiles(context.Background())
	if len(profiles) != 1 {
		t.Fatalf("registration replay created %d profiles", len(profiles))
	}
	authStart, err := env.service.StartAuthentication(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	authResponse, _ := json.Marshal(fakeResponse{UserID: complete.UserID.String(), CredentialID: complete.CredentialID})
	issued, err := env.service.VerifyAuthentication(context.Background(), authStart.CeremonyID, authStart.BrowserBinding, authResponse)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.sessions.Authenticate(context.Background(), issued.Token.Reveal()); err != nil {
		t.Fatalf("issued login session error = %v", err)
	}
	if _, err := env.service.VerifyAuthentication(context.Background(), authStart.CeremonyID, authStart.BrowserBinding, authResponse); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("authentication replay error = %v", err)
	}
}

func TestPublicRegistrationRateLimitIsLocalDeterministicAndBounded(t *testing.T) {
	env := newIdentityEnvironment(t, RegistrationPolicy{AllowPublic: true})
	for index := 0; index < 10; index++ {
		if _, err := env.service.StartRegistration(context.Background(), RegistrationStartRequest{DisplayName: "Rate User", ClientKey: "192.0.2.10:1234"}); err != nil {
			t.Fatalf("attempt %d: %v", index, err)
		}
	}
	if _, err := env.service.StartRegistration(context.Background(), RegistrationStartRequest{DisplayName: "Rate User", ClientKey: "192.0.2.10:9999"}); !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("bounded attempt error = %v", err)
	}
	if _, err := env.service.StartRegistration(context.Background(), RegistrationStartRequest{DisplayName: "Other Client", ClientKey: "192.0.2.11:1234"}); err != nil {
		t.Fatalf("other client error = %v", err)
	}
	env.clock.Advance(time.Minute)
	if _, err := env.service.StartRegistration(context.Background(), RegistrationStartRequest{DisplayName: "Rate Reset", ClientKey: "192.0.2.10:1234"}); err != nil {
		t.Fatalf("reset window error = %v", err)
	}
}

func TestIntegrationInviteIsHashedSingleUseAndConcurrent(t *testing.T) {
	env := newIdentityEnvironment(t, RegistrationPolicy{AllowInvite: true})
	adminID, actor := bootstrapIdentity(t, env)
	expiry := env.clock.Now().Add(time.Hour)
	created, err := env.service.CreateInvite(context.Background(), actor, &expiry, "invite-concurrency-0001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.CreateInvite(context.Background(), actor, &expiry, "invite-concurrency-0001"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("idempotent invite replay error = %v", err)
	}
	rawLink := created.Link.Reveal()
	rawToken := strings.TrimPrefix(rawLink, env.service.baseURL+"/register/invite/")
	if rawToken == rawLink || !secret.ValidBearerToken(rawToken) || strings.Contains(mustJSON(t, created.Record), rawToken) {
		t.Fatal("invite token was malformed or persisted in plaintext")
	}
	starts := make([]CeremonyStart, 2)
	for index := range starts {
		starts[index], err = env.service.StartRegistration(context.Background(), RegistrationStartRequest{
			DisplayName: "Duplicate Name", InviteToken: secret.Value(rawToken),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var successes int
	var wait sync.WaitGroup
	var mu sync.Mutex
	for index, start := range starts {
		wait.Add(1)
		go func(index int, start CeremonyStart) {
			defer wait.Done()
			if _, verifyErr := env.service.VerifyRegistration(context.Background(), start.CeremonyID, start.BrowserBinding, fakeRegistrationResponse(byte(0xb0+index))); verifyErr == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}(index, start)
	}
	wait.Wait()
	if successes != 1 {
		t.Fatalf("successful invite uses = %d, want 1", successes)
	}
	invite, _, _ := env.repository.InviteByToken(context.Background(), rawToken)
	if invite.Uses != 1 || invite.UsedByUserID == nil || invite.OperationID == "" {
		t.Fatalf("consumed invite = %+v", invite)
	}
	profiles, _ := env.repository.Profiles(context.Background())
	if len(profiles) != 2 || profiles[0].UserID == profiles[1].UserID || adminID == *invite.UsedByUserID {
		t.Fatalf("profiles/invite owner = %+v / %+v", profiles, invite)
	}
	listed, err := env.service.Invites(context.Background(), actor)
	if err != nil || len(listed) != 1 || strings.Contains(mustJSON(t, listed), rawToken) {
		t.Fatalf("safe invite listing = %+v, %v", listed, err)
	}
	expiringAt := env.clock.Now().Add(time.Minute)
	expiringInvite, err := env.service.CreateInvite(context.Background(), actor, &expiringAt, "invite-expiry-test-0001")
	if err != nil {
		t.Fatal(err)
	}
	expiringToken := strings.TrimPrefix(expiringInvite.Link.Reveal(), env.service.baseURL+"/register/invite/")
	env.clock.Advance(2 * time.Minute)
	if _, err := env.service.StartRegistration(context.Background(), RegistrationStartRequest{DisplayName: "Expired", InviteToken: secret.Value(expiringToken)}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("expired invite error = %v", err)
	}
	revokedInvite, err := env.service.CreateInvite(context.Background(), actor, nil, "invite-revoke-test-0001")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.service.RevokeInvite(context.Background(), actor, revokedInvite.InviteID); err != nil {
		t.Fatal(err)
	}
	revokedToken := strings.TrimPrefix(revokedInvite.Link.Reveal(), env.service.baseURL+"/register/invite/")
	if _, err := env.service.StartRegistration(context.Background(), RegistrationStartRequest{DisplayName: "Revoked", InviteToken: secret.Value(revokedToken)}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("revoked invite error = %v", err)
	}
}

func TestIntegrationSessionsCSRFRotationExpiryAndDisabledAccount(t *testing.T) {
	env := newIdentityEnvironment(t, RegistrationPolicy{})
	userIDValue, _ := bootstrapIdentity(t, env)
	credential := firstCredential(t, env, userIDValue)
	issued, err := env.sessions.Issue(context.Background(), userIDValue, credential.CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := env.sessions.Authenticate(context.Background(), issued.Token.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	if err := env.sessions.AuthorizeMutation(current, issued.CSRFToken.Reveal(), "https://drive.example.test"); err != nil {
		t.Fatal(err)
	}
	if err := env.sessions.AuthorizeMutation(current, bearer(0xc1), "https://drive.example.test"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("wrong CSRF error = %v", err)
	}
	if err := env.sessions.AuthorizeMutation(current, issued.CSRFToken.Reveal(), "https://evil.example"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("wrong origin error = %v", err)
	}
	cookie := env.sessions.Cookie(issued)
	if cookie.Name != auth.SecureSessionCookieName || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != 3 || cookie.Path != "/" || cookie.Domain != "" {
		t.Fatalf("secure session cookie = %+v", cookie)
	}
	rotated, err := env.sessions.Rotate(context.Background(), current, credential.CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.sessions.Authenticate(context.Background(), issued.Token.Reveal()); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("old rotated token error = %v", err)
	}
	env.clock.Advance(13 * time.Hour)
	if _, err := env.sessions.Authenticate(context.Background(), rotated.Token.Reveal()); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("expired token error = %v", err)
	}

	env.clock.Advance(-13 * time.Hour)
	again, _ := env.sessions.Issue(context.Background(), userIDValue, credential.CredentialID)
	account, version, _ := env.repository.Account(context.Background(), userIDValue)
	account.Status = model.AccountDisabled
	account.UpdatedAt = env.clock.Now()
	_, _ = env.repository.UpdateAccount(context.Background(), account, version)
	if _, err := env.sessions.Authenticate(context.Background(), again.Token.Reveal()); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("disabled account session error = %v", err)
	}
	authStart, _ := env.service.StartAuthentication(context.Background())
	authResponse, _ := json.Marshal(fakeResponse{UserID: userIDValue.String(), CredentialID: credential.CredentialID})
	if _, err := env.service.VerifyAuthentication(context.Background(), authStart.CeremonyID, authStart.BrowserBinding, authResponse); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("disabled account authentication error = %v", err)
	}
}

func TestIntegrationRecoveryAddsPasskeyPreservesIdentityAndRevokesSessions(t *testing.T) {
	env := newIdentityEnvironment(t, RegistrationPolicy{})
	userIDValue, actor := bootstrapIdentity(t, env)
	original := firstCredential(t, env, userIDValue)
	prior, _ := env.sessions.Issue(context.Background(), userIDValue, original.CredentialID)
	created, err := env.service.CreateRecovery(context.Background(), actor, userIDValue, 15*time.Minute, "recovery-flow-test-0001")
	if err != nil {
		t.Fatal(err)
	}
	rawToken := strings.TrimPrefix(created.Link.Reveal(), env.service.baseURL+"/recover/")
	start, err := env.service.StartRecovery(context.Background(), secret.Value(rawToken))
	if err != nil {
		t.Fatal(err)
	}
	complete, err := env.service.VerifyRegistration(context.Background(), start.CeremonyID, start.BrowserBinding, fakeRegistrationResponse(0xd1))
	if err != nil || complete.UserID != userIDValue {
		t.Fatalf("recovery complete = %+v, %v", complete, err)
	}
	if _, err := env.sessions.Authenticate(context.Background(), prior.Token.Reveal()); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("prior recovery session error = %v", err)
	}
	credentials, err := env.repository.Credentials(context.Background(), userIDValue)
	if err != nil || len(credentials) != 2 {
		t.Fatalf("credentials after recovery = %+v, %v", credentials, err)
	}
	recovery, _, _ := env.repository.RecoveryByToken(context.Background(), rawToken)
	if recovery.UsedAt == nil || recovery.OperationID == "" || strings.Contains(mustJSON(t, recovery), rawToken) {
		t.Fatalf("consumed recovery = %+v", recovery)
	}
	for _, credential := range credentials {
		start, _ := env.service.StartAuthentication(context.Background())
		response, _ := json.Marshal(fakeResponse{UserID: userIDValue.String(), CredentialID: credential.CredentialID})
		issued, err := env.service.VerifyAuthentication(context.Background(), start.CeremonyID, start.BrowserBinding, response)
		if err != nil {
			t.Fatalf("authenticate with credential %q: %v", credential.CredentialID, err)
		}
		if _, err := env.sessions.Authenticate(context.Background(), issued.Token.Reveal()); err != nil {
			t.Fatalf("session for credential %q: %v", credential.CredentialID, err)
		}
	}
	fresh, _ := env.sessions.Issue(context.Background(), userIDValue, credentials[1].CredentialID)
	current, _ := env.sessions.Authenticate(context.Background(), fresh.Token.Reveal())
	if err := env.service.RemovePasskey(context.Background(), current, credentials[0].CredentialID); err != nil {
		t.Fatal(err)
	}
	if err := env.service.RemovePasskey(context.Background(), current, credentials[1].CredentialID); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("final passkey removal error = %v", err)
	}
}

func TestDisplayNameAndPasskeyLabelChangesDoNotAlterIdentityOrRoles(t *testing.T) {
	env := newIdentityEnvironment(t, RegistrationPolicy{})
	userIDValue, actor := bootstrapIdentity(t, env)
	originalCredential := firstCredential(t, env, userIDValue)
	updated, err := env.service.UpdateDisplayName(context.Background(), actor, "Renamed Administrator")
	if err != nil || updated.UserID != userIDValue || updated.DisplayName.String() != "Renamed Administrator" {
		t.Fatalf("updated profile = %+v, %v", updated, err)
	}
	roles, _, _ := env.repository.AdminRoles(context.Background())
	if len(roles.UserIDs) != 1 || roles.UserIDs[0] != userIDValue {
		t.Fatalf("roles changed with display name: %+v", roles)
	}
	start, err := env.service.StartAddPasskey(context.Background(), actor, "Backup Key")
	if err != nil {
		t.Fatal(err)
	}
	complete, err := env.service.VerifyRegistration(context.Background(), start.CeremonyID, start.BrowserBinding, fakeRegistrationResponse(0xf1))
	if err != nil || complete.UserID != userIDValue || complete.Flow != model.CeremonyAddPasskey {
		t.Fatalf("add passkey = %+v, %v", complete, err)
	}
	credentials, err := env.repository.Credentials(context.Background(), userIDValue)
	if err != nil || len(credentials) != 2 {
		t.Fatalf("credentials = %+v, %v", credentials, err)
	}
	if credentials[0].CredentialID != originalCredential.CredentialID && credentials[1].CredentialID != originalCredential.CredentialID {
		t.Fatal("original credential identity changed")
	}
	foundLabel := false
	for _, credential := range credentials {
		if credential.Label != nil && credential.Label.String() == "Backup Key" {
			foundLabel = true
		}
	}
	if !foundLabel {
		t.Fatalf("new credential label missing: %+v", credentials)
	}
}

func TestIntegrationConcurrentAdminChangesPreserveEnabledAdministrator(t *testing.T) {
	env := newIdentityEnvironment(t, RegistrationPolicy{AllowPublic: true})
	firstID, firstActor := bootstrapIdentity(t, env)
	start, err := env.service.StartRegistration(context.Background(), RegistrationStartRequest{DisplayName: "Second Admin"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := env.service.VerifyRegistration(context.Background(), start.CeremonyID, start.BrowserBinding, fakeRegistrationResponse(0xe1))
	if err != nil {
		t.Fatal(err)
	}
	if admin, err := env.service.isAdmin(context.Background(), second.UserID); err != nil || admin {
		t.Fatalf("public registration implicitly became admin: admin=%v err=%v", admin, err)
	}
	if err := env.service.GrantAdmin(context.Background(), firstActor, second.UserID); err != nil {
		t.Fatal(err)
	}
	page, err := env.service.AdminUsersPage(context.Background(), firstActor, 1, "")
	if err != nil || len(page.Users) != 1 || page.NextCursor == "" {
		t.Fatalf("first admin page = %+v, %v", page, err)
	}
	next, err := env.service.AdminUsersPage(context.Background(), firstActor, 1, page.NextCursor)
	if err != nil || len(next.Users) != 1 || next.NextCursor != "" || next.Users[0].UserID == page.Users[0].UserID {
		t.Fatalf("second admin page = %+v, %v", next, err)
	}
	if _, err := env.service.AdminUsersPage(context.Background(), firstActor, 1, page.NextCursor+"x"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("tampered admin cursor error = %v", err)
	}
	secondCredential := firstCredential(t, env, second.UserID)
	secondIssued, _ := env.sessions.Issue(context.Background(), second.UserID, secondCredential.CredentialID)
	secondActor, _ := env.sessions.Authenticate(context.Background(), secondIssued.Token.Reveal())
	var wait sync.WaitGroup
	errorsFound := make(chan error, 2)
	wait.Add(2)
	go func() {
		defer wait.Done()
		errorsFound <- env.service.DisableUser(context.Background(), firstActor, firstID)
	}()
	go func() {
		defer wait.Done()
		errorsFound <- env.service.DisableUser(context.Background(), secondActor, second.UserID)
	}()
	wait.Wait()
	close(errorsFound)
	successes := 0
	for err := range errorsFound {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent admin disables = %d, want 1", successes)
	}
	roles, _, err := env.repository.AdminRoles(context.Background())
	if err != nil || len(roles.UserIDs) != 1 {
		t.Fatalf("admin roles = %+v, %v", roles, err)
	}
	remaining, _, _ := env.repository.Account(context.Background(), roles.UserIDs[0])
	if remaining.Status != model.AccountEnabled {
		t.Fatalf("remaining administrator account = %+v", remaining)
	}
}

type identityEnvironment struct {
	service        *Service
	repository     *Repository
	sessions       *auth.SessionManager
	clock          *domain.FixedClock
	policy         *MutablePolicy
	bootstrapToken secret.Value
}

func newIdentityEnvironment(t *testing.T, registrationPolicy RegistrationPolicy) identityEnvironment {
	t.Helper()
	repository := NewRepository(state.NewMemoryStore())
	reader := &deterministicReader{next: 1}
	ids := domain.NewIDGenerator(reader)
	clock := domain.NewFixedClock(identityEpoch)
	sessions, err := auth.NewSessionManager(repository, ids, clock, 12*time.Hour, "https://drive.example.test", true, secret.Value(bearer(0x61)))
	if err != nil {
		t.Fatal(err)
	}
	policy := NewMutablePolicy(registrationPolicy)
	bootstrapToken := secret.Value(bearer(0x71))
	service, err := NewService(repository, fakeWebAuthn{}, sessions, ids, clock, policy, bootstrapToken, "https://drive.example.test")
	if err != nil {
		t.Fatal(err)
	}
	return identityEnvironment{service: service, repository: repository, sessions: sessions, clock: clock, policy: policy, bootstrapToken: bootstrapToken}
}

func bootstrapIdentity(t *testing.T, env identityEnvironment) (domain.UserID, auth.AuthenticatedSession) {
	t.Helper()
	start, err := env.service.StartBootstrap(context.Background(), env.bootstrapToken, "Administrator")
	if err != nil {
		t.Fatal(err)
	}
	complete, err := env.service.VerifyRegistration(context.Background(), start.CeremonyID, start.BrowserBinding, fakeRegistrationResponse(0x81))
	if err != nil {
		t.Fatal(err)
	}
	credential := firstCredential(t, env, complete.UserID)
	issued, err := env.sessions.Issue(context.Background(), complete.UserID, credential.CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := env.sessions.Authenticate(context.Background(), issued.Token.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	return complete.UserID, current
}

func firstCredential(t *testing.T, env identityEnvironment, userID domain.UserID) model.Credential {
	t.Helper()
	credentials, err := env.repository.Credentials(context.Background(), userID)
	if err != nil || len(credentials) == 0 {
		t.Fatalf("Credentials() = %+v, %v", credentials, err)
	}
	return credentials[0]
}

type fakeWebAuthn struct{}

type fakeSession struct {
	UserID string `json:"userID,omitempty"`
}

type fakeResponse struct {
	CredentialSeed byte   `json:"credentialSeed"`
	UserID         string `json:"userID,omitempty"`
	CredentialID   string `json:"credentialID,omitempty"`
}

func (fakeWebAuthn) BeginRegistration(user auth.User, challenge []byte) (json.RawMessage, json.RawMessage, error) {
	options, _ := json.Marshal(map[string]any{"challenge": base64.RawURLEncoding.EncodeToString(challenge)})
	session, _ := json.Marshal(fakeSession{UserID: user.ID.String()})
	return options, session, nil
}

func (fakeWebAuthn) FinishRegistration(user auth.User, session json.RawMessage, response []byte) (auth.RegistrationResult, error) {
	var saved fakeSession
	var submitted fakeResponse
	if json.Unmarshal(session, &saved) != nil || json.Unmarshal(response, &submitted) != nil || saved.UserID != user.ID.String() || submitted.CredentialSeed == 0 {
		return auth.RegistrationResult{}, domain.NewError(domain.ErrorUnauthenticated, "fake verification failed")
	}
	return auth.RegistrationResult{Credential: model.Credential{
		CredentialID: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{submitted.CredentialSeed}, 32)),
		PublicKey:    base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{submitted.CredentialSeed ^ 0xff}, 32)),
	}}, nil
}

func (fakeWebAuthn) BeginAuthentication(challenge []byte) (json.RawMessage, json.RawMessage, error) {
	options, _ := json.Marshal(map[string]any{"challenge": base64.RawURLEncoding.EncodeToString(challenge)})
	return options, json.RawMessage(`{"authentication":true}`), nil
}

func (fakeWebAuthn) FinishAuthentication(_ json.RawMessage, response []byte, resolve auth.UserResolver) (auth.AuthenticationResult, error) {
	var submitted fakeResponse
	if err := json.Unmarshal(response, &submitted); err != nil {
		return auth.AuthenticationResult{}, err
	}
	rawID, err := base64.RawURLEncoding.DecodeString(submitted.CredentialID)
	if err != nil {
		return auth.AuthenticationResult{}, err
	}
	userID, err := domain.ParseUserID(submitted.UserID)
	if err != nil {
		return auth.AuthenticationResult{}, err
	}
	handle, _ := base64.RawURLEncoding.DecodeString(userID.String())
	user, err := resolve(rawID, handle)
	if err != nil {
		return auth.AuthenticationResult{}, err
	}
	for _, credential := range user.Credentials {
		if credential.CredentialID == submitted.CredentialID {
			credential.SignCount++
			return auth.AuthenticationResult{UserID: user.ID, Credential: credential}, nil
		}
	}
	return auth.AuthenticationResult{}, domain.NewError(domain.ErrorUnauthenticated, "credential not found")
}

func fakeRegistrationResponse(seed byte) []byte {
	data, _ := json.Marshal(fakeResponse{CredentialSeed: seed})
	return data
}

type deterministicReader struct {
	mu   sync.Mutex
	next byte
}

func (r *deterministicReader) Read(buffer []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := range buffer {
		buffer[index] = r.next
		r.next++
		if r.next == 0 {
			r.next = 1
		}
	}
	return len(buffer), nil
}

var _ io.Reader = (*deterministicReader)(nil)

func bearer(fill byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

func opaque(fill byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 16))
}

func userID(t *testing.T, fill byte) domain.UserID {
	t.Helper()
	value, err := domain.ParseUserID(opaque(fill))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func boolName(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
