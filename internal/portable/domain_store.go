package portable

import (
	"context"
	"encoding/base64"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const domainHeadSchema = "consistency-domain-head-v1"

// The architecture sensitivity fixture compared windows 1, 8, 32, and 128
// across 256 sequential mutations. Thirty-two is the measured knee: within
// three percent of the lowest request cost while keeping the steady head about
// four times smaller than 128 (25,970 versus 102,132 bytes). The bound is an
// implementation policy, not part of the canonical record grammar.
const consistencyDomainDeltaWindow = 32

type consistencyDomainRef struct {
	Kind storageformat.ConsistencyDomainKind
	ID   string
}

type domainValueRequirement uint8

const (
	domainValueAny domainValueRequirement = iota + 1
	domainValueAbsent
	domainValuePresent
)

type consistencyDomainChange struct {
	Key             string                 `json:"key"`
	Require         domainValueRequirement `json:"require"`
	ExpectedVersion string                 `json:"expectedVersion,omitempty"`
	Delete          bool                   `json:"delete,omitempty"`
	Value           []byte                 `json:"value,omitempty"`
}

type consistencyDomainMutation struct {
	ID           string
	TransitionID string
	RetainUntil  time.Time
	Changes      []consistencyDomainChange
	Result       []byte
}

type consistencyDomainOutcome struct {
	MutationID  string
	Fingerprint string
	Revision    uint64
	RetainUntil time.Time
	Result      []byte
	Replayed    bool
}

type consistencyDomainValue struct {
	Data           []byte
	LogicalVersion string
	Revision       uint64
}

type consistencyDomainHeadSnapshot struct {
	head     storageformat.DomainHead
	envelope storageformat.Envelope
	object   objectstore.Object
	exists   bool
}

type consistencyDomainStore struct {
	backend   objectstore.Backend
	scheduler Scheduler
	clock     domain.Clock
}

func newConsistencyDomainStore(backend objectstore.Backend, scheduler Scheduler, clocks ...domain.Clock) *consistencyDomainStore {
	var clock domain.Clock = domain.SystemClock{}
	if len(clocks) > 0 && clocks[0] != nil {
		clock = clocks[0]
	}
	return &consistencyDomainStore{backend: backend, scheduler: scheduler, clock: clock}
}

func (store *consistencyDomainStore) step(ctx context.Context, step string) error {
	if store.scheduler == nil {
		return nil
	}
	return store.scheduler.Step(ctx, step)
}

func validateConsistencyDomainRef(reference consistencyDomainRef) error {
	if reference.ID == "" {
		return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain reference")
	}
	switch reference.Kind {
	case storageformat.DomainNamespace, storageformat.DomainOwnerControl, storageformat.DomainAdmin, storageformat.DomainCapability, storageformat.DomainShare, storageformat.DomainIdentity, storageformat.DomainOwnerJobs:
		return nil
	default:
		return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain reference")
	}
}

func (store *consistencyDomainStore) loadHead(ctx context.Context, reference consistencyDomainRef) (consistencyDomainHeadSnapshot, error) {
	if store == nil || store.backend == nil {
		return consistencyDomainHeadSnapshot{}, domain.NewError(domain.ErrorInvalid, "invalid consistency-domain store")
	}
	if err := validateConsistencyDomainRef(reference); err != nil {
		return consistencyDomainHeadSnapshot{}, err
	}
	key := storageformat.DomainHeadKey(reference.Kind, reference.ID)
	object, err := store.backend.Get(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		return consistencyDomainHeadSnapshot{}, nil
	}
	if err != nil {
		return consistencyDomainHeadSnapshot{}, err
	}
	var envelope storageformat.Envelope
	var head storageformat.DomainHead
	if err := storageformat.DecodeEnvelope(object.Body, key, domainHeadSchema, &envelope, &head); err != nil {
		return consistencyDomainHeadSnapshot{}, err
	}
	if head.DomainID != reference.ID || head.Kind != reference.Kind {
		return consistencyDomainHeadSnapshot{}, domain.NewError(domain.ErrorInvalid, "consistency-domain head key binding mismatch")
	}
	if head.Registered {
		if err := storageformat.ValidateDomainHead(head); err != nil {
			return consistencyDomainHeadSnapshot{}, err
		}
	} else if err := storageformat.ValidateInitialDomainHead(head); err != nil {
		return consistencyDomainHeadSnapshot{}, err
	}
	return consistencyDomainHeadSnapshot{head: head, envelope: envelope, object: object, exists: true}, nil
}

func normalizeConsistencyDomainMutation(mutation consistencyDomainMutation) (consistencyDomainMutation, string, error) {
	if mutation.ID == "" || len(mutation.Changes) == 0 {
		return consistencyDomainMutation{}, "", domain.NewError(domain.ErrorInvalid, "invalid consistency-domain mutation")
	}
	changes := append([]consistencyDomainChange(nil), mutation.Changes...)
	sort.Slice(changes, func(left, right int) bool { return changes[left].Key < changes[right].Key })
	previous := ""
	for _, change := range changes {
		if change.Key == "" || change.Key == previous || change.Require < domainValueAny || change.Require > domainValuePresent || change.Require != domainValuePresent && change.ExpectedVersion != "" || change.Delete && len(change.Value) != 0 {
			return consistencyDomainMutation{}, "", domain.NewError(domain.ErrorInvalid, "invalid consistency-domain change")
		}
		previous = change.Key
	}
	intent := struct {
		Changes []consistencyDomainChange `json:"changes"`
		Result  []byte                    `json:"result,omitempty"`
	}{Changes: changes, Result: append([]byte(nil), mutation.Result...)}
	body, err := storageformat.EncodeCanonical(intent)
	if err != nil {
		return consistencyDomainMutation{}, "", err
	}
	fingerprint := storageformat.Digest(append([]byte("endlessfs-consistency-domain-mutation-v2\x00"), body...))
	probe := storageformat.DomainDelta{MutationID: mutation.ID, Fingerprint: fingerprint, Revision: 1, RetainUntil: mutation.RetainUntil.UTC(), Result: append([]byte(nil), mutation.Result...), Changes: make([]storageformat.DomainChange, len(changes))}
	for index, change := range changes {
		probe.Changes[index] = storageformat.DomainChange{Key: change.Key, Delete: change.Delete, Value: append([]byte(nil), change.Value...), LogicalVersion: consistencyDomainLogicalVersion(mutation.ID, fingerprint, change)}
	}
	if _, err := storageformat.EncodeCanonical(probe); err != nil {
		return consistencyDomainMutation{}, "", err
	}
	mutation.Changes = changes
	mutation.Result = append([]byte(nil), mutation.Result...)
	return mutation, fingerprint, nil
}

func consistencyDomainLogicalVersion(mutationID, fingerprint string, change consistencyDomainChange) string {
	if change.Delete {
		return ""
	}
	body, _ := storageformat.EncodeCanonical(struct {
		MutationID  string `json:"mutationID"`
		Fingerprint string `json:"fingerprint"`
		Key         string `json:"key"`
		Value       []byte `json:"value"`
	}{mutationID, fingerprint, change.Key, change.Value})
	return storageformat.Digest(append([]byte("endlessfs-consistency-domain-value-v1\x00"), body...))
}

func (store *consistencyDomainStore) mutate(ctx context.Context, reference consistencyDomainRef, mutation consistencyDomainMutation) (consistencyDomainOutcome, error) {
	return store.mutateAtHead(ctx, reference, mutation, nil)
}

// mutateAtHead conditionally publishes against a head the caller already
// authenticated. Namespace mutations use it after resolving their immutable
// path pages so the visibility commit does not pay for, or race through, a
// redundant second head read. A lost head race is retried only after the
// winning head revalidates every key precondition; changed preconditions are
// returned for the caller to recompute from the winning state.
func (store *consistencyDomainStore) mutateAtHead(ctx context.Context, reference consistencyDomainRef, mutation consistencyDomainMutation, initial *consistencyDomainHeadSnapshot) (consistencyDomainOutcome, error) {
	return store.mutatePrepared(ctx, reference, mutation, initial, nil)
}

func (store *consistencyDomainStore) mutatePrepared(ctx context.Context, reference consistencyDomainRef, mutation consistencyDomainMutation, initial *consistencyDomainHeadSnapshot, preparedSession *consistencyDomainTreeSession) (consistencyDomainOutcome, error) {
	if err := validateConsistencyDomainRef(reference); err != nil {
		return consistencyDomainOutcome{}, err
	}
	if mutation.RetainUntil.IsZero() {
		mutation.RetainUntil = store.clock.Now().UTC().Add(terminalOperationRetention)
	} else {
		mutation.RetainUntil = mutation.RetainUntil.UTC()
	}
	mutation, fingerprint, err := normalizeConsistencyDomainMutation(mutation)
	if err != nil {
		return consistencyDomainOutcome{}, err
	}
	var preflight consistencyDomainHeadSnapshot
	if initial == nil {
		preflight, err = store.loadHead(ctx, reference)
		if err != nil {
			return consistencyDomainOutcome{}, err
		}
	} else {
		preflight = *initial
		if preflight.exists && (preflight.head.DomainID != reference.ID || preflight.head.Kind != reference.Kind) {
			return consistencyDomainOutcome{}, domain.NewError(domain.ErrorInvalid, "misbound prepared consistency-domain head")
		}
	}
	for preflight.exists && preflight.head.Registered && len(preflight.head.Deltas) >= consistencyDomainDeltaWindow {
		if err := store.compactSnapshot(ctx, reference, preflight); err != nil && !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return consistencyDomainOutcome{}, err
		}
		preflight, err = store.loadHead(ctx, reference)
		if err != nil {
			return consistencyDomainOutcome{}, err
		}
	}
	var firstSnapshot *consistencyDomainHeadSnapshot
	if !preflight.exists || !preflight.head.Registered {
		empty := storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true}
		if err := store.validateMutationAtHeadWithSession(ctx, reference, empty, mutation.Changes, preparedSession); err != nil {
			return consistencyDomainOutcome{}, err
		}
		registered, err := store.prepareRegistration(ctx, reference, preflight)
		if err != nil {
			return consistencyDomainOutcome{}, err
		}
		firstSnapshot = &registered
	} else {
		firstSnapshot = &preflight
	}
	for {
		var snapshot consistencyDomainHeadSnapshot
		if firstSnapshot != nil {
			snapshot = *firstSnapshot
			firstSnapshot = nil
		} else {
			snapshot, err = store.loadHead(ctx, reference)
			if err != nil {
				return consistencyDomainOutcome{}, err
			}
		}
		if outcome, found, err := store.lookupOutcomeAtHeadWithSession(ctx, reference, snapshot.head, mutation.ID, preparedSession); err != nil {
			return consistencyDomainOutcome{}, err
		} else if found {
			if outcome.Fingerprint != fingerprint {
				return consistencyDomainOutcome{}, domain.NewError(domain.ErrorConflict, "consistency-domain idempotency key was reused")
			}
			outcome.Replayed = true
			return outcome, nil
		}
		if snapshot.head.Frozen {
			return consistencyDomainOutcome{}, domain.NewError(domain.ErrorUnavailable, "consistency domain is frozen")
		}
		if lock, _, found, lockErr := transitionLockAtHead009(ctx, store, reference, snapshot.head); lockErr != nil {
			return consistencyDomainOutcome{}, lockErr
		} else if found && mutation.TransitionID != lock.TransitionID {
			return consistencyDomainOutcome{}, domain.WrapError(domain.ErrorUnavailable, "consistency domain has a pending transition", errTransitionPending009)
		}
		if err := store.validateMutationAtHeadWithSession(ctx, reference, snapshot.head, mutation.Changes, preparedSession); err != nil {
			return consistencyDomainOutcome{}, err
		}

		revision := snapshot.head.Revision + 1
		delta := storageformat.DomainDelta{MutationID: mutation.ID, Fingerprint: fingerprint, Revision: revision, RetainUntil: mutation.RetainUntil, Result: append([]byte(nil), mutation.Result...), Changes: make([]storageformat.DomainChange, len(mutation.Changes))}
		for index, change := range mutation.Changes {
			delta.Changes[index] = storageformat.DomainChange{Key: change.Key, Delete: change.Delete, Value: append([]byte(nil), change.Value...), LogicalVersion: consistencyDomainLogicalVersion(mutation.ID, fingerprint, change)}
		}
		next := snapshot.head
		next.Registered = true
		next.Revision = revision
		next.Deltas = append(append([]storageformat.DomainDelta(nil), snapshot.head.Deltas...), delta)
		headKey := storageformat.DomainHeadKey(reference.Kind, reference.ID)
		headBody, encodeErr := store.encodeHead(headKey, snapshot, next)
		if encodeErr != nil {
			// A bounded delta window is a liveness mechanism, not an eventual
			// write-denial threshold. Fold the exact current window and retry.
			if len(snapshot.head.Deltas) == 0 {
				return consistencyDomainOutcome{}, encodeErr
			}
			if compactErr := store.compact(ctx, reference); compactErr != nil && !errors.Is(compactErr, domain.ErrConflict) && !errors.Is(compactErr, domain.ErrPreconditionFailed) {
				return consistencyDomainOutcome{}, compactErr
			}
			continue
		}
		condition := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
		if snapshot.exists {
			condition = objectstore.PutCondition{Mode: objectstore.PutMatch, Version: snapshot.object.Version}
		}
		if err := store.step(ctx, StepDomainBeforeHeadCommit); err != nil {
			return consistencyDomainOutcome{}, err
		}
		_, putErr := store.backend.Put(ctx, headKey, headBody, condition)
		if putErr == nil {
			if err := store.step(ctx, StepDomainAfterHeadCommit); err != nil {
				putErr = err
			}
		}
		if putErr != nil {
			// Conditional responses can be lost. The canonical head, rather
			// than the transport result, decides whether this exact intent won.
			recovered, loadErr := store.loadHead(ctx, reference)
			if loadErr == nil {
				if outcome, found, outcomeErr := store.lookupOutcomeAtHead(ctx, reference, recovered.head, mutation.ID); outcomeErr != nil {
					return consistencyDomainOutcome{}, outcomeErr
				} else if found {
					if outcome.Fingerprint != fingerprint {
						return consistencyDomainOutcome{}, domain.NewError(domain.ErrorConflict, "consistency-domain idempotency key was reused")
					}
					outcome.Replayed = true
					return outcome, nil
				}
				// Translate a lost head race through the canonical preconditions.
				// Callers receive the portable semantic conflict instead of a raw
				// provider-generation failure.
				if validationErr := store.validateMutationAtHead(ctx, reference, recovered.head, mutation.Changes); validationErr != nil {
					return consistencyDomainOutcome{}, validationErr
				}
				// A different mutation may have advanced this shared domain head
				// without touching any of this intent's keys. Canonical
				// revalidation above proves that retrying the same mutation is
				// safe; do so against the winning head instead of leaking a native
				// CAS race through the application API. True same-key races fail
				// revalidation and return their portable semantic error above.
				advanced := recovered.exists != snapshot.exists || recovered.object.Version != snapshot.object.Version
				if advanced && !recovered.head.Frozen && (errors.Is(putErr, domain.ErrConflict) || errors.Is(putErr, domain.ErrPreconditionFailed)) {
					firstSnapshot = &recovered
					continue
				}
			}
			return consistencyDomainOutcome{}, putErr
		}
		return consistencyDomainOutcome{MutationID: mutation.ID, Fingerprint: fingerprint, Revision: revision, RetainUntil: mutation.RetainUntil, Result: append([]byte(nil), mutation.Result...)}, nil
	}
}

