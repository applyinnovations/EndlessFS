package portable

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestDirectoryIndexSingleEntryUpdateRetainsUnchangedNodesAndReadsBoundedPages(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2045, 3, 4, 5, 6, 7, 0, time.UTC))
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("directory-index-scalability", 1<<18)))
	user, _ := domain.ParseUserID("aWlpaWlpaWlpaWlpaWlpaQ")
	scope, _ := domain.NewScope(user, domain.AreaLive)

	entries := make([]storageformat.DirectoryEntry, 1024)
	for index := range entries {
		name := fmt.Sprintf("file-%04d.bin", index)
		entries[index] = withCurrentTestFingerprint(storageformat.DirectoryEntry{
			Name: name, NameDigest: storageformat.NameDigest(name), Kind: domain.EntryFile,
			BlobID: fmt.Sprintf("blob-%04d", index), Size: 1, MediaType: "application/octet-stream", ModifiedAt: clock.Now(),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].NameDigest < entries[j].NameDigest })
	initial, err := engine.Files().prepareDirectory(ctx, scope, storageformat.RootDirectoryID, entries, 1)
	if err != nil {
		t.Fatal(err)
	}
	initialNodes := 0
	for _, prerequisite := range initial.prerequisites {
		if strings.Contains(prerequisite.Key, "/index/") {
			initialNodes++
		}
		if _, err := backend.Put(ctx, objectstore.MustKey(prerequisite.Key), prerequisite.Body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}
	rootKey := storageformat.DirectoryRootKey(user.String(), "live", storageformat.RootDirectoryID)
	if _, err := backend.Put(ctx, rootKey, initial.rootBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := engine.Files().readDirectory(ctx, scope, storageformat.RootDirectoryID, true)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.manifest.ContentIndexRootID == "" || snapshot.manifest.ContentIndexRootDigest == "" {
		t.Fatal("non-empty directory has no persistent content-occurrence index")
	}
	if len(snapshot.manifest.ContentSketch) != directoryContentSketchSize {
		t.Fatalf("directory content sketch size = %d", len(snapshot.manifest.ContentSketch))
	}
	changed, found := findDirectoryEntry(entries, "file-0512.bin")
	if !found {
		t.Fatal("test entry is missing")
	}
	old := changed
	changed.Size = 2
	changed.LogicalVersion, _ = directoryEntryVersion(changed)
	updates := make(map[string]directoryUpdate)
	trail := []directoryTrailNode{{scope: scope, path: domain.MustParseUserPath("/"), directoryID: storageformat.RootDirectoryID, snapshot: snapshot}}
	if err := applyDirectoryEntryChange(updates, trail, &old, &changed); err != nil {
		t.Fatal(err)
	}
	updated, err := engine.Files().prepareDirectoryMutation(ctx, updates[rootKey.String()], 2)
	if err != nil {
		t.Fatal(err)
	}
	updatedNodes := 0
	updatedContentNodes := 0
	for _, prerequisite := range updated.prerequisites {
		if strings.Contains(prerequisite.Key, "/index/") {
			updatedNodes++
		}
		if strings.Contains(prerequisite.Key, "/content-index/") {
			updatedContentNodes++
		}
		if _, err := backend.Put(ctx, objectstore.MustKey(prerequisite.Key), prerequisite.Body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}
	if initialNodes < 10 || updatedNodes > 4 {
		t.Fatalf("directory index node writes: initial=%d update=%d; want a multi-page index and at most four copy-on-write nodes", initialNodes, updatedNodes)
	}
	if updatedContentNodes == 0 || updatedContentNodes > 8 {
		t.Fatalf("directory content-index node writes = %d; want a bounded copy-on-write path", updatedContentNodes)
	}
	if _, err := backend.Put(ctx, rootKey, updated.rootBody, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: snapshot.object.Version}); err != nil {
		t.Fatal(err)
	}

	gets := 0
	hooks := &hookedBackend{Backend: backend}
	hooks.get = func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
		gets++
		return backend.Get(ctx, key)
	}
	engine.backend = hooks
	page, err := engine.Files().List(ctx, scope, domain.ListRequest{Directory: domain.MustParseUserPath("/"), PageSize: 10, Sort: domain.SortName})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 10 || page.NextCursor == "" || len(page.NextCursor) > 2048 || gets > 8 {
		t.Fatalf("bounded directory page = entries:%d cursor:%d backend gets:%d", len(page.Entries), len(page.NextCursor), gets)
	}
	for _, field := range directorySecondarySorts {
		gets = 0
		secondary, err := engine.Files().List(ctx, scope, domain.ListRequest{Directory: domain.MustParseUserPath("/"), PageSize: 10, Sort: field})
		if err != nil {
			t.Fatal(err)
		}
		if len(secondary.Entries) != 10 || secondary.NextCursor == "" || len(secondary.NextCursor) > 2048 || gets > 9 {
			t.Fatalf("bounded %s page = entries:%d cursor:%d backend gets:%d", field, len(secondary.Entries), len(secondary.NextCursor), gets)
		}
		gets = 0
		continued, err := engine.Files().List(ctx, scope, domain.ListRequest{Directory: domain.MustParseUserPath("/"), PageSize: 10, Sort: field, Cursor: secondary.NextCursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(continued.Entries) != 10 || gets > 10 {
			t.Fatalf("bounded continued %s page = entries:%d backend gets:%d", field, len(continued.Entries), gets)
		}
	}
	if err := engine.CloseWrites(ctx, "invalidate-directory-cursor"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Files().List(ctx, scope, domain.ListRequest{Directory: domain.MustParseUserPath("/"), PageSize: 10, Sort: domain.SortName, Cursor: page.NextCursor}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("closed-gate directory cursor error = %v; want invalid", err)
	}
}

func TestDirectoryContentIndexSharesRootAcrossSameContentReplacement(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2045, 3, 5, 5, 6, 7, 0, time.UTC))
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("same-content-index-entropy", 1<<17)))
	user, _ := domain.ParseUserID("amtra2tra2tra2tra2traw")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	entry := withCurrentTestFingerprint(storageformat.DirectoryEntry{
		Name: "same.bin", NameDigest: storageformat.NameDigest("same.bin"), Kind: domain.EntryFile,
		BlobID: "first-blob", Size: 9, MediaType: "application/octet-stream", ModifiedAt: clock.Now(),
	})
	initial, err := engine.Files().prepareDirectory(ctx, scope, storageformat.RootDirectoryID, []storageformat.DirectoryEntry{entry}, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, prerequisite := range initial.prerequisites {
		if _, err := backend.Put(ctx, objectstore.MustKey(prerequisite.Key), prerequisite.Body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}
	rootKey := storageformat.DirectoryRootKey(user.String(), "live", storageformat.RootDirectoryID)
	if _, err := backend.Put(ctx, rootKey, initial.rootBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := engine.Files().readDirectoryMetadata(ctx, scope, storageformat.RootDirectoryID, true)
	if err != nil {
		t.Fatal(err)
	}
	replacement := entry
	replacement.BlobID = "second-blob"
	replacement.ModifiedAt = clock.Now().Add(time.Second)
	replacement.LogicalVersion, err = directoryEntryVersion(replacement)
	if err != nil {
		t.Fatal(err)
	}
	updates := make(map[string]directoryUpdate)
	trail := []directoryTrailNode{{scope: scope, path: domain.MustParseUserPath("/"), directoryID: storageformat.RootDirectoryID, snapshot: snapshot}}
	if err := applyDirectoryEntryChange(updates, trail, &entry, &replacement); err != nil {
		t.Fatal(err)
	}
	updated, err := engine.Files().prepareDirectoryMutation(ctx, updates[rootKey.String()], 2)
	if err != nil {
		t.Fatal(err)
	}
	var manifest storageformat.DirectoryManifest
	for _, prerequisite := range updated.prerequisites {
		if strings.Contains(prerequisite.Key, "/content-index/") {
			t.Fatalf("same-content replacement rewrote content node %s", prerequisite.Key)
		}
		if strings.Contains(prerequisite.Key, "/manifests/") {
			var envelope storageformat.Envelope
			if err := storageformat.DecodeEnvelope(prerequisite.Body, objectstore.MustKey(prerequisite.Key), directoryManifestSchema, &envelope, &manifest); err != nil {
				t.Fatal(err)
			}
		}
	}
	if manifest.ContentIndexRootID != snapshot.manifest.ContentIndexRootID || manifest.ContentIndexRootDigest != snapshot.manifest.ContentIndexRootDigest {
		t.Fatalf("same-content replacement changed content root: before=%s/%s after=%s/%s", snapshot.manifest.ContentIndexRootID, snapshot.manifest.ContentIndexRootDigest, manifest.ContentIndexRootID, manifest.ContentIndexRootDigest)
	}
}

func TestDirectoryContentAccumulatorMatchesFullRebuildAcrossSmallChanges(t *testing.T) {
	now := time.Date(2045, 3, 4, 5, 6, 7, 0, time.UTC)
	entry := func(name string, size int64) storageformat.DirectoryEntry {
		return withCurrentTestFingerprint(storageformat.DirectoryEntry{
			Name: name, NameDigest: storageformat.NameDigest(name), Kind: domain.EntryFile,
			BlobID: "blob-" + name, Size: size, MediaType: "application/octet-stream", ModifiedAt: now,
		})
	}
	before := []storageformat.DirectoryEntry{entry("alpha", 1), entry("bravo", 2), entry("charlie", 3)}
	sort.Slice(before, func(i, j int) bool { return before[i].NameDigest < before[j].NameDigest })
	accumulator, digest, err := directoryContentIdentity(before)
	if err != nil {
		t.Fatal(err)
	}
	after := replaceDirectoryEntry(before, nil, entry("delta", 4))
	old, _ := findDirectoryEntry(after, "bravo")
	replacement := entry("bravo", 9)
	after = replaceDirectoryEntry(after, &old, replacement)
	after = removeDirectoryEntry(after, "alpha")
	incrementalAccumulator, incrementalDigest, err := updateDirectoryContentIdentity(accumulator, before, after)
	if err != nil {
		t.Fatal(err)
	}
	rebuiltAccumulator, rebuiltDigest, err := directoryContentIdentity(after)
	if err != nil {
		t.Fatal(err)
	}
	if incrementalAccumulator != rebuiltAccumulator || incrementalDigest != rebuiltDigest || digest == rebuiltDigest {
		t.Fatalf("incremental directory identity mismatch: incremental=(%q,%q) rebuilt=(%q,%q)", incrementalAccumulator, incrementalDigest, rebuiltAccumulator, rebuiltDigest)
	}
	restoredAccumulator, restoredDigest, err := updateDirectoryContentIdentity(incrementalAccumulator, after, before)
	if err != nil {
		t.Fatal(err)
	}
	if restoredAccumulator != accumulator || restoredDigest != digest {
		t.Fatalf("restored directory identity = (%q,%q); want (%q,%q)", restoredAccumulator, restoredDigest, accumulator, digest)
	}
}
