package preview

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
)

const queueScanLimit = 64
const queueAttempts = 3

// Queue records and their ordering indexes share the existing administration
// consistency domain. A claim atomically takes both one job and one global
// worker lane, so MaxConcurrency applies across processes, not per replica.
// Source locators stay in application state, never in the preview provider.
type thumbnailJob struct {
	SchemaVersion  int         `json:"schemaVersion"`
	ID             string      `json:"id"`
	Binding        Binding     `json:"binding"`
	Item           ItemRequest `json:"item"`
	RequestedAt    time.Time   `json:"requestedAt"`
	EnqueuedAt     time.Time   `json:"enqueuedAt"`
	AvailableAt    time.Time   `json:"availableAt"`
	State          State       `json:"state"`
	Attempts       int         `json:"attempts"`
	Epoch          uint64      `json:"epoch"`
	LeaseID        string      `json:"leaseID,omitempty"`
	LeaseExpiresAt time.Time   `json:"leaseExpiresAt,omitempty"`
}

type thumbnailIndex struct {
	SchemaVersion int    `json:"schemaVersion"`
	JobID         string `json:"jobID"`
}

type thumbnailLane struct {
	SchemaVersion  int       `json:"schemaVersion"`
	Claims         uint64    `json:"claims"`
	LeaseID        string    `json:"leaseID,omitempty"`
	LeaseExpiresAt time.Time `json:"leaseExpiresAt,omitempty"`
}

type thumbnailClaim struct {
	job     thumbnailJob
	laneKey state.Key
}

func thumbnailKey(kind string, parts ...string) state.Key {
	return state.MustKey(state.NamespaceOperations, append([]string{"admin", "thumbnail-" + kind}, parts...)...)
}

