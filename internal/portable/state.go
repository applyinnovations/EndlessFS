package portable

import (
	"context"
	"encoding/base64"
	"sort"
	"strings"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type stateListCursor struct {
	SchemaVersion int       `json:"schemaVersion"`
	Prefix        string    `json:"prefix"`
	Limit         int       `json:"limit"`
	Index         int       `json:"index"`
	GateEpoch     uint64    `json:"gateEpoch"`
	GateVersion   string    `json:"gateVersion"`
	ExpiresAt     time.Time `json:"expiresAt"`
	Snapshots     []string  `json:"snapshots"`
}

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
	if request.Cursor != "" {
		cursor, err := e.decodeStateListCursor(request.Cursor)
		if err != nil || cursor.Prefix != prefix.String() || cursor.Limit != limit || cursor.Index < 1 || cursor.Index >= len(cursor.Snapshots) || !e.clock.Now().Before(cursor.ExpiresAt) {
			return state.Page{}, domain.NewError(domain.ErrorInvalid, "invalid or out-of-scope state cursor")
		}
		_, gateEnvelope, gate, err := e.readGate(ctx)
		if err != nil || gate.Epoch != cursor.GateEpoch || gateEnvelope.LogicalVersion != cursor.GateVersion {
			return state.Page{}, domain.NewError(domain.ErrorInvalid, "state cursor is no longer valid")
		}
		return e.stateCursorPage(ctx, cursor)
	}
	_, gateEnvelope, gate, err := e.readGate(ctx)
	if err != nil {
		return state.Page{}, err
	}
	namespace := strings.SplitN(prefix.String(), "/", 2)[0]
	objects, err := e.listAll(ctx, storageformat.StatePrefix(namespace))
	if err != nil {
		return state.Page{}, err
	}
	items := make([]state.Item, 0, len(objects))
	snapshots := make([]string, 0, len(objects))
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
		snapshots = append(snapshots, storageformat.StateVersionKey(namespace, logical.String(), envelope.LogicalVersion).String())
	}
	order := make([]int, len(items))
	for index := range order {
		order[index] = index
	}
	sort.Slice(order, func(i, j int) bool { return items[order[i]].Key.String() < items[order[j]].Key.String() })
	sortedItems := make([]state.Item, len(items))
	sortedSnapshots := make([]string, len(items))
	for index, original := range order {
		sortedItems[index] = items[original]
		sortedSnapshots[index] = snapshots[original]
	}
	items, snapshots = sortedItems, sortedSnapshots
	if len(items) <= limit {
		return state.Page{Items: items}, nil
	}
	cursor := stateListCursor{
		SchemaVersion: 1, Prefix: prefix.String(), Limit: limit, Index: limit,
		GateEpoch: gate.Epoch, GateVersion: gateEnvelope.LogicalVersion,
		ExpiresAt: e.clock.Now().UTC().Add(e.cursorTTL), Snapshots: snapshots,
	}
	next, err := e.encodeStateListCursor(cursor)
	if err != nil {
		return state.Page{}, err
	}
	return state.Page{Items: append([]state.Item(nil), items[:limit]...), NextCursor: next}, nil
}

func (e *Engine) stateCursorPage(ctx context.Context, cursor stateListCursor) (state.Page, error) {
	end := min(cursor.Index+cursor.Limit, len(cursor.Snapshots))
	items := make([]state.Item, 0, end-cursor.Index)
	for _, keyValue := range cursor.Snapshots[cursor.Index:end] {
		key, err := objectstore.ParseKey(keyValue)
		if err != nil {
			return state.Page{}, domain.NewError(domain.ErrorInvalid, "invalid state cursor snapshot")
		}
		object, err := e.backend.Get(ctx, key)
		if err != nil {
			return state.Page{}, err
		}
		var envelope storageformat.Envelope
		var record storageformat.StateVersionRecord
		if err := storageformat.DecodeEnvelope(object.Body, key, stateVersionSchema, &envelope, &record); err != nil {
			return state.Page{}, err
		}
		logical, err := parseExistingStateKey(record.LogicalKey)
		if err != nil || record.SchemaVersion != 1 || !strings.HasPrefix(record.LogicalKey, cursor.Prefix) || record.LogicalVersion == "" || state.Version(record.LogicalVersion) == "" || storageformat.StateVersionKey(strings.SplitN(record.LogicalKey, "/", 2)[0], record.LogicalKey, record.LogicalVersion) != key {
			return state.Page{}, domain.NewError(domain.ErrorInvalid, "invalid state cursor snapshot")
		}
		items = append(items, state.Item{Key: logical, Value: state.Value{Data: append([]byte(nil), record.Data...), Version: state.Version(record.LogicalVersion)}})
	}
	next := ""
	if end < len(cursor.Snapshots) {
		cursor.Index = end
		var err error
		next, err = e.encodeStateListCursor(cursor)
		if err != nil {
			return state.Page{}, err
		}
	}
	return state.Page{Items: items, NextCursor: next}, nil
}

