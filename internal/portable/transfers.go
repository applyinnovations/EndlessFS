package portable

import (
	"context"
	"errors"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const (
	uploadRecordSchema                  = "upload-record-v1"
	transferLeaseSchema                 = "transfer-lease-v1"
	maxInternalUploadCompletionAttempts = 8
)

var errUploadCompletionNeedsRetry = errors.New("upload completion lost a directory-root race")

func (s *FileStore) markUploadAborted(ctx context.Context, operationKey objectstore.Key) {
	object, err := s.engine.backend.Get(ctx, operationKey)
	if err != nil {
		return
	}
	var envelope storageformat.Envelope
	var record storageformat.UploadRecord
	if err := storageformat.DecodeEnvelope(object.Body, operationKey, uploadRecordSchema, &envelope, &record); err != nil || record.State != storageformat.UploadActive {
		return
	}
	record.State = storageformat.UploadAborted
	body, err := storageformat.EncodeEnvelope(uploadRecordSchema, operationKey, envelope.Revision+1, record)
	if err != nil {
		return
	}
	_, _ = s.engine.backend.Put(ctx, operationKey, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version})
}

func domainUploadCapability(uploadID string, capability objectstore.UploadCapability) domain.UploadCapability {
	return domain.UploadCapability{
		UploadID: domain.UploadID(uploadID), Protocol: capability.Protocol, URL: capability.URL,
		Method: capability.Method, Headers: copyHeaders(capability.Headers), ExpiresAt: capability.ExpiresAt, ChunkRules: capability.ChunkRules,
		Framing: capability.Framing, DeclaredSize: capability.DeclaredSize,
	}
}

func (s *FileStore) lookupIdempotentUpload(ctx context.Context, userID domain.UserID, keyValue, fingerprint string) (domain.UploadCapability, bool, error) {
	if keyValue == "" {
		return domain.UploadCapability{}, false, nil
	}
	key := storageformat.IdempotencyKey(userID.String(), keyValue)
	object, err := s.engine.backend.Get(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.UploadCapability{}, false, nil
	}
	if err != nil {
		return domain.UploadCapability{}, false, err
	}
	var envelope storageformat.Envelope
	var idempotency storageformat.IdempotencyRecord
	if err := storageformat.DecodeEnvelope(object.Body, key, idempotencySchema, &envelope, &idempotency); err != nil {
		return domain.UploadCapability{}, false, err
	}
	if idempotency.SchemaVersion != 1 || idempotency.UserID != userID.String() || idempotency.Kind != "upload" || idempotency.KeyDigest != storageformat.Digest([]byte(keyValue)) || idempotency.OperationID == "" {
		return domain.UploadCapability{}, false, domain.NewError(domain.ErrorConflict, "idempotency key belongs to another operation")
	}
	if idempotency.Fingerprint != fingerprint {
		return domain.UploadCapability{}, false, domain.NewError(domain.ErrorConflict, "idempotency key was used for a different request")
	}
	_, _, record, err := s.readUploadRecord(ctx, userID, idempotency.OperationID)
	if err != nil {
		return domain.UploadCapability{}, true, err
	}
	if record.State != storageformat.UploadActive || !s.engine.clock.Now().Before(record.ExpiresAt) {
		return domain.UploadCapability{}, true, domain.NewError(domain.ErrorConflict, "idempotent upload is no longer active")
	}
	lease, _, err := s.readTransferLease(ctx, record)
	if err != nil {
		return domain.UploadCapability{}, true, err
	}
	transfers, err := s.transferBackend()
	if err != nil {
		return domain.UploadCapability{}, true, err
	}
	capability, err := transfers.ResumeUpload(ctx, lease.Ciphertext)
	if err != nil {
		return domain.UploadCapability{}, true, err
	}
	return domainUploadCapability(record.UploadID, capability), true, nil
}

