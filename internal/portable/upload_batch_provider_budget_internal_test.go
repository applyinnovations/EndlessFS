package portable

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

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

type uploadBatchScaleFixture struct {
	engine      *Engine
	server      *httptest.Server
	stateLedger *providerbudget.Ledger
	fileLedger  *providerbudget.Ledger
	scope       domain.Scope
}

func newUploadBatchScaleFixture(t *testing.T, seed uint64) uploadBatchScaleFixture {
	t.Helper()
	clock := domain.NewFixedClock(time.Date(2068, 1, 2, 3, 4, 5, 0, time.UTC))
	stateBase, fileBase := objectmemory.New(), objectmemory.New()
	server := httptest.NewServer(fileBase)
	t.Cleanup(server.Close)
	if err := fileBase.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(&deterministicScaleReader{state: seed ^ 0xa5a5a5a5})); err != nil {
		t.Fatal(err)
	}
	stateLedger, fileLedger := providerbudget.NewLedger(), providerbudget.NewLedger()
	stateBackend := budgettest.Wrap(providerbudget.RoleState, stateBase, stateLedger)
	fileBackend := budgettest.Wrap(providerbudget.RoleFile, fileBase, fileLedger)
	engine, err := Open(context.Background(), Options{
		Backend: stateBackend, FileBackend: fileBackend, Clock: clock,
		IDs:      domain.NewIDGenerator(&deterministicScaleReader{state: seed}),
		Writer:   WriterConfiguration{WriterSetID: "upload-scale", ConfigurationDigest: "upload-scale-v1", KeyringIdentifiers: []string{"key"}},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x64}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := domain.ParseUserID("U0NBTEVTQ0FMRVNDQUxFU0NBTA")
	if err != nil {
		t.Fatal(err)
	}
	scope, err := domain.NewScope(owner, domain.AreaLive)
	if err != nil {
		t.Fatal(err)
	}
	// Account/namespace registration is a one-time lifecycle cost, not part of
	// the steady-state transfer workflow. Establish the mature owner snapshot
	// before measuring the per-batch provider budget.
	if err := engine.stateDomainStore().ensureRegistered(context.Background(), uploadDomainReference(owner)); err != nil {
		t.Fatal(err)
	}
	stateLedger.Reset()
	fileLedger.Reset()
	return uploadBatchScaleFixture{engine: engine, server: server, stateLedger: stateLedger, fileLedger: fileLedger, scope: scope}
}

func scaleUploadRequests(prefix string) []domain.CreateUploadRequest {
	requests := make([]domain.CreateUploadRequest, 10_000)
	for index := range requests {
		requests[index] = domain.CreateUploadRequest{
			Path: domain.MustParseUserPath(fmt.Sprintf("/%s-%05d.bin", prefix, index)), Size: 0,
			MediaType: "application/octet-stream", Resumable: true,
			IdempotencyKey: fmt.Sprintf("%s-upload-item-%05d", prefix, index),
		}
	}
	return requests
}

func assertTransferScaleShape(t *testing.T, operation string, state, file []providerbudget.Event, fileKind providerbudget.RequestKind) {
	t.Helper()
	for index, event := range state {
		t.Logf("%s state event %d: %s %s %s", operation, index+1, event.Kind, event.Subsystem, event.Target)
	}
	if len(state) > 13 {
		t.Fatalf("%s state requests = %d, want at most 13 before authentication", operation, len(state))
	}
	if len(file) != 10_000 {
		t.Fatalf("%s file requests = %d, want 10000", operation, len(file))
	}
	for _, event := range file {
		if event.Kind != fileKind {
			t.Fatalf("%s unexpected file-provider event: %+v", operation, event)
		}
	}
	for _, event := range state {
		if event.Kind == providerbudget.RequestObjectOpen || event.Kind == providerbudget.RequestDataDownload {
			t.Fatalf("%s read object bytes through the control plane: %+v", operation, event)
		}
	}
	t.Logf("provider-scale-observed operation=%s state=%d file=%d total=%d", operation, len(state), len(file), len(state)+len(file))
}

