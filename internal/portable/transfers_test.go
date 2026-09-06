package portable_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/provider/providercontract"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestContractPortableProviderOverMemoryBackend(t *testing.T) {
	providercontract.Run(t, func(t *testing.T) providercontract.Harness {
		backend := objectmemory.New()
		server := httptest.NewServer(backend)
		t.Cleanup(server.Close)
		clock := domain.NewFixedClock(time.Date(2038, 1, 2, 3, 4, 5, 0, time.UTC))
		if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(39, 1<<20)))); err != nil {
			t.Fatal(err)
		}
		engine := openEngine(t, backend, clock, 40, nil)
		return providercontract.Harness{
			Storage: engine.Files(), Client: server.Client(), Advance: clock.Advance,
			UploadOffset: func(ctx context.Context, scope domain.Scope, uploadID domain.UploadID) (int64, error) {
				status, err := engine.Files().UploadStatus(ctx, scope, uploadID)
				return status.ConfirmedOffset, err
			},
			SimulateOffset: func(ctx context.Context, _ domain.Scope, uploadID domain.UploadID, offset int64) error {
				return backend.SimulateUploadOffset(ctx, string(uploadID), offset)
			},
			ByteCounts: func() providercontract.ByteCounts {
				counts := backend.TransferByteCounts()
				return providercontract.ByteCounts{Upload: counts.Upload, Download: counts.Download}
			},
		}
	})
}

func TestPortableDirectUploadPublishesImmutableBlobAndRangeDownload(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2039, 1, 2, 3, 4, 5, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(41, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	engine := openEngine(t, backend, clock, 42, nil)
	user, _ := domain.ParseUserID("QkJCQkJCQkJCQkJCQkJCQg")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	path := domain.MustParseUserPath("/hello.txt")
	content := []byte("hello world")
	capability, err := engine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: path, Size: int64(len(content)), MediaType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(capability.Method, capability.URL, bytes.NewReader(content))
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("upload status = %d", response.StatusCode)
	}
	status, err := engine.Files().UploadStatus(context.Background(), scope, capability.UploadID)
	if err != nil || status.State != domain.UploadStateActive || status.ConfirmedOffset != int64(len(content)) {
		t.Fatalf("UploadStatus() = %+v, %v", status, err)
	}
	backend.InjectTransferFault(objectmemory.TransferUploadData, objectmemory.TransferFaultNoFingerprint)
	if _, err := engine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: int64(len(content)), MediaType: "text/plain"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("missing provider fingerprint error = %v", err)
	}
	entry, err := engine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: int64(len(content)), MediaType: "text/plain"})
	if err != nil || entry.Version == "" {
		t.Fatalf("CompleteUpload() = %+v, %v", entry, err)
	}
	completed, err := engine.Files().UploadStatus(context.Background(), scope, capability.UploadID)
	if err != nil || completed.State != domain.UploadStateCompleted || completed.ConfirmedOffset != int64(len(content)) {
		t.Fatalf("completed UploadStatus() = %+v, %v", completed, err)
	}
	download, err := engine.Files().CreateDownload(context.Background(), scope, domain.CreateDownloadRequest{Path: path, Version: entry.Version, Disposition: domain.DispositionAttachment})
	if err != nil {
		t.Fatal(err)
	}
	downloadRequest, _ := http.NewRequest(download.Method, download.URL, nil)
	downloadRequest.Header.Set("Range", "bytes=1-3")
	downloadResponse, err := server.Client().Do(downloadRequest)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(downloadResponse.Body)
	_ = downloadResponse.Body.Close()
	if downloadResponse.StatusCode != http.StatusPartialContent || string(body) != "ell" || downloadResponse.Header.Get("Content-Range") != "bytes 1-3/11" {
		t.Fatalf("range response = %d %q %q", downloadResponse.StatusCode, body, downloadResponse.Header.Get("Content-Range"))
	}
	counts := backend.TransferByteCounts()
	if counts.Upload != int64(len(content)) || counts.Download != 3 {
		t.Fatalf("data-plane counts = %+v", counts)
	}
}

