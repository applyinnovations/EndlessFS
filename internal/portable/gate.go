package portable

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// Terminal operation/idempotency results remain replayable and pollable for a
// fixed recovery window. Application Trash has separate, indefinite retention.
const terminalOperationRetention = 30 * 24 * time.Hour

func (e *Engine) GateStatus(ctx context.Context) (storageformat.WriteGate, error) {
	_, _, gate, err := e.readGate(ctx)
	return gate, err
}

func (e *Engine) CloseWrites(ctx context.Context, checkpointID string) error {
	if checkpointID == "" {
		return domain.NewError(domain.ErrorInvalid, "checkpoint ID is required")
	}
	gateObject, gateEnvelope, gate, err := e.readGate(ctx)
	if err != nil {
		return err
	}
	switch gate.Mode {
	case storageformat.GateOpen:
		gate.Mode = storageformat.GateClosing
		gate.CheckpointID = checkpointID
		body, encodeErr := storageformat.EncodeEnvelope(writeGateSchema, storageformat.WriteGateKey(), gateEnvelope.Revision+1, gate)
		if encodeErr != nil {
			return encodeErr
		}
		if _, err = e.backend.Put(ctx, storageformat.WriteGateKey(), body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: gateObject.Version}); err != nil {
			return err
		}
	case storageformat.GateClosing:
		if gate.CheckpointID != checkpointID {
			return domain.NewError(domain.ErrorConflict, "write gate is closing for another checkpoint")
		}
	case storageformat.GateClosed:
		if gate.CheckpointID == checkpointID {
			return nil
		}
		return domain.NewError(domain.ErrorConflict, "write gate is closed for another checkpoint")
	}
	return e.finishClosingWrites(ctx, checkpointID)
}

