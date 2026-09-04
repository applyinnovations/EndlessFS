package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// TestProviderBudgetIdentityMutationWorkflows measures the real identity and
// session use cases, not synthetic object-store calls. The exact ratchets make
// changes to orchestration, record partitioning, or retry behavior visible.
func TestProviderBudgetIdentityMutationWorkflows(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	backend := budgettest.WrapClassified(providerbudget.RoleState, objectmemory.New(), ledger, func(_ providerbudget.RequestKind, target string) string {
		return storageformat.ClassifyEconomicsTarget(target)
	})
	reader := &deterministicReader{next: 1}
	ids := domain.NewIDGenerator(reader)
	clock := domain.NewFixedClock(identityEpoch)
	engine, err := portable.Open(ctx, portable.Options{
		Backend: backend, FileBackend: objectmemory.New(), Clock: clock, IDs: ids,
		Writer:   portable.WriterConfiguration{WriterSetID: "identity-budget", ConfigurationDigest: "identity-budget-v1", KeyringIdentifiers: []string{"budget-key"}},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x63}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(engine)
	sessions, err := auth.NewSessionManager(repository, ids, clock, 12*time.Hour, "https://drive.example.test", true, secret.Value(bearer(0x61)))
	if err != nil {
		t.Fatal(err)
	}
	policy := NewMutablePolicy(RegistrationPolicy{AllowPublic: true, AllowInvite: true})
	bootstrapToken := secret.Value(bearer(0x71))
	service, err := NewService(repository, fakeWebAuthn{}, sessions, ids, clock, policy, bootstrapToken, "https://drive.example.test")
	if err != nil {
		t.Fatal(err)
	}
	economics, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	ratchet, err := gcs.RegionalStandardFlatBudgetRatchet()
	if err != nil {
		t.Fatal(err)
	}
	check := func(name string) {
		t.Helper()
		if report, checkErr := ratchet.CheckExact(name, economics, []providerbudget.Role{providerbudget.RoleState}, ledger.Events()); checkErr != nil {
			t.Errorf("%s: %v; observed=%+v; events=%+v", name, checkErr, report.Totals, ledger.Events())
		}
		ledger.Reset()
	}
	ledger.Reset()

	bootstrap, err := service.StartBootstrap(ctx, bootstrapToken, "Administrator")
	if err != nil {
		t.Fatal(err)
	}
	check("identity-bootstrap-options-schema-011")
	admin, err := service.VerifyRegistration(ctx, bootstrap.CeremonyID, bootstrap.BrowserBinding, fakeRegistrationResponse(0x81))
	if err != nil {
		t.Fatal(err)
	}
	check("identity-bootstrap-verify-schema-011")
	credential := firstCredential(t, identityEnvironment{repository: repository}, admin.UserID)

	issued, err := sessions.Issue(ctx, admin.UserID, credential.CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	check("session-issue-schema-011")
	current, err := sessions.Authenticate(ctx, issued.Token.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	check("session-authenticate-schema-011")

	if _, err := service.CurrentUser(ctx, current); err != nil {
		t.Fatal(err)
	}
	check("identity-current-user-schema-011")
	if _, err := service.UpdateDisplayName(ctx, current, "Budget Administrator"); err != nil {
		t.Fatal(err)
	}
	check("identity-update-profile-schema-011")
	if _, err := service.Passkeys(ctx, current); err != nil {
		t.Fatal(err)
	}
	check("identity-list-passkeys-schema-011")

	authentication, err := service.StartAuthentication(ctx)
	if err != nil {
		t.Fatal(err)
	}
	check("identity-authentication-options-schema-011")
	authResponse, _ := json.Marshal(fakeResponse{UserID: admin.UserID.String(), CredentialID: credential.CredentialID})
	authenticated, err := service.VerifyAuthentication(ctx, authentication.CeremonyID, authentication.BrowserBinding, authResponse)
	if err != nil {
		t.Fatal(err)
	}
	check("identity-authentication-verify-schema-011")
	authenticatedCurrent, err := sessions.Authenticate(ctx, authenticated.Token.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	ledger.Reset()
	rotated, err := sessions.Rotate(ctx, authenticatedCurrent, credential.CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	check("session-rotate-schema-011")
	rotatedCurrent, err := sessions.Authenticate(ctx, rotated.Token.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	ledger.Reset()
	if err := sessions.Logout(ctx, rotatedCurrent); err != nil {
		t.Fatal(err)
	}
	check("session-logout-schema-011")

	addPasskey, err := service.StartAddPasskey(ctx, current, "Backup key")
	if err != nil {
		t.Fatal(err)
	}
	check("identity-add-passkey-options-schema-011")
	added, err := service.VerifyRegistration(ctx, addPasskey.CeremonyID, addPasskey.BrowserBinding, fakeRegistrationResponse(0x82))
	if err != nil {
		t.Fatal(err)
	}
	check("identity-add-passkey-verify-schema-011")
	if err := service.RemovePasskey(ctx, current, added.CredentialID); err != nil {
		t.Fatal(err)
	}
	check("identity-remove-passkey-schema-011")

	invite, err := service.CreateInvite(ctx, current, nil, "budget-invite-create-0001")
	if err != nil {
		t.Fatal(err)
	}
	check("identity-create-invite-schema-011")
	if _, err := service.Invites(ctx, current); err != nil {
		t.Fatal(err)
	}
	check("identity-list-invites-schema-011")
	if err := service.RevokeInvite(ctx, current, invite.InviteID); err != nil {
		t.Fatal(err)
	}
	check("identity-revoke-invite-schema-011")
	activeInvite, err := service.CreateInvite(ctx, current, nil, "budget-invite-use-00001")
	if err != nil {
		t.Fatal(err)
	}
	inviteToken := strings.TrimPrefix(activeInvite.Link.Reveal(), "https://drive.example.test/register/invite/")
	ledger.Reset()
	invitedStart, err := service.StartRegistration(ctx, RegistrationStartRequest{DisplayName: "Invited Budget User", InviteToken: secret.Value(inviteToken)})
	if err != nil {
		t.Fatal(err)
	}
	check("identity-invited-registration-options-schema-011")
	if _, err := service.VerifyRegistration(ctx, invitedStart.CeremonyID, invitedStart.BrowserBinding, fakeRegistrationResponse(0x90)); err != nil {
		t.Fatal(err)
	}
	check("identity-invited-registration-verify-schema-011")

	registration, err := service.StartRegistration(ctx, RegistrationStartRequest{DisplayName: "Budget User", ClientKey: "192.0.2.40"})
	if err != nil {
		t.Fatal(err)
	}
	check("identity-registration-options-schema-011")
	registered, err := service.VerifyRegistration(ctx, registration.CeremonyID, registration.BrowserBinding, fakeRegistrationResponse(0x91))
	if err != nil {
		t.Fatal(err)
	}
	check("identity-registration-verify-schema-011")
	if _, err := service.AdminUsersPage(ctx, current, 100, ""); err != nil {
		t.Fatal(err)
	}
	check("identity-list-admin-users-schema-011")

	if err := service.DisableUser(ctx, current, registered.UserID); err != nil {
		t.Fatal(err)
	}
	check("identity-disable-user-schema-011")
	if err := service.EnableUser(ctx, current, registered.UserID); err != nil {
		t.Fatal(err)
	}
	check("identity-enable-user-schema-011")
	if err := service.GrantAdmin(ctx, current, registered.UserID); err != nil {
		t.Fatal(err)
	}
	check("identity-grant-admin-schema-011")
	if err := service.RevokeAdmin(ctx, current, registered.UserID); err != nil {
		t.Fatal(err)
	}
	check("identity-revoke-admin-schema-011")

	recovery, err := service.CreateRecovery(ctx, current, registered.UserID, time.Hour, "budget-recovery-create-01")
	if err != nil {
		t.Fatal(err)
	}
	check("identity-create-recovery-schema-011")
	recoveryToken := strings.TrimPrefix(recovery.Link.Reveal(), "https://drive.example.test/recover/")
	recoveryStart, err := service.StartRecovery(ctx, secret.Value(recoveryToken))
	if err != nil {
		t.Fatal(err)
	}
	check("identity-recovery-options-schema-011")
	if _, err := service.VerifyRegistration(ctx, recoveryStart.CeremonyID, recoveryStart.BrowserBinding, fakeRegistrationResponse(0x92)); err != nil {
		t.Fatal(err)
	}
	check("identity-recovery-verify-schema-011")

	revocable, err := sessions.Issue(ctx, registered.UserID, registered.CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	_ = revocable
	ledger.Reset()
	if err := sessions.RevokeUser(ctx, registered.UserID); err != nil {
		t.Fatal(err)
	}
	check("session-revoke-user-schema-011")
}