// prepareRegistration creates an inert head and registers its deterministic
// key in the catalog. The caller may then activate it alone or atomically with
// the first mutation. A catalog freeze either excludes the inert head or names
// it before any application value can become visible.
func (store *consistencyDomainStore) prepareRegistration(ctx context.Context, reference consistencyDomainRef, initial ...consistencyDomainHeadSnapshot) (consistencyDomainHeadSnapshot, error) {
	var supplied *consistencyDomainHeadSnapshot
	if len(initial) > 0 {
		snapshot := initial[0]
		supplied = &snapshot
	}
	for {
		var snapshot consistencyDomainHeadSnapshot
		var err error
		if supplied != nil {
			snapshot, supplied = *supplied, nil
		} else {
			snapshot, err = store.loadHead(ctx, reference)
			if err != nil {
				return consistencyDomainHeadSnapshot{}, err
			}
		}
		if snapshot.exists && snapshot.head.Registered {
			return snapshot, nil
		}
		if !snapshot.exists {
			initial := storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind}
			if err := storageformat.ValidateInitialDomainHead(initial); err != nil {
				return consistencyDomainHeadSnapshot{}, err
			}
			key := storageformat.DomainHeadKey(reference.Kind, reference.ID)
			body, err := storageformat.EncodeEnvelope(domainHeadSchema, key, 1, initial)
			if err != nil {
				return consistencyDomainHeadSnapshot{}, err
			}
			version, err := store.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
			if err != nil {
				if errors.Is(err, domain.ErrConflict) {
					continue
				}
				return consistencyDomainHeadSnapshot{}, err
			}
			var envelope storageformat.Envelope
			var decoded storageformat.DomainHead
			if err := storageformat.DecodeEnvelope(body, key, domainHeadSchema, &envelope, &decoded); err != nil {
				return consistencyDomainHeadSnapshot{}, err
			}
			snapshot = consistencyDomainHeadSnapshot{head: decoded, envelope: envelope, object: objectstore.Object{Key: key, Body: body, Version: version}, exists: true}
		}
		if err := newDomainCatalog(store.backend, store.scheduler).register(ctx, reference); err != nil {
			return consistencyDomainHeadSnapshot{}, err
		}
		return snapshot, nil
	}
}

