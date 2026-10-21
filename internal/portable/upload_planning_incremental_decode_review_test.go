package portable

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestUploadPlanningIncrementalDecodePreservesProjectionWire(t *testing.T) {
	for _, name := range []string{"append", "prepend", "middle", "remove", "nested-rename"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			engine := openNamespaceTestEngine(t, objectmemory.New())
			store := newNamespaceStore(engine)
			live := namespaceTestScope(t, domain.AreaLive)
			files := seedNamespaceBatchFiles(t, store, live, 600)
			if name == "nested-rename" {
				if _, err := engine.Files().CreateDirectory(ctx, live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/nested"), Conflict: domain.ConflictFail}); err != nil {
					t.Fatal(err)
				}
				publishNamespaceTestFile(t, store, live, "/nested/inside.bin", 7, "nested-seed")
			}
			snapshot, err := engine.Files().uploadPlanningProjection008(ctx, live.UserID())
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "append":
				publishNamespaceTestFile(t, store, live, "/zz-new.bin", 7, "new-append")
			case "prepend":
				publishNamespaceTestFile(t, store, live, "/aa-new.bin", 7, "new-prepend")
			case "middle":
				publishNamespaceTestFile(t, store, live, "/file-00300a.bin", 7, "new-middle")
			case "remove":
				if _, err := engine.Files().MoveToTrash(ctx, live.UserID(), domain.TrashRequest{Path: files[300].Path, ExpectedVersion: files[300].Version, TrashID: "diff-remove", IdempotencyKey: "diff-remove"}); err != nil {
					t.Fatal(err)
				}
			case "nested-rename":
				entry, err := engine.Files().Stat(ctx, live, domain.MustParseUserPath("/nested"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err = store.copyOrMove(ctx, true, live, live, domain.MoveRequest{Source: entry.Path, Destination: domain.MustParseUserPath("/relocated"), ExpectedSource: entry.Version, IdempotencyKey: "diff-rename"}); err != nil {
					t.Fatal(err)
				}
			}
			roots := make([]storageformat.DomainTreeRoot, 2)
			wires := make([][]byte, 2)
			var projections [][]storageformat.DomainEntry
			for variant := range 2 {
				view, err := store.loadView(ctx, live.UserID(), "")
				if err != nil {
					t.Fatal(err)
				}
				session := duplicateProjectionSession008(engine, live.UserID(), snapshot.projectionID)
				binding, err := storageformat.EncodeCanonical(struct{ Source, PriorSource, Prior storageformat.DomainTreeRoot }{view.roots[domain.AreaLive].Children, snapshot.head.SourceRoot, snapshot.root})
				if err != nil {
					t.Fatal(err)
				}
				if err = session.bindPackedMutation(storageformat.Digest(binding)); err != nil {
					t.Fatal(err)
				}
				if variant == 0 {
					roots[variant], err = engine.Files().originalUploadPlanningProjection008(ctx, view, snapshot.head.SourceRoot, view.roots[domain.AreaLive].Children, snapshot.root, session)
				} else {
					roots[variant], err = engine.Files().updateUploadPlanningProjection008(ctx, view, snapshot.head.SourceRoot, view.roots[domain.AreaLive].Children, snapshot.root, session)
				}
				if err != nil {
					t.Fatal(err)
				}
				entries, err := session.collect(ctx, roots[variant], "", "", 10000)
				if err != nil {
					t.Fatal(err)
				}
				projections = append(projections, entries)
				if err = session.prunePendingProjectionPages(roots[variant]); err != nil {
					t.Fatal(err)
				}
				var pages []storageformat.DomainPackedPage
				for digest, page := range session.pendingPack {
					pages = append(pages, storageformat.DomainPackedPage{Digest: digest, Page: page})
				}
				wires[variant], err = storageformat.EncodeDomainPagePack(storageformat.DomainPagePack{SchemaVersion: 1, DomainID: session.reference.ID, Kind: session.reference.Kind, PackID: session.packID, Pages: pages})
				if err != nil {
					t.Fatal(err)
				}
			}
			if roots[0] != roots[1] || !bytes.Equal(wires[0], wires[1]) || !reflect.DeepEqual(projections[0], projections[1]) {
				t.Fatal("projection authority/physical wire differs from exact old implementation")
			}
			result, err := engine.Files().PlanUploadSizes(ctx, live.UserID(), domain.UploadSizePlanRequest{Items: []domain.UploadSizePlanItem{{ID: "existing", Path: files[0].Path, Size: 1}, {ID: "new-size", Path: domain.MustParseUserPath("/incoming.bin"), Size: 7}}})
			if err != nil || len(result.Items) != 2 || !result.Items[0].TargetExists || !result.Items[0].FingerprintRequired {
				t.Fatalf("public decision positive control=%+v err=%v", result, err)
			}
			t.Logf("case=%s exact-projection-entries=%d exact-wire-bytes=%d", name, len(projections[0]), len(wires[0]))
		})
	}
}

