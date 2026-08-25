package portable

import (
	"context"
	"errors"
	"fmt"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func uploadDomainReference(owner domain.UserID) consistencyDomainRef {
	// Upload publication state belongs to the owner namespace authority: the
	// completed outcome and the newly visible file edge must share one commit.
	return namespaceReference(owner)
}

func uploadRecordKey(uploadID string) string { return "upload/" + uploadID }

func uploadIdempotencyKey(value string) string {
	return "upload-idempotency/" + storageformat.Digest([]byte(value))
}

func decodePortableUploadRecord(body []byte, owner domain.UserID, uploadID string) (storageformat.PortableUploadRecord, error) {
	var record storageformat.PortableUploadRecord
	if err := decodeCanonicalValue(body, &record); err != nil {
		return storageformat.PortableUploadRecord{}, err
	}
	if err := storageformat.ValidatePortableUploadRecord(record); err != nil || record.OwnerID != owner.String() || record.UploadID != uploadID || record.BlobID != uploadID {
		return storageformat.PortableUploadRecord{}, domain.NewError(domain.ErrorInvalid, "misbound portable upload record")
	}
	return record, nil
}

func (s *FileStore) portableUpload(ctx context.Context, owner domain.UserID, uploadID string) (storageformat.PortableUploadRecord, consistencyDomainValue, error) {
	value, err := s.engine.stateDomainStore().get(ctx, uploadDomainReference(owner), uploadRecordKey(uploadID))
	if err != nil {
		return storageformat.PortableUploadRecord{}, consistencyDomainValue{}, err
	}
	record, err := decodePortableUploadRecord(value.Data, owner, uploadID)
	return record, value, err
}

// portableUploadSnapshot authenticates the upload record and returns the
// exact domain head that made it visible. Helpers that subsequently publish a
// state transition can reuse that snapshot as their CAS precondition instead
// of issuing a redundant second provider read.
func (s *FileStore) portableUploadSnapshot(ctx context.Context, owner domain.UserID, uploadID string) (storageformat.PortableUploadRecord, consistencyDomainValue, consistencyDomainHeadSnapshot, *consistencyDomainTreeSession, error) {
	store := s.engine.stateDomainStore()
	reference := uploadDomainReference(owner)
	snapshot, err := store.loadHead(ctx, reference)
	if err != nil {
		return storageformat.PortableUploadRecord{}, consistencyDomainValue{}, consistencyDomainHeadSnapshot{}, nil, err
	}
	if !snapshot.exists || !snapshot.head.Registered {
		return storageformat.PortableUploadRecord{}, consistencyDomainValue{}, consistencyDomainHeadSnapshot{}, nil, domain.NewError(domain.ErrorNotFound, "upload does not exist")
	}
	if _, _, found, lockErr := transitionLockAtHead009(ctx, store, reference, snapshot.head); lockErr != nil {
		return storageformat.PortableUploadRecord{}, consistencyDomainValue{}, consistencyDomainHeadSnapshot{}, nil, lockErr
	} else if found {
		return storageformat.PortableUploadRecord{}, consistencyDomainValue{}, consistencyDomainHeadSnapshot{}, nil, domain.WrapError(domain.ErrorUnavailable, "upload domain has a pending transition", errTransitionPending009)
	}
	session := newConsistencyDomainTreeSession(store, reference)
	value, found, err := store.lookupAtHeadWithSession(ctx, reference, snapshot.head, uploadRecordKey(uploadID), session)
	if err != nil {
		return storageformat.PortableUploadRecord{}, consistencyDomainValue{}, consistencyDomainHeadSnapshot{}, nil, err
	}
	if !found {
		return storageformat.PortableUploadRecord{}, consistencyDomainValue{}, consistencyDomainHeadSnapshot{}, nil, domain.NewError(domain.ErrorNotFound, "upload does not exist")
	}
	record, err := decodePortableUploadRecord(value.Data, owner, uploadID)
	return record, value, snapshot, session, err
}

func (s *FileStore) portableUploadAtView(ctx context.Context, view *namespaceView, owner domain.UserID, uploadID string) (storageformat.PortableUploadRecord, consistencyDomainValue, error) {
	if view == nil || view.reference != uploadDomainReference(owner) {
		return storageformat.PortableUploadRecord{}, consistencyDomainValue{}, domain.NewError(domain.ErrorInvalid, "upload view is misbound")
	}
	value, found, err := s.engine.stateDomainStore().lookupAtHeadWithSession(ctx, view.reference, view.head, uploadRecordKey(uploadID), view.session)
	if err != nil {
		return storageformat.PortableUploadRecord{}, consistencyDomainValue{}, err
	}
	if !found {
		return storageformat.PortableUploadRecord{}, consistencyDomainValue{}, domain.NewError(domain.ErrorNotFound, "upload does not exist")
	}
	record, err := decodePortableUploadRecord(value.Data, owner, uploadID)
	return record, value, err
}

func (s *FileStore) portableUploadByIdempotencyAtView(ctx context.Context, view *namespaceView, owner domain.UserID, keyValue, fingerprint string) (storageformat.PortableUploadRecord, bool, error) {
	if keyValue == "" {
		return storageformat.PortableUploadRecord{}, false, nil
	}
	if view == nil || view.reference != uploadDomainReference(owner) {
		return storageformat.PortableUploadRecord{}, false, domain.NewError(domain.ErrorInvalid, "upload idempotency view is misbound")
	}
	store := s.engine.stateDomainStore()
	value, found, err := store.lookupAtHeadWithSession(ctx, view.reference, view.head, uploadIdempotencyKey(keyValue), view.session)
	if err != nil || !found {
		return storageformat.PortableUploadRecord{}, false, err
	}
	var idempotency storageformat.PortableUploadIdempotency
	if err := decodeCanonicalValue(value.Data, &idempotency); err != nil || storageformat.ValidatePortableUploadIdempotency(idempotency) != nil || idempotency.OwnerID != owner.String() || idempotency.KeyDigest != storageformat.Digest([]byte(keyValue)) {
		return storageformat.PortableUploadRecord{}, true, domain.NewError(domain.ErrorInvalid, "invalid upload idempotency record")
	}
	if idempotency.Fingerprint != fingerprint {
		return storageformat.PortableUploadRecord{}, true, domain.NewError(domain.ErrorConflict, "idempotency key was used for another upload")
	}
	record, _, err := s.portableUploadAtView(ctx, view, owner, idempotency.UploadID)
	return record, true, err
}

func (s *FileStore) portableUploadByIdempotency(ctx context.Context, owner domain.UserID, keyValue, fingerprint string) (storageformat.PortableUploadRecord, bool, error) {
	if keyValue == "" {
		return storageformat.PortableUploadRecord{}, false, nil
	}
	store := s.engine.stateDomainStore()
	reference := uploadDomainReference(owner)
	value, err := store.get(ctx, reference, uploadIdempotencyKey(keyValue))
	if errors.Is(err, domain.ErrNotFound) {
		return storageformat.PortableUploadRecord{}, false, nil
	}
	if err != nil {
		return storageformat.PortableUploadRecord{}, false, err
	}
	var idempotency storageformat.PortableUploadIdempotency
	if err := decodeCanonicalValue(value.Data, &idempotency); err != nil || storageformat.ValidatePortableUploadIdempotency(idempotency) != nil || idempotency.OwnerID != owner.String() || idempotency.KeyDigest != storageformat.Digest([]byte(keyValue)) {
		return storageformat.PortableUploadRecord{}, true, domain.NewError(domain.ErrorInvalid, "invalid upload idempotency record")
	}
	if idempotency.Fingerprint != fingerprint {
		return storageformat.PortableUploadRecord{}, true, domain.NewError(domain.ErrorConflict, "idempotency key was used for another upload")
	}
	recordValue, err := store.get(ctx, reference, uploadRecordKey(idempotency.UploadID))
	if err != nil {
		return storageformat.PortableUploadRecord{}, true, err
	}
	record, err := decodePortableUploadRecord(recordValue.Data, owner, idempotency.UploadID)
	return record, true, err
}

func (s *FileStore) runtimeUploadLease(ctx context.Context, uploadID string) ([]byte, objectstore.Object, error) {
	transfers, err := s.transferBackend()
	if err != nil {
		return nil, objectstore.Object{}, err
	}
	key := storageformat.LeaseKey(transfers.BackendKind(), uploadID)
	object, err := s.engine.backend.Get(ctx, key)
	if err != nil {
		return nil, objectstore.Object{}, err
	}
	if len(object.Body) == 0 {
		return nil, objectstore.Object{}, domain.NewError(domain.ErrorInvalid, "empty runtime upload lease")
	}
	return append([]byte(nil), object.Body...), object, nil
}

func (s *FileStore) deleteRuntimeUploadLease(ctx context.Context, uploadID string) error {
	transfers, err := s.transferBackend()
	if err != nil {
		return err
	}
	key := storageformat.LeaseKey(transfers.BackendKind(), uploadID)
	object, err := s.engine.backend.Get(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.deleteKnownRuntimeUploadLease(ctx, object)
}

func (s *FileStore) deleteKnownRuntimeUploadLease(ctx context.Context, object objectstore.Object) error {
	err := s.engine.backend.Delete(ctx, object.Key, objectstore.DeleteCondition{Version: object.Version})
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrPreconditionFailed) {
		return nil
	}
	return err
}

func (s *FileStore) abortAndDeleteRuntimeUploadLease(ctx context.Context, uploadID string) error {
	lease, object, err := s.runtimeUploadLease(ctx, uploadID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	transfers, err := s.transferBackend()
	if err != nil {
		return err
	}
	if err := transfers.AbortUpload(ctx, lease); err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	err = s.engine.backend.Delete(ctx, object.Key, objectstore.DeleteCondition{Version: object.Version})
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrPreconditionFailed) {
		return nil
	}
	return err
}

// cleanupPortableUpload is an idempotent, helpable external-effect task. The
// terminal logical state is already authoritative; provider cleanup failure is
// retained as CleanupPending and can be retried by any replica or checkpoint.
func (s *FileStore) cleanupPortableUpload(ctx context.Context, owner domain.UserID, uploadID string, knownLease *objectstore.Object) error {
	record, value, snapshot, session, err := s.portableUploadSnapshot(ctx, owner, uploadID)
	if err != nil || !record.CleanupPending {
		return err
	}
	switch record.State {
	case storageformat.UploadCompleted:
		if knownLease != nil {
			transfers, transferErr := s.transferBackend()
			if transferErr != nil {
				return transferErr
			}
			expected := storageformat.LeaseKey(transfers.BackendKind(), uploadID)
			if knownLease.Key != expected || len(knownLease.Body) == 0 {
				return domain.NewError(domain.ErrorInvalid, "known runtime upload lease is misbound")
			}
			err = s.deleteKnownRuntimeUploadLease(ctx, *knownLease)
		} else {
			err = s.deleteRuntimeUploadLease(ctx, uploadID)
		}
		if err != nil {
			return err
		}
	case storageformat.UploadAborted:
		if err := s.abortAndDeleteRuntimeUploadLease(ctx, uploadID); err != nil {
			return err
		}
	default:
		return domain.NewError(domain.ErrorInvalid, "non-terminal upload requested provider cleanup")
	}
	record.CleanupPending = false
	body, err := storageformat.EncodeCanonical(record)
	if err != nil {
		return err
	}
	result, err := storageformat.EncodeCanonical(storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: namespaceRequestFingerprint("upload-cleanup", record.UploadID, string(record.State)), Upload: &storageformat.NamespaceUploadMutationResult{UploadID: record.UploadID, State: string(record.State)}})
	if err != nil {
		return err
	}
	_, err = s.engine.stateDomainStore().mutatePrepared(ctx, uploadDomainReference(owner), consistencyDomainMutation{
		ID:      "upload-cleanup:" + record.UploadID + ":" + string(record.State),
		Changes: []consistencyDomainChange{{Key: uploadRecordKey(record.UploadID), Require: domainValuePresent, ExpectedVersion: value.LogicalVersion, Value: body}}, Result: result,
	}, &snapshot, session)
	if err != nil {
		current, _, readErr := s.portableUpload(ctx, owner, uploadID)
		if readErr == nil && current.State == record.State && !current.CleanupPending {
			return nil
		}
	}
	return err
}

