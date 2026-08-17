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

func TestConfigureDataPlaneRejectsMalformedURLWithoutPanicking(t *testing.T) {
	backend := New()
	clock := domain.NewFixedClock(time.Date(2042, 2, 3, 4, 5, 6, 0, time.UTC))
	ids := domain.NewIDGenerator(bytes.NewReader(transferEntropy(1024)))

	for _, rawURL := range []string{"%", "https://127.0.0.1:8080", "http://example.test:8080", "http://127.0.0.1", "http://user@127.0.0.1:8080", "http://127.0.0.1:8080/path", "http://127.0.0.1:8080?query=1", "http://127.0.0.1:8080#fragment"} {
		t.Run(rawURL, func(t *testing.T) {
			if err := backend.ConfigureDataPlane(rawURL, clock, ids); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("ConfigureDataPlane(%q) error = %v", rawURL, err)
			}
		})
	}
	if err := backend.ConfigureDataPlane("http://127.0.0.1:8080", nil, ids); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("nil clock error = %v", err)
	}
	if err := backend.ConfigureDataPlane("http://127.0.0.1:8080", clock, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("nil IDs error = %v", err)
	}
}

func TestDirectTransferBoundaryMatrix(t *testing.T) {
	backend, server, clock := configuredTransferBackend(t)
	defer server.Close()
	key := objectstore.MustKey("endlessfs/v1/blobs/sha256/aa/body")
	expires := clock.Now().Add(time.Hour)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backend.BeginUpload(canceled, objectstore.UploadRequest{}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled BeginUpload error = %v", err)
	}
	for _, request := range []objectstore.UploadRequest{
		{},
		{UploadID: "upload", Key: key, Size: -1, MediaType: "text/plain", ExpiresAt: expires},
		{UploadID: "upload", Key: key, Size: 1, MediaType: "", ExpiresAt: expires},
		{UploadID: "upload", Key: key, Size: 1, MediaType: "text/plain", ExpiresAt: clock.Now()},
	} {
		if _, err := backend.BeginUpload(context.Background(), request); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid BeginUpload(%+v) error = %v", request, err)
		}
	}

	handle, err := backend.BeginUpload(context.Background(), objectstore.UploadRequest{UploadID: "upload", Key: key, Size: 4, MediaType: "text/plain", ExpiresAt: expires, Resumable: true})
	if err != nil {
		t.Fatal(err)
	}
	handle.Capability.Headers["Content-Type"] = "mutated"
	handle.Capability.ChunkRules.MinimumSize = 99
	replayed, err := backend.BeginUpload(context.Background(), objectstore.UploadRequest{UploadID: "upload", Key: key, Size: 4, MediaType: "text/plain", ExpiresAt: expires, Resumable: true})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Capability.Headers["Content-Type"] != "text/plain" || replayed.Capability.ChunkRules.MinimumSize != 1 {
		t.Fatalf("stored capability was aliased: %+v", replayed.Capability)
	}
	for _, conflict := range []objectstore.UploadRequest{
		{UploadID: "upload", Key: key, Size: 5, MediaType: "text/plain", ExpiresAt: expires, Resumable: true},
		{UploadID: "upload", Key: key, Size: 4, MediaType: "application/octet-stream", ExpiresAt: expires, Resumable: true},
	} {
		if _, err := backend.BeginUpload(context.Background(), conflict); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("conflicting BeginUpload error = %v", err)
		}
	}
	if _, err := backend.ResumeUpload(canceled, handle.Lease); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled ResumeUpload error = %v", err)
	}
	if _, err := backend.ResumeUpload(context.Background(), []byte("missing")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing ResumeUpload error = %v", err)
	}
	if _, err := backend.UploadProgress(canceled, handle.Lease); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled UploadProgress error = %v", err)
	}
	if _, err := backend.UploadProgress(context.Background(), []byte("missing")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing UploadProgress error = %v", err)
	}
	if err := backend.AbortUpload(canceled, handle.Lease); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled AbortUpload error = %v", err)
	}
	if err := backend.AbortUpload(context.Background(), []byte("missing")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing AbortUpload error = %v", err)
	}
	if _, err := backend.CreateDownload(canceled, objectstore.DownloadRequest{}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled CreateDownload error = %v", err)
	}
	if _, err := backend.CreateDownload(context.Background(), objectstore.DownloadRequest{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid CreateDownload error = %v", err)
	}
}

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

func TestRangeParserBoundaryMatrix(t *testing.T) {
	tests := []struct {
		header      string
		size        int64
		start, end  int64
		partial     bool
		wantInvalid bool
	}{
		{"", 0, 0, -1, false, false},
		{"bytes=0-0", 0, 0, 0, false, true},
		{"", 4, 0, 3, false, false},
		{"bytes=1-", 4, 1, 3, true, false},
		{"bytes=1-99", 4, 1, 3, true, false},
		{"items=1-2", 4, 0, 0, false, true},
		{"bytes=1-2,3-4", 4, 0, 0, false, true},
		{"bytes=", 4, 0, 0, false, true},
		{"bytes=x-2", 4, 0, 0, false, true},
		{"bytes=4-5", 4, 0, 0, false, true},
		{"bytes=2-1", 4, 0, 0, false, true},
	}
	for _, test := range tests {
		start, end, partial, err := parseRange(test.header, test.size)
		if errors.Is(err, domain.ErrInvalid) != test.wantInvalid || start != test.start || end != test.end || partial != test.partial {
			t.Fatalf("parseRange(%q, %d) = (%d,%d,%t,%v)", test.header, test.size, start, end, partial, err)
		}
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
