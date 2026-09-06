package portable

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const (
	schema008MigrationStageSchema   = 1
	schema008MigrationSubtreeSchema = 1
)

type schema008MigrationStage struct {
	SchemaVersion  int                                 `json:"schemaVersion"`
	SourceKey      string                              `json:"sourceKey"`
	DomainKind     storageformat.ConsistencyDomainKind `json:"domainKind"`
	DomainID       string                              `json:"domainID"`
	Key            string                              `json:"key"`
	Value          []byte                              `json:"value"`
	LogicalVersion string                              `json:"logicalVersion"`
	Tree           string                              `json:"tree,omitempty"`
	MigrationOnly  bool                                `json:"migrationOnly,omitempty"`
}

type schema008MigrationSourceMarker struct {
	SchemaVersion int      `json:"schemaVersion"`
	SourceKey     string   `json:"sourceKey"`
	StageKeys     []string `json:"stageKeys"`
}

type schema008MigrationSubtree struct {
	SchemaVersion int                          `json:"schemaVersion"`
	SourceID      string                       `json:"sourceID"`
	Entry         storageformat.NamespaceEntry `json:"entry"`
}

// Frozen to the exact schema-007 application trash payload. Schema 008 embeds
// this authority in the top-level trash namespace occurrence.
type schema007TrashRecord struct {
	SchemaVersion   int              `json:"schemaVersion"`
	TrashID         string           `json:"trashID"`
	OwnerUserID     domain.UserID    `json:"ownerUserID"`
	OriginalPath    domain.UserPath  `json:"originalPath"`
	TrashedPath     domain.UserPath  `json:"trashedPath"`
	Kind            domain.EntryKind `json:"kind"`
	TrashedAt       time.Time        `json:"trashedAt"`
	OriginalVersion domain.Version   `json:"originalVersion"`
}

func schema008DomainIdentity(reference consistencyDomainRef) string {
	return storageformat.Digest([]byte(string(reference.Kind) + "\x00" + reference.ID))
}

func validateSchema008MigrationStage(value schema008MigrationStage) (consistencyDomainRef, []byte, error) {
	reference := consistencyDomainRef{Kind: value.DomainKind, ID: value.DomainID}
	if value.SchemaVersion != schema008MigrationStageSchema || value.SourceKey == "" || value.Key == "" || value.LogicalVersion == "" || value.Tree != "" && value.Tree != "base" && value.Tree != "outcomes" && value.Tree != "outcome-expiry" || validateConsistencyDomainRef(reference) != nil {
		return consistencyDomainRef{}, nil, domain.NewError(domain.ErrorInvalid, "invalid schema-008 migration stage")
	}
	body, err := storageformat.EncodeCanonical(value)
	if err != nil {
		return consistencyDomainRef{}, nil, err
	}
	return reference, body, nil
}

func (e *Engine) runStorageMigration007To008(ctx context.Context, transition storageMigration, superblockObject objectstore.Object, superblock storageformat.Superblock) error {
	e.observeMigration(MigrationProgress{MigrationID: transition.id.String(), Stage: MigrationStageStarted})
	if err := e.step(ctx, MigrationStepName(string(transition.id), StepMigrationAfterDetection)); err != nil {
		return err
	}
	if complete, err := e.storageMigrationComplete(ctx, transition); err == nil && complete {
		return nil
	} else if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if err := e.verifyMigrationWriterSet(ctx, transition); err != nil {
		return err
	}
	closed, err := e.closeStorageMigrationGate(ctx, transition, aggregateMigrationPlan{})
	if err != nil || !closed {
		return err
	}
	gate, active, err := e.readClosedStorageMigrationGate(ctx, transition)
	if err != nil || !active {
		return err
	}
	e.observeMigration(MigrationProgress{MigrationID: transition.id.String(), Stage: MigrationStageGateClosed})
	if err := e.step(ctx, MigrationStepName(string(transition.id), StepMigrationAfterGateClosed)); err != nil {
		return err
	}
	if err := e.stageSchema007State008(ctx); err != nil {
		return domain.WrapError(domain.KindOf(err), "stage schema-007 state", err)
	}
	if err := e.stageSchema007Uploads008(ctx); err != nil {
		return domain.WrapError(domain.KindOf(err), "stage schema-007 uploads", err)
	}
	if err := e.stageSchema007Operations008(ctx); err != nil {
		return domain.WrapError(domain.KindOf(err), "stage schema-007 operation outcomes", err)
	}
	if err := e.stageSchema007UploadIdempotency008(ctx); err != nil {
		return domain.WrapError(domain.KindOf(err), "stage schema-007 upload idempotency", err)
	}
	if err := e.stageSchema007RecoveredTrashMetadata008(ctx); err != nil {
		return domain.WrapError(domain.KindOf(err), "recover schema-007 trash metadata", err)
	}
	if err := e.stageSchema007Namespaces008(ctx); err != nil {
		return domain.WrapError(domain.KindOf(err), "stage schema-007 namespaces", err)
	}
	if err := e.installSchema008StagedDomains(ctx); err != nil {
		return domain.WrapError(domain.KindOf(err), "install schema-008 domains", err)
	}
	if err := e.freezeSchema008MigrationDomains(ctx, transition, gate.Epoch); err != nil {
		return domain.WrapError(domain.KindOf(err), "freeze migrated schema-008 domains", err)
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
	checkpoint, err := e.createCheckpointWhileClosed(ctx, transition.checkpointID)
	if err != nil {
		if complete, completeErr := e.storageMigrationComplete(ctx, transition); completeErr == nil && complete {
			return nil
		}
		return domain.WrapError(domain.KindOf(err), "create schema-008 migration checkpoint", err)
	}
	e.observeMigration(MigrationProgress{MigrationID: transition.id.String(), Stage: MigrationStageCheckpointCreated})
	if err := e.step(ctx, MigrationStepName(string(transition.id), StepMigrationAfterCheckpoint)); err != nil {
		return err
	}
	if err := e.openWritesAfterCreatedCheckpoint(ctx, checkpoint); err != nil {
		if complete, completeErr := e.storageMigrationComplete(ctx, transition); completeErr == nil && complete {
			return nil
		}
		return domain.WrapError(domain.KindOf(err), "open schema-008 migration checkpoint", err)
	}
	if err := newDomainCatalog(e.backend, e.scheduler).unfreeze(ctx, checkpoint.GateEpoch); err != nil {
		return domain.WrapError(domain.KindOf(err), "unfreeze migrated schema-008 domains", err)
	}
	e.observeMigration(MigrationProgress{MigrationID: transition.id.String(), Stage: MigrationStageComplete})
	return nil
}

// freezeSchema008MigrationDomains fences each domain-head freeze against the
// canonical migration gate. The catalog entry list alone is insufficient: a
// lagging worker can retain that list while the winner opens the gate and
// unfreezes the same domains. The check after each head CAS is the ordering
// point: when it still observes the closed gate, a later winner thaw includes
// the freeze; when it observes completion, this worker removes the complete
// old-epoch catalog freeze itself. A check before the CAS would not close any
// additional race window and would double the steady provider reads.
func (e *Engine) freezeSchema008MigrationDomains(ctx context.Context, transition storageMigration, epoch uint64) error {
	catalog := newDomainCatalog(e.backend, e.scheduler)
	entries, err := catalog.freeze(ctx, epoch)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		reference := consistencyDomainRef{Kind: entry.Kind, ID: entry.DomainID}
		if err := catalog.store.freeze(ctx, reference, epoch); err != nil {
			return domain.WrapError(domain.KindOf(err), "freeze registered schema-008 consistency domain", err)
		}
		if _, active, err := e.readClosedStorageMigrationGate(ctx, transition); err != nil {
			return err
		} else if !active {
			// Always retract this worker's own old-epoch head first. The
			// catalog may already belong to the next closure, in which case
			// catalog.unfreeze must (and will) reject the stale epoch while the
			// head-level cleanup remains both necessary and safe.
			if unfreezeErr := catalog.store.unfreeze(ctx, reference, epoch); unfreezeErr != nil && !errors.Is(unfreezeErr, domain.ErrConflict) && !errors.Is(unfreezeErr, domain.ErrNotFound) {
				return unfreezeErr
			}
			if unfreezeErr := catalog.unfreeze(ctx, epoch); unfreezeErr != nil && !errors.Is(unfreezeErr, domain.ErrConflict) && !errors.Is(unfreezeErr, domain.ErrNotFound) {
				return unfreezeErr
			}
			return nil
		}
	}
	return nil
}