func (s *FileStore) finishUploadCleanup(ctx context.Context, owner domain.UserID, uploadID string, knownLease ...objectstore.Object) {
	// A single transient provider response should not leave routine operations
	// dirty. Persistent outages remain represented durably by CleanupPending.
	for attempts := 0; attempts < 2; attempts++ {
		var lease *objectstore.Object
		if attempts == 0 && len(knownLease) != 0 {
			lease = &knownLease[0]
		}
		if err := s.cleanupPortableUpload(ctx, owner, uploadID, lease); err == nil {
			return
		}
	}
}

func (s *FileStore) resumePortableUpload(ctx context.Context, record storageformat.PortableUploadRecord) (domain.UploadCapability, error) {
	if !s.engine.clock.Now().Before(record.ExpiresAt) {
		return domain.UploadCapability{}, domain.NewError(domain.ErrorConflict, "upload is no longer active")
	}
	if record.State == storageformat.UploadInitializing {
		return s.initializePortableUpload(ctx, record, false)
	}
	if record.State != storageformat.UploadActive {
		return domain.UploadCapability{}, domain.NewError(domain.ErrorConflict, "upload is no longer active")
	}
	lease, _, err := s.runtimeUploadLease(ctx, record.UploadID)
	if err != nil {
		return domain.UploadCapability{}, err
	}
	transfers, err := s.transferBackend()
	if err != nil {
		return domain.UploadCapability{}, err
	}
	capability, err := transfers.ResumeUpload(ctx, lease)
	if err != nil {
		return domain.UploadCapability{}, err
	}
	return domainUploadCapability(record.UploadID, capability), nil
}

