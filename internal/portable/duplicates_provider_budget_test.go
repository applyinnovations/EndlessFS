package portable_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

func TestProviderBudgetDuplicateReconciliationWorkflows(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2053, 2, 3, 4, 5, 6, 0, time.UTC))
	stateLedger, fileLedger := providerbudget.NewLedger(), providerbudget.NewLedger()
	stateBackend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), stateLedger)
	fileBase := objectmemory.New()
	server := httptest.NewServer(fileBase)
	t.Cleanup(server.Close)
	if err := fileBase.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(220, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	fileBackend := budgettest.Wrap(providerbudget.RoleFile, fileBase, fileLedger)
	engine, err := portable.Open(ctx, portable.Options{
		Backend: stateBackend, FileBackend: fileBackend, Clock: clock,
		IDs:      domain.NewIDGenerator(bytes.NewReader(deterministic(221, 4<<20))),
		Writer:   portable.WriterConfiguration{WriterSetID: "duplicate-budget", ConfigurationDigest: "duplicate-budget-v1", KeyringIdentifiers: []string{"budget-key"}},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x61}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	economics, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	ratchet, err := gcs.RegionalStandardFlatBudgetRatchet()
	if err != nil {
		t.Fatal(err)
	}
	check := func(name string) {
		t.Helper()
		events := append(stateLedger.Events(), fileLedger.Events()...)
		if report, err := ratchet.CheckExact(name, economics, []providerbudget.Role{providerbudget.RoleState, providerbudget.RoleFile}, events); err != nil {
			t.Errorf("%s: %v; observed=%+v; events=%+v", name, err, report.Totals, events)
		}
		stateLedger.Reset()
		fileLedger.Reset()
	}

	owner, _ := domain.ParseUserID("Y2NjY2NjY2NjY2NjY2NjYw")
	live, _ := domain.NewScope(owner, domain.AreaLive)
	for _, directory := range []string{"/left", "/right"} {
		if _, err := engine.Files().CreateDirectory(ctx, live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(directory)}); err != nil {
			t.Fatal(err)
		}
		uploadPortableFile(t, server.Client(), engine.Files(), live, domain.MustParseUserPath(directory+"/same.bin"), []byte("identical"))
	}
	stateLedger.Reset()
	fileLedger.Reset()

	groups, err := engine.Files().ListDuplicateGroups(ctx, owner, domain.DuplicateGroupRequest{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	check("maintenance-derived-view-rebuild-schema-011")
	fileGroup := duplicateGroupByKind(t, groups.Groups, domain.DuplicateFile)

	groups, err = engine.Files().ListDuplicateGroups(ctx, owner, domain.DuplicateGroupRequest{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	check("duplicates-list-groups-schema-011")
	if _, err := engine.Files().ListDuplicateOccurrences(ctx, owner, domain.DuplicateOccurrenceRequest{GroupID: fileGroup.ID, Limit: 20}); err != nil {
		t.Fatal(err)
	}
	check("duplicates-list-occurrences-schema-011")

	ignored, err := engine.Files().SetDuplicateGroupIgnored(ctx, owner, domain.SetDuplicateIgnoredRequest{GroupID: fileGroup.ID, Ignored: true})
	if err != nil {
		t.Fatal(err)
	}
	check("duplicates-set-group-ignored-schema-011")
	if _, err := engine.Files().SetDuplicateGroupIgnored(ctx, owner, domain.SetDuplicateIgnoredRequest{GroupID: fileGroup.ID, Ignored: false, ExpectedRevision: ignored.Revision}); err != nil {
		t.Fatal(err)
	}
	stateLedger.Reset()
	fileLedger.Reset()

	left := domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/left")}
	right := domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/right")}
	if _, err := engine.Files().CompareDuplicateDirectories(ctx, owner, domain.DuplicateDirectoryComparisonRequest{Left: left, Right: right}); err != nil {
		t.Fatal(err)
	}
	stateLedger.Reset()
	fileLedger.Reset()
	if _, err := engine.Files().CompareDuplicateDirectories(ctx, owner, domain.DuplicateDirectoryComparisonRequest{Left: left, Right: right}); err != nil {
		t.Fatal(err)
	}
	check("duplicates-compare-directories-schema-011")
	if _, err := engine.Files().ListDuplicateDirectoryOverlaps(ctx, owner, domain.DuplicateDirectoryOverlapRequest{Directory: left, Limit: 20}); err != nil {
		t.Fatal(err)
	}
	check("duplicates-list-directory-overlaps-schema-011")

	pair, err := engine.Files().SetDuplicateDirectoryIgnored(ctx, owner, domain.SetDuplicateDirectoryIgnoredRequest{Left: left, Right: right, Ignored: true})
	if err != nil {
		t.Fatal(err)
	}
	check("duplicates-set-directory-ignored-schema-011")
	if _, err := engine.Files().SetDuplicateDirectoryIgnored(ctx, owner, domain.SetDuplicateDirectoryIgnoredRequest{Left: left, Right: right, Ignored: false, ExpectedRevision: pair.Revision}); err != nil {
		t.Fatal(err)
	}
	stateLedger.Reset()
	fileLedger.Reset()

	preview, err := engine.Files().PreviewDuplicateReconciliation(ctx, owner, domain.DuplicateReconciliationPreviewRequest{Left: left, Right: right, RemoveFrom: domain.DuplicateSideRight})
	if err != nil || preview.PlanToken == "" {
		t.Fatalf("reconciliation preview = %+v, %v", preview, err)
	}
	check("duplicates-preview-reconciliation-schema-011")
	if _, err := engine.Files().ValidateDuplicateReconciliation(ctx, owner, preview.PlanToken); err != nil {
		t.Fatal(err)
	}
	check("duplicates-validate-reconciliation-schema-011")
	if _, err := engine.Files().ApplyDuplicateReconciliation(ctx, owner, preview.PlanToken, "duplicate-budget-apply-01"); err != nil {
		t.Fatal(err)
	}
	check("duplicates-apply-reconciliation-schema-011")
}
