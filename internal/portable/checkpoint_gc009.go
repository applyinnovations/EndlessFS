package portable

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const (
	checkpointGarbageCollectionSchema   = "checkpoint-garbage-collection-v2"
	checkpointGarbageCollectionSweeping = "sweeping"
	checkpointGarbageCollectionComplete = "complete"

	StepCheckpointGarbageAfterSession  = "checkpoint-garbage:after-session"
	StepCheckpointGarbageAfterPage     = "checkpoint-garbage:after-page"
	StepCheckpointGarbageAfterComplete = "checkpoint-garbage:after-complete"
)

var errCheckpointGarbageContended = errors.New("checkpoint garbage-collection session CAS contended")

type checkpointGarbageCollectionSession struct {
	object   objectstore.Object
	envelope storageformat.Envelope
	value    storageformat.GarbageCollectionSession
}

type checkpointGarbageSweep struct {
	backend  objectstore.FileControlBackend
	role     string
	prefix   string
	eligible func(objectstore.Key) bool
}

// runCheckpointGarbageCollection reclaims only objects omitted from one
// verified, closed-gate checkpoint. The checkpoint is the immutable mark set;
// no object-per-item provider journal is created. Progress carries only a
// portable ordered-key cursor. Conditional deletes bind a stale worker to the
// exact object incarnation it listed, so a key recreated after gate reopening
// cannot be deleted by that worker.
func (e *Engine) runCheckpointGarbageCollection(ctx context.Context, checkpoint storageformat.Checkpoint) error {
	return e.runCheckpointGarbageCollectionWithVersions(ctx, checkpoint, nil)
}

func (e *Engine) runCheckpointGarbageCollectionWithVersions(ctx context.Context, checkpoint storageformat.Checkpoint, fastVersions map[string]objectstore.ObjectInfo) error {
	if checkpoint.SchemaVersion != 3 || checkpoint.CheckpointID == "" || checkpoint.InventoryDigest == "" {
		return domain.NewError(domain.ErrorInvalid, "checkpoint garbage collection requires checkpoint v3")
	}
	traced := providerbudget.WithTrace(ctx, providerbudget.Trace{Operation: "checkpoint-garbage", Subsystem: "garbage-collection"})
	plan, found, err := e.readCheckpointGarbagePlan(traced, checkpoint)
	if err != nil {
		return err
	}
	if found {
		return e.runCheckpointGarbagePlan(traced, checkpoint, plan, fastVersions)
	}
	return e.runLegacyCheckpointGarbageCollection(traced, checkpoint)
}

// runLegacyCheckpointGarbageCollection retains recovery support for schema-009
// checkpoints created before immutable garbage plans were introduced.
func (e *Engine) runLegacyCheckpointGarbageCollection(ctx context.Context, checkpoint storageformat.Checkpoint) error {
	session, err := e.readOrCreateCheckpointGarbageSession(ctx, checkpoint)
	if err != nil {
		return err
	}
	if session.value.Phase == checkpointGarbageCollectionComplete {
		return nil
	}
	reachable, err := e.checkpointGarbageReachability(ctx, checkpoint)
	if err != nil {
		return err
	}
	defer reachable.Close()
	for range 64 {
		if err := e.sweepCheckpointGarbage(ctx, checkpoint, session, reachable); errors.Is(err, errCheckpointGarbageContended) {
			session, err = e.readOrCreateCheckpointGarbageSession(ctx, checkpoint)
			if err != nil {
				return err
			}
			if session.value.Phase == checkpointGarbageCollectionComplete {
				return nil
			}
			continue
		} else {
			return err
		}
	}
	return domain.WrapError(domain.ErrorUnavailable, "checkpoint garbage collection remained contended", errCheckpointGarbageContended)
}

func (e *Engine) checkpointGarbageReachability(ctx context.Context, checkpoint storageformat.Checkpoint) (*checkpointVisitSet, error) {
	reachable, err := newCheckpointVisitSet()
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*checkpointVisitSet, error) {
		_ = reachable.Close()
		return nil, err
	}
	if err := e.visitCheckpointInventory(ctx, checkpoint, func(entry storageformat.CheckpointInventoryEntry) error {
		role := garbageCollectionStateRole
		if entry.FileData {
			role = garbageCollectionFileRole
		}
		seen, err := reachable.Seen(role + "\x00" + entry.Object.Key)
		if err != nil {
			return err
		}
		if seen {
			return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint inventory repeats an object")
		}
		return nil
	}); err != nil {
		return fail(err)
	}
	return reachable, nil
}

func (e *Engine) checkpointGarbageSweeps() []checkpointGarbageSweep {
	return []checkpointGarbageSweep{
		{backend: e.backend, role: garbageCollectionStateRole, prefix: storageformat.DomainPrefix(), eligible: checkpointDomainGarbageEligible},
		{backend: e.backend, role: garbageCollectionStateRole, prefix: storageformat.TransitionPrefix(), eligible: checkpointJSONGarbageEligible},
		{backend: e.backend, role: garbageCollectionStateRole, prefix: storageformat.ProjectionPrefix(), eligible: checkpointJSONGarbageEligible},
		{backend: e.backend, role: garbageCollectionStateRole, prefix: storageformat.LeasePrefix(), eligible: checkpointJSONGarbageEligible},
		{backend: e.fileBackend, role: garbageCollectionFileRole, prefix: storageformat.FilesystemPrefix(), eligible: checkpointBlobGarbageEligible},
	}
}

