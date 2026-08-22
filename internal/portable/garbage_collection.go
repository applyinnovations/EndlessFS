package portable

import (
	"context"
	"errors"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const (
	garbageCollectionSessionSchema = "garbage-collection-session-v1"
	garbageCollectionMarkSchema    = "garbage-collection-mark-v1"
	garbageCollectionMarking       = "marking"
	garbageCollectionSweeping      = "sweeping"
	garbageCollectionCleanup       = "cleanup"
	garbageCollectionStateRole     = "state"
	garbageCollectionFileRole      = "files"
)

type garbageCollectionSessionSnapshot struct {
	object   objectstore.Object
	envelope storageformat.Envelope
	value    storageformat.GarbageCollectionSession
}

func (e *Engine) runGarbageCollection(ctx context.Context, checkpointID string, gateEpoch uint64, gateVersion string) error {
	session, err := e.readOrCreateGarbageCollectionSession(ctx, checkpointID, gateEpoch, gateVersion)
	if err != nil {
		return err
	}
	if session.value.Phase == garbageCollectionMarking {
		if err := e.markGarbageCollectionRoots(ctx, session.value); err != nil {
			return err
		}
		session.value.Phase = garbageCollectionSweeping
		session.value.SweepIndex = 0
		session.value.After = ""
		session, err = e.updateGarbageCollectionSession(ctx, session)
		if err != nil {
			return err
		}
	}
	if session.value.Phase == garbageCollectionSweeping {
		session, err = e.sweepGarbageCollection(ctx, session)
		if err != nil {
			return err
		}
	}
	if session.value.Phase != garbageCollectionCleanup {
		return domain.NewError(domain.ErrorInvalid, "invalid garbage collection phase")
	}
	if err := visitObjectPages(ctx, e.backend, storageformat.GarbageCollectionMarkPrefix(checkpointID), func(info objectstore.ObjectInfo) error {
		if err := e.backend.Delete(ctx, info.Key, objectstore.DeleteCondition{Version: info.Version}); err != nil && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	if err := e.backend.Delete(ctx, session.object.Key, objectstore.DeleteCondition{Version: session.object.Version}); err != nil && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrPreconditionFailed) {
		return err
	}
	return nil
}

func (e *Engine) readOrCreateGarbageCollectionSession(ctx context.Context, checkpointID string, gateEpoch uint64, gateVersion string) (garbageCollectionSessionSnapshot, error) {
	key := storageformat.GarbageCollectionSessionKey(checkpointID)
	object, err := e.backend.Get(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		value := storageformat.GarbageCollectionSession{
			SchemaVersion: 1, CheckpointID: checkpointID, GateEpoch: gateEpoch, GateVersion: gateVersion,
			Phase: garbageCollectionMarking, UpdatedAt: e.clock.Now().UTC(),
		}
		body, encodeErr := storageformat.EncodeEnvelope(garbageCollectionSessionSchema, key, 1, value)
		if encodeErr != nil {
			return garbageCollectionSessionSnapshot{}, encodeErr
		}
		version, putErr := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
		if putErr == nil {
			return garbageCollectionSessionSnapshot{object: objectstore.Object{Key: key, Body: body, Version: version, Size: int64(len(body))}, envelope: storageformat.Envelope{Revision: 1}, value: value}, nil
		}
		if !errors.Is(putErr, domain.ErrConflict) && !errors.Is(putErr, domain.ErrPreconditionFailed) {
			return garbageCollectionSessionSnapshot{}, putErr
		}
		object, err = e.backend.Get(ctx, key)
	}
	if err != nil {
		return garbageCollectionSessionSnapshot{}, err
	}
	var envelope storageformat.Envelope
	var value storageformat.GarbageCollectionSession
	if err := storageformat.DecodeEnvelope(object.Body, key, garbageCollectionSessionSchema, &envelope, &value); err != nil {
		return garbageCollectionSessionSnapshot{}, err
	}
	if value.SchemaVersion != 1 || value.CheckpointID != checkpointID || value.GateEpoch != gateEpoch || value.GateVersion != gateVersion || value.UpdatedAt.IsZero() || value.SweepIndex < 0 || value.Phase != garbageCollectionMarking && value.Phase != garbageCollectionSweeping && value.Phase != garbageCollectionCleanup {
		return garbageCollectionSessionSnapshot{}, domain.NewError(domain.ErrorInvalid, "invalid garbage collection session")
	}
	return garbageCollectionSessionSnapshot{object: object, envelope: envelope, value: value}, nil
}

func (e *Engine) updateGarbageCollectionSession(ctx context.Context, session garbageCollectionSessionSnapshot) (garbageCollectionSessionSnapshot, error) {
	session.value.UpdatedAt = e.clock.Now().UTC()
	body, err := storageformat.EncodeEnvelope(garbageCollectionSessionSchema, session.object.Key, session.envelope.Revision+1, session.value)
	if err != nil {
		return garbageCollectionSessionSnapshot{}, err
	}
	version, err := e.backend.Put(ctx, session.object.Key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: session.object.Version})
	if err != nil {
		if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrNotFound) {
			return garbageCollectionSessionSnapshot{}, domain.NewError(domain.ErrorUnavailable, "garbage collection session changed concurrently")
		}
		return garbageCollectionSessionSnapshot{}, err
	}
	session.object.Body, session.object.Version, session.object.Size = body, version, int64(len(body))
	session.envelope.Revision++
	return session, nil
}

