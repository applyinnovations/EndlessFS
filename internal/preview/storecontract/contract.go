// Package storecontract defines the reusable preview-artifact store contract.
package storecontract

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/preview"
)

type Harness struct {
	Store        preview.Store
	Client       *http.Client
	SetAvailable func(bool)
	Advance      func(time.Duration)
}

type Factory func(*testing.T) Harness

func Run(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("startup probe and immutable exact capabilities", func(t *testing.T) {
		harness := factory(t)
		if err := harness.Store.Validate(context.Background()); err != nil {
			t.Fatalf("Validate() error = %v", err)
		}
		binding := testBinding(t, 0x11)
		artifact := testArtifact("generation-one", 256, preview.OnePixelWebP())
		if err := harness.Store.Commit(context.Background(), binding, artifact); err != nil {
			t.Fatalf("Commit() error = %v", err)
		}
		if err := harness.Store.Commit(context.Background(), binding, artifact); err == nil {
			t.Fatal("Commit accepted an existing immutable generation")
		}
		stored, err := harness.Store.Latest(context.Background(), binding)
		if err != nil || stored.GenerationID != artifact.GenerationID || !bytes.Equal(stored.Bytes, artifact.Bytes) {
			t.Fatalf("Latest() = %+v, %v", stored.Metadata(), err)
		}
		capability, err := harness.Store.CreateDownload(context.Background(), binding)
		if err != nil {
			t.Fatal(err)
		}
		response, err := harness.Client.Get(capability.URL)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("artifact response body errors = read %v, close %v", readErr, closeErr)
		}
		if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "image/webp" || response.Header.Get("Cache-Control") != "no-store" || !bytes.Equal(body, artifact.Bytes) {
			t.Fatalf("artifact response = %d %q %v", response.StatusCode, body, response.Header)
		}
	})

	t.Run("owner and content bindings are isolated", func(t *testing.T) {
		harness := factory(t)
		if err := harness.Store.Validate(context.Background()); err != nil {
			t.Fatal(err)
		}
		binding := testBinding(t, 0x21)
		if err := harness.Store.Commit(context.Background(), binding, testArtifact("isolated-generation", 256, preview.OnePixelWebP())); err != nil {
			t.Fatal(err)
		}
		otherOwner := testBinding(t, 0x22)
		otherOwner.ContentID = binding.ContentID
		otherOwner.ContentVersion = binding.ContentVersion
		if _, err := harness.Store.Latest(context.Background(), otherOwner); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("other-owner Latest() error = %v", err)
		}
		otherContent := binding
		otherContent.ContentVersion = "different-content-version"
		if _, err := harness.Store.Latest(context.Background(), otherContent); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("other-content Latest() error = %v", err)
		}
	})

	t.Run("corrupt WebP and expired capabilities fail closed", func(t *testing.T) {
		harness := factory(t)
		if err := harness.Store.Validate(context.Background()); err != nil {
			t.Fatal(err)
		}
		binding := testBinding(t, 0x29)
		corrupt := preview.OnePixelWebP()
		corrupt[51] ^= 0xff
		if err := harness.Store.Commit(context.Background(), binding, testArtifact("corrupt-generation", 256, corrupt)); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("corrupt Commit() error = %v", err)
		}
		artifact := testArtifact("expiring-generation", 256, preview.OnePixelWebP())
		if err := harness.Store.Commit(context.Background(), binding, artifact); err != nil {
			t.Fatal(err)
		}
		capability, err := harness.Store.CreateDownload(context.Background(), binding)
		if err != nil {
			t.Fatal(err)
		}
		harness.Advance(time.Hour)
		response, err := harness.Client.Get(capability.URL)
		if err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatalf("expired response close error = %v", err)
		}
		if response.StatusCode != http.StatusGone {
			t.Fatalf("expired capability status = %d", response.StatusCode)
		}
	})

	t.Run("runtime loss fails readiness and bounded revalidation recovers", func(t *testing.T) {
		harness := factory(t)
		if err := harness.Store.Validate(context.Background()); err != nil || !harness.Store.Ready() {
			t.Fatalf("initial readiness = %v, %v", harness.Store.Ready(), err)
		}
		harness.SetAvailable(false)
		if _, err := harness.Store.Latest(context.Background(), testBinding(t, 0x31)); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("unavailable Latest() error = %v", err)
		}
		if harness.Store.Ready() {
			t.Fatal("store remained ready after runtime access loss")
		}
		harness.SetAvailable(true)
		if harness.Store.Ready() {
			t.Fatal("availability change bypassed explicit revalidation")
		}
		if err := harness.Store.Validate(context.Background()); err != nil || !harness.Store.Ready() {
			t.Fatalf("revalidation = %v, ready %v", err, harness.Store.Ready())
		}
	})
}

func testBinding(t *testing.T, fill byte) preview.Binding {
	t.Helper()
	owner, err := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	return preview.Binding{
		Owner: owner, ContentID: "content-id", ContentVersion: "content-version", MediaType: "image/png",
		SourceSize: 128, RecipeID: "image-webp-q80-v1", Variant: 256,
	}
}

func testArtifact(generationID string, variant int, data []byte) preview.Artifact {
	sum := sha256.Sum256(data)
	return preview.Artifact{
		GenerationID: generationID, Variant: variant, Width: 1, Height: 1, ContentType: "image/webp",
		Size: int64(len(data)), SHA256: base64.RawURLEncoding.EncodeToString(sum[:]), Bytes: data,
	}
}
