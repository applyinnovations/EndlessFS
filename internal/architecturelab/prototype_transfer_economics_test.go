package architecturelab

import (
	"context"
	"io"
	"net/http"
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

func TestPrototypeTransferProviderEconomics(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2049, 4, 5, 6, 7, 8, 0, time.UTC))
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	stateLedger := providerbudget.NewLedger()
	stateBackend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), stateLedger)
	fileLedger := providerbudget.NewLedger()
	fileBase := objectmemory.New()
	server := httptest.NewServer(fileBase)
	t.Cleanup(server.Close)
	if err := fileBase.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(&currentBatchEntropy{})); err != nil {
		t.Fatal(err)
	}
	fileBackend := budgettest.Wrap(providerbudget.RoleFile, fileBase, fileLedger)
	uploads, err := openRecordDomain(ctx, stateBackend, "transfer-uploads")
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := openHybrid(ctx, stateBackend, Options{DomainID: "transfer-namespace"})
	if err != nil {
		t.Fatal(err)
	}
	namespace := candidate.(*hybridEngine)
	measure := func(name string, run func() error) {
		t.Helper()
		stateLedger.Reset()
		fileLedger.Reset()
		if err := run(); err != nil {
			t.Fatal(err)
		}
		events := append(stateLedger.Events(), fileLedger.Events()...)
		totals, err := model.Estimate(events)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("after/%s requests=%d state=%d file=%d costPicoUSD=%d serialP95=%d criticalP95=%d requestBytes=%d responseBytes=%d", name, totals.Requests, totals.ByRole[providerbudget.RoleState].Requests, totals.ByRole[providerbudget.RoleFile].Requests, totals.CostPicoUSD, totals.P95Micros, totals.CriticalP95Micros, totals.RequestBytes, totals.ResponseBytes)
	}
	fileKey := objectstore.MustKey("endlessfs/research/file/blob-one")
	var handle objectstore.UploadHandle
	measure("transfer/create-upload", func() error {
		if _, err := uploads.Mutate(ctx, RecordMutation{ID: "create-upload", Key: "upload/one", Value: []byte(`{"path":"/file","size":7}`)}); err != nil {
			return err
		}
		var err error
		handle, err = fileBackend.BeginUpload(ctx, objectstore.UploadRequest{UploadID: "one", Key: fileKey, Size: 7, MediaType: "text/plain", ExpiresAt: clock.Now().Add(time.Minute)})
		return err
	})
	request, _ := http.NewRequestWithContext(ctx, handle.Capability.Method, handle.Capability.URL, strings.NewReader("payload"))
	for name, value := range handle.Capability.Headers {
		request.Header.Set(name, value)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	measure("transfer/upload-status", func() error {
		if _, _, err := uploads.Get(ctx, "upload/one"); err != nil {
			return err
		}
		_, err := fileBackend.UploadProgress(ctx, handle.Lease)
		return err
	})
	measure("transfer/complete-upload", func() error {
		if _, _, err := uploads.Get(ctx, "upload/one"); err != nil {
			return err
		}
		progress, err := fileBackend.UploadProgress(ctx, handle.Lease)
		if err != nil {
			return err
		}
		if _, err := fileBackend.Verify(ctx, fileKey, objectstore.IntegrityFor([]byte("payload"))); err != nil {
			return err
		}
		_, err = namespace.Mutate(ctx, Mutation{ID: "publish-upload", Kind: MutationCreateFile, ToArea: AreaLive, Destination: "/file", NodeID: "file", Size: progress.Size, BlobIdentity: fileKey.String()})
		return err
	})
	if err := namespace.Compact(ctx); err != nil {
		t.Fatal(err)
	}

	abortKey := objectstore.MustKey("endlessfs/research/file/blob-abort")
	if _, err := uploads.Mutate(ctx, RecordMutation{ID: "create-abort", Key: "upload/abort", Value: []byte(`{"path":"/abort","size":3}`)}); err != nil {
		t.Fatal(err)
	}
	abortHandle, err := fileBackend.BeginUpload(ctx, objectstore.UploadRequest{UploadID: "abort", Key: abortKey, Size: 3, MediaType: "application/octet-stream", ExpiresAt: clock.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	measure("transfer/abort-upload", func() error {
		if _, _, err := uploads.Get(ctx, "upload/abort"); err != nil {
			return err
		}
		if err := fileBackend.AbortUpload(ctx, abortHandle.Lease); err != nil {
			return err
		}
		_, err := uploads.Mutate(ctx, RecordMutation{ID: "abort-upload", Key: "upload/abort", Value: []byte(`{"state":"aborted"}`)})
		return err
	})

	measure("transfer/create-download", func() error {
		if _, found, err := namespace.Stat(ctx, AreaLive, "/file"); err != nil || !found {
			return err
		}
		info, err := fileBackend.Head(ctx, fileKey)
		if err != nil {
			return err
		}
		_, err = fileBackend.CreateDownload(ctx, objectstore.DownloadRequest{Key: fileKey, Version: info.Version, Filename: "file", MediaType: "text/plain", Disposition: domain.DispositionAttachment, ExpiresAt: clock.Now().Add(time.Minute)})
		return err
	})
}
