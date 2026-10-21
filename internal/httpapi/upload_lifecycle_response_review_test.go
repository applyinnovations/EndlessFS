package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

const reviewLifecycleOrigin = "https://drive.example.test"

var reviewLifecyclePayload = []byte{0, 1, 2, 3}

// These router regressions intentionally observe the response separately from
// authoritative publication. A bounded 503 is not a successful lifecycle result
// when the exact same page remains unconfirmable after publication and replay.
func TestIntegrationUploadLifecycleResponseReviewLongValidPathsRemainConfirmableAcrossReplay(t *testing.T) {
	env := newDriveHTTPEnvironment(t)
	cookies := []*http.Cookie{env.session, env.csrf}
	parent := "/"
	for index := range 15 {
		if parent != "/" {
			parent += "/"
		}
		parent += strings.Repeat("<", 255)
		path, err := domain.ParseUserPath(parent)
		if err != nil {
			t.Fatalf("synthetic parent %d is not valid: %v", index, err)
		}
		body, err := json.Marshal(map[string]any{"path": path})
		if err != nil {
			t.Fatal(err)
		}
		response := performRequest(t, env.handler, http.MethodPost, "/api/v1/directories", reviewLifecycleOrigin, string(body), cookies, driveMutationHeaders(env.csrf.Value, ""))
		if response.Code != http.StatusCreated {
			t.Fatalf("parent preparation failed at index %d: status=%d", index, response.Code)
		}
	}
	paths := make([]domain.UserPath, 24)
	for index := range paths {
		value := parent + "/" + strings.Repeat("<", 249) + fmt.Sprintf("f%05d", index)
		path, err := domain.ParseUserPath(value)
		if err != nil || len(value) != 4096 {
			t.Fatalf("synthetic maximum path is invalid: bytes=%d err=%v", len(value), err)
		}
		paths[index] = path
	}
	capabilities := reviewLifecycleAdmitAndTransfer(t, env, paths)
	items := make([]domain.UploadActionItem, len(capabilities))
	for index, capability := range capabilities {
		items[index] = domain.UploadActionItem{UploadID: capability.UploadID, Action: "complete", CRC32C: objectstore.FingerprintFor(reviewLifecyclePayload).CRC32C}
	}
	requestBody, err := json.Marshal(map[string]any{"items": items})
	if err != nil || len(requestBody) >= maxControlBodyBytes {
		t.Fatalf("completion request does not fit the real envelope: bytes=%d err=%v", len(requestBody), err)
	}
	var versions []domain.Version
	for attempt := range 2 {
		currentItems := append([]domain.UploadActionItem(nil), items...)
		if attempt != 0 {
			for left, right := 0, len(currentItems)-1; left < right; left, right = left+1, right-1 {
				currentItems[left], currentItems[right] = currentItems[right], currentItems[left]
			}
		}
		body, err := json.Marshal(map[string]any{"items": currentItems})
		if err != nil {
			t.Fatal(err)
		}
		response := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/batch/actions", reviewLifecycleOrigin, string(body), cookies, driveMutationHeaders(env.csrf.Value, "response-review-action-page"))
		// Establish that the request reached actual publication before making
		// the independent response assertion. Readback uses the provider API,
		// rather than inferring authority from the failed HTTP response.
		current := reviewLifecycleAssertPublished(t, env, paths, capabilities)
		if attempt == 0 {
			versions = current
		} else {
			for index := range versions {
				if versions[index] != current[index] {
					t.Errorf("replay allocated another authoritative version at item %d", index)
				}
			}
		}
		t.Logf("long-path lifecycle attempt=%d request_bytes=%d response_status=%d response_bytes=%d authoritative_completed=%d actual_payload_bytes=%d", attempt, len(requestBody), response.Code, response.Body.Len(), len(current), env.storage.Instrumentation().UploadBytes)
		reviewLifecycleRequireConfirmableResponse(t, response, currentItems, attempt != 0)
	}
	wrong := append([]domain.UploadActionItem(nil), items...)
	for index := range wrong {
		wrong[index].CRC32C = objectstore.FingerprintFor([]byte{0, 0, 0, 0}).CRC32C
	}
	wrongBody, err := json.Marshal(map[string]any{"items": wrong})
	if err != nil {
		t.Fatal(err)
	}
	denied := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/batch/actions", reviewLifecycleOrigin, string(wrongBody), cookies, driveMutationHeaders(env.csrf.Value, "response-review-changed-crc"))
	reviewLifecycleRequireDeniedItems(t, denied, wrong, domain.ErrorConflict)
	foreign := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/batch/actions", reviewLifecycleOrigin, string(requestBody), []*http.Cookie{env.otherSession, env.otherCSRF}, driveMutationHeaders(env.otherCSRF.Value, "response-review-foreign"))
	reviewLifecycleRequireDeniedItems(t, foreign, items, domain.ErrorNotFound)
	for _, forbidden := range []string{`"path"`, `"entry"`, `"capability"`} {
		if bytes.Contains(foreign.Body.Bytes(), []byte(forbidden)) {
			t.Errorf("foreign response exposed forbidden field %s", forbidden)
		}
	}
	final := reviewLifecycleAssertPublished(t, env, paths, capabilities)
	for index := range versions {
		if final[index] != versions[index] {
			t.Errorf("denied requests changed authoritative version at item %d", index)
		}
	}
}

