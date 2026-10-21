package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

// A terminal action's correlated outcome is the acknowledgement. Its optional
// entry/path details can expand beyond the response envelope even though every
// requested ID fits. Keep all acknowledgements and progress when that happens;
// callers can obtain full details through the individual status/file APIs.
// This representation is used only for independent terminal actions, never to
// discard a capability needed by admission or resume.
type compactUploadActionStatus struct {
	UploadID        domain.UploadID       `json:"uploadID"`
	State           domain.UploadState    `json:"state"`
	Protocol        domain.UploadProtocol `json:"protocol"`
	ConfirmedOffset int64                 `json:"confirmedOffset"`
	DataComplete    bool                  `json:"dataComplete"`
	DeclaredSize    int64                 `json:"declaredSize"`
	ExpiresAt       time.Time             `json:"expiresAt"`
}

type compactUploadActionResult struct {
	Index          int                        `json:"index"`
	UploadID       domain.UploadID            `json:"uploadID"`
	Action         string                     `json:"action"`
	Outcome        string                     `json:"outcome"`
	Status         *compactUploadActionStatus `json:"status,omitempty"`
	ErrorKind      domain.ErrorKind           `json:"errorKind,omitempty"`
	RequestID      string                     `json:"requestID,omitempty"`
	Retryable      bool                       `json:"retryable"`
	NextAction     string                     `json:"nextAction"`
	DetailsOmitted bool                       `json:"detailsOmitted"`
}

func writeUploadActionPage(w http.ResponseWriter, r *http.Request, requested []domain.UploadActionResult) {
	w.Header().Set("Cache-Control", "no-store")
	items := append([]domain.UploadActionResult(nil), requested...)
	for index := range items {
		// Unknown actions remain independently rejected, but arbitrary input
		// is not an unbounded acknowledgement field. Index/ID correlate it.
		if items[index].Action != "complete" && items[index].Action != "cancel" {
			items[index].Action = "invalid"
		}
	}
	body, err := json.Marshal(map[string]any{"uploads": items})
	if err != nil {
		writeProblem(w, r, domain.NewError(domain.ErrorUnavailable, "upload action response cannot be encoded"))
		return
	}
	if len(body) > maxControlBodyBytes {
		compact := make([]compactUploadActionResult, len(items))
		for index, item := range items {
			compact[index] = compactUploadActionResult{Index: item.Index, UploadID: item.UploadID, Action: item.Action, Outcome: item.Outcome, ErrorKind: item.ErrorKind, RequestID: item.RequestID, Retryable: item.Retryable, NextAction: item.NextAction, DetailsOmitted: true}
			if status := item.Status; status != nil {
				compact[index].Status = &compactUploadActionStatus{UploadID: status.UploadID, State: status.State, Protocol: status.Protocol, ConfirmedOffset: status.ConfirmedOffset, DataComplete: status.DataComplete, DeclaredSize: status.DeclaredSize, ExpiresAt: status.ExpiresAt}
			}
		}
		body, err = json.Marshal(map[string]any{"uploads": compact})
		if err != nil || len(body) > maxControlBodyBytes {
			writeProblem(w, r, domain.NewError(domain.ErrorUnavailable, "upload action acknowledgements exceed their safe bound"))
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
