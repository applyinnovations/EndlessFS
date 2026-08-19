package portable

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const (
	recursiveByteMigrationCheckpointID = "automatic-recursive-byte-aggregates-v1"

	StepMigrationAfterDetection              = "migration:recursive-bytes:after-detection"
	StepMigrationAfterGateClosed             = "migration:recursive-bytes:after-gate-closed"
	StepMigrationAfterDirectoryPrerequisites = "migration:recursive-bytes:after-directory-prerequisites"
	StepMigrationAfterDirectoryRoot          = "migration:recursive-bytes:after-directory-root"
	StepMigrationAfterDirectories            = "migration:recursive-bytes:after-directories"
	StepMigrationAfterWriterSet              = "migration:recursive-bytes:after-writer-set"
	StepMigrationAfterSuperblock             = "migration:recursive-bytes:after-superblock"
	StepMigrationAfterGateBinding            = "migration:recursive-bytes:after-gate-binding"
	StepMigrationAfterCheckpoint             = "migration:recursive-bytes:after-checkpoint"
)

type legacyDirectoryRoot struct {
	SchemaVersion int                        `json:"schemaVersion"`
	DirectoryID   string                     `json:"directoryID"`
	ManifestID    string                     `json:"manifestID"`
	Pending       *legacyDirectoryTransition `json:"pending,omitempty"`
}

type legacyDirectoryTransition struct {
	OperationID    string `json:"operationID"`
	Fence          uint64 `json:"fence"`
	PreManifestID  string `json:"preManifestID,omitempty"`
	PostManifestID string `json:"postManifestID"`
}

type legacyDirectoryManifest struct {
	SchemaVersion int       `json:"schemaVersion"`
	DirectoryID   string    `json:"directoryID"`
	ManifestID    string    `json:"manifestID"`
	PageIDs       []string  `json:"pageIDs"`
	EntryCount    int       `json:"entryCount"`
	CreatedAt     time.Time `json:"createdAt"`
}

type migrationDirectoryRoot struct {
	object         objectstore.Object
	envelope       storageformat.Envelope
	manifestID     string
	recursiveBytes int64
	current        bool
}

type migrationDirectoryManifest struct {
	manifest storageformat.DirectoryManifest
	current  bool
}

type migrationScope struct {
	scope domain.Scope
	roots map[string]struct{}
}

type migrationWalk struct {
	engine  *Engine
	group   migrationScope
	state   map[string]uint8
	totals  map[string]int64
	parents map[string]string
}

func legacyRecursiveByteFeatures(features []string) []string {
	legacy := make([]string, 0, len(features))
	for _, feature := range features {
		if feature != storageformat.FeatureRecursiveBytes {
			legacy = append(legacy, feature)
		}
	}
	return legacy
}

