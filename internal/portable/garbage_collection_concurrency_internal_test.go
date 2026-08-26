package portable

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestGarbageCollectionTerminalMarksProtectLaggingSweeper(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2060, 1, 2, 3, 4, 5, 0, time.UTC))
	engine := &Engine{backend: backend, fileBackend: backend, clock: clock}
	const checkpointID = "lagging-sweeper"
	target := storageformat.BlobKey("owner", "live-blob")
	if _, err := backend.Put(ctx, target, []byte("authority"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	session, err := engine.readOrCreateGarbageCollectionSession(ctx, checkpointID, 9, "gate-version")
	if err != nil {
		t.Fatal(err)
	}
	session.value.Phase = garbageCollectionSweeping
	session.value.SweepIndex = 3
	session.value.UpdatedAt = time.Date(2060, 1, 2, 3, 4, 5, 0, time.UTC)
	session, err = engine.updateGarbageCollectionSession(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ensureGarbageCollectionMark(ctx, session.value, garbageCollectionFileRole, target); err != nil {
		t.Fatal(err)
	}
	lagging := session
	winner := session
	winner.value.Phase = garbageCollectionCleanup
	winner.value.SweepIndex = 4
	winner.value.After = ""
	if _, err := engine.updateGarbageCollectionSession(ctx, winner); err != nil {
		t.Fatal(err)
	}
	if err := engine.runGarbageCollectionAttempt(ctx, checkpointID, 9, "gate-version"); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Head(ctx, storageformat.GarbageCollectionSessionKey(checkpointID)); err != nil {
		t.Fatalf("terminal session was removed: %v", err)
	}
	if _, err := backend.Head(ctx, storageformat.GarbageCollectionMarkKey(checkpointID, garbageCollectionFileRole, target.String())); err != nil {
		t.Fatalf("terminal live mark was removed: %v", err)
	}
	if _, err := engine.sweepGarbageCollection(ctx, lagging); !errors.Is(err, errGarbageCollectionContended) {
		t.Fatalf("lagging sweep error = %v; want reconciled contention", err)
	}
	if object, err := backend.Get(ctx, target); err != nil || string(object.Body) != "authority" {
		t.Fatalf("lagging sweeper deleted live authority: %q, %v", object.Body, err)
	}
}