func TestIntegrationUploadLifecycleResponseReviewInvalidActionCannotPoisonPublishedSibling(t *testing.T) {
	env := newDriveHTTPEnvironment(t)
	paths := []domain.UserPath{domain.MustParseUserPath("/response-review.bin")}
	capabilities := reviewLifecycleAdmitAndTransfer(t, env, paths)
	items := []domain.UploadActionItem{
		{UploadID: capabilities[0].UploadID, Action: "complete", CRC32C: objectstore.FingerprintFor(reviewLifecyclePayload).CRC32C},
		{UploadID: "invalid-action-item", Action: ""},
	}
	empty, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		t.Fatal(err)
	}
	// ASCII has no variable JSON escaping. This is one byte below the actual
	// strict request limit, not an oversized input rejected before dispatch.
	items[1].Action = strings.Repeat("x", maxControlBodyBytes-len(empty)-1)
	body, err := json.Marshal(map[string]any{"items": items})
	if err != nil || len(body) != maxControlBodyBytes-1 {
		t.Fatalf("invalid-action envelope does not fit exactly: bytes=%d err=%v", len(body), err)
	}
	cookies := []*http.Cookie{env.session, env.csrf}
	var prior domain.Version
	for attempt := range 2 {
		response := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/batch/actions", reviewLifecycleOrigin, string(body), cookies, driveMutationHeaders(env.csrf.Value, "response-review-echo-page"))
		versions := reviewLifecycleAssertPublished(t, env, paths, capabilities)
		if attempt == 0 {
			prior = versions[0]
		} else if versions[0] != prior {
			t.Error("invalid sibling caused completion replay to allocate another authoritative version")
		}
		t.Logf("invalid-action lifecycle attempt=%d request_bytes=%d response_status=%d response_bytes=%d authoritative_completed=%d actual_payload_bytes=%d", attempt, len(body), response.Code, response.Body.Len(), len(versions), env.storage.Instrumentation().UploadBytes)
		reviewLifecycleRequireConfirmableResponse(t, response, items, attempt != 0)
	}
}

