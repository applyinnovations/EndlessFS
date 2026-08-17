package portable

import (
	"context"
	"encoding/base64"
	"sort"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func (e *Engine) Get(ctx context.Context, key state.Key) (state.Value, error) {
	if err := validateStateKey(key); err != nil {
		return state.Value{}, err
	}
	objectKey := canonicalStateKey(key)
	object, err := e.backend.Get(ctx, objectKey)
	if err != nil {
		return state.Value{}, err
	}
	record, envelope, err := decodeStateObject(object, key)
	if err != nil {
		return state.Value{}, err
	}
	return state.Value{Data: append([]byte(nil), record.Data...), Version: state.Version(envelope.LogicalVersion)}, nil
}

func (e *Engine) List(ctx context.Context, prefix state.Prefix, request state.PageRequest) (state.Page, error) {
	if !prefix.Valid() {
		return state.Page{}, domain.NewError(domain.ErrorInvalid, "invalid state prefix")
	}
	limit := request.Limit
	if limit == 0 {
		limit = 200
	}
	if limit < 1 || limit > 1000 {
		return state.Page{}, domain.NewError(domain.ErrorInvalid, "page limit must be between 1 and 1000")
	}
	e.snapshotMu.Lock()
	defer e.snapshotMu.Unlock()
	if request.Cursor != "" {
		snapshot, found := e.snapshots[request.Cursor]
		if !found || snapshot.prefix != prefix.String() || snapshot.limit != limit {
			return state.Page{}, domain.NewError(domain.ErrorInvalid, "invalid or out-of-scope state cursor")
		}
		return e.stateSnapshotPage(request.Cursor, snapshot), nil
	}
	namespace := strings.SplitN(prefix.String(), "/", 2)[0]
	objects, err := e.listAll(ctx, storageformat.StatePrefix(namespace))
	if err != nil {
		return state.Page{}, err
	}
	items := make([]state.Item, 0, len(objects))
	for _, info := range objects {
		object, getErr := e.backend.Get(ctx, info.Key)
		if getErr != nil {
			return state.Page{}, getErr
		}
		var envelope storageformat.Envelope
		var record storageformat.StateRecord
		if decodeErr := storageformat.DecodeEnvelope(object.Body, info.Key, stateRecordSchema, &envelope, &record); decodeErr != nil {
			return state.Page{}, decodeErr
		}
		if !strings.HasPrefix(record.LogicalKey, prefix.String()) {
			continue
		}
		logical, keyErr := parseExistingStateKey(record.LogicalKey)
		if keyErr != nil || canonicalStateKey(logical) != info.Key {
			return state.Page{}, domain.NewError(domain.ErrorInvalid, "state key digest collision or corruption")
		}
		items = append(items, state.Item{Key: logical, Value: state.Value{Data: append([]byte(nil), record.Data...), Version: state.Version(envelope.LogicalVersion)}})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Key.String() < items[j].Key.String() })
	if len(items) <= limit {
		return state.Page{Items: items}, nil
	}
	cursor, err := e.ids.OpaqueID()
	if err != nil {
		return state.Page{}, err
	}
	snapshot := &stateListSnapshot{prefix: prefix.String(), limit: limit, items: items}
	e.snapshots[cursor] = snapshot
	return e.stateSnapshotPage(cursor, snapshot), nil
}

func (e *Engine) stateSnapshotPage(cursor string, snapshot *stateListSnapshot) state.Page {
	end := min(snapshot.index+snapshot.limit, len(snapshot.items))
	items := append([]state.Item(nil), snapshot.items[snapshot.index:end]...)
	snapshot.index = end
	if end == len(snapshot.items) {
		delete(e.snapshots, cursor)
		cursor = ""
	}
	return state.Page{Items: items, NextCursor: cursor}
}

func (e *Engine) Create(ctx context.Context, key state.Key, data []byte) (state.Version, error) {
	if err := validateStateMutation(key, data); err != nil {
		return "", err
	}
	var result state.Version
	err := e.withAdmission(ctx, "state-create:"+key.String(), func() error {
		objectKey := canonicalStateKey(key)
		body, err := storageformat.EncodeEnvelope(stateRecordSchema, objectKey, 1, storageformat.StateRecord{SchemaVersion: 1, LogicalKey: key.String(), Data: append([]byte(nil), data...)})
		if err != nil {
			return err
		}
		if _, err := e.backend.Put(ctx, objectKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			return err
		}
		result = envelopeVersion(body)
		return nil
	})
	return result, err
}

