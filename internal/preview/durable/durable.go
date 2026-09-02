// Package durable implements the provider-neutral durable preview artifact
// store over the same thin conditional-object boundary used by EndlessFS.
package durable

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"math"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/applyinnovations/endlessfs/internal/cachecontrol"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/preview"
	"github.com/applyinnovations/endlessfs/internal/secret"
)

const (
	schemaVersion            = 1
	previewPrefix            = "preview/v1/"
	defaultMaxGenerations    = 4
	defaultMaxArtifactBytes  = int64(32 << 20)
	maximumRecordBytes       = int64(64 << 10)
	maximumReadyCatalogBytes = int64(4 << 20)
	maximumReadyCatalogItems = 10_000
	maximumRetries           = 32
)

type Options struct {
	Backend          objectstore.Backend
	IndexBackend     objectstore.Backend
	Transfers        objectstore.DirectTransferBackend
	Clock            domain.Clock
	IDs              *domain.IDGenerator
	Key              secret.Value
	CapabilityTTL    time.Duration
	DataOrigin       string
	AllowedOrigin    string
	HTTPClient       *http.Client
	MaxGenerations   int
	MaxArtifactBytes int64
}

type Store struct {
	backend          objectstore.Backend
	indexBackend     objectstore.Backend
	transfers        objectstore.DirectTransferBackend
	clock            domain.Clock
	ids              *domain.IDGenerator
	key              []byte
	capabilityTTL    time.Duration
	dataOrigin       string
	allowedOrigin    string
	httpClient       *http.Client
	maxGenerations   int
	maxArtifactBytes int64

	mu    sync.RWMutex
	ready bool
}

type headRecord struct {
	SchemaVersion int                        `json:"schemaVersion"`
	BindingDigest string                     `json:"bindingDigest"`
	Epoch         uint64                     `json:"epoch"`
	ClaimID       string                     `json:"claimID,omitempty"`
	ClaimExpires  time.Time                  `json:"claimExpiresAt,omitempty"`
	Generations   []preview.ArtifactMetadata `json:"generations,omitempty"`
}

type manifestRecord struct {
	SchemaVersion int                      `json:"schemaVersion"`
	BindingDigest string                   `json:"bindingDigest"`
	Artifact      preview.ArtifactMetadata `json:"artifact"`
}

type readyCatalogRecord struct {
	SchemaVersion int                 `json:"schemaVersion"`
	ScopeDigest   string              `json:"scopeDigest"`
	Revision      uint64              `json:"revision"`
	Entries       []readyCatalogEntry `json:"entries"`
}

type readyCatalogEntry struct {
	BindingDigest string                   `json:"bindingDigest"`
	RecordedAt    time.Time                `json:"recordedAt"`
	Artifact      preview.ArtifactMetadata `json:"artifact"`
}

func New(options Options) (*Store, error) {
	if options.Clock == nil {
		options.Clock = domain.SystemClock{}
	}
	if options.IDs == nil {
		options.IDs = domain.SystemIDGenerator()
	}
	if options.CapabilityTTL == 0 {
		options.CapabilityTTL = time.Minute
	}
	if options.HTTPClient == nil {
		options.HTTPClient = http.DefaultClient
	}
	if options.MaxGenerations == 0 {
		options.MaxGenerations = defaultMaxGenerations
	}
	if options.MaxArtifactBytes == 0 {
		options.MaxArtifactBytes = defaultMaxArtifactBytes
	}
	if options.IndexBackend == nil {
		options.IndexBackend = options.Backend
	}
	key, err := base64.RawURLEncoding.DecodeString(options.Key.Reveal())
	if options.Backend == nil || options.IndexBackend == nil || options.Transfers == nil || err != nil || len(key) < 32 ||
		options.CapabilityTTL <= 0 || options.CapabilityTTL > 10*time.Minute ||
		options.DataOrigin == "" || options.MaxGenerations < 1 || options.MaxGenerations > 32 ||
		options.MaxArtifactBytes < int64(len(preview.OnePixelWebP())) || options.MaxArtifactBytes > 128<<20 {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid durable preview store configuration")
	}
	return &Store{
		backend: options.Backend, indexBackend: options.IndexBackend, transfers: options.Transfers, clock: options.Clock, ids: options.IDs,
		key: key, capabilityTTL: options.CapabilityTTL, dataOrigin: strings.TrimRight(options.DataOrigin, "/"),
		allowedOrigin: options.AllowedOrigin, httpClient: options.HTTPClient,
		maxGenerations: options.MaxGenerations, maxArtifactBytes: options.MaxArtifactBytes,
	}, nil
}

