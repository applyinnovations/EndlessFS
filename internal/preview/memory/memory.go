// Package memory provides the deterministic, independently faultable preview
// artifact store used for local proof.
package memory

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/preview"
	"github.com/applyinnovations/endlessfs/internal/secret"
)

type Options struct {
	Clock            domain.Clock
	IDs              *domain.IDGenerator
	Key              secret.Value
	CapabilityTTL    time.Duration
	AllowedOrigin    string
	MaxGenerations   int
	MaxCapabilities  int
	MaxBindings      int
	MaxArtifactBytes int64
}

type capability struct {
	key          string
	generationID string
	expiresAt    time.Time
}

type Store struct {
	mu sync.Mutex

	clock            domain.Clock
	ids              *domain.IDGenerator
	key              []byte
	capabilityTTL    time.Duration
	allowedOrigin    string
	baseURL          string
	available        bool
	ready            bool
	artifacts        map[string][]preview.Artifact
	claims           map[string]preview.GenerationClaim
	capabilities     map[[sha256.Size]byte]capability
	maxGenerations   int
	maxCapabilities  int
	maxBindings      int
	maxArtifactBytes int64
	artifactBytes    int64
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
	if options.MaxGenerations == 0 {
		options.MaxGenerations = 4
	}
	if options.MaxCapabilities == 0 {
		options.MaxCapabilities = 4096
	}
	if options.MaxBindings == 0 {
		options.MaxBindings = 4096
	}
	if options.MaxArtifactBytes == 0 {
		options.MaxArtifactBytes = 512 << 20
	}
	if !secret.ValidBearerToken(options.Key.Reveal()) || options.CapabilityTTL <= 0 || options.CapabilityTTL > 10*time.Minute || options.MaxGenerations < 1 || options.MaxGenerations > 32 || options.MaxCapabilities < 1 || options.MaxCapabilities > 65536 || options.MaxBindings < 1 || options.MaxBindings > 65536 || options.MaxArtifactBytes < int64(len(preview.OnePixelWebP())) || options.MaxArtifactBytes > 4<<30 {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid preview store configuration")
	}
	key, err := base64.RawURLEncoding.DecodeString(options.Key.Reveal())
	if err != nil {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid preview store configuration")
	}
	return &Store{
		clock: options.Clock, ids: options.IDs, key: key, capabilityTTL: options.CapabilityTTL,
		allowedOrigin: options.AllowedOrigin, available: true, artifacts: make(map[string][]preview.Artifact), claims: make(map[string]preview.GenerationClaim),
		maxGenerations: options.MaxGenerations, maxCapabilities: options.MaxCapabilities, maxBindings: options.MaxBindings, maxArtifactBytes: options.MaxArtifactBytes,
		capabilities: make(map[[sha256.Size]byte]capability),
	}, nil
}

func (s *Store) SetDataPlaneBaseURL(baseURL string) error {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return domain.NewError(domain.ErrorInvalid, "mock preview data plane must use a loopback URL")
	}
	ip := net.ParseIP(parsed.Hostname())
	if parsed.Scheme != "http" || parsed.Port() == "" || ip == nil || !ip.IsLoopback() || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return domain.NewError(domain.ErrorInvalid, "mock preview data plane must use a loopback URL")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.baseURL = strings.TrimRight(baseURL, "/")
	return nil
}

func (s *Store) SetAvailable(available bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.available = available
}

func (s *Store) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

func (s *Store) DataOrigin() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.baseURL
}

