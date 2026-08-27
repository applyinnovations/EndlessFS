package gcs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"google.golang.org/api/googleapi"
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

func TestTransferPublicInputAndContextBoundaries(t *testing.T) {
	configuration, err := newTransferConfiguration(TransferOptions{LeaseKey: bytes.Repeat([]byte{4}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	backend := &Backend{transfer: configuration}
	if _, err := backend.BeginUpload(t.Context(), objectstore.UploadRequest{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid BeginUpload() error = %v", err)
	}
	if _, err := backend.CreateDownload(t.Context(), objectstore.DownloadRequest{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid CreateDownload() error = %v", err)
	}
	withoutTransfers := &Backend{}
	if _, err := withoutTransfers.BeginUpload(t.Context(), objectstore.UploadRequest{}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("unconfigured BeginUpload() error = %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := backend.BeginUpload(canceled, objectstore.UploadRequest{}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled BeginUpload() error = %v", err)
	}
	if _, err := backend.ResumeUpload(canceled, []byte("invalid")); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid ResumeUpload() error = %v", err)
	}
	if err := backend.AbortUpload(t.Context(), []byte("invalid")); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid AbortUpload() error = %v", err)
	}
}

func TestTransferStatusClassificationIsStable(t *testing.T) {
	for status, expected := range map[int]error{
		http.StatusBadRequest: domain.ErrInvalid, http.StatusUnauthorized: domain.ErrUnauthenticated,
		http.StatusForbidden: domain.ErrUnauthorized, http.StatusNotFound: domain.ErrNotFound,
		http.StatusGone: domain.ErrNotFound, http.StatusConflict: domain.ErrPreconditionFailed,
		http.StatusPreconditionFailed: domain.ErrPreconditionFailed, http.StatusTooManyRequests: domain.ErrUnavailable,
		http.StatusRequestTimeout: domain.ErrUnavailable, http.StatusInternalServerError: domain.ErrUnavailable,
		http.StatusBadGateway: domain.ErrUnavailable, http.StatusServiceUnavailable: domain.ErrUnavailable,
		http.StatusGatewayTimeout: domain.ErrUnavailable, http.StatusTeapot: domain.ErrInternal,
	} {
		if err := classifyHTTPStatus("safe", status); !errors.Is(err, expected) {
			t.Errorf("status %d error = %v, want %v", status, err, expected)
		}
	}
}

func TestBackendValueAndErrorHelpers(t *testing.T) {
	for _, bucket := range []string{"", "ab", strings.Repeat("x", 223), "bad/name", "bad\\name", "bad\x00name", "bad\rname", "bad\nname"} {
		if err := validateBucket(bucket); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("validateBucket(%q) error = %v", bucket, err)
		}
	}
	if err := validateBucket("endlessfs-portable-test"); err != nil {
		t.Fatal(err)
	}
	for _, version := range []objectstore.NativeVersion{"", "other.1", "gcs-v1.0", "gcs-v1.-1", "gcs-v1.01", "gcs-v1.x"} {
		if _, err := decodeVersion(version); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("decodeVersion(%q) error = %v", version, err)
		}
	}
	if generation, err := decodeVersion(encodeVersion(42)); err != nil || generation != 42 {
		t.Fatalf("version round trip = %d, %v", generation, err)
	}

	if classify("safe", nil) != nil {
		t.Fatal("classify(nil) returned an error")
	}
	for input, expected := range map[error]error{
		storage.ErrObjectNotExist:                              domain.ErrNotFound,
		context.Canceled:                                       domain.ErrUnavailable,
		context.DeadlineExceeded:                               domain.ErrUnavailable,
		&googleapi.Error{Code: http.StatusBadRequest}:          domain.ErrInvalid,
		&googleapi.Error{Code: http.StatusUnauthorized}:        domain.ErrUnauthenticated,
		&googleapi.Error{Code: http.StatusForbidden}:           domain.ErrUnauthorized,
		&googleapi.Error{Code: http.StatusNotFound}:            domain.ErrNotFound,
		&googleapi.Error{Code: http.StatusConflict}:            domain.ErrConflict,
		&googleapi.Error{Code: http.StatusPreconditionFailed}:  domain.ErrPreconditionFailed,
		&googleapi.Error{Code: http.StatusTooManyRequests}:     domain.ErrRateLimited,
		&googleapi.Error{Code: http.StatusRequestTimeout}:      domain.ErrUnavailable,
		&googleapi.Error{Code: http.StatusBadGateway}:          domain.ErrUnavailable,
		&googleapi.Error{Code: http.StatusInternalServerError}: domain.ErrUnavailable,
		&googleapi.Error{Code: http.StatusServiceUnavailable}:  domain.ErrUnavailable,
		&googleapi.Error{Code: http.StatusGatewayTimeout}:      domain.ErrUnavailable,
		fmt.Errorf("opaque failure"):                           domain.ErrInternal,
	} {
		if err := classify("safe", input); !errors.Is(err, expected) || strings.Contains(err.Error(), "opaque failure") {
			t.Fatalf("classify(%T) = %v, want %v", input, err, expected)
		}
	}
	createOnly := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
	if err := classifyPut(createOnly, &googleapi.Error{Code: http.StatusPreconditionFailed}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("create-only classifyPut error = %v", err)
	}
	match := objectstore.PutCondition{Mode: objectstore.PutMatch, Version: encodeVersion(1)}
	if err := classifyPut(match, &googleapi.Error{Code: http.StatusPreconditionFailed}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("match classifyPut error = %v", err)
	}

	if backend, err := NewWithTransfers(nil, "endlessfs-test", TransferOptions{}); backend != nil || !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("NewWithTransfers(nil) = %+v, %v", backend, err)
	}
	var nilBackend *Backend
	if err := nilBackend.Close(); err != nil {
		t.Fatalf("nil Close() error = %v", err)
	}
	backend := &Backend{}
	if err := backend.Close(); err != nil {
		t.Fatalf("injected Close() error = %v", err)
	}
	if err := backend.EnableWorkloadIdentityTransfers([]byte("short"), "account@example.iam.gserviceaccount.com"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid transfer enablement error = %v", err)
	}
	if err := backend.EnableWorkloadIdentityTransfers(bytes.Repeat([]byte{7}, 32), "account@example.iam.gserviceaccount.com"); err != nil || backend.transfer == nil {
		t.Fatalf("valid transfer enablement error = %v", err)
	}
}

