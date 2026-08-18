package durable

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/preview"
	"github.com/applyinnovations/endlessfs/internal/secret"
)

func TestDurableStoreBoundaryAndRetentionMatrix(t *testing.T) {
	valid := internalOptions(t, nil, 2)
	invalid := []Options{
		{},
		func() Options { value := valid; value.Backend = nil; return value }(),
		func() Options { value := valid; value.Transfers = nil; return value }(),
		func() Options { value := valid; value.Key = secret.Value("invalid"); return value }(),
		func() Options { value := valid; value.CapabilityTTL = 11 * time.Minute; return value }(),
		func() Options { value := valid; value.DataOrigin = ""; return value }(),
		func() Options { value := valid; value.MaxGenerations = 33; return value }(),
		func() Options { value := valid; value.MaxArtifactBytes = 1; return value }(),
	}
	for index, options := range invalid {
		if _, err := New(options); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid options %d error = %v", index, err)
		}
	}

	store, err := New(valid)
	if err != nil {
		t.Fatal(err)
	}
	if store.DataOrigin() == "" {
		t.Fatal("DataOrigin() is empty")
	}
	if err := store.Validate(context.Background()); err != nil || !store.Ready() {
		t.Fatalf("Validate() = %v, ready=%v", err, store.Ready())
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Check(canceled); !errors.Is(err, domain.ErrUnavailable) || store.Ready() {
		t.Fatalf("canceled Check() = %v, ready=%v", err, store.Ready())
	}
	if err := store.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}

	binding := internalBinding(t)
	artifact := internalArtifact("generation-one", binding.Variant)
	invalidClaim := preview.GenerationClaim{}
	for name, run := range map[string]func() error{
		"claim canceled": func() error {
			_, err := store.Claim(canceled, binding, "claim", valid.Clock.Now().Add(time.Minute))
			return err
		},
		"claim binding": func() error {
			_, err := store.Claim(context.Background(), preview.Binding{}, "claim", valid.Clock.Now().Add(time.Minute))
			return err
		},
		"claim id": func() error {
			_, err := store.Claim(context.Background(), binding, "bad/id", valid.Clock.Now().Add(time.Minute))
			return err
		},
		"release canceled": func() error { return store.Release(canceled, binding, internalClaim("claim", 1, valid.Clock.Now())) },
		"release invalid":  func() error { return store.Release(context.Background(), binding, invalidClaim) },
		"commit canceled": func() error {
			return store.Commit(canceled, binding, internalClaim("claim", 1, valid.Clock.Now()), artifact)
		},
		"commit invalid":  func() error { return store.Commit(context.Background(), binding, invalidClaim, artifact) },
		"latest canceled": func() error { _, err := store.Latest(canceled, binding); return err },
		"latest invalid":  func() error { _, err := store.Latest(context.Background(), preview.Binding{}); return err },
		"read canceled":   func() error { _, err := store.Read(canceled, binding, artifact.GenerationID); return err },
		"read invalid": func() error {
			_, err := store.Read(context.Background(), preview.Binding{}, artifact.GenerationID)
			return err
		},
		"download canceled": func() error { _, err := store.CreateDownload(canceled, binding, artifact.GenerationID); return err },
		"download invalid": func() error {
			_, err := store.CreateDownload(context.Background(), preview.Binding{}, artifact.GenerationID)
			return err
		},
	} {
		err := run()
		if err == nil || (strings.Contains(name, "canceled") && !errors.Is(err, domain.ErrUnavailable)) || (!strings.Contains(name, "canceled") && !errors.Is(err, domain.ErrInvalid)) {
			t.Fatalf("%s error = %v", name, err)
		}
	}

	for index, generationID := range []string{"generation-one", "generation-two", "generation-three"} {
		claim, claimErr := store.Claim(context.Background(), binding, "claim-"+generationID, valid.Clock.Now().Add(time.Minute))
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		if index == 0 {
			wrong := claim
			wrong.Epoch++
			if err := store.Release(context.Background(), binding, wrong); !errors.Is(err, domain.ErrPreconditionFailed) {
				t.Fatalf("wrong Release() error = %v", err)
			}
		}
		if err := store.Commit(context.Background(), binding, claim, internalArtifact(generationID, binding.Variant)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Read(context.Background(), binding, "generation-one"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("evicted generation error = %v", err)
	}
	latest, err := store.Latest(context.Background(), binding)
	if err != nil || latest.GenerationID != "generation-three" {
		t.Fatalf("Latest() = %+v, %v", latest, err)
	}
}

func TestDurableStoreCorruptionLifecycleAndOpaqueLayout(t *testing.T) {
	options := internalOptions(t, nil, 4)
	store, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	binding := internalBinding(t)
	artifact := internalArtifact("opaque-generation", binding.Variant)
	claim, err := store.Claim(context.Background(), binding, "opaque-claim", options.Clock.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(context.Background(), binding, claim, artifact); err != nil {
		t.Fatal(err)
	}

	backend := options.Backend.(*objectmemory.Backend)
	digest := store.bindingDigest(binding)
	generationDigest := store.generationDigest(digest, artifact.GenerationID)
	for _, key := range []objectstore.Key{headKey(digest), generationManifestKey(digest, generationDigest), generationArtifactKey(digest, generationDigest)} {
		object, getErr := backend.Get(context.Background(), key)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if strings.Contains(key.String(), binding.Owner.String()) || strings.Contains(key.String(), string(binding.ContentID)) || strings.Contains(string(object.Body), binding.Owner.String()) || strings.Contains(string(object.Body), string(binding.ContentID)) {
			t.Fatalf("preview layout exposed a source identity: %s", key.String())
		}
	}

	artifactKey := generationArtifactKey(digest, generationDigest)
	artifactInfo, _ := backend.Head(context.Background(), artifactKey)
	if err := backend.Delete(context.Background(), artifactKey, objectstore.DeleteCondition{Version: artifactInfo.Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(context.Background(), binding, artifact.GenerationID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("lifecycle deletion Read() error = %v", err)
	}
	if _, err := store.Latest(context.Background(), binding); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("lifecycle deletion Latest() error = %v", err)
	}
	if _, err := backend.Put(context.Background(), artifactKey, artifact.Bytes, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}

	manifestKey := generationManifestKey(digest, generationDigest)
	manifestInfo, _ := backend.Head(context.Background(), manifestKey)
	if _, err := backend.Put(context.Background(), manifestKey, []byte(`{"schemaVersion":1}`), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: manifestInfo.Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Latest(context.Background(), binding); !errors.Is(err, domain.ErrUnavailable) || store.Ready() {
		t.Fatalf("corrupt manifest Latest() = %v, ready=%v", err, store.Ready())
	}

	headInfo, _ := backend.Head(context.Background(), headKey(digest))
	if _, err := backend.Put(context.Background(), headKey(digest), []byte(`{"schemaVersion":2}`), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: headInfo.Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Latest(context.Background(), binding); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("corrupt head Latest() error = %v", err)
	}
}

func TestDurableStoreLostSuccessAndImmutableRecovery(t *testing.T) {
	base := objectmemory.New()
	options := internalOptions(t, base, 4)
	faults := &putFaultBackend{Backend: base, DirectTransferBackend: base}
	options.Backend, options.Transfers = faults, faults
	store, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	binding := internalBinding(t)
	claim, err := store.Claim(context.Background(), binding, "retry-claim", options.Clock.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	artifact := internalArtifact("retry-generation", binding.Variant)
	faults.reset(1)
	if err := store.Commit(context.Background(), binding, claim, artifact); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("lost artifact success error = %v", err)
	}
	faults.reset(0)
	if err := store.Commit(context.Background(), binding, claim, artifact); err != nil {
		t.Fatalf("artifact retry error = %v", err)
	}

	secondClaim, err := store.Claim(context.Background(), binding, "head-recovery-claim", options.Clock.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	faults.reset(3)
	if err := store.Commit(context.Background(), binding, secondClaim, internalArtifact("head-recovery-generation", binding.Variant)); err != nil {
		t.Fatalf("lost head success recovery error = %v", err)
	}

	racingClaim, err := store.Claim(context.Background(), binding, "racing-head-recovery-claim", options.Clock.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var laterClaim preview.GenerationClaim
	faults.afterLostSuccess = func() {
		var claimErr error
		laterClaim, claimErr = store.Claim(context.Background(), binding, "later-claim", options.Clock.Now().Add(time.Minute))
		if claimErr != nil {
			t.Errorf("later Claim() error = %v", claimErr)
		}
	}
	faults.reset(3)
	if err := store.Commit(context.Background(), binding, racingClaim, internalArtifact("racing-head-recovery-generation", binding.Variant)); err != nil {
		t.Fatalf("lost head success with a later claim error = %v", err)
	}
	faults.afterLostSuccess = nil
	if err := store.Release(context.Background(), binding, laterClaim); err != nil {
		t.Fatalf("release later claim = %v", err)
	}

	thirdClaim, err := store.Claim(context.Background(), binding, "conflict-claim", options.Clock.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	third := internalArtifact("conflict-generation", binding.Variant)
	digest := store.bindingDigest(binding)
	key := generationArtifactKey(digest, store.generationDigest(digest, third.GenerationID))
	if _, err := base.Put(context.Background(), key, []byte("different"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	faults.reset(0)
	if err := store.Commit(context.Background(), binding, thirdClaim, third); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("immutable mismatch error = %v", err)
	}
}

func TestDurableRecordAndValidationFailureBoundaries(t *testing.T) {
	binding := internalBinding(t)
	metadata := internalArtifact("generation", binding.Variant).Metadata()
	valid := headRecord{SchemaVersion: schemaVersion, BindingDigest: strings.Repeat("a", 64), Epoch: 1, Generations: []preview.ArtifactMetadata{metadata}}
	invalid := []headRecord{
		{},
		func() headRecord { value := valid; value.SchemaVersion = 2; return value }(),
		func() headRecord { value := valid; value.Epoch = 0; return value }(),
		func() headRecord { value := valid; value.ClaimID = "claim"; return value }(),
		func() headRecord {
			value := valid
			value.ClaimID = "bad/id"
			value.ClaimExpires = time.Now()
			return value
		}(),
		func() headRecord {
			value := valid
			value.Generations = append(value.Generations, metadata)
			return value
		}(),
	}
	for index, record := range invalid {
		if record.valid(binding, valid.BindingDigest, 4) {
			t.Fatalf("invalid record %d accepted", index)
		}
	}
	for _, data := range [][]byte{[]byte(`{`), []byte(`{"unknown":true}`), append(encode(valid), []byte(` {}`)...), bytes.Repeat([]byte(" "), int(maximumRecordBytes+1))} {
		var record headRecord
		if decode(data, &record) == nil {
			t.Fatalf("invalid record decoded: %.32q", data)
		}
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("encode did not panic for an unsupported fixed record")
			}
		}()
		_ = encode(make(chan int))
	}()

	badIDs := internalOptions(t, nil, 4)
	badIDs.IDs = domain.NewIDGenerator(bytes.NewReader(nil))
	store, err := New(badIDs)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Validate(context.Background()); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("identity validation error = %v", err)
	}

	for name, response := range map[string]*http.Response{
		"status":             {StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": {preview.ContentTypeWebP}, "Content-Disposition": {"inline"}, "Cache-Control": {"no-store"}}, Body: io.NopCloser(bytes.NewReader(preview.OnePixelWebP()))},
		"type":               {StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/html"}, "Content-Disposition": {"inline"}, "Cache-Control": {"no-store"}}, Body: io.NopCloser(bytes.NewReader(preview.OnePixelWebP()))},
		"disposition":        {StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {preview.ContentTypeWebP}, "Cache-Control": {"no-store"}}, Body: io.NopCloser(bytes.NewReader(preview.OnePixelWebP()))},
		"unsafe disposition": {StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {preview.ContentTypeWebP}, "Content-Disposition": {"inline-unsafe"}, "Cache-Control": {"no-store"}}, Body: io.NopCloser(bytes.NewReader(preview.OnePixelWebP()))},
		"cache":              {StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {preview.ContentTypeWebP}, "Content-Disposition": {"inline; filename=preview.webp"}}, Body: io.NopCloser(bytes.NewReader(preview.OnePixelWebP()))},
		"body":               {StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {preview.ContentTypeWebP}, "Content-Disposition": {"inline; filename=preview.webp"}, "Cache-Control": {"no-store"}}, Body: io.NopCloser(strings.NewReader("wrong"))},
	} {
		response := response
		options := internalOptions(t, nil, 4)
		options.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response, nil })}
		store, err := New(options)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Validate(context.Background()); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("%s validation error = %v", name, err)
		}
	}

	options := internalOptions(t, nil, 4)
	options.AllowedOrigin = "https://drive.example.test"
	options.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return validationHTTPResponse(io.NopCloser(bytes.NewReader(preview.OnePixelWebP()))), nil
	})}
	store, err = New(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Validate(context.Background()); !errors.Is(err, domain.ErrUnavailable) || !strings.Contains(err.Error(), "capability origin") {
		t.Fatalf("missing exact CORS origin validation error = %v", err)
	}
}

