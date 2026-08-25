package identity

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
)

type failBeforeAtomicCommit struct {
	*state.MemoryStore
	fail bool
}

func (store *failBeforeAtomicCommit) Mutate(ctx context.Context, mutation state.Mutation) (state.MutationOutcome, error) {
	if store.fail {
		store.fail = false
		return state.MutationOutcome{}, domain.NewError(domain.ErrorUnavailable, "injected process loss")
	}
	return store.MemoryStore.Mutate(ctx, mutation)
}

func (store *failBeforeAtomicCommit) Transact(ctx context.Context, mutation state.Mutation) (state.MutationOutcome, error) {
	return store.Mutate(ctx, mutation)
}

func TestCredentialIndexAndCredentialRemovalHaveOneAtomicBoundary(t *testing.T) {
	store := &failBeforeAtomicCommit{MemoryStore: state.NewMemoryStore()}
	repository := NewRepository(store)
	owner, _ := domain.ParseUserID("aWRlbnRpdHktb3duZXIwMQ")
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
	owner, _ := domain.ParseUserID("c2Vzc2lvbi1vd25lci0wMQ")
	oldSecret := secret.Value("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	newSecret := secret.Value("BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB")
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

func ctxbg() context.Context { return context.Background() }