func (s *FileStore) recoverUploadLease(ctx context.Context, operationKey objectstore.Key) error {
	object, err := s.engine.backend.Get(ctx, operationKey)
	if err != nil {
		return err
	}
	var envelope storageformat.Envelope
	var record storageformat.UploadRecord
	if err := storageformat.DecodeEnvelope(object.Body, operationKey, uploadRecordSchema, &envelope, &record); err != nil {
		return err
	}
	if (record.SchemaVersion != 1 && record.SchemaVersion != 2) || record.State != storageformat.UploadActive || record.UploadID == "" || record.StagingKey == "" {
		return domain.NewError(domain.ErrorInvalid, "invalid recoverable upload")
	}
	transfers, err := s.transferBackend()
	if err != nil {
		return err
	}
	leaseKey := storageformat.LeaseKey(transfers.BackendKind(), record.UploadID)
	if record.BackendKind != transfers.BackendKind() || record.LeaseKey != leaseKey.String() {
		return domain.NewError(domain.ErrorPreconditionFailed, "recoverable upload backend does not match")
	}
	if _, err := s.engine.backend.Get(ctx, leaseKey); err == nil {
		return nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if !s.engine.clock.Now().Before(record.ExpiresAt) {
		return nil
	}
	stagingKey, err := objectstore.ParseKey(record.StagingKey)
	if err != nil {
		return err
	}
	handle, err := transfers.BeginUpload(ctx, objectstore.UploadRequest{
		UploadID: record.UploadID, Key: stagingKey, Size: record.Size, MediaType: record.MediaType,
		Resumable: record.Resumable, ExpiresAt: record.ExpiresAt,
	})
	if err != nil {
		return err
	}
	body, err := storageformat.EncodeEnvelope(transferLeaseSchema, leaseKey, 1, storageformat.TransferLease{
		SchemaVersion: 1, BackendKind: transfers.BackendKind(), UploadID: record.UploadID,
		Ciphertext: append([]byte(nil), handle.Lease...), ExpiresAt: record.ExpiresAt,
	})
	if err != nil {
		return err
	}
	if _, err := s.engine.backend.Put(ctx, leaseKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil && !errors.Is(err, domain.ErrConflict) {
		return err
	}
	return nil
}

func (s *FileStore) completeUpload(ctx context.Context, scope domain.Scope, request domain.CompleteUploadRequest, mediaType string, transfers objectstore.DirectTransferBackend, attemptsRemaining int) (domain.Entry, error) {
	operationObject, operationEnvelope, record, err := s.readUploadRecord(ctx, scope.UserID(), string(request.UploadID))
	if err != nil {
		return domain.Entry{}, err
	}
	if record.Area != areaName(scope.Area()) || record.State == storageformat.UploadAborted {
		return domain.Entry{}, domain.NewError(domain.ErrorNotFound, "upload not found")
	}
	if record.RequestedPath != request.Path.String() || record.Size != request.Size || record.MediaType != mediaType {
		return domain.Entry{}, domain.NewError(domain.ErrorPreconditionFailed, "upload constraints do not match initiation")
	}
	resolvedPath, err := domain.ParseUserPath(record.ResolvedPath)
	if err != nil {
		return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "stored upload destination is invalid")
	}
	if record.State == storageformat.UploadActive {
		if resumed, found, resumeErr := s.resumeUploadCompletion(ctx, scope, record); found || resumeErr != nil {
			if resumeErr != nil {
				if errors.Is(resumeErr, errUploadCompletionNeedsRetry) {
					return s.retryUploadCompletion(ctx, scope, request, mediaType, transfers, operationObject, operationEnvelope, record, attemptsRemaining)
				}
				return domain.Entry{}, resumeErr
			}
			if err := s.finishUpload(ctx, operationObject, operationEnvelope, record); err != nil {
				return domain.Entry{}, err
			}
			return resumed, nil
		}
	}
	parentTrail, err := s.resolveMutableDirectoryMetadataTrail(ctx, scope, resolvedPath.Parent())
	if err != nil {
		return domain.Entry{}, err
	}
	parentNode := parentTrail[len(parentTrail)-1]
	parent := parentNode.snapshot
	if parent.pending {
		return domain.Entry{}, domain.NewError(domain.ErrorUnavailable, "upload destination has a pending operation")
	}
	current, lookupErr := s.directoryIndexEntry(ctx, scope, parentNode.directoryID, parent.manifest, resolvedPath.Name())
	exists := lookupErr == nil
	if lookupErr != nil && !errors.Is(lookupErr, domain.ErrNotFound) {
		return domain.Entry{}, lookupErr
	}
	if record.State == storageformat.UploadCompleted {
		if !exists || !matchesUploadEntry(record, current) {
			return domain.Entry{}, domain.NewError(domain.ErrorPreconditionFailed, "completed upload destination changed")
		}
		return domainEntry(resolvedPath, current), nil
	}
	if exists && matchesUploadEntry(record, current) {
		if err := s.finishUpload(ctx, operationObject, operationEnvelope, record); err != nil {
			return domain.Entry{}, err
		}
		return domainEntry(resolvedPath, current), nil
	}
	if !s.engine.clock.Now().Before(record.ExpiresAt) {
		return domain.Entry{}, domain.NewError(domain.ErrorPreconditionFailed, "upload capability expired")
	}
	lease, _, err := s.readTransferLease(ctx, record)
	if err != nil {
		return domain.Entry{}, err
	}
	progress, err := transfers.UploadProgress(ctx, lease.Ciphertext)
	if err != nil {
		return domain.Entry{}, err
	}
	if !progress.Complete || progress.Offset != record.Size || progress.Size != record.Size || progress.Version == "" {
		return domain.Entry{}, domain.NewError(domain.ErrorPreconditionFailed, "upload is incomplete")
	}
	if !progress.Fingerprint.Complete() {
		return domain.Entry{}, domain.NewError(domain.ErrorPreconditionFailed, "provider did not return the required MD5 and CRC32C content fingerprint")
	}
	if record.TargetExisted {
		if !exists || domain.Version(current.LogicalVersion) != record.ExpectedVersion {
			return domain.Entry{}, domain.NewError(domain.ErrorPreconditionFailed, "upload destination changed")
		}
	} else if exists {
		return domain.Entry{}, domain.NewError(domain.ErrorConflict, "upload destination appeared during transfer")
	}
	blobID := record.UploadID
	blobKey := storageformat.BlobKey(scope.UserID().String(), blobID)
	entry := storageformat.DirectoryEntry{
		Name: resolvedPath.Name(), NameDigest: storageformat.NameDigest(resolvedPath.Name()), Kind: domain.EntryFile,
		BlobID: blobID, Size: record.Size, MediaType: mediaType,
		MD5: progress.Fingerprint.MD5, CRC32C: progress.Fingerprint.CRC32C, ModifiedAt: record.CreatedAt,
	}
	entry.LogicalVersion, err = directoryEntryVersion(entry)
	if err != nil {
		return domain.Entry{}, err
	}
	var existingPointer *storageformat.DirectoryEntry
	if exists {
		existingPointer = &current
	}
	updates := make(map[string]directoryUpdate)
	if err := applyDirectoryEntryChange(updates, parentTrail, existingPointer, &entry); err != nil {
		return domain.Entry{}, err
	}
	ownerID, err := s.engine.ids.OpaqueID()
	if err != nil {
		return domain.Entry{}, err
	}
	stagingKey, err := objectstore.ParseKey(record.StagingKey)
	if err != nil {
		return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "stored staging key is invalid")
	}
	var copies []storageformat.MutationCopy
	if record.SchemaVersion == 1 {
		copies = []storageformat.MutationCopy{{
			SourceKey: stagingKey.String(), DestinationKey: blobKey.String(), Size: record.Size,
			MD5: progress.Fingerprint.MD5, CRC32C: progress.Fingerprint.CRC32C,
		}}
	} else if stagingKey != blobKey {
		return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "direct upload did not target its immutable blob")
	}
	postOccurrence, err := catalogOccurrence(scope, resolvedPath, entry)
	if err != nil {
		return domain.Entry{}, err
	}
	catalogChanges := []catalogChange{{post: &postOccurrence}}
	if exists {
		preOccurrence, occurrenceErr := catalogOccurrence(scope, resolvedPath, current)
		if occurrenceErr != nil {
			return domain.Entry{}, occurrenceErr
		}
		catalogChanges[0].pre = &preOccurrence
	}
	operation, operationBody, err := s.buildFileOperation(ctx, scope.UserID(), record.CompletionOperationID, ownerID, "upload-complete", updates, nil, copies, catalogChanges)
	if err != nil {
		if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrUnavailable) {
			return s.retryUploadCompletion(ctx, scope, request, mediaType, transfers, operationObject, operationEnvelope, record, attemptsRemaining)
		}
		return domain.Entry{}, err
	}
	result, err := s.startFileOperation(ctx, operation, operationBody, "", "")
	if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPreconditionFailed) {
		if resumed, found, resumeErr := s.resumeUploadCompletion(ctx, scope, record); found || resumeErr != nil {
			if resumeErr != nil {
				if errors.Is(resumeErr, errUploadCompletionNeedsRetry) {
					return s.retryUploadCompletion(ctx, scope, request, mediaType, transfers, operationObject, operationEnvelope, record, attemptsRemaining)
				}
				return domain.Entry{}, resumeErr
			}
			if err := s.finishUpload(ctx, operationObject, operationEnvelope, record); err != nil {
				return domain.Entry{}, err
			}
			return resumed, nil
		}
	}
	if err != nil || result.State != domain.OperationSucceeded {
		if err != nil {
			return domain.Entry{}, err
		}
		if result.State == domain.OperationFailed && result.ErrorKind == domain.ErrorPreconditionFailed {
			return s.retryUploadCompletion(ctx, scope, request, mediaType, transfers, operationObject, operationEnvelope, record, attemptsRemaining)
		}
		return domain.Entry{}, domain.NewError(domain.ErrorPreconditionFailed, "upload completion operation failed")
	}
	if err := s.finishUpload(ctx, operationObject, operationEnvelope, record); err != nil {
		return domain.Entry{}, err
	}
	return domainEntry(resolvedPath, entry), nil
}

