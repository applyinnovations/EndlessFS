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

// DuplicateStorage is the optional provider-neutral duplicate reconciliation
// control plane introduced by the duplicate-catalog storage epoch. Keeping it
// separate lets historical fixture providers remain deliberately minimal.
type DuplicateStorage interface {
	ListDuplicateGroups(context.Context, domain.UserID, domain.DuplicateGroupRequest) (domain.DuplicateGroupPage, error)
	ListDuplicateOccurrences(context.Context, domain.UserID, domain.DuplicateOccurrenceRequest) (domain.DuplicateOccurrencePage, error)
	SetDuplicateGroupIgnored(context.Context, domain.UserID, domain.SetDuplicateIgnoredRequest) (domain.DuplicateIgnore, error)
	CompareDuplicateDirectories(context.Context, domain.UserID, domain.DuplicateDirectoryComparisonRequest) (domain.DuplicateDirectoryComparison, error)
	PreviewDuplicateReconciliation(context.Context, domain.UserID, domain.DuplicateReconciliationPreviewRequest) (domain.DuplicateReconciliationPreview, error)
	ValidateDuplicateReconciliation(context.Context, domain.UserID, string) (domain.DuplicateReconciliationSelection, error)
}