func (e *Engine) migrateRecursiveByteAggregates(ctx context.Context, superblockObject objectstore.Object, superblock storageformat.Superblock) error {
	if err := e.step(ctx, StepMigrationAfterDetection); err != nil {
		return err
	}
	complete, err := e.recursiveByteMigrationComplete(ctx)
	if err == nil && complete {
		return nil
	}
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if err := e.verifyMigrationWriterSet(ctx); err != nil {
		return err
	}
	closed, err := e.closeRecursiveByteMigrationGate(ctx)
	if err != nil {
		return err
	}
	if !closed {
		return nil
	}
	if err := e.step(ctx, StepMigrationAfterGateClosed); err != nil {
		return err
	}
	if err := e.migrateAllDirectoryAggregates(ctx); err != nil {
		return err
	}
	if err := e.migrateAllDirectoryAggregates(ctx); err != nil {
		return err
	}
	if err := e.step(ctx, StepMigrationAfterDirectories); err != nil {
		return err
	}
	if err := e.activateRecursiveByteWriterSet(ctx); err != nil {
		return err
	}
	if err := e.step(ctx, StepMigrationAfterWriterSet); err != nil {
		return err
	}
	if err := e.activateRecursiveByteSuperblock(ctx, superblockObject, superblock); err != nil {
		return err
	}
	if err := e.step(ctx, StepMigrationAfterSuperblock); err != nil {
		return err
	}
	if err := e.bindMigrationGateToWriterFeatures(ctx); err != nil {
		return err
	}
	if err := e.step(ctx, StepMigrationAfterGateBinding); err != nil {
		return err
	}
	if complete, completeErr := e.recursiveByteMigrationComplete(ctx); completeErr == nil && complete {
		return nil
	}
	if _, err := e.createCheckpointWhileClosed(ctx, recursiveByteMigrationCheckpointID); err != nil {
		if complete, completeErr := e.recursiveByteMigrationComplete(ctx); completeErr == nil && complete {
			return nil
		}
		return err
	}
	if err := e.step(ctx, StepMigrationAfterCheckpoint); err != nil {
		return err
	}
	if err := e.OpenWrites(ctx, recursiveByteMigrationCheckpointID); err != nil {
		if complete, completeErr := e.recursiveByteMigrationComplete(ctx); completeErr == nil && complete {
			return nil
		}
		return err
	}
	return nil
}

func (e *Engine) verifyMigrationWriterSet(ctx context.Context) error {
	_, _, writer, err := e.readStoredWriterSet(ctx)
	if err != nil {
		return err
	}
	legacy := e.writer
	legacy.RequiredFeatures = legacyRecursiveByteFeatures(legacy.RequiredFeatures)
	if !reflect.DeepEqual(writer, legacy) && !reflect.DeepEqual(writer, e.writer) {
		return domain.NewError(domain.ErrorPreconditionFailed, "incompatible portable writer set")
	}
	return nil
}

func (e *Engine) readStoredWriterSet(ctx context.Context) (objectstore.Object, storageformat.Envelope, storageformat.WriterSet, error) {
	object, err := e.backend.Get(ctx, storageformat.WriterSetKey())
	if err != nil {
		return objectstore.Object{}, storageformat.Envelope{}, storageformat.WriterSet{}, err
	}
	var envelope storageformat.Envelope
	var writer storageformat.WriterSet
	if err := storageformat.DecodeEnvelope(object.Body, object.Key, writerSetSchema, &envelope, &writer); err != nil {
		return objectstore.Object{}, storageformat.Envelope{}, storageformat.WriterSet{}, err
	}
	return object, envelope, writer, nil
}

func (e *Engine) closeRecursiveByteMigrationGate(ctx context.Context) (bool, error) {
	for range 16 {
		object, envelope, gate, err := e.readGate(ctx)
		if err != nil {
			return false, err
		}
		if gate.Mode == storageformat.GateOpen && reflect.DeepEqual(gate.WriterFeatures, e.writer.RequiredFeatures) {
			complete, completeErr := e.recursiveByteMigrationComplete(ctx)
			if completeErr != nil {
				return false, completeErr
			}
			if complete {
				return false, nil
			}
			return false, domain.NewError(domain.ErrorPreconditionFailed, "migration gate opened before feature activation completed")
		}
		if gate.Mode != storageformat.GateOpen && gate.CheckpointID != recursiveByteMigrationCheckpointID {
			return false, domain.NewError(domain.ErrorConflict, "write gate is reserved by another maintenance operation")
		}
		if len(gate.WriterFeatures) > 0 && !reflect.DeepEqual(gate.WriterFeatures, legacyRecursiveByteFeatures(e.writer.RequiredFeatures)) && !reflect.DeepEqual(gate.WriterFeatures, e.writer.RequiredFeatures) {
			return false, domain.NewError(domain.ErrorPreconditionFailed, "incompatible write-gate feature binding")
		}
		switch gate.Mode {
		case storageformat.GateOpen:
			gate.Mode = storageformat.GateClosing
			gate.CheckpointID = recursiveByteMigrationCheckpointID
			body, encodeErr := storageformat.EncodeEnvelope(writeGateSchema, object.Key, envelope.Revision+1, gate)
			if encodeErr != nil {
				return false, encodeErr
			}
			_, err = e.backend.Put(ctx, object.Key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version})
			if err == nil || errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrConflict) {
				continue
			}
			return false, err
		case storageformat.GateClosing:
			err = e.finishClosingWrites(ctx, recursiveByteMigrationCheckpointID)
			if err == nil {
				return true, nil
			}
			if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrConflict) {
				continue
			}
			return false, err
		case storageformat.GateClosed:
			return true, nil
		}
	}
	return false, domain.NewError(domain.ErrorUnavailable, "recursive-byte migration gate remained contended")
}

