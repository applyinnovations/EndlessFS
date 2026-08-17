package gcs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"google.golang.org/api/googleapi"
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
