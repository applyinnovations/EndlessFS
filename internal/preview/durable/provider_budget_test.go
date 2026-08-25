package durable_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/preview/durable"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/secret"
)

func TestProviderBudgetDurablePreviewStore(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2049, 1, 2, 3, 4, 5, 0, time.UTC))
	base := objectmemory.New()
	server := httptest.NewServer(base)
	t.Cleanup(server.Close)
	ids := domain.NewIDGenerator(bytes.NewReader(deterministicBytes(4 << 20)))
	if err := base.ConfigureDataPlane(server.URL, clock, ids); err != nil {
		t.Fatal(err)
	}
	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RolePreviewArtifact, base, ledger)
	store, err := durable.New(durable.Options{
		Backend: backend, Transfers: backend, Clock: clock, IDs: ids,
		Key: secret.Value(testBearer(0x41)), CapabilityTTL: time.Minute,
		DataOrigin: server.URL, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	model, err := gcs.RegionalStandardFlatEconomics()
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
		if report, err := budget.CheckRatchet(model, ledger.Events()); err != nil {
			t.Errorf("%s: %v; observed=%+v; events=%+v", name, err, report.Totals, ledger.Events())
		}
		ledger.Reset()
	}

	binding := testBinding(t)
	artifact := testArtifact("budget-generation", binding.Variant)

	if err := store.Check(ctx); err != nil {
		t.Fatal(err)
	}
	check("preview-check")

	claim, err := store.Claim(ctx, binding, "budget-claim", clock.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	check("preview-claim-new")

	if err := store.Commit(ctx, binding, claim, artifact); err != nil {
		t.Fatal(err)
	}
	check("preview-commit")

	if _, err := store.Latest(ctx, binding); err != nil {
		t.Fatal(err)
	}
	check("preview-latest")

	if _, err := store.Read(ctx, binding, artifact.GenerationID); err != nil {
		t.Fatal(err)
	}
	check("preview-read")

	if _, err := store.CreateDownload(ctx, binding, artifact.GenerationID); err != nil {
		t.Fatal(err)
	}
	check("preview-create-download")

	nextClaim, err := store.Claim(ctx, binding, "budget-release", clock.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ledger.Reset()
	if err := store.Release(ctx, binding, nextClaim); err != nil {
		t.Fatal(err)
	}
	check("preview-release")
}
