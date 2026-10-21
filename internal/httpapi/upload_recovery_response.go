package httpapi

import (
	"bytes"
	"encoding/json"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func encodeUploadRecoveryPage(items any) ([]byte, bool, error) {
	switch values := items.(type) {
	case []domain.UploadLifecycleItem:
		return encodeBoundedUploadPage(values)
	case []domain.UploadAdmissionResult:
		return encodeBoundedUploadPage(values)
	default:
		body, err := json.Marshal(map[string]any{"uploads": items})
		return body, len(body) > maxControlBodyBytes, err
	}
}

// Marshal and validate one item at a time, retaining at most the wire budget.
// Continue validating after overflow so compaction never masks invalid detail.
// This preserves json.Marshal's exact ordinary representation, including nil.
func encodeBoundedUploadPage[T any](items []T) ([]byte, bool, error) {
	if items == nil {
		return []byte(`{"uploads":null}`), false, nil
	}
	var body bytes.Buffer
	body.WriteString(`{"uploads":[`)
	overflow := false
	for index, item := range items {
		encoded, err := json.Marshal(item)
		if err != nil {
			return nil, false, err
		}
		if body.Len()+len(encoded)+min(index, 1)+2 > maxControlBodyBytes {
			overflow = true
		}
		if !overflow {
			if index != 0 {
				body.WriteByte(',')
			}
			body.Write(encoded)
		}
	}
	if overflow {
		return nil, true, nil
	}
	body.WriteString(`]}`)
	return body.Bytes(), false, nil
}

// Long paths are display detail; an already durable admission or a usable
// capability must not become an envelope failure merely because repeating its
// path exceeds the wire bound. This explicit exceptional view keeps every
// operational status field and the complete capability. Clients needing the
// omitted path can query a smaller cohort or the scalar status endpoint.
type compactUploadRecoveryResult struct {
	Index              int                        `json:"index"`
	UploadID           domain.UploadID            `json:"uploadID"`
	Status             *compactUploadActionStatus `json:"status,omitempty"`
	Capability         *domain.UploadCapability   `json:"capability,omitempty"`
	ErrorKind          domain.ErrorKind           `json:"errorKind,omitempty"`
	RequestID          string                     `json:"requestID,omitempty"`
	Retryable          bool                       `json:"retryable"`
	NextAction         string                     `json:"nextAction"`
	DetailsOmitted     bool                       `json:"detailsOmitted"`
	CapacityDeferred   bool                       `json:"capacityDeferred,omitempty"`
	DetailsUnavailable bool                       `json:"detailsUnavailable,omitempty"`
}

type compactUploadAdmissionResult struct {
	Index              int                        `json:"index"`
	ID                 string                     `json:"id"`
	UploadID           domain.UploadID            `json:"uploadID,omitempty"`
	Outcome            string                     `json:"outcome"`
	Status             *compactUploadActionStatus `json:"status,omitempty"`
	Capability         *domain.UploadCapability   `json:"capability,omitempty"`
	ErrorKind          domain.ErrorKind           `json:"errorKind,omitempty"`
	RequestID          string                     `json:"requestID,omitempty"`
	Retryable          bool                       `json:"retryable"`
	NextAction         string                     `json:"nextAction"`
	DetailsOmitted     bool                       `json:"detailsOmitted"`
	CapacityDeferred   bool                       `json:"capacityDeferred,omitempty"`
	DetailsUnavailable bool                       `json:"detailsUnavailable,omitempty"`
}

func compactRecoveryStatus(status *domain.UploadStatus) *compactUploadActionStatus {
	if status == nil {
		return nil
	}
	return &compactUploadActionStatus{UploadID: status.UploadID, State: status.State, Protocol: status.Protocol, ConfirmedOffset: status.ConfirmedOffset, DataComplete: status.DataComplete, DeclaredSize: status.DeclaredSize, ExpiresAt: status.ExpiresAt}
}

func encodeCompactUploadRecoveryPage(requested any, requestID string) ([]byte, error) {
	switch items := requested.(type) {
	case []domain.UploadLifecycleItem:
		compact := make([]compactUploadRecoveryResult, len(items))
		singletonFits := make([]bool, len(items))
		for index, item := range items {
			item.Index = 0
			encoded, err := json.Marshal(item)
			if err != nil {
				return nil, err
			}
			singletonFits[index] = len(encoded)+len(`{"uploads":[]}`) <= maxControlBodyBytes
			compact[index] = compactUploadRecoveryResult{Index: items[index].Index, UploadID: item.UploadID, Status: compactRecoveryStatus(item.Status), Capability: item.Capability, ErrorKind: item.ErrorKind, RequestID: item.RequestID, Retryable: item.Retryable, NextAction: item.NextAction, DetailsOmitted: true}
			single := compact[index]
			single.Index = 0
			encoded, err = json.Marshal(single)
			if err != nil {
				return nil, err
			}
			singletonFits[index] = singletonFits[index] || len(encoded)+len(`{"uploads":[]}`) <= maxControlBodyBytes
		}
		return encodeRecoveryCapacityPage(compact, singletonFits, func(item compactUploadRecoveryResult, unavailable bool) compactUploadRecoveryResult {
			item.Capability = nil
			if item.ErrorKind != "" || item.Status != nil && item.Status.DataComplete {
				return item
			}
			if unavailable {
				item.DetailsUnavailable, item.ErrorKind, item.Retryable, item.NextAction = true, domain.ErrorUnavailable, false, "attention"
				item.RequestID = requestID
			} else {
				item.CapacityDeferred, item.Retryable, item.NextAction = true, true, "smaller_page"
			}
			return item
		})
	case []domain.UploadAdmissionResult:
		compact := make([]compactUploadAdmissionResult, len(items))
		for index, item := range items {
			compact[index] = compactUploadAdmissionResult{Index: item.Index, ID: item.ID, UploadID: item.UploadID, Outcome: item.Outcome, Status: compactRecoveryStatus(item.Status), Capability: item.Capability, ErrorKind: item.ErrorKind, RequestID: item.RequestID, Retryable: item.Retryable, NextAction: item.NextAction, DetailsOmitted: true}
		}
		return encodeRecoveryCapacityPage(compact, nil, func(item compactUploadAdmissionResult, _ bool) compactUploadAdmissionResult {
			item.Capability = nil
			if item.ErrorKind != "" || item.Status != nil && item.Status.DataComplete {
				return item
			}
			// The lease exists. Resume that exact attempt, whose provider
			// capability can differ from BeginUpload's representation. Only
			// actual resume can establish an unrepresentable singleton.
			item.CapacityDeferred, item.NextAction = true, "resume"
			return item
		})
	default:
		return nil, domain.NewError(domain.ErrorUnavailable, "unsupported upload recovery response")
	}
}

// Reserve a correlated placeholder for every item before admitting details.
// Only an item that cannot fit even alone is terminal. Other deferred recovery
// consumers halve their cohort without spending a transient-failure retry;
// successful siblings are removed. Thus even a zero-detail page makes finite
// progress toward singleton recovery, rather than replaying the same overflow.
func encodeRecoveryCapacityPage[T any](items []T, singletonFits []bool, deferItem func(T, bool) T) ([]byte, error) {
	body, overflow, err := encodeBoundedUploadPage(items)
	if err != nil || !overflow {
		return body, err
	}
	if len(items) < 1 || len(items) > domain.MaxUploadLifecycleItems {
		return nil, domain.NewError(domain.ErrorUnavailable, "invalid recovery response cohort")
	}
	const prefix, suffix = `{"uploads":[`, `]}`
	fullSizes, selected := make([]int, len(items)), make([][]byte, len(items))
	used := len(prefix) + len(suffix) + len(items) - 1
	for index, item := range items {
		full, encodeErr := json.Marshal(item)
		err = encodeErr
		if err != nil {
			return nil, err
		}
		fullSizes[index] = len(full)
		// Subdivision renumbers index to zero. Either valid singleton
		// representation can fit even when this page's compact item cannot.
		unavailable := singletonFits != nil && !singletonFits[index]
		selected[index], err = json.Marshal(deferItem(item, unavailable))
		if err != nil {
			return nil, err
		}
		used += len(selected[index])
	}
	if used > maxControlBodyBytes {
		return nil, domain.NewError(domain.ErrorUnavailable, "recovery correlations exceed response bound")
	}
	for index := range items {
		delta := fullSizes[index] - len(selected[index])
		if used+delta <= maxControlBodyBytes {
			selected[index], err = json.Marshal(items[index])
			if err != nil {
				return nil, err
			}
			used += delta
		}
	}
	var encoded bytes.Buffer
	encoded.Grow(used)
	encoded.WriteString(prefix)
	for index, item := range selected {
		if index != 0 {
			encoded.WriteByte(',')
		}
		encoded.Write(item)
	}
	encoded.WriteString(suffix)
	return encoded.Bytes(), nil
}
