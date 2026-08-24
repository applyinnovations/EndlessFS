package portable

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestCheckpointFreezeClosesEveryRegisteredDomainAndRegistrationRace(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	existing := state.MustKey(state.NamespaceAccounts, "WVhXWVhXWVhXWVhXWVhXWQ")
	version, err := engine.Create(ctx, existing, []byte("enabled"))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.CloseWrites(ctx, "schema008-freeze"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CompareAndSwap(ctx, existing, version, []byte("disabled")); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("existing domain mutation during freeze error = %v", err)
	}
	newOwner := state.MustKey(state.NamespaceAccounts, "aGhoaGhoaGhoaGhoaGhoaA")
	if _, err := engine.Create(ctx, newOwner, []byte("new")); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("new domain registration during freeze error = %v", err)
	}
}

func TestSchema008CheckpointRejectsCorruptReachableNamespacePage(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	engine := openNamespaceTestEngine(t, backend)
	store := newNamespaceStore(engine)
	live := namespaceTestScope(t, domain.AreaLive)
	view, err := store.loadView(ctx, live.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	edits := make([]namespaceDirectoryEdit, 300)
	for index := range edits {
		name := fmt.Sprintf("directory-%04d", index)
		accumulator, digest, err := directoryContentIdentity(nil)
		if err != nil {
			t.Fatal(err)
		}
		nodeID := fmt.Sprintf("checkpoint-directory-%04d", index)
		entry := storageformat.NamespaceEntry{SchemaVersion: 1, NodeID: nodeID, Entry: storageformat.DirectoryEntry{Name: name, NameDigest: storageformat.NameDigest(name), Kind: domain.EntryDirectory, DirectoryID: nodeID, ContentDigest: digest, ModifiedAt: engine.clock.Now().UTC()}, ContentAccumulator: accumulator}
		entry.Entry.LogicalVersion, err = directoryEntryVersion(entry.Entry)
		if err != nil {
			t.Fatal(err)
		}
		edits[index] = namespaceDirectoryEdit{after: &entry}
	}
	root, err := store.applyDirectoryEdits(ctx, view, view.roots[live.Area()], edits, engine.clock.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.commit(ctx, view, "checkpoint-corrupt-seed", namespaceRequestFingerprint("checkpoint-corrupt-seed", "300"), map[string]storageformat.NamespaceEntry{namespaceFrameKey(live.Area(), namespaceRootPath()): root}, storageformat.NamespaceMutationResult{Operation: &domain.Operation{ID: "checkpoint-corrupt-seed", State: domain.OperationSucceeded, StartedAt: engine.clock.Now(), UpdatedAt: engine.clock.Now()}}); err != nil {
		t.Fatal(err)
	}
	target := storageformat.DomainPageKey(storageformat.DomainNamespace, live.UserID().String(), root.Children.Digest)
	object, err := backend.Get(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, target, []byte(`{"corrupt":true}`), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CreateCheckpoint(ctx, "corrupt-namespace-closure"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("checkpoint corruption error = %v", err)
	}
}

func TestSchema008CheckpointValidatesClosureAndReopensDomains(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	engine := openNamespaceTestEngine(t, backend)
	key := state.MustKey(state.NamespacePreferences, "WVhXWVhXWVhXWVhXWVhXWQ", "theme")
	version, err := engine.Create(ctx, key, []byte("dark"))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := engine.CreateCheckpoint(ctx, "schema008-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.VerifyCheckpoint(ctx, checkpoint.CheckpointID); err != nil {
		t.Fatal(err)
	}
	if err := engine.OpenWrites(ctx, checkpoint.CheckpointID); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CompareAndSwap(ctx, key, version, []byte("light")); err != nil {
		t.Fatalf("mutation after checkpoint reopen: %v", err)
	}
}

func TestStartupFinishesInterruptedCheckpointReopen(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	engine := openNamespaceTestEngine(t, backend)
	key := state.MustKey(state.NamespaceAccounts, "WVhXWVhXWVhXWVhXWVhXWQ")
	version, err := engine.Create(ctx, key, []byte("enabled"))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := engine.CreateCheckpoint(ctx, "interrupted-reopen")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate process loss after the global gate authorizes reopening but
	// before the frozen catalog/domain suffix is released.
	if err := engine.openClosedWriteGate(ctx, checkpoint.CheckpointID); err != nil {
		t.Fatal(err)
	}
	restarted := openInternalTestEngine(t, backend, domain.NewFixedClock(time.Date(2046, 1, 2, 3, 4, 5, 0, time.UTC)), strings.NewReader(strings.Repeat("different-restart-entropy-", 1<<15)))
	if _, err := restarted.CompareAndSwap(ctx, key, version, []byte("disabled")); err != nil {
		t.Fatalf("restart did not finish checkpoint reopen: %v", err)
	}
}

func TestSchema008CheckpointOmitsUnreachablePagesAndBlobsFromInventory(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	engine := openNamespaceTestEngine(t, backend)
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	reference := namespaceReference(owner)
	if _, err := engine.Create(ctx, state.MustKey(state.NamespacePreferences, owner.String(), "theme"), []byte("dark")); err != nil {
		t.Fatal(err)
	}

	stalePage := storageformat.DomainPage{
		SchemaVersion: 1,
		DomainID:      reference.ID,
		Kind:          reference.Kind,
		Level:         0,
		Entries:       []storageformat.DomainEntry{{Key: "stale", Value: []byte("garbage"), LogicalVersion: "stale-version"}},
	}
	pageBody, err := storageformat.EncodeCanonical(stalePage)
	if err != nil {
		t.Fatal(err)
	}
	pageKey := storageformat.DomainPageKey(reference.Kind, reference.ID, storageformat.Digest(pageBody))
	if _, err := backend.Put(ctx, pageKey, pageBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	blobKey := storageformat.BlobKey(owner.String(), "unreachable-blob")
	if _, err := backend.Put(ctx, blobKey, []byte("unreferenced bytes"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}

	checkpoint, err := engine.CreateCheckpoint(ctx, "schema008-exact-reachability")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.VisitCheckpointObjects(ctx, checkpoint.CheckpointID, func(object storageformat.CheckpointObject) error {
		if object.Key == pageKey.String() || object.Key == blobKey.String() {
			t.Fatalf("unreachable object entered checkpoint inventory: %s", object.Key)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
