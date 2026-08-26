package theme

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestProviderBudgetThemePreferenceWorkflows(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	backend := budgettest.WrapClassified(providerbudget.RoleState, objectmemory.New(), ledger, func(_ providerbudget.RequestKind, target string) string {
		return storageformat.ClassifyEconomicsTarget(target)
	})
	clock := domain.NewFixedClock(time.Date(2053, 1, 2, 3, 4, 5, 0, time.UTC))
	engine, err := portable.Open(ctx, portable.Options{
		Backend: backend, FileBackend: objectmemory.New(), Clock: clock,
		IDs:      domain.NewIDGenerator(strings.NewReader(strings.Repeat("theme-budget-entropy-0123456789", 1<<14))),
		Writer:   portable.WriterConfiguration{WriterSetID: "theme-budget", ConfigurationDigest: "theme-budget-v1", KeyringIdentifiers: []string{"budget-key"}},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x45}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(registry, engine, "endlessfs-light", "endlessfs-dark", true, clock)
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
		if report, checkErr := ratchet.CheckExact(name, economics, []providerbudget.Role{providerbudget.RoleState}, ledger.Events()); checkErr != nil {
			t.Errorf("%s: %v; observed=%+v; events=%+v", name, checkErr, report.Totals, ledger.Events())
		}
		ledger.Reset()
	}
	userID := themeUserID(t)
	ledger.Reset()
	if _, _, err := manager.Preference(ctx, userID); err != nil {
		t.Fatal(err)
	}
	check("theme-get-default-schema-009")
	if _, err := manager.SetPreference(ctx, userID, "endlessfs-dark"); err != nil {
		t.Fatal(err)
	}
	check("theme-set-create-schema-009")
	if _, err := manager.SetPreference(ctx, userID, "endlessfs-light"); err != nil {
		t.Fatal(err)
	}
	check("theme-set-update-schema-009")
}
