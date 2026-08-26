package portable_test

import (
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

func checkMaintenanceProviderBudget(t *testing.T, name string, roles []providerbudget.Role, ledgers ...*providerbudget.Ledger) {
	t.Helper()
	economics, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	ratchet, err := gcs.RegionalStandardFlatBudgetRatchet()
	if err != nil {
		t.Fatal(err)
	}
	events := make([]providerbudget.Event, 0)
	for _, ledger := range ledgers {
		events = append(events, ledger.Events()...)
	}
	if report, err := ratchet.CheckExact(name, economics, roles, events); err != nil {
		t.Errorf("%s: %v; observed=%+v; events=%+v", name, err, report.Totals, events)
	}
}

func TestProviderBudgetCurrentSchemaMaintenanceWorkflows(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2056, 1, 2, 3, 4, 5, 0, time.UTC))
	stateBase, fileBase := objectmemory.New(), objectmemory.New()
	if _, err := portable.Open(ctx, schemaSplitMigrationOptions(stateBase, fileBase, clock, 231, nil)); err != nil {
		t.Fatal(err)
	}
	stateLedger, fileLedger := providerbudget.NewLedger(), providerbudget.NewLedger()
	stateBackend := budgettest.WrapClassified(providerbudget.RoleState, stateBase, stateLedger, func(_ providerbudget.RequestKind, target string) string {
		return storageformat.ClassifyEconomicsTarget(target)
	})
	fileBackend := budgettest.Wrap(providerbudget.RoleFile, fileBase, fileLedger)
	engine, err := portable.Open(ctx, schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 232, nil))
	if err != nil {
		t.Fatal(err)
	}
	checkMaintenanceProviderBudget(t, "maintenance-startup-warm-schema-009", []providerbudget.Role{providerbudget.RoleState, providerbudget.RoleFile}, stateLedger, fileLedger)

	stateLedger.Reset()
	fileLedger.Reset()
	if _, err := engine.GateStatus(ctx); err != nil {
		t.Fatal(err)
	}
	checkMaintenanceProviderBudget(t, "maintenance-gate-status-schema-009", []providerbudget.Role{providerbudget.RoleState, providerbudget.RoleFile}, stateLedger, fileLedger)

	stateLedger.Reset()
	fileLedger.Reset()
	checkpoint, err := engine.CreateCheckpoint(ctx, "provider-budget-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	checkMaintenanceProviderBudget(t, "maintenance-checkpoint-minimal-schema-009", []providerbudget.Role{providerbudget.RoleState, providerbudget.RoleFile}, stateLedger, fileLedger)

	stateLedger.Reset()
	fileLedger.Reset()
	if err := engine.VerifyCheckpoint(ctx, checkpoint.CheckpointID); err != nil {
		t.Fatal(err)
	}
	checkMaintenanceProviderBudget(t, "maintenance-verify-checkpoint-minimal-schema-009", []providerbudget.Role{providerbudget.RoleState, providerbudget.RoleFile}, stateLedger, fileLedger)

	stateLedger.Reset()
	fileLedger.Reset()
	visited := 0
	if err := engine.VisitCheckpointObjects(ctx, checkpoint.CheckpointID, func(storageformat.CheckpointObject) error {
		visited++
		return nil
	}); err != nil || visited == 0 {
		t.Fatalf("VisitCheckpointObjects() visited %d objects: %v", visited, err)
	}
	checkMaintenanceProviderBudget(t, "maintenance-visit-checkpoint-minimal-schema-009", []providerbudget.Role{providerbudget.RoleState, providerbudget.RoleFile}, stateLedger, fileLedger)

	stateLedger.Reset()
	fileLedger.Reset()
	if err := engine.OpenWrites(ctx, checkpoint.CheckpointID); err != nil {
		t.Fatal(err)
	}
	checkMaintenanceProviderBudget(t, "maintenance-open-writes-schema-009", []providerbudget.Role{providerbudget.RoleState, providerbudget.RoleFile}, stateLedger, fileLedger)
}

