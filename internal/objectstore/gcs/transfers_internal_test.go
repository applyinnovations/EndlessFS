package gcs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"google.golang.org/api/option"
)

func TestTransferHelpersRejectMalformedProviderValues(t *testing.T) {
	if _, err := newTransferConfiguration(TransferOptions{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("missing lease key error = %v", err)
	}
	if _, err := newTransferConfiguration(TransferOptions{LeaseKey: bytes.Repeat([]byte{1}, 32), SignBytes: func([]byte) ([]byte, error) { return nil, nil }}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("signer without identity error = %v", err)
	}
	for _, test := range []struct {
		value string
		size  int64
		want  int64
		valid bool
	}{
		{"", 10, 0, true}, {"bytes=0-0", 10, 1, true}, {"bytes=0-8", 10, 9, true},
		{"bytes=1-2", 10, 0, false}, {"bytes=0-x", 10, 0, false}, {"bytes=0-10", 10, 0, false},
	} {
		got, err := confirmedOffset(test.value, test.size)
		if test.valid && (err != nil || got != test.want) {
			t.Errorf("confirmedOffset(%q) = %d, %v", test.value, got, err)
		}
		if !test.valid && !errors.Is(err, domain.ErrInternal) {
			t.Errorf("confirmedOffset(%q) error = %v", test.value, err)
		}
	}
	for _, value := range []string{
		"", "://bad", "https://other.example/session", "https://user@storage.example/session", "https://storage.example/session#fragment",
	} {
		if _, err := validateSessionURL(value, "https://storage.example/init"); !errors.Is(err, domain.ErrInternal) {
			t.Errorf("validateSessionURL(%q) error = %v", value, err)
		}
	}
	if value, err := validateSessionURL("https://storage.example/session", "https://storage.example/init"); err != nil || value != "https://storage.example/session" {
		t.Fatalf("valid session URL = %q, %v", value, err)
	}
}

func TestUploadInitiationLifetimeIsIndependentFromResumableSession(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var calls int
	backend, _ := newTransferBoundaryBackend(t, now, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		got, err := strconv.Atoi(r.URL.Query().Get("X-Goog-Expires"))
		if err != nil || got < 1 || got > 300 {
			t.Errorf("initiation bearer lifetime = %d, want at most 300 seconds", got)
		}
		w.Header().Set("Location", "http://"+r.Host+"/session")
		w.WriteHeader(http.StatusCreated)
	}))
	request := objectstore.UploadRequest{UploadID: "slow-source", Key: objectstore.MustKey("endlessfs/v1/staging/user/slow/data"), Size: 8 << 20, MediaType: "application/octet-stream", Resumable: true, ExpiresAt: now.Add(24 * time.Hour), InitiationExpiresAt: now.Add(5 * time.Minute)}
	handle, err := backend.BeginUpload(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !handle.Capability.ExpiresAt.Equal(request.ExpiresAt) {
		t.Fatal("session inherited the short initiation deadline")
	}
	backend.transfer.clock.(*domain.FixedClock).Advance(6 * time.Hour)
	resumed, err := backend.ResumeUpload(t.Context(), handle.Lease)
	if err != nil || resumed.URL != handle.Capability.URL || !resumed.ExpiresAt.Equal(request.ExpiresAt) {
		t.Fatalf("slow source resume changed session or authority: %v, %+v", err, resumed)
	}
	backend.transfer.clock.(*domain.FixedClock).Advance(18 * time.Hour)
	if _, err := backend.ResumeUpload(t.Context(), handle.Lease); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired session resumed: %v", err)
	}
	for _, expiry := range []time.Time{now, request.ExpiresAt.Add(time.Second)} {
		backend.transfer.clock.(*domain.FixedClock).Advance(now.Sub(backend.transfer.clock.Now()))
		request.InitiationExpiresAt = expiry
		if _, err := backend.BeginUpload(t.Context(), request); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid initiation deadline accepted: %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("invalid lifetimes contacted provider: %d requests", calls)
	}
}

func TestObjectInfoRejectsMalformedProviderMetadata(t *testing.T) {
	key := objectstore.MustKey("endlessfs/v1/state/users/malformed-provider-metadata.json")
	for name, attrs := range map[string]*storage.ObjectAttrs{
		"generation": {Generation: 0},
		"size":       {Generation: 1, Size: -1},
		"md5":        {Generation: 1, MD5: []byte("short")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := objectInfoFromAttrs(key, attrs); !errors.Is(err, domain.ErrInternal) {
				t.Fatalf("objectInfoFromAttrs() error = %v", err)
			}
		})
	}
}

