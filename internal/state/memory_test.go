package state_test

import (
	"context"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/state/statecontract"
)

func TestContractMemoryStore(t *testing.T) {
	if testing.Short() {
		t.Skip("state contract suite")
	}
	statecontract.Run(t, func(*testing.T) state.Store {
		return state.NewMemoryStore()
	})
}

func TestMemoryStoreRejectsInvalidRecordSizes(t *testing.T) {
	store := state.NewMemoryStore()
	key := state.MustKey(state.NamespaceAccounts, "record-size")
	tooLarge := make([]byte, state.MaxRecordBytes+1)
	if _, err := store.Create(context.Background(), key, tooLarge); domain.KindOf(err) != domain.ErrorInvalid {
		t.Fatalf("Create(too large) error = %v, want invalid", err)
	}
	version, err := store.Create(context.Background(), key, []byte(`{"ok":true}`))
	if err != nil {
		t.Fatalf("Create(valid): %v", err)
	}
	if _, err := store.CompareAndSwap(context.Background(), key, version, tooLarge); domain.KindOf(err) != domain.ErrorInvalid {
		t.Fatalf("CompareAndSwap(too large) error = %v, want invalid", err)
	}
}
