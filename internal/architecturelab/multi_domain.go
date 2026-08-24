package architecturelab

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

type multiDomainPending struct {
	OperationRef string                     `json:"operationRef"`
	After        map[string]json.RawMessage `json:"after"`
}

type multiDomainHead struct {
	SchemaVersion int                        `json:"schemaVersion"`
	Revision      uint64                     `json:"revision"`
	Current       map[string]json.RawMessage `json:"current"`
	Pending       *multiDomainPending        `json:"pending,omitempty"`
}

type multiDomainOperation struct {
	SchemaVersion int                                   `json:"schemaVersion"`
	ID            string                                `json:"id"`
	Fingerprint   string                                `json:"fingerprint"`
	State         string                                `json:"state"`
	Changes       map[string]map[string]json.RawMessage `json:"changes"`
}

type multiDomainCoordinator struct {
	backend        objectstore.Backend
	id             string
	beforeDecision func() error
	afterPrepare   func() error
	afterDecision  func() error
}

func openMultiDomainCoordinator(_ context.Context, backend objectstore.Backend, id string) (*multiDomainCoordinator, error) {
	if backend == nil || !domainPattern.MatchString(id) {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid multi-domain coordinator")
	}
	return &multiDomainCoordinator{backend: backend, id: id}, nil
}

func (coordinator *multiDomainCoordinator) domainKey(id string) objectstore.Key {
	return candidateKey("multi-domain", coordinator.id, "domains/"+id+".json")
}

func (coordinator *multiDomainCoordinator) operationKey(id string) objectstore.Key {
	return candidateKey("multi-domain", coordinator.id, "operations/"+digest([]byte(id))+".json")
}

func (coordinator *multiDomainCoordinator) CreateDomain(ctx context.Context, id string, records map[string][]byte) error {
	if !domainPattern.MatchString(id) || len(records) == 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid transaction domain")
	}
	current := make(map[string]json.RawMessage, len(records))
	for key, value := range records {
		if !recordKeyPattern.MatchString(key) || !json.Valid(value) {
			return domain.NewError(domain.ErrorInvalid, "invalid transaction-domain record")
		}
		current[key] = append(json.RawMessage(nil), value...)
	}
	body, _ := encode(multiDomainHead{SchemaVersion: 1, Revision: 1, Current: current})
	_, err := coordinator.backend.Put(trace(ctx, "initialize", "multi-domain-head", ""), coordinator.domainKey(id), body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
	return err
}

func (coordinator *multiDomainCoordinator) Get(ctx context.Context, domainID, key string) ([]byte, error) {
	object, err := coordinator.backend.Get(trace(ctx, "multi-domain-read", "multi-domain-head", ""), coordinator.domainKey(domainID))
	if err != nil {
		return nil, err
	}
	head, err := decodeMultiDomainHead(object.Body)
	if err != nil {
		return nil, err
	}
	current := head.Current
	if head.Pending != nil {
		operationKey, err := objectstore.ParseKey(head.Pending.OperationRef)
		if err != nil {
			return nil, err
		}
		operationObject, err := coordinator.backend.Get(trace(ctx, "multi-domain-read", "multi-domain-decision", ""), operationKey)
		if err != nil {
			return nil, err
		}
		operation, err := decodeMultiDomainOperation(operationObject.Body)
		if err != nil {
			return nil, err
		}
		if operation.State == "committed" {
			current = head.Pending.After
		}
	}
	value, found := current[key]
	if !found {
		return nil, domain.NewError(domain.ErrorNotFound, "transaction-domain record does not exist")
	}
	return append([]byte(nil), value...), nil
}

func (coordinator *multiDomainCoordinator) Commit(ctx context.Context, operationID string, changes map[string]map[string][]byte) error {
	if operationID == "" || len(changes) < 2 {
		return domain.NewError(domain.ErrorInvalid, "multi-domain operation requires at least two domains")
	}
	domains := sortedStringKeys(changes)
	normalized := make(map[string]map[string]json.RawMessage, len(changes))
	for _, domainID := range domains {
		if !domainPattern.MatchString(domainID) || len(changes[domainID]) == 0 {
			return domain.NewError(domain.ErrorInvalid, "invalid multi-domain change")
		}
		normalized[domainID] = make(map[string]json.RawMessage, len(changes[domainID]))
		for key, value := range changes[domainID] {
			if !recordKeyPattern.MatchString(key) || !json.Valid(value) {
				return domain.NewError(domain.ErrorInvalid, "invalid multi-domain record")
			}
			normalized[domainID][key] = append(json.RawMessage(nil), value...)
		}
	}
	fingerprintBody, _ := encode(normalized)
	operation := multiDomainOperation{SchemaVersion: 1, ID: operationID, Fingerprint: digest(fingerprintBody), State: "prepared", Changes: normalized}
	operationBody, _ := encode(operation)
	operationKey := coordinator.operationKey(operationID)
	operationVersion, err := coordinator.backend.Put(trace(ctx, "multi-domain-commit", "multi-domain-plan", ""), operationKey, operationBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
	if err != nil {
		return err
	}
	type domainCandidate struct {
		id      string
		head    multiDomainHead
		version objectstore.NativeVersion
	}
	candidates := make([]domainCandidate, 0, len(domains))
	for _, domainID := range domains {
		object, err := coordinator.backend.Get(trace(ctx, "multi-domain-commit", "multi-domain-head", "multi-domain-read-heads"), coordinator.domainKey(domainID))
		if err != nil {
			return err
		}
		head, err := decodeMultiDomainHead(object.Body)
		if err != nil || head.Pending != nil {
			return domain.NewError(domain.ErrorConflict, "transaction domain is busy or invalid")
		}
		after := cloneRawMap(head.Current)
		for key, value := range normalized[domainID] {
			after[key] = append(json.RawMessage(nil), value...)
		}
		head.Pending = &multiDomainPending{OperationRef: operationKey.String(), After: after}
		candidates = append(candidates, domainCandidate{id: domainID, head: head, version: object.Version})
	}
	for index := range candidates {
		body, _ := encode(candidates[index].head)
		version, err := coordinator.backend.Put(trace(ctx, "multi-domain-commit", "multi-domain-prepare", "multi-domain-prepare-heads"), coordinator.domainKey(candidates[index].id), body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: candidates[index].version})
		if err != nil {
			return err
		}
		candidates[index].version = version
	}
	if coordinator.afterPrepare != nil {
		if err := coordinator.afterPrepare(); err != nil {
			return err
		}
	}
	if coordinator.beforeDecision != nil {
		if err := coordinator.beforeDecision(); err != nil {
			return err
		}
	}
	operation.State = "committed"
	operationBody, _ = encode(operation)
	if _, err := coordinator.backend.Put(trace(ctx, "multi-domain-commit", "multi-domain-decision", ""), operationKey, operationBody, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: operationVersion}); err != nil {
		return err
	}
	if coordinator.afterDecision != nil {
		if err := coordinator.afterDecision(); err != nil {
			return err
		}
	}
	for _, candidate := range candidates {
		candidate.head.Current = candidate.head.Pending.After
		candidate.head.Pending = nil
		candidate.head.Revision++
		body, _ := encode(candidate.head)
		if _, err := coordinator.backend.Put(trace(ctx, "multi-domain-commit", "multi-domain-finalize", "multi-domain-finalize-heads"), coordinator.domainKey(candidate.id), body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: candidate.version}); err != nil {
			return err
		}
	}
	return nil
}

