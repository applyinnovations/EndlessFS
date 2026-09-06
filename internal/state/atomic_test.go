package state_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/state"
)

func TestMemoryAtomicMutationPublishesAllChangesOrNone(t *testing.T) {
	ctx := context.Background()
	store := state.NewMemoryStore()
	first := state.MustKey(state.NamespaceAccounts, "owner")
	second := state.MustKey(state.NamespaceUsers, "owner")
	outcome, err := store.Mutate(ctx, state.Mutation{
		ID:          "create-owner",
		RetainUntil: time.Date(2050, 1, 1, 0, 0, 0, 0, time.UTC),
		Changes: []state.Change{
			{Key: first, Requirement: state.RequirementAbsent, Data: []byte("account")},
			{Key: second, Requirement: state.RequirementAbsent, Data: []byte("profile")},
		},
		Result: []byte("created"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Replayed || string(outcome.Result) != "created" || len(outcome.Changes) != 2 {
		t.Fatalf("outcome = %+v", outcome)
	}

	replay, err := store.Mutate(ctx, state.Mutation{
		ID:          "create-owner",
		RetainUntil: time.Date(2051, 1, 1, 0, 0, 0, 0, time.UTC),
		Changes: []state.Change{
			{Key: second, Requirement: state.RequirementAbsent, Data: []byte("profile")},
			{Key: first, Requirement: state.RequirementAbsent, Data: []byte("account")},
		},
		Result: []byte("created"),
	})
	if err != nil || !replay.Replayed || string(replay.Result) != "created" {
		t.Fatalf("replay = %+v, %v", replay, err)
	}

	failed := state.MustKey(state.NamespaceAccounts, "other")
	if _, err := store.Create(ctx, failed, []byte("winner")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Mutate(ctx, state.Mutation{
		ID: "must-not-partially-apply",
		Changes: []state.Change{
			{Key: first, Requirement: state.RequirementPresent, ExpectedVersion: outcome.Changes[0].Version, Data: []byte("changed")},
			{Key: failed, Requirement: state.RequirementAbsent, Data: []byte("loser")},
		},
	}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("failed mutation error = %v", err)
	}
	value, err := store.Get(ctx, first)
	if err != nil || string(value.Data) != "account" {
		t.Fatalf("first after failed mutation = %+v, %v", value, err)
	}
}

func TestMemoryAtomicMutationRejectsReusedIDAndInvalidChanges(t *testing.T) {
	ctx := context.Background()
	store := state.NewMemoryStore()
	key := state.MustKey(state.NamespaceAccounts, "owner")
	base := state.Mutation{ID: "same-id", Changes: []state.Change{{Key: key, Requirement: state.RequirementAbsent, Data: []byte("one")}}}
	if _, err := store.Mutate(ctx, base); err != nil {
		t.Fatal(err)
	}
	base.Changes[0].Data = []byte("two")
	if _, err := store.Mutate(ctx, base); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("reused ID error = %v", err)
	}

	for name, mutation := range map[string]state.Mutation{
		"empty-id":       {Changes: []state.Change{{Key: key, Requirement: state.RequirementAny, Data: []byte("x")}}},
		"empty-changes":  {ID: "empty"},
		"duplicate-key":  {ID: "duplicate", Changes: []state.Change{{Key: key, Requirement: state.RequirementAny, Data: []byte("x")}, {Key: key, Requirement: state.RequirementAny, Data: []byte("y")}}},
		"delete-data":    {ID: "delete-data", Changes: []state.Change{{Key: key, Requirement: state.RequirementPresent, Delete: true, Data: []byte("x")}}},
		"delete-any":     {ID: "delete-any", Changes: []state.Change{{Key: key, Requirement: state.RequirementAny, Delete: true}}},
		"version-absent": {ID: "version-absent", Changes: []state.Change{{Key: key, Requirement: state.RequirementAbsent, ExpectedVersion: "v", Data: []byte("x")}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.Mutate(ctx, mutation); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