// initializePortableUpload performs the non-transactional provider step only
// after the canonical intent exists. Concurrent helpers race on the transient
// lease create; losers revoke their own provider session and resume the winner.
func (s *FileStore) initializePortableUpload(ctx context.Context, intent storageformat.PortableUploadRecord, leaseKnownAbsent bool) (domain.UploadCapability, error) {
	if intent.State != storageformat.UploadInitializing || !s.engine.clock.Now().Before(intent.ExpiresAt) {
		return domain.UploadCapability{}, domain.NewError(domain.ErrorConflict, "upload is no longer initializable")
	}
	owner, err := domain.ParseUserID(intent.OwnerID)
	if err != nil {
		return domain.UploadCapability{}, domain.NewError(domain.ErrorInvalid, "upload owner is invalid")
	}
	transfers, err := s.transferBackend()
	if err != nil {
		return domain.UploadCapability{}, err
	}
	record, value, snapshot, session, err := s.portableUploadSnapshot(ctx, owner, intent.UploadID)
	if err != nil {
		return domain.UploadCapability{}, err
	}
	if record.State != storageformat.UploadInitializing && record.State != storageformat.UploadActive {
		_ = s.cleanupPortableUpload(ctx, owner, record.UploadID, nil)
		return domain.UploadCapability{}, domain.NewError(domain.ErrorConflict, "upload initialization was superseded")
	}
	leaseKey := storageformat.LeaseKey(transfers.BackendKind(), intent.UploadID)
	var capability objectstore.UploadCapability
	var leaseObject objectstore.Object
	var leaseErr error
	if !leaseKnownAbsent || record.State == storageformat.UploadActive {
		leaseObject, leaseErr = s.engine.backend.Get(ctx, leaseKey)
	} else {
		leaseErr = domain.ErrNotFound
	}
	if errors.Is(leaseErr, domain.ErrNotFound) {
		handle, beginErr := transfers.BeginUpload(ctx, objectstore.UploadRequest{
			UploadID: intent.UploadID, Key: storageformat.BlobKey(intent.OwnerID, intent.BlobID), Size: intent.Size,
			MediaType: intent.MediaType, Resumable: intent.Resumable, ExpiresAt: intent.ExpiresAt,
		})
		if beginErr != nil {
			return domain.UploadCapability{}, beginErr
		}
		version, putErr := s.engine.backend.Put(ctx, leaseKey, handle.Lease, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
		if putErr == nil {
			leaseObject = objectstore.Object{Key: leaseKey, Body: append([]byte(nil), handle.Lease...), Version: version, Size: int64(len(handle.Lease))}
			capability = handle.Capability
		} else {
			_ = transfers.AbortUpload(ctx, handle.Lease)
			if !errors.Is(putErr, domain.ErrConflict) && !errors.Is(putErr, domain.ErrPreconditionFailed) {
				return domain.UploadCapability{}, putErr
			}
			leaseObject, leaseErr = s.engine.backend.Get(ctx, leaseKey)
			if leaseErr != nil {
				return domain.UploadCapability{}, leaseErr
			}
		}
	} else if leaseErr != nil {
		return domain.UploadCapability{}, leaseErr
	}
	if len(leaseObject.Body) == 0 {
		return domain.UploadCapability{}, domain.NewError(domain.ErrorInvalid, "empty runtime upload lease")
	}
	if capability.URL == "" {
		capability, err = transfers.ResumeUpload(ctx, leaseObject.Body)
		if err != nil {
			return domain.UploadCapability{}, err
		}
	}
	if record.State == storageformat.UploadActive {
		return domainUploadCapability(record.UploadID, capability), nil
	}
	record.State = storageformat.UploadActive
	body, err := storageformat.EncodeCanonical(record)
	if err != nil {
		return domain.UploadCapability{}, err
	}
	resultBody, err := storageformat.EncodeCanonical(storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: namespaceRequestFingerprint("upload-activate", record.UploadID), Upload: &storageformat.NamespaceUploadMutationResult{UploadID: record.UploadID, State: "active"}})
	if err != nil {
		return domain.UploadCapability{}, err
	}
	_, err = s.engine.stateDomainStore().mutatePrepared(ctx, uploadDomainReference(owner), consistencyDomainMutation{
		ID: record.UploadID + "-activate", Changes: []consistencyDomainChange{{Key: uploadRecordKey(record.UploadID), Require: domainValuePresent, ExpectedVersion: value.LogicalVersion, Value: body}}, Result: resultBody,
	}, &snapshot, session)
	if err != nil {
		current, _, readErr := s.portableUpload(ctx, owner, record.UploadID)
		if readErr == nil && current.State == storageformat.UploadActive {
			return domainUploadCapability(record.UploadID, capability), nil
		}
		if readErr == nil && current.State != storageformat.UploadInitializing {
			_ = s.cleanupPortableUpload(ctx, owner, record.UploadID, nil)
			return domain.UploadCapability{}, domain.NewError(domain.ErrorConflict, "upload initialization was superseded")
		}
		return domain.UploadCapability{}, err
	}
	return domainUploadCapability(record.UploadID, capability), nil
}

