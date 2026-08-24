package architecturelab

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/drive"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type currentProviderHarness struct {
	ctx     context.Context
	engine  *portable.Engine
	service *drive.Service
	server  *httptest.Server
	ledger  *providerbudget.Ledger
	user    domain.UserID
	live    domain.Scope
}

func openCurrentProviderHarness(t *testing.T, id string) currentProviderHarness {
	t.Helper()
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2049, 2, 3, 4, 5, 6, 0, time.UTC))
	ledger := providerbudget.NewLedger()
	stateBackend := budgettest.WrapClassified(providerbudget.RoleState, objectmemory.New(), ledger, func(_ providerbudget.RequestKind, target string) string {
		return storageformat.ClassifyEconomicsTarget(target)
	})
	fileBase := objectmemory.New()
	server := httptest.NewServer(fileBase)
	t.Cleanup(server.Close)
	ids := domain.NewIDGenerator(&currentBatchEntropy{})
	if err := fileBase.ConfigureDataPlane(server.URL, clock, ids); err != nil {
		t.Fatal(err)
	}
	fileBackend := budgettest.Wrap(providerbudget.RoleFile, fileBase, ledger)
	engine, err := portable.Open(ctx, portable.Options{
		Backend: stateBackend, FileBackend: fileBackend, Clock: clock, IDs: ids,
		Writer:   portable.WriterConfiguration{WriterSetID: id, ConfigurationDigest: id + "-v1", KeyringIdentifiers: []string{"benchmark-key"}},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x51}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	protection := secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x61}, 32)))
	service, err := drive.NewService(engine.Files(), engine, currentBatchAccounts{}, ids, clock, protection, "https://endlessfs.test", server.URL, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	user, err := domain.ParseUserID("YmVuY2htYXJrLXVzZXItMDAwMA")
	if err != nil {
		t.Fatal(err)
	}
	live, _ := domain.NewScope(user, domain.AreaLive)
	return currentProviderHarness{ctx: ctx, engine: engine, service: service, server: server, ledger: ledger, user: user, live: live}
}

func (h currentProviderHarness) upload(t *testing.T, pathValue, contents string) {
	t.Helper()
	path := domain.MustParseUserPath(pathValue)
	capability, err := h.service.CreateUpload(h.ctx, h.user, domain.CreateUploadRequest{Path: path, Size: int64(len(contents)), MediaType: "text/plain", IdempotencyKey: "upload-" + strings.ReplaceAll(pathValue, "/", "-")})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequestWithContext(h.ctx, capability.Method, capability.URL, strings.NewReader(contents))
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	response, err := h.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("upload status=%d", response.StatusCode)
	}
	if _, err := h.service.CompleteUpload(h.ctx, h.user, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: int64(len(contents)), MediaType: "text/plain"}); err != nil {
		t.Fatal(err)
	}
}

func logCurrentEconomics(t *testing.T, name string, model providerbudget.Model, ledger *providerbudget.Ledger) {
	t.Helper()
	totals, err := model.Estimate(ledger.Events())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s requests=%d costPicoUSD=%d serialP95=%d criticalP95=%d requestBytes=%d responseBytes=%d failed=%d", name, totals.Requests, totals.CostPicoUSD, totals.P95Micros, totals.CriticalP95Micros, totals.RequestBytes, totals.ResponseBytes, totals.FailedRequests)
	ledger.Reset()
}

func TestCurrentDuplicateWorkflowProviderEconomics(t *testing.T) {
	h := openCurrentProviderHarness(t, "duplicate-baseline")
	for _, path := range []string{"/project-a", "/backup", "/backup/project-a"} {
		if _, err := h.engine.Files().CreateDirectory(h.ctx, h.live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(path)}); err != nil {
			t.Fatal(err)
		}
	}
	h.upload(t, "/project-a/same.txt", "same bytes")
	h.upload(t, "/backup/project-a/same.txt", "same bytes")
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	h.ledger.Reset()
	groups, err := h.engine.Files().ListDuplicateGroups(h.ctx, h.user, domain.DuplicateGroupRequest{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "duplicates/list-groups", model, h.ledger)
	var fileGroup, directoryGroup domain.DuplicateGroup
	for _, group := range groups.Groups {
		if group.Kind == domain.DuplicateFile {
			fileGroup = group
		} else if group.Kind == domain.DuplicateDirectory {
			directoryGroup = group
		}
	}
	if fileGroup.ID == "" || directoryGroup.ID == "" {
		t.Fatalf("duplicate groups=%+v", groups.Groups)
	}

	if _, err := h.engine.Files().ListDuplicateOccurrences(h.ctx, h.user, domain.DuplicateOccurrenceRequest{GroupID: fileGroup.ID, Limit: 20}); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "duplicates/list-occurrences", model, h.ledger)

	ignored, err := h.engine.Files().SetDuplicateGroupIgnored(h.ctx, h.user, domain.SetDuplicateIgnoredRequest{GroupID: fileGroup.ID, Ignored: true})
	if err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "duplicates/set-group-ignored", model, h.ledger)
	if _, err := h.engine.Files().SetDuplicateGroupIgnored(h.ctx, h.user, domain.SetDuplicateIgnoredRequest{GroupID: fileGroup.ID, Ignored: false, ExpectedRevision: ignored.Revision}); err != nil {
		t.Fatal(err)
	}
	h.ledger.Reset()

	left := domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/project-a")}
	right := domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/backup/project-a")}
	if _, err := h.engine.Files().CompareDuplicateDirectories(h.ctx, h.user, domain.DuplicateDirectoryComparisonRequest{Left: left, Right: right}); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "duplicates/compare-directories", model, h.ledger)

	if _, err := h.engine.Files().ListDuplicateDirectoryOverlaps(h.ctx, h.user, domain.DuplicateDirectoryOverlapRequest{Directory: left, Limit: 20}); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "duplicates/list-directory-overlaps", model, h.ledger)

	pair, err := h.engine.Files().SetDuplicateDirectoryIgnored(h.ctx, h.user, domain.SetDuplicateDirectoryIgnoredRequest{Left: left, Right: right, Ignored: true})
	if err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "duplicates/set-directory-ignored", model, h.ledger)
	if _, err := h.engine.Files().SetDuplicateDirectoryIgnored(h.ctx, h.user, domain.SetDuplicateDirectoryIgnoredRequest{Left: left, Right: right, Ignored: false, ExpectedRevision: pair.Revision}); err != nil {
		t.Fatal(err)
	}
	h.ledger.Reset()

	preview, err := h.engine.Files().PreviewDuplicateReconciliation(h.ctx, h.user, domain.DuplicateReconciliationPreviewRequest{Left: left, Right: right, RemoveFrom: domain.DuplicateSideRight, Limit: 20})
	if err != nil || preview.PlanToken == "" {
		t.Fatalf("PreviewDuplicateReconciliation() = %+v, %v", preview, err)
	}
	logCurrentEconomics(t, "duplicates/preview-reconciliation", model, h.ledger)
	if _, err := h.engine.Files().ValidateDuplicateReconciliation(h.ctx, h.user, preview.PlanToken); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "duplicates/validate-reconciliation", model, h.ledger)

	if _, err := h.service.ApplyDuplicateReconciliation(h.ctx, h.user, preview.PlanToken, "apply-reconcile-0001"); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "duplicates/apply-reconciliation", model, h.ledger)
}
