//endlessfs:file-body-read-exemption feature=image-preview-generation

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
	maxAutomaticPerBatch  = 1
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
	ApplicationState   state.AtomicStore
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
	waitingFor string
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
	done       chan struct{}
	waitingFor string
	err        error
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
	state      state.AtomicStore
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
	WaitGenerationID  string    `json:"waitGenerationID,omitempty"`
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
			message := "preview store access validation failed"
			if category := StoreValidationCategory(err); category != "" {
				message += ": " + category
			}
			return nil, domain.WrapError(domain.ErrorUnavailable, message, err)
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
	for _, item := range request.Items {
		if err := s.validateItem(item); err != nil {
			return ResolveResponse{}, err
		}
	}
	response := ResolveResponse{Items: make([]ItemResult, 0, len(request.Items))}
	if s.store == nil {
		for _, item := range request.Items {
			response.Items = append(response.Items, ItemResult{Path: item.Path, Version: item.Version, Variant: item.Variant, State: StateDisabled})
		}
		return response, nil
	}
	entries, scope, err := s.resolveSourceEntries(ctx, owner, request.Items)
	if err != nil {
		return ResolveResponse{}, err
	}
	known := make([]*ArtifactMetadata, len(request.Items))
	selections := make([]ReadySelection, 0, len(request.Items))
	selectionIndices := make([]int, 0, len(request.Items))
	for index, item := range request.Items {
		entry := entries[index]
		generator := s.generatorFor(entry.MediaType)
		if entry.Kind != domain.EntryFile || entry.Version != item.Version || generator == nil {
			continue
		}
		binding := previewBinding(scope.UserID(), entry, item.Variant, generator.RecipeID())
		if !binding.Valid() {
			return ResolveResponse{}, domain.NewError(domain.ErrorInternal, "source provider omitted preview content identity")
		}
		selections = append(selections, readySelection(scope.UserID(), item.Path, binding))
		selectionIndices = append(selectionIndices, index)
	}
	if len(selections) > 0 {
		resolved, resolveErr := s.store.ResolveReady(ctx, selections)
		if resolveErr != nil {
			if !errors.Is(resolveErr, domain.ErrUnavailable) {
				return ResolveResponse{}, resolveErr
			}
			for _, item := range request.Items {
				response.Items = append(response.Items, ItemResult{Path: item.Path, Version: item.Version, Variant: item.Variant, State: StateUnavailable})
			}
			return response, nil
		}
		if len(resolved) != len(selections) {
			return ResolveResponse{}, domain.NewError(domain.ErrorInternal, "preview ready store returned an invalid result")
		}
		for index, metadata := range resolved {
			known[selectionIndices[index]] = metadata
		}
	}
	automaticRemaining := maxAutomaticPerBatch
	for index, item := range request.Items {
		result, err := s.resolveItemWithEntry(ctx, scope, item, entries[index], known[index], true, false, false, false)
		if err != nil {
			return ResolveResponse{}, err
		}
		if result.State == StateMissing && s.options.Automatic && automaticRemaining > 0 {
			// One bounded fallback per resolve preserves ready artifacts created
			// before this disposable catalog or reused through a directory move.
			result, err = s.resolveItemWithEntry(ctx, scope, item, entries[index], nil, false, true, false, false)
			if err != nil {
				return ResolveResponse{}, err
			}
			if result.State != StateIneligible {
				automaticRemaining--
			}
		}
		response.Items = append(response.Items, result)
	}
	return response, nil
}

