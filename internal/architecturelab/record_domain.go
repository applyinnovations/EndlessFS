package architecturelab

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

const maxRecordDomainHeadBytes = 512 << 10

var recordKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)

type RecordChange struct {
	Key    string          `json:"key"`
	Value  json.RawMessage `json:"value,omitempty"`
	Delete bool            `json:"delete,omitempty"`
}

type RecordMutation struct {
	ID      string          `json:"id"`
	Key     string          `json:"key,omitempty"`
	Value   json.RawMessage `json:"value,omitempty"`
	Delete  bool            `json:"delete,omitempty"`
	Changes []RecordChange  `json:"changes,omitempty"`
}

type RecordOutcome struct {
	MutationID  string `json:"mutationID"`
	Fingerprint string `json:"fingerprint"`
	Revision    uint64 `json:"revision"`
	Committed   bool   `json:"committed"`
	Replayed    bool   `json:"-"`
}

type recordDomainHead struct {
	SchemaVersion int                        `json:"schemaVersion"`
	Revision      uint64                     `json:"revision"`
	Frozen        bool                       `json:"frozen"`
	Records       map[string]json.RawMessage `json:"records"`
	Latest        RecordOutcome              `json:"latest,omitempty"`
}

type recordMutationClaim struct {
	SchemaVersion int           `json:"schemaVersion"`
	MutationID    string        `json:"mutationID"`
	Fingerprint   string        `json:"fingerprint"`
	Committed     bool          `json:"committed"`
	Outcome       RecordOutcome `json:"outcome,omitempty"`
}

// recordDomain is the executable prototype for naturally bounded identity,
// preference, policy, capability, and share consistency domains. It does not
// replace the paged namespace: domains whose collections can grow without a
// product bound use immutable pages behind this same conditional-head shape.
type recordDomain struct {
	backend  objectstore.Backend
	domainID string
	headKey  objectstore.Key
}

func openRecordDomain(ctx context.Context, backend objectstore.Backend, domainID string) (*recordDomain, error) {
	if err := validateOptions(backend, Options{DomainID: domainID}); err != nil {
		return nil, err
	}
	engine := &recordDomain{backend: backend, domainID: domainID, headKey: candidateKey("record-domain", domainID, "head.json")}
	body, _ := encode(recordDomainHead{SchemaVersion: 1, Revision: 1, Records: map[string]json.RawMessage{}})
	if _, err := backend.Put(recordTrace(ctx, "initialize", "record-head"), engine.headKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil && !errors.Is(err, domain.ErrConflict) {
		return nil, err
	}
	if _, _, err := engine.load(ctx, "initialize"); err != nil {
		return nil, err
	}
	return engine, nil
}

func recordTrace(ctx context.Context, operation, subsystem string) context.Context {
	return trace(ctx, MutationKind(operation), subsystem, "")
}

func (engine *recordDomain) load(ctx context.Context, operation string) (recordDomainHead, objectstore.NativeVersion, error) {
	object, err := engine.backend.Get(recordTrace(ctx, operation, "record-head"), engine.headKey)
	if err != nil {
		return recordDomainHead{}, "", err
	}
	if len(object.Body) > maxRecordDomainHeadBytes {
		return recordDomainHead{}, "", domain.NewError(domain.ErrorInvalid, "record-domain head exceeds its prototype bound")
	}
	var head recordDomainHead
	if decode(object.Body, &head) != nil || validateRecordDomainHead(head) != nil {
		return recordDomainHead{}, "", domain.NewError(domain.ErrorInvalid, "invalid record-domain head")
	}
	return head, object.Version, nil
}

func (engine *recordDomain) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if !recordKeyPattern.MatchString(key) {
		return nil, false, domain.NewError(domain.ErrorInvalid, "invalid record-domain key")
	}
	head, _, err := engine.load(ctx, "record-read")
	if err != nil {
		return nil, false, err
	}
	value, found := head.Records[key]
	return append([]byte(nil), value...), found, nil
}

func (engine *recordDomain) List(ctx context.Context, prefix string) (map[string][]byte, error) {
	head, _, err := engine.load(ctx, "record-list")
	if err != nil {
		return nil, err
	}
	result := make(map[string][]byte)
	for key, value := range head.Records {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			result[key] = append([]byte(nil), value...)
		}
	}
	return result, nil
}

