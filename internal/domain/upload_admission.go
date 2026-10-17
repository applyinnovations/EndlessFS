package domain

// UploadAdmissionResult binds one admission outcome to its durable client item
// key. Envelopes may be reordered or reduced without changing an accepted item.
// A capability is returned only after both intent and native lease are durable.
type UploadAdmissionResult struct {
	Index      int               `json:"index"`
	ID         string            `json:"id"`
	UploadID   UploadID          `json:"uploadID,omitempty"`
	Outcome    string            `json:"outcome"`
	Status     *UploadStatus     `json:"status,omitempty"`
	Capability *UploadCapability `json:"capability,omitempty"`
	ErrorKind  ErrorKind         `json:"errorKind,omitempty"`
	RequestID  string            `json:"requestID,omitempty"`
	Retryable  bool              `json:"retryable"`
	NextAction string            `json:"nextAction"`
}
