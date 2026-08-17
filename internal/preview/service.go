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
	"github.com/applyinnovations/endlessfs/internal/state"
)

const (
	MaxResolveItems       = 64
	defaultHardMaxSize    = int64(128 << 20)
	operationIndexShards  = 16
	maxOperationsPerShard = 128
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
	OperationRetention time.Duration
	HardMaxSourceBytes int64
	ApplicationState   state.Store
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

type userLimit struct {
	semaphore  chan struct{}
	references int
}

type Service struct {
	options    Options
	source     provider.Storage
	store      Store
	generators []Generator
	client     *http.Client
	ids        *domain.IDGenerator
	clock      domain.Clock
	state      state.Store
	global     chan struct{}

	mu       sync.Mutex
	perUser  map[string]*userLimit
	inflight map[string]*generationCall
}

type operationRecord struct {
	SchemaVersion     int       `json:"schemaVersion"`
	OwnerID           string    `json:"ownerID"`
	Fingerprint       string    `json:"fingerprint"`
	IdempotencyDigest string    `json:"idempotencyDigest"`
	LeaseEpoch        uint64    `json:"leaseEpoch"`
	LeaseExpiresAt    time.Time `json:"leaseExpiresAt,omitempty"`
	ExpiresAt         time.Time `json:"expiresAt"`
	Operation         Operation `json:"operation"`
}

type idempotencyRecord struct {
	SchemaVersion int                `json:"schemaVersion"`
	Fingerprint   string             `json:"fingerprint"`
	OperationID   domain.OperationID `json:"operationID"`
	ExpiresAt     time.Time          `json:"expiresAt"`
}

type operationClaim struct {
	record           operationRecord
	operationKey     state.Key
	operationVersion state.Version
	claimed          bool
}

type operationIndexRecord struct {
	SchemaVersion int                   `json:"schemaVersion"`
	Entries       []operationIndexEntry `json:"entries"`
}

type operationIndexEntry struct {
	OperationID       domain.OperationID `json:"operationID"`
	IdempotencyDigest string             `json:"idempotencyDigest"`
	ExpiresAt         time.Time          `json:"expiresAt"`
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
	if options.OperationRetention == 0 {
		options.OperationRetention = 24 * time.Hour
	}
	if options.HardMaxSourceBytes == 0 {
		options.HardMaxSourceBytes = defaultHardMaxSize
	}
	if options.ApplicationState == nil && store == nil {
		options.ApplicationState = state.NewMemoryStore()
	}
	if err := validateOptions(options); err != nil || store != nil && options.ApplicationState == nil {
		if err == nil {
			err = domain.NewError(domain.ErrorInvalid, "preview application state is required")
		}
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
		client: client, ids: ids, clock: clock, state: options.ApplicationState, global: make(chan struct{}, options.MaxConcurrency),
		perUser: make(map[string]*userLimit), inflight: make(map[string]*generationCall),
	}, nil
}

func validateOptions(options Options) error {
	if options.MaxConcurrency < 1 || options.MaxConcurrency > 8 || options.OperationTimeout <= 0 || options.OperationTimeout > 5*time.Minute ||
		options.StartupTimeout <= 0 || options.StartupTimeout > time.Minute || options.OperationRetention < time.Hour || options.OperationRetention > 30*24*time.Hour || options.HardMaxSourceBytes < 1 {
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
	claim, err := s.claimOperation(ctx, owner, request.IdempotencyKey, fingerprint)
	if err != nil {
		return Operation{}, err
	}
	if !claim.claimed {
		operation, replayErr := replayedOperation(claim.record.Operation)
		if replayErr != nil {
			return operation, replayErr
		}
		return s.hydrateOperation(ctx, owner, operation)
	}
	operation := claim.record.Operation
	executionContext, executionCancel := context.WithTimeout(ctx, s.options.OperationTimeout)
	defer executionCancel()
	result, resolveErr := s.resolveItem(executionContext, owner, item, true, request.Regenerate, true)
	operation.UpdatedAt = s.clock.Now().UTC()
	if resolveErr != nil {
		operation.State = domain.OperationFailed
		operation.ErrorKind = domain.KindOf(resolveErr)
	} else if result.State == StateReady {
		operation.State = domain.OperationSucceeded
		operation.Result = &result
	} else if result.State == StateGenerating {
		operation.State = domain.OperationRunning
		operation.Result = &result
	} else {
		operation.State = domain.OperationFailed
		operation.ErrorKind = stateErrorKind(result.State)
		operation.Result = &result
	}
	operation, err = s.finishOperation(ctx, claim, operation)
	if err != nil {
		return Operation{}, err
	}
	if resolveErr != nil && (errors.Is(resolveErr, domain.ErrInvalid) || errors.Is(resolveErr, domain.ErrUnauthorized) || errors.Is(resolveErr, domain.ErrNotFound) || errors.Is(resolveErr, domain.ErrPreconditionFailed)) {
		return operation, resolveErr
	}
	return s.hydrateOperation(ctx, owner, operation)
}

func (s *Service) Operation(ctx context.Context, owner domain.UserID, operationID domain.OperationID) (Operation, error) {
	if !owner.Valid() || operationID == "" {
		return Operation{}, domain.NewError(domain.ErrorInvalid, "invalid preview operation")
	}
	record, _, err := s.readOperation(ctx, owner, operationID)
	if err != nil {
		return Operation{}, err
	}
	if !s.clock.Now().Before(record.ExpiresAt) {
		return Operation{}, domain.NewError(domain.ErrorNotFound, "preview operation expired")
	}
	return s.hydrateOperation(ctx, owner, record.Operation)
}

func (s *Service) claimOperation(ctx context.Context, owner domain.UserID, idempotencyKey, fingerprint string) (operationClaim, error) {
	digestBytes := sha256.Sum256([]byte(owner.String() + "\x00" + idempotencyKey))
	idempotencyDigest := base64.RawURLEncoding.EncodeToString(digestBytes[:])
	idempotencyStateKey := state.MustKey(state.NamespaceIdempotency, "preview", owner.String(), idempotencyDigest)
	if existing, err := s.state.Get(ctx, idempotencyStateKey); err == nil {
		var record idempotencyRecord
		if err := state.DecodeJSON(existing.Data, &record); err != nil || !validIdempotencyRecord(record) {
			return operationClaim{}, domain.NewError(domain.ErrorInvalid, "invalid preview idempotency state")
		}
		if record.Fingerprint != fingerprint {
			return operationClaim{}, domain.NewError(domain.ErrorConflict, "idempotency key was already used for another preview request")
		}
		claim, claimErr := s.claimExistingOperation(ctx, owner, idempotencyStateKey, existing.Version, record)
		if errors.Is(claimErr, domain.ErrNotFound) {
			_ = s.state.Delete(ctx, idempotencyStateKey, existing.Version)
			return s.claimOperation(ctx, owner, idempotencyKey, fingerprint)
		}
		return claim, claimErr
	} else if !errors.Is(err, domain.ErrNotFound) {
		return operationClaim{}, err
	}

	operationIDValue, err := s.ids.OpaqueID()
	if err != nil {
		return operationClaim{}, err
	}
	now := s.clock.Now().UTC()
	operationID := domain.OperationID(operationIDValue)
	operationStateKey := previewOperationKey(owner, operationID)
	operationRecordValue := operationRecord{
		SchemaVersion: 1, OwnerID: owner.String(), Fingerprint: fingerprint, IdempotencyDigest: idempotencyDigest,
		LeaseEpoch: 1, LeaseExpiresAt: now.Add(s.options.OperationTimeout), ExpiresAt: now.Add(s.options.OperationRetention),
		Operation: Operation{ID: operationID, State: domain.OperationRunning, StartedAt: now, UpdatedAt: now},
	}
	operationBody, err := state.EncodeJSON(operationRecordValue)
	if err != nil {
		return operationClaim{}, err
	}
	if err := s.registerOperation(ctx, owner, operationIndexEntry{OperationID: operationID, IdempotencyDigest: idempotencyDigest, ExpiresAt: operationRecordValue.ExpiresAt}); err != nil {
		return operationClaim{}, err
	}
	operationVersion, err := s.state.Create(ctx, operationStateKey, operationBody)
	if err != nil {
		return operationClaim{}, err
	}
	idempotencyValue := idempotencyRecord{SchemaVersion: 1, Fingerprint: fingerprint, OperationID: operationID, ExpiresAt: operationRecordValue.ExpiresAt}
	idempotencyBody, err := state.EncodeJSON(idempotencyValue)
	if err != nil {
		_ = s.state.Delete(ctx, operationStateKey, operationVersion)
		return operationClaim{}, err
	}
	if _, err = s.state.Create(ctx, idempotencyStateKey, idempotencyBody); err != nil {
		_ = s.state.Delete(ctx, operationStateKey, operationVersion)
		if errors.Is(err, domain.ErrConflict) {
			return s.claimOperation(ctx, owner, idempotencyKey, fingerprint)
		}
		return operationClaim{}, err
	}
	return operationClaim{record: operationRecordValue, operationKey: operationStateKey, operationVersion: operationVersion, claimed: true}, nil
}

func (s *Service) claimExistingOperation(ctx context.Context, owner domain.UserID, idempotencyKey state.Key, idempotencyVersion state.Version, idempotency idempotencyRecord) (operationClaim, error) {
	now := s.clock.Now().UTC()
	if !now.Before(idempotency.ExpiresAt) {
		_ = s.state.Delete(ctx, idempotencyKey, idempotencyVersion)
		return operationClaim{}, domain.NewError(domain.ErrorNotFound, "preview idempotency record expired")
	}
	record, version, err := s.readOperation(ctx, owner, idempotency.OperationID)
	if err != nil {
		return operationClaim{}, err
	}
	claim := operationClaim{record: record, operationKey: previewOperationKey(owner, idempotency.OperationID), operationVersion: version}
	if record.Operation.State != domain.OperationRunning || now.Before(record.LeaseExpiresAt) {
		return claim, nil
	}
	record.LeaseEpoch++
	record.LeaseExpiresAt = now.Add(s.options.OperationTimeout)
	record.Operation.UpdatedAt = now
	body, err := state.EncodeJSON(record)
	if err != nil {
		return operationClaim{}, err
	}
	version, err = s.state.CompareAndSwap(ctx, claim.operationKey, version, body)
	if errors.Is(err, domain.ErrPreconditionFailed) {
		record, version, err = s.readOperation(ctx, owner, idempotency.OperationID)
		if err != nil {
			return operationClaim{}, err
		}
		claim.record, claim.operationVersion = record, version
		return claim, nil
	}
	if err != nil {
		return operationClaim{}, err
	}
	claim.record, claim.operationVersion, claim.claimed = record, version, true
	return claim, nil
}

func (s *Service) finishOperation(ctx context.Context, claim operationClaim, operation Operation) (Operation, error) {
	persisted := operation
	if operation.Result != nil {
		result := *operation.Result
		result.Capability = nil
		persisted.Result = &result
	}
	claim.record.Operation = persisted
	if operation.State != domain.OperationRunning {
		claim.record.LeaseExpiresAt = time.Time{}
	}
	body, err := state.EncodeJSON(claim.record)
	if err != nil {
		return Operation{}, err
	}
	if _, err := s.state.CompareAndSwap(ctx, claim.operationKey, claim.operationVersion, body); errors.Is(err, domain.ErrPreconditionFailed) {
		owner, parseErr := domain.ParseUserID(claim.record.OwnerID)
		if parseErr != nil {
			return Operation{}, domain.NewError(domain.ErrorInvalid, "invalid preview operation state")
		}
		record, _, readErr := s.readOperation(ctx, owner, operation.ID)
		if readErr != nil {
			return Operation{}, readErr
		}
		return record.Operation, nil
	} else if err != nil {
		return Operation{}, err
	}
	return operation, nil
}

func (s *Service) hydrateOperation(ctx context.Context, owner domain.UserID, operation Operation) (Operation, error) {
	if operation.State != domain.OperationSucceeded || operation.Result == nil || operation.Result.State != StateReady || operation.Result.Artifact == nil || operation.Result.Capability != nil {
		return operation, nil
	}
	result := *operation.Result
	item := ItemRequest{Path: result.Path, Version: result.Version, Variant: result.Variant}
	if err := s.validateItem(item); err != nil {
		return Operation{}, domain.NewError(domain.ErrorInvalid, "invalid preview operation result")
	}
	scope, err := domain.NewScope(owner, domain.AreaLive)
	if err != nil {
		return Operation{}, err
	}
	entry, err := s.source.Stat(ctx, scope, item.Path)
	if err != nil {
		return Operation{}, err
	}
	if entry.Kind != domain.EntryFile || entry.Version != item.Version {
		return Operation{}, domain.NewError(domain.ErrorPreconditionFailed, "preview source version does not match")
	}
	generator := s.generatorFor(entry.MediaType)
	if generator == nil {
		return Operation{}, domain.NewError(domain.ErrorPreconditionFailed, "preview source format is no longer supported")
	}
	binding := Binding{
		Owner: owner, ContentID: entry.ContentID, ContentVersion: entry.ContentVersion, MediaType: entry.MediaType,
		SourceSize: entry.Size, RecipeID: generator.RecipeID(), Variant: item.Variant,
	}
	if !result.Artifact.ValidFor(binding) {
		return Operation{}, domain.NewError(domain.ErrorInvalid, "invalid preview operation artifact metadata")
	}
	capability, err := s.store.CreateDownload(ctx, binding, result.Artifact.GenerationID)
	if err != nil {
		return Operation{}, err
	}
	result.Capability = &capability
	operation.Result = &result
	return operation, nil
}

func (s *Service) readOperation(ctx context.Context, owner domain.UserID, operationID domain.OperationID) (operationRecord, state.Version, error) {
	value, err := s.state.Get(ctx, previewOperationKey(owner, operationID))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return operationRecord{}, "", domain.NewError(domain.ErrorNotFound, "preview operation not found")
		}
		return operationRecord{}, "", err
	}
	var record operationRecord
	if err := state.DecodeJSON(value.Data, &record); err != nil || !validOperationRecord(record, owner, operationID) {
		return operationRecord{}, "", domain.NewError(domain.ErrorInvalid, "invalid preview operation state")
	}
	return record, value.Version, nil
}

