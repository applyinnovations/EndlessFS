package portable

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestSchema008NamespaceProjectionRepairMergeAndProviderFailures(t *testing.T) {
	ctx := context.Background()
	live := namespaceTestScope(t, domain.AreaLive)

	t.Run("load-provider-failure", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		store := newNamespaceStore(engine)
		hooks := &hookedBackend{Backend: memory, get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "projection unavailable")
		}}
		engine.backend = hooks
		if _, err := store.loadNamespaceProjection(ctx, live.UserID(), storageformat.Digest([]byte("projection")), storageformat.ProjectionSize); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("load projection error = %v", err)
		}
	})

	t.Run("corrupt-head-is-rebuilt", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		store := newNamespaceStore(engine)
		seedNamespaceBatchFiles(t, store, live, 3)
		view, err := store.loadView(ctx, live.UserID(), "")
		if err != nil {
			t.Fatal(err)
		}
		root := view.roots[domain.AreaLive]
		projectionID := namespaceProjectionID(live.UserID(), domain.AreaLive, root, domain.SortSize)
		key := storageformat.ScopedProjectionHeadKey(live.UserID().String(), storageformat.ProjectionSize, projectionID)
		if _, err := memory.Put(ctx, key, []byte("corrupt"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		projection, err := store.namespaceSortProjection(ctx, view, domain.AreaLive, root, domain.SortSize)
		if err != nil || projection.EntryCount != 3 {
			t.Fatalf("repaired projection = %+v, %v", projection, err)
		}
		loaded, err := store.loadNamespaceProjection(ctx, live.UserID(), projectionID, storageformat.ProjectionSize)
		if err != nil || !loaded.valid {
			t.Fatalf("repaired head = %+v, %v", loaded, err)
		}
	})

	t.Run("head-publication-provider-failure", func(t *testing.T) {
		memory := objectmemory.New()
		hooks := &hookedBackend{Backend: memory}
		engine := openNamespaceTestEngine(t, hooks)
		store := newNamespaceStore(engine)
		seedNamespaceBatchFiles(t, store, live, 2)
		view, err := store.loadView(ctx, live.UserID(), "")
		if err != nil {
			t.Fatal(err)
		}
		hooks.put = func(callCtx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if key.String() != "" && key == storageformat.ScopedProjectionHeadKey(live.UserID().String(), storageformat.ProjectionSize, namespaceProjectionID(live.UserID(), domain.AreaLive, view.roots[domain.AreaLive], domain.SortSize)) {
				return "", domain.NewError(domain.ErrorUnavailable, "projection publish failed")
			}
			return memory.Put(callCtx, key, body, condition)
		}
		if _, err := store.namespaceSortProjection(ctx, view, domain.AreaLive, view.roots[domain.AreaLive], domain.SortSize); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("projection publish error = %v", err)
		}
	})

	t.Run("multi-run-merge-and-duplicate-denial", func(t *testing.T) {
		memory := objectmemory.New()
		engine := openNamespaceTestEngine(t, memory)
		store := newNamespaceStore(engine)
		view, err := store.loadView(ctx, live.UserID(), "")
		if err != nil {
			t.Fatal(err)
		}
		entries := make([]storageformat.DomainEntry, domainPageMaximumItems+1)
		for index := range entries {
			name := fmt.Sprintf("projection-%04d", len(entries)-index)
			stored := storageformat.NamespaceEntry{SchemaVersion: 1, NodeID: name, Entry: storageformat.DirectoryEntry{
				Name: name, NameDigest: storageformat.NameDigest(name), Kind: domain.EntryFile, BlobID: "blob-" + name,
				Size: int64(index + 1), MediaType: "application/octet-stream", MD5: testProviderMD5, CRC32C: testProviderCRC32C,
				ModifiedAt: engine.clock.Now().UTC(),
			}}
			stored.Entry.LogicalVersion, err = directoryEntryVersion(stored.Entry)
			if err != nil {
				t.Fatal(err)
			}
			body, err := encodeNamespaceEntry(stored)
			if err != nil {
				t.Fatal(err)
			}
			entries[index] = storageformat.DomainEntry{Key: name, Value: body, LogicalVersion: stored.Entry.LogicalVersion}
		}
		sortDomainProjectionEntries(entries)
		source, err := view.session.buildTree(ctx, entries)
		if err != nil {
			t.Fatal(err)
		}
		projectionID := storageformat.Digest([]byte("large-projection"))
		projectionSession := newNamespaceProjectionTreeSession(store.domain, live.UserID(), projectionID, storageformat.ProjectionSize)
		root, err := store.buildNamespaceSortProjection(ctx, view, projectionSession, source, domain.SortSize)
		if err != nil || root.EntryCount != uint64(len(entries)) {
			t.Fatalf("merged projection = %+v, %v", root, err)
		}

		duplicateBody := entries[0].Value
		first, err := projectionSession.buildTree(ctx, []storageformat.DomainEntry{{Key: "same", Value: duplicateBody, LogicalVersion: "one"}})
		if err != nil {
			t.Fatal(err)
		}
		second, err := projectionSession.buildTree(ctx, []storageformat.DomainEntry{{Key: "same", Value: duplicateBody, LogicalVersion: "two"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := mergeNamespaceProjectionRuns(ctx, projectionSession, []storageformat.DomainTreeRoot{first, second}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("duplicate merge error = %v", err)
		}
	})
}

func TestSchema008NamespaceProjectionStructuralDenialMatrix(t *testing.T) {
	ctx := context.Background()
	live := namespaceTestScope(t, domain.AreaLive)
	memory := objectmemory.New()
	engine := openNamespaceTestEngine(t, memory)
	store := newNamespaceStore(engine)
	seedNamespaceBatchFiles(t, store, live, 2)
	view, err := store.loadView(ctx, live.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	directory := view.roots[domain.AreaLive]
	projectionID := namespaceProjectionID(live.UserID(), domain.AreaLive, directory, domain.SortSize)
	projectionKey := storageformat.ScopedProjectionHeadKey(live.UserID().String(), storageformat.ProjectionSize, projectionID)
	misbound := storageformat.ProjectionHead{SchemaVersion: 1, OwnerID: "WFhYWFhYWFhYWFhYWFhYWA", ProjectionID: projectionID, Kind: storageformat.ProjectionSize, SourceDomainID: view.reference.ID, SourceRevision: view.head.Revision, SourceRoot: directory.Children}
	body, err := storageformat.EncodeEnvelope(namespaceProjectionHeadSchema, projectionKey, 1, misbound)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Put(ctx, projectionKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.loadNamespaceProjection(ctx, live.UserID(), projectionID, storageformat.ProjectionSize)
	if err != nil || !loaded.exists || loaded.valid {
		t.Fatalf("misbound projection head = %+v, %v", loaded, err)
	}

	projection := newNamespaceProjectionTreeSession(store.domain, live.UserID(), storageformat.Digest([]byte("structural")), storageformat.ProjectionSize)
	if root, err := store.buildNamespaceSortProjection(ctx, view, projection, storageformat.DomainTreeRoot{}, domain.SortSize); err != nil || root.Digest != "" {
		t.Fatalf("empty projection = %+v, %v", root, err)
	}
	missing := storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("missing")), Level: 0, EntryCount: 1}
	if _, err := store.buildNamespaceSortProjection(ctx, view, projection, missing, domain.SortSize); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing source projection error = %v", err)
	}
	if _, err := mergeNamespaceProjectionRuns(ctx, projection, []storageformat.DomainTreeRoot{missing}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing merge run error = %v", err)
	}

	badRoot, err := view.session.buildTree(ctx, []storageformat.DomainEntry{{Key: "bad", Value: []byte("bad"), LogicalVersion: "version"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.buildNamespaceSortProjection(ctx, view, projection, badRoot, domain.SortSize); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("bad namespace projection value error = %v", err)
	}
	valid := validNamespaceTestFile(t, engine, "valid-sort", 1)
	validBody, err := encodeNamespaceEntry(valid)
	if err != nil {
		t.Fatal(err)
	}
	validRoot, err := view.session.buildTree(ctx, []storageformat.DomainEntry{{Key: valid.Entry.Name, Value: validBody, LogicalVersion: valid.Entry.LogicalVersion}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.buildNamespaceSortProjection(ctx, view, projection, validRoot, "invalid"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid projection sort error = %v", err)
	}
	badOwnerView := *view
	badOwnerView.reference.ID = "invalid-owner"
	if _, err := store.namespaceSortProjection(ctx, &badOwnerView, domain.AreaLive, directory, domain.SortSize); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid projection owner error = %v", err)
	}

	// A valid but stale derived head must be rebuilt from the new authoritative
	// namespace rather than trusted or treated as corruption.
	if _, err := store.namespaceSortProjection(ctx, view, domain.AreaLive, directory, domain.SortSize); err != nil {
		t.Fatal(err)
	}
	publishNamespaceTestFile(t, store, live, "/later.bin", 1, "projection-later")
	view, err = store.loadView(ctx, live.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	if root, err := store.namespaceSortProjection(ctx, view, domain.AreaLive, view.roots[domain.AreaLive], domain.SortSize); err != nil || root.EntryCount != 3 {
		t.Fatalf("stale projection repair = %+v, %v", root, err)
	}
}