func TestPortableSeparateFileBackendIsolatesBytesAndSharesOneCheckpoint(t *testing.T) {
	stateBackend := objectmemory.New()
	fileBackend := objectmemory.New()
	server := httptest.NewServer(fileBackend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2039, 1, 2, 3, 4, 5, 0, time.UTC))
	if err := fileBackend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(141, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	writer := portable.WriterConfiguration{
		WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1",
		KeyringIdentifiers: []string{"session-v1"},
	}
	engine, err := portable.Open(context.Background(), portable.Options{
		Backend: stateBackend, FileBackend: fileBackend, Clock: clock,
		IDs: domain.NewIDGenerator(bytes.NewReader(deterministic(142, 1<<20))), Writer: writer,
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x63}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	user, _ := domain.ParseUserID("Tk5OTk5OTk5OTk5OTk5OTg")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	path := domain.MustParseUserPath("/isolated.txt")
	content := []byte("separate bytes")
	uploadPortableFile(t, server.Client(), engine.Files(), scope, path, content)
	second, err := portable.Open(context.Background(), portable.Options{
		Backend: stateBackend, FileBackend: fileBackend, Clock: clock,
		IDs: domain.NewIDGenerator(bytes.NewReader(deterministic(143, 1<<20))), Writer: writer,
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x63}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := second.Files().Stat(context.Background(), scope, path)
	if err != nil {
		t.Fatal(err)
	}
	download, err := second.Files().CreateDownload(context.Background(), scope, domain.CreateDownloadRequest{Path: path, Version: entry.Version})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(download.Method, download.URL, nil)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	downloaded, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Equal(downloaded, content) {
		t.Fatalf("download = %d %q", response.StatusCode, downloaded)
	}
	if _, err := engine.Create(context.Background(), state.MustKey(state.NamespaceAccounts, "separate-buckets"), []byte(`{"enabled":true}`)); err != nil {
		t.Fatal(err)
	}

	stateObjects := stateBackend.Export()
	fileObjects := fileBackend.Export()
	for key := range stateObjects {
		if strings.Contains(key, "/blobs/") || strings.HasPrefix(key, "endlessfs/v1/staging/") {
			t.Fatalf("file bytes leaked into state backend at %q", key)
		}
	}
	var blobKey string
	for key := range fileObjects {
		if strings.Contains(key, "/blobs/") {
			blobKey = key
		}
		if !strings.Contains(key, "/blobs/") && !strings.HasPrefix(key, "endlessfs/v1/staging/") {
			t.Fatalf("state metadata leaked into file backend at %q", key)
		}
	}
	if blobKey == "" {
		t.Fatal("completed upload did not publish a file-backend blob")
	}

	checkpoint, err := second.CreateCheckpoint(context.Background(), "separate-buckets")
	if err != nil {
		t.Fatal(err)
	}
	if !checkpointContains(t, engine, checkpoint.CheckpointID, blobKey) {
		t.Fatalf("checkpoint does not include file-backend blob %q", blobKey)
	}
	if err := portable.VerifyCheckpointReadOnlyWithFileBackend(context.Background(), stateBackend, fileBackend, writer, checkpoint.CheckpointID); err != nil {
		t.Fatalf("split checkpoint verification error = %v", err)
	}
	superblock, err := stateBackend.Get(context.Background(), storageformat.SuperblockKey())
	if err != nil {
		t.Fatal(err)
	}
	misplacedVersion, err := fileBackend.Put(context.Background(), storageformat.SuperblockKey(), superblock.Body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
	if err != nil {
		t.Fatal(err)
	}
	if err := portable.VerifyCheckpointReadOnlyWithFileBackend(context.Background(), stateBackend, fileBackend, writer, checkpoint.CheckpointID); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("split checkpoint accepted misplaced state object: %v", err)
	}
	if err := fileBackend.Delete(context.Background(), storageformat.SuperblockKey(), objectstore.DeleteCondition{Version: misplacedVersion}); err != nil {
		t.Fatal(err)
	}
	blobObject, err := fileBackend.Get(context.Background(), objectstore.MustKey(blobKey))
	if err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Delete(context.Background(), blobObject.Key, objectstore.DeleteCondition{Version: blobObject.Version}); err != nil {
		t.Fatal(err)
	}
	if err := portable.VerifyCheckpointReadOnlyWithFileBackend(context.Background(), stateBackend, fileBackend, writer, checkpoint.CheckpointID); !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("split checkpoint accepted missing file blob: %v", err)
	}
}

func checkpointContains(t *testing.T, engine *portable.Engine, checkpointID, key string) bool {
	t.Helper()
	found := false
	if err := engine.VisitCheckpointObjects(context.Background(), checkpointID, func(object storageformat.CheckpointObject) error {
		found = found || object.Key == key
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return found
}

func TestPortableUploadInitiationIsIdempotentAcrossReplicas(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2039, 1, 3, 3, 4, 5, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(90, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	first := openEngine(t, backend, clock, 91, nil)
	second := openEngine(t, backend, clock, 92, nil)
	user, _ := domain.ParseUserID("SkpKSkpKSkpKSkpKSkpKSg")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	request := domain.CreateUploadRequest{
		Path: domain.MustParseUserPath("/idempotent.bin"), Size: 4, MediaType: "application/octet-stream",
		Resumable: true, IdempotencyKey: "same-upload-initiation",
	}
	created, err := first.Files().CreateUpload(context.Background(), scope, request)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := second.Files().CreateUpload(context.Background(), scope, request)
	if err != nil || replayed.UploadID != created.UploadID || replayed.URL != created.URL || replayed.Method != created.Method {
		t.Fatalf("replayed CreateUpload() = %+v, %v; created=%+v", replayed, err, created)
	}
	request.Size++
	if _, err := second.Files().CreateUpload(context.Background(), scope, request); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("changed idempotent CreateUpload() error = %v", err)
	}
}

func TestConcurrentReplicaUploadInitiationHasOneIdempotentOutcome(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2039, 1, 4, 3, 4, 5, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(93, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	engines := []*portable.Engine{openEngine(t, backend, clock, 94, nil), openEngine(t, backend, clock, 95, nil)}
	user, _ := domain.ParseUserID("S0tLS0tLS0tLS0tLS0tLSw")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	request := domain.CreateUploadRequest{
		Path: domain.MustParseUserPath("/concurrent.bin"), Size: 8, MediaType: "application/octet-stream",
		Resumable: true, IdempotencyKey: "concurrent-upload-initiation",
	}
	start := make(chan struct{})
	results := make([]domain.UploadCapability, len(engines))
	errorsFound := make([]error, len(engines))
	var wait sync.WaitGroup
	for index, engine := range engines {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			results[index], errorsFound[index] = engine.Files().CreateUpload(context.Background(), scope, request)
		}()
	}
	close(start)
	wait.Wait()
	for index, err := range errorsFound {
		if err != nil {
			t.Fatalf("replica %d CreateUpload() error = %v", index, err)
		}
	}
	if results[0].UploadID != results[1].UploadID || results[0].URL != results[1].URL || results[0].Method != results[1].Method {
		t.Fatalf("concurrent outcomes differ: %+v and %+v", results[0], results[1])
	}
}

func TestEightReplicaDistinctUploadInitiationsSurviveSharedHeadContention(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2039, 1, 4, 4, 5, 6, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(96, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	const replicaCount = 8
	barrier := newAggregateBarrier(replicaCount)
	engines := make([]*portable.Engine, replicaCount)
	schedulers := make([]*aggregateOneShotScheduler, replicaCount)
	for index := range engines {
		schedulers[index] = &aggregateOneShotScheduler{step: portable.StepDomainBeforeHeadCommit, barrier: barrier}
		engines[index] = openEngine(t, backend, clock, byte(97+index), schedulers[index])
	}
	owner, _ := domain.ParseUserID("UFBQUFBQUFBQUFBQUFBQUA")
	scope, _ := domain.NewScope(owner, domain.AreaLive)
	// Register the upload domain before forcing every replica to race the same
	// established head, matching concurrent browser admission in production.
	if _, err := engines[0].Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{
		Path: domain.MustParseUserPath("/seed.bin"), Size: 1, MediaType: "application/octet-stream", IdempotencyKey: "distinct-upload-seed",
	}); err != nil {
		t.Fatal(err)
	}
	for _, scheduler := range schedulers {
		scheduler.Enable()
	}

	results := make([]domain.UploadCapability, replicaCount)
	errorsFound := make([]error, replicaCount)
	var wait sync.WaitGroup
	for index, engine := range engines {
		index, engine := index, engine
		wait.Add(1)
		go func() {
			defer wait.Done()
			results[index], errorsFound[index] = engine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{
				Path: domain.MustParseUserPath(fmt.Sprintf("/concurrent-%d.bin", index)), Size: int64(index + 1), MediaType: "application/octet-stream", IdempotencyKey: fmt.Sprintf("distinct-concurrent-upload-%d", index),
			})
		}()
	}
	wait.Wait()
	seen := make(map[domain.UploadID]struct{}, replicaCount)
	for index, err := range errorsFound {
		if err != nil {
			t.Fatalf("replica %d CreateUpload() error = %v", index, err)
		}
		if _, duplicate := seen[results[index].UploadID]; duplicate {
			t.Fatalf("replica %d reused upload ID %q", index, results[index].UploadID)
		}
		seen[results[index].UploadID] = struct{}{}
		status, statusErr := engines[(index+1)%replicaCount].Files().UploadStatus(context.Background(), scope, results[index].UploadID)
		if statusErr != nil || status.State != domain.UploadStateActive {
			t.Fatalf("replica %d status = %+v, %v", index, status, statusErr)
		}
	}
}

func TestPortableUploadBatchResumesEveryCrashBoundary(t *testing.T) {
	for _, step := range []string{
		portable.StepUploadBatchAfterIntents,
		portable.StepUploadBatchAfterSessions,
		portable.StepUploadBatchAfterActivation,
	} {
		t.Run(step, func(t *testing.T) {
			backend := objectmemory.New()
			server := httptest.NewServer(backend)
			t.Cleanup(server.Close)
			clock := domain.NewFixedClock(time.Date(2039, 1, 5, 3, 4, 5, 0, time.UTC))
			if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(96, 1<<20)))); err != nil {
				t.Fatal(err)
			}
			crasher := &stepFailure{step: step}
			first := openEngine(t, backend, clock, 97, crasher)
			owner, _ := domain.ParseUserID("TE1NTE1NTE1NTE1NTE1NTQ")
			scope, _ := domain.NewScope(owner, domain.AreaLive)
			requests := []domain.CreateUploadRequest{
				{Path: domain.MustParseUserPath("/batch-a.bin"), Size: 1, MediaType: "application/octet-stream", IdempotencyKey: "batch-crash-item-a"},
				{Path: domain.MustParseUserPath("/batch-b.bin"), Size: 2, MediaType: "application/octet-stream", IdempotencyKey: "batch-crash-item-b"},
				{Path: domain.MustParseUserPath("/batch-c.bin"), Size: 3, MediaType: "application/octet-stream", IdempotencyKey: "batch-crash-item-c"},
			}
			if _, err := first.Files().CreateUploadBatch(context.Background(), scope, requests); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("crashed CreateUploadBatch() error = %v", err)
			}
			restarted := openEngine(t, backend, clock, 98, nil)
			capabilities, err := restarted.Files().CreateUploadBatch(context.Background(), scope, requests)
			if err != nil || len(capabilities) != len(requests) {
				t.Fatalf("resumed CreateUploadBatch() = %d capabilities, %v", len(capabilities), err)
			}
			for index, capability := range capabilities {
				status, statusErr := restarted.Files().UploadStatus(context.Background(), scope, capability.UploadID)
				if statusErr != nil || status.State != domain.UploadStateActive || status.Path != requests[index].Path {
					t.Fatalf("resumed upload %d status = %+v, %v", index, status, statusErr)
				}
			}
		})
	}
}