func (e *Engine) ensureGarbageCollectionMark(ctx context.Context, session storageformat.GarbageCollectionSession, role string, target objectstore.Key) error {
	if role != garbageCollectionStateRole && role != garbageCollectionFileRole {
		return domain.NewError(domain.ErrorInvalid, "invalid garbage collection mark role")
	}
	key := storageformat.GarbageCollectionMarkKey(session.CheckpointID, role, target.String())
	value := storageformat.GarbageCollectionMark{SchemaVersion: 1, CheckpointID: session.CheckpointID, GateEpoch: session.GateEpoch, GateVersion: session.GateVersion, Role: role, TargetKey: target.String()}
	body, err := storageformat.EncodeEnvelope(garbageCollectionMarkSchema, key, 1, value)
	if err != nil {
		return err
	}
	if _, err := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err == nil {
		return nil
	} else if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
		return err
	}
	existing, err := e.backend.Get(ctx, key)
	if err != nil {
		return err
	}
	if string(existing.Body) != string(body) {
		return domain.NewError(domain.ErrorInvalid, "garbage collection mark collision")
	}
	return nil
}

func (e *Engine) garbageCollectionMarked(ctx context.Context, session storageformat.GarbageCollectionSession, role string, target objectstore.Key) (bool, error) {
	key := storageformat.GarbageCollectionMarkKey(session.CheckpointID, role, target.String())
	object, err := e.backend.Get(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var envelope storageformat.Envelope
	var mark storageformat.GarbageCollectionMark
	if err := storageformat.DecodeEnvelope(object.Body, key, garbageCollectionMarkSchema, &envelope, &mark); err != nil {
		return false, err
	}
	if mark.SchemaVersion != 1 || mark.CheckpointID != session.CheckpointID || mark.GateEpoch != session.GateEpoch || mark.GateVersion != session.GateVersion || mark.Role != role || mark.TargetKey != target.String() {
		return false, domain.NewError(domain.ErrorInvalid, "invalid garbage collection mark")
	}
	return true, nil
}

func (e *Engine) markGarbageCollectionRoots(ctx context.Context, session storageformat.GarbageCollectionSession) error {
	if err := visitObjectPages(ctx, e.backend, storageformat.FilesystemPrefix(), func(info objectstore.ObjectInfo) error {
		userValue, areaValue, directoryID, matched, err := storageformat.ParseDirectoryRootKey(info.Key)
		if err != nil || !matched || directoryID != storageformat.RootDirectoryID {
			return err
		}
		userID, err := domain.ParseUserID(userValue)
		if err != nil {
			return err
		}
		area := domain.AreaLive
		if areaValue == "trash" {
			area = domain.AreaTrash
		} else if areaValue != "live" {
			return domain.NewError(domain.ErrorInvalid, "invalid filesystem area during garbage collection")
		}
		scope, err := domain.NewScope(userID, area)
		if err != nil {
			return err
		}
		snapshot, err := e.Files().readDirectoryMetadata(ctx, scope, storageformat.RootDirectoryID, true)
		if err != nil {
			return err
		}
		return e.Files().markDirectoryTree(ctx, session, scope, storageformat.RootDirectoryID, snapshot, make(map[string]struct{}))
	}); err != nil {
		return err
	}
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
		if root.SchemaVersion != 1 || root.Namespace == "" || root.EntryCount == 0 && (root.NodeID != "" || root.NodeDigest != "") {
			return domain.NewError(domain.ErrorInvalid, "invalid state index root during garbage collection")
		}
		if root.EntryCount == 0 {
			return nil
		}
		reference, err := e.rootStateIndexChild(ctx, root)
		if err != nil {
			return err
		}
		return e.markStateIndexTree(ctx, session, root.Namespace, reference, make(map[string]struct{}))
	})
}

