package domain

// MaxUploadLifecycleItems bounds exceptional recovery independently of the
// larger atomic admission/publication batches. File bytes never use this API.
const MaxUploadLifecycleItems = 100

// UploadLifecycleItem correlates one independently authorized recovery result.
// Status never contains a capability. Capability is returned only by explicit
// resume and must remain ephemeral in the browser.
type UploadLifecycleItem struct {
	Index      int               `json:"index"`
	UploadID   UploadID          `json:"uploadID"`
	Status     *UploadStatus     `json:"status,omitempty"`
	Capability *UploadCapability `json:"capability,omitempty"`
	ErrorKind  ErrorKind         `json:"errorKind,omitempty"`
	RequestID  string            `json:"requestID,omitempty"`
	Retryable  bool              `json:"retryable"`
	NextAction string            `json:"nextAction"`
}