func (e *Engine) migrateAllDirectoryAggregates(ctx context.Context) error {
	infos, err := e.listAll(ctx, storageformat.FilesystemPrefix())
	if err != nil {
		return err
	}
	groups := make(map[string]migrationScope)
	for _, info := range infos {
		userIDValue, areaValue, directoryID, matched, parseErr := storageformat.ParseDirectoryRootKey(info.Key)
		if parseErr != nil {
			return parseErr
		}
		if !matched {
			continue
		}
		userID, parseErr := domain.ParseUserID(userIDValue)
		if parseErr != nil {
			return parseErr
		}
		area := domain.AreaLive
		if areaValue == "trash" {
			area = domain.AreaTrash
		}
		scope, scopeErr := domain.NewScope(userID, area)
		if scopeErr != nil {
			return scopeErr
		}
		groupKey := userIDValue + "\x00" + areaValue
		group := groups[groupKey]
		if group.roots == nil {
			group = migrationScope{scope: scope, roots: make(map[string]struct{})}
		}
		group.roots[directoryID] = struct{}{}
		groups[groupKey] = group
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		group := groups[key]
		if _, found := group.roots[storageformat.RootDirectoryID]; !found {
			return domain.NewError(domain.ErrorInvalid, "directory scope has no canonical root")
		}
		walk := migrationWalk{
			engine: e, group: group, state: make(map[string]uint8, len(group.roots)),
			totals: make(map[string]int64, len(group.roots)), parents: make(map[string]string, len(group.roots)),
		}
		if _, err := walk.directory(ctx, storageformat.RootDirectoryID, ""); err != nil {
			return err
		}
		if len(walk.totals) != len(group.roots) {
			return domain.NewError(domain.ErrorInvalid, "directory scope contains an unreachable directory root")
		}
	}
	return nil
}

