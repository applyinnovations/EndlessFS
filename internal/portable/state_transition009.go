package portable

import (
	"context"
	"errors"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

var errTransitionPending009 = errors.New("consistency-domain transition pending")

const (
	transitionPlanSchema009     = "transition-plan-v1"
	transitionDecisionSchema009 = "transition-decision-v1"
	transitionLockKey009        = "__transition_lock__"

	StepTransitionAfterPlan                 = "transition:after-plan"
	StepTransitionAfterParticipantPrepared  = "transition:after-participant-prepared"
	StepTransitionBeforeDecision            = "transition:before-decision"
	StepTransitionAfterDecision             = "transition:after-decision"
	StepTransitionAfterParticipantFinalized = "transition:after-participant-finalized"
)

type transitionPlanSnapshot009 struct {
	plan   storageformat.TransitionPlan009
	object objectstore.Object
}

func decodeTransitionPlanObject009(object objectstore.Object) (storageformat.TransitionPlan009, error) {
	var envelope storageformat.Envelope
	var plan storageformat.TransitionPlan009
	if err := storageformat.DecodeEnvelope(object.Body, object.Key, transitionPlanSchema009, &envelope, &plan); err != nil || storageformat.ValidateTransitionPlan009(plan) != nil || storageformat.TransitionPlanKey(plan.TransitionID) != object.Key {
		return storageformat.TransitionPlan009{}, domain.NewError(domain.ErrorInvalid, "invalid transition plan authority")
	}
	return plan, nil
}

func (e *Engine) visitTransitionPlans009(ctx context.Context, visit func(storageformat.TransitionPlan009, objectstore.Object) error) error {
	if visit == nil {
		return domain.NewError(domain.ErrorInvalid, "transition plan visitor is required")
	}
	return visitObjectPages(ctx, e.backend, storageformat.TransitionPrefix()+"plans/", func(info objectstore.ObjectInfo) error {
		object, err := e.backend.Get(ctx, info.Key)
		if err != nil {
			return err
		}
		plan, err := decodeTransitionPlanObject009(object)
		if err != nil {
			return err
		}
		return visit(plan, object)
	})
}

// resolveAllTransitions009 is the checkpoint/maintenance help path. A caller
// may disappear after any durable response; listing immutable plans gives a
// replacement replica enough information to publish the decision and remove
// every participant lock before domains are frozen.
func (e *Engine) resolveAllTransitions009(ctx context.Context) error {
	return e.visitTransitionPlans009(ctx, func(plan storageformat.TransitionPlan009, object objectstore.Object) error {
		if e.clock.Now().UTC().Before(plan.RetainUntil) {
			_, err := e.executeTransition009(ctx, plan)
			return err
		}
		return e.collectExpiredTransition009(ctx, plan, object)
	})
}

func (e *Engine) transitionHasLocks009(ctx context.Context, plan storageformat.TransitionPlan009) (bool, error) {
	store := e.stateDomainStore()
	for _, participant := range plan.Participants {
		reference := transitionReference009(participant)
		snapshot, err := store.loadHead(ctx, reference)
		if errors.Is(err, domain.ErrNotFound) || !snapshot.exists || !snapshot.head.Registered {
			continue
		}
		if err != nil {
			return false, err
		}
		lock, _, found, err := transitionLockAtHead009(ctx, store, reference, snapshot.head)
		if err != nil {
			return false, err
		}
		if found {
			if lock.TransitionID != plan.TransitionID || lock.Fingerprint != plan.Fingerprint {
				return false, domain.NewError(domain.ErrorInvalid, "transition participant is locked by another plan")
			}
			return true, nil
		}
	}
	return false, nil
}

func (e *Engine) collectExpiredTransition009(ctx context.Context, plan storageformat.TransitionPlan009, planObject objectstore.Object) error {
	decision, decided, err := e.readTransitionDecision009(ctx, plan)
	if err != nil {
		return err
	}
	locked, err := e.transitionHasLocks009(ctx, plan)
	if err != nil {
		return err
	}
	if locked && !decided {
		decision, err = e.decideTransition009(ctx, plan, false, domain.NewError(domain.ErrorPreconditionFailed, "expired transition was aborted"))
		if err != nil {
			if winner, found, readErr := e.readTransitionDecision009(ctx, plan); readErr != nil || !found {
				return err
			} else {
				decision, decided, err = winner, true, nil
			}
		} else {
			decided = true
		}
	}
	if locked && decided {
		for _, participant := range plan.Participants {
			if err := e.finalizeTransitionParticipant009(ctx, plan, participant, decision); err != nil {
				return err
			}
		}
	}
	if decided {
		key := storageformat.TransitionDecisionKey(plan.TransitionID)
		object, getErr := e.backend.Get(ctx, key)
		if getErr != nil && !errors.Is(getErr, domain.ErrNotFound) {
			return getErr
		}
		if getErr == nil {
			if deleteErr := e.backend.Delete(ctx, key, objectstore.DeleteCondition{Version: object.Version}); deleteErr != nil && !errors.Is(deleteErr, domain.ErrNotFound) && !errors.Is(deleteErr, domain.ErrPreconditionFailed) {
				return deleteErr
			}
		}
	}
	if deleteErr := e.backend.Delete(ctx, planObject.Key, objectstore.DeleteCondition{Version: planObject.Version}); deleteErr != nil && !errors.Is(deleteErr, domain.ErrNotFound) && !errors.Is(deleteErr, domain.ErrPreconditionFailed) {
		return deleteErr
	}
	return nil
}

func transitionReference009(participant storageformat.TransitionParticipant009) consistencyDomainRef {
	return consistencyDomainRef{Kind: participant.Kind, ID: participant.DomainID}
}

func transitionParticipantChanges009(participant storageformat.TransitionParticipant009) []consistencyDomainChange {
	changes := make([]consistencyDomainChange, len(participant.Changes))
	for index, change := range participant.Changes {
		changes[index] = consistencyDomainChange{
			Key: change.Key, Require: domainValueRequirement(change.Requirement), ExpectedVersion: change.ExpectedVersion,
			Delete: change.Delete, Value: append([]byte(nil), change.Value...),
		}
	}
	return changes
}

func (e *Engine) transitionPlan009(normalized state.Mutation, fingerprint string) (storageformat.TransitionPlan009, error) {
	byReference := make(map[consistencyDomainRef][]storageformat.TransitionChange009)
	for _, change := range normalized.Changes {
		reference, err := stateDomainReferenceForKey(change.Key)
		if err != nil {
			return storageformat.TransitionPlan009{}, err
		}
		byReference[reference] = append(byReference[reference], storageformat.TransitionChange009{
			Key: change.Key.String(), Requirement: uint8(change.Requirement), ExpectedVersion: string(change.ExpectedVersion),
			Delete: change.Delete, Value: append([]byte(nil), change.Data...),
		})
	}
	if len(byReference) < 2 {
		return storageformat.TransitionPlan009{}, domain.NewError(domain.ErrorInvalid, "transition does not span consistency domains")
	}
	plan := storageformat.TransitionPlan009{
		SchemaVersion: 1, TransitionID: normalized.ID, Fingerprint: storageformat.Digest([]byte(fingerprint)),
		RetainUntil: normalized.RetainUntil, Result: append([]byte(nil), normalized.Result...),
		Participants: make([]storageformat.TransitionParticipant009, 0, len(byReference)),
	}
	if plan.RetainUntil.IsZero() {
		plan.RetainUntil = e.clock.Now().UTC().Add(terminalOperationRetention)
	}
	for reference, changes := range byReference {
		sort.Slice(changes, func(left, right int) bool { return changes[left].Key < changes[right].Key })
		plan.Participants = append(plan.Participants, storageformat.TransitionParticipant009{Kind: reference.Kind, DomainID: reference.ID, Changes: changes})
	}
	storageformat.SortTransitionParticipants009(plan.Participants)
	if err := storageformat.ValidateTransitionPlan009(plan); err != nil {
		return storageformat.TransitionPlan009{}, err
	}
	return plan, nil
}

func (e *Engine) createOrReadTransitionPlan009(ctx context.Context, plan storageformat.TransitionPlan009) (transitionPlanSnapshot009, bool, error) {
	key := storageformat.TransitionPlanKey(plan.TransitionID)
	body, err := storageformat.EncodeEnvelope(transitionPlanSchema009, key, 1, plan)
	if err != nil {
		return transitionPlanSnapshot009{}, false, err
	}
	version, err := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
	if err == nil {
		return transitionPlanSnapshot009{plan: plan, object: objectstore.Object{Key: key, Body: body, Version: version, Size: int64(len(body))}}, true, nil
	}
	if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
		return transitionPlanSnapshot009{}, false, err
	}
	object, err := e.backend.Get(ctx, key)
	if err != nil {
		return transitionPlanSnapshot009{}, false, err
	}
	var envelope storageformat.Envelope
	var existing storageformat.TransitionPlan009
	if err := storageformat.DecodeEnvelope(object.Body, key, transitionPlanSchema009, &envelope, &existing); err != nil || storageformat.ValidateTransitionPlan009(existing) != nil || existing.TransitionID != plan.TransitionID {
		return transitionPlanSnapshot009{}, false, domain.NewError(domain.ErrorInvalid, "invalid transition plan authority")
	}
	if existing.Fingerprint != plan.Fingerprint {
		return transitionPlanSnapshot009{}, false, domain.NewError(domain.ErrorConflict, "transition ID was reused with another intent")
	}
	return transitionPlanSnapshot009{plan: existing, object: object}, false, nil
}

