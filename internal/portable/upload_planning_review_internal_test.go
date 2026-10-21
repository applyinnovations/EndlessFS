package portable

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func reviewProjectionLeaf(t *testing.T, session *consistencyDomainTreeSession, key string) domainPageRef {
	t.Helper()
	reference, err := session.writePage(context.Background(), storageformat.DomainPage{
		SchemaVersion: 1, DomainID: session.reference.ID, Kind: session.reference.Kind,
		Entries: []storageformat.DomainEntry{{Key: key, Value: []byte("synthetic"), LogicalVersion: "synthetic-version"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return reference
}

func reviewSeedDeepProjectionNamespace(t *testing.T, engine *Engine) (domain.Scope, domain.UploadSizePlanRequest) {
	t.Helper()
	ctx := context.Background()
	live := namespaceTestScope(t, domain.AreaLive)
	store := newNamespaceStore(engine)
	directory := namespaceRootPath()
	for level := range 15 {
		var err error
		directory, err = directory.Join(fmt.Sprintf("level-%02d-", level) + strings.Repeat("x", 231))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Files().CreateDirectory(ctx, live, domain.CreateDirectoryRequest{Path: directory}); err != nil {
			t.Fatal(err)
		}
	}
	view, err := store.loadView(ctx, live.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	trail, err := store.resolveTrail(ctx, view, domain.AreaLive, directory)
	if err != nil {
		t.Fatal(err)
	}
	parent := trail[len(trail)-1]
	const count = 2_048
	edits := make([]namespaceDirectoryEdit, count)
	request := domain.UploadSizePlanRequest{Items: make([]domain.UploadSizePlanItem, count)}
	for index := range edits {
		name := fmt.Sprintf("file-%04d-", index) + strings.Repeat("y", 230)
		path, err := directory.Join(name)
		if err != nil {
			t.Fatal(err)
		}
		entry := storageformat.NamespaceEntry{SchemaVersion: 1, NodeID: fmt.Sprintf("deep-node-%04d", index), Entry: storageformat.DirectoryEntry{
			Name: name, NameDigest: storageformat.NameDigest(name), Kind: domain.EntryFile, BlobID: fmt.Sprintf("deep-blob-%04d", index),
			Size: int64(index + 1), MediaType: "application/octet-stream", MD5: testProviderMD5, CRC32C: testProviderCRC32C, ModifiedAt: engine.clock.Now().UTC(),
		}}
		entry.Entry.LogicalVersion, err = directoryEntryVersion(entry.Entry)
		if err != nil {
			t.Fatal(err)
		}
		edits[index] = namespaceDirectoryEdit{after: &entry}
		request.Items[index] = domain.UploadSizePlanItem{ID: fmt.Sprintf("query-%04d", index), Path: path, Size: int64(index + 1)}
	}
	updated, err := store.applyDirectoryEdits(ctx, view, parent.entry, edits, engine.clock.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	changes := map[string]storageformat.NamespaceEntry{parent.key: updated}
	if err := store.propagate(ctx, view, mergeNamespaceFrames(trail), changes, engine.clock.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	operation := domain.Operation{ID: "deep-review-seed-operation", State: domain.OperationSucceeded, StartedAt: engine.clock.Now(), UpdatedAt: engine.clock.Now()}
	if _, err := store.commit(ctx, view, "deep-review-seed", namespaceRequestFingerprint("deep-review-seed"), changes, storageformat.NamespaceMutationResult{Operation: &operation}); err != nil {
		t.Fatal(err)
	}
	return live, request
}

func TestUploadPlanningReviewDeepPathFinalClosureExceedsPackBound(t *testing.T) {
	parallelCorrectiveHeavyFixture(t)
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	engine := openNamespaceTestEngine(t, budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger))
	live, request := reviewSeedDeepProjectionNamespace(t, engine)
	count := len(request.Items)
	store := newNamespaceStore(engine)
	view, err := store.loadView(ctx, live.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	projectionID := uploadPlanningProjectionID008(live.UserID())
	session := duplicateProjectionSession008(engine, live.UserID(), projectionID)
	root, err := engine.Files().buildUploadPlanningProjection008(ctx, view, session)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.prunePendingProjectionPages(root); err != nil {
		t.Fatal(err)
	}
	pages := make([]storageformat.DomainPackedPage, 0, len(session.pendingPack))
	for digest, page := range session.pendingPack {
		pages = append(pages, storageformat.DomainPackedPage{Digest: digest, Page: page})
	}
	_, err = storageformat.EncodeDomainPagePack(storageformat.DomainPagePack{SchemaVersion: 1, DomainID: session.reference.ID, Kind: session.reference.Kind, PackID: session.packID, Pages: pages})
	if !errors.Is(err, storageformat.ErrDomainPagePackTooLarge) {
		t.Fatalf("fixture did not exceed final closure bound: pages=%d err=%v", len(pages), err)
	}
	ledger.Reset()
	plan, err := engine.Files().PlanUploadSizes(ctx, live.UserID(), request)
	if err != nil || len(plan.Items) != count {
		t.Fatalf("bounded pack fallback did not plan legal deep paths: items=%d err=%v", len(plan.Items), err)
	}
	coldEvents := ledger.Events()
	ledger.Reset()
	plan, err = engine.Files().PlanUploadSizes(ctx, live.UserID(), request)
	if err != nil || len(plan.Items) != count {
		t.Fatalf("warm fallback projection could not reopen: items=%d err=%v", len(plan.Items), err)
	}
	for _, item := range plan.Items {
		if !item.TargetExists || !item.FingerprintRequired || item.TargetKind != domain.EntryFile {
			t.Fatalf("fallback lost an existing target: %+v", item)
		}
	}
	economics, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	for phase, events := range map[string][]providerbudget.Event{"cold": coldEvents, "warm": ledger.Events()} {
		for _, event := range events {
			if event.Role != providerbudget.RoleState || event.Kind != providerbudget.RequestObjectGet && event.Kind != providerbudget.RequestObjectPut {
				t.Fatalf("%s fallback touched file data or nonimmutable provider work: %+v", phase, event)
			}
		}
		metrics, err := economics.Estimate(events)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("deep-path fallback %s: files=%d path-bytes=%d final-pages=%d metrics=%+v", phase, count, len(request.Items[0].Path.String()), len(pages), metrics)
	}
	if _, err := engine.Files().CreateDirectory(ctx, live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/post-fallback-empty")}); err != nil {
		t.Fatal(err)
	}
	if result, err := engine.Files().PlanUploadSizes(ctx, live.UserID(), request); err != nil || len(result.Items) != count {
		t.Fatalf("directory-only source mutation broke the unchanged unpacked projection: items=%d err=%v", len(result.Items), err)
	}
	// Explore the credible alternative on the identical authenticated final
	// tree. These are physical-stage metrics, not a complete lifecycle budget.
	ledger.Reset()
	rollingRoot, rollingID := reviewRepackProjection(t, ctx, session, root, engine, live.UserID())
	rollingWrites := ledger.Events()
	ledger.Reset()
	reader := duplicateProjectionSession008(engine, live.UserID(), rollingID)
	for _, item := range request.Items {
		values, err := uploadPlanningCollect008(ctx, reader, rollingRoot, duplicateProjectionSizePrefix008(item.Size))
		if err != nil || len(values) != 1 {
			t.Fatalf("bounded rolling candidate lost size decision: id=%s values=%d err=%v", item.ID, len(values), err)
		}
		var occurrence storageformat.DuplicateProjectionOccurrence
		if err := decodeCanonicalValue(values[0].Value, &occurrence); err != nil || occurrence.Occurrence.Path != item.Path.String() {
			t.Fatalf("bounded rolling candidate changed occurrence: id=%s err=%v", item.ID, err)
		}
	}
	for phase, events := range map[string][]providerbudget.Event{"output-only": rollingWrites, "warm-index-only": ledger.Events()} {
		metrics, err := economics.Estimate(events)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("deep-path rolling prototype %s: metrics=%+v", phase, metrics)
	}
}

func reviewProjectionBranch(t *testing.T, session *consistencyDomainTreeSession, children ...domainPageRef) domainPageRef {
	t.Helper()
	page := storageformat.DomainPage{SchemaVersion: 1, DomainID: session.reference.ID, Kind: session.reference.Kind, Level: 1}
	for _, child := range children {
		page.Children = append(page.Children, storageformat.DomainPageChild{
			FirstKey: child.firstKey, LastKey: child.lastKey, Digest: child.root.Digest,
			PackID: child.root.PackID, LeafKeyFilter: child.leafKeyFilter, Level: child.root.Level,
			EntryCount: child.root.EntryCount, ByteCount: child.root.ByteCount,
		})
	}
	reference, err := session.writePage(context.Background(), page)
	if err != nil {
		t.Fatal(err)
	}
	return reference
}

func TestUploadPlanningProjectionPruningVerifiesReachableClosure(t *testing.T) {
	for _, mode := range []string{"valid", "missing-staged-child", "corrupt-staged-child", "mismatched-child-key-bounds", "duplicate-staged-child-mismatched-bounds"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			engine := openNamespaceTestEngine(t, objectmemory.New())
			owner := namespaceTestScope(t, domain.AreaLive).UserID()
			projectionID := uploadPlanningProjectionID008(owner)
			session := duplicateProjectionSession008(engine, owner, projectionID)
			child := reviewProjectionLeaf(t, session, "a")
			orphan := reviewProjectionLeaf(t, session, "orphan")
			if mode == "mismatched-child-key-bounds" {
				child.firstKey, child.lastKey = "b", "b"
			}
			root := reviewProjectionBranch(t, session, child)
			if mode == "duplicate-staged-child-mismatched-bounds" {
				duplicate := child
				duplicate.firstKey, duplicate.lastKey = "b", "b"
				root = reviewProjectionBranch(t, session, child, duplicate)
			}
			if mode == "missing-staged-child" {
				delete(session.pendingPack, child.root.Digest)
			}
			if mode == "corrupt-staged-child" {
				page := session.pendingPack[child.root.Digest]
				page.Entries = append([]storageformat.DomainEntry(nil), page.Entries...)
				page.Entries[0].Value = []byte("corrupt-synthetic-value")
				session.pendingPack[child.root.Digest] = page
			}
			err := session.prunePendingProjectionPages(root.root)
			if mode != "valid" {
				if !errors.Is(err, domain.ErrInvalid) {
					t.Fatalf("malformed staged closure accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(session.pendingPack) != 2 {
				t.Fatalf("retained %d pages, want exactly final branch and leaf", len(session.pendingPack))
			}
			if _, retained := session.pendingPack[orphan.root.Digest]; retained {
				t.Fatal("unreachable sort run retained")
			}
			if err := session.flushPack(ctx); err != nil {
				t.Fatal(err)
			}
			reader := duplicateProjectionSession008(engine, owner, projectionID)
			value, found, err := reader.lookup(ctx, root.root, "a")
			if err != nil || !found || string(value.Data) != "synthetic" {
				t.Fatalf("pruning lost reachable value: found=%t value=%q err=%v", found, value.Data, err)
			}
		})
	}
}

func TestUploadPlanningProjectionPruningPreservesPriorPackReferences(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	projectionID := uploadPlanningProjectionID008(owner)
	prior := duplicateProjectionSession008(engine, owner, projectionID)
	oldChild := reviewProjectionLeaf(t, prior, "a")
	if err := prior.flushPack(ctx); err != nil {
		t.Fatal(err)
	}
	current := duplicateProjectionSession008(engine, owner, projectionID)
	if err := current.bindPackedMutation("new-immutable-source"); err != nil {
		t.Fatal(err)
	}
	newChild := reviewProjectionLeaf(t, current, "c")
	root := reviewProjectionBranch(t, current, oldChild, newChild)
	if err := current.prunePendingProjectionPages(root.root); err != nil {
		t.Fatal(err)
	}
	if len(current.pendingPack) != 2 {
		t.Fatalf("current pack contains %d pages, want two staged pages", len(current.pendingPack))
	}
	if err := current.flushPack(ctx); err != nil {
		t.Fatal(err)
	}
	reader := duplicateProjectionSession008(engine, owner, projectionID)
	for _, key := range []string{"a", "c"} {
		value, found, err := reader.lookup(ctx, root.root, key)
		if err != nil || !found || string(value.Data) != "synthetic" {
			t.Fatalf("mixed-pack closure lost %q: found=%t value=%q err=%v", key, found, value.Data, err)
		}
	}
}

func TestUploadPlanningProjectionPruningPreservesUnchangedUnpackedRoot(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	projectionID := uploadPlanningProjectionID008(owner)
	unpacked := duplicateProjectionSession008(engine, owner, projectionID)
	unpacked.packedWrites = false
	root := reviewProjectionLeaf(t, unpacked, "a")
	if root.root.PackID != "" {
		t.Fatal("fixture did not create an ordinary canonical page")
	}
	// An empty-directory namespace mutation changes the source root but has
	// no file-posting changes, so an incremental projection retains this root.
	current := duplicateProjectionSession008(engine, owner, projectionID)
	if err := current.prunePendingProjectionPages(root.root); err != nil {
		t.Fatalf("unchanged canonical-page projection was treated as a missing staged page: %v", err)
	}
	value, found, err := current.lookup(ctx, root.root, "a")
	if err != nil || !found || string(value.Data) != "synthetic" {
		t.Fatalf("unchanged canonical-page projection lost authority: found=%t value=%q err=%v", found, value.Data, err)
	}
}