func (e *Engine) finishClosingWrites(ctx context.Context, checkpointID string) error {
	_, initialEnvelope, initialGate, err := e.readGate(ctx)
	if err != nil {
		return err
	}
	if initialGate.Mode == storageformat.GateClosed && initialGate.CheckpointID == checkpointID {
		return nil
	}
	if initialGate.Mode != storageformat.GateClosing || initialGate.CheckpointID != checkpointID {
		return domain.NewError(domain.ErrorPreconditionFailed, "write gate changed before closure drain")
	}
	if err := e.drainAdmissions(ctx, initialGate.Epoch); err != nil {
		return err
	}
	if err := e.drainOperationRecords(ctx, true, true); err != nil {
		return err
	}
	if err := e.pruneDuplicateTombstones(ctx); err != nil {
		return err
	}
	if err := e.pruneExpiredOperationRecords(ctx); err != nil {
		return err
	}
	if err := e.pruneOperationArtifacts(ctx); err != nil {
		return err
	}
	garbageCollectionSupported, err := e.garbageCollectionSupported(ctx)
	if err != nil {
		return err
	}
	// Epoch-004 reachability collection owns state-version pruning. Running the
	// legacy version-by-version lookup pass first would duplicate a complete
	// namespace traversal and an index lookup for every live value.
	if !garbageCollectionSupported {
		if err := e.pruneStateVersions(ctx); err != nil {
			return err
		}
	}
	if garbageCollectionSupported {
		if err := e.runGarbageCollection(ctx, checkpointID, initialGate.Epoch, initialEnvelope.LogicalVersion); err != nil {
			return err
		}
	}
	gateObject, gateEnvelope, gate, err := e.readGate(ctx)
	if err != nil {
		return err
	}
	if gate.Mode == storageformat.GateClosed && gate.CheckpointID == checkpointID {
		return nil
	}
	if gate.Mode != storageformat.GateClosing || gate.CheckpointID != checkpointID {
		return domain.NewError(domain.ErrorPreconditionFailed, "write gate changed while closing")
	}
	gate.Mode = storageformat.GateClosed
	body, err := storageformat.EncodeEnvelope(writeGateSchema, storageformat.WriteGateKey(), gateEnvelope.Revision+1, gate)
	if err != nil {
		return err
	}
	_, err = e.backend.Put(ctx, storageformat.WriteGateKey(), body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: gateObject.Version})
	if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrConflict) {
		_, _, winner, readErr := e.readGate(ctx)
		if readErr == nil && winner.Mode == storageformat.GateClosed && winner.CheckpointID == checkpointID {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
	return err
}

func (e *Engine) garbageCollectionSupported(ctx context.Context) (bool, error) {
	object, err := e.backend.Get(ctx, storageformat.SuperblockKey())
	if err != nil {
		return false, err
	}
	var superblock storageformat.Superblock
	if err := decodeCanonicalSuperblock(object.Body, &superblock); err != nil {
		return false, err
	}
	for _, feature := range superblock.RequiredFeatures {
		if feature == storageformat.FeatureDirectoryIndexes {
			return true, nil
		}
	}
	return false, nil
}

func (e *Engine) pruneOperationArtifacts(ctx context.Context) error {
	for _, prefix := range []string{storageformat.OperationStagingPrefix(), storageformat.FileOperationStepsPrefix(), storageformat.LeasePrefix()} {
		if err := visitObjectPages(ctx, e.backend, prefix, func(info objectstore.ObjectInfo) error {
			if err := e.backend.Delete(ctx, info.Key, objectstore.DeleteCondition{Version: info.Version}); err != nil && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrPreconditionFailed) {
				return err
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return visitObjectPages(ctx, e.backend, storageformat.CheckpointPrefix(), func(info objectstore.ObjectInfo) error {
		if !strings.Contains(info.Key.String(), "/work/") {
			return nil
		}
		if err := e.backend.Delete(ctx, info.Key, objectstore.DeleteCondition{Version: info.Version}); err != nil && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
		return nil
	})
}

func (e *Engine) pruneExpiredOperationRecords(ctx context.Context) error {
	cutoff := e.clock.Now().UTC().Add(-terminalOperationRetention)
	if err := visitObjectPages(ctx, e.backend, storageformat.IdempotencyPrefix(), func(info objectstore.ObjectInfo) error {
		object, err := e.backend.Get(ctx, info.Key)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var envelope storageformat.Envelope
		var binding storageformat.IdempotencyRecord
		if err := storageformat.DecodeEnvelope(object.Body, info.Key, idempotencySchema, &envelope, &binding); err != nil {
			return err
		}
		if binding.SchemaVersion != 1 || binding.UserID == "" || binding.OperationID == "" || binding.Kind == "" || binding.KeyDigest == "" || binding.Fingerprint == "" {
			return domain.NewError(domain.ErrorInvalid, "invalid idempotency record during retention pruning")
		}
		operationKey := storageformat.OperationKey(binding.UserID, binding.OperationID)
		operation, operationErr := e.backend.Get(ctx, operationKey)
		if errors.Is(operationErr, domain.ErrNotFound) {
			return deleteMaintenanceObject(ctx, e.backend, object)
		}
		if operationErr != nil {
			return operationErr
		}
		expired, err := terminalOperationExpired(operation, cutoff)
		if err != nil {
			return err
		}
		if !expired {
			return nil
		}
		if err := deleteMaintenanceObject(ctx, e.backend, object); err != nil {
			return err
		}
		return deleteMaintenanceObject(ctx, e.backend, operation)
	}); err != nil {
		return err
	}
	return visitObjectPages(ctx, e.backend, storageformat.OperationPrefix(), func(info objectstore.ObjectInfo) error {
		object, err := e.backend.Get(ctx, info.Key)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		expired, err := terminalOperationExpired(object, cutoff)
		if err != nil {
			return err
		}
		if !expired {
			return nil
		}
		return deleteMaintenanceObject(ctx, e.backend, object)
	})
}

func terminalOperationExpired(object objectstore.Object, cutoff time.Time) (bool, error) {
	var generic storageformat.Envelope
	if err := state.DecodeJSONWithLimit(object.Body, &generic, storageformat.MaxCanonicalBytes); err != nil {
		return false, err
	}
	switch generic.Schema {
	case fileOperationSchema:
		var envelope storageformat.Envelope
		var operation storageformat.FileOperation
		if err := storageformat.DecodeEnvelope(object.Body, object.Key, fileOperationSchema, &envelope, &operation); err != nil {
			return false, err
		}
		if operation.SchemaVersion != 1 && operation.SchemaVersion != 2 || storageformat.OperationKey(operation.UserID, operation.OperationID) != object.Key || operation.UpdatedAt.IsZero() {
			return false, domain.NewError(domain.ErrorInvalid, "invalid file operation during retention pruning")
		}
		terminal := operation.State == storageformat.FileOperationSucceeded || operation.State == storageformat.FileOperationFailed
		return terminal && operation.UpdatedAt.Before(cutoff), nil
	case uploadRecordSchema:
		var envelope storageformat.Envelope
		var upload storageformat.UploadRecord
		if err := storageformat.DecodeEnvelope(object.Body, object.Key, uploadRecordSchema, &envelope, &upload); err != nil {
			return false, err
		}
		if upload.SchemaVersion != 1 || storageformat.OperationKey(upload.UserID, upload.UploadID) != object.Key || upload.CreatedAt.IsZero() {
			return false, domain.NewError(domain.ErrorInvalid, "invalid upload operation during retention pruning")
		}
		terminal := upload.State == storageformat.UploadCompleted || upload.State == storageformat.UploadAborted
		return terminal && upload.CreatedAt.Before(cutoff), nil
	default:
		return false, domain.NewError(domain.ErrorInvalid, "unknown operation record during retention pruning")
	}
}

func deleteMaintenanceObject(ctx context.Context, backend interface {
	Delete(context.Context, objectstore.Key, objectstore.DeleteCondition) error
}, object objectstore.Object) error {
	err := backend.Delete(ctx, object.Key, objectstore.DeleteCondition{Version: object.Version})
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrPreconditionFailed) {
		return nil
	}
	return err
}

func (e *Engine) pruneStateVersions(ctx context.Context) error {
	if err := visitObjectPages(ctx, e.backend, storageformat.StateRecordsPrefix(), func(info objectstore.ObjectInfo) error {
		object, err := e.backend.Get(ctx, info.Key)
		if err != nil {
			return err
		}
		var envelope storageformat.Envelope
		var record storageformat.StateRecord
		if err := storageformat.DecodeEnvelope(object.Body, info.Key, stateRecordSchema, &envelope, &record); err != nil {
			return err
		}
		logical, err := parseExistingStateKey(record.LogicalKey)
		if err != nil || record.SchemaVersion != 1 || logical.String() != record.LogicalKey || canonicalStateKey(logical) != info.Key {
			return domain.NewError(domain.ErrorInvalid, "invalid state record during checkpoint")
		}
		return nil
	}); err != nil {
		return err
	}
	return visitObjectPages(ctx, e.backend, storageformat.StateVersionsPrefix(), func(info objectstore.ObjectInfo) error {
		versionObject, getErr := e.backend.Get(ctx, info.Key)
		if getErr != nil {
			return getErr
		}
		var versionEnvelope storageformat.Envelope
		var versionRecord storageformat.StateVersionRecord
		if err := storageformat.DecodeEnvelope(versionObject.Body, info.Key, stateVersionSchema, &versionEnvelope, &versionRecord); err != nil {
			// Historical pruning treated an undecodable version object as
			// unreachable garbage. Preserve that behavior while keeping the
			// traversal bounded; authoritative current records were validated in
			// the preceding pass.
			if deleteErr := e.backend.Delete(ctx, info.Key, objectstore.DeleteCondition{Version: info.Version}); deleteErr != nil && !errors.Is(deleteErr, domain.ErrNotFound) && !errors.Is(deleteErr, domain.ErrPreconditionFailed) {
				return deleteErr
			}
			return nil
		}
		logical, err := parseExistingStateKey(versionRecord.LogicalKey)
		if err != nil || logical.String() != versionRecord.LogicalKey || versionRecord.SchemaVersion != 1 || versionRecord.LogicalVersion == "" {
			return domain.NewError(domain.ErrorInvalid, "invalid state-version record during checkpoint")
		}
		namespace := strings.SplitN(versionRecord.LogicalKey, "/", 2)[0]
		if storageformat.StateVersionKey(namespace, versionRecord.LogicalKey, versionRecord.LogicalVersion) != info.Key {
			return domain.NewError(domain.ErrorInvalid, "state-version key digest collision or corruption")
		}
		current, currentErr := e.backend.Get(ctx, canonicalStateKey(logical))
		keep := false
		if currentErr == nil {
			_, currentEnvelope, decodeErr := decodeStateObject(current, logical)
			if decodeErr != nil {
				return decodeErr
			}
			keep = currentEnvelope.LogicalVersion == versionRecord.LogicalVersion
		} else if errors.Is(currentErr, domain.ErrNotFound) {
			indexed, indexErr := e.stateIndexEntry(ctx, logical)
			if indexErr == nil {
				keep = indexed.LogicalVersion == versionRecord.LogicalVersion
			} else if !errors.Is(indexErr, domain.ErrNotFound) {
				return indexErr
			}
		} else {
			return currentErr
		}
		if keep {
			return nil
		}
		if err := e.backend.Delete(ctx, info.Key, objectstore.DeleteCondition{Version: info.Version}); err != nil && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
		return nil
	})
}

func (e *Engine) drainActiveUploads(ctx context.Context) error {
	return e.drainOperationRecords(ctx, false, true)
}

func (e *Engine) drainOperationRecords(ctx context.Context, recoverFiles, drainUploads bool) error {
	return visitObjectPages(ctx, e.backend, storageformat.OperationPrefix(), func(info objectstore.ObjectInfo) error {
		object, getErr := e.backend.Get(ctx, info.Key)
		if errors.Is(getErr, domain.ErrNotFound) {
			return nil
		}
		if getErr != nil {
			return getErr
		}
		var generic storageformat.Envelope
		if err := state.DecodeJSONWithLimit(object.Body, &generic, storageformat.MaxCanonicalBytes); err != nil {
			return err
		}
		if generic.Schema == fileOperationSchema {
			if !recoverFiles {
				return nil
			}
			return e.Files().recoverFileOperation(ctx, info.Key)
		}
		if generic.Schema != uploadRecordSchema || !drainUploads {
			return nil
		}
		var envelope storageformat.Envelope
		var upload storageformat.UploadRecord
		if err := storageformat.DecodeEnvelope(object.Body, info.Key, uploadRecordSchema, &envelope, &upload); err != nil {
			return err
		}
		if upload.State != storageformat.UploadActive {
			return nil
		}
		if !expired(e.clock.Now(), upload.ExpiresAt) {
			return domain.NewError(domain.ErrorUnavailable, "active upload prevents write-gate closure")
		}
		transfers, ok := e.fileBackend.(objectstore.DirectTransferBackend)
		if !ok {
			return domain.NewError(domain.ErrorPreconditionFailed, "active upload cannot be drained")
		}
		lease, leaseObject, leaseErr := e.Files().readTransferLease(ctx, upload)
		if leaseErr == nil {
			if err := transfers.AbortUpload(ctx, lease.Ciphertext); err != nil && !errors.Is(err, domain.ErrNotFound) {
				return err
			}
			_ = e.backend.Delete(ctx, leaseObject.Key, objectstore.DeleteCondition{Version: leaseObject.Version})
		} else if !errors.Is(leaseErr, domain.ErrNotFound) {
			return leaseErr
		}
		upload.State = storageformat.UploadAborted
		body, err := storageformat.EncodeEnvelope(uploadRecordSchema, info.Key, envelope.Revision+1, upload)
		if err != nil {
			return err
		}
		if _, err = e.backend.Put(ctx, info.Key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
			if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrNotFound) {
				return domain.NewError(domain.ErrorUnavailable, "upload changed while closing write gate")
			}
			return err
		}
		return nil
	})
}

func (e *Engine) OpenWrites(ctx context.Context, checkpointID string) error {
	if err := e.VerifyCheckpoint(ctx, checkpointID); err != nil {
		return err
	}
	return e.openClosedWriteGate(ctx, checkpointID)
}

func (e *Engine) openClosedWriteGate(ctx context.Context, checkpointID string) error {
	gateObject, gateEnvelope, gate, err := e.readGate(ctx)
	if err != nil {
		return err
	}
	if gate.Mode != storageformat.GateClosed || checkpointID == "" || gate.CheckpointID != checkpointID {
		return domain.NewError(domain.ErrorPreconditionFailed, "write gate is not closed for this checkpoint")
	}
	gate.Mode = storageformat.GateOpen
	gate.Epoch++
	gate.CheckpointID = ""
	body, err := storageformat.EncodeEnvelope(writeGateSchema, storageformat.WriteGateKey(), gateEnvelope.Revision+1, gate)
	if err != nil {
		return err
	}
	_, err = e.backend.Put(ctx, storageformat.WriteGateKey(), body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: gateObject.Version})
	return err
}

func (e *Engine) drainAdmissions(ctx context.Context, epoch uint64) error {
	for {
		active := false
		err := visitObjectPages(ctx, e.backend, storageformat.AdmissionPrefix(epoch), func(info objectstore.ObjectInfo) error {
			object, getErr := e.backend.Get(ctx, info.Key)
			if errors.Is(getErr, domain.ErrNotFound) {
				return nil
			}
			if getErr != nil {
				return getErr
			}
			var envelope storageformat.Envelope
			var admission storageformat.Admission
			if err := storageformat.DecodeEnvelope(object.Body, object.Key, admissionSchema, &envelope, &admission); err != nil {
				return err
			}
			if admission.Epoch != epoch || admission.WriterSetID != e.writer.WriterSetID {
				return domain.NewError(domain.ErrorPreconditionFailed, "incompatible admission record")
			}
			switch admission.State {
			case storageformat.AdmissionCandidate, storageformat.AdmissionCancelled:
				if err := e.cancelAndRemoveAdmission(ctx, object, envelope, admission); err != nil && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrPreconditionFailed) {
					return err
				}
				active = true
			case storageformat.AdmissionAdmitted:
				if !expired(e.clock.Now(), admission.ExpiresAt) {
					return domain.NewError(domain.ErrorUnavailable, "admitted mutation is still active")
				}
				if err := e.takeoverAndRecover(ctx, object, envelope, admission); err != nil {
					return err
				}
				active = true
			default:
				return domain.NewError(domain.ErrorInvalid, "invalid admission state")
			}
			return nil
		})
		if err != nil {
			return err
		}
		if !active {
			return nil
		}
	}
}

