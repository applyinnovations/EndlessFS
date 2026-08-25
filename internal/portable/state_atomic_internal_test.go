package portable

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/state"
)

func TestAtomicStateMutationUsesOneConditionalPublicationAndReplaysLostSuccess(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	engine := openNamespaceTestEngine(t, budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger))
	owner := "WVhXWVhXWVhXWVhXWVhXWQ"
	account := state.MustKey(state.NamespaceAccounts, owner)
	profile := state.MustKey(state.NamespaceUsers, owner)
	mutation := state.Mutation{
		ID:          "atomic-owner-create",
		RetainUntil: time.Date(2055, 1, 1, 0, 0, 0, 0, time.UTC),
		Changes: []state.Change{
			{Key: account, Requirement: state.RequirementAbsent, Data: []byte("account")},
			{Key: profile, Requirement: state.RequirementAbsent, Data: []byte("profile")},
		},
		Result: []byte("ok"),
	}
	if _, err := engine.Mutate(ctx, mutation); err != nil {
		t.Fatal(err)
	}
	puts := 0
	for _, event := range ledger.Events() {
		if event.Kind == providerbudget.RequestObjectPut {
			puts++
		}
	}
	// Cold-domain registration may create inert catalog/tree objects, but the
	// application values themselves have exactly one visibility publication.
	if puts == 0 {
		t.Fatal("atomic mutation made no conditional publication")
	}
	ledger.Reset()
	replay, err := engine.Mutate(ctx, mutation)
	if err != nil || !replay.Replayed || string(replay.Result) != "ok" {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	for _, event := range ledger.Events() {
		if event.Kind == providerbudget.RequestObjectPut || event.Kind == providerbudget.RequestObjectDelete || event.Kind == providerbudget.RequestObjectCopy {
			t.Fatalf("exact replay wrote provider state: %+v", event)
		}
	}
}

func TestAtomicStateMutationRejectsCrossDomainBeforeProviderWrite(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	engine := openNamespaceTestEngine(t, budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger))
	ledger.Reset()
	_, err := engine.Mutate(ctx, state.Mutation{
		ID: "cross-domain",
		Changes: []state.Change{
			{Key: state.MustKey(state.NamespaceAccounts, "WVhXWVhXWVhXWVhXWVhXWQ"), Requirement: state.RequirementAbsent, Data: []byte("a")},
			{Key: state.MustKey(state.NamespaceAccounts, "aGhoaGhoaGhoaGhoaGhoaA"), Requirement: state.RequirementAbsent, Data: []byte("b")},
		},
	})
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("cross-domain mutation error = %v", err)
	}
	if events := ledger.Events(); len(events) != 0 {
		t.Fatalf("cross-domain denial touched provider: %+v", events)
	}
}