func (s *Service) registerOperation(ctx context.Context, owner domain.UserID, entry operationIndexEntry) error {
	digest := sha256.Sum256([]byte(entry.OperationID))
	shard := strconv.Itoa(int(digest[0]) % operationIndexShards)
	indexKey := state.MustKey(state.NamespaceOperations, "preview-index", owner.String(), shard)
	for attempt := 0; attempt < 16; attempt++ {
		value, err := s.state.Get(ctx, indexKey)
		record := operationIndexRecord{SchemaVersion: 1, Entries: make([]operationIndexEntry, 0, maxOperationsPerShard)}
		if err == nil {
			var decoded operationIndexRecord
			if decodeErr := state.DecodeJSON(value.Data, &decoded); decodeErr != nil || !validOperationIndex(decoded) {
				return domain.NewError(domain.ErrorInvalid, "invalid preview operation index")
			}
			record = decoded
		} else if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		now := s.clock.Now().UTC()
		expired := make([]operationIndexEntry, 0)
		retained := record.Entries[:0]
		for _, current := range record.Entries {
			if now.Before(current.ExpiresAt) {
				if current.OperationID == entry.OperationID {
					return domain.NewError(domain.ErrorInternal, "preview operation identity collision")
				}
				retained = append(retained, current)
			} else {
				expired = append(expired, current)
			}
		}
		record.Entries = retained
		if len(record.Entries) >= maxOperationsPerShard {
			return domain.NewError(domain.ErrorUnavailable, "preview operation retention capacity reached")
		}
		record.Entries = append(record.Entries, entry)
		body, encodeErr := state.EncodeJSON(record)
		if encodeErr != nil {
			return encodeErr
		}
		if errors.Is(err, domain.ErrNotFound) {
			_, err = s.state.Create(ctx, indexKey, body)
		} else {
			_, err = s.state.CompareAndSwap(ctx, indexKey, value.Version, body)
		}
		if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPreconditionFailed) {
			continue
		}
		if err != nil {
			return err
		}
		for _, expiredEntry := range expired {
			s.deleteIndexedOperation(ctx, owner, expiredEntry)
		}
		return nil
	}
	return domain.NewError(domain.ErrorUnavailable, "preview operation index contention")
}