func (s *FileStore) markDirectoryTree(ctx context.Context, session storageformat.GarbageCollectionSession, scope domain.Scope, directoryID string, snapshot directorySnapshot, visiting map[string]struct{}) error {
	rootKey := storageformat.DirectoryRootKey(scope.UserID().String(), areaName(scope.Area()), directoryID)
	if _, cycle := visiting[rootKey.String()]; cycle {
		return domain.NewError(domain.ErrorInvalid, "directory cycle encountered during garbage collection")
	}
	visiting[rootKey.String()] = struct{}{}
	defer delete(visiting, rootKey.String())
	if snapshot.manifestID != "" {
		if snapshot.manifest.EntryCount > 0 {
			reference, err := s.directoryIndexRoot(ctx, scope, directoryID, snapshot.manifest)
			if err != nil {
				return err
			}
			if err := s.markDirectoryIndexTree(ctx, session, scope, directoryID, reference, visiting, make(map[string]struct{})); err != nil {
				return err
			}
			for _, field := range directorySecondarySorts {
				sortRoot, err := s.directorySortIndexRoot(ctx, scope, directoryID, snapshot.manifest, field)
				if err != nil {
					return err
				}
				if err := s.markDirectorySortIndexTree(ctx, session, scope, directoryID, field, sortRoot, make(map[string]struct{})); err != nil {
					return err
				}
			}
		}
		manifestKey := storageformat.DirectoryManifestKey(scope.UserID().String(), areaName(scope.Area()), directoryID, snapshot.manifestID)
		if err := s.engine.ensureGarbageCollectionMark(ctx, session, garbageCollectionStateRole, manifestKey); err != nil {
			return err
		}
	}
	return s.engine.ensureGarbageCollectionMark(ctx, session, garbageCollectionStateRole, rootKey)
}

func (s *FileStore) markDirectorySortIndexTree(ctx context.Context, session storageformat.GarbageCollectionSession, scope domain.Scope, directoryID string, field domain.SortField, reference storageformat.DirectorySortIndexChild, visiting map[string]struct{}) error {
	key := storageformat.DirectorySortIndexNodeKey(scope.UserID().String(), areaName(scope.Area()), directoryID, field, reference.NodeID)
	if _, cycle := visiting[key.String()]; cycle {
		return domain.NewError(domain.ErrorInvalid, "directory sort-index cycle encountered during garbage collection")
	}
	marked, err := s.engine.garbageCollectionMarked(ctx, session, garbageCollectionStateRole, key)
	if err != nil || marked {
		return err
	}
	visiting[key.String()] = struct{}{}
	defer delete(visiting, key.String())
	node, err := s.readDirectorySortIndexNode(ctx, scope, directoryID, field, reference)
	if err != nil {
		return err
	}
	for _, child := range node.Children {
		if err := s.markDirectorySortIndexTree(ctx, session, scope, directoryID, field, child, visiting); err != nil {
			return err
		}
	}
	return s.engine.ensureGarbageCollectionMark(ctx, session, garbageCollectionStateRole, key)
}

func (s *FileStore) markDirectoryIndexTree(ctx context.Context, session storageformat.GarbageCollectionSession, scope domain.Scope, directoryID string, reference storageformat.DirectoryIndexChild, directoryVisiting, indexVisiting map[string]struct{}) error {
	key := storageformat.DirectoryIndexNodeKey(scope.UserID().String(), areaName(scope.Area()), directoryID, reference.NodeID)
	if _, cycle := indexVisiting[key.String()]; cycle {
		return domain.NewError(domain.ErrorInvalid, "directory index cycle encountered during garbage collection")
	}
	marked, err := s.engine.garbageCollectionMarked(ctx, session, garbageCollectionStateRole, key)
	if err != nil || marked {
		return err
	}
	indexVisiting[key.String()] = struct{}{}
	defer delete(indexVisiting, key.String())
	node, err := s.readDirectoryIndexNode(ctx, scope, directoryID, reference)
	if err != nil {
		return err
	}
	for _, child := range node.Children {
		if err := s.markDirectoryIndexTree(ctx, session, scope, directoryID, child, directoryVisiting, indexVisiting); err != nil {
			return err
		}
	}
	for _, entry := range node.Entries {
		if entry.Kind == domain.EntryFile {
			blobKey := storageformat.BlobKey(scope.UserID().String(), entry.BlobID)
			if err := s.engine.ensureGarbageCollectionMark(ctx, session, garbageCollectionFileRole, blobKey); err != nil {
				return err
			}
			continue
		}
		child, err := s.readDirectoryMetadata(ctx, scope, entry.DirectoryID, false)
		if err != nil {
			return err
		}
		if child.recursiveBytes != entry.Size || child.recursiveFileCount != entry.FileCount || child.contentDigest != entry.ContentDigest {
			return domain.NewError(domain.ErrorInvalid, "directory aggregate mismatch during garbage collection")
		}
		if err := s.markDirectoryTree(ctx, session, scope, entry.DirectoryID, child, directoryVisiting); err != nil {
			return err
		}
	}
	return s.engine.ensureGarbageCollectionMark(ctx, session, garbageCollectionStateRole, key)
}