func TestUploadPlanningIncrementalDecodePreservesSemanticDenials(t *testing.T) {
	for _, name := range []string{"same-invalid-json", "same-invalid-schema", "same-name-mismatch", "different-new-invalid", "same-invalid-different-version"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			engine := openNamespaceTestEngine(t, objectmemory.New())
			live := namespaceTestScope(t, domain.AreaLive)
			writer := newConsistencyDomainTreeSession(engine.stateDomainStore(), consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: live.UserID().String()})
			valid := storageformat.NamespaceEntry{SchemaVersion: 1, NodeID: "semantic-node", Entry: storageformat.DirectoryEntry{Name: "a", NameDigest: storageformat.NameDigest("a"), Kind: domain.EntryFile, BlobID: "semantic-blob", Size: 1, MediaType: "application/octet-stream", MD5: testProviderMD5, CRC32C: testProviderCRC32C, ModifiedAt: engine.clock.Now().UTC()}}
			var err error
			valid.Entry.LogicalVersion, err = directoryEntryVersion(valid.Entry)
			if err != nil {
				t.Fatal(err)
			}
			validBody, err := storageformat.EncodeCanonical(valid)
			if err != nil {
				t.Fatal(err)
			}
			invalidBody := []byte("{")
			if name == "same-invalid-schema" {
				bad := valid
				bad.SchemaVersion = 0
				invalidBody, err = storageformat.EncodeCanonical(bad)
				if err != nil {
					t.Fatal(err)
				}
			}
			if name == "same-name-mismatch" {
				bad := valid
				bad.Entry.Name = "wrong"
				bad.Entry.NameDigest = storageformat.NameDigest("wrong")
				bad.Entry.LogicalVersion, err = directoryEntryVersion(bad.Entry)
				if err != nil {
					t.Fatal(err)
				}
				invalidBody, err = storageformat.EncodeCanonical(bad)
				if err != nil {
					t.Fatal(err)
				}
			}
			roots := make([]storageformat.DomainTreeRoot, 2)
			for i := range 2 {
				body := invalidBody
				if i == 0 && name == "different-new-invalid" {
					body = validBody
				}
				version := "a-version"
				if i == 1 && name == "same-invalid-different-version" {
					version = "changed-a-version"
				}
				sibling := valid
				sibling.NodeID = "z-semantic-node"
				sibling.Entry.Name = "z"
				sibling.Entry.NameDigest = storageformat.NameDigest("z")
				sibling.Entry.BlobID = []string{"old-z-blob", "new-z-blob"}[i]
				sibling.Entry.LogicalVersion, err = directoryEntryVersion(sibling.Entry)
				if err != nil {
					t.Fatal(err)
				}
				siblingBody, err := storageformat.EncodeCanonical(sibling)
				if err != nil {
					t.Fatal(err)
				}
				ref, err := writer.writePage(ctx, storageformat.DomainPage{SchemaVersion: 1, DomainID: writer.reference.ID, Kind: writer.reference.Kind, Entries: []storageformat.DomainEntry{{Key: "a", Value: body, LogicalVersion: version}, {Key: "z", Value: siblingBody, LogicalVersion: sibling.Entry.LogicalVersion}}})
				if err != nil {
					t.Fatal(err)
				}
				roots[i] = ref.root
			}
			for variant := range 2 {
				view := &namespaceView{reference: writer.reference, session: newConsistencyDomainTreeSession(engine.stateDomainStore(), writer.reference)}
				projection := duplicateProjectionSession008(engine, live.UserID(), uploadPlanningProjectionID008(live.UserID()))
				if variant == 0 {
					_, err = engine.Files().originalUploadPlanningProjection008(ctx, view, roots[0], roots[1], storageformat.DomainTreeRoot{}, projection)
				} else {
					_, err = engine.Files().updateUploadPlanningProjection008(ctx, view, roots[0], roots[1], storageformat.DomainTreeRoot{}, projection)
				}
				if !errors.Is(err, domain.ErrInvalid) || len(projection.pendingPack) != 0 {
					t.Fatalf("variant=%d semantic denial=%v staged=%d", variant, err, len(projection.pendingPack))
				}
			}
		})
	}
}
