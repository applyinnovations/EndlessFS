package portable

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type checkpointGarbageStepFailure struct {
	step string
	done bool
}

func (failure *checkpointGarbageStepFailure) Step(_ context.Context, step string) error {
	if step == failure.step && !failure.done {
		failure.done = true
		return domain.NewError(domain.ErrorUnavailable, "injected checkpoint garbage-collection interruption")
	}
	return nil
}

func openCheckpointGarbageTestEngine(t *testing.T, stateBackend objectstore.Backend, fileBackend objectstore.FileControlBackend, scheduler Scheduler) *Engine {
	t.Helper()
	engine, err := Open(context.Background(), Options{
		Backend: stateBackend, FileBackend: fileBackend,
		Clock:    domain.NewFixedClock(time.Date(2058, 1, 2, 3, 4, 5, 0, time.UTC)),
		IDs:      domain.NewIDGenerator(strings.NewReader(strings.Repeat("checkpoint-garbage-entropy-", 1<<15))),
		Writer:   WriterConfiguration{WriterSetID: "checkpoint-garbage", ConfigurationDigest: "checkpoint-garbage-v1", KeyringIdentifiers: []string{"budget-key"}},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x57}, 32), Scheduler: scheduler,
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

type checkpointGarbageFixture struct {
	engine          *Engine
	stateBase       *objectmemory.Backend
	fileBase        *objectmemory.Backend
	unreachablePage objectstore.Key
	unreachableBlob objectstore.Key
	projection      objectstore.Key
	lease           objectstore.Key
}

func newCheckpointGarbageFixture(t *testing.T, scheduler Scheduler) checkpointGarbageFixture {
	t.Helper()
	ctx := context.Background()
	stateBase, fileBase := objectmemory.New(), objectmemory.New()
	engine := openCheckpointGarbageTestEngine(t, stateBase, fileBase, scheduler)
	owner, err := domain.ParseUserID("WVhXWVhXWVhXWVhXWVhXWQ")
	if err != nil {
		t.Fatal(err)
	}
	live, _ := domain.NewScope(owner, domain.AreaLive)
	store := newNamespaceStore(engine)
	publishNamespaceTestFile(t, store, live, "/retained.bin", 4, "retained")
	retainedBlob := storageformat.BlobKey(owner.String(), "blob-retained")
	if _, err := fileBase.Put(ctx, retainedBlob, bytes.Repeat([]byte{4}, 4), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	unreachablePage := storageformat.DomainPageKey(storageformat.DomainOwnerControl, "garbage-owner", "garbage-page")
	unreachableBlob := storageformat.BlobKey(owner.String(), "unreachable")
	projection := storageformat.ProjectionPageKey(owner.String(), storageformat.ProjectionDuplicates, "stale-projection")
	lease := storageformat.LeaseKey("memory", "stale-lease")
	for key, body := range map[objectstore.Key][]byte{
		unreachablePage: []byte("unreachable-page"),
		projection:      []byte("rebuildable-projection"),
		lease:           []byte("stale-lease"),
	} {
		if _, err := stateBase.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fileBase.Put(ctx, unreachableBlob, []byte("orphan"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	return checkpointGarbageFixture{engine: engine, stateBase: stateBase, fileBase: fileBase, unreachablePage: unreachablePage, unreachableBlob: unreachableBlob, projection: projection, lease: lease}
}

func TestCheckpointGarbageCollectionReclaimsOnlyObjectsOutsideFrozenClosure(t *testing.T) {
	ctx := context.Background()
	stateLedger, fileLedger := providerbudget.NewLedger(), providerbudget.NewLedger()
	stateBase, fileBase := objectmemory.New(), objectmemory.New()
	stateBackend := budgettest.Wrap(providerbudget.RoleState, stateBase, stateLedger)
	fileBackend := budgettest.Wrap(providerbudget.RoleFile, fileBase, fileLedger)
	engine := openCheckpointGarbageTestEngine(t, stateBackend, fileBackend, nil)
	owner, _ := domain.ParseUserID("WVhXWVhXWVhXWVhXWVhXWQ")
	live, _ := domain.NewScope(owner, domain.AreaLive)
	store := newNamespaceStore(engine)
	publishNamespaceTestFile(t, store, live, "/retained.bin", 4, "retained")
	retainedBlob := storageformat.BlobKey(owner.String(), "blob-retained")
	if _, err := fileBase.Put(ctx, retainedBlob, bytes.Repeat([]byte{4}, 4), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	unreachablePage := storageformat.DomainPageKey(storageformat.DomainOwnerControl, "garbage-owner", "garbage-page")
	unreachableBlob := storageformat.BlobKey(owner.String(), "unreachable")
	projection := storageformat.ProjectionPageKey(owner.String(), storageformat.ProjectionDuplicates, "stale-projection")
	lease := storageformat.LeaseKey("memory", "stale-lease")
	for key, body := range map[objectstore.Key][]byte{unreachablePage: []byte("page"), projection: []byte("projection"), lease: []byte("lease")} {
		if _, err := stateBase.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fileBase.Put(ctx, unreachableBlob, []byte("orphan"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	stateLedger.Reset()
	fileLedger.Reset()
	checkpoint, err := engine.CreateCheckpoint(ctx, "checkpoint-garbage-reclaim")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct {
		backend objectstore.Backend
		key     objectstore.Key
	}{{stateBase, unreachablePage}, {stateBase, projection}, {stateBase, lease}, {fileBase, unreachableBlob}} {
		if _, err := target.backend.Head(ctx, target.key); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("garbage %s still exists: %v", target.key.String(), err)
		}
	}
	if _, err := fileBase.Head(ctx, retainedBlob); err != nil {
		t.Fatalf("reachable blob was removed: %v", err)
	}
	if err := engine.VerifyCheckpoint(ctx, checkpoint.CheckpointID); err != nil {
		t.Fatalf("checkpoint after sweep: %v", err)
	}
	for _, event := range fileLedger.Events() {
		if event.Kind == providerbudget.RequestObjectGet || event.Kind == providerbudget.RequestObjectOpen || event.Kind == providerbudget.RequestDataDownload {
			t.Fatalf("checkpoint garbage collection read file bytes: %+v", event)
		}
	}
}

func TestProviderBudgetCheckpointGarbageCollection128Objects(t *testing.T) {
	ctx := context.Background()
	stateLedger, fileLedger := providerbudget.NewLedger(), providerbudget.NewLedger()
	stateBase, fileBase := objectmemory.New(), objectmemory.New()
	stateBackend := budgettest.Wrap(providerbudget.RoleState, stateBase, stateLedger)
	fileBackend := budgettest.Wrap(providerbudget.RoleFile, fileBase, fileLedger)
	engine := openCheckpointGarbageTestEngine(t, stateBackend, fileBackend, nil)
	for index := range 128 {
		key := storageformat.DomainPageKey(storageformat.DomainOwnerControl, "garbage-owner", "garbage-page-"+strconv.Itoa(index))
		if _, err := stateBase.Put(ctx, key, []byte("unreachable"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}
	stateLedger.Reset()
	fileLedger.Reset()
	if _, err := engine.CreateCheckpoint(ctx, "checkpoint-garbage-budget-128"); err != nil {
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
	events := append(stateLedger.Events(), fileLedger.Events()...)
	garbageEvents := events[:0]
	for _, event := range events {
		if event.Operation == "checkpoint-garbage" {
			garbageEvents = append(garbageEvents, event)
		}
	}
	if report, err := ratchet.CheckExact("maintenance-checkpoint-garbage-128-schema-011", economics, []providerbudget.Role{providerbudget.RoleState, providerbudget.RoleFile}, garbageEvents); err != nil {
		t.Errorf("128-object checkpoint garbage provider budget: %v; observed=%+v", err, report.Totals)
	}
	deletes := 0
	for _, event := range stateLedger.Events() {
		if event.Kind == providerbudget.RequestObjectDelete && !event.Failed {
			deletes++
		}
	}
	if deletes != 128 {
		t.Fatalf("checkpoint garbage deletes = %d, want 128", deletes)
	}
}

func TestCheckpointGarbageCollectionResumesEveryDurableBoundary(t *testing.T) {
	for _, step := range []string{StepCheckpointGarbageAfterSession, StepCheckpointGarbageAfterPage, StepCheckpointGarbageAfterComplete} {
		t.Run(step, func(t *testing.T) {
			ctx := context.Background()
			failure := &checkpointGarbageStepFailure{step: step}
			fixture := newCheckpointGarbageFixture(t, failure)
			if _, err := fixture.engine.CreateCheckpoint(ctx, "checkpoint-garbage-resume"); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("interrupted checkpoint error = %v; want unavailable", err)
			}
			restarted := openCheckpointGarbageTestEngine(t, fixture.stateBase, fixture.fileBase, nil)
			checkpoint, err := restarted.CreateCheckpoint(ctx, "checkpoint-garbage-resume")
			if err != nil {
				t.Fatal(err)
			}
			for _, target := range []struct {
				backend objectstore.Backend
				key     objectstore.Key
			}{{fixture.stateBase, fixture.unreachablePage}, {fixture.stateBase, fixture.projection}, {fixture.stateBase, fixture.lease}, {fixture.fileBase, fixture.unreachableBlob}} {
				if _, err := target.backend.Head(ctx, target.key); !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("garbage %s after restart still exists: %v", target.key.String(), err)
				}
			}
			if err := restarted.OpenWrites(ctx, checkpoint.CheckpointID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCheckpointGarbageCollectionResumesFromPortableMultiPageCursor(t *testing.T) {
	ctx := context.Background()
	stateBase, fileBase := objectmemory.New(), objectmemory.New()
	failure := &checkpointGarbageStepFailure{step: StepCheckpointGarbageAfterPage}
	engine := openCheckpointGarbageTestEngine(t, stateBase, fileBase, failure)
	for index := range 300 {
		key := storageformat.DomainPageKey(storageformat.DomainOwnerControl, "garbage-owner", "multi-page-"+strconv.Itoa(index))
		if _, err := stateBase.Put(ctx, key, []byte("unreachable"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := engine.CreateCheckpoint(ctx, "checkpoint-garbage-multi-page"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("interrupted multi-page checkpoint error = %v; want unavailable", err)
	}
	sessionObject, err := stateBase.Get(ctx, storageformat.GarbageCollectionSessionKey("checkpoint-garbage-multi-page"))
	if err != nil {
		t.Fatal(err)
	}
	var envelope storageformat.Envelope
	var session storageformat.GarbageCollectionSession
	if err := storageformat.DecodeEnvelope(sessionObject.Body, sessionObject.Key, checkpointGarbageCollectionSchema, &envelope, &session); err != nil {
		t.Fatal(err)
	}
	if session.Phase != checkpointGarbageCollectionSweeping || session.SweepIndex != 1 || session.After != "" {
		t.Fatalf("persisted multi-page progress = %+v", session)
	}

	restarted := openCheckpointGarbageTestEngine(t, stateBase, fileBase, nil)
	checkpoint, err := restarted.CreateCheckpoint(ctx, "checkpoint-garbage-multi-page")
	if err != nil {
		t.Fatal(err)
	}
	page, err := stateBase.List(ctx, objectstore.ListRequest{Prefix: storageformat.DomainPrefix(), Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range page.Objects {
		if checkpointDomainGarbageEligible(object.Key) {
			t.Fatalf("unreachable domain object survived resumed sweep: %s", object.Key.String())
		}
	}
	if err := restarted.OpenWrites(ctx, checkpoint.CheckpointID); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointGarbageCollectionConcurrentReplicasConverge(t *testing.T) {
	ctx := context.Background()
	fixture := newCheckpointGarbageFixture(t, nil)
	second := openCheckpointGarbageTestEngine(t, fixture.stateBase, fixture.fileBase, nil)
	start := make(chan struct{})
	errorsByReplica := make(chan error, 2)
	var wait sync.WaitGroup
	for _, engine := range []*Engine{fixture.engine, second} {
		engine := engine
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := engine.CreateCheckpoint(ctx, "checkpoint-garbage-concurrent")
			errorsByReplica <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errorsByReplica)
	for err := range errorsByReplica {
		if err != nil && !errors.Is(err, domain.ErrUnavailable) && !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("concurrent checkpoint error = %v", err)
		}
	}
	checkpoint, err := fixture.engine.CreateCheckpoint(ctx, "checkpoint-garbage-concurrent")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.engine.OpenWrites(ctx, checkpoint.CheckpointID); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointGarbageCollectionRejectsCorruptOrMisbindingSession(t *testing.T) {
	ctx := context.Background()
	failure := &checkpointGarbageStepFailure{step: StepCheckpointGarbageAfterSession}
	fixture := newCheckpointGarbageFixture(t, failure)
	if _, err := fixture.engine.CreateCheckpoint(ctx, "checkpoint-garbage-corrupt"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("interrupted checkpoint error = %v", err)
	}
	key := storageformat.GarbageCollectionSessionKey("checkpoint-garbage-corrupt")
	object, err := fixture.stateBase.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.stateBase.Put(ctx, key, []byte(`{"schema":"checkpoint-garbage-collection-v2","revision":1,"payload":{"schemaVersion":2}}`), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
		t.Fatal(err)
	}
	restarted := openCheckpointGarbageTestEngine(t, fixture.stateBase, fixture.fileBase, nil)
	if _, err := restarted.CreateCheckpoint(ctx, "checkpoint-garbage-corrupt"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt checkpoint garbage session error = %v; want invalid", err)
	}
}

func TestCheckpointGarbageEligibilityRejectsUnknownReservedKeys(t *testing.T) {
	unknown := objectstore.MustKey("endlessfs/v1/domains/unknown/domain/pages/page.json")
	if checkpointDomainGarbageEligible(unknown) {
		t.Fatal("unknown consistency-domain key became deletion-eligible")
	}
	if checkpointBlobGarbageEligible(storageformat.DirectoryRootKey("owner", "live", "root")) {
		t.Fatal("directory metadata became file-blob deletion eligible")
	}
}