func TestTransferLeaseRandomFailureAndSingleResume(t *testing.T) {
	configuration, err := newTransferConfiguration(TransferOptions{LeaseKey: bytes.Repeat([]byte{8}, 32), Random: strings.NewReader("short")})
	if err != nil {
		t.Fatal(err)
	}
	backend := &Backend{transfer: configuration}
	lease := uploadLease{SchemaVersion: 1, UploadID: "upload", Key: "endlessfs/v1/staging/user/op/data", Size: 1, MediaType: "text/plain", Protocol: domain.UploadSingle, ExpiresAt: time.Now().Add(time.Hour)}
	if _, err := backend.sealLease(lease); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("short nonce source error = %v", err)
	}

	configuration, err = newTransferConfiguration(TransferOptions{LeaseKey: bytes.Repeat([]byte{9}, 32), Random: bytes.NewReader(bytes.Repeat([]byte{1}, 64)), Clock: domain.NewFixedClock(time.Now())})
	if err != nil {
		t.Fatal(err)
	}
	backend.transfer = configuration
	sealed, err := backend.sealLease(lease)
	if err != nil {
		t.Fatal(err)
	}
	capability, err := backend.ResumeUpload(context.Background(), sealed)
	if err != nil || capability.Protocol != domain.UploadSingle || capability.ChunkRules != nil || capability.DeclaredSize != 1 {
		t.Fatalf("single resume = %+v, %v", capability, err)
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

func TestTransferProviderFailureMatrixFailsClosed(t *testing.T) {
	now := time.Now().UTC()
	key := objectstore.MustKey("endlessfs/v1/staging/user/failure/data")
	request := objectstore.UploadRequest{UploadID: "failure-upload", Key: key, Size: 4, MediaType: "text/plain", Resumable: true, ExpiresAt: now.Add(time.Minute)}

	t.Run("construction", func(t *testing.T) {
		backend, _ := newTransferBoundaryBackend(t, now, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writeBoundaryGCSProblem(writer, http.StatusNotFound)
		}))
		if configured, err := NewWithTransfers(backend.client, "endlessfs-test", TransferOptions{}); configured != nil || !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("NewWithTransfers() = %+v, %v", configured, err)
		}
		backend.owned = true
		if err := backend.Close(); err != nil || backend.owned {
			t.Fatalf("owned Close() error = %v, owned=%v", err, backend.owned)
		}
	})

	for name, configure := range map[string]func(*Backend, *httptest.Server){
		"signing": func(backend *Backend, _ *httptest.Server) {
			backend.transfer.signBytes = func([]byte) ([]byte, error) { return nil, errors.New("signing unavailable") }
		},
		"transport": func(backend *Backend, _ *httptest.Server) {
			backend.transfer.httpClient = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("transport unavailable") })}
		},
	} {
		t.Run("begin-"+name, func(t *testing.T) {
			backend, server := newTransferBoundaryBackend(t, now, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Location", "http://"+request.Host+"/session")
				writer.WriteHeader(http.StatusCreated)
			}))
			configure(backend, server)
			if _, err := backend.BeginUpload(context.Background(), request); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("BeginUpload() error = %v", err)
			}
		})
	}

	for name, handler := range map[string]http.Handler{
		"status": http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writeBoundaryGCSProblem(writer, http.StatusTeapot) }),
		"location": http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Location", "https://other.example/session")
			writer.WriteHeader(http.StatusCreated)
		}),
	} {
		t.Run("begin-"+name, func(t *testing.T) {
			backend, _ := newTransferBoundaryBackend(t, now, handler)
			if _, err := backend.BeginUpload(context.Background(), request); err == nil {
				t.Fatal("BeginUpload() unexpectedly succeeded")
			}
		})
	}

	t.Run("begin-seal", func(t *testing.T) {
		backend, _ := newTransferBoundaryBackend(t, now, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Location", "http://"+request.Host+"/session")
			writer.WriteHeader(http.StatusCreated)
		}))
		backend.transfer.random = strings.NewReader("short")
		if _, err := backend.BeginUpload(context.Background(), request); !errors.Is(err, domain.ErrInternal) {
			t.Fatalf("BeginUpload() seal error = %v", err)
		}
	})

	t.Run("progress-and-abort", func(t *testing.T) {
		status := http.StatusPermanentRedirect
		rangeValue := "bytes=0-x"
		backend, server := newTransferBoundaryBackend(t, now, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if strings.HasPrefix(request.URL.Path, "/storage/v1/") {
				writeBoundaryGCSProblem(writer, http.StatusNotFound)
				return
			}
			if request.Method == http.MethodDelete {
				writer.WriteHeader(status)
				return
			}
			writer.Header().Set("Range", rangeValue)
			writer.WriteHeader(status)
		}))
		lease := uploadLease{SchemaVersion: 1, UploadID: "upload", Key: key.String(), Size: 4, MediaType: "text/plain", Protocol: domain.UploadResumable, SessionURL: server.URL + "/session", ExpiresAt: now.Add(time.Minute)}
		sealed, err := backend.sealLease(lease)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.UploadProgress(context.Background(), []byte("invalid")); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid progress lease error = %v", err)
		}
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := backend.UploadProgress(canceled, sealed); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("canceled progress error = %v", err)
		}
		if _, err := backend.ResumeUpload(canceled, sealed); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("canceled resume error = %v", err)
		}
		if _, err := backend.UploadProgress(context.Background(), sealed); !errors.Is(err, domain.ErrInternal) {
			t.Fatalf("invalid progress range error = %v", err)
		}

		rangeValue = ""
		status = http.StatusTeapot
		if _, err := backend.UploadProgress(context.Background(), sealed); !errors.Is(err, domain.ErrInternal) {
			t.Fatalf("progress status error = %v", err)
		}
		if err := backend.AbortUpload(context.Background(), sealed); !errors.Is(err, domain.ErrInternal) {
			t.Fatalf("abort status error = %v", err)
		}

		backend.transfer.httpClient = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("transport unavailable") })}
		if _, err := backend.UploadProgress(context.Background(), sealed); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("progress transport error = %v", err)
		}
		if err := backend.AbortUpload(context.Background(), sealed); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("abort transport error = %v", err)
		}

		invalidURL := lease
		invalidURL.SessionURL = ":"
		invalidSealed, err := backend.sealLease(invalidURL)
		if err != nil {
			t.Fatal(err)
		}
		if err := backend.AbortUpload(context.Background(), invalidSealed); !errors.Is(err, domain.ErrInternal) {
			t.Fatalf("abort URL error = %v", err)
		}
	})

	t.Run("download-and-seal", func(t *testing.T) {
		backend, _ := newTransferBoundaryBackend(t, now, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writeBoundaryGCSProblem(writer, http.StatusNotFound)
		}))
		download := objectstore.DownloadRequest{Key: key, Version: "foreign", Filename: "safe.txt", MediaType: "text/plain", Disposition: domain.DispositionAttachment, ExpiresAt: now.Add(time.Minute)}
		if _, err := backend.CreateDownload(context.Background(), download); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("foreign download version error = %v", err)
		}
		download.Version = encodeVersion(1)
		backend.transfer.signBytes = func([]byte) ([]byte, error) { return nil, errors.New("signing unavailable") }
		if _, err := backend.CreateDownload(context.Background(), download); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("download signing error = %v", err)
		}
		lease := uploadLease{SchemaVersion: 1, UploadID: "upload", Key: key.String(), Size: 1, MediaType: "text/plain", Protocol: domain.UploadSingle, ExpiresAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}
		if _, err := backend.sealLease(lease); !errors.Is(err, domain.ErrInternal) {
			t.Fatalf("lease encoding error = %v", err)
		}
	})
}

