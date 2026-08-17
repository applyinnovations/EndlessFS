package objectstore

import (
	"context"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

type UploadRequest struct {
	UploadID  string
	Key       Key
	Size      int64
	MediaType string
	Resumable bool
	ExpiresAt time.Time
}

type UploadCapability struct {
	Protocol     domain.UploadProtocol
	URL          string
	Method       string
	Headers      map[string]string
	ExpiresAt    time.Time
	ChunkRules   *domain.ChunkRules
	Framing      domain.UploadFraming
	DeclaredSize int64
}

type UploadProgress struct {
	Offset       int64
	Size         int64
	ExpiresAt    time.Time
	Complete     bool
	Version      NativeVersion
	SHA256       string
	Materialized bool
}

type DownloadRequest struct {
	Key         Key
	Version     NativeVersion
	Filename    string
	MediaType   string
	Disposition domain.Disposition
	ExpiresAt   time.Time
}

type DownloadCapability struct {
	URL       string
	Method    string
	Headers   map[string]string
	ExpiresAt time.Time
}

// DirectTransferBackend exposes only provider-native data-plane capabilities.
// It receives canonical object keys, never virtual paths or filesystem state.
type DirectTransferBackend interface {
	BeginUpload(context.Context, UploadRequest) (UploadCapability, error)
	UploadProgress(context.Context, string) (UploadProgress, error)
	AbortUpload(context.Context, string) error
	CreateDownload(context.Context, DownloadRequest) (DownloadCapability, error)
}