func TestDurableBackendFailureAndStoredGenerationMatrix(t *testing.T) {
	base := objectmemory.New()
	options := internalOptions(t, base, 4)
	faults := &operationFaultBackend{Backend: base, DirectTransferBackend: base}
	options.Backend, options.Transfers = faults, faults
	store, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	binding := internalBinding(t)
	artifact := internalArtifact("failure-generation", binding.Variant)
	claim, err := store.Claim(context.Background(), binding, "failure-claim", options.Clock.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(context.Background(), binding, claim, artifact); err != nil {
		t.Fatal(err)
	}
	digest := store.bindingDigest(binding)
	generationDigest := store.generationDigest(digest, artifact.GenerationID)
	manifestKey := generationManifestKey(digest, generationDigest)
	artifactKey := generationArtifactKey(digest, generationDigest)

	faults.listErr = true
	if err := store.Check(context.Background()); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("list failure Check() = %v", err)
	}
	faults.listErr = false

	if _, err := store.validateStoredGeneration(context.Background(), binding, digest, preview.ArtifactMetadata{}, false); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("invalid metadata error = %v", err)
	}

	manifest, _ := base.Get(context.Background(), manifestKey)
	if err := base.Delete(context.Background(), manifestKey, objectstore.DeleteCondition{Version: manifest.Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.validateStoredGeneration(context.Background(), binding, digest, artifact.Metadata(), false); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing manifest error = %v", err)
	}
	if _, err := base.Put(context.Background(), manifestKey, manifest.Body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}

	faults.getErrKey = manifestKey.String()
	if _, err := store.Latest(context.Background(), binding); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("manifest access error = %v", err)
	}
	faults.getErrKey = ""

	faults.headErrKey = artifactKey.String()
	if _, err := store.Latest(context.Background(), binding); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("artifact head access error = %v", err)
	}
	faults.headErrKey = ""
	faults.headSizeDelta = 1
	if _, err := store.Latest(context.Background(), binding); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("artifact head size error = %v", err)
	}
	faults.headSizeDelta = 0

	faults.getErrKey = artifactKey.String()
	if _, err := store.Read(context.Background(), binding, artifact.GenerationID); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("artifact read access error = %v", err)
	}
	faults.getErrKey = ""
	faults.getSizeDelta = 1
	if _, err := store.Read(context.Background(), binding, artifact.GenerationID); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("artifact body size error = %v", err)
	}
	faults.getSizeDelta = 0

	// Capability issuance must use the backend integrity-verification primitive,
	// not retrieve the artifact body through the control plane.
	faults.getErrKey = artifactKey.String()
	if _, err := store.CreateDownload(context.Background(), binding, artifact.GenerationID); err != nil {
		t.Fatalf("metadata-verified capability error = %v", err)
	}
	if _, err := store.Read(context.Background(), binding, artifact.GenerationID); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("artifact body fault was not exercised by Read() = %v", err)
	}
	faults.getErrKey = ""

	storedArtifact, _ := base.Get(context.Background(), artifactKey)
	corrupt := append([]byte(nil), storedArtifact.Body...)
	corrupt[len(corrupt)-1] ^= 0xff
	if _, err := base.Put(context.Background(), artifactKey, corrupt, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: storedArtifact.Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(context.Background(), binding, artifact.GenerationID); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("corrupt artifact error = %v", err)
	}
	if _, err := store.CreateDownload(context.Background(), binding, artifact.GenerationID); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("corrupt artifact capability error = %v", err)
	}
	current, _ := base.Head(context.Background(), artifactKey)
	if _, err := base.Put(context.Background(), artifactKey, artifact.Bytes, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: current.Version}); err != nil {
		t.Fatal(err)
	}

	faults.downloadErr = true
	if _, err := store.CreateDownload(context.Background(), binding, artifact.GenerationID); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("capability failure error = %v", err)
	}
	faults.downloadErr = false

	missingBinding := binding
	missingBinding.ContentVersion = "missing-version"
	if _, err := store.Read(context.Background(), missingBinding, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing head Read() error = %v", err)
	}
	emptyClaim, err := store.Claim(context.Background(), missingBinding, "empty-claim", options.Clock.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Release(context.Background(), missingBinding, emptyClaim); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Latest(context.Background(), missingBinding); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("empty head Latest() error = %v", err)
	}
}

