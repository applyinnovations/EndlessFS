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