func reviewLifecycleAdmitAndTransfer(t *testing.T, env driveHTTPEnvironment, paths []domain.UserPath) []domain.UploadCapability {
	t.Helper()
	requests := make([]map[string]any, len(paths))
	for index, path := range paths {
		requests[index] = map[string]any{"path": path, "size": len(reviewLifecyclePayload), "mediaType": "application/octet-stream", "resumable": true, "idempotencyKey": fmt.Sprintf("response-review-item-%04d", index)}
	}
	body, err := json.Marshal(map[string]any{"uploads": requests})
	if err != nil || len(body) >= maxControlBodyBytes {
		t.Fatalf("real admission request does not fit: bytes=%d err=%v", len(body), err)
	}
	response := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/batch/admit", reviewLifecycleOrigin, string(body), []*http.Cookie{env.session, env.csrf}, driveMutationHeaders(env.csrf.Value, "response-review-admission"))
	if response.Code != http.StatusOK || response.Body.Len() > maxControlBodyBytes {
		t.Fatalf("real bounded admission failed: status=%d request_bytes=%d response_bytes=%d", response.Code, len(body), response.Body.Len())
	}
	var page struct {
		Uploads []domain.UploadAdmissionResult `json:"uploads"`
	}
	decodeResponse(t, response, &page)
	if len(page.Uploads) != len(paths) {
		t.Fatalf("admission lost items: got=%d want=%d", len(page.Uploads), len(paths))
	}
	capabilities := make([]domain.UploadCapability, len(paths))
	for index, item := range page.Uploads {
		if item.Index != index || item.Capability == nil || item.Status == nil || item.Status.Path != paths[index] || item.Outcome != "accepted" {
			t.Fatalf("admission item %d did not establish the valid fixture", index)
		}
		capabilities[index] = *item.Capability
		request, err := http.NewRequest(item.Capability.Method, item.Capability.URL, bytes.NewReader(reviewLifecyclePayload))
		if err != nil {
			t.Fatal(err)
		}
		for name, value := range item.Capability.Headers {
			request.Header.Set(name, value)
		}
		request.Header.Set("Origin", reviewLifecycleOrigin)
		dataResponse, err := env.data.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		dataResponse.Body.Close()
		if dataResponse.StatusCode != http.StatusNoContent {
			t.Fatalf("real synthetic data transfer %d failed: status=%d", index, dataResponse.StatusCode)
		}
	}
	t.Logf("actual admission items=%d request_bytes=%d response_bytes=%d actual_payload_bytes=%d", len(paths), len(body), response.Body.Len(), env.storage.Instrumentation().UploadBytes)
	return capabilities
}

func reviewLifecycleAssertPublished(t *testing.T, env driveHTTPEnvironment, paths []domain.UserPath, capabilities []domain.UploadCapability) []domain.Version {
	t.Helper()
	scope, err := domain.NewScope(env.user, domain.AreaLive)
	if err != nil {
		t.Fatal(err)
	}
	versions := make([]domain.Version, len(paths))
	for index, path := range paths {
		entry, err := env.storage.Stat(context.Background(), scope, path)
		if err != nil || entry.Path != path || entry.Kind != domain.EntryFile || entry.Size != int64(len(reviewLifecyclePayload)) || entry.Version == "" {
			t.Fatalf("valid sibling %d did not reach authoritative publication: err=%v size=%d", index, err, entry.Size)
		}
		status, err := env.storage.UploadStatus(context.Background(), scope, capabilities[index].UploadID)
		if err != nil || status.State != domain.UploadStateCompleted || !status.DataComplete || status.ConfirmedOffset != int64(len(reviewLifecyclePayload)) {
			t.Fatalf("valid sibling %d terminal receipt not published: err=%v state=%s", index, err, status.State)
		}
		versions[index] = entry.Version
	}
	if got := env.storage.Instrumentation().UploadBytes; got != int64(len(paths)*len(reviewLifecyclePayload)) {
		t.Fatalf("completed files were retransferred: actual_bytes=%d", got)
	}
	return versions
}

