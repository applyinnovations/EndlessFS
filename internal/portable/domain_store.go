package portable

import (
	"context"
	"errors"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const (
	domainHeadSchema  = "consistency-domain-head-v1"
	domainClaimSchema = "consistency-domain-claim-v1"
)

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
	LogicalVersion  string                 `json:"logicalVersion,omitempty"`
}

type consistencyDomainMutation struct {
	ID      string
	Changes []consistencyDomainChange
	Result  []byte
}

type consistencyDomainOutcome struct {
	MutationID  string
	Fingerprint string
	Revision    uint64
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
}

func newConsistencyDomainStore(backend objectstore.Backend, scheduler Scheduler) *consistencyDomainStore {
	return &consistencyDomainStore{backend: backend, scheduler: scheduler}
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
	// Key construction owns the closed kind grammar and deliberately panics for
	// programmer misuse. Convert that internal invariant into a request error at
	// this boundary.
	switch reference.Kind {
	case storageformat.DomainNamespace, storageformat.DomainOwnerControl, storageformat.DomainAdmin, storageformat.DomainCapability, storageformat.DomainShare:
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
		return consistencyDomainHeadSnapshot{head: storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind}}, nil
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
	if err := storageformat.ValidateDomainHead(head); err != nil {
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
		if change.Key == "" || change.Key == previous || change.Require < domainValueAny || change.Require > domainValuePresent || change.Require != domainValuePresent && change.ExpectedVersion != "" || change.Delete && (len(change.Value) != 0 || change.LogicalVersion != "") || !change.Delete && change.LogicalVersion == "" {
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
	fingerprint := storageformat.Digest(append([]byte("endlessfs-consistency-domain-mutation-v1\x00"), body...))
	probe := storageformat.DomainDelta{MutationID: mutation.ID, Fingerprint: fingerprint, Revision: 1, Result: append([]byte(nil), mutation.Result...), Changes: make([]storageformat.DomainChange, len(changes))}
	for index, change := range changes {
		probe.Changes[index] = storageformat.DomainChange{Key: change.Key, Delete: change.Delete, Value: append([]byte(nil), change.Value...), LogicalVersion: change.LogicalVersion}
	}
	if _, err := storageformat.EncodeCanonical(probe); err != nil {
		return consistencyDomainMutation{}, "", err
	}
	mutation.Changes = changes
	mutation.Result = append([]byte(nil), mutation.Result...)
	return mutation, fingerprint, nil
}

func (store *consistencyDomainStore) mutate(ctx context.Context, reference consistencyDomainRef, mutation consistencyDomainMutation) (consistencyDomainOutcome, error) {
	if err := validateConsistencyDomainRef(reference); err != nil {
		return consistencyDomainOutcome{}, err
	}
	mutation, fingerprint, err := normalizeConsistencyDomainMutation(mutation)
	if err != nil {
		return consistencyDomainOutcome{}, err
	}
	snapshot, err := store.loadHead(ctx, reference)
	if err != nil {
		return consistencyDomainOutcome{}, err
	}
	if snapshot.head.Frozen {
		return consistencyDomainOutcome{}, domain.NewError(domain.ErrorUnavailable, "consistency domain is frozen")
	}
	claim, claimEnvelope, claimObject, replay, err := store.claim(ctx, reference, snapshot.head, mutation, fingerprint)
	if err != nil || replay != nil {
		if replay != nil {
			return *replay, nil
		}
		return consistencyDomainOutcome{}, err
	}
	changes := mutation.Changes
	for _, change := range changes {
		current, found, err := store.lookupAtHead(ctx, reference, snapshot.head, change.Key)
		if err != nil {
			return consistencyDomainOutcome{}, err
		}
		switch change.Require {
		case domainValueAbsent:
			if found {
				return consistencyDomainOutcome{}, domain.NewError(domain.ErrorConflict, "consistency-domain value already exists")
			}
		case domainValuePresent:
			if !found {
				return consistencyDomainOutcome{}, domain.NewError(domain.ErrorNotFound, "consistency-domain value does not exist")
			}
			if change.ExpectedVersion != "" && current.LogicalVersion != change.ExpectedVersion {
				return consistencyDomainOutcome{}, domain.NewError(domain.ErrorPreconditionFailed, "stale consistency-domain value version")
			}
		}
		if change.Delete && !found {
			return consistencyDomainOutcome{}, domain.NewError(domain.ErrorNotFound, "consistency-domain value does not exist")
		}
	}

	revision := snapshot.head.Revision + 1
	delta := storageformat.DomainDelta{MutationID: mutation.ID, Fingerprint: fingerprint, Revision: revision, Result: append([]byte(nil), mutation.Result...), Changes: make([]storageformat.DomainChange, len(changes))}
	for index, change := range changes {
		delta.Changes[index] = storageformat.DomainChange{Key: change.Key, Delete: change.Delete, Value: append([]byte(nil), change.Value...), LogicalVersion: change.LogicalVersion}
	}
	next := snapshot.head
	next.Revision = revision
	next.Deltas = append(append([]storageformat.DomainDelta(nil), snapshot.head.Deltas...), delta)
	if err := storageformat.ValidateDomainHead(next); err != nil {
		return consistencyDomainOutcome{}, err
	}
	headKey := storageformat.DomainHeadKey(reference.Kind, reference.ID)
	headBody, err := storageformat.EncodeEnvelope(domainHeadSchema, headKey, revision, next)
	if err != nil {
		return consistencyDomainOutcome{}, err
	}
	condition := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
	if snapshot.exists {
		condition = objectstore.PutCondition{Mode: objectstore.PutMatch, Version: snapshot.object.Version}
	}
	if err := store.step(ctx, "consistency-domain:before-head-commit"); err != nil {
		return consistencyDomainOutcome{}, err
	}
	if _, err := store.backend.Put(ctx, headKey, headBody, condition); err != nil {
		return consistencyDomainOutcome{}, err
	}
	if err := store.step(ctx, "consistency-domain:after-head-commit"); err != nil {
		return consistencyDomainOutcome{}, err
	}
	outcome := consistencyDomainOutcome{MutationID: mutation.ID, Fingerprint: fingerprint, Revision: revision, Result: append([]byte(nil), mutation.Result...)}
	claim.State, claim.Revision, claim.Result = storageformat.DomainClaimCommitted, revision, append([]byte(nil), mutation.Result...)
	if err := store.finalizeClaim(ctx, reference, claim, claimEnvelope.Revision+1, claimObject.Version); err != nil {
		return consistencyDomainOutcome{}, err
	}
	return outcome, nil
}

func (store *consistencyDomainStore) claim(ctx context.Context, reference consistencyDomainRef, head storageformat.DomainHead, mutation consistencyDomainMutation, fingerprint string) (storageformat.DomainClaim, storageformat.Envelope, objectstore.Object, *consistencyDomainOutcome, error) {
	key := storageformat.DomainClaimKey(reference.Kind, reference.ID, mutation.ID)
	claim := storageformat.DomainClaim{SchemaVersion: 1, DomainID: reference.ID, MutationID: mutation.ID, Fingerprint: fingerprint, State: storageformat.DomainClaimPrepared}
	body, err := storageformat.EncodeEnvelope(domainClaimSchema, key, 1, claim)
	if err != nil {
		return storageformat.DomainClaim{}, storageformat.Envelope{}, objectstore.Object{}, nil, err
	}
	version, err := store.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
	if err == nil {
		return claim, storageformat.Envelope{Revision: 1}, objectstore.Object{Key: key, Version: version}, nil, nil
	}
	if !errors.Is(err, domain.ErrConflict) {
		return storageformat.DomainClaim{}, storageformat.Envelope{}, objectstore.Object{}, nil, err
	}
	object, err := store.backend.Get(ctx, key)
	if err != nil {
		return storageformat.DomainClaim{}, storageformat.Envelope{}, objectstore.Object{}, nil, err
	}
	var envelope storageformat.Envelope
	if err := storageformat.DecodeEnvelope(object.Body, key, domainClaimSchema, &envelope, &claim); err != nil {
		return storageformat.DomainClaim{}, storageformat.Envelope{}, objectstore.Object{}, nil, err
	}
	if err := storageformat.ValidateDomainClaim(claim); err != nil || claim.DomainID != reference.ID || claim.MutationID != mutation.ID || claim.Fingerprint != fingerprint {
		return storageformat.DomainClaim{}, storageformat.Envelope{}, objectstore.Object{}, nil, domain.NewError(domain.ErrorConflict, "consistency-domain idempotency key was reused or corrupt")
	}
	if claim.State == storageformat.DomainClaimCommitted {
		return claim, envelope, object, &consistencyDomainOutcome{MutationID: claim.MutationID, Fingerprint: claim.Fingerprint, Revision: claim.Revision, Result: append([]byte(nil), claim.Result...), Replayed: true}, nil
	}
	for _, delta := range head.Deltas {
		if delta.MutationID != mutation.ID {
			continue
		}
		if delta.Fingerprint != fingerprint {
			return storageformat.DomainClaim{}, storageformat.Envelope{}, objectstore.Object{}, nil, domain.NewError(domain.ErrorInvalid, "consistency-domain claim disagrees with committed delta")
		}
		claim.State, claim.Revision, claim.Result = storageformat.DomainClaimCommitted, delta.Revision, append([]byte(nil), delta.Result...)
		if err := store.finalizeClaim(ctx, reference, claim, envelope.Revision+1, object.Version); err != nil {
			return storageformat.DomainClaim{}, storageformat.Envelope{}, objectstore.Object{}, nil, err
		}
		return claim, envelope, object, &consistencyDomainOutcome{MutationID: claim.MutationID, Fingerprint: claim.Fingerprint, Revision: claim.Revision, Result: append([]byte(nil), claim.Result...), Replayed: true}, nil
	}
	return claim, envelope, object, nil, nil
}

func (store *consistencyDomainStore) finalizeClaim(ctx context.Context, reference consistencyDomainRef, claim storageformat.DomainClaim, envelopeRevision uint64, nativeVersion objectstore.NativeVersion) error {
	if err := storageformat.ValidateDomainClaim(claim); err != nil {
		return err
	}
	key := storageformat.DomainClaimKey(reference.Kind, reference.ID, claim.MutationID)
	body, err := storageformat.EncodeEnvelope(domainClaimSchema, key, envelopeRevision, claim)
	if err != nil {
		return err
	}
	_, err = store.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: nativeVersion})
	return err
}

