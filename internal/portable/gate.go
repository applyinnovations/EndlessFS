package portable

import (
	"bytes"
	"context"
	"errors"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

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
	if err := e.drainAdmissions(ctx, gate.Epoch); err != nil {
		return err
	}
	if err := e.drainActiveUploads(ctx); err != nil {
		return err
	}
	gateObject, gateEnvelope, gate, err = e.readGate(ctx)
	if err != nil {
		return err
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
	return err
}

func (e *Engine) drainActiveUploads(ctx context.Context) error {
	objects, err := e.listAll(ctx, storageformat.OperationPrefix())
	if err != nil {
		return err
	}
	for _, info := range objects {
		object, getErr := e.backend.Get(ctx, info.Key)
		if errors.Is(getErr, domain.ErrNotFound) {
			continue
		}
		if getErr != nil {
			return getErr
		}
		var generic storageformat.Envelope
		if err := state.DecodeJSONWithLimit(object.Body, &generic, storageformat.MaxCanonicalBytes); err != nil {
			return err
		}
		if generic.Schema != uploadRecordSchema {
			continue
		}
		var envelope storageformat.Envelope
		var upload storageformat.UploadRecord
		if err := storageformat.DecodeEnvelope(object.Body, info.Key, uploadRecordSchema, &envelope, &upload); err != nil {
			return err
		}
		if upload.State != storageformat.UploadActive {
			continue
		}
		if !expired(e.clock.Now(), upload.ExpiresAt) {
			return domain.NewError(domain.ErrorUnavailable, "active upload prevents write-gate closure")
		}
		transfers, ok := e.backend.(objectstore.DirectTransferBackend)
		if !ok {
			return domain.NewError(domain.ErrorPreconditionFailed, "active upload cannot be drained")
		}
		if err := transfers.AbortUpload(ctx, upload.UploadID); err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
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
	}
	return nil
}

func (e *Engine) OpenWrites(ctx context.Context, checkpointID string) error {
	if err := e.VerifyCheckpoint(ctx, checkpointID); err != nil {
		return err
	}
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
		objects, err := e.listAll(ctx, storageformat.AdmissionPrefix(epoch))
		if err != nil {
			return err
		}
		active := false
		for _, info := range objects {
			object, getErr := e.backend.Get(ctx, info.Key)
			if errors.Is(getErr, domain.ErrNotFound) {
				continue
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
	return e.backend.Delete(ctx, object.Key, objectstore.DeleteCondition{Version: version})
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
	if err := e.ensureMutationPrerequisites(ctx, intent.Prerequisites); err != nil {
		return err
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
	transfers, ok := e.backend.(objectstore.DirectTransferBackend)
	if !ok {
		return domain.NewError(domain.ErrorPreconditionFailed, "object backend has no direct transfer support")
	}
	previous := ""
	for _, uploadID := range uploadIDs {
		if uploadID == "" || uploadID <= previous {
			return domain.NewError(domain.ErrorInvalid, "invalid upload abort order")
		}
		if err := transfers.AbortUpload(ctx, uploadID); err != nil && !errors.Is(err, domain.ErrNotFound) {
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
		source, err := objectstore.ParseKey(copyIntent.SourceKey)
		if err != nil {
			return err
		}
		destination, err := objectstore.ParseKey(copyIntent.DestinationKey)
		if err != nil {
			return err
		}
		if existing, headErr := e.backend.Head(ctx, destination); headErr == nil {
			if existing.Size != copyIntent.Size {
				return domain.NewError(domain.ErrorInvalid, "mutation copy destination collision")
			}
			previous = copyIntent.DestinationKey
			continue
		} else if !errors.Is(headErr, domain.ErrNotFound) {
			return headErr
		}
		sourceInfo, err := e.backend.Head(ctx, source)
		if err != nil {
			return err
		}
		if sourceInfo.Size != copyIntent.Size {
			return domain.NewError(domain.ErrorPreconditionFailed, "mutation copy source size changed")
		}
		_, err = e.backend.Copy(ctx, source, destination, objectstore.CopyCondition{SourceVersion: sourceInfo.Version, Destination: objectstore.PutCondition{Mode: objectstore.PutCreateOnly}})
		if err != nil {
			if !errors.Is(err, domain.ErrConflict) {
				return err
			}
			winner, headErr := e.backend.Head(ctx, destination)
			if headErr != nil || winner.Size != copyIntent.Size {
				return domain.NewError(domain.ErrorInvalid, "mutation copy destination collision")
			}
		}
		previous = copyIntent.DestinationKey
	}
	return nil
}

func (e *Engine) ensureMutationPrerequisites(ctx context.Context, prerequisites []storageformat.MutationObject) error {
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
			if !bytes.Equal(existing.Body, prerequisite.Body) {
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
			if getErr != nil || !bytes.Equal(winner.Body, prerequisite.Body) {
				return domain.NewError(domain.ErrorInvalid, "mutation prerequisite collision")
			}
		}
		previous = prerequisite.Key
	}
	return nil
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
