package portable

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
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
	// The persisted value cannot change because interrupted schema-001-to-002
	// migrations use it to resume after a newer binary starts.
	schema001To002CheckpointID = "automatic-recursive-byte-aggregates-v1"

	StepMigrationAfterDetection              = "after-detection"
	StepMigrationAfterGateClosed             = "after-gate-closed"
	StepMigrationAfterUploadRecord           = "after-upload-record"
	StepMigrationAfterDirectoryPrerequisites = "after-directory-prerequisites"
	StepMigrationAfterDirectoryRoot          = "after-directory-root"
	StepMigrationAfterDirectories            = "after-directories"
	StepMigrationAfterWriterSet              = "after-writer-set"
	StepMigrationAfterSuperblock             = "after-superblock"
	StepMigrationAfterGateBinding            = "after-gate-binding"
	StepMigrationAfterCheckpoint             = "after-checkpoint"

	migrationDirectoryMarkSchema = "migration-directory-mark-v1"
	migrationPhaseTransform      = "transform"
	migrationPhaseVerify         = "verify"
)

type schema001DirectoryRoot struct {
	SchemaVersion int                           `json:"schemaVersion"`
	DirectoryID   string                        `json:"directoryID"`
	ManifestID    string                        `json:"manifestID"`
	Pending       *schema001DirectoryTransition `json:"pending,omitempty"`
}