func checkpointJSONGarbageEligible(key objectstore.Key) bool {
	return strings.HasSuffix(key.String(), ".json")
}

func checkpointDomainGarbageEligible(key objectstore.Key) bool {
	value := key.String()
	if strings.HasPrefix(value, storageformat.StateQuerySnapshotPrefix()) {
		return strings.HasSuffix(value, ".json")
	}
	segments := strings.Split(value, "/")
	if len(segments) == 6 && segments[0] == "endlessfs" && segments[1] == "v1" && segments[2] == "domains" && segments[3] == "catalog" {
		return segments[4] == "pages" && strings.HasSuffix(segments[5], ".json")
	}
	if len(segments) != 6 && len(segments) != 7 || segments[0] != "endlessfs" || segments[1] != "v1" || segments[2] != "domains" || segments[4] == "" {
		return false
	}
	switch storageformat.ConsistencyDomainKind(segments[3]) {
	case storageformat.DomainNamespace, storageformat.DomainOwnerControl, storageformat.DomainAdmin, storageformat.DomainCapability, storageformat.DomainShare, storageformat.DomainIdentity, storageformat.DomainOwnerJobs:
	default:
		return false
	}
	if len(segments) == 6 {
		return segments[5] == "head.json"
	}
	return (segments[5] == "pages" || segments[5] == "snapshots") && strings.HasSuffix(segments[6], ".json")
}

func checkpointBlobGarbageEligible(key objectstore.Key) bool {
	segments := strings.Split(key.String(), "/")
	return len(segments) == 6 && segments[0] == "endlessfs" && segments[1] == "v1" && segments[2] == "fs" && segments[3] != "" && segments[4] == "blobs" && segments[5] != ""
}

func (e *Engine) readOrCreateCheckpointGarbageSession(ctx context.Context, checkpoint storageformat.Checkpoint) (checkpointGarbageCollectionSession, error) {
	if err := e.verifyCheckpointGarbageGate(ctx, checkpoint); err != nil {
		return checkpointGarbageCollectionSession{}, err
	}
	key := storageformat.GarbageCollectionSessionKey(checkpoint.CheckpointID)
	object, err := e.backend.Get(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		value := storageformat.GarbageCollectionSession{
			SchemaVersion: 2, CheckpointID: checkpoint.CheckpointID, GateEpoch: checkpoint.GateEpoch,
			GateVersion: checkpoint.InventoryDigest, Phase: checkpointGarbageCollectionSweeping, UpdatedAt: e.clock.Now().UTC(),
		}
		body, encodeErr := storageformat.EncodeEnvelope(checkpointGarbageCollectionSchema, key, 1, value)
		if encodeErr != nil {
			return checkpointGarbageCollectionSession{}, encodeErr
		}
		version, putErr := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
		if putErr == nil {
			if stepErr := e.step(ctx, StepCheckpointGarbageAfterSession); stepErr != nil {
				return checkpointGarbageCollectionSession{}, stepErr
			}
			return checkpointGarbageCollectionSession{
				object:   objectstore.Object{Key: key, Body: body, Version: version, Size: int64(len(body))},
				envelope: storageformat.Envelope{Revision: 1}, value: value,
			}, nil
		}
		if !errors.Is(putErr, domain.ErrConflict) && !errors.Is(putErr, domain.ErrPreconditionFailed) {
			return checkpointGarbageCollectionSession{}, putErr
		}
		object, err = e.backend.Get(ctx, key)
	}
	if err != nil {
		return checkpointGarbageCollectionSession{}, err
	}
	var envelope storageformat.Envelope
	var value storageformat.GarbageCollectionSession
	if err := storageformat.DecodeEnvelope(object.Body, key, checkpointGarbageCollectionSchema, &envelope, &value); err != nil || validateCheckpointGarbageSession(value, checkpoint, len(e.checkpointGarbageSweeps())) != nil {
		return checkpointGarbageCollectionSession{}, domain.NewError(domain.ErrorInvalid, "invalid checkpoint garbage-collection session")
	}
	return checkpointGarbageCollectionSession{object: object, envelope: envelope, value: value}, nil
}

