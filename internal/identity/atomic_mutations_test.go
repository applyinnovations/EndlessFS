package identity

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
)

type failBeforeAtomicCommit struct {
	*state.MemoryStore
	fail      bool
	failAfter bool
}

func (store *failBeforeAtomicCommit) Mutate(ctx context.Context, mutation state.Mutation) (state.MutationOutcome, error) {
	if store.fail {
		store.fail = false
		return state.MutationOutcome{}, domain.NewError(domain.ErrorUnavailable, "injected process loss")
	}
	outcome, err := store.MemoryStore.Mutate(ctx, mutation)
	if err == nil && store.failAfter {
		store.failAfter = false
		return state.MutationOutcome{}, domain.NewError(domain.ErrorUnavailable, "injected lost success response")
	}
	return outcome, err
}

func (store *failBeforeAtomicCommit) Transact(ctx context.Context, mutation state.Mutation) (state.MutationOutcome, error) {
	return store.Mutate(ctx, mutation)
}

func TestCredentialIndexAndCredentialRemovalHaveOneAtomicBoundary(t *testing.T) {
	store := &failBeforeAtomicCommit{MemoryStore: state.NewMemoryStore()}
	repository := NewRepository(store)
	owner, _ := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x71}, 16)))
	now := time.Date(2049, 2, 3, 4, 5, 6, 0, time.UTC)
	credentialIDs := []string{base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)), base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))}
	for index, credentialID := range credentialIDs {
		record := model.Credential{SchemaVersion: model.SchemaVersion, CredentialID: credentialID, UserID: owner, PublicKey: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{byte(index + 11)}, 32)), CreatedAt: now, LastUsedAt: now}
		if err := repository.CreateCredential(ctxbg(), record); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.CreateCredentialIndex(ctxbg(), model.CredentialIndex{SchemaVersion: model.SchemaVersion, UserID: owner, CredentialIDs: credentialIDs}); err != nil {
		t.Fatal(err)
	}
	store.fail = true
	if err := repository.RemoveCredentialAtomic(ctxbg(), owner, credentialIDs[0]); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("injected removal error = %v", err)
	}
	if _, _, err := repository.Credential(ctxbg(), owner, credentialIDs[0]); err != nil {
		t.Fatalf("failed removal deleted credential: %v", err)
	}
	index, _, err := repository.CredentialIndex(ctxbg(), owner)
	if err != nil || len(index.CredentialIDs) != 2 {
		t.Fatalf("failed removal changed index: %+v, %v", index, err)
	}
	if err := repository.RemoveCredentialAtomic(ctxbg(), owner, credentialIDs[0]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.Credential(ctxbg(), owner, credentialIDs[0]); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("removed credential error = %v", err)
	}
	index, _, _ = repository.CredentialIndex(ctxbg(), owner)
	if len(index.CredentialIDs) != 1 || index.CredentialIDs[0] != credentialIDs[1] {
		t.Fatalf("credential index after removal = %+v", index)
	}
	if err := repository.RemoveCredentialAtomic(ctxbg(), owner, credentialIDs[1]); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("final credential removal error = %v", err)
	}
}

func TestSessionRotationNeverDeletesTheOldSessionWithoutCreatingTheNewOne(t *testing.T) {
	store := &failBeforeAtomicCommit{MemoryStore: state.NewMemoryStore()}
	repository := NewRepository(store)
	owner, _ := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x72}, 16)))
	oldSecret := secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x81}, 32)))
	newSecret := secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x82}, 32)))
	oldToken, _ := secret.ScopeBearerToken(owner, oldSecret.Reveal())
	newToken, _ := secret.ScopeBearerToken(owner, newSecret.Reveal())
	now := time.Date(2049, 2, 3, 4, 5, 6, 0, time.UTC)
	oldRecord := model.Session{SchemaVersion: model.SchemaVersion, SessionTokenHash: secret.Hash(oldToken), UserID: owner, AuthEpoch: 1, CSRFTokenHash: secret.Hash("csrf-old"), CreatedAt: now, ExpiresAt: now.Add(time.Hour), AuthnCredentialIDHash: secret.Hash("credential")}
	newRecord := oldRecord
	newRecord.SessionTokenHash = secret.Hash(newToken)
	if err := repository.CreateSession(context.Background(), oldToken, oldRecord); err != nil {
		t.Fatal(err)
	}
	_, oldVersion, _ := repository.Session(context.Background(), oldToken)
	store.fail = true
	if err := repository.RotateSessionAtomic(context.Background(), oldToken, oldVersion, newToken, newRecord); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("injected rotation error = %v", err)
	}
	if _, _, err := repository.Session(context.Background(), oldToken); err != nil {
		t.Fatalf("failed rotation removed old session: %v", err)
	}
	if _, _, err := repository.Session(context.Background(), newToken); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("failed rotation created new session: %v", err)
	}
	if err := repository.RotateSessionAtomic(context.Background(), oldToken, oldVersion, newToken, newRecord); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.Session(context.Background(), oldToken); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("old session after rotation = %v", err)
	}
	if _, _, err := repository.Session(context.Background(), newToken); err != nil {
		t.Fatalf("new session after rotation = %v", err)
	}
}