func (e *Engine) markStateIndexTree(ctx context.Context, session storageformat.GarbageCollectionSession, namespace string, reference storageformat.StateIndexChild, visiting map[string]struct{}) error {
	key := storageformat.StateIndexNodeKey(namespace, reference.NodeID)
	if _, cycle := visiting[key.String()]; cycle {
		return domain.NewError(domain.ErrorInvalid, "state index cycle encountered during garbage collection")
	}
	marked, err := e.garbageCollectionMarked(ctx, session, garbageCollectionStateRole, key)
	if err != nil || marked {
		return err
	}
	visiting[key.String()] = struct{}{}
	defer delete(visiting, key.String())
	node, err := e.readStateIndexNode(ctx, namespace, reference)
	if err != nil {
		return err
	}
	for _, child := range node.Children {
		if err := e.markStateIndexTree(ctx, session, namespace, child, visiting); err != nil {
			return err
		}
	}
	for _, entry := range node.Entries {
		versionKey := storageformat.StateVersionKey(namespace, entry.LogicalKey, entry.LogicalVersion)
		if err := e.ensureGarbageCollectionMark(ctx, session, garbageCollectionStateRole, versionKey); err != nil {
			return err
		}
	}
	return e.ensureGarbageCollectionMark(ctx, session, garbageCollectionStateRole, key)
}

type garbageCollectionSweep struct {
	backend objectstore.FileControlBackend
	prefix  string
	role    string
	filter  func(objectstore.Key) bool
}

func (e *Engine) sweepGarbageCollection(ctx context.Context, session garbageCollectionSessionSnapshot) (garbageCollectionSessionSnapshot, error) {
	sweeps := []garbageCollectionSweep{
		{backend: e.backend, prefix: storageformat.FilesystemPrefix(), role: garbageCollectionStateRole, filter: func(key objectstore.Key) bool { return strings.Contains(key.String(), "/dirs/") }},
		{backend: e.backend, prefix: storageformat.StateIndexRootPrefix(), role: garbageCollectionStateRole, filter: func(key objectstore.Key) bool { return strings.Contains(key.String(), "/nodes/") }},
		{backend: e.backend, prefix: storageformat.StateVersionsPrefix(), role: garbageCollectionStateRole, filter: func(objectstore.Key) bool { return true }},
		{backend: e.fileBackend, prefix: storageformat.FilesystemPrefix(), role: garbageCollectionFileRole, filter: func(key objectstore.Key) bool { return strings.Contains(key.String(), "/blobs/") }},
	}
	if session.value.SweepIndex > len(sweeps) {
		return garbageCollectionSessionSnapshot{}, domain.NewError(domain.ErrorInvalid, "invalid garbage collection sweep position")
	}
	for session.value.SweepIndex < len(sweeps) {
		sweep := sweeps[session.value.SweepIndex]
		page, err := sweep.backend.List(ctx, objectstore.ListRequest{Prefix: sweep.prefix, Limit: 256, After: session.value.After})
		if err != nil {
			return garbageCollectionSessionSnapshot{}, err
		}
		for _, info := range page.Objects {
			session.value.After = info.Key.String()
			if !sweep.filter(info.Key) {
				continue
			}
			marked, err := e.garbageCollectionMarked(ctx, session.value, sweep.role, info.Key)
			if err != nil {
				return garbageCollectionSessionSnapshot{}, err
			}
			if !marked {
				if err := sweep.backend.Delete(ctx, info.Key, objectstore.DeleteCondition{Version: info.Version}); err != nil && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrPreconditionFailed) {
					return garbageCollectionSessionSnapshot{}, err
				}
			}
		}
		if len(page.Objects) == 0 || page.NextCursor == "" {
			session.value.SweepIndex++
			session.value.After = ""
		}
		session, err = e.updateGarbageCollectionSession(ctx, session)
		if err != nil {
			return garbageCollectionSessionSnapshot{}, err
		}
	}
	session.value.Phase = garbageCollectionCleanup
	return e.updateGarbageCollectionSession(ctx, session)
}
