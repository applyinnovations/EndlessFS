package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

// Ordinary /x status details are smaller than the compact representation's
// detailsOmitted marker. A valid ordinary singleton at the exact wire limit
// must be recoverable by subdivision, even when its compact singleton is too
// large. An independent rejected sibling must retain its own outcome.
func TestUploadRecoveryBoundaryDefersRepresentableOrdinarySingleton(t *testing.T) {
	for _, admission := range []bool{false, true} {
		name := "recovery"
		if admission {
			name = "admission"
		}
		t.Run(name, func(t *testing.T) {
			id := domain.UploadID("boundary-upload-a")
			expires := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
			status := &domain.UploadStatus{UploadID: id, State: domain.UploadStateActive, Path: domain.MustParseUserPath("/x"), Protocol: domain.UploadResumable, DeclaredSize: 4, ExpiresAt: expires}
			capability := &domain.UploadCapability{UploadID: id, Protocol: domain.UploadResumable, URL: "https://data.example.test/upload#", Method: http.MethodPatch, Headers: map[string]string{"Upload-Offset": "0"}, ExpiresAt: expires, Framing: domain.UploadFramingOffsetHeader, DeclaredSize: 4}
			recovery := domain.UploadLifecycleItem{Index: 0, UploadID: id, Status: status, Capability: capability, NextAction: "transfer"}
			accepted := domain.UploadAdmissionResult{Index: 0, ID: "boundary-item-key-a", UploadID: id, Outcome: "accepted", Status: status, Capability: capability, NextAction: "transfer"}
			var single any = []domain.UploadLifecycleItem{recovery}
			if admission {
				single = []domain.UploadAdmissionResult{accepted}
			}
			base, err := json.Marshal(map[string]any{"uploads": single})
			if err != nil {
				t.Fatal(err)
			}
			capability.URL += strings.Repeat("x", maxControlBodyBytes-len(base))
			ordinary, err := json.Marshal(map[string]any{"uploads": single})
			if err != nil {
				t.Fatal(err)
			}
			if len(ordinary) != maxControlBodyBytes {
				t.Fatal("ordinary singleton fixture is not exactly at the supported wire limit")
			}
			response := httptest.NewRecorder()
			writeUploadLifecyclePage(response, httptest.NewRequest(http.MethodPost, "/api/v1/uploads/batch/resume", nil), single)
			if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), ordinary) {
				t.Fatal("otherwise-valid ordinary singleton positive control did not succeed byte-exactly")
			}
			var compact any = []compactUploadRecoveryResult{{Index: 0, UploadID: id, Status: compactRecoveryStatus(status), Capability: capability, NextAction: "transfer", DetailsOmitted: true}}
			if admission {
				compact = []compactUploadAdmissionResult{{Index: 0, ID: accepted.ID, UploadID: id, Outcome: "accepted", Status: compactRecoveryStatus(status), Capability: capability, NextAction: "transfer", DetailsOmitted: true}}
			}
			compactBody, err := json.Marshal(map[string]any{"uploads": compact})
			if err != nil {
				t.Fatal(err)
			}
			if len(compactBody) <= maxControlBodyBytes {
				t.Fatal("compact singleton fixture did not exceed its ordinary representation")
			}
			var cohort any = []domain.UploadLifecycleItem{recovery, {Index: 1, UploadID: "boundary-upload-b", ErrorKind: domain.ErrorNotFound, RequestID: "boundary-request-42", NextAction: "attention"}}
			if admission {
				cohort = []domain.UploadAdmissionResult{accepted, {Index: 1, ID: "boundary-item-key-b", Outcome: "rejected", ErrorKind: domain.ErrorNotFound, RequestID: "boundary-request-42", NextAction: "attention"}}
			}
			page := reviewCapacityPage(t, cohort)
			if len(page.Uploads) != 2 {
				t.Fatal("larger response lost independent correlations")
			}
			item := page.Uploads[0]
			t.Logf("ordinary_singleton_bytes=%d compact_singleton_bytes=%d outcome=%s deferred=%t terminal=%t error_kind=%s", len(ordinary), len(compactBody), item.Outcome, item.CapacityDeferred, item.DetailsUnavailable, item.ErrorKind)
			wantAction := "smaller_page"
			if admission {
				wantAction = "resume"
			}
			if item.Index != 0 || item.UploadID != id || !item.CapacityDeferred || item.DetailsUnavailable || item.ErrorKind != "" || item.Capability != nil || item.NextAction != wantAction || item.Status == nil || item.Status.UploadID != id || item.Status.DeclaredSize != 4 {
				t.Fatal("representable ordinary singleton was made terminal instead of deferring to a smaller page")
			}
			if admission && (item.ID != accepted.ID || item.Outcome != "accepted") {
				t.Fatal("deferral lost already-durable admission identity")
			}
			if page.Uploads[1].Index != 1 || page.Uploads[1].ErrorKind != domain.ErrorNotFound || page.Uploads[1].CapacityDeferred || page.Uploads[1].DetailsUnavailable {
				t.Fatal("near-boundary sibling changed independent rejection")
			}
			// The exact original capability remains available when the requested
			// cohort becomes one; no admission or payload replay is necessary.
			smaller := reviewCapacityPage(t, single)
			if smaller.Uploads[0].Capability == nil || smaller.Uploads[0].Capability.URL != capability.URL || smaller.Uploads[0].CapacityDeferred || smaller.Uploads[0].DetailsUnavailable {
				t.Fatal("ordinary singleton could not recover its complete unchanged capability")
			}
		})
	}
}

