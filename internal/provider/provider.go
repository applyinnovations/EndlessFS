// Package provider defines the provider-neutral file control interface.
package provider

import (
	"context"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

type Storage interface {
	List(context.Context, domain.Scope, domain.ListRequest) (domain.ListPage, error)
	Stat(context.Context, domain.Scope, domain.UserPath) (domain.Entry, error)
	CreateDirectory(context.Context, domain.Scope, domain.CreateDirectoryRequest) (domain.Entry, error)

	CreateUpload(context.Context, domain.Scope, domain.CreateUploadRequest) (domain.UploadCapability, error)
	CompleteUpload(context.Context, domain.Scope, domain.CompleteUploadRequest) (domain.Entry, error)
	AbortUpload(context.Context, domain.Scope, domain.UploadID) error
	CreateDownload(context.Context, domain.Scope, domain.CreateDownloadRequest) (domain.DownloadCapability, error)

	Copy(context.Context, domain.Scope, domain.Scope, domain.CopyRequest) (domain.Operation, error)
	Move(context.Context, domain.Scope, domain.Scope, domain.MoveRequest) (domain.Operation, error)
	Delete(context.Context, domain.Scope, domain.DeleteRequest) (domain.Operation, error)
	GetOperation(context.Context, domain.UserID, domain.OperationID) (domain.Operation, error)
}
