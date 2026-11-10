package memory

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

func TestDirectTransferHTTPBoundaryMatrix(t *testing.T) {
	backend, server, clock := configuredTransferBackend(t)
	defer server.Close()
	key := objectstore.MustKey("endlessfs/v1/blobs/sha256/bb/body")
	expires := clock.Now().Add(time.Hour)

	assertStatus := func(method, target, contentType, offset, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, target, strings.NewReader(body))
		if contentType != "" {
			request.Header.Set("Content-Type", contentType)
		}
		if offset != "" {
			request.Header.Set("Upload-Offset", offset)
		}
		response := httptest.NewRecorder()
		backend.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s %s status = %d, want %d; body=%q", method, target, response.Code, want, response.Body.String())
		}
		return response
	}

	assertStatus(http.MethodGet, "/", "", "", "", http.StatusNotFound)
	assertStatus(http.MethodGet, "/cap/unknown/token", "", "", "", http.StatusNotFound)
	assertStatus(http.MethodPut, "/cap/upload/missing", "text/plain", "", "x", http.StatusNotFound)

	single, err := backend.BeginUpload(context.Background(), objectstore.UploadRequest{UploadID: "single", Key: key, Size: 4, MediaType: "text/plain", ExpiresAt: expires})
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(http.MethodPost, single.Capability.URL, "text/plain", "", "data", http.StatusMethodNotAllowed)
	assertStatus(http.MethodPut, single.Capability.URL, "application/json", "", "data", http.StatusPreconditionFailed)
	assertStatus(http.MethodPut, single.Capability.URL, "text/plain", "", "x", http.StatusPreconditionFailed)
	assertStatus(http.MethodPut, single.Capability.URL, "text/plain", "", "12345", http.StatusRequestEntityTooLarge)
	assertStatus(http.MethodPut, single.Capability.URL, "text/plain", "", "data", http.StatusNoContent)

	resumableKey := objectstore.MustKey("endlessfs/v1/blobs/sha256/cc/body")
	resumable, err := backend.BeginUpload(context.Background(), objectstore.UploadRequest{UploadID: "resumable", Key: resumableKey, Size: 4, MediaType: "text/plain", ExpiresAt: expires, Resumable: true})
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(http.MethodPatch, resumable.Capability.URL, "text/plain", "bad", "da", http.StatusConflict)
	backend.InjectTransferFault(TransferUploadData, TransferFaultInterrupted)
	assertStatus(http.MethodPatch, resumable.Capability.URL, "text/plain", "0", "data", http.StatusServiceUnavailable)
	progress, err := backend.UploadProgress(context.Background(), resumable.Lease)
	if err != nil || progress.Offset != 2 {
		t.Fatalf("interrupted progress = %+v, %v", progress, err)
	}
	assertStatus(http.MethodPatch, resumable.Capability.URL, "text/plain", "2", "ta", http.StatusNoContent)

	info, err := backend.Head(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	download, err := backend.CreateDownload(context.Background(), objectstore.DownloadRequest{Key: key, Version: info.Version, Filename: "a.txt", MediaType: "text/plain", Disposition: domain.DispositionAttachment, ExpiresAt: expires})
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(http.MethodPost, download.URL, "", "", "", http.StatusMethodNotAllowed)
	response := assertStatus(http.MethodGet, download.URL, "", "", "", http.StatusOK)
	if response.Body.String() != "data" {
		t.Fatalf("download body = %q", response.Body.String())
	}
	rangeRequest := httptest.NewRequest(http.MethodGet, download.URL, nil)
	rangeRequest.Header.Set("Range", "bytes=1-2")
	rangeResponse := httptest.NewRecorder()
	backend.ServeHTTP(rangeResponse, rangeRequest)
	if rangeResponse.Code != http.StatusPartialContent || rangeResponse.Body.String() != "at" {
		t.Fatalf("range response = %d %q", rangeResponse.Code, rangeResponse.Body.String())
	}
	invalidRange := httptest.NewRequest(http.MethodGet, download.URL, nil)
	invalidRange.Header.Set("Range", "bytes=-1")
	invalidResponse := httptest.NewRecorder()
	backend.ServeHTTP(invalidResponse, invalidRange)
	if invalidResponse.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("invalid range status = %d", invalidResponse.Code)
	}

	clock.Advance(2 * time.Hour)
	assertStatus(http.MethodPut, single.Capability.URL, "text/plain", "", "data", http.StatusGone)
	assertStatus(http.MethodGet, download.URL, "", "", "", http.StatusGone)
	if _, err := backend.ResumeUpload(context.Background(), resumable.Lease); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired ResumeUpload error = %v", err)
	}
}

func configuredTransferBackend(t *testing.T) (*Backend, *httptest.Server, *domain.FixedClock) {
	t.Helper()
	backend := New()
	server := httptest.NewServer(backend)
	clock := domain.NewFixedClock(time.Date(2042, 2, 3, 4, 5, 6, 0, time.UTC))
	ids := domain.NewIDGenerator(bytes.NewReader(transferEntropy(1 << 20)))
	if err := backend.ConfigureDataPlane(server.URL, clock, ids); err != nil {
		server.Close()
		t.Fatal(err)
	}
	return backend, server, clock
}

func transferEntropy(size int) []byte {
	result := make([]byte, size)
	for index := range result {
		result[index] = byte(index)
	}
	return result
}