func (e *Engine) cancelAndRemoveAdmission(ctx context.Context, object objectstore.Object, envelope storageformat.Envelope, admission storageformat.Admission) error {
	if admission.State != storageformat.AdmissionCancelled {
		admission.State = storageformat.AdmissionCancelled
		body, err := storageformat.EncodeEnvelope(admissionSchema, object.Key, envelope.Revision+1, admission)
		if err != nil {
			return err
		}
		version, err := e.backend.Put(ctx, object.Key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version})
		if err != nil {
			return err
		}
		object.Version = version
	}
	return e.backend.Delete(ctx, object.Key, objectstore.DeleteCondition{Version: object.Version})
}

func (e *Engine) takeoverAndRecover(ctx context.Context, object objectstore.Object, envelope storageformat.Envelope, admission storageformat.Admission) error {
	owner, err := e.ids.OpaqueID()
	if err != nil {
		return err
	}
	admission.Attempt++
	admission.Fence++
	admission.ReplicaAttemptID = owner
	admission.ExpiresAt = e.clock.Now().UTC().Add(e.leaseTTL)
	body, err := storageformat.EncodeEnvelope(admissionSchema, object.Key, envelope.Revision+1, admission)
	if err != nil {
		return err
	}
	version, err := e.backend.Put(ctx, object.Key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version})
	if err != nil {
		if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrNotFound) {
			return domain.NewError(domain.ErrorUnavailable, "another replica won admission takeover")
		}
		return err
	}
	if err := e.recoverMutation(ctx, admission); err != nil {
		return err
	}
	if admission.Mutation.RecoverOperationKey != "" {
		key, parseErr := objectstore.ParseKey(admission.Mutation.RecoverOperationKey)
		if parseErr != nil {
			return parseErr
		}
		if err := e.Files().recoverFileOperation(ctx, key); err != nil {
			return err
		}
	}
	return e.backend.Delete(ctx, object.Key, objectstore.DeleteCondition{Version: version})
}

