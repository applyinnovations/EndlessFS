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
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
)

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
	if err != nil || status.ConfirmedOffset != int64(len(content)) {
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