func uploadCapabilityBody(t *testing.T, client *http.Client, capability domain.UploadCapability, body []byte) {
	t.Helper()
	request, err := http.NewRequest(capability.Method, capability.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("upload status = %d", response.StatusCode)
	}
}

func TestPortableUploadBatchCompletionIsAtomicReplayableAndChecksumBound(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2039, 1, 5, 5, 6, 7, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(151, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	crasher := &stepFailure{step: portable.StepUploadBatchCompletionVerified}
	engine := openEngine(t, backend, clock, 152, crasher)
	owner, _ := domain.ParseUserID("VVVVVVVVVVVVVVVVVVVVVQ")
	scope, _ := domain.NewScope(owner, domain.AreaLive)
	requests := []domain.CreateUploadRequest{
		{Path: domain.MustParseUserPath("/atomic-a.bin"), Size: 3, MediaType: "application/octet-stream", IdempotencyKey: "atomic-completion-item-a"},
		{Path: domain.MustParseUserPath("/atomic-b.bin"), Size: 4, MediaType: "application/octet-stream", IdempotencyKey: "atomic-completion-item-b"},
	}
	capabilities, err := engine.Files().CreateUploadBatch(context.Background(), scope, requests)
	if err != nil {
		t.Fatal(err)
	}
	bodies := [][]byte{[]byte("one"), []byte("four")}
	items := make([]domain.CompleteUploadBatchItem, len(capabilities))
	for index, capability := range capabilities {
		uploadCapabilityBody(t, server.Client(), capability, bodies[index])
		items[index] = domain.CompleteUploadBatchItem{UploadID: capability.UploadID, CRC32C: objectstore.FingerprintFor(bodies[index]).CRC32C}
	}
	completion := domain.CompleteUploadBatchRequest{Items: items, IdempotencyKey: "atomic-upload-completion-batch"}
	if _, err := engine.Files().CompleteUploadBatch(context.Background(), scope, completion); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("crashed completion error = %v", err)
	}
	for _, request := range requests {
		if _, err := engine.Files().Stat(context.Background(), scope, request.Path); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("partially published %s: %v", request.Path, err)
		}
	}
	restarted := openEngine(t, backend, clock, 153, nil)
	result, err := restarted.Files().CompleteUploadBatch(context.Background(), scope, completion)
	if err != nil || len(result.Entries) != len(requests) {
		t.Fatalf("resumed completion = %+v, %v", result, err)
	}
	replayed, err := restarted.Files().CompleteUploadBatch(context.Background(), scope, completion)
	if err != nil || len(replayed.Entries) != len(requests) {
		t.Fatalf("replayed completion = %+v, %v", replayed, err)
	}
	for index, request := range requests {
		entry, statErr := restarted.Files().Stat(context.Background(), scope, request.Path)
		if statErr != nil || entry.Version != result.Entries[index].Version || replayed.Entries[index] != result.Entries[index] {
			t.Fatalf("entry %d = %+v, replay=%+v, stat=%+v, %v", index, result.Entries[index], replayed.Entries[index], entry, statErr)
		}
	}
	changed := completion
	changed.Items = append([]domain.CompleteUploadBatchItem(nil), completion.Items...)
	changed.Items[0].CRC32C = objectstore.FingerprintFor([]byte("bad")).CRC32C
	if _, err := restarted.Files().CompleteUploadBatch(context.Background(), scope, changed); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("changed replay error = %v", err)
	}
}

