package architecturelab

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
)

func TestImmutableTreeSplitsUpdatesRemovesAndWalks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := objectmemory.New()
	tree := immutableTree{backend: backend, domainID: "tree-semantics"}
	root, err := tree.empty(ctx, "seed", "tree-test")
	if err != nil {
		t.Fatal(err)
	}
	const entries = maxTreePageItems*4 + 1
	for index := entries - 1; index >= 0; index-- {
		key := fmt.Sprintf("item-%04d", index)
		root, _, err = tree.upsert(ctx, "seed", "tree-test", root, key, map[string]int{"value": index})
		if err != nil {
			t.Fatalf("insert %s: %v", key, err)
		}
	}
	var updated bool
	root, updated, err = tree.upsert(ctx, "update", "tree-test", root, "item-0128", map[string]int{"value": 999})
	if err != nil || !updated {
		t.Fatalf("update existing: updated=%t err=%v", updated, err)
	}
	root, updated, err = tree.upsert(ctx, "update", "tree-test", root, "item-9999", map[string]int{"value": 9999})
	if err != nil || updated {
		t.Fatalf("insert new: updated=%t err=%v", updated, err)
	}
	root, updated, err = tree.remove(ctx, "remove", "tree-test", root, "item-0000")
	if err != nil || !updated {
		t.Fatalf("remove existing: existed=%t err=%v", updated, err)
	}
	unchanged := root
	root, updated, err = tree.remove(ctx, "remove", "tree-test", root, "missing")
	if err != nil || updated || root != unchanged {
		t.Fatalf("remove missing changed tree: root=%q want=%q existed=%t err=%v", root, unchanged, updated, err)
	}

	keys := make([]string, 0, entries)
	if err := tree.walk(ctx, "read", "tree-test", root, func(key string, body json.RawMessage) error {
		keys = append(keys, key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(keys) != entries || !sort.StringsAreSorted(keys) || keys[0] != "item-0001" || keys[len(keys)-1] != "item-9999" {
		t.Fatalf("unexpected walk: count=%d first=%q last=%q sorted=%t", len(keys), keys[0], keys[len(keys)-1], sort.StringsAreSorted(keys))
	}
	body, found, err := tree.lookup(ctx, "read", "tree-test", root, "item-0128")
	if err != nil || !found || string(body) != `{"value":999}` {
		t.Fatalf("updated lookup: found=%t body=%s err=%v", found, body, err)
	}

	assertTreePagesBounded(t, ctx, backend)
}

func TestImmutableTreeRejectsCorruptContentAddressedPage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := objectmemory.New()
	tree := immutableTree{backend: backend, domainID: "tree-corruption"}
	root, err := tree.empty(ctx, "seed", "tree-test")
	if err != nil {
		t.Fatal(err)
	}
	key := objectstore.MustKey(root)
	object, err := backend.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, key, []byte(`{"schemaVersion":1,"level":0,"values":[{"key":"forged","value":true}]}`), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tree.lookup(ctx, "read", "tree-test", root, "forged"); err == nil {
		t.Fatal("corrupt content-addressed page was accepted")
	}
}

func TestImmutableTreeRejectsSemanticallyFalseChildDescriptor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := objectmemory.New()
	tree := immutableTree{backend: backend, domainID: "tree-false-descriptor"}
	value, _ := encode(1)
	leaf := treePage{SchemaVersion: 1, Level: 0, Values: []treeValue{{Key: "value", Value: value}}}
	leafRef, descriptor, err := tree.writePage(ctx, "seed", "tree-test", "", leaf)
	if err != nil {
		t.Fatal(err)
	}
	descriptor.Ref, descriptor.Count = leafRef, descriptor.Count+1
	rootRef, _, err := tree.writePage(ctx, "seed", "tree-test", "", treePage{SchemaVersion: 1, Level: 1, Children: []treeChild{descriptor}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tree.lookup(ctx, "read", "tree-test", rootRef, "value"); err == nil {
		t.Fatal("semantically false child descriptor was accepted")
	}
}

func TestTreeSessionBatchesEditsAcrossPages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := objectmemory.New()
	tree := immutableTree{backend: backend, domainID: "tree-batch"}
	root, err := tree.empty(ctx, "seed", "tree-test")
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < maxTreePageItems*3; index++ {
		root, _, err = tree.upsert(ctx, "seed", "tree-test", root, fmt.Sprintf("item-%04d", index), index)
		if err != nil {
			t.Fatal(err)
		}
	}
	session := newTreeSession(tree)
	replacement, _ := encode(999)
	insertion, _ := encode(1000)
	root, err = session.apply(ctx, "batch", "tree-test", root, []treeEdit{
		{Key: "item-0000", Remove: true, Requirement: treePresent},
		{Key: "item-0096", Value: replacement, Requirement: treePresent},
		{Key: "item-0192", Value: insertion, Requirement: treeAbsent},
	})
	if err != nil {
		t.Fatal(err)
	}
	for key, expected := range map[string]string{"item-0000": "", "item-0096": "999", "item-0192": "1000"} {
		body, found, err := session.lookup(ctx, "read", "tree-test", root, key)
		if err != nil || found != (expected != "") || string(body) != expected {
			t.Fatalf("lookup %s: found=%t body=%s err=%v", key, found, body, err)
		}
	}
	if _, err := session.apply(ctx, "batch", "tree-test", root, []treeEdit{{Key: "item-0096", Value: replacement, Requirement: treeAbsent}}); err == nil {
		t.Fatal("absent precondition accepted an existing value")
	}
	if _, err := session.apply(ctx, "batch", "tree-test", root, []treeEdit{{Key: "missing", Remove: true, Requirement: treePresent}}); err == nil {
		t.Fatal("present precondition accepted a missing value")
	}
}

func TestArchitectureDecodeRejectsTrailingValue(t *testing.T) {
	t.Parallel()
	var value map[string]int
	if err := decode([]byte(`{"one":1} {"two":2}`), &value); err == nil {
		t.Fatal("trailing JSON value was accepted")
	}
}

func assertTreePagesBounded(t *testing.T, ctx context.Context, backend objectstore.Backend) {
	t.Helper()
	page, err := backend.List(ctx, objectstore.ListRequest{Prefix: "endlessfs/research/paged/tree-semantics/pages/", Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Objects) == 0 || page.NextCursor != "" {
		t.Fatalf("unexpected page inventory: count=%d cursor=%q", len(page.Objects), page.NextCursor)
	}
	for _, info := range page.Objects {
		object, err := backend.Get(ctx, info.Key)
		if err != nil {
			t.Fatal(err)
		}
		var candidate treePage
		if err := decode(object.Body, &candidate); err != nil || validateTreePage(candidate) != nil {
			t.Fatalf("invalid stored page %s: decode=%v validate=%v", info.Key.String(), err, validateTreePage(candidate))
		}
	}
}
