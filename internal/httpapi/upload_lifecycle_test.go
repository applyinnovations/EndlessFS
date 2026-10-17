package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

func TestIntegrationUploadLifecycleMixedResultsAndBoundaries(t *testing.T) {
	env := newDriveHTTPEnvironment(t)
	const origin = "https://drive.example.test"
	cookies := []*http.Cookie{env.session, env.csrf}
	headers := driveMutationHeaders(env.csrf.Value, "lifecycle-admission-key")
	created := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads", origin, `{"path":"/recovery.bin","size":3,"mediaType":"application/octet-stream","resumable":true}`, cookies, headers)
	if created.Code != http.StatusCreated {
		t.Fatalf("admission: %d %s", created.Code, created.Body.String())
	}
	var capability domain.UploadCapability
	decodeResponse(t, created, &capability)
	body, _ := json.Marshal(map[string]any{"uploadIDs": []domain.UploadID{capability.UploadID, "missing"}})
	for _, endpoint := range []string{"status", "resume"} {
		target := "/api/v1/uploads/batch/" + endpoint
		response := performRequest(t, env.handler, http.MethodPost, target, origin, string(body), cookies, headers)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", endpoint, response.Code, response.Body.String())
		}
		var decoded struct {
			Uploads []domain.UploadLifecycleItem `json:"uploads"`
		}
		decodeResponse(t, response, &decoded)
		if response.Header().Get("Cache-Control") != "no-store" || decoded.Uploads[1].RequestID == "" {
			t.Fatalf("missing recovery privacy/reference boundary: %s", response.Body.String())
		}
		if len(decoded.Uploads) != 2 || decoded.Uploads[0].Index != 0 || decoded.Uploads[0].Status == nil || decoded.Uploads[0].Status.State != domain.UploadStateActive || decoded.Uploads[1].ErrorKind != domain.ErrorNotFound {
			t.Fatalf("mixed page: %+v", decoded)
		}
		if (decoded.Uploads[0].Capability != nil) != (endpoint == "resume") {
			t.Fatalf("status leaked or resume omitted capability: %+v", decoded.Uploads[0])
		}
		foreign := performRequest(t, env.handler, http.MethodPost, target, origin, string(body), []*http.Cookie{env.otherSession, env.otherCSRF}, driveMutationHeaders(env.otherCSRF.Value, "lifecycle-foreign-key"))
		if foreign.Code != http.StatusOK || strings.Contains(foreign.Body.String(), "recovery.bin") || strings.Contains(foreign.Body.String(), `"capability"`) {
			t.Fatalf("cross-owner disclosure: %d %s", foreign.Code, foreign.Body.String())
		}
		decodeResponse(t, foreign, &decoded)
		if decoded.Uploads[0].ErrorKind != domain.ErrorNotFound || decoded.Uploads[1].ErrorKind != domain.ErrorNotFound {
			t.Fatalf("foreign and missing differ: %+v", decoded)
		}
		for _, bad := range []string{`{"uploadIDs":[]}`, `{"uploadIDs":["a","a"]}`, `{"uploadIDs":[""]}`, `{"uploadIDs":["a"],"ownerID":"foreign"}`, `{"uploadIDs":["a"],"uploadIDs":["b"]}`, `{"uploadIDs":["a"]} {}`} {
			response = performRequest(t, env.handler, http.MethodPost, target, origin, bad, cookies, headers)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("invalid envelope: %d %s", response.Code, response.Body.String())
			}
		}
		for _, denied := range []struct {
			origin  string
			cookies []*http.Cookie
			headers map[string]string
		}{{"https://foreign.example.test", cookies, headers}, {origin, cookies, nil}, {origin, nil, nil}} {
			response = performRequest(t, env.handler, http.MethodPost, target, denied.origin, string(body), denied.cookies, denied.headers)
			if response.Code != http.StatusForbidden && response.Code != http.StatusUnauthorized {
				t.Fatalf("missing browser boundary: %d", response.Code)
			}
		}
	}
}

func TestUploadLifecycleResponseByteBound(t *testing.T) {
	for _, items := range []any{map[string]string{"path": strings.Repeat("x", 1<<20)}, func() {}} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/uploads/batch/resume", nil)
		writeUploadLifecyclePage(response, request, items)
		if response.Code != http.StatusServiceUnavailable || response.Body.Len() > 4096 || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unbounded/uncacheable recovery response: status=%d bytes=%d", response.Code, response.Body.Len())
		}
	}
}

