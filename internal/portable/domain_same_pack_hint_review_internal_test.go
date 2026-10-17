package portable

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestPreparedPackSamePackLeafHintsPreserveReadsAndIntegrity(t *testing.T) {
	ctx := context.Background()
	var priorBytes int
	for _, prepared := range []bool{false, true} {
		t.Run(map[bool]string{false: "derived-unchanged", true: "authoritative-preparation"}[prepared], func(t *testing.T) {
			clock := domain.NewFixedClock(time.Date(2074, 3, 9, 0, 0, 0, 0, time.UTC))
			base := objectmemory.New()
			backend := &admissionBackendProbe{Backend: base}
			store := newConsistencyDomainStore(backend, nil, clock)
			reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "same-pack-hint-proof"}
			session := newConsistencyDomainTreeSession(store, reference)
			session.enablePackedWrites("authenticated-source")
			var err error
			if prepared {
				err = session.bindPreparedMutation("logical-operation", time.Time{})
			} else {
				err = session.bindPackedMutation("logical-operation")
			}
			if err != nil {
				t.Fatal(err)
			}
			entries := make([]storageformat.DomainEntry, 1024)
			for index := range entries {
				entries[index] = storageformat.DomainEntry{Key: fmt.Sprintf("item-%04d", index), Value: []byte(fmt.Sprintf("value-%04d", index)), LogicalVersion: fmt.Sprintf("version-%04d", index)}
			}
			root, err := session.buildTree(ctx, entries)
			if err != nil {
				t.Fatal(err)
			}
			if err := session.flushPack(ctx); err != nil {
				t.Fatal(err)
			}
			pack, err := base.Get(ctx, session.packKey(root.PackID))
			if err != nil {
				t.Fatal(err)
			}
			if !prepared {
				priorBytes = len(pack.Body)
			} else if len(pack.Body) >= priorBytes {
				t.Fatalf("same-pack hints did not reduce actual wire bytes: prior=%d selected=%d", priorBytes, len(pack.Body))
			}
			var reads int
			backend.get = func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
				if strings.Contains(key.String(), "/packs/") {
					reads++
				}
				return base.Get(ctx, key)
			}
			reader := newConsistencyDomainTreeSession(store, reference)
			page, err := reader.readPage(ctx, domainPageRef{root: root})
			if err != nil || len(page.Children) != 4 {
				t.Fatalf("independent root read = %+v %v", page, err)
			}
			for _, child := range page.Children {
				if (child.LeafKeyFilter == "") != prepared {
					t.Fatalf("wrong hint policy: prepared=%t child=%+v", prepared, child)
				}
			}
			for key, present := range map[string]bool{"item-0000": true, "item-1023": true, "item-0000-absent": false, "item-0900-absent": false} {
				_, found, err := reader.lookup(ctx, root, key)
				if err != nil || found != present {
					t.Fatalf("lookup %s: found=%t err=%v", key, found, err)
				}
			}
			if reads != 1 {
				t.Fatalf("same-pack hint policy increased provider GETs: %d", reads)
			}
			t.Logf("same-pack hint comparison prepared=%t wireBytes=%d packGets=%d priorBytes=%d", prepared, len(pack.Body), reads, priorBytes)
			child := page.Children[0]
			bad := domainPageRef{root: storageformat.DomainTreeRoot{Digest: child.Digest, PackID: child.PackID, Level: child.Level, EntryCount: child.EntryCount, ByteCount: child.ByteCount + 1}, firstKey: child.FirstKey, lastKey: child.LastKey, leafKeyFilter: child.LeafKeyFilter}
			if _, err := reader.readPage(ctx, bad); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("optional hint absence weakened authenticated child aggregates: %v", err)
			}
			if !prepared {
				return
			}
			// A later path-copy still preserves hints for unchanged children in
			// the old pack. Only its newly written, same-pack leaf omits one.
			clock.Advance(time.Millisecond)
			next := newConsistencyDomainTreeSession(store, reference)
			next.enablePackedWrites("next-authenticated-source")
			if err := next.bindPreparedMutation("next-operation", time.Time{}); err != nil {
				t.Fatal(err)
			}
			updated, err := next.apply(ctx, root, []storageformat.DomainChange{{Key: "item-0000", Value: []byte("changed"), LogicalVersion: "changed-version"}})
			if err != nil {
				t.Fatal(err)
			}
			branch, err := next.readPage(ctx, domainPageRef{root: updated})
			if err != nil {
				t.Fatal(err)
			}
			var samePack, crossPack int
			for _, child := range branch.Children {
				if child.PackID == updated.PackID {
					samePack++
					if child.LeafKeyFilter != "" {
						t.Fatal("new same-pack child retained redundant hint")
					}
				} else {
					crossPack++
					if child.LeafKeyFilter == "" {
						t.Fatal("old same-pack omission lost a useful cross-pack hint")
					}
				}
			}
			if samePack != 1 || crossPack != 3 {
				t.Fatalf("wrong structural sharing: same=%d cross=%d", samePack, crossPack)
			}
			// Contradictory cached-child metadata must fail before a restored
			// negative hint can hide the descriptor error on later lookups.
			old := page.Children[1]
			honest := domainPageRef{root: storageformat.DomainTreeRoot{Digest: old.Digest, PackID: old.PackID, Level: old.Level, EntryCount: old.EntryCount, ByteCount: old.ByteCount}, firstKey: old.FirstKey, lastKey: old.LastKey}
			beforeReads, deniedPuts := reads, 0
			backend.put = func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
				deniedPuts++
				return "", domain.ErrInternal
			}
			for _, field := range []string{"count", "bytes", "first-key", "last-key"} {
				bad := honest
				switch field {
				case "count":
					bad.root.EntryCount++
				case "bytes":
					bad.root.ByteCount++
				case "first-key":
					bad.firstKey += "-forged"
				case "last-key":
					bad.lastKey += "-forged"
				}
				if _, err := next.pageChild(bad); !errors.Is(err, domain.ErrInvalid) {
					t.Fatalf("cached %s descriptor minted negative hint: %v", field, err)
				}
				if _, err := next.writeBranchGroups(ctx, 1, []domainPageRef{bad}); !errors.Is(err, domain.ErrInvalid) {
					t.Fatalf("cached %s descriptor passed branch sizing: %v", field, err)
				}
				builder := newConsistencyDomainTreeBuilder(ctx, next)
				builder.levels = [][]domainPageRef{{bad}}
				if err := builder.flushLevel(0); !errors.Is(err, domain.ErrInvalid) {
					t.Fatalf("cached %s descriptor passed builder: %v", field, err)
				}
			}
			wrongLevel := domainPageRef{root: root, firstKey: page.Children[0].FirstKey, lastKey: page.Children[len(page.Children)-1].LastKey}
			wrongLevel.root.Level = 0
			if _, err := next.pageChild(wrongLevel); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("authenticated branch was reinterpreted as a hinted leaf: %v", err)
			}
			if reads != beforeReads || deniedPuts != 0 {
				t.Fatalf("descriptor denial contacted provider: gets=%d puts=%d", reads-beforeReads, deniedPuts)
			}
			unpacked := domainPageRef{root: storageformat.DomainTreeRoot{Digest: "digest", Level: 0}, leafKeyFilter: "unpacked-hint"}
			if descriptor, err := next.pageChild(unpacked); err != nil || descriptor.LeafKeyFilter != "unpacked-hint" {
				t.Fatal("unpacked negative hint was removed")
			}
		})
	}
}
