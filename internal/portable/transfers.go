package portable

import (
	"context"
	"sort"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const uploadRecordSchema = "upload-record-v1"

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
	now := s.engine.clock.Now().UTC()
	record := storageformat.UploadRecord{
		SchemaVersion: 1, UploadID: uploadID, UserID: scope.UserID().String(), Area: areaName(scope.Area()),
		RequestedPath: request.Path.String(), ResolvedPath: resolved.String(), StagingKey: stagingKey.String(),
		Size: request.Size, MediaType: mediaType, Conflict: conflict, ExpectedVersion: request.ExpectedVersion,
		TargetExisted: existing != nil, Resumable: request.Resumable, State: storageformat.UploadActive,
		CreatedAt: now, ExpiresAt: now.Add(s.engine.uploadTTL),
	}
	body, err := storageformat.EncodeEnvelope(uploadRecordSchema, operationKey, 1, record)
	if err != nil {
		return domain.UploadCapability{}, err
	}
	intent := storageformat.MutationIntent{Action: storageformat.MutationCreate, TargetKey: operationKey.String(), TargetBody: body}
	if err := s.engine.withAdmission(ctx, intent, func() error {
		_, putErr := s.engine.backend.Put(ctx, operationKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
		return putErr
	}); err != nil {
		return domain.UploadCapability{}, err
	}
	capability, err := transfers.BeginUpload(ctx, objectstore.UploadRequest{
		UploadID: uploadID, Key: stagingKey, Size: request.Size, MediaType: mediaType,
		Resumable: request.Resumable, ExpiresAt: record.ExpiresAt,
	})
	if err != nil {
		return domain.UploadCapability{}, err
	}
	return domain.UploadCapability{
		UploadID: domain.UploadID(uploadID), Protocol: capability.Protocol, URL: capability.URL,
		Method: capability.Method, Headers: copyHeaders(capability.Headers), ExpiresAt: capability.ExpiresAt, ChunkRules: capability.ChunkRules,
		Framing: capability.Framing, DeclaredSize: capability.DeclaredSize,
	}, nil
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
	progress, err := transfers.UploadProgress(ctx, string(uploadID))
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
	if record.Area != areaName(scope.Area()) || record.State != storageformat.UploadActive {
		return domain.Entry{}, domain.NewError(domain.ErrorNotFound, "upload not found")
	}
	if record.RequestedPath != request.Path.String() || record.Size != request.Size || record.MediaType != mediaType {
		return domain.Entry{}, domain.NewError(domain.ErrorPreconditionFailed, "upload constraints do not match initiation")
	}
	if !s.engine.clock.Now().Before(record.ExpiresAt) {
		return domain.Entry{}, domain.NewError(domain.ErrorPreconditionFailed, "upload capability expired")
	}
	progress, err := transfers.UploadProgress(ctx, string(request.UploadID))
	if err != nil {
		return domain.Entry{}, err
	}
	if !progress.Complete || progress.Offset != record.Size || progress.Size != record.Size || progress.Version == "" {
		return domain.Entry{}, domain.NewError(domain.ErrorPreconditionFailed, "upload is incomplete")
	}
	if request.ChecksumSHA256 != "" && (progress.SHA256 == "" || !strings.EqualFold(request.ChecksumSHA256, progress.SHA256)) {
		return domain.Entry{}, domain.NewError(domain.ErrorPreconditionFailed, "upload checksum does not match")
	}
	resolvedPath, err := domain.ParseUserPath(record.ResolvedPath)
	if err != nil {
		return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "stored upload destination is invalid")
	}
	parentID, parent, err := s.resolveDirectory(ctx, scope, resolvedPath.Parent())
	if err != nil {
		return domain.Entry{}, err
	}
	current, exists := findDirectoryEntry(parent.entries, resolvedPath.Name())
	if record.TargetExisted {
		if !exists || domain.Version(current.LogicalVersion) != record.ExpectedVersion {
			return domain.Entry{}, domain.NewError(domain.ErrorPreconditionFailed, "upload destination changed")
		}
	} else if exists {
		return domain.Entry{}, domain.NewError(domain.ErrorConflict, "upload destination appeared during transfer")
	}
	blobID, err := s.engine.ids.OpaqueID()
	if err != nil {
		return domain.Entry{}, err
	}
	blobKey := storageformat.BlobKey(scope.UserID().String(), blobID)
	entry := storageformat.DirectoryEntry{
		Name: resolvedPath.Name(), NameDigest: storageformat.NameDigest(resolvedPath.Name()), Kind: domain.EntryFile,
		BlobID: blobID, Size: record.Size, MediaType: mediaType, SHA256: progress.SHA256, ModifiedAt: s.engine.clock.Now().UTC(),
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
	record.State = storageformat.UploadCompleted
	completedBody, encodeErr := storageformat.EncodeEnvelope(uploadRecordSchema, operationObject.Key, operationEnvelope.Revision+1, record)
	if encodeErr == nil {
		_, _ = s.engine.backend.Put(ctx, operationObject.Key, completedBody, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: operationObject.Version})
	}
	_ = transfers.AbortUpload(ctx, string(request.UploadID))
	return domainEntry(resolvedPath, entry), nil
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
	info, err := s.engine.backend.Head(ctx, blobKey)
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
	if record.SchemaVersion != 1 || record.UploadID != uploadID || record.UserID != userID.String() || record.Size < 0 || record.StagingKey == "" || record.ExpiresAt.IsZero() || record.CreatedAt.IsZero() || (record.State != storageformat.UploadActive && record.State != storageformat.UploadCompleted && record.State != storageformat.UploadAborted) {
		return objectstore.Object{}, storageformat.Envelope{}, storageformat.UploadRecord{}, domain.NewError(domain.ErrorInvalid, "invalid stored upload record")
	}
	return object, envelope, record, nil
}

func (s *FileStore) transferBackend() (objectstore.DirectTransferBackend, error) {
	transfers, ok := s.engine.backend.(objectstore.DirectTransferBackend)
	if !ok {
		return nil, domain.NewError(domain.ErrorPreconditionFailed, "object backend has no direct transfer support")
	}
	return transfers, nil
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
