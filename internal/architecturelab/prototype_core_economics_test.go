package architecturelab

import (
	"context"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

func TestPrototypeCoreProviderEconomics(t *testing.T) {
	ctx := context.Background()
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	type harness struct {
		engine *hybridEngine
		ledger *providerbudget.Ledger
	}
	open := func(id string) harness {
		t.Helper()
		ledger := providerbudget.NewLedger()
		backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
		candidate, err := openHybrid(ctx, backend, Options{DomainID: id})
		if err != nil {
			t.Fatal(err)
		}
		return harness{engine: candidate.(*hybridEngine), ledger: ledger}
	}
	seedFile := func(h harness, path, id string) {
		t.Helper()
		if _, err := h.engine.Mutate(ctx, Mutation{ID: "seed-" + id, Kind: MutationCreateFile, ToArea: AreaLive, Destination: path, NodeID: id, Size: 7, BlobIdentity: "blob-" + id}); err != nil {
			t.Fatal(err)
		}
		if err := h.engine.Compact(ctx); err != nil {
			t.Fatal(err)
		}
		h.ledger.Reset()
	}
	measure := func(name string, h harness, run func() error) {
		t.Helper()
		h.ledger.Reset()
		if err := run(); err != nil {
			t.Fatal(err)
		}
		logCurrentEconomics(t, "after/"+name, model, h.ledger)
	}

	createDirectory := open("core-create-directory")
	measure("namespace/create-directory", createDirectory, func() error {
		_, err := createDirectory.engine.Mutate(ctx, Mutation{ID: "mkdir", Kind: MutationCreateDirectory, ToArea: AreaLive, Destination: "/directory", NodeID: "directory"})
		return err
	})
	completeUpload := open("core-complete-upload")
	measure("namespace/complete-upload-publication", completeUpload, func() error {
		_, err := completeUpload.engine.Mutate(ctx, Mutation{ID: "complete", Kind: MutationCreateFile, ToArea: AreaLive, Destination: "/file", NodeID: "file", Size: 7, BlobIdentity: "blob"})
		return err
	})

	move := open("core-move")
	seedFile(move, "/file", "file")
	measure("namespace/move", move, func() error {
		_, err := move.engine.Mutate(ctx, Mutation{ID: "move", Kind: MutationMove, FromArea: AreaLive, ToArea: AreaLive, Source: "/file", Destination: "/moved"})
		return err
	})
	trash := open("core-trash")
	seedFile(trash, "/file", "file")
	measure("namespace/trash", trash, func() error {
		_, err := trash.engine.Mutate(ctx, Mutation{ID: "trash", Kind: MutationMove, FromArea: AreaLive, ToArea: AreaTrash, Source: "/file", Destination: "/trash-id"})
		return err
	})
	restore := open("core-restore")
	seedFile(restore, "/file", "file")
	if _, err := restore.engine.Mutate(ctx, Mutation{ID: "seed-trash", Kind: MutationMove, FromArea: AreaLive, ToArea: AreaTrash, Source: "/file", Destination: "/trash-id"}); err != nil {
		t.Fatal(err)
	}
	if err := restore.engine.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	measure("namespace/restore", restore, func() error {
		_, err := restore.engine.Mutate(ctx, Mutation{ID: "restore", Kind: MutationMove, FromArea: AreaTrash, ToArea: AreaLive, Source: "/trash-id", Destination: "/file"})
		return err
	})
	copy := open("core-copy")
	seedFile(copy, "/file", "file")
	measure("namespace/copy", copy, func() error {
		_, err := copy.engine.Mutate(ctx, Mutation{ID: "copy", Kind: MutationCopy, FromArea: AreaLive, ToArea: AreaLive, Source: "/file", Destination: "/copy", NodeID: "copy"})
		return err
	})
	deleteHarness := open("core-delete")
	seedFile(deleteHarness, "/file", "file")
	measure("namespace/delete", deleteHarness, func() error {
		_, err := deleteHarness.engine.Mutate(ctx, Mutation{ID: "delete", Kind: MutationDelete, FromArea: AreaLive, Source: "/file"})
		return err
	})

	reads := open("core-reads")
	seedFile(reads, "/file", "file")
	measure("namespace/stat", reads, func() error { _, _, err := reads.engine.Stat(ctx, AreaLive, "/file"); return err })
	measure("namespace/list", reads, func() error { _, err := reads.engine.List(ctx, AreaLive, "/", 100); return err })
	measure("namespace/lookup-children", reads, func() error { _, err := reads.engine.LookupChildren(ctx, AreaLive, "/", []string{"file"}); return err })
	measure("namespace/get-operation", move, func() error { _, _, err := move.engine.loadHead(ctx, "get-operation"); return err })

	controlLedger := providerbudget.NewLedger()
	controlBackend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), controlLedger)
	control, err := openRecordDomain(ctx, controlBackend, "core-control")
	if err != nil {
		t.Fatal(err)
	}
	controlLedger.Reset()
	if _, err := control.Mutate(ctx, RecordMutation{ID: "create", Key: "record", Value: []byte(`{"value":1}`)}); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "after/control/create-or-update-or-delete", model, controlLedger)
	controlLedger.Reset()
	if _, _, err := control.Get(ctx, "record"); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "after/control/read", model, controlLedger)
	controlLedger.Reset()
	if _, err := control.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "after/control/list", model, controlLedger)
}
