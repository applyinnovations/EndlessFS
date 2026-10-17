package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func TestIntegrationUploadAdmissionMixedResultsReplayAndBoundaries(t *testing.T) {
	env := newDriveHTTPEnvironment(t)
	const origin = "https://drive.example.test"
	const target = "/api/v1/uploads/batch/admit"
	cookies := []*http.Cookie{env.session, env.csrf}
	headers := driveMutationHeaders(env.csrf.Value, "admission-page-envelope")
	body := `{"defaults":{"path":"/","mediaType":"application/octet-stream","resumable":true},"uploads":[{"name":"a.bin","size":3,"idempotencyKey":"admission-http-item-a"},{"name":"invalid.bin","size":-1,"idempotencyKey":"admission-http-item-b"},{"name":"c.bin","size":3,"idempotencyKey":"admission-http-item-c"}]}`
	response := performRequest(t, env.handler, http.MethodPost, target, origin, body, cookies, headers)
	if response.Code != http.StatusOK {
		t.Fatalf("admission route: %d %s", response.Code, response.Body.String())
	}
	var page struct {
		Uploads []domain.UploadAdmissionResult `json:"uploads"`
	}
	decodeResponse(t, response, &page)
	if len(page.Uploads) != 3 || page.Uploads[0].Capability == nil || page.Uploads[1].ErrorKind != domain.ErrorInvalid || page.Uploads[2].Capability == nil || page.Uploads[1].RequestID == "" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("mixed admission: %+v", page)
	}
	original := page.Uploads[2].UploadID
	response = performRequest(t, env.handler, http.MethodPost, target, origin, `{"uploads":[{"path":"/","name":"c.bin","size":3,"mediaType":"application/octet-stream","resumable":true,"idempotencyKey":"admission-http-item-c"}]}`, cookies, headers)
	decodeResponse(t, response, &page)
	if response.Code != http.StatusOK || len(page.Uploads) != 1 || page.Uploads[0].UploadID != original {
		t.Fatalf("selective replay: %d %+v", response.Code, page)
	}
	for _, bad := range []string{`{"uploads":[]}`, `{"uploads":[{"name":"a","size":0}]}`, `{"uploads":[{"name":"a","size":0,"idempotencyKey":"duplicate-item-key"},{"name":"b","size":0,"idempotencyKey":"duplicate-item-key"}]}`, `{"uploads":[],"ownerID":"foreign"}`, `{"uploads":[],"uploads":[]}`, `{"uploads":[]} {}`} {
		r := performRequest(t, env.handler, http.MethodPost, target, origin, bad, cookies, headers)
		if r.Code != http.StatusBadRequest {
			t.Fatalf("invalid envelope: %d %s", r.Code, r.Body.String())
		}
	}
	oversized := make([]map[string]any, 101)
	for i := range oversized {
		oversized[i] = map[string]any{"name": "a", "size": 0, "idempotencyKey": "repeated-too-many-items"}
	}
	encoded, _ := json.Marshal(map[string]any{"uploads": oversized})
	response = performRequest(t, env.handler, http.MethodPost, target, origin, string(encoded), cookies, headers)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("page bound: %d", response.Code)
	}
	for _, denied := range []struct {
		origin  string
		cookies []*http.Cookie
		headers map[string]string
		want    int
	}{{"https://foreign.example", cookies, headers, http.StatusForbidden}, {origin, nil, headers, http.StatusUnauthorized}, {origin, cookies, map[string]string{}, http.StatusForbidden}} {
		r := performRequest(t, env.handler, http.MethodPost, target, denied.origin, body, denied.cookies, denied.headers)
		if r.Code != denied.want {
			t.Fatalf("mutation boundary: %d want%d %s", r.Code, denied.want, r.Body.String())
		}
	}
}
