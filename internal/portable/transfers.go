package portable

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const (
	uploadRecordSchema  = "upload-record-v1"
	transferLeaseSchema = "transfer-lease-v1"
)

func (s *FileStore) CreateUpload(ctx context.Context, scope domain.Scope, request domain.CreateUploadRequest) (domain.UploadCapability, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return domain.UploadCapability{}, err
	}
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
	fingerprint := storageformat.Digest([]byte(fmt.Sprintf("upload\x00%s\x00%s\x00%d\x00%s\x00%s\x00%s\x00%t", areaName(scope.Area()), request.Path.String(), request.Size, mediaType, conflict, request.ExpectedVersion, request.Resumable)))
	if replayed, found, err := s.lookupIdempotentUpload(ctx, scope.UserID(), request.IdempotencyKey, fingerprint); found || err != nil {
		return replayed, err
	}
	transfers, err := s.transferBackend()
	if err != nil {
		return domain.UploadCapability{}, err
	}
	_, parent, err := s.resolveDirectory(ctx, scope, request.Path.Parent())
	if err != nil {
		return domain.UploadCapability{}, err
	}
	resolved, existing, err := resolveDirectoryDestination(request.Path, conflict, request.ExpectedVersion, parent.entries)
	if err != nil {
		return domain.UploadCapability{}, err
	}
	uploadID, err := s.engine.ids.OpaqueID()
	if err != nil {
		return domain.UploadCapability{}, err
	}
	operationKey := storageformat.OperationKey(scope.UserID().String(), uploadID)
	stagingKey := storageformat.StagingKey(scope.UserID().String(), uploadID, "upload")
	leaseKey := storageformat.LeaseKey(transfers.BackendKind(), uploadID)
	now := s.engine.clock.Now().UTC()
	record := storageformat.UploadRecord{
		SchemaVersion: 1, UploadID: uploadID, UserID: scope.UserID().String(), Area: areaName(scope.Area()),
		RequestedPath: request.Path.String(), ResolvedPath: resolved.String(), StagingKey: stagingKey.String(),
		BackendKind: transfers.BackendKind(), LeaseKey: leaseKey.String(),
		Size: request.Size, MediaType: mediaType, Conflict: conflict, ExpectedVersion: request.ExpectedVersion,
		TargetExisted: existing != nil, Resumable: request.Resumable, State: storageformat.UploadActive,
		CreatedAt: now, ExpiresAt: now.Add(s.engine.uploadTTL),
	}
	body, err := storageformat.EncodeEnvelope(uploadRecordSchema, operationKey, 1, record)
	if err != nil {
		return domain.UploadCapability{}, err
	}
	intent := storageformat.MutationIntent{Action: storageformat.MutationCreate, TargetKey: operationKey.String(), TargetBody: body}
	if request.IdempotencyKey != "" {
		intent.Prerequisites = []storageformat.MutationObject{{Key: operationKey.String(), Body: body}}
		idempotencyKey := storageformat.IdempotencyKey(scope.UserID().String(), request.IdempotencyKey)
		idempotencyBody, encodeErr := storageformat.EncodeEnvelope(idempotencySchema, idempotencyKey, 1, storageformat.IdempotencyRecord{
			SchemaVersion: 1, UserID: scope.UserID().String(), Kind: "upload",
			KeyDigest: storageformat.Digest([]byte(request.IdempotencyKey)), Fingerprint: fingerprint, OperationID: uploadID,
		})
		if encodeErr != nil {
			return domain.UploadCapability{}, encodeErr
		}
		intent.TargetKey = idempotencyKey.String()
		intent.TargetBody = idempotencyBody
		intent.RecoverUploadKey = operationKey.String()
	}
	var handle objectstore.UploadHandle
	var replayed domain.UploadCapability
	if err := s.engine.withAdmission(ctx, intent, func() error {
		if len(intent.Prerequisites) > 0 {
			if prerequisiteErr := s.engine.ensureMutationPrerequisites(ctx, intent.Prerequisites); prerequisiteErr != nil {
				return prerequisiteErr
			}
		} else if _, putErr := s.engine.backend.Put(ctx, operationKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); putErr != nil {
			return putErr
		}
		handle, err = transfers.BeginUpload(ctx, objectstore.UploadRequest{
			UploadID: uploadID, Key: stagingKey, Size: request.Size, MediaType: mediaType,
			Resumable: request.Resumable, ExpiresAt: record.ExpiresAt,
		})
		if err != nil {
			return err
		}
		leaseBody, encodeErr := storageformat.EncodeEnvelope(transferLeaseSchema, leaseKey, 1, storageformat.TransferLease{
			SchemaVersion: 1, BackendKind: transfers.BackendKind(), UploadID: uploadID,
			Ciphertext: append([]byte(nil), handle.Lease...), ExpiresAt: record.ExpiresAt,
		})
		if encodeErr != nil {
			return encodeErr
		}
		leaseVersion, putErr := s.engine.backend.Put(ctx, leaseKey, leaseBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
		if putErr != nil {
			return putErr
		}
		if request.IdempotencyKey == "" {
			return nil
		}
		target := objectstore.MustKey(intent.TargetKey)
		if _, putErr := s.engine.backend.Put(ctx, target, intent.TargetBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); putErr != nil {
			if !errors.Is(putErr, domain.ErrConflict) {
				return putErr
			}
			_ = transfers.AbortUpload(ctx, handle.Lease)
			_ = s.engine.backend.Delete(ctx, leaseKey, objectstore.DeleteCondition{Version: leaseVersion})
			s.markUploadAborted(ctx, operationKey)
			existing, found, lookupErr := s.lookupIdempotentUpload(ctx, scope.UserID(), request.IdempotencyKey, fingerprint)
			if found && lookupErr == nil {
				replayed = existing
				return nil
			}
			if lookupErr != nil {
				return lookupErr
			}
			return domain.NewError(domain.ErrorConflict, "idempotent upload winner is unavailable")
		}
		return nil
	}); err != nil {
		return domain.UploadCapability{}, err
	}
	if replayed.UploadID != "" {
		return replayed, nil
	}
	capability := handle.Capability
	return domainUploadCapability(uploadID, capability), nil
}

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
	if record.SchemaVersion != 1 || record.State != storageformat.UploadActive || record.UploadID == "" || record.StagingKey == "" {
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

func (s *FileStore) UploadStatus(ctx context.Context, scope domain.Scope, uploadID domain.UploadID) (domain.UploadStatus, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return domain.UploadStatus{}, err
	}
	if uploadID == "" {
		return domain.UploadStatus{}, domain.NewError(domain.ErrorInvalid, "upload ID is required")
	}
	_, _, record, err := s.readUploadRecord(ctx, scope.UserID(), string(uploadID))
	if err != nil || record.Area != areaName(scope.Area()) || record.State != storageformat.UploadActive {
		if err != nil {
			return domain.UploadStatus{}, err
		}
		return domain.UploadStatus{}, domain.NewError(domain.ErrorNotFound, "upload not found")
	}
	transfers, err := s.transferBackend()
	if err != nil {
		return domain.UploadStatus{}, err
	}
	lease, _, err := s.readTransferLease(ctx, record)
	if err != nil {
		return domain.UploadStatus{}, err
	}
	progress, err := transfers.UploadProgress(ctx, lease.Ciphertext)
	if err != nil {
		return domain.UploadStatus{}, err
	}
	path, err := domain.ParseUserPath(record.RequestedPath)
	if err != nil {
		return domain.UploadStatus{}, domain.NewError(domain.ErrorInvalid, "stored upload path is invalid")
	}
	protocol := domain.UploadSingle
	if record.Resumable {
		protocol = domain.UploadResumable
	}
	return domain.UploadStatus{UploadID: uploadID, Path: path, Protocol: protocol, ConfirmedOffset: progress.Offset, DeclaredSize: record.Size, ExpiresAt: record.ExpiresAt}, nil
}

