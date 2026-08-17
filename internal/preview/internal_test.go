package preview

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
	"github.com/applyinnovations/endlessfs/internal/provider"
)

func TestContentBindingAndArtifactValidationFailures(t *testing.T) {
	binding := internalBinding(t)
	if !binding.Valid() {
		t.Fatal("valid binding was rejected")
	}
	tooShort := binding
	tooShort.RecipeID = ""
	if tooShort.Valid() {
		t.Fatal("empty recipe was accepted")
	}
	badCharacter := binding
	badCharacter.RecipeID = "image_v1"
	if badCharacter.Valid() {
		t.Fatal("invalid recipe character was accepted")
	}
	artifact := internalArtifact("generation", binding.Variant)
	if !artifact.ValidFor(binding) {
		t.Fatal("valid artifact was rejected")
	}
	invalidShape := artifact
	invalidShape.Width = 0
	if invalidShape.ValidFor(binding) {
		t.Fatal("invalid artifact dimensions were accepted")
	}
	invalidDigest := artifact
	invalidDigest.SHA256 = "invalid"
	if invalidDigest.ValidFor(binding) {
		t.Fatal("invalid artifact digest was accepted")
	}
	mismatchedDigest := artifact
	mismatchedDigest.SHA256 = base64.RawURLEncoding.EncodeToString(make([]byte, sha256.Size))
	if mismatchedDigest.ValidFor(binding) {
		t.Fatal("mismatched artifact digest was accepted")
	}
	invalidWebP := artifact
	invalidWebP.Bytes = append([]byte(nil), artifact.Bytes...)
	invalidWebP.Bytes[51] ^= 0xff
	sum := sha256.Sum256(invalidWebP.Bytes)
	invalidWebP.SHA256 = base64.RawURLEncoding.EncodeToString(sum[:])
	if invalidWebP.ValidFor(binding) {
		t.Fatal("corrupt WebP was accepted")
	}
}

func TestReadyResultAndConcurrencyFailureMapping(t *testing.T) {
	binding := internalBinding(t)
	result := ItemResult{Variant: binding.Variant}
	tests := []struct {
		name      string
		store     *scriptedStore
		wantState State
		wantError bool
	}{
		{name: "latest error", store: &scriptedStore{latestErr: domain.NewError(domain.ErrorInvalid, "bad manifest")}, wantState: StateFailed},
		{name: "capability unavailable", store: &scriptedStore{latest: internalArtifact("one", binding.Variant), capabilityErr: domain.NewError(domain.ErrorUnavailable, "offline")}, wantState: StateUnavailable},
		{name: "capability invalid", store: &scriptedStore{latest: internalArtifact("two", binding.Variant), capabilityErr: domain.NewError(domain.ErrorInvalid, "bad capability")}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &Service{store: test.store}
			got, _, err := service.readyResult(context.Background(), binding, result)
			if (err != nil) != test.wantError || !test.wantError && got.State != test.wantState {
				t.Fatalf("readyResult = %+v, %v", got, err)
			}
		})
	}
	if stateErrorKind(StateUnavailable) != domain.ErrorUnavailable || stateErrorKind(StateFailed) != domain.ErrorInvalid {
		t.Fatal("preview state error mapping changed")
	}
	owner := binding.Owner
	globalBlocked := &Service{global: make(chan struct{}, 1), perUser: make(map[string]chan struct{})}
	globalBlocked.global <- struct{}{}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := globalBlocked.acquire(canceled, owner); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("global canceled acquire error = %v", err)
	}
	userBlocked := &Service{global: make(chan struct{}, 1), perUser: map[string]chan struct{}{owner.String(): make(chan struct{}, 1)}}
	userBlocked.perUser[owner.String()] <- struct{}{}
	if _, err := userBlocked.acquire(canceled, owner); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("per-user canceled acquire error = %v", err)
	}
}