func (s *FileStore) retryUploadCompletion(
	ctx context.Context,
	scope domain.Scope,
	request domain.CompleteUploadRequest,
	mediaType string,
	transfers objectstore.DirectTransferBackend,
	recordObject objectstore.Object,
	recordEnvelope storageformat.Envelope,
	record storageformat.UploadRecord,
	attemptsRemaining int,
) (domain.Entry, error) {
	if attemptsRemaining <= 1 {
		return domain.Entry{}, domain.NewError(domain.ErrorUnavailable, "upload completion remained contended")
	}
	if err := s.rotateUploadCompletionOperation(ctx, recordObject, recordEnvelope, record); err != nil && !errors.Is(err, domain.ErrPreconditionFailed) && !errors.Is(err, domain.ErrConflict) {
		return domain.Entry{}, err
	}
	return s.completeUpload(ctx, scope, request, mediaType, transfers, attemptsRemaining-1)
}

func (s *FileStore) rotateUploadCompletionOperation(ctx context.Context, object objectstore.Object, envelope storageformat.Envelope, record storageformat.UploadRecord) error {
	operationID, err := s.engine.ids.OpaqueID()
	if err != nil {
		return err
	}
	record.CompletionOperationID = operationID
	body, err := storageformat.EncodeEnvelope(uploadRecordSchema, object.Key, envelope.Revision+1, record)
	if err != nil {
		return err
	}
	intent := storageformat.MutationIntent{
		Action: storageformat.MutationCAS, TargetKey: object.Key.String(),
		ExpectedLogicalVersion: envelope.LogicalVersion, TargetBody: body,
	}
	return s.engine.withAdmission(ctx, intent, func() error {
		_, putErr := s.engine.backend.Put(ctx, object.Key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version})
		return putErr
	})
}