func (s *Store) Ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ready
}

func (s *Store) DataOrigin() string { return s.dataOrigin }

func (s *Store) setReady(ready bool) {
	s.mu.Lock()
	s.ready = ready
	s.mu.Unlock()
}

func (s *Store) Check(ctx context.Context) error {
	if err := objectstore.ContextError(ctx); err != nil {
		s.setReady(false)
		return domain.WrapError(domain.ErrorUnavailable, "preview store check timed out", err)
	}
	_, err := s.backend.List(ctx, objectstore.ListRequest{Prefix: previewPrefix, Limit: 1})
	if err != nil {
		s.setReady(false)
		return domain.NewError(domain.ErrorUnavailable, "preview store unavailable")
	}
	return nil
}

func (s *Store) Validate(ctx context.Context) error {
	s.setReady(false)
	if err := s.Check(ctx); err != nil {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: list")
	}
	generationID, err := s.ids.OpaqueID()
	if err != nil {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: identity")
	}
	binding := validationBinding(generationID)
	defer s.cleanupProbe(ctx, binding, generationID)
	data := preview.OnePixelWebP()
	digest := sha256.Sum256(data)
	artifact := preview.Artifact{
		GenerationID: generationID, Variant: binding.Variant, Width: 1, Height: 1,
		ContentType: preview.ContentTypeWebP, Size: int64(len(data)),
		SHA256: base64.RawURLEncoding.EncodeToString(digest[:]), CRC32C: preview.ChecksumCRC32C(data), Bytes: data,
	}
	claim, err := s.Claim(ctx, binding, generationID, s.clock.Now().Add(time.Minute))
	if err != nil {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: claim")
	}
	if err := s.Commit(ctx, binding, claim, artifact); err != nil {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: commit")
	}
	latest, err := s.Latest(ctx, binding)
	if err != nil || latest.GenerationID != generationID {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: manifest")
	}
	stored, err := s.Read(ctx, binding, generationID)
	if err != nil || !bytes.Equal(stored.Bytes, data) {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: read")
	}
	capability, err := s.CreateDownload(ctx, binding, generationID)
	if err != nil {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: capability")
	}
	request, err := http.NewRequestWithContext(ctx, capability.Method, capability.URL, http.NoBody)
	if err != nil {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: capability")
	}
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	if s.allowedOrigin != "" {
		request.Header.Set("Origin", s.allowedOrigin)
	}
	response, err := s.httpClient.Do(request)
	if err != nil {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: capability")
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, int64(len(data))+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: capability")
	}
	if response.StatusCode != http.StatusOK {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: capability status")
	}
	if s.allowedOrigin != "" && response.Header.Get("Access-Control-Allow-Origin") != s.allowedOrigin {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: capability origin")
	}
	if response.Header.Get("Content-Type") != preview.ContentTypeWebP {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: capability content type")
	}
	disposition, parameters, dispositionErr := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
	if dispositionErr != nil || disposition != string(domain.DispositionInline) || parameters["filename"] != "preview.webp" {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: capability disposition")
	}
	if !cachecontrol.HasNoStore(response.Header) {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: capability cache control")
	}
	if !bytes.Equal(body, data) {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: capability body")
	}
	s.setReady(true)
	return nil
}