func TestPortableUploadBatchAbortIsAtomicAndReplayable(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2039, 1, 5, 6, 7, 8, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(154, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	crasher := &stepFailure{step: portable.StepUploadBatchAbortApplied}
	engine := openEngine(t, backend, clock, 155, crasher)
	owner, _ := domain.ParseUserID("VlZWVlZWVlZWVlZWVlZWVg")
	scope, _ := domain.NewScope(owner, domain.AreaLive)
	requests := []domain.CreateUploadRequest{
		{Path: domain.MustParseUserPath("/abort-a.bin"), Size: 3, MediaType: "application/octet-stream", IdempotencyKey: "atomic-abort-item-a"},
		{Path: domain.MustParseUserPath("/abort-b.bin"), Size: 4, MediaType: "application/octet-stream", IdempotencyKey: "atomic-abort-item-b"},
	}
	capabilities, err := engine.Files().CreateUploadBatch(context.Background(), scope, requests)
	if err != nil {
		t.Fatal(err)
	}
	uploadIDs := []domain.UploadID{capabilities[0].UploadID, capabilities[1].UploadID}
	abort := domain.AbortUploadBatchRequest{UploadIDs: uploadIDs, BatchID: capabilities[0].BatchID, IdempotencyKey: "atomic-upload-abort-batch"}
	if err := engine.Files().AbortUploadBatch(context.Background(), scope, abort); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("crashed abort error = %v", err)
	}
	restarted := openEngine(t, backend, clock, 156, nil)
	if err := restarted.Files().AbortUploadBatch(context.Background(), scope, abort); err != nil {
		t.Fatalf("resumed abort error = %v", err)
	}
	if err := restarted.Files().AbortUploadBatch(context.Background(), scope, abort); err != nil {
		t.Fatalf("replayed abort error = %v", err)
	}
	changed := abort
	changed.UploadIDs = []domain.UploadID{uploadIDs[1], uploadIDs[0]}
	if err := restarted.Files().AbortUploadBatch(context.Background(), scope, changed); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("changed abort replay error = %v", err)
	}
	for _, uploadID := range uploadIDs {
		status, statusErr := restarted.Files().UploadStatus(context.Background(), scope, uploadID)
		if statusErr != nil || status.State != domain.UploadStateAborted {
			t.Fatalf("aborted status = %+v, %v", status, statusErr)
		}
	}
}