func (walk *migrationWalk) directory(ctx context.Context, directoryID, parentID string) (int64, error) {
	if _, found := walk.group.roots[directoryID]; !found {
		return 0, domain.NewError(domain.ErrorInvalid, "directory entry references a missing child root")
	}
	if directoryID == storageformat.RootDirectoryID && parentID != "" {
		return 0, domain.NewError(domain.ErrorInvalid, "directory graph references its area root")
	}
	if parentID != "" {
		if _, found := walk.parents[directoryID]; found {
			return 0, domain.NewError(domain.ErrorInvalid, "directory graph references a child more than once")
		}
		walk.parents[directoryID] = parentID
	}
	switch walk.state[directoryID] {
	case 1:
		return 0, domain.NewError(domain.ErrorInvalid, "directory graph contains a cycle")
	case 2:
		return walk.totals[directoryID], nil
	}
	walk.state[directoryID] = 1
	root, err := walk.engine.readMigrationDirectoryRoot(ctx, walk.group.scope, directoryID)
	if err != nil {
		return 0, err
	}
	manifest, err := walk.engine.readMigrationDirectoryManifest(ctx, walk.group.scope, directoryID, root.manifestID)
	if err != nil {
		return 0, err
	}
	if root.current != manifest.current {
		return 0, domain.NewError(domain.ErrorInvalid, "directory root and manifest migration states differ")
	}
	entries, err := walk.engine.Files().readManifestPageEntries(ctx, walk.group.scope, directoryID, manifest.manifest)
	if err != nil {
		return 0, err
	}
	for index := range entries {
		if entries[index].Kind != domain.EntryDirectory {
			continue
		}
		childTotal, childErr := walk.directory(ctx, entries[index].DirectoryID, directoryID)
		if childErr != nil {
			return 0, childErr
		}
		if root.current && entries[index].Size != childTotal {
			return 0, domain.NewError(domain.ErrorInvalid, "migrated directory entry aggregate mismatch")
		}
		if !root.current {
			entries[index].Size = childTotal
			entries[index].LogicalVersion, err = directoryEntryVersion(entries[index])
			if err != nil {
				return 0, err
			}
		}
	}
	total, err := recursiveByteSize(entries)
	if err != nil {
		return 0, err
	}
	if root.current {
		if root.recursiveBytes != total || manifest.manifest.RecursiveBytes != total {
			return 0, domain.NewError(domain.ErrorInvalid, "migrated directory aggregate mismatch")
		}
		walk.state[directoryID] = 2
		walk.totals[directoryID] = total
		return total, nil
	}
	prepared, err := walk.engine.prepareMigratedDirectory(walk.group.scope, directoryID, entries, root, manifest.manifest.CreatedAt)
	if err != nil {
		return 0, err
	}
	if err := walk.engine.ensureMutationPrerequisites(ctx, prepared.prerequisites); err != nil {
		return 0, err
	}
	if err := walk.engine.step(ctx, StepMigrationAfterDirectoryPrerequisites); err != nil {
		return 0, err
	}
	_, err = walk.engine.backend.Put(ctx, root.object.Key, prepared.rootBody, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: root.object.Version})
	if err != nil {
		if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrConflict) {
			winner, getErr := walk.engine.backend.Get(ctx, root.object.Key)
			if getErr == nil && bytes.Equal(winner.Body, prepared.rootBody) {
				walk.state[directoryID] = 2
				walk.totals[directoryID] = total
				return total, nil
			}
			if getErr != nil {
				return 0, getErr
			}
			return 0, domain.NewError(domain.ErrorInvalid, "directory root changed unexpectedly during migration")
		}
		return 0, err
	}
	if err := walk.engine.step(ctx, StepMigrationAfterDirectoryRoot); err != nil {
		return 0, err
	}
	walk.state[directoryID] = 2
	walk.totals[directoryID] = total
	return total, nil
}

func (e *Engine) readMigrationDirectoryRoot(ctx context.Context, scope domain.Scope, directoryID string) (migrationDirectoryRoot, error) {
	key := storageformat.DirectoryRootKey(scope.UserID().String(), areaName(scope.Area()), directoryID)
	object, err := e.backend.Get(ctx, key)
	if err != nil {
		return migrationDirectoryRoot{}, err
	}
	var envelope storageformat.Envelope
	var current storageformat.DirectoryRoot
	if err := storageformat.DecodeEnvelope(object.Body, key, directoryRootSchema, &envelope, &current); err == nil {
		if current.SchemaVersion != 1 || current.DirectoryID != directoryID || current.ManifestID == "" || current.RecursiveBytes < 0 || current.Pending != nil {
			return migrationDirectoryRoot{}, domain.NewError(domain.ErrorInvalid, "invalid migrated directory root")
		}
		return migrationDirectoryRoot{object: object, envelope: envelope, manifestID: current.ManifestID, recursiveBytes: current.RecursiveBytes, current: true}, nil
	}
	var legacyEnvelope storageformat.Envelope
	var legacy legacyDirectoryRoot
	if err := storageformat.DecodeEnvelope(object.Body, key, directoryRootSchema, &legacyEnvelope, &legacy); err != nil {
		return migrationDirectoryRoot{}, err
	}
	if legacy.SchemaVersion != 1 || legacy.DirectoryID != directoryID || legacy.ManifestID == "" || legacy.Pending != nil {
		return migrationDirectoryRoot{}, domain.NewError(domain.ErrorInvalid, "invalid legacy directory root")
	}
	return migrationDirectoryRoot{object: object, envelope: legacyEnvelope, manifestID: legacy.ManifestID}, nil
}