func (s *Store) Validate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "preview store validation timed out", err)
	}
	s.mu.Lock()
	s.ready = false
	if !s.available || s.baseURL == "" {
		s.mu.Unlock()
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: access")
	}
	s.mu.Unlock()
	generationID, err := s.ids.OpaqueID()
	if err != nil {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: create")
	}
	binding := validationBinding(generationID)
	defer s.cleanupValidationProbe(binding, generationID)
	data := preview.OnePixelWebP()
	sum := sha256.Sum256(data)
	artifact := preview.Artifact{
		GenerationID: generationID, Variant: 64, Width: 1, Height: 1, ContentType: preview.ContentTypeWebP,
		Size: int64(len(data)), SHA256: base64.RawURLEncoding.EncodeToString(sum[:]), Bytes: data,
	}
	if !artifact.ValidFor(binding) {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: read")
	}
	claim, err := s.Claim(ctx, binding, generationID, s.clock.Now().Add(time.Minute))
	if err != nil {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: claim")
	}
	if err := s.Commit(ctx, binding, claim, artifact); err != nil {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: create")
	}
	latest, err := s.Latest(ctx, binding)
	if err != nil || latest.GenerationID != generationID {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: manifest")
	}
	stored, err := s.Read(ctx, binding, generationID)
	if err != nil || !stored.ValidFor(binding) || !bytes.Equal(stored.Bytes, data) {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: read")
	}
	capability, err := s.CreateDownload(ctx, binding, generationID)
	if err != nil {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: capability")
	}
	request := httptest.NewRequest(capability.Method, capability.URL, nil)
	if s.allowedOrigin != "" {
		request.Header.Set("Origin", s.allowedOrigin)
	}
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != preview.ContentTypeWebP || !bytes.Equal(response.Body.Bytes(), data) {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: capability")
	}
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
	return nil
}

func validationBinding(generationID string) preview.Binding {
	owner, _ := domain.ParseUserID("AAAAAAAAAAAAAAAAAAAAAA")
	return preview.Binding{
		Owner: owner, ContentID: domain.ContentID("startup-probe-" + generationID), ContentVersion: "startup-probe-version",
		MediaType: "image/webp", SourceSize: 1, RecipeID: "startup-probe-v1", Variant: 64,
	}
}

func (s *Store) cleanupValidationProbe(binding preview.Binding, generationID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.bindingKey(binding)
	for _, artifact := range s.artifacts[key] {
		s.artifactBytes -= artifact.Size
	}
	delete(s.artifacts, key)
	delete(s.claims, key)
	for token, value := range s.capabilities {
		if value.key == key && value.generationID == generationID {
			delete(s.capabilities, token)
		}
	}
}

func (s *Store) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "preview store check timed out", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.available || s.baseURL == "" {
		s.ready = false
		return domain.NewError(domain.ErrorUnavailable, "preview store unavailable")
	}
	return nil
}

func (s *Store) Claim(ctx context.Context, binding preview.Binding, claimID string, expiresAt time.Time) (preview.GenerationClaim, error) {
	if err := ctx.Err(); err != nil {
		return preview.GenerationClaim{}, domain.WrapError(domain.ErrorUnavailable, "preview store request canceled", err)
	}
	if !binding.Valid() || claimID == "" || len(claimID) > 128 || strings.ContainsAny(claimID, "\r\n\x00/") || !s.clock.Now().Before(expiresAt) {
		return preview.GenerationClaim{}, domain.NewError(domain.ErrorInvalid, "invalid preview generation claim")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireAvailableLocked(); err != nil {
		return preview.GenerationClaim{}, err
	}
	key := s.bindingKey(binding)
	current, knownBinding := s.claims[key]
	if current.Valid() && s.clock.Now().Before(current.ExpiresAt) {
		return current, domain.NewError(domain.ErrorConflict, "preview generation is already claimed")
	}
	if _, exists := s.artifacts[key]; !exists && !knownBinding && s.bindingCountLocked() >= s.maxBindings {
		return preview.GenerationClaim{}, domain.NewError(domain.ErrorUnavailable, "preview binding capacity reached")
	}
	claim := preview.GenerationClaim{ID: claimID, Epoch: current.Epoch + 1, ExpiresAt: expiresAt.UTC()}
	s.claims[key] = claim
	return claim, nil
}

func (s *Store) Release(ctx context.Context, binding preview.Binding, claim preview.GenerationClaim) error {
	if err := ctx.Err(); err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "preview store request canceled", err)
	}
	if !binding.Valid() || !claim.Valid() {
		return domain.NewError(domain.ErrorInvalid, "invalid preview generation claim")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireAvailableLocked(); err != nil {
		return err
	}
	key := s.bindingKey(binding)
	if s.claims[key] != claim {
		return domain.NewError(domain.ErrorPreconditionFailed, "preview generation claim changed")
	}
	s.claims[key] = preview.GenerationClaim{Epoch: claim.Epoch}
	return nil
}