func (e *Engine) stageSchema007State008(ctx context.Context) error {
	return visitObjectPages(ctx, e.backend, storageformat.StateRecordsPrefix(), func(info objectstore.ObjectInfo) error {
		if staged, err := e.schema008MigrationSourceStaged(ctx, info.Key); err != nil || staged {
			return err
		}
		object, err := e.backend.Get(ctx, info.Key)
		if err != nil {
			return err
		}
		var envelope storageformat.Envelope
		var record storageformat.StateRecord
		if err := storageformat.DecodeEnvelope(object.Body, info.Key, stateRecordSchema, &envelope, &record); err != nil || record.SchemaVersion != 1 {
			return domain.NewError(domain.ErrorInvalid, "invalid schema-007 state migration source")
		}
		key, err := parseExistingStateKey(record.LogicalKey)
		if err != nil || canonicalStateKey(key) != info.Key {
			return domain.NewError(domain.ErrorInvalid, "schema-007 state migration source key mismatch")
		}
		reference, err := stateDomainReferenceForKey(key)
		if err != nil {
			return err
		}
		stage := schema008MigrationStage{SchemaVersion: 1, SourceKey: info.Key.String(), DomainKind: reference.Kind, DomainID: reference.ID, Key: key.String(), Value: record.Data, LogicalVersion: envelope.LogicalVersion, MigrationOnly: strings.HasPrefix(key.String(), string(state.NamespaceTrash)+"/")}
		stageKey, err := e.writeSchema008MigrationStage(ctx, stage)
		if err != nil {
			return err
		}
		return e.markSchema008MigrationSource(ctx, info.Key, stageKey)
	})
}

func (e *Engine) writeSchema008MigrationStage(ctx context.Context, stage schema008MigrationStage) (objectstore.Key, error) {
	reference, body, err := validateSchema008MigrationStage(stage)
	if err != nil {
		return objectstore.Key{}, err
	}
	key := storageformat.Schema008MigrationStageKey(schema008DomainIdentity(reference), stage.SourceKey)
	if _, err := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		if !errors.Is(err, domain.ErrConflict) {
			return objectstore.Key{}, err
		}
		winner, getErr := e.backend.Get(ctx, key)
		if getErr != nil || !bytes.Equal(winner.Body, body) {
			return objectstore.Key{}, domain.NewError(domain.ErrorInvalid, "schema-008 migration stage winner differs")
		}
	}
	return key, nil
}