func thumbnailID(binding Binding) string {
	digest := sha256.Sum256([]byte(binding.Owner.String() + "\x00" + string(binding.ContentID) + "\x00" + string(binding.ContentVersion) + "\x00" + binding.MediaType + "\x00" + strconv.FormatInt(binding.SourceSize, 10) + "\x00" + binding.RecipeID + "\x00" + strconv.Itoa(binding.Variant)))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// Binary digits retain their order through state's base64url part encoding.
// Ordinary decimal/hex timestamp strings do not have that property.
func thumbnailOrder(at time.Time, newest bool) string {
	ordered := uint64(at.UnixNano()) ^ (uint64(1) << 63)
	if newest {
		ordered = ^ordered
	}
	return fmt.Sprintf("%064b", ordered)
}

func (job thumbnailJob) recentKey() state.Key {
	return thumbnailKey("recent", thumbnailOrder(job.RequestedAt, true), job.ID)
}
func (job thumbnailJob) oldestKey() state.Key {
	return thumbnailKey("oldest", thumbnailOrder(job.EnqueuedAt, false), job.ID)
}
func (job thumbnailJob) terminalKey() state.Key {
	return thumbnailKey("terminal", thumbnailOrder(job.AvailableAt, false), job.ID)
}

func (job thumbnailJob) valid() bool {
	if job.SchemaVersion != 1 || !job.Binding.Valid() || thumbnailID(job.Binding) != job.ID || !job.Item.Path.Valid() || job.Item.Path.IsRoot() || job.Item.Version == "" || job.Item.Variant != job.Binding.Variant || job.EnqueuedAt.IsZero() || job.RequestedAt.Before(job.EnqueuedAt) || job.AvailableAt.IsZero() || job.Attempts < 0 || job.Attempts > queueAttempts {
		return false
	}
	switch job.State {
	case StateQueued, StateFailed:
		return job.LeaseID == "" && job.LeaseExpiresAt.IsZero()
	case StateGenerating:
		return validGenerationID(job.LeaseID) && !job.LeaseExpiresAt.IsZero() && job.Epoch > 0
	default:
		return false
	}
}

func (s *Service) loadThumbnail(ctx context.Context, id string) (thumbnailJob, state.Value, error) {
	value, err := s.state.Get(ctx, thumbnailKey("job", id))
	if err != nil {
		return thumbnailJob{}, value, err
	}
	return decodeThumbnail(id, value)
}

func decodeThumbnail(id string, value state.Value) (thumbnailJob, state.Value, error) {
	var job thumbnailJob
	if state.DecodeJSON(value.Data, &job) != nil || !job.valid() || job.ID != id {
		return thumbnailJob{}, value, domain.NewError(domain.ErrorInvalid, "invalid thumbnail job")
	}
	return job, value, nil
}

type thumbnailRequest struct {
	binding Binding
	item    ItemRequest
}

func (s *Service) enqueueThumbnail(ctx context.Context, binding Binding, item ItemRequest) (State, error) {
	states, err := s.enqueueThumbnails(ctx, []thumbnailRequest{{binding: binding, item: item}})
	if err != nil {
		return "", err
	}
	return states[0], nil
}

// A visible batch publishes all job/index upserts through one existing state
// transaction. Identities remain per artifact; batching only amortizes the
// shared consistency-domain publication and never becomes an idempotency key.
func (s *Service) enqueueThumbnails(ctx context.Context, requests []thumbnailRequest) ([]State, error) {
	if len(requests) == 0 {
		return nil, nil
	}
	for range 16 {
		now := s.clock.Now().UTC()
		unique := make(map[string]thumbnailRequest, len(requests))
		for _, request := range requests {
			unique[thumbnailID(request.binding)] = request
		}
		ids := make([]string, 0, len(unique))
		for id := range unique {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		// The existing portable multi-reader shares one authenticated domain
		// snapshot for a warm batch. A cold/partial batch uses exact gets;
		// unavailable or invalid state never falls back to presumed absence.
		batchValues := make(map[string]state.Value, len(ids))
		if reader, ok := s.state.(interface {
			GetMany(context.Context, []state.Key) ([]state.Value, error)
		}); ok {
			keys := make([]state.Key, len(ids))
			for i, id := range ids {
				keys[i] = thumbnailKey("job", id)
			}
			values, err := reader.GetMany(ctx, keys)
			if err == nil {
				if len(values) != len(ids) {
					return nil, domain.NewError(domain.ErrorInternal, "invalid thumbnail state batch")
				}
				for i, id := range ids {
					batchValues[id] = values[i]
				}
			} else if !errors.Is(err, domain.ErrNotFound) {
				return nil, err
			}
		}
		statuses := make(map[string]State, len(unique))
		changes := make([]state.Change, 0, len(unique)*3)
		intent := []string{now.Format(time.RFC3339Nano)}
		for _, id := range ids {
			request := unique[id]
			previous, value, err := thumbnailJob{}, state.Value{}, error(nil)
			if batchValue, found := batchValues[id]; found {
				previous, value, err = decodeThumbnail(id, batchValue)
			} else {
				previous, value, err = s.loadThumbnail(ctx, id)
			}
			if err != nil && !errors.Is(err, domain.ErrNotFound) {
				return nil, err
			}
			exists := err == nil
			job, requestedAt := previous, now
			if !exists {
				job = thumbnailJob{SchemaVersion: 1, ID: id, Binding: request.binding, EnqueuedAt: now, AvailableAt: now, State: StateQueued}
			}
			if exists && job.State == StateFailed {
				if now.Before(job.AvailableAt) {
					statuses[id] = StateFailed
					continue
				}
				job = thumbnailJob{SchemaVersion: 1, ID: id, Binding: request.binding, EnqueuedAt: now, AvailableAt: now, State: StateQueued}
			}
			if requestedAt.Before(job.RequestedAt) {
				requestedAt = job.RequestedAt
			}
			job.Item, job.RequestedAt = request.item, requestedAt
			statuses[id] = job.State
			if job.State == StateGenerating && !now.Before(job.LeaseExpiresAt) {
				statuses[id] = StateQueued
			}
			// A refresh never extends a lease or resets running/retry attempts.
			if exists && previous.RequestedAt.Equal(requestedAt) && previous.Item == request.item {
				continue
			}
			body, err := state.EncodeJSON(job)
			if err != nil {
				return nil, err
			}
			index, _ := state.EncodeJSON(thumbnailIndex{SchemaVersion: 1, JobID: id})
			requirement := state.RequirementAbsent
			if exists {
				requirement = state.RequirementPresent
			}
			changes = append(changes, state.Change{Key: itemKey(job), Requirement: requirement, ExpectedVersion: value.Version, Data: body})
			if exists && previous.State == StateFailed {
				changes = append(changes, state.Change{Key: previous.terminalKey(), Requirement: state.RequirementPresent, Delete: true})
			} else if exists && previous.recentKey() != job.recentKey() {
				changes = append(changes, state.Change{Key: previous.recentKey(), Requirement: state.RequirementPresent, Delete: true})
			}
			changes = append(changes, state.Change{Key: job.recentKey(), Requirement: state.RequirementAny, Data: index}, state.Change{Key: job.oldestKey(), Requirement: state.RequirementAny, Data: index})
			intent = append(intent, id, string(value.Version), requestedAt.Format(time.RFC3339Nano), request.item.Path.String(), string(request.item.Version))
		}
		if len(changes) > 0 {
			_, err := s.state.Mutate(ctx, state.Mutation{ID: previewMutationID("thumbnail-upsert", intent...), RetainUntil: now.Add(s.options.OperationRetention), Changes: changes})
			if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPreconditionFailed) {
				continue
			}
			if err != nil {
				return nil, err
			}
		}
		states := make([]State, len(requests))
		for i, request := range requests {
			states[i] = statuses[thumbnailID(request.binding)]
		}
		s.wakeThumbnailWorker()
		return states, nil
	}
	return nil, domain.NewError(domain.ErrorUnavailable, "thumbnail queue contention")
}

func (s *Service) wakeThumbnailWorker() {
	select {
	case s.thumbnailWake <- struct{}{}:
	default:
	}
}

// Run processes durable jobs until cancellation. It owns no correctness state:
// another replica or a restarted process discovers pending/expired claims from
// the same indexes. Polling also discovers work enqueued on the other replica.
func (s *Service) Run(ctx context.Context) {
	if s.store == nil || !s.options.Automatic {
		return
	}
	var workers sync.WaitGroup
	for range s.options.MaxConcurrency {
		workers.Go(func() {
			idleDelay := time.Second
			for ctx.Err() == nil {
				worked, err := s.WorkOne(ctx)
				if worked && err == nil {
					idleDelay = time.Second
					continue
				}
				timer := time.NewTimer(idleDelay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-s.thumbnailWake:
					timer.Stop()
					idleDelay = time.Second
				case <-timer.C:
					idleDelay = min(30*time.Second, idleDelay*2)
				}
			}
		})
	}
	workers.Wait()
}

// WorkOne performs at most one bounded job. Separating one step from Run makes
// crash/restart, priority and multi-worker schedules deterministic in tests.
func (s *Service) WorkOne(ctx context.Context) (worked bool, err error) {
	ctx, activity := telemetry.Start(ctx, telemetry.PreviewQueue, telemetry.Application)
	defer telemetry.Finish(activity, &err)
	if s.store == nil || !s.options.Automatic {
		return false, nil
	}
	if removed, err := s.cleanupThumbnail(ctx); err != nil || removed {
		return removed, err
	}
	claim, found, err := s.claimThumbnail(ctx)
	if err != nil || !found {
		return false, err
	}
	workCtx, cancel := context.WithTimeout(ctx, s.options.OperationTimeout)
	defer cancel()
	job := claim.job
	result, workErr := s.runThumbnail(workCtx, job)
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), s.options.StartupTimeout)
	defer finishCancel()
	return true, s.finishThumbnail(finishCtx, claim, result, workErr)
}

