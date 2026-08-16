package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/config"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/drive"
	"github.com/applyinnovations/endlessfs/internal/identity"
	"github.com/applyinnovations/endlessfs/internal/model"
	providermemory "github.com/applyinnovations/endlessfs/internal/provider/memory"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
)

type driveHTTPEnvironment struct {
	handler      http.Handler
	data         *httptest.Server
	storage      *providermemory.Provider
	session      *http.Cookie
	csrf         *http.Cookie
	otherSession *http.Cookie
	otherCSRF    *http.Cookie
}

func newDriveHTTPEnvironment(t *testing.T) driveHTTPEnvironment {
	t.Helper()
	ctx := context.Background()
	store := state.NewMemoryStore()
	repository := identity.NewRepository(store)
	ids := domain.NewIDGenerator(&httpDeterministicReader{next: 1})
	clock := domain.NewFixedClock(time.Date(2032, 1, 2, 3, 4, 5, 0, time.UTC))
	origin := "https://drive.example.test"
	protection := secret.Value(httpBearer(0x65))
	sessions, err := auth.NewSessionManager(repository, ids, clock, 12*time.Hour, origin, true, protection)
	if err != nil {
		t.Fatal(err)
	}
	storage := providermemory.New(providermemory.Options{Clock: clock, IDs: ids, AllowedOrigin: origin})
	data := httptest.NewServer(storage)
	t.Cleanup(data.Close)
	if err := storage.SetDataPlaneBaseURL(data.URL); err != nil {
		t.Fatal(err)
	}
	service, err := drive.NewService(storage, store, repository, ids, clock, protection, origin, data.URL, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	users := []domain.UserID{httpUserID(t, 0x51), httpUserID(t, 0x61)}
	issued := make([]auth.IssuedSession, 2)
	for index, userID := range users {
		now := clock.Now()
		if err := repository.CreateAccount(ctx, model.Account{SchemaVersion: model.SchemaVersion, UserID: userID, Status: model.AccountEnabled, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		issued[index], err = sessions.Issue(ctx, userID, base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{byte(index + 1)}, 32)))
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{BaseURL: origin, AllowedOrigin: origin, Secure: true}
	return driveHTTPEnvironment{handler: NewCompleteApplication(cfg, "test", nil, sessions, service), data: data, storage: storage, session: sessions.Cookie(issued[0]), csrf: sessions.CSRFCookie(issued[0]), otherSession: sessions.Cookie(issued[1]), otherCSRF: sessions.CSRFCookie(issued[1])}
}

func httpUserID(t *testing.T, fill byte) domain.UserID {
	t.Helper()
	value, err := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func driveMutationHeaders(csrf, key string) map[string]string {
	headers := map[string]string{"Content-Type": "application/json", csrfHeader: csrf}
	if key != "" {
		headers["Idempotency-Key"] = key
	}
	return headers
}

func TestIntegrationFileHTTPDirectDataPathTrashAndShare(t *testing.T) {
	env := newDriveHTTPEnvironment(t)
	origin := "https://drive.example.test"
	cookies := []*http.Cookie{env.session, env.csrf}
	createdDirectory := performRequest(t, env.handler, http.MethodPost, "/api/v1/directories", origin, `{"path":"/docs"}`, cookies, driveMutationHeaders(env.csrf.Value, ""))
	if createdDirectory.Code != http.StatusCreated {
		t.Fatalf("directory = %d %s", createdDirectory.Code, createdDirectory.Body.String())
	}
	missingKey := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads", origin, `{"path":"/docs/file.txt","size":5,"mediaType":"text/plain"}`, cookies, driveMutationHeaders(env.csrf.Value, ""))
	if missingKey.Code != http.StatusBadRequest {
		t.Fatalf("missing key = %d", missingKey.Code)
	}
	createdUpload := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads", origin, `{"path":"/docs/file.txt","size":5,"mediaType":"text/plain","resumable":true}`, cookies, driveMutationHeaders(env.csrf.Value, "http-upload-request-0001"))
	if createdUpload.Code != http.StatusCreated {
		t.Fatalf("upload init = %d %s", createdUpload.Code, createdUpload.Body.String())
	}
	var uploadCapability domain.UploadCapability
	decodeResponse(t, createdUpload, &uploadCapability)
	uploadRequest, _ := http.NewRequest(uploadCapability.Method, uploadCapability.URL, bytes.NewBufferString("hello"))
	for name, value := range uploadCapability.Headers {
		uploadRequest.Header.Set(name, value)
	}
	uploadRequest.Header.Set("Origin", origin)
	uploadResponse, err := env.data.Client().Do(uploadRequest)
	if err != nil {
		t.Fatal(err)
	}
	uploadResponse.Body.Close()
	if uploadResponse.StatusCode != http.StatusNoContent || uploadResponse.Header.Get("Access-Control-Allow-Origin") != origin {
		t.Fatalf("data upload = %d %v", uploadResponse.StatusCode, uploadResponse.Header)
	}
	complete := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/"+string(uploadCapability.UploadID)+"/complete", origin, `{"path":"/docs/file.txt","size":5,"mediaType":"text/plain"}`, cookies, driveMutationHeaders(env.csrf.Value, ""))
	if complete.Code != http.StatusOK {
		t.Fatalf("complete = %d %s", complete.Code, complete.Body.String())
	}
	var entry domain.Entry
	decodeResponse(t, complete, &entry)
	listing := performRequest(t, env.handler, http.MethodGet, "/api/v1/files?path=/docs", "", "", []*http.Cookie{env.session}, nil)
	if listing.Code != http.StatusOK || !bytes.Contains(listing.Body.Bytes(), []byte("file.txt")) {
		t.Fatalf("listing = %d %s", listing.Code, listing.Body.String())
	}
	otherListing := performRequest(t, env.handler, http.MethodGet, "/api/v1/files?path=/docs", "", "", []*http.Cookie{env.otherSession}, nil)
	if otherListing.Code != http.StatusNotFound {
		t.Fatalf("cross-user listing = %d", otherListing.Code)
	}
	downloadBody, _ := json.Marshal(map[string]any{"path": "/docs/file.txt", "version": entry.Version, "preview": true})
	download := performRequest(t, env.handler, http.MethodPost, "/api/v1/downloads", origin, string(downloadBody), cookies, driveMutationHeaders(env.csrf.Value, ""))
	if download.Code != http.StatusCreated {
		t.Fatalf("download init = %d %s", download.Code, download.Body.String())
	}
	var downloadEnvelope struct {
		Capability domain.DownloadCapability `json:"capability"`
		Mode       string                    `json:"mode"`
	}
	decodeResponse(t, download, &downloadEnvelope)
	if downloadEnvelope.Mode != "text" {
		t.Fatalf("preview mode = %q", downloadEnvelope.Mode)
	}
	downloadRequest, _ := http.NewRequest(http.MethodGet, downloadEnvelope.Capability.URL, nil)
	downloadRequest.Header.Set("Origin", origin)
	response, err := env.data.Client().Do(downloadRequest)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "hello" || response.Header.Get("Content-Disposition")[:6] != "inline" {
		t.Fatalf("direct download = %q %v", body, response.Header)
	}
	share := performRequest(t, env.handler, http.MethodPost, "/api/v1/shares", origin, `{"path":"/docs"}`, cookies, driveMutationHeaders(env.csrf.Value, "http-share-request-00001"))
	if share.Code != http.StatusCreated {
		t.Fatalf("share = %d %s", share.Code, share.Body.String())
	}
	var shareEnvelope struct {
		Share model.Share `json:"share"`
		Link  string      `json:"link"`
	}
	decodeResponse(t, share, &shareEnvelope)
	token := shareEnvelope.Link[len(origin+"/s/"):]
	public := performRequest(t, env.handler, http.MethodGet, "/api/v1/public/shares/"+token+"?path=/", "", "", nil, nil)
	if public.Code != http.StatusOK || public.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("public share = %d %v %s", public.Code, public.Header(), public.Body.String())
	}
	trashed := performRequest(t, env.handler, http.MethodPost, "/api/v1/files/trash", origin, `{"paths":["/docs"]}`, cookies, driveMutationHeaders(env.csrf.Value, "http-trash-request-00001"))
	if trashed.Code != http.StatusAccepted {
		t.Fatalf("trash = %d %s", trashed.Code, trashed.Body.String())
	}
	unavailable := performRequest(t, env.handler, http.MethodGet, "/api/v1/public/shares/"+token+"?path=/", "", "", nil, nil)
	if unavailable.Code != http.StatusNotFound {
		t.Fatalf("trashed share = %d", unavailable.Code)
	}
	metrics := env.storage.Instrumentation()
	if metrics.ControlPlaneBytes != 0 || metrics.UploadBytes != 5 || metrics.DownloadBytes != 5 {
		t.Fatalf("byte paths = %+v", metrics)
	}
	if csp := createdDirectory.Header().Get("Content-Security-Policy"); csp == "" || !bytes.Contains([]byte(csp), []byte(env.data.URL)) {
		t.Fatalf("CSP = %q", csp)
	}
}

func TestFileHTTPRejectsProviderFieldsBodiesAndTraversalBeforeProvider(t *testing.T) {
	env := newDriveHTTPEnvironment(t)
	origin := "https://drive.example.test"
	cookies := []*http.Cookie{env.session, env.csrf}
	before := env.storage.Instrumentation()
	for name, testCase := range map[string][2]string{
		"provider key": {"/api/v1/directories", `{"path":"/safe","providerKey":"users/other/file"}`},
		"dot segment":  {"/api/v1/directories", `{"path":"/../escape"}`},
		"backslash":    {"/api/v1/directories", `{"path":"/safe\\escape"}`},
		"reserved":     {"/api/v1/directories", `{"path":"/.endlessfs/metadata"}`},
	} {
		t.Run(name, func(t *testing.T) {
			response := performRequest(t, env.handler, http.MethodPost, testCase[0], origin, testCase[1], cookies, driveMutationHeaders(env.csrf.Value, ""))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
			}
			assertProblem(t, response)
		})
	}
	after := env.storage.Instrumentation()
	if after.ProviderCalls[providermemory.OperationCreateDirectory] != before.ProviderCalls[providermemory.OperationCreateDirectory] {
		t.Fatalf("invalid requests reached provider: before=%+v after=%+v", before, after)
	}
	fileBody := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads", origin, "raw file bytes", cookies, map[string]string{csrfHeader: env.csrf.Value, "Content-Type": "application/octet-stream", "Idempotency-Key": "http-upload-request-0002"})
	if fileBody.Code != http.StatusBadRequest {
		t.Fatalf("file body status = %d", fileBody.Code)
	}
}
