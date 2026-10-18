package portable

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// A cached descriptor does not authenticate its page bytes. Repacking must
// reject corruption under the original digest before it derives a new digest;
// otherwise it can turn corrupt staged authority into a valid successor.
func TestBoundedClosureReviewRejectsCorruptOriginalCachedPage(t *testing.T) {
	ctx := context.Background()
	backend := &reviewRepackFaultBackend{Backend: objectmemory.New()}
	clock := domain.NewFixedClock(time.Date(2073, 4, 5, 6, 7, 8, 0, time.UTC))
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "bounded-closure-source"}
	source := newConsistencyDomainTreeSession(newConsistencyDomainStore(backend, nil, clock), reference)
	source.enablePackedWrites("original-source")
	if err := source.bindPackedMutation("complete-original-mutation"); err != nil {
		t.Fatal("fixture mutation binding failed")
	}
	page := storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind,
		Entries: []storageformat.DomainEntry{{Key: "selected", Value: []byte("original-value"), LogicalVersion: "original-version"}}}
	root, err := source.writePage(ctx, page)
	if err != nil {
		t.Fatal("fixture page staging failed")
	}
	corrupted := source.pages[root.root.Digest]
	corrupted.Entries = append([]storageformat.DomainEntry(nil), corrupted.Entries...)
	corrupted.Entries[0].Value = bytes.Clone(corrupted.Entries[0].Value)
	corrupted.Entries[0].Value[0] ^= 1 // Retain every descriptor/count/length field.
	source.pages[root.root.Digest] = corrupted
	source.pendingPack[root.root.Digest] = corrupted
	_, err = repackStagedRoots(ctx, source, []storageformat.DomainTreeRoot{root.root})
	if !errors.Is(err, domain.ErrInvalid) || backend.packPuts != 0 {
		t.Fatalf("corrupt original page was normalized or published: invalid=%t packPuts=%d", errors.Is(err, domain.ErrInvalid), backend.packPuts)
	}
}

func TestBoundedClosureReviewSplitsActualWireAndExpandedOverflow(t *testing.T) {
	for _, mode := range []string{"wire", "expanded"} {
		t.Run(mode, func(t *testing.T) {
			parallelCorrectiveHeavyFixture(t)
			ctx := context.Background()
			base := objectmemory.New()
			backend := &reviewRepackFaultBackend{Backend: base}
			reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "bounded-closure-" + mode}
			clock := domain.NewFixedClock(time.Date(2073, 4, 5, 6, 7, 8, 0, time.UTC))
			source := newConsistencyDomainTreeSession(newConsistencyDomainStore(backend, nil, clock), reference)
			source.enablePackedWrites("original-source")
			if err := source.bindPreparedMutation("complete-original-mutation", time.Time{}); err != nil {
				t.Fatal(err)
			}
			count := 8
			if mode == "expanded" {
				count = 43
			}
			noise := &deterministicScaleReader{state: 0x92673541}
			values := make([][]byte, count)
			children := make([]storageformat.DomainPageChild, count)
			for index := range values {
				value := bytes.Repeat([]byte{byte('a' + index%26)}, 600_000)
				if mode == "wire" {
					_, _ = noise.Read(value)
				}
				values[index] = value
				child, err := source.writePage(ctx, storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Entries: []storageformat.DomainEntry{{Key: fmt.Sprintf("item-%02d", index), Value: value, LogicalVersion: "original-version"}}})
				if err != nil {
					t.Fatal(err)
				}
				children[index] = domainClosureCopiedChild(child)
			}
			root, err := source.writePage(ctx, storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Level: 1, Children: children})
			if err != nil {
				t.Fatal(err)
			}
			members := make([]storageformat.DomainPackedPage, 0, len(source.pendingPack))
			for digest, page := range source.pendingPack {
				members = append(members, storageformat.DomainPackedPage{Digest: digest, Page: page})
			}
			if _, err := storageformat.EncodeDomainPagePack(storageformat.DomainPagePack{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, PackID: source.packID, Pages: members}); !errors.Is(err, storageformat.ErrDomainPagePackTooLarge) {
				t.Fatalf("fixture did not exceed an actual pack bound: %v", err)
			}
			actual, err := repackStagedRoots(ctx, source, []storageformat.DomainTreeRoot{root.root})
			if err != nil || len(actual) != 1 {
				t.Fatalf("bounded rewrite failed: roots=%d err=%v", len(actual), err)
			}
			if backend.packPuts < 2 || backend.headPuts != 0 {
				t.Fatalf("expected multiple immutable packs without authority publication: packs=%d heads=%d", backend.packPuts, backend.headPuts)
			}
			reader := newConsistencyDomainTreeSession(newConsistencyDomainStore(base, nil, clock), reference)
			page, err := reader.readPage(ctx, domainPageRef{root: actual[0]})
			if err != nil || len(page.Children) != count {
				t.Fatalf("fresh reader could not traverse rewritten root: children=%d err=%v", len(page.Children), err)
			}
			crossPack := false
			packIDs := map[string]bool{actual[0].PackID: true}
			for _, child := range page.Children {
				crossPack = crossPack || child.PackID != actual[0].PackID
				packIDs[child.PackID] = true
			}
			if !crossPack {
				t.Fatal("fixture did not exercise a cross-pack child dependency")
			}
			// Every stored object belongs to the final reachable layout: no
			// provisional group was PUT while capacity splitting was in progress.
			objects := base.Export()
			if len(objects) != len(packIDs) {
				t.Fatalf("provisional or missing packs: objects=%d finalIDs=%d", len(objects), len(packIDs))
			}
			for packID := range packIDs {
				body, found := objects[source.packKey(packID).String()]
				// The unchanged strict format reader verifies every stored member
				// and both actual bounds, independently of layout decisions.
				if !found {
					t.Fatal("final pack was not durable")
				}
				if _, err := storageformat.DecodeDomainPagePack(ctx, body, reference.ID, reference.Kind, packID); err != nil {
					t.Fatalf("strict existing reader rejected a final pack: %v", err)
				}
			}
			for index, expected := range values {
				value, found, err := reader.lookup(ctx, actual[0], fmt.Sprintf("item-%02d", index))
				if err != nil || !found || value.LogicalVersion != "original-version" || !bytes.Equal(value.Data, expected) {
					t.Fatalf("fresh reader lost original value/version: item=%d found=%t err=%v", index, found, err)
				}
			}
		})
	}
}
