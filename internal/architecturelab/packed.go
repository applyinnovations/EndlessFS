package architecturelab

import (
	"context"
	"errors"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

type packedEngine struct {
	backend objectstore.Backend
	key     objectstore.Key
}

func openPacked(ctx context.Context, backend objectstore.Backend, options Options) (Engine, error) {
	if err := validateOptions(backend, options); err != nil {
		return nil, err
	}
	engine := &packedEngine{backend: backend, key: candidateKey("packed", options.DomainID, "root.json")}
	body, err := encode(initialSnapshot())
	if err != nil {
		return nil, err
	}
	if _, err := backend.Put(ctx, engine.key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil && !errors.Is(err, domain.ErrConflict) {
		return nil, err
	}
	if _, _, err := engine.load(ctx, "initialize"); err != nil {
		return nil, err
	}
	return engine, nil
}

func (engine *packedEngine) Name() string { return "packed-snapshot" }

func (engine *packedEngine) load(ctx context.Context, operation MutationKind) (Snapshot, objectstore.NativeVersion, error) {
	object, err := engine.backend.Get(trace(ctx, operation, "packed-snapshot", ""), engine.key)
	if err != nil {
		return Snapshot{}, "", err
	}
	var snapshot Snapshot
	if err := decode(object.Body, &snapshot); err != nil {
		return Snapshot{}, "", err
	}
	if err := validateSnapshot(snapshot); err != nil {
		return Snapshot{}, "", err
	}
	return snapshot, object.Version, nil
}

func (engine *packedEngine) Mutate(ctx context.Context, mutation Mutation) (Outcome, error) {
	snapshot, version, err := engine.load(ctx, mutation.Kind)
	if err != nil {
		return Outcome{}, err
	}
	next, outcome, changed, err := applyMutation(snapshot, mutation)
	if err != nil || !changed {
		return outcome, err
	}
	body, err := encode(next)
	if err != nil {
		return Outcome{}, err
	}
	if _, err := engine.backend.Put(trace(ctx, mutation.Kind, "namespace-commit", ""), engine.key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version}); err != nil {
		return Outcome{}, err
	}
	return outcome, nil
}

func (engine *packedEngine) Snapshot(ctx context.Context) (Snapshot, error) {
	snapshot, _, err := engine.load(ctx, "snapshot")
	return snapshot, err
}

func (engine *packedEngine) Freeze(ctx context.Context, checkpointID string) (Checkpoint, error) {
	if checkpointID == "" {
		return Checkpoint{}, domain.NewError(domain.ErrorInvalid, "checkpoint identity is required")
	}
	object, err := engine.backend.Get(checkpointTrace(ctx, "packed-snapshot"), engine.key)
	if err != nil {
		return Checkpoint{}, err
	}
	var snapshot Snapshot
	if err := decode(object.Body, &snapshot); err != nil {
		return Checkpoint{}, err
	}
	if !snapshot.Frozen {
		snapshot.Frozen = true
		body, err := encode(snapshot)
		if err != nil {
			return Checkpoint{}, err
		}
		if _, err := engine.backend.Put(checkpointTrace(ctx, "freeze-commit"), engine.key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
			return Checkpoint{}, err
		}
		object.Body = body
	}
	return Checkpoint{ID: checkpointID, Revision: snapshot.Revision, Digest: digest(object.Body)}, nil
}

func (engine *packedEngine) Compact(context.Context) error { return nil }