// resolveSourceEntries authorizes a resolve batch from one snapshot lookup per
// distinct parent directory. The public resolve limit is smaller than the
// provider's 1,000-name lookup bound, so a visible grid never performs one
// namespace Stat (and therefore one provider read sequence) per tile.
func (s *Service) resolveSourceEntries(ctx context.Context, owner domain.UserID, items []ItemRequest) ([]domain.Entry, domain.Scope, error) {
	scope, err := domain.NewScope(owner, domain.AreaLive)
	if err != nil {
		return nil, domain.Scope{}, err
	}
	type parentLookup struct {
		path  domain.UserPath
		names []string
		seen  map[string]struct{}
	}
	parents := make(map[string]*parentLookup)
	parentOrder := make([]string, 0)
	for _, item := range items {
		parent := item.Path.Parent()
		key := parent.String()
		lookup := parents[key]
		if lookup == nil {
			lookup = &parentLookup{path: parent, seen: make(map[string]struct{})}
			parents[key] = lookup
			parentOrder = append(parentOrder, key)
		}
		if _, duplicate := lookup.seen[item.Path.Name()]; !duplicate {
			lookup.seen[item.Path.Name()] = struct{}{}
			lookup.names = append(lookup.names, item.Path.Name())
		}
	}
	resolved := make(map[string]domain.Entry, len(items))
	for _, key := range parentOrder {
		lookup := parents[key]
		children, lookupErr := s.source.LookupChildren(ctx, scope, domain.ChildLookupRequest{Directory: lookup.path, Names: lookup.names})
		if lookupErr != nil {
			return nil, domain.Scope{}, lookupErr
		}
		if len(children.Entries) != len(lookup.names) {
			return nil, domain.Scope{}, domain.NewError(domain.ErrorInternal, "preview source lookup returned an invalid result")
		}
		for index, name := range lookup.names {
			resolved[key+"\x00"+name] = children.Entries[index]
		}
	}
	entries := make([]domain.Entry, len(items))
	for index, item := range items {
		entry, found := resolved[item.Path.Parent().String()+"\x00"+item.Path.Name()]
		if !found {
			return nil, domain.Scope{}, domain.NewError(domain.ErrorInternal, "preview source lookup omitted an item")
		}
		entries[index] = entry
	}
	return entries, scope, nil
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
		return s.operationResponse(ctx, owner, claim.record, operation)
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
		claim.record.WaitGenerationID = result.waitingFor
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
	return s.operationResponse(ctx, owner, claim.record, operation)
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
	return s.operationResponse(ctx, owner, record, record.Operation)
}

