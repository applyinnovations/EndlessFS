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
		return storecontract.Harness{Store: store, Client: server.Client(), SetAvailable: store.SetAvailable, Advance: clock.Advance, Now: clock.Now}
	})
}

func TestMemoryPreviewStoreBoundaryFailures(t *testing.T) {
	if _, err := previewmemory.New(previewmemory.Options{Key: secret.Value("invalid")}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("weak key error = %v", err)
	}
	if _, err := previewmemory.New(previewmemory.Options{Key: secret.Value(testBearer(0x31)), CapabilityTTL: 11 * time.Minute}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("long TTL error = %v", err)
	}
	clock := domain.NewFixedClock(time.Date(2035, 1, 2, 3, 4, 5, 0, time.UTC))
	store, err := previewmemory.New(previewmemory.Options{
		Clock: clock,
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
	if _, err := store.CreateDownload(context.Background(), binding, "missing"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("CreateDownload without origin error = %v", err)
	}
	server := httptest.NewServer(store)
	t.Cleanup(server.Close)
	if err := store.SetDataPlaneBaseURL(server.URL); err != nil {
		t.Fatal(err)
	}
	if store.DataOrigin() != server.URL {
		t.Fatalf("DataOrigin() = %q", store.DataOrigin())
	}
	if err := store.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateDownload(context.Background(), binding, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing CreateDownload error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Validate(canceled); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled Validate error = %v", err)
	}
	artifact := memoryTestArtifact("boundary-generation", 256)
	claim := memoryClaim(t, store, binding, artifact.GenerationID, clock.Now())
	if err := store.Commit(canceled, binding, claim, artifact); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled Commit error = %v", err)
	}
	if _, err := store.Latest(canceled, binding); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled Latest error = %v", err)
	}
	if _, err := store.CreateDownload(canceled, binding, artifact.GenerationID); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled CreateDownload error = %v", err)
	}
	if err := store.Commit(context.Background(), preview.Binding{}, claim, artifact); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid Commit error = %v", err)
	}
	if _, err := store.Latest(context.Background(), preview.Binding{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid Latest error = %v", err)
	}
	if _, err := store.CreateDownload(context.Background(), preview.Binding{}, artifact.GenerationID); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid CreateDownload error = %v", err)
	}
	if err := store.Commit(context.Background(), binding, claim, artifact); err != nil {
		t.Fatal(err)
	}
	capability, err := store.CreateDownload(context.Background(), binding, artifact.GenerationID)
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
	if _, err := store.CreateDownload(context.Background(), binding, artifact.GenerationID); !errors.Is(err, domain.ErrUnavailable) {
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
	exhaustedClaim, err := exhaustedIDs.Claim(context.Background(), binding, artifact.GenerationID, domain.SystemClock{}.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := exhaustedIDs.Commit(context.Background(), binding, exhaustedClaim, artifact); err != nil {
		t.Fatal(err)
	}
	if _, err := exhaustedIDs.CreateDownload(context.Background(), binding, artifact.GenerationID); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("capability randomness error = %v", err)
	}
}

