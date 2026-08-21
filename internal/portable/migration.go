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
	StepMigrationAfterUploadRecord           = "migration:recursive-bytes:after-upload-record"
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

// legacyUploadRecord is the exact upload payload written by v0.1.0 through
// v0.1.4. Keep this migration-only type frozen: normal runtime reads must
// continue to require the current schema.
type legacyUploadRecord struct {
	SchemaVersion   int                       `json:"schemaVersion"`
	UploadID        string                    `json:"uploadID"`
	UserID          string                    `json:"userID"`
	Area            string                    `json:"area"`
	RequestedPath   string                    `json:"requestedPath"`
	ResolvedPath    string                    `json:"resolvedPath"`
	StagingKey      string                    `json:"stagingKey"`
	BackendKind     string                    `json:"backendKind,omitempty"`
	LeaseKey        string                    `json:"leaseKey,omitempty"`
	Size            int64                     `json:"size"`
	MediaType       string                    `json:"mediaType"`
	Conflict        domain.ConflictMode       `json:"conflict"`
	ExpectedVersion domain.Version            `json:"expectedVersion,omitempty"`
	TargetExisted   bool                      `json:"targetExisted"`
	Resumable       bool                      `json:"resumable"`
	State           storageformat.UploadState `json:"state"`
	CreatedAt       time.Time                 `json:"createdAt"`
	ExpiresAt       time.Time                 `json:"expiresAt"`
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

type recursiveByteDirectoryRoot struct {
	SchemaVersion  int                               `json:"schemaVersion"`
	DirectoryID    string                            `json:"directoryID"`
	ManifestID     string                            `json:"manifestID"`
	RecursiveBytes int64                             `json:"recursiveBytes"`
	Pending        *recursiveByteDirectoryTransition `json:"pending,omitempty"`
}

type recursiveByteDirectoryTransition struct {
	OperationID        string `json:"operationID"`
	Fence              uint64 `json:"fence"`
	PreManifestID      string `json:"preManifestID,omitempty"`
	PostManifestID     string `json:"postManifestID"`
	PostRecursiveBytes int64  `json:"postRecursiveBytes"`
}

type recursiveByteDirectoryManifest struct {
	SchemaVersion  int       `json:"schemaVersion"`
	DirectoryID    string    `json:"directoryID"`
	ManifestID     string    `json:"manifestID"`
	PageIDs        []string  `json:"pageIDs"`
	EntryCount     int       `json:"entryCount"`
	RecursiveBytes int64     `json:"recursiveBytes"`
	CreatedAt      time.Time `json:"createdAt"`
}

type migrationAggregate struct {
	bytes int64
	files int64
}

type migrationDirectoryRoot struct {
	object             objectstore.Object
	envelope           storageformat.Envelope
	manifestID         string
	recursiveBytes     int64
	recursiveFileCount int64
	hasRecursiveBytes  bool
	current            bool
}

type migrationDirectoryManifest struct {
	manifest          storageformat.DirectoryManifest
	hasRecursiveBytes bool
	current           bool
}

type migrationScope struct {
	scope domain.Scope
	roots map[string]struct{}
}

type migrationWalk struct {
	engine  *Engine
	group   migrationScope
	state   map[string]uint8
	totals  map[string]migrationAggregate
	parents map[string]string
}

func predecessorAggregateFeatureSets(features []string) [][]string {
	byteOnly := make([]string, 0, len(features))
	preAggregate := make([]string, 0, len(features))
	for _, feature := range features {
		if feature != storageformat.FeatureRecursiveFileCounts {
			byteOnly = append(byteOnly, feature)
		}
		if feature != storageformat.FeatureRecursiveBytes && feature != storageformat.FeatureRecursiveFileCounts {
			preAggregate = append(preAggregate, feature)
		}
	}
	return [][]string{byteOnly, preAggregate}
}

func isAggregatePredecessor(features, current []string) bool {
	for _, predecessor := range predecessorAggregateFeatureSets(current) {
		if equalStrings(features, predecessor) {
			return true
		}
	}
	return false
}

// Canonical v0.1.0-v0.1.4 records represented an empty feature set as null.
// Feature-set equality is value equality, so nil and an allocated empty slice
// must not split one format family into incompatible representations.
func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sameWriterExceptFeatures(stored, current storageformat.WriterSet) bool {
	stored.RequiredFeatures = append([]string(nil), current.RequiredFeatures...)
	return reflect.DeepEqual(stored, current)
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

func (e *Engine) migrateLegacyUploadRecords(ctx context.Context) error {
	objects, err := e.listAll(ctx, storageformat.OperationPrefix())
	if err != nil {
		return err
	}
	for _, info := range objects {
		migrated := false
		for range 16 {
			object, getErr := e.backend.Get(ctx, info.Key)
			if errors.Is(getErr, domain.ErrNotFound) {
				migrated = true
				break
			}
			if getErr != nil {
				return getErr
			}
			var generic storageformat.Envelope
			if err := state.DecodeJSONWithLimit(object.Body, &generic, storageformat.MaxCanonicalBytes); err != nil {
				return err
			}
			if generic.Schema != uploadRecordSchema {
				migrated = true
				break
			}
			var currentEnvelope storageformat.Envelope
			var current storageformat.UploadRecord
			if err := storageformat.DecodeEnvelope(object.Body, info.Key, uploadRecordSchema, &currentEnvelope, &current); err == nil {
				migrated = true
				break
			}
			var legacyEnvelope storageformat.Envelope
			var legacy legacyUploadRecord
			if err := storageformat.DecodeEnvelope(object.Body, info.Key, uploadRecordSchema, &legacyEnvelope, &legacy); err != nil {
				return err
			}
			if err := validateLegacyUploadRecord(info.Key, legacy); err != nil {
				return err
			}
			current = storageformat.UploadRecord{
				SchemaVersion: legacy.SchemaVersion, UploadID: legacy.UploadID,
				CompletionOperationID: legacy.UploadID + "-complete", UserID: legacy.UserID,
				Area: legacy.Area, RequestedPath: legacy.RequestedPath, ResolvedPath: legacy.ResolvedPath,
				StagingKey: legacy.StagingKey, BackendKind: legacy.BackendKind, LeaseKey: legacy.LeaseKey,
				Size: legacy.Size, MediaType: legacy.MediaType, Conflict: legacy.Conflict,
				ExpectedVersion: legacy.ExpectedVersion, TargetExisted: legacy.TargetExisted,
				Resumable: legacy.Resumable, State: legacy.State, CreatedAt: legacy.CreatedAt, ExpiresAt: legacy.ExpiresAt,
			}
			body, encodeErr := storageformat.EncodeEnvelope(uploadRecordSchema, info.Key, legacyEnvelope.Revision+1, current)
			if encodeErr != nil {
				return encodeErr
			}
			if _, putErr := e.backend.Put(ctx, info.Key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); putErr == nil {
				if err := e.step(ctx, StepMigrationAfterUploadRecord); err != nil {
					return err
				}
				migrated = true
				break
			} else if !errors.Is(putErr, domain.ErrPreconditionFailed) && !errors.Is(putErr, domain.ErrConflict) {
				return putErr
			}
		}
		if !migrated {
			return domain.NewError(domain.ErrorUnavailable, "upload-record migration remained contended")
		}
	}
	return nil
}

func validateLegacyUploadRecord(key objectstore.Key, record legacyUploadRecord) error {
	userID, err := domain.ParseUserID(record.UserID)
	if err != nil || record.SchemaVersion != 1 || record.UploadID == "" || storageformat.OperationKey(record.UserID, record.UploadID) != key || record.Size < 0 || record.CreatedAt.IsZero() || record.ExpiresAt.IsZero() {
		return domain.NewError(domain.ErrorInvalid, "invalid legacy upload record")
	}
	if record.Area != areaName(domain.AreaLive) && record.Area != areaName(domain.AreaTrash) {
		return domain.NewError(domain.ErrorInvalid, "invalid legacy upload area")
	}
	requested, requestedErr := domain.ParseUserPath(record.RequestedPath)
	resolved, resolvedErr := domain.ParseUserPath(record.ResolvedPath)
	if requestedErr != nil || resolvedErr != nil || requested.IsRoot() || resolved.IsRoot() {
		return domain.NewError(domain.ErrorInvalid, "invalid legacy upload path")
	}
	mediaType, mediaErr := domain.NormalizeMediaType(record.MediaType)
	conflict, conflictErr := domain.NormalizeConflictMode(record.Conflict)
	if mediaErr != nil || mediaType != record.MediaType || conflictErr != nil || conflict != record.Conflict {
		return domain.NewError(domain.ErrorInvalid, "invalid legacy upload constraints")
	}
	if err := storageformat.ValidateNamespace(record.BackendKind); err != nil {
		return domain.NewError(domain.ErrorInvalid, "invalid legacy upload backend")
	}
	stagingKey, stagingErr := objectstore.ParseKey(record.StagingKey)
	leaseKey, leaseErr := objectstore.ParseKey(record.LeaseKey)
	if stagingErr != nil || leaseErr != nil || stagingKey != storageformat.StagingKey(userID.String(), record.UploadID, "upload") || leaseKey != storageformat.LeaseKey(record.BackendKind, record.UploadID) {
		return domain.NewError(domain.ErrorInvalid, "invalid legacy upload storage keys")
	}
	if record.State != storageformat.UploadActive && record.State != storageformat.UploadCompleted && record.State != storageformat.UploadAborted {
		return domain.NewError(domain.ErrorInvalid, "invalid legacy upload state")
	}
	return nil
}

func (e *Engine) verifyMigrationWriterSet(ctx context.Context) error {
	_, _, writer, err := e.readStoredWriterSet(ctx)
	if err != nil {
		return err
	}
	if !isAggregatePredecessor(writer.RequiredFeatures, e.writer.RequiredFeatures) && !reflect.DeepEqual(writer, e.writer) {
		return domain.NewError(domain.ErrorPreconditionFailed, "incompatible portable writer set")
	}
	if !sameWriterExceptFeatures(writer, e.writer) {
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
		if len(gate.WriterFeatures) > 0 && !isAggregatePredecessor(gate.WriterFeatures, e.writer.RequiredFeatures) && !reflect.DeepEqual(gate.WriterFeatures, e.writer.RequiredFeatures) {
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
			if err := e.migrateLegacyUploadRecords(ctx); err != nil {
				return false, err
			}
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
			totals: make(map[string]migrationAggregate, len(group.roots)), parents: make(map[string]string, len(group.roots)),
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

func (walk *migrationWalk) directory(ctx context.Context, directoryID, parentID string) (migrationAggregate, error) {
	if _, found := walk.group.roots[directoryID]; !found {
		return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "directory entry references a missing child root")
	}
	if directoryID == storageformat.RootDirectoryID && parentID != "" {
		return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "directory graph references its area root")
	}
	if parentID != "" {
		if _, found := walk.parents[directoryID]; found {
			return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "directory graph references a child more than once")
		}
		walk.parents[directoryID] = parentID
	}
	switch walk.state[directoryID] {
	case 1:
		return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "directory graph contains a cycle")
	case 2:
		return walk.totals[directoryID], nil
	}
	walk.state[directoryID] = 1
	root, err := walk.engine.readMigrationDirectoryRoot(ctx, walk.group.scope, directoryID)
	if err != nil {
		return migrationAggregate{}, err
	}
	manifest, err := walk.engine.readMigrationDirectoryManifest(ctx, walk.group.scope, directoryID, root.manifestID)
	if err != nil {
		return migrationAggregate{}, err
	}
	if root.current != manifest.current || root.hasRecursiveBytes != manifest.hasRecursiveBytes {
		return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "directory root and manifest migration states differ")
	}
	entries, err := walk.engine.Files().readManifestPageEntries(ctx, walk.group.scope, directoryID, manifest.manifest)
	if err != nil {
		return migrationAggregate{}, err
	}
	for index := range entries {
		if entries[index].Kind != domain.EntryDirectory {
			continue
		}
		childTotal, childErr := walk.directory(ctx, entries[index].DirectoryID, directoryID)
		if childErr != nil {
			return migrationAggregate{}, childErr
		}
		if root.hasRecursiveBytes && entries[index].Size != childTotal.bytes {
			return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "migrated directory byte aggregate mismatch")
		}
		if root.current && entries[index].FileCount != childTotal.files {
			return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "migrated directory file count mismatch")
		}
		if !root.current {
			entries[index].Size = childTotal.bytes
			entries[index].FileCount = childTotal.files
			entries[index].LogicalVersion, err = directoryEntryVersion(entries[index])
			if err != nil {
				return migrationAggregate{}, err
			}
		}
	}
	totalBytes, err := recursiveByteSize(entries)
	if err != nil {
		return migrationAggregate{}, err
	}
	totalFiles, err := recursiveFileCount(entries)
	if err != nil {
		return migrationAggregate{}, err
	}
	total := migrationAggregate{bytes: totalBytes, files: totalFiles}
	if root.current {
		if root.recursiveBytes != total.bytes || manifest.manifest.RecursiveBytes != total.bytes || root.recursiveFileCount != total.files || manifest.manifest.RecursiveFileCount != total.files {
			return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "migrated directory aggregate mismatch")
		}
		walk.state[directoryID] = 2
		walk.totals[directoryID] = total
		return total, nil
	}
	if root.hasRecursiveBytes && (root.recursiveBytes != total.bytes || manifest.manifest.RecursiveBytes != total.bytes) {
		return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "recursive-byte predecessor aggregate mismatch")
	}
	prepared, err := walk.engine.prepareMigratedDirectory(walk.group.scope, directoryID, entries, root, manifest.manifest.CreatedAt)
	if err != nil {
		return migrationAggregate{}, err
	}
	if err := walk.engine.ensureMutationPrerequisites(ctx, prepared.prerequisites); err != nil {
		return migrationAggregate{}, err
	}
	if err := walk.engine.step(ctx, StepMigrationAfterDirectoryPrerequisites); err != nil {
		return migrationAggregate{}, err
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
				return migrationAggregate{}, getErr
			}
			return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "directory root changed unexpectedly during migration")
		}
		return migrationAggregate{}, err
	}
	if err := walk.engine.step(ctx, StepMigrationAfterDirectoryRoot); err != nil {
		return migrationAggregate{}, err
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
		if current.SchemaVersion != 1 || current.DirectoryID != directoryID || current.ManifestID == "" || current.RecursiveBytes < 0 || current.RecursiveFileCount < 0 || current.Pending != nil {
			return migrationDirectoryRoot{}, domain.NewError(domain.ErrorInvalid, "invalid migrated directory root")
		}
		return migrationDirectoryRoot{object: object, envelope: envelope, manifestID: current.ManifestID, recursiveBytes: current.RecursiveBytes, recursiveFileCount: current.RecursiveFileCount, hasRecursiveBytes: true, current: true}, nil
	}
	var byteEnvelope storageformat.Envelope
	var byteRoot recursiveByteDirectoryRoot
	if err := storageformat.DecodeEnvelope(object.Body, key, directoryRootSchema, &byteEnvelope, &byteRoot); err == nil {
		if byteRoot.SchemaVersion != 1 || byteRoot.DirectoryID != directoryID || byteRoot.ManifestID == "" || byteRoot.RecursiveBytes < 0 || byteRoot.Pending != nil {
			return migrationDirectoryRoot{}, domain.NewError(domain.ErrorInvalid, "invalid recursive-byte directory root")
		}
		return migrationDirectoryRoot{object: object, envelope: byteEnvelope, manifestID: byteRoot.ManifestID, recursiveBytes: byteRoot.RecursiveBytes, hasRecursiveBytes: true}, nil
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
		return migrationDirectoryManifest{manifest: current, hasRecursiveBytes: true, current: true}, nil
	}
	var byteEnvelope storageformat.Envelope
	var byteManifest recursiveByteDirectoryManifest
	if err := storageformat.DecodeEnvelope(object.Body, key, directoryManifestSchema, &byteEnvelope, &byteManifest); err == nil {
		current = storageformat.DirectoryManifest{
			SchemaVersion: byteManifest.SchemaVersion, DirectoryID: byteManifest.DirectoryID, ManifestID: byteManifest.ManifestID,
			PageIDs: append([]string(nil), byteManifest.PageIDs...), EntryCount: byteManifest.EntryCount,
			RecursiveBytes: byteManifest.RecursiveBytes, CreatedAt: byteManifest.CreatedAt,
		}
		if err := validateMigrationManifest(current, directoryID, manifestID); err != nil {
			return migrationDirectoryManifest{}, err
		}
		return migrationDirectoryManifest{manifest: current, hasRecursiveBytes: true}, nil
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
	if manifest.SchemaVersion != 1 || manifest.DirectoryID != directoryID || manifest.ManifestID != manifestID || manifest.EntryCount < 0 || manifest.RecursiveBytes < 0 || manifest.RecursiveFileCount < 0 || len(manifest.PageIDs) == 0 || manifest.CreatedAt.IsZero() {
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
	fileCount, err := recursiveFileCount(entries)
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
		EntryCount: len(entries), RecursiveBytes: recursiveBytes, RecursiveFileCount: fileCount, CreatedAt: createdAt,
	})
	if err != nil {
		return preparedDirectory{}, err
	}
	prerequisites := append(pages, storageformat.MutationObject{Key: manifestKey.String(), Body: manifestBody})
	sort.Slice(prerequisites, func(i, j int) bool { return prerequisites[i].Key < prerequisites[j].Key })
	rootBody, err := storageformat.EncodeEnvelope(directoryRootSchema, rootKey, root.envelope.Revision+1, storageformat.DirectoryRoot{
		SchemaVersion: 1, DirectoryID: directoryID, ManifestID: manifestID, RecursiveBytes: recursiveBytes, RecursiveFileCount: fileCount,
	})
	if err != nil {
		return preparedDirectory{}, err
	}
	return preparedDirectory{manifestID: manifestID, recursiveBytes: recursiveBytes, recursiveFileCount: fileCount, rootBody: rootBody, prerequisites: prerequisites}, nil
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
		if !isAggregatePredecessor(writer.RequiredFeatures, e.writer.RequiredFeatures) {
			return domain.NewError(domain.ErrorPreconditionFailed, "incompatible portable writer set during migration")
		}
		if !sameWriterExceptFeatures(writer, e.writer) {
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
		if !isAggregatePredecessor(superblock.RequiredFeatures, e.writer.RequiredFeatures) {
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
		if len(gate.WriterFeatures) > 0 && !isAggregatePredecessor(gate.WriterFeatures, e.writer.RequiredFeatures) && !reflect.DeepEqual(gate.WriterFeatures, e.writer.RequiredFeatures) {
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