func TestDurableConditionalWriteFailureMatrix(t *testing.T) {
	newFixture := func(t *testing.T) (*Store, *operationFaultBackend, Options, preview.Binding) {
		base := objectmemory.New()
		options := internalOptions(t, base, 4)
		faults := &operationFaultBackend{Backend: base, DirectTransferBackend: base}
		options.Backend, options.Transfers = faults, faults
		store, err := New(options)
		if err != nil {
			t.Fatal(err)
		}
		return store, faults, options, internalBinding(t)
	}

	t.Run("create unavailable", func(t *testing.T) {
		store, faults, options, binding := newFixture(t)
		faults.putErr = domain.NewError(domain.ErrorUnavailable, "injected")
		if _, err := store.Claim(context.Background(), binding, "claim", options.Clock.Now().Add(time.Minute)); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("Claim() error = %v", err)
		}
	})
	t.Run("contention exhausted", func(t *testing.T) {
		store, faults, options, binding := newFixture(t)
		faults.putErr = domain.NewError(domain.ErrorConflict, "injected")
		if _, err := store.Claim(context.Background(), binding, "claim", options.Clock.Now().Add(time.Minute)); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("Claim() error = %v", err)
		}
	})
	t.Run("takeover unavailable", func(t *testing.T) {
		store, faults, options, binding := newFixture(t)
		claim, err := store.Claim(context.Background(), binding, "claim", options.Clock.Now().Add(time.Minute))
		if err != nil || store.Release(context.Background(), binding, claim) != nil {
			t.Fatal(err)
		}
		faults.putErr = domain.NewError(domain.ErrorUnavailable, "injected")
		if _, err := store.Claim(context.Background(), binding, "takeover", options.Clock.Now().Add(time.Minute)); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("takeover Claim() error = %v", err)
		}
	})
	t.Run("release conflict", func(t *testing.T) {
		store, faults, options, binding := newFixture(t)
		claim, err := store.Claim(context.Background(), binding, "claim", options.Clock.Now().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		faults.putErr = domain.NewError(domain.ErrorConflict, "injected")
		if err := store.Release(context.Background(), binding, claim); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("Release() error = %v", err)
		}
	})
	t.Run("release outage clears readiness", func(t *testing.T) {
		store, faults, options, binding := newFixture(t)
		claim, err := store.Claim(context.Background(), binding, "claim", options.Clock.Now().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		store.setReady(true)
		faults.putErr = domain.NewError(domain.ErrorUnavailable, "injected")
		if err := store.Release(context.Background(), binding, claim); !errors.Is(err, domain.ErrUnavailable) || store.Ready() {
			t.Fatalf("Release() = %v, ready=%v", err, store.Ready())
		}
	})
	t.Run("claim epoch exhaustion", func(t *testing.T) {
		store, faults, options, binding := newFixture(t)
		digest := store.bindingDigest(binding)
		record := headRecord{SchemaVersion: schemaVersion, BindingDigest: digest, Epoch: math.MaxUint64}
		if _, err := faults.Backend.Put(context.Background(), headKey(digest), encode(record), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Claim(context.Background(), binding, "claim", options.Clock.Now().Add(time.Minute)); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("Claim() error = %v", err)
		}
	})
	t.Run("visibility precondition", func(t *testing.T) {
		store, faults, options, binding := newFixture(t)
		claim, err := store.Claim(context.Background(), binding, "claim", options.Clock.Now().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		faults.failPutNumber = 3
		faults.putCount = 0
		faults.putErr = domain.NewError(domain.ErrorPreconditionFailed, "injected")
		if err := store.Commit(context.Background(), binding, claim, internalArtifact("generation", binding.Variant)); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("Commit() error = %v", err)
		}
	})
}

