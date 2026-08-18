package durable_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/preview"
	"github.com/applyinnovations/endlessfs/internal/preview/durable"
	"github.com/applyinnovations/endlessfs/internal/preview/storecontract"
	"github.com/applyinnovations/endlessfs/internal/secret"
)

func TestContractDurablePreviewStoreOverObjectBackend(t *testing.T) {
	storecontract.Run(t, func(t *testing.T) storecontract.Harness {
		clock := domain.NewFixedClock(time.Date(2035, 1, 2, 3, 4, 5, 0, time.UTC))
		backend := objectmemory.New()
		server := httptest.NewServer(backend)
		t.Cleanup(server.Close)
		ids := domain.NewIDGenerator(bytes.NewReader(deterministicBytes(4 << 20)))
		if err := backend.ConfigureDataPlane(server.URL, clock, ids); err != nil {
			t.Fatal(err)
		}
		faultable := &faultBackend{Backend: backend, DirectTransferBackend: backend, available: true}
		store, err := durable.New(durable.Options{
			Backend: faultable, Transfers: faultable, Clock: clock, IDs: ids,
			Key: secret.Value(testBearer(0x51)), CapabilityTTL: time.Minute,
			DataOrigin: server.URL, HTTPClient: server.Client(), AllowedOrigin: "https://drive.example.test",
		})
		if err != nil {
			t.Fatal(err)
		}
		return storecontract.Harness{
			Store: store, Client: server.Client(), Advance: clock.Advance, Now: clock.Now,
			SetAvailable: faultable.SetAvailable,
		}
	})
}

func TestDurablePreviewClaimsFenceAcrossReplicas(t *testing.T) {
	clock := domain.NewFixedClock(time.Date(2035, 1, 2, 3, 4, 5, 0, time.UTC))
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	ids := domain.NewIDGenerator(bytes.NewReader(deterministicBytes(8 << 20)))
	if err := backend.ConfigureDataPlane(server.URL, clock, ids); err != nil {
		t.Fatal(err)
	}
	newStore := func() *durable.Store {
		store, err := durable.New(durable.Options{
			Backend: backend, Transfers: backend, Clock: clock, IDs: ids,
			Key: secret.Value(testBearer(0x61)), CapabilityTTL: time.Minute,
			DataOrigin: server.URL, HTTPClient: server.Client(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	first, second := newStore(), newStore()
	binding := testBinding(t)

	var claims [2]preview.GenerationClaim
	var claimErrors [2]error
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		claims[0], claimErrors[0] = first.Claim(context.Background(), binding, "replica-one", clock.Now().Add(time.Minute))
	}()
	go func() {
		defer wait.Done()
		claims[1], claimErrors[1] = second.Claim(context.Background(), binding, "replica-two", clock.Now().Add(time.Minute))
	}()
	wait.Wait()
	winners := 0
	for _, err := range claimErrors {
		if err == nil {
			winners++
		} else if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("Claim() error = %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("claim winners = %d, errors = %v", winners, claimErrors)
	}

	winningClaim := claims[0]
	winningStore := first
	staleStore := second
	if claimErrors[1] == nil {
		winningClaim, winningStore, staleStore = claims[1], second, first
	}
	clock.Advance(2 * time.Minute)
	takeover, err := staleStore.Claim(context.Background(), binding, "takeover", clock.Now().Add(time.Minute))
	if err != nil || takeover.Epoch <= winningClaim.Epoch {
		t.Fatalf("takeover = %+v, %v", takeover, err)
	}
	artifact := testArtifact("takeover", binding.Variant)
	if err := winningStore.Commit(context.Background(), binding, winningClaim, artifact); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale Commit() error = %v", err)
	}
	if err := staleStore.Commit(context.Background(), binding, takeover, artifact); err != nil {
		t.Fatalf("takeover Commit() error = %v", err)
	}
}

type faultBackend struct {
	objectstore.Backend
	objectstore.DirectTransferBackend
	mu        sync.RWMutex
	available bool
}

func (b *faultBackend) SetAvailable(available bool) {
	b.mu.Lock()
	b.available = available
	b.mu.Unlock()
}

func (b *faultBackend) allowed() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.available
}

func (b *faultBackend) unavailable() error {
	return domain.NewError(domain.ErrorUnavailable, "injected object backend outage")
}

func (b *faultBackend) Head(ctx context.Context, key objectstore.Key) (objectstore.ObjectInfo, error) {
	if !b.allowed() {
		return objectstore.ObjectInfo{}, b.unavailable()
	}
	return b.Backend.Head(ctx, key)
}

func (b *faultBackend) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	if !b.allowed() {
		return objectstore.Object{}, b.unavailable()
	}
	return b.Backend.Get(ctx, key)
}

func (b *faultBackend) List(ctx context.Context, request objectstore.ListRequest) (objectstore.ListPage, error) {
	if !b.allowed() {
		return objectstore.ListPage{}, b.unavailable()
	}
	return b.Backend.List(ctx, request)
}

func (b *faultBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	if !b.allowed() {
		return "", b.unavailable()
	}
	return b.Backend.Put(ctx, key, body, condition)
}

func (b *faultBackend) Delete(ctx context.Context, key objectstore.Key, condition objectstore.DeleteCondition) error {
	if !b.allowed() {
		return b.unavailable()
	}
	return b.Backend.Delete(ctx, key, condition)
}

func (b *faultBackend) Copy(ctx context.Context, source, destination objectstore.Key, condition objectstore.CopyCondition) (objectstore.CopyResult, error) {
	if !b.allowed() {
		return objectstore.CopyResult{}, b.unavailable()
	}
	return b.Backend.Copy(ctx, source, destination, condition)
}

func (b *faultBackend) CreateDownload(ctx context.Context, request objectstore.DownloadRequest) (objectstore.DownloadCapability, error) {
	if !b.allowed() {
		return objectstore.DownloadCapability{}, b.unavailable()
	}
	return b.DirectTransferBackend.CreateDownload(ctx, request)
}

func testBinding(t *testing.T) preview.Binding {
	t.Helper()
	owner, err := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x71}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	return preview.Binding{Owner: owner, ContentID: "content-id", ContentVersion: "content-version", MediaType: "image/png", SourceSize: 128, RecipeID: "image-webp-q80-v1", Variant: 256}
}

func testArtifact(generationID string, variant int) preview.Artifact {
	data := preview.OnePixelWebP()
	sum := sha256.Sum256(data)
	return preview.Artifact{GenerationID: generationID, Variant: variant, Width: 1, Height: 1, ContentType: preview.ContentTypeWebP, Size: int64(len(data)), SHA256: base64.RawURLEncoding.EncodeToString(sum[:]), Bytes: data}
}

func testBearer(fill byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

func deterministicBytes(size int) []byte {
	result := make([]byte, size)
	for index := range result {
		result[index] = byte(index*31 + 17)
	}
	return result
}

var _ http.Handler = (*objectmemory.Backend)(nil)