func (e *Engine) schema008MigrationSourceStaged(ctx context.Context, source objectstore.Key) (bool, error) {
	markerKey := storageformat.Schema008MigrationSourceMarkerKey(source.String())
	marker, err := e.backend.Get(ctx, markerKey)
	if errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var stored schema008MigrationSourceMarker
	if decodeCanonicalValue(marker.Body, &stored) != nil || stored.SchemaVersion != 1 || stored.SourceKey != source.String() || len(stored.StageKeys) == 0 {
		return false, domain.NewError(domain.ErrorInvalid, "invalid schema-008 migration source marker")
	}
	for _, value := range stored.StageKeys {
		stageKey, err := objectstore.ParseKey(value)
		if err != nil {
			return false, err
		}
		if _, err := e.backend.Head(ctx, stageKey); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (e *Engine) markSchema008MigrationSource(ctx context.Context, source objectstore.Key, stages ...objectstore.Key) error {
	if len(stages) == 0 {
		return domain.NewError(domain.ErrorInvalid, "schema-008 migration source has no stages")
	}
	stageKeys := make([]string, len(stages))
	for index, stage := range stages {
		stageKeys[index] = stage.String()
	}
	sort.Strings(stageKeys)
	marker := schema008MigrationSourceMarker{SchemaVersion: 1, SourceKey: source.String(), StageKeys: stageKeys}
	body, err := storageformat.EncodeCanonical(marker)
	if err != nil {
		return err
	}
	key := storageformat.Schema008MigrationSourceMarkerKey(source.String())
	if _, err := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err == nil {
		return nil
	} else if !errors.Is(err, domain.ErrConflict) {
		return err
	}
	winner, err := e.backend.Get(ctx, key)
	if err != nil || !bytes.Equal(winner.Body, body) {
		return domain.NewError(domain.ErrorInvalid, "schema-008 migration source marker winner differs")
	}
	return nil
}

func (e *Engine) stageSchema007Uploads008(ctx context.Context) error {
	return visitObjectPages(ctx, e.backend, storageformat.OperationPrefix(), func(info objectstore.ObjectInfo) error {
		if staged, err := e.schema008MigrationSourceStaged(ctx, info.Key); err != nil || staged {
			return err
		}
		object, err := e.backend.Get(ctx, info.Key)
		if err != nil {
			return err
		}
		var generic storageformat.Envelope
		if err := state.DecodeJSONWithLimit(object.Body, &generic, storageformat.MaxCanonicalBytes); err != nil {
			return err
		}
		if generic.Schema != uploadRecordSchema {
			return nil
		}
		var envelope storageformat.Envelope
		var legacy storageformat.UploadRecord
		if err := storageformat.DecodeEnvelope(object.Body, info.Key, uploadRecordSchema, &envelope, &legacy); err != nil {
			return err
		}
		owner, err := domain.ParseUserID(legacy.UserID)
		if err != nil || storageformat.OperationKey(legacy.UserID, legacy.UploadID) != info.Key || legacy.CompletionOperationID == "" || legacy.State == storageformat.UploadActive {
			return domain.NewError(domain.ErrorInvalid, "invalid terminal schema-007 upload")
		}
		portable := storageformat.PortableUploadRecord{
			SchemaVersion: 1, UploadID: legacy.UploadID, OwnerID: owner.String(), Area: legacy.Area,
			RequestedPath: legacy.RequestedPath, ResolvedPath: legacy.ResolvedPath, BlobID: legacy.UploadID,
			Size: legacy.Size, MediaType: legacy.MediaType, Conflict: legacy.Conflict, ExpectedVersion: legacy.ExpectedVersion,
			TargetExisted: legacy.TargetExisted, Resumable: legacy.Resumable, State: legacy.State,
			CreatedAt: legacy.CreatedAt.UTC(), ExpiresAt: legacy.ExpiresAt.UTC(),
		}
		if err := storageformat.ValidatePortableUploadRecord(portable); err != nil {
			return err
		}
		body, err := storageformat.EncodeCanonical(portable)
		if err != nil {
			return err
		}
		reference := uploadDomainReference(owner)
		stage, err := e.writeSchema008MigrationStage(ctx, schema008MigrationStage{SchemaVersion: 1, SourceKey: info.Key.String(), DomainKind: reference.Kind, DomainID: reference.ID, Key: uploadRecordKey(legacy.UploadID), Value: body, LogicalVersion: envelope.LogicalVersion})
		if err != nil {
			return err
		}
		return e.markSchema008MigrationSource(ctx, info.Key, stage)
	})
}

func (e *Engine) stageSchema007UploadIdempotency008(ctx context.Context) error {
	return visitObjectPages(ctx, e.backend, storageformat.IdempotencyPrefix(), func(info objectstore.ObjectInfo) error {
		if staged, err := e.schema008MigrationSourceStaged(ctx, info.Key); err != nil || staged {
			return err
		}
		object, err := e.backend.Get(ctx, info.Key)
		if err != nil {
			return err
		}
		var envelope storageformat.Envelope
		var legacy storageformat.IdempotencyRecord
		if err := storageformat.DecodeEnvelope(object.Body, info.Key, idempotencySchema, &envelope, &legacy); err != nil {
			return err
		}
		owner, err := domain.ParseUserID(legacy.UserID)
		if err != nil || legacy.SchemaVersion != 1 || legacy.KeyDigest == "" || legacy.Fingerprint == "" || legacy.OperationID == "" {
			return domain.NewError(domain.ErrorInvalid, "invalid schema-007 idempotency record")
		}
		expectedKey, err := storageformat.Schema007IdempotencyKeyFromDigest(owner.String(), legacy.KeyDigest)
		if err != nil || expectedKey != info.Key {
			return domain.NewError(domain.ErrorInvalid, "schema-007 idempotency key binding mismatch")
		}
		if legacy.Kind != "upload" {
			if legacy.Kind != operationCopy && legacy.Kind != operationMove && legacy.Kind != operationDelete {
				return domain.NewError(domain.ErrorInvalid, "unsupported schema-007 file-operation idempotency kind")
			}
			operationKey := storageformat.OperationKey(owner.String(), legacy.OperationID)
			_, _, operation, err := e.Files().readFileOperationObject(ctx, operationKey)
			if err != nil {
				return err
			}
			if operation.UserID != owner.String() || operation.Kind != legacy.Kind || operation.IntentFingerprint != "" && operation.IntentFingerprint != legacy.Fingerprint || operation.State != storageformat.FileOperationSucceeded && operation.State != storageformat.FileOperationFailed {
				return domain.NewError(domain.ErrorInvalid, "schema-007 idempotency operation binding mismatch")
			}
			mutationID := string(namespaceOperationIDFromKeyDigest(owner, legacy.Kind, legacy.KeyDigest))
			requestFingerprint := schema008MigrationDigest(legacy.Fingerprint, "endlessfs-schema008-migrated-request-v1\x00"+storageformat.Digest(object.Body))
			resultBody, err := storageformat.EncodeCanonical(storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: requestFingerprint, Operation: ptrDomainOperation(domainFileOperation(operation))})
			if err != nil {
				return err
			}
			retainUntil := operation.UpdatedAt.UTC().Add(terminalOperationRetention)
			outcome := storageformat.DomainOutcome{MutationID: mutationID, Fingerprint: storageformat.Digest([]byte("endlessfs-schema008-migrated-idempotent-outcome-v1\x00" + storageformat.Digest(object.Body) + "\x00" + legacy.KeyDigest)), Revision: 1, RetainUntil: retainUntil, Result: resultBody}
			outcomeBody, err := storageformat.EncodeCanonical(outcome)
			if err != nil {
				return err
			}
			reference := namespaceReference(owner)
			outcomeStage, err := e.writeSchema008MigrationStage(ctx, schema008MigrationStage{SchemaVersion: 1, SourceKey: info.Key.String() + "#outcome", DomainKind: reference.Kind, DomainID: reference.ID, Key: mutationID, Value: outcomeBody, LogicalVersion: storageformat.Digest(append([]byte("endlessfs-consistency-domain-outcome-v1\x00"), outcomeBody...)), Tree: "outcomes"})
			if err != nil {
				return err
			}
			expiryKey := consistencyDomainOutcomeExpiryKey(retainUntil, mutationID)
			expiryStage, err := e.writeSchema008MigrationStage(ctx, schema008MigrationStage{SchemaVersion: 1, SourceKey: info.Key.String() + "#outcome-expiry", DomainKind: reference.Kind, DomainID: reference.ID, Key: expiryKey, Value: []byte(mutationID), LogicalVersion: storageformat.Digest([]byte("endlessfs-consistency-domain-outcome-expiry-v1\x00" + expiryKey + "\x00" + mutationID)), Tree: "outcome-expiry"})
			if err != nil {
				return err
			}
			return e.markSchema008MigrationSource(ctx, info.Key, outcomeStage, expiryStage)
		}
		portable := storageformat.PortableUploadIdempotency{SchemaVersion: 1, OwnerID: owner.String(), KeyDigest: legacy.KeyDigest, Fingerprint: legacy.Fingerprint, UploadID: legacy.OperationID}
		if err := storageformat.ValidatePortableUploadIdempotency(portable); err != nil {
			return err
		}
		uploadSource := storageformat.OperationKey(owner.String(), legacy.OperationID)
		uploadStageKey := storageformat.Schema008MigrationStageKey(schema008DomainIdentity(uploadDomainReference(owner)), uploadSource.String())
		uploadStageObject, err := e.backend.Get(ctx, uploadStageKey)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return domain.NewError(domain.ErrorInvalid, "schema-007 upload idempotency target is missing")
			}
			return err
		}
		var uploadStage schema008MigrationStage
		var upload storageformat.PortableUploadRecord
		decodeStageErr := decodeCanonicalValue(uploadStageObject.Body, &uploadStage)
		uploadReference, _, stageErr := validateSchema008MigrationStage(uploadStage)
		if decodeStageErr != nil || stageErr != nil || uploadReference != uploadDomainReference(owner) || uploadStage.Tree != "" || uploadStage.MigrationOnly || uploadStage.SourceKey != uploadSource.String() || uploadStage.Key != uploadRecordKey(legacy.OperationID) || decodeCanonicalValue(uploadStage.Value, &upload) != nil || storageformat.ValidatePortableUploadRecord(upload) != nil || upload.OwnerID != owner.String() || upload.UploadID != legacy.OperationID || upload.BlobID != legacy.OperationID {
			return domain.NewError(domain.ErrorInvalid, "schema-007 upload idempotency target is invalid")
		}
		body, err := storageformat.EncodeCanonical(portable)
		if err != nil {
			return err
		}
		reference := uploadDomainReference(owner)
		stage, err := e.writeSchema008MigrationStage(ctx, schema008MigrationStage{SchemaVersion: 1, SourceKey: info.Key.String(), DomainKind: reference.Kind, DomainID: reference.ID, Key: "upload-idempotency/" + legacy.KeyDigest, Value: body, LogicalVersion: envelope.LogicalVersion})
		if err != nil {
			return err
		}
		return e.markSchema008MigrationSource(ctx, info.Key, stage)
	})
}

