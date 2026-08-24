package storageformat

import (
	"github.com/applyinnovations/endlessfs/internal/domain"
)

// DuplicateProjectionSummary is rebuildable discovery data derived from one
// exact owner-namespace revision. It never authorizes deletion.
type DuplicateProjectionSummary struct {
	SchemaVersion   int                  `json:"schemaVersion"`
	GroupID         string               `json:"groupID"`
	Kind            domain.DuplicateKind `json:"kind"`
	OccurrenceCount int64                `json:"occurrenceCount"`
	Size            int64                `json:"size"`
	FileCount       int64                `json:"fileCount"`
	ContainedBy     string               `json:"containedBy,omitempty"`
}

type DuplicateProjectionOccurrence struct {
	SchemaVersion int                 `json:"schemaVersion"`
	Occurrence    DuplicateOccurrence `json:"occurrence"`
	BlobID        string              `json:"blobID,omitempty"`
}

type DuplicateDirectoryPreference struct {
	SchemaVersion int    `json:"schemaVersion"`
	PairID        string `json:"pairID"`
	LeftIdentity  string `json:"leftIdentity"`
	RightIdentity string `json:"rightIdentity"`
	Ignored       bool   `json:"ignored"`
	Revision      uint64 `json:"revision"`
}

func ValidateDuplicateProjectionSummary(summary DuplicateProjectionSummary) error {
	if summary.SchemaVersion != 1 || !validDomainDigest(summary.GroupID) || !summary.Kind.Valid() || summary.OccurrenceCount < 1 || summary.Size < 0 || summary.FileCount < 0 || summary.Kind == domain.DuplicateFile && (summary.FileCount != 1 || summary.ContainedBy != "") || summary.ContainedBy != "" && !validDomainDigest(summary.ContainedBy) {
		return domain.NewError(domain.ErrorInvalid, "invalid duplicate projection summary")
	}
	_, err := EncodeCanonical(summary)
	return err
}

func ValidateDuplicateDirectoryPreference(value DuplicateDirectoryPreference) error {
	if value.SchemaVersion != 1 || !validDomainDigest(value.PairID) || !validDomainText(value.LeftIdentity) || !validDomainText(value.RightIdentity) || value.LeftIdentity >= value.RightIdentity || value.Revision == 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid duplicate directory preference")
	}
	_, err := EncodeCanonical(value)
	return err
}

func ValidateDuplicateProjectionOccurrence(value DuplicateProjectionOccurrence) error {
	if value.SchemaVersion != 1 || !value.Occurrence.Kind.Valid() || !validDomainDigest(value.Occurrence.GroupID) || !validDomainText(value.Occurrence.Area) || !validDomainText(value.Occurrence.Path) || !validDomainText(value.Occurrence.Version) || value.Occurrence.Size < 0 || value.Occurrence.FileCount < 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid duplicate projection occurrence")
	}
	if value.Occurrence.Kind == domain.DuplicateFile {
		if !validDomainText(value.BlobID) || value.Occurrence.FileCount != 1 {
			return domain.NewError(domain.ErrorInvalid, "invalid duplicate file projection occurrence")
		}
	} else if value.BlobID != "" {
		return domain.NewError(domain.ErrorInvalid, "invalid duplicate directory projection occurrence")
	}
	_, err := EncodeCanonical(value)
	return err
}