func (s *Service) deleteIndexedOperation(ctx context.Context, owner domain.UserID, entry operationIndexEntry) {
	operationKey := previewOperationKey(owner, entry.OperationID)
	if value, err := s.state.Get(ctx, operationKey); err == nil {
		_ = s.state.Delete(ctx, operationKey, value.Version)
	}
	idempotencyKey := state.MustKey(state.NamespaceIdempotency, "preview", owner.String(), entry.IdempotencyDigest)
	if value, err := s.state.Get(ctx, idempotencyKey); err == nil {
		var record idempotencyRecord
		if state.DecodeJSON(value.Data, &record) == nil && record.OperationID == entry.OperationID {
			_ = s.state.Delete(ctx, idempotencyKey, value.Version)
		}
	}
}

func previewOperationKey(owner domain.UserID, operationID domain.OperationID) state.Key {
	return state.MustKey(state.NamespaceOperations, "preview", owner.String(), string(operationID))
}

func validIdempotencyRecord(record idempotencyRecord) bool {
	return record.SchemaVersion == 1 && record.Fingerprint != "" && record.OperationID != "" && !record.ExpiresAt.IsZero()
}

func validOperationRecord(record operationRecord, owner domain.UserID, operationID domain.OperationID) bool {
	capabilitySafe := record.Operation.Result == nil || record.Operation.Result.Capability == nil
	return capabilitySafe && record.SchemaVersion == 1 && record.OwnerID == owner.String() && record.Fingerprint != "" && record.IdempotencyDigest != "" && record.LeaseEpoch > 0 &&
		!record.ExpiresAt.IsZero() && record.Operation.ID == operationID && !record.Operation.StartedAt.IsZero() && !record.Operation.UpdatedAt.IsZero()
}