func (s *Store) Claim(ctx context.Context, binding preview.Binding, claimID string, expiresAt time.Time) (preview.GenerationClaim, error) {
	if err := objectstore.ContextError(ctx); err != nil {
		return preview.GenerationClaim{}, domain.WrapError(domain.ErrorUnavailable, "preview store request canceled", err)
	}
	if !binding.Valid() || claimID == "" || len(claimID) > 128 || strings.ContainsAny(claimID, "\r\n\x00/") || !s.clock.Now().Before(expiresAt) {
		return preview.GenerationClaim{}, domain.NewError(domain.ErrorInvalid, "invalid preview generation claim")
	}
	digest := s.bindingDigest(binding)
	key := headKey(digest)
	for range maximumRetries {
		object, record, err := s.readHead(ctx, binding, digest)
		if errors.Is(err, domain.ErrNotFound) {
			claim := preview.GenerationClaim{ID: claimID, Epoch: 1, ExpiresAt: expiresAt.UTC()}
			record = headRecord{SchemaVersion: schemaVersion, BindingDigest: digest, Epoch: claim.Epoch, ClaimID: claim.ID, ClaimExpires: claim.ExpiresAt}
			if _, err = s.backend.Put(ctx, key, encode(record), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err == nil {
				return claim, nil
			}
			if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPreconditionFailed) {
				continue
			}
			s.setReady(false)
			return preview.GenerationClaim{}, err
		}
		if err != nil {
			return preview.GenerationClaim{}, err
		}
		if record.ClaimID != "" && s.clock.Now().Before(record.ClaimExpires) {
			return preview.GenerationClaim{ID: record.ClaimID, Epoch: record.Epoch, ExpiresAt: record.ClaimExpires}, domain.NewError(domain.ErrorConflict, "preview generation is already claimed")
		}
		if record.Epoch == math.MaxUint64 {
			s.setReady(false)
			return preview.GenerationClaim{}, domain.NewError(domain.ErrorUnavailable, "preview generation claim epoch exhausted")
		}
		claim := preview.GenerationClaim{ID: claimID, Epoch: record.Epoch + 1, ExpiresAt: expiresAt.UTC()}
		record.Epoch, record.ClaimID, record.ClaimExpires = claim.Epoch, claim.ID, claim.ExpiresAt
		if _, err = s.backend.Put(ctx, key, encode(record), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err == nil {
			return claim, nil
		}
		if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrNotFound) {
			continue
		}
		s.setReady(false)
		return preview.GenerationClaim{}, err
	}
	return preview.GenerationClaim{}, domain.NewError(domain.ErrorUnavailable, "preview generation claim contention exceeded")
}

func (s *Store) Release(ctx context.Context, binding preview.Binding, claim preview.GenerationClaim) error {
	if err := objectstore.ContextError(ctx); err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "preview store request canceled", err)
	}
	if !binding.Valid() || !claim.Valid() {
		return domain.NewError(domain.ErrorInvalid, "invalid preview generation claim")
	}
	digest := s.bindingDigest(binding)
	object, record, err := s.readHead(ctx, binding, digest)
	if err != nil {
		return err
	}
	if !sameClaim(record, claim) {
		return domain.NewError(domain.ErrorPreconditionFailed, "preview generation claim changed")
	}
	record.ClaimID, record.ClaimExpires = "", time.Time{}
	_, err = s.backend.Put(ctx, headKey(digest), encode(record), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version})
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPreconditionFailed) {
		return domain.NewError(domain.ErrorPreconditionFailed, "preview generation claim changed")
	}
	if err != nil {
		s.setReady(false)
	}
	return err
}

func (s *Store) Commit(ctx context.Context, binding preview.Binding, claim preview.GenerationClaim, artifact preview.Artifact) error {
	if err := objectstore.ContextError(ctx); err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "preview store request canceled", err)
	}
	if !binding.Valid() || !claim.Valid() || !artifact.ValidFor(binding) || artifact.Size > s.maxArtifactBytes {
		return domain.NewError(domain.ErrorInvalid, "invalid preview artifact")
	}
	digest := s.bindingDigest(binding)
	_, initial, err := s.readHead(ctx, binding, digest)
	if err != nil {
		return err
	}
	if !sameClaim(initial, claim) || !s.clock.Now().Before(claim.ExpiresAt) {
		return domain.NewError(domain.ErrorPreconditionFailed, "preview generation claim changed or expired")
	}
	if findGeneration(initial.Generations, artifact.GenerationID) != nil {
		return domain.NewError(domain.ErrorConflict, "preview generation already exists")
	}
	artifactKey := generationArtifactKey(digest, s.generationDigest(digest, artifact.GenerationID))
	if err := s.putImmutableOrVerify(ctx, artifactKey, artifact.Bytes); err != nil {
		return err
	}
	manifest := manifestRecord{SchemaVersion: schemaVersion, BindingDigest: digest, Artifact: artifact.Metadata()}
	manifestBody := encode(manifest)
	manifestKey := generationManifestKey(digest, s.generationDigest(digest, artifact.GenerationID))
	if err := s.putImmutableOrVerify(ctx, manifestKey, manifestBody); err != nil {
		return err
	}

	object, record, err := s.readHead(ctx, binding, digest)
	if err != nil {
		return err
	}
	if !sameClaim(record, claim) || !s.clock.Now().Before(claim.ExpiresAt) {
		return domain.NewError(domain.ErrorPreconditionFailed, "preview generation claim changed or expired")
	}
	if findGeneration(record.Generations, artifact.GenerationID) != nil {
		return domain.NewError(domain.ErrorConflict, "preview generation already exists")
	}
	record.Generations = append(record.Generations, artifact.Metadata())
	if len(record.Generations) > s.maxGenerations {
		record.Generations = append([]preview.ArtifactMetadata(nil), record.Generations[len(record.Generations)-s.maxGenerations:]...)
	}
	record.ClaimID, record.ClaimExpires = "", time.Time{}
	_, err = s.backend.Put(ctx, headKey(digest), encode(record), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version})
	if err == nil {
		return nil
	}
	if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrNotFound) {
		return domain.NewError(domain.ErrorPreconditionFailed, "preview generation claim changed")
	}
	// Recover a provider response lost after the successful visibility CAS.
	_, recovered, readErr := s.readHead(ctx, binding, digest)
	if readErr == nil {
		metadata := findGeneration(recovered.Generations, artifact.GenerationID)
		if metadata != nil && *metadata == artifact.Metadata() {
			return nil
		}
	}
	s.setReady(false)
	return err
}