// Recover completes an operation from its durable decision protocol. Prepared
// operations remain entirely old-visible; committed operations are already
// entirely new-visible through pending-head decision resolution. Finalization
// is idempotent cleanup in either case.
func (coordinator *multiDomainCoordinator) Recover(ctx context.Context, operationID string) error {
	operationKey := coordinator.operationKey(operationID)
	operationObject, err := coordinator.backend.Get(trace(ctx, "multi-domain-recovery", "multi-domain-plan", ""), operationKey)
	if err != nil {
		return err
	}
	operation, err := decodeMultiDomainOperation(operationObject.Body)
	if err != nil || operation.ID != operationID {
		return domain.NewError(domain.ErrorInvalid, "invalid recoverable multi-domain operation")
	}
	type candidate struct {
		id      string
		head    multiDomainHead
		version objectstore.NativeVersion
	}
	candidates := make([]candidate, 0, len(operation.Changes))
	for _, domainID := range sortedStringKeys(operation.Changes) {
		object, err := coordinator.backend.Get(trace(ctx, "multi-domain-recovery", "multi-domain-head", "multi-domain-recovery-heads"), coordinator.domainKey(domainID))
		if err != nil {
			return err
		}
		head, err := decodeMultiDomainHead(object.Body)
		if err != nil || head.Pending == nil || head.Pending.OperationRef != operationKey.String() {
			return domain.NewError(domain.ErrorInvalid, "multi-domain recovery is missing a prepared head")
		}
		candidates = append(candidates, candidate{id: domainID, head: head, version: object.Version})
	}
	if operation.State == "prepared" {
		operation.State = "committed"
		body, _ := encode(operation)
		if _, err := coordinator.backend.Put(trace(ctx, "multi-domain-recovery", "multi-domain-decision", ""), operationKey, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: operationObject.Version}); err != nil {
			return err
		}
	}
	for _, candidate := range candidates {
		candidate.head.Current = candidate.head.Pending.After
		candidate.head.Pending = nil
		candidate.head.Revision++
		body, _ := encode(candidate.head)
		if _, err := coordinator.backend.Put(trace(ctx, "multi-domain-recovery", "multi-domain-finalize", "multi-domain-recovery-finalize"), coordinator.domainKey(candidate.id), body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: candidate.version}); err != nil {
			return err
		}
	}
	return nil
}

func decodeMultiDomainHead(body []byte) (multiDomainHead, error) {
	var head multiDomainHead
	if decode(body, &head) != nil || head.SchemaVersion != 1 || head.Revision == 0 || head.Current == nil {
		return multiDomainHead{}, domain.NewError(domain.ErrorInvalid, "invalid multi-domain head")
	}
	for key, value := range head.Current {
		if !recordKeyPattern.MatchString(key) || !json.Valid(value) {
			return multiDomainHead{}, domain.NewError(domain.ErrorInvalid, "invalid multi-domain current record")
		}
	}
	return head, nil
}

func decodeMultiDomainOperation(body []byte) (multiDomainOperation, error) {
	var operation multiDomainOperation
	if decode(body, &operation) != nil || operation.SchemaVersion != 1 || operation.ID == "" || operation.Fingerprint == "" || operation.State != "prepared" && operation.State != "committed" || len(operation.Changes) < 2 {
		return multiDomainOperation{}, domain.NewError(domain.ErrorInvalid, "invalid multi-domain operation")
	}
	return operation, nil
}

func cloneRawMap(source map[string]json.RawMessage) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage, len(source))
	keys := make([]string, 0, len(source))
	for key := range source {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result[key] = append(json.RawMessage(nil), source[key]...)
	}
	return result
}
