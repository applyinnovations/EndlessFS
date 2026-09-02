package durable_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/preview"
	"github.com/applyinnovations/endlessfs/internal/preview/durable"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/secret"
)

type previewDataBudgetTransport struct {
	base   http.RoundTripper
	ledger *providerbudget.Ledger
}

func (transport previewDataBudgetTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	event := providerbudget.Event{
		Role: providerbudget.RolePreviewArtifact, Kind: providerbudget.RequestDataDownload,
		Operation: "visible-grid-preview-resolution", Subsystem: "direct-artifact", ParallelGroup: "visible-preview-downloads",
	}
	if err != nil {
		event.Failed = true
	} else {
		event.StatusCode = response.StatusCode
		if response.ContentLength > 0 {
			event.ResponseBytes = response.ContentLength
		}
	}
	transport.ledger.Record(event)
	return response, err
}

func TestProviderBudgetVisiblePreviewWindowUsesOneReadyCatalogRead(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2069, 2, 3, 4, 5, 6, 0, time.UTC))
	base := objectmemory.New()
	server := httptest.NewServer(base)
	t.Cleanup(server.Close)
	ids := domain.NewIDGenerator(&deterministicScaleReader{state: 0x92746153})
	if err := base.ConfigureDataPlane(server.URL, clock, ids); err != nil {
		t.Fatal(err)
	}
	stateLedger, dataLedger := providerbudget.NewLedger(), providerbudget.NewLedger()
	index := budgettest.Wrap(providerbudget.RolePreviewState, base, stateLedger)
	store, err := durable.New(durable.Options{
		Backend: base, IndexBackend: index, Transfers: base, Clock: clock, IDs: ids,
		Key: secret.Value(testBearer(0x72)), CapabilityTTL: time.Minute, DataOrigin: server.URL, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := domain.ParseUserID("UFJFVklFV1NQREVFRFNDQUxFUA")
	if err != nil {
		t.Fatal(err)
	}
	scopeDigest := sha256.Sum256([]byte("visible-directory"))
	cacheScope := base64.RawURLEncoding.EncodeToString(scopeDigest[:])
	selections := make([]preview.ReadySelection, 32)
	for index := range selections {
		binding := preview.Binding{
			Owner: owner, ContentID: domain.ContentID(fmt.Sprintf("content-%02d", index)), ContentVersion: domain.ContentVersion(fmt.Sprintf("version-%02d", index)),
			MediaType: "image/png", SourceSize: 128, RecipeID: "image-webp-q80-v1", Variant: 256,
		}
		artifact := testArtifact(fmt.Sprintf("generation-%02d", index), binding.Variant)
		claim, claimErr := store.Claim(ctx, binding, fmt.Sprintf("claim-%02d", index), clock.Now().Add(time.Minute))
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		if err := store.Commit(ctx, binding, claim, artifact); err != nil {
			t.Fatal(err)
		}
		selection := preview.ReadySelection{CacheScope: cacheScope, Binding: binding}
		if err := store.RecordReady(ctx, selection, artifact.Metadata()); err != nil {
			t.Fatal(err)
		}
		selections[index] = selection
	}
	stateLedger.Reset()
	resolved, err := store.ResolveReady(ctx, selections)
	if err != nil || len(resolved) != len(selections) {
		t.Fatalf("ResolveReady() = %d, %v", len(resolved), err)
	}
	stateEvents := stateLedger.Events()
	if len(stateEvents) != 1 || stateEvents[0].Kind != providerbudget.RequestObjectGet {
		t.Fatalf("ready catalog events = %+v, want one object GET", stateEvents)
	}

	client := *server.Client()
	client.Transport = previewDataBudgetTransport{base: client.Transport, ledger: dataLedger}
	jobs := make(chan int)
	errorsFound := make(chan error, len(selections))
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range jobs {
				capability, capabilityErr := store.CreateKnownDownload(ctx, selections[index].Binding, *resolved[index])
				if capabilityErr != nil {
					errorsFound <- capabilityErr
					continue
				}
				request, requestErr := http.NewRequest(capability.Method, capability.URL, nil)
				if requestErr != nil {
					errorsFound <- requestErr
					continue
				}
				response, requestErr := client.Do(request)
				if requestErr == nil {
					_, requestErr = io.Copy(io.Discard, response.Body)
					closeErr := response.Body.Close()
					if requestErr == nil {
						requestErr = closeErr
					}
					if requestErr == nil && response.StatusCode != http.StatusOK {
						requestErr = fmt.Errorf("preview status %d", response.StatusCode)
					}
				}
				if requestErr != nil {
					errorsFound <- requestErr
				}
			}
		}()
	}
	for index := range selections {
		jobs <- index
	}
	close(jobs)
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	dataEvents := dataLedger.Events()
	if len(dataEvents) != 32 {
		t.Fatalf("direct preview downloads = %d, want 32: %+v", len(dataEvents), dataEvents)
	}
	for _, event := range dataEvents {
		if event.Kind != providerbudget.RequestDataDownload || event.Failed {
			t.Fatalf("unexpected direct preview event: %+v", event)
		}
	}
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	ratchet, err := gcs.RegionalStandardFlatBudgetRatchet()
	if err != nil {
		t.Fatal(err)
	}
	events := append(append([]providerbudget.Event(nil), stateEvents...), dataEvents...)
	if report, checkErr := ratchet.CheckExact("preview-ready-window-32-schema-011", model, []providerbudget.Role{providerbudget.RolePreviewState, providerbudget.RolePreviewArtifact}, events); checkErr != nil {
		t.Errorf("preview-ready-window-32-schema-011: %v; observed=%+v", checkErr, report.Totals)
	}
	t.Logf("provider-scale-observed operation=visible-grid-preview-resolution preview-state=%d direct-downloads=%d subtotal=%d", len(stateEvents), len(dataEvents), len(stateEvents)+len(dataEvents))
}

type deterministicScaleReader struct {
	mu    sync.Mutex
	state uint64
}

func (reader *deterministicScaleReader) Read(buffer []byte) (int, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	for index := range buffer {
		reader.state ^= reader.state << 13
		reader.state ^= reader.state >> 7
		reader.state ^= reader.state << 17
		buffer[index] = byte(reader.state >> 23)
	}
	return len(buffer), nil
}
