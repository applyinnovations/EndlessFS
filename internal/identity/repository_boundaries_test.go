package identity

import (
	"context"
	"errors"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/state"
)

func TestRepositoryWrappersEncodingAndCredentialConsistency(t *testing.T) {
	store := state.NewMemoryStore()
	repository := NewRepository(store)
	owner := userID(t, 0x11)
	other := userID(t, 0x12)
	now := identityEpoch
	account := model.Account{SchemaVersion: model.SchemaVersion, UserID: owner, Status: model.AccountEnabled, CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateAccount(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	if accounts, err := repository.Accounts(context.Background()); err != nil || len(accounts) != 1 {
		t.Fatalf("accounts = %+v, %v", accounts, err)
	}
	if exists, err := repository.UserExists(context.Background()); err != nil || exists {
		t.Fatalf("empty users = %v, %v", exists, err)
	}
	name, _ := domain.ParseDisplayName("Owner")
	if err := repository.CreateProfile(context.Background(), model.Profile{UserID: owner, DisplayName: name}); err != nil {
		t.Fatal(err)
	}
	if exists, err := repository.UserExists(context.Background()); err != nil || !exists {
		t.Fatalf("existing users = %v, %v", exists, err)
	}
	if err := repository.CreateCredentialIndex(context.Background(), model.CredentialIndex{SchemaVersion: model.SchemaVersion, UserID: owner, CredentialIDs: []string{bearer(0x14)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Credentials(context.Background(), owner); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("missing indexed credential = %v", err)
	}
	credential := model.Credential{SchemaVersion: model.SchemaVersion, CredentialID: bearer(0x14), UserID: other, PublicKey: bearer(0x15), CreatedAt: now, LastUsedAt: now}
	if err := repository.CreateCredential(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Credentials(context.Background(), owner); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("foreign indexed credential = %v", err)
	}
	if err := createRecord(context.Background(), store, state.MustKey(state.NamespaceAccounts, "invalid"), struct{ Value chan int }{Value: make(chan int)}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("create marshal error = %v", err)
	}
	if _, err := swapRecord(context.Background(), store, state.MustKey(state.NamespaceAccounts, "invalid"), "v1", struct{ Value chan int }{Value: make(chan int)}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("swap marshal error = %v", err)
	}
}