func reviewLifecycleRequireConfirmableResponse(t *testing.T, response *httptest.ResponseRecorder, items []domain.UploadActionItem, replay bool) {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Errorf("valid published siblings cannot be confirmed: status=%d response_bytes=%d replay=%t", response.Code, response.Body.Len(), replay)
		return
	}
	if response.Body.Len() > maxControlBodyBytes {
		t.Errorf("terminal response exceeds real envelope: bytes=%d", response.Body.Len())
		return
	}
	var page struct {
		Uploads []struct {
			domain.UploadActionResult
			DetailsOmitted bool `json:"detailsOmitted"`
		} `json:"uploads"`
	}
	decodeResponse(t, response, &page)
	if len(page.Uploads) != len(items) {
		t.Errorf("terminal response lost per-item outcomes: got=%d want=%d", len(page.Uploads), len(items))
		return
	}
	for index, item := range page.Uploads {
		if item.Index != index || item.UploadID != items[index].UploadID {
			t.Errorf("terminal outcome correlation changed at item %d", index)
		}
		if items[index].Action != "complete" {
			if item.Outcome != "rejected" || item.ErrorKind != domain.ErrorInvalid || item.Action != "invalid" {
				t.Errorf("invalid sibling was not independently rejected: item=%d", index)
			}
			continue
		}
		want := "applied"
		if replay {
			want = "already_applied"
		}
		if item.Outcome != want || item.Status == nil || item.Status.State != domain.UploadStateCompleted || item.Action != items[index].Action {
			t.Errorf("valid sibling outcome was lost: item=%d outcome=%s", index, item.Outcome)
		}
		if item.DetailsOmitted {
			if item.Entry != nil || item.Status == nil || item.Status.Path.Valid() {
				t.Errorf("compact acknowledgement did not explicitly omit details: item=%d", index)
			}
		} else if item.Entry == nil || item.Status == nil || !item.Status.Path.Valid() {
			t.Errorf("ordinary full-detail acknowledgement lost details: item=%d", index)
		}
	}
}

func reviewLifecycleRequireDeniedItems(t *testing.T, response *httptest.ResponseRecorder, requests []domain.UploadActionItem, kind domain.ErrorKind) {
	t.Helper()
	if response.Code != http.StatusOK || response.Body.Len() > maxControlBodyBytes {
		t.Fatalf("denied page did not preserve bounded independent outcomes: status=%d bytes=%d", response.Code, response.Body.Len())
	}
	var page struct {
		Uploads []domain.UploadActionResult `json:"uploads"`
	}
	decodeResponse(t, response, &page)
	if len(page.Uploads) != len(requests) {
		t.Fatalf("denied page dropped items: %d", len(page.Uploads))
	}
	for index, item := range page.Uploads {
		if item.Index != index || item.UploadID != requests[index].UploadID || item.ErrorKind != kind || item.Outcome != "rejected" || item.Entry != nil {
			t.Errorf("denied outcome changed: item=%d kind=%s outcome=%s", index, item.ErrorKind, item.Outcome)
		}
	}
}