func TestDurableStartupValidationFailureMatrix(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*operationFaultBackend, *Options)
	}{
		{name: "list", configure: func(faults *operationFaultBackend, _ *Options) { faults.listErr = true }},
		{name: "claim", configure: func(faults *operationFaultBackend, _ *Options) {
			faults.putErr = domain.NewError(domain.ErrorUnavailable, "injected")
		}},
		{name: "commit", configure: func(faults *operationFaultBackend, _ *Options) {
			faults.failPutNumber = 2
			faults.putErr = domain.NewError(domain.ErrorUnavailable, "injected")
		}},
		{name: "latest", configure: func(faults *operationFaultBackend, _ *Options) { faults.failGetNumber = 4 }},
		{name: "read", configure: func(faults *operationFaultBackend, _ *Options) { faults.failGetNumber = 6 }},
		{name: "capability", configure: func(faults *operationFaultBackend, _ *Options) { faults.downloadErr = true }},
		{name: "request", configure: func(faults *operationFaultBackend, _ *Options) { faults.downloadURL = ":%" }},
		{name: "transfer", configure: func(faults *operationFaultBackend, options *Options) {
			faults.downloadHeaders = map[string]string{"X-Preview-Probe": "1"}
			options.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("injected transport error")
			})}
		}},
		{name: "read body", configure: func(faults *operationFaultBackend, options *Options) {
			faults.downloadHeaders = map[string]string{"X-Preview-Probe": "1"}
			options.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return validationHTTPResponse(&brokenReadCloser{readErr: errors.New("injected read error")}), nil
			})}
		}},
		{name: "close body", configure: func(_ *operationFaultBackend, options *Options) {
			options.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return validationHTTPResponse(&brokenReadCloser{reader: bytes.NewReader(preview.OnePixelWebP()), closeErr: errors.New("injected close error")}), nil
			})}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := objectmemory.New()
			options := internalOptions(t, base, 4)
			faults := &operationFaultBackend{Backend: base, DirectTransferBackend: base}
			options.Backend, options.Transfers = faults, faults
			test.configure(faults, &options)
			store, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Validate(context.Background()); !errors.Is(err, domain.ErrUnavailable) || store.Ready() {
				t.Fatalf("Validate() = %v, ready=%v", err, store.Ready())
			}
		})
	}
}

