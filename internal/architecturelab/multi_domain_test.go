package architecturelab

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

func TestMultiDomainCommitHasOneDecisionPoint(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
	coordinator, err := openMultiDomainCoordinator(ctx, backend, "identity")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"capability", "owner"} {
		if err := coordinator.CreateDomain(ctx, id, map[string][]byte{"state": []byte(`{"value":"before"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	coordinator.beforeDecision = func() error {
		for _, id := range []string{"capability", "owner"} {
			value, err := coordinator.Get(ctx, id, "state")
			if err != nil || string(value) != `{"value":"before"}` {
				t.Fatalf("%s became visible before decision: %q, %v", id, value, err)
			}
		}
		return nil
	}
	ledger.Reset()
	if err := coordinator.Commit(ctx, "registration", map[string]map[string][]byte{
		"capability": {"state": []byte(`{"value":"consumed"}`)},
		"owner":      {"state": []byte(`{"value":"created"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	coordinator.beforeDecision = nil
	for id, want := range map[string]string{"capability": `{"value":"consumed"}`, "owner": `{"value":"created"}`} {
		value, err := coordinator.Get(ctx, id, "state")
		if err != nil || string(value) != want {
			t.Fatalf("%s after commit = %q, %v; want %q", id, value, err, want)
		}
	}
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	// Exclude the proof reads in beforeDecision and the postcondition reads;
	// their operation labels distinguish them from the commit protocol.
	events := make([]providerbudget.Event, 0)
	for _, event := range ledger.Events() {
		if event.Operation == "multi-domain-commit" {
			events = append(events, event)
		}
	}
	totals, err := model.Estimate(events)
	if err != nil {
		t.Fatal(err)
	}
	if totals.Requests != 8 {
		t.Fatalf("two-domain commit requests=%d, want 8; events=%+v", totals.Requests, events)
	}
	t.Logf("after/control/two-domain-commit requests=%d costPicoUSD=%d serialP95=%d criticalP95=%d requestBytes=%d responseBytes=%d", totals.Requests, totals.CostPicoUSD, totals.P95Micros, totals.CriticalP95Micros, totals.RequestBytes, totals.ResponseBytes)
}

func TestMultiDomainEconomicsScalesOnlyWithTouchedDomains(t *testing.T) {
	ctx := context.Background()
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{2, 3, 8} {
		t.Run(fmt.Sprintf("domains-%d", count), func(t *testing.T) {
			ledger := providerbudget.NewLedger()
			backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
			coordinator, err := openMultiDomainCoordinator(ctx, backend, fmt.Sprintf("domains-%d", count))
			if err != nil {
				t.Fatal(err)
			}
			changes := make(map[string]map[string][]byte, count)
			for index := 0; index < count; index++ {
				id := fmt.Sprintf("domain-%d", index)
				if err := coordinator.CreateDomain(ctx, id, map[string][]byte{"state": []byte(`{"value":"before"}`)}); err != nil {
					t.Fatal(err)
				}
				changes[id] = map[string][]byte{"state": []byte(`{"value":"after"}`)}
			}
			ledger.Reset()
			if err := coordinator.Commit(ctx, "commit", changes); err != nil {
				t.Fatal(err)
			}
			totals, err := model.Estimate(ledger.Events())
			if err != nil {
				t.Fatal(err)
			}
			if want := int64(2 + 3*count); totals.Requests != want {
				t.Fatalf("requests=%d, want %d", totals.Requests, want)
			}
			t.Logf("after/control/multi-domain-%d requests=%d costPicoUSD=%d serialP95=%d criticalP95=%d requestBytes=%d responseBytes=%d", count, totals.Requests, totals.CostPicoUSD, totals.P95Micros, totals.CriticalP95Micros, totals.RequestBytes, totals.ResponseBytes)
		})
	}
}

func TestMultiDomainRecoveryPreservesAtomicVisibility(t *testing.T) {
	ctx := context.Background()
	for _, phase := range []string{"prepared", "decided"} {
		t.Run(phase, func(t *testing.T) {
			backend := objectmemory.New()
			coordinator, err := openMultiDomainCoordinator(ctx, backend, "recovery-"+phase)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"capability", "owner"} {
				if err := coordinator.CreateDomain(ctx, id, map[string][]byte{"state": []byte(`{"value":"before"}`)}); err != nil {
					t.Fatal(err)
				}
			}
			injected := errors.New("injected crash")
			if phase == "prepared" {
				coordinator.afterPrepare = func() error { return injected }
			} else {
				coordinator.afterDecision = func() error { return injected }
			}
			err = coordinator.Commit(ctx, "registration", map[string]map[string][]byte{
				"capability": {"state": []byte(`{"value":"consumed"}`)},
				"owner":      {"state": []byte(`{"value":"created"}`)},
			})
			if !errors.Is(err, injected) {
				t.Fatalf("Commit() error=%v", err)
			}
			wantBefore := phase == "prepared"
			for _, id := range []string{"capability", "owner"} {
				value, err := coordinator.Get(ctx, id, "state")
				if err != nil || (string(value) == `{"value":"before"}`) != wantBefore {
					t.Fatalf("%s visibility after %s crash = %q, %v", id, phase, value, err)
				}
			}
			coordinator.afterPrepare, coordinator.afterDecision = nil, nil
			if err := coordinator.Recover(ctx, "registration"); err != nil {
				t.Fatal(err)
			}
			for id, want := range map[string]string{"capability": `{"value":"consumed"}`, "owner": `{"value":"created"}`} {
				value, err := coordinator.Get(ctx, id, "state")
				if err != nil || string(value) != want {
					t.Fatalf("%s recovered = %q, %v; want %q", id, value, err, want)
				}
			}
		})
	}
}