// schema001UploadRecord is frozen to the exact upload payload for storage
// schema 001. Normal runtime reads continue to require the current schema.
type schema001UploadRecord struct {
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

type schema001DirectoryTransition struct {
	OperationID    string `json:"operationID"`
	Fence          uint64 `json:"fence"`
	PreManifestID  string `json:"preManifestID,omitempty"`
	PostManifestID string `json:"postManifestID"`
}

type schema001DirectoryManifest struct {
	SchemaVersion int       `json:"schemaVersion"`
	DirectoryID   string    `json:"directoryID"`
	ManifestID    string    `json:"manifestID"`
	PageIDs       []string  `json:"pageIDs"`
	EntryCount    int       `json:"entryCount"`
	CreatedAt     time.Time `json:"createdAt"`
}

type schema002DirectoryRoot struct {
	SchemaVersion  int                           `json:"schemaVersion"`
	DirectoryID    string                        `json:"directoryID"`
	ManifestID     string                        `json:"manifestID"`
	RecursiveBytes int64                         `json:"recursiveBytes"`
	Pending        *schema002DirectoryTransition `json:"pending,omitempty"`
}

type schema002DirectoryTransition struct {
	OperationID        string `json:"operationID"`
	Fence              uint64 `json:"fence"`
	PreManifestID      string `json:"preManifestID,omitempty"`
	PostManifestID     string `json:"postManifestID"`
	PostRecursiveBytes int64  `json:"postRecursiveBytes"`
}

type schema002DirectoryManifest struct {
	SchemaVersion  int       `json:"schemaVersion"`
	DirectoryID    string    `json:"directoryID"`
	ManifestID     string    `json:"manifestID"`
	PageIDs        []string  `json:"pageIDs"`
	EntryCount     int       `json:"entryCount"`
	RecursiveBytes int64     `json:"recursiveBytes"`
	CreatedAt      time.Time `json:"createdAt"`
}

type migrationAggregate struct {
	bytes       int64
	files       int64
	directories int64
	accumulator string
	digest      string
}

type migrationDirectoryRoot struct {
	object             objectstore.Object
	envelope           storageformat.Envelope
	manifestID         string
	recursiveBytes     int64
	recursiveFileCount int64
	contentAccumulator string
	contentDigest      string
	hasRecursiveBytes  bool
	current            bool
}

type migrationDirectoryManifest struct {
	manifest          storageformat.DirectoryManifest
	envelope          storageformat.Envelope
	hasRecursiveBytes bool
	current           bool
}

type migrationScope struct {
	scope     domain.Scope
	rootSeen  bool
	rootCount int
	// roots is populated only by focused legacy migration tests. Production
	// traversal counts roots while streaming the namespace.
	roots map[string]struct{}
}

type migrationWalk struct {
	engine     *Engine
	group      migrationScope
	transition storageMigration
	plan       aggregateMigrationPlan
	state      map[string]uint8
	// totals is optional compatibility state for focused walker tests. The
	// production walker returns child totals on the bounded traversal stack.
	totals  map[string]migrationAggregate
	parents map[string]string
	phase   string
	active  map[string]struct{}
}

type aggregateMigrationPlan struct {
	migrateSchema001Uploads   bool
	writeFileCounts           bool
	writeProviderFingerprints bool
	writeDuplicateCatalog     bool
	writeStateIndexes         bool
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

func (e *Engine) runStorageMigration001To002(ctx context.Context, transition storageMigration, superblockObject objectstore.Object, superblock storageformat.Superblock) error {
	return e.runAggregateSchemaMigration(ctx, transition, superblockObject, superblock, aggregateMigrationPlan{migrateSchema001Uploads: true})
}

func (e *Engine) runStorageMigration002To003(ctx context.Context, transition storageMigration, superblockObject objectstore.Object, superblock storageformat.Superblock) error {
	return e.runAggregateSchemaMigration(ctx, transition, superblockObject, superblock, aggregateMigrationPlan{writeFileCounts: true})
}

func (e *Engine) runStorageMigration003To004(ctx context.Context, transition storageMigration, superblockObject objectstore.Object, superblock storageformat.Superblock) error {
	return e.runAggregateSchemaMigration(ctx, transition, superblockObject, superblock, aggregateMigrationPlan{writeProviderFingerprints: true, writeDuplicateCatalog: true, writeStateIndexes: true})
}

func (e *Engine) runAggregateSchemaMigration(ctx context.Context, transition storageMigration, superblockObject objectstore.Object, superblock storageformat.Superblock, plan aggregateMigrationPlan) error {
	e.observeMigration(MigrationProgress{MigrationID: transition.id.String(), Stage: MigrationStageStarted})
	if err := e.step(ctx, MigrationStepName(string(transition.id), StepMigrationAfterDetection)); err != nil {
		return err
	}
	complete, err := e.storageMigrationComplete(ctx, transition)
	if err == nil && complete {
		e.observeMigration(MigrationProgress{MigrationID: transition.id.String(), Stage: MigrationStageComplete})
		return nil
	}
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if err := e.verifyMigrationWriterSet(ctx, transition); err != nil {
		return err
	}
	closed, err := e.closeStorageMigrationGate(ctx, transition, plan)
	if err != nil {
		return err
	}
	if !closed {
		return nil
	}
	e.observeMigration(MigrationProgress{MigrationID: transition.id.String(), Stage: MigrationStageGateClosed})
	if err := e.step(ctx, MigrationStepName(string(transition.id), StepMigrationAfterGateClosed)); err != nil {
		return err
	}
	if err := e.migrateAllDirectoryAggregatesPhase(ctx, transition, plan, migrationPhaseTransform); err != nil {
		return err
	}
	if plan.writeDuplicateCatalog {
		if err := e.rebuildDuplicateCatalog(ctx); err != nil {
			return err
		}
	}
	if plan.writeStateIndexes {
		if err := e.migrateLegacyStateIndexes(ctx); err != nil {
			return err
		}
	}
	e.observeMigration(MigrationProgress{MigrationID: transition.id.String(), Stage: MigrationStageDirectoriesVerified})
	if err := e.migrateAllDirectoryAggregatesPhase(ctx, transition, plan, migrationPhaseVerify); err != nil {
		return err
	}
	if plan.writeDuplicateCatalog {
		if err := e.rebuildDuplicateCatalog(ctx); err != nil {
			return err
		}
	}
	if plan.writeStateIndexes {
		if err := e.migrateLegacyStateIndexes(ctx); err != nil {
			return err
		}
		if err := e.verifyStateIndexes(ctx); err != nil {
			return err
		}
	}
	if err := e.cleanupMigrationDirectoryMarks(ctx, transition.checkpointID); err != nil {
		return err
	}
	if err := e.step(ctx, MigrationStepName(string(transition.id), StepMigrationAfterDirectories)); err != nil {
		return err
	}
	if err := e.activateMigrationWriterSet(ctx, transition); err != nil {
		return err
	}
	if err := e.step(ctx, MigrationStepName(string(transition.id), StepMigrationAfterWriterSet)); err != nil {
		return err
	}
	if err := e.activateMigrationSuperblock(ctx, transition, superblockObject, superblock); err != nil {
		return err
	}
	if err := e.step(ctx, MigrationStepName(string(transition.id), StepMigrationAfterSuperblock)); err != nil {
		return err
	}
	if err := e.bindMigrationGateToTarget(ctx, transition); err != nil {
		return err
	}
	if err := e.step(ctx, MigrationStepName(string(transition.id), StepMigrationAfterGateBinding)); err != nil {
		return err
	}
	if complete, completeErr := e.storageMigrationComplete(ctx, transition); completeErr == nil && complete {
		return nil
	}
	checkpoint, err := e.createCheckpointWhileClosed(ctx, transition.checkpointID)
	if err != nil {
		if complete, completeErr := e.storageMigrationComplete(ctx, transition); completeErr == nil && complete {
			return nil
		}
		return err
	}
	e.observeMigration(MigrationProgress{MigrationID: transition.id.String(), Stage: MigrationStageCheckpointCreated})
	if err := e.step(ctx, MigrationStepName(string(transition.id), StepMigrationAfterCheckpoint)); err != nil {
		return err
	}
	if err := e.openWritesAfterCreatedCheckpoint(ctx, checkpoint); err != nil {
		if complete, completeErr := e.storageMigrationComplete(ctx, transition); completeErr == nil && complete {
			return nil
		}
		return err
	}
	e.observeMigration(MigrationProgress{MigrationID: transition.id.String(), Stage: MigrationStageComplete})
	return nil
}

func (e *Engine) migrateSchema001UploadRecords(ctx context.Context) error {
	return visitObjectPages(ctx, e.backend, storageformat.OperationPrefix(), func(info objectstore.ObjectInfo) error {
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
			var schema001Envelope storageformat.Envelope
			var schema001 schema001UploadRecord
			if err := storageformat.DecodeEnvelope(object.Body, info.Key, uploadRecordSchema, &schema001Envelope, &schema001); err != nil {
				return err
			}
			if err := validateSchema001UploadRecord(info.Key, schema001); err != nil {
				return err
			}
			current = storageformat.UploadRecord{
				SchemaVersion: schema001.SchemaVersion, UploadID: schema001.UploadID,
				CompletionOperationID: schema001.UploadID + "-complete", UserID: schema001.UserID,
				Area: schema001.Area, RequestedPath: schema001.RequestedPath, ResolvedPath: schema001.ResolvedPath,
				StagingKey: schema001.StagingKey, BackendKind: schema001.BackendKind, LeaseKey: schema001.LeaseKey,
				Size: schema001.Size, MediaType: schema001.MediaType, Conflict: schema001.Conflict,
				ExpectedVersion: schema001.ExpectedVersion, TargetExisted: schema001.TargetExisted,
				Resumable: schema001.Resumable, State: schema001.State, CreatedAt: schema001.CreatedAt, ExpiresAt: schema001.ExpiresAt,
			}
			body, encodeErr := storageformat.EncodeEnvelope(uploadRecordSchema, info.Key, schema001Envelope.Revision+1, current)
			if encodeErr != nil {
				return encodeErr
			}
			if _, putErr := e.backend.Put(ctx, info.Key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); putErr == nil {
				if err := e.step(ctx, MigrationStepName(string(storageMigration001To002), StepMigrationAfterUploadRecord)); err != nil {
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
		return nil
	})
}

func validateSchema001UploadRecord(key objectstore.Key, record schema001UploadRecord) error {
	userID, err := domain.ParseUserID(record.UserID)
	if err != nil || record.SchemaVersion != 1 || record.UploadID == "" || storageformat.OperationKey(record.UserID, record.UploadID) != key || record.Size < 0 || record.CreatedAt.IsZero() || record.ExpiresAt.IsZero() {
		return domain.NewError(domain.ErrorInvalid, "invalid schema-001 upload record")
	}
	if record.Area != areaName(domain.AreaLive) && record.Area != areaName(domain.AreaTrash) {
		return domain.NewError(domain.ErrorInvalid, "invalid schema-001 upload area")
	}
	requested, requestedErr := domain.ParseUserPath(record.RequestedPath)
	resolved, resolvedErr := domain.ParseUserPath(record.ResolvedPath)
	if requestedErr != nil || resolvedErr != nil || requested.IsRoot() || resolved.IsRoot() {
		return domain.NewError(domain.ErrorInvalid, "invalid schema-001 upload path")
	}
	mediaType, mediaErr := domain.NormalizeMediaType(record.MediaType)
	conflict, conflictErr := domain.NormalizeConflictMode(record.Conflict)
	if mediaErr != nil || mediaType != record.MediaType || conflictErr != nil || conflict != record.Conflict {
		return domain.NewError(domain.ErrorInvalid, "invalid schema-001 upload constraints")
	}
	if err := storageformat.ValidateNamespace(record.BackendKind); err != nil {
		return domain.NewError(domain.ErrorInvalid, "invalid schema-001 upload backend")
	}
	stagingKey, stagingErr := objectstore.ParseKey(record.StagingKey)
	leaseKey, leaseErr := objectstore.ParseKey(record.LeaseKey)
	if stagingErr != nil || leaseErr != nil || stagingKey != storageformat.StagingKey(userID.String(), record.UploadID, "upload") || leaseKey != storageformat.LeaseKey(record.BackendKind, record.UploadID) {
		return domain.NewError(domain.ErrorInvalid, "invalid schema-001 upload storage keys")
	}
	if record.State != storageformat.UploadActive && record.State != storageformat.UploadCompleted && record.State != storageformat.UploadAborted {
		return domain.NewError(domain.ErrorInvalid, "invalid schema-001 upload state")
	}
	return nil
}

func (e *Engine) verifyMigrationWriterSet(ctx context.Context, transition storageMigration) error {
	_, _, writer, err := e.readStoredWriterSet(ctx)
	if err != nil {
		return err
	}
	if !sameWriterExceptFeatures(writer, e.writer) {
		return domain.NewError(domain.ErrorPreconditionFailed, "incompatible portable writer set")
	}
	detected, found := detectStorageSchema(writer.RequiredFeatures, e.writer.RequiredFeatures)
	detectedIndex, _ := schemaIndex(detected.id)
	fromIndex, _ := schemaIndex(transition.from)
	if !found || detectedIndex < fromIndex {
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

func (e *Engine) closeStorageMigrationGate(ctx context.Context, transition storageMigration, plan aggregateMigrationPlan) (bool, error) {
	for range 16 {
		object, envelope, gate, err := e.readGate(ctx)
		if err != nil {
			return false, err
		}
		if gate.Mode == storageformat.GateOpen && writeGateSchemaAtLeast(gate.WriterFeatures, transition.to, e.writer.RequiredFeatures) {
			complete, completeErr := e.storageMigrationComplete(ctx, transition)
			if completeErr != nil {
				return false, completeErr
			}
			if complete {
				return false, nil
			}
			return false, domain.NewError(domain.ErrorPreconditionFailed, "migration gate opened before feature activation completed")
		}
		if gate.Mode != storageformat.GateOpen && gate.CheckpointID != transition.checkpointID {
			if other, found := migrationForCheckpoint(gate.CheckpointID); found {
				otherIndex, _ := schemaIndex(other.from)
				transitionIndex, _ := schemaIndex(transition.from)
				if otherIndex > transitionIndex {
					return false, nil
				}
			}
			return false, domain.NewError(domain.ErrorConflict, "write gate is reserved by another maintenance operation")
		}
		gateSchema, knownGateSchema := detectWriteGateSchema(gate.WriterFeatures, e.writer.RequiredFeatures)
		gateIndex, _ := schemaIndex(gateSchema.id)
		fromIndex, _ := schemaIndex(transition.from)
		if !knownGateSchema || gateIndex < fromIndex {
			return false, domain.NewError(domain.ErrorPreconditionFailed, "incompatible write-gate feature binding")
		}
		switch gate.Mode {
		case storageformat.GateOpen:
			gate.Mode = storageformat.GateClosing
			gate.CheckpointID = transition.checkpointID
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
			if plan.migrateSchema001Uploads {
				if err := e.migrateSchema001UploadRecords(ctx); err != nil {
					return false, err
				}
			}
			err = e.finishClosingWrites(ctx, transition.checkpointID)
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
	return false, domain.NewError(domain.ErrorUnavailable, "storage-schema migration gate remained contended")
}

func (e *Engine) migrateAllDirectoryAggregates(ctx context.Context, transition storageMigration, plan aggregateMigrationPlan) error {
	return e.migrateAllDirectoryAggregatesPhase(ctx, transition, plan, "")
}

func (e *Engine) migrateAllDirectoryAggregatesPhase(ctx context.Context, transition storageMigration, plan aggregateMigrationPlan, phase string) error {
	if phase != "" && phase != migrationPhaseTransform && phase != migrationPhaseVerify {
		return domain.NewError(domain.ErrorInvalid, "invalid directory migration phase")
	}
	groups := make(map[string]migrationScope)
	if err := visitObjectPages(ctx, e.backend, storageformat.FilesystemPrefix(), func(info objectstore.ObjectInfo) error {
		userIDValue, areaValue, directoryID, matched, parseErr := storageformat.ParseDirectoryRootKey(info.Key)
		if parseErr != nil {
			return parseErr
		}
		if !matched {
			return nil
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
		if !group.scope.Valid() {
			group.scope = scope
		}
		group.rootCount++
		group.rootSeen = group.rootSeen || directoryID == storageformat.RootDirectoryID
		groups[groupKey] = group
		return nil
	}); err != nil {
		return err
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		group := groups[key]
		if !group.rootSeen {
			return domain.NewError(domain.ErrorInvalid, "directory scope has no canonical root")
		}
		walk := migrationWalk{
			engine: e, group: group, transition: transition, plan: plan, phase: phase,
		}
		if phase == "" {
			walk.state = make(map[string]uint8)
			walk.parents = make(map[string]string)
		} else {
			walk.active = make(map[string]struct{})
		}
		total, err := walk.directory(ctx, storageformat.RootDirectoryID, "")
		if err != nil {
			return err
		}
		if phase == "" && len(walk.state) != group.rootCount || phase != "" && total.directories != int64(group.rootCount) {
			return domain.NewError(domain.ErrorInvalid, "directory scope contains an unreachable directory root")
		}
	}
	return nil
}

func (walk *migrationWalk) directory(ctx context.Context, directoryID, parentID string) (migrationAggregate, error) {
	return walk.directoryEntry(ctx, directoryID, parentID, "")
}

func (walk *migrationWalk) directoryEntry(ctx context.Context, directoryID, parentID, parentEntryName string) (migrationAggregate, error) {
	if walk.group.roots != nil {
		if _, found := walk.group.roots[directoryID]; !found {
			return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "directory entry references a missing child root")
		}
	}
	if directoryID == storageformat.RootDirectoryID && parentID != "" {
		return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "directory graph references its area root")
	}
	if walk.phase != "" {
		if _, found := walk.active[directoryID]; found {
			return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "directory graph contains a cycle")
		}
		if total, found, err := walk.readCompletedDirectoryMark(ctx, directoryID, parentID, parentEntryName); err != nil {
			return migrationAggregate{}, err
		} else if found {
			return total, nil
		}
		walk.active[directoryID] = struct{}{}
		defer delete(walk.active, directoryID)
	} else {
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
			if walk.totals != nil {
				return walk.totals[directoryID], nil
			}
			return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "directory graph references a child more than once")
		}
		walk.state[directoryID] = 1
	}
	root, err := walk.engine.readMigrationDirectoryRoot(ctx, walk.group.scope, directoryID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "directory entry references a missing child root")
		}
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
	directoryChanged := false
	totalDirectories := int64(1)
	for index := range entries {
		if entries[index].Kind != domain.EntryDirectory {
			if walk.plan.writeProviderFingerprints {
				changed, fingerprintErr := walk.engine.migrateDirectoryEntryFingerprint(ctx, walk.group.scope, &entries[index])
				if fingerprintErr != nil {
					return migrationAggregate{}, fingerprintErr
				}
				directoryChanged = directoryChanged || changed
			}
			continue
		}
		childTotal, childErr := walk.directoryEntry(ctx, entries[index].DirectoryID, directoryID, entries[index].Name)
		if childErr != nil {
			return migrationAggregate{}, childErr
		}
		if root.hasRecursiveBytes && entries[index].Size != childTotal.bytes {
			return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "migrated directory byte aggregate mismatch")
		}
		if root.current && entries[index].FileCount != childTotal.files {
			return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "migrated directory file count mismatch")
		}
		if childTotal.directories <= 0 || totalDirectories > math.MaxInt64-childTotal.directories {
			return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "directory count aggregate overflow")
		}
		totalDirectories += childTotal.directories
		changed := false
		if !root.hasRecursiveBytes {
			entries[index].Size = childTotal.bytes
			changed = true
		}
		if walk.plan.writeFileCounts && !root.current {
			entries[index].FileCount = childTotal.files
			changed = true
		}
		if walk.plan.writeProviderFingerprints && entries[index].ContentDigest != childTotal.digest {
			entries[index].ContentDigest = childTotal.digest
			changed = true
		}
		if changed {
			directoryChanged = true
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
	totalAccumulator, totalDigest := "", ""
	if walk.plan.writeProviderFingerprints {
		totalAccumulator, totalDigest, err = directoryContentIdentity(entries)
		if err != nil {
			return migrationAggregate{}, err
		}
	}
	total := migrationAggregate{bytes: totalBytes, files: totalFiles, directories: totalDirectories, accumulator: totalAccumulator, digest: totalDigest}
	if walk.plan.writeProviderFingerprints {
		if root.contentDigest == "" && manifest.manifest.ContentDigest == "" {
			directoryChanged = true
		} else if root.contentAccumulator != total.accumulator || manifest.manifest.ContentAccumulator != total.accumulator || root.contentDigest != total.digest || manifest.manifest.ContentDigest != total.digest {
			return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "source directory content digest mismatch")
		}
	}
	if root.current && !directoryChanged {
		if root.recursiveBytes != total.bytes || manifest.manifest.RecursiveBytes != total.bytes || root.recursiveFileCount != total.files || manifest.manifest.RecursiveFileCount != total.files || walk.plan.writeProviderFingerprints && (root.contentAccumulator != total.accumulator || manifest.manifest.ContentAccumulator != total.accumulator || root.contentDigest != total.digest || manifest.manifest.ContentDigest != total.digest) {
			return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "migrated directory aggregate mismatch")
		}
		return total, walk.completeDirectory(ctx, directoryID, parentID, parentEntryName, total)
	}
	if root.hasRecursiveBytes && !walk.plan.writeFileCounts && !walk.plan.writeProviderFingerprints {
		if root.recursiveBytes != total.bytes || manifest.manifest.RecursiveBytes != total.bytes {
			return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "schema-002 directory byte aggregate mismatch")
		}
		return total, walk.completeDirectory(ctx, directoryID, parentID, parentEntryName, total)
	}
	if walk.transition.from == storageSchema002 && !root.hasRecursiveBytes {
		return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "schema-002 migration encountered a schema-001 directory")
	}
	if root.hasRecursiveBytes && (root.recursiveBytes != total.bytes || manifest.manifest.RecursiveBytes != total.bytes) {
		return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "source schema directory byte aggregate mismatch")
	}
	prepared, err := walk.engine.prepareMigratedDirectory(walk.group.scope, directoryID, entries, root, manifest.manifest.CreatedAt, walk.transition, walk.plan)
	if err != nil {
		return migrationAggregate{}, err
	}
	if err := walk.engine.ensureMutationPrerequisites(ctx, prepared.prerequisites); err != nil {
		return migrationAggregate{}, err
	}
	if err := walk.engine.step(ctx, MigrationStepName(string(walk.transition.id), StepMigrationAfterDirectoryPrerequisites)); err != nil {
		return migrationAggregate{}, err
	}
	_, err = walk.engine.backend.Put(ctx, root.object.Key, prepared.rootBody, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: root.object.Version})
	if err != nil {
		if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrConflict) {
			matched, matchErr := walk.engine.migrationWinnerMatchesTarget(ctx, walk.group.scope, directoryID, total, walk.transition)
			if matchErr == nil && matched {
				return total, walk.completeDirectory(ctx, directoryID, parentID, parentEntryName, total)
			}
			if matchErr != nil {
				return migrationAggregate{}, matchErr
			}
			return migrationAggregate{}, domain.NewError(domain.ErrorInvalid, "directory root changed unexpectedly during migration")
		}
		return migrationAggregate{}, err
	}
	if err := walk.engine.step(ctx, MigrationStepName(string(walk.transition.id), StepMigrationAfterDirectoryRoot)); err != nil {
		return migrationAggregate{}, err
	}
	return total, walk.completeDirectory(ctx, directoryID, parentID, parentEntryName, total)
}

