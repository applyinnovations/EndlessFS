package preview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/provider"
)

const (
	MaxResolveItems    = 64
	defaultHardMaxSize = int64(128 << 20)
)

type State string

const (
	StateDisabled    State = "disabled"
	StateUnsupported State = "unsupported"
	StateIneligible  State = "ineligible"
	StateMissing     State = "missing"
	StateGenerating  State = "generating"
	StateReady       State = "ready"
	StateFailed      State = "failed"
	StateUnavailable State = "unavailable"
)

type Options struct {
	Automatic          bool
	MaxAge             *time.Duration
	MaxSourceBytes     *int64
	Resolutions        []int
	MaxConcurrency     int
	OperationTimeout   time.Duration
	StartupTimeout     time.Duration
	HardMaxSourceBytes int64
}

type ItemRequest struct {
	Path    domain.UserPath `json:"path"`
	Version domain.Version  `json:"version"`
	Variant int             `json:"variant"`
}

type ResolveRequest struct {
	Items []ItemRequest `json:"items"`
}

type ItemResult struct {
	Path       domain.UserPath            `json:"path"`
	Version    domain.Version             `json:"version"`
	Variant    int                        `json:"variant"`
	State      State                      `json:"state"`
	Reason     string                     `json:"reason,omitempty"`
	Artifact   *ArtifactMetadata          `json:"artifact,omitempty"`
	Capability *domain.DownloadCapability `json:"capability,omitempty"`
}

type ResolveResponse struct {
	Items []ItemResult `json:"items"`
}

type GenerateRequest struct {
	Path           domain.UserPath
	Version        domain.Version
	Variant        int
	Regenerate     bool
	IdempotencyKey string
}

type Operation struct {
	ID        domain.OperationID    `json:"id"`
	State     domain.OperationState `json:"state"`
	ErrorKind domain.ErrorKind      `json:"errorKind,omitempty"`
	StartedAt time.Time             `json:"startedAt"`
	UpdatedAt time.Time             `json:"updatedAt"`
	Result    *ItemResult           `json:"result,omitempty"`
}

type GenerationRequest struct {
	Source     io.Reader
	SourceSize int64
	MediaType  string
	Variant    int
}

type GeneratedArtifact struct {
	Bytes  []byte
	Width  int
	Height int
}

type Generator interface {
	Capability() string
	RecipeID() string
	Supports(string) bool
	SelfTest(context.Context) error
	Generate(context.Context, GenerationRequest) (GeneratedArtifact, error)
}

type generationCall struct {
	done chan struct{}
	err  error
}

type idempotentOperation struct {
	fingerprint string
	operation   Operation
}

type Service struct {
	options    Options
	source     provider.Storage
	store      Store
	generators []Generator
	client     *http.Client
	ids        *domain.IDGenerator
	clock      domain.Clock
	global     chan struct{}

	mu         sync.Mutex
	perUser    map[string]chan struct{}
	inflight   map[string]*generationCall
	operations map[string]map[domain.OperationID]Operation
	idempotent map[string]idempotentOperation
}

func NewService(options Options, source provider.Storage, store Store, generators []Generator, client *http.Client, ids *domain.IDGenerator, clock domain.Clock) (*Service, error) {
	if source == nil || client == nil || ids == nil || clock == nil {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid preview service configuration")
	}
	if len(options.Resolutions) == 0 {
		options.Resolutions = []int{256, 512, 1600}
	}
	if options.MaxConcurrency == 0 {
		options.MaxConcurrency = 2
	}
	if options.OperationTimeout == 0 {
		options.OperationTimeout = 45 * time.Second
	}
	if options.StartupTimeout == 0 {
		options.StartupTimeout = 10 * time.Second
	}
	if options.HardMaxSourceBytes == 0 {
		options.HardMaxSourceBytes = defaultHardMaxSize
	}
	if err := validateOptions(options); err != nil {
		return nil, err
	}
	seenCapabilities := make(map[string]bool)
	startupContext, cancel := context.WithTimeout(context.Background(), options.StartupTimeout)
	defer cancel()
	for _, generator := range generators {
		if generator == nil || generator.Capability() == "" || seenCapabilities[generator.Capability()] {
			return nil, domain.NewError(domain.ErrorInvalid, "invalid preview generator registry")
		}
		seenCapabilities[generator.Capability()] = true
		if err := generator.SelfTest(startupContext); err != nil {
			return nil, domain.NewError(domain.ErrorUnavailable, "preview generator integrity check failed for "+generator.Capability())
		}
	}
	if store != nil {
		if err := store.Validate(startupContext); err != nil {
			return nil, domain.NewError(domain.ErrorUnavailable, "preview store access validation failed")
		}
	}
	return &Service{
		options: options, source: source, store: store, generators: append([]Generator(nil), generators...),
		client: client, ids: ids, clock: clock, global: make(chan struct{}, options.MaxConcurrency),
		perUser: make(map[string]chan struct{}), inflight: make(map[string]*generationCall),
		operations: make(map[string]map[domain.OperationID]Operation), idempotent: make(map[string]idempotentOperation),
	}, nil
}