func TestIntegrationUploadActionsPartialOutcomesSelectiveRetryAndIsolation(t *testing.T) {
	env := newDriveHTTPEnvironment(t)
	const origin = "https://drive.example.test"
	cookies := []*http.Cookie{env.session, env.csrf}
	headers := driveMutationHeaders(env.csrf.Value, "independent-action-admit")
	created := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/batch", origin, `{"uploads":[{"path":"/action-good.bin","size":3,"mediaType":"application/octet-stream"},{"path":"/action-bad.bin","size":3,"mediaType":"application/octet-stream"},{"path":"/action-cancel.bin","size":3,"mediaType":"application/octet-stream"}]}`, cookies, headers)
	if created.Code != http.StatusCreated {
		t.Fatalf("admission: %d %s", created.Code, created.Body.String())
	}
	var admitted struct {
		Uploads []struct {
			Capability *domain.UploadCapability `json:"capability"`
		} `json:"uploads"`
	}
	decodeResponse(t, created, &admitted)
	for _, item := range admitted.Uploads {
		cap := item.Capability
		if cap == nil {
			t.Fatal("admission missing capability")
		}
		request, err := http.NewRequest(cap.Method, cap.URL, bytes.NewBufferString("abc"))
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range cap.Headers {
			request.Header.Set(key, value)
		}
		response, err := env.data.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("synthetic data: %d", response.StatusCode)
		}
	}
	items := []domain.UploadActionItem{{UploadID: admitted.Uploads[0].Capability.UploadID, Action: "complete", CRC32C: objectstore.FingerprintFor([]byte("abc")).CRC32C}, {UploadID: admitted.Uploads[1].Capability.UploadID, Action: "complete", CRC32C: objectstore.FingerprintFor([]byte("bad")).CRC32C}, {UploadID: admitted.Uploads[2].Capability.UploadID, Action: "cancel"}, {UploadID: "missing", Action: "cancel"}}
	body, _ := json.Marshal(map[string]any{"items": items})
	target := "/api/v1/uploads/batch/actions"
	response := performRequest(t, env.handler, http.MethodPost, target, origin, string(body), cookies, headers)
	if response.Code != http.StatusOK {
		t.Fatalf("actions: %d %s", response.Code, response.Body.String())
	}
	var decoded struct {
		Uploads []domain.UploadActionResult `json:"uploads"`
	}
	decodeResponse(t, response, &decoded)
	if len(decoded.Uploads) != 4 || decoded.Uploads[0].Outcome != "applied" || decoded.Uploads[1].Outcome != "rejected" || decoded.Uploads[2].Outcome != "applied" || decoded.Uploads[3].ErrorKind != domain.ErrorNotFound {
		t.Fatalf("partial outcomes: %+v", decoded)
	}
	body, _ = json.Marshal(map[string]any{"items": []domain.UploadActionItem{items[2], items[0]}})
	response = performRequest(t, env.handler, http.MethodPost, target, origin, string(body), cookies, headers)
	decodeResponse(t, response, &decoded)
	if response.Code != http.StatusOK || decoded.Uploads[0].Outcome != "already_applied" || decoded.Uploads[1].Outcome != "already_applied" {
		t.Fatalf("selective replay: %d %+v", response.Code, decoded)
	}
	foreign := performRequest(t, env.handler, http.MethodPost, target, origin, string(body), []*http.Cookie{env.otherSession, env.otherCSRF}, driveMutationHeaders(env.otherCSRF.Value, "foreign-actions-key"))
	decodeResponse(t, foreign, &decoded)
	if foreign.Code != http.StatusOK || decoded.Uploads[0].ErrorKind != domain.ErrorNotFound || decoded.Uploads[1].ErrorKind != domain.ErrorNotFound || strings.Contains(foreign.Body.String(), "action-good") {
		t.Fatalf("foreign actions: %d %s", foreign.Code, foreign.Body.String())
	}
	for _, bad := range []string{`{"items":[]}`, `{"items":[{"uploadID":"a","action":"cancel"},{"uploadID":"a","action":"complete","crc32c":"AAAAAA"}]}`, `{"items":[{"uploadID":"a","action":"cancel","ownerID":"other"}]}`, `{"items":[],"items":[]}`, `{"items":[]} {}`} {
		response = performRequest(t, env.handler, http.MethodPost, target, origin, bad, cookies, headers)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid action envelope: %d %s", response.Code, response.Body.String())
		}
	}
	for _, denied := range []struct {
		origin  string
		cookies []*http.Cookie
		headers map[string]string
	}{{"https://foreign.example.test", cookies, headers}, {origin, cookies, nil}, {origin, nil, nil}} {
		response = performRequest(t, env.handler, http.MethodPost, target, denied.origin, string(body), denied.cookies, denied.headers)
		if response.Code != http.StatusForbidden && response.Code != http.StatusUnauthorized {
			t.Fatalf("unprotected actions: %d", response.Code)
		}
	}
}