func TestProviderBudgetTransitionRecoveryWorkflow(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2057, 1, 2, 3, 4, 5, 0, time.UTC))
	base := objectmemory.New()
	ledger := providerbudget.NewLedger()
	backend := budgettest.WrapClassified(providerbudget.RoleState, base, ledger, func(_ providerbudget.RequestKind, target string) string {
		return storageformat.ClassifyEconomicsTarget(target)
	})
	failure := &stepFailure{step: portable.StepTransitionAfterParticipantPrepared}
	options := schemaMigrationOptions(backend, clock, 233, failure)
	engine, err := portable.Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	admin := state.MustKey(state.NamespaceRoles, "budget-recovery-admins")
	owner := state.MustKey(state.NamespacePreferences, "WVhXWVhXWVhXWVhXWVhXWQ")
	adminVersion, err := engine.Create(ctx, admin, []byte("old-admin"))
	if err != nil {
		t.Fatal(err)
	}
	ownerVersion, err := engine.Create(ctx, owner, []byte("old-owner"))
	if err != nil {
		t.Fatal(err)
	}
	mutation := state.Mutation{ID: "provider-budget-transition-recovery", Changes: []state.Change{
		{Key: admin, Requirement: state.RequirementPresent, ExpectedVersion: adminVersion, Data: []byte("new-admin")},
		{Key: owner, Requirement: state.RequirementPresent, ExpectedVersion: ownerVersion, Data: []byte("new-owner")},
	}}
	if _, err := engine.Transact(ctx, mutation); err == nil {
		t.Fatal("injected transition interruption unexpectedly completed")
	}
	options = schemaMigrationOptions(backend, clock, 234, nil)
	restarted, err := portable.Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	ledger.Reset()
	outcome, err := restarted.Transact(ctx, mutation)
	if err != nil || outcome.ID != mutation.ID || !outcome.Replayed {
		t.Fatalf("recovered transition = %+v, %v", outcome, err)
	}
	checkMaintenanceProviderBudget(t, "maintenance-transition-recovery-schema-009", []providerbudget.Role{providerbudget.RoleState}, ledger)
}

func TestProviderBudgetMigration008To009MinimalFixture(t *testing.T) {
	ctx := context.Background()
	var family storageSchemaFixtureEntry
	for _, candidate := range storageSchemaFixtures {
		if candidate.schemaID == "endlessfs-portable-v1/schema-008" && candidate.profile == "portable-minimal" {
			family = candidate
			break
		}
	}
	if family.file == "" {
		t.Fatal("schema-008 portable-minimal fixture is missing")
	}
	fixture := loadStorageSchemaFixture(t, family)
	stateBase, fileBase := objectmemory.New(), objectmemory.New()
	if err := stateBase.Import(fixture.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBase.Import(fixture.FileObjects); err != nil {
		t.Fatal(err)
	}
	stateLedger, fileLedger := providerbudget.NewLedger(), providerbudget.NewLedger()
	stateBackend := budgettest.WrapClassified(providerbudget.RoleState, stateBase, stateLedger, func(_ providerbudget.RequestKind, target string) string {
		return storageformat.ClassifyEconomicsTarget(target)
	})
	fileBackend := budgettest.Wrap(providerbudget.RoleFile, fileBase, fileLedger)
	options := schemaSplitMigrationOptions(stateBackend, fileBackend, domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour)), 235, nil)
	options.Writer = currentWriterForSchemaFixture(t, fixture)
	if _, err := portable.Open(ctx, options); err != nil {
		t.Fatal(err)
	}
	checkMaintenanceProviderBudget(t, "maintenance-migration-008-to-009-minimal-fixture", []providerbudget.Role{providerbudget.RoleState, providerbudget.RoleFile}, stateLedger, fileLedger)
}