func (e *Engine) drainFileOperations(ctx context.Context) error {
	return e.drainOperationRecords(ctx, true, false)
}

func visitObjectPages(ctx context.Context, backend objectstore.ListBackend, prefix string, visit func(objectstore.ObjectInfo) error) error {
	request := objectstore.ListRequest{Prefix: prefix, Limit: 256}
	for {
		page, err := backend.List(ctx, request)
		if err != nil {
			return err
		}
		for _, info := range page.Objects {
			if err := visit(info); err != nil {
				return err
			}
			request.After = info.Key.String()
		}
		if len(page.Objects) == 0 || page.NextCursor == "" {
			return nil
		}
	}
}

func (e *Engine) recoverMutation(ctx context.Context, admission storageformat.Admission) error {
	if admission.Mutation == nil {
		return domain.NewError(domain.ErrorPreconditionFailed, "admission has no recoverable mutation")
	}
	encoded, err := storageformat.EncodeCanonical(*admission.Mutation)
	if err != nil || storageformat.Digest(encoded) != admission.IntentDigest {
		return domain.NewError(domain.ErrorInvalid, "admission intent digest mismatch")
	}
	intent := *admission.Mutation
	if err := e.ensureMutationPrerequisitesForRecovery(ctx, intent.Prerequisites, intent.RecoverOperationKey); err != nil {
		return err
	}
	if err := e.ensureMutationPrerequisiteReferences(ctx, intent.PrerequisiteRefs); err != nil {
		return err
	}
	if intent.RecoverUploadKey != "" {
		key, parseErr := objectstore.ParseKey(intent.RecoverUploadKey)
		if parseErr != nil {
			return parseErr
		}
		if err := e.Files().recoverUploadLease(ctx, key); err != nil {
			return err
		}
	}
	if err := e.ensureMutationCopies(ctx, intent.Copies); err != nil {
		return err
	}
	if err := e.ensureUploadAborts(ctx, intent.AbortUploads); err != nil {
		return err
	}
	key, err := objectstore.ParseKey(intent.TargetKey)
	if err != nil {
		return err
	}
	object, getErr := e.backend.Get(ctx, key)
	switch intent.Action {
	case storageformat.MutationCreate:
		if getErr == nil {
			// Either the original admitted create committed this exact body, or
			// another admitted create won the create-only race. Both are terminal.
			return nil
		}
		if !errors.Is(getErr, domain.ErrNotFound) {
			return getErr
		}
		_, err = e.backend.Put(ctx, key, intent.TargetBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
		if errors.Is(err, domain.ErrConflict) {
			return nil
		}
		return err
	case storageformat.MutationCAS:
		if getErr != nil {
			if errors.Is(getErr, domain.ErrNotFound) {
				return nil // The original CAS reached a terminal not-found outcome.
			}
			return getErr
		}
		if bytes.Equal(object.Body, intent.TargetBody) {
			return nil
		}
		logical, err := canonicalLogicalVersion(object.Body)
		if err != nil {
			return err
		}
		if logical != intent.ExpectedLogicalVersion {
			return nil // A different successful writer made the original CAS terminal.
		}
		_, err = e.backend.Put(ctx, key, intent.TargetBody, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version})
		if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		return err
	case storageformat.MutationDelete:
		if errors.Is(getErr, domain.ErrNotFound) {
			return nil
		}
		if getErr != nil {
			return getErr
		}
		logical, err := canonicalLogicalVersion(object.Body)
		if err != nil {
			return err
		}
		if logical != intent.ExpectedLogicalVersion {
			return nil
		}
		err = e.backend.Delete(ctx, key, objectstore.DeleteCondition{Version: object.Version})
		if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		return err
	default:
		return domain.NewError(domain.ErrorInvalid, "unknown admission mutation")
	}
}