func (s *Store) Latest(ctx context.Context, binding preview.Binding) (preview.ArtifactMetadata, error) {
	if err := objectstore.ContextError(ctx); err != nil {
		return preview.ArtifactMetadata{}, domain.WrapError(domain.ErrorUnavailable, "preview store request canceled", err)
	}
	if !binding.Valid() {
		return preview.ArtifactMetadata{}, domain.NewError(domain.ErrorInvalid, "invalid preview binding")
	}
	digest := s.bindingDigest(binding)
	_, record, err := s.readHead(ctx, binding, digest)
	if errors.Is(err, domain.ErrNotFound) {
		return preview.ArtifactMetadata{}, domain.NewError(domain.ErrorNotFound, "preview artifact not found")
	}
	if err != nil {
		return preview.ArtifactMetadata{}, err
	}
	if len(record.Generations) == 0 {
		return preview.ArtifactMetadata{}, domain.NewError(domain.ErrorNotFound, "preview artifact not found")
	}
	metadata := record.Generations[len(record.Generations)-1]
	if _, err := s.validateStoredGeneration(ctx, binding, digest, metadata, false); err != nil {
		return preview.ArtifactMetadata{}, err
	}
	return metadata, nil
}

func (s *Store) ResolveReady(ctx context.Context, selections []preview.ReadySelection) ([]*preview.ArtifactMetadata, error) {
	if err := objectstore.ContextError(ctx); err != nil {
		return nil, domain.WrapError(domain.ErrorUnavailable, "preview store request canceled", err)
	}
	if len(selections) < 1 || len(selections) > 64 {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid preview ready batch")
	}
	results := make([]*preview.ArtifactMetadata, len(selections))
	groups := make(map[string][]int)
	for index, selection := range selections {
		if !selection.Valid() {
			return nil, domain.NewError(domain.ErrorInvalid, "invalid preview ready selection")
		}
		groups[selection.CacheScope] = append(groups[selection.CacheScope], index)
	}
	for cacheScope, indices := range groups {
		scopeDigest := s.readyScopeDigest(cacheScope)
		object, err := s.indexBackend.Get(ctx, readyCatalogKey(scopeDigest))
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			s.setReady(false)
			return nil, err
		}
		record, err := decodeReadyCatalog(object.Body, scopeDigest)
		if err != nil {
			s.setReady(false)
			return nil, domain.NewError(domain.ErrorUnavailable, "preview ready catalog is corrupt")
		}
		for _, resultIndex := range indices {
			selection := selections[resultIndex]
			bindingDigest := s.bindingDigest(selection.Binding)
			entryIndex := sort.Search(len(record.Entries), func(index int) bool { return record.Entries[index].BindingDigest >= bindingDigest })
			if entryIndex == len(record.Entries) || record.Entries[entryIndex].BindingDigest != bindingDigest || !record.Entries[entryIndex].Artifact.ValidFor(selection.Binding) {
				continue
			}
			metadata := record.Entries[entryIndex].Artifact
			results[resultIndex] = &metadata
		}
	}
	return results, nil
}

