package portable

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestConsistencyDomainTreeDescriptorAndCacheDenialMatrix(t *testing.T) {
	ctx := context.Background()
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner-tree-denials"}
	backend := objectmemory.New()
	session := newConsistencyDomainTreeSession(newConsistencyDomainStore(backend, nil), reference)
	if _, err := session.readPage(ctx, domainPageRef{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty page reference error = %v", err)
	}
	if _, err := consistencyDomainPageDescriptor(storageformat.DomainPage{Level: 1}, "digest"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty branch descriptor error = %v", err)
	}
	overflow := storageformat.DomainPage{Level: 1, Children: []storageformat.DomainPageChild{{FirstKey: "a", LastKey: "a", EntryCount: math.MaxUint64, ByteCount: math.MaxUint64}, {FirstKey: "b", LastKey: "b", EntryCount: 1, ByteCount: 1}}}
	if _, err := consistencyDomainPageDescriptor(overflow, "digest"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("overflowing branch descriptor error = %v", err)
	}
	if index := consistencyDomainChildIndex(nil, "a"); index != -1 {
		t.Fatalf("empty child index = %d", index)
	}
	children := []storageformat.DomainPageChild{{FirstKey: "b", LastKey: "c"}, {FirstKey: "d", LastKey: "e"}}
	if index := consistencyDomainChildIndex(children, "z"); index != 1 {
		t.Fatalf("high child index = %d", index)
	}

	page := storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Level: 0, Entries: []storageformat.DomainEntry{{Key: "a", Value: []byte("a"), LogicalVersion: "v1"}}}
	written, err := session.writePage(ctx, page)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyConsistencyDomainPageRef(page, domainPageRef{root: written.root, firstKey: "wrong", lastKey: written.lastKey}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("descriptor key-bound error = %v", err)
	}
	if _, err := session.readPage(ctx, domainPageRef{root: storageformat.DomainTreeRoot{Digest: written.root.Digest, Level: written.root.Level, EntryCount: written.root.EntryCount + 1, ByteCount: written.root.ByteCount}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("cached descriptor mismatch error = %v", err)
	}

	corruptCache := newConsistencyDomainTreeSession(newConsistencyDomainStore(objectmemory.New(), nil), reference)
	corruptCache.pages[written.root.Digest] = storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Level: 0, Entries: []storageformat.DomainEntry{{Key: "b", Value: []byte("b"), LogicalVersion: "v2"}}}
	if _, err := corruptCache.writePage(ctx, page); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("cached digest collision error = %v", err)
	}

	knownMismatch := newConsistencyDomainTreeSession(newConsistencyDomainStore(backend, nil), reference)
	knownMismatch.known[written.root.Digest] = storageformat.DomainTreeRoot{Digest: written.root.Digest, Level: 9}
	if _, err := knownMismatch.writePage(ctx, page); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("known descriptor mismatch error = %v", err)
	}
	missingKnown := newConsistencyDomainTreeSession(newConsistencyDomainStore(objectmemory.New(), nil), reference)
	missingKnown.known[written.root.Digest] = written.root
	if _, err := missingKnown.writePage(ctx, page); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing known page error = %v", err)
	}

	collisionBackend := objectmemory.New()
	collisionSession := newConsistencyDomainTreeSession(newConsistencyDomainStore(collisionBackend, nil), reference)
	key := storageformat.DomainPageKey(reference.Kind, reference.ID, written.root.Digest)
	if _, err := collisionBackend.Put(ctx, key, []byte("different"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := collisionSession.writePage(ctx, page); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("provider digest collision error = %v", err)
	}
}