func TestGenerateFailureIsolationMatrix(t *testing.T) {
	binding := internalBinding(t)
	entry := domain.Entry{
		Path: domain.MustParseUserPath("/source.png"), Name: "source.png", Kind: domain.EntryFile,
		Size: binding.SourceSize, MediaType: binding.MediaType, Version: "source-version",
		ContentID: binding.ContentID, ContentVersion: binding.ContentVersion, ContentModifiedAt: time.Now(),
	}
	scope, err := domain.NewScope(binding.Owner, domain.AreaLive)
	if err != nil {
		t.Fatal(err)
	}
	validCapability := domain.DownloadCapability{URL: "http://127.0.0.1:1234/source", Method: http.MethodGet}
	sourceBytes := []byte("source")
	tests := []struct {
		name        string
		download    domain.DownloadCapability
		downloadErr error
		client      *http.Client
		generator   Generator
		ids         *domain.IDGenerator
	}{
		{name: "source capability", downloadErr: domain.NewError(domain.ErrorUnavailable, "offline")},
		{name: "invalid source URL", download: domain.DownloadCapability{URL: ":%", Method: http.MethodGet}},
		{name: "source transport", download: validCapability, client: internalClient(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })},
		{name: "source status", download: validCapability, client: internalResponseClient(http.StatusBadGateway, io.NopCloser(bytes.NewReader(nil)))},
		{name: "source read", download: validCapability, client: internalResponseClient(http.StatusOK, errorReadCloser{})},
		{name: "source size", download: validCapability, client: internalResponseClient(http.StatusOK, io.NopCloser(bytes.NewReader([]byte("short"))))},
		{name: "generator", download: validCapability, client: internalResponseClient(http.StatusOK, io.NopCloser(bytes.NewReader(sourceBytes))), generator: scriptedGenerator{err: errors.New("decode")}},
		{name: "generation ID", download: validCapability, client: internalResponseClient(http.StatusOK, io.NopCloser(bytes.NewReader(sourceBytes))), generator: scriptedGenerator{generated: GeneratedArtifact{Bytes: OnePixelWebP(), Width: 1, Height: 1}}, ids: domain.NewIDGenerator(bytes.NewReader(nil))},
		{name: "invalid generated artifact", download: validCapability, client: internalResponseClient(http.StatusOK, io.NopCloser(bytes.NewReader(sourceBytes))), generator: scriptedGenerator{generated: GeneratedArtifact{Bytes: []byte("invalid"), Width: 1, Height: 1}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := test.client
			if client == nil {
				client = internalResponseClient(http.StatusOK, io.NopCloser(bytes.NewReader(sourceBytes)))
			}
			generator := test.generator
			if generator == nil {
				generator = scriptedGenerator{generated: GeneratedArtifact{Bytes: OnePixelWebP(), Width: 1, Height: 1}}
			}
			ids := test.ids
			if ids == nil {
				ids = domain.NewIDGenerator(bytes.NewReader(make([]byte, 1024)))
			}
			service := &Service{
				options: Options{OperationTimeout: time.Second, HardMaxSourceBytes: 1024},
				source:  &scriptedStorage{download: test.download, downloadErr: test.downloadErr}, store: &scriptedStore{},
				client: client, ids: ids, clock: domain.SystemClock{}, global: make(chan struct{}, 1), perUser: make(map[string]chan struct{}),
			}
			if err := service.generate(context.Background(), scope, entry, binding, generator, true); err == nil {
				t.Fatal("generate failure path returned success")
			}
		})
	}
}

func TestServiceRejectsInvalidRegistryAndGenerationRequests(t *testing.T) {
	binding := internalBinding(t)
	options := Options{Automatic: true, Resolutions: []int{256}}
	validGenerator := scriptedGenerator{generated: GeneratedArtifact{Bytes: OnePixelWebP(), Width: 1, Height: 1}}
	for _, generators := range [][]Generator{{nil}, {validGenerator, validGenerator}} {
		if _, err := NewService(options, &scriptedStorage{}, nil, generators, http.DefaultClient, domain.NewIDGenerator(bytes.NewReader(make([]byte, 64))), domain.SystemClock{}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid generator registry error = %v", err)
		}
	}
	service := &Service{
		options: options, ids: domain.NewIDGenerator(bytes.NewReader(nil)), clock: domain.SystemClock{},
		operations: make(map[string]map[domain.OperationID]Operation), idempotent: make(map[string]idempotentOperation),
	}
	if _, err := service.Generate(context.Background(), binding.Owner, GenerateRequest{
		Path: domain.MustParseUserPath("/source.png"), Version: "version", Variant: 512, IdempotencyKey: "preview-invalid-item-0001",
	}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid item error = %v", err)
	}
	if _, err := service.Generate(context.Background(), binding.Owner, GenerateRequest{
		Path: domain.MustParseUserPath("/source.png"), Version: "version", Variant: 256, IdempotencyKey: "preview-exhausted-id-0001",
	}); err == nil {
		t.Fatal("exhausted operation ID source returned success")
	}
}

