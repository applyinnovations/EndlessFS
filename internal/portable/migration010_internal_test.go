package portable

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestMigrationSchema009RecoversIndexedApplicationStateDroppedBySchema008(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2046, 8, 27, 1, 2, 3, 0, time.UTC))
	options := internalMigration010Options(backend, clock, 0x41)
	engine, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	configureMigrationSourceSchema(t, backend, engine, storageSchema009)

	preference := state.MustKey(state.NamespacePreferences, "dXNlci0wMDAx")
	want := []byte(`{"themeID":"endlessfs-dark"}`)
	putSchema007IndexedState(t, engine, preference, "legacy-preference-v1", want)

	reopened, err := Open(ctx, internalMigration010Options(backend, clock, 0x42))
	if err != nil {
		t.Fatalf("reopen production-shaped schema-009 state: %v", err)
	}
	got, err := reopened.Get(ctx, preference)
	if err != nil {
		t.Fatalf("recovered indexed preference: %v", err)
	}
	if !bytes.Equal(got.Data, want) || got.Version != state.Version("legacy-preference-v1") {
		t.Fatalf("recovered indexed preference = %+v; want version and bytes preserved", got)
	}
}

func TestMigrationSchema010FailsClosedForConflictingCurrentState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2046, 8, 27, 2, 3, 4, 0, time.UTC))
	engine, err := Open(ctx, internalMigration010Options(backend, clock, 0x51))
	if err != nil {
		t.Fatal(err)
	}
	preference := state.MustKey(state.NamespacePreferences, "owner-a")
	if _, err := engine.Create(ctx, preference, []byte(`{"themeID":"endlessfs-light"}`)); err != nil {
		t.Fatal(err)
	}
	configureMigrationSourceSchema(t, backend, engine, storageSchema009)
	putSchema007IndexedState(t, engine, preference, "legacy-preference-v1", []byte(`{"themeID":"endlessfs-dark"}`))

	if _, err := Open(ctx, internalMigration010Options(backend, clock, 0x52)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("conflicting indexed-state recovery error = %v; want conflict", err)
	}
	assertMigration010SourceSchemaRemainsActive(t, backend)
}

func TestMigrationSchema010FailsClosedWhenIndexedVersionIsMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2046, 8, 27, 3, 4, 5, 0, time.UTC))
	engine, err := Open(ctx, internalMigration010Options(backend, clock, 0x61))
	if err != nil {
		t.Fatal(err)
	}
	configureMigrationSourceSchema(t, backend, engine, storageSchema009)
	key := state.MustKey(state.NamespaceAccounts, "owner-a")
	putSchema007IndexedState(t, engine, key, "legacy-account-v1", []byte(`{"userID":"owner-a","status":"enabled"}`))
	versionKey := storageformat.StateVersionKey(stateNamespace(key), key.String(), "legacy-account-v1")
	object, err := backend.Get(ctx, versionKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Delete(ctx, versionKey, objectstore.DeleteCondition{Version: object.Version}); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(ctx, internalMigration010Options(backend, clock, 0x62)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing indexed version recovery error = %v; want not found", err)
	}
	assertMigration010SourceSchemaRemainsActive(t, backend)
}

func TestMigrationSchema010RejectsCorruptDurableConservationReceiptOnRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2046, 8, 27, 4, 5, 6, 0, time.UTC))
	engine, err := Open(ctx, internalMigration010Options(backend, clock, 0x71))
	if err != nil {
		t.Fatal(err)
	}
	configureMigrationSourceSchema(t, backend, engine, storageSchema009)
	key := state.MustKey(state.NamespacePreferences, "owner-a")
	putSchema007IndexedState(t, engine, key, "legacy-preference-v1", []byte(`{"themeID":"endlessfs-dark"}`))

	interrupted := internalMigration010Options(backend, clock, 0x72)
	interrupted.Scheduler = SchedulerFunc(func(_ context.Context, step string) error {
		if step == MigrationStepName(string(storageMigration009To010), StepMigrationAfterDirectoryPrerequisites) {
			return domain.NewError(domain.ErrorUnavailable, "injected schema-010 interruption")
		}
		return nil
	})
	if _, err := Open(ctx, interrupted); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("interrupted recovery error = %v; want unavailable", err)
	}
	var receiptKey objectstore.Key
	for keyValue := range backend.Export() {
		if strings.HasPrefix(keyValue, storageformat.Schema010MigrationReceiptPrefix()) {
			receiptKey = objectstore.MustKey(keyValue)
			break
		}
	}
	if !receiptKey.Valid() {
		t.Fatal("interrupted migration wrote no conservation receipt")
	}
	receipt, err := backend.Get(ctx, receiptKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, receiptKey, []byte(`{}`), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: receipt.Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, internalMigration010Options(backend, clock, 0x73)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt receipt restart error = %v; want invalid", err)
	}
	assertMigration010SourceSchemaRemainsActive(t, backend)
}