func TestConsistencyDomainTreeNestedCorruptionAndConstructionFailureMatrix(t *testing.T) {
	ctx := context.Background()
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner-tree-nested-failures"}

	t.Run("provider-page-binding", func(t *testing.T) {
		backend := objectmemory.New()
		page := storageformat.DomainPage{SchemaVersion: 1, DomainID: "another-domain", Kind: reference.Kind, Level: 0, Entries: []storageformat.DomainEntry{{Key: "key", Value: []byte("value"), LogicalVersion: "version"}}}
		body, err := storageformat.EncodeCanonical(page)
		if err != nil {
			t.Fatal(err)
		}
		digest := storageformat.Digest(body)
		key := storageformat.DomainPageKey(reference.Kind, reference.ID, digest)
		if _, err := backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		session := newConsistencyDomainTreeSession(newConsistencyDomainStore(backend, nil), reference)
		if _, err := session.readPage(ctx, domainPageRef{root: storageformat.DomainTreeRoot{Digest: digest, Level: 0, EntryCount: 1, ByteCount: 5}}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("misbound provider page error = %v", err)
		}
	})

	t.Run("cached-descriptor", func(t *testing.T) {
		session := newConsistencyDomainTreeSession(newConsistencyDomainStore(objectmemory.New(), nil), reference)
		digest := storageformat.Digest([]byte("invalid-cached-descriptor"))
		session.pages[digest] = storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Level: 1}
		if _, err := session.readPage(ctx, domainPageRef{root: storageformat.DomainTreeRoot{Digest: digest, Level: 1}}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid cached descriptor error = %v", err)
		}
	})

	branchSession := func() (*consistencyDomainTreeSession, storageformat.DomainTreeRoot) {
		session := newConsistencyDomainTreeSession(newConsistencyDomainStore(objectmemory.New(), nil), reference)
		childDigest := storageformat.Digest([]byte("missing-nested-child"))
		page := storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Level: 1, Children: []storageformat.DomainPageChild{{FirstKey: "a", LastKey: "z", Digest: childDigest, Level: 0, EntryCount: 1}}}
		body, err := storageformat.EncodeCanonical(page)
		if err != nil {
			t.Fatal(err)
		}
		digest := storageformat.Digest(body)
		descriptor, err := consistencyDomainPageDescriptor(page, digest)
		if err != nil {
			t.Fatal(err)
		}
		session.pages[digest] = page
		return session, descriptor.root
	}

	t.Run("forward-and-reverse-recursion", func(t *testing.T) {
		forward, root := branchSession()
		if _, err := forward.collect(ctx, root, "", "", 1); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("forward nested read error = %v", err)
		}
		reverse, root := branchSession()
		if _, err := reverse.collectOrdered(ctx, root, "", 1, true); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("reverse nested read error = %v", err)
		}
	})

	t.Run("apply-and-rewrite", func(t *testing.T) {
		session := newConsistencyDomainTreeSession(newConsistencyDomainStore(objectmemory.New(), nil), reference)
		missing := storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("missing-apply-root")), Level: 0, EntryCount: 1}
		if _, err := session.apply(ctx, missing, []storageformat.DomainChange{{Key: "key", Value: []byte("value"), LogicalVersion: "version"}}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("apply root read error = %v", err)
		}
		emptyBranch := storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Level: 1}
		if _, err := session.rewrite(ctx, emptyBranch, []storageformat.DomainChange{{Key: "key", Delete: true}}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("branch without editable child error = %v", err)
		}
		missingChild := storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Level: 1, Children: []storageformat.DomainPageChild{{FirstKey: "a", LastKey: "z", Digest: storageformat.Digest([]byte("missing-rewrite-child")), Level: 0, EntryCount: 1}}}
		if _, err := session.rewrite(ctx, missingChild, []storageformat.DomainChange{{Key: "key", Delete: true}}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("rewrite child read error = %v", err)
		}
	})

	t.Run("bounded-construction", func(t *testing.T) {
		failure := domain.NewError(domain.ErrorUnavailable, "tree construction unavailable")
		hooks := &hookedBackend{Backend: objectmemory.New(), put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return "", failure
		}}
		session := newConsistencyDomainTreeSession(newConsistencyDomainStore(hooks, nil), reference)
		valid := storageformat.DomainEntry{Key: "key", Value: []byte("value"), LogicalVersion: "version"}
		if _, err := session.buildTree(ctx, []storageformat.DomainEntry{valid}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("build tree write error = %v", err)
		}
		oversizedEntry := storageformat.DomainEntry{Key: "oversized", Value: make([]byte, storageformat.MaxCanonicalBytes), LogicalVersion: "version"}
		if _, err := session.writeLeafGroups(ctx, []storageformat.DomainEntry{oversizedEntry}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("leaf group bound error = %v", err)
		}
		oversizedText := strings.Repeat("x", storageformat.MaxCanonicalBytes)
		child := domainPageRef{root: storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("child")), Level: 0, EntryCount: 1}, firstKey: oversizedText, lastKey: oversizedText}
		if _, err := session.writeBranchGroups(ctx, 1, []domainPageRef{child}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("branch group bound error = %v", err)
		}
		page := storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Level: 0, Entries: []storageformat.DomainEntry{oversizedEntry}}
		if _, err := session.writePage(ctx, page); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("page encoding bound error = %v", err)
		}
	})
}