func (s *FileStore) createUpload008(ctx context.Context, scope domain.Scope, request domain.CreateUploadRequest) (domain.UploadCapability, error) {
	if !request.Path.Valid() || request.Path.IsRoot() || request.Size < 0 {
		return domain.UploadCapability{}, domain.NewError(domain.ErrorInvalid, "invalid upload destination or size")
	}
	mediaType, err := domain.NormalizeMediaType(request.MediaType)
	if err != nil {
		return domain.UploadCapability{}, err
	}
	conflict, err := domain.NormalizeConflictMode(request.Conflict)
	if err != nil {
		return domain.UploadCapability{}, err
	}
	if err := validatePortableIdempotencyKey(request.IdempotencyKey); err != nil {
		return domain.UploadCapability{}, err
	}
	fingerprint := namespaceRequestFingerprint("upload", areaName(scope.Area()), request.Path.String(), fmt.Sprint(request.Size), mediaType, string(conflict), string(request.ExpectedVersion), fmt.Sprint(request.Resumable))
	namespace := newNamespaceStore(s.engine)
	view, err := namespace.loadView(ctx, scope.UserID(), "")
	if err != nil {
		return domain.UploadCapability{}, err
	}
	mutationID := ""
	if request.IdempotencyKey != "" {
		mutationID = "upload-create:" + storageformat.Digest([]byte(request.IdempotencyKey))
		if replay, replayErr := namespace.operationReplay(ctx, view, mutationID, fingerprint); replayErr != nil {
			return domain.UploadCapability{}, replayErr
		} else if replay != nil && replay.Upload != nil {
			existing, _, recordErr := s.portableUploadAtView(ctx, view, scope.UserID(), replay.Upload.UploadID)
			if recordErr != nil {
				return domain.UploadCapability{}, recordErr
			}
			return s.resumePortableUpload(ctx, existing)
		}
		// Schema-007 upload idempotency records are retained by the adjacent
		// transformer. New outcomes are authoritative, but this same-head lookup
		// preserves replay across cutover without another provider HEAD read.
		if existing, found, replayErr := s.portableUploadByIdempotencyAtView(ctx, view, scope.UserID(), request.IdempotencyKey, fingerprint); found || replayErr != nil {
			if replayErr != nil {
				return domain.UploadCapability{}, replayErr
			}
			return s.resumePortableUpload(ctx, existing)
		}
	}
	resolved, targetExisted, err := namespace.prepareDestinationAtView(ctx, view, scope, request.Path, conflict, request.ExpectedVersion)
	if err != nil {
		return domain.UploadCapability{}, err
	}
	uploadID, err := s.engine.ids.OpaqueID()
	if err != nil {
		return domain.UploadCapability{}, err
	}
	now := s.engine.clock.Now().UTC()
	record := storageformat.PortableUploadRecord{
		SchemaVersion: 1, UploadID: uploadID, OwnerID: scope.UserID().String(), Area: areaName(scope.Area()), RequestedPath: request.Path.String(), ResolvedPath: resolved.String(), BlobID: uploadID,
		Size: request.Size, MediaType: mediaType, Conflict: conflict, ExpectedVersion: request.ExpectedVersion, TargetExisted: targetExisted, Resumable: request.Resumable, State: storageformat.UploadInitializing,
		CreatedAt: now, ExpiresAt: now.Add(s.engine.uploadTTL),
	}
	recordBody, err := storageformat.EncodeCanonical(record)
	if err != nil {
		return domain.UploadCapability{}, err
	}
	changes := []consistencyDomainChange{{Key: uploadRecordKey(uploadID), Require: domainValueAbsent, Value: recordBody}}
	if mutationID == "" {
		mutationID = "upload-create:" + uploadID
	}
	if request.IdempotencyKey != "" {
		idempotency := storageformat.PortableUploadIdempotency{SchemaVersion: 1, OwnerID: scope.UserID().String(), KeyDigest: storageformat.Digest([]byte(request.IdempotencyKey)), Fingerprint: fingerprint, UploadID: uploadID}
		idempotencyBody, encodeErr := storageformat.EncodeCanonical(idempotency)
		if encodeErr != nil {
			return domain.UploadCapability{}, encodeErr
		}
		changes = append(changes, consistencyDomainChange{Key: uploadIdempotencyKey(request.IdempotencyKey), Require: domainValueAbsent, Value: idempotencyBody})
	}
	resultBody, err := storageformat.EncodeCanonical(storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: fingerprint, Upload: &storageformat.NamespaceUploadMutationResult{UploadID: uploadID, State: "initializing"}})
	if err != nil {
		return domain.UploadCapability{}, err
	}
	if _, err := s.engine.stateDomainStore().mutatePrepared(ctx, uploadDomainReference(scope.UserID()), consistencyDomainMutation{ID: mutationID, Changes: changes, Result: resultBody}, view.headSnapshot, view.session); err != nil {
		if existing, found, replayErr := s.portableUploadByIdempotency(ctx, scope.UserID(), request.IdempotencyKey, fingerprint); found || replayErr != nil {
			if replayErr != nil {
				return domain.UploadCapability{}, replayErr
			}
			return s.resumePortableUpload(ctx, existing)
		}
		return domain.UploadCapability{}, err
	}
	return s.initializePortableUpload(ctx, record, true)
}