func (s *FileStore) CompleteUpload(ctx context.Context, scope domain.Scope, request domain.CompleteUploadRequest) (domain.Entry, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return domain.Entry{}, err
	}
	if request.UploadID == "" || !request.Path.Valid() || request.Size < 0 {
		return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "invalid upload completion")
	}
	mediaType, err := domain.NormalizeMediaType(request.MediaType)
	if err != nil {
		return domain.Entry{}, err
	}
	transfers, err := s.transferBackend()
	if err != nil {
		return domain.Entry{}, err
	}
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
	parentID, parent, err := s.resolveDirectory(ctx, scope, resolvedPath.Parent())
	if err != nil {
		return domain.Entry{}, err
	}
	if parent.pending {
		return domain.Entry{}, domain.NewError(domain.ErrorUnavailable, "upload destination has a pending operation")
	}
	current, exists := findDirectoryEntry(parent.entries, resolvedPath.Name())
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
	if request.ChecksumSHA256 != "" && (progress.SHA256 == "" || !strings.EqualFold(request.ChecksumSHA256, progress.SHA256)) {
		return domain.Entry{}, domain.NewError(domain.ErrorPreconditionFailed, "upload checksum does not match")
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
	contentID := ""
	if exists {
		contentID = current.ContentID
	}
	if contentID == "" {
		contentID, err = s.engine.ids.OpaqueID()
		if err != nil {
			return domain.Entry{}, err
		}
	}
	contentVersion, err := s.engine.ids.OpaqueID()
	if err != nil {
		return domain.Entry{}, err
	}
	contentModifiedAt := s.engine.clock.Now().UTC()
	entry := storageformat.DirectoryEntry{
		Name: resolvedPath.Name(), NameDigest: storageformat.NameDigest(resolvedPath.Name()), Kind: domain.EntryFile,
		BlobID: blobID, Size: record.Size, MediaType: mediaType, SHA256: progress.SHA256, ModifiedAt: record.CreatedAt,
		ContentID: contentID, ContentVersion: contentVersion, ContentModifiedAt: contentModifiedAt,
	}
	entry.LogicalVersion, err = directoryEntryVersion(entry)
	if err != nil {
		return domain.Entry{}, err
	}
	var existingPointer *storageformat.DirectoryEntry
	if exists {
		existingPointer = &current
	}
	updated := replaceDirectoryEntry(parent.entries, existingPointer, entry)
	parentRevision := uint64(1)
	if parent.exists {
		parentRevision = parent.envelope.Revision + 1
	}
	prepared, err := s.prepareDirectory(ctx, scope, parentID, updated, parentRevision)
	if err != nil {
		return domain.Entry{}, err
	}
	stagingKey, err := objectstore.ParseKey(record.StagingKey)
	if err != nil {
		return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "stored staging key is invalid")
	}
	copyIntent := storageformat.MutationCopy{SourceKey: stagingKey.String(), DestinationKey: blobKey.String(), Size: record.Size, SHA256: progress.SHA256}
	parentKey := storageformat.DirectoryRootKey(scope.UserID().String(), areaName(scope.Area()), parentID)
	action := storageformat.MutationCreate
	expected := ""
	condition := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
	if parent.exists {
		action = storageformat.MutationCAS
		expected = parent.envelope.LogicalVersion
		condition = objectstore.PutCondition{Mode: objectstore.PutMatch, Version: parent.object.Version}
	}
	intent := storageformat.MutationIntent{Action: action, TargetKey: parentKey.String(), ExpectedLogicalVersion: expected, TargetBody: prepared.rootBody, Prerequisites: prepared.prerequisites, Copies: []storageformat.MutationCopy{copyIntent}}
	err = s.engine.withAdmission(ctx, intent, func() error {
		if err := s.engine.ensureMutationPrerequisites(ctx, prepared.prerequisites); err != nil {
			return err
		}
		if err := s.engine.ensureMutationCopies(ctx, intent.Copies); err != nil {
			return err
		}
		_, putErr := s.engine.backend.Put(ctx, parentKey, prepared.rootBody, condition)
		return putErr
	})
	if err != nil {
		return domain.Entry{}, err
	}
	if err := s.finishUpload(ctx, operationObject, operationEnvelope, record); err != nil {
		return domain.Entry{}, err
	}
	return domainEntry(resolvedPath, entry), nil
}