func (store *consistencyDomainStore) get(ctx context.Context, reference consistencyDomainRef, key string) (consistencyDomainValue, error) {
	if key == "" {
		return consistencyDomainValue{}, domain.NewError(domain.ErrorInvalid, "invalid consistency-domain key")
	}
	snapshot, err := store.loadHead(ctx, reference)
	if err != nil {
		return consistencyDomainValue{}, err
	}
	if !snapshot.exists {
		return consistencyDomainValue{}, domain.NewError(domain.ErrorNotFound, "consistency-domain value does not exist")
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

func (store *consistencyDomainStore) lookupAtHead(ctx context.Context, reference consistencyDomainRef, head storageformat.DomainHead, key string) (consistencyDomainValue, bool, error) {
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
	return newConsistencyDomainTreeSession(store, reference).lookup(ctx, head.Base, key)
}

func (store *consistencyDomainStore) compact(ctx context.Context, reference consistencyDomainRef) error {
	snapshot, err := store.loadHead(ctx, reference)
	if err != nil {
		return err
	}
	if !snapshot.exists || len(snapshot.head.Deltas) == 0 {
		return nil
	}
	for _, delta := range snapshot.head.Deltas {
		if err := store.reconcileCommittedClaim(ctx, reference, delta); err != nil {
			return err
		}
	}
	latest := make(map[string]storageformat.DomainChange)
	for _, delta := range snapshot.head.Deltas {
		for _, change := range delta.Changes {
			latest[change.Key] = change
		}
	}
	changes := make([]storageformat.DomainChange, 0, len(latest))
	for _, change := range latest {
		changes = append(changes, change)
	}
	sort.Slice(changes, func(left, right int) bool { return changes[left].Key < changes[right].Key })
	root, err := newConsistencyDomainTreeSession(store, reference).apply(ctx, snapshot.head.Base, changes)
	if err != nil {
		return err
	}
	next := snapshot.head
	next.Base = root
	next.BaseRevision = next.Revision
	next.Deltas = nil
	if err := storageformat.ValidateDomainHead(next); err != nil {
		return err
	}
	key := storageformat.DomainHeadKey(reference.Kind, reference.ID)
	body, err := storageformat.EncodeEnvelope(domainHeadSchema, key, snapshot.envelope.Revision+1, next)
	if err != nil {
		return err
	}
	if err := store.step(ctx, "consistency-domain:before-compaction-commit"); err != nil {
		return err
	}
	_, err = store.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: snapshot.object.Version})
	return err
}