func (s *Store) RecordReady(ctx context.Context, selection preview.ReadySelection, metadata preview.ArtifactMetadata) error {
	if err := objectstore.ContextError(ctx); err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "preview store request canceled", err)
	}
	if !selection.Valid() || !metadata.ValidFor(selection.Binding) {
		return domain.NewError(domain.ErrorInvalid, "invalid preview ready entry")
	}
	scopeDigest := s.readyScopeDigest(selection.CacheScope)
	bindingDigest := s.bindingDigest(selection.Binding)
	key := readyCatalogKey(scopeDigest)
	for attempts := 0; attempts < maximumRetries; attempts++ {
		object, getErr := s.indexBackend.Get(ctx, key)
		record := readyCatalogRecord{SchemaVersion: schemaVersion, ScopeDigest: scopeDigest, Revision: 1}
		condition := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
		if getErr == nil {
			var decodeErr error
			record, decodeErr = decodeReadyCatalog(object.Body, scopeDigest)
			if decodeErr != nil {
				s.setReady(false)
				return domain.NewError(domain.ErrorUnavailable, "preview ready catalog is corrupt")
			}
			if record.Revision == math.MaxUint64 {
				return domain.NewError(domain.ErrorUnavailable, "preview ready catalog revision exhausted")
			}
			record.Revision++
			condition = objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}
		} else if !errors.Is(getErr, domain.ErrNotFound) {
			s.setReady(false)
			return getErr
		}
		index := sort.Search(len(record.Entries), func(index int) bool { return record.Entries[index].BindingDigest >= bindingDigest })
		entry := readyCatalogEntry{BindingDigest: bindingDigest, RecordedAt: s.clock.Now().UTC(), Artifact: metadata}
		if index < len(record.Entries) && record.Entries[index].BindingDigest == bindingDigest {
			record.Entries[index] = entry
		} else {
			record.Entries = append(record.Entries, readyCatalogEntry{})
			copy(record.Entries[index+1:], record.Entries[index:])
			record.Entries[index] = entry
		}
		if len(record.Entries) > maximumReadyCatalogItems {
			evict := 0
			for candidate := 1; candidate < len(record.Entries); candidate++ {
				if record.Entries[candidate].RecordedAt.Before(record.Entries[evict].RecordedAt) || record.Entries[candidate].RecordedAt.Equal(record.Entries[evict].RecordedAt) && record.Entries[candidate].BindingDigest < record.Entries[evict].BindingDigest {
					evict = candidate
				}
			}
			record.Entries = append(record.Entries[:evict], record.Entries[evict+1:]...)
		}
		body, encodeErr := encodeReadyCatalog(record)
		if encodeErr != nil {
			return encodeErr
		}
		if _, putErr := s.indexBackend.Put(ctx, key, body, condition); putErr == nil {
			return nil
		} else if !errors.Is(putErr, domain.ErrConflict) && !errors.Is(putErr, domain.ErrPreconditionFailed) {
			s.setReady(false)
			return putErr
		}
	}
	return domain.NewError(domain.ErrorUnavailable, "preview ready catalog contention exceeded")
}

func (s *Store) Read(ctx context.Context, binding preview.Binding, generationID string) (preview.Artifact, error) {
	if err := objectstore.ContextError(ctx); err != nil {
		return preview.Artifact{}, domain.WrapError(domain.ErrorUnavailable, "preview store request canceled", err)
	}
	if !binding.Valid() || generationID == "" {
		return preview.Artifact{}, domain.NewError(domain.ErrorInvalid, "invalid preview artifact selection")
	}
	digest := s.bindingDigest(binding)
	metadata, err := s.committedMetadata(ctx, binding, digest, generationID)
	if err != nil {
		return preview.Artifact{}, err
	}
	object, err := s.validateStoredGeneration(ctx, binding, digest, metadata, true)
	if err != nil {
		return preview.Artifact{}, err
	}
	artifact := preview.Artifact{
		GenerationID: metadata.GenerationID, Variant: metadata.Variant, Width: metadata.Width, Height: metadata.Height,
		ContentType: metadata.ContentType, Size: metadata.Size, SHA256: metadata.SHA256, CRC32C: metadata.CRC32C, Bytes: object.Body,
	}
	return artifact, nil
}