func matchesUploadEntry(record storageformat.UploadRecord, entry storageformat.DirectoryEntry) bool {
	return entry.Kind == domain.EntryFile && entry.BlobID == record.UploadID && entry.Size == record.Size && entry.MediaType == record.MediaType
}

func (s *FileStore) resumeUploadCompletion(ctx context.Context, scope domain.Scope, record storageformat.UploadRecord) (domain.Entry, bool, error) {
	key := storageformat.OperationKey(record.UserID, record.CompletionOperationID)
	if _, err := s.engine.backend.Get(ctx, key); errors.Is(err, domain.ErrNotFound) {
		return domain.Entry{}, false, nil
	} else if err != nil {
		return domain.Entry{}, true, err
	}
	var executionErr error
	for range maxInternalUploadCompletionAttempts {
		executionErr = s.executeFileOperation(ctx, key)
		if executionErr == nil || !errors.Is(executionErr, domain.ErrPreconditionFailed) && !errors.Is(executionErr, domain.ErrConflict) {
			break
		}
	}
	if executionErr != nil {
		if errors.Is(executionErr, domain.ErrPreconditionFailed) || errors.Is(executionErr, domain.ErrConflict) {
			return domain.Entry{}, true, domain.NewError(domain.ErrorUnavailable, "upload completion changed concurrently")
		}
		return domain.Entry{}, true, executionErr
	}
	operation, err := s.readFileOperation(ctx, scope.UserID(), record.CompletionOperationID)
	if err != nil {
		return domain.Entry{}, true, err
	}
	if operation.State == storageformat.FileOperationFailed {
		return domain.Entry{}, true, errUploadCompletionNeedsRetry
	}
	path, err := domain.ParseUserPath(record.ResolvedPath)
	if err != nil {
		return domain.Entry{}, true, domain.NewError(domain.ErrorInvalid, "stored upload destination is invalid")
	}
	entry, err := s.resolveEntry(ctx, scope, path)
	if err != nil {
		return domain.Entry{}, true, err
	}
	if !matchesUploadEntry(record, entry) {
		return domain.Entry{}, true, domain.NewError(domain.ErrorPreconditionFailed, "completed upload destination changed")
	}
	return domainEntry(path, entry), true, nil
}