func (e *Engine) readTransitionPlan009(ctx context.Context, transitionID string) (transitionPlanSnapshot009, error) {
	key := storageformat.TransitionPlanKey(transitionID)
	object, err := e.backend.Get(ctx, key)
	if err != nil {
		return transitionPlanSnapshot009{}, err
	}
	plan, err := decodeTransitionPlanObject009(object)
	if err != nil || plan.TransitionID != transitionID {
		return transitionPlanSnapshot009{}, domain.NewError(domain.ErrorInvalid, "invalid transition plan authority")
	}
	return transitionPlanSnapshot009{plan: plan, object: object}, nil
}

func (e *Engine) readTransitionDecision009(ctx context.Context, plan storageformat.TransitionPlan009) (storageformat.TransitionDecision009, bool, error) {
	key := storageformat.TransitionDecisionKey(plan.TransitionID)
	object, err := e.backend.Get(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		return storageformat.TransitionDecision009{}, false, nil
	}
	if err != nil {
		return storageformat.TransitionDecision009{}, false, err
	}
	var envelope storageformat.Envelope
	var decision storageformat.TransitionDecision009
	if err := storageformat.DecodeEnvelope(object.Body, key, transitionDecisionSchema009, &envelope, &decision); err != nil || storageformat.ValidateTransitionDecision009(decision) != nil || decision.TransitionID != plan.TransitionID || decision.Fingerprint != plan.Fingerprint {
		return storageformat.TransitionDecision009{}, false, domain.NewError(domain.ErrorInvalid, "invalid transition decision authority")
	}
	return decision, true, nil
}