func (s *Service) runThumbnail(ctx context.Context, job thumbnailJob) (ItemResult, error) {
	scope, err := domain.NewScope(job.Binding.Owner, domain.AreaLive)
	if err != nil {
		return ItemResult{}, err
	}
	entry, err := s.source.Stat(ctx, scope, job.Item.Path)
	if err != nil {
		return ItemResult{}, err
	}
	if entry.Version != job.Item.Version || previewBinding(scope.UserID(), entry, job.Item.Variant, job.Binding.RecipeID) != job.Binding {
		return ItemResult{}, domain.NewError(domain.ErrorPreconditionFailed, "thumbnail source changed")
	}
	generator := s.generatorFor(entry.MediaType)
	if generator == nil || generator.RecipeID() != job.Binding.RecipeID {
		return ItemResult{State: StateUnsupported}, nil
	}
	// This rechecks the full artifact manifest before claiming/reading a source.
	return s.resolveItemWithEntry(ctx, scope, job.Item, entry, nil, false, true, false, false)
}

func (s *Service) claimThumbnail(ctx context.Context) (thumbnailClaim, bool, error) {
	now := s.clock.Now().UTC()
	for laneNumber := 0; laneNumber < s.options.MaxConcurrency; laneNumber++ {
		laneKey := thumbnailKey("lane", strconv.Itoa(laneNumber))
		laneValue, err := s.state.Get(ctx, laneKey)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return thumbnailClaim{}, false, err
		}
		lane := thumbnailLane{}
		exists := err == nil
		if exists && (state.DecodeJSON(laneValue.Data, &lane) != nil || lane.SchemaVersion != 1 || (lane.LeaseID == "") != lane.LeaseExpiresAt.IsZero()) {
			return thumbnailClaim{}, false, domain.NewError(domain.ErrorInvalid, "invalid thumbnail worker lane")
		}
		if !exists {
			lane.SchemaVersion = 1
		}
		if lane.LeaseID != "" && now.Before(lane.LeaseExpiresAt) {
			continue
		}
		kind := "recent"
		if lane.Claims%8 == 7 {
			kind = "oldest"
		}
		job, value, found, err := s.nextThumbnail(ctx, kind, now)
		if err != nil {
			return thumbnailClaim{}, false, err
		}
		if !found {
			return thumbnailClaim{}, false, nil
		}
		leaseID, err := s.ids.OpaqueID()
		if err != nil {
			return thumbnailClaim{}, false, err
		}
		job.State, job.LeaseID, job.LeaseExpiresAt = StateGenerating, leaseID, now.Add(s.options.OperationTimeout)
		job.Epoch++
		job.Attempts++
		lane.Claims++
		lane.LeaseID, lane.LeaseExpiresAt = leaseID, job.LeaseExpiresAt
		jobBody, _ := state.EncodeJSON(job)
		laneBody, _ := state.EncodeJSON(lane)
		requirement := state.RequirementAbsent
		if exists {
			requirement = state.RequirementPresent
		}
		_, err = s.state.Mutate(ctx, state.Mutation{ID: "thumbnail-claim-" + leaseID, RetainUntil: now.Add(s.options.OperationRetention), Changes: []state.Change{
			{Key: thumbnailKey("job", job.ID), Requirement: state.RequirementPresent, ExpectedVersion: value.Version, Data: jobBody},
			{Key: laneKey, Requirement: requirement, ExpectedVersion: laneValue.Version, Data: laneBody},
		}})
		if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPreconditionFailed) {
			continue
		}
		if err != nil {
			return thumbnailClaim{}, false, err
		}
		return thumbnailClaim{job: job, laneKey: laneKey}, true, nil
	}
	return thumbnailClaim{}, false, nil
}

