package state

import (
	"context"
	"errors"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func TestMemoryTransactionIsAtomicReplayableAndConflictBound(t *testing.T) {
	store := NewMemoryStore()
	left := MustKey(NamespaceRoles, "admins")
	right := MustKey(NamespaceAccounts, "owner")
	leftVersion, err := store.Create(context.Background(), left, []byte(`{"value":"old-left"}`))
	if err != nil {
		t.Fatal(err)
	}
	rightVersion, err := store.Create(context.Background(), right, []byte(`{"value":"old-right"}`))
	if err != nil {
		t.Fatal(err)
	}
	mutation := Mutation{ID: "cross-domain-transition", Result: []byte(`{"ok":true}`), Changes: []Change{
		{Key: left, Requirement: RequirementPresent, ExpectedVersion: leftVersion, Data: []byte(`{"value":"new-left"}`)},
		{Key: right, Requirement: RequirementPresent, ExpectedVersion: rightVersion, Data: []byte(`{"value":"new-right"}`)},
	}}
	outcome, err := store.Transact(context.Background(), mutation)
	if err != nil || outcome.Replayed || string(outcome.Result) != `{"ok":true}` {
		t.Fatalf("Transact() = %+v, %v", outcome, err)
	}
	replay, err := store.Transact(context.Background(), mutation)
	if err != nil || !replay.Replayed || replay.ID != outcome.ID {
		t.Fatalf("Transact() replay = %+v, %v", replay, err)
	}
	conflicting := mutation
	conflicting.Changes = append([]Change(nil), mutation.Changes...)
	conflicting.Changes[0].Data = []byte(`{"value":"different"}`)
	if _, err := store.Transact(context.Background(), conflicting); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("conflicting transition error = %v", err)
	}

	stale := Mutation{ID: "stale-cross-domain-transition", Changes: []Change{
		{Key: left, Requirement: RequirementPresent, ExpectedVersion: leftVersion, Data: []byte(`{"value":"partial-left"}`)},
		{Key: right, Requirement: RequirementAny, Data: []byte(`{"value":"partial-right"}`)},
	}}
	if _, err := store.Transact(context.Background(), stale); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale transition error = %v", err)
	}
	leftValue, _ := store.Get(context.Background(), left)
	rightValue, _ := store.Get(context.Background(), right)
	if string(leftValue.Data) != `{"value":"new-left"}` || string(rightValue.Data) != `{"value":"new-right"}` {
		t.Fatalf("failed transition was partial: left=%s right=%s", leftValue.Data, rightValue.Data)
	}
}