func (s *FileStore) finishUpload(ctx context.Context, object objectstore.Object, envelope storageformat.Envelope, record storageformat.UploadRecord) error {
	userID, err := domain.ParseUserID(record.UserID)
	if err != nil {
		return domain.NewError(domain.ErrorInvalid, "stored upload user is invalid")
	}
	for range maxInternalUploadCompletionAttempts {
		record.State = storageformat.UploadCompleted
		body, encodeErr := storageformat.EncodeEnvelope(uploadRecordSchema, object.Key, envelope.Revision+1, record)
		if encodeErr != nil {
			return encodeErr
		}
		intent := storageformat.MutationIntent{
			Action: storageformat.MutationCAS, TargetKey: object.Key.String(), ExpectedLogicalVersion: envelope.LogicalVersion,
			TargetBody: body,
		}
		if record.SchemaVersion == 1 {
			intent.AbortUploads = []string{record.UploadID}
		} else {
			intent.CompleteUploads = []string{record.UploadID}
		}
		err = s.engine.withAdmission(ctx, intent, func() error {
			if len(intent.AbortUploads) != 0 {
				if abortErr := s.engine.ensureUploadAborts(ctx, intent.AbortUploads); abortErr != nil {
					return abortErr
				}
			}
			if _, putErr := s.engine.backend.Put(ctx, object.Key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); putErr != nil {
				return putErr
			}
			return s.engine.ensureUploadCompletions(ctx, intent.CompleteUploads)
		})
		if err == nil {
			return nil
		}
		if !errors.Is(err, domain.ErrPreconditionFailed) && !errors.Is(err, domain.ErrConflict) {
			return err
		}
		object, envelope, record, err = s.readUploadRecord(ctx, userID, record.UploadID)
		if err != nil {
			return err
		}
		if record.State == storageformat.UploadCompleted {
			return nil
		}
		if record.State != storageformat.UploadActive {
			return domain.NewError(domain.ErrorPreconditionFailed, "upload state changed during completion")
		}
	}
	return domain.NewError(domain.ErrorUnavailable, "upload finalization remained contended")
}