func (s *Store) Commit(ctx context.Context, binding preview.Binding, claim preview.GenerationClaim, artifact preview.Artifact) error {
	if err := ctx.Err(); err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "preview store request canceled", err)
	}
	if !binding.Valid() || !claim.Valid() || !artifact.ValidFor(binding) {
		return domain.NewError(domain.ErrorInvalid, "invalid preview artifact")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireAvailableLocked(); err != nil {
		return err
	}
	key := s.bindingKey(binding)
	if s.claims[key] != claim || !s.clock.Now().Before(claim.ExpiresAt) {
		return domain.NewError(domain.ErrorPreconditionFailed, "preview generation claim changed or expired")
	}
	for _, existing := range s.artifacts[key] {
		if existing.GenerationID == artifact.GenerationID {
			return domain.NewError(domain.ErrorConflict, "preview generation already exists")
		}
	}
	if !s.makeArtifactCapacityLocked(key) {
		return domain.NewError(domain.ErrorUnavailable, "preview generation retention capacity reached")
	}
	if s.artifactBytes > s.maxArtifactBytes-artifact.Size {
		return domain.NewError(domain.ErrorUnavailable, "preview artifact byte capacity reached")
	}
	s.artifacts[key] = append(s.artifacts[key], cloneArtifact(artifact))
	s.artifactBytes += artifact.Size
	s.claims[key] = preview.GenerationClaim{Epoch: claim.Epoch}
	return nil
}