func (s *Service) claimOperation(ctx context.Context, owner domain.UserID, idempotencyKey, fingerprint string) (operationClaim, error) {
	digestBytes := sha256.Sum256([]byte(owner.String() + "\x00" + idempotencyKey))
	idempotencyDigest := base64.RawURLEncoding.EncodeToString(digestBytes[:])
	idempotencyStateKey := state.MustKey(state.NamespaceIdempotency, "preview", owner.String(), idempotencyDigest)
	operationIDValue, err := s.ids.OpaqueID()
	if err != nil {
		return operationClaim{}, err
	}
	operationID := domain.OperationID(operationIDValue)
	for attempt := 0; attempt < 16; attempt++ {
		if existing, getErr := s.state.Get(ctx, idempotencyStateKey); getErr == nil {
			var record idempotencyRecord
			if decodeErr := state.DecodeJSON(existing.Data, &record); decodeErr != nil || !validIdempotencyRecord(record) {
				return operationClaim{}, domain.NewError(domain.ErrorInvalid, "invalid preview idempotency state")
			}
			if record.Fingerprint != fingerprint {
				return operationClaim{}, domain.NewError(domain.ErrorConflict, "idempotency key was already used for another preview request")
			}
			claim, claimErr := s.claimExistingOperation(ctx, owner, idempotencyStateKey, existing.Version, idempotencyDigest, record)
			if errors.Is(claimErr, domain.ErrNotFound) {
				continue
			}
			return claim, claimErr
		} else if !errors.Is(getErr, domain.ErrNotFound) {
			return operationClaim{}, getErr
		}

		now := s.clock.Now().UTC()
		operationStateKey := previewOperationKey(owner, operationID)
		operationRecordValue := operationRecord{
			SchemaVersion: 1, OwnerID: owner.String(), Fingerprint: fingerprint, IdempotencyDigest: idempotencyDigest,
			LeaseEpoch: 1, LeaseExpiresAt: now.Add(s.options.OperationTimeout), ExpiresAt: now.Add(s.options.OperationRetention),
			Operation: Operation{ID: operationID, State: domain.OperationRunning, StartedAt: now, UpdatedAt: now},
		}
		operationBody, encodeErr := state.EncodeJSON(operationRecordValue)
		if encodeErr != nil {
			return operationClaim{}, encodeErr
		}
		idempotencyBody, encodeErr := state.EncodeJSON(idempotencyRecord{SchemaVersion: 1, Fingerprint: fingerprint, OperationID: operationID, ExpiresAt: operationRecordValue.ExpiresAt})
		if encodeErr != nil {
			return operationClaim{}, encodeErr
		}
		index, loadErr := s.loadOperationIndex(ctx, owner, operationID)
		if loadErr != nil {
			return operationClaim{}, loadErr
		}
		if index.hasExpired(now) {
			if cleanupErr := s.cleanupExpiredOperationIndex(ctx, owner, index, now); cleanupErr != nil && !errors.Is(cleanupErr, domain.ErrPreconditionFailed) {
				return operationClaim{}, cleanupErr
			}
			continue
		}
		if len(index.record.Entries) >= maxOperationsPerShard {
			return operationClaim{}, domain.NewError(domain.ErrorUnavailable, "preview operation retention capacity reached")
		}
		index.record.Entries = append(index.record.Entries, operationIndexEntry{OperationID: operationID, IdempotencyDigest: idempotencyDigest, ExpiresAt: operationRecordValue.ExpiresAt})
		indexBody, encodeErr := state.EncodeJSON(index.record)
		if encodeErr != nil {
			return operationClaim{}, encodeErr
		}
		indexRequirement := state.RequirementAbsent
		if index.exists {
			indexRequirement = state.RequirementPresent
		}
		outcome, mutateErr := s.state.Mutate(ctx, state.Mutation{
			ID:          "preview-claim-" + string(operationID) + "-" + string(index.version),
			RetainUntil: operationRecordValue.ExpiresAt,
			Changes: []state.Change{
				{Key: operationStateKey, Requirement: state.RequirementAbsent, Data: operationBody},
				{Key: idempotencyStateKey, Requirement: state.RequirementAbsent, Data: idempotencyBody},
				{Key: index.key, Requirement: indexRequirement, ExpectedVersion: index.version, Data: indexBody},
			},
		})
		if errors.Is(mutateErr, domain.ErrConflict) || errors.Is(mutateErr, domain.ErrPreconditionFailed) {
			continue
		}
		if mutateErr != nil {
			return operationClaim{}, mutateErr
		}
		operationVersion := mutationVersion(outcome, operationStateKey)
		if operationVersion == "" {
			return operationClaim{}, domain.NewError(domain.ErrorInternal, "atomic preview claim omitted operation version")
		}
		return operationClaim{record: operationRecordValue, operationKey: operationStateKey, operationVersion: operationVersion, claimed: true}, nil
	}
	return operationClaim{}, domain.NewError(domain.ErrorUnavailable, "preview operation claim contention")
}