func TestBackendAndTransferReconciliationBranchesFailClosed(t *testing.T) {
	now := time.Now().UTC()
	key := objectstore.MustKey("endlessfs/v1/staging/user/reconcile/data")

	if _, err := Open(context.Background(), "x"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("Open(invalid bucket) error = %v", err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", t.TempDir()+"/missing-credentials.json")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(canceled, "endlessfs-test"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("Open(invalid ADC) error = %v", err)
	}

	backend, _ := newTransferBoundaryBackend(t, now, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeBoundaryGCSProblem(writer, http.StatusNotFound)
	}))
	if _, err := backend.Put(context.Background(), key, nil, objectstore.PutCondition{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("Put(invalid condition) error = %v", err)
	}
	if _, err := backend.Copy(context.Background(), key, objectstore.MustKey("endlessfs/v1/staging/user/reconcile/copy"), objectstore.CopyCondition{SourceVersion: encodeVersion(1)}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("Copy(invalid destination condition) error = %v", err)
	}
	if _, err := backend.conditionedObject(key, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: "foreign"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("conditionedObject(foreign version) error = %v", err)
	}
	if _, err := backend.conditionedObject(key, objectstore.PutCondition{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("conditionedObject(invalid mode) error = %v", err)
	}
	transportFailure := errors.New("opaque copy failure")
	if err := backend.classifyCopy(context.Background(), key, 1, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: encodeVersion(1)}, transportFailure); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("classifyCopy(match) error = %v", err)
	}
	if err := backend.classifyCopy(context.Background(), key, 1, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}, transportFailure); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("classifyCopy(non-precondition) error = %v", err)
	}
	verificationFailureBackend, _ := newTransferBoundaryBackend(t, now, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeBoundaryGCSProblem(writer, http.StatusTeapot)
	}))
	if err := verificationFailureBackend.classifyCopy(context.Background(), key, 1, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}, &googleapi.Error{Code: http.StatusPreconditionFailed}); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("classifyCopy(source verification failure) error = %v", err)
	}

	for name, test := range map[string]struct {
		handler func(http.ResponseWriter, *http.Request, *int)
		lease   uploadLease
		invoke  func(*Backend, []byte) error
		want    error
	}{
		"progress-head-failure": {
			handler: func(writer http.ResponseWriter, request *http.Request, _ *int) {
				writeBoundaryGCSProblem(writer, http.StatusTeapot)
			},
			lease: boundaryLease(now, key, domain.UploadSingle, "", 4),
			invoke: func(backend *Backend, sealed []byte) error {
				_, err := backend.UploadProgress(context.Background(), sealed)
				return err
			},
			want: domain.ErrInternal,
		},
		"progress-single-missing": {
			handler: func(writer http.ResponseWriter, request *http.Request, _ *int) {
				writeBoundaryGCSProblem(writer, http.StatusNotFound)
			},
			lease: boundaryLease(now, key, domain.UploadSingle, "", 4),
			invoke: func(backend *Backend, sealed []byte) error {
				progress, err := backend.UploadProgress(context.Background(), sealed)
				if err == nil && progress.Complete {
					return errors.New("missing single upload was complete")
				}
				return err
			},
		},
		"progress-existing-size": {
			handler: func(writer http.ResponseWriter, request *http.Request, _ *int) {
				writeBoundaryObject(writer, key.String(), 1, 3)
			},
			lease: boundaryLease(now, key, domain.UploadSingle, "", 4),
			invoke: func(backend *Backend, sealed []byte) error {
				_, err := backend.UploadProgress(context.Background(), sealed)
				return err
			},
			want: domain.ErrPreconditionFailed,
		},
		"progress-invalid-session": {
			handler: func(writer http.ResponseWriter, request *http.Request, _ *int) {
				writeBoundaryGCSProblem(writer, http.StatusNotFound)
			},
			lease: boundaryLease(now, key, domain.UploadResumable, ":", 4),
			invoke: func(backend *Backend, sealed []byte) error {
				_, err := backend.UploadProgress(context.Background(), sealed)
				return err
			},
			want: domain.ErrInternal,
		},
		"progress-complete-missing-object": {
			handler: func(writer http.ResponseWriter, request *http.Request, calls *int) {
				if strings.HasPrefix(request.URL.Path, "/storage/v1/") {
					*calls++
					writeBoundaryGCSProblem(writer, http.StatusNotFound)
					return
				}
				writer.WriteHeader(http.StatusOK)
			},
			lease: boundaryLease(now, key, domain.UploadResumable, "SESSION", 4),
			invoke: func(backend *Backend, sealed []byte) error {
				_, err := backend.UploadProgress(context.Background(), sealed)
				return err
			},
			want: domain.ErrNotFound,
		},
		"abort-head-failure": {
			handler: func(writer http.ResponseWriter, request *http.Request, _ *int) {
				writeBoundaryGCSProblem(writer, http.StatusTeapot)
			},
			lease:  boundaryLease(now, key, domain.UploadSingle, "", 4),
			invoke: func(backend *Backend, sealed []byte) error { return backend.AbortUpload(context.Background(), sealed) },
			want:   domain.ErrInternal,
		},
		"abort-size-mismatch": {
			handler: func(writer http.ResponseWriter, request *http.Request, _ *int) {
				writeBoundaryObject(writer, key.String(), 1, 3)
			},
			lease:  boundaryLease(now, key, domain.UploadSingle, "", 4),
			invoke: func(backend *Backend, sealed []byte) error { return backend.AbortUpload(context.Background(), sealed) },
			want:   domain.ErrPreconditionFailed,
		},
		"abort-delete-failure": {
			handler: func(writer http.ResponseWriter, request *http.Request, _ *int) {
				if request.Method == http.MethodDelete {
					writeBoundaryGCSProblem(writer, http.StatusInternalServerError)
					return
				}
				writeBoundaryObject(writer, key.String(), 1, 4)
			},
			lease:  boundaryLease(now, key, domain.UploadSingle, "", 4),
			invoke: func(backend *Backend, sealed []byte) error { return backend.AbortUpload(context.Background(), sealed) },
			want:   domain.ErrUnavailable,
		},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			var server *httptest.Server
			handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if test.lease.SessionURL == "SESSION" {
					test.lease.SessionURL = server.URL + "/session"
				}
				test.handler(writer, request, &calls)
			})
			backend, server = newTransferBoundaryBackend(t, now, handler)
			if test.lease.SessionURL == "SESSION" {
				test.lease.SessionURL = server.URL + "/session"
			}
			sealed, err := backend.sealLease(test.lease)
			if err != nil {
				t.Fatal(err)
			}
			err = test.invoke(backend, sealed)
			if test.want == nil && err != nil {
				t.Fatalf("operation error = %v", err)
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("operation error = %v, want %v", err, test.want)
			}
		})
	}

	backend, _ = newTransferBoundaryBackend(t, now, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeBoundaryGCSProblem(writer, http.StatusNotFound)
	}))
	invalidFilename := objectstore.DownloadRequest{Key: key, Version: encodeVersion(1), Filename: "safe.txt", MediaType: "text/plain", Disposition: domain.Disposition("\n"), ExpiresAt: now.Add(time.Minute)}
	if _, err := backend.CreateDownload(context.Background(), invalidFilename); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid download filename error = %v", err)
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

func writeBoundaryGCSProblem(writer http.ResponseWriter, status int) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = fmt.Fprintf(writer, `{"error":{"code":%d,"message":"failure"}}`, status)
}

func boundaryLease(now time.Time, key objectstore.Key, protocol domain.UploadProtocol, session string, size int64) uploadLease {
	return uploadLease{SchemaVersion: 1, UploadID: "upload", Key: key.String(), Size: size, MediaType: "text/plain", Protocol: protocol, SessionURL: session, ExpiresAt: now.Add(time.Minute)}
}

func writeBoundaryObject(writer http.ResponseWriter, key string, generation, size int64) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(writer, `{"kind":"storage#object","bucket":"endlessfs-test","name":%q,"size":%q,"generation":%q,"metageneration":"1","crc32c":"AAAAAA=="}`, key, fmt.Sprint(size), fmt.Sprint(generation))
}
