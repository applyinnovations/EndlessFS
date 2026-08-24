package portable_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/state"
)

func TestProviderBudgetStateStoreContract(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	backend := providerbudget.InstrumentBackend(providerbudget.RoleState, objectmemory.New(), ledger)
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
		budget, ok := ratchet.Latest(name)
		if !ok {
			t.Fatalf("provider budget %q is missing", name)
		}
		if report, err := budget.CheckRatchet(economics, ledger.Events()); err != nil {
			t.Errorf("%s: %v; observed=%+v", name, err, report.Totals)
		}
	}
	key := state.MustKey(state.NamespacePreferences, "budget-user")

	ledger.Reset()
	if _, err := engine.Get(ctx, key); err == nil {
		t.Fatal("Get(missing) unexpectedly succeeded")
	}
	check("state-get-missing")

	ledger.Reset()
	if _, err := engine.List(ctx, state.MustPrefix(state.NamespacePreferences), state.PageRequest{Limit: 10}); err != nil {
		t.Fatal(err)
	}
	check("state-list-empty")

	ledger.Reset()
	version, err := engine.Create(ctx, key, []byte("one"))
	if err != nil {
		t.Fatal(err)
	}
	check("state-create")

	ledger.Reset()
	if _, err := engine.Get(ctx, key); err != nil {
		t.Fatal(err)
	}
	check("state-get")

	ledger.Reset()
	version, err = engine.CompareAndSwap(ctx, key, version, []byte("two"))
	if err != nil {
		t.Fatal(err)
	}
	check("state-compare-and-swap")

	ledger.Reset()
	if err := engine.Delete(ctx, key, version); err != nil {
		t.Fatal(err)
	}
	check("state-delete")
}