func TestDurableStartupCleanupUsesBoundedContext(t *testing.T) {
	base := objectmemory.New()
	options := internalOptions(t, base, 4)
	faults := &operationFaultBackend{Backend: base, DirectTransferBackend: base}
	options.Backend, options.Transfers = faults, faults
	store, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := store.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	if !faults.deleteHadDeadline {
		t.Fatal("startup probe cleanup escaped its bounded validation context")
	}
}

func TestDurableStartupAcceptsEffectiveNoStoreCachePolicy(t *testing.T) {
	for name, cacheControl := range map[string][]string{
		"additional directives": {"no-cache, no-store, max-age=0"},
		"separate fields":       {"private", "max-age=0, No-Store"},
	} {
		t.Run(name, func(t *testing.T) {
			options := internalOptions(t, nil, 4)
			options.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				response := validationHTTPResponse(io.NopCloser(bytes.NewReader(preview.OnePixelWebP())))
				response.Header["Cache-Control"] = cacheControl
				return response, nil
			})}
			store, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Validate(context.Background()); err != nil || !store.Ready() {
				t.Fatalf("Validate() = %v, ready=%v", err, store.Ready())
			}
		})
	}
}

func validationHTTPResponse(body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":        {preview.ContentTypeWebP},
			"Content-Disposition": {"inline; filename=preview.webp"},
			"Cache-Control":       {"no-store"},
		},
		Body: body,
	}
}

