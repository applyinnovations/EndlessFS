package portable_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
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

func TestProviderBudgetMigrationPendingCleanup(t *testing.T) {
	for _, family := range storageSchemaFixturesFor("endlessfs-portable-v1/schema-010") {
		if !strings.Contains(family.profile, "cleanup") {
			continue
		}
		t.Run(family.profile, func(t *testing.T) {
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
			clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
			options := schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 0xd4, nil)
			options.Writer = currentWriterForSchemaFixture(t, fixture)
			if _, err := portable.Open(context.Background(), options); err != nil {
				t.Fatal(err)
			}
			model, err := gcs.RegionalStandardFlatEconomics()
			if err != nil {
				t.Fatal(err)
			}
			name := "maintenance-migration-application-complete-pending-cleanup-schema-011"
			if family.producer == "v0.7.1-interrupted" {
				name = "maintenance-migration-application-complete-interrupted-cleanup-schema-011"
			}
			events := append(stateLedger.Events(), fileLedger.Events()...)
			budget, err := providerbudget.Calibrate(name, model, []providerbudget.Role{providerbudget.RoleState, providerbudget.RoleFile}, events)
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(budget)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("measured budget: %s", body)
			totals, err := model.Estimate(events)
			if err != nil {
				t.Fatal(err)
			}
			body, err = json.Marshal(totals)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("complete economics: %s; retained state objects: %d; predecessor state objects: %d", body, len(stateBase.Export()), len(fixture.StateObjects))
			checkMaintenanceProviderBudget(t, name, []providerbudget.Role{providerbudget.RoleState, providerbudget.RoleFile}, stateLedger, fileLedger)
		})
	}
}

func TestMigrationPendingCleanupReplicasConverge(t *testing.T) {
	for _, family := range storageSchemaFixturesFor("endlessfs-portable-v1/schema-010") {
		if !strings.Contains(family.profile, "cleanup") {
			continue
		}
		for replicas := 2; replicas <= 8; replicas++ {
			t.Run(fmt.Sprintf("%s/%d-replicas", family.profile, replicas), func(t *testing.T) {
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
				clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
				barrier := newAggregateBarrier(replicas)
				engines := make([]*portable.Engine, replicas)
				failures := make([]error, replicas)
				var wait sync.WaitGroup
				for index := range replicas {
					wait.Add(1)
					go func() {
						defer wait.Done()
						scheduler := &aggregateOneShotScheduler{step: portable.MigrationStepName("schema-010-to-011", portable.StepMigrationAfterDetection), barrier: barrier, enabled: true}
						options := schemaSplitMigrationOptions(stateBackend, fileBackend, clock, byte(100+index), scheduler)
						options.Writer = currentWriterForSchemaFixture(t, fixture)
						engines[index], failures[index] = portable.Open(context.Background(), options)
					}()
				}
				wait.Wait()
				for index, err := range failures {
					if err != nil {
						t.Fatalf("replica %d could not resume pending cleanup: %v", index, err)
					}
				}
				for key, expected := range fixture.FileObjects {
					if !bytes.Equal(fileBase.Export()[key], expected) {
						t.Fatalf("migration changed predecessor file object %q", key)
					}
				}
				for _, event := range fileLedger.Events() {
					switch event.Kind {
					case providerbudget.RequestObjectGet, providerbudget.RequestObjectOpen, providerbudget.RequestObjectPut, providerbudget.RequestObjectCopy, providerbudget.RequestObjectDelete:
						t.Fatalf("migration read or changed file bytes: %+v", event)
					}
				}
				assertCompleteMigrationSemanticOracle(t, engines[0], fixture, clock, 0xe4)
				owner, _ := domain.ParseUserID(fixture.UserID)
				live, _ := domain.NewScope(owner, domain.AreaLive)
				root, err := engines[0].Files().Stat(context.Background(), live, domain.MustParseUserPath("/"))
				if err != nil || root.Size != family.wantSize || root.FileCount != family.wantFiles {
					t.Fatalf("migrated namespace = %+v, %v", root, err)
				}
				if _, err := engines[0].Files().CreateDirectory(context.Background(), live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/after-cleanup-migration")}); err != nil {
					t.Fatalf("new mutation after concurrent migration: %v", err)
				}
			})
		}
	}
}

func TestCheckpointPendingCleanupRawCopyPreservesAuthority(t *testing.T) {
	ctx := context.Background()
	for _, family := range storageSchemaFixturesFor("endlessfs-portable-v1/schema-010") {
		if !strings.Contains(family.profile, "cleanup") {
			continue
		}
		t.Run(family.profile, func(t *testing.T) {
			fixture := loadStorageSchemaFixture(t, family)
			stateBase, fileBase := objectmemory.New(), objectmemory.New()
			if err := stateBase.Import(fixture.StateObjects); err != nil {
				t.Fatal(err)
			}
			if err := fileBase.Import(fixture.FileObjects); err != nil {
				t.Fatal(err)
			}
			clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
			writer := currentWriterForSchemaFixture(t, fixture)
			options := schemaSplitMigrationOptions(stateBase, fileBase, clock, 0xd5, nil)
			options.Writer = writer
			source, err := portable.Open(ctx, options)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err := source.CreateCheckpoint(ctx, "pending-cleanup-portability")
			if err != nil {
				t.Fatal(err)
			}
			stateObjects, fileObjects := stateBase.Export(), fileBase.Export()
			stateCopy, fileCopy := make(map[string][]byte), make(map[string][]byte)
			if err := source.VisitCheckpointObjects(ctx, checkpoint.CheckpointID, func(object storageformat.CheckpointObject) error {
				if strings.Contains(object.Key, "/blobs/") {
					fileCopy[object.Key] = fileObjects[object.Key]
				} else {
					stateCopy[object.Key] = stateObjects[object.Key]
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			checkpointKey := storageformat.CheckpointKey(checkpoint.CheckpointID).String()
			stateCopy[checkpointKey] = stateObjects[checkpointKey]
			for index := uint64(0); index < checkpoint.InventoryPageCount; index++ {
				key := storageformat.CheckpointInventoryPageKey(checkpoint.CheckpointID, index).String()
				stateCopy[key] = stateObjects[key]
			}
			for key := range stateCopy {
				if strings.HasPrefix(key, storageformat.LeasePrefix()) {
					t.Fatalf("transient lease entered the checkpoint: %q", key)
				}
			}
			destinationState, destinationFile := objectmemory.New(), objectmemory.New()
			if err := destinationState.Import(stateCopy); err != nil {
				t.Fatal(err)
			}
			if err := destinationFile.Import(fileCopy); err != nil {
				t.Fatal(err)
			}
			options = schemaSplitMigrationOptions(destinationState, destinationFile, clock, 0xd6, nil)
			options.Writer = writer
			destination, err := portable.Open(ctx, options)
			if err != nil {
				t.Fatal(err)
			}
			if err := destination.OpenWrites(ctx, checkpoint.CheckpointID); err != nil {
				t.Fatal(err)
			}
			assertCompleteMigrationSemanticOracle(t, destination, fixture, clock, 0xe5)
			owner, _ := domain.ParseUserID(fixture.UserID)
			live, _ := domain.NewScope(owner, domain.AreaLive)
			if _, err := destination.Files().CreateDirectory(ctx, live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/after-raw-copy")}); err != nil {
				t.Fatalf("new mutation after pending-cleanup raw copy: %v", err)
			}
		})
	}
}