func (e *Engine) decideTransition009(ctx context.Context, plan storageformat.TransitionPlan009, committed bool, failure error) (storageformat.TransitionDecision009, error) {
	decision := storageformat.TransitionDecision009{SchemaVersion: 1, TransitionID: plan.TransitionID, Fingerprint: plan.Fingerprint, Committed: committed, DecidedAt: e.clock.Now().UTC()}
	if !committed {
		decision.ErrorKind = string(domain.KindOf(failure))
	}
	if err := storageformat.ValidateTransitionDecision009(decision); err != nil {
		return storageformat.TransitionDecision009{}, err
	}
	key := storageformat.TransitionDecisionKey(plan.TransitionID)
	body, err := storageformat.EncodeEnvelope(transitionDecisionSchema009, key, 1, decision)
	if err != nil {
		return storageformat.TransitionDecision009{}, err
	}
	if err := e.step(ctx, StepTransitionBeforeDecision); err != nil {
		return storageformat.TransitionDecision009{}, err
	}
	_, putErr := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
	if putErr == nil {
		if stepErr := e.step(ctx, StepTransitionAfterDecision); stepErr != nil {
			putErr = stepErr
		}
	}
	if putErr != nil && !errors.Is(putErr, domain.ErrConflict) && !errors.Is(putErr, domain.ErrPreconditionFailed) {
		// A provider response or process boundary can be lost after the create.
		// The immutable decision itself, not the transport result, is authority.
		if recovered, found, readErr := e.readTransitionDecision009(ctx, plan); readErr == nil && found {
			return recovered, nil
		}
		return storageformat.TransitionDecision009{}, putErr
	}
	existing, found, err := e.readTransitionDecision009(ctx, plan)
	if err != nil || !found {
		if err == nil {
			err = domain.NewError(domain.ErrorInvalid, "transition decision disappeared")
		}
		return storageformat.TransitionDecision009{}, err
	}
	if existing.Committed != decision.Committed || existing.ErrorKind != decision.ErrorKind {
		return storageformat.TransitionDecision009{}, domain.NewError(domain.ErrorInvalid, "conflicting transition decisions")
	}
	return existing, nil
}