func (s *Store) CreateDownload(ctx context.Context, binding preview.Binding, generationID string) (domain.DownloadCapability, error) {
	if err := objectstore.ContextError(ctx); err != nil {
		return domain.DownloadCapability{}, domain.WrapError(domain.ErrorUnavailable, "preview store request canceled", err)
	}
	if !binding.Valid() || generationID == "" {
		return domain.DownloadCapability{}, domain.NewError(domain.ErrorInvalid, "invalid preview binding")
	}
	digest := s.bindingDigest(binding)
	metadata, err := s.committedMetadata(ctx, binding, digest, generationID)
	if err != nil {
		return domain.DownloadCapability{}, err
	}
	object, err := s.validateStoredGeneration(ctx, binding, digest, metadata, false)
	if err != nil {
		return domain.DownloadCapability{}, err
	}
	capability, err := s.transfers.CreateDownload(ctx, objectstore.DownloadRequest{
		Key: object.Key, Version: object.Version, Filename: "preview.webp", MediaType: preview.ContentTypeWebP,
		Disposition: domain.DispositionInline, ExpiresAt: s.clock.Now().Add(s.capabilityTTL),
	})
	if err != nil {
		s.setReady(false)
		return domain.DownloadCapability{}, err
	}
	return domain.DownloadCapability{URL: capability.URL, Method: capability.Method, Headers: capability.Headers, ExpiresAt: capability.ExpiresAt}, nil
}

func (s *Store) CreateKnownDownload(ctx context.Context, binding preview.Binding, metadata preview.ArtifactMetadata) (domain.DownloadCapability, error) {
	if err := objectstore.ContextError(ctx); err != nil {
		return domain.DownloadCapability{}, domain.WrapError(domain.ErrorUnavailable, "preview store request canceled", err)
	}
	if !metadata.ValidFor(binding) {
		return domain.DownloadCapability{}, domain.NewError(domain.ErrorInvalid, "invalid preview ready artifact")
	}
	digest := s.bindingDigest(binding)
	generationDigest := s.generationDigest(digest, metadata.GenerationID)
	capability, err := s.transfers.CreateDownload(ctx, objectstore.DownloadRequest{
		Key: generationArtifactKey(digest, generationDigest), Immutable: true, Filename: "preview.webp", MediaType: preview.ContentTypeWebP,
		Disposition: domain.DispositionInline, ExpiresAt: s.clock.Now().Add(s.capabilityTTL),
	})
	if err != nil {
		s.setReady(false)
		return domain.DownloadCapability{}, err
	}
	return domain.DownloadCapability{URL: capability.URL, Method: capability.Method, Headers: capability.Headers, ExpiresAt: capability.ExpiresAt}, nil
}

func (s *Store) committedMetadata(ctx context.Context, binding preview.Binding, digest, generationID string) (preview.ArtifactMetadata, error) {
	_, record, err := s.readHead(ctx, binding, digest)
	if errors.Is(err, domain.ErrNotFound) {
		return preview.ArtifactMetadata{}, domain.NewError(domain.ErrorNotFound, "preview artifact not found")
	}
	if err != nil {
		return preview.ArtifactMetadata{}, err
	}
	metadata := findGeneration(record.Generations, generationID)
	if metadata == nil {
		return preview.ArtifactMetadata{}, domain.NewError(domain.ErrorNotFound, "preview artifact not found")
	}
	return *metadata, nil
}

func (s *Store) validateStoredGeneration(ctx context.Context, binding preview.Binding, digest string, metadata preview.ArtifactMetadata, withBody bool) (objectstore.Object, error) {
	if !metadata.ValidFor(binding) {
		s.setReady(false)
		return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "preview manifest is corrupt")
	}
	generationDigest := s.generationDigest(digest, metadata.GenerationID)
	manifestObject, err := s.backend.Get(ctx, generationManifestKey(digest, generationDigest))
	if errors.Is(err, domain.ErrNotFound) {
		return objectstore.Object{}, domain.NewError(domain.ErrorNotFound, "preview artifact not found")
	}
	if err != nil {
		s.setReady(false)
		return objectstore.Object{}, err
	}
	var manifest manifestRecord
	if err := decode(manifestObject.Body, &manifest); err != nil || manifest.SchemaVersion != schemaVersion || manifest.BindingDigest != digest || manifest.Artifact != metadata {
		s.setReady(false)
		return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "preview manifest is corrupt")
	}
	key := generationArtifactKey(digest, generationDigest)
	if !withBody {
		info, err := s.backend.Verify(ctx, key, objectstore.ExpectedIntegrity{
			Size: metadata.Size, Checksum: objectstore.Checksum{Algorithm: objectstore.ChecksumCRC32C, Value: metadata.CRC32C},
		})
		if errors.Is(err, domain.ErrNotFound) {
			return objectstore.Object{}, domain.NewError(domain.ErrorNotFound, "preview artifact not found")
		}
		if err != nil {
			s.setReady(false)
			if errors.Is(err, domain.ErrPreconditionFailed) {
				return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "preview artifact is corrupt")
			}
			return objectstore.Object{}, err
		}
		if info.Size != metadata.Size {
			s.setReady(false)
			return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "preview artifact is corrupt")
		}
		return objectstore.Object{Key: key, Version: info.Version, Size: info.Size}, nil
	}
	object, err := s.backend.Get(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		return objectstore.Object{}, domain.NewError(domain.ErrorNotFound, "preview artifact not found")
	}
	if err != nil {
		s.setReady(false)
		return objectstore.Object{}, err
	}
	if object.Size != metadata.Size {
		s.setReady(false)
		return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "preview artifact is corrupt")
	}
	artifact := preview.Artifact{
		GenerationID: metadata.GenerationID, Variant: metadata.Variant, Width: metadata.Width, Height: metadata.Height,
		ContentType: metadata.ContentType, Size: metadata.Size, SHA256: metadata.SHA256, CRC32C: metadata.CRC32C, Bytes: object.Body,
	}
	if !artifact.ValidFor(binding) {
		s.setReady(false)
		return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "preview artifact is corrupt")
	}
	return object, nil
}

