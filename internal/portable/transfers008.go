package portable

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func uploadDomainReference(owner domain.UserID) consistencyDomainRef {
	// Upload publication state belongs to the owner namespace authority: the
	// completed outcome and the newly visible file edge must share one commit.
	return namespaceReference(owner)
}

func uploadRecordKey(uploadID string) string { return "upload/" + uploadID }

func uploadBatchAbortKey(batchID string) string { return "upload-batch-abort/" + batchID }

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
	view, err := newNamespaceStore(s.engine).loadView(ctx, owner, "")
	if err != nil {
		return storageformat.PortableUploadRecord{}, consistencyDomainValue{}, err
	}
	return s.portableUploadAtView(ctx, view, owner, uploadID)
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
	session := newConsistencyDomainTreeSession(store, reference)
	if _, _, found, lockErr := transitionLockAtHeadWithSession009(ctx, store, reference, snapshot.head, session); lockErr != nil {
		return storageformat.PortableUploadRecord{}, consistencyDomainValue{}, consistencyDomainHeadSnapshot{}, nil, lockErr
	} else if found {
		return storageformat.PortableUploadRecord{}, consistencyDomainValue{}, consistencyDomainHeadSnapshot{}, nil, domain.WrapError(domain.ErrorUnavailable, "upload domain has a pending transition", errTransitionPending009)
	}
	value, found, err := store.lookupAtHeadWithSession(ctx, reference, snapshot.head, uploadRecordKey(uploadID), session)
	if err != nil {
		return storageformat.PortableUploadRecord{}, consistencyDomainValue{}, consistencyDomainHeadSnapshot{}, nil, err
	}
	if !found {
		return storageformat.PortableUploadRecord{}, consistencyDomainValue{}, consistencyDomainHeadSnapshot{}, nil, domain.NewError(domain.ErrorNotFound, "upload does not exist")
	}
	record, err := decodePortableUploadRecord(value.Data, owner, uploadID)
	if err == nil && record.Batch != nil {
		view := &namespaceView{reference: reference, head: snapshot.head, headSnapshot: &snapshot, session: session, uploadAborts: make(map[string]portableUploadAbortCache)}
		if abort, found, abortErr := s.portableUploadBatchAbortAtView(ctx, view, owner, record.Batch.BatchID); abortErr != nil {
			return storageformat.PortableUploadRecord{}, consistencyDomainValue{}, consistencyDomainHeadSnapshot{}, nil, abortErr
		} else if found && abort.Aborts(record.Batch.Index) {
			record.State, record.CleanupPending, record.Completion = storageformat.UploadAborted, false, nil
		}
	}
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
	if err != nil || record.Batch == nil {
		return record, value, err
	}
	abort, found, err := s.portableUploadBatchAbortAtView(ctx, view, owner, record.Batch.BatchID)
	if err != nil {
		return storageformat.PortableUploadRecord{}, consistencyDomainValue{}, err
	}
	if found {
		if abort.Count != record.Batch.Count {
			return storageformat.PortableUploadRecord{}, consistencyDomainValue{}, domain.NewError(domain.ErrorInvalid, "upload batch abort count is misbound")
		}
		if abort.Aborts(record.Batch.Index) {
			record.State, record.CleanupPending, record.Completion = storageformat.UploadAborted, false, nil
		}
	}
	return record, value, nil
}

