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
	Clock         domain.Clock
	IDs           *domain.IDGenerator
	Key           secret.Value
	CapabilityTTL time.Duration
	AllowedOrigin string
}

type capability struct {
	key          string
	generationID string
	expiresAt    time.Time
}

type Store struct {
	mu sync.Mutex

	clock         domain.Clock
	ids           *domain.IDGenerator
	key           []byte
	capabilityTTL time.Duration
	allowedOrigin string
	baseURL       string
	available     bool
	ready         bool
	artifacts     map[string][]preview.Artifact
	capabilities  map[[sha256.Size]byte]capability
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
	if !secret.ValidBearerToken(options.Key.Reveal()) || options.CapabilityTTL <= 0 || options.CapabilityTTL > 10*time.Minute {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid preview store configuration")
	}
	key, err := base64.RawURLEncoding.DecodeString(options.Key.Reveal())
	if err != nil {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid preview store configuration")
	}
	return &Store{
		clock: options.Clock, ids: options.IDs, key: key, capabilityTTL: options.CapabilityTTL,
		allowedOrigin: options.AllowedOrigin, available: true, artifacts: make(map[string][]preview.Artifact),
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
	owner, _ := domain.ParseUserID("AAAAAAAAAAAAAAAAAAAAAA")
	binding := preview.Binding{
		Owner: owner, ContentID: "startup-probe-content", ContentVersion: "startup-probe-version",
		MediaType: "image/webp", SourceSize: 1, RecipeID: "startup-probe-v1", Variant: 64,
	}
	generationID, err := s.ids.OpaqueID()
	if err != nil {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: create")
	}
	data := preview.OnePixelWebP()
	sum := sha256.Sum256(data)
	artifact := preview.Artifact{
		GenerationID: generationID, Variant: 64, Width: 1, Height: 1, ContentType: preview.ContentTypeWebP,
		Size: int64(len(data)), SHA256: base64.RawURLEncoding.EncodeToString(sum[:]), Bytes: data,
	}
	if !artifact.ValidFor(binding) {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: read")
	}
	if err := s.Commit(ctx, binding, artifact); err != nil {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: create")
	}
	stored, err := s.Latest(ctx, binding)
	if err != nil || !stored.ValidFor(binding) || !bytes.Equal(stored.Bytes, data) {
		return domain.NewError(domain.ErrorUnavailable, "preview store validation failed: read")
	}
	capability, err := s.CreateDownload(ctx, binding)
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

func (s *Store) Commit(ctx context.Context, binding preview.Binding, artifact preview.Artifact) error {
	if err := ctx.Err(); err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "preview store request canceled", err)
	}
	if !binding.Valid() || !artifact.ValidFor(binding) {
		return domain.NewError(domain.ErrorInvalid, "invalid preview artifact")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireAvailableLocked(); err != nil {
		return err
	}
	key := s.bindingKey(binding)
	for _, existing := range s.artifacts[key] {
		if existing.GenerationID == artifact.GenerationID {
			return domain.NewError(domain.ErrorConflict, "preview generation already exists")
		}
	}
	s.artifacts[key] = append(s.artifacts[key], cloneArtifact(artifact))
	return nil
}

func (s *Store) Latest(ctx context.Context, binding preview.Binding) (preview.Artifact, error) {
	if err := ctx.Err(); err != nil {
		return preview.Artifact{}, domain.WrapError(domain.ErrorUnavailable, "preview store request canceled", err)
	}
	if !binding.Valid() {
		return preview.Artifact{}, domain.NewError(domain.ErrorInvalid, "invalid preview binding")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireAvailableLocked(); err != nil {
		return preview.Artifact{}, err
	}
	items := s.artifacts[s.bindingKey(binding)]
	if len(items) == 0 {
		return preview.Artifact{}, domain.NewError(domain.ErrorNotFound, "preview artifact not found")
	}
	return cloneArtifact(items[len(items)-1]), nil
}

func (s *Store) CreateDownload(ctx context.Context, binding preview.Binding) (domain.DownloadCapability, error) {
	if err := ctx.Err(); err != nil {
		return domain.DownloadCapability{}, domain.WrapError(domain.ErrorUnavailable, "preview store request canceled", err)
	}
	if !binding.Valid() {
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
	if len(items) == 0 {
		return domain.DownloadCapability{}, domain.NewError(domain.ErrorNotFound, "preview artifact not found")
	}
	token, err := s.ids.BearerToken()
	if err != nil {
		return domain.DownloadCapability{}, err
	}
	expiresAt := s.clock.Now().Add(s.capabilityTTL)
	s.capabilities[tokenHash(token)] = capability{key: key, generationID: items[len(items)-1].GenerationID, expiresAt: expiresAt}
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
	defer s.mu.Unlock()
	if err := s.requireAvailableLocked(); err != nil {
		http.Error(writer, "preview unavailable", http.StatusServiceUnavailable)
		return
	}
	capability, found := s.capabilities[tokenHash(parts[2])]
	if !found {
		http.NotFound(writer, request)
		return
	}
	if !s.clock.Now().Before(capability.expiresAt) {
		http.Error(writer, "capability unavailable", http.StatusGone)
		return
	}
	for _, artifact := range s.artifacts[capability.key] {
		if artifact.GenerationID != capability.generationID {
			continue
		}
		writer.Header().Set("Content-Type", preview.ContentTypeWebP)
		writer.Header().Set("Content-Length", strconv.FormatInt(artifact.Size, 10))
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(artifact.Bytes)
		return
	}
	http.NotFound(writer, request)
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