func (e *Engine) readMigrationDirectoryManifest(ctx context.Context, scope domain.Scope, directoryID, manifestID string) (migrationDirectoryManifest, error) {
	key := storageformat.DirectoryManifestKey(scope.UserID().String(), areaName(scope.Area()), directoryID, manifestID)
	object, err := e.backend.Get(ctx, key)
	if err != nil {
		return migrationDirectoryManifest{}, err
	}
	var envelope storageformat.Envelope
	var current storageformat.DirectoryManifest
	if err := storageformat.DecodeEnvelope(object.Body, key, directoryManifestSchema, &envelope, &current); err == nil {
		if err := validateMigrationManifest(current, directoryID, manifestID); err != nil {
			return migrationDirectoryManifest{}, err
		}
		return migrationDirectoryManifest{manifest: current, current: true}, nil
	}
	var legacyEnvelope storageformat.Envelope
	var legacy legacyDirectoryManifest
	if err := storageformat.DecodeEnvelope(object.Body, key, directoryManifestSchema, &legacyEnvelope, &legacy); err != nil {
		return migrationDirectoryManifest{}, err
	}
	current = storageformat.DirectoryManifest{
		SchemaVersion: legacy.SchemaVersion, DirectoryID: legacy.DirectoryID, ManifestID: legacy.ManifestID,
		PageIDs: append([]string(nil), legacy.PageIDs...), EntryCount: legacy.EntryCount, CreatedAt: legacy.CreatedAt,
	}
	if err := validateMigrationManifest(current, directoryID, manifestID); err != nil {
		return migrationDirectoryManifest{}, err
	}
	return migrationDirectoryManifest{manifest: current}, nil
}

func validateMigrationManifest(manifest storageformat.DirectoryManifest, directoryID, manifestID string) error {
	if manifest.SchemaVersion != 1 || manifest.DirectoryID != directoryID || manifest.ManifestID != manifestID || manifest.EntryCount < 0 || manifest.RecursiveBytes < 0 || len(manifest.PageIDs) == 0 || manifest.CreatedAt.IsZero() {
		return domain.NewError(domain.ErrorInvalid, "invalid directory manifest during migration")
	}
	return nil
}