func (e *Engine) ensureUploadAborts(ctx context.Context, uploadIDs []string) error {
	if len(uploadIDs) == 0 {
		return nil
	}
	transfers, ok := e.fileBackend.(objectstore.DirectTransferBackend)
	if !ok {
		return domain.NewError(domain.ErrorPreconditionFailed, "object backend has no direct transfer support")
	}
	previous := ""
	for _, uploadID := range uploadIDs {
		if uploadID == "" || uploadID <= previous {
			return domain.NewError(domain.ErrorInvalid, "invalid upload abort order")
		}
		leaseKey := storageformat.LeaseKey(transfers.BackendKind(), uploadID)
		leaseObject, err := e.backend.Get(ctx, leaseKey)
		if errors.Is(err, domain.ErrNotFound) {
			previous = uploadID
			continue
		}
		if err != nil {
			return err
		}
		var envelope storageformat.Envelope
		var lease storageformat.TransferLease
		if err := storageformat.DecodeEnvelope(leaseObject.Body, leaseKey, transferLeaseSchema, &envelope, &lease); err != nil {
			return err
		}
		if lease.SchemaVersion != 1 || lease.BackendKind != transfers.BackendKind() || lease.UploadID != uploadID || len(lease.Ciphertext) == 0 {
			return domain.NewError(domain.ErrorInvalid, "invalid upload abort lease")
		}
		if err := transfers.AbortUpload(ctx, lease.Ciphertext); err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if err := e.backend.Delete(ctx, leaseKey, objectstore.DeleteCondition{Version: leaseObject.Version}); err != nil && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
		previous = uploadID
	}
	return nil
}