func validateOptions(options Options) error {
	if options.MaxConcurrency < 1 || options.MaxConcurrency > 8 || options.OperationTimeout <= 0 || options.OperationTimeout > 5*time.Minute ||
		options.StartupTimeout <= 0 || options.StartupTimeout > time.Minute || options.HardMaxSourceBytes < 1 {
		return domain.NewError(domain.ErrorInvalid, "invalid preview execution limits")
	}
	if options.MaxAge != nil && *options.MaxAge <= 0 || options.MaxSourceBytes != nil && *options.MaxSourceBytes <= 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid preview automatic policy")
	}
	if len(options.Resolutions) < 1 || len(options.Resolutions) > 4 {
		return domain.NewError(domain.ErrorInvalid, "invalid preview resolutions")
	}
	previous := 0
	for _, resolution := range options.Resolutions {
		if resolution < 64 || resolution > 4096 || resolution <= previous {
			return domain.NewError(domain.ErrorInvalid, "invalid preview resolutions")
		}
		previous = resolution
	}
	return nil
}

func (s *Service) Resolve(ctx context.Context, owner domain.UserID, request ResolveRequest) (ResolveResponse, error) {
	if !owner.Valid() || len(request.Items) < 1 || len(request.Items) > MaxResolveItems {
		return ResolveResponse{}, domain.NewError(domain.ErrorInvalid, "preview resolve must contain 1 to 64 items")
	}
	response := ResolveResponse{Items: make([]ItemResult, 0, len(request.Items))}
	for _, item := range request.Items {
		if err := s.validateItem(item); err != nil {
			return ResolveResponse{}, err
		}
		result, err := s.resolveItem(ctx, owner, item, s.options.Automatic, false, false)
		if err != nil {
			return ResolveResponse{}, err
		}
		response.Items = append(response.Items, result)
	}
	return response, nil
}

func (s *Service) Generate(ctx context.Context, owner domain.UserID, request GenerateRequest) (Operation, error) {
	item := ItemRequest{Path: request.Path, Version: request.Version, Variant: request.Variant}
	if !owner.Valid() || errInvalidIdempotency(request.IdempotencyKey) != nil {
		return Operation{}, domain.NewError(domain.ErrorInvalid, "a valid Idempotency-Key is required")
	}
	if err := s.validateItem(item); err != nil {
		return Operation{}, err
	}
	fingerprint := item.Path.String() + "\x00" + string(item.Version) + "\x00" + strconv.Itoa(item.Variant) + "\x00" + strconv.FormatBool(request.Regenerate)
	idempotencyKey := owner.String() + "\x00" + request.IdempotencyKey
	s.mu.Lock()
	if existing, found := s.idempotent[idempotencyKey]; found {
		s.mu.Unlock()
		if existing.fingerprint != fingerprint {
			return Operation{}, domain.NewError(domain.ErrorConflict, "idempotency key was already used for another preview request")
		}
		return existing.operation, nil
	}
	s.mu.Unlock()
	operationID, err := s.ids.OpaqueID()
	if err != nil {
		return Operation{}, err
	}
	now := s.clock.Now().UTC()
	operation := Operation{ID: domain.OperationID(operationID), State: domain.OperationRunning, StartedAt: now, UpdatedAt: now}
	result, resolveErr := s.resolveItem(ctx, owner, item, true, request.Regenerate, true)
	operation.UpdatedAt = s.clock.Now().UTC()
	if resolveErr != nil {
		if errors.Is(resolveErr, domain.ErrInvalid) || errors.Is(resolveErr, domain.ErrUnauthorized) || errors.Is(resolveErr, domain.ErrNotFound) || errors.Is(resolveErr, domain.ErrPreconditionFailed) {
			return Operation{}, resolveErr
		}
		operation.State = domain.OperationFailed
		operation.ErrorKind = domain.KindOf(resolveErr)
	} else if result.State == StateReady {
		operation.State = domain.OperationSucceeded
		operation.Result = &result
	} else {
		operation.State = domain.OperationFailed
		operation.ErrorKind = stateErrorKind(result.State)
		operation.Result = &result
	}
	s.mu.Lock()
	if s.operations[owner.String()] == nil {
		s.operations[owner.String()] = make(map[domain.OperationID]Operation)
	}
	s.operations[owner.String()][operation.ID] = operation
	s.idempotent[idempotencyKey] = idempotentOperation{fingerprint: fingerprint, operation: operation}
	s.mu.Unlock()
	return operation, nil
}