func (engine *recordDomain) Mutate(ctx context.Context, mutation RecordMutation) (RecordOutcome, error) {
	changes, fingerprint, err := normalizeRecordMutation(mutation)
	if err != nil {
		return RecordOutcome{}, err
	}
	head, version, err := engine.load(ctx, mutation.ID)
	if err != nil {
		return RecordOutcome{}, err
	}
	claim, claimVersion, replay, err := engine.claim(ctx, mutation.ID, fingerprint, head)
	if err != nil || replay != nil {
		if replay == nil {
			return RecordOutcome{}, err
		}
		return *replay, nil
	}
	if head.Frozen {
		return RecordOutcome{}, domain.NewError(domain.ErrorUnavailable, "record domain is frozen")
	}
	for _, change := range changes {
		if change.Delete {
			delete(head.Records, change.Key)
		} else {
			head.Records[change.Key] = append(json.RawMessage(nil), change.Value...)
		}
	}
	head.Revision++
	outcome := RecordOutcome{MutationID: mutation.ID, Fingerprint: fingerprint, Revision: head.Revision, Committed: true}
	head.Latest = outcome
	body, _ := encode(head)
	if len(body) > maxRecordDomainHeadBytes {
		return RecordOutcome{}, domain.NewError(domain.ErrorUnavailable, "record-domain mutation requires a paged domain")
	}
	if _, err := engine.backend.Put(recordTrace(ctx, mutation.ID, "record-commit"), engine.headKey, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version}); err != nil {
		return RecordOutcome{}, err
	}
	claim.Committed, claim.Outcome = true, outcome
	body, _ = encode(claim)
	if _, err := engine.backend.Put(recordTrace(ctx, mutation.ID, "idempotency-finalize"), engine.claimKey(mutation.ID), body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: claimVersion}); err != nil {
		return RecordOutcome{}, err
	}
	return outcome, nil
}

func (engine *recordDomain) claimKey(mutationID string) objectstore.Key {
	return candidateKey("record-domain", engine.domainID, "claims/"+digest([]byte(mutationID))+".json")
}

func (engine *recordDomain) claim(ctx context.Context, mutationID, fingerprint string, head recordDomainHead) (recordMutationClaim, objectstore.NativeVersion, *RecordOutcome, error) {
	claim := recordMutationClaim{SchemaVersion: 1, MutationID: mutationID, Fingerprint: fingerprint}
	body, _ := encode(claim)
	version, err := engine.backend.Put(recordTrace(ctx, mutationID, "idempotency-claim"), engine.claimKey(mutationID), body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
	if err == nil {
		return claim, version, nil, nil
	}
	if !errors.Is(err, domain.ErrConflict) {
		return recordMutationClaim{}, "", nil, err
	}
	object, err := engine.backend.Get(recordTrace(ctx, mutationID, "idempotency-claim"), engine.claimKey(mutationID))
	if err != nil || decode(object.Body, &claim) != nil || claim.SchemaVersion != 1 || claim.MutationID != mutationID || claim.Fingerprint != fingerprint {
		return recordMutationClaim{}, "", nil, domain.NewError(domain.ErrorConflict, "record-domain idempotency claim conflicts")
	}
	if claim.Committed {
		outcome := claim.Outcome
		outcome.Replayed = true
		return claim, object.Version, &outcome, nil
	}
	if head.Latest.MutationID == mutationID && head.Latest.Fingerprint == fingerprint && head.Latest.Committed {
		claim.Committed, claim.Outcome = true, head.Latest
		body, _ = encode(claim)
		if _, err := engine.backend.Put(recordTrace(ctx, mutationID, "idempotency-finalize"), engine.claimKey(mutationID), body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
			return recordMutationClaim{}, "", nil, err
		}
		outcome := head.Latest
		outcome.Replayed = true
		return claim, object.Version, &outcome, nil
	}
	return claim, object.Version, nil, nil
}

func normalizeRecordMutation(mutation RecordMutation) ([]RecordChange, string, error) {
	if mutation.ID == "" || (mutation.Key == "" && len(mutation.Changes) == 0) || (mutation.Key != "" && len(mutation.Changes) != 0) {
		return nil, "", domain.NewError(domain.ErrorInvalid, "invalid record-domain mutation")
	}
	changes := append([]RecordChange(nil), mutation.Changes...)
	if mutation.Key != "" {
		changes = []RecordChange{{Key: mutation.Key, Value: mutation.Value, Delete: mutation.Delete}}
	}
	sort.Slice(changes, func(left, right int) bool { return changes[left].Key < changes[right].Key })
	for index, change := range changes {
		if !recordKeyPattern.MatchString(change.Key) || index > 0 && changes[index-1].Key == change.Key || change.Delete && len(change.Value) != 0 || !change.Delete && !json.Valid(change.Value) {
			return nil, "", domain.NewError(domain.ErrorInvalid, "invalid record-domain change")
		}
	}
	normalized := struct {
		ID      string         `json:"id"`
		Changes []RecordChange `json:"changes"`
	}{ID: mutation.ID, Changes: changes}
	body, _ := encode(normalized)
	return changes, digest(body), nil
}

func validateRecordDomainHead(head recordDomainHead) error {
	if head.SchemaVersion != 1 || head.Revision == 0 || head.Records == nil {
		return errors.New("invalid record-domain head")
	}
	for key, value := range head.Records {
		if !recordKeyPattern.MatchString(key) || !json.Valid(value) {
			return errors.New("invalid record-domain record")
		}
	}
	return nil
}