func (e *Engine) prepareMigratedDirectory(scope domain.Scope, directoryID string, entries []storageformat.DirectoryEntry, root migrationDirectoryRoot, createdAt time.Time) (preparedDirectory, error) {
	if err := validateDirectoryEntries(entries); err != nil {
		return preparedDirectory{}, err
	}
	recursiveBytes, err := recursiveByteSize(entries)
	if err != nil {
		return preparedDirectory{}, err
	}
	rootKey := storageformat.DirectoryRootKey(scope.UserID().String(), areaName(scope.Area()), directoryID)
	identity := rootKey.String() + "\x00" + root.envelope.LogicalVersion
	manifestID := deterministicMigrationID(identity + "\x00manifest")
	pages := make([]storageformat.MutationObject, 0, max(1, (len(entries)+maxEntriesPerPage-1)/maxEntriesPerPage))
	pageIDs := make([]string, 0, cap(pages))
	for start := 0; start < max(1, len(entries)); start += maxEntriesPerPage {
		end := min(start+maxEntriesPerPage, len(entries))
		pageID := deterministicMigrationID(identity + "\x00page\x00" + strconv.Itoa(start/maxEntriesPerPage))
		pageKey := storageformat.DirectoryPageKey(scope.UserID().String(), areaName(scope.Area()), directoryID, pageID)
		body, encodeErr := storageformat.EncodeEnvelope(directoryPageSchema, pageKey, 1, storageformat.DirectoryPage{
			SchemaVersion: 1, DirectoryID: directoryID, PageID: pageID, Entries: append([]storageformat.DirectoryEntry(nil), entries[start:end]...),
		})
		if encodeErr != nil {
			return preparedDirectory{}, encodeErr
		}
		pages = append(pages, storageformat.MutationObject{Key: pageKey.String(), Body: body})
		pageIDs = append(pageIDs, pageID)
		if len(entries) == 0 {
			break
		}
	}
	manifestKey := storageformat.DirectoryManifestKey(scope.UserID().String(), areaName(scope.Area()), directoryID, manifestID)
	manifestBody, err := storageformat.EncodeEnvelope(directoryManifestSchema, manifestKey, 1, storageformat.DirectoryManifest{
		SchemaVersion: 1, DirectoryID: directoryID, ManifestID: manifestID, PageIDs: pageIDs,
		EntryCount: len(entries), RecursiveBytes: recursiveBytes, CreatedAt: createdAt,
	})
	if err != nil {
		return preparedDirectory{}, err
	}
	prerequisites := append(pages, storageformat.MutationObject{Key: manifestKey.String(), Body: manifestBody})
	sort.Slice(prerequisites, func(i, j int) bool { return prerequisites[i].Key < prerequisites[j].Key })
	rootBody, err := storageformat.EncodeEnvelope(directoryRootSchema, rootKey, root.envelope.Revision+1, storageformat.DirectoryRoot{
		SchemaVersion: 1, DirectoryID: directoryID, ManifestID: manifestID, RecursiveBytes: recursiveBytes,
	})
	if err != nil {
		return preparedDirectory{}, err
	}
	return preparedDirectory{manifestID: manifestID, recursiveBytes: recursiveBytes, rootBody: rootBody, prerequisites: prerequisites}, nil
}