func (s *Service) Operation(_ context.Context, owner domain.UserID, operationID domain.OperationID) (Operation, error) {
	if !owner.Valid() || operationID == "" {
		return Operation{}, domain.NewError(domain.ErrorInvalid, "invalid preview operation")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	operation, found := s.operations[owner.String()][operationID]
	if !found {
		return Operation{}, domain.NewError(domain.ErrorNotFound, "preview operation not found")
	}
	return operation, nil
}

func (s *Service) Ready() bool { return s.store == nil || s.store.Ready() }

func (s *Service) DataOrigin() string {
	if s.store == nil {
		return ""
	}
	return s.store.DataOrigin()
}

func (s *Service) validateItem(item ItemRequest) error {
	if !item.Path.Valid() || item.Path.IsRoot() || item.Version == "" || !slices.Contains(s.options.Resolutions, item.Variant) {
		return domain.NewError(domain.ErrorInvalid, "invalid preview item")
	}
	return nil
}

func (s *Service) resolveItem(ctx context.Context, owner domain.UserID, item ItemRequest, allowGeneration, force, explicit bool) (ItemResult, error) {
	result := ItemResult{Path: item.Path, Version: item.Version, Variant: item.Variant}
	if s.store == nil {
		result.State = StateDisabled
		return result, nil
	}
	scope, err := domain.NewScope(owner, domain.AreaLive)
	if err != nil {
		return ItemResult{}, err
	}
	entry, err := s.source.Stat(ctx, scope, item.Path)
	if err != nil {
		return ItemResult{}, err
	}
	if entry.Kind != domain.EntryFile || entry.Version != item.Version {
		return ItemResult{}, domain.NewError(domain.ErrorPreconditionFailed, "preview source version does not match")
	}
	generator := s.generatorFor(entry.MediaType)
	if generator == nil {
		result.State, result.Reason = StateUnsupported, "input-format"
		return result, nil
	}
	binding := Binding{
		Owner: owner, ContentID: entry.ContentID, ContentVersion: entry.ContentVersion, MediaType: entry.MediaType,
		SourceSize: entry.Size, RecipeID: generator.RecipeID(), Variant: item.Variant,
	}
	if !binding.Valid() {
		return ItemResult{}, domain.NewError(domain.ErrorInternal, "source provider omitted preview content identity")
	}
	if !force {
		ready, found, resolveErr := s.readyResult(ctx, binding, result)
		if resolveErr != nil || found {
			return ready, resolveErr
		}
	}
	if !allowGeneration {
		result.State = StateMissing
		return result, nil
	}
	if !explicit {
		if s.options.MaxAge != nil && entry.ContentModifiedAt.Before(s.clock.Now().Add(-*s.options.MaxAge)) {
			result.State, result.Reason = StateIneligible, "age"
			return result, nil
		}
		if s.options.MaxSourceBytes != nil && entry.Size > *s.options.MaxSourceBytes {
			result.State, result.Reason = StateIneligible, "size"
			return result, nil
		}
	}
	if err := s.generateOnce(ctx, scope, entry, binding, generator, force); err != nil {
		if errors.Is(err, domain.ErrUnavailable) {
			result.State = StateUnavailable
			return result, nil
		}
		result.State = StateFailed
		return result, nil
	}
	ready, found, err := s.readyResult(ctx, binding, result)
	if err != nil {
		return ItemResult{}, err
	}
	if !found {
		result.State = StateFailed
		return result, nil
	}
	return ready, nil
}

func (s *Service) readyResult(ctx context.Context, binding Binding, result ItemResult) (ItemResult, bool, error) {
	artifact, err := s.store.Latest(ctx, binding)
	if errors.Is(err, domain.ErrNotFound) {
		return result, false, nil
	}
	if errors.Is(err, domain.ErrUnavailable) {
		result.State = StateUnavailable
		return result, true, nil
	}
	if err != nil {
		result.State = StateFailed
		return result, true, nil
	}
	capability, err := s.store.CreateDownload(ctx, binding)
	if errors.Is(err, domain.ErrUnavailable) {
		result.State = StateUnavailable
		return result, true, nil
	}
	if err != nil {
		return ItemResult{}, false, err
	}
	metadata := artifact.Metadata()
	result.State, result.Artifact, result.Capability = StateReady, &metadata, &capability
	return result, true, nil
}

func (s *Service) generateOnce(ctx context.Context, scope domain.Scope, entry domain.Entry, binding Binding, generator Generator, force bool) error {
	key := generationKey(binding)
	s.mu.Lock()
	if call, found := s.inflight[key]; found {
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return domain.WrapError(domain.ErrorUnavailable, "preview generation canceled", ctx.Err())
		case <-call.done:
			return call.err
		}
	}
	call := &generationCall{done: make(chan struct{})}
	s.inflight[key] = call
	s.mu.Unlock()

	call.err = s.generate(ctx, scope, entry, binding, generator, force)
	s.mu.Lock()
	delete(s.inflight, key)
	close(call.done)
	s.mu.Unlock()
	return call.err
}