func (s *Store) Latest(ctx context.Context, binding preview.Binding) (preview.ArtifactMetadata, error) {
	if err := ctx.Err(); err != nil {
		return preview.ArtifactMetadata{}, domain.WrapError(domain.ErrorUnavailable, "preview store request canceled", err)
	}
	if !binding.Valid() {
		return preview.ArtifactMetadata{}, domain.NewError(domain.ErrorInvalid, "invalid preview binding")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireAvailableLocked(); err != nil {
		return preview.ArtifactMetadata{}, err
	}
	items := s.artifacts[s.bindingKey(binding)]
	if len(items) == 0 {
		return preview.ArtifactMetadata{}, domain.NewError(domain.ErrorNotFound, "preview artifact not found")
	}
	return items[len(items)-1].Metadata(), nil
}

func (s *Store) Read(ctx context.Context, binding preview.Binding, generationID string) (preview.Artifact, error) {
	if err := ctx.Err(); err != nil {
		return preview.Artifact{}, domain.WrapError(domain.ErrorUnavailable, "preview store request canceled", err)
	}
	if !binding.Valid() || generationID == "" {
		return preview.Artifact{}, domain.NewError(domain.ErrorInvalid, "invalid preview artifact selection")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireAvailableLocked(); err != nil {
		return preview.Artifact{}, err
	}
	for _, artifact := range s.artifacts[s.bindingKey(binding)] {
		if artifact.GenerationID == generationID {
			return cloneArtifact(artifact), nil
		}
	}
	return preview.Artifact{}, domain.NewError(domain.ErrorNotFound, "preview artifact not found")
}

func (s *Store) CreateDownload(ctx context.Context, binding preview.Binding, generationID string) (domain.DownloadCapability, error) {
	if err := ctx.Err(); err != nil {
		return domain.DownloadCapability{}, domain.WrapError(domain.ErrorUnavailable, "preview store request canceled", err)
	}
	if !binding.Valid() || generationID == "" {
		return domain.DownloadCapability{}, domain.NewError(domain.ErrorInvalid, "invalid preview binding")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireAvailableLocked(); err != nil {
		return domain.DownloadCapability{}, err
	}
	if s.baseURL == "" {
		return domain.DownloadCapability{}, domain.NewError(domain.ErrorUnavailable, "preview capability service is unavailable")
	}
	key := s.bindingKey(binding)
	items := s.artifacts[key]
	found := false
	for _, artifact := range items {
		found = found || artifact.GenerationID == generationID
	}
	if !found {
		return domain.DownloadCapability{}, domain.NewError(domain.ErrorNotFound, "preview artifact not found")
	}
	s.cleanupCapabilitiesLocked()
	if len(s.capabilities) >= s.maxCapabilities {
		return domain.DownloadCapability{}, domain.NewError(domain.ErrorUnavailable, "preview capability capacity reached")
	}
	token, err := s.ids.BearerToken()
	if err != nil {
		return domain.DownloadCapability{}, err
	}
	expiresAt := s.clock.Now().Add(s.capabilityTTL)
	s.capabilities[tokenHash(token)] = capability{key: key, generationID: generationID, expiresAt: expiresAt}
	return domain.DownloadCapability{URL: s.baseURL + "/cap/preview/" + token, Method: http.MethodGet, ExpiresAt: expiresAt}, nil
}

func (s *Store) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	if origin := request.Header.Get("Origin"); origin != "" {
		if s.allowedOrigin == "" || origin != s.allowedOrigin {
			http.Error(writer, "origin is not allowed", http.StatusForbidden)
			return
		}
		writer.Header().Set("Access-Control-Allow-Origin", s.allowedOrigin)
		writer.Header().Set("Vary", "Origin")
	}
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "cap" || parts[1] != "preview" || parts[2] == "" {
		http.NotFound(writer, request)
		return
	}
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	if err := s.requireAvailableLocked(); err != nil {
		s.mu.Unlock()
		http.Error(writer, "preview unavailable", http.StatusServiceUnavailable)
		return
	}
	capability, found := s.capabilities[tokenHash(parts[2])]
	if !found {
		s.mu.Unlock()
		http.NotFound(writer, request)
		return
	}
	if !s.clock.Now().Before(capability.expiresAt) {
		delete(s.capabilities, tokenHash(parts[2]))
		s.mu.Unlock()
		http.Error(writer, "capability unavailable", http.StatusGone)
		return
	}
	for _, artifact := range s.artifacts[capability.key] {
		if artifact.GenerationID != capability.generationID {
			continue
		}
		body := append([]byte(nil), artifact.Bytes...)
		size := artifact.Size
		s.mu.Unlock()
		writer.Header().Set("Content-Type", preview.ContentTypeWebP)
		writer.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(body)
		return
	}
	s.mu.Unlock()
	http.NotFound(writer, request)
}

func (s *Store) cleanupCapabilitiesLocked() {
	now := s.clock.Now()
	for token, value := range s.capabilities {
		if !now.Before(value.expiresAt) {
			delete(s.capabilities, token)
		}
	}
}

func (s *Store) bindingCountLocked() int {
	count := len(s.artifacts)
	for key := range s.claims {
		if _, exists := s.artifacts[key]; !exists {
			count++
		}
	}
	return count
}

func (s *Store) makeArtifactCapacityLocked(key string) bool {
	items := s.artifacts[key]
	if len(items) < s.maxGenerations {
		return true
	}
	s.cleanupCapabilitiesLocked()
	protected := make(map[string]struct{})
	for _, value := range s.capabilities {
		if value.key == key {
			protected[value.generationID] = struct{}{}
		}
	}
	retained := make([]preview.Artifact, 0, len(items))
	removed := false
	var removedSize int64
	for _, item := range items {
		if !removed {
			if _, exists := protected[item.GenerationID]; !exists {
				removed = true
				removedSize = item.Size
				continue
			}
		}
		retained = append(retained, item)
	}
	if removed {
		s.artifacts[key] = retained
		s.artifactBytes -= removedSize
	}
	return removed
}

func (s *Store) requireAvailableLocked() error {
	if !s.available {
		s.ready = false
		return domain.NewError(domain.ErrorUnavailable, "preview store unavailable")
	}
	return nil
}

func (s *Store) bindingKey(binding preview.Binding) string {
	mac := hmac.New(sha256.New, s.key)
	for _, value := range []string{
		binding.Owner.String(), string(binding.ContentID), string(binding.ContentVersion), binding.MediaType,
		strconv.FormatInt(binding.SourceSize, 10), binding.RecipeID, strconv.Itoa(binding.Variant),
	} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = mac.Write(size[:])
		_, _ = mac.Write([]byte(value))
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func cloneArtifact(value preview.Artifact) preview.Artifact {
	value.Bytes = append([]byte(nil), value.Bytes...)
	return value
}

func tokenHash(value string) [sha256.Size]byte { return sha256.Sum256([]byte(value)) }
