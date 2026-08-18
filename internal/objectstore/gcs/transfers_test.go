package gcs_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	gcstransport "github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
)

func TestGCSResumableCapabilityCanMoveBetweenReplicas(t *testing.T) {
	server, _ := newGCSServerWithFake(t)
	client := protocolClient(t, server)
	options := gcstransport.TransferOptions{
		HTTPClient: server.Client(), GoogleAccessID: "writer@example.iam.gserviceaccount.com",
		SignBytes: func([]byte) ([]byte, error) { return bytes.Repeat([]byte{0x5a}, 256), nil },
		Hostname:  strings.TrimPrefix(server.URL, "http://"), Insecure: true,
		LeaseKey: bytes.Repeat([]byte{0x42}, 32), Random: bytes.NewReader(bytes.Repeat([]byte{0x24}, 4096)),
	}
	first, err := gcstransport.NewWithTransfers(client, "endlessfs-test", options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := gcstransport.NewWithTransfers(client, "endlessfs-test", options)
	if err != nil {
		t.Fatal(err)
	}
	key := objectstore.MustKey("endlessfs/v1/staging/user/upload/data")
	handle, err := first.BeginUpload(context.Background(), objectstore.UploadRequest{
		UploadID: "upload-1", Key: key, Size: 6, MediaType: "text/plain", Resumable: true,
		ExpiresAt: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if handle.Capability.Protocol != domain.UploadResumable || handle.Capability.Framing != domain.UploadFramingContentRange || len(handle.Lease) == 0 || bytes.Contains(handle.Lease, []byte("resumable")) {
		t.Fatalf("BeginUpload() = %+v", handle)
	}
	request, _ := http.NewRequest(http.MethodPut, handle.Capability.URL, strings.NewReader("abc"))
	request.Header.Set("Content-Type", "text/plain")
	request.Header.Set("Content-Range", "bytes 0-2/6")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusPermanentRedirect {
		t.Fatalf("first chunk status = %d", response.StatusCode)
	}
	progress, err := second.UploadProgress(context.Background(), handle.Lease)
	if err != nil || progress.Offset != 3 || progress.Complete {
		t.Fatalf("UploadProgress() = %+v, %v", progress, err)
	}
	request, _ = http.NewRequest(http.MethodPut, handle.Capability.URL, strings.NewReader("def"))
	request.Header.Set("Content-Type", "text/plain")
	request.Header.Set("Content-Range", "bytes 3-5/6")
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("final chunk status = %d", response.StatusCode)
	}
	progress, err = second.UploadProgress(context.Background(), handle.Lease)
	if err != nil || !progress.Complete || progress.Offset != 6 || progress.Version == "" {
		t.Fatalf("complete UploadProgress() = %+v, %v", progress, err)
	}
}

func TestGCSUploadCleanupDistinguishesFinalizedAndIncompleteSessions(t *testing.T) {
	t.Run("finalized-object", func(t *testing.T) {
		server, fake := newGCSServerWithFake(t)
		fake.rejectCompletedDelete = true
		backend, err := gcstransport.NewWithTransfers(protocolClient(t, server), "endlessfs-test", gcstransport.TransferOptions{
			HTTPClient: server.Client(), GoogleAccessID: "writer@example.iam.gserviceaccount.com",
			SignBytes: func([]byte) ([]byte, error) { return bytes.Repeat([]byte{0x5a}, 256), nil },
			Hostname:  strings.TrimPrefix(server.URL, "http://"), Insecure: true,
			LeaseKey: bytes.Repeat([]byte{0x42}, 32), Random: bytes.NewReader(bytes.Repeat([]byte{0x27}, 4096)),
		})
		if err != nil {
			t.Fatal(err)
		}
		key := objectstore.MustKey("endlessfs/v1/staging/user/finalized/data")
		handle, err := backend.BeginUpload(context.Background(), objectstore.UploadRequest{
			UploadID: "finalized-1", Key: key, Size: 4, MediaType: "application/octet-stream", Resumable: true,
			ExpiresAt: time.Now().UTC().Add(time.Minute),
		})
		if err != nil {
			t.Fatal(err)
		}
		request, _ := http.NewRequest(http.MethodPut, handle.Capability.URL, strings.NewReader("data"))
		request.Header.Set("Content-Type", "application/octet-stream")
		request.Header.Set("Content-Range", "bytes 0-3/4")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("upload status = %d", response.StatusCode)
		}
		if err := backend.AbortUpload(context.Background(), handle.Lease); err != nil {
			t.Fatalf("clean finalized upload: %v", err)
		}
		fake.mu.Lock()
		deleteAttempts := fake.sessionDeleteAttempts
		_, objectExists := fake.objects[key.String()]
		fake.mu.Unlock()
		if deleteAttempts != 0 || objectExists {
			t.Fatalf("finalized cleanup = session deletes %d, object exists %t", deleteAttempts, objectExists)
		}
	})

	t.Run("incomplete-session", func(t *testing.T) {
		server, fake := newGCSServerWithFake(t)
		fake.rejectCompletedDelete = true
		backend, err := gcstransport.NewWithTransfers(protocolClient(t, server), "endlessfs-test", gcstransport.TransferOptions{
			HTTPClient: server.Client(), GoogleAccessID: "writer@example.iam.gserviceaccount.com",
			SignBytes: func([]byte) ([]byte, error) { return bytes.Repeat([]byte{0x5a}, 256), nil },
			Hostname:  strings.TrimPrefix(server.URL, "http://"), Insecure: true,
			LeaseKey: bytes.Repeat([]byte{0x42}, 32), Random: bytes.NewReader(bytes.Repeat([]byte{0x28}, 4096)),
		})
		if err != nil {
			t.Fatal(err)
		}
		handle, err := backend.BeginUpload(context.Background(), objectstore.UploadRequest{
			UploadID: "incomplete-1", Key: objectstore.MustKey("endlessfs/v1/staging/user/incomplete/data"),
			Size: 4, MediaType: "application/octet-stream", Resumable: true, ExpiresAt: time.Now().UTC().Add(time.Minute),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := backend.AbortUpload(context.Background(), handle.Lease); err != nil {
			t.Fatalf("cancel incomplete upload: %v", err)
		}
		fake.mu.Lock()
		deleteAttempts := fake.sessionDeleteAttempts
		activeSessions := len(fake.sessions)
		fake.mu.Unlock()
		if deleteAttempts != 1 || activeSessions != 0 {
			t.Fatalf("incomplete cancellation = session deletes %d, active sessions %d", deleteAttempts, activeSessions)
		}
	})
}

func TestWorkloadIdentityTransferConstructionRequiresNoPrivateKeyOrNetwork(t *testing.T) {
	server, _ := newGCSServerWithFake(t)
	backend, err := gcstransport.New(protocolClient(t, server), "endlessfs-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.EnableWorkloadIdentityTransfers(bytes.Repeat([]byte{0x43}, 32), "writer@example-project.iam.gserviceaccount.com"); err != nil {
		t.Fatalf("EnableWorkloadIdentityTransfers() with explicit IAM signing identity: %v", err)
	}
	if err := backend.EnableWorkloadIdentityTransfers(bytes.Repeat([]byte{0x44}, 32), ""); err != nil {
		t.Fatalf("EnableWorkloadIdentityTransfers() with ADC identity discovery: %v", err)
	}
}

func TestGCSSignedSingleUploadAndDownloadAreGenerationBound(t *testing.T) {
	server, _ := newGCSServerWithFake(t)
	client := protocolClient(t, server)
	backend, err := gcstransport.NewWithTransfers(client, "endlessfs-test", gcstransport.TransferOptions{
		HTTPClient: server.Client(), GoogleAccessID: "writer@example.iam.gserviceaccount.com",
		SignBytes: func([]byte) ([]byte, error) { return bytes.Repeat([]byte{0x5a}, 256), nil },
		Hostname:  strings.TrimPrefix(server.URL, "http://"), Insecure: true,
		LeaseKey: bytes.Repeat([]byte{0x42}, 32), Random: bytes.NewReader(bytes.Repeat([]byte{0x25}, 4096)),
	})
	if err != nil {
		t.Fatal(err)
	}
	key := objectstore.MustKey("endlessfs/v1/staging/user/single/data")
	handle, err := backend.BeginUpload(context.Background(), objectstore.UploadRequest{
		UploadID: "single-1", Key: key, Size: 4, MediaType: "text/plain",
		ExpiresAt: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPut, handle.Capability.URL, strings.NewReader("data"))
	request.Header.Set("Content-Type", "text/plain")
	request.Header.Set("Content-Range", "bytes 0-3/4")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("single upload status = %d", response.StatusCode)
	}
	progress, err := backend.UploadProgress(context.Background(), handle.Lease)
	if err != nil || !progress.Complete || progress.Size != 4 {
		t.Fatalf("UploadProgress() = %+v, %v", progress, err)
	}
	download, err := backend.CreateDownload(context.Background(), objectstore.DownloadRequest{
		Key: key, Version: progress.Version, Filename: "safe.txt", MediaType: "text/plain",
		Disposition: domain.DispositionAttachment, ExpiresAt: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err = server.Client().Get(download.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "data" || response.Header.Get("Content-Disposition") != `attachment; filename=safe.txt` {
		t.Fatalf("download = %d %q %q", response.StatusCode, body, response.Header.Get("Content-Disposition"))
	}
}

func TestGCSCORSRequiresExactApplicationOriginAndTransferHeaders(t *testing.T) {
	server, fake := newGCSServerWithFake(t)
	fake.mu.Lock()
	fake.allowedOrigin = "https://drive.example"
	fake.mu.Unlock()
	client := protocolClient(t, server)
	backend, err := gcstransport.NewWithTransfers(client, "endlessfs-test", gcstransport.TransferOptions{
		HTTPClient: server.Client(), GoogleAccessID: "writer@example.iam.gserviceaccount.com",
		SignBytes: func([]byte) ([]byte, error) { return bytes.Repeat([]byte{0x5a}, 256), nil },
		Hostname:  strings.TrimPrefix(server.URL, "http://"), Insecure: true,
		LeaseKey: bytes.Repeat([]byte{0x42}, 32), Random: bytes.NewReader(bytes.Repeat([]byte{0x26}, 4096)),
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := backend.BeginUpload(context.Background(), objectstore.UploadRequest{
		UploadID: "cors-1", Key: objectstore.MustKey("endlessfs/v1/staging/user/cors/data"),
		Size: 4, MediaType: "text/plain", Resumable: true, ExpiresAt: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	preflight, _ := http.NewRequest(http.MethodOptions, handle.Capability.URL, nil)
	preflight.Header.Set("Origin", "https://drive.example")
	preflight.Header.Set("Access-Control-Request-Method", http.MethodPut)
	preflight.Header.Set("Access-Control-Request-Headers", "content-type,content-range")
	response, err := server.Client().Do(preflight)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent || response.Header.Get("Access-Control-Allow-Origin") != "https://drive.example" || !strings.Contains(response.Header.Get("Access-Control-Allow-Headers"), "Content-Range") || !strings.Contains(response.Header.Get("Access-Control-Expose-Headers"), "Range") {
		t.Fatalf("preflight = %d headers=%v", response.StatusCode, response.Header)
	}
	denied, _ := http.NewRequest(http.MethodOptions, handle.Capability.URL, nil)
	denied.Header.Set("Origin", "https://evil.example")
	denied.Header.Set("Access-Control-Request-Method", http.MethodPut)
	response, err = server.Client().Do(denied)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden || response.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("denied preflight = %d headers=%v", response.StatusCode, response.Header)
	}
}