func (walk *migrationWalk) completeDirectory(ctx context.Context, directoryID, parentID, parentEntryName string, total migrationAggregate) error {
	if walk.phase == "" {
		walk.state[directoryID] = 2
		if walk.totals != nil {
			walk.totals[directoryID] = total
		}
		return nil
	}
	return walk.writeCompletedDirectoryMark(ctx, directoryID, parentID, parentEntryName, total)
}

func (walk *migrationWalk) readCompletedDirectoryMark(ctx context.Context, directoryID, parentID, parentEntryName string) (migrationAggregate, bool, error) {
	key := storageformat.MigrationDirectoryMarkKey(walk.transition.checkpointID, walk.phase, walk.group.scope.UserID().String(), areaName(walk.group.scope.Area()), directoryID)
	object, err := walk.engine.backend.Get(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		return migrationAggregate{}, false, nil
	}
	if err != nil {
		return migrationAggregate{}, false, err
	}
	var envelope storageformat.Envelope
	var mark storageformat.MigrationDirectoryMark
	if err := storageformat.DecodeEnvelope(object.Body, key, migrationDirectoryMarkSchema, &envelope, &mark); err != nil {
		return migrationAggregate{}, false, err
	}
	if mark.SchemaVersion != 1 || mark.CheckpointID != walk.transition.checkpointID || mark.Phase != walk.phase || mark.UserID != walk.group.scope.UserID().String() || mark.Area != areaName(walk.group.scope.Area()) || mark.DirectoryID != directoryID || mark.ParentDirectoryID != parentID || mark.ParentEntryName != parentEntryName || mark.ManifestID == "" || mark.RootLogicalVersion == "" || mark.ManifestVersion == "" || mark.RecursiveBytes < 0 || mark.RecursiveFileCount < 0 || mark.DirectoryCount <= 0 {
		return migrationAggregate{}, false, domain.NewError(domain.ErrorInvalid, "invalid completed directory migration mark")
	}
	root, err := walk.engine.readMigrationDirectoryRoot(ctx, walk.group.scope, directoryID)
	if err != nil {
		return migrationAggregate{}, false, err
	}
	manifest, err := walk.engine.readMigrationDirectoryManifest(ctx, walk.group.scope, directoryID, root.manifestID)
	if err != nil {
		return migrationAggregate{}, false, err
	}
	if root.manifestID != mark.ManifestID || root.envelope.LogicalVersion != mark.RootLogicalVersion || manifest.envelope.LogicalVersion != mark.ManifestVersion {
		return migrationAggregate{}, false, nil
	}
	if root.recursiveBytes != mark.RecursiveBytes || manifest.manifest.RecursiveBytes != mark.RecursiveBytes || walk.transition.to != storageSchema002 && (root.recursiveFileCount != mark.RecursiveFileCount || manifest.manifest.RecursiveFileCount != mark.RecursiveFileCount) || walk.transition.to == storageSchema004 && (root.contentAccumulator != mark.ContentAccumulator || manifest.manifest.ContentAccumulator != mark.ContentAccumulator || root.contentDigest != mark.ContentDigest || manifest.manifest.ContentDigest != mark.ContentDigest) {
		return migrationAggregate{}, false, domain.NewError(domain.ErrorInvalid, "completed directory migration mark is stale")
	}
	return migrationAggregate{bytes: mark.RecursiveBytes, files: mark.RecursiveFileCount, directories: mark.DirectoryCount, accumulator: mark.ContentAccumulator, digest: mark.ContentDigest}, true, nil
}