func checkTransferScaleBudget(t *testing.T, name string, state, file []providerbudget.Event) {
	t.Helper()
	economics, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	ratchet, err := gcs.RegionalStandardFlatBudgetRatchet()
	if err != nil {
		t.Fatal(err)
	}
	events := append(append([]providerbudget.Event(nil), state...), file...)
	if report, err := ratchet.CheckExact(name, economics, []providerbudget.Role{providerbudget.RoleState, providerbudget.RoleFile}, events); err != nil {
		t.Errorf("%s: %v; observed=%+v", name, err, report.Totals)
	}
}

func uploadEmptyCapabilities(t *testing.T, client *http.Client, capabilities []domain.UploadCapability) {
	t.Helper()
	jobs := make(chan domain.UploadCapability)
	errorsFound := make(chan error, 100)
	var wait sync.WaitGroup
	for range 100 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for capability := range jobs {
				request, err := http.NewRequest(capability.Method, capability.URL, nil)
				if err == nil {
					for name, value := range capability.Headers {
						request.Header.Set(name, value)
					}
					var response *http.Response
					response, err = client.Do(request)
					if err == nil {
						_ = response.Body.Close()
						if response.StatusCode != http.StatusNoContent {
							err = fmt.Errorf("upload status %d", response.StatusCode)
						}
					}
				}
				if err != nil {
					select {
					case errorsFound <- err:
					default:
					}
				}
			}
		}()
	}
	for _, capability := range capabilities {
		jobs <- capability
	}
	close(jobs)
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
}

func TestProviderBudgetUploadBatchTenThousandLifecycle(t *testing.T) {
	ctx := context.Background()
	fixture := newUploadBatchScaleFixture(t, 0x81726354)
	capabilities, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, scaleUploadRequests("complete"))
	if err != nil || len(capabilities) != 10_000 {
		t.Fatalf("CreateUploadBatch() = %d capabilities, %v", len(capabilities), err)
	}
	assertTransferScaleShape(t, "upload-admission", fixture.stateLedger.Events(), fixture.fileLedger.Events(), providerbudget.RequestUploadBegin)
	checkTransferScaleBudget(t, "file-create-upload-batch-10000-schema-011", fixture.stateLedger.Events(), fixture.fileLedger.Events())

	uploadEmptyCapabilities(t, fixture.server.Client(), capabilities)
	items := make([]domain.CompleteUploadBatchItem, len(capabilities))
	for index, capability := range capabilities {
		items[index] = domain.CompleteUploadBatchItem{UploadID: capability.UploadID, CRC32C: "AAAAAA"}
	}
	fixture.stateLedger.Reset()
	fixture.fileLedger.Reset()
	result, err := fixture.engine.Files().CompleteUploadBatch(ctx, fixture.scope, domain.CompleteUploadBatchRequest{Items: items, IdempotencyKey: "complete-ten-thousand-uploads"})
	if err != nil || len(result.Entries) != 10_000 {
		t.Fatalf("CompleteUploadBatch() = %d entries, %v", len(result.Entries), err)
	}
	assertTransferScaleShape(t, "upload-completion", fixture.stateLedger.Events(), fixture.fileLedger.Events(), providerbudget.RequestObjectVerify)
	checkTransferScaleBudget(t, "file-complete-upload-batch-10000-schema-011", fixture.stateLedger.Events(), fixture.fileLedger.Events())

	fixture = newUploadBatchScaleFixture(t, 0x19283746)
	capabilities, err = fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, scaleUploadRequests("abort"))
	if err != nil || len(capabilities) != 10_000 {
		t.Fatalf("abort CreateUploadBatch() = %d capabilities, %v", len(capabilities), err)
	}
	uploadIDs := make([]domain.UploadID, len(capabilities))
	for index, capability := range capabilities {
		uploadIDs[index] = capability.UploadID
	}
	fixture.stateLedger.Reset()
	fixture.fileLedger.Reset()
	if err := fixture.engine.Files().AbortUploadBatch(ctx, fixture.scope, domain.AbortUploadBatchRequest{UploadIDs: uploadIDs, BatchID: capabilities[0].BatchID, IdempotencyKey: "abort-ten-thousand-uploads"}); err != nil {
		t.Fatal(err)
	}
	assertTransferScaleShape(t, "upload-cancellation", fixture.stateLedger.Events(), fixture.fileLedger.Events(), providerbudget.RequestUploadAbort)
	checkTransferScaleBudget(t, "file-abort-upload-batch-10000-schema-011", fixture.stateLedger.Events(), fixture.fileLedger.Events())
}
