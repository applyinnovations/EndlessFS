package architecturelab

import (
	"context"
	"errors"
	"fmt"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

type journalHead struct {
	SchemaVersion int    `json:"schemaVersion"`
	Revision      uint64 `json:"revision"`
	BaseKey       string `json:"baseKey"`
	LastKey       string `json:"lastKey,omitempty"`
	Frozen        bool   `json:"frozen"`
}

type journalTransaction struct {
	SchemaVersion int      `json:"schemaVersion"`
	PreviousKey   string   `json:"previousKey,omitempty"`
	Mutation      Mutation `json:"mutation"`
	Outcome       Outcome  `json:"outcome"`
}

type journalEngine struct {
	backend  objectstore.Backend
	domainID string
	headKey  objectstore.Key
}

func openJournal(ctx context.Context, backend objectstore.Backend, options Options) (Engine, error) {
	if err := validateOptions(backend, options); err != nil {
		return nil, err
	}
	engine := &journalEngine{backend: backend, domainID: options.DomainID, headKey: candidateKey("journal", options.DomainID, "head.json")}
	baseKey := candidateKey("journal", options.DomainID, "bases/initial.json")
	baseBody, err := encode(initialSnapshot())
	if err != nil {
		return nil, err
	}
	if err := createImmutable(ctx, backend, baseKey, baseBody); err != nil {
		return nil, err
	}
	headBody, err := encode(journalHead{SchemaVersion: 1, Revision: 1, BaseKey: baseKey.String()})
	if err != nil {
		return nil, err
	}
	if _, err := backend.Put(ctx, engine.headKey, headBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil && !errors.Is(err, domain.ErrConflict) {
		return nil, err
	}
	if _, _, _, err := engine.load(ctx, "initialize"); err != nil {
		return nil, err
	}
	return engine, nil
}

func (engine *journalEngine) Name() string { return "immutable-journal" }

func (engine *journalEngine) load(ctx context.Context, operation MutationKind) (Snapshot, journalHead, objectstore.NativeVersion, error) {
	headObject, err := engine.backend.Get(trace(ctx, operation, "journal-head", ""), engine.headKey)
	if err != nil {
		return Snapshot{}, journalHead{}, "", err
	}
	var head journalHead
	if err := decode(headObject.Body, &head); err != nil || head.SchemaVersion != 1 || head.Revision == 0 || head.BaseKey == "" {
		return Snapshot{}, journalHead{}, "", domain.NewError(domain.ErrorInvalid, "invalid journal head")
	}
	baseKey, err := objectstore.ParseKey(head.BaseKey)
	if err != nil {
		return Snapshot{}, journalHead{}, "", err
	}
	baseObject, err := engine.backend.Get(trace(ctx, operation, "journal-base", ""), baseKey)
	if err != nil {
		return Snapshot{}, journalHead{}, "", err
	}
	var snapshot Snapshot
	if err := decode(baseObject.Body, &snapshot); err != nil {
		return Snapshot{}, journalHead{}, "", err
	}
	transactions := make([]journalTransaction, 0)
	for keyValue := head.LastKey; keyValue != ""; {
		key, err := objectstore.ParseKey(keyValue)
		if err != nil {
			return Snapshot{}, journalHead{}, "", err
		}
		object, err := engine.backend.Get(trace(ctx, operation, "journal-transaction", ""), key)
		if err != nil {
			return Snapshot{}, journalHead{}, "", err
		}
		var transaction journalTransaction
		if err := decode(object.Body, &transaction); err != nil || transaction.SchemaVersion != 1 {
			return Snapshot{}, journalHead{}, "", domain.NewError(domain.ErrorInvalid, "invalid journal transaction")
		}
		transactions = append(transactions, transaction)
		keyValue = transaction.PreviousKey
	}
	for index := len(transactions) - 1; index >= 0; index-- {
		next, outcome, changed, err := applyMutation(snapshot, transactions[index].Mutation)
		if err != nil || !changed || outcome.Revision != transactions[index].Outcome.Revision || outcome.Fingerprint != transactions[index].Outcome.Fingerprint {
			return Snapshot{}, journalHead{}, "", domain.NewError(domain.ErrorInvalid, "journal replay is inconsistent")
		}
		snapshot = next
	}
	snapshot.Frozen = head.Frozen
	if err := validateSnapshot(snapshot); err != nil || snapshot.Revision != head.Revision {
		return Snapshot{}, journalHead{}, "", domain.NewError(domain.ErrorInvalid, "journal revision is inconsistent")
	}
	return snapshot, head, headObject.Version, nil
}

func (engine *journalEngine) Mutate(ctx context.Context, mutation Mutation) (Outcome, error) {
	snapshot, head, version, err := engine.load(ctx, mutation.Kind)
	if err != nil {
		return Outcome{}, err
	}
	next, outcome, changed, err := applyMutation(snapshot, mutation)
	if err != nil || !changed {
		return outcome, err
	}
	transactionBody, err := encode(journalTransaction{SchemaVersion: 1, PreviousKey: head.LastKey, Mutation: mutation, Outcome: outcome})
	if err != nil {
		return Outcome{}, err
	}
	transactionKey := candidateKey("journal", engine.domainID, fmt.Sprintf("transactions/%s.json", digest(transactionBody)))
	if err := createImmutable(trace(ctx, mutation.Kind, "journal-preparation", "prepare"), engine.backend, transactionKey, transactionBody); err != nil {
		return Outcome{}, err
	}
	head.LastKey, head.Revision = transactionKey.String(), next.Revision
	headBody, err := encode(head)
	if err != nil {
		return Outcome{}, err
	}
	if _, err := engine.backend.Put(trace(ctx, mutation.Kind, "namespace-commit", ""), engine.headKey, headBody, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version}); err != nil {
		return Outcome{}, err
	}
	return outcome, nil
}

func (engine *journalEngine) Snapshot(ctx context.Context) (Snapshot, error) {
	snapshot, _, _, err := engine.load(ctx, "snapshot")
	return snapshot, err
}

func (engine *journalEngine) Freeze(ctx context.Context, checkpointID string) (Checkpoint, error) {
	if checkpointID == "" {
		return Checkpoint{}, domain.NewError(domain.ErrorInvalid, "checkpoint identity is required")
	}
	snapshot, head, version, err := engine.load(ctx, "checkpoint")
	if err != nil {
		return Checkpoint{}, err
	}
	if !head.Frozen {
		head.Frozen = true
		body, err := encode(head)
		if err != nil {
			return Checkpoint{}, err
		}
		if _, err := engine.backend.Put(checkpointTrace(ctx, "freeze-commit"), engine.headKey, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version}); err != nil {
			return Checkpoint{}, err
		}
	}
	body, _ := encode(snapshot)
	return Checkpoint{ID: checkpointID, Revision: snapshot.Revision, Digest: digest(body)}, nil
}

func (engine *journalEngine) Compact(ctx context.Context) error {
	snapshot, head, version, err := engine.load(ctx, "compaction")
	if err != nil || head.LastKey == "" {
		return err
	}
	snapshot.Frozen = false
	baseBody, err := encode(snapshot)
	if err != nil {
		return err
	}
	baseKey := candidateKey("journal", engine.domainID, fmt.Sprintf("bases/%016x.json", snapshot.Revision))
	if err := createImmutable(trace(ctx, "compaction", "compaction-base", ""), engine.backend, baseKey, baseBody); err != nil {
		return err
	}
	head.BaseKey, head.LastKey = baseKey.String(), ""
	headBody, err := encode(head)
	if err != nil {
		return err
	}
	_, err = engine.backend.Put(trace(ctx, "compaction", "compaction-commit", ""), engine.headKey, headBody, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version})
	return err
}