func (walk *migrationWalk) writeCompletedDirectoryMark(ctx context.Context, directoryID, parentID, parentEntryName string, total migrationAggregate) error {
	if total.bytes < 0 || total.files < 0 || total.directories <= 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid completed directory migration aggregate")
	}
	root, err := walk.engine.readMigrationDirectoryRoot(ctx, walk.group.scope, directoryID)
	if err != nil {
		return err
	}
	manifest, err := walk.engine.readMigrationDirectoryManifest(ctx, walk.group.scope, directoryID, root.manifestID)
	if err != nil {
		return err
	}
	if root.manifestID != manifest.manifest.ManifestID || root.recursiveBytes != total.bytes || manifest.manifest.RecursiveBytes != total.bytes {
		return domain.NewError(domain.ErrorInvalid, "completed directory migration aggregate changed")
	}
	if walk.transition.to != storageSchema002 && (!root.current || !manifest.current || root.recursiveFileCount != total.files || manifest.manifest.RecursiveFileCount != total.files) {
		return domain.NewError(domain.ErrorInvalid, "completed directory migration file count changed")
	}
	if walk.transition.to == storageSchema004 && (root.contentAccumulator != total.accumulator || manifest.manifest.ContentAccumulator != total.accumulator || root.contentDigest != total.digest || manifest.manifest.ContentDigest != total.digest || total.digest == "") {
		return domain.NewError(domain.ErrorInvalid, "completed directory migration content identity changed")
	}
	key := storageformat.MigrationDirectoryMarkKey(walk.transition.checkpointID, walk.phase, walk.group.scope.UserID().String(), areaName(walk.group.scope.Area()), directoryID)
	mark := storageformat.MigrationDirectoryMark{
		SchemaVersion: 1, CheckpointID: walk.transition.checkpointID, Phase: walk.phase,
		UserID: walk.group.scope.UserID().String(), Area: areaName(walk.group.scope.Area()), DirectoryID: directoryID,
		ParentDirectoryID: parentID, ParentEntryName: parentEntryName, ManifestID: root.manifestID,
		RootLogicalVersion: root.envelope.LogicalVersion, ManifestVersion: manifest.envelope.LogicalVersion,
		RecursiveBytes: total.bytes, RecursiveFileCount: total.files, DirectoryCount: total.directories,
		ContentAccumulator: total.accumulator, ContentDigest: total.digest,
	}
	for range 8 {
		existing, getErr := walk.engine.backend.Get(ctx, key)
		if errors.Is(getErr, domain.ErrNotFound) {
			body, encodeErr := storageformat.EncodeEnvelope(migrationDirectoryMarkSchema, key, 1, mark)
			if encodeErr != nil {
				return encodeErr
			}
			if _, putErr := walk.engine.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); putErr == nil {
				return nil
			} else if errors.Is(putErr, domain.ErrConflict) || errors.Is(putErr, domain.ErrPreconditionFailed) {
				continue
			} else {
				return putErr
			}
		}
		if getErr != nil {
			return getErr
		}
		var envelope storageformat.Envelope
		var current storageformat.MigrationDirectoryMark
		if err := storageformat.DecodeEnvelope(existing.Body, key, migrationDirectoryMarkSchema, &envelope, &current); err != nil {
			return err
		}
		if current == mark {
			return nil
		}
		if current.SchemaVersion != 1 || current.CheckpointID != mark.CheckpointID || current.Phase != mark.Phase || current.UserID != mark.UserID || current.Area != mark.Area || current.DirectoryID != mark.DirectoryID || current.ParentDirectoryID != mark.ParentDirectoryID || current.ParentEntryName != mark.ParentEntryName {
			return domain.NewError(domain.ErrorInvalid, "completed directory migration mark conflicts with another traversal")
		}
		body, err := storageformat.EncodeEnvelope(migrationDirectoryMarkSchema, key, envelope.Revision+1, mark)
		if err != nil {
			return err
		}
		if _, err := walk.engine.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: existing.Version}); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
	}
	return domain.NewError(domain.ErrorUnavailable, "completed directory migration mark remained contended")
}