func transitionLockAtHead009(ctx context.Context, store *consistencyDomainStore, reference consistencyDomainRef, head storageformat.DomainHead) (storageformat.TransitionLock009, consistencyDomainValue, bool, error) {
	value, found, err := store.lookupAtHead(ctx, reference, head, transitionLockKey009)
	if err != nil || !found {
		return storageformat.TransitionLock009{}, consistencyDomainValue{}, found, err
	}
	var lock storageformat.TransitionLock009
	if err := decodeCanonicalValue(value.Data, &lock); err != nil || storageformat.ValidateTransitionLock009(lock) != nil || lock.Kind != reference.Kind || lock.DomainID != reference.ID {
		return storageformat.TransitionLock009{}, consistencyDomainValue{}, false, domain.NewError(domain.ErrorInvalid, "invalid consistency-domain transition lock")
	}
	return lock, value, true, nil
}

func (e *Engine) prepareTransitionParticipant009(ctx context.Context, plan storageformat.TransitionPlan009, participant storageformat.TransitionParticipant009) error {
	reference := transitionReference009(participant)
	store := e.stateDomainStore()
	for attempts := 0; attempts < 32; attempts++ {
		snapshot, err := store.loadHead(ctx, reference)
		if err != nil {
			return err
		}
		if snapshot.exists && snapshot.head.Registered {
			if snapshot.head.Frozen {
				return domain.NewError(domain.ErrorPreconditionFailed, "consistency domain is frozen")
			}
			lock, _, found, err := transitionLockAtHead009(ctx, store, reference, snapshot.head)
			if err != nil {
				return err
			}
			if found {
				if lock.TransitionID == plan.TransitionID && lock.Fingerprint == plan.Fingerprint {
					return nil
				}
				if err := e.helpTransition009(ctx, lock.TransitionID); err != nil {
					return err
				}
				continue
			}
		}
		head := snapshot.head
		if !snapshot.exists || !head.Registered {
			head = storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true}
		}
		if err := store.validateMutationAtHead(ctx, reference, head, transitionParticipantChanges009(participant)); err != nil {
			return err
		}
		lock := storageformat.TransitionLock009{SchemaVersion: 1, TransitionID: plan.TransitionID, Fingerprint: plan.Fingerprint, Kind: reference.Kind, DomainID: reference.ID}
		lockBody, err := storageformat.EncodeCanonical(lock)
		if err != nil {
			return err
		}
		_, err = store.mutateAtHead(ctx, reference, consistencyDomainMutation{
			ID: "transition-prepare:" + storageformat.Digest([]byte(plan.TransitionID)), TransitionID: plan.TransitionID, RetainUntil: plan.RetainUntil,
			Changes: []consistencyDomainChange{{Key: transitionLockKey009, Require: domainValueAbsent, Value: lockBody}},
		}, &snapshot)
		if err == nil {
			return e.step(ctx, StepTransitionAfterParticipantPrepared)
		}
		if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
	}
	return domain.NewError(domain.ErrorUnavailable, "transition preparation remained contended")
}