func (s *FileStore) portableUploadBatchAbortAtView(ctx context.Context, view *namespaceView, owner domain.UserID, batchID string) (storageformat.PortableUploadBatchAbort, bool, error) {
	if view == nil || view.reference != uploadDomainReference(owner) || batchID == "" {
		return storageformat.PortableUploadBatchAbort{}, false, domain.NewError(domain.ErrorInvalid, "upload batch abort view is misbound")
	}
	if cached, ok := view.uploadAborts[batchID]; ok {
		return cached.record, cached.found, nil
	}
	if view.uploadAborts == nil {
		view.uploadAborts = make(map[string]portableUploadAbortCache)
	}
	value, found, err := s.engine.stateDomainStore().lookupAtHeadWithSession(ctx, view.reference, view.head, uploadBatchAbortKey(batchID), view.session)
	if err != nil {
		return storageformat.PortableUploadBatchAbort{}, false, err
	}
	if !found {
		view.uploadAborts[batchID] = portableUploadAbortCache{}
		return storageformat.PortableUploadBatchAbort{}, false, nil
	}
	var record storageformat.PortableUploadBatchAbort
	if err := decodeCanonicalValue(value.Data, &record); err != nil || storageformat.ValidatePortableUploadBatchAbort(record) != nil || record.OwnerID != owner.String() || record.BatchID != batchID {
		return storageformat.PortableUploadBatchAbort{}, false, domain.NewError(domain.ErrorInvalid, "invalid portable upload batch abort")
	}
	view.uploadAborts[batchID] = portableUploadAbortCache{record: record, value: value, found: true}
	return record, true, nil
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

func (s *FileStore) runtimeUploadLeaseForRecord(ctx context.Context, record storageformat.PortableUploadRecord) ([]byte, objectstore.Object, error) {
	if record.Batch == nil {
		return s.runtimeUploadLease(ctx, record.UploadID)
	}
	transfers, err := s.transferBackend()
	if err != nil {
		return nil, objectstore.Object{}, err
	}
	segmentIndex := (record.Batch.Count - 1) / storageformat.MaxUploadLeaseSegmentItems
	key := storageformat.UploadLeaseSegmentKey(transfers.BackendKind(), record.Batch.BatchID, segmentIndex)
	object, err := s.engine.backend.Get(ctx, key)
	if errors.Is(err, domain.ErrNotFound) && segmentIndex != record.Batch.Index/storageformat.MaxUploadLeaseSegmentItems {
		// Admission may have crashed before publishing the terminal cumulative
		// segment. An earlier progress segment is still sufficient to resume or
		// clean up a member that was already exposed.
		segmentIndex = record.Batch.Index / storageformat.MaxUploadLeaseSegmentItems
		key = storageformat.UploadLeaseSegmentKey(transfers.BackendKind(), record.Batch.BatchID, segmentIndex)
		object, err = s.engine.backend.Get(ctx, key)
	}
	if err != nil {
		return nil, objectstore.Object{}, err
	}
	segment, err := storageformat.DecodePortableUploadLeaseSegment(object.Body, transfers.BackendKind(), record.OwnerID, record.Batch.BatchID, segmentIndex)
	if err != nil {
		return nil, objectstore.Object{}, err
	}
	if segment.TotalCount != record.Batch.Count || record.Batch.Index < segment.FirstIndex {
		return nil, objectstore.Object{}, domain.NewError(domain.ErrorInvalid, "upload lease segment total is misbound")
	}
	offset := record.Batch.Index - segment.FirstIndex
	if offset >= uint64(len(segment.Leases)) || segment.Leases[offset].Index != record.Batch.Index || segment.Leases[offset].UploadID != record.UploadID {
		return nil, objectstore.Object{}, domain.NewError(domain.ErrorInvalid, "upload lease segment member is missing or misbound")
	}
	return append([]byte(nil), segment.Leases[offset].Lease...), object, nil
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
		if record.Batch != nil {
			// A segment is shared by up to 1,000 uploads and remains transient
			// recovery authority until every member is terminal or expired.
			err = nil
		} else if knownLease != nil {
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
		if record.Batch != nil {
			lease, _, leaseErr := s.runtimeUploadLeaseForRecord(ctx, record)
			if errors.Is(leaseErr, domain.ErrNotFound) {
				leaseErr = nil
			}
			if leaseErr == nil {
				transfers, transferErr := s.transferBackend()
				if transferErr != nil {
					return transferErr
				}
				if abortErr := transfers.AbortUpload(ctx, lease); abortErr != nil && !errors.Is(abortErr, domain.ErrNotFound) {
					return abortErr
				}
			}
			if leaseErr != nil {
				return leaseErr
			}
		} else if err := s.abortAndDeleteRuntimeUploadLease(ctx, uploadID); err != nil {
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
	lease, _, err := s.runtimeUploadLeaseForRecord(ctx, record)
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

// ensurePortableUploadLease performs the only unavoidable per-object provider
// work in upload initialization. The canonical intent must already be
// committed before this function is called. A retained lease makes the step
// idempotent after a process crash or lost provider response.
func (s *FileStore) ensurePortableUploadLease(ctx context.Context, intent storageformat.PortableUploadRecord, leaseKnownAbsent bool) (objectstore.UploadCapability, objectstore.Object, error) {
	transfers, err := s.transferBackend()
	if err != nil {
		return objectstore.UploadCapability{}, objectstore.Object{}, err
	}
	leaseKey := storageformat.LeaseKey(transfers.BackendKind(), intent.UploadID)
	var capability objectstore.UploadCapability
	var leaseObject objectstore.Object
	var leaseErr error
	if !leaseKnownAbsent {
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
			return objectstore.UploadCapability{}, objectstore.Object{}, beginErr
		}
		version, putErr := s.engine.backend.Put(ctx, leaseKey, handle.Lease, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
		if putErr == nil {
			leaseObject = objectstore.Object{Key: leaseKey, Body: append([]byte(nil), handle.Lease...), Version: version, Size: int64(len(handle.Lease))}
			capability = handle.Capability
		} else {
			if !errors.Is(putErr, domain.ErrConflict) && !errors.Is(putErr, domain.ErrPreconditionFailed) {
				_ = transfers.AbortUpload(ctx, handle.Lease)
				return objectstore.UploadCapability{}, objectstore.Object{}, putErr
			}
			leaseObject, leaseErr = s.engine.backend.Get(ctx, leaseKey)
			if leaseErr != nil {
				_ = transfers.AbortUpload(ctx, handle.Lease)
				return objectstore.UploadCapability{}, objectstore.Object{}, leaseErr
			}
			// Providers may make BeginUpload idempotent for the canonical upload
			// ID. In that case both contenders receive the same native session;
			// aborting the losing create attempt would revoke the durable winner.
			// Only a genuinely distinct, uncommitted lease is disposable.
			if !bytes.Equal(handle.Lease, leaseObject.Body) {
				_ = transfers.AbortUpload(ctx, handle.Lease)
			}
		}
	} else if leaseErr != nil {
		return objectstore.UploadCapability{}, objectstore.Object{}, leaseErr
	}
	if len(leaseObject.Body) == 0 {
		return objectstore.UploadCapability{}, objectstore.Object{}, domain.NewError(domain.ErrorInvalid, "empty runtime upload lease")
	}
	if capability.URL == "" {
		capability, err = transfers.ResumeUpload(ctx, leaseObject.Body)
		if err != nil {
			return objectstore.UploadCapability{}, objectstore.Object{}, err
		}
	}
	return capability, leaseObject, nil
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
	record, value, snapshot, session, err := s.portableUploadSnapshot(ctx, owner, intent.UploadID)
	if err != nil {
		return domain.UploadCapability{}, err
	}
	if record.State != storageformat.UploadInitializing && record.State != storageformat.UploadActive {
		_ = s.cleanupPortableUpload(ctx, owner, record.UploadID, nil)
		return domain.UploadCapability{}, domain.NewError(domain.ErrorConflict, "upload initialization was superseded")
	}
	capability, _, err := s.ensurePortableUploadLease(ctx, record, leaseKnownAbsent && record.State != storageformat.UploadActive)
	if err != nil {
		return domain.UploadCapability{}, err
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

type portableUploadBatchItem struct {
	record      storageformat.PortableUploadRecord
	fingerprint string
}

func normalizePortableUploadRequest(scope domain.Scope, request domain.CreateUploadRequest) (domain.CreateUploadRequest, string, error) {
	if !request.Path.Valid() || request.Path.IsRoot() || request.Size < 0 {
		return domain.CreateUploadRequest{}, "", domain.NewError(domain.ErrorInvalid, "invalid upload destination or size")
	}
	mediaType, err := domain.NormalizeMediaType(request.MediaType)
	if err != nil {
		return domain.CreateUploadRequest{}, "", err
	}
	conflict, err := domain.NormalizeConflictMode(request.Conflict)
	if err != nil {
		return domain.CreateUploadRequest{}, "", err
	}
	if err := validatePortableIdempotencyKey(request.IdempotencyKey); err != nil || request.IdempotencyKey == "" {
		return domain.CreateUploadRequest{}, "", domain.NewError(domain.ErrorInvalid, "batch upload idempotency key is required")
	}
	request.MediaType, request.Conflict = mediaType, conflict
	fingerprint := namespaceRequestFingerprint("upload", areaName(scope.Area()), request.Path.String(), fmt.Sprint(request.Size), mediaType, string(conflict), string(request.ExpectedVersion), fmt.Sprint(request.Resumable))
	return request, fingerprint, nil
}

// createUploadBatch008 publishes every canonical upload intent in one owner-
// namespace mutation, creates only the unavoidable provider upload sessions in
// parallel, then activates all records in one further namespace mutation. A
// crash at either boundary is resumed from the immutable idempotency records
// and disposable lease objects; it never reruns one state transaction per
// upload.
func (s *FileStore) createUploadBatch008(ctx context.Context, scope domain.Scope, requests []domain.CreateUploadRequest) ([]domain.UploadCapability, error) {
	if len(requests) < 1 || len(requests) > storageformat.MaxPortableUploadBatchItems {
		return nil, domain.NewError(domain.ErrorInvalid, "upload batch must contain 1 to 10000 items")
	}
	normalized := make([]domain.CreateUploadRequest, len(requests))
	fingerprints := make([]string, len(requests))
	seenKeys := make(map[string]struct{}, len(requests))
	for index, request := range requests {
		var err error
		normalized[index], fingerprints[index], err = normalizePortableUploadRequest(scope, request)
		if err != nil {
			return nil, err
		}
		if _, exists := seenKeys[request.IdempotencyKey]; exists {
			return nil, domain.NewError(domain.ErrorInvalid, "upload batch repeats an idempotency key")
		}
		seenKeys[request.IdempotencyKey] = struct{}{}
	}
	batchFingerprint := namespaceRequestFingerprint("upload-batch", strings.Join(fingerprints, "\x00"))
	batchID := storageformat.Digest([]byte("endlessfs-upload-batch-v1\x00" + scope.UserID().String() + "\x00" + batchFingerprint))
	items := make([]portableUploadBatchItem, len(requests))
	created := false
	intentsReady := false
	for attempts := 0; attempts < 8; attempts++ {
		namespace := newNamespaceStore(s.engine)
		view, err := namespace.loadView(ctx, scope.UserID(), "")
		if err != nil {
			return nil, err
		}
		if view.headSnapshot == nil || !view.headSnapshot.exists || !view.headSnapshot.head.Registered {
			if view.headSnapshot == nil {
				return nil, domain.NewError(domain.ErrorInvalid, "upload batch registration view is missing")
			}
			if err := s.engine.stateDomainStore().ensureRegistered(ctx, uploadDomainReference(scope.UserID()), *view.headSnapshot); err != nil {
				return nil, err
			}
			continue
		}
		existing := 0
		for index, request := range normalized {
			record, found, lookupErr := s.portableUploadByIdempotencyAtView(ctx, view, scope.UserID(), request.IdempotencyKey, fingerprints[index])
			if lookupErr != nil {
				return nil, lookupErr
			}
			if found {
				items[index] = portableUploadBatchItem{record: record, fingerprint: fingerprints[index]}
				existing++
			}
		}
		if existing == len(items) {
			for index := range items {
				if items[index].record.Batch == nil || items[index].record.Batch.BatchID != batchID || items[index].record.Batch.Index != uint64(index) {
					return nil, domain.NewError(domain.ErrorInvalid, "upload batch replay has invalid membership")
				}
			}
			intentsReady = true
			break
		}
		if existing != 0 {
			return nil, domain.NewError(domain.ErrorConflict, "upload batch partially overlaps existing idempotency records")
		}
		changes := make([]consistencyDomainChange, 0, len(items)*2)
		seenDestinations := make(map[string]struct{}, len(items))
		now := s.engine.clock.Now().UTC()
		for index, request := range normalized {
			resolved, targetExisted, resolveErr := namespace.prepareDestinationAtView(ctx, view, scope, request.Path, request.Conflict, request.ExpectedVersion)
			if resolveErr != nil {
				return nil, resolveErr
			}
			if _, duplicate := seenDestinations[resolved.String()]; duplicate {
				return nil, domain.NewError(domain.ErrorConflict, "upload batch resolves more than one item to the same destination")
			}
			seenDestinations[resolved.String()] = struct{}{}
			uploadID := storageformat.Digest([]byte(fmt.Sprintf("endlessfs-upload-batch-member-v1\x00%s\x00%016x", batchID, index)))
			record := storageformat.PortableUploadRecord{
				SchemaVersion: 1, UploadID: uploadID, OwnerID: scope.UserID().String(), Area: areaName(scope.Area()), RequestedPath: request.Path.String(), ResolvedPath: resolved.String(), BlobID: uploadID,
				Size: request.Size, MediaType: request.MediaType, Conflict: request.Conflict, ExpectedVersion: request.ExpectedVersion, TargetExisted: targetExisted, Resumable: request.Resumable,
				State: storageformat.UploadInitializing, CreatedAt: now, ExpiresAt: now.Add(s.engine.uploadTTL),
				Batch: &storageformat.PortableUploadBatchMember{BatchID: batchID, Index: uint64(index), Count: uint64(len(requests))},
			}
			recordBody, encodeErr := storageformat.EncodeCanonical(record)
			if encodeErr != nil {
				return nil, encodeErr
			}
			idempotency := storageformat.PortableUploadIdempotency{SchemaVersion: 1, OwnerID: scope.UserID().String(), KeyDigest: storageformat.Digest([]byte(request.IdempotencyKey)), Fingerprint: fingerprints[index], UploadID: uploadID}
			idempotencyBody, encodeErr := storageformat.EncodeCanonical(idempotency)
			if encodeErr != nil {
				return nil, encodeErr
			}
			items[index] = portableUploadBatchItem{record: record, fingerprint: fingerprints[index]}
			changes = append(changes,
				consistencyDomainChange{Key: uploadRecordKey(uploadID), Require: domainValueAbsent, Value: recordBody},
				consistencyDomainChange{Key: uploadIdempotencyKey(request.IdempotencyKey), Require: domainValueAbsent, Value: idempotencyBody},
			)
		}
		resultBody, err := storageformat.EncodeCanonical(storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: batchFingerprint, Upload: &storageformat.NamespaceUploadMutationResult{UploadID: items[0].record.UploadID, State: "initializing"}})
		if err != nil {
			return nil, err
		}
		mutationID := "upload-batch-create:" + storageformat.Digest([]byte(strings.Join(fingerprints, "\x00")))
		if err := view.bindMutation(mutationID, batchFingerprint); err != nil {
			return nil, err
		}
		_, err = s.engine.stateDomainStore().mutateMaterializedPrepared(ctx, uploadDomainReference(scope.UserID()), consistencyDomainMutation{
			ID: mutationID, Changes: changes, Result: resultBody,
		}, view.headSnapshot, view.session)
		if err == nil {
			created = true
			intentsReady = true
			break
		}
		if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) && !errors.Is(err, domain.ErrUnavailable) {
			return nil, err
		}
	}
	if !intentsReady {
		return nil, domain.NewError(domain.ErrorUnavailable, "upload batch intent remained contended")
	}
	if created {
		if err := s.engine.step(ctx, StepUploadBatchAfterIntents); err != nil {
			return nil, err
		}
	}
	return s.activatePortableUploadBatch(ctx, scope.UserID(), items, batchFingerprint, created)
}

func (s *FileStore) activatePortableUploadBatch(ctx context.Context, owner domain.UserID, items []portableUploadBatchItem, batchFingerprint string, leasesKnownAbsent bool) ([]domain.UploadCapability, error) {
	if len(items) == 0 || items[0].record.Batch == nil {
		return nil, domain.NewError(domain.ErrorInvalid, "upload batch membership is missing")
	}
	batchID := items[0].record.Batch.BatchID
	capabilities := make([]domain.UploadCapability, len(items))
	allLeases := make([]storageformat.PortableUploadLease, 0, len(items))
	for first := 0; first < len(items); first += storageformat.MaxUploadLeaseSegmentItems {
		last := first + storageformat.MaxUploadLeaseSegmentItems
		if last > len(items) {
			last = len(items)
		}
		segmentCapabilities, leases, err := s.ensurePortableUploadLeaseSegment(ctx, owner, batchID, uint64(first/storageformat.MaxUploadLeaseSegmentItems), items[first:last], allLeases, leasesKnownAbsent)
		if err != nil {
			return nil, err
		}
		copy(capabilities[first:last], segmentCapabilities)
		allLeases = leases
	}
	if err := s.engine.step(ctx, StepUploadBatchAfterSessions); err != nil {
		return nil, err
	}
	if err := s.engine.step(ctx, StepUploadBatchAfterActivation); err != nil {
		return nil, err
	}
	_ = batchFingerprint // retained in the durable admission outcome binding.
	return capabilities, nil
}

const uploadBatchProviderConcurrency = 100

func (s *FileStore) ensurePortableUploadLeaseSegment(ctx context.Context, owner domain.UserID, batchID string, segmentIndex uint64, items []portableUploadBatchItem, priorLeases []storageformat.PortableUploadLease, knownAbsent bool) ([]domain.UploadCapability, []storageformat.PortableUploadLease, error) {
	transfers, err := s.transferBackend()
	if err != nil {
		return nil, nil, err
	}
	if len(items) < 1 || len(items) > storageformat.MaxUploadLeaseSegmentItems {
		return nil, nil, domain.NewError(domain.ErrorInvalid, "invalid upload lease segment cardinality")
	}
	if items[0].record.Batch == nil {
		return nil, nil, domain.NewError(domain.ErrorInvalid, "upload lease segment batch binding is missing")
	}
	firstIndex := segmentIndex * storageformat.MaxUploadLeaseSegmentItems
	totalCount := items[0].record.Batch.Count
	if uint64(len(priorLeases)) != firstIndex || totalCount < firstIndex+uint64(len(items)) {
		return nil, nil, domain.NewError(domain.ErrorInvalid, "invalid upload lease progress")
	}
	for offset, item := range items {
		if item.record.Batch == nil || item.record.Batch.BatchID != batchID || item.record.Batch.Count != totalCount || item.record.Batch.Index != firstIndex+uint64(offset) || item.record.OwnerID != owner.String() || item.record.State != storageformat.UploadInitializing && item.record.State != storageformat.UploadActive {
			return nil, nil, domain.NewError(domain.ErrorInvalid, "misbound upload lease segment member")
		}
	}
	key := storageformat.UploadLeaseSegmentKey(transfers.BackendKind(), batchID, segmentIndex)
	if !knownAbsent {
		object, getErr := s.engine.backend.Get(ctx, key)
		if getErr == nil {
			stored, decodeErr := storageformat.DecodePortableUploadLeaseSegment(object.Body, transfers.BackendKind(), owner.String(), batchID, segmentIndex)
			if decodeErr != nil || stored.TotalCount != totalCount {
				if decodeErr != nil {
					return nil, nil, decodeErr
				}
				return nil, nil, domain.NewError(domain.ErrorInvalid, "upload lease segment total is misbound")
			}
			capabilities, resumeErr := resumePortableUploadLeaseSegment(ctx, transfers, items, stored)
			if resumeErr != nil {
				return nil, nil, resumeErr
			}
			leases, mergeErr := mergePortableUploadLeaseProgress(priorLeases, stored, firstIndex, totalCount)
			return capabilities, leases, mergeErr
		}
		if !errors.Is(getErr, domain.ErrNotFound) {
			return nil, nil, getErr
		}
	}

	handles := make([]objectstore.UploadHandle, len(items))
	errorsByIndex := make([]error, len(items))
	jobs := make(chan int)
	workers := uploadBatchProviderConcurrency
	if workers > len(items) {
		workers = len(items)
	}
	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func() {
			defer wait.Done()
			for index := range jobs {
				record := items[index].record
				traced := providerbudget.WithTrace(ctx, providerbudget.Trace{Operation: "create-upload-batch", Subsystem: "upload-session", ParallelGroup: "upload-session-batch"})
				handle, beginErr := transfers.BeginUpload(traced, objectstore.UploadRequest{
					UploadID: record.UploadID, Key: storageformat.BlobKey(record.OwnerID, record.BlobID), Size: record.Size,
					MediaType: record.MediaType, Resumable: record.Resumable, ExpiresAt: record.ExpiresAt,
				})
				if beginErr == nil && (len(handle.Lease) < 1 || len(handle.Lease) > storageformat.MaxSealedUploadLeaseBytes) {
					beginErr = domain.NewError(domain.ErrorInvalid, "provider upload lease exceeds the bounded segment envelope")
				}
				handles[index], errorsByIndex[index] = handle, beginErr
			}
		}()
	}
	for index := range items {
		jobs <- index
	}
	close(jobs)
	wait.Wait()
	for _, beginErr := range errorsByIndex {
		if beginErr != nil {
			abortUploadHandles(ctx, transfers, handles)
			return nil, nil, beginErr
		}
	}
	segmentLeases := make([]storageformat.PortableUploadLease, len(items))
	stored := storageformat.PortableUploadLeaseSegment{
		SchemaVersion: 1, BackendKind: transfers.BackendKind(), OwnerID: owner.String(), BatchID: batchID,
		Segment: segmentIndex, TotalCount: totalCount, FirstIndex: firstIndex,
	}
	capabilities := make([]domain.UploadCapability, len(items))
	for index, handle := range handles {
		segmentLeases[index] = storageformat.PortableUploadLease{Index: items[index].record.Batch.Index, UploadID: items[index].record.UploadID, Lease: append([]byte(nil), handle.Lease...)}
		capabilities[index] = domainUploadCapability(items[index].record.UploadID, handle.Capability)
		capabilities[index].BatchID = batchID
		capabilities[index].BatchIndex = items[index].record.Batch.Index
		capabilities[index].BatchCount = items[index].record.Batch.Count
	}
	allLeases := append(append([]storageformat.PortableUploadLease(nil), priorLeases...), segmentLeases...)
	stored.Leases = segmentLeases
	if uint64(len(allLeases)) == totalCount {
		stored.FirstIndex = 0
		stored.Leases = allLeases
	}
	body, err := storageformat.EncodePortableUploadLeaseSegment(stored)
	if err != nil {
		abortUploadHandles(ctx, transfers, handles)
		return nil, nil, err
	}
	if _, err := s.engine.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err == nil {
		return capabilities, allLeases, nil
	} else if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
		abortUploadHandles(ctx, transfers, handles)
		return nil, nil, err
	}
	winnerObject, err := s.engine.backend.Get(ctx, key)
	if err != nil {
		abortUploadHandles(ctx, transfers, handles)
		return nil, nil, err
	}
	winner, err := storageformat.DecodePortableUploadLeaseSegment(winnerObject.Body, transfers.BackendKind(), owner.String(), batchID, segmentIndex)
	if err != nil {
		abortUploadHandles(ctx, transfers, handles)
		return nil, nil, err
	}
	if !bytes.Equal(winnerObject.Body, body) {
		abortUploadHandles(ctx, transfers, handles)
	}
	resumed, resumeErr := resumePortableUploadLeaseSegment(ctx, transfers, items, winner)
	if resumeErr != nil {
		return nil, nil, resumeErr
	}
	leases, mergeErr := mergePortableUploadLeaseProgress(priorLeases, winner, firstIndex, totalCount)
	return resumed, leases, mergeErr
}

func mergePortableUploadLeaseProgress(prior []storageformat.PortableUploadLease, stored storageformat.PortableUploadLeaseSegment, firstIndex, totalCount uint64) ([]storageformat.PortableUploadLease, error) {
	if stored.TotalCount != totalCount {
		return nil, domain.NewError(domain.ErrorInvalid, "upload lease progress total is misbound")
	}
	if stored.FirstIndex == 0 && uint64(len(stored.Leases)) == totalCount {
		return append([]storageformat.PortableUploadLease(nil), stored.Leases...), nil
	}
	if uint64(len(prior)) != firstIndex || stored.FirstIndex != firstIndex {
		return nil, domain.NewError(domain.ErrorInvalid, "upload lease progress is non-contiguous")
	}
	return append(append([]storageformat.PortableUploadLease(nil), prior...), stored.Leases...), nil
}

func abortUploadHandles(ctx context.Context, transfers objectstore.DirectTransferBackend, handles []objectstore.UploadHandle) {
	for _, handle := range handles {
		if len(handle.Lease) != 0 {
			_ = transfers.AbortUpload(ctx, handle.Lease)
		}
	}
}

func resumePortableUploadLeaseSegment(ctx context.Context, transfers objectstore.DirectTransferBackend, items []portableUploadBatchItem, stored storageformat.PortableUploadLeaseSegment) ([]domain.UploadCapability, error) {
	capabilities := make([]domain.UploadCapability, len(items))
	for index, item := range items {
		if item.record.Batch == nil || item.record.Batch.Count != stored.TotalCount || item.record.Batch.Index < stored.FirstIndex {
			return nil, domain.NewError(domain.ErrorInvalid, "upload lease segment does not match authority")
		}
		offset := item.record.Batch.Index - stored.FirstIndex
		if offset >= uint64(len(stored.Leases)) {
			return nil, domain.NewError(domain.ErrorInvalid, "upload lease segment cardinality does not match authority")
		}
		lease := stored.Leases[offset]
		if lease.UploadID != item.record.UploadID || lease.Index != item.record.Batch.Index {
			return nil, domain.NewError(domain.ErrorInvalid, "upload lease segment does not match authority")
		}
		capability, err := transfers.ResumeUpload(ctx, lease.Lease)
		if err != nil {
			return nil, err
		}
		capabilities[index] = domainUploadCapability(lease.UploadID, capability)
		capabilities[index].BatchID = stored.BatchID
		capabilities[index].BatchIndex = lease.Index
		capabilities[index].BatchCount = stored.TotalCount
	}
	return capabilities, nil
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
	lease, _, err := s.runtimeUploadLeaseForRecord(ctx, record)
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
	lease, leaseObject, err := s.runtimeUploadLeaseForRecord(ctx, record)
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