func (e *Engine) ensureMutationCopies(ctx context.Context, copies []storageformat.MutationCopy) error {
	previous := ""
	for _, copyIntent := range copies {
		if copyIntent.DestinationKey <= previous || copyIntent.Size < 0 || copyIntent.SourceKey == copyIntent.DestinationKey {
			return domain.NewError(domain.ErrorInvalid, "invalid mutation copy order")
		}
		expectedFingerprint, hasFingerprint, err := mutationCopyFingerprint(copyIntent)
		if err != nil {
			return err
		}
		source, err := objectstore.ParseKey(copyIntent.SourceKey)
		if err != nil {
			return err
		}
		destination, err := objectstore.ParseKey(copyIntent.DestinationKey)
		if err != nil {
			return err
		}
		if existing, headErr := e.fileBackend.Head(ctx, destination); headErr == nil {
			if existing.Size != copyIntent.Size || hasFingerprint && existing.Fingerprint != expectedFingerprint {
				return domain.NewError(domain.ErrorInvalid, "mutation copy destination collision")
			}
			previous = copyIntent.DestinationKey
			continue
		} else if !errors.Is(headErr, domain.ErrNotFound) {
			return headErr
		}
		sourceInfo, err := e.fileBackend.Head(ctx, source)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				winner, headErr := e.fileBackend.Head(ctx, destination)
				if headErr == nil && winner.Size == copyIntent.Size && (!hasFingerprint || winner.Fingerprint == expectedFingerprint) {
					previous = copyIntent.DestinationKey
					continue
				}
			}
			return err
		}
		if sourceInfo.Size != copyIntent.Size || hasFingerprint && sourceInfo.Fingerprint != expectedFingerprint {
			return domain.NewError(domain.ErrorPreconditionFailed, "mutation copy source size changed")
		}
		_, err = e.fileBackend.Copy(ctx, source, destination, objectstore.CopyCondition{SourceVersion: sourceInfo.Version, Destination: objectstore.PutCondition{Mode: objectstore.PutCreateOnly}})
		if err != nil {
			if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrPreconditionFailed) {
				return err
			}
			winner, headErr := e.fileBackend.Head(ctx, destination)
			if headErr == nil && winner.Size == copyIntent.Size && (!hasFingerprint || winner.Fingerprint == expectedFingerprint) {
				previous = copyIntent.DestinationKey
				continue
			}
			if headErr != nil && !errors.Is(headErr, domain.ErrNotFound) {
				return headErr
			}
			if errors.Is(headErr, domain.ErrNotFound) && (errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrPreconditionFailed)) {
				return err
			}
			return domain.NewError(domain.ErrorInvalid, "mutation copy destination collision")
		}
		previous = copyIntent.DestinationKey
	}
	return nil
}

