package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func TestUploadRecoveryBoundedEncodingPreservesOrdinaryWire(t *testing.T) {
	status := &domain.UploadStatus{UploadID: "wire", State: domain.UploadStateActive, Path: domain.MustParseUserPath("/quote-\"-<->-\u2028.bin"), DeclaredSize: 4}
	for _, items := range [][]domain.UploadLifecycleItem{nil, {}, {{Index: 0, UploadID: "wire", Status: status, Capability: reviewCapacityCapability("wire", 64), NextAction: "transfer"}}} {
		want, err := json.Marshal(map[string]any{"uploads": items})
		if err != nil {
			t.Fatal(err)
		}
		got, overflow, err := encodeBoundedUploadPage(items)
		if err != nil || overflow || !bytes.Equal(got, want) {
			t.Fatalf("ordinary wire changed: overflow=%t err=%v", overflow, err)
		}
	}
}

func TestUploadRecoveryOverflowCannotMaskInvalidLaterDetail(t *testing.T) {
	items := []domain.UploadLifecycleItem{{Index: 0, UploadID: "large", Capability: reviewCapacityCapability("large", maxControlBodyBytes)}, {Index: 1, UploadID: "invalid", Status: &domain.UploadStatus{UploadID: "invalid"}}}
	response := httptest.NewRecorder()
	writeUploadLifecyclePage(response, httptest.NewRequest(http.MethodPost, "/api/v1/uploads/batch/resume", nil), items)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("invalid later authority was hidden by compaction: status=%d", response.Code)
	}
}

func TestUploadRecoveryResponseExactWireBoundary(t *testing.T) {
	item := domain.UploadLifecycleItem{Index: 0, UploadID: "boundary", Capability: reviewCapacityCapability("boundary", 0), NextAction: "transfer"}
	initial, err := json.Marshal(map[string]any{"uploads": []domain.UploadLifecycleItem{item}})
	if err != nil {
		t.Fatal(err)
	}
	item.Capability.URL += strings.Repeat("x", maxControlBodyBytes-len(initial))
	want, err := json.Marshal(map[string]any{"uploads": []domain.UploadLifecycleItem{item}})
	if err != nil {
		t.Fatal(err)
	}
	got, overflow, err := encodeBoundedUploadPage([]domain.UploadLifecycleItem{item})
	if err != nil || overflow || len(got) != maxControlBodyBytes || !bytes.Equal(got, want) {
		t.Fatalf("exact wire boundary changed: bytes=%d overflow=%t err=%v", len(got), overflow, err)
	}
	item.Capability.URL += "x"
	page := reviewCapacityPage(t, []domain.UploadLifecycleItem{item})
	if len(page.Uploads) != 1 || page.Uploads[0].UploadID != item.UploadID || !page.Uploads[0].DetailsUnavailable {
		t.Fatal("one-byte overflow lost terminal singleton semantics")
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

func TestUploadRecoveryCapacityRetainsUsableSiblingAndSplitsOnlyDeferred(t *testing.T) {
	items := make([]domain.UploadLifecycleItem, 2)
	for index := range items {
		id := domain.UploadID(string(rune('a' + index)))
		items[index] = domain.UploadLifecycleItem{Index: index, UploadID: id, Status: &domain.UploadStatus{UploadID: id, State: domain.UploadStateActive, Path: domain.MustParseUserPath("/synthetic.bin"), DeclaredSize: 4}, Capability: reviewCapacityCapability(id, 600_000), NextAction: "transfer"}
	}
	page := reviewCapacityPage(t, items)
	if len(page.Uploads) != 2 {
		t.Fatal("capacity envelope lost item correlation")
	}
	first, deferred := page.Uploads[0], page.Uploads[1]
	if first.Capability == nil || first.Capability.URL != items[0].Capability.URL || first.CapacityDeferred || first.ErrorKind != "" {
		t.Fatal("usable sibling was lost or changed")
	}
	if deferred.Index != 1 || deferred.UploadID != items[1].UploadID || !deferred.CapacityDeferred || deferred.Capability != nil || deferred.NextAction != "smaller_page" {
		t.Fatal("oversized cohort did not explicitly defer only its remaining capability")
	}
	// Fewer requested IDs recover the complete same attempt. Capacity progress
	// does not allocate a replacement upload or spend transient-error retries.
	items[1].Index = 0
	single := reviewCapacityPage(t, items[1:])
	if len(single.Uploads) != 1 || single.Uploads[0].CapacityDeferred || single.Uploads[0].Capability == nil || single.Uploads[0].Capability.URL != items[1].Capability.URL {
		t.Fatal("smaller cohort did not recover the exact deferred capability")
	}
}

func TestUploadAdmissionCapacityPreservesDurableAttemptAndDirectsResume(t *testing.T) {
	items := make([]domain.UploadAdmissionResult, 2)
	for index := range items {
		id := domain.UploadID(string(rune('a' + index)))
		items[index] = domain.UploadAdmissionResult{Index: index, ID: string(rune('c' + index)), UploadID: id, Outcome: "accepted", Status: &domain.UploadStatus{UploadID: id, State: domain.UploadStateActive, Path: domain.MustParseUserPath("/synthetic.bin"), DeclaredSize: 4}, Capability: reviewCapacityCapability(id, 600_000), NextAction: "transfer"}
	}
	page := reviewCapacityPage(t, items)
	if len(page.Uploads) != 2 {
		t.Fatal("admission lost correlated outcomes")
	}
	deferred := page.Uploads[1]
	if page.Uploads[0].Capability == nil || deferred.Index != 1 || deferred.ID != items[1].ID || deferred.UploadID != items[1].UploadID || deferred.Outcome != "accepted" || !deferred.CapacityDeferred || deferred.Capability != nil || deferred.NextAction != "resume" || deferred.ErrorKind != "" {
		t.Fatal("durable admission was converted into rejection or missing capability without explicit recovery")
	}
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
