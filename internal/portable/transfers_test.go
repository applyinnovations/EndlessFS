package portable_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/provider/providercontract"
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
			ChecksumSHA256: true,
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
	if _, err := engine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: int64(len(content)), MediaType: "text/plain", ChecksumSHA256: "wrong"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("wrong checksum error = %v", err)
	}
	sum := sha256.Sum256(content)
	entry, err := engine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: int64(len(content)), MediaType: "text/plain", ChecksumSHA256: hex.EncodeToString(sum[:])})
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
	crasher.step = portable.StepStateAfterBackend
	completion := domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: int64(len(content)), MediaType: "text/plain"}
	if _, err := engine.Files().CompleteUpload(context.Background(), scope, completion); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("lost-success CompleteUpload() error = %v", err)
	}
	visible, err := engine.Files().Stat(context.Background(), scope, path)
	if err != nil {
		t.Fatalf("lost-success file was not visible: %v", err)
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
		portable.StepUploadCompletionAfterPrepared,
		portable.StepUploadCompletionAfterCommitted,
		portable.StepUploadCompletionAfterFinalized,
	} {
		t.Run(step, func(t *testing.T) {
			backend := objectmemory.New()
			server := httptest.NewServer(backend)
			t.Cleanup(server.Close)
			clock := domain.NewFixedClock(time.Date(2039, 4, 6+index, 6, 7, 8, 0, time.UTC))
			if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(deterministic(byte(170+index), 1<<20)))); err != nil {
				t.Fatal(err)
			}
			crasher := &stepFailure{step: step}
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
			completion := domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: int64(len(content)), MediaType: "text/plain"}
			if _, err := engine.Files().CompleteUpload(context.Background(), scope, completion); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("interrupted CompleteUpload() error = %v", err)
			}
			root, err := engine.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/"))
			if err != nil {
				t.Fatal(err)
			}
			wantBeforeRetry := int64(0)
			if step != portable.StepUploadCompletionAfterPrepared {
				wantBeforeRetry = int64(len(content))
			}
			if root.Size != wantBeforeRetry {
				t.Fatalf("interrupted root aggregate = %d; want %d", root.Size, wantBeforeRetry)
			}
			completed, err := engine.Files().CompleteUpload(context.Background(), scope, completion)
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
