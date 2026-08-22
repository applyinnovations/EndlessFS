package portable

import (
	"context"
	"errors"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func (e *Engine) migrateLegacyStateIndexes(ctx context.Context) error {
	return visitObjectPages(ctx, e.backend, storageformat.StateRecordsPrefix(), func(info objectstore.ObjectInfo) error {
		object, err := e.backend.Get(ctx, info.Key)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var envelope storageformat.Envelope
		var record storageformat.StateRecord
		if err := storageformat.DecodeEnvelope(object.Body, info.Key, stateRecordSchema, &envelope, &record); err != nil {
			return err
		}
		logical, err := parseExistingStateKey(record.LogicalKey)
		if err != nil || record.SchemaVersion != 1 || canonicalStateKey(logical) != info.Key {
			return domain.NewError(domain.ErrorInvalid, "invalid legacy state record during index migration")
		}
		snapshot, err := stateVersionObject(logical, state.Version(envelope.LogicalVersion), record.Data)
		if err != nil {
			return err
		}
		if err := e.ensureMutationPrerequisites(ctx, []storageformat.MutationObject{snapshot}); err != nil {
			return err
		}
		migrated := false
		for range 16 {
			indexed, lookupErr := e.stateIndexEntry(ctx, logical)
			if lookupErr == nil {
				if indexed.LogicalVersion != envelope.LogicalVersion {
					return domain.NewError(domain.ErrorInvalid, "state index disagrees with legacy state")
				}
				value, valueErr := e.readIndexedStateValue(ctx, indexed)
				if valueErr != nil || string(value.Data) != string(record.Data) {
					if valueErr != nil {
						return valueErr
					}
					return domain.NewError(domain.ErrorInvalid, "state index value disagrees with legacy state")
				}
				migrated = true
				break
			}
			if !errors.Is(lookupErr, domain.ErrNotFound) {
				return lookupErr
			}
			prepared, err := e.prepareStateIndexMutation(ctx, logical, envelope.LogicalVersion, false)
			if err != nil {
				return err
			}
			if err := e.ensureMutationPrerequisites(ctx, prepared.prerequisites); err != nil {
				return err
			}
			key := storageformat.StateIndexRootKey(stateNamespace(logical))
			condition := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
			if prepared.snapshot.exists {
				condition = objectstore.PutCondition{Mode: objectstore.PutMatch, Version: prepared.snapshot.object.Version}
			}
			if _, err := e.backend.Put(ctx, key, prepared.rootBody, condition); err == nil {
				migrated = true
				break
			} else if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
				return err
			}
		}
		if !migrated {
			return domain.NewError(domain.ErrorUnavailable, "state index migration remained contended")
		}
		if err := e.backend.Delete(ctx, info.Key, objectstore.DeleteCondition{Version: object.Version}); err != nil && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
		return nil
	})
}

func (e *Engine) verifyStateIndexes(ctx context.Context) error {
	return visitObjectPages(ctx, e.backend, storageformat.StateIndexRootPrefix(), func(info objectstore.ObjectInfo) error {
		if !strings.HasSuffix(info.Key.String(), "/root.json") {
			return nil
		}
		object, err := e.backend.Get(ctx, info.Key)
		if err != nil {
			return err
		}
		var envelope storageformat.Envelope
		var root storageformat.StateIndexRoot
		if err := storageformat.DecodeEnvelope(object.Body, info.Key, stateIndexRootSchema, &envelope, &root); err != nil {
			return err
		}
		if root.SchemaVersion != 1 || storageformat.StateIndexRootKey(root.Namespace) != info.Key {
			return domain.NewError(domain.ErrorInvalid, "invalid migrated state index root")
		}
		var count uint64
		after := ""
		prefix := root.Namespace + "/"
		for {
			entries, err := e.collectStateIndexEntries(ctx, root, prefix, after, 256)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if _, err := e.readIndexedStateValue(ctx, entry); err != nil {
					return err
				}
				count++
				after = entry.LogicalKey
			}
			if len(entries) < 256 {
				break
			}
		}
		if count != root.EntryCount {
			return domain.NewError(domain.ErrorInvalid, "migrated state index count mismatch")
		}
		return nil
	})
}