func (s *Service) nextThumbnail(ctx context.Context, kind string, now time.Time) (thumbnailJob, state.Value, bool, error) {
	page, err := s.state.List(ctx, state.MustPrefix(state.NamespaceOperations, "admin", "thumbnail-"+kind), state.PageRequest{Limit: queueScanLimit})
	if err != nil {
		return thumbnailJob{}, state.Value{}, false, err
	}
	for _, item := range page.Items {
		var index thumbnailIndex
		if state.DecodeJSON(item.Value.Data, &index) != nil || index.SchemaVersion != 1 || !validSHA256Digest(index.JobID) {
			return thumbnailJob{}, state.Value{}, false, domain.NewError(domain.ErrorInvalid, "invalid thumbnail queue index")
		}
		job, value, loadErr := s.loadThumbnail(ctx, index.JobID)
		if loadErr != nil {
			if errors.Is(loadErr, domain.ErrNotFound) {
				if _, err := s.state.Get(ctx, item.Key); errors.Is(err, domain.ErrNotFound) {
					continue
				}
			}
			return thumbnailJob{}, state.Value{}, false, loadErr
		}
		expected := job.recentKey()
		if kind == "oldest" {
			expected = job.oldestKey()
		}
		if item.Key != expected {
			if _, err := s.state.Get(ctx, item.Key); errors.Is(err, domain.ErrNotFound) {
				continue
			}
			return thumbnailJob{}, state.Value{}, false, domain.NewError(domain.ErrorInvalid, "thumbnail index does not match job")
		}
		if job.State == StateFailed || now.Before(job.AvailableAt) || job.State == StateGenerating && now.Before(job.LeaseExpiresAt) {
			continue
		}
		if job.Attempts >= queueAttempts { // A repeatedly crashing worker also reaches a terminal state.
			job.State, job.LeaseID, job.LeaseExpiresAt = StateFailed, "", time.Time{}
			job.AvailableAt = now.Add(s.options.OperationRetention)
			body, _ := state.EncodeJSON(job)
			index, _ := state.EncodeJSON(thumbnailIndex{SchemaVersion: 1, JobID: job.ID})
			_, err := s.state.Mutate(ctx, state.Mutation{ID: previewMutationID("thumbnail-abandoned", job.ID, string(value.Version)), RetainUntil: now.Add(s.options.OperationRetention), Changes: []state.Change{
				{Key: itemKey(job), Requirement: state.RequirementPresent, ExpectedVersion: value.Version, Data: body},
				{Key: job.recentKey(), Requirement: state.RequirementPresent, Delete: true},
				{Key: job.oldestKey(), Requirement: state.RequirementPresent, Delete: true},
				{Key: job.terminalKey(), Requirement: state.RequirementAbsent, Data: index},
			}})
			if err != nil && !errors.Is(err, domain.ErrPreconditionFailed) && !errors.Is(err, domain.ErrConflict) {
				return thumbnailJob{}, state.Value{}, false, err
			}
			continue
		}
		return job, value, true, nil
	}
	return thumbnailJob{}, state.Value{}, false, nil
}

