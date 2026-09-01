package domain

import (
	"mime"
	"strings"
	"time"
)

type EntryKind string

const (
	EntryFile      EntryKind = "file"
	EntryDirectory EntryKind = "directory"
)

type Version string
type UploadID string
type OperationID string
type ContentID string
type ContentVersion string

// PreviewContentIdentity is trusted provider metadata. It is deliberately
// absent from every public JSON representation and remains stable when a file
// is renamed, moved, trashed, or restored.
type PreviewContentIdentity struct {
	ContentID         ContentID
	ContentVersion    ContentVersion
	ContentModifiedAt time.Time
}

type Entry struct {
	Path       UserPath  `json:"path"`
	Name       string    `json:"name"`
	Kind       EntryKind `json:"kind"`
	Size       int64     `json:"size"`
	FileCount  int64     `json:"fileCount"`
	MediaType  string    `json:"mediaType,omitempty"`
	ModifiedAt time.Time `json:"modifiedAt"`
	Version    Version   `json:"version"`

	ContentID         ContentID      `json:"-"`
	ContentVersion    ContentVersion `json:"-"`
	ContentModifiedAt time.Time      `json:"-"`
}

func (e Entry) PreviewContentIdentity() PreviewContentIdentity {
	return PreviewContentIdentity{
		ContentID: e.ContentID, ContentVersion: e.ContentVersion, ContentModifiedAt: e.ContentModifiedAt,
	}
}

type ConflictMode string

const (
	ConflictFail    ConflictMode = "fail"
	ConflictReplace ConflictMode = "replace"
	ConflictRename  ConflictMode = "rename"
)

func (m ConflictMode) Valid() bool {
	return m == ConflictFail || m == ConflictReplace || m == ConflictRename
}

func NormalizeConflictMode(mode ConflictMode) (ConflictMode, error) {
	if mode == "" {
		return ConflictFail, nil
	}
	if !mode.Valid() {
		return "", NewError(ErrorInvalid, "invalid conflict mode")
	}
	return mode, nil
}

type SortField string

const (
	SortName     SortField = "name"
	SortModified SortField = "modified"
	SortSize     SortField = "size"
	SortKind     SortField = "kind"
)

type ListRequest struct {
	Directory  UserPath
	PageSize   int
	Cursor     string
	Sort       SortField
	Descending bool
}

type ListPage struct {
	Current    Entry   `json:"current"`
	Entries    []Entry `json:"entries"`
	NextCursor string  `json:"nextCursor,omitempty"`
}

// ChildLookupRequest resolves a bounded set of immediate children from one
// authoritative directory snapshot. Names are returned in request order.
type ChildLookupRequest struct {
	Directory UserPath
	Names     []string
}

type ChildLookup struct {
	Current Entry
	Entries []Entry
}

type CreateDirectoryRequest struct {
	Path            UserPath
	Conflict        ConflictMode
	ExpectedVersion Version
}

type UploadProtocol string

const (
	UploadSingle    UploadProtocol = "single"
	UploadResumable UploadProtocol = "resumable"
)

type UploadFraming string

const (
	UploadFramingWholeObject  UploadFraming = "whole-object"
	UploadFramingOffsetHeader UploadFraming = "offset-header"
	UploadFramingContentRange UploadFraming = "content-range"
)

type ChunkRules struct {
	MinimumSize int64 `json:"minimumSize"`
	MaximumSize int64 `json:"maximumSize"`
	Multiple    int64 `json:"multiple"`
}

type UploadCapability struct {
	UploadID     UploadID          `json:"uploadID"`
	Protocol     UploadProtocol    `json:"protocol"`
	URL          string            `json:"url"`
	Method       string            `json:"method"`
	Headers      map[string]string `json:"headers"`
	ExpiresAt    time.Time         `json:"expiresAt"`
	ChunkRules   *ChunkRules       `json:"chunkRules,omitempty"`
	Framing      UploadFraming     `json:"framing"`
	DeclaredSize int64             `json:"declaredSize"`
}

// UploadState is the safe provider-independent lifecycle exposed by the
// control plane. It never contains a provider upload identifier or bearer.
type UploadState string

const (
	UploadStateActive    UploadState = "active"
	UploadStateCompleted UploadState = "completed"
	UploadStateAborted   UploadState = "aborted"
	UploadStateExpired   UploadState = "expired"
)

// UploadStatus is the safe control-plane view of provider-confirmed progress.
// It deliberately contains no capability URL or bearer material.
type UploadStatus struct {
	UploadID        UploadID       `json:"uploadID"`
	State           UploadState    `json:"state"`
	Path            UserPath       `json:"path"`
	Protocol        UploadProtocol `json:"protocol"`
	ConfirmedOffset int64          `json:"confirmedOffset"`
	DeclaredSize    int64          `json:"declaredSize"`
	ExpiresAt       time.Time      `json:"expiresAt"`
}

type DownloadCapability struct {
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt time.Time         `json:"expiresAt"`
}

type CreateUploadRequest struct {
	Path            UserPath
	Size            int64
	MediaType       string
	Conflict        ConflictMode
	ExpectedVersion Version
	Resumable       bool
	IdempotencyKey  string
}

