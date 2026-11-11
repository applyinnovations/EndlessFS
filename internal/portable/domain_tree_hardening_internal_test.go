package portable

import (
	"context"
	"errors"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

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

func TestConsistencyDomainPackedMutationBindingPreventsSharedPrefixCollision(t *testing.T) {
	ctx := context.Background()
	reference := consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "packed-mutation-binding"}
	backend := objectmemory.New()
	newSession := func(binding string) *consistencyDomainTreeSession {
		session := newConsistencyDomainTreeSession(newConsistencyDomainStore(backend, nil), reference)
		session.enablePackedWrites("same-authenticated-head")
		if err := session.bindPackedMutation(binding); err != nil {
			t.Fatal(err)
		}
		return session
	}
	first := storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Level: 0, Entries: []storageformat.DomainEntry{{Key: "shared", Value: []byte("same"), LogicalVersion: "same-version"}}}
	left, right := newSession("move-to-left"), newSession("move-to-right")
	leftFirst, err := left.writePage(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	rightFirst, err := right.writePage(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if leftFirst.root.Digest != rightFirst.root.Digest || leftFirst.root.PackID == rightFirst.root.PackID {
		t.Fatalf("shared page digest/pack binding = %+v / %+v", leftFirst.root, rightFirst.root)
	}
	for session, value := range map[*consistencyDomainTreeSession]string{left: "left", right: "right"} {
		page := storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Level: 0, Entries: []storageformat.DomainEntry{{Key: value, Value: []byte(value), LogicalVersion: value + "-version"}}}
		if _, err := session.writePage(ctx, page); err != nil {
			t.Fatal(err)
		}
		if err := session.flushPack(ctx); err != nil {
			t.Fatalf("flush %s mutation: %v", value, err)
		}
	}
	if err := left.bindPackedMutation("another-operation"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("late mutation rebind error = %v", err)
	}
}
