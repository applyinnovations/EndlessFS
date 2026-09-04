package storageformat

import (
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

// PortableUploadBatchAbort is the canonical lifecycle overlay for cancelled
// members of one admitted upload batch. Admission records remain immutable
// facts; one bounded bitmap changes their effective lifecycle atomically
// without rewriting one state record per member.
type PortableUploadBatchAbort struct {
	SchemaVersion int       `json:"schemaVersion"`
	OwnerID       string    `json:"ownerID"`
	BatchID       string    `json:"batchID"`
	Count         uint64    `json:"count"`
	Aborted       []byte    `json:"aborted"`
	ModifiedAt    time.Time `json:"modifiedAt"`
}

func ValidatePortableUploadBatchAbort(value PortableUploadBatchAbort) error {
	if value.SchemaVersion != 1 || !validDomainText(value.OwnerID) || !validDomainDigest(value.BatchID) || value.Count < 1 || value.Count > MaxPortableUploadBatchItems || value.ModifiedAt.IsZero() {
		return domain.NewError(domain.ErrorInvalid, "invalid portable upload batch abort")
	}
	wantBytes := int((value.Count + 7) / 8)
	if len(value.Aborted) != wantBytes {
		return domain.NewError(domain.ErrorInvalid, "invalid portable upload batch abort bitmap")
	}
	set := false
	for _, bits := range value.Aborted {
		set = set || bits != 0
	}
	if !set {
		return domain.NewError(domain.ErrorInvalid, "empty portable upload batch abort bitmap")
	}
	if remainder := value.Count % 8; remainder != 0 && value.Aborted[len(value.Aborted)-1]&^byte((1<<remainder)-1) != 0 {
		return domain.NewError(domain.ErrorInvalid, "portable upload batch abort bitmap exceeds its member count")
	}
	_, err := EncodeCanonical(value)
	return err
}

func (value PortableUploadBatchAbort) Aborts(index uint64) bool {
	return index < value.Count && value.Aborted[index/8]&(1<<(index%8)) != 0
}
