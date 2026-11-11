package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func TestUploadRecoveryOverflowCannotMaskInvalidLaterDetail(t *testing.T) {
	items := []domain.UploadLifecycleItem{{Index: 0, UploadID: "large", Capability: reviewCapacityCapability("large", maxControlBodyBytes)}, {Index: 1, UploadID: "invalid", Status: &domain.UploadStatus{UploadID: "invalid"}}}
	response := httptest.NewRecorder()
	writeUploadLifecyclePage(response, httptest.NewRequest(http.MethodPost, "/api/v1/uploads/batch/resume", nil), items)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("invalid later authority was hidden by compaction: status=%d", response.Code)
	}
}

type recoveryCapacityPage struct {
	Uploads []struct {
		Index              int                      `json:"index"`
		ID                 string                   `json:"id"`
		UploadID           domain.UploadID          `json:"uploadID"`
		Outcome            string                   `json:"outcome"`
		Status             *domain.UploadStatus     `json:"status"`
		Capability         *domain.UploadCapability `json:"capability"`
		ErrorKind          domain.ErrorKind         `json:"errorKind"`
		RequestID          string                   `json:"requestID"`
		Retryable          bool                     `json:"retryable"`
		NextAction         string                   `json:"nextAction"`
		DetailsOmitted     bool                     `json:"detailsOmitted"`
		CapacityDeferred   bool                     `json:"capacityDeferred"`
		DetailsUnavailable bool                     `json:"detailsUnavailable"`
	} `json:"uploads"`
}

func reviewCapacityCapability(id domain.UploadID, size int) *domain.UploadCapability {
	return &domain.UploadCapability{UploadID: id, URL: "https://data.example.test/upload#" + strings.Repeat("x", size), Method: http.MethodPatch}
}

func reviewCapacityPage(t *testing.T, items any) recoveryCapacityPage {
	t.Helper()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/uploads/batch/resume", nil)
	request.Header.Set("X-Request-ID", "capacity-review")
	requestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeUploadLifecyclePage(w, r, items) })).ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.Len() > maxControlBodyBytes || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("correlated capacity result unavailable: status=%d bytes=%d", response.Code, response.Body.Len())
	}
	var page recoveryCapacityPage
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func TestUploadRecoveryCapacityUnrepresentableSingletonIsTerminal(t *testing.T) {
	for _, admission := range []bool{false, true} {
		id := domain.UploadID("singleton")
		capability := reviewCapacityCapability(id, maxControlBodyBytes)
		status := &domain.UploadStatus{UploadID: id, State: domain.UploadStateActive, Path: domain.MustParseUserPath("/synthetic.bin"), DeclaredSize: 4}
		var items any = []domain.UploadLifecycleItem{{Index: 0, UploadID: id, Status: status, Capability: capability, NextAction: "transfer"}}
		if admission {
			items = []domain.UploadAdmissionResult{{Index: 0, ID: "stable-attempt", UploadID: id, Outcome: "accepted", Status: status, Capability: capability, NextAction: "transfer"}}
		}
		page := reviewCapacityPage(t, items)
		if len(page.Uploads) != 1 {
			t.Fatal("singleton correlation lost")
		}
		item := page.Uploads[0]
		if admission {
			if item.UploadID != id || item.Outcome != "accepted" || !item.CapacityDeferred || item.DetailsUnavailable || item.Capability != nil || item.ErrorKind != "" || item.NextAction != "resume" {
				t.Fatal("oversized admission did not retain its attempt for actual resume")
			}
			continue
		}
		if item.UploadID != id || !item.DetailsUnavailable || item.CapacityDeferred || item.Capability != nil || item.ErrorKind != domain.ErrorUnavailable || item.Retryable || item.NextAction != "attention" || item.RequestID != "capacity-review" {
			t.Fatal("unrepresentable singleton can loop or lost its admitted attempt")
		}
	}
}

func TestUploadRecoveryCapacityMaximumCorrelationsAndMixedOutcomes(t *testing.T) {
	items := make([]domain.UploadLifecycleItem, domain.MaxUploadLifecycleItems)
	for index := range items {
		id := domain.UploadID(fmt.Sprintf("%03d", index) + strings.Repeat("<", 253))
		items[index] = domain.UploadLifecycleItem{Index: index, UploadID: id, Status: &domain.UploadStatus{UploadID: id, State: domain.UploadStateActive, Path: domain.MustParseUserPath("/x"), DeclaredSize: 4}, Capability: reviewCapacityCapability(id, 600_000), NextAction: "transfer"}
	}
	items[98].Capability, items[98].Status = nil, nil
	items[98].ErrorKind, items[98].RequestID, items[98].NextAction = domain.ErrorNotFound, strings.Repeat("r", 64), "attention"
	items[99].Capability = nil
	items[99].Status.State, items[99].Status.DataComplete, items[99].Status.ConfirmedOffset = domain.UploadStateCompleted, true, 4
	items[99].NextAction = "done"
	page := reviewCapacityPage(t, items)
	if len(page.Uploads) != len(items) {
		t.Fatal("maximum cohort lost correlated outcomes")
	}
	var usable, deferred int
	for index, item := range page.Uploads {
		if item.Index != index || item.UploadID != items[index].UploadID || item.DetailsUnavailable {
			t.Fatal("maximum escaped correlation was lost or made terminal")
		}
		if item.Capability != nil {
			usable++
			if item.Capability.URL != items[index].Capability.URL {
				t.Fatal("capacity changed an opaque capability")
			}
		}
		if item.CapacityDeferred {
			deferred++
			if item.Capability != nil || item.NextAction != "smaller_page" {
				t.Fatal("capacity deferral retained unusable data")
			}
		}
	}
	if usable != 1 || deferred != 97 || page.Uploads[98].ErrorKind != domain.ErrorNotFound || page.Uploads[98].RequestID != items[98].RequestID || page.Uploads[98].CapacityDeferred || page.Uploads[99].Status == nil || !page.Uploads[99].Status.DataComplete || page.Uploads[99].NextAction != "done" || page.Uploads[99].CapacityDeferred {
		t.Fatal("capacity cross-contaminated independent success, rejection or completed siblings")
	}
}
