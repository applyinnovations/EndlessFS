package portable_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestProviderBudgetStateStoreContract(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	backend := budgettest.WrapClassified(providerbudget.RoleState, objectmemory.New(), ledger, func(_ providerbudget.RequestKind, target string) string {
		return storageformat.ClassifyEconomicsTarget(target)
	})
	clock := domain.NewFixedClock(time.Date(2050, 1, 2, 3, 4, 5, 0, time.UTC))
	engine, err := portable.Open(ctx, portable.Options{
		Backend: backend, FileBackend: objectmemory.New(), Clock: clock,
		IDs:      domain.NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte("provider-budget-state-entropy"), 1<<15))),
		Writer:   portable.WriterConfiguration{WriterSetID: "state-budget", ConfigurationDigest: "state-budget-v1", KeyringIdentifiers: []string{"budget-key"}},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x51}, 32),
	})
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
		if report, err := ratchet.CheckExact(name, economics, []providerbudget.Role{providerbudget.RoleState}, ledger.Events()); err != nil {
			t.Errorf("%s: %v; observed=%+v; events=%+v", name, err, report.Totals, ledger.Events())
		}
	}
	key := state.MustKey(state.NamespacePreferences, "budget-user")

	ledger.Reset()
	if _, err := engine.Get(ctx, key); err == nil {
		t.Fatal("Get(missing) unexpectedly succeeded")
	}
	check("state-get-missing-schema-011")

	ledger.Reset()
	if _, err := engine.List(ctx, state.MustPrefix(state.NamespacePreferences), state.PageRequest{Limit: 10}); err != nil {
		t.Fatal(err)
	}
	check("state-list-empty-schema-011")

	ledger.Reset()
	version, err := engine.Create(ctx, key, []byte("one"))
	if err != nil {
		t.Fatal(err)
	}
	check("state-create-schema-011")

	ledger.Reset()
	if _, err := engine.Get(ctx, key); err != nil {
		t.Fatal(err)
	}
	check("state-get-schema-011")

	ledger.Reset()
	version, err = engine.CompareAndSwap(ctx, key, version, []byte("two"))
	if err != nil {
		t.Fatal(err)
	}
	check("state-compare-and-swap-schema-011")

	ledger.Reset()
	if err := engine.Delete(ctx, key, version); err != nil {
		t.Fatal(err)
	}
	check("state-delete-schema-011")

	owner := "YnVkZ2V0LXVzZXItMDAwMDAwMA"
	ledger.Reset()
	if _, err := engine.Mutate(ctx, state.Mutation{
		ID: "budget-owner-atomic-create",
		Changes: []state.Change{
			{Key: state.MustKey(state.NamespaceUsers, owner), Requirement: state.RequirementAbsent, Data: []byte("profile")},
			{Key: state.MustKey(state.NamespaceAccounts, owner), Requirement: state.RequirementAbsent, Data: []byte("account")},
		},
		Result: []byte("created"),
	}); err != nil {
		t.Fatal(err)
	}
	check("state-mutate-two-records-schema-011")

	ledger.Reset()
	if _, err := engine.Transact(ctx, state.Mutation{
		ID: "budget-cross-domain-create",
		Changes: []state.Change{
			{Key: state.MustKey(state.NamespacePreferences, owner), Requirement: state.RequirementAbsent, Data: []byte("preference")},
			{Key: state.MustKey(state.NamespaceRoles, "administrators"), Requirement: state.RequirementAbsent, Data: []byte("roles")},
		},
		Result: []byte("committed"),
	}); err != nil {
		t.Fatal(err)
	}
	check("state-transact-two-domains-schema-011")
}
