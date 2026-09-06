package portable

import (
	"context"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

// FileStore exposes only the current schema-009 runtime. The 008 suffix on
// implementation helpers records where the owner-namespace graph originated;
// schema-007/008 migration readers remain private to adjacent transformers and
// no ordinary request can select or fall back to a retired state layout.
func (s *FileStore) List(ctx context.Context, scope domain.Scope, request domain.ListRequest) (domain.ListPage, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return domain.ListPage{}, err
	}
	return newNamespaceStore(s.engine).list(ctx, scope, request)
}

func (s *FileStore) LookupChildren(ctx context.Context, scope domain.Scope, request domain.ChildLookupRequest) (domain.ChildLookup, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return domain.ChildLookup{}, err
	}
	return newNamespaceStore(s.engine).lookupChildren(ctx, scope, request)
}

func (s *FileStore) Stat(ctx context.Context, scope domain.Scope, path domain.UserPath) (domain.Entry, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return domain.Entry{}, err
	}
	if !path.Valid() {
		return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "path is required")
	}
	return newNamespaceStore(s.engine).stat(ctx, scope, path)
}

func (s *FileStore) CreateDirectory(ctx context.Context, scope domain.Scope, request domain.CreateDirectoryRequest) (domain.Entry, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return domain.Entry{}, err
	}
	return newNamespaceStore(s.engine).createDirectory(ctx, scope, request)
}

func (s *FileStore) Copy(ctx context.Context, from, to domain.Scope, request domain.CopyRequest) (domain.Operation, error) {
	if err := validateFileRequest(ctx, from); err != nil {
		return domain.Operation{}, err
	}
	if err := validateFileRequest(ctx, to); err != nil {
		return domain.Operation{}, err
	}
	return newNamespaceStore(s.engine).copyOrMove(ctx, false, from, to, request)
}

func (s *FileStore) Move(ctx context.Context, from, to domain.Scope, request domain.MoveRequest) (domain.Operation, error) {
	if err := validateFileRequest(ctx, from); err != nil {
		return domain.Operation{}, err
	}
	if err := validateFileRequest(ctx, to); err != nil {
		return domain.Operation{}, err
	}
	return newNamespaceStore(s.engine).copyOrMove(ctx, true, from, to, request)
}

func (s *FileStore) Delete(ctx context.Context, scope domain.Scope, request domain.DeleteRequest) (domain.Operation, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return domain.Operation{}, err
	}
	return newNamespaceStore(s.engine).delete(ctx, scope, request)
}

func (s *FileStore) GetOperation(ctx context.Context, userID domain.UserID, operationID domain.OperationID) (domain.Operation, error) {
	if err := ctx.Err(); err != nil {
		return domain.Operation{}, domain.WrapError(domain.ErrorUnavailable, "provider request canceled", err)
	}
	if !userID.Valid() || operationID == "" {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "user and operation IDs are required")
	}
	return newNamespaceStore(s.engine).getOperation(ctx, userID, operationID)
}

func (s *FileStore) CreateUpload(ctx context.Context, scope domain.Scope, request domain.CreateUploadRequest) (domain.UploadCapability, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return domain.UploadCapability{}, err
	}
	return s.createUpload008(ctx, scope, request)
}

func (s *FileStore) CreateUploadBatch(ctx context.Context, scope domain.Scope, requests []domain.CreateUploadRequest) ([]domain.UploadCapability, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return nil, err
	}
	return s.createUploadBatch008(ctx, scope, requests)
}

func (s *FileStore) CompleteUploadBatch(ctx context.Context, scope domain.Scope, request domain.CompleteUploadBatchRequest) (domain.CompleteUploadBatchResult, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return domain.CompleteUploadBatchResult{}, err
	}
	return s.completeUploadBatch011(ctx, scope, request)
}

func (s *FileStore) AbortUploadBatch(ctx context.Context, scope domain.Scope, request domain.AbortUploadBatchRequest) error {
	if err := validateFileRequest(ctx, scope); err != nil {
		return err
	}
	return s.abortUploadBatch011(ctx, scope, request)
}