func TestConsistencyDomainTreeRangeRewriteAndParallelFailureMatrix(t *testing.T) {
	ctx := context.Background()
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner-tree-ranges"}
	backend := objectmemory.New()
	session := newConsistencyDomainTreeSession(newConsistencyDomainStore(backend, nil), reference)
	entries := make([]storageformat.DomainEntry, 600)
	for index := range entries {
		key := domainTestKey(index)
		entries[index] = storageformat.DomainEntry{Key: key, Value: []byte(key), LogicalVersion: "version-" + key}
	}
	root, err := session.buildTree(ctx, entries)
	if err != nil {
		t.Fatal(err)
	}
	if values, err := session.collect(ctx, root, "", "", 0); err != nil || values != nil {
		t.Fatalf("zero-limit collect = %+v, %v", values, err)
	}
	if values, err := session.collectOrdered(ctx, root, "", 0, true); err != nil || values != nil {
		t.Fatalf("zero-limit reverse collect = %+v, %v", values, err)
	}
	forward, err := session.collect(ctx, root, "item-0", "item-0100", 5)
	if err != nil || len(forward) != 5 || forward[0].Key <= "item-0100" {
		t.Fatalf("forward range = %+v, %v", forward, err)
	}
	reverse, err := session.collectOrdered(ctx, root, "item-0500", 5, true)
	if err != nil || len(reverse) != 5 || reverse[0].Key >= "item-0500" {
		t.Fatalf("reverse range = %+v, %v", reverse, err)
	}
	if unchanged, err := session.apply(ctx, root, nil); err != nil || unchanged != root {
		t.Fatalf("empty apply = %+v, %v", unchanged, err)
	}
	if empty, err := session.apply(ctx, storageformat.DomainTreeRoot{}, []storageformat.DomainChange{{Key: "missing", Delete: true}}); err != nil || empty.Digest != "" {
		t.Fatalf("delete from empty tree = %+v, %v", empty, err)
	}
	updated, err := session.apply(ctx, root, []storageformat.DomainChange{{Key: entries[0].Key, Delete: true}, {Key: "item-0250", Value: []byte("changed"), LogicalVersion: "changed"}, {Key: "item-9999", Value: []byte("added"), LogicalVersion: "added"}})
	if err != nil {
		t.Fatal(err)
	}
	if updated.EntryCount != root.EntryCount {
		t.Fatalf("updated entry count = %d, want %d", updated.EntryCount, root.EntryCount)
	}
	if _, found, err := session.lookup(ctx, updated, entries[0].Key); err != nil || found {
		t.Fatalf("deleted lookup found=%v error=%v", found, err)
	}
	if value, found, err := session.lookup(ctx, updated, "item-9999"); err != nil || !found || string(value.Data) != "added" {
		t.Fatalf("added lookup = %+v found=%v error=%v", value, found, err)
	}

	failingBase := objectmemory.New()
	failure := domain.NewError(domain.ErrorUnavailable, "page write denied")
	failing := &hookedBackend{Backend: failingBase, put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
		return "", failure
	}}
	failingSession := newConsistencyDomainTreeSession(newConsistencyDomainStore(failing, nil), reference)
	if _, err := failingSession.writePages(ctx, []storageformat.DomainPage{
		{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Level: 0, Entries: []storageformat.DomainEntry{{Key: "a", Value: []byte("a"), LogicalVersion: "a"}}},
		{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Level: 0, Entries: []storageformat.DomainEntry{{Key: "b", Value: []byte("b"), LogicalVersion: "b"}}},
	}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("parallel page failure error = %v", err)
	}
	if pages, err := session.writePages(ctx, nil); err != nil || pages != nil {
		t.Fatalf("empty page write = %+v, %v", pages, err)
	}
	if pages, err := session.writeLeafGroups(ctx, nil); err != nil || pages != nil {
		t.Fatalf("empty leaf write = %+v, %v", pages, err)
	}
	if pages, err := session.writeBranchGroups(ctx, 1, nil); err != nil || pages != nil {
		t.Fatalf("empty branch write = %+v, %v", pages, err)
	}
	oversized := []storageformat.DomainEntry{{Key: "oversized", Value: make([]byte, storageformat.MaxCanonicalBytes), LogicalVersion: "version"}}
	if _, err := session.leafGroups(oversized); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized leaf group error = %v", err)
	}
}