func TestPortablePartialBatchAbortRetainsLegacyPerRecordCompatibility(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2039, 1, 5, 6, 8, 9, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(163, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	engine := openEngine(t, backend, clock, 164, nil)
	owner, _ := domain.ParseUserID("WVlZWVlZWVlZWVlZWVlZWQ")
	scope, _ := domain.NewScope(owner, domain.AreaLive)
	capabilities, err := engine.Files().CreateUploadBatch(context.Background(), scope, []domain.CreateUploadRequest{
		{Path: domain.MustParseUserPath("/partial-a.bin"), Size: 1, MediaType: "application/octet-stream", IdempotencyKey: "partial-a"},
		{Path: domain.MustParseUserPath("/partial-b.bin"), Size: 1, MediaType: "application/octet-stream", IdempotencyKey: "partial-b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := domain.AbortUploadBatchRequest{UploadIDs: []domain.UploadID{capabilities[0].UploadID}, IdempotencyKey: "partial-abort"}
	if err := engine.Files().AbortUploadBatch(context.Background(), scope, request); err != nil {
		t.Fatal(err)
	}
	first, err := engine.Files().UploadStatus(context.Background(), scope, capabilities[0].UploadID)
	if err != nil || first.State != domain.UploadStateAborted {
		t.Fatalf("partial aborted member = %+v, %v", first, err)
	}
	second, err := engine.Files().UploadStatus(context.Background(), scope, capabilities[1].UploadID)
	if err != nil || second.State != domain.UploadStateActive {
		t.Fatalf("partial retained member = %+v, %v", second, err)
	}
}

func TestConcurrentCompletionAndCompactBatchAbortHaveOneAtomicWinner(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2039, 1, 5, 7, 8, 9, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(157, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	first := openEngine(t, backend, clock, 158, nil)
	second := openEngine(t, backend, clock, 159, nil)
	owner, _ := domain.ParseUserID("V1dXV1dXV1dXV1dXV1dXVw")
	scope, _ := domain.NewScope(owner, domain.AreaLive)
	requests := []domain.CreateUploadRequest{
		{Path: domain.MustParseUserPath("/race-a.bin"), Size: 3, MediaType: "application/octet-stream", IdempotencyKey: "race-item-a"},
		{Path: domain.MustParseUserPath("/race-b.bin"), Size: 4, MediaType: "application/octet-stream", IdempotencyKey: "race-item-b"},
	}
	capabilities, err := first.Files().CreateUploadBatch(context.Background(), scope, requests)
	if err != nil {
		t.Fatal(err)
	}
	bodies := [][]byte{[]byte("one"), []byte("four")}
	completion := domain.CompleteUploadBatchRequest{Items: make([]domain.CompleteUploadBatchItem, len(capabilities)), IdempotencyKey: "race-completion"}
	abort := domain.AbortUploadBatchRequest{UploadIDs: make([]domain.UploadID, len(capabilities)), BatchID: capabilities[0].BatchID, IdempotencyKey: "race-abort"}
	for index, capability := range capabilities {
		uploadCapabilityBody(t, server.Client(), capability, bodies[index])
		completion.Items[index] = domain.CompleteUploadBatchItem{UploadID: capability.UploadID, CRC32C: objectstore.FingerprintFor(bodies[index]).CRC32C}
		abort.UploadIDs[index] = capability.UploadID
	}
	start := make(chan struct{})
	var completed domain.CompleteUploadBatchResult
	var completeErr, abortErr error
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		completed, completeErr = first.Files().CompleteUploadBatch(context.Background(), scope, completion)
	}()
	go func() {
		defer wait.Done()
		<-start
		abortErr = second.Files().AbortUploadBatch(context.Background(), scope, abort)
	}()
	close(start)
	wait.Wait()
	if (completeErr == nil) == (abortErr == nil) {
		t.Fatalf("completion/abort winners = complete:%v abort:%v", completeErr, abortErr)
	}
	if completeErr == nil {
		if len(completed.Entries) != len(requests) || !errors.Is(abortErr, domain.ErrConflict) {
			t.Fatalf("completion winner = %+v; abort error=%v", completed, abortErr)
		}
	} else if !errors.Is(completeErr, domain.ErrConflict) && !errors.Is(completeErr, domain.ErrNotFound) && !errors.Is(completeErr, domain.ErrPreconditionFailed) {
		t.Fatalf("completion loser error = %v", completeErr)
	}
	for index, capability := range capabilities {
		status, statusErr := second.Files().UploadStatus(context.Background(), scope, capability.UploadID)
		if statusErr != nil {
			t.Fatal(statusErr)
		}
		entry, statErr := second.Files().Stat(context.Background(), scope, requests[index].Path)
		if completeErr == nil {
			if status.State != domain.UploadStateCompleted || statErr != nil || entry.Version != completed.Entries[index].Version {
				t.Fatalf("completed member %d = status:%+v entry:%+v error:%v", index, status, entry, statErr)
			}
		} else if status.State != domain.UploadStateAborted || !errors.Is(statErr, domain.ErrNotFound) {
			t.Fatalf("aborted member %d = status:%+v stat error:%v", index, status, statErr)
		}
	}
}

func TestUploadBatchAbortProgressRestartBoundsRepeatedProviderWork(t *testing.T) {
	base := objectmemory.New()
	server := httptest.NewServer(base)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2039, 1, 5, 8, 9, 10, 0, time.UTC))
	if err := base.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(160, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	ledger := providerbudget.NewLedger()
	backend := budgettest.Wrap(providerbudget.RoleFile, base, ledger)
	crasher := &stepFailure{step: portable.StepUploadBatchAbortProgress}
	engine := openEngine(t, backend, clock, 161, crasher)
	owner, _ := domain.ParseUserID("WFhYWFhYWFhYWFhYWFhYWA")
	scope, _ := domain.NewScope(owner, domain.AreaLive)
	requests := make([]domain.CreateUploadRequest, 1001)
	for index := range requests {
		requests[index] = domain.CreateUploadRequest{
			Path: domain.MustParseUserPath(fmt.Sprintf("/restart-abort-%04d.bin", index)), Size: 0,
			MediaType: "application/octet-stream", IdempotencyKey: fmt.Sprintf("restart-abort-item-%04d", index),
		}
	}
	capabilities, err := engine.Files().CreateUploadBatch(context.Background(), scope, requests)
	if err != nil {
		t.Fatal(err)
	}
	abort := domain.AbortUploadBatchRequest{UploadIDs: make([]domain.UploadID, len(capabilities)), BatchID: capabilities[0].BatchID, IdempotencyKey: "restart-abort-batch"}
	for index, capability := range capabilities {
		abort.UploadIDs[index] = capability.UploadID
	}
	ledger.Reset()
	if err := engine.Files().AbortUploadBatch(context.Background(), scope, abort); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("progress-boundary crash error = %v", err)
	}
	restarted := openEngine(t, backend, clock, 162, nil)
	if err := restarted.Files().AbortUploadBatch(context.Background(), scope, abort); err != nil {
		t.Fatalf("resumed abort error = %v", err)
	}
	aborts := 0
	for _, event := range ledger.Events() {
		if event.Kind == providerbudget.RequestUploadAbort {
			aborts++
		}
	}
	if repeated := aborts - len(capabilities); repeated < 0 || repeated > storageformat.UploadTransactionSegmentItems {
		t.Fatalf("abort restart repeated %d provider calls; total=%d logical=%d bound=%d", repeated, aborts, len(capabilities), storageformat.UploadTransactionSegmentItems)
	}
	for _, capability := range []domain.UploadCapability{capabilities[0], capabilities[len(capabilities)/2], capabilities[len(capabilities)-1]} {
		status, statusErr := restarted.Files().UploadStatus(context.Background(), scope, capability.UploadID)
		if statusErr != nil || status.State != domain.UploadStateAborted {
			t.Fatalf("resumed abort status = %+v, %v", status, statusErr)
		}
	}
}

func TestConcurrentReplicaUploadBatchHasOneIdempotentOutcome(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2039, 1, 6, 3, 4, 5, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(99, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	barrier := &concurrentBeginBackend{Backend: backend, ready: make(chan struct{}), want: 4}
	engines := []*portable.Engine{openEngine(t, barrier, clock, 100, nil), openEngine(t, barrier, clock, 101, nil)}
	owner, _ := domain.ParseUserID("TU5PTU5PTU5PTU5PTU5PTw")
	scope, _ := domain.NewScope(owner, domain.AreaLive)
	requests := []domain.CreateUploadRequest{
		{Path: domain.MustParseUserPath("/concurrent-a.bin"), Size: 1, MediaType: "application/octet-stream", IdempotencyKey: "concurrent-batch-a"},
		{Path: domain.MustParseUserPath("/concurrent-b.bin"), Size: 1, MediaType: "application/octet-stream", IdempotencyKey: "concurrent-batch-b"},
	}
	start := make(chan struct{})
	results := make([][]domain.UploadCapability, len(engines))
	errorsFound := make([]error, len(engines))
	var wait sync.WaitGroup
	for index, engine := range engines {
		index, engine := index, engine
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			results[index], errorsFound[index] = engine.Files().CreateUploadBatch(context.Background(), scope, requests)
		}()
	}
	close(start)
	wait.Wait()
	for index := range results {
		if errorsFound[index] != nil || len(results[index]) != len(requests) {
			t.Fatalf("replica %d batch = %+v, %v", index, results[index], errorsFound[index])
		}
	}
	for index := range requests {
		if results[0][index].UploadID != results[1][index].UploadID || results[0][index].URL != results[1][index].URL {
			t.Fatalf("batch item %d outcomes differ: %+v / %+v", index, results[0][index], results[1][index])
		}
	}
}

// concurrentBeginBackend makes both replicas finish each provider initiation
// before either can publish its lease. The memory provider intentionally
// returns one idempotent native session per upload ID, reproducing the exact
// race where a losing lease CAS must not abort the winner's identical lease.
type concurrentBeginBackend struct {
	*objectmemory.Backend
	mu    sync.Mutex
	calls int
	want  int
	ready chan struct{}
}

func (backend *concurrentBeginBackend) BeginUpload(ctx context.Context, request objectstore.UploadRequest) (objectstore.UploadHandle, error) {
	handle, err := backend.Backend.BeginUpload(ctx, request)
	if err != nil {
		return objectstore.UploadHandle{}, err
	}
	backend.mu.Lock()
	backend.calls++
	if backend.calls == backend.want {
		close(backend.ready)
	}
	ready := backend.ready
	backend.mu.Unlock()
	select {
	case <-ctx.Done():
		return objectstore.UploadHandle{}, domain.NewError(domain.ErrorUnavailable, "concurrent upload initiation canceled")
	case <-ready:
		return handle, nil
	}
}

func TestPortableResumableUploadAbortExpiryAndLargeLogicalObject(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2039, 2, 3, 4, 5, 6, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(43, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	engine := openEngine(t, backend, clock, 44, nil)
	user, _ := domain.ParseUserID("Q0NDQ0NDQ0NDQ0NDQ0NDQw")
	scope, _ := domain.NewScope(user, domain.AreaTrash)
	path := domain.MustParseUserPath("/huge.bin")
	size := int64(1<<40) + 17
	capability, err := engine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: path, Size: size, MediaType: "application/octet-stream", Resumable: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.SimulateUploadOffset(context.Background(), string(capability.UploadID), size); err != nil {
		t.Fatal(err)
	}
	entry, err := engine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: size, MediaType: "application/octet-stream"})
	if err != nil || entry.Size != size {
		t.Fatalf("large CompleteUpload() = %+v, %v", entry, err)
	}
	download, err := engine.Files().CreateDownload(context.Background(), scope, domain.CreateDownloadRequest{Path: path, Version: entry.Version})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(download.Method, download.URL, nil)
	request.Header.Set("Range", "bytes="+strconv.FormatInt(size-4, 10)+"-")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusPartialContent || !bytes.Equal(body, []byte{0, 0, 0, 0}) {
		t.Fatalf("large range = %d %v", response.StatusCode, body)
	}
	abortPath := domain.MustParseUserPath("/abort.bin")
	abortCapability, err := engine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: abortPath, Size: 1, MediaType: "application/octet-stream"})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Files().AbortUpload(context.Background(), scope, abortCapability.UploadID); err != nil {
		t.Fatal(err)
	}
	abortRequest, _ := http.NewRequest(abortCapability.Method, abortCapability.URL, bytes.NewReader([]byte("x")))
	for name, value := range abortCapability.Headers {
		abortRequest.Header.Set(name, value)
	}
	abortResponse, err := server.Client().Do(abortRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = abortResponse.Body.Close()
	if abortResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("aborted upload status = %d", abortResponse.StatusCode)
	}
}