func mutationCopyFingerprint(copyIntent storageformat.MutationCopy) (objectstore.ContentFingerprint, bool, error) {
	if copyIntent.MD5 == "" && copyIntent.CRC32C == "" {
		if copyIntent.SHA256 != "" {
			return objectstore.ContentFingerprint{}, false, nil
		}
		return objectstore.ContentFingerprint{}, false, nil
	}
	fingerprint := objectstore.ContentFingerprint{MD5: copyIntent.MD5, CRC32C: copyIntent.CRC32C}
	if copyIntent.SHA256 != "" || fingerprint.Validate() != nil {
		return objectstore.ContentFingerprint{}, false, domain.NewError(domain.ErrorInvalid, "invalid mutation copy content fingerprint")
	}
	return fingerprint, true, nil
}

func (e *Engine) ensureMutationPrerequisites(ctx context.Context, prerequisites []storageformat.MutationObject) error {
	return e.ensureMutationPrerequisitesForRecovery(ctx, prerequisites, "")
}

func (e *Engine) ensureMutationPrerequisiteReferences(ctx context.Context, references []storageformat.MutationObjectReference) error {
	previous := ""
	for _, reference := range references {
		if reference.Key <= previous || reference.BodyDigest == "" {
			return domain.NewError(domain.ErrorInvalid, "invalid mutation prerequisite reference order")
		}
		key, err := objectstore.ParseKey(reference.Key)
		if err != nil {
			return err
		}
		object, err := e.backend.Get(ctx, key)
		if err != nil {
			return err
		}
		if storageformat.Digest(object.Body) != reference.BodyDigest {
			return domain.NewError(domain.ErrorInvalid, "mutation prerequisite reference digest mismatch")
		}
		previous = reference.Key
	}
	return nil
}