func TestResolveGenerationFailureStates(t *testing.T) {
	binding := internalBinding(t)
	entry := domain.Entry{
		Path: domain.MustParseUserPath("/source.png"), Name: "source.png", Kind: domain.EntryFile,
		Size: binding.SourceSize, MediaType: binding.MediaType, Version: "source-version",
		ContentID: binding.ContentID, ContentVersion: binding.ContentVersion, ContentModifiedAt: time.Now(),
	}
	item := ItemRequest{Path: entry.Path, Version: entry.Version, Variant: binding.Variant}
	newService := func(generator Generator) *Service {
		return &Service{
			options: Options{Automatic: true, Resolutions: []int{256}, OperationTimeout: time.Second, HardMaxSourceBytes: 1024},
			source:  &scriptedStorage{stat: entry, download: domain.DownloadCapability{URL: "http://127.0.0.1:1234/source", Method: http.MethodGet}},
			store:   &scriptedStore{latestErr: domain.ErrNotFound}, generators: []Generator{generator},
			client: internalResponseClient(http.StatusOK, io.NopCloser(bytes.NewReader([]byte("source")))),
			ids:    domain.NewIDGenerator(bytes.NewReader(make([]byte, 1024))), clock: domain.SystemClock{},
			global: make(chan struct{}, 1), perUser: make(map[string]chan struct{}), inflight: make(map[string]*generationCall),
		}
	}
	if _, err := newService(scriptedGenerator{}).resolveItem(context.Background(), domain.UserID{}, item, true, false, false); err == nil {
		t.Fatal("invalid owner returned success")
	}
	missingIdentity := newService(scriptedGenerator{})
	missingIdentity.source = &scriptedStorage{stat: domain.Entry{Path: entry.Path, Kind: domain.EntryFile, Version: entry.Version, MediaType: entry.MediaType}}
	if _, err := missingIdentity.resolveItem(context.Background(), binding.Owner, item, true, false, false); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("missing content identity error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	canceledService := newService(scriptedGenerator{})
	canceledService.global <- struct{}{}
	if result, err := canceledService.resolveItem(canceled, binding.Owner, item, true, false, false); err != nil || result.State != StateUnavailable {
		t.Fatalf("canceled generation = %+v, %v", result, err)
	}
	if result, err := newService(scriptedGenerator{err: errors.New("decode")}).resolveItem(context.Background(), binding.Owner, item, true, false, false); err != nil || result.State != StateFailed {
		t.Fatalf("generator rejection = %+v, %v", result, err)
	}
	if result, err := newService(scriptedGenerator{generated: GeneratedArtifact{Bytes: OnePixelWebP(), Width: 1, Height: 1}}).resolveItem(context.Background(), binding.Owner, item, true, false, false); err != nil || result.State != StateFailed {
		t.Fatalf("missing committed artifact = %+v, %v", result, err)
	}
}

type scriptedStore struct {
	latest        Artifact
	latestErr     error
	capabilityErr error
}

func (*scriptedStore) Validate(context.Context) error                  { return nil }
func (*scriptedStore) Commit(context.Context, Binding, Artifact) error { return nil }
func (s *scriptedStore) Latest(context.Context, Binding) (Artifact, error) {
	return s.latest, s.latestErr
}
func (s *scriptedStore) CreateDownload(context.Context, Binding) (domain.DownloadCapability, error) {
	return domain.DownloadCapability{URL: "http://127.0.0.1:1234/preview", Method: http.MethodGet}, s.capabilityErr
}
func (*scriptedStore) Ready() bool        { return true }
func (*scriptedStore) DataOrigin() string { return "http://127.0.0.1:1234" }

type scriptedStorage struct {
	provider.Storage
	download    domain.DownloadCapability
	downloadErr error
	stat        domain.Entry
	statErr     error
}

func (s *scriptedStorage) Stat(context.Context, domain.Scope, domain.UserPath) (domain.Entry, error) {
	return s.stat, s.statErr
}

func (s *scriptedStorage) CreateDownload(context.Context, domain.Scope, domain.CreateDownloadRequest) (domain.DownloadCapability, error) {
	return s.download, s.downloadErr
}

type scriptedGenerator struct {
	generated GeneratedArtifact
	err       error
}

func (scriptedGenerator) Capability() string             { return "image" }
func (scriptedGenerator) RecipeID() string               { return "image-webp-q80-v1" }
func (scriptedGenerator) Supports(string) bool           { return true }
func (scriptedGenerator) SelfTest(context.Context) error { return nil }
func (g scriptedGenerator) Generate(context.Context, GenerationRequest) (GeneratedArtifact, error) {
	return g.generated, g.err
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func internalClient(function roundTripFunc) *http.Client { return &http.Client{Transport: function} }

func internalResponseClient(status int, body io.ReadCloser) *http.Client {
	return internalClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: body, Header: make(http.Header)}, nil
	})
}

type errorReadCloser struct{}

func (errorReadCloser) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (errorReadCloser) Close() error             { return nil }

func internalBinding(t *testing.T) Binding {
	t.Helper()
	owner, err := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x62}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	return Binding{Owner: owner, ContentID: "content", ContentVersion: "version", MediaType: "image/png", SourceSize: 6, RecipeID: "image-webp-q80-v1", Variant: 256}
}

func internalArtifact(generationID string, variant int) Artifact {
	data := OnePixelWebP()
	sum := sha256.Sum256(data)
	return Artifact{GenerationID: generationID, Variant: variant, Width: 1, Height: 1, ContentType: ContentTypeWebP, Size: int64(len(data)), SHA256: base64.RawURLEncoding.EncodeToString(sum[:]), Bytes: data}
}
