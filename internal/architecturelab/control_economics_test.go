package architecturelab

import (
	"context"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/state"
)

func TestControlRecordEconomicsBeforeAndAfter(t *testing.T) {
	ctx := context.Background()
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	current := openCurrentProviderHarness(t, "control-baseline")
	key := state.MustKey(state.NamespacePreferences, "control-record")
	current.ledger.Reset()
	version, err := current.engine.Create(ctx, key, []byte(`{"value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "before/control/create", model, current.ledger)
	if _, err := current.engine.Get(ctx, key); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "before/control/read", model, current.ledger)
	if _, err := current.engine.List(ctx, state.MustPrefix(state.NamespacePreferences), state.PageRequest{Limit: 100}); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "before/control/list-one", model, current.ledger)
	version, err = current.engine.CompareAndSwap(ctx, key, version, []byte(`{"value":2}`))
	if err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "before/control/update", model, current.ledger)
	if err := current.engine.Delete(ctx, key, version); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "before/control/delete", model, current.ledger)

	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
	after, err := openRecordDomain(ctx, backend, "control-after")
	if err != nil {
		t.Fatal(err)
	}
	ledger.Reset()
	if _, err := after.Mutate(ctx, RecordMutation{ID: "create", Key: "record", Value: []byte(`{"value":1}`)}); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "after/control/create", model, ledger)
	if _, _, err := after.Get(ctx, "record"); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "after/control/read", model, ledger)
	if _, err := after.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "after/control/list-one", model, ledger)
	if _, err := after.Mutate(ctx, RecordMutation{ID: "update", Key: "record", Value: []byte(`{"value":2}`)}); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "after/control/update", model, ledger)
	if _, err := after.Mutate(ctx, RecordMutation{ID: "delete", Key: "record", Delete: true}); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "after/control/delete", model, ledger)
}
