package preview_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// These vectors isolate the new durable state plane. Existing preview-store
// and source-transfer suites retain their independent provider-role budgets;
// the source here is an instrumented deterministic provider, not live GCS.
func TestProviderBudgetThumbnailQueueStateLifecycle(t *testing.T) {
	env := newPreviewEnvironmentWithoutService(t)
	ledger := providerbudget.NewLedger()
	backend := budgettest.WrapClassified(providerbudget.RoleState, objectmemory.New(), ledger, func(_ providerbudget.RequestKind, target string) string {
		return storageformat.ClassifyEconomicsTarget(target)
	})
	engine, err := portable.Open(t.Context(), portable.Options{Backend: backend, Clock: env.clock, IDs: env.ids, LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x51}, 32), Writer: portable.WriterConfiguration{WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "queue-economics", KeyringIdentifiers: []string{"session-v1"}}})
	if err != nil {
		t.Fatal(err)
	}
	env.applicationState = engine
	env.service = queueReplica(t, env, env.generator, 1)
	items := queueImages(t, env, 64)
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
		if _, err := budgettest.CheckExact(t, ratchet, name, model, []providerbudget.Role{providerbudget.RoleState}, ledger.Events()); err != nil {
			t.Error(err)
		}
	}
	before := env.source.Instrumentation()
	ledger.Reset()
	resolveQueue(t, env.service, env, items)
	check("thumbnail-queue-state-admit-64-schema-013")
	assertNoSourceRead(t, before, env.source.Instrumentation())
	env.clock.Advance(time.Second)
	ledger.Reset()
	resolveQueue(t, env.service, env, items)
	check("thumbnail-queue-state-refresh-64-schema-013")
	assertNoSourceRead(t, before, env.source.Instrumentation())
	ledger.Reset()
	for range 64 {
		if worked, err := env.service.WorkOne(t.Context()); err != nil || !worked {
			t.Fatalf("durable queue drain: %v %v", worked, err)
		}
	}
	check("thumbnail-queue-state-drain-64-schema-013")
	if env.generator.Calls() != 64 {
		t.Fatalf("complete queue decoded %d sources, want 64", env.generator.Calls())
	}
	ledger.Reset()
	resolveQueue(t, env.service, env, items)
	check("thumbnail-queue-state-ready-64-schema-013")
}