func (s *FileStore) readUploadRecord(ctx context.Context, userID domain.UserID, uploadID string) (objectstore.Object, storageformat.Envelope, storageformat.UploadRecord, error) {
	key := storageformat.OperationKey(userID.String(), uploadID)
	object, err := s.engine.backend.Get(ctx, key)
	if err != nil {
		return objectstore.Object{}, storageformat.Envelope{}, storageformat.UploadRecord{}, err
	}
	var envelope storageformat.Envelope
	var record storageformat.UploadRecord
	if err := storageformat.DecodeEnvelope(object.Body, key, uploadRecordSchema, &envelope, &record); err != nil {
		return objectstore.Object{}, storageformat.Envelope{}, storageformat.UploadRecord{}, err
	}
	if (record.SchemaVersion != 1 && record.SchemaVersion != 2) || record.UploadID != uploadID || record.CompletionOperationID == "" || record.UserID != userID.String() || record.Size < 0 || record.StagingKey == "" || record.BackendKind == "" || record.LeaseKey == "" || record.ExpiresAt.IsZero() || record.CreatedAt.IsZero() || (record.State != storageformat.UploadActive && record.State != storageformat.UploadCompleted && record.State != storageformat.UploadAborted) {
		return objectstore.Object{}, storageformat.Envelope{}, storageformat.UploadRecord{}, domain.NewError(domain.ErrorInvalid, "invalid stored upload record")
	}
	return object, envelope, record, nil
}

func (s *FileStore) transferBackend() (objectstore.DirectTransferBackend, error) {
	transfers, ok := s.engine.fileBackend.(objectstore.DirectTransferBackend)
	if !ok {
		return nil, domain.NewError(domain.ErrorPreconditionFailed, "object backend has no direct transfer support")
	}
	return transfers, nil
}

func (s *FileStore) readTransferLease(ctx context.Context, record storageformat.UploadRecord) (storageformat.TransferLease, objectstore.Object, error) {
	transfers, err := s.transferBackend()
	if err != nil {
		return storageformat.TransferLease{}, objectstore.Object{}, err
	}
	expected := storageformat.LeaseKey(transfers.BackendKind(), record.UploadID)
	if record.BackendKind != transfers.BackendKind() || record.LeaseKey != expected.String() {
		return storageformat.TransferLease{}, objectstore.Object{}, domain.NewError(domain.ErrorPreconditionFailed, "upload lease backend does not match")
	}
	object, err := s.engine.backend.Get(ctx, expected)
	if err != nil {
		return storageformat.TransferLease{}, objectstore.Object{}, err
	}
	var envelope storageformat.Envelope
	var lease storageformat.TransferLease
	if err := storageformat.DecodeEnvelope(object.Body, expected, transferLeaseSchema, &envelope, &lease); err != nil {
		return storageformat.TransferLease{}, objectstore.Object{}, err
	}
	if lease.SchemaVersion != 1 || lease.BackendKind != record.BackendKind || lease.UploadID != record.UploadID || lease.ExpiresAt != record.ExpiresAt || len(lease.Ciphertext) == 0 {
		return storageformat.TransferLease{}, objectstore.Object{}, domain.NewError(domain.ErrorInvalid, "invalid stored transfer lease")
	}
	return lease, object, nil
}

func copyHeaders(headers map[string]string) map[string]string {
	result := make(map[string]string, len(headers))
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result[key] = headers[key]
	}
	return result
}