func TestCheckpointWaitsForActiveCapabilityThenDrainsItAfterExpiry(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2039, 3, 4, 5, 6, 7, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(45, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	engine := openEngine(t, backend, clock, 46, nil)
	user, _ := domain.ParseUserID("RERERERERERERERERERERA")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	capability, err := engine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/pending.bin"), Size: 1, MediaType: "application/octet-stream"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CreateCheckpoint(context.Background(), "active-upload"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("checkpoint with active upload error = %v", err)
	}
	clock.Advance(11 * time.Minute)
	expired, err := engine.Files().UploadStatus(context.Background(), scope, capability.UploadID)
	if err != nil || expired.State != domain.UploadStateExpired || expired.ConfirmedOffset != 0 {
		t.Fatalf("expired UploadStatus() = %+v, %v", expired, err)
	}
	if _, err := engine.CreateCheckpoint(context.Background(), "active-upload"); err != nil {
		t.Fatalf("checkpoint after expiry error = %v", err)
	}
	request, _ := http.NewRequest(capability.Method, capability.URL, bytes.NewReader([]byte("x")))
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("drained capability status = %d", response.StatusCode)
	}
}

func TestUploadCompletionLostSuccessIsIdempotentlyReconciled(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2039, 4, 5, 6, 7, 8, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(47, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	crasher := &stepFailure{}
	engine := openEngine(t, backend, clock, 48, crasher)
	user, _ := domain.ParseUserID("RUVFRUVFRUVFRUVFRUVFRQ")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	path := domain.MustParseUserPath("/lost-success.txt")
	content := []byte("durable")
	capability, err := engine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: path, Size: int64(len(content)), MediaType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(capability.Method, capability.URL, bytes.NewReader(content))
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	crasher.step = portable.StepDomainAfterHeadCommit
	completion := domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: int64(len(content)), MediaType: "text/plain"}
	completed, err := engine.Files().CompleteUpload(context.Background(), scope, completion)
	if err != nil {
		t.Fatalf("lost-success was not reconciled in the original CompleteUpload() call: %v", err)
	}
	visible, err := engine.Files().Stat(context.Background(), scope, path)
	if err != nil {
		t.Fatalf("lost-success file was not visible: %v", err)
	}
	if completed.Version != visible.Version {
		t.Fatalf("same-call recovery = %+v; visible=%+v", completed, visible)
	}
	reconciled, err := engine.Files().CompleteUpload(context.Background(), scope, completion)
	if err != nil || reconciled.Version != visible.Version {
		t.Fatalf("reconciled CompleteUpload() = %+v, %v; visible=%+v", reconciled, err, visible)
	}
	replayed, err := engine.Files().CompleteUpload(context.Background(), scope, completion)
	if err != nil || replayed.Version != visible.Version {
		t.Fatalf("replayed CompleteUpload() = %+v, %v", replayed, err)
	}
}