func (s *Service) claimExistingOperation(ctx context.Context, owner domain.UserID, idempotencyKey state.Key, idempotencyVersion state.Version, idempotencyDigest string, idempotency idempotencyRecord) (operationClaim, error) {
	now := s.clock.Now().UTC()
	if !now.Before(idempotency.ExpiresAt) {
		if err := s.cleanupExpiredPreviewBinding(ctx, owner, idempotencyKey, idempotencyVersion, idempotency); err != nil && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return operationClaim{}, err
		}
		return operationClaim{}, domain.NewError(domain.ErrorNotFound, "preview idempotency record expired")
	}
	record, version, err := s.readOperation(ctx, owner, idempotency.OperationID)
	if err != nil {
		return operationClaim{}, err
	}
	if record.Fingerprint != idempotency.Fingerprint || record.IdempotencyDigest != idempotencyDigest || !record.ExpiresAt.Equal(idempotency.ExpiresAt) {
		return operationClaim{}, domain.NewError(domain.ErrorInvalid, "preview idempotency state does not match its operation")
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
		claim.record.WaitGenerationID = ""
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

func (s *Service) operationResponse(ctx context.Context, owner domain.UserID, record operationRecord, operation Operation) (Operation, error) {
	if operation.State == domain.OperationRunning && operation.Result != nil && operation.Result.State == StateGenerating && record.WaitGenerationID != "" {
		result, ready, err := s.resultForExactGeneration(ctx, owner, *operation.Result, record.WaitGenerationID)
		if err != nil {
			return Operation{}, err
		}
		if ready {
			operation.State = domain.OperationSucceeded
			operation.ErrorKind = ""
			operation.UpdatedAt = s.clock.Now().UTC()
			operation.Result = &result
			return operation, nil
		}
	}
	return s.hydrateOperation(ctx, owner, operation)
}

func (s *Service) resultForExactGeneration(ctx context.Context, owner domain.UserID, result ItemResult, generationID string) (ItemResult, bool, error) {
	item := ItemRequest{Path: result.Path, Version: result.Version, Variant: result.Variant}
	binding, err := s.bindingForItem(ctx, owner, item)
	if err != nil {
		return ItemResult{}, false, err
	}
	artifact, err := s.store.Read(ctx, binding, generationID)
	if errors.Is(err, domain.ErrNotFound) {
		return result, false, nil
	}
	if err != nil {
		return ItemResult{}, false, err
	}
	if !artifact.ValidFor(binding) {
		return ItemResult{}, false, domain.NewError(domain.ErrorInvalid, "invalid preview artifact")
	}
	capability, err := s.store.CreateDownload(ctx, binding, generationID)
	if err != nil {
		return ItemResult{}, false, err
	}
	metadata := artifact.Metadata()
	result.State, result.Artifact, result.Capability = StateReady, &metadata, &capability
	return result, true, nil
}

func (s *Service) bindingForItem(ctx context.Context, owner domain.UserID, item ItemRequest) (Binding, error) {
	if err := s.validateItem(item); err != nil {
		return Binding{}, err
	}
	scope, err := domain.NewScope(owner, domain.AreaLive)
	if err != nil {
		return Binding{}, err
	}
	entry, err := s.source.Stat(ctx, scope, item.Path)
	if err != nil {
		return Binding{}, err
	}
	if entry.Kind != domain.EntryFile || entry.Version != item.Version {
		return Binding{}, domain.NewError(domain.ErrorPreconditionFailed, "preview source version does not match")
	}
	generator := s.generatorFor(entry.MediaType)
	if generator == nil {
		return Binding{}, domain.NewError(domain.ErrorPreconditionFailed, "preview source format is no longer supported")
	}
	binding := Binding{
		Owner: scope.UserID(), ContentID: entry.ContentID, ContentVersion: entry.ContentVersion, MediaType: entry.MediaType,
		SourceSize: entry.Size, RecipeID: generator.RecipeID(), Variant: item.Variant,
	}
	if !binding.Valid() {
		return Binding{}, domain.NewError(domain.ErrorInvalid, "invalid preview source binding")
	}
	return binding, nil
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

type operationIndexSnapshot struct {
	key     state.Key
	record  operationIndexRecord
	version state.Version
	exists  bool
}

func (snapshot operationIndexSnapshot) hasExpired(now time.Time) bool {
	for _, entry := range snapshot.record.Entries {
		if !now.Before(entry.ExpiresAt) {
			return true
		}
	}
	return false
}

func (s *Service) loadOperationIndex(ctx context.Context, owner domain.UserID, operationID domain.OperationID) (operationIndexSnapshot, error) {
	digest := sha256.Sum256([]byte(operationID))
	shard := strconv.Itoa(int(digest[0]) % operationIndexShards)
	indexKey := state.MustKey(state.NamespaceOperations, "preview-index", owner.String(), shard)
	value, err := s.state.Get(ctx, indexKey)
	if errors.Is(err, domain.ErrNotFound) {
		return operationIndexSnapshot{key: indexKey, record: operationIndexRecord{SchemaVersion: 1, Entries: []operationIndexEntry{}}}, nil
	}
	if err != nil {
		return operationIndexSnapshot{}, err
	}
	var record operationIndexRecord
	if decodeErr := state.DecodeJSON(value.Data, &record); decodeErr != nil || !validOperationIndex(record) {
		return operationIndexSnapshot{}, domain.NewError(domain.ErrorInvalid, "invalid preview operation index")
	}
	return operationIndexSnapshot{key: indexKey, record: record, version: value.Version, exists: true}, nil
}

func mutationVersion(outcome state.MutationOutcome, key state.Key) state.Version {
	for _, change := range outcome.Changes {
		if change.Key.String() == key.String() {
			return change.Version
		}
	}
	return ""
}

func previewMutationID(label string, values ...string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("endlessfs-preview-state-mutation-v1\x00" + label))
	for _, value := range values {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(value))
	}
	return label + "-" + base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
}