func schema008MigrationDigest(value, fallback string) string {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == value {
		return value
	}
	return storageformat.Digest([]byte(fallback))
}

func (e *Engine) stageSchema007Operations008(ctx context.Context) error {
	return visitObjectPages(ctx, e.backend, storageformat.OperationPrefix(), func(info objectstore.ObjectInfo) error {
		if staged, err := e.schema008MigrationSourceStaged(ctx, info.Key); err != nil || staged {
			return err
		}
		object, err := e.backend.Get(ctx, info.Key)
		if err != nil {
			return err
		}
		var generic storageformat.Envelope
		if err := state.DecodeJSONWithLimit(object.Body, &generic, storageformat.MaxCanonicalBytes); err != nil {
			return err
		}
		if generic.Schema != fileOperationSchema {
			return nil
		}
		_, _, operation, err := e.Files().readFileOperationObject(ctx, info.Key)
		if err != nil {
			return err
		}
		if operation.State != storageformat.FileOperationSucceeded && operation.State != storageformat.FileOperationFailed {
			return domain.NewError(domain.ErrorInvalid, "schema-007 operation did not quiesce before migration")
		}
		switch operation.Kind {
		case operationCopy, operationMove, operationDelete, operationCreateDirectory, "upload-complete":
		default:
			return domain.NewError(domain.ErrorInvalid, "unsupported terminal schema-007 operation kind")
		}
		owner, err := domain.ParseUserID(operation.UserID)
		if err != nil || storageformat.OperationKey(operation.UserID, operation.OperationID) != info.Key {
			return domain.NewError(domain.ErrorInvalid, "invalid terminal schema-007 operation")
		}
		requestFingerprint := schema008MigrationDigest(operation.IntentFingerprint, "endlessfs-schema008-migrated-request-v1\x00"+storageformat.Digest(object.Body))
		resultBody, err := storageformat.EncodeCanonical(storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: requestFingerprint, Operation: ptrDomainOperation(domainFileOperation(operation))})
		if err != nil {
			return err
		}
		retainUntil := operation.UpdatedAt.UTC().Add(terminalOperationRetention)
		outcome := storageformat.DomainOutcome{
			MutationID:  operation.OperationID,
			Fingerprint: storageformat.Digest([]byte("endlessfs-schema008-migrated-outcome-v1\x00" + storageformat.Digest(object.Body))),
			Revision:    1, RetainUntil: retainUntil, Result: resultBody,
		}
		outcomeBody, err := storageformat.EncodeCanonical(outcome)
		if err != nil {
			return err
		}
		reference := namespaceReference(owner)
		outcomeStage, err := e.writeSchema008MigrationStage(ctx, schema008MigrationStage{
			SchemaVersion: 1, SourceKey: info.Key.String() + "#outcome", DomainKind: reference.Kind, DomainID: reference.ID,
			Key: operation.OperationID, Value: outcomeBody, LogicalVersion: storageformat.Digest(append([]byte("endlessfs-consistency-domain-outcome-v1\x00"), outcomeBody...)), Tree: "outcomes",
		})
		if err != nil {
			return err
		}
		expiryKey := consistencyDomainOutcomeExpiryKey(retainUntil, operation.OperationID)
		expiryStage, err := e.writeSchema008MigrationStage(ctx, schema008MigrationStage{
			SchemaVersion: 1, SourceKey: info.Key.String() + "#outcome-expiry", DomainKind: reference.Kind, DomainID: reference.ID,
			Key: expiryKey, Value: []byte(operation.OperationID), LogicalVersion: storageformat.Digest([]byte("endlessfs-consistency-domain-outcome-expiry-v1\x00" + expiryKey + "\x00" + operation.OperationID)), Tree: "outcome-expiry",
		})
		if err != nil {
			return err
		}
		return e.markSchema008MigrationSource(ctx, info.Key, outcomeStage, expiryStage)
	})
}

func ptrDomainOperation(value domain.Operation) *domain.Operation { return &value }

type schema007TrashMigrationKey struct {
	owner   domain.UserID
	trashID string
}

type schema007TrashRecoveryCandidate struct {
	metadata  storageformat.NamespaceTrashMetadata
	target    storageformat.DuplicateOccurrence
	updatedAt time.Time
	operation string
}

func schema007RecoveredTrashSource(owner domain.UserID, trashID string) string {
	return "schema-007-recovered-trash-v1/" + owner.String() + "/" + trashID
}