func matchesUploadEntry(record storageformat.UploadRecord, entry storageformat.DirectoryEntry) bool {
	return entry.Kind == domain.EntryFile && entry.BlobID == record.UploadID && entry.Size == record.Size && entry.MediaType == record.MediaType
}

func (s *FileStore) finishUpload(ctx context.Context, object objectstore.Object, envelope storageformat.Envelope, record storageformat.UploadRecord) error {
	record.State = storageformat.UploadCompleted
	body, err := storageformat.EncodeEnvelope(uploadRecordSchema, object.Key, envelope.Revision+1, record)
	if err != nil {
		return err
	}
	intent := storageformat.MutationIntent{
		Action: storageformat.MutationCAS, TargetKey: object.Key.String(), ExpectedLogicalVersion: envelope.LogicalVersion,
		TargetBody: body, AbortUploads: []string{record.UploadID},
	}
	return s.engine.withAdmission(ctx, intent, func() error {
		if err := s.engine.ensureUploadAborts(ctx, intent.AbortUploads); err != nil {
			return err
		}
		_, err := s.engine.backend.Put(ctx, object.Key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version})
		return err
	})
}

func (s *FileStore) AbortUpload(ctx context.Context, scope domain.Scope, uploadID domain.UploadID) error {
	if err := validateFileRequest(ctx, scope); err != nil {
		return err
	}
	if uploadID == "" {
		return domain.NewError(domain.ErrorInvalid, "upload ID is required")
	}
	object, envelope, record, err := s.readUploadRecord(ctx, scope.UserID(), string(uploadID))
	if err != nil {
		return err
	}
	if record.Area != areaName(scope.Area()) || record.State != storageformat.UploadActive {
		return domain.NewError(domain.ErrorNotFound, "upload not found")
	}
	record.State = storageformat.UploadAborted
	body, err := storageformat.EncodeEnvelope(uploadRecordSchema, object.Key, envelope.Revision+1, record)
	if err != nil {
		return err
	}
	intent := storageformat.MutationIntent{
		Action: storageformat.MutationCAS, TargetKey: object.Key.String(), ExpectedLogicalVersion: envelope.LogicalVersion,
		TargetBody: body, AbortUploads: []string{string(uploadID)},
	}
	return s.engine.withAdmission(ctx, intent, func() error {
		if err := s.engine.ensureUploadAborts(ctx, intent.AbortUploads); err != nil {
			return err
		}
		_, putErr := s.engine.backend.Put(ctx, object.Key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version})
		return putErr
	})
}