type putFaultBackend struct {
	objectstore.Backend
	objectstore.DirectTransferBackend
	mu               sync.Mutex
	putCount         int
	failAfter        int
	afterLostSuccess func()
}

type operationFaultBackend struct {
	objectstore.Backend
	objectstore.DirectTransferBackend
	getErrKey         string
	headErrKey        string
	getSizeDelta      int64
	headSizeDelta     int64
	listErr           bool
	downloadErr       bool
	downloadURL       string
	downloadHeaders   map[string]string
	putErr            error
	failPutNumber     int
	putCount          int
	failGetNumber     int
	getCount          int
	deleteHadDeadline bool
}

func (b *operationFaultBackend) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	b.getCount++
	if b.failGetNumber > 0 && b.getCount == b.failGetNumber {
		return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "injected")
	}
	if key.String() == b.getErrKey {
		return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "injected")
	}
	object, err := b.Backend.Get(ctx, key)
	object.Size += b.getSizeDelta
	return object, err
}

func (b *operationFaultBackend) Head(ctx context.Context, key objectstore.Key) (objectstore.ObjectInfo, error) {
	if key.String() == b.headErrKey {
		return objectstore.ObjectInfo{}, domain.NewError(domain.ErrorUnavailable, "injected")
	}
	info, err := b.Backend.Head(ctx, key)
	info.Size += b.headSizeDelta
	return info, err
}

