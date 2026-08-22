package portable

import (
	"context"
	"encoding/base64"
	"errors"
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
	Namespace     string    `json:"namespace"`
	RootNodeID    string    `json:"rootNodeID"`
	RootDigest    string    `json:"rootDigest"`
	RootCount     uint64    `json:"rootCount"`
	After         string    `json:"after"`
	GateEpoch     uint64    `json:"gateEpoch"`
	GateVersion   string    `json:"gateVersion"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

func (e *Engine) Get(ctx context.Context, key state.Key) (state.Value, error) {
	if err := validateStateKey(key); err != nil {
		return state.Value{}, err
	}
	entry, err := e.stateIndexEntry(ctx, key)
	if err != nil {
		return state.Value{}, err
	}
	return e.readIndexedStateValue(ctx, entry)
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
	namespace := strings.SplitN(prefix.String(), "/", 2)[0]
	_, gateEnvelope, gate, err := e.readGate(ctx)
	if err != nil {
		return state.Page{}, err
	}
	rootSnapshot, err := e.readStateIndexRoot(ctx, namespace)
	if err != nil {
		return state.Page{}, err
	}
	root := rootSnapshot.root
	after := ""
	if request.Cursor != "" {
		cursor, err := e.decodeStateListCursor(request.Cursor)
		if err != nil || cursor.Prefix != prefix.String() || cursor.Namespace != namespace || cursor.Limit != limit || cursor.After == "" || cursor.GateEpoch != gate.Epoch || cursor.GateVersion != gateEnvelope.LogicalVersion || !e.clock.Now().Before(cursor.ExpiresAt) {
			return state.Page{}, domain.NewError(domain.ErrorInvalid, "invalid or out-of-scope state cursor")
		}
		root = storageformat.StateIndexRoot{SchemaVersion: 1, Namespace: namespace, NodeID: cursor.RootNodeID, NodeDigest: cursor.RootDigest, EntryCount: cursor.RootCount}
		after = cursor.After
	}
	entries, err := e.collectStateIndexEntries(ctx, root, prefix.String(), after, limit+1)
	if err != nil {
		return state.Page{}, err
	}
	hasMore := len(entries) > limit
	if hasMore {
		entries = entries[:limit]
	}
	page := state.Page{Items: make([]state.Item, 0, len(entries))}
	for _, entry := range entries {
		logical, err := parseExistingStateKey(entry.LogicalKey)
		if err != nil {
			return state.Page{}, err
		}
		value, err := e.readIndexedStateValue(ctx, entry)
		if err != nil {
			return state.Page{}, err
		}
		page.Items = append(page.Items, state.Item{Key: logical, Value: value})
	}
	if hasMore {
		cursor := stateListCursor{
			SchemaVersion: 3, Prefix: prefix.String(), Limit: limit, Namespace: namespace,
			RootNodeID: root.NodeID, RootDigest: root.NodeDigest, RootCount: root.EntryCount,
			After: entries[len(entries)-1].LogicalKey, GateEpoch: gate.Epoch, GateVersion: gateEnvelope.LogicalVersion, ExpiresAt: e.clock.Now().UTC().Add(e.cursorTTL),
		}
		page.NextCursor, err = e.encodeStateListCursor(cursor)
		if err != nil {
			return state.Page{}, err
		}
	}
	return page, nil
}

func (e *Engine) Create(ctx context.Context, key state.Key, data []byte) (state.Version, error) {
	if err := validateStateMutation(key, data); err != nil {
		return "", err
	}
	if _, err := e.stateIndexEntry(ctx, key); err == nil {
		return "", domain.NewError(domain.ErrorConflict, "state key already exists")
	} else if !errors.Is(err, domain.ErrNotFound) {
		return "", err
	}
	version, err := e.newStateVersion(key, data)
	if err != nil {
		return "", err
	}
	return e.mutateIndexedState(ctx, key, "", version, data, false)
}

func (e *Engine) CompareAndSwap(ctx context.Context, key state.Key, current state.Version, data []byte) (state.Version, error) {
	if err := validateStateMutation(key, data); err != nil {
		return "", err
	}
	if current == "" {
		return "", domain.NewError(domain.ErrorInvalid, "current state version is required")
	}
	entry, err := e.stateIndexEntry(ctx, key)
	if err != nil {
		return "", err
	}
	if state.Version(entry.LogicalVersion) != current {
		return "", domain.NewError(domain.ErrorPreconditionFailed, "stale state version")
	}
	version, err := e.newStateVersion(key, data)
	if err != nil {
		return "", err
	}
	return e.mutateIndexedState(ctx, key, current, version, data, false)
}

func (e *Engine) Delete(ctx context.Context, key state.Key, current state.Version) error {
	if err := validateStateKey(key); err != nil {
		return err
	}
	if current == "" {
		return domain.NewError(domain.ErrorInvalid, "current state version is required")
	}
	entry, err := e.stateIndexEntry(ctx, key)
	if err != nil {
		return err
	}
	if state.Version(entry.LogicalVersion) != current {
		return domain.NewError(domain.ErrorPreconditionFailed, "stale state version")
	}
	_, err = e.mutateIndexedState(ctx, key, current, "", nil, true)
	return err
}

func (e *Engine) mutateIndexedState(ctx context.Context, key state.Key, expected, next state.Version, data []byte, remove bool) (state.Version, error) {
	prepared, err := e.prepareStateIndexMutation(ctx, key, string(next), remove)
	if err != nil {
		return "", err
	}
	if expected == "" && !remove {
		if _, err := e.stateIndexEntryAtRoot(ctx, prepared.snapshot.root, key.String()); err == nil {
			return "", domain.NewError(domain.ErrorConflict, "state key already exists")
		} else if !errors.Is(err, domain.ErrNotFound) {
			return "", err
		}
	}
	if expected != "" {
		current, err := e.stateIndexEntryAtRoot(ctx, prepared.snapshot.root, key.String())
		if err != nil {
			return "", err
		}
		if state.Version(current.LogicalVersion) != expected {
			return "", domain.NewError(domain.ErrorPreconditionFailed, "state index changed before mutation")
		}
	}
	prerequisites := append([]storageformat.MutationObject(nil), prepared.prerequisites...)
	if !remove {
		snapshot, err := stateVersionObject(key, next, data)
		if err != nil {
			return "", err
		}
		prerequisites = append(prerequisites, snapshot)
	}
	prerequisites, err = normalizeMutationObjects(prerequisites)
	if err != nil {
		return "", err
	}
	rootKey := storageformat.StateIndexRootKey(stateNamespace(key))
	intent := storageformat.MutationIntent{Action: storageformat.MutationCreate, TargetKey: rootKey.String(), TargetBody: prepared.rootBody, Prerequisites: prerequisites}
	condition := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
	if prepared.snapshot.exists {
		intent.Action = storageformat.MutationCAS
		intent.ExpectedLogicalVersion = prepared.snapshot.envelope.LogicalVersion
		condition = objectstore.PutCondition{Mode: objectstore.PutMatch, Version: prepared.snapshot.object.Version}
	}
	err = e.withAdmission(ctx, intent, func() error {
		if err := e.ensureMutationPrerequisites(ctx, prerequisites); err != nil {
			return err
		}
		_, err := e.backend.Put(ctx, rootKey, prepared.rootBody, condition)
		return err
	})
	if expected == "" && !remove && (errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrConflict)) {
		if _, lookupErr := e.stateIndexEntry(ctx, key); lookupErr == nil {
			return "", domain.NewError(domain.ErrorConflict, "state key was created concurrently")
		} else if !errors.Is(lookupErr, domain.ErrNotFound) {
			return "", lookupErr
		}
	}
	return next, err
}

func (e *Engine) newStateVersion(key state.Key, data []byte) (state.Version, error) {
	nonce, err := e.ids.OpaqueID()
	if err != nil {
		return "", err
	}
	body, err := storageformat.EncodeCanonical(struct {
		Key   string `json:"key"`
		Nonce string `json:"nonce"`
		Data  []byte `json:"data"`
	}{key.String(), nonce, data})
	if err != nil {
		return "", err
	}
	return state.Version(storageformat.Digest(append([]byte("endlessfs-state-value-v2\x00"), body...))), nil
}

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
	sealed := e.cursorAEAD.Seal(append([]byte(nil), nonce...), nonce, body, []byte("endlessfs-state-cursor-v3"))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (e *Engine) decodeStateListCursor(value string) (stateListCursor, error) {
	sealed, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(sealed) <= e.cursorAEAD.NonceSize() {
		return stateListCursor{}, domain.NewError(domain.ErrorInvalid, "invalid state cursor")
	}
	nonceSize := e.cursorAEAD.NonceSize()
	body, err := e.cursorAEAD.Open(nil, sealed[:nonceSize], sealed[nonceSize:], []byte("endlessfs-state-cursor-v3"))
	if err != nil {
		return stateListCursor{}, domain.NewError(domain.ErrorInvalid, "invalid state cursor")
	}
	var cursor stateListCursor
	if err := decodeCanonicalValue(body, &cursor); err != nil || cursor.SchemaVersion != 3 || cursor.Namespace == "" || cursor.RootNodeID == "" || cursor.RootDigest == "" || cursor.RootCount == 0 || cursor.GateEpoch == 0 || cursor.GateVersion == "" || cursor.ExpiresAt.IsZero() {
		return stateListCursor{}, domain.NewError(domain.ErrorInvalid, "invalid state cursor")
	}
	return cursor, nil
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

func parseExistingStateKey(value string) (state.Key, error) {
	parts := strings.Split(value, "/")
	if len(parts) < 1 {
		return state.Key{}, domain.NewError(domain.ErrorInvalid, "invalid stored state key")
	}
	namespace := state.Namespace(parts[0])
	decoded := make([]string, 0, len(parts)-1)
	for _, part := range parts[1:] {
		value, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil || base64.RawURLEncoding.EncodeToString(value) != part {
			return state.Key{}, domain.NewError(domain.ErrorInvalid, "invalid stored state key")
		}
		decoded = append(decoded, string(value))
	}
	key, err := state.NewKey(namespace, decoded...)
	if err != nil || key.String() != value {
		return state.Key{}, domain.NewError(domain.ErrorInvalid, "invalid stored state key")
	}
	return key, nil
}