func (s *FileStore) CreateDownload(ctx context.Context, scope domain.Scope, request domain.CreateDownloadRequest) (domain.DownloadCapability, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return domain.DownloadCapability{}, err
	}
	if !request.Path.Valid() || request.Path.IsRoot() {
		return domain.DownloadCapability{}, domain.NewError(domain.ErrorInvalid, "download path is invalid")
	}
	if request.Disposition == "" {
		request.Disposition = domain.DispositionAttachment
	}
	if request.Disposition != domain.DispositionAttachment && request.Disposition != domain.DispositionInline {
		return domain.DownloadCapability{}, domain.NewError(domain.ErrorInvalid, "invalid download disposition")
	}
	entry, err := s.resolveEntry(ctx, scope, request.Path)
	if err != nil {
		return domain.DownloadCapability{}, err
	}
	if entry.Kind != domain.EntryFile || entry.BlobID == "" {
		return domain.DownloadCapability{}, domain.NewError(domain.ErrorNotFound, "file not found")
	}
	if request.Version == "" || request.Version != domain.Version(entry.LogicalVersion) {
		return domain.DownloadCapability{}, domain.NewError(domain.ErrorPreconditionFailed, "download version does not match")
	}
	transfers, err := s.transferBackend()
	if err != nil {
		return domain.DownloadCapability{}, err
	}
	blobKey := storageformat.BlobKey(scope.UserID().String(), entry.BlobID)
	info, err := s.engine.fileBackend.Head(ctx, blobKey)
	if err != nil {
		return domain.DownloadCapability{}, err
	}
	capability, err := transfers.CreateDownload(ctx, objectstore.DownloadRequest{
		Key: blobKey, Version: info.Version, Filename: entry.Name, MediaType: entry.MediaType,
		Disposition: request.Disposition, ExpiresAt: s.engine.clock.Now().UTC().Add(s.engine.downloadTTL),
	})
	if err != nil {
		return domain.DownloadCapability{}, err
	}
	return domain.DownloadCapability{URL: capability.URL, Method: capability.Method, Headers: copyHeaders(capability.Headers), ExpiresAt: capability.ExpiresAt}, nil
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
	if record.SchemaVersion != 1 || record.UploadID != uploadID || record.UserID != userID.String() || record.Size < 0 || record.StagingKey == "" || record.BackendKind == "" || record.LeaseKey == "" || record.ExpiresAt.IsZero() || record.CreatedAt.IsZero() || (record.State != storageformat.UploadActive && record.State != storageformat.UploadCompleted && record.State != storageformat.UploadAborted) {
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