func (b *operationFaultBackend) Verify(ctx context.Context, key objectstore.Key, expected objectstore.ExpectedIntegrity) (objectstore.ObjectInfo, error) {
	if key.String() == b.headErrKey {
		return objectstore.ObjectInfo{}, domain.NewError(domain.ErrorUnavailable, "injected")
	}
	info, err := b.Backend.Verify(ctx, key, expected)
	info.Size += b.headSizeDelta
	return info, err
}

func (b *operationFaultBackend) List(ctx context.Context, request objectstore.ListRequest) (objectstore.ListPage, error) {
	if b.listErr {
		return objectstore.ListPage{}, domain.NewError(domain.ErrorUnavailable, "injected")
	}
	return b.Backend.List(ctx, request)
}

func (b *operationFaultBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	b.putCount++
	if b.putErr != nil && (b.failPutNumber == 0 || b.putCount == b.failPutNumber) {
		return "", b.putErr
	}
	return b.Backend.Put(ctx, key, body, condition)
}

func (b *operationFaultBackend) CreateDownload(ctx context.Context, request objectstore.DownloadRequest) (objectstore.DownloadCapability, error) {
	if b.downloadErr {
		return objectstore.DownloadCapability{}, domain.NewError(domain.ErrorUnavailable, "injected")
	}
	capability, err := b.DirectTransferBackend.CreateDownload(ctx, request)
	if b.downloadURL != "" {
		capability.URL = b.downloadURL
	}
	if b.downloadHeaders != nil {
		capability.Headers = b.downloadHeaders
	}
	return capability, err
}

