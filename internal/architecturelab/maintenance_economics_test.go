package architecturelab

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/state"
)

func TestCheckpointEconomicsBeforeAndAfter(t *testing.T) {
	ctx := context.Background()
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	for _, domains := range []int{1, 128} {
		t.Run(fmt.Sprintf("current-records-%d", domains), func(t *testing.T) {
			current := openCurrentProviderHarness(t, fmt.Sprintf("checkpoint-baseline-%d", domains))
			for index := 0; index < domains; index++ {
				key := state.MustKey(state.NamespacePreferences, fmt.Sprintf("benchmark-%03d", index))
				if _, err := current.engine.Create(ctx, key, []byte(`{"value":true}`)); err != nil {
					t.Fatal(err)
				}
			}
			current.ledger.Reset()
			if _, err := current.engine.CreateCheckpoint(ctx, "benchmark-checkpoint"); err != nil {
				t.Fatal(err)
			}
			logCurrentEconomics(t, fmt.Sprintf("before/maintenance/checkpoint-%d-records", domains), model, current.ledger)
		})
		t.Run(fmt.Sprintf("prototype-domains-%d", domains), func(t *testing.T) {
			ledger := providerbudget.NewLedger()
			backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
			catalog, err := openDomainCatalog(ctx, backend, fmt.Sprintf("checkpoint-%d", domains))
			if err != nil {
				t.Fatal(err)
			}
			for index := 0; index < domains; index++ {
				if _, err := catalog.Register(ctx, fmt.Sprintf("domain-%03d", index)); err != nil {
					t.Fatal(err)
				}
			}
			ledger.Reset()
			if _, err := catalog.CreateCheckpoint(ctx, "checkpoint"); err != nil {
				t.Fatal(err)
			}
			logCurrentEconomics(t, fmt.Sprintf("after/maintenance/checkpoint-%d-domains", domains), model, ledger)
		})
	}
}

func TestPrototypeMaintenanceEconomics(t *testing.T) {
	ctx := context.Background()
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	currentLedger := providerbudget.NewLedger()
	currentBackend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), currentLedger)
	clock := domain.NewFixedClock(time.Date(2049, 5, 6, 7, 8, 9, 0, time.UTC))
	options := portable.Options{
		Backend: currentBackend, FileBackend: objectmemory.New(), Clock: clock,
		IDs:      domain.NewIDGenerator(&currentBatchEntropy{}),
		Writer:   portable.WriterConfiguration{WriterSetID: "startup-baseline", ConfigurationDigest: "startup-baseline-v1", KeyringIdentifiers: []string{"startup-key"}},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x21}, 32),
	}
	if _, err := portable.Open(ctx, options); err != nil {
		t.Fatal(err)
	}
	currentLedger.Reset()
	if _, err := portable.Open(ctx, options); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "before/maintenance/warm-startup", model, currentLedger)

	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
	candidate, err := openHybrid(ctx, backend, Options{DomainID: "maintenance"})
	if err != nil {
		t.Fatal(err)
	}
	engine := candidate.(*hybridEngine)
	for index := 0; index < 32; index++ {
		name := fmt.Sprintf("file-%02d", index)
		if _, err := engine.Mutate(ctx, Mutation{ID: "create-" + name, Kind: MutationCreateFile, ToArea: AreaLive, Destination: "/" + name, NodeID: name, Size: 1, BlobIdentity: "blob-" + name}); err != nil {
			t.Fatal(err)
		}
	}
	ledger.Reset()
	if err := engine.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "after/maintenance/compact-32-deltas", model, ledger)

	ledger.Reset()
	if _, _, err := engine.loadHead(ctx, "startup"); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "after/maintenance/warm-startup-domain", model, ledger)

	viewLedger := providerbudget.NewLedger()
	viewBackend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), viewLedger)
	if _, err := openDerivedView(ctx, viewBackend, "rebuild", 1, []byte(`{"rows":[1,2,3]}`)); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "after/maintenance/derived-view-rebuild-one-page", model, viewLedger)
}

func TestRecoveryEconomicsBeforeAndAfter(t *testing.T) {
	ctx := context.Background()
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	current := openCurrentProviderHarness(t, "recovery-baseline")
	current.upload(t, "/recovery-file.txt", "payload")
	if _, err := current.service.Trash(ctx, current.user, []domain.UserPath{domain.MustParseUserPath("/recovery-file.txt")}, "replay-trash-key-0001"); err != nil {
		t.Fatal(err)
	}
	current.ledger.Reset()
	if _, err := current.service.Trash(ctx, current.user, []domain.UserPath{domain.MustParseUserPath("/recovery-file.txt")}, "replay-trash-key-0001"); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "before/maintenance/idempotent-replay", model, current.ledger)

	ledger := providerbudget.NewLedger()
	instrumented := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
	faults := &commitFaultBackend{Backend: instrumented}
	candidate, err := openHybrid(ctx, faults, Options{DomainID: "recovery-economics"})
	if err != nil {
		t.Fatal(err)
	}
	engine := candidate.(*hybridEngine)
	mutation := Mutation{ID: "mkdir-lost", Kind: MutationCreateDirectory, ToArea: AreaLive, Destination: "/lost", NodeID: "lost"}
	faults.armLostSuccess()
	if _, err := engine.Mutate(ctx, mutation); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("lost-success mutation error=%v", err)
	}
	ledger.Reset()
	outcome, err := engine.Mutate(ctx, mutation)
	if err != nil || !outcome.Replayed || !outcome.Committed {
		t.Fatalf("lost-success recovery=%+v, %v", outcome, err)
	}
	logCurrentEconomics(t, "after/maintenance/lost-success-recovery", model, ledger)
}

func TestPrototypeGarbageCollectionEconomics(t *testing.T) {
	ctx := context.Background()
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
	catalog, err := openDomainCatalog(ctx, backend, "garbage-economics")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Register(ctx, "owner"); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 128; index++ {
		body := []byte(fmt.Sprintf(`{"schemaVersion":1,"level":0,"values":[{"key":"garbage-%03d","value":{"schemaVersion":1}}]}`, index))
		key := candidateKey("embedded", "owner", "pages/"+digest(body)+".json")
		if _, err := backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}
	checkpoint, err := catalog.CreateCheckpoint(ctx, "retained")
	if err != nil {
		t.Fatal(err)
	}
	ledger.Reset()
	result, err := catalog.CollectGarbage(ctx, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted < 128 {
		t.Fatalf("garbage deleted=%d, want at least 128", result.Deleted)
	}
	logCurrentEconomics(t, "after/maintenance/garbage-collection-128-unreachable-pages", model, ledger)
}