func validateCheckpointGarbageSession(session storageformat.GarbageCollectionSession, checkpoint storageformat.Checkpoint, sweepCount int) error {
	if session.SchemaVersion != 2 || session.CheckpointID != checkpoint.CheckpointID || session.GateEpoch != checkpoint.GateEpoch || session.GateVersion != checkpoint.InventoryDigest || session.UpdatedAt.IsZero() || session.SweepIndex < 0 || session.SweepIndex > sweepCount || session.Phase != checkpointGarbageCollectionSweeping && session.Phase != checkpointGarbageCollectionComplete || session.Phase == checkpointGarbageCollectionComplete && (session.SweepIndex != sweepCount || session.After != "") {
		return domain.NewError(domain.ErrorInvalid, "invalid checkpoint garbage-collection session")
	}
	if session.After != "" {
		if _, err := objectstore.ParseKey(session.After); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) verifyCheckpointGarbageGate(ctx context.Context, checkpoint storageformat.Checkpoint) error {
	_, _, gate, err := e.readGate(ctx)
	if err != nil {
		return err
	}
	if gate.Mode != storageformat.GateClosed || gate.Epoch != checkpoint.GateEpoch || gate.CheckpointID != checkpoint.CheckpointID {
		return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint garbage collection requires its closed gate")
	}
	return nil
}

func (e *Engine) updateCheckpointGarbageSession(ctx context.Context, session checkpointGarbageCollectionSession) (checkpointGarbageCollectionSession, error) {
	session.value.UpdatedAt = e.clock.Now().UTC()
	body, err := storageformat.EncodeEnvelope(checkpointGarbageCollectionSchema, session.object.Key, session.envelope.Revision+1, session.value)
	if err != nil {
		return checkpointGarbageCollectionSession{}, err
	}
	version, err := e.backend.Put(ctx, session.object.Key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: session.object.Version})
	if err != nil {
		if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrNotFound) {
			return checkpointGarbageCollectionSession{}, errCheckpointGarbageContended
		}
		return checkpointGarbageCollectionSession{}, err
	}
	session.object.Body, session.object.Version, session.object.Size = body, version, int64(len(body))
	session.envelope.Revision++
	return session, nil
}

func (e *Engine) sweepCheckpointGarbage(ctx context.Context, checkpoint storageformat.Checkpoint, session checkpointGarbageCollectionSession, reachable *checkpointVisitSet) error {
	sweeps := e.checkpointGarbageSweeps()
	if err := validateCheckpointGarbageSession(session.value, checkpoint, len(sweeps)); err != nil {
		return err
	}
	for session.value.SweepIndex < len(sweeps) {
		sweep := sweeps[session.value.SweepIndex]
		page, err := sweep.backend.List(ctx, objectstore.ListRequest{Prefix: sweep.prefix, Limit: 256, After: session.value.After})
		if err != nil {
			return err
		}
		garbage := make([]objectstore.ObjectInfo, 0, len(page.Objects))
		for _, info := range page.Objects {
			if !strings.HasPrefix(info.Key.String(), sweep.prefix) || session.value.After != "" && info.Key.String() <= session.value.After {
				return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint garbage listing is not ordered")
			}
			session.value.After = info.Key.String()
			if !sweep.eligible(info.Key) {
				continue
			}
			seen, err := reachable.Seen(sweep.role + "\x00" + info.Key.String())
			if err != nil {
				return err
			}
			if seen {
				continue
			}
			garbage = append(garbage, info)
		}
		// The closed gate cannot be reopened until this session is complete.
		// Revalidate immediately before a destructive page, while avoiding a
		// billed state read for empty prefixes and pages that contain only the
		// immutable checkpoint closure.
		if len(garbage) > 0 {
			if err := e.verifyCheckpointGarbageGate(ctx, checkpoint); err != nil {
				return err
			}
		}
		deleteErrors := make([]error, len(garbage))
		var wait sync.WaitGroup
		for index, info := range garbage {
			index, info := index, info
			wait.Add(1)
			go func() {
				defer wait.Done()
				traced := providerbudget.WithTrace(ctx, providerbudget.Trace{Operation: "checkpoint-garbage", Subsystem: "garbage-collection", ParallelGroup: "checkpoint-garbage-page"})
				if err := sweep.backend.Delete(traced, info.Key, objectstore.DeleteCondition{Version: info.Version}); err != nil && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrPreconditionFailed) {
					deleteErrors[index] = err
				}
			}()
		}
		wait.Wait()
		for _, err := range deleteErrors {
			if err != nil {
				return err
			}
		}
		if page.NextCursor == "" {
			session.value.SweepIndex++
			session.value.After = ""
		} else {
			// Persist only provider-page boundaries that have more work. Final
			// pages and consecutive empty prefixes are safely replayable, so a
			// separate state write for each would add cost without improving
			// recovery or visibility guarantees.
			session, err = e.updateCheckpointGarbageSession(ctx, session)
			if err != nil {
				return err
			}
			if err := e.step(ctx, StepCheckpointGarbageAfterPage); err != nil {
				return err
			}
		}
	}
	session.value.Phase = checkpointGarbageCollectionComplete
	session.value.After = ""
	if _, err := e.updateCheckpointGarbageSession(ctx, session); err != nil {
		return err
	}
	if err := e.step(ctx, StepCheckpointGarbageAfterPage); err != nil {
		return err
	}
	return e.step(ctx, StepCheckpointGarbageAfterComplete)
}