func (e *Engine) CompareAndSwap(ctx context.Context, key state.Key, current state.Version, data []byte) (state.Version, error) {
	if err := validateStateMutation(key, data); err != nil {
		return "", err
	}
	if current == "" {
		return "", domain.NewError(domain.ErrorInvalid, "current state version is required")
	}
	var result state.Version
	err := e.withAdmission(ctx, "state-cas:"+key.String(), func() error {
		objectKey := canonicalStateKey(key)
		object, err := e.backend.Get(ctx, objectKey)
		if err != nil {
			return err
		}
		_, envelope, err := decodeStateObject(object, key)
		if err != nil {
			return err
		}
		if state.Version(envelope.LogicalVersion) != current {
			return domain.NewError(domain.ErrorPreconditionFailed, "stale state version")
		}
		body, err := storageformat.EncodeEnvelope(stateRecordSchema, objectKey, envelope.Revision+1, storageformat.StateRecord{SchemaVersion: 1, LogicalKey: key.String(), Data: append([]byte(nil), data...)})
		if err != nil {
			return err
		}
		if _, err := e.backend.Put(ctx, objectKey, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
			return err
		}
		result = envelopeVersion(body)
		return nil
	})
	return result, err
}

func (e *Engine) Delete(ctx context.Context, key state.Key, current state.Version) error {
	if err := validateStateKey(key); err != nil {
		return err
	}
	if current == "" {
		return domain.NewError(domain.ErrorInvalid, "current state version is required")
	}
	return e.withAdmission(ctx, "state-delete:"+key.String(), func() error {
		objectKey := canonicalStateKey(key)
		object, err := e.backend.Get(ctx, objectKey)
		if err != nil {
			return err
		}
		_, envelope, err := decodeStateObject(object, key)
		if err != nil {
			return err
		}
		if state.Version(envelope.LogicalVersion) != current {
			return domain.NewError(domain.ErrorPreconditionFailed, "stale state version")
		}
		return e.backend.Delete(ctx, objectKey, objectstore.DeleteCondition{Version: object.Version})
	})
}

func (e *Engine) listAll(ctx context.Context, prefix string) ([]objectstore.ObjectInfo, error) {
	request := objectstore.ListRequest{Prefix: prefix, Limit: 1000}
	var result []objectstore.ObjectInfo
	for {
		page, err := e.backend.List(ctx, request)
		if err != nil {
			return nil, err
		}
		result = append(result, page.Objects...)
		if page.NextCursor == "" {
			return result, nil
		}
		request.Cursor = page.NextCursor
	}
}

func validateStateKey(key state.Key) error {
	if !key.Valid() {
		return domain.NewError(domain.ErrorInvalid, "invalid state key")
	}
	return nil
}

func validateStateMutation(key state.Key, data []byte) error {
	if err := validateStateKey(key); err != nil {
		return err
	}
	if len(data) > state.MaxRecordBytes {
		return domain.NewError(domain.ErrorInvalid, "invalid state record size")
	}
	return nil
}

func canonicalStateKey(key state.Key) objectstore.Key {
	namespace := strings.SplitN(key.String(), "/", 2)[0]
	return storageformat.StateKey(namespace, key.String())
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

func parseExistingStateKey(value string) (state.Key, error) {
	parts := strings.Split(value, "/")
	if len(parts) < 1 {
		return state.Key{}, domain.NewError(domain.ErrorInvalid, "invalid stored state key")
	}
	namespace := state.Namespace(parts[0])
	decoded := make([]string, 0, len(parts)-1)
	for _, encoded := range parts[1:] {
		// Reconstruct through the public constructor by decoding its base64url parts.
		part, err := decodeStatePart(encoded)
		if err != nil {
			return state.Key{}, err
		}
		decoded = append(decoded, part)
	}
	return state.NewKey(namespace, decoded...)
}

func decodeStatePart(value string) (string, error) {
	decoded, err := base64RawURLDecode(value)
	if err != nil {
		return "", domain.NewError(domain.ErrorInvalid, "invalid stored state key encoding")
	}
	return string(decoded), nil
}

var base64RawURLDecode = func(value string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(value)
}
