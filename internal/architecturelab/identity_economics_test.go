package architecturelab

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/identity"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/secret"
)

type benchmarkWebAuthn struct{}

func (benchmarkWebAuthn) BeginRegistration(user auth.User, _ []byte) (json.RawMessage, json.RawMessage, error) {
	session, _ := json.Marshal(map[string]string{"userID": user.ID.String()})
	return json.RawMessage(`{"challenge":"benchmark"}`), session, nil
}

func (benchmarkWebAuthn) FinishRegistration(user auth.User, _ json.RawMessage, _ []byte) (auth.RegistrationResult, error) {
	return auth.RegistrationResult{Credential: model.Credential{
		CredentialID: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x31}, 32)),
		PublicKey:    base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x32}, 32)),
		UserID:       user.ID,
	}}, nil
}

func (benchmarkWebAuthn) BeginAuthentication(_ []byte) (json.RawMessage, json.RawMessage, error) {
	return json.RawMessage(`{"challenge":"benchmark"}`), json.RawMessage(`{"authentication":true}`), nil
}

func (benchmarkWebAuthn) FinishAuthentication(_ json.RawMessage, response []byte, resolve auth.UserResolver) (auth.AuthenticationResult, error) {
	var submitted struct {
		UserID       string `json:"userID"`
		CredentialID string `json:"credentialID"`
	}
	if err := json.Unmarshal(response, &submitted); err != nil {
		return auth.AuthenticationResult{}, err
	}
	userID, err := domain.ParseUserID(submitted.UserID)
	if err != nil {
		return auth.AuthenticationResult{}, err
	}
	rawID, err := base64.RawURLEncoding.DecodeString(submitted.CredentialID)
	if err != nil {
		return auth.AuthenticationResult{}, err
	}
	handle, _ := base64.RawURLEncoding.DecodeString(userID.String())
	user, err := resolve(rawID, handle)
	if err != nil {
		return auth.AuthenticationResult{}, err
	}
	credential := user.Credentials[0]
	credential.SignCount++
	return auth.AuthenticationResult{UserID: user.ID, Credential: credential}, nil
}

func TestBootstrapAndAuthenticationEconomicsBeforeAndAfter(t *testing.T) {
	ctx := context.Background()
	modelEconomics, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	current := openCurrentProviderHarness(t, "identity-baseline")
	repository := identity.NewRepository(current.engine)
	clock := domain.NewFixedClock(time.Date(2049, 10, 11, 12, 13, 14, 0, time.UTC))
	ids := domain.NewIDGenerator(&currentBatchEntropy{})
	protection := secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32)))
	sessions, err := auth.NewSessionManager(repository, ids, clock, time.Hour, "https://endlessfs.test", true, protection)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapToken := secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x43}, 32)))
	service, err := identity.NewService(repository, benchmarkWebAuthn{}, sessions, ids, clock, identity.NewMutablePolicy(identity.RegistrationPolicy{}), bootstrapToken, "https://endlessfs.test")
	if err != nil {
		t.Fatal(err)
	}
	current.ledger.Reset()
	bootstrap, err := service.StartBootstrap(ctx, bootstrapToken, "Administrator")
	if err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "before/identity/bootstrap-options", modelEconomics, current.ledger)
	complete, err := service.VerifyRegistration(ctx, bootstrap.CeremonyID, bootstrap.BrowserBinding, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "before/identity/bootstrap-verify", modelEconomics, current.ledger)
	authentication, err := service.StartAuthentication(ctx)
	if err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "before/identity/authentication-options", modelEconomics, current.ledger)
	response, _ := json.Marshal(map[string]string{"userID": complete.UserID.String(), "credentialID": complete.CredentialID})
	if _, err := service.VerifyAuthentication(ctx, authentication.CeremonyID, authentication.BrowserBinding, response); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "before/identity/authentication-verify", modelEconomics, current.ledger)

	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
	bootstrapControl, err := openRecordDomain(ctx, backend, "bootstrap-control")
	if err != nil {
		t.Fatal(err)
	}
	ledger.Reset()
	if _, _, err := bootstrapControl.Get(ctx, "bootstrap-state"); err != nil {
		t.Fatal(err)
	}
	bootstrapCeremony := objectstore.MustKey("endlessfs/research/identity/bootstrap-ceremony.json")
	if _, err := backend.Put(ctx, bootstrapCeremony, []byte(`{"flow":"bootstrap"}`), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "after/identity/bootstrap-options", modelEconomics, ledger)
	bootstrapCoordinator, err := openMultiDomainCoordinator(ctx, backend, "bootstrap-registration")
	if err != nil {
		t.Fatal(err)
	}
	bootstrapChanges := make(map[string]map[string][]byte)
	for _, id := range []string{"administration", "capability", "owner"} {
		if err := bootstrapCoordinator.CreateDomain(ctx, id, map[string][]byte{"state": []byte(`{"value":"before"}`)}); err != nil {
			t.Fatal(err)
		}
		bootstrapChanges[id] = map[string][]byte{"state": []byte(`{"value":"after"}`)}
	}
	ledger.Reset()
	if _, err := backend.Get(ctx, bootstrapCeremony); err != nil {
		t.Fatal(err)
	}
	if err := bootstrapCoordinator.Commit(ctx, "bootstrap", bootstrapChanges); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "after/identity/bootstrap-verify", modelEconomics, ledger)
	authCeremony := objectstore.MustKey("endlessfs/research/identity/auth-ceremony.json")
	if _, err := backend.Put(ctx, authCeremony, []byte(`{"flow":"authentication"}`), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "after/identity/authentication-options", modelEconomics, ledger)
	authCoordinator, err := openMultiDomainCoordinator(ctx, backend, "authentication")
	if err != nil {
		t.Fatal(err)
	}
	authChanges := make(map[string]map[string][]byte)
	for _, id := range []string{"ceremony", "owner"} {
		if err := authCoordinator.CreateDomain(ctx, id, map[string][]byte{"state": []byte(`{"value":"before"}`)}); err != nil {
			t.Fatal(err)
		}
		authChanges[id] = map[string][]byte{"state": []byte(`{"value":"after"}`)}
	}
	sessionKey := objectstore.MustKey("endlessfs/research/identity/session.json")
	ledger.Reset()
	if _, err := backend.Get(ctx, authCeremony); err != nil {
		t.Fatal(err)
	}
	if err := authCoordinator.Commit(ctx, "authenticate", authChanges); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, sessionKey, []byte(`{"owner":"user","generation":1}`), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "after/identity/authentication-verify", modelEconomics, ledger)
}
