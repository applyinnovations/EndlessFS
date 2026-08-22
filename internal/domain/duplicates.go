package domain

type DuplicateKind string

const (
	DuplicateFile      DuplicateKind = "file"
	DuplicateDirectory DuplicateKind = "directory"
)

func (kind DuplicateKind) Valid() bool {
	return kind == DuplicateFile || kind == DuplicateDirectory
}

type DuplicateGroupRequest struct {
	Limit          int
	Cursor         string
	Kind           DuplicateKind
	IncludeIgnored bool
}

type DuplicateGroup struct {
	ID               string        `json:"id"`
	Kind             DuplicateKind `json:"kind"`
	OccurrenceCount  int64         `json:"occurrenceCount"`
	Size             int64         `json:"size"`
	FileCount        int64         `json:"fileCount"`
	ReclaimableBytes int64         `json:"reclaimableBytes"`
	Ignored          bool          `json:"ignored"`
	IgnoreRevision   uint64        `json:"ignoreRevision,omitempty"`
}

type DuplicateGroupPage struct {
	Groups     []DuplicateGroup `json:"groups"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

type DuplicateOccurrenceRequest struct {
	GroupID string
	Limit   int
	Cursor  string
}

type DuplicateOccurrence struct {
	GroupID   string        `json:"groupID"`
	Kind      DuplicateKind `json:"kind"`
	Area      Area          `json:"-"`
	AreaName  string        `json:"area"`
	Path      UserPath      `json:"path"`
	Size      int64         `json:"size"`
	FileCount int64         `json:"fileCount"`
	Version   Version       `json:"version"`
}

type DuplicateOccurrencePage struct {
	Occurrences []DuplicateOccurrence `json:"occurrences"`
	NextCursor  string                `json:"nextCursor,omitempty"`
}

type SetDuplicateIgnoredRequest struct {
	GroupID          string
	Ignored          bool
	ExpectedRevision uint64
}

type DuplicateIgnore struct {
	GroupID  string `json:"groupID"`
	Ignored  bool   `json:"ignored"`
	Revision uint64 `json:"revision"`
}

type DuplicateLocation struct {
	Area Area     `json:"area"`
	Path UserPath `json:"path"`
}

type DuplicateDirectoryComparisonRequest struct {
	Left  DuplicateLocation
	Right DuplicateLocation
}

type DuplicateDirectoryComparison struct {
	Left           DuplicateOccurrence `json:"left"`
	Right          DuplicateOccurrence `json:"right"`
	Exact          bool                `json:"exact"`
	CommonFiles    int64               `json:"commonFiles"`
	CommonBytes    int64               `json:"commonBytes"`
	LeftOnlyFiles  int64               `json:"leftOnlyFiles"`
	LeftOnlyBytes  int64               `json:"leftOnlyBytes"`
	RightOnlyFiles int64               `json:"rightOnlyFiles"`
	RightOnlyBytes int64               `json:"rightOnlyBytes"`
}

type DuplicateDirectoryOverlapRequest struct {
	Directory      DuplicateLocation
	Limit          int
	Cursor         string
	IncludeIgnored bool
}

type DuplicateDirectoryOverlapCandidate struct {
	SharedSketch             int                          `json:"sharedSketch"`
	SketchSize               int                          `json:"sketchSize"`
	Ignored                  bool                         `json:"ignored"`
	IgnoreRevision           uint64                       `json:"ignoreRevision,omitempty"`
	ExactGroupIgnored        bool                         `json:"exactGroupIgnored"`
	ExactGroupIgnoreRevision uint64                       `json:"exactGroupIgnoreRevision,omitempty"`
	Comparison               DuplicateDirectoryComparison `json:"comparison"`
}

type DuplicateDirectoryOverlapPage struct {
	Candidates []DuplicateDirectoryOverlapCandidate `json:"candidates"`
	NextCursor string                               `json:"nextCursor,omitempty"`
}

type SetDuplicateDirectoryIgnoredRequest struct {
	Left             DuplicateLocation
	Right            DuplicateLocation
	Ignored          bool
	ExpectedRevision uint64
}

type DuplicateDirectoryIgnore struct {
	Ignored  bool   `json:"ignored"`
	Revision uint64 `json:"revision"`
}

type DuplicateSide string

const (
	DuplicateSideLeft  DuplicateSide = "left"
	DuplicateSideRight DuplicateSide = "right"
)

func (side DuplicateSide) Valid() bool {
	return side == DuplicateSideLeft || side == DuplicateSideRight
}

type DuplicateReconciliationPreviewRequest struct {
	Left       DuplicateLocation
	Right      DuplicateLocation
	RemoveFrom DuplicateSide
	Limit      int
	Cursor     string
}

type DuplicateReconciliationItem struct {
	GroupID        string              `json:"groupID"`
	Remove         DuplicateOccurrence `json:"remove"`
	Keep           DuplicateOccurrence `json:"keep"`
	IgnoreRevision uint64              `json:"ignoreRevision,omitempty"`
}

type DuplicateReconciliationPreview struct {
	Comparison       DuplicateDirectoryComparison  `json:"comparison"`
	RemoveFrom       DuplicateSide                 `json:"removeFrom"`
	Items            []DuplicateReconciliationItem `json:"items"`
	ReclaimableBytes int64                         `json:"reclaimableBytes"`
	NextCursor       string                        `json:"nextCursor,omitempty"`
	PlanToken        string                        `json:"planToken,omitempty"`
}

type DuplicateReconciliationSelection struct {
	Left       DuplicateOccurrence
	Right      DuplicateOccurrence
	RemoveFrom DuplicateSide
	Items      []DuplicateReconciliationItem
}