// cleanupExpiredOperationIndex removes one complete operation binding per
// call. The index, operation, and idempotency record disappear at the same
// owner-jobs linearization point; a malformed or partial binding fails closed.
func (s *Service) cleanupExpiredOperationIndex(ctx context.Context, owner domain.UserID, index operationIndexSnapshot, now time.Time) error {
	for _, entry := range index.record.Entries {
		if now.Before(entry.ExpiresAt) {
			continue
		}
		idempotencyKey := state.MustKey(state.NamespaceIdempotency, "preview", owner.String(), entry.IdempotencyDigest)
		value, err := s.state.Get(ctx, idempotencyKey)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return domain.NewError(domain.ErrorInvalid, "preview operation index has no idempotency binding")
			}
			return err
		}
		var record idempotencyRecord
		if state.DecodeJSON(value.Data, &record) != nil || !validIdempotencyRecord(record) || record.OperationID != entry.OperationID || !record.ExpiresAt.Equal(entry.ExpiresAt) {
			return domain.NewError(domain.ErrorInvalid, "preview operation index binding is invalid")
		}
		return s.cleanupExpiredPreviewBinding(ctx, owner, idempotencyKey, value.Version, record)
	}
	return nil
}

func (s *Service) cleanupExpiredPreviewBinding(ctx context.Context, owner domain.UserID, idempotencyKey state.Key, idempotencyVersion state.Version, idempotency idempotencyRecord) error {
	operationKey := previewOperationKey(owner, idempotency.OperationID)
	operationValue, err := s.state.Get(ctx, operationKey)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.NewError(domain.ErrorInvalid, "preview idempotency binding has no operation")
		}
		return err
	}
	var operation operationRecord
	if state.DecodeJSON(operationValue.Data, &operation) != nil || !validOperationRecord(operation, owner, idempotency.OperationID) || operation.Fingerprint != idempotency.Fingerprint || operation.IdempotencyDigest == "" || !operation.ExpiresAt.Equal(idempotency.ExpiresAt) {
		return domain.NewError(domain.ErrorInvalid, "preview idempotency operation binding is invalid")
	}
	expectedIdempotencyKey := state.MustKey(state.NamespaceIdempotency, "preview", owner.String(), operation.IdempotencyDigest)
	if expectedIdempotencyKey.String() != idempotencyKey.String() {
		return domain.NewError(domain.ErrorInvalid, "preview idempotency key binding is invalid")
	}
	index, err := s.loadOperationIndex(ctx, owner, idempotency.OperationID)
	if err != nil {
		return err
	}
	if !index.exists {
		return domain.NewError(domain.ErrorInvalid, "preview operation has no index binding")
	}
	found := false
	retained := make([]operationIndexEntry, 0, len(index.record.Entries)-1)
	for _, entry := range index.record.Entries {
		if entry.OperationID == idempotency.OperationID {
			if found || entry.IdempotencyDigest != operation.IdempotencyDigest || !entry.ExpiresAt.Equal(idempotency.ExpiresAt) {
				return domain.NewError(domain.ErrorInvalid, "preview operation index binding is invalid")
			}
			found = true
			continue
		}
		retained = append(retained, entry)
	}
	if !found {
		return domain.NewError(domain.ErrorInvalid, "preview operation has no index entry")
	}
	index.record.Entries = retained
	indexBody, err := state.EncodeJSON(index.record)
	if err != nil {
		return err
	}
	_, err = s.state.Mutate(ctx, state.Mutation{
		ID: previewMutationID("preview-cleanup", owner.String(), string(idempotency.OperationID), string(idempotencyVersion), string(operationValue.Version), string(index.version)),
		Changes: []state.Change{
			{Key: idempotencyKey, Requirement: state.RequirementPresent, ExpectedVersion: idempotencyVersion, Delete: true},
			{Key: operationKey, Requirement: state.RequirementPresent, ExpectedVersion: operationValue.Version, Delete: true},
			{Key: index.key, Requirement: state.RequirementPresent, ExpectedVersion: index.version, Data: indexBody},
		},
	})
	return err
}

