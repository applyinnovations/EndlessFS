package portable

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type admissionLease struct {
	key       objectstore.Key
	version   objectstore.NativeVersion
	operation string
}

func (e *Engine) withAdmission(ctx context.Context, intent storageformat.MutationIntent, action func() error) error {
	lease, err := e.admit(ctx, intent)
	if err != nil {
		return err
	}
	if err := e.step(ctx, StepStateAfterAdmitted); err != nil {
		return err
	}
	actionErr := action()
	if actionErr == nil {
		if err := e.step(ctx, StepStateAfterBackend); err != nil {
			return err
		}
	}
	if actionErr != nil && (errors.Is(actionErr, domain.ErrUnavailable) || errors.Is(actionErr, domain.ErrRateLimited)) {
		return actionErr
	}
	deleteErr := e.backend.Delete(ctx, lease.key, objectstore.DeleteCondition{Version: lease.version})
	if actionErr != nil {
		return actionErr
	}
	return deleteErr
}

func (e *Engine) admit(ctx context.Context, intent storageformat.MutationIntent) (admissionLease, error) {
	_, gateEnvelope, gate, err := e.readGate(ctx)
	if err != nil {
		return admissionLease{}, err
	}
	if gate.Mode != storageformat.GateOpen {
		return admissionLease{}, domain.NewError(domain.ErrorUnavailable, "bucket write gate is not open")
	}
	if !reflect.DeepEqual(gate.WriterFeatures, e.writer.RequiredFeatures) {
		return admissionLease{}, domain.NewError(domain.ErrorPreconditionFailed, "write gate is bound to an incompatible writer feature set")
	}
	operationRandom, err := e.ids.OpaqueID()
	if err != nil {
		return admissionLease{}, err
	}
	sequence := e.admissionSequence.Add(1)
	operationID := operationRandom + "-" + strconv.FormatUint(sequence, 36)
	replicaAttemptID, err := e.ids.OpaqueID()
	if err != nil {
		return admissionLease{}, err
	}
	key := storageformat.AdmissionKey(gate.Epoch, operationID)
	now := e.clock.Now().UTC()
	candidate := storageformat.Admission{
		SchemaVersion: 1, Epoch: gate.Epoch, OperationID: operationID,
		WriterSetID: e.writer.WriterSetID, ReplicaAttemptID: replicaAttemptID,
		ObservedGate: gateEnvelope.LogicalVersion, State: storageformat.AdmissionCandidate,
		Attempt: 1, Fence: 1, CreatedAt: now, ExpiresAt: now.Add(e.leaseTTL),
	}
	intentBytes, err := storageformat.EncodeCanonical(intent)
	if err != nil {
		return admissionLease{}, err
	}
	candidate.IntentDigest = storageformat.Digest(intentBytes)
	candidate.Mutation = &intent
	body, err := storageformat.EncodeEnvelope(admissionSchema, key, 1, candidate)
	if err != nil {
		return admissionLease{}, err
	}
	candidateVersion, err := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
	if err != nil {
		return admissionLease{}, err
	}
	if err := e.step(ctx, StepAdmissionAfterCandidate); err != nil {
		return admissionLease{}, err
	}
	_, secondEnvelope, secondGate, err := e.readGate(ctx)
	if err != nil {
		_ = e.backend.Delete(ctx, key, objectstore.DeleteCondition{Version: candidateVersion})
		return admissionLease{}, err
	}
	if secondGate.Mode != storageformat.GateOpen || secondGate.Epoch != gate.Epoch || secondEnvelope.LogicalVersion != gateEnvelope.LogicalVersion || !reflect.DeepEqual(secondGate.WriterFeatures, e.writer.RequiredFeatures) {
		_ = e.cancelAdmission(ctx, key, candidateVersion, candidate)
		return admissionLease{}, domain.NewError(domain.ErrorUnavailable, "bucket write gate changed during admission")
	}
	candidate.State = storageformat.AdmissionAdmitted
	admittedBody, err := storageformat.EncodeEnvelope(admissionSchema, key, 2, candidate)
	if err != nil {
		return admissionLease{}, err
	}
	admittedVersion, err := e.backend.Put(ctx, key, admittedBody, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: candidateVersion})
	if err != nil {
		if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrNotFound) {
			return admissionLease{}, domain.NewError(domain.ErrorUnavailable, "admission was cancelled")
		}
		return admissionLease{}, err
	}
	return admissionLease{key: key, version: admittedVersion, operation: operationID}, nil
}

func (e *Engine) cancelAdmission(ctx context.Context, key objectstore.Key, version objectstore.NativeVersion, admission storageformat.Admission) error {
	admission.State = storageformat.AdmissionCancelled
	body, err := storageformat.EncodeEnvelope(admissionSchema, key, 2, admission)
	if err != nil {
		return err
	}
	next, err := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version})
	if err != nil {
		return err
	}
	return e.backend.Delete(ctx, key, objectstore.DeleteCondition{Version: next})
}

func (e *Engine) readGate(ctx context.Context) (objectstore.Object, storageformat.Envelope, storageformat.WriteGate, error) {
	object, err := e.backend.Get(ctx, storageformat.WriteGateKey())
	if err != nil {
		return objectstore.Object{}, storageformat.Envelope{}, storageformat.WriteGate{}, err
	}
	var envelope storageformat.Envelope
	var gate storageformat.WriteGate
	if err := storageformat.DecodeEnvelope(object.Body, storageformat.WriteGateKey(), writeGateSchema, &envelope, &gate); err != nil {
		return objectstore.Object{}, storageformat.Envelope{}, storageformat.WriteGate{}, err
	}
	if err := storageformat.ValidateGate(gate); err != nil {
		return objectstore.Object{}, storageformat.Envelope{}, storageformat.WriteGate{}, err
	}
	return object, envelope, gate, nil
}

func expired(now time.Time, deadline time.Time) bool { return !now.Before(deadline) }
