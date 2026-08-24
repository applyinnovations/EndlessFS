package architecturelab

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

func TestShareEconomicsBeforeAndAfter(t *testing.T) {
	ctx := context.Background()
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	current := openCurrentProviderHarness(t, "share-baseline")
	if _, err := current.engine.Files().CreateDirectory(ctx, current.live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/shared")}); err != nil {
		t.Fatal(err)
	}
	current.upload(t, "/shared/file.txt", "payload")
	current.ledger.Reset()
	created, err := current.service.CreateShare(ctx, current.user, domain.MustParseUserPath("/shared"), nil, "share-economics-0001")
	if err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "before/share/create", model, current.ledger)
	if _, err := current.service.Shares(ctx, current.user); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "before/share/list", model, current.ledger)
	token := created.Link.Reveal()[strings.LastIndex(created.Link.Reveal(), "/")+1:]
	if _, err := current.service.PublicShare(ctx, token, "/", 100, ""); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "before/share/public-list", model, current.ledger)
	if _, err := current.service.PublicStat(ctx, token, "/file.txt"); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "before/share/public-stat", model, current.ledger)
	entry, err := current.engine.Files().Stat(ctx, current.live, domain.MustParseUserPath("/shared/file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	current.ledger.Reset()
	if _, _, err := current.service.PublicDownload(ctx, token, "/file.txt", entry.Version, false); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "before/share/public-download", model, current.ledger)
	if err := current.service.RevokeShare(ctx, current.user, created.Record.ShareID); err != nil {
		t.Fatal(err)
	}
	logCurrentEconomics(t, "before/share/revoke", model, current.ledger)

	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
	shares, err := openRecordDomain(ctx, backend, "share-after")
	if err != nil {
		t.Fatal(err)
	}
	namespaceCandidate, err := openHybrid(ctx, backend, Options{DomainID: "share-namespace"})
	if err != nil {
		t.Fatal(err)
	}
	namespace := namespaceCandidate.(*hybridEngine)
	for _, mutation := range []Mutation{
		{ID: "shared", Kind: MutationCreateDirectory, ToArea: AreaLive, Destination: "/shared", NodeID: "shared"},
		{ID: "file", Kind: MutationCreateFile, ToArea: AreaLive, Destination: "/shared/file.txt", NodeID: "file", Size: 7, BlobIdentity: "blob"},
	} {
		if _, err := namespace.Mutate(ctx, mutation); err != nil {
			t.Fatal(err)
		}
	}
	if err := namespace.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	measure := func(name string, run func() error) {
		t.Helper()
		ledger.Reset()
		if err := run(); err != nil {
			t.Fatal(err)
		}
		logCurrentEconomics(t, "after/share/"+name, model, ledger)
	}
	measure("create", func() error {
		if _, found, err := namespace.Stat(ctx, AreaLive, "/shared"); err != nil || !found {
			return err
		}
		_, err := shares.Mutate(ctx, RecordMutation{ID: "create-share", Key: "share", Value: []byte(`{"root":"/shared"}`)})
		return err
	})
	measure("list", func() error { _, err := shares.List(ctx, ""); return err })
	measure("public-list", func() error {
		if _, _, err := shares.Get(ctx, "share"); err != nil {
			return err
		}
		_, err := namespace.List(ctx, AreaLive, "/shared", 100)
		return err
	})
	measure("public-stat", func() error {
		if _, _, err := shares.Get(ctx, "share"); err != nil {
			return err
		}
		_, _, err := namespace.Stat(ctx, AreaLive, "/shared/file.txt")
		return err
	})
	fileLedger := providerbudget.NewLedger()
	fileBase := objectmemory.New()
	fileServer := httptest.NewServer(fileBase)
	t.Cleanup(fileServer.Close)
	clock := domain.NewFixedClock(time.Date(2049, 8, 9, 10, 11, 12, 0, time.UTC))
	if err := fileBase.ConfigureDataPlane(fileServer.URL, clock, domain.NewIDGenerator(&currentBatchEntropy{})); err != nil {
		t.Fatal(err)
	}
	fileBackend := budgettest.Wrap(providerbudget.RoleFile, fileBase, fileLedger)
	fileKey := objectstore.MustKey("endlessfs/research/share/blob")
	if _, err := fileBackend.Put(ctx, fileKey, []byte("payload"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	ledger.Reset()
	fileLedger.Reset()
	if _, _, err := shares.Get(ctx, "share"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := namespace.Stat(ctx, AreaLive, "/shared/file.txt"); err != nil {
		t.Fatal(err)
	}
	info, err := fileBackend.Head(ctx, fileKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fileBackend.CreateDownload(ctx, objectstore.DownloadRequest{Key: fileKey, Version: info.Version, Filename: "file.txt", MediaType: "text/plain", Disposition: domain.DispositionAttachment, ExpiresAt: clock.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	totals, err := model.Estimate(append(ledger.Events(), fileLedger.Events()...))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after/share/public-download requests=%d costPicoUSD=%d serialP95=%d criticalP95=%d requestBytes=%d responseBytes=%d", totals.Requests, totals.CostPicoUSD, totals.P95Micros, totals.CriticalP95Micros, totals.RequestBytes, totals.ResponseBytes)
	measure("revoke", func() error {
		_, err := shares.Mutate(ctx, RecordMutation{ID: "revoke-share", Key: "share", Value: []byte(`{"root":"/shared","revoked":true}`)})
		return err
	})
}