func (s *Store) readHead(ctx context.Context, binding preview.Binding, digest string) (objectstore.Object, headRecord, error) {
	object, err := s.backend.Get(ctx, headKey(digest))
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			s.setReady(false)
			return objectstore.Object{}, headRecord{}, domain.NewError(domain.ErrorUnavailable, "preview store unavailable")
		}
		return objectstore.Object{}, headRecord{}, err
	}
	var record headRecord
	if err := decode(object.Body, &record); err != nil || !record.valid(binding, digest, s.maxGenerations) {
		s.setReady(false)
		return objectstore.Object{}, headRecord{}, domain.NewError(domain.ErrorUnavailable, "preview store record is corrupt")
	}
	return object, record, nil
}

func (record headRecord) valid(binding preview.Binding, digest string, maxGenerations int) bool {
	if record.SchemaVersion != schemaVersion || record.BindingDigest != digest || record.Epoch == 0 || len(record.Generations) > maxGenerations {
		return false
	}
	if (record.ClaimID == "") != record.ClaimExpires.IsZero() || len(record.ClaimID) > 128 || strings.ContainsAny(record.ClaimID, "\r\n\x00/") {
		return false
	}
	seen := make(map[string]bool, len(record.Generations))
	for _, metadata := range record.Generations {
		if !metadata.ValidFor(binding) || seen[metadata.GenerationID] {
			return false
		}
		seen[metadata.GenerationID] = true
	}
	return true
}

func (s *Store) putImmutableOrVerify(ctx context.Context, key objectstore.Key, body []byte) error {
	_, err := s.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
	if err == nil {
		return nil
	}
	if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
		s.setReady(false)
		return err
	}
	existing, getErr := s.backend.Get(ctx, key)
	if getErr == nil && bytes.Equal(existing.Body, body) {
		return nil
	}
	if getErr != nil && !errors.Is(getErr, domain.ErrNotFound) {
		s.setReady(false)
		return getErr
	}
	return domain.NewError(domain.ErrorConflict, "preview immutable object already exists")
}

func (s *Store) cleanupProbe(ctx context.Context, binding preview.Binding, generationID string) {
	digest := s.bindingDigest(binding)
	generationDigest := s.generationDigest(digest, generationID)
	for _, key := range []objectstore.Key{generationArtifactKey(digest, generationDigest), generationManifestKey(digest, generationDigest), headKey(digest)} {
		info, err := s.backend.Head(ctx, key)
		if err == nil {
			_ = s.backend.Delete(ctx, key, objectstore.DeleteCondition{Version: info.Version})
		}
	}
}

func (s *Store) bindingDigest(binding preview.Binding) string {
	hasher := hmac.New(sha256.New, s.key)
	writeFields(
		hasher, "binding-v1", binding.Owner.String(), string(binding.ContentID), string(binding.ContentVersion),
		binding.MediaType, binding.RecipeID, strconv.FormatInt(binding.SourceSize, 10), strconv.Itoa(binding.Variant),
	)
	return hex.EncodeToString(hasher.Sum(nil))
}

func (s *Store) generationDigest(bindingDigest, generationID string) string {
	hasher := hmac.New(sha256.New, s.key)
	writeFields(hasher, "generation-v1", bindingDigest, generationID)
	return hex.EncodeToString(hasher.Sum(nil))
}

func (s *Store) readyScopeDigest(cacheScope string) string {
	hasher := hmac.New(sha256.New, s.key)
	writeFields(hasher, "ready-scope-v1", cacheScope)
	return hex.EncodeToString(hasher.Sum(nil))
}