func (e *Engine) cleanupMigrationDirectoryMarks(ctx context.Context, checkpointID string) error {
	return visitObjectPages(ctx, e.backend, storageformat.MigrationDirectoryMarkPrefix(checkpointID), func(info objectstore.ObjectInfo) error {
		if err := e.backend.Delete(ctx, info.Key, objectstore.DeleteCondition{Version: info.Version}); err != nil && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
		return nil
	})
}

func (e *Engine) migrateDirectoryEntryFingerprint(ctx context.Context, scope domain.Scope, entry *storageformat.DirectoryEntry) (bool, error) {
	if entry == nil || entry.Kind != domain.EntryFile || entry.BlobID == "" || entry.Size < 0 {
		return false, domain.NewError(domain.ErrorInvalid, "invalid file entry during provider-fingerprint migration")
	}
	info, err := e.fileBackend.Head(ctx, storageformat.BlobKey(scope.UserID().String(), entry.BlobID))
	if err != nil {
		return false, err
	}
	if info.Size != entry.Size || !info.Fingerprint.Complete() {
		return false, domain.NewError(domain.ErrorPreconditionFailed, "provider metadata does not match the canonical file entry")
	}
	if entry.SHA256 == "" && entry.MD5 == info.Fingerprint.MD5 && entry.CRC32C == info.Fingerprint.CRC32C {
		return false, nil
	}
	if entry.MD5 != "" || entry.CRC32C != "" {
		fingerprint := objectstore.ContentFingerprint{MD5: entry.MD5, CRC32C: entry.CRC32C}
		if !fingerprint.Complete() || fingerprint != info.Fingerprint {
			return false, domain.NewError(domain.ErrorPreconditionFailed, "stored and provider file fingerprints differ")
		}
	}
	entry.SHA256 = ""
	entry.MD5 = info.Fingerprint.MD5
	entry.CRC32C = info.Fingerprint.CRC32C
	entry.LogicalVersion, err = directoryEntryVersion(*entry)
	return true, err
}