func TestUploadCompletionRecoversAtEveryAggregateCommitBoundary(t *testing.T) {
	for index, step := range []string{
		portable.StepDomainBeforeHeadCommit,
		portable.StepDomainAfterHeadCommit,
	} {
		t.Run(step, func(t *testing.T) {
			backend := objectmemory.New()
			server := httptest.NewServer(backend)
			t.Cleanup(server.Close)
			clock := domain.NewFixedClock(time.Date(2039, 4, 6+index, 6, 7, 8, 0, time.UTC))
			if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(byte(170+index), 1<<20)))); err != nil {
				t.Fatal(err)
			}
			crasher := &stepFailure{}
			engine := openEngine(t, backend, clock, byte(180+index), crasher)
			user, _ := domain.ParseUserID("RkdHRkdHRkdHRkdHRkdHRw")
			scope, _ := domain.NewScope(user, domain.AreaLive)
			path := domain.MustParseUserPath("/aggregate.txt")
			content := []byte("durable aggregate")
			capability, err := engine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: path, Size: int64(len(content)), MediaType: "text/plain"})
			if err != nil {
				t.Fatal(err)
			}
			request, _ := http.NewRequest(capability.Method, capability.URL, bytes.NewReader(content))
			for name, value := range capability.Headers {
				request.Header.Set(name, value)
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			crasher.step = step
			completion := domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: int64(len(content)), MediaType: "text/plain"}
			completed, completionErr := engine.Files().CompleteUpload(context.Background(), scope, completion)
			root, err := engine.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/"))
			if err != nil {
				t.Fatal(err)
			}
			wantBeforeRetry := int64(0)
			if step == portable.StepDomainBeforeHeadCommit {
				if !errors.Is(completionErr, domain.ErrUnavailable) {
					t.Fatalf("pre-publication interruption error = %v", completionErr)
				}
			} else {
				wantBeforeRetry = int64(len(content))
				if completionErr != nil || completed.Size != wantBeforeRetry {
					t.Fatalf("post-publication lost response was not recovered in the same call: %+v, %v", completed, completionErr)
				}
			}
			if root.Size != wantBeforeRetry {
				t.Fatalf("interrupted root aggregate = %d; want %d", root.Size, wantBeforeRetry)
			}
			completed, err = engine.Files().CompleteUpload(context.Background(), scope, completion)
			if err != nil || completed.Size != int64(len(content)) {
				t.Fatalf("retried CompleteUpload() = %+v, %v", completed, err)
			}
			root, err = engine.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/"))
			if err != nil || root.Size != int64(len(content)) {
				t.Fatalf("recovered root aggregate = %+v, %v", root, err)
			}
		})
	}
}

