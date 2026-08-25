package portable

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestStateIndexLifecycleSplitsUpdatesRemovesAndScans(t *testing.T) {
	ctx := context.Background()
	backend, engine := currentMigrationEngine(t)
	install := func(t *testing.T, prepared preparedStateIndex) {
		t.Helper()
		if err := engine.ensureMutationPrerequisites(ctx, prepared.prerequisites); err != nil {
			t.Fatal(err)
		}
		condition := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
		if prepared.snapshot.exists {
			condition = objectstore.PutCondition{Mode: objectstore.PutMatch, Version: prepared.snapshot.object.Version}
		}
		if _, err := backend.Put(ctx, storageformat.StateIndexRootKey(prepared.root.Namespace), prepared.rootBody, condition); err != nil {
			t.Fatal(err)
		}
	}
	key := func(index int) state.Key {
		return state.MustKey(state.NamespaceAccounts, fmt.Sprintf("item-%03d", index))
	}

	for index := range 130 {
		prepared, err := engine.prepareStateIndexMutation(ctx, key(index), fmt.Sprintf("version-%03d", index), false)
		if err != nil {
			t.Fatalf("prepare insert %d: %v", index, err)
		}
		install(t, prepared)
	}
	root, err := engine.readStateIndexRoot(ctx, string(state.NamespaceAccounts))
	if err != nil || !root.exists || root.root.EntryCount != 130 {
		t.Fatalf("split index root = %+v, %v", root.root, err)
	}
	entry, err := engine.stateIndexEntry(ctx, key(64))
	if err != nil || entry.LogicalVersion != "version-064" {
		t.Fatalf("indexed entry = %+v, %v", entry, err)
	}
	if _, err := engine.stateIndexEntry(ctx, state.MustKey(state.NamespaceAccounts, "absent")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("absent index entry error = %v", err)
	}
	entries, err := engine.collectStateIndexEntries(ctx, root.root, "accounts/", "", 17)
	if err != nil || len(entries) != 17 || entries[0].LogicalKey >= entries[16].LogicalKey {
		t.Fatalf("bounded index scan = %+v, %v", entries, err)
	}
	next, err := engine.collectStateIndexEntries(ctx, root.root, "accounts/", entries[16].LogicalKey, 17)
	if err != nil || len(next) != 17 || next[0].LogicalKey <= entries[16].LogicalKey {
		t.Fatalf("continued index scan = %+v, %v", next, err)
	}

	updated, err := engine.prepareStateIndexMutation(ctx, key(64), "version-updated", false)
	if err != nil {
		t.Fatal(err)
	}
	install(t, updated)
	entry, err = engine.stateIndexEntry(ctx, key(64))
	if err != nil || entry.LogicalVersion != "version-updated" {
		t.Fatalf("updated index entry = %+v, %v", entry, err)
	}
	removed, err := engine.prepareStateIndexMutation(ctx, key(64), "", true)
	if err != nil {
		t.Fatal(err)
	}
	install(t, removed)
	if _, err := engine.stateIndexEntry(ctx, key(64)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("removed index entry error = %v", err)
	}
	if _, err := engine.prepareStateIndexMutation(ctx, key(64), "", true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("repeat index removal error = %v", err)
	}
	if values, err := engine.collectStateIndexEntries(ctx, storageformat.StateIndexRoot{}, "accounts/", "", 10); err != nil || len(values) != 0 {
		t.Fatalf("empty index scan = %+v, %v", values, err)
	}
}

func TestStateIndexValidationRejectsCorruptionAndOverflow(t *testing.T) {
	ctx := context.Background()
	backend, engine := currentMigrationEngine(t)
	namespace := string(state.NamespaceAccounts)
	logical := state.MustKey(state.NamespaceAccounts, "valid").String()
	validEntry := storageformat.StateIndexEntry{LogicalKey: logical, LogicalVersion: "version"}
	validLeaf := storageformat.StateIndexNode{SchemaVersion: 1, Namespace: namespace, NodeID: "leaf", Leaf: true, Entries: []storageformat.StateIndexEntry{validEntry}}

	for name, child := range map[string]storageformat.StateIndexChild{
		"identity": {},
		"range":    {NodeID: "node", NodeDigest: "digest", FirstKey: "z", LastKey: "a", EntryCount: 1},
		"count":    {NodeID: "node", NodeDigest: "digest", FirstKey: "a", LastKey: "z"},
	} {
		t.Run("child-"+name, func(t *testing.T) {
			if err := validateStateIndexChild(child); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("invalid child error = %v", err)
			}
		})
	}
	for name, mutate := range map[string]func(*storageformat.StateIndexNode){
		"schema":        func(node *storageformat.StateIndexNode) { node.SchemaVersion = 0 },
		"namespace":     func(node *storageformat.StateIndexNode) { node.Namespace = "other" },
		"identity":      func(node *storageformat.StateIndexNode) { node.NodeID = "" },
		"shape":         func(node *storageformat.StateIndexNode) { node.Leaf = false },
		"logical-key":   func(node *storageformat.StateIndexNode) { node.Entries[0].LogicalKey = "invalid" },
		"logical-route": func(node *storageformat.StateIndexNode) { node.Entries[0].LogicalKey = "profiles/valid" },
		"version":       func(node *storageformat.StateIndexNode) { node.Entries[0].LogicalVersion = "" },
		"order": func(node *storageformat.StateIndexNode) {
			node.Entries = append(node.Entries, node.Entries[0])
		},
	} {
		t.Run("node-"+name, func(t *testing.T) {
			candidate := validLeaf
			candidate.Entries = append([]storageformat.StateIndexEntry(nil), validLeaf.Entries...)
			mutate(&candidate)
			if err := validateStateIndexNode(namespace, candidate); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("invalid node error = %v", err)
			}
		})
	}
	if _, err := stateIndexNodeChild(storageformat.StateIndexNode{Leaf: true}, "digest"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty leaf error = %v", err)
	}
	if _, err := stateIndexNodeChild(storageformat.StateIndexNode{}, "digest"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty branch error = %v", err)
	}
	if _, err := stateIndexNodeChild(storageformat.StateIndexNode{Children: []storageformat.StateIndexChild{
		{FirstKey: "a", LastKey: "b", EntryCount: math.MaxUint64},
		{FirstKey: "c", LastKey: "d", EntryCount: 1},
	}}, "digest"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("overflowing branch error = %v", err)
	}
	if _, err := engine.rootStateIndexChild(ctx, storageformat.StateIndexRoot{Namespace: namespace}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty non-empty root error = %v", err)
	}
	if _, _, err := engine.mutateStateIndexNode(ctx, namespace, nil, logical, "", true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("nil-root removal error = %v", err)
	}

	rootKey := storageformat.StateIndexRootKey(namespace)
	invalidRoot := storageformat.StateIndexRoot{SchemaVersion: 1, Namespace: namespace, EntryCount: 1}
	if _, err := backend.Put(ctx, rootKey, encodeInternalEnvelope(t, stateIndexRootSchema, rootKey, 1, invalidRoot), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.readStateIndexRoot(ctx, namespace); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid root error = %v", err)
	}
}