func (s *FileStore) uploadStatus008(ctx context.Context, scope domain.Scope, uploadID domain.UploadID) (domain.UploadStatus, error) {
	if uploadID == "" {
		return domain.UploadStatus{}, domain.NewError(domain.ErrorInvalid, "upload ID is required")
	}
	record, _, err := s.portableUpload(ctx, scope.UserID(), string(uploadID))
	if err != nil {
		return domain.UploadStatus{}, err
	}
	if record.Area != areaName(scope.Area()) {
		return domain.UploadStatus{}, domain.NewError(domain.ErrorNotFound, "upload does not exist")
	}
	path, err := domain.ParseUserPath(record.RequestedPath)
	if err != nil {
		return domain.UploadStatus{}, domain.NewError(domain.ErrorInvalid, "stored upload path is invalid")
	}
	status := domain.UploadStatus{UploadID: uploadID, Path: path, DeclaredSize: record.Size, ExpiresAt: record.ExpiresAt}
	if record.Resumable {
		status.Protocol = domain.UploadResumable
	} else {
		status.Protocol = domain.UploadSingle
	}
	switch record.State {
	case storageformat.UploadInitializing:
		status.State = domain.UploadStateActive
		return status, nil
	case storageformat.UploadCompleted:
		status.State, status.ConfirmedOffset = domain.UploadStateCompleted, record.Size
		return status, nil
	case storageformat.UploadAborted:
		status.State = domain.UploadStateAborted
		return status, nil
	}
	if !s.engine.clock.Now().Before(record.ExpiresAt) {
		status.State = domain.UploadStateExpired
		return status, nil
	}
	lease, _, err := s.runtimeUploadLease(ctx, record.UploadID)
	if err != nil {
		return domain.UploadStatus{}, err
	}
	transfers, err := s.transferBackend()
	if err != nil {
		return domain.UploadStatus{}, err
	}
	progress, err := transfers.UploadProgress(ctx, lease)
	if err != nil {
		return domain.UploadStatus{}, err
	}
	status.State, status.ConfirmedOffset = domain.UploadStateActive, progress.Offset
	return status, nil
}

