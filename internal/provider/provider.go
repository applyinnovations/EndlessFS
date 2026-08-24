// Package provider defines the provider-neutral file control interface.
package provider

import (
	"context"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

type Storage interface {
	List(context.Context, domain.Scope, domain.ListRequest) (domain.ListPage, error)
	LookupChildren(context.Context, domain.Scope, domain.ChildLookupRequest) (domain.ChildLookup, error)
	Stat(context.Context, domain.Scope, domain.UserPath) (domain.Entry, error)
	CreateDirectory(context.Context, domain.Scope, domain.CreateDirectoryRequest) (domain.Entry, error)

	CreateUpload(context.Context, domain.Scope, domain.CreateUploadRequest) (domain.UploadCapability, error)
	UploadStatus(context.Context, domain.Scope, domain.UploadID) (domain.UploadStatus, error)
	CompleteUpload(context.Context, domain.Scope, domain.CompleteUploadRequest) (domain.Entry, error)
	AbortUpload(context.Context, domain.Scope, domain.UploadID) error
	CreateDownload(context.Context, domain.Scope, domain.CreateDownloadRequest) (domain.DownloadCapability, error)

	Copy(context.Context, domain.Scope, domain.Scope, domain.CopyRequest) (domain.Operation, error)
	Move(context.Context, domain.Scope, domain.Scope, domain.MoveRequest) (domain.Operation, error)
	Delete(context.Context, domain.Scope, domain.DeleteRequest) (domain.Operation, error)
	GetOperation(context.Context, domain.UserID, domain.OperationID) (domain.Operation, error)
}

// TrashStorage keeps trash placement and original-location metadata inside the
// owner namespace authority. Implementations publish each action through one
// namespace visibility point; no separate application-state transaction is
// permitted for the trash record.
type TrashStorage interface {
	MoveToTrash(context.Context, domain.UserID, domain.TrashRequest) (domain.Operation, error)
	ListTrash(context.Context, domain.UserID, domain.TrashListRequest) (domain.TrashListPage, error)
	RestoreFromTrash(context.Context, domain.UserID, string, domain.ConflictMode, string) (domain.Operation, error)
	DeleteFromTrash(context.Context, domain.UserID, string, string) (domain.Operation, error)
}

// BatchStorage publishes a bounded selection through one owner-namespace
// visibility point. Preparation may write immutable pages proportional to the
// touched page set, but must not run one transaction protocol per item.
type BatchStorage interface {
	BatchCopyMove(context.Context, domain.UserID, []domain.CopyRequest, bool, string) (domain.NamespaceBatchResult, error)
	BatchMoveToTrash(context.Context, domain.UserID, []domain.TrashRequest, string) (domain.NamespaceBatchResult, error)
	BatchDeleteFromTrash(context.Context, domain.UserID, []string, string) (domain.NamespaceBatchResult, error)
	GetBatchOperation(context.Context, domain.UserID, domain.OperationID) (domain.Operation, error)
}

// NamespaceStorage is the complete file-control contract required by the
// application runtime. Trash placement and bounded batches are mandatory
// schema-008 namespace mutations, never optional fallbacks to per-item state
// records or repeated provider transactions.
type NamespaceStorage interface {
	Storage
	TrashStorage
	BatchStorage
}

// DuplicateStorage is the optional provider-neutral duplicate reconciliation
// control plane introduced by the duplicate-catalog storage epoch. Keeping it
// separate lets historical fixture providers remain deliberately minimal.
type DuplicateStorage interface {
	ListDuplicateGroups(context.Context, domain.UserID, domain.DuplicateGroupRequest) (domain.DuplicateGroupPage, error)
	ListDuplicateOccurrences(context.Context, domain.UserID, domain.DuplicateOccurrenceRequest) (domain.DuplicateOccurrencePage, error)
	SetDuplicateGroupIgnored(context.Context, domain.UserID, domain.SetDuplicateIgnoredRequest) (domain.DuplicateIgnore, error)
	CompareDuplicateDirectories(context.Context, domain.UserID, domain.DuplicateDirectoryComparisonRequest) (domain.DuplicateDirectoryComparison, error)
	ListDuplicateDirectoryOverlaps(context.Context, domain.UserID, domain.DuplicateDirectoryOverlapRequest) (domain.DuplicateDirectoryOverlapPage, error)
	SetDuplicateDirectoryIgnored(context.Context, domain.UserID, domain.SetDuplicateDirectoryIgnoredRequest) (domain.DuplicateDirectoryIgnore, error)
	PreviewDuplicateReconciliation(context.Context, domain.UserID, domain.DuplicateReconciliationPreviewRequest) (domain.DuplicateReconciliationPreview, error)
	ValidateDuplicateReconciliation(context.Context, domain.UserID, string) (domain.DuplicateReconciliationSelection, error)
}