func boundaryReviewExactLifecycleSingleton(t *testing.T) (domain.UploadLifecycleItem, []byte) {
	t.Helper()
	id := domain.UploadID("boundary-upload-a")
	expires := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	status := &domain.UploadStatus{UploadID: id, State: domain.UploadStateActive, Path: domain.MustParseUserPath("/x"), Protocol: domain.UploadResumable, DeclaredSize: 4, ExpiresAt: expires}
	capability := &domain.UploadCapability{UploadID: id, Protocol: domain.UploadResumable, URL: "https://data.example.test/upload#", Method: http.MethodPatch, Headers: map[string]string{"Upload-Offset": "0"}, ExpiresAt: expires, Framing: domain.UploadFramingOffsetHeader, DeclaredSize: 4}
	item := domain.UploadLifecycleItem{Index: 0, UploadID: id, Status: status, Capability: capability, NextAction: "transfer"}
	base, err := json.Marshal(map[string]any{"uploads": []domain.UploadLifecycleItem{item}})
	if err != nil {
		t.Fatal(err)
	}
	capability.URL += strings.Repeat("x", maxControlBodyBytes-len(base))
	ordinary, err := json.Marshal(map[string]any{"uploads": []domain.UploadLifecycleItem{item}})
	if err != nil || len(ordinary) != maxControlBodyBytes {
		t.Fatal("ordinary lifecycle singleton fixture failed")
	}
	positive := httptest.NewRecorder()
	writeUploadLifecyclePage(positive, httptest.NewRequest(http.MethodPost, "/api/v1/uploads/batch/resume", nil), []domain.UploadLifecycleItem{item})
	if positive.Code != http.StatusOK || !bytes.Equal(positive.Body.Bytes(), ordinary) {
		t.Fatal("actual singleton resume positive control failed")
	}
	return item, ordinary
}

func TestUploadRecoveryBoundaryNormalizesSmallerQueryIndex(t *testing.T) {
	item, ordinary := boundaryReviewExactLifecycleSingleton(t)
	cohort := make([]domain.UploadLifecycleItem, 100)
	for index := 0; index < 99; index++ {
		cohort[index] = domain.UploadLifecycleItem{Index: index, UploadID: domain.UploadID(fmt.Sprintf("missing-%02d", index)), ErrorKind: domain.ErrorNotFound, NextAction: "attention"}
	}
	item.Index = 99
	cohort[99] = item
	page := reviewCapacityPage(t, cohort)
	if len(page.Uploads) != 100 {
		t.Fatal("larger response lost correlations")
	}
	last := page.Uploads[99]
	t.Logf("actual_singleton_index=0 original_singleton_bytes=%d original_cohort_index=%d deferred=%t terminal=%t", len(ordinary), item.Index, last.CapacityDeferred, last.DetailsUnavailable)
	if last.Index != 99 || last.UploadID != item.UploadID || !last.CapacityDeferred || last.DetailsUnavailable || last.ErrorKind != "" || last.NextAction != "smaller_page" {
		t.Fatal("cohort index width made a recoverable singleton terminal")
	}
}

func TestUploadAdmissionBoundaryUsesActualResumeRepresentation(t *testing.T) {
	item, ordinaryResume := boundaryReviewExactLifecycleSingleton(t)
	admitted := domain.UploadAdmissionResult{Index: 0, ID: "boundary-item-key-a", UploadID: item.UploadID, Outcome: "accepted", Status: item.Status, Capability: item.Capability, NextAction: "transfer"}
	ordinaryAdmission, err := json.Marshal(map[string]any{"uploads": []domain.UploadAdmissionResult{admitted}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ordinaryAdmission) <= maxControlBodyBytes {
		t.Fatal("admission representation did not exceed its actual resume representation")
	}
	page := reviewCapacityPage(t, []domain.UploadAdmissionResult{admitted})
	if len(page.Uploads) != 1 {
		t.Fatal("already admitted singleton correlation lost")
	}
	result := page.Uploads[0]
	t.Logf("actual_resume_singleton_bytes=%d ordinary_admission_bytes=%d deferred=%t terminal=%t", len(ordinaryResume), len(ordinaryAdmission), result.CapacityDeferred, result.DetailsUnavailable)
	if result.UploadID != admitted.UploadID || result.ID != admitted.ID || result.Outcome != "accepted" || !result.CapacityDeferred || result.DetailsUnavailable || result.ErrorKind != "" || result.NextAction != "resume" || result.Capability != nil {
		t.Fatal("admission-only correlation fields made a usable exact-upload resume terminal")
	}
}