func (store *consistencyDomainStore) reconcileCommittedClaim(ctx context.Context, reference consistencyDomainRef, delta storageformat.DomainDelta) error {
	key := storageformat.DomainClaimKey(reference.Kind, reference.ID, delta.MutationID)
	object, err := store.backend.Get(ctx, key)
	if err != nil {
		return err
	}
	var envelope storageformat.Envelope
	var claim storageformat.DomainClaim
	if err := storageformat.DecodeEnvelope(object.Body, key, domainClaimSchema, &envelope, &claim); err != nil {
		return err
	}
	if err := storageformat.ValidateDomainClaim(claim); err != nil || claim.DomainID != reference.ID || claim.MutationID != delta.MutationID || claim.Fingerprint != delta.Fingerprint {
		return domain.NewError(domain.ErrorInvalid, "consistency-domain claim cannot be reconciled")
	}
	if claim.State == storageformat.DomainClaimCommitted {
		if claim.Revision != delta.Revision || string(claim.Result) != string(delta.Result) {
			return domain.NewError(domain.ErrorInvalid, "consistency-domain committed claim disagrees with delta")
		}
		return nil
	}
	if claim.State != storageformat.DomainClaimPrepared {
		return domain.NewError(domain.ErrorInvalid, "consistency-domain delta has a failed claim")
	}
	claim.State, claim.Revision, claim.Result = storageformat.DomainClaimCommitted, delta.Revision, append([]byte(nil), delta.Result...)
	return store.finalizeClaim(ctx, reference, claim, envelope.Revision+1, object.Version)
}

func (store *consistencyDomainStore) freeze(ctx context.Context, reference consistencyDomainRef, epoch uint64) error {
	if epoch == 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain freeze epoch")
	}
	snapshot, err := store.loadHead(ctx, reference)
	if err != nil {
		return err
	}
	if !snapshot.exists {
		return domain.NewError(domain.ErrorNotFound, "consistency domain does not exist")
	}
	if snapshot.head.Frozen {
		if snapshot.head.FreezeEpoch == epoch {
			return nil
		}
		return domain.NewError(domain.ErrorConflict, "consistency domain is frozen at another epoch")
	}
	next := snapshot.head
	next.Frozen, next.FreezeEpoch = true, epoch
	if err := storageformat.ValidateDomainHead(next); err != nil {
		return err
	}
	key := storageformat.DomainHeadKey(reference.Kind, reference.ID)
	body, err := storageformat.EncodeEnvelope(domainHeadSchema, key, snapshot.envelope.Revision+1, next)
	if err != nil {
		return err
	}
	_, err = store.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: snapshot.object.Version})
	return err
}
