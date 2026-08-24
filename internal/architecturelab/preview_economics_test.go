package architecturelab

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/preview/durable"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/secret"
)

func TestPreviewValidationEconomicsIsUnchanged(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2049, 6, 7, 8, 9, 10, 0, time.UTC))
	base := objectmemory.New()
	server := httptest.NewServer(base)
	t.Cleanup(server.Close)
	ids := domain.NewIDGenerator(&currentBatchEntropy{})
	if err := base.ConfigureDataPlane(server.URL, clock, ids); err != nil {
		t.Fatal(err)
	}
	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RolePreviewArtifact, base, ledger)
	client := *server.Client()
	client.Transport = providerbudget.InstrumentRoundTripper(providerbudget.RolePreviewArtifact, client.Transport, ledger, func(request *http.Request) (providerbudget.RequestKind, error) {
		return providerbudget.RequestDataDownload, nil
	})
	store, err := durable.New(durable.Options{
		Backend: backend, Transfers: backend, Clock: clock, IDs: ids,
		Key:           secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x41}, 32))),
		CapabilityTTL: time.Minute, DataOrigin: server.URL, HTTPClient: &client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	totals, err := model.Estimate(ledger.Events())
	if err != nil {
		t.Fatal(err)
	}
	if totals.ByKind[providerbudget.RequestDataDownload].Requests != 1 {
		t.Fatalf("preview validation data-plane downloads=%d, want 1", totals.ByKind[providerbudget.RequestDataDownload].Requests)
	}
	t.Logf("before=after/preview/validate requests=%d costPicoUSD=%d serialP95=%d criticalP95=%d requestBytes=%d responseBytes=%d", totals.Requests, totals.CostPicoUSD, totals.P95Micros, totals.CriticalP95Micros, totals.RequestBytes, totals.ResponseBytes)
}
