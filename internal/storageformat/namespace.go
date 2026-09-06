package storageformat

import (
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

// NamespaceEntry is one immutable namespace occurrence. Directory children
// and secondary listing orders are persistent trees; copying a directory
// shares those roots until a later path-local mutation creates new roots.
type NamespaceEntry struct {
	SchemaVersion int    `json:"schemaVersion"`
	NodeID        string `json:"nodeID"`
	// OccurrenceContextID distinguishes structurally shared descendants after
	// an O(1) copy. It is set only on the copied/moved subtree root and is
	// inherited logically by descendants, so identifying one occurrence never
	// requires rewriting the shared subtree.
	OccurrenceContextID string                  `json:"occurrenceContextID,omitempty"`
	Entry               DirectoryEntry          `json:"entry"`
	Children            DomainTreeRoot          `json:"children"`
	EntryCount          uint64                  `json:"entryCount,omitempty"`
	ContentAccumulator  string                  `json:"contentAccumulator,omitempty"`
	Trash               *NamespaceTrashMetadata `json:"trash,omitempty"`
}

type NamespaceTrashMetadata struct {
	OriginalPath    string         `json:"originalPath"`
	OriginalVersion domain.Version `json:"originalVersion"`
	TrashedAt       time.Time      `json:"trashedAt"`
}

type NamespaceMutationResult struct {
	SchemaVersion      int                            `json:"schemaVersion"`
	RequestFingerprint string                         `json:"requestFingerprint"`
	Operation          *domain.Operation              `json:"operation,omitempty"`
	Entry              *DirectoryEntry                `json:"entry,omitempty"`
	Batch              *NamespaceBatch                `json:"batch,omitempty"`
	Upload             *NamespaceUploadMutationResult `json:"upload,omitempty"`
	UploadBatch        *NamespaceUploadBatchResult    `json:"uploadBatch,omitempty"`
}

type NamespaceUploadMutationResult struct {
	UploadID string `json:"uploadID"`
	State    string `json:"state"`
}

type NamespaceUploadBatchResult struct {
	TransactionID string `json:"transactionID"`
	ItemCount     uint64 `json:"itemCount"`
	State         string `json:"state"`
}

type NamespaceBatch struct {
	Operation domain.Operation `json:"operation"`
	Items     DomainTreeRoot   `json:"items"`
	ItemCount uint64           `json:"itemCount"`
}

type NamespaceBatchItem struct {
	Index       uint64                `json:"index"`
	Source      string                `json:"source"`
	Destination string                `json:"destination,omitempty"`
	TrashID     string                `json:"trashID,omitempty"`
	OperationID domain.OperationID    `json:"operationID"`
	State       domain.OperationState `json:"state"`
}

// MaxNamespaceBatchItems is a durable schema bound. It prevents a corrupt
// outcome from turning its uint64 item count into an overflowing allocation
// or unbounded tree walk when decoded by an application replica.
const MaxNamespaceBatchItems = 10_000

func ValidateNamespaceEntry(entry NamespaceEntry) error {
	if entry.SchemaVersion != 1 || !validDomainText(entry.NodeID) || entry.OccurrenceContextID != "" && !validDomainDigest(entry.OccurrenceContextID) || entry.Entry.LogicalVersion == "" || entry.Entry.Size < 0 || entry.Entry.FileCount < 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid namespace entry")
	}
	if err := validateDomainTreeRoot(entry.Children); err != nil {
		return err
	}
	if entry.Trash != nil {
		path, err := domain.ParseUserPath(entry.Trash.OriginalPath)
		if err != nil || !path.Valid() || path.IsRoot() || entry.Trash.OriginalVersion == "" || entry.Trash.TrashedAt.IsZero() {
			return domain.NewError(domain.ErrorInvalid, "invalid namespace trash metadata")
		}
	}
	switch entry.Entry.Kind {
	case domain.EntryFile:
		if entry.OccurrenceContextID != "" || entry.Entry.Name == "" || entry.Entry.BlobID == "" || entry.Entry.DirectoryID != "" || entry.Entry.FileCount != 0 || entry.EntryCount != 0 || entry.ContentAccumulator != "" || entry.Children.Digest != "" {
			return domain.NewError(domain.ErrorInvalid, "invalid namespace file")
		}
	case domain.EntryDirectory:
		if entry.Entry.BlobID != "" || entry.Entry.DirectoryID != entry.NodeID || entry.EntryCount != entry.Children.EntryCount || entry.ContentAccumulator == "" || entry.Entry.ContentDigest == "" {
			return domain.NewError(domain.ErrorInvalid, "invalid namespace directory")
		}
	default:
		return domain.NewError(domain.ErrorInvalid, "invalid namespace kind")
	}
	_, err := EncodeCanonical(entry)
	return err
}

func ValidateNamespaceMutationResult(result NamespaceMutationResult) error {
	kinds := 0
	if result.Operation != nil {
		kinds++
	}
	if result.Entry != nil {
		kinds++
	}
	if result.Batch != nil {
		kinds++
	}
	if result.Upload != nil {
		kinds++
	}
	if result.UploadBatch != nil {
		kinds++
	}
	if result.SchemaVersion != 1 || !validDomainDigest(result.RequestFingerprint) || kinds != 1 {
		return domain.NewError(domain.ErrorInvalid, "invalid namespace mutation result")
	}
	if result.Batch != nil {
		if err := ValidateNamespaceBatch(*result.Batch); err != nil {
			return err
		}
	}
	if result.Upload != nil && (!validDomainText(result.Upload.UploadID) || result.Upload.State != "created" && result.Upload.State != "initializing" && result.Upload.State != "active" && result.Upload.State != "completed" && result.Upload.State != "aborted") {
		return domain.NewError(domain.ErrorInvalid, "invalid namespace upload result")
	}
	if result.UploadBatch != nil && (!validDomainDigest(result.UploadBatch.TransactionID) || result.UploadBatch.ItemCount < 1 || result.UploadBatch.ItemCount > MaxPortableUploadBatchItems || result.UploadBatch.State != "completed" && result.UploadBatch.State != "aborted") {
		return domain.NewError(domain.ErrorInvalid, "invalid namespace upload batch result")
	}
	_, err := EncodeCanonical(result)
	return err
}

func ValidateNamespaceBatch(batch NamespaceBatch) error {
	if batch.Operation.ID == "" || batch.Operation.State != domain.OperationSucceeded || batch.ItemCount == 0 || batch.ItemCount > MaxNamespaceBatchItems || batch.Items.EntryCount != batch.ItemCount {
		return domain.NewError(domain.ErrorInvalid, "invalid namespace batch result")
	}
	return validateDomainTreeRoot(batch.Items)
}

func ValidateNamespaceBatchItem(item NamespaceBatchItem) error {
	if item.Source == "" || item.OperationID == "" || item.State != domain.OperationSucceeded {
		return domain.NewError(domain.ErrorInvalid, "invalid namespace batch item")
	}
	if _, err := domain.ParseUserPath(item.Source); err != nil {
		return domain.NewError(domain.ErrorInvalid, "invalid namespace batch source")
	}
	if item.Destination != "" {
		if _, err := domain.ParseUserPath(item.Destination); err != nil {
			return domain.NewError(domain.ErrorInvalid, "invalid namespace batch destination")
		}
	}
	_, err := EncodeCanonical(item)
	return err
}