func writeFields(hasher hash.Hash, fields ...string) {
	var length [8]byte
	for _, field := range fields {
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		_, _ = hasher.Write(length[:])
		_, _ = hasher.Write([]byte(field))
	}
}

func headKey(bindingDigest string) objectstore.Key {
	return objectstore.MustKey("preview/v1/b/" + bindingDigest + "/head.json")
}

func generationArtifactKey(bindingDigest, generationDigest string) objectstore.Key {
	return objectstore.MustKey("preview/v1/b/" + bindingDigest + "/g/" + generationDigest + "/artifact.webp")
}

func generationManifestKey(bindingDigest, generationDigest string) objectstore.Key {
	return objectstore.MustKey("preview/v1/b/" + bindingDigest + "/g/" + generationDigest + "/manifest.json")
}

func readyCatalogKey(scopeDigest string) objectstore.Key {
	return objectstore.MustKey("preview/v1/i/" + scopeDigest + "/ready.json")
}

func encodeReadyCatalog(record readyCatalogRecord) ([]byte, error) {
	if err := validateReadyCatalog(record); err != nil {
		return nil, err
	}
	body, err := json.Marshal(record)
	if err != nil || len(body) > int(maximumReadyCatalogBytes) {
		return nil, domain.NewError(domain.ErrorInvalid, "preview ready catalog exceeds its bounded envelope")
	}
	return body, nil
}

func decodeReadyCatalog(body []byte, scopeDigest string) (readyCatalogRecord, error) {
	if len(body) == 0 || len(body) > int(maximumReadyCatalogBytes) || !json.Valid(body) {
		return readyCatalogRecord{}, errors.New("invalid preview ready catalog")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var record readyCatalogRecord
	if err := decoder.Decode(&record); err != nil {
		return readyCatalogRecord{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) || record.ScopeDigest != scopeDigest {
		return readyCatalogRecord{}, errors.New("invalid preview ready catalog binding")
	}
	if err := validateReadyCatalog(record); err != nil {
		return readyCatalogRecord{}, err
	}
	canonical, err := json.Marshal(record)
	if err != nil || !bytes.Equal(canonical, body) {
		return readyCatalogRecord{}, errors.New("non-canonical preview ready catalog")
	}
	return record, nil
}

func validateReadyCatalog(record readyCatalogRecord) error {
	if record.SchemaVersion != schemaVersion || len(record.ScopeDigest) != sha256.Size*2 || record.Revision == 0 || len(record.Entries) > maximumReadyCatalogItems {
		return domain.NewError(domain.ErrorInvalid, "invalid preview ready catalog")
	}
	previous := ""
	for _, entry := range record.Entries {
		if len(entry.BindingDigest) != sha256.Size*2 || previous != "" && entry.BindingDigest <= previous || entry.RecordedAt.IsZero() || entry.Artifact.GenerationID == "" {
			return domain.NewError(domain.ErrorInvalid, "invalid preview ready catalog entry")
		}
		if _, err := hex.DecodeString(entry.BindingDigest); err != nil {
			return domain.NewError(domain.ErrorInvalid, "invalid preview ready catalog entry")
		}
		previous = entry.BindingDigest
	}
	return nil
}

func encode(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic("encode fixed preview record: " + err.Error())
	}
	return data
}

func decode(data []byte, value any) error {
	if int64(len(data)) > maximumRecordBytes || !json.Valid(data) {
		return errors.New("invalid preview record")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing preview record data")
	}
	return nil
}

func sameClaim(record headRecord, claim preview.GenerationClaim) bool {
	return record.ClaimID == claim.ID && record.Epoch == claim.Epoch && record.ClaimExpires.Equal(claim.ExpiresAt)
}

func findGeneration(generations []preview.ArtifactMetadata, generationID string) *preview.ArtifactMetadata {
	for index := range generations {
		if generations[index].GenerationID == generationID {
			return &generations[index]
		}
	}
	return nil
}

func validationBinding(generationID string) preview.Binding {
	owner, _ := domain.ParseUserID("AAAAAAAAAAAAAAAAAAAAAA")
	return preview.Binding{
		Owner: owner, ContentID: domain.ContentID("startup-probe-" + generationID), ContentVersion: "startup-probe-version",
		MediaType: "image/webp", SourceSize: 1, RecipeID: "startup-probe-v1", Variant: 64,
	}
}

var _ preview.Store = (*Store)(nil)
