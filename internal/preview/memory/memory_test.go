package memory_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/preview"
	previewmemory "github.com/applyinnovations/endlessfs/internal/preview/memory"
	"github.com/applyinnovations/endlessfs/internal/preview/storecontract"
	"github.com/applyinnovations/endlessfs/internal/secret"
)

func TestContractMemoryPreviewStore(t *testing.T) {
	storecontract.Run(t, func(t *testing.T) storecontract.Harness {
		clock := domain.NewFixedClock(time.Date(2035, 1, 2, 3, 4, 5, 0, time.UTC))
		store, err := previewmemory.New(previewmemory.Options{
			Clock: clock, IDs: domain.NewIDGenerator(bytes.NewReader(deterministicBytes(1 << 20))),
			Key: secret.Value(testBearer(0x44)), CapabilityTTL: time.Minute,
		})
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(store)
		t.Cleanup(server.Close)
		if err := store.SetDataPlaneBaseURL(server.URL); err != nil {
			t.Fatal(err)
		}
		return storecontract.Harness{Store: store, Client: server.Client(), SetAvailable: store.SetAvailable, Advance: clock.Advance}
	})
}

func TestMemoryPreviewStoreBoundaryFailures(t *testing.T) {
	if _, err := previewmemory.New(previewmemory.Options{Key: secret.Value("invalid")}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("weak key error = %v", err)
	}
	if _, err := previewmemory.New(previewmemory.Options{Key: secret.Value(testBearer(0x31)), CapabilityTTL: 11 * time.Minute}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("long TTL error = %v", err)
	}
	store, err := previewmemory.New(previewmemory.Options{
		Clock: domain.NewFixedClock(time.Date(2035, 1, 2, 3, 4, 5, 0, time.UTC)),
		IDs:   domain.NewIDGenerator(bytes.NewReader(deterministicBytes(1 << 20))), Key: secret.Value(testBearer(0x32)),
		AllowedOrigin: "https://drive.example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{":%", "https://example.test", "http://127.0.0.1", "http://user@127.0.0.1:1234", "http://127.0.0.1:1234/path"} {
		if err := store.SetDataPlaneBaseURL(invalid); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("SetDataPlaneBaseURL(%q) error = %v", invalid, err)
		}
	}
	if err := store.Validate(context.Background()); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("Validate without origin error = %v", err)
	}
	binding := memoryTestBinding(t)
	if _, err := store.CreateDownload(context.Background(), binding); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("CreateDownload without origin error = %v", err)
	}
	server := httptest.NewServer(store)
	t.Cleanup(server.Close)
	if err := store.SetDataPlaneBaseURL(server.URL); err != nil {
		t.Fatal(err)
	}
	if err := store.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateDownload(context.Background(), binding); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing CreateDownload error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Validate(canceled); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled Validate error = %v", err)
	}
	artifact := memoryTestArtifact("boundary-generation", 256)
	if err := store.Commit(canceled, binding, artifact); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled Commit error = %v", err)
	}
	if _, err := store.Latest(canceled, binding); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled Latest error = %v", err)
	}
	if _, err := store.CreateDownload(canceled, binding); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled CreateDownload error = %v", err)
	}
	if err := store.Commit(context.Background(), preview.Binding{}, artifact); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid Commit error = %v", err)
	}
	if _, err := store.Latest(context.Background(), preview.Binding{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid Latest error = %v", err)
	}
	if _, err := store.CreateDownload(context.Background(), preview.Binding{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid CreateDownload error = %v", err)
	}
	if err := store.Commit(context.Background(), binding, artifact); err != nil {
		t.Fatal(err)
	}
	capability, err := store.CreateDownload(context.Background(), binding)
	if err != nil {
		t.Fatal(err)
	}
	requests := []struct {
		method string
		url    string
		origin string
		status int
	}{
		{method: http.MethodGet, url: server.URL + "/missing", status: http.StatusNotFound},
		{method: http.MethodGet, url: server.URL + "/cap/preview/unknown", status: http.StatusNotFound},
		{method: http.MethodPost, url: capability.URL, status: http.StatusMethodNotAllowed},
		{method: http.MethodGet, url: capability.URL, origin: "https://wrong.example.test", status: http.StatusForbidden},
		{method: http.MethodGet, url: capability.URL, origin: "https://drive.example.test", status: http.StatusOK},
	}
	for _, test := range requests {
		request, _ := http.NewRequest(test.method, test.url, nil)
		if test.origin != "" {
			request.Header.Set("Origin", test.origin)
		}
		response, requestErr := server.Client().Do(request)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		response.Body.Close()
		if response.StatusCode != test.status {
			t.Fatalf("%s %s status = %d, want %d", test.method, test.url, response.StatusCode, test.status)
		}
	}
	store.SetAvailable(false)
	if _, err := store.CreateDownload(context.Background(), binding); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("unavailable CreateDownload error = %v", err)
	}
	unavailableResponse, err := server.Client().Get(capability.URL)
	if err != nil {
		t.Fatal(err)
	}
	unavailableResponse.Body.Close()
	if unavailableResponse.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unavailable capability status = %d", unavailableResponse.StatusCode)
	}
	brokenIDs, err := previewmemory.New(previewmemory.Options{IDs: domain.NewIDGenerator(bytes.NewReader(nil)), Key: secret.Value(testBearer(0x33))})
	if err != nil {
		t.Fatal(err)
	}
	if err := brokenIDs.SetDataPlaneBaseURL(server.URL); err != nil {
		t.Fatal(err)
	}
	if err := brokenIDs.Validate(context.Background()); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("startup probe randomness error = %v", err)
	}
	exhaustedIDs, err := previewmemory.New(previewmemory.Options{IDs: domain.NewIDGenerator(bytes.NewReader(make([]byte, 48))), Key: secret.Value(testBearer(0x34))})
	if err != nil {
		t.Fatal(err)
	}
	if err := exhaustedIDs.SetDataPlaneBaseURL(server.URL); err != nil {
		t.Fatal(err)
	}
	if err := exhaustedIDs.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := exhaustedIDs.Commit(context.Background(), binding, artifact); err != nil {
		t.Fatal(err)
	}
	if _, err := exhaustedIDs.CreateDownload(context.Background(), binding); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("capability randomness error = %v", err)
	}
}

func memoryTestBinding(t *testing.T) preview.Binding {
	t.Helper()
	owner, err := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	return preview.Binding{Owner: owner, ContentID: "content", ContentVersion: "version", MediaType: "image/png", SourceSize: 1, RecipeID: "image-webp-q80-v1", Variant: 256}
}

func memoryTestArtifact(generationID string, variant int) preview.Artifact {
	data := preview.OnePixelWebP()
	sum := sha256.Sum256(data)
	return preview.Artifact{GenerationID: generationID, Variant: variant, Width: 1, Height: 1, ContentType: preview.ContentTypeWebP, Size: int64(len(data)), SHA256: base64.RawURLEncoding.EncodeToString(sum[:]), Bytes: data}
}

func testBearer(fill byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

func deterministicBytes(size int) []byte {
	value := make([]byte, size)
	for index := range value {
		value[index] = byte(index*29 + 7)
	}
	return value
}
