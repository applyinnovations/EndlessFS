package portable

import (
	"context"
	"errors"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// Apply this file only after listAtHeadWithSession exists. The primary typed
// validator regression is deliberately in a separate baseline-compatible file.
func TestCheckpointListAtHeadWithSessionPreservesLiteralDeltaPagination(t *testing.T) {
	ctx := withoutMetadataPackCache(context.Background())
	ledger := providerbudget.NewLedger()
	store := newConsistencyDomainStore(budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger), nil)
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:pagination-contract"}
	seed := newConsistencyDomainTreeSession(store, reference)
	seed.enablePackedWrites("pagination-contract-source")
	entry := func(key, value, version string) storageformat.DomainEntry {
		return storageformat.DomainEntry{Key: key, Value: []byte(value), LogicalVersion: version}
	}
	root, err := seed.buildTree(ctx, []storageformat.DomainEntry{
		entry("item/a", "A0", "va0"), entry("item/b", "B0", "vb0"),
		entry("item/c", "C0", "vc0"), entry("item/d", "D0", "vd0"),
		entry("item/e", "E0", "ve0"), entry("other/a", "X0", "vx0"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.flushPack(ctx); err != nil {
		t.Fatal(err)
	}
	head := storageformat.DomainHead{Base: root, Deltas: []storageformat.DomainDelta{
		{Revision: 1, Changes: []storageformat.DomainChange{
			{Key: "item/a", Delete: true},
			{Key: "item/aa", Value: []byte("AA1"), LogicalVersion: "vaa1"},
			{Key: "item/b", Value: []byte("B1"), LogicalVersion: "vb1"},
			{Key: "item/c", Delete: true},
		}},
		{Revision: 2, Changes: []storageformat.DomainChange{
			{Key: "item/b", Value: []byte("B2"), LogicalVersion: "vb2"},
			{Key: "item/c", Value: []byte("C2"), LogicalVersion: "vc2"},
			{Key: "item/d", Delete: true},
			{Key: "item/f", Value: []byte("F2"), LogicalVersion: "vf2"},
		}},
	}}
	reader := newConsistencyDomainTreeSession(store, reference)
	ledger.Reset()
	pages := [][]storageformat.DomainEntry{
		{entry("item/aa", "AA1", "vaa1"), entry("item/b", "B2", "vb2")},
		{entry("item/c", "C2", "vc2"), entry("item/e", "E0", "ve0")},
		{entry("item/f", "F2", "vf2")},
	}
	after := ""
	for _, expected := range pages {
		got, err := store.listAtHeadWithSession(ctx, reference, head, "item/", after, 2, reader)
		if err != nil {
			t.Fatal(err)
		}
		checkpointSessionReviewDetachedValues(t, got, expected)
		after = got[len(got)-1].Key
		// Mutating the response must not poison cached base or delta bytes.
		got[0].Value[0] ^= 0xff
	}
	got, err := store.listAtHeadWithSession(ctx, reference, head, "item/", after, 2, reader)
	if err != nil || len(got) != 0 {
		t.Fatalf("terminal page count=%d err=%v", len(got), err)
	}
	got, err = store.listAtHeadWithSession(ctx, reference, head, "item/", "item/b", 2, reader)
	if err != nil {
		t.Fatal(err)
	}
	checkpointSessionReviewDetachedValues(t, got, pages[1])
	if events := ledger.Events(); len(events) != 1 || events[0].Kind != providerbudget.RequestObjectGet || events[0].Failed {
		t.Fatal("reused pagination session did not read exactly one durable pack")
	}

	// Cancellation must be checked before reading already-cached pages or
	// returning a delta-only result; no additional provider work is justified.
	for _, prefix := range []string{"item/", "absent/"} {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		before := len(ledger.Events())
		if _, err := store.listAtHeadWithSession(canceled, reference, head, prefix, "", 2, reader); !errors.Is(err, context.Canceled) {
			t.Fatalf("cached pagination ignored caller cancellation: %v", err)
		}
		if len(ledger.Events()) != before {
			t.Fatal("canceled cached pagination issued provider work")
		}
	}

	// Neither store identity nor domain identity may be rebound around a
	// warmed decoded session, even when supplied a syntactically valid root.
	for name, supplied := range map[string]*consistencyDomainTreeSession{
		"other-store":     newConsistencyDomainTreeSession(newConsistencyDomainStore(objectmemory.New(), nil), reference),
		"other-reference": newConsistencyDomainTreeSession(store, consistencyDomainRef{Kind: reference.Kind, ID: "owner:other-pagination"}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.listAtHeadWithSession(ctx, reference, head, "item/", "", 2, supplied); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("misbound pagination session accepted: %v", err)
			}
		})
	}
	// Repeat a real descriptor with a false authenticated aggregate after the
	// correct descriptor has already populated the reused page cache.
	forged := head
	forged.Base.EntryCount++
	if _, err := store.listAtHeadWithSession(ctx, reference, forged, "item/", "", 2, reader); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("reused pagination session hid descriptor mismatch: %v", err)
	}
}