func (s *FileStore) completeUpload008(ctx context.Context, scope domain.Scope, request domain.CompleteUploadRequest) (domain.Entry, error) {
	if request.UploadID == "" || !request.Path.Valid() || request.Path.IsRoot() || request.Size < 0 {
		return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "invalid upload completion")
	}
	namespace := newNamespaceStore(s.engine)
	view, err := namespace.loadView(ctx, scope.UserID(), "")
	if err != nil {
		return domain.Entry{}, err
	}
	record, value, err := s.portableUploadAtView(ctx, view, scope.UserID(), string(request.UploadID))
	if err != nil {
		return domain.Entry{}, err
	}
	if record.Area != areaName(scope.Area()) {
		return domain.Entry{}, domain.NewError(domain.ErrorNotFound, "upload does not exist")
	}
	requested, err := domain.ParseUserPath(record.RequestedPath)
	if err != nil || requested != request.Path || request.Size != record.Size {
		return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "upload completion does not match initiation")
	}
	mediaType, err := domain.NormalizeMediaType(request.MediaType)
	if err != nil || mediaType != record.MediaType {
		return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "upload completion media type does not match")
	}
	resolved, err := domain.ParseUserPath(record.ResolvedPath)
	if err != nil {
		return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "stored upload path is invalid")
	}
	if record.State == storageformat.UploadCompleted {
		resolvedEntry, statErr := namespace.resolveEntryAtView(ctx, view, scope, resolved)
		if statErr != nil {
			return domain.Entry{}, statErr
		}
		s.finishUploadCleanup(ctx, scope.UserID(), record.UploadID)
		return namespaceDomainEntry(resolved, resolvedEntry), nil
	}
	if record.State == storageformat.UploadAborted {
		return domain.Entry{}, domain.NewError(domain.ErrorNotFound, "upload does not exist")
	}
	if record.State != storageformat.UploadActive || !s.engine.clock.Now().Before(record.ExpiresAt) {
		return domain.Entry{}, domain.NewError(domain.ErrorConflict, "upload is not active")
	}
	lease, leaseObject, err := s.runtimeUploadLease(ctx, record.UploadID)
	if err != nil {
		return domain.Entry{}, err
	}
	transfers, err := s.transferBackend()
	if err != nil {
		return domain.Entry{}, err
	}
	progress, err := transfers.UploadProgress(ctx, lease)
	if err != nil {
		return domain.Entry{}, err
	}
	if !progress.Complete || progress.Size != record.Size || !progress.Fingerprint.Complete() {
		return domain.Entry{}, domain.NewError(domain.ErrorPreconditionFailed, "uploaded object is incomplete or has invalid provider integrity metadata")
	}
	conflict := record.Conflict
	if conflict == domain.ConflictRename {
		conflict = domain.ConflictFail
	}
	completionFingerprint := namespaceRequestFingerprint("upload-complete", record.UploadID, record.ResolvedPath, fmt.Sprint(record.Size), record.MediaType, progress.Fingerprint.MD5, progress.Fingerprint.CRC32C)
	record.State = storageformat.UploadCompleted
	record.CleanupPending = true
	body, err := storageformat.EncodeCanonical(record)
	if err != nil {
		return domain.Entry{}, err
	}
	entry, err := namespace.publishFileWithChangesAtView(ctx, view, scope, resolved, conflict, record.ExpectedVersion, record.UploadID+"-complete", completionFingerprint, storageformat.DirectoryEntry{
		Kind: domain.EntryFile, BlobID: record.BlobID, Size: record.Size, MediaType: record.MediaType, MD5: progress.Fingerprint.MD5, CRC32C: progress.Fingerprint.CRC32C, ModifiedAt: s.engine.clock.Now().UTC(),
	}, []consistencyDomainChange{{Key: uploadRecordKey(record.UploadID), Require: domainValuePresent, ExpectedVersion: value.LogicalVersion, Value: body}})
	if err != nil {
		return domain.Entry{}, err
	}
	s.finishUploadCleanup(ctx, scope.UserID(), record.UploadID, leaseObject)
	return entry, nil
}