func (e *Engine) finalizeTransitionParticipant009(ctx context.Context, plan storageformat.TransitionPlan009, participant storageformat.TransitionParticipant009, decision storageformat.TransitionDecision009) error {
	reference := transitionReference009(participant)
	store := e.stateDomainStore()
	for attempts := 0; attempts < 32; attempts++ {
		snapshot, err := store.loadHead(ctx, reference)
		if err != nil {
			return err
		}
		if !snapshot.exists || !snapshot.head.Registered {
			if decision.Committed {
				return domain.NewError(domain.ErrorInvalid, "committed transition participant is missing")
			}
			return nil
		}
		if decision.Committed {
			if outcome, found, err := store.lookupOutcomeAtHead(ctx, reference, snapshot.head, plan.TransitionID); err != nil {
				return err
			} else if found {
				if string(outcome.Result) != string(plan.Result) {
					return domain.NewError(domain.ErrorInvalid, "transition outcome result is misbound")
				}
				return nil
			}
		}
		lock, lockValue, found, err := transitionLockAtHead009(ctx, store, reference, snapshot.head)
		if err != nil {
			return err
		}
		if !found {
			if decision.Committed {
				if !e.clock.Now().UTC().Before(plan.RetainUntil) {
					return nil
				}
				return domain.NewError(domain.ErrorInvalid, "committed transition lock is missing")
			}
			return nil
		}
		if lock.TransitionID != plan.TransitionID || lock.Fingerprint != plan.Fingerprint {
			return domain.NewError(domain.ErrorInvalid, "transition lock does not bind its decision")
		}
		changes := []consistencyDomainChange{{Key: transitionLockKey009, Require: domainValuePresent, ExpectedVersion: lockValue.LogicalVersion, Delete: true}}
		mutationID := "transition-abort:" + storageformat.Digest([]byte(plan.TransitionID))
		result := []byte(nil)
		if decision.Committed {
			changes = append(changes, transitionParticipantChanges009(participant)...)
			mutationID, result = plan.TransitionID, append([]byte(nil), plan.Result...)
		}
		_, err = store.mutateAtHead(ctx, reference, consistencyDomainMutation{ID: mutationID, TransitionID: plan.TransitionID, RetainUntil: plan.RetainUntil, Changes: changes, Result: result}, &snapshot)
		if err == nil {
			return e.step(ctx, StepTransitionAfterParticipantFinalized)
		}
		if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
	}
	return domain.NewError(domain.ErrorUnavailable, "transition finalization remained contended")
}

func deterministicTransitionFailure009(err error) bool {
	kind := domain.KindOf(err)
	return kind == domain.ErrorConflict || kind == domain.ErrorNotFound || kind == domain.ErrorPreconditionFailed
}

func (e *Engine) executeTransition009(ctx context.Context, plan storageformat.TransitionPlan009) (storageformat.TransitionDecision009, error) {
	if decision, found, err := e.readTransitionDecision009(ctx, plan); err != nil {
		return storageformat.TransitionDecision009{}, err
	} else if found {
		for _, participant := range plan.Participants {
			if err := e.finalizeTransitionParticipant009(ctx, plan, participant, decision); err != nil {
				return storageformat.TransitionDecision009{}, err
			}
		}
		return decision, nil
	}
	var preparationFailure error
	for _, participant := range plan.Participants {
		if err := e.prepareTransitionParticipant009(ctx, plan, participant); err != nil {
			if !deterministicTransitionFailure009(err) {
				return storageformat.TransitionDecision009{}, err
			}
			preparationFailure = err
			break
		}
	}
	decision, err := e.decideTransition009(ctx, plan, preparationFailure == nil, preparationFailure)
	if err != nil {
		return storageformat.TransitionDecision009{}, err
	}
	for _, participant := range plan.Participants {
		if err := e.finalizeTransitionParticipant009(ctx, plan, participant, decision); err != nil {
			return storageformat.TransitionDecision009{}, err
		}
	}
	return decision, nil
}