func TestRegistrationCommitHasOneCrashSafeTransition(t *testing.T) {
	store := &failBeforeAtomicCommit{MemoryStore: state.NewMemoryStore()}
	repository := NewRepository(store)
	reader := &deterministicReader{next: 1}
	ids := domain.NewIDGenerator(reader)
	clock := domain.NewFixedClock(identityEpoch)
	sessions, err := auth.NewSessionManager(repository, ids, clock, 12*time.Hour, "https://drive.example.test", true, secret.Value(bearer(0x61)))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repository, fakeWebAuthn{}, sessions, ids, clock, NewMutablePolicy(RegistrationPolicy{AllowPublic: true}), "", "https://drive.example.test")
	if err != nil {
		t.Fatal(err)
	}
	start, err := service.StartRegistration(context.Background(), RegistrationStartRequest{DisplayName: "Atomic User", ClientKey: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	store.fail = true
	if _, err := service.VerifyRegistration(context.Background(), start.CeremonyID, start.BrowserBinding, fakeRegistrationResponse(0x91)); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("injected registration error = %v", err)
	}
	ceremony, _, err := repository.Ceremony(context.Background(), start.CeremonyID)
	if err != nil || ceremony.ConsumedAt != nil || ceremony.OperationID != "" {
		t.Fatalf("failed transition consumed ceremony: %+v, %v", ceremony, err)
	}
	if _, _, err := repository.Profile(context.Background(), *ceremony.UserID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("failed transition materialized profile: %v", err)
	}
	if _, err := repository.Credentials(context.Background(), *ceremony.UserID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("failed transition materialized credentials: %v", err)
	}
	complete, err := service.VerifyRegistration(context.Background(), start.CeremonyID, start.BrowserBinding, fakeRegistrationResponse(0x91))
	if err != nil || complete.UserID != *ceremony.UserID {
		t.Fatalf("registration retry = %+v, %v", complete, err)
	}
	account, _, accountErr := repository.Account(context.Background(), complete.UserID)
	credentials, credentialErr := repository.Credentials(context.Background(), complete.UserID)
	if accountErr != nil || credentialErr != nil || account.Status != model.AccountEnabled || len(credentials) != 1 {
		t.Fatalf("atomic registration result: account=%+v credentials=%+v errors=%v/%v", account, credentials, accountErr, credentialErr)
	}
}

func TestAuthenticationCommitIsAtomicAndLostSuccessIsReplayable(t *testing.T) {
	store := &failBeforeAtomicCommit{MemoryStore: state.NewMemoryStore()}
	repository := NewRepository(store)
	reader := &deterministicReader{next: 1}
	ids := domain.NewIDGenerator(reader)
	clock := domain.NewFixedClock(identityEpoch)
	sessions, err := auth.NewSessionManager(repository, ids, clock, 12*time.Hour, "https://drive.example.test", true, secret.Value(bearer(0x61)))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repository, fakeWebAuthn{}, sessions, ids, clock, NewMutablePolicy(RegistrationPolicy{AllowPublic: true}), "", "https://drive.example.test")
	if err != nil {
		t.Fatal(err)
	}
	registration, err := service.StartRegistration(ctxbg(), RegistrationStartRequest{DisplayName: "Atomic Login", ClientKey: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	registered, err := service.VerifyRegistration(ctxbg(), registration.CeremonyID, registration.BrowserBinding, fakeRegistrationResponse(0x92))
	if err != nil {
		t.Fatal(err)
	}
	credential, credentialVersion, err := repository.Credential(ctxbg(), registered.UserID, registered.CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	start, err := service.StartAuthentication(ctxbg())
	if err != nil {
		t.Fatal(err)
	}
	response, _ := json.Marshal(fakeResponse{UserID: registered.UserID.String(), CredentialID: registered.CredentialID})
	store.fail = true
	if _, err := service.VerifyAuthentication(ctxbg(), start.CeremonyID, start.BrowserBinding, response); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("injected authentication error = %v", err)
	}
	ceremony, _, err := repository.Ceremony(ctxbg(), start.CeremonyID)
	if err != nil || ceremony.ConsumedAt != nil || ceremony.OperationID != "" || ceremony.UserID != nil {
		t.Fatalf("failed authentication changed ceremony: %+v, %v", ceremony, err)
	}
	unchanged, unchangedVersion, err := repository.Credential(ctxbg(), registered.UserID, registered.CredentialID)
	if err != nil || unchanged.SignCount != credential.SignCount || unchangedVersion != credentialVersion {
		t.Fatalf("failed authentication changed credential: %+v/%q, want %+v/%q; %v", unchanged, unchangedVersion, credential, credentialVersion, err)
	}

	store.failAfter = true
	if _, err := service.VerifyAuthentication(ctxbg(), start.CeremonyID, start.BrowserBinding, response); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("lost-success authentication error = %v", err)
	}
	replayed, err := service.VerifyAuthentication(ctxbg(), start.CeremonyID, start.BrowserBinding, response)
	if err != nil {
		t.Fatalf("authentication replay error = %v", err)
	}
	if _, err := sessions.Authenticate(ctxbg(), replayed.Token.Reveal()); err != nil {
		t.Fatalf("replayed session is unusable: %v", err)
	}
	committed, _, err := repository.Credential(ctxbg(), registered.UserID, registered.CredentialID)
	if err != nil || committed.SignCount != credential.SignCount+1 {
		t.Fatalf("committed credential = %+v, %v", committed, err)
	}
}

func ctxbg() context.Context { return context.Background() }