func (s *FileStore) abortUpload008(ctx context.Context, scope domain.Scope, uploadID domain.UploadID) error {
	if uploadID == "" {
		return domain.NewError(domain.ErrorInvalid, "upload ID is required")
	}
	namespace := newNamespaceStore(s.engine)
	view, err := namespace.loadView(ctx, scope.UserID(), "")
	if err != nil {
		return err
	}
	record, value, err := s.portableUploadAtView(ctx, view, scope.UserID(), string(uploadID))
	if err != nil {
		return err
	}
	if record.Area != areaName(scope.Area()) {
		return domain.NewError(domain.ErrorNotFound, "upload does not exist")
	}
	if record.State == storageformat.UploadAborted {
		s.finishUploadCleanup(ctx, scope.UserID(), record.UploadID)
		return nil
	}
	if record.State == storageformat.UploadCompleted {
		return domain.NewError(domain.ErrorConflict, "completed upload cannot be aborted")
	}
	record.State = storageformat.UploadAborted
	record.CleanupPending = true
	body, err := storageformat.EncodeCanonical(record)
	if err != nil {
		return err
	}
	resultBody, err := storageformat.EncodeCanonical(storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: namespaceRequestFingerprint("upload-abort", record.UploadID), Upload: &storageformat.NamespaceUploadMutationResult{UploadID: record.UploadID, State: "aborted"}})
	if err != nil {
		return err
	}
	if _, err := s.engine.stateDomainStore().mutatePrepared(ctx, uploadDomainReference(scope.UserID()), consistencyDomainMutation{ID: record.UploadID + "-abort", Changes: []consistencyDomainChange{{Key: uploadRecordKey(record.UploadID), Require: domainValuePresent, ExpectedVersion: value.LogicalVersion, Value: body}}, Result: resultBody}, view.headSnapshot, view.session); err != nil {
		return err
	}
	s.finishUploadCleanup(ctx, scope.UserID(), record.UploadID)
	return nil
}

