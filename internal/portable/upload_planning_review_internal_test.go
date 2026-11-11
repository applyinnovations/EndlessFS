package portable

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
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
