package architecturelab

import (
	"context"
	"errors"
	"fmt"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

const maxPrototypeDeltas = 16

type deltaRecord struct {
	Mutation Mutation `json:"mutation"`
	Outcome  Outcome  `json:"outcome"`
}

type deltaHead struct {
	SchemaVersion int           `json:"schemaVersion"`
	Revision      uint64        `json:"revision"`
	BaseKey       string        `json:"baseKey"`
	Deltas        []deltaRecord `json:"deltas"`
	Frozen        bool          `json:"frozen"`
}

type deltaEngine struct {
	backend  objectstore.Backend
	domainID string
	headKey  objectstore.Key
}

func openDelta(ctx context.Context, backend objectstore.Backend, options Options) (Engine, error) {
	if err := validateOptions(backend, options); err != nil {
		return nil, err
	}
	engine := &deltaEngine{backend: backend, domainID: options.DomainID, headKey: candidateKey("delta", options.DomainID, "head.json")}
	baseKey := candidateKey("delta", options.DomainID, "bases/initial.json")
	baseBody, err := encode(initialSnapshot())
	if err != nil {
		return nil, err
	}
	if err := createImmutable(ctx, backend, baseKey, baseBody); err != nil {
		return nil, err
	}
	headBody, err := encode(deltaHead{SchemaVersion: 1, Revision: 1, BaseKey: baseKey.String(), Deltas: []deltaRecord{}})
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

func (engine *deltaEngine) Name() string { return "bounded-delta" }

func (engine *deltaEngine) load(ctx context.Context, operation MutationKind) (Snapshot, deltaHead, objectstore.NativeVersion, error) {
	headObject, err := engine.backend.Get(trace(ctx, operation, "delta-head", ""), engine.headKey)
	if err != nil {
		return Snapshot{}, deltaHead{}, "", err
	}
	var head deltaHead
	if err := decode(headObject.Body, &head); err != nil || head.SchemaVersion != 1 || head.Revision == 0 || head.BaseKey == "" || len(head.Deltas) > maxPrototypeDeltas {
		return Snapshot{}, deltaHead{}, "", domain.NewError(domain.ErrorInvalid, "invalid delta head")
	}
	baseKey, err := objectstore.ParseKey(head.BaseKey)
	if err != nil {
		return Snapshot{}, deltaHead{}, "", err
	}
	baseObject, err := engine.backend.Get(trace(ctx, operation, "delta-base", ""), baseKey)
	if err != nil {
		return Snapshot{}, deltaHead{}, "", err
	}
	var snapshot Snapshot
	if err := decode(baseObject.Body, &snapshot); err != nil {
		return Snapshot{}, deltaHead{}, "", err
	}
	for _, delta := range head.Deltas {
		next, outcome, changed, err := applyMutation(snapshot, delta.Mutation)
		if err != nil || !changed || outcome.Revision != delta.Outcome.Revision || outcome.Fingerprint != delta.Outcome.Fingerprint {
			return Snapshot{}, deltaHead{}, "", domain.NewError(domain.ErrorInvalid, "delta replay is inconsistent")
		}
		snapshot = next
	}
	snapshot.Frozen = head.Frozen
	if err := validateSnapshot(snapshot); err != nil || snapshot.Revision != head.Revision {
		return Snapshot{}, deltaHead{}, "", domain.NewError(domain.ErrorInvalid, "delta revision is inconsistent")
	}
	return snapshot, head, headObject.Version, nil
}

func (engine *deltaEngine) Mutate(ctx context.Context, mutation Mutation) (Outcome, error) {
	for attempts := 0; attempts < 2; attempts++ {
		snapshot, head, version, err := engine.load(ctx, mutation.Kind)
		if err != nil {
			return Outcome{}, err
		}
		next, outcome, changed, err := applyMutation(snapshot, mutation)
		if err != nil || !changed {
			return outcome, err
		}
		if len(head.Deltas) == maxPrototypeDeltas {
			if err := engine.compactSnapshot(ctx, snapshot, head, version); err != nil {
				return Outcome{}, err
			}
			continue
		}
		head.Deltas = append(head.Deltas, deltaRecord{Mutation: mutation, Outcome: outcome})
		head.Revision = next.Revision
		body, err := encode(head)
		if err != nil {
			return Outcome{}, err
		}
		if _, err := engine.backend.Put(trace(ctx, mutation.Kind, "namespace-commit", ""), engine.headKey, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version}); err != nil {
			return Outcome{}, err
		}
		return outcome, nil
	}
	return Outcome{}, domain.NewError(domain.ErrorUnavailable, "delta compaction did not make progress")
}

func (engine *deltaEngine) Snapshot(ctx context.Context) (Snapshot, error) {
	snapshot, _, _, err := engine.load(ctx, "snapshot")
	return snapshot, err
}

func (engine *deltaEngine) Freeze(ctx context.Context, checkpointID string) (Checkpoint, error) {
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

func (engine *deltaEngine) Compact(ctx context.Context) error {
	snapshot, head, version, err := engine.load(ctx, "compaction")
	if err != nil || len(head.Deltas) == 0 {
		return err
	}
	return engine.compactSnapshot(ctx, snapshot, head, version)
}

func (engine *deltaEngine) compactSnapshot(ctx context.Context, snapshot Snapshot, head deltaHead, version objectstore.NativeVersion) error {
	snapshot.Frozen = false
	baseBody, err := encode(snapshot)
	if err != nil {
		return err
	}
	baseKey := candidateKey("delta", engine.domainID, fmt.Sprintf("bases/%016x.json", snapshot.Revision))
	if err := createImmutable(trace(ctx, "compaction", "compaction-base", ""), engine.backend, baseKey, baseBody); err != nil {
		return err
	}
	head.BaseKey, head.Deltas = baseKey.String(), []deltaRecord{}
	headBody, err := encode(head)
	if err != nil {
		return err
	}
	_, err = engine.backend.Put(trace(ctx, "compaction", "compaction-commit", ""), engine.headKey, headBody, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version})
	return err
}