func (e *Engine) migrationWinnerMatchesTarget(ctx context.Context, scope domain.Scope, directoryID string, want migrationAggregate, transition storageMigration) (bool, error) {
	root, err := e.readMigrationDirectoryRoot(ctx, scope, directoryID)
	if err != nil {
		return false, err
	}
	manifest, err := e.readMigrationDirectoryManifest(ctx, scope, directoryID, root.manifestID)
	if err != nil {
		return false, err
	}
	if root.current != manifest.current || root.hasRecursiveBytes != manifest.hasRecursiveBytes {
		return false, domain.NewError(domain.ErrorInvalid, "winning directory root and manifest migration states differ")
	}
	if !root.hasRecursiveBytes || root.recursiveBytes != want.bytes || manifest.manifest.RecursiveBytes != want.bytes {
		return false, nil
	}
	switch transition.to {
	case storageSchema002:
		return true, nil
	case storageSchema003:
		return root.current && manifest.current && root.recursiveFileCount == want.files && manifest.manifest.RecursiveFileCount == want.files, nil
	case storageSchema004:
		if !root.current || !manifest.current || root.recursiveFileCount != want.files || manifest.manifest.RecursiveFileCount != want.files || root.contentAccumulator != want.accumulator || manifest.manifest.ContentAccumulator != want.accumulator || root.contentDigest != want.digest || manifest.manifest.ContentDigest != want.digest || want.digest == "" {
			return false, nil
		}
		entries, err := e.Files().readManifestPageEntries(ctx, scope, directoryID, manifest.manifest)
		if err != nil {
			return false, err
		}
		for _, entry := range entries {
			if entry.Kind == domain.EntryFile && (entry.SHA256 != "" || !objectstore.ContentFingerprint{MD5: entry.MD5, CRC32C: entry.CRC32C}.Complete()) {
				return false, nil
			}
		}
		accumulator, digest, err := directoryContentIdentity(entries)
		return err == nil && accumulator == want.accumulator && digest == want.digest, err
	default:
		return false, domain.NewError(domain.ErrorPreconditionFailed, "aggregate migration has no target-schema reconciliation rule")
	}
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
		return migrationDirectoryRoot{object: object, envelope: envelope, manifestID: current.ManifestID, recursiveBytes: current.RecursiveBytes, recursiveFileCount: current.RecursiveFileCount, contentAccumulator: current.ContentAccumulator, contentDigest: current.ContentDigest, hasRecursiveBytes: true, current: true}, nil
	}
	var byteEnvelope storageformat.Envelope
	var byteRoot schema002DirectoryRoot
	if err := storageformat.DecodeEnvelope(object.Body, key, directoryRootSchema, &byteEnvelope, &byteRoot); err == nil {
		if byteRoot.SchemaVersion != 1 || byteRoot.DirectoryID != directoryID || byteRoot.ManifestID == "" || byteRoot.RecursiveBytes < 0 || byteRoot.Pending != nil {
			return migrationDirectoryRoot{}, domain.NewError(domain.ErrorInvalid, "invalid recursive-byte directory root")
		}
		return migrationDirectoryRoot{object: object, envelope: byteEnvelope, manifestID: byteRoot.ManifestID, recursiveBytes: byteRoot.RecursiveBytes, hasRecursiveBytes: true}, nil
	}
	var schema001Envelope storageformat.Envelope
	var schema001 schema001DirectoryRoot
	if err := storageformat.DecodeEnvelope(object.Body, key, directoryRootSchema, &schema001Envelope, &schema001); err != nil {
		return migrationDirectoryRoot{}, err
	}
	if schema001.SchemaVersion != 1 || schema001.DirectoryID != directoryID || schema001.ManifestID == "" || schema001.Pending != nil {
		return migrationDirectoryRoot{}, domain.NewError(domain.ErrorInvalid, "invalid schema-001 directory root")
	}
	return migrationDirectoryRoot{object: object, envelope: schema001Envelope, manifestID: schema001.ManifestID}, nil
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
		return migrationDirectoryManifest{manifest: current, envelope: envelope, hasRecursiveBytes: true, current: true}, nil
	}
	var byteEnvelope storageformat.Envelope
	var byteManifest schema002DirectoryManifest
	if err := storageformat.DecodeEnvelope(object.Body, key, directoryManifestSchema, &byteEnvelope, &byteManifest); err == nil {
		current = storageformat.DirectoryManifest{
			SchemaVersion: byteManifest.SchemaVersion, DirectoryID: byteManifest.DirectoryID, ManifestID: byteManifest.ManifestID,
			PageIDs: append([]string(nil), byteManifest.PageIDs...), EntryCount: byteManifest.EntryCount,
			RecursiveBytes: byteManifest.RecursiveBytes, CreatedAt: byteManifest.CreatedAt,
		}
		if err := validateMigrationManifest(current, directoryID, manifestID); err != nil {
			return migrationDirectoryManifest{}, err
		}
		return migrationDirectoryManifest{manifest: current, envelope: byteEnvelope, hasRecursiveBytes: true}, nil
	}
	var schema001Envelope storageformat.Envelope
	var schema001 schema001DirectoryManifest
	if err := storageformat.DecodeEnvelope(object.Body, key, directoryManifestSchema, &schema001Envelope, &schema001); err != nil {
		return migrationDirectoryManifest{}, err
	}
	current = storageformat.DirectoryManifest{
		SchemaVersion: schema001.SchemaVersion, DirectoryID: schema001.DirectoryID, ManifestID: schema001.ManifestID,
		PageIDs: append([]string(nil), schema001.PageIDs...), EntryCount: schema001.EntryCount, CreatedAt: schema001.CreatedAt,
	}
	if err := validateMigrationManifest(current, directoryID, manifestID); err != nil {
		return migrationDirectoryManifest{}, err
	}
	return migrationDirectoryManifest{manifest: current, envelope: schema001Envelope}, nil
}

