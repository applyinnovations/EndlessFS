package storageformat

import (
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/integrity"
)

type PortableUploadRecord struct {
	SchemaVersion   int                        `json:"schemaVersion"`
	UploadID        string                     `json:"uploadID"`
	OwnerID         string                     `json:"ownerID"`
	Area            string                     `json:"area"`
	RequestedPath   string                     `json:"requestedPath"`
	ResolvedPath    string                     `json:"resolvedPath"`
	BlobID          string                     `json:"blobID"`
	Size            int64                      `json:"size"`
	MediaType       string                     `json:"mediaType"`
	Conflict        domain.ConflictMode        `json:"conflict"`
	ExpectedVersion domain.Version             `json:"expectedVersion,omitempty"`
	TargetExisted   bool                       `json:"targetExisted,omitempty"`
	Resumable       bool                       `json:"resumable,omitempty"`
	State           UploadState                `json:"state"`
	CleanupPending  bool                       `json:"cleanupPending,omitempty"`
	CreatedAt       time.Time                  `json:"createdAt"`
	ExpiresAt       time.Time                  `json:"expiresAt"`
	Batch           *PortableUploadBatchMember `json:"batch,omitempty"`
	Completion      *PortableUploadCompletion  `json:"completion,omitempty"`
}

const (
	MaxPortableUploadBatchItems = 10_000
	MaxUploadLeaseSegmentItems  = 1_000
	MaxSealedUploadLeaseBytes   = 2 << 10
)

type PortableUploadBatchMember struct {
	BatchID string `json:"batchID"`
	Index   uint64 `json:"index"`
	Count   uint64 `json:"count"`
}

type PortableUploadCompletion struct {
	MD5        string    `json:"md5"`
	CRC32C     string    `json:"crc32c"`
	ModifiedAt time.Time `json:"modifiedAt"`
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
	if record.Batch != nil && (!validDomainDigest(record.Batch.BatchID) || record.Batch.Count < 1 || record.Batch.Count > MaxPortableUploadBatchItems || record.Batch.Index >= record.Batch.Count) {
		return domain.NewError(domain.ErrorInvalid, "invalid portable upload batch binding")
	}
	if record.Completion != nil {
		if record.State != UploadCompleted || record.Completion.ModifiedAt.IsZero() {
			return domain.NewError(domain.ErrorInvalid, "invalid portable upload completion")
		}
		if _, ok := integrity.ParseMD5(record.Completion.MD5); !ok {
			return domain.NewError(domain.ErrorInvalid, "invalid portable upload completion md5")
		}
		if _, ok := integrity.ParseCRC32C(record.Completion.CRC32C); !ok {
			return domain.NewError(domain.ErrorInvalid, "invalid portable upload completion crc32c")
		}
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