func TestUploadActionResponseReviewMaximumEscapingPreservesTypedAcknowledgements(t *testing.T) {
	for name, segment := range map[string]string{
		"html":    strings.Repeat("<", 255),
		"quotes":  strings.Repeat(`"`, 255),
		"unicode": strings.Repeat("\u2028\u2029", 42) + "\u2028",
	} {
		t.Run(name, func(t *testing.T) {
			path, err := domain.ParseUserPath(strings.Repeat("/"+segment, 16))
			if err != nil || len(path.String()) != 4096 {
				t.Fatalf("invalid exact4096B fixture: bytes=%d err=%v", len(path.String()), err)
			}
			items := make([]domain.UploadActionResult, domain.MaxUploadLifecycleItems)
			deadline := time.Date(2099, 12, 31, 23, 59, 59, 999999999, time.UTC)
			for index := range items {
				id := domain.UploadID(fmt.Sprintf("%03d", index) + strings.Repeat("<", 253))
				status := &domain.UploadStatus{UploadID: id, State: domain.UploadStateCompleted, Path: path, Protocol: domain.UploadResumable, ConfirmedOffset: 9223372036854775807, DeclaredSize: 9223372036854775807, DataComplete: true, ExpiresAt: deadline}
				items[index] = domain.UploadActionResult{Index: index, UploadID: id, Action: "complete", Outcome: "applied", Status: status, Entry: &domain.Entry{Path: path, Name: path.Name(), Kind: domain.EntryFile, Size: status.DeclaredSize, FileCount: 1, MediaType: "application/octet-stream", ModifiedAt: deadline, Version: "version"}, NextAction: "done"}
				switch index % 5 {
				case 1:
					items[index].Outcome = "already_applied"
				case 2:
					status.State, status.ConfirmedOffset, status.DataComplete = domain.UploadStateExpired, 0, false
					items[index].Outcome, items[index].Entry, items[index].ErrorKind, items[index].NextAction = "rejected", nil, domain.ErrorConflict, "restart"
					items[index].RequestID = strings.Repeat("r", 64)
				case 3:
					status.State, status.DataComplete = domain.UploadStateActive, false
					items[index].Action, items[index].Outcome, items[index].Entry = "cancel", "unknown", nil
					items[index].ErrorKind, items[index].Retryable, items[index].NextAction = domain.ErrorUnavailable, true, "retry"
					items[index].RequestID = strings.Repeat("r", 64)
				case 4:
					status.State, status.ConfirmedOffset, status.DataComplete = domain.UploadStateAborted, 0, false
					items[index].Action, items[index].Entry = "cancel", nil
				}
			}
			full, err := json.Marshal(map[string]any{"uploads": items})
			if err != nil || len(full) <= maxControlBodyBytes {
				t.Fatalf("fixture does not exercise encoded overflow: bytes=%d err=%v", len(full), err)
			}
			response := httptest.NewRecorder()
			writeUploadActionPage(response, httptest.NewRequest(http.MethodPost, "/api/v1/uploads/batch/actions", nil), items)
			if response.Code != http.StatusOK || response.Body.Len() > maxControlBodyBytes || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("compact maximum page failed: status=%d bytes=%d", response.Code, response.Body.Len())
			}
			var decoded struct {
				Uploads []struct {
					domain.UploadActionResult
					DetailsOmitted bool `json:"detailsOmitted"`
				} `json:"uploads"`
			}
			decodeResponse(t, response, &decoded)
			if len(decoded.Uploads) != len(items) {
				t.Fatalf("compact page lost acknowledgements: %d", len(decoded.Uploads))
			}
			for index, got := range decoded.Uploads {
				want := items[index]
				if !got.DetailsOmitted || got.Entry != nil || got.Index != want.Index || got.UploadID != want.UploadID || got.Action != want.Action || got.Outcome != want.Outcome || got.ErrorKind != want.ErrorKind || got.RequestID != want.RequestID || got.Retryable != want.Retryable || got.NextAction != want.NextAction {
					t.Errorf("compact terminal acknowledgement changed fields at item %d", index)
				}
				if got.Status == nil || got.Status.Path.Valid() || got.Status.UploadID != want.Status.UploadID || got.Status.State != want.Status.State || got.Status.Protocol != want.Status.Protocol || got.Status.ConfirmedOffset != want.Status.ConfirmedOffset || got.Status.DataComplete != want.Status.DataComplete || got.Status.DeclaredSize != want.Status.DeclaredSize || !got.Status.ExpiresAt.Equal(want.Status.ExpiresAt) {
					t.Errorf("compact status lost progress at item %d", index)
				}
			}
			t.Logf("typed compact acknowledgements escaping=%s items=%d full_bytes=%d compact_bytes=%d", name, len(items), len(full), response.Body.Len())
		})
	}
}

func TestUploadActionResponseReviewOrdinaryWireAndMarshalFailure(t *testing.T) {
	path := domain.MustParseUserPath("/ordinary.bin")
	items := []domain.UploadActionResult{{Index: 0, UploadID: "ordinary-upload", Action: "complete", Outcome: "applied", Status: &domain.UploadStatus{UploadID: "ordinary-upload", State: domain.UploadStateCompleted, Path: path, ConfirmedOffset: 4, DataComplete: true, DeclaredSize: 4}, Entry: &domain.Entry{Path: path, Name: "ordinary.bin", Kind: domain.EntryFile, Size: 4}, NextAction: "done"}}
	want, err := json.Marshal(map[string]any{"uploads": items})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/uploads/batch/actions", nil)
	writeUploadActionPage(response, request, items)
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), want) {
		t.Fatalf("ordinary action response wire changed: status=%d", response.Code)
	}
	items[0].Entry.Path = domain.UserPath{}
	response = httptest.NewRecorder()
	writeUploadActionPage(response, request, items)
	if response.Code != http.StatusServiceUnavailable || bytes.Contains(response.Body.Bytes(), []byte("detailsOmitted")) {
		t.Fatalf("invalid authority was hidden behind compact success: status=%d", response.Code)
	}
}