func deterministicMigrationID(value string) string {
	sum := sha256.Sum256([]byte("endlessfs-recursive-byte-migration-v1\x00" + value))
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

func (e *Engine) activateRecursiveByteWriterSet(ctx context.Context) error {
	for range 8 {
		object, envelope, writer, err := e.readStoredWriterSet(ctx)
		if err != nil {
			return err
		}
		if reflect.DeepEqual(writer, e.writer) {
			return nil
		}
		legacy := e.writer
		legacy.RequiredFeatures = legacyRecursiveByteFeatures(legacy.RequiredFeatures)
		if !reflect.DeepEqual(writer, legacy) {
			return domain.NewError(domain.ErrorPreconditionFailed, "incompatible portable writer set during migration")
		}
		body, err := storageformat.EncodeEnvelope(writerSetSchema, object.Key, envelope.Revision+1, e.writer)
		if err != nil {
			return err
		}
		if _, err = e.backend.Put(ctx, object.Key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrPreconditionFailed) && !errors.Is(err, domain.ErrConflict) {
			return err
		}
	}
	return domain.NewError(domain.ErrorUnavailable, "writer-set migration remained contended")
}

func (e *Engine) activateRecursiveByteSuperblock(ctx context.Context, initial objectstore.Object, decoded storageformat.Superblock) error {
	object, superblock := initial, decoded
	for range 8 {
		if err := validateCompatibleSuperblock(superblock); err != nil {
			return err
		}
		if reflect.DeepEqual(superblock.RequiredFeatures, e.writer.RequiredFeatures) {
			return nil
		}
		if !reflect.DeepEqual(superblock.RequiredFeatures, legacyRecursiveByteFeatures(e.writer.RequiredFeatures)) {
			return domain.NewError(domain.ErrorPreconditionFailed, "incompatible portable superblock during migration")
		}
		superblock.RequiredFeatures = append([]string(nil), e.writer.RequiredFeatures...)
		body, err := storageformat.EncodeCanonical(superblock)
		if err != nil {
			return err
		}
		if _, err = e.backend.Put(ctx, object.Key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrPreconditionFailed) && !errors.Is(err, domain.ErrConflict) {
			return err
		}
		object, err = e.backend.Get(ctx, storageformat.SuperblockKey())
		if err != nil {
			return err
		}
		if err := decodeCanonicalSuperblock(object.Body, &superblock); err != nil {
			return err
		}
	}
	return domain.NewError(domain.ErrorUnavailable, "superblock migration remained contended")
}

func decodeCanonicalSuperblock(body []byte, destination *storageformat.Superblock) error {
	if destination == nil {
		return domain.NewError(domain.ErrorInvalid, "missing superblock destination")
	}
	// Superblocks are not enveloped, but canonical re-encoding is still checked.
	if err := decodeCanonicalRecord(body, destination); err != nil {
		return err
	}
	return nil
}

func (e *Engine) bindMigrationGateToWriterFeatures(ctx context.Context) error {
	for range 8 {
		object, envelope, gate, err := e.readGate(ctx)
		if err != nil {
			return err
		}
		if gate.Mode == storageformat.GateOpen && reflect.DeepEqual(gate.WriterFeatures, e.writer.RequiredFeatures) {
			return nil
		}
		if gate.Mode != storageformat.GateClosed || gate.CheckpointID != recursiveByteMigrationCheckpointID {
			return domain.NewError(domain.ErrorPreconditionFailed, "migration write gate is not closed")
		}
		if len(gate.WriterFeatures) > 0 && !reflect.DeepEqual(gate.WriterFeatures, legacyRecursiveByteFeatures(e.writer.RequiredFeatures)) && !reflect.DeepEqual(gate.WriterFeatures, e.writer.RequiredFeatures) {
			return domain.NewError(domain.ErrorPreconditionFailed, "incompatible write-gate feature binding")
		}
		if reflect.DeepEqual(gate.WriterFeatures, e.writer.RequiredFeatures) {
			return nil
		}
		gate.WriterFeatures = append([]string(nil), e.writer.RequiredFeatures...)
		body, err := storageformat.EncodeEnvelope(writeGateSchema, object.Key, envelope.Revision+1, gate)
		if err != nil {
			return err
		}
		if _, err = e.backend.Put(ctx, object.Key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrPreconditionFailed) && !errors.Is(err, domain.ErrConflict) {
			return err
		}
	}
	return domain.NewError(domain.ErrorUnavailable, "migration gate binding remained contended")
}

func (e *Engine) recursiveByteMigrationComplete(ctx context.Context) (bool, error) {
	superblockObject, err := e.backend.Get(ctx, storageformat.SuperblockKey())
	if err != nil {
		return false, err
	}
	var superblock storageformat.Superblock
	if err := decodeCanonicalSuperblock(superblockObject.Body, &superblock); err != nil {
		return false, err
	}
	if !reflect.DeepEqual(superblock.RequiredFeatures, e.writer.RequiredFeatures) {
		return false, nil
	}
	_, _, writer, err := e.readStoredWriterSet(ctx)
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(writer, e.writer) {
		return false, nil
	}
	_, _, gate, err := e.readGate(ctx)
	if err != nil {
		return false, err
	}
	return gate.Mode == storageformat.GateOpen && gate.CheckpointID == "" && reflect.DeepEqual(gate.WriterFeatures, e.writer.RequiredFeatures), nil
}

func decodeCanonicalRecord(body []byte, destination any) error {
	if err := decodeCanonicalJSON(body, destination); err != nil {
		return err
	}
	canonical, err := storageformat.EncodeCanonical(destination)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, body) {
		return domain.NewError(domain.ErrorInvalid, "non-canonical record encoding")
	}
	return nil
}

// Kept small so migration decoding follows the same strict bounded codec used
// by the rest of the portable format without exporting migration-only helpers.
func decodeCanonicalJSON(body []byte, destination any) error {
	return state.DecodeJSONWithLimit(body, destination, storageformat.MaxCanonicalBytes)
}