func (s *FileStore) UploadStatus(ctx context.Context, scope domain.Scope, uploadID domain.UploadID) (domain.UploadStatus, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return domain.UploadStatus{}, err
	}
	return s.uploadStatus008(ctx, scope, uploadID)
}

func (s *FileStore) CompleteUpload(ctx context.Context, scope domain.Scope, request domain.CompleteUploadRequest) (domain.Entry, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return domain.Entry{}, err
	}
	return s.completeUpload008(ctx, scope, request)
}

func (s *FileStore) AbortUpload(ctx context.Context, scope domain.Scope, uploadID domain.UploadID) error {
	if err := validateFileRequest(ctx, scope); err != nil {
		return err
	}
	return s.abortUpload008(ctx, scope, uploadID)
}

func (s *FileStore) CreateDownload(ctx context.Context, scope domain.Scope, request domain.CreateDownloadRequest) (domain.DownloadCapability, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return domain.DownloadCapability{}, err
	}
	return s.createDownload008(ctx, scope, request)
}

func (s *FileStore) ListDuplicateGroups(ctx context.Context, userID domain.UserID, request domain.DuplicateGroupRequest) (domain.DuplicateGroupPage, error) {
	return s.listDuplicateGroups008(ctx, userID, request)
}

func (s *FileStore) PlanUploadSizes(ctx context.Context, userID domain.UserID, request domain.UploadSizePlanRequest) (domain.UploadSizePlan, error) {
	return s.planUploadSizes008(ctx, userID, request)
}

func (s *FileStore) PlanUploadFingerprints(ctx context.Context, userID domain.UserID, request domain.UploadFingerprintPlanRequest) (domain.UploadFingerprintPlan, error) {
	return s.planUploadFingerprints008(ctx, userID, request)
}

func (s *FileStore) ListDuplicateOccurrences(ctx context.Context, userID domain.UserID, request domain.DuplicateOccurrenceRequest) (domain.DuplicateOccurrencePage, error) {
	return s.listDuplicateOccurrences008(ctx, userID, request)
}

func (s *FileStore) SetDuplicateGroupIgnored(ctx context.Context, userID domain.UserID, request domain.SetDuplicateIgnoredRequest) (domain.DuplicateIgnore, error) {
	return s.setDuplicateGroupIgnored008(ctx, userID, request)
}

func (s *FileStore) SetDuplicateDirectoryIgnored(ctx context.Context, userID domain.UserID, request domain.SetDuplicateDirectoryIgnoredRequest) (domain.DuplicateDirectoryIgnore, error) {
	return s.setDuplicateDirectoryIgnored008(ctx, userID, request)
}

func (s *FileStore) CompareDuplicateDirectories(ctx context.Context, userID domain.UserID, request domain.DuplicateDirectoryComparisonRequest) (domain.DuplicateDirectoryComparison, error) {
	comparison, _, _, err := s.compareDuplicateDirectories008(ctx, userID, request)
	return comparison, err
}

func (s *FileStore) ListDuplicateDirectoryOverlaps(ctx context.Context, userID domain.UserID, request domain.DuplicateDirectoryOverlapRequest) (domain.DuplicateDirectoryOverlapPage, error) {
	return s.listDuplicateDirectoryOverlaps008(ctx, userID, request)
}

func (s *FileStore) PreviewDuplicateReconciliation(ctx context.Context, userID domain.UserID, request domain.DuplicateReconciliationPreviewRequest) (domain.DuplicateReconciliationPreview, error) {
	return s.previewDuplicateReconciliation008(ctx, userID, request)
}

func (s *FileStore) ValidateDuplicateReconciliation(ctx context.Context, userID domain.UserID, token string) (domain.DuplicateReconciliationSelection, error) {
	return s.validateDuplicateReconciliation008(ctx, userID, token)
}

func (s *FileStore) ApplyDuplicateReconciliation(ctx context.Context, userID domain.UserID, token, idempotencyKey string) (domain.NamespaceBatchResult, error) {
	return s.applyDuplicateReconciliation008(ctx, userID, token, idempotencyKey)
}
