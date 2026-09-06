package portable

import (
	"context"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// These helpers decode schema-007 state-index authority during the adjacent
// migration and closed-gate recovery only. The schema-008 StateStore runtime
// publishes directly through consistency domains in state.go.
func stateVersionObject(key state.Key, version state.Version, data []byte) (storageformat.MutationObject, error) {
	objectKey := storageformat.StateVersionKey(stateNamespace(key), key.String(), string(version))
	body, err := storageformat.EncodeEnvelope(stateVersionSchema, objectKey, 1, storageformat.StateVersionRecord{SchemaVersion: 1, LogicalKey: key.String(), LogicalVersion: string(version), Data: append([]byte(nil), data...)})
	if err != nil {
		return storageformat.MutationObject{}, err
	}
	return storageformat.MutationObject{Key: objectKey.String(), Body: body}, nil
}

func (e *Engine) readIndexedStateValue(ctx context.Context, entry storageformat.StateIndexEntry) (state.Value, error) {
	logical, err := parseExistingStateKey(entry.LogicalKey)
	if err != nil || entry.LogicalVersion == "" {
		return state.Value{}, domain.NewError(domain.ErrorInvalid, "invalid state index entry")
	}
	key := storageformat.StateVersionKey(stateNamespace(logical), logical.String(), entry.LogicalVersion)
	object, err := e.backend.Get(ctx, key)
	if err != nil {
		return state.Value{}, err
	}
	var envelope storageformat.Envelope
	var record storageformat.StateVersionRecord
	if err := storageformat.DecodeEnvelope(object.Body, key, stateVersionSchema, &envelope, &record); err != nil {
		return state.Value{}, err
	}
	if record.SchemaVersion != 1 || record.LogicalKey != logical.String() || record.LogicalVersion != entry.LogicalVersion {
		return state.Value{}, domain.NewError(domain.ErrorInvalid, "state index snapshot mismatch")
	}
	return state.Value{Data: append([]byte(nil), record.Data...), Version: state.Version(record.LogicalVersion)}, nil
}

func canonicalStateKey(key state.Key) objectstore.Key {
	return storageformat.StateKey(stateNamespace(key), key.String())
}

func decodeStateObject(object objectstore.Object, key state.Key) (storageformat.StateRecord, storageformat.Envelope, error) {
	var envelope storageformat.Envelope
	var record storageformat.StateRecord
	if err := storageformat.DecodeEnvelope(object.Body, object.Key, stateRecordSchema, &envelope, &record); err != nil {
		return record, envelope, err
	}
	if record.SchemaVersion != 1 || record.LogicalKey != key.String() || canonicalStateKey(key) != object.Key {
		return record, envelope, domain.NewError(domain.ErrorInvalid, "state key digest collision or corruption")
	}
	return record, envelope, nil
}