func (s *FileStore) createDownload008(ctx context.Context, scope domain.Scope, request domain.CreateDownloadRequest) (domain.DownloadCapability, error) {
	if !request.Path.Valid() || request.Path.IsRoot() {
		return domain.DownloadCapability{}, domain.NewError(domain.ErrorInvalid, "download path is invalid")
	}
	if request.Disposition == "" {
		request.Disposition = domain.DispositionAttachment
	}
	if request.Disposition != domain.DispositionAttachment && request.Disposition != domain.DispositionInline {
		return domain.DownloadCapability{}, domain.NewError(domain.ErrorInvalid, "invalid download disposition")
	}
	entry, err := newNamespaceStore(s.engine).resolveEntry(ctx, scope, request.Path)
	if err != nil {
		return domain.DownloadCapability{}, err
	}
	if entry.Entry.Kind != domain.EntryFile {
		// Preserve the public file-only capability contract. Directories are not
		// downloadable objects and are deliberately indistinguishable from a
		// missing file at this boundary.
		return domain.DownloadCapability{}, domain.NewError(domain.ErrorNotFound, "downloadable file does not exist")
	}
	if request.Version == "" || request.Version != domain.Version(entry.Entry.LogicalVersion) {
		return domain.DownloadCapability{}, domain.NewError(domain.ErrorPreconditionFailed, "download version does not match")
	}
	transfers, err := s.transferBackend()
	if err != nil {
		return domain.DownloadCapability{}, err
	}
	key := storageformat.BlobKey(scope.UserID().String(), entry.Entry.BlobID)
	info, err := s.engine.fileBackend.Head(ctx, key)
	if err != nil {
		return domain.DownloadCapability{}, err
	}
	capability, err := transfers.CreateDownload(ctx, objectstore.DownloadRequest{Key: key, Version: info.Version, Filename: entry.Entry.Name, MediaType: entry.Entry.MediaType, Disposition: request.Disposition, ExpiresAt: s.engine.clock.Now().UTC().Add(s.engine.downloadTTL)})
	if err != nil {
		return domain.DownloadCapability{}, err
	}
	return domain.DownloadCapability{URL: capability.URL, Method: capability.Method, Headers: copyHeaders(capability.Headers), ExpiresAt: capability.ExpiresAt}, nil
}