func (store *consistencyDomainStore) ensureRegistered(ctx context.Context, reference consistencyDomainRef, initial ...consistencyDomainHeadSnapshot) error {
	for {
		snapshot, err := store.prepareRegistration(ctx, reference, initial...)
		initial = nil
		if err != nil {
			return err
		}
		if snapshot.head.Registered {
			return nil
		}
		next := snapshot.head
		next.Registered = true
		key := storageformat.DomainHeadKey(reference.Kind, reference.ID)
		body, err := store.encodeHead(key, snapshot, next)
		if err != nil {
			return err
		}
		if _, err := store.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: snapshot.object.Version}); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
	}
}

func (store *consistencyDomainStore) writeHeadSnapshot(ctx context.Context, reference consistencyDomainRef, head storageformat.DomainHead, expiresAt time.Time) (string, error) {
	snapshot := storageformat.DomainSnapshot{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Head: head, ExpiresAt: expiresAt.UTC()}
	if err := storageformat.ValidateDomainSnapshot(snapshot); err != nil {
		return "", err
	}
	body, err := storageformat.EncodeCanonical(snapshot)
	if err != nil {
		return "", err
	}
	digest := storageformat.Digest(body)
	_, err = store.backend.Put(ctx, storageformat.DomainSnapshotKey(reference.Kind, reference.ID, digest), body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
	if err != nil && !errors.Is(err, domain.ErrConflict) {
		return "", err
	}
	return digest, nil
}

func (store *consistencyDomainStore) loadHeadSnapshot(ctx context.Context, reference consistencyDomainRef, digest string) (storageformat.DomainHead, error) {
	if digest == "" {
		return storageformat.DomainHead{}, domain.NewError(domain.ErrorInvalid, "invalid consistency-domain snapshot")
	}
	key := storageformat.DomainSnapshotKey(reference.Kind, reference.ID, digest)
	object, err := store.backend.Get(ctx, key)
	if err != nil {
		return storageformat.DomainHead{}, err
	}
	if storageformat.Digest(object.Body) != digest {
		return storageformat.DomainHead{}, domain.NewError(domain.ErrorInvalid, "consistency-domain snapshot digest mismatch")
	}
	var snapshot storageformat.DomainSnapshot
	if err := decodeCanonicalValue(object.Body, &snapshot); err != nil {
		return storageformat.DomainHead{}, err
	}
	if snapshot.DomainID != reference.ID || snapshot.Kind != reference.Kind {
		return storageformat.DomainHead{}, domain.NewError(domain.ErrorInvalid, "consistency-domain snapshot key binding mismatch")
	}
	if err := storageformat.ValidateDomainSnapshot(snapshot); err != nil {
		return storageformat.DomainHead{}, err
	}
	return snapshot.Head, nil
}

func (store *consistencyDomainStore) validateMutationAtHead(ctx context.Context, reference consistencyDomainRef, head storageformat.DomainHead, changes []consistencyDomainChange) error {
	return store.validateMutationAtHeadWithSession(ctx, reference, head, changes, nil)
}

func (store *consistencyDomainStore) validateMutationAtHeadWithSession(ctx context.Context, reference consistencyDomainRef, head storageformat.DomainHead, changes []consistencyDomainChange, session *consistencyDomainTreeSession) error {
	if session == nil {
		session = newConsistencyDomainTreeSession(store, reference)
	}
	for _, change := range changes {
		current, found, err := store.lookupAtHeadWithSession(ctx, reference, head, change.Key, session)
		if err != nil {
			return err
		}
		switch change.Require {
		case domainValueAbsent:
			if found {
				return domain.NewError(domain.ErrorConflict, "consistency-domain value already exists")
			}
		case domainValuePresent:
			if !found {
				return domain.NewError(domain.ErrorNotFound, "consistency-domain value does not exist")
			}
			if change.ExpectedVersion != "" && current.LogicalVersion != change.ExpectedVersion {
				return domain.NewError(domain.ErrorPreconditionFailed, "stale consistency-domain value version")
			}
		}
		if change.Delete && !found {
			return domain.NewError(domain.ErrorNotFound, "consistency-domain value does not exist")
		}
	}
	return nil
}

func (store *consistencyDomainStore) encodeHead(key objectstore.Key, snapshot consistencyDomainHeadSnapshot, head storageformat.DomainHead) ([]byte, error) {
	if err := storageformat.ValidateDomainHead(head); err != nil {
		return nil, err
	}
	envelopeRevision := uint64(1)
	if snapshot.exists {
		envelopeRevision = snapshot.envelope.Revision + 1
	}
	return storageformat.EncodeEnvelope(domainHeadSchema, key, envelopeRevision, head)
}

func (store *consistencyDomainStore) lookupOutcomeAtHead(ctx context.Context, reference consistencyDomainRef, head storageformat.DomainHead, mutationID string) (consistencyDomainOutcome, bool, error) {
	return store.lookupOutcomeAtHeadWithSession(ctx, reference, head, mutationID, nil)
}

func (store *consistencyDomainStore) lookupOutcomeAtHeadWithSession(ctx context.Context, reference consistencyDomainRef, head storageformat.DomainHead, mutationID string, session *consistencyDomainTreeSession) (consistencyDomainOutcome, bool, error) {
	for index := len(head.Deltas) - 1; index >= 0; index-- {
		delta := head.Deltas[index]
		if delta.MutationID == mutationID {
			if !store.clock.Now().UTC().Before(delta.RetainUntil) {
				continue
			}
			return consistencyDomainOutcome{MutationID: mutationID, Fingerprint: delta.Fingerprint, Revision: delta.Revision, RetainUntil: delta.RetainUntil, Result: append([]byte(nil), delta.Result...)}, true, nil
		}
	}
	if head.Outcomes.Digest == "" {
		return consistencyDomainOutcome{}, false, nil
	}
	if session == nil {
		session = newConsistencyDomainTreeSession(store, reference)
	}
	value, found, err := session.lookup(ctx, head.Outcomes, mutationID)
	if err != nil || !found {
		return consistencyDomainOutcome{}, found, err
	}
	var recorded storageformat.DomainOutcome
	if err := decodeCanonicalValue(value.Data, &recorded); err != nil {
		return consistencyDomainOutcome{}, false, err
	}
	if err := storageformat.ValidateDomainOutcome(recorded); err != nil || recorded.MutationID != mutationID {
		return consistencyDomainOutcome{}, false, domain.NewError(domain.ErrorInvalid, "misbound consistency-domain outcome")
	}
	if !store.clock.Now().UTC().Before(recorded.RetainUntil) {
		return consistencyDomainOutcome{}, false, nil
	}
	return consistencyDomainOutcome{MutationID: recorded.MutationID, Fingerprint: recorded.Fingerprint, Revision: recorded.Revision, RetainUntil: recorded.RetainUntil, Result: append([]byte(nil), recorded.Result...)}, true, nil
}

func (store *consistencyDomainStore) get(ctx context.Context, reference consistencyDomainRef, key string) (consistencyDomainValue, error) {
	if key == "" {
		return consistencyDomainValue{}, domain.NewError(domain.ErrorInvalid, "invalid consistency-domain key")
	}
	snapshot, err := store.loadHead(ctx, reference)
	if err != nil {
		return consistencyDomainValue{}, err
	}
	if !snapshot.exists || !snapshot.head.Registered {
		return consistencyDomainValue{}, domain.NewError(domain.ErrorNotFound, "consistency-domain value does not exist")
	}
	if _, _, found, lockErr := transitionLockAtHead009(ctx, store, reference, snapshot.head); lockErr != nil {
		return consistencyDomainValue{}, lockErr
	} else if found {
		return consistencyDomainValue{}, domain.WrapError(domain.ErrorUnavailable, "consistency domain has a pending transition", errTransitionPending009)
	}
	value, found, err := store.lookupAtHead(ctx, reference, snapshot.head, key)
	if err != nil {
		return consistencyDomainValue{}, err
	}
	if !found {
		return consistencyDomainValue{}, domain.NewError(domain.ErrorNotFound, "consistency-domain value does not exist")
	}
	return value, nil
}

func (store *consistencyDomainStore) list(ctx context.Context, reference consistencyDomainRef, prefix, after string, limit int, snapshotExpiresAt time.Time) ([]storageformat.DomainEntry, uint64, string, error) {
	if prefix == "" || limit < 1 {
		return nil, 0, "", domain.NewError(domain.ErrorInvalid, "invalid consistency-domain range")
	}
	snapshot, err := store.loadHead(ctx, reference)
	if err != nil {
		return nil, 0, "", err
	}
	if !snapshot.exists || !snapshot.head.Registered {
		return nil, 0, "", nil
	}
	if _, _, found, lockErr := transitionLockAtHead009(ctx, store, reference, snapshot.head); lockErr != nil {
		return nil, 0, "", lockErr
	} else if found {
		return nil, 0, "", domain.WrapError(domain.ErrorUnavailable, "consistency domain has a pending transition", errTransitionPending009)
	}
	entries, err := store.listAtHead(ctx, reference, snapshot.head, prefix, after, limit)
	if err != nil || len(entries) < limit {
		return entries, snapshot.head.Revision, "", err
	}
	digest, err := store.writeHeadSnapshot(ctx, reference, snapshot.head, snapshotExpiresAt)
	return entries, snapshot.head.Revision, digest, err
}

func (store *consistencyDomainStore) listSnapshot(ctx context.Context, reference consistencyDomainRef, digest, prefix, after string, limit int) ([]storageformat.DomainEntry, uint64, error) {
	head, err := store.loadHeadSnapshot(ctx, reference, digest)
	if err != nil {
		return nil, 0, err
	}
	entries, err := store.listAtHead(ctx, reference, head, prefix, after, limit)
	return entries, head.Revision, err
}

func (store *consistencyDomainStore) listAtHead(ctx context.Context, reference consistencyDomainRef, head storageformat.DomainHead, prefix, after string, limit int) ([]storageformat.DomainEntry, error) {
	relevantDeltaChanges := 0
	for _, delta := range head.Deltas {
		for _, change := range delta.Changes {
			if change.Key > after && strings.HasPrefix(change.Key, prefix) {
				relevantDeltaChanges++
			}
		}
	}
	base, err := newConsistencyDomainTreeSession(store, reference).collect(ctx, head.Base, prefix, after, limit+relevantDeltaChanges)
	if err != nil {
		return nil, err
	}
	values := make(map[string]storageformat.DomainEntry, len(base)+relevantDeltaChanges)
	for _, entry := range base {
		values[entry.Key] = entry
	}
	for _, delta := range head.Deltas {
		for _, change := range delta.Changes {
			if change.Key <= after || !strings.HasPrefix(change.Key, prefix) {
				continue
			}
			if change.Delete {
				delete(values, change.Key)
				continue
			}
			values[change.Key] = storageformat.DomainEntry{Key: change.Key, Value: append([]byte(nil), change.Value...), LogicalVersion: change.LogicalVersion}
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > limit {
		keys = keys[:limit]
	}
	result := make([]storageformat.DomainEntry, 0, len(keys))
	for _, key := range keys {
		result = append(result, values[key])
	}
	return result, nil
}

func (store *consistencyDomainStore) lookupAtHead(ctx context.Context, reference consistencyDomainRef, head storageformat.DomainHead, key string) (consistencyDomainValue, bool, error) {
	return store.lookupAtHeadWithSession(ctx, reference, head, key, nil)
}

func (store *consistencyDomainStore) lookupAtHeadWithSession(ctx context.Context, reference consistencyDomainRef, head storageformat.DomainHead, key string, session *consistencyDomainTreeSession) (consistencyDomainValue, bool, error) {
	for deltaIndex := len(head.Deltas) - 1; deltaIndex >= 0; deltaIndex-- {
		delta := head.Deltas[deltaIndex]
		changeIndex := sort.Search(len(delta.Changes), func(index int) bool { return delta.Changes[index].Key >= key })
		if changeIndex == len(delta.Changes) || delta.Changes[changeIndex].Key != key {
			continue
		}
		change := delta.Changes[changeIndex]
		if change.Delete {
			return consistencyDomainValue{}, false, nil
		}
		return consistencyDomainValue{Data: append([]byte(nil), change.Value...), LogicalVersion: change.LogicalVersion, Revision: delta.Revision}, true, nil
	}
	if head.Base.Digest == "" {
		return consistencyDomainValue{}, false, nil
	}
	if session == nil {
		session = newConsistencyDomainTreeSession(store, reference)
	}
	return session.lookup(ctx, head.Base, key)
}

func (store *consistencyDomainStore) compact(ctx context.Context, reference consistencyDomainRef) error {
	snapshot, err := store.loadHead(ctx, reference)
	if err != nil {
		return err
	}
	return store.compactSnapshot(ctx, reference, snapshot)
}

func (store *consistencyDomainStore) compactSnapshot(ctx context.Context, reference consistencyDomainRef, snapshot consistencyDomainHeadSnapshot) error {
	if snapshot.head.Frozen {
		return domain.NewError(domain.ErrorUnavailable, "consistency domain is frozen")
	}
	if !snapshot.exists || len(snapshot.head.Deltas) == 0 && snapshot.head.OutcomeExpiry.Digest == "" {
		return nil
	}
	latest := make(map[string]storageformat.DomainChange)
	outcomes := make(map[string]storageformat.DomainChange, len(snapshot.head.Deltas))
	expiries := make(map[string]storageformat.DomainChange, len(snapshot.head.Deltas))
	now := store.clock.Now().UTC()
	for _, delta := range snapshot.head.Deltas {
		for _, change := range delta.Changes {
			latest[change.Key] = change
		}
		if !now.Before(delta.RetainUntil) {
			continue
		}
		recorded := storageformat.DomainOutcome{MutationID: delta.MutationID, Fingerprint: delta.Fingerprint, Revision: delta.Revision, RetainUntil: delta.RetainUntil, Result: append([]byte(nil), delta.Result...)}
		body, err := storageformat.EncodeCanonical(recorded)
		if err != nil {
			return err
		}
		outcomes[delta.MutationID] = storageformat.DomainChange{Key: delta.MutationID, Value: body, LogicalVersion: storageformat.Digest(append([]byte("endlessfs-consistency-domain-outcome-v1\x00"), body...))}
		expiryKey := consistencyDomainOutcomeExpiryKey(delta.RetainUntil, delta.MutationID)
		expiries[expiryKey] = storageformat.DomainChange{Key: expiryKey, Value: []byte(delta.MutationID), LogicalVersion: storageformat.Digest([]byte("endlessfs-consistency-domain-outcome-expiry-v1\x00" + expiryKey + "\x00" + delta.MutationID))}
	}
	changes := make([]storageformat.DomainChange, 0, len(latest))
	for _, change := range latest {
		changes = append(changes, change)
	}
	sort.Slice(changes, func(left, right int) bool { return changes[left].Key < changes[right].Key })
	session := newConsistencyDomainTreeSession(store, reference)
	root, err := session.apply(ctx, snapshot.head.Base, changes)
	if err != nil {
		return err
	}
	outcomeRoot, expiryRoot, err := store.pruneExpiredOutcomes(ctx, session, snapshot.head.Outcomes, snapshot.head.OutcomeExpiry, now, outcomes, expiries)
	if err != nil {
		return err
	}
	next := snapshot.head
	next.Base = root
	next.Outcomes = outcomeRoot
	next.OutcomeExpiry = expiryRoot
	next.BaseRevision = next.Revision
	next.Deltas = nil
	key := storageformat.DomainHeadKey(reference.Kind, reference.ID)
	body, err := store.encodeHead(key, snapshot, next)
	if err != nil {
		return err
	}
	if err := store.step(ctx, "consistency-domain:before-compaction-commit"); err != nil {
		return err
	}
	_, err = store.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: snapshot.object.Version})
	return err
}

func consistencyDomainOutcomeExpiryKey(retainUntil time.Time, mutationID string) string {
	return retainUntil.UTC().Format("20060102T150405.000000000Z") + "." + base64.RawURLEncoding.EncodeToString([]byte(mutationID))
}

// pruneExpiredOutcomes consumes only the earliest bounded expiry page on each
// compaction. It never scans the complete replay history and applies expired
// removals through the same persistent-tree path-copy machinery.
func (store *consistencyDomainStore) pruneExpiredOutcomes(ctx context.Context, session *consistencyDomainTreeSession, outcomeRoot, expiryRoot storageformat.DomainTreeRoot, now time.Time, outcomes, expiries map[string]storageformat.DomainChange) (storageformat.DomainTreeRoot, storageformat.DomainTreeRoot, error) {
	if expiryRoot.Digest != "" {
		entries, err := session.collectOrdered(ctx, expiryRoot, "", domainPageMaximumItems, false)
		if err != nil {
			return storageformat.DomainTreeRoot{}, storageformat.DomainTreeRoot{}, err
		}
		for _, entry := range entries {
			retainUntil := parseConsistencyDomainExpiryTime(entry.Key)
			if retainUntil.IsZero() {
				return storageformat.DomainTreeRoot{}, storageformat.DomainTreeRoot{}, domain.NewError(domain.ErrorInvalid, "invalid consistency-domain outcome expiry")
			}
			if retainUntil.After(now) {
				break
			}
			mutationID := string(entry.Value)
			if mutationID == "" || consistencyDomainOutcomeExpiryKey(retainUntil, mutationID) != entry.Key {
				return storageformat.DomainTreeRoot{}, storageformat.DomainTreeRoot{}, domain.NewError(domain.ErrorInvalid, "invalid consistency-domain outcome expiry")
			}
			expiries[entry.Key] = storageformat.DomainChange{Key: entry.Key, Delete: true}
			if _, replacing := outcomes[mutationID]; !replacing {
				outcomes[mutationID] = storageformat.DomainChange{Key: mutationID, Delete: true}
			}
		}
	}
	outcomeChanges := normalizeDomainChangeMap(outcomes)
	expiryChanges := normalizeDomainChangeMap(expiries)
	var err error
	outcomeRoot, err = session.apply(ctx, outcomeRoot, outcomeChanges)
	if err != nil {
		return storageformat.DomainTreeRoot{}, storageformat.DomainTreeRoot{}, err
	}
	expiryRoot, err = session.apply(ctx, expiryRoot, expiryChanges)
	return outcomeRoot, expiryRoot, err
}

func parseConsistencyDomainExpiryTime(key string) time.Time {
	const layout = "20060102T150405.000000000Z"
	if len(key) <= len(layout) || key[len(layout)] != '.' {
		return time.Time{}
	}
	value, err := time.Parse(layout, key[:len(layout)])
	if err != nil {
		return time.Time{}
	}
	return value.UTC()
}

func normalizeDomainChangeMap(values map[string]storageformat.DomainChange) []storageformat.DomainChange {
	changes := make([]storageformat.DomainChange, 0, len(values))
	for _, change := range values {
		changes = append(changes, change)
	}
	sort.Slice(changes, func(left, right int) bool { return changes[left].Key < changes[right].Key })
	return changes
}

func (store *consistencyDomainStore) freeze(ctx context.Context, reference consistencyDomainRef, epoch uint64) error {
	if epoch == 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain freeze epoch")
	}
	if err := store.ensureRegistered(ctx, reference); err != nil {
		return err
	}
	for {
		snapshot, err := store.loadHead(ctx, reference)
		if err != nil {
			return err
		}
		if !snapshot.exists || !snapshot.head.Registered {
			return domain.NewError(domain.ErrorNotFound, "consistency domain does not exist")
		}
		_, _, transitionPending, lockErr := transitionLockAtHead009(ctx, store, reference, snapshot.head)
		if lockErr != nil {
			return lockErr
		}
		if transitionPending {
			if snapshot.head.Frozen {
				return domain.NewError(domain.ErrorInvalid, "frozen consistency domain contains a transition lock")
			}
			return errTransitionPending009
		}
		if snapshot.head.Frozen {
			if snapshot.head.FreezeEpoch == epoch {
				return nil
			}
			return domain.NewError(domain.ErrorConflict, "consistency domain is frozen at another epoch")
		}
		next := snapshot.head
		next.Frozen, next.FreezeEpoch = true, epoch
		key := storageformat.DomainHeadKey(reference.Kind, reference.ID)
		body, err := store.encodeHead(key, snapshot, next)
		if err != nil {
			return err
		}
		if _, err := store.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: snapshot.object.Version}); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
		// A racing writer changed the head after it was read. Reload and freeze
		// the winner rather than leaking a provider-generation conflict to the
		// checkpoint coordinator or leaving a registered domain writable.
	}
}

func (store *consistencyDomainStore) unfreeze(ctx context.Context, reference consistencyDomainRef, epoch uint64) error {
	if epoch == 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain freeze epoch")
	}
	for {
		snapshot, err := store.loadHead(ctx, reference)
		if err != nil {
			return err
		}
		if !snapshot.exists || !snapshot.head.Registered {
			return domain.NewError(domain.ErrorNotFound, "consistency domain does not exist")
		}
		if !snapshot.head.Frozen {
			return nil
		}
		if snapshot.head.FreezeEpoch != epoch {
			return domain.NewError(domain.ErrorConflict, "consistency domain is frozen at another epoch")
		}
		next := snapshot.head
		next.Frozen, next.FreezeEpoch = false, 0
		key := storageformat.DomainHeadKey(reference.Kind, reference.ID)
		body, err := store.encodeHead(key, snapshot, next)
		if err != nil {
			return err
		}
		if _, err := store.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: snapshot.object.Version}); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
	}
}