// stageSchema007RecoveredTrashMetadata008 repairs only the historical case in
// which callers used the schema-007 low-level live-to-trash Move operation
// without creating the application Trash record. The committed operation's
// duplicate-occurrence roots contain the exact pre-move live path/version and
// post-move trash path/version, so recovery does not infer values from names or
// timestamps. Application-authored Trash records always take precedence.
func (e *Engine) stageSchema007RecoveredTrashMetadata008(ctx context.Context) error {
	current := make(map[schema007TrashMigrationKey]storageformat.DirectoryEntry)
	if err := visitObjectPages(ctx, e.backend, storageformat.FilesystemPrefix(), func(info objectstore.ObjectInfo) error {
		ownerValue, areaValue, directoryID, matched, err := storageformat.ParseDirectoryRootKey(info.Key)
		if err != nil || !matched || areaValue != "trash" || directoryID != storageformat.RootDirectoryID {
			return err
		}
		owner, _ := domain.ParseUserID(ownerValue) // ParseDirectoryRootKey authenticated the owner.
		scope, _ := domain.NewScope(owner, domain.AreaTrash)
		root, err := e.readMigrationDirectoryRoot(ctx, scope, directoryID)
		if err != nil {
			return err
		}
		manifest, err := e.readMigrationDirectoryManifest(ctx, scope, directoryID, root.manifestID)
		if err != nil {
			return err
		}
		return e.visitSchema007DirectoryEntries(ctx, scope, directoryID, manifest.manifest, func(entry storageformat.DirectoryEntry) error {
			key := schema007TrashMigrationKey{owner: owner, trashID: entry.Name}
			if _, exists := current[key]; exists {
				return domain.NewError(domain.ErrorInvalid, "schema-007 trash root contains duplicate names")
			}
			current[key] = entry
			return nil
		})
	}); err != nil {
		return err
	}
	if len(current) == 0 {
		return nil
	}
	missing := make(map[schema007TrashMigrationKey]storageformat.DirectoryEntry)
	for key, entry := range current {
		metadata, err := e.schema007ExplicitTrashMetadata008(ctx, key.owner, key.trashID, entry.Kind)
		if err != nil {
			return err
		}
		if metadata == nil {
			missing[key] = entry
		}
	}
	if len(missing) == 0 {
		return nil
	}

	candidates := make(map[schema007TrashMigrationKey]schema007TrashRecoveryCandidate)
	if err := visitObjectPages(ctx, e.backend, storageformat.OperationPrefix(), func(info objectstore.ObjectInfo) error {
		object, err := e.backend.Get(ctx, info.Key)
		if err != nil {
			return err
		}
		var generic storageformat.Envelope
		if err := state.DecodeJSONWithLimit(object.Body, &generic, storageformat.MaxCanonicalBytes); err != nil {
			return err
		}
		if generic.Schema != fileOperationSchema {
			return nil
		}
		_, _, operation, err := e.Files().readFileOperationObject(ctx, info.Key)
		if err != nil {
			return err
		}
		if operation.Kind != operationMove || operation.State != storageformat.FileOperationSucceeded {
			return nil
		}
		owner, err := domain.ParseUserID(operation.UserID)
		if err != nil {
			return err
		}
		var live, trash []storageformat.DuplicateOccurrence
		if err := e.Files().forEachFileOperationStepPage(ctx, operation, func(page storageformat.FileOperationStepPage) error {
			for _, root := range page.Roots {
				if !strings.HasPrefix(root.Key, storageformat.DuplicateOccurrenceOwnerPrefix(owner.String())) || !strings.Contains(root.Key, "/occurrences/") {
					continue
				}
				if len(root.RollbackBody) != 0 {
					occurrence, err := decodeSchema007OperationOccurrence008(root.Key, root.RollbackBody, owner)
					if err != nil {
						continue
					}
					if occurrence != nil && occurrence.Area == "live" {
						live = append(live, *occurrence)
					}
				}
				occurrence, err := decodeSchema007OperationOccurrence008(root.Key, root.FinalBody, owner)
				if err != nil {
					continue
				}
				if occurrence != nil && occurrence.Area == "trash" {
					trash = append(trash, *occurrence)
				}
			}
			return nil
		}); err != nil {
			if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrInvalid) {
				return nil
			}
			return err
		}
		for _, target := range trash {
			trashPath, err := domain.ParseUserPath(target.Path)
			if err != nil || trashPath.IsRoot() || !trashPath.Parent().IsRoot() {
				continue
			}
			key := schema007TrashMigrationKey{owner: owner, trashID: trashPath.Name()}
			entry, exists := missing[key]
			if !exists || target.Version != entry.LogicalVersion || target.Kind == domain.DuplicateFile && entry.Kind != domain.EntryFile || target.Kind == domain.DuplicateDirectory && entry.Kind != domain.EntryDirectory {
				continue
			}
			var source *storageformat.DuplicateOccurrence
			for index := range live {
				candidate := &live[index]
				if candidate.GroupID != target.GroupID || candidate.Kind != target.Kind || candidate.Size != target.Size || candidate.FileCount != target.FileCount {
					continue
				}
				if source == nil {
					source = candidate
					continue
				}
				left, leftErr := domain.ParseUserPath(source.Path)
				right, rightErr := domain.ParseUserPath(candidate.Path)
				if leftErr != nil || rightErr != nil || len(left.Segments()) == len(right.Segments()) {
					return domain.NewError(domain.ErrorInvalid, "schema-007 trash operation has ambiguous live source")
				}
				if len(right.Segments()) < len(left.Segments()) {
					source = candidate
				}
			}
			if source == nil || source.Version == "" {
				continue
			}
			originalPath, err := domain.ParseUserPath(source.Path)
			if err != nil || originalPath.IsRoot() {
				return domain.NewError(domain.ErrorInvalid, "schema-007 trash operation has invalid live source")
			}
			candidate := schema007TrashRecoveryCandidate{
				metadata: storageformat.NamespaceTrashMetadata{OriginalPath: originalPath.String(), OriginalVersion: domain.Version(source.Version), TrashedAt: operation.UpdatedAt.UTC()},
				target:   target, updatedAt: operation.UpdatedAt.UTC(), operation: operation.OperationID,
			}
			winner, exists := candidates[key]
			if !exists || candidate.updatedAt.After(winner.updatedAt) || candidate.updatedAt.Equal(winner.updatedAt) && candidate.operation > winner.operation {
				candidates[key] = candidate
			}
		}
		return nil
	}); err != nil {
		return err
	}

	for key, entry := range missing {
		candidate, found := candidates[key]
		if !found {
			// Epochs before trash records and duplicate-operation transitions had no
			// durable representation of the original path. Preserve those otherwise
			// valid entries under their current top-level name instead of making the
			// entire bucket unavailable. This deterministic salvage path is used only
			// when neither of the later authoritative sources exists.
			trashedAt := entry.ModifiedAt.UTC()
			if trashedAt.IsZero() {
				trashedAt = time.Unix(0, 0).UTC()
			}
			candidate = schema007TrashRecoveryCandidate{metadata: storageformat.NamespaceTrashMetadata{
				OriginalPath: "/" + key.trashID, OriginalVersion: domain.Version(entry.LogicalVersion), TrashedAt: trashedAt,
			}}
		}
		record := schema007TrashRecord{
			SchemaVersion: 1, TrashID: key.trashID, OwnerUserID: key.owner,
			OriginalPath: domain.MustParseUserPath(candidate.metadata.OriginalPath), TrashedPath: domain.MustParseUserPath("/" + key.trashID),
			Kind: entry.Kind, TrashedAt: candidate.metadata.TrashedAt, OriginalVersion: candidate.metadata.OriginalVersion,
		}
		body, err := storageformat.EncodeCanonical(record)
		if err != nil {
			return err
		}
		logicalKey := state.MustKey(state.NamespaceTrash, key.owner.String(), key.trashID)
		reference, _ := stateDomainReferenceForKey(logicalKey) // The fixed trash namespace has an exact route.
		_, err = e.writeSchema008MigrationStage(ctx, schema008MigrationStage{
			SchemaVersion: 1, SourceKey: schema007RecoveredTrashSource(key.owner, key.trashID), DomainKind: reference.Kind, DomainID: reference.ID,
			Key: logicalKey.String(), Value: body, LogicalVersion: storageformat.Digest(append([]byte("endlessfs-schema008-recovered-trash-v1\x00"), body...)), MigrationOnly: true,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func decodeSchema007OperationOccurrence008(keyValue string, body []byte, owner domain.UserID) (*storageformat.DuplicateOccurrence, error) {
	key, err := objectstore.ParseKey(keyValue)
	if err != nil {
		return nil, err
	}
	var envelope storageformat.Envelope
	var root storageformat.DuplicateOccurrenceRoot
	if err := storageformat.DecodeEnvelope(body, key, duplicateOccurrenceSchema, &envelope, &root); err != nil {
		return nil, err
	}
	if root.SchemaVersion != 1 || root.UserID != owner.String() || root.Pending != nil {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid schema-007 operation occurrence")
	}
	if root.Current == nil {
		return nil, nil
	}
	current := root.Current
	if current.GroupID == "" || current.Version == "" || current.Path == "" || current.Area != "live" && current.Area != "trash" || current.Kind != domain.DuplicateFile && current.Kind != domain.DuplicateDirectory || storageformat.DuplicateOccurrenceKey(owner.String(), string(current.Kind), current.GroupID, current.Area, current.Path) != key {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid schema-007 operation occurrence binding")
	}
	return current, nil
}

func (e *Engine) stageSchema007Namespaces008(ctx context.Context) error {
	return visitObjectPages(ctx, e.backend, storageformat.FilesystemPrefix(), func(info objectstore.ObjectInfo) error {
		ownerValue, areaValue, directoryID, matched, err := storageformat.ParseDirectoryRootKey(info.Key)
		if err != nil || !matched || directoryID != storageformat.RootDirectoryID {
			return err
		}
		owner, _ := domain.ParseUserID(ownerValue) // ParseDirectoryRootKey authenticated the owner.
		area := domain.AreaLive
		if areaValue == "trash" {
			area = domain.AreaTrash
		}
		reference := namespaceReference(owner)
		stageKey := storageformat.Schema008MigrationStageKey(schema008DomainIdentity(reference), info.Key.String())
		if _, err := e.backend.Head(ctx, stageKey); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		scope, _ := domain.NewScope(owner, area) // Canonical owner and area were authenticated above.
		root, err := e.readMigrationDirectoryRoot(ctx, scope, storageformat.RootDirectoryID)
		if err != nil {
			return err
		}
		entry, err := e.buildSchema008NamespaceSubtree(ctx, owner, area, area, namespaceRootPath(), storageformat.RootDirectoryID, root.manifestID, nil, make(map[string]struct{}))
		if err != nil {
			return err
		}
		body, err := encodeNamespaceEntry(entry)
		if err != nil {
			return err
		}
		_, err = e.writeSchema008MigrationStage(ctx, schema008MigrationStage{SchemaVersion: 1, SourceKey: info.Key.String(), DomainKind: reference.Kind, DomainID: reference.ID, Key: namespaceRootKey(area), Value: body, LogicalVersion: entry.Entry.LogicalVersion})
		return err
	})
}

func (e *Engine) buildSchema008NamespaceSubtree(ctx context.Context, owner domain.UserID, logicalArea, storageArea domain.Area, logicalPath domain.UserPath, directoryID, manifestID string, parentMetadata *storageformat.DirectoryEntry, active map[string]struct{}) (storageformat.NamespaceEntry, error) {
	sourceID := owner.String() + "\x00" + areaName(logicalArea) + "\x00" + logicalPath.String() + "\x00" + areaName(storageArea) + "\x00" + directoryID + "\x00" + manifestID
	activeID := owner.String() + "\x00" + areaName(storageArea) + "\x00" + directoryID + "\x00" + manifestID
	cacheKey := storageformat.Schema008MigrationSubtreeKey(storageformat.Digest([]byte(sourceID)))
	if object, err := e.backend.Get(ctx, cacheKey); err == nil {
		var cached schema008MigrationSubtree
		if decodeCanonicalValue(object.Body, &cached) != nil || cached.SchemaVersion != 1 || cached.SourceID != sourceID || storageformat.ValidateNamespaceEntry(cached.Entry) != nil {
			return storageformat.NamespaceEntry{}, domain.NewError(domain.ErrorInvalid, "invalid cached schema-008 migration subtree")
		}
		return cached.Entry, nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return storageformat.NamespaceEntry{}, err
	}
	if _, found := active[activeID]; found {
		return storageformat.NamespaceEntry{}, domain.NewError(domain.ErrorInvalid, "schema-007 namespace graph contains a cycle")
	}
	active[activeID] = struct{}{}
	defer delete(active, activeID)
	scope, _ := domain.NewScope(owner, storageArea)
	if manifestID == "" {
		root, err := e.readMigrationDirectoryRoot(ctx, scope, directoryID)
		if err != nil {
			return storageformat.NamespaceEntry{}, err
		}
		manifestID = root.manifestID
	}
	manifest, err := e.readMigrationDirectoryManifest(ctx, scope, directoryID, manifestID)
	if err != nil {
		return storageformat.NamespaceEntry{}, err
	}
	session := newConsistencyDomainTreeSession(e.stateDomainStore(), namespaceReference(owner))
	runs := newSchema008MigrationRuns(ctx, session)
	err = e.visitSchema007DirectoryEntries(ctx, scope, directoryID, manifest.manifest, func(legacy storageformat.DirectoryEntry) error {
		childPath, err := logicalPath.Join(legacy.Name)
		if err != nil {
			return err
		}
		var child storageformat.NamespaceEntry
		if legacy.Kind == domain.EntryDirectory {
			childStorageArea := storageArea
			if legacy.StorageArea == "live" {
				childStorageArea = domain.AreaLive
			} else if legacy.StorageArea == "trash" {
				childStorageArea = domain.AreaTrash
			} else if legacy.StorageArea != "" {
				return domain.NewError(domain.ErrorInvalid, "invalid schema-007 directory storage area")
			}
			child, err = e.buildSchema008NamespaceSubtree(ctx, owner, logicalArea, childStorageArea, childPath, legacy.DirectoryID, legacy.ManifestID, &legacy, active)
		} else {
			legacy.ManifestID, legacy.StorageArea, legacy.SHA256 = "", "", ""
			legacy.LogicalVersion, err = directoryEntryVersion(legacy)
			child = storageformat.NamespaceEntry{SchemaVersion: 1, NodeID: storageformat.Digest([]byte("endlessfs-schema008-migrated-file-node-v1\x00" + owner.String() + "\x00" + areaName(logicalArea) + "\x00" + childPath.String() + "\x00" + legacy.BlobID)), Entry: legacy}
		}
		if err != nil {
			return err
		}
		if logicalArea == domain.AreaTrash && logicalPath.IsRoot() {
			metadata, err := e.schema007TrashMetadata008(ctx, owner, childPath.Name(), child.Entry.Kind)
			if err != nil {
				return err
			}
			child.Trash = metadata
		}
		body, err := encodeNamespaceEntry(child)
		if err != nil {
			return err
		}
		return runs.Add(storageformat.DomainEntry{Key: child.Entry.Name, Value: body, LogicalVersion: child.Entry.LogicalVersion})
	})
	if err != nil {
		return storageformat.NamespaceEntry{}, err
	}
	children, err := runs.Finish()
	if err != nil {
		return storageformat.NamespaceEntry{}, err
	}
	nodeID := "root-" + areaName(logicalArea)
	entry := storageformat.DirectoryEntry{Kind: domain.EntryDirectory, DirectoryID: nodeID, Size: manifest.manifest.RecursiveBytes, FileCount: manifest.manifest.RecursiveFileCount, ContentDigest: manifest.manifest.ContentDigest, ModifiedAt: manifest.manifest.CreatedAt.UTC()}
	if parentMetadata != nil {
		entry = *parentMetadata
		nodeID = storageformat.Digest([]byte("endlessfs-schema008-migrated-directory-node-v1\x00" + owner.String() + "\x00" + areaName(logicalArea) + "\x00" + logicalPath.String() + "\x00" + directoryID + "\x00" + manifestID))
		entry.DirectoryID = nodeID
	}
	entry.ManifestID, entry.StorageArea, entry.SHA256 = "", "", ""
	entry.Size, entry.FileCount, entry.ContentDigest = manifest.manifest.RecursiveBytes, manifest.manifest.RecursiveFileCount, manifest.manifest.ContentDigest
	entry.LogicalVersion, err = directoryEntryVersion(entry)
	if err != nil {
		return storageformat.NamespaceEntry{}, err
	}
	if manifest.manifest.EntryCount < 0 {
		return storageformat.NamespaceEntry{}, domain.NewError(domain.ErrorInvalid, "legacy directory manifest has a negative entry count")
	}
	result := storageformat.NamespaceEntry{SchemaVersion: 1, NodeID: nodeID, Entry: entry, Children: children, EntryCount: uint64(manifest.manifest.EntryCount), ContentAccumulator: manifest.manifest.ContentAccumulator}
	if err := storageformat.ValidateNamespaceEntry(result); err != nil {
		return storageformat.NamespaceEntry{}, err
	}
	cached := schema008MigrationSubtree{SchemaVersion: 1, SourceID: sourceID, Entry: result}
	body, err := storageformat.EncodeCanonical(cached)
	if err != nil {
		return storageformat.NamespaceEntry{}, err
	}
	if _, err := e.backend.Put(ctx, cacheKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		if !errors.Is(err, domain.ErrConflict) {
			return storageformat.NamespaceEntry{}, err
		}
		winner, getErr := e.backend.Get(ctx, cacheKey)
		if getErr != nil || !bytes.Equal(winner.Body, body) {
			return storageformat.NamespaceEntry{}, domain.NewError(domain.ErrorConflict, "schema-008 migration subtree winner differs")
		}
	}
	return result, nil
}

func (e *Engine) visitSchema007DirectoryEntries(ctx context.Context, scope domain.Scope, directoryID string, manifest storageformat.DirectoryManifest, visit func(storageformat.DirectoryEntry) error) error {
	if manifest.SchemaVersion == 1 {
		count := 0
		for _, pageID := range manifest.PageIDs {
			key := storageformat.DirectoryPageKey(scope.UserID().String(), areaName(scope.Area()), directoryID, pageID)
			object, err := e.backend.Get(ctx, key)
			if err != nil {
				return err
			}
			var envelope storageformat.Envelope
			var page storageformat.DirectoryPage
			if err := storageformat.DecodeEnvelope(object.Body, key, directoryPageSchema, &envelope, &page); err != nil || page.SchemaVersion != 1 || page.DirectoryID != directoryID || page.PageID != pageID || len(page.Entries) > maxEntriesPerPage {
				return domain.NewError(domain.ErrorInvalid, "invalid schema-007 directory page")
			}
			for _, entry := range page.Entries {
				if err := visit(entry); err != nil {
					return err
				}
				count++
			}
		}
		if count != manifest.EntryCount {
			return domain.NewError(domain.ErrorInvalid, "schema-007 directory page count mismatch")
		}
		return nil
	}
	after, count := "", 0
	for {
		entries, err := e.Files().collectDirectoryIndexEntries(ctx, scope, directoryID, manifest, after, false, domainPageMaximumItems)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := visit(entry); err != nil {
				return err
			}
			count++
			after = entry.Name
		}
		if len(entries) < domainPageMaximumItems {
			break
		}
	}
	if count != manifest.EntryCount {
		return domain.NewError(domain.ErrorInvalid, "schema-007 directory index count mismatch")
	}
	return nil
}

func (e *Engine) schema007TrashMetadata008(ctx context.Context, owner domain.UserID, trashID string, kind domain.EntryKind) (*storageformat.NamespaceTrashMetadata, error) {
	metadata, err := e.schema007ExplicitTrashMetadata008(ctx, owner, trashID, kind)
	if err != nil || metadata != nil {
		return metadata, err
	}
	key := state.MustKey(state.NamespaceTrash, owner.String(), trashID)
	reference, _ := stateDomainReferenceForKey(key) // The fixed trash namespace has an exact route.
	return e.readSchema007TrashMetadataStage008(ctx, storageformat.Schema008MigrationStageKey(schema008DomainIdentity(reference), schema007RecoveredTrashSource(owner, trashID)), schema007RecoveredTrashSource(owner, trashID), key, owner, trashID, kind)
}

func (e *Engine) schema007ExplicitTrashMetadata008(ctx context.Context, owner domain.UserID, trashID string, kind domain.EntryKind) (*storageformat.NamespaceTrashMetadata, error) {
	key := state.MustKey(state.NamespaceTrash, owner.String(), trashID)
	reference, _ := stateDomainReferenceForKey(key) // The fixed trash namespace has an exact route.
	sourceKey := canonicalStateKey(key)
	stageKey := storageformat.Schema008MigrationStageKey(schema008DomainIdentity(reference), sourceKey.String())
	return e.readSchema007TrashMetadataStage008(ctx, stageKey, sourceKey.String(), key, owner, trashID, kind)
}

func (e *Engine) readSchema007TrashMetadataStage008(ctx context.Context, stageKey objectstore.Key, sourceKey string, key state.Key, owner domain.UserID, trashID string, kind domain.EntryKind) (*storageformat.NamespaceTrashMetadata, error) {
	object, err := e.backend.Get(ctx, stageKey)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var stage schema008MigrationStage
	if err := decodeCanonicalValue(object.Body, &stage); err != nil || !stage.MigrationOnly || stage.SourceKey != sourceKey || stage.Key != key.String() {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid staged schema-007 trash metadata")
	}
	var record schema007TrashRecord
	if err := state.DecodeJSONWithLimit(stage.Value, &record, storageformat.MaxCanonicalBytes); err != nil || record.SchemaVersion != 1 || record.OwnerUserID != owner || record.TrashID != trashID || record.Kind != kind || record.TrashedPath.String() != "/"+trashID || record.OriginalPath.IsRoot() || record.OriginalVersion == "" || record.TrashedAt.IsZero() {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid schema-007 trash metadata")
	}
	return &storageformat.NamespaceTrashMetadata{OriginalPath: record.OriginalPath.String(), OriginalVersion: record.OriginalVersion, TrashedAt: record.TrashedAt.UTC()}, nil
}

type schema008MigrationRuns struct {
	ctx     context.Context
	session *consistencyDomainTreeSession
	chunk   []storageformat.DomainEntry
	levels  [][]storageformat.DomainTreeRoot
}

func newSchema008MigrationRuns(ctx context.Context, session *consistencyDomainTreeSession) *schema008MigrationRuns {
	return &schema008MigrationRuns{ctx: ctx, session: session, chunk: make([]storageformat.DomainEntry, 0, domainPageMaximumItems)}
}

func (runs *schema008MigrationRuns) Add(entry storageformat.DomainEntry) error {
	runs.chunk = append(runs.chunk, entry)
	if len(runs.chunk) < domainPageMaximumItems {
		return nil
	}
	return runs.flushChunk()
}

func (runs *schema008MigrationRuns) flushChunk() error {
	if len(runs.chunk) == 0 {
		return nil
	}
	sort.Slice(runs.chunk, func(left, right int) bool { return runs.chunk[left].Key < runs.chunk[right].Key })
	root, err := runs.session.buildTree(runs.ctx, runs.chunk)
	if err != nil {
		return err
	}
	runs.chunk = runs.chunk[:0]
	runs.session.pages = make(map[string]storageformat.DomainPage)
	return runs.addRun(0, root)
}

func (runs *schema008MigrationRuns) addRun(level int, root storageformat.DomainTreeRoot) error {
	for len(runs.levels) <= level {
		runs.levels = append(runs.levels, nil)
	}
	runs.levels[level] = append(runs.levels[level], root)
	if len(runs.levels[level]) < namespaceProjectionMergeFanIn {
		return nil
	}
	merged, err := mergeNamespaceProjectionRuns(runs.ctx, runs.session, runs.levels[level])
	if err != nil {
		return err
	}
	runs.levels[level] = nil
	return runs.addRun(level+1, merged)
}

func (runs *schema008MigrationRuns) Finish() (storageformat.DomainTreeRoot, error) {
	if err := runs.flushChunk(); err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	var roots []storageformat.DomainTreeRoot
	for _, level := range runs.levels {
		roots = append(roots, level...)
	}
	return mergeDuplicateProjectionRuns008(runs.ctx, runs.session, roots)
}

func (e *Engine) installSchema008StagedDomains(ctx context.Context) error {
	prefix := storageformat.Schema008MigrationStagePrefix()
	request := objectstore.ListRequest{Prefix: prefix, Limit: 1000}
	currentGroup := ""
	var currentRef consistencyDomainRef
	type domainRuns struct {
		base, outcomes, expiry *schema008MigrationRuns
	}
	var runs *domainRuns
	finish := func() error {
		if runs == nil {
			return nil
		}
		base, err := runs.base.Finish()
		if err != nil {
			return err
		}
		outcomes, err := runs.outcomes.Finish()
		if err != nil {
			return err
		}
		expiry, err := runs.expiry.Finish()
		if err != nil {
			return err
		}
		if base.Digest == "" && outcomes.Digest == "" && expiry.Digest == "" {
			return nil
		}
		return e.installSchema008Domain(ctx, currentRef, base, outcomes, expiry)
	}
	previous := ""
	for {
		page, err := e.backend.List(ctx, request)
		if err != nil {
			return err
		}
		for _, info := range page.Objects {
			key := info.Key.String()
			if !strings.HasPrefix(key, prefix) || previous != "" && key <= previous {
				return domain.NewError(domain.ErrorInvalid, "invalid schema-008 migration stage listing")
			}
			previous = key
			relative := strings.TrimPrefix(key, prefix)
			separator := strings.IndexByte(relative, '/')
			if separator <= 0 {
				return domain.NewError(domain.ErrorInvalid, "invalid schema-008 migration stage key")
			}
			group := relative[:separator]
			if currentGroup != "" && group != currentGroup {
				if err := finish(); err != nil {
					return err
				}
				runs = nil
			}
			object, err := e.backend.Get(ctx, info.Key)
			if err != nil {
				return err
			}
			var stage schema008MigrationStage
			if err := decodeCanonicalValue(object.Body, &stage); err != nil {
				return err
			}
			reference, _, err := validateSchema008MigrationStage(stage)
			if err != nil || storageformat.Schema008MigrationStageKey(schema008DomainIdentity(reference), stage.SourceKey) != info.Key {
				return domain.NewError(domain.ErrorInvalid, "schema-008 migration stage key binding mismatch")
			}
			if currentGroup == "" || group != currentGroup {
				currentGroup, currentRef = group, reference
				runs = &domainRuns{
					base:     newSchema008MigrationRuns(ctx, newConsistencyDomainTreeSession(e.stateDomainStore(), reference)),
					outcomes: newSchema008MigrationRuns(ctx, newConsistencyDomainTreeSession(e.stateDomainStore(), reference)),
					expiry:   newSchema008MigrationRuns(ctx, newConsistencyDomainTreeSession(e.stateDomainStore(), reference)),
				}
			} else if reference != currentRef {
				return domain.NewError(domain.ErrorInvalid, "schema-008 migration domain digest collision")
			}
			if !stage.MigrationOnly {
				target := runs.base
				switch stage.Tree {
				case "", "base":
				case "outcomes":
					target = runs.outcomes
				case "outcome-expiry":
					target = runs.expiry
				}
				if err := target.Add(storageformat.DomainEntry{Key: stage.Key, Value: stage.Value, LogicalVersion: stage.LogicalVersion}); err != nil {
					return err
				}
			}
		}
		if page.NextCursor == "" {
			break
		}
		request.Cursor = page.NextCursor
	}
	return finish()
}

func (e *Engine) installSchema008Domain(ctx context.Context, reference consistencyDomainRef, root, outcomes, expiry storageformat.DomainTreeRoot) error {
	store := e.stateDomainStore()
	if err := store.ensureRegistered(ctx, reference); err != nil {
		return err
	}
	for {
		snapshot, err := store.loadHead(ctx, reference)
		if err != nil {
			return err
		}
		if snapshot.head.Revision == 1 && snapshot.head.BaseRevision == 1 && snapshot.head.Base == root && snapshot.head.Outcomes == outcomes && snapshot.head.OutcomeExpiry == expiry && len(snapshot.head.Deltas) == 0 {
			return nil
		}
		if snapshot.head.Revision != 0 || snapshot.head.Base.Digest != "" || len(snapshot.head.Deltas) != 0 || snapshot.head.Frozen {
			return domain.NewError(domain.ErrorInvalid, "schema-008 migration domain was already mutated")
		}
		next := snapshot.head
		next.Revision, next.BaseRevision, next.Base, next.Outcomes, next.OutcomeExpiry = 1, 1, root, outcomes, expiry
		key := storageformat.DomainHeadKey(reference.Kind, reference.ID)
		body, err := store.encodeHead(key, snapshot, next)
		if err != nil {
			return err
		}
		if _, err := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: snapshot.object.Version}); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
	}
}