func validateMigrationManifest(manifest storageformat.DirectoryManifest, directoryID, manifestID string) error {
	validShape := manifest.SchemaVersion == 1 && len(manifest.PageIDs) != 0 && manifest.IndexRootID == "" && manifest.IndexRootDigest == ""
	if manifest.SchemaVersion == 2 {
		accumulator, accumulatorErr := decodeDirectoryContentAccumulator(manifest.ContentAccumulator)
		digest, digestErr := directoryContentAccumulatorDigest(accumulator, manifest.EntryCount)
		validShape = accumulatorErr == nil && digestErr == nil && digest == manifest.ContentDigest && len(manifest.PageIDs) == 0 && validateDirectorySortIndexRoots(manifest.SortIndexes, manifest.EntryCount) == nil && (manifest.EntryCount == 0 && manifest.IndexRootID == "" && manifest.IndexRootDigest == "" || manifest.EntryCount > 0 && manifest.IndexRootID != "" && manifest.IndexRootDigest != "")
	}
	if !validShape || manifest.DirectoryID != directoryID || manifest.ManifestID != manifestID || manifest.EntryCount < 0 || manifest.RecursiveBytes < 0 || manifest.RecursiveFileCount < 0 || manifest.CreatedAt.IsZero() {
		return domain.NewError(domain.ErrorInvalid, fmt.Sprintf("invalid directory manifest during migration (schema=%d entries=%d pages=%d index=%t)", manifest.SchemaVersion, manifest.EntryCount, len(manifest.PageIDs), manifest.IndexRootID != ""))
	}
	return nil
}

