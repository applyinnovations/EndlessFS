package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

// Admission can commit every independent intent before response expansion
// exceeds the envelope. Recovery queries use compact IDs, so their returned
// paths can exceed the same envelope even when the request is tiny.
func TestIntegrationUploadRecoveryResponseMaximumPathsRemainUsable(t *testing.T) {
	env := newDriveHTTPEnvironment(t)
	cookies := []*http.Cookie{env.session, env.csrf}
	headers := driveMutationHeaders(env.csrf.Value, "recovery-response-review")
	parent := ""
	for range 15 {
		parent += "/" + strings.Repeat("<", 255)
		body, _ := json.Marshal(map[string]any{"path": parent})
		response := performRequest(t, env.handler, http.MethodPost, "/api/v1/directories", reviewLifecycleOrigin, string(body), cookies, headers)
		if response.Code != http.StatusCreated {
			t.Fatalf("synthetic directory preparation: status=%d", response.Code)
		}
	}
	paths := make([]domain.UserPath, 48)
	requests := make([]map[string]any, len(paths))
	for index := range paths {
		path, err := domain.ParseUserPath(parent + "/" + strings.Repeat("<", 249) + fmt.Sprintf("r%05d", index))
		if err != nil || len(path.String()) != 4096 {
			t.Fatalf("invalid synthetic maximum path: bytes=%d err=%v", len(path.String()), err)
		}
		paths[index] = path
		requests[index] = map[string]any{"path": path.String(), "size": 4, "mediaType": "application/octet-stream", "resumable": true, "idempotencyKey": fmt.Sprintf("recovery-capacity-%03d", index)}
	}
	encode := func(value any) string {
		var body bytes.Buffer
		encoder := json.NewEncoder(&body)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(value); err != nil {
			t.Fatal(err)
		}
		if body.Len() > maxControlBodyBytes {
			t.Fatalf("synthetic request exceeds the actual envelope: bytes=%d", body.Len())
		}
		return body.String()
	}
	admissionBody := encode(map[string]any{"uploads": requests})
	first := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/batch/admit", reviewLifecycleOrigin, admissionBody, cookies, headers)
	if first.Code != http.StatusOK || first.Body.Len() > maxControlBodyBytes {
		t.Errorf("admitted valid cohort cannot be represented: status=%d request_bytes=%d response_bytes=%d", first.Code, len(admissionBody), first.Body.Len())
	}
	// Independent replay proves the original admitted all48 rather than
	// interpreting a503 as a denial or allocating replacement attempts.
	capabilities := make([]domain.UploadCapability, 0, len(paths))
	ids := make([]domain.UploadID, 0, len(paths))
	for first := 0; first < len(requests); first += 12 {
		response := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/batch/admit", reviewLifecycleOrigin, encode(map[string]any{"uploads": requests[first : first+12]}), cookies, headers)
		if response.Code != http.StatusOK {
			t.Fatalf("small independent replay: status=%d", response.Code)
		}
		var page struct {
			Uploads []domain.UploadAdmissionResult `json:"uploads"`
		}
		decodeResponse(t, response, &page)
		if len(page.Uploads) != 12 {
			t.Fatal("small replay lost admission outcomes")
		}
		for index, item := range page.Uploads {
			if item.Outcome != "already_accepted" || item.Capability == nil || item.Status == nil || item.Status.Path != paths[first+index] {
				t.Fatalf("original admission was not retained at index=%d outcome=%s", first+index, item.Outcome)
			}
			capabilities = append(capabilities, *item.Capability)
			ids = append(ids, item.UploadID)
		}
	}
	for _, endpoint := range []string{"status", "resume"} {
		response := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/batch/"+endpoint, reviewLifecycleOrigin, encode(map[string]any{"uploadIDs": ids}), cookies, headers)
		if response.Code != http.StatusOK || response.Body.Len() > maxControlBodyBytes {
			t.Errorf("valid recovery cohort cannot be represented: endpoint=%s status=%d response_bytes=%d", endpoint, response.Code, response.Body.Len())
			continue
		}
		var page struct {
			Uploads []domain.UploadLifecycleItem `json:"uploads"`
		}
		decodeResponse(t, response, &page)
		if len(page.Uploads) != len(ids) {
			t.Fatalf("recovery lost correlated results: endpoint=%s count=%d", endpoint, len(page.Uploads))
		}
		for index, item := range page.Uploads {
			if item.Index != index || item.UploadID != ids[index] || item.Status == nil || item.Status.State != domain.UploadStateActive || item.Status.ConfirmedOffset != 0 || item.ErrorKind != "" {
				t.Fatalf("recovery lost active state: endpoint=%s index=%d", endpoint, index)
			}
			if (item.Capability != nil) != (endpoint == "resume") {
				t.Fatalf("capability was lost or disclosed: endpoint=%s index=%d", endpoint, index)
			}
		}
	}
	// Actual bytes and completion distinguish usable recovery from a bounded
	// error-classification page. Only synthetic four-byte payloads are read.
	items := make([]domain.UploadActionItem, len(ids))
	for index, capability := range capabilities {
		request, err := http.NewRequest(capability.Method, capability.URL, bytes.NewReader(reviewLifecyclePayload))
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range capability.Headers {
			request.Header.Set(key, value)
		}
		response, err := env.data.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("actual payload index=%d status=%d", index, response.StatusCode)
		}
		items[index] = domain.UploadActionItem{UploadID: ids[index], Action: "complete", CRC32C: objectstore.FingerprintFor(reviewLifecyclePayload).CRC32C}
	}
	response := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/batch/actions", reviewLifecycleOrigin, encode(map[string]any{"items": items}), cookies, headers)
	if response.Code != http.StatusOK {
		t.Fatalf("terminal acknowledgements: status=%d", response.Code)
	}
	scope, _ := domain.NewScope(env.user, domain.AreaLive)
	for index, path := range paths {
		entry, err := env.storage.Stat(context.Background(), scope, path)
		if err != nil || entry.Size != 4 {
			t.Fatalf("authoritative file index=%d err=%v", index, err)
		}
	}
	if env.storage.Instrumentation().UploadBytes != 4*int64(len(paths)) {
		t.Fatal("recovery retransferred synthetic payloads")
	}
	t.Logf("bounded recovery admitted=%d actual_completed=%d actual_payload_bytes=%d admission_request_bytes=%d", len(ids), len(paths), env.storage.Instrumentation().UploadBytes, len(admissionBody))
}
