package architecturelab

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

func TestUploadBatchEconomicsBeforeAndAfter(t *testing.T) {
	ctx := context.Background()
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{1, 100} {
		t.Run(fmt.Sprintf("items-%d", count), func(t *testing.T) {
			current := openCurrentProviderHarness(t, fmt.Sprintf("upload-batch-before-%d", count))
			current.ledger.Reset()
			for index := 0; index < count; index++ {
				_, err := current.service.CreateUpload(ctx, current.user, domain.CreateUploadRequest{
					Path: domain.MustParseUserPath(fmt.Sprintf("/upload-%05d", index)), Size: 7, MediaType: "text/plain",
					IdempotencyKey: fmt.Sprintf("upload-batch-before-%05d", index),
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			logCurrentEconomics(t, fmt.Sprintf("before/batch/create-upload-%d", count), model, current.ledger)

			stateLedger := providerbudget.NewLedger()
			stateBackend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), stateLedger)
			uploads, err := openRecordDomain(ctx, stateBackend, fmt.Sprintf("upload-batch-after-%d", count))
			if err != nil {
				t.Fatal(err)
			}
			fileLedger := providerbudget.NewLedger()
			fileBase := objectmemory.New()
			server := httptest.NewServer(fileBase)
			t.Cleanup(server.Close)
			clock := domain.NewFixedClock(time.Date(2049, 7, 8, 9, 10, 11, 0, time.UTC))
			if err := fileBase.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(&currentBatchEntropy{})); err != nil {
				t.Fatal(err)
			}
			fileBackend := budgettest.Wrap(providerbudget.RoleFile, fileBase, fileLedger)
			changes := make([]RecordChange, count)
			parallel := providerbudget.WithTrace(ctx, providerbudget.Trace{Operation: "create-upload-batch", Subsystem: "provider-upload-session", ParallelGroup: "upload-begins"})
			stateLedger.Reset()
			fileLedger.Reset()
			for index := 0; index < count; index++ {
				id := fmt.Sprintf("upload-%05d", index)
				key := objectstore.MustKey("endlessfs/research/upload-batch/blob-" + fmt.Sprintf("%05d", index))
				if _, err := fileBackend.BeginUpload(parallel, objectstore.UploadRequest{UploadID: id, Key: key, Size: 7, MediaType: "text/plain", ExpiresAt: clock.Now().Add(time.Minute)}); err != nil {
					t.Fatal(err)
				}
				changes[index] = RecordChange{Key: id, Value: []byte(fmt.Sprintf(`{"path":"/upload-%05d","size":7}`, index))}
			}
			if _, err := uploads.Mutate(ctx, RecordMutation{ID: "create-upload-batch", Changes: changes}); err != nil {
				t.Fatal(err)
			}
			events := append(stateLedger.Events(), fileLedger.Events()...)
			totals, err := model.Estimate(events)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("after/batch/create-upload-%d requests=%d costPicoUSD=%d serialP95=%d criticalP95=%d requestBytes=%d responseBytes=%d", count, totals.Requests, totals.CostPicoUSD, totals.P95Micros, totals.CriticalP95Micros, totals.RequestBytes, totals.ResponseBytes)
		})
	}
}