func (e *Engine) prepareMigratedDirectory(scope domain.Scope, directoryID string, entries []storageformat.DirectoryEntry, root migrationDirectoryRoot, createdAt time.Time, transition storageMigration, plan aggregateMigrationPlan) (preparedDirectory, error) {
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
	contentAccumulator, contentDigest := "", ""
	if plan.writeProviderFingerprints {
		contentAccumulator, contentDigest, err = directoryContentIdentity(entries)
		if err != nil {
			return preparedDirectory{}, err
		}
	}
	rootKey := storageformat.DirectoryRootKey(scope.UserID().String(), areaName(scope.Area()), directoryID)
	identity := rootKey.String() + "\x00" + root.envelope.LogicalVersion
	manifestID := deterministicMigrationID(transition.to, identity+"\x00manifest")
	if plan.writeProviderFingerprints {
		indexRoot, nodes, err := e.Files().buildDirectoryIndex(scope, directoryID, entries)
		if err != nil {
			return preparedDirectory{}, err
		}
		sortRoots, sortNodes, err := e.Files().buildDirectorySortIndexes(scope, directoryID, entries)
		if err != nil {
			return preparedDirectory{}, err
		}
		nodes = append(nodes, sortNodes...)
		manifestKey := storageformat.DirectoryManifestKey(scope.UserID().String(), areaName(scope.Area()), directoryID, manifestID)
		manifestBody, err := storageformat.EncodeEnvelope(directoryManifestSchema, manifestKey, 1, storageformat.DirectoryManifest{
			SchemaVersion: 2, DirectoryID: directoryID, ManifestID: manifestID,
			IndexRootID: indexRoot.NodeID, IndexRootDigest: indexRoot.NodeDigest, SortIndexes: sortRoots,
			EntryCount: len(entries), RecursiveBytes: recursiveBytes, RecursiveFileCount: fileCount, ContentAccumulator: contentAccumulator, ContentDigest: contentDigest, CreatedAt: createdAt,
		})
		if err != nil {
			return preparedDirectory{}, err
		}
		prerequisites := append(nodes, storageformat.MutationObject{Key: manifestKey.String(), Body: manifestBody})
		sort.Slice(prerequisites, func(i, j int) bool { return prerequisites[i].Key < prerequisites[j].Key })
		rootBody, err := storageformat.EncodeEnvelope(directoryRootSchema, rootKey, root.envelope.Revision+1, storageformat.DirectoryRoot{
			SchemaVersion: 1, DirectoryID: directoryID, ManifestID: manifestID,
			RecursiveBytes: recursiveBytes, RecursiveFileCount: fileCount, ContentAccumulator: contentAccumulator, ContentDigest: contentDigest,
		})
		if err != nil {
			return preparedDirectory{}, err
		}
		return preparedDirectory{manifestID: manifestID, recursiveBytes: recursiveBytes, recursiveFileCount: fileCount, contentAccumulator: contentAccumulator, contentDigest: contentDigest, rootBody: rootBody, prerequisites: prerequisites}, nil
	}
	pages := make([]storageformat.MutationObject, 0, max(1, (len(entries)+maxEntriesPerPage-1)/maxEntriesPerPage))
	pageIDs := make([]string, 0, cap(pages))
	for start := 0; start < max(1, len(entries)); start += maxEntriesPerPage {
		end := min(start+maxEntriesPerPage, len(entries))
		pageID := deterministicMigrationID(transition.to, identity+"\x00page\x00"+strconv.Itoa(start/maxEntriesPerPage))
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
	var manifestPayload any = schema002DirectoryManifest{
		SchemaVersion: 1, DirectoryID: directoryID, ManifestID: manifestID, PageIDs: pageIDs,
		EntryCount: len(entries), RecursiveBytes: recursiveBytes, CreatedAt: createdAt,
	}
	if plan.writeFileCounts || plan.writeProviderFingerprints {
		manifestPayload = storageformat.DirectoryManifest{
			SchemaVersion: 1, DirectoryID: directoryID, ManifestID: manifestID, PageIDs: pageIDs,
			EntryCount: len(entries), RecursiveBytes: recursiveBytes, RecursiveFileCount: fileCount, ContentDigest: contentDigest, CreatedAt: createdAt,
		}
	}
	manifestBody, err := storageformat.EncodeEnvelope(directoryManifestSchema, manifestKey, 1, manifestPayload)
	if err != nil {
		return preparedDirectory{}, err
	}
	prerequisites := append(pages, storageformat.MutationObject{Key: manifestKey.String(), Body: manifestBody})
	sort.Slice(prerequisites, func(i, j int) bool { return prerequisites[i].Key < prerequisites[j].Key })
	var rootPayload any = schema002DirectoryRoot{
		SchemaVersion: 1, DirectoryID: directoryID, ManifestID: manifestID, RecursiveBytes: recursiveBytes,
	}
	if plan.writeFileCounts || plan.writeProviderFingerprints {
		rootPayload = storageformat.DirectoryRoot{
			SchemaVersion: 1, DirectoryID: directoryID, ManifestID: manifestID,
			RecursiveBytes: recursiveBytes, RecursiveFileCount: fileCount, ContentDigest: contentDigest,
		}
	}
	rootBody, err := storageformat.EncodeEnvelope(directoryRootSchema, rootKey, root.envelope.Revision+1, rootPayload)
	if err != nil {
		return preparedDirectory{}, err
	}
	return preparedDirectory{manifestID: manifestID, recursiveBytes: recursiveBytes, recursiveFileCount: fileCount, contentDigest: contentDigest, rootBody: rootBody, prerequisites: prerequisites}, nil
}

func deterministicMigrationID(schema storageSchemaID, value string) string {
	sum := sha256.Sum256([]byte("endlessfs-storage-schema-migration-v1\x00" + string(schema) + "\x00" + value))
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

func (e *Engine) activateMigrationWriterSet(ctx context.Context, transition storageMigration) error {
	targetFeatures, _ := schemaFeatures(transition.to, e.writer.RequiredFeatures)
	for range 8 {
		object, envelope, writer, err := e.readStoredWriterSet(ctx)
		if err != nil {
			return err
		}
		if !sameWriterExceptFeatures(writer, e.writer) {
			return domain.NewError(domain.ErrorPreconditionFailed, "incompatible portable writer set during migration")
		}
		if schemaAtLeast(writer.RequiredFeatures, transition.to, e.writer.RequiredFeatures) {
			return nil
		}
		detected, found := detectStorageSchema(writer.RequiredFeatures, e.writer.RequiredFeatures)
		if !found || detected.id != transition.from {
			return domain.NewError(domain.ErrorPreconditionFailed, "incompatible portable writer set during migration")
		}
		writer.RequiredFeatures = append([]string(nil), targetFeatures...)
		body, err := storageformat.EncodeEnvelope(writerSetSchema, object.Key, envelope.Revision+1, writer)
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

func (e *Engine) activateMigrationSuperblock(ctx context.Context, transition storageMigration, initial objectstore.Object, decoded storageformat.Superblock) error {
	targetFeatures, _ := schemaFeatures(transition.to, e.writer.RequiredFeatures)
	object, superblock := initial, decoded
	for range 8 {
		if err := validateCompatibleSuperblock(superblock); err != nil {
			return err
		}
		if schemaAtLeast(superblock.RequiredFeatures, transition.to, e.writer.RequiredFeatures) {
			return nil
		}
		detected, found := detectStorageSchema(superblock.RequiredFeatures, e.writer.RequiredFeatures)
		if !found || detected.id != transition.from {
			return domain.NewError(domain.ErrorPreconditionFailed, "incompatible portable superblock during migration")
		}
		superblock.RequiredFeatures = append([]string(nil), targetFeatures...)
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

func (e *Engine) bindMigrationGateToTarget(ctx context.Context, transition storageMigration) error {
	targetFeatures, _ := schemaFeatures(transition.to, e.writer.RequiredFeatures)
	for range 8 {
		object, envelope, gate, err := e.readGate(ctx)
		if err != nil {
			return err
		}
		if gate.Mode == storageformat.GateOpen && writeGateSchemaAtLeast(gate.WriterFeatures, transition.to, e.writer.RequiredFeatures) {
			return nil
		}
		if gate.Mode != storageformat.GateClosed || gate.CheckpointID != transition.checkpointID {
			if other, found := migrationForCheckpoint(gate.CheckpointID); found {
				otherFrom, _ := schemaIndex(other.from)
				transitionTo, _ := schemaIndex(transition.to)
				if otherFrom >= transitionTo && writeGateSchemaAtLeast(gate.WriterFeatures, transition.to, e.writer.RequiredFeatures) {
					return nil
				}
			}
			return domain.NewError(domain.ErrorPreconditionFailed, "migration write gate is not closed")
		}
		if writeGateSchemaAtLeast(gate.WriterFeatures, transition.to, e.writer.RequiredFeatures) {
			return nil
		}
		detected, found := detectWriteGateSchema(gate.WriterFeatures, e.writer.RequiredFeatures)
		if !found || detected.id != transition.from {
			return domain.NewError(domain.ErrorPreconditionFailed, "incompatible write-gate feature binding")
		}
		gate.WriterFeatures = append([]string(nil), targetFeatures...)
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

func (e *Engine) storageMigrationComplete(ctx context.Context, transition storageMigration) (bool, error) {
	superblockObject, err := e.backend.Get(ctx, storageformat.SuperblockKey())
	if err != nil {
		return false, err
	}
	var superblock storageformat.Superblock
	if err := decodeCanonicalSuperblock(superblockObject.Body, &superblock); err != nil {
		return false, err
	}
	if !schemaAtLeast(superblock.RequiredFeatures, transition.to, e.writer.RequiredFeatures) {
		return false, nil
	}
	_, _, writer, err := e.readStoredWriterSet(ctx)
	if err != nil {
		return false, err
	}
	if !sameWriterExceptFeatures(writer, e.writer) || !schemaAtLeast(writer.RequiredFeatures, transition.to, e.writer.RequiredFeatures) {
		return false, nil
	}
	_, _, gate, err := e.readGate(ctx)
	if err != nil {
		return false, err
	}
	if gate.Mode == storageformat.GateOpen && gate.CheckpointID == "" && writeGateSchemaAtLeast(gate.WriterFeatures, transition.to, e.writer.RequiredFeatures) {
		return true, nil
	}
	if other, found := migrationForCheckpoint(gate.CheckpointID); found {
		otherFrom, _ := schemaIndex(other.from)
		transitionTo, _ := schemaIndex(transition.to)
		if otherFrom >= transitionTo && writeGateSchemaAtLeast(gate.WriterFeatures, transition.to, e.writer.RequiredFeatures) {
			return true, nil
		}
	}
	return false, nil
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