func TestMigrationSchema010ConcurrentReplicasConvergeWithIndexedState(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2046, 8, 27, 5, 6, 7, 0, time.UTC))
	engine, err := Open(ctx, internalMigration010Options(backend, clock, 0x81))
	if err != nil {
		t.Fatal(err)
	}
	configureMigrationSourceSchema(t, backend, engine, storageSchema009)
	key := state.MustKey(state.NamespacePreferences, "owner-a")
	want := []byte(`{"themeID":"endlessfs-dark"}`)
	putSchema007IndexedState(t, engine, key, "legacy-preference-v1", want)

	results := make(chan error, 8)
	for index := range 8 {
		go func() {
			_, err := Open(ctx, internalMigration010Options(backend, clock, byte(0x82+index)))
			results <- err
		}()
	}
	for range 8 {
		if err := <-results; err != nil {
			chain := []string{}
			for current := err; current != nil; current = errors.Unwrap(current) {
				chain = append(chain, current.Error())
			}
			t.Fatalf("concurrent schema-010 migration: %s", strings.Join(chain, ": "))
		}
	}
	reopened, err := Open(ctx, internalMigration010Options(backend, clock, 0x91))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get(ctx, key)
	if err != nil || !bytes.Equal(got.Data, want) || got.Version != "legacy-preference-v1" {
		t.Fatalf("converged indexed state = %+v, %v", got, err)
	}
}

func internalMigration010Options(backend objectstore.Backend, clock domain.Clock, seed byte) Options {
	return Options{
		Backend: backend,
		Clock:   clock,
		IDs:     domain.NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{seed}, 1<<20))),
		Writer: WriterConfiguration{
			WriterSetID:         "d3JpdGVyLXNldC0wMDAx",
			ConfigurationDigest: "config-v1",
			KeyringIdentifiers:  []string{"session-v1"},
		},
		LeaseTTL:  time.Minute,
		CursorKey: bytes.Repeat([]byte{0x63}, 32),
	}
}

func putSchema007IndexedState(t *testing.T, engine *Engine, key state.Key, version string, data []byte) {
	t.Helper()
	ctx := context.Background()
	value, err := stateVersionObject(key, state.Version(version), data)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ensureMutationPrerequisites(ctx, []storageformat.MutationObject{value}); err != nil {
		t.Fatal(err)
	}
	prepared, err := engine.prepareStateIndexMutation(ctx, key, version, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ensureMutationPrerequisites(ctx, prepared.prerequisites); err != nil {
		t.Fatal(err)
	}
	condition := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
	if prepared.snapshot.exists {
		condition = objectstore.PutCondition{Mode: objectstore.PutMatch, Version: prepared.snapshot.object.Version}
	}
	if _, err := engine.backend.Put(ctx, storageformat.StateIndexRootKey(stateNamespace(key)), prepared.rootBody, condition); err != nil {
		t.Fatal(err)
	}
	indexed, err := engine.stateIndexEntry(ctx, key)
	if err != nil || indexed.LogicalVersion != version {
		t.Fatalf("seed schema-007 indexed state = %+v, %v", indexed, err)
	}
}

func assertMigration010SourceSchemaRemainsActive(t *testing.T, backend objectstore.Backend) {
	t.Helper()
	object, err := backend.Get(context.Background(), storageformat.SuperblockKey())
	if err != nil {
		t.Fatal(err)
	}
	var superblock storageformat.Superblock
	if err := decodeCanonicalSuperblock(object.Body, &superblock); err != nil {
		t.Fatal(err)
	}
	detected, found := detectStorageSchema(superblock.RequiredFeatures, nil)
	if !found || detected.id != storageSchema009 {
		t.Fatalf("failed migration activated schema = %+v, %t; want schema-009", detected, found)
	}
}
