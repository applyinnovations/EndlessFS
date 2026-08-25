package storageformat

import (
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

type PortableUploadRecord struct {
	SchemaVersion   int                 `json:"schemaVersion"`
	UploadID        string              `json:"uploadID"`
	OwnerID         string              `json:"ownerID"`
	Area            string              `json:"area"`
	RequestedPath   string              `json:"requestedPath"`
	ResolvedPath    string              `json:"resolvedPath"`
	BlobID          string              `json:"blobID"`
	Size            int64               `json:"size"`
	MediaType       string              `json:"mediaType"`
	Conflict        domain.ConflictMode `json:"conflict"`
	ExpectedVersion domain.Version      `json:"expectedVersion,omitempty"`
	TargetExisted   bool                `json:"targetExisted,omitempty"`
	Resumable       bool                `json:"resumable,omitempty"`
	State           UploadState         `json:"state"`
	CleanupPending  bool                `json:"cleanupPending,omitempty"`
	CreatedAt       time.Time           `json:"createdAt"`
	ExpiresAt       time.Time           `json:"expiresAt"`
}

type PortableUploadIdempotency struct {
	SchemaVersion int    `json:"schemaVersion"`
	OwnerID       string `json:"ownerID"`
	KeyDigest     string `json:"keyDigest"`
	Fingerprint   string `json:"fingerprint"`
	UploadID      string `json:"uploadID"`
}

func ValidatePortableUploadRecord(record PortableUploadRecord) error {
	if record.SchemaVersion != 1 || !validDomainText(record.UploadID) || !validDomainText(record.OwnerID) || record.Area != "live" && record.Area != "trash" || record.RequestedPath == "" || record.ResolvedPath == "" || !validDomainText(record.BlobID) || record.Size < 0 || record.MediaType == "" || !record.Conflict.Valid() || record.State != UploadInitializing && record.State != UploadActive && record.State != UploadCompleted && record.State != UploadAborted || record.CleanupPending && record.State != UploadCompleted && record.State != UploadAborted || record.CreatedAt.IsZero() || !record.ExpiresAt.After(record.CreatedAt) {
		return domain.NewError(domain.ErrorInvalid, "invalid portable upload record")
	}
	_, err := EncodeCanonical(record)
	return err
}

func ValidatePortableUploadIdempotency(record PortableUploadIdempotency) error {
	if record.SchemaVersion != 1 || !validDomainText(record.OwnerID) || !validDomainDigest(record.KeyDigest) || !validDomainDigest(record.Fingerprint) || !validDomainText(record.UploadID) {
		return domain.NewError(domain.ErrorInvalid, "invalid portable upload idempotency record")
	}
	_, err := EncodeCanonical(record)
	return err
}