func TestMemoryPreviewStoreClaimsRetentionAndCapacityBoundaries(t *testing.T) {
	clock := domain.NewFixedClock(time.Date(2046, 1, 2, 3, 4, 5, 0, time.UTC))
	store, err := previewmemory.New(previewmemory.Options{
		Clock: clock, IDs: domain.NewIDGenerator(bytes.NewReader(deterministicBytes(1 << 20))), Key: secret.Value(testBearer(0x45)),
		CapabilityTTL: time.Minute, MaxGenerations: 1, MaxCapabilities: 1, MaxBindings: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Check(canceled); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled Check() error = %v", err)
	}
	if err := store.Check(context.Background()); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("unconfigured Check() error = %v", err)
	}
	server := httptest.NewServer(store)
	t.Cleanup(server.Close)
	if err := store.SetDataPlaneBaseURL(server.URL); err != nil {
		t.Fatal(err)
	}
	if err := store.Validate(context.Background()); err != nil || store.Check(context.Background()) != nil {
		t.Fatalf("validated Check() error = %v", err)
	}

	binding := memoryTestBinding(t)
	if _, err := store.Claim(canceled, binding, "claim", clock.Now().Add(time.Hour)); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled Claim() error = %v", err)
	}
	for _, request := range []struct {
		binding preview.Binding
		id      string
		expires time.Time
	}{{preview.Binding{}, "claim", clock.Now().Add(time.Hour)}, {binding, "", clock.Now().Add(time.Hour)}, {binding, "claim", clock.Now()}} {
		if _, err := store.Claim(context.Background(), request.binding, request.id, request.expires); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid Claim(%q) error = %v", request.id, err)
		}
	}
	firstClaim := memoryClaim(t, store, binding, "first-claim", clock.Now())
	releasedEpoch := firstClaim.Epoch
	if _, err := store.Claim(context.Background(), binding, "concurrent-claim", clock.Now().Add(time.Hour)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("concurrent Claim() error = %v", err)
	}
	if err := store.Release(canceled, binding, firstClaim); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled Release() error = %v", err)
	}
	if err := store.Release(context.Background(), preview.Binding{}, firstClaim); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid Release() error = %v", err)
	}
	staleClaim := firstClaim
	staleClaim.Epoch++
	if err := store.Release(context.Background(), binding, staleClaim); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale Release() error = %v", err)
	}
	store.SetAvailable(false)
	if err := store.Release(context.Background(), binding, firstClaim); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("unavailable Release() error = %v", err)
	}
	if _, err := store.Read(context.Background(), binding, "missing"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("unavailable Read() error = %v", err)
	}
	store.SetAvailable(true)
	if err := store.Release(context.Background(), binding, firstClaim); err != nil {
		t.Fatal(err)
	}

	firstArtifact := memoryTestArtifact("first-generation", 256)
	firstClaim = memoryClaim(t, store, binding, firstArtifact.GenerationID, clock.Now())
	if firstClaim.Epoch <= releasedEpoch {
		t.Fatalf("claim epoch did not advance after release: %d", firstClaim.Epoch)
	}
	if err := store.Commit(context.Background(), binding, firstClaim, firstArtifact); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(canceled, binding, firstArtifact.GenerationID); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled Read() error = %v", err)
	}
	if _, err := store.Read(context.Background(), preview.Binding{}, firstArtifact.GenerationID); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid Read() error = %v", err)
	}
	if _, err := store.Read(context.Background(), binding, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing Read() error = %v", err)
	}
	if _, err := store.CreateDownload(context.Background(), binding, firstArtifact.GenerationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateDownload(context.Background(), binding, firstArtifact.GenerationID); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("capability capacity error = %v", err)
	}

	duplicateClaim := memoryClaim(t, store, binding, "duplicate-claim", clock.Now())
	if duplicateClaim.Epoch <= firstClaim.Epoch {
		t.Fatalf("claim epoch did not advance after commit: %d", duplicateClaim.Epoch)
	}
	if err := store.Commit(context.Background(), binding, duplicateClaim, firstArtifact); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate Commit() error = %v", err)
	}
	if err := store.Release(context.Background(), binding, duplicateClaim); err != nil {
		t.Fatal(err)
	}
	secondArtifact := memoryTestArtifact("second-generation", 256)
	secondClaim := memoryClaim(t, store, binding, secondArtifact.GenerationID, clock.Now())
	if err := store.Commit(context.Background(), binding, secondClaim, secondArtifact); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("protected retention Commit() error = %v", err)
	}
	if err := store.Release(context.Background(), binding, secondClaim); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute + time.Second)
	secondClaim = memoryClaim(t, store, binding, secondArtifact.GenerationID, clock.Now())
	if err := store.Commit(context.Background(), binding, secondClaim, secondArtifact); err != nil {
		t.Fatal(err)
	}
	if latest, err := store.Latest(context.Background(), binding); err != nil || latest.GenerationID != secondArtifact.GenerationID {
		t.Fatalf("retained Latest() = %+v, %v", latest, err)
	}
	if _, err := store.Read(context.Background(), binding, firstArtifact.GenerationID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("pruned Read() error = %v", err)
	}

	other := binding
	other.ContentID = "other-content"
	if _, err := store.Claim(context.Background(), other, "other-claim", clock.Now().Add(time.Hour)); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("binding capacity error = %v", err)
	}
	expiredClaim := preview.GenerationClaim{ID: "expired", Epoch: 1, ExpiresAt: clock.Now()}
	if err := store.Commit(context.Background(), binding, expiredClaim, secondArtifact); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("expired Commit() error = %v", err)
	}
}

func TestMemoryPreviewStoreBoundsTotalArtifactBytes(t *testing.T) {
	clock := domain.NewFixedClock(time.Date(2046, 1, 2, 3, 4, 5, 0, time.UTC))
	artifact := memoryTestArtifact("bounded-generation", 256)
	store, err := previewmemory.New(previewmemory.Options{
		Clock: clock, IDs: domain.NewIDGenerator(bytes.NewReader(deterministicBytes(1 << 20))), Key: secret.Value(testBearer(0x46)),
		MaxArtifactBytes: artifact.Size,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(store)
	t.Cleanup(server.Close)
	if err := store.SetDataPlaneBaseURL(server.URL); err != nil {
		t.Fatal(err)
	}
	if err := store.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	binding := memoryTestBinding(t)
	if err := store.Commit(context.Background(), binding, memoryClaim(t, store, binding, artifact.GenerationID, clock.Now()), artifact); err != nil {
		t.Fatal(err)
	}
	other := binding
	other.ContentID = "other-content"
	otherArtifact := memoryTestArtifact("other-generation", 256)
	if err := store.Commit(context.Background(), other, memoryClaim(t, store, other, otherArtifact.GenerationID, clock.Now()), otherArtifact); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("artifact byte capacity error = %v", err)
	}
}

func memoryClaim(t *testing.T, store *previewmemory.Store, binding preview.Binding, claimID string, now time.Time) preview.GenerationClaim {
	t.Helper()
	claim, err := store.Claim(context.Background(), binding, claimID, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return claim
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