func (e *Engine) ensureMutationPrerequisitesForRecovery(ctx context.Context, prerequisites []storageformat.MutationObject, recoverOperationKey string) error {
	previous := ""
	for _, prerequisite := range prerequisites {
		if prerequisite.Key <= previous || len(prerequisite.Body) == 0 {
			return domain.NewError(domain.ErrorInvalid, "invalid mutation prerequisite order")
		}
		key, err := objectstore.ParseKey(prerequisite.Key)
		if err != nil {
			return err
		}
		existing, err := e.backend.Get(ctx, key)
		if err == nil {
			if !bytes.Equal(existing.Body, prerequisite.Body) && (prerequisite.Key != recoverOperationKey || !sameFileOperationIntent(existing.Body, prerequisite.Body, key)) {
				return domain.NewError(domain.ErrorInvalid, "mutation prerequisite collision")
			}
			previous = prerequisite.Key
			continue
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if _, err = e.backend.Put(ctx, key, prerequisite.Body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			if !errors.Is(err, domain.ErrConflict) {
				return err
			}
			winner, getErr := e.backend.Get(ctx, key)
			if getErr != nil || (!bytes.Equal(winner.Body, prerequisite.Body) && (prerequisite.Key != recoverOperationKey || !sameFileOperationIntent(winner.Body, prerequisite.Body, key))) {
				return domain.NewError(domain.ErrorInvalid, "mutation prerequisite collision")
			}
		}
		previous = prerequisite.Key
	}
	return nil
}

func sameFileOperationIntent(currentBody, initialBody []byte, key objectstore.Key) bool {
	var currentEnvelope, initialEnvelope storageformat.Envelope
	var current, initial storageformat.FileOperation
	if storageformat.DecodeEnvelope(currentBody, key, fileOperationSchema, &currentEnvelope, &current) != nil || storageformat.DecodeEnvelope(initialBody, key, fileOperationSchema, &initialEnvelope, &initial) != nil {
		return false
	}
	current.State = initial.State
	current.Attempt = initial.Attempt
	current.Fence = initial.Fence
	current.ReplicaAttemptID = initial.ReplicaAttemptID
	current.ExpiresAt = initial.ExpiresAt
	current.UpdatedAt = initial.UpdatedAt
	current.ErrorKind = initial.ErrorKind
	current.Error = initial.Error
	return reflect.DeepEqual(current, initial)
}

func canonicalLogicalVersion(body []byte) (string, error) {
	var envelope storageformat.Envelope
	if err := state.DecodeJSONWithLimit(body, &envelope, storageformat.MaxCanonicalBytes); err != nil {
		return "", err
	}
	if envelope.LogicalVersion == "" {
		return "", domain.NewError(domain.ErrorInvalid, "missing logical version")
	}
	return envelope.LogicalVersion, nil
}
