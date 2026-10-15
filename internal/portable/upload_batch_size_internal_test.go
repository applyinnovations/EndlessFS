package portable

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestUploadBatchLargeMetadataAdmission(t *testing.T) {
	fixture := newUploadBatchScaleFixture(t, 0x67584930)
	ctx := context.Background()
	parent := ""
	for range 12 {
		parent += "/" + strings.Repeat("a", 250)
		if _, err := fixture.engine.Files().CreateDirectory(ctx, fixture.scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(parent)}); err != nil {
			t.Fatal(err)
		}
	}
	requests := scaleUploadRequestsCount("large-metadata", 4_001)
	for index := range requests {
		requests[index].Path = domain.MustParseUserPath(parent + requests[index].Path.String())
	}
	// The browser sends the common directory once in defaults. A small control
	// request can therefore expand beyond the pack bound without file bytes.
	items := make([]map[string]any, len(requests))
	for index, request := range requests {
		items[index] = map[string]any{"name": request.Path.Name(), "size": request.Size}
	}
	wire, err := json.Marshal(map[string]any{"defaults": map[string]any{"path": parent, "mediaType": "application/octet-stream", "conflict": "rename", "resumable": true}, "uploads": items})
	if err != nil || len(wire) >= 1<<20 {
		t.Fatalf("compact control request = %d bytes, %v", len(wire), err)
	}
	fixture.stateLedger.Reset()
	fixture.fileLedger.Reset()
	capabilities, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, requests)
	if err != nil || len(capabilities) != len(requests) {
		t.Fatalf("CreateUploadBatch() = %d capabilities, %v; want %d", len(capabilities), err, len(requests))
	}
	headKey := storageformat.DomainHeadKey(storageformat.DomainNamespace, fixture.scope.UserID().String()).String()
	headPuts, pagePuts := 0, 0
	for _, event := range fixture.stateLedger.Events() {
		if event.Kind == providerbudget.RequestObjectPut && !event.Failed {
			if event.Target == headKey {
				headPuts++
			}
			if strings.Contains(event.Target, "/pages/") {
				pagePuts++
			}
		}
	}
	if headPuts != 1 || pagePuts < 2 || len(fixture.fileLedger.Events()) != len(requests) {
		t.Fatalf("admission: head puts=%d page puts=%d provider starts=%d", headPuts, pagePuts, len(fixture.fileLedger.Events()))
	}
	t.Logf("compact request=%d bytes; admission state calls=%d file calls=%d pages=%d head publications=%d", len(wire), len(fixture.stateLedger.Events()), len(fixture.fileLedger.Events()), pagePuts, headPuts)
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	for role, events := range map[string][]providerbudget.Event{"state": fixture.stateLedger.Events(), "file": fixture.fileLedger.Events()} {
		totals, err := model.Estimate(events)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("large metadata admission %s economics: %+v", role, totals)
	}
	fixture.fileLedger.Reset()
	replayed, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, requests)
	if err != nil || len(replayed) != len(capabilities) {
		t.Fatalf("replay = %d capabilities, %v", len(replayed), err)
	}
	for index := range capabilities {
		if replayed[index].UploadID != capabilities[index].UploadID || replayed[index].BatchID != capabilities[index].BatchID {
			t.Fatalf("replay changed item %d identity", index)
		}
	}
	for _, event := range fixture.fileLedger.Events() {
		if event.Kind == providerbudget.RequestUploadBegin {
			t.Fatal("replay created a new provider session")
		}
	}
	requests[0].Size++
	if _, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, requests); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("changed intent under the same keys = %v", err)
	}
}
