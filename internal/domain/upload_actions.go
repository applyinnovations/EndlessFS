package domain

// UploadActionItem selects a naturally idempotent terminal transition. UploadID
// is the immutable attempt identity; successful completion binds its checksum.
// Admission retains its separate stable-key, full-membership batch contract.
type UploadActionItem struct {
	UploadID UploadID `json:"uploadID"`
	Action   string   `json:"action"`
	CRC32C   string   `json:"crc32c,omitempty"`
}

type UploadActionResult struct {
	Index      int           `json:"index"`
	UploadID   UploadID      `json:"uploadID"`
	Action     string        `json:"action"`
	Outcome    string        `json:"outcome"`
	Status     *UploadStatus `json:"status,omitempty"`
	Entry      *Entry        `json:"entry,omitempty"`
	ErrorKind  ErrorKind     `json:"errorKind,omitempty"`
	RequestID  string        `json:"requestID,omitempty"`
	Retryable  bool          `json:"retryable"`
	NextAction string        `json:"nextAction"`
}