func itemKey(job thumbnailJob) state.Key { return thumbnailKey("job", job.ID) }

func (s *Service) finishThumbnail(ctx context.Context, claim thumbnailClaim, result ItemResult, workErr error) error {
	for range 16 {
		var laneValue state.Value
		var value state.Value
		var job thumbnailJob
		var err error
		batched := false
		if reader, ok := s.state.(interface {
			GetMany(context.Context, []state.Key) ([]state.Value, error)
		}); ok {
			values, readErr := reader.GetMany(ctx, []state.Key{itemKey(claim.job), claim.laneKey})
			if readErr == nil {
				if len(values) != 2 {
					return domain.NewError(domain.ErrorInternal, "invalid thumbnail completion batch")
				}
				job, value, err = decodeThumbnail(claim.job.ID, values[0])
				laneValue, batched = values[1], true
			} else if !errors.Is(readErr, domain.ErrNotFound) {
				return readErr
			}
		}
		if !batched {
			job, value, err = s.loadThumbnail(ctx, claim.job.ID)
		}
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if job.Epoch != claim.job.Epoch || job.LeaseID != claim.job.LeaseID {
			return nil
		}
		if !batched {
			laneValue, err = s.state.Get(ctx, claim.laneKey)
			if err != nil {
				return err
			}
		}
		var lane thumbnailLane
		if state.DecodeJSON(laneValue.Data, &lane) != nil || lane.SchemaVersion != 1 {
			return domain.NewError(domain.ErrorInvalid, "invalid thumbnail worker lane")
		}
		if lane.LeaseID != claim.job.LeaseID {
			return nil
		}
		now := s.clock.Now().UTC()
		lane.LeaseID, lane.LeaseExpiresAt = "", time.Time{}
		laneBody, _ := state.EncodeJSON(lane)
		changes := []state.Change{{Key: claim.laneKey, Requirement: state.RequirementPresent, ExpectedVersion: laneValue.Version, Data: laneBody}}
		if workErr == nil && result.State == StateReady {
			changes = append(changes, state.Change{Key: itemKey(job), Requirement: state.RequirementPresent, ExpectedVersion: value.Version, Delete: true}, state.Change{Key: job.recentKey(), Requirement: state.RequirementPresent, Delete: true}, state.Change{Key: job.oldestKey(), Requirement: state.RequirementPresent, Delete: true})
		} else {
			job.State, job.LeaseID, job.LeaseExpiresAt = StateFailed, "", time.Time{}
			if result.State == StateGenerating || (errors.Is(workErr, domain.ErrUnavailable) || result.State == StateUnavailable || errors.Is(workErr, context.Canceled) || errors.Is(workErr, context.DeadlineExceeded)) && job.Attempts < queueAttempts {
				job.State = StateQueued
				job.AvailableAt = now.Add(time.Second * time.Duration(1<<job.Attempts))
				if result.State == StateGenerating {
					job.Attempts--
				}
			}
			if job.Item != claim.job.Item && (errors.Is(workErr, domain.ErrNotFound) || errors.Is(workErr, domain.ErrPreconditionFailed)) {
				job.State = StateQueued
				job.AvailableAt = now
				job.Attempts--
			}
			if job.State == StateFailed {
				job.AvailableAt = now.Add(s.options.OperationRetention)
			}
			body, _ := state.EncodeJSON(job)
			changes = append(changes, state.Change{Key: itemKey(job), Requirement: state.RequirementPresent, ExpectedVersion: value.Version, Data: body})
			if job.State == StateFailed {
				index, _ := state.EncodeJSON(thumbnailIndex{SchemaVersion: 1, JobID: job.ID})
				changes = append(changes, state.Change{Key: job.recentKey(), Requirement: state.RequirementPresent, Delete: true}, state.Change{Key: job.oldestKey(), Requirement: state.RequirementPresent, Delete: true}, state.Change{Key: job.terminalKey(), Requirement: state.RequirementAbsent, Data: index})
			}
		}
		_, err = s.state.Mutate(ctx, state.Mutation{ID: previewMutationID("thumbnail-finish", job.ID, string(value.Version), claim.job.LeaseID), RetainUntil: now.Add(s.options.OperationRetention), Changes: changes})
		if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPreconditionFailed) {
			continue
		}
		return err
	}
	return domain.NewError(domain.ErrorUnavailable, "thumbnail completion contention")
}