func TestTransferLeaseAuthenticationAndSchemaValidation(t *testing.T) {
	configuration, err := newTransferConfiguration(TransferOptions{
		LeaseKey: bytes.Repeat([]byte{2}, 32), Random: bytes.NewReader(bytes.Repeat([]byte{3}, 1024)),
		Clock: domain.NewFixedClock(time.Date(2042, 1, 2, 3, 4, 5, 0, time.UTC)),
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := &Backend{transfer: configuration}
	valid := uploadLease{
		SchemaVersion: 1, UploadID: "upload", Key: "endlessfs/v1/staging/user/operation/data",
		Size: 4, MediaType: "text/plain", Protocol: domain.UploadResumable,
		SessionURL: "https://storage.example/session", ExpiresAt: time.Date(2042, 1, 2, 3, 5, 5, 0, time.UTC),
	}
	sealed, err := backend.sealLease(valid)
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := backend.openLease(sealed); err != nil || opened.UploadID != valid.UploadID {
		t.Fatalf("openLease() = %+v, %v", opened, err)
	}
	if capability, err := backend.ResumeUpload(t.Context(), sealed); err != nil || capability.URL != valid.SessionURL || capability.Protocol != domain.UploadResumable || capability.ChunkRules == nil {
		t.Fatalf("ResumeUpload() = %+v, %v", capability, err)
	}
	if backend.BackendKind() != "gcs" {
		t.Fatalf("BackendKind() = %q", backend.BackendKind())
	}
	expired := valid
	expired.ExpiresAt = configuration.clock.Now().Add(-time.Second)
	expiredBody, err := backend.sealLease(expired)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.ResumeUpload(t.Context(), expiredBody); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired ResumeUpload() error = %v", err)
	}
	if _, err := backend.UploadProgress(t.Context(), expiredBody); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("expired UploadProgress() error = %v", err)
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := backend.openLease(sealed); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("tampered lease error = %v", err)
	}
	for name, mutate := range map[string]func(*uploadLease){
		"schema":   func(value *uploadLease) { value.SchemaVersion = 2 },
		"upload":   func(value *uploadLease) { value.UploadID = "" },
		"key":      func(value *uploadLease) { value.Key = "INVALID" },
		"size":     func(value *uploadLease) { value.Size = -1 },
		"media":    func(value *uploadLease) { value.MediaType = "" },
		"protocol": func(value *uploadLease) { value.Protocol = "unknown" },
		"session":  func(value *uploadLease) { value.SessionURL = "" },
		"expiry":   func(value *uploadLease) { value.ExpiresAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := valid
			mutate(&invalid)
			body, sealErr := backend.sealLease(invalid)
			if sealErr != nil {
				t.Fatal(sealErr)
			}
			if _, openErr := backend.openLease(body); !errors.Is(openErr, domain.ErrInvalid) {
				t.Fatalf("openLease() error = %v", openErr)
			}
		})
	}
}

func TestTransferLeaseSealingSerializesInjectedEntropyReader(t *testing.T) {
	configuration, err := newTransferConfiguration(TransferOptions{
		LeaseKey: bytes.Repeat([]byte{9}, 32),
		Random:   bytes.NewReader(bytes.Repeat([]byte{1}, 4096)),
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := &Backend{transfer: configuration}
	const workers = 64
	sealed := make([][]byte, workers)
	errorsFound := make([]error, workers)
	var wait sync.WaitGroup
	for index := range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			lease := uploadLease{SchemaVersion: 1, UploadID: fmt.Sprintf("upload-%d", index), Key: fmt.Sprintf("endlessfs/v1/staging/user/op-%d/data", index), Size: 1, MediaType: "text/plain", Protocol: domain.UploadSingle, ExpiresAt: time.Now().Add(time.Hour)}
			sealed[index], errorsFound[index] = backend.sealLease(lease)
		}()
	}
	wait.Wait()
	seen := make(map[string]struct{}, workers)
	for index, err := range errorsFound {
		if err != nil {
			t.Fatalf("seal worker %d: %v", index, err)
		}
		if _, found := seen[string(sealed[index])]; found {
			t.Fatalf("seal worker %d reused a nonce", index)
		}
		seen[string(sealed[index])] = struct{}{}
	}
}

func TestBackendRejectsInvalidProviderMutationMetadata(t *testing.T) {
	now := time.Now().UTC()
	key := objectstore.MustKey("endlessfs/v1/state/users/metadata.json")
	destination := objectstore.MustKey("endlessfs/v1/state/users/metadata-copy.json")
	backend, _ := newTransferBoundaryBackend(t, now, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "rewriteTo") {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(writer, `{"kind":"storage#rewriteResponse","totalBytesRewritten":"1","objectSize":"1","done":true,"resource":{"kind":"storage#object","bucket":"endlessfs-test","name":%q,"size":"1","generation":"0","metageneration":"1","crc32c":"AAAAAA=="}}`, destination.String())
			return
		}
		writeBoundaryObject(writer, key.String(), 0, 1)
	}))
	if _, err := backend.Head(context.Background(), key); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("Head(invalid generation) error = %v", err)
	}
	if _, err := backend.Put(context.Background(), key, []byte("x"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("Put(invalid generation) error = %v", err)
	}
	if _, err := backend.Put(context.Background(), key, []byte("x"), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: "foreign"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("Put(foreign match version) error = %v", err)
	}
	if _, err := backend.Copy(context.Background(), key, destination, objectstore.CopyCondition{SourceVersion: encodeVersion(1), Destination: objectstore.PutCondition{Mode: objectstore.PutMatch, Version: "foreign"}}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("Copy(foreign destination version) error = %v", err)
	}
	if _, err := backend.Copy(context.Background(), key, destination, objectstore.CopyCondition{SourceVersion: encodeVersion(1), Destination: objectstore.PutCondition{Mode: objectstore.PutCreateOnly}}); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("Copy(invalid generation) error = %v", err)
	}
}