type CompleteUploadRequest struct {
	UploadID  UploadID
	Path      UserPath
	Size      int64
	MediaType string
}

// UploadSizePlanItem is safe local-file metadata used to decide whether the
// browser needs to calculate a content fingerprint. It never carries bytes or
// a provider object identifier.
type UploadSizePlanItem struct {
	ID   string   `json:"id"`
	Path UserPath `json:"path"`
	Size int64    `json:"size"`
}

type UploadSizePlanRequest struct {
	Items []UploadSizePlanItem `json:"items"`
}

type UploadSizePlanDecision struct {
	ID                  string    `json:"id"`
	FingerprintRequired bool      `json:"fingerprintRequired"`
	TargetExists        bool      `json:"targetExists"`
	TargetKind          EntryKind `json:"targetKind,omitempty"`
	TargetSize          int64     `json:"targetSize,omitempty"`
	TargetVersion       Version   `json:"targetVersion,omitempty"`
}

type UploadSizePlan struct {
	Token string                   `json:"token"`
	Items []UploadSizePlanDecision `json:"items"`
}

type UploadFingerprintPlanItem struct {
	ID     string   `json:"id"`
	Path   UserPath `json:"path"`
	Size   int64    `json:"size"`
	MD5    string   `json:"md5"`
	CRC32C string   `json:"crc32c"`
}

type UploadFingerprintPlanRequest struct {
	Token string                      `json:"token"`
	Items []UploadFingerprintPlanItem `json:"items"`
}

type UploadPlanAction string

const (
	UploadPlanUpload UploadPlanAction = "upload"
	UploadPlanSkip   UploadPlanAction = "skip"
	UploadPlanReuse  UploadPlanAction = "reuse"
)

type UploadFingerprintPlanDecision struct {
	ID            string           `json:"id"`
	Action        UploadPlanAction `json:"action"`
	SourcePath    *UserPath        `json:"sourcePath,omitempty"`
	SourceVersion Version          `json:"sourceVersion,omitempty"`
	TargetExists  bool             `json:"targetExists"`
	TargetKind    EntryKind        `json:"targetKind,omitempty"`
	TargetVersion Version          `json:"targetVersion,omitempty"`
}

type UploadFingerprintPlan struct {
	Items []UploadFingerprintPlanDecision `json:"items"`
}

type CreateDownloadRequest struct {
	Path        UserPath
	Version     Version
	Disposition Disposition
}

type Disposition string

const (
	DispositionAttachment Disposition = "attachment"
	DispositionInline     Disposition = "inline"
)

type CopyRequest struct {
	Source         UserPath
	Destination    UserPath
	Conflict       ConflictMode
	ExpectedSource Version
	ExpectedTarget Version
	IdempotencyKey string
}

type MoveRequest = CopyRequest

type DeleteRequest struct {
	Path            UserPath
	ExpectedVersion Version
	IdempotencyKey  string
}

type TrashRequest struct {
	Path            UserPath
	ExpectedVersion Version
	TrashID         string
	IdempotencyKey  string
}

type TrashEntry struct {
	TrashID         string    `json:"trashID"`
	OwnerUserID     UserID    `json:"ownerUserID"`
	OriginalPath    UserPath  `json:"originalPath"`
	TrashedPath     UserPath  `json:"trashedPath"`
	Entry           Entry     `json:"entry"`
	TrashedAt       time.Time `json:"trashedAt"`
	OriginalVersion Version   `json:"originalVersion"`
}

type TrashListRequest struct {
	Limit  int
	Cursor string
}

type TrashListPage struct {
	Items      []TrashEntry `json:"items"`
	NextCursor string       `json:"nextCursor,omitempty"`
}

// NamespaceBatchItemResult is one item in an atomically published owner
// namespace batch. All items share the batch operation ID and visibility
// point; a returned result never represents a partially published batch.
type NamespaceBatchItemResult struct {
	Source      UserPath       `json:"source"`
	Destination UserPath       `json:"destination,omitempty"`
	TrashID     string         `json:"trashID,omitempty"`
	OperationID OperationID    `json:"operationID"`
	State       OperationState `json:"state"`
}

type NamespaceBatchResult struct {
	Operation Operation                  `json:"operation"`
	Items     []NamespaceBatchItemResult `json:"items"`
}

type OperationState string

const (
	OperationPending   OperationState = "pending"
	OperationRunning   OperationState = "running"
	OperationSucceeded OperationState = "succeeded"
	OperationFailed    OperationState = "failed"
)

type Operation struct {
	ID        OperationID    `json:"id"`
	State     OperationState `json:"state"`
	ErrorKind ErrorKind      `json:"errorKind,omitempty"`
	Error     string         `json:"error,omitempty"`
	StartedAt time.Time      `json:"startedAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
}

func NormalizeMediaType(value string) (string, error) {
	if value == "" || len(value) > 255 || strings.ContainsAny(value, "\r\n") {
		return "", NewError(ErrorInvalid, "invalid media type")
	}
	base, _, err := mime.ParseMediaType(value)
	if err != nil || !strings.Contains(base, "/") {
		return "", NewError(ErrorInvalid, "invalid media type")
	}
	return strings.ToLower(base), nil
}