// Retained failures are bounded by the existing operation-retention window;
// they cannot fill the active priority indexes or accumulate forever.
func (s *Service) cleanupThumbnail(ctx context.Context) (bool, error) {
	page, err := s.state.List(ctx, state.MustPrefix(state.NamespaceOperations, "admin", "thumbnail-terminal"), state.PageRequest{Limit: 1})
	if err != nil || len(page.Items) == 0 {
		return false, err
	}
	item := page.Items[0]
	var index thumbnailIndex
	if state.DecodeJSON(item.Value.Data, &index) != nil || index.SchemaVersion != 1 || !validSHA256Digest(index.JobID) {
		return false, domain.NewError(domain.ErrorInvalid, "invalid retained thumbnail index")
	}
	job, value, err := s.loadThumbnail(ctx, index.JobID)
	if err != nil {
		return false, err
	}
	if job.State != StateFailed || job.terminalKey() != item.Key {
		return false, domain.NewError(domain.ErrorInvalid, "invalid retained thumbnail binding")
	}
	now := s.clock.Now().UTC()
	if now.Before(job.AvailableAt) {
		return false, nil
	}
	_, err = s.state.Mutate(ctx, state.Mutation{ID: previewMutationID("thumbnail-cleanup", job.ID, string(value.Version)), RetainUntil: now.Add(s.options.OperationRetention), Changes: []state.Change{
		{Key: itemKey(job), Requirement: state.RequirementPresent, ExpectedVersion: value.Version, Delete: true},
		{Key: item.Key, Requirement: state.RequirementPresent, ExpectedVersion: item.Value.Version, Delete: true},
	}})
	if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrConflict) {
		return false, nil
	}
	return err == nil, err
}