func (e *Engine) helpTransition009(ctx context.Context, transitionID string) error {
	snapshot, err := e.readTransitionPlan009(ctx, transitionID)
	if err != nil {
		return err
	}
	_, err = e.executeTransition009(ctx, snapshot.plan)
	return err
}

func (e *Engine) resolveStateTransition009(ctx context.Context, reference consistencyDomainRef) error {
	store := e.stateDomainStore()
	for attempts := 0; attempts < 32; attempts++ {
		snapshot, err := store.loadHead(ctx, reference)
		if err != nil || !snapshot.exists || !snapshot.head.Registered {
			return err
		}
		lock, _, found, err := transitionLockAtHead009(ctx, store, reference, snapshot.head)
		if err != nil || !found {
			return err
		}
		if err := e.helpTransition009(ctx, lock.TransitionID); err != nil {
			return err
		}
	}
	return domain.NewError(domain.ErrorUnavailable, "consistency-domain transition remained unresolved")
}

func (e *Engine) transitionOutcome009(normalized state.Mutation, plan storageformat.TransitionPlan009, replayed bool) (state.MutationOutcome, error) {
	versions := make(map[string]state.Version, len(normalized.Changes))
	for _, participant := range plan.Participants {
		changes := append(transitionParticipantChanges009(participant), consistencyDomainChange{Key: transitionLockKey009, Require: domainValuePresent, Delete: true})
		canonical, fingerprint, err := normalizeConsistencyDomainMutation(consistencyDomainMutation{ID: plan.TransitionID, RetainUntil: plan.RetainUntil, Changes: changes, Result: plan.Result})
		if err != nil {
			return state.MutationOutcome{}, err
		}
		for _, change := range canonical.Changes {
			if change.Key != transitionLockKey009 && !change.Delete {
				versions[change.Key] = state.Version(consistencyDomainLogicalVersion(plan.TransitionID, fingerprint, change))
			}
		}
	}
	outcome := state.MutationOutcome{ID: normalized.ID, Result: append([]byte(nil), plan.Result...), Replayed: replayed, Changes: make([]state.ChangeResult, len(normalized.Changes))}
	for index, change := range normalized.Changes {
		outcome.Changes[index] = state.ChangeResult{Key: change.Key, Version: versions[change.Key.String()]}
	}
	return outcome, nil
}

// Transact implements a helpable multi-domain transition. Immutable plan and
// decision objects are portable; one create-only decision is the global
// linearization point, and every participant lock is finalized by any replica.
func (e *Engine) Transact(ctx context.Context, mutation state.Mutation) (state.MutationOutcome, error) {
	normalized, fingerprint, err := state.NormalizeMutation(mutation)
	if err != nil {
		return state.MutationOutcome{}, err
	}
	references := make(map[consistencyDomainRef]struct{})
	for _, change := range normalized.Changes {
		if change.Delete {
			err = validateStateKey(change.Key)
		} else {
			err = validateStateMutation(change.Key, change.Data)
		}
		if err != nil {
			return state.MutationOutcome{}, err
		}
		reference, routeErr := stateDomainReferenceForKey(change.Key)
		if routeErr != nil {
			return state.MutationOutcome{}, routeErr
		}
		references[reference] = struct{}{}
	}
	if len(references) == 1 {
		return e.Mutate(ctx, normalized)
	}
	plan, err := e.transitionPlan009(normalized, fingerprint)
	if err != nil {
		return state.MutationOutcome{}, err
	}
	_, created, err := e.createOrReadTransitionPlan009(ctx, plan)
	if err != nil {
		return state.MutationOutcome{}, err
	}
	if created {
		if err := e.step(ctx, StepTransitionAfterPlan); err != nil {
			return state.MutationOutcome{}, err
		}
	}
	decision, err := e.executeTransition009(ctx, plan)
	if err != nil {
		return state.MutationOutcome{}, err
	}
	if !decision.Committed {
		return state.MutationOutcome{}, domain.NewError(domain.ErrorKind(decision.ErrorKind), "cross-domain transition precondition failed")
	}
	return e.transitionOutcome009(normalized, plan, !created)
}

var _ state.TransactionalStore = (*Engine)(nil)