func (s *Service) generate(ctx context.Context, scope domain.Scope, entry domain.Entry, binding Binding, generator Generator, force bool) error {
	operationContext, cancel := context.WithTimeout(ctx, s.options.OperationTimeout)
	defer cancel()
	release, err := s.acquire(operationContext, scope.UserID())
	if err != nil {
		return err
	}
	defer release()
	if !force {
		if _, err := s.store.Latest(operationContext, binding); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
	}
	capability, err := s.source.CreateDownload(operationContext, scope, domain.CreateDownloadRequest{Path: entry.Path, Version: entry.Version, Disposition: domain.DispositionInline})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(operationContext, capability.Method, capability.URL, nil)
	if err != nil {
		return domain.WrapError(domain.ErrorInternal, "could not construct preview source request", err)
	}
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "preview source unavailable", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return domain.NewError(domain.ErrorUnavailable, "preview source unavailable")
	}
	sourceBytes, err := io.ReadAll(io.LimitReader(response.Body, s.options.HardMaxSourceBytes+1))
	if err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "preview source unavailable", err)
	}
	if int64(len(sourceBytes)) > s.options.HardMaxSourceBytes || int64(len(sourceBytes)) != entry.Size {
		return domain.NewError(domain.ErrorInvalid, "preview source exceeds hard limits")
	}
	generated, err := generator.Generate(operationContext, GenerationRequest{Source: bytes.NewReader(sourceBytes), SourceSize: entry.Size, MediaType: entry.MediaType, Variant: binding.Variant})
	if err != nil {
		return domain.NewError(domain.ErrorInvalid, "preview generator rejected source")
	}
	generationID, err := s.ids.OpaqueID()
	if err != nil {
		return err
	}
	sum := sha256.Sum256(generated.Bytes)
	artifact := Artifact{
		GenerationID: generationID, Variant: binding.Variant, Width: generated.Width, Height: generated.Height,
		ContentType: ContentTypeWebP, Size: int64(len(generated.Bytes)), SHA256: base64.RawURLEncoding.EncodeToString(sum[:]), Bytes: generated.Bytes,
	}
	if !artifact.ValidFor(binding) {
		return domain.NewError(domain.ErrorInvalid, "preview generator produced invalid artifact")
	}
	return s.store.Commit(operationContext, binding, artifact)
}

func (s *Service) acquire(ctx context.Context, owner domain.UserID) (func(), error) {
	select {
	case s.global <- struct{}{}:
	case <-ctx.Done():
		return nil, domain.WrapError(domain.ErrorUnavailable, "preview concurrency wait canceled", ctx.Err())
	}
	s.mu.Lock()
	userSemaphore := s.perUser[owner.String()]
	if userSemaphore == nil {
		userSemaphore = make(chan struct{}, 1)
		s.perUser[owner.String()] = userSemaphore
	}
	s.mu.Unlock()
	select {
	case userSemaphore <- struct{}{}:
		return func() { <-userSemaphore; <-s.global }, nil
	case <-ctx.Done():
		<-s.global
		return nil, domain.WrapError(domain.ErrorUnavailable, "preview concurrency wait canceled", ctx.Err())
	}
}

func (s *Service) generatorFor(mediaType string) Generator {
	for _, generator := range s.generators {
		if generator.Supports(mediaType) {
			return generator
		}
	}
	return nil
}

func generationKey(binding Binding) string {
	return strings.Join([]string{
		binding.Owner.String(), string(binding.ContentID), string(binding.ContentVersion), binding.MediaType,
		strconv.FormatInt(binding.SourceSize, 10), binding.RecipeID, strconv.Itoa(binding.Variant),
	}, "\x00")
}

func errInvalidIdempotency(value string) error {
	if len(value) < 16 || len(value) > 128 || strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("invalid")
	}
	return nil
}

func stateErrorKind(state State) domain.ErrorKind {
	if state == StateUnavailable {
		return domain.ErrorUnavailable
	}
	return domain.ErrorInvalid
}
