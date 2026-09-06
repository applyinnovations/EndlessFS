package identity

import (
	"context"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/state"
)

type countingAtomicState struct {
	state.AtomicStore
	lists   int
	deletes int
	mutates int
}

func (store *countingAtomicState) List(ctx context.Context, prefix state.Prefix, request state.PageRequest) (state.Page, error) {
	store.lists++
	return store.AtomicStore.List(ctx, prefix, request)
}

func (store *countingAtomicState) Delete(ctx context.Context, key state.Key, version state.Version) error {
	store.deletes++
	return store.AtomicStore.Delete(ctx, key, version)
}

func (store *countingAtomicState) Mutate(ctx context.Context, mutation state.Mutation) (state.MutationOutcome, error) {
	store.mutates++
	return store.AtomicStore.Mutate(ctx, mutation)
}

func TestRevokeUserSessionsAdvancesOneAuthEpochWithoutSessionScan(t *testing.T) {
	ctx := context.Background()
	owner := userID(t, 0x29)
	store := &countingAtomicState{AtomicStore: state.NewMemoryStore()}
	repository := NewRepository(store)
	account := model.Account{SchemaVersion: model.SchemaVersion, UserID: owner, Status: model.AccountEnabled, AuthEpoch: 11, CreatedAt: identityEpoch, UpdatedAt: identityEpoch}
	if err := repository.CreateAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	if err := repository.RevokeUserSessions(ctx, owner); err != nil {
		t.Fatal(err)
	}
	updated, _, err := repository.Account(ctx, owner)
	if err != nil || updated.AuthEpoch != 12 {
		t.Fatalf("updated account = %+v, %v", updated, err)
	}
	if store.lists != 0 || store.deletes != 0 || store.mutates != 1 {
		t.Fatalf("revocation calls: list=%d delete=%d mutate=%d", store.lists, store.deletes, store.mutates)
	}
}