func (s *Service) registerOperation(ctx context.Context, owner domain.UserID, entry operationIndexEntry) error {
	for attempt := 0; attempt < 16; attempt++ {
		index, err := s.loadOperationIndex(ctx, owner, entry.OperationID)
		if err != nil {
			return err
		}
		now := s.clock.Now().UTC()
		if index.hasExpired(now) {
			if err := s.cleanupExpiredOperationIndex(ctx, owner, index, now); err != nil && !errors.Is(err, domain.ErrPreconditionFailed) {
				return err
			}
			continue
		}
		for _, current := range index.record.Entries {
			if current.OperationID == entry.OperationID {
				return domain.NewError(domain.ErrorInternal, "preview operation identity collision")
			}
		}
		if len(index.record.Entries) >= maxOperationsPerShard {
			return domain.NewError(domain.ErrorUnavailable, "preview operation retention capacity reached")
		}
		index.record.Entries = append(index.record.Entries, entry)
		body, encodeErr := state.EncodeJSON(index.record)
		if encodeErr != nil {
			return encodeErr
		}
		requirement := state.RequirementAbsent
		if index.exists {
			requirement = state.RequirementPresent
		}
		_, err = s.state.Mutate(ctx, state.Mutation{ID: "preview-index-" + string(entry.OperationID) + "-" + string(index.version), RetainUntil: entry.ExpiresAt, Changes: []state.Change{{Key: index.key, Requirement: requirement, ExpectedVersion: index.version, Data: body}}})
		if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPreconditionFailed) {
			continue
		}
		if err != nil {
			return err
		}
		return nil
	}
	return domain.NewError(domain.ErrorUnavailable, "preview operation index contention")
}

func previewOperationKey(owner domain.UserID, operationID domain.OperationID) state.Key {
	return state.MustKey(state.NamespaceOperations, "preview", owner.String(), string(operationID))
}

func validIdempotencyRecord(record idempotencyRecord) bool {
	return record.SchemaVersion == 1 && record.Fingerprint != "" && record.OperationID != "" && !record.ExpiresAt.IsZero()
}

func validOperationRecord(record operationRecord, owner domain.UserID, operationID domain.OperationID) bool {
	capabilitySafe := record.Operation.Result == nil || record.Operation.Result.Capability == nil
	if !capabilitySafe || record.SchemaVersion != 1 || record.OwnerID != owner.String() || record.Fingerprint == "" || record.IdempotencyDigest == "" || record.LeaseEpoch == 0 ||
		record.ExpiresAt.IsZero() || record.Operation.ID != operationID || record.Operation.StartedAt.IsZero() || record.Operation.UpdatedAt.IsZero() ||
		record.Operation.UpdatedAt.Before(record.Operation.StartedAt) || !record.Operation.UpdatedAt.Before(record.ExpiresAt) || !validSHA256Digest(record.IdempotencyDigest) {
		return false
	}
	result := record.Operation.Result
	switch record.Operation.State {
	case domain.OperationRunning:
		if record.LeaseExpiresAt.IsZero() || !record.Operation.UpdatedAt.Before(record.LeaseExpiresAt) || record.Operation.ErrorKind != "" {
			return false
		}
		if result == nil {
			return record.WaitGenerationID == ""
		}
		return result.State == StateGenerating && result.Path.Valid() && !result.Path.IsRoot() && result.Version != "" && result.Variant > 0 && validGenerationID(record.WaitGenerationID)
	case domain.OperationSucceeded:
		return record.LeaseExpiresAt.IsZero() && record.WaitGenerationID == "" && record.Operation.ErrorKind == "" && result != nil && result.State == StateReady && result.Artifact != nil
	case domain.OperationFailed:
		return record.LeaseExpiresAt.IsZero() && record.WaitGenerationID == "" && validOperationErrorKind(record.Operation.ErrorKind) && (result == nil || result.State != StateReady && result.State != StateGenerating)
	default:
		return false
	}
}

func validGenerationID(value string) bool {
	return value != "" && len(value) <= 128 && !strings.ContainsAny(value, "\r\n\x00/")
}