func validOperationIndex(record operationIndexRecord) bool {
	if record.SchemaVersion != 1 || record.Entries == nil || len(record.Entries) > maxOperationsPerShard {
		return false
	}
	seen := make(map[domain.OperationID]struct{}, len(record.Entries))
	for _, entry := range record.Entries {
		if entry.OperationID == "" || entry.IdempotencyDigest == "" || entry.ExpiresAt.IsZero() {
			return false
		}
		if _, exists := seen[entry.OperationID]; exists {
			return false
		}
		seen[entry.OperationID] = struct{}{}
	}
	return true
}

func replayedOperation(operation Operation) (Operation, error) {
	if operation.State == domain.OperationFailed && operation.Result == nil {
		return operation, domain.NewError(operation.ErrorKind, "preview generation request failed")
	}
	return operation, nil
}

func (s *Service) Ready() bool { return s.store == nil || s.store.Ready() }

func (s *Service) Revalidate(ctx context.Context) bool {
	if s.store == nil {
		return true
	}
	checkContext, cancel := context.WithTimeout(ctx, s.options.StartupTimeout)
	defer cancel()
	if s.store.Ready() {
		return s.store.Check(checkContext) == nil
	}
	return s.store.Validate(checkContext) == nil
}

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
		if errors.Is(err, domain.ErrConflict) {
			result.State = StateGenerating
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
	capability, err := s.store.CreateDownload(ctx, binding, artifact.GenerationID)
	if errors.Is(err, domain.ErrUnavailable) {
		result.State = StateUnavailable
		return result, true, nil
	}
	if err != nil {
		return ItemResult{}, false, err
	}
	result.State, result.Artifact, result.Capability = StateReady, &artifact, &capability
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
	generationID, err := s.ids.OpaqueID()
	if err != nil {
		return err
	}
	claim, err := s.store.Claim(operationContext, binding, generationID, s.clock.Now().UTC().Add(s.options.OperationTimeout))
	if errors.Is(err, domain.ErrConflict) {
		if !force {
			if _, latestErr := s.store.Latest(operationContext, binding); latestErr == nil {
				return nil
			}
		}
		return err
	}
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			releaseContext, releaseCancel := context.WithTimeout(context.Background(), s.options.StartupTimeout)
			defer releaseCancel()
			_ = s.store.Release(releaseContext, binding, claim)
		}
	}()
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
		if operationContext.Err() != nil {
			return domain.WrapError(domain.ErrorUnavailable, "preview generation timed out", operationContext.Err())
		}
		return domain.NewError(domain.ErrorInvalid, "preview generator rejected source")
	}
	sum := sha256.Sum256(generated.Bytes)
	artifact := Artifact{
		GenerationID: generationID, Variant: binding.Variant, Width: generated.Width, Height: generated.Height,
		ContentType: ContentTypeWebP, Size: int64(len(generated.Bytes)), SHA256: base64.RawURLEncoding.EncodeToString(sum[:]), Bytes: generated.Bytes,
	}
	if !artifact.ValidFor(binding) {
		return domain.NewError(domain.ErrorInvalid, "preview generator produced invalid artifact")
	}
	if err := s.store.Commit(operationContext, binding, claim, artifact); err != nil {
		return err
	}
	committed = true
	return nil
}

func (s *Service) acquire(ctx context.Context, owner domain.UserID) (func(), error) {
	select {
	case s.global <- struct{}{}:
	case <-ctx.Done():
		return nil, domain.WrapError(domain.ErrorUnavailable, "preview concurrency wait canceled", ctx.Err())
	}
	s.mu.Lock()
	limit := s.perUser[owner.String()]
	if limit == nil {
		limit = &userLimit{semaphore: make(chan struct{}, 1)}
		s.perUser[owner.String()] = limit
	}
	limit.references++
	s.mu.Unlock()
	select {
	case limit.semaphore <- struct{}{}:
		return func() {
			<-limit.semaphore
			<-s.global
			s.releaseUserLimit(owner, limit)
		}, nil
	case <-ctx.Done():
		<-s.global
		s.releaseUserLimit(owner, limit)
		return nil, domain.WrapError(domain.ErrorUnavailable, "preview concurrency wait canceled", ctx.Err())
	}
}

func (s *Service) releaseUserLimit(owner domain.UserID, limit *userLimit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit.references--
	if limit.references == 0 && s.perUser[owner.String()] == limit {
		delete(s.perUser, owner.String())
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