func TestConsistencyDomainTreeStreamingFailureBoundaries(t *testing.T) {
	ctx := context.Background()
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner-tree-stream-failures"}
	failure := domain.NewError(domain.ErrorUnavailable, "tree transport unavailable")
	missingRoot := storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("missing-tree-page")), Level: 0, EntryCount: 1}

	readFailure := &hookedBackend{Backend: objectmemory.New(), get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
		return objectstore.Object{}, failure
	}}
	readSession := newConsistencyDomainTreeSession(newConsistencyDomainStore(readFailure, nil), reference)
	if _, err := newConsistencyDomainTreeIteratorAfter(ctx, readSession, missingRoot, ""); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("seek iterator read error = %v", err)
	}
	if _, err := newConsistencyDomainTreeIterator(ctx, readSession, missingRoot); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("iterator read error = %v", err)
	}
	if _, _, err := readSession.lookup(ctx, missingRoot, "key"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("lookup read error = %v", err)
	}
	if _, err := readSession.collect(ctx, missingRoot, "", "", 1); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("collect read error = %v", err)
	}
	if _, err := readSession.collectOrdered(ctx, missingRoot, "", 1, true); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("reverse collect read error = %v", err)
	}

	childDigest := storageformat.Digest([]byte("unavailable-child"))
	iterator := &consistencyDomainTreeIterator{ctx: ctx, session: readSession, stack: []domainTreeIteratorFrame{
		{page: storageformat.DomainPage{Level: 1, Children: []storageformat.DomainPageChild{
			{FirstKey: "a", LastKey: "a", Digest: storageformat.Digest([]byte("exhausted-child")), Level: 0, EntryCount: 1},
			{FirstKey: "b", LastKey: "b", Digest: childDigest, Level: 0, EntryCount: 1},
		}}},
		{page: storageformat.DomainPage{Level: 0}, index: 0},
	}}
	if _, _, err := iterator.Next(); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("iterator sibling descent error = %v", err)
	}

	writeFailure := &hookedBackend{Backend: objectmemory.New(), put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
		return "", failure
	}}
	writeSession := newConsistencyDomainTreeSession(newConsistencyDomainStore(writeFailure, nil), reference)
	entry := storageformat.DomainEntry{Key: "key", Value: []byte("value"), LogicalVersion: "version"}
	builder := newConsistencyDomainTreeBuilder(ctx, writeSession)
	builder.leaf = []storageformat.DomainEntry{entry}
	if err := builder.flushLeaf(); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("stream leaf write error = %v", err)
	}
	builder = newConsistencyDomainTreeBuilder(ctx, writeSession)
	builder.levels = [][]domainPageRef{{{
		root:     storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("child")), Level: 0, EntryCount: 1, ByteCount: 5},
		firstKey: "key", lastKey: "key",
	}}}
	if err := builder.flushLevel(0); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("stream branch write error = %v", err)
	}

	conflictThenReadFailure := &hookedBackend{
		Backend: objectmemory.New(),
		put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return "", domain.NewError(domain.ErrorConflict, "already exists")
		},
		get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, failure
		},
	}
	conflictSession := newConsistencyDomainTreeSession(newConsistencyDomainStore(conflictThenReadFailure, nil), reference)
	page := storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Level: 0, Entries: []storageformat.DomainEntry{entry}}
	if _, err := conflictSession.writePage(ctx, page); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("conflicting page verification read error = %v", err)
	}
}