func validSHA256Digest(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func validOperationErrorKind(kind domain.ErrorKind) bool {
	switch kind {
	case domain.ErrorInvalid, domain.ErrorUnauthenticated, domain.ErrorUnauthorized, domain.ErrorNotFound, domain.ErrorConflict,
		domain.ErrorPreconditionFailed, domain.ErrorRateLimited, domain.ErrorUnavailable, domain.ErrorInternal:
		return true
	default:
		return false
	}
}

func validOperationIndex(record operationIndexRecord) bool {
	if record.SchemaVersion != 1 || record.Entries == nil || len(record.Entries) > maxOperationsPerShard {
		return false
	}
	seen := make(map[domain.OperationID]struct{}, len(record.Entries))
	seenIdempotency := make(map[string]struct{}, len(record.Entries))
	for _, entry := range record.Entries {
		if entry.OperationID == "" || entry.IdempotencyDigest == "" || entry.ExpiresAt.IsZero() {
			return false
		}
		if _, exists := seen[entry.OperationID]; exists {
			return false
		}
		if _, exists := seenIdempotency[entry.IdempotencyDigest]; exists {
			return false
		}
		seen[entry.OperationID] = struct{}{}
		seenIdempotency[entry.IdempotencyDigest] = struct{}{}
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
	return s.resolveItemWithEntry(ctx, scope, item, entry, nil, false, allowGeneration, force, explicit)
}

func (s *Service) resolveItemWithEntry(ctx context.Context, scope domain.Scope, item ItemRequest, entry domain.Entry, known *ArtifactMetadata, catalogChecked, allowGeneration, force, explicit bool) (ItemResult, error) {
	result := ItemResult{Path: item.Path, Version: item.Version, Variant: item.Variant}
	if entry.Kind != domain.EntryFile || entry.Version != item.Version {
		return ItemResult{}, domain.NewError(domain.ErrorPreconditionFailed, "preview source version does not match")
	}
	generator := s.generatorFor(entry.MediaType)
	if generator == nil {
		result.State, result.Reason = StateUnsupported, "input-format"
		return result, nil
	}
	binding := previewBinding(scope.UserID(), entry, item.Variant, generator.RecipeID())
	if !binding.Valid() {
		return ItemResult{}, domain.NewError(domain.ErrorInternal, "source provider omitted preview content identity")
	}
	if !force {
		if known != nil {
			ready, found, resolveErr := s.knownReadyResult(ctx, binding, *known, result)
			if resolveErr != nil || found {
				return ready, resolveErr
			}
		}
		if catalogChecked {
			result.State = StateMissing
			return result, nil
		}
		ready, found, resolveErr := s.readyResult(ctx, readySelection(scope.UserID(), item.Path, binding), result)
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
	waitingFor, generationErr := s.generateOnce(ctx, scope, entry, binding, generator, force)
	if generationErr != nil {
		if errors.Is(generationErr, domain.ErrUnavailable) {
			result.State = StateUnavailable
			return result, nil
		}
		if errors.Is(generationErr, domain.ErrConflict) {
			result.State = StateGenerating
			result.waitingFor = waitingFor
			return result, nil
		}
		result.State = StateFailed
		return result, nil
	}
	ready, found, err := s.readyResult(ctx, readySelection(scope.UserID(), item.Path, binding), result)
	if err != nil {
		return ItemResult{}, err
	}
	if !found {
		result.State = StateFailed
		return result, nil
	}
	return ready, nil
}

func (s *Service) readyResult(ctx context.Context, selection ReadySelection, result ItemResult) (ItemResult, bool, error) {
	artifact, err := s.store.Latest(ctx, selection.Binding)
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
	if err := s.store.RecordReady(ctx, selection, artifact); err != nil {
		if errors.Is(err, domain.ErrUnavailable) {
			result.State = StateUnavailable
			return result, true, nil
		}
		return ItemResult{}, false, err
	}
	return s.knownReadyResult(ctx, selection.Binding, artifact, result)
}

func (s *Service) knownReadyResult(ctx context.Context, binding Binding, artifact ArtifactMetadata, result ItemResult) (ItemResult, bool, error) {
	capability, err := s.store.CreateKnownDownload(ctx, binding, artifact)
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

func previewBinding(owner domain.UserID, entry domain.Entry, variant int, recipeID string) Binding {
	return Binding{
		Owner: owner, ContentID: entry.ContentID, ContentVersion: entry.ContentVersion, MediaType: entry.MediaType,
		SourceSize: entry.Size, RecipeID: recipeID, Variant: variant,
	}
}

func readySelection(owner domain.UserID, path domain.UserPath, binding Binding) ReadySelection {
	digest := sha256.Sum256([]byte("endlessfs-preview-ready-scope-v1\x00" + owner.String() + "\x00" + path.Parent().String()))
	return ReadySelection{CacheScope: base64.RawURLEncoding.EncodeToString(digest[:]), Binding: binding}
}

func (s *Service) generateOnce(ctx context.Context, scope domain.Scope, entry domain.Entry, binding Binding, generator Generator, force bool) (string, error) {
	key := generationKey(binding)
	s.mu.Lock()
	if call, found := s.inflight[key]; found {
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", domain.WrapError(domain.ErrorUnavailable, "preview generation canceled", ctx.Err())
		case <-call.done:
			return call.waitingFor, call.err
		}
	}
	call := &generationCall{done: make(chan struct{})}
	s.inflight[key] = call
	s.mu.Unlock()

	call.waitingFor, call.err = s.generate(ctx, scope, entry, binding, generator, force)
	s.mu.Lock()
	delete(s.inflight, key)
	close(call.done)
	s.mu.Unlock()
	return call.waitingFor, call.err
}

func (s *Service) generate(ctx context.Context, scope domain.Scope, entry domain.Entry, binding Binding, generator Generator, force bool) (string, error) {
	operationContext, cancel := context.WithTimeout(ctx, s.options.OperationTimeout)
	defer cancel()
	release, err := s.acquire(operationContext, scope.UserID())
	if err != nil {
		return "", err
	}
	defer release()
	if !force {
		if _, err := s.store.Latest(operationContext, binding); err == nil {
			return "", nil
		} else if !errors.Is(err, domain.ErrNotFound) {
			return "", err
		}
	}
	generationID, err := s.ids.OpaqueID()
	if err != nil {
		return "", err
	}
	claim, err := s.store.Claim(operationContext, binding, generationID, s.clock.Now().UTC().Add(s.options.OperationTimeout))
	if errors.Is(err, domain.ErrConflict) {
		if !force {
			if _, latestErr := s.store.Latest(operationContext, binding); latestErr == nil {
				return "", nil
			}
		}
		return claim.ID, err
	}
	if err != nil {
		return "", err
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
		return "", err
	}
	request, err := http.NewRequestWithContext(operationContext, capability.Method, capability.URL, nil)
	if err != nil {
		return "", domain.WrapError(domain.ErrorInternal, "could not construct preview source request", err)
	}
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return "", domain.WrapError(domain.ErrorUnavailable, "preview source unavailable", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", domain.NewError(domain.ErrorUnavailable, "preview source unavailable")
	}
	sourceBytes, err := io.ReadAll(io.LimitReader(response.Body, s.options.HardMaxSourceBytes+1))
	if err != nil {
		return "", domain.WrapError(domain.ErrorUnavailable, "preview source unavailable", err)
	}
	if int64(len(sourceBytes)) > s.options.HardMaxSourceBytes || int64(len(sourceBytes)) != entry.Size {
		return "", domain.NewError(domain.ErrorInvalid, "preview source exceeds hard limits")
	}
	generated, err := generator.Generate(operationContext, GenerationRequest{Source: bytes.NewReader(sourceBytes), SourceSize: entry.Size, MediaType: entry.MediaType, Variant: binding.Variant})
	if err != nil {
		if operationContext.Err() != nil {
			return "", domain.WrapError(domain.ErrorUnavailable, "preview generation timed out", operationContext.Err())
		}
		return "", domain.NewError(domain.ErrorInvalid, "preview generator rejected source")
	}
	sum := sha256.Sum256(generated.Bytes)
	artifact := Artifact{
		GenerationID: generationID, Variant: binding.Variant, Width: generated.Width, Height: generated.Height,
		ContentType: ContentTypeWebP, Size: int64(len(generated.Bytes)), SHA256: base64.RawURLEncoding.EncodeToString(sum[:]),
		CRC32C: ChecksumCRC32C(generated.Bytes), Bytes: generated.Bytes,
	}
	if !artifact.ValidFor(binding) {
		return "", domain.NewError(domain.ErrorInvalid, "preview generator produced invalid artifact")
	}
	if err := s.store.Commit(operationContext, binding, claim, artifact); err != nil {
		return "", err
	}
	committed = true
	return "", nil
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