func (b *operationFaultBackend) Delete(ctx context.Context, key objectstore.Key, condition objectstore.DeleteCondition) error {
	_, b.deleteHadDeadline = ctx.Deadline()
	return b.Backend.Delete(ctx, key, condition)
}

type brokenReadCloser struct {
	reader   io.Reader
	readErr  error
	closeErr error
}

func (b *brokenReadCloser) Read(data []byte) (int, error) {
	if b.reader != nil {
		return b.reader.Read(data)
	}
	return 0, b.readErr
}

func (b *brokenReadCloser) Close() error { return b.closeErr }

func (b *putFaultBackend) reset(failAfter int) {
	b.mu.Lock()
	b.putCount, b.failAfter = 0, failAfter
	b.mu.Unlock()
}

func (b *putFaultBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	version, err := b.Backend.Put(ctx, key, body, condition)
	if err != nil {
		return version, err
	}
	b.mu.Lock()
	b.putCount++
	fail := b.failAfter > 0 && b.putCount == b.failAfter
	b.mu.Unlock()
	if fail {
		if b.afterLostSuccess != nil {
			b.afterLostSuccess()
		}
		return "", domain.NewError(domain.ErrorUnavailable, "injected lost success")
	}
	return version, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func internalOptions(t *testing.T, backend *objectmemory.Backend, maxGenerations int) Options {
	t.Helper()
	if backend == nil {
		backend = objectmemory.New()
	}
	clock := domain.NewFixedClock(time.Now().UTC().Truncate(time.Second))
	ids := domain.NewIDGenerator(bytes.NewReader(internalDeterministicBytes(8 << 20)))
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	if err := backend.ConfigureDataPlane(server.URL, clock, ids); err != nil {
		t.Fatal(err)
	}
	return Options{
		Backend: backend, Transfers: backend, Clock: clock, IDs: ids,
		Key:           secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x45}, 32))),
		CapabilityTTL: time.Minute, DataOrigin: server.URL, HTTPClient: server.Client(), MaxGenerations: maxGenerations,
	}
}

func internalBinding(t *testing.T) preview.Binding {
	t.Helper()
	owner, err := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x35}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	return preview.Binding{Owner: owner, ContentID: "sensitive-content", ContentVersion: "sensitive-version", MediaType: "image/png", SourceSize: 72, RecipeID: "image-webp-q80-v1", Variant: 256}
}

func internalArtifact(generationID string, variant int) preview.Artifact {
	data := preview.OnePixelWebP()
	digest := sha256.Sum256(data)
	return preview.Artifact{GenerationID: generationID, Variant: variant, Width: 1, Height: 1, ContentType: preview.ContentTypeWebP, Size: int64(len(data)), SHA256: base64.RawURLEncoding.EncodeToString(digest[:]), CRC32C: preview.ChecksumCRC32C(data), Bytes: data}
}

func internalClaim(id string, epoch uint64, now time.Time) preview.GenerationClaim {
	return preview.GenerationClaim{ID: id, Epoch: epoch, ExpiresAt: now.Add(time.Minute)}
}

func internalDeterministicBytes(size int) []byte {
	result := make([]byte, size)
	for index := range result {
		result[index] = byte(index*19 + 7)
	}
	return result
}
