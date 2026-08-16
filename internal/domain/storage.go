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

type Entry struct {
	Path       UserPath  `json:"path"`
	Name       string    `json:"name"`
	Kind       EntryKind `json:"kind"`
	Size       int64     `json:"size"`
	MediaType  string    `json:"mediaType,omitempty"`
	ModifiedAt time.Time `json:"modifiedAt"`
	Version    Version   `json:"version"`
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
	Entries    []Entry `json:"entries"`
	NextCursor string  `json:"nextCursor,omitempty"`
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

type ChunkRules struct {
	MinimumSize int64 `json:"minimumSize"`
	MaximumSize int64 `json:"maximumSize"`
	Multiple    int64 `json:"multiple"`
}

type UploadCapability struct {
	UploadID   UploadID          `json:"uploadID"`
	Protocol   UploadProtocol    `json:"protocol"`
	URL        string            `json:"url"`
	Method     string            `json:"method"`
	Headers    map[string]string `json:"headers"`
	ExpiresAt  time.Time         `json:"expiresAt"`
	ChunkRules *ChunkRules       `json:"chunkRules,omitempty"`
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
}

type CompleteUploadRequest struct {
	UploadID       UploadID
	Path           UserPath
	Size           int64
	MediaType      string
	ChecksumSHA256 string
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