func (e *Engine) Create(ctx context.Context, key state.Key, data []byte) (state.Version, error) {
	if err := validateStateMutation(key, data); err != nil {
		return "", err
	}
	objectKey := canonicalStateKey(key)
	body, err := storageformat.EncodeEnvelope(stateRecordSchema, objectKey, 1, storageformat.StateRecord{SchemaVersion: 1, LogicalKey: key.String(), Data: append([]byte(nil), data...)})
	if err != nil {
		return "", err
	}
	snapshot, err := stateVersionObject(key, envelopeVersion(body), data)
	if err != nil {
		return "", err
	}
	intent := storageformat.MutationIntent{Action: storageformat.MutationCreate, TargetKey: objectKey.String(), TargetBody: body, Prerequisites: []storageformat.MutationObject{snapshot}}
	var result state.Version
	err = e.withAdmission(ctx, intent, func() error {
		if err := e.ensureMutationPrerequisites(ctx, intent.Prerequisites); err != nil {
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
	objectKey := canonicalStateKey(key)
	object, err := e.backend.Get(ctx, objectKey)
	if err != nil {
		return "", err
	}
	_, envelope, err := decodeStateObject(object, key)
	if err != nil {
		return "", err
	}
	if state.Version(envelope.LogicalVersion) != current {
		return "", domain.NewError(domain.ErrorPreconditionFailed, "stale state version")
	}
	body, err := storageformat.EncodeEnvelope(stateRecordSchema, objectKey, envelope.Revision+1, storageformat.StateRecord{SchemaVersion: 1, LogicalKey: key.String(), Data: append([]byte(nil), data...)})
	if err != nil {
		return "", err
	}
	snapshot, err := stateVersionObject(key, envelopeVersion(body), data)
	if err != nil {
		return "", err
	}
	intent := storageformat.MutationIntent{Action: storageformat.MutationCAS, TargetKey: objectKey.String(), ExpectedLogicalVersion: string(current), TargetBody: body, Prerequisites: []storageformat.MutationObject{snapshot}}
	var result state.Version
	err = e.withAdmission(ctx, intent, func() error {
		if err := e.ensureMutationPrerequisites(ctx, intent.Prerequisites); err != nil {
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

func stateVersionObject(key state.Key, version state.Version, data []byte) (storageformat.MutationObject, error) {
	namespace := strings.SplitN(key.String(), "/", 2)[0]
	objectKey := storageformat.StateVersionKey(namespace, key.String(), string(version))
	body, err := storageformat.EncodeEnvelope(stateVersionSchema, objectKey, 1, storageformat.StateVersionRecord{
		SchemaVersion: 1, LogicalKey: key.String(), LogicalVersion: string(version), Data: append([]byte(nil), data...),
	})
	if err != nil {
		return storageformat.MutationObject{}, err
	}
	return storageformat.MutationObject{Key: objectKey.String(), Body: body}, nil
}

func (e *Engine) encodeStateListCursor(cursor stateListCursor) (string, error) {
	body, err := storageformat.EncodeCanonical(cursor)
	if err != nil {
		return "", err
	}
	random, err := e.ids.BearerToken()
	if err != nil {
		return "", err
	}
	nonceMaterial, err := base64.RawURLEncoding.DecodeString(random)
	if err != nil || len(nonceMaterial) < e.cursorAEAD.NonceSize() {
		return "", domain.NewError(domain.ErrorInternal, "secure cursor randomness unavailable")
	}
	nonce := nonceMaterial[:e.cursorAEAD.NonceSize()]
	sealed := e.cursorAEAD.Seal(append([]byte(nil), nonce...), nonce, body, []byte("endlessfs-state-cursor-v1"))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (e *Engine) decodeStateListCursor(value string) (stateListCursor, error) {
	sealed, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(sealed) <= e.cursorAEAD.NonceSize() {
		return stateListCursor{}, err
	}
	nonceSize := e.cursorAEAD.NonceSize()
	body, err := e.cursorAEAD.Open(nil, sealed[:nonceSize], sealed[nonceSize:], []byte("endlessfs-state-cursor-v1"))
	if err != nil {
		return stateListCursor{}, domain.NewError(domain.ErrorInvalid, "invalid state cursor")
	}
	var cursor stateListCursor
	if err := decodeCanonicalValue(body, &cursor); err != nil || cursor.SchemaVersion != 1 || cursor.GateEpoch == 0 || cursor.GateVersion == "" || cursor.ExpiresAt.IsZero() || len(cursor.Snapshots) == 0 {
		return stateListCursor{}, domain.NewError(domain.ErrorInvalid, "invalid state cursor")
	}
	return cursor, nil
}

func (e *Engine) Delete(ctx context.Context, key state.Key, current state.Version) error {
	if err := validateStateKey(key); err != nil {
		return err
	}
	if current == "" {
		return domain.NewError(domain.ErrorInvalid, "current state version is required")
	}
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
	intent := storageformat.MutationIntent{Action: storageformat.MutationDelete, TargetKey: objectKey.String(), ExpectedLogicalVersion: string(current)}
	return e.withAdmission(ctx, intent, func() error {
		return e.backend.Delete(ctx, objectKey, objectstore.DeleteCondition{Version: object.Version})
	})
}

func (e *Engine) listAll(ctx context.Context, prefix string) ([]objectstore.ObjectInfo, error) {
	return listAllFrom(ctx, e.backend, prefix)
}

func listAllFrom(ctx context.Context, backend objectstore.Backend, prefix string) ([]objectstore.ObjectInfo, error) {
	request := objectstore.ListRequest{Prefix: prefix, Limit: 1000}
	var result []objectstore.ObjectInfo
	for {
		page, err := backend.List(ctx, request)
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