func TestNamespaceMutationLostSuccessDoesNotLeaveARecoveryLease(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2039, 4, 10, 6, 7, 8, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(190, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	crasher := &stepFailure{}
	engine := openEngine(t, backend, clock, 191, crasher)
	user, _ := domain.ParseUserID("SEhISEhISEhISEhISEhISA")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	uploadPortableFile(t, server.Client(), engine.Files(), scope, domain.MustParseUserPath("/before.txt"), []byte("before"))

	crasher.step = portable.StepDomainAfterHeadCommit
	moved, err := engine.Files().Move(context.Background(), scope, scope, domain.MoveRequest{
		Source: domain.MustParseUserPath("/before.txt"), Destination: domain.MustParseUserPath("/after.txt"),
	})
	if err != nil || moved.State != domain.OperationSucceeded {
		t.Fatalf("lost-success Move() = %+v, %v", moved, err)
	}
	if _, err := engine.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/after.txt")); err != nil {
		t.Fatalf("committed move is not visible: %v", err)
	}
	if _, err := engine.Files().Delete(context.Background(), scope, domain.DeleteRequest{Path: domain.MustParseUserPath("/after.txt")}); err != nil {
		t.Fatalf("a committed namespace mutation left a blocking recovery lease: %v", err)
	}

	path := domain.MustParseUserPath("/photo.jpg")
	content := []byte("photo bytes")
	capability, err := engine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{
		Path: path, Size: int64(len(content)), MediaType: "image/jpeg",
	})
	if err != nil {
		t.Fatalf("CreateUpload() after committed namespace transition = %v", err)
	}
	request, _ := http.NewRequest(capability.Method, capability.URL, bytes.NewReader(content))
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("upload status = %d", response.StatusCode)
	}

	completed, err := engine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{
		UploadID: capability.UploadID, Path: path, Size: int64(len(content)), MediaType: "image/jpeg",
	})
	if err != nil || completed.Path != path || completed.Size != int64(len(content)) {
		t.Fatalf("CompleteUpload() after committed namespace transition = %+v, %v", completed, err)
	}
}