func TestGCSTransferConfigurationRejectsMalformedBrowserOrigins(t *testing.T) {
	for _, origin := range []string{"*", "https://", "https://user:password@drive.example", "https://drive.example/path", "https://drive.example?scope=all", "https://drive.example#fragment", "https://drive.example\r\nX-Injected: yes"} {
		t.Run(origin, func(t *testing.T) {
			if _, err := newTransferConfiguration(TransferOptions{LeaseKey: bytes.Repeat([]byte{7}, 32), AllowedOrigin: origin}); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("origin %q accepted: %v", origin, err)
			}
		})
	}
	for _, origin := range []string{"https://drive.example", "https://drive.example:8443", "http://127.0.0.1:8080"} {
		if cfg, err := newTransferConfiguration(TransferOptions{LeaseKey: bytes.Repeat([]byte{7}, 32), AllowedOrigin: origin}); err != nil || cfg.allowedOrigin != origin {
			t.Fatalf("valid origin %q: %v", origin, err)
		}
	}
}

func TestResumableSessionTerminalClassification(t *testing.T) {
	now := time.Now().UTC()
	key := objectstore.MustKey("endlessfs/v1/staging/user/terminal-status/data")
	for _, test := range []struct {
		name         string
		status       int
		rangeValue   string
		wantTerminal bool
		wantError    error
	}{
		{name: "completed", status: http.StatusOK, wantTerminal: true},
		{name: "created", status: http.StatusCreated, wantTerminal: true},
		{name: "cancelled", status: http.StatusNoContent, wantTerminal: true},
		{name: "missing", status: http.StatusNotFound, wantTerminal: true},
		{name: "gone", status: http.StatusGone, wantTerminal: true},
		{name: "active", status: http.StatusPermanentRedirect, rangeValue: "bytes=0-1"},
		{name: "malformed-active-range", status: http.StatusPermanentRedirect, rangeValue: "invalid", wantError: domain.ErrInternal},
		{name: "unexpected", status: http.StatusTeapot, wantError: domain.ErrInternal},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend, server := newTransferBoundaryBackend(t, now, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Range", test.rangeValue)
				writer.WriteHeader(test.status)
			}))
			lease := boundaryLease(now, key, domain.UploadResumable, server.URL+"/session", 4)
			terminal, err := backend.resumableSessionTerminal(context.Background(), lease)
			if !errors.Is(err, test.wantError) || terminal != test.wantTerminal {
				t.Fatalf("resumableSessionTerminal() = %t, %v; want %t, %v", terminal, err, test.wantTerminal, test.wantError)
			}
		})
	}

	backend, _ := newTransferBoundaryBackend(t, now, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	invalid := boundaryLease(now, key, domain.UploadResumable, ":", 4)
	if _, err := backend.resumableSessionTerminal(context.Background(), invalid); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("invalid status URL error = %v", err)
	}
	backend.transfer.httpClient = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("transport unavailable")
	})}
	lease := boundaryLease(now, key, domain.UploadResumable, "https://storage.example/session", 4)
	if _, err := backend.resumableSessionTerminal(context.Background(), lease); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("status transport error = %v", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func newTransferBoundaryBackend(t *testing.T, now time.Time, handler http.Handler) (*Backend, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := storage.NewClient(context.Background(), storage.WithJSONReads(), option.WithEndpoint(server.URL+"/storage/v1/"), option.WithHTTPClient(server.Client()), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	backend, err := NewWithTransfers(client, "endlessfs-test", TransferOptions{
		HTTPClient: server.Client(), GoogleAccessID: "writer@example.iam.gserviceaccount.com",
		SignBytes: func([]byte) ([]byte, error) { return bytes.Repeat([]byte{0x55}, 256), nil },
		Hostname:  strings.TrimPrefix(server.URL, "http://"), Insecure: true,
		LeaseKey: bytes.Repeat([]byte{0x44}, 32), Random: bytes.NewReader(bytes.Repeat([]byte{0x22}, 4096)), Clock: domain.NewFixedClock(now),
	})
	if err != nil {
		t.Fatal(err)
	}
	return backend, server
}

func boundaryLease(now time.Time, key objectstore.Key, protocol domain.UploadProtocol, session string, size int64) uploadLease {
	return uploadLease{SchemaVersion: 1, UploadID: "upload", Key: key.String(), Size: size, MediaType: "text/plain", Protocol: protocol, SessionURL: session, ExpiresAt: now.Add(time.Minute)}
}

func writeBoundaryObject(writer http.ResponseWriter, key string, generation, size int64) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(writer, `{"kind":"storage#object","bucket":"endlessfs-test","name":%q,"size":%q,"generation":%q,"metageneration":"1","crc32c":"AAAAAA=="}`, key, fmt.Sprint(size), fmt.Sprint(generation))
}
