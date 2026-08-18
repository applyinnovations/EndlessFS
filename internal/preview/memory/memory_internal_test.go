package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/preview"
	"github.com/applyinnovations/endlessfs/internal/secret"
)

type lockCheckingWriter struct {
	header http.Header
	store  *Store
	locked bool
}

func (w *lockCheckingWriter) Header() http.Header { return w.header }
func (*lockCheckingWriter) WriteHeader(int)       {}
func (w *lockCheckingWriter) Write(body []byte) (int, error) {
	if w.store.mu.TryLock() {
		w.store.mu.Unlock()
	} else {
		w.locked = true
	}
	return len(body), nil
}

func TestCapabilityResponseDoesNotWriteUnderStoreLock(t *testing.T) {
	store, capability := internalReadyStore(t)
	request := httptest.NewRequest(capability.Method, capability.URL, nil)
	writer := &lockCheckingWriter{header: make(http.Header), store: store}
	store.ServeHTTP(writer, request)
	if writer.locked {
		t.Fatal("preview artifact bytes were written while the global store lock was held")
	}
}

func TestValidationProbeBindingsAreUnique(t *testing.T) {
	first := validationBinding("first-generation")
	second := validationBinding("second-generation")
	if !first.Valid() || !second.Valid() || first.ContentID == second.ContentID {
		t.Fatalf("validation bindings are not unique: first=%+v second=%+v", first, second)
	}
}

func internalReadyStore(t *testing.T) (*Store, domain.DownloadCapability) {
	t.Helper()
	clock := domain.NewFixedClock(time.Date(2046, 1, 2, 3, 4, 5, 0, time.UTC))
	key := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x61}, 32))
	store, err := New(Options{Clock: clock, IDs: domain.NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x62}, 4096))), Key: secret.Value(key)})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetDataPlaneBaseURL("http://127.0.0.1:1234"); err != nil {
		t.Fatal(err)
	}
	owner, _ := domain.ParseUserID("AAAAAAAAAAAAAAAAAAAAAA")
	binding := preview.Binding{Owner: owner, ContentID: "content", ContentVersion: "version", MediaType: "image/png", SourceSize: 1, RecipeID: "image-webp-q80-v1", Variant: 256}
	data := preview.OnePixelWebP()
	sum := sha256.Sum256(data)
	artifact := preview.Artifact{GenerationID: "generation", Variant: 256, Width: 1, Height: 1, ContentType: preview.ContentTypeWebP, Size: int64(len(data)), SHA256: base64.RawURLEncoding.EncodeToString(sum[:]), CRC32C: preview.ChecksumCRC32C(data), Bytes: data}
	claim, err := store.Claim(context.Background(), binding, artifact.GenerationID, clock.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(context.Background(), binding, claim, artifact); err != nil {
		t.Fatal(err)
	}
	capability, err := store.CreateDownload(context.Background(), binding, artifact.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	return store, capability
}
