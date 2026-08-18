// Package storecontract defines the reusable preview-artifact store contract.
package storecontract

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"mime"
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
	Now          func() time.Time
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
		claim := claimGeneration(t, harness, binding, artifact.GenerationID)
		if err := harness.Store.Commit(context.Background(), binding, claim, artifact); err != nil {
			t.Fatalf("Commit() error = %v", err)
		}
		if err := harness.Store.Commit(context.Background(), binding, claim, artifact); err == nil {
			t.Fatal("Commit accepted an existing immutable generation")
		}
		stored, err := harness.Store.Latest(context.Background(), binding)
		if err != nil || stored.GenerationID != artifact.GenerationID {
			t.Fatalf("Latest() = %+v, %v", stored, err)
		}
		read, err := harness.Store.Read(context.Background(), binding, artifact.GenerationID)
		if err != nil || !bytes.Equal(read.Bytes, artifact.Bytes) {
			t.Fatalf("Read() = %+v, %v", read.Metadata(), err)
		}
		capability, err := harness.Store.CreateDownload(context.Background(), binding, artifact.GenerationID)
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
		disposition, parameters, dispositionErr := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
		if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "image/webp" || response.Header.Get("Cache-Control") != "no-store" || dispositionErr != nil || disposition != "inline" || parameters["filename"] != "preview.webp" || !bytes.Equal(body, artifact.Bytes) {
			t.Fatalf("artifact response = %d %q %v", response.StatusCode, body, response.Header)
		}
		second := testArtifact("generation-two", 256, preview.OnePixelWebP())
		if err := harness.Store.Commit(context.Background(), binding, claimGeneration(t, harness, binding, second.GenerationID), second); err != nil {
			t.Fatal(err)
		}
		latest, err := harness.Store.Latest(context.Background(), binding)
		if err != nil || latest.GenerationID != second.GenerationID {
			t.Fatalf("latest exact generation = %+v, %v", latest, err)
		}
		exactOld, err := harness.Store.CreateDownload(context.Background(), binding, artifact.GenerationID)
		if err != nil || exactOld.URL == "" {
			t.Fatalf("exact prior-generation capability = %+v, %v", exactOld, err)
		}
		if _, err := harness.Store.CreateDownload(context.Background(), binding, "missing-generation"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing exact generation error = %v", err)
		}
	})

	t.Run("owner and content bindings are isolated", func(t *testing.T) {
		harness := factory(t)
		if err := harness.Store.Validate(context.Background()); err != nil {
			t.Fatal(err)
		}
		binding := testBinding(t, 0x21)
		artifact := testArtifact("isolated-generation", 256, preview.OnePixelWebP())
		if err := harness.Store.Commit(context.Background(), binding, claimGeneration(t, harness, binding, artifact.GenerationID), artifact); err != nil {
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
		corruptArtifact := testArtifact("corrupt-generation", 256, corrupt)
		corruptClaim := claimGeneration(t, harness, binding, corruptArtifact.GenerationID)
		if err := harness.Store.Commit(context.Background(), binding, corruptClaim, corruptArtifact); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("corrupt Commit() error = %v", err)
		}
		if err := harness.Store.Release(context.Background(), binding, corruptClaim); err != nil {
			t.Fatal(err)
		}
		artifact := testArtifact("expiring-generation", 256, preview.OnePixelWebP())
		if err := harness.Store.Commit(context.Background(), binding, claimGeneration(t, harness, binding, artifact.GenerationID), artifact); err != nil {
			t.Fatal(err)
		}
		capability, err := harness.Store.CreateDownload(context.Background(), binding, artifact.GenerationID)
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

	t.Run("expired generation claims are fenced", func(t *testing.T) {
		harness := factory(t)
		if err := harness.Store.Validate(context.Background()); err != nil {
			t.Fatal(err)
		}
		binding := testBinding(t, 0x2a)
		first, err := harness.Store.Claim(context.Background(), binding, "first-claim", harness.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		contended, err := harness.Store.Claim(context.Background(), binding, "concurrent-claim", harness.Now().Add(time.Hour))
		if !errors.Is(err, domain.ErrConflict) || contended != first {
			t.Fatalf("concurrent Claim() = %+v, %v; want current claim %+v", contended, err, first)
		}
		harness.Advance(2 * time.Hour)
		second, err := harness.Store.Claim(context.Background(), binding, "takeover-claim", harness.Now().Add(time.Hour))
		if err != nil || second.Epoch <= first.Epoch {
			t.Fatalf("takeover Claim() = %+v, %v", second, err)
		}
		artifact := testArtifact("takeover-generation", 256, preview.OnePixelWebP())
		if err := harness.Store.Commit(context.Background(), binding, first, artifact); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("stale Commit() error = %v", err)
		}
		if err := harness.Store.Commit(context.Background(), binding, second, artifact); err != nil {
			t.Fatalf("takeover Commit() error = %v", err)
		}
		afterCommit, err := harness.Store.Claim(context.Background(), binding, "after-commit-claim", harness.Now().Add(time.Hour))
		if err != nil || afterCommit.Epoch <= second.Epoch {
			t.Fatalf("post-commit Claim() = %+v, %v", afterCommit, err)
		}
		if err := harness.Store.Release(context.Background(), binding, afterCommit); err != nil {
			t.Fatalf("Release() error = %v", err)
		}
		afterRelease, err := harness.Store.Claim(context.Background(), binding, "after-release-claim", harness.Now().Add(time.Hour))
		if err != nil || afterRelease.Epoch <= afterCommit.Epoch {
			t.Fatalf("post-release Claim() = %+v, %v", afterRelease, err)
		}
		if err := harness.Store.Release(context.Background(), binding, afterRelease); err != nil {
			t.Fatalf("final Release() error = %v", err)
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

func claimGeneration(t *testing.T, harness Harness, binding preview.Binding, claimID string) preview.GenerationClaim {
	t.Helper()
	claim, err := harness.Store.Claim(context.Background(), binding, claimID, harness.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return claim
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
		Size: int64(len(data)), SHA256: base64.RawURLEncoding.EncodeToString(sum[:]), CRC32C: preview.ChecksumCRC32C(data), Bytes: data,
	}
}
