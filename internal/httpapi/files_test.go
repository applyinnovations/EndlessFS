package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/config"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/drive"
	"github.com/applyinnovations/endlessfs/internal/identity"
	endlesslogging "github.com/applyinnovations/endlessfs/internal/logging"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/preview"
	"github.com/applyinnovations/endlessfs/internal/preview/imagegen"
	previewmemory "github.com/applyinnovations/endlessfs/internal/preview/memory"
	providermemory "github.com/applyinnovations/endlessfs/internal/provider/memory"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/theme"
)

type driveHTTPEnvironment struct {
	handler       http.Handler
	data          *httptest.Server
	storage       *providermemory.Provider
	session       *http.Cookie
	csrf          *http.Cookie
	otherSession  *http.Cookie
	otherCSRF     *http.Cookie
	themes        *theme.Manager
	previews      *preview.Service
	previewStore  *previewmemory.Store
	previewOrigin string
	logs          *bytes.Buffer
	user          domain.UserID
	otherUser     domain.UserID
}

func newDriveHTTPEnvironment(t *testing.T) driveHTTPEnvironment {
	return newDriveHTTPEnvironmentConfigured(t, false)
}

func newDriveHTTPEnvironmentWithPreviews(t *testing.T) driveHTTPEnvironment {
	return newDriveHTTPEnvironmentConfigured(t, true)
}

func newDriveHTTPEnvironmentConfigured(t *testing.T, withPreviews bool) driveHTTPEnvironment {
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
	var previewStore *previewmemory.Store
	if withPreviews {
		previewStore, err = previewmemory.New(previewmemory.Options{Clock: clock, IDs: ids, Key: protection, AllowedOrigin: origin})
		if err != nil {
			t.Fatal(err)
		}
	}
	data := httptest.NewServer(storage)
	t.Cleanup(data.Close)
	if err := storage.SetDataPlaneBaseURL(data.URL); err != nil {
		t.Fatal(err)
	}
	previewOrigin := ""
	if previewStore != nil {
		previewData := httptest.NewServer(previewStore)
		t.Cleanup(previewData.Close)
		previewOrigin = previewData.URL
		if err := previewStore.SetDataPlaneBaseURL(previewOrigin); err != nil {
			t.Fatal(err)
		}
	}
	service, err := drive.NewService(storage, store, repository, ids, clock, protection, origin, data.URL, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := theme.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	themeManager, err := theme.NewManager(registry, store, "endlessfs-light", "endlessfs-dark", true, clock)
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
	var previewService *preview.Service
	var handler http.Handler
	var logOutput bytes.Buffer
	if withPreviews {
		cfg.PreviewProvider = "mock"
		cfg.PreviewAutomatic = true
		cfg.PreviewFormats = []string{"image"}
		cfg.PreviewResolutions = []int{256, 512, 1600}
		previewService, err = preview.NewService(preview.Options{Automatic: true, Resolutions: cfg.PreviewResolutions, ApplicationState: store}, storage, previewStore, []preview.Generator{imagegen.New(imagegen.Options{})}, data.Client(), ids, clock)
		if err != nil {
			t.Fatal(err)
		}
		handler = NewCompleteApplicationWithPreviewAndLogger(cfg, "test", nil, sessions, service, previewService, endlesslogging.NewJSON(&logOutput, 0), themeManager)
	} else {
		handler = NewCompleteApplication(cfg, "test", nil, sessions, service, themeManager)
	}
	return driveHTTPEnvironment{handler: handler, data: data, storage: storage, session: sessions.Cookie(issued[0]), csrf: sessions.CSRFCookie(issued[0]), otherSession: sessions.Cookie(issued[1]), otherCSRF: sessions.CSRFCookie(issued[1]), themes: themeManager, previews: previewService, previewStore: previewStore, previewOrigin: previewOrigin, logs: &logOutput, user: users[0], otherUser: users[1]}
}

func httpUserID(t *testing.T, fill byte) domain.UserID {
	t.Helper()
	value, err := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func uploadHTTPPreviewImage(t *testing.T, env driveHTTPEnvironment, pathValue string) domain.Entry {
	t.Helper()
	value := image.NewNRGBA(image.Rect(0, 0, 24, 12))
	for y := range 12 {
		for x := range 24 {
			value.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 9), G: uint8(y * 13), B: 80, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, value); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	scope, err := domain.NewScope(env.user, domain.AreaLive)
	if err != nil {
		t.Fatal(err)
	}
	path := domain.MustParseUserPath(pathValue)
	upload, err := env.storage.CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: path, Size: int64(len(data)), MediaType: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(upload.Method, upload.URL, bytes.NewReader(data))
	for name, header := range upload.Headers {
		request.Header.Set(name, header)
	}
	response, err := env.data.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("preview fixture upload = %d", response.StatusCode)
	}
	entry, err := env.storage.CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: upload.UploadID, Path: path, Size: int64(len(data)), MediaType: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	return entry
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
	var listed domain.ListPage
	decodeResponse(t, listing, &listed)
	if listed.Current.Path != domain.MustParseUserPath("/docs") || listed.Current.Size != 5 || listed.Current.FileCount != 1 {
		t.Fatalf("listing current directory = %+v; want /docs size/count 5/1", listed.Current)
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
	var publicPage drive.PublicPage
	decodeResponse(t, public, &publicPage)
	if publicPage.Root.Path != "/" || publicPage.Current.Path != "/" || publicPage.Current.Size != 5 || publicPage.Current.FileCount != 1 {
		t.Fatalf("public current target = %+v; root=%+v", publicPage.Current, publicPage.Root)
	}
	publicStat := performRequest(t, env.handler, http.MethodGet, "/api/v1/public/shares/"+token+"/stat?path=/file.txt", "", "", nil, nil)
	var publicEntry drive.PublicEntry
	decodeResponse(t, publicStat, &publicEntry)
	if publicStat.Code != http.StatusOK || publicEntry.Path != "/file.txt" || publicEntry.Kind != domain.EntryFile || publicEntry.Version != entry.Version || publicEntry.Size != 5 {
		t.Fatalf("public stat = %d %+v %s", publicStat.Code, publicEntry, publicStat.Body.String())
	}
	publicStatTraversal := performRequest(t, env.handler, http.MethodGet, "/api/v1/public/shares/"+token+"/stat?path=/../outside", "", "", nil, nil)
	if publicStatTraversal.Code != http.StatusNotFound {
		t.Fatalf("public stat traversal = %d %s", publicStatTraversal.Code, publicStatTraversal.Body.String())
	}
	trashed := performRequest(t, env.handler, http.MethodPost, "/api/v1/files/trash", origin, `{"paths":["/docs"]}`, cookies, driveMutationHeaders(env.csrf.Value, "http-trash-request-00001"))
	if trashed.Code != http.StatusAccepted {
		t.Fatalf("trash = %d %s", trashed.Code, trashed.Body.String())
	}
	trashListing := performRequest(t, env.handler, http.MethodGet, "/api/v1/trash", "", "", []*http.Cookie{env.session}, nil)
	var trashPage drive.TrashPage
	decodeResponse(t, trashListing, &trashPage)
	if trashListing.Code != http.StatusOK || len(trashPage.Items) != 1 || trashPage.Items[0].Size != 5 || trashPage.Items[0].FileCount != 1 || trashPage.Items[0].MediaType != "" {
		t.Fatalf("trash metadata response = %d %+v %s", trashListing.Code, trashPage, trashListing.Body.String())
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

func TestIntegrationUploadPlanningRoutesAreStrictOwnerScopedMetadataQueries(t *testing.T) {
	env := newDriveHTTPEnvironment(t)
	origin := "https://drive.example.test"
	cookies := []*http.Cookie{env.session, env.csrf}
	headers := driveMutationHeaders(env.csrf.Value, "")
	missingSession := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/plan/sizes", origin, `{"items":[{"id":"item-1","path":"/photo.jpg","size":12}]}`, nil, headers)
	if missingSession.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous upload plan = %d %s", missingSession.Code, missingSession.Body.String())
	}
	unknown := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/plan/sizes", origin, `{"items":[{"id":"item-1","path":"/photo.jpg","size":12,"providerKey":"forbidden"}]}`, cookies, headers)
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("upload plan unknown field = %d %s", unknown.Code, unknown.Body.String())
	}
	valid := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/plan/sizes", origin, `{"items":[{"id":"item-1","path":"/photo.jpg","size":12}]}`, cookies, headers)
	if valid.Code != http.StatusServiceUnavailable || strings.Contains(valid.Body.String(), "providerKey") {
		t.Fatalf("upload plan optional-provider boundary = %d %s", valid.Code, valid.Body.String())
	}
	invalidFingerprint := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/plan/fingerprints", origin, `{"token":"opaque","items":[{"id":"item-1","path":"/photo.jpg","size":12,"md5":"bad","crc32c":"bad"}]}`, cookies, headers)
	if invalidFingerprint.Code != http.StatusServiceUnavailable {
		// The HTTP layer is transport-only; the portable implementation owns
		// fingerprint validation. A minimal provider deliberately reports the
		// optional planning feature as unavailable without echoing the body.
		t.Fatalf("upload fingerprint optional-provider boundary = %d %s", invalidFingerprint.Code, invalidFingerprint.Body.String())
	}
}

func TestIntegrationStorageMapHierarchyIsAuthenticatedAndOwnerScoped(t *testing.T) {
	env := newDriveHTTPEnvironment(t)
	ctx := context.Background()
	scope, err := domain.NewScope(env.user, domain.AreaLive)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/Projects", "/Projects/Assets"} {
		if _, err := env.storage.CreateDirectory(ctx, scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(path)}); err != nil {
			t.Fatal(err)
		}
	}
	uploadHTTPPreviewImage(t, env, "/Projects/Assets/Hero.png")

	unauthenticated := performRequest(t, env.handler, http.MethodGet, "/api/v1/files/storage-map?path=/", "", "", nil, nil)
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated storage map = %d", unauthenticated.Code)
	}
	response := performRequest(t, env.handler, http.MethodGet, "/api/v1/files/storage-map?path=/", "", "", []*http.Cookie{env.session}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("storage map = %d %s", response.Code, response.Body.String())
	}
	var page drive.StorageMapPage
	decodeResponse(t, response, &page)
	if len(page.Entries) != 1 || page.Entries[0].Path != domain.MustParseUserPath("/Projects") || len(page.Entries[0].Children) != 1 || page.Entries[0].Children[0].Path != domain.MustParseUserPath("/Projects/Assets") {
		t.Fatalf("storage map hierarchy = %+v", page)
	}
	other := performRequest(t, env.handler, http.MethodGet, "/api/v1/files/storage-map?path=/", "", "", []*http.Cookie{env.otherSession}, nil)
	if other.Code != http.StatusOK || bytes.Contains(other.Body.Bytes(), []byte("Projects")) {
		t.Fatalf("cross-owner storage map = %d %s", other.Code, other.Body.String())
	}
	invalid := performRequest(t, env.handler, http.MethodGet, "/api/v1/files/storage-map?path=relative", "", "", []*http.Cookie{env.session}, nil)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid storage map path = %d", invalid.Code)
	}
}

func TestIntegrationGeneratedPreviewResolveRegenerateAndAuthorization(t *testing.T) {
	env := newDriveHTTPEnvironmentWithPreviews(t)
	const origin = "https://drive.example.test"
	entry := uploadHTTPPreviewImage(t, env, "/photo.png")
	cookies := []*http.Cookie{env.session, env.csrf}
	body, _ := json.Marshal(map[string]any{"items": []map[string]any{{"path": entry.Path.String(), "version": entry.Version, "variant": 256}}})

	missingCSRF := performRequest(t, env.handler, http.MethodPost, "/api/v1/previews/resolve", origin, string(body), []*http.Cookie{env.session}, map[string]string{"Content-Type": "application/json"})
	if missingCSRF.Code != http.StatusForbidden {
		t.Fatalf("resolve without CSRF = %d %s", missingCSRF.Code, missingCSRF.Body.String())
	}
	resolved := performRequest(t, env.handler, http.MethodPost, "/api/v1/previews/resolve", origin, string(body), cookies, driveMutationHeaders(env.csrf.Value, ""))
	if resolved.Code != http.StatusOK {
		t.Fatalf("resolve = %d %s", resolved.Code, resolved.Body.String())
	}
	if csp := resolved.Header().Get("Content-Security-Policy"); env.previewOrigin == env.data.URL || !strings.Contains(csp, env.data.URL) || !strings.Contains(csp, env.previewOrigin) {
		t.Fatalf("preview CSP origins = %q, source=%q preview=%q", csp, env.data.URL, env.previewOrigin)
	}
	var resolveResponse preview.ResolveResponse
	decodeResponse(t, resolved, &resolveResponse)
	if len(resolveResponse.Items) != 1 || resolveResponse.Items[0].State != preview.StateReady || resolveResponse.Items[0].Artifact == nil || resolveResponse.Items[0].Artifact.ContentType != "image/webp" || resolveResponse.Items[0].Capability == nil {
		t.Fatalf("resolve response = %+v", resolveResponse)
	}
	if bytes.Contains(resolved.Body.Bytes(), []byte("contentID")) || bytes.Contains(resolved.Body.Bytes(), []byte(string(entry.ContentID))) {
		t.Fatalf("resolve exposed private content identity: %s", resolved.Body.String())
	}
	artifactRequest, _ := http.NewRequest(resolveResponse.Items[0].Capability.Method, resolveResponse.Items[0].Capability.URL, nil)
	artifactRequest.Header.Set("Origin", origin)
	artifactResponse, err := env.data.Client().Do(artifactRequest)
	if err != nil {
		t.Fatal(err)
	}
	artifactBytes, _ := io.ReadAll(artifactResponse.Body)
	artifactResponse.Body.Close()
	if artifactResponse.StatusCode != http.StatusOK || artifactResponse.Header.Get("Content-Type") != "image/webp" || string(artifactBytes[:4]) != "RIFF" {
		t.Fatalf("artifact = %d %v %q", artifactResponse.StatusCode, artifactResponse.Header, artifactBytes)
	}

	regenerateBody, _ := json.Marshal(map[string]any{"path": entry.Path.String(), "version": entry.Version, "variant": 256, "action": "regenerate"})
	regenerated := performRequest(t, env.handler, http.MethodPost, "/api/v1/previews/generations", origin, string(regenerateBody), cookies, driveMutationHeaders(env.csrf.Value, "preview-http-regenerate-0001"))
	if regenerated.Code != http.StatusAccepted {
		t.Fatalf("regenerate = %d %s", regenerated.Code, regenerated.Body.String())
	}
	var operation preview.Operation
	decodeResponse(t, regenerated, &operation)
	if operation.State != domain.OperationSucceeded || operation.Result == nil || operation.Result.Artifact.GenerationID == resolveResponse.Items[0].Artifact.GenerationID {
		t.Fatalf("regenerate operation = %+v", operation)
	}
	polled := performRequest(t, env.handler, http.MethodGet, "/api/v1/previews/operations/"+string(operation.ID), "", "", []*http.Cookie{env.session}, nil)
	if polled.Code != http.StatusOK {
		t.Fatalf("operation poll = %d %s", polled.Code, polled.Body.String())
	}
	other := performRequest(t, env.handler, http.MethodPost, "/api/v1/previews/resolve", origin, string(body), []*http.Cookie{env.otherSession, env.otherCSRF}, driveMutationHeaders(env.otherCSRF.Value, ""))
	if other.Code != http.StatusNotFound {
		t.Fatalf("cross-owner resolve = %d %s", other.Code, other.Body.String())
	}
	otherOperation := performRequest(t, env.handler, http.MethodGet, "/api/v1/previews/operations/"+string(operation.ID), "", "", []*http.Cookie{env.otherSession}, nil)
	if otherOperation.Code != http.StatusNotFound {
		t.Fatalf("cross-owner operation = %d", otherOperation.Code)
	}
}

func TestIntegrationPreviewRuntimeLossFailsReadinessButNotFileListing(t *testing.T) {
	env := newDriveHTTPEnvironmentWithPreviews(t)
	const origin = "https://drive.example.test"
	entry := uploadHTTPPreviewImage(t, env, "/ready.png")
	env.previewStore.SetAvailable(false)
	body, _ := json.Marshal(map[string]any{"items": []map[string]any{{"path": entry.Path.String(), "version": entry.Version, "variant": 256}}})
	resolve := performRequest(t, env.handler, http.MethodPost, "/api/v1/previews/resolve", origin, string(body), []*http.Cookie{env.session, env.csrf}, driveMutationHeaders(env.csrf.Value, ""))
	if resolve.Code != http.StatusOK || !bytes.Contains(resolve.Body.Bytes(), []byte(`"state":"unavailable"`)) {
		t.Fatalf("unavailable resolve = %d %s", resolve.Code, resolve.Body.String())
	}
	if logValue := env.logs.String(); !strings.Contains(logValue, `"msg":"preview_unavailable"`) || !strings.Contains(logValue, `"category":"preview_store"`) || strings.Contains(logValue, entry.Path.String()) || strings.Contains(logValue, string(entry.ContentID)) {
		t.Fatalf("preview loss log was not safe and loud: %s", logValue)
	}
	ready := performRequest(t, env.handler, http.MethodGet, "/readyz", "", "", nil, nil)
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness after preview loss = %d %s", ready.Code, ready.Body.String())
	}
	listing := performRequest(t, env.handler, http.MethodGet, "/api/v1/files?path=/", "", "", []*http.Cookie{env.session}, nil)
	if listing.Code != http.StatusOK || !bytes.Contains(listing.Body.Bytes(), []byte("ready.png")) {
		t.Fatalf("authoritative listing during preview loss = %d %s", listing.Code, listing.Body.String())
	}
	env.previewStore.SetAvailable(true)
	ready = performRequest(t, env.handler, http.MethodGet, "/readyz", "", "", nil, nil)
	if ready.Code != http.StatusOK {
		t.Fatalf("readiness after revalidation = %d %s", ready.Code, ready.Body.String())
	}
}

func TestPreviewHTTPRejectsUnexpectedFieldsAndStaleVersions(t *testing.T) {
	env := newDriveHTTPEnvironmentWithPreviews(t)
	const origin = "https://drive.example.test"
	entry := uploadHTTPPreviewImage(t, env, "/strict.png")
	cookies := []*http.Cookie{env.session, env.csrf}
	unexpected := performRequest(t, env.handler, http.MethodPost, "/api/v1/previews/resolve", origin, `{"items":[{"path":"/strict.png","version":"stale","variant":256,"bucket":"private"}]}`, cookies, driveMutationHeaders(env.csrf.Value, ""))
	if unexpected.Code != http.StatusBadRequest {
		t.Fatalf("unexpected field = %d %s", unexpected.Code, unexpected.Body.String())
	}
	stale := performRequest(t, env.handler, http.MethodPost, "/api/v1/previews/resolve", origin, `{"items":[{"path":"/strict.png","version":"stale","variant":256}]}`, cookies, driveMutationHeaders(env.csrf.Value, ""))
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale resolve = %d %s, current=%s", stale.Code, stale.Body.String(), entry.Version)
	}
}

func TestFileHTTPRejectsProviderFieldsBodiesAndTraversalBeforeProvider(t *testing.T) {
	env := newDriveHTTPEnvironment(t)
	origin := "https://drive.example.test"
	cookies := []*http.Cookie{env.session, env.csrf}
	before := env.storage.Instrumentation()
	for name, testCase := range map[string][2]string{
		"provider key":              {"/api/v1/directories", `{"path":"/safe","providerKey":"users/other/file"}`},
		"dot segment":               {"/api/v1/directories", `{"path":"/../escape"}`},
		"backslash":                 {"/api/v1/directories", `{"path":"/safe\\escape"}`},
		"reserved":                  {"/api/v1/directories", `{"path":"/.endlessfs/metadata"}`},
		"overlap provider identity": {"/api/v1/duplicates/directories/overlaps", `{"directory":{"area":"live","path":"/safe"},"bucket":"private"}`},
	} {
		t.Run(name, func(t *testing.T) {
			response := performRequest(t, env.handler, http.MethodPost, testCase[0], origin, testCase[1], cookies, driveMutationHeaders(env.csrf.Value, ""))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
			}
			assertProblem(t, response)
		})
	}
	pairIgnore := performRequest(t, env.handler, http.MethodPut, "/api/v1/duplicates/directories/ignore", origin, `{"left":{"area":"live","path":"/safe"},"right":{"area":"live","path":"/../escape"},"ignored":true}`, cookies, driveMutationHeaders(env.csrf.Value, ""))
	if pairIgnore.Code != http.StatusBadRequest {
		t.Fatalf("pair ignore traversal = %d %s", pairIgnore.Code, pairIgnore.Body.String())
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

func TestIntegrationBatchUploadEmptyTrashAndPublicDownloadRoutes(t *testing.T) {
	env := newDriveHTTPEnvironment(t)
	const origin = "https://drive.example.test"
	cookies := []*http.Cookie{env.session, env.csrf}
	headers := driveMutationHeaders(env.csrf.Value, "batch-upload-route-0001")

	emptyBatch := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/batch", origin, `{"uploads":[]}`, cookies, headers)
	if emptyBatch.Code != http.StatusBadRequest {
		t.Fatalf("empty batch = %d %s", emptyBatch.Code, emptyBatch.Body.String())
	}
	batch := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/batch", origin, `{"uploads":[{"path":"/one.txt","size":3,"mediaType":"text/plain"},{"path":"relative","size":1,"mediaType":"text/plain"}]}`, cookies, headers)
	if batch.Code != http.StatusCreated || !bytes.Contains(batch.Body.Bytes(), []byte(`"capability"`)) || !bytes.Contains(batch.Body.Bytes(), []byte(`"errorKind":"invalid"`)) {
		t.Fatalf("mixed batch = %d %s", batch.Code, batch.Body.String())
	}

	entry, _ := createHTTPFile(t, env, env.session, env.csrf, "/public.txt", "public", "public-download-upload-1")
	share := performRequest(t, env.handler, http.MethodPost, "/api/v1/shares", origin, `{"path":"/public.txt"}`, cookies, driveMutationHeaders(env.csrf.Value, "public-download-share-01"))
	if share.Code != http.StatusCreated {
		t.Fatalf("share = %d %s", share.Code, share.Body.String())
	}
	var shareEnvelope struct {
		Link string `json:"link"`
	}
	decodeResponse(t, share, &shareEnvelope)
	token := strings.TrimPrefix(shareEnvelope.Link, origin+"/s/")
	downloadBody, _ := json.Marshal(map[string]any{"path": "/", "version": entry.Version, "preview": true})
	denied := performRequest(t, env.handler, http.MethodPost, "/api/v1/public/shares/"+token+"/downloads", "https://attacker.example", string(downloadBody), nil, map[string]string{"Content-Type": "application/json"})
	if denied.Code != http.StatusForbidden {
		t.Fatalf("cross-origin public download = %d %s", denied.Code, denied.Body.String())
	}
	download := performRequest(t, env.handler, http.MethodPost, "/api/v1/public/shares/"+token+"/downloads", origin, string(downloadBody), nil, map[string]string{"Content-Type": "application/json"})
	if download.Code != http.StatusCreated || !bytes.Contains(download.Body.Bytes(), []byte(`"mode":"text"`)) {
		t.Fatalf("public download = %d %s", download.Code, download.Body.String())
	}

	trashed := performRequest(t, env.handler, http.MethodPost, "/api/v1/files/trash", origin, `{"paths":["/public.txt"]}`, cookies, driveMutationHeaders(env.csrf.Value, "empty-trash-route-00001"))
	if trashed.Code != http.StatusAccepted {
		t.Fatalf("trash = %d %s", trashed.Code, trashed.Body.String())
	}
	listing := performRequest(t, env.handler, http.MethodGet, "/api/v1/trash", "", "", []*http.Cookie{env.session}, nil)
	if listing.Code != http.StatusOK || !bytes.Contains(listing.Body.Bytes(), []byte("public.txt")) || !bytes.Contains(listing.Body.Bytes(), []byte(`"mediaType":"text/plain"`)) || !bytes.Contains(listing.Body.Bytes(), []byte(`"size":6`)) {
		t.Fatalf("trash listing = %d %s", listing.Code, listing.Body.String())
	}
	unconfirmed := performRequest(t, env.handler, http.MethodPost, "/api/v1/trash/empty", origin, `{"confirm":false}`, cookies, driveMutationHeaders(env.csrf.Value, "empty-trash-route-00002"))
	if unconfirmed.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed empty = %d %s", unconfirmed.Code, unconfirmed.Body.String())
	}
	empty := performRequest(t, env.handler, http.MethodPost, "/api/v1/trash/empty", origin, `{"confirm":true}`, cookies, driveMutationHeaders(env.csrf.Value, "empty-trash-route-00003"))
	if empty.Code != http.StatusAccepted {
		t.Fatalf("empty trash = %d %s", empty.Code, empty.Body.String())
	}

	shell := performRequest(t, env.handler, http.MethodGet, "/s/"+token, "", "", nil, nil)
	if shell.Code != http.StatusOK || !bytes.Contains(shell.Body.Bytes(), []byte("EndlessFS")) {
		t.Fatalf("public shell = %d %s", shell.Code, shell.Body.String())
	}
}

func TestIntegrationSelectedTrashRestoreAndDeleteUseAtomicBatchRoutes(t *testing.T) {
	env := newDriveHTTPEnvironment(t)
	const origin = "https://drive.example.test"
	cookies := []*http.Cookie{env.session, env.csrf}
	createHTTPFile(t, env, env.session, env.csrf, "/batch-one.txt", "one", "batch-route-upload-0001")
	createHTTPFile(t, env, env.session, env.csrf, "/batch-two.txt", "two", "batch-route-upload-0002")

	trash := performRequest(t, env.handler, http.MethodPost, "/api/v1/files/trash", origin, `{"paths":["/batch-one.txt","/batch-two.txt"]}`, cookies, driveMutationHeaders(env.csrf.Value, "batch-route-trash-0001"))
	if trash.Code != http.StatusAccepted {
		t.Fatalf("trash = %d %s", trash.Code, trash.Body.String())
	}
	var trashed drive.BatchResult
	decodeResponse(t, trash, &trashed)
	if len(trashed.Items) != 2 {
		t.Fatalf("trash result = %+v", trashed)
	}
	restoreBody, _ := json.Marshal(map[string]any{"trashIDs": []string{trashed.Items[0].TrashID, trashed.Items[1].TrashID}, "conflict": "fail"})
	restored := performRequest(t, env.handler, http.MethodPost, "/api/v1/trash/restore", origin, string(restoreBody), cookies, driveMutationHeaders(env.csrf.Value, "batch-route-restore-0001"))
	if restored.Code != http.StatusAccepted || !bytes.Contains(restored.Body.Bytes(), []byte(`"state":"succeeded"`)) {
		t.Fatalf("restore = %d %s", restored.Code, restored.Body.String())
	}

	trash = performRequest(t, env.handler, http.MethodPost, "/api/v1/files/trash", origin, `{"paths":["/batch-one.txt","/batch-two.txt"]}`, cookies, driveMutationHeaders(env.csrf.Value, "batch-route-trash-0002"))
	if trash.Code != http.StatusAccepted {
		t.Fatalf("retrash = %d %s", trash.Code, trash.Body.String())
	}
	decodeResponse(t, trash, &trashed)
	deleteBody, _ := json.Marshal(map[string]any{"trashIDs": []string{trashed.Items[0].TrashID, trashed.Items[1].TrashID}})
	deleted := performRequest(t, env.handler, http.MethodPost, "/api/v1/trash/delete", origin, string(deleteBody), cookies, driveMutationHeaders(env.csrf.Value, "batch-route-delete-0001"))
	if deleted.Code != http.StatusAccepted || !bytes.Contains(deleted.Body.Bytes(), []byte(`"state":"succeeded"`)) {
		t.Fatalf("delete = %d %s", deleted.Code, deleted.Body.String())
	}
	listing := performRequest(t, env.handler, http.MethodGet, "/api/v1/trash", "", "", []*http.Cookie{env.session}, nil)
	if listing.Code != http.StatusOK || !bytes.Contains(listing.Body.Bytes(), []byte(`"items":[]`)) {
		t.Fatalf("trash after delete = %d %s", listing.Code, listing.Body.String())
	}

	for _, target := range []string{"/api/v1/trash/restore", "/api/v1/trash/delete"} {
		invalid := performRequest(t, env.handler, http.MethodPost, target, origin, `{"trashIDs":[]}`, cookies, driveMutationHeaders(env.csrf.Value, "batch-route-invalid-0001"))
		if invalid.Code != http.StatusBadRequest {
			t.Fatalf("invalid %s = %d %s", target, invalid.Code, invalid.Body.String())
		}
	}
}

func TestIntegrationBatchUploadItemIdempotencySurvivesEnvelopeRetry(t *testing.T) {
	env := newDriveHTTPEnvironment(t)
	const origin = "https://drive.example.test"
	cookies := []*http.Cookie{env.session, env.csrf}
	body := `{"uploads":[{"path":"/stable.txt","size":3,"mediaType":"text/plain","idempotencyKey":"stable-browser-transfer-item"}]}`

	first := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/batch", origin, body, cookies, driveMutationHeaders(env.csrf.Value, "batch-envelope-attempt-0001"))
	if first.Code != http.StatusCreated {
		t.Fatalf("first batch = %d %s", first.Code, first.Body.String())
	}
	second := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/batch", origin, body, cookies, driveMutationHeaders(env.csrf.Value, "batch-envelope-attempt-0002"))
	if second.Code != http.StatusCreated {
		t.Fatalf("retried batch = %d %s", second.Code, second.Body.String())
	}
	var firstResult, secondResult struct {
		Uploads []struct {
			Capability *domain.UploadCapability `json:"capability"`
		} `json:"uploads"`
	}
	decodeResponse(t, first, &firstResult)
	decodeResponse(t, second, &secondResult)
	if len(firstResult.Uploads) != 1 || len(secondResult.Uploads) != 1 || firstResult.Uploads[0].Capability == nil || secondResult.Uploads[0].Capability == nil {
		t.Fatalf("batch results = %+v / %+v", firstResult, secondResult)
	}
	if firstResult.Uploads[0].Capability.UploadID != secondResult.Uploads[0].Capability.UploadID {
		t.Fatalf("batch retry allocated a second upload: %q / %q", firstResult.Uploads[0].Capability.UploadID, secondResult.Uploads[0].Capability.UploadID)
	}
}

func TestIntegrationUploadBatchCompletionAndCancellationAreStrictAtomicMutations(t *testing.T) {
	env := newDriveHTTPEnvironment(t)
	const origin = "https://drive.example.test"
	cookies := []*http.Cookie{env.session, env.csrf}
	createBody := `{"uploads":[{"path":"/batch-complete-a.bin","size":3,"mediaType":"application/octet-stream","idempotencyKey":"batch-complete-item-a"},{"path":"/batch-complete-b.bin","size":4,"mediaType":"application/octet-stream","idempotencyKey":"batch-complete-item-b"}]}`
	created := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/batch", origin, createBody, cookies, driveMutationHeaders(env.csrf.Value, "batch-complete-admission"))
	if created.Code != http.StatusCreated {
		t.Fatalf("batch admission = %d %s", created.Code, created.Body.String())
	}
	var admitted struct {
		Uploads []struct {
			Capability *domain.UploadCapability `json:"capability"`
		} `json:"uploads"`
	}
	decodeResponse(t, created, &admitted)
	if len(admitted.Uploads) != 2 || admitted.Uploads[0].Capability == nil || admitted.Uploads[1].Capability == nil {
		t.Fatalf("batch admission result = %+v", admitted)
	}
	bodies := [][]byte{[]byte("one"), []byte("four")}
	completionItems := make([]map[string]string, len(bodies))
	for index, body := range bodies {
		capability := admitted.Uploads[index].Capability
		request, err := http.NewRequest(capability.Method, capability.URL, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for name, value := range capability.Headers {
			request.Header.Set(name, value)
		}
		response, err := env.data.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("batch data upload %d = %d", index, response.StatusCode)
		}
		completionItems[index] = map[string]string{"uploadID": string(capability.UploadID), "crc32c": objectstore.FingerprintFor(body).CRC32C}
	}
	completionBody, _ := json.Marshal(map[string]any{"uploads": completionItems})
	completionHeaders := driveMutationHeaders(env.csrf.Value, "batch-completion-transaction")
	completed := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/batch/complete", origin, string(completionBody), cookies, completionHeaders)
	if completed.Code != http.StatusOK || !bytes.Contains(completed.Body.Bytes(), []byte(`"entries"`)) {
		t.Fatalf("batch completion = %d %s", completed.Code, completed.Body.String())
	}
	replayed := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/batch/complete", origin, string(completionBody), cookies, completionHeaders)
	if replayed.Code != http.StatusOK || replayed.Body.String() != completed.Body.String() {
		t.Fatalf("batch completion replay = %d %s; first=%s", replayed.Code, replayed.Body.String(), completed.Body.String())
	}
	changedItems := append([]map[string]string(nil), completionItems...)
	changedItems[0] = map[string]string{"uploadID": completionItems[0]["uploadID"], "crc32c": objectstore.FingerprintFor([]byte("bad")).CRC32C}
	changedBody, _ := json.Marshal(map[string]any{"uploads": changedItems})
	changed := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/batch/complete", origin, string(changedBody), cookies, completionHeaders)
	if changed.Code != http.StatusConflict {
		t.Fatalf("changed completion replay = %d %s", changed.Code, changed.Body.String())
	}
	unknown := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/batch/complete", origin, `{"uploads":[],"providerKey":"forbidden"}`, cookies, driveMutationHeaders(env.csrf.Value, "batch-completion-unknown"))
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("unknown completion field = %d %s", unknown.Code, unknown.Body.String())
	}

	abortCreate := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/batch", origin, `{"uploads":[{"path":"/batch-abort.bin","size":1,"mediaType":"application/octet-stream","idempotencyKey":"batch-abort-item-a"}]}`, cookies, driveMutationHeaders(env.csrf.Value, "batch-abort-admission"))
	if abortCreate.Code != http.StatusCreated {
		t.Fatalf("abort admission = %d %s", abortCreate.Code, abortCreate.Body.String())
	}
	decodeResponse(t, abortCreate, &admitted)
	abortBody, _ := json.Marshal(map[string]any{"uploadIDs": []domain.UploadID{admitted.Uploads[0].Capability.UploadID}, "batchID": admitted.Uploads[0].Capability.BatchID})
	abortHeaders := driveMutationHeaders(env.csrf.Value, "batch-abort-transaction")
	aborted := performRequest(t, env.handler, http.MethodDelete, "/api/v1/uploads/batch", origin, string(abortBody), cookies, abortHeaders)
	if aborted.Code != http.StatusNoContent {
		t.Fatalf("batch abort = %d %s", aborted.Code, aborted.Body.String())
	}
	replayedAbort := performRequest(t, env.handler, http.MethodDelete, "/api/v1/uploads/batch", origin, string(abortBody), cookies, abortHeaders)
	if replayedAbort.Code != http.StatusNoContent {
		t.Fatalf("batch abort replay = %d %s", replayedAbort.Code, replayedAbort.Body.String())
	}
	invalidBatch := performRequest(t, env.handler, http.MethodDelete, "/api/v1/uploads/batch", origin, `{"uploadIDs":["upload"],"batchID":"not-a-digest"}`, cookies, driveMutationHeaders(env.csrf.Value, "batch-abort-invalid-binding"))
	if invalidBatch.Code != http.StatusBadRequest {
		t.Fatalf("invalid batch abort binding = %d %s", invalidBatch.Code, invalidBatch.Body.String())
	}
	crossOwner := performRequest(t, env.handler, http.MethodDelete, "/api/v1/uploads/batch", origin, string(abortBody), []*http.Cookie{env.otherSession, env.otherCSRF}, driveMutationHeaders(env.otherCSRF.Value, "batch-abort-cross-owner"))
	if crossOwner.Code != http.StatusNotFound {
		t.Fatalf("cross-owner batch abort = %d %s", crossOwner.Code, crossOwner.Body.String())
	}
}

func TestIntegrationBatchCopyMoveAndUploadLifecycleRoutes(t *testing.T) {
	env := newDriveHTTPEnvironment(t)
	const origin = "https://drive.example.test"
	cookies := []*http.Cookie{env.session, env.csrf}
	createHTTPFile(t, env, env.session, env.csrf, "/a.txt", "a", "batch-copy-upload-a-01")
	createHTTPFile(t, env, env.session, env.csrf, "/b.txt", "b", "batch-copy-upload-b-01")

	mixed := performRequest(t, env.handler, http.MethodPost, "/api/v1/files/copy", origin, `{"source":"/a.txt","items":[{"source":"/a.txt","destination":"/copy-a.txt"}]}`, cookies, driveMutationHeaders(env.csrf.Value, "batch-copy-mixed-0001"))
	if mixed.Code != http.StatusBadRequest {
		t.Fatalf("mixed batch fields = %d %s", mixed.Code, mixed.Body.String())
	}
	invalidSource := performRequest(t, env.handler, http.MethodPost, "/api/v1/files/copy", origin, `{"items":[{"source":"relative","destination":"/copy-a.txt"}]}`, cookies, driveMutationHeaders(env.csrf.Value, "batch-copy-invalid-001"))
	if invalidSource.Code != http.StatusBadRequest {
		t.Fatalf("invalid batch source = %d %s", invalidSource.Code, invalidSource.Body.String())
	}
	invalidDestination := performRequest(t, env.handler, http.MethodPost, "/api/v1/files/copy", origin, `{"items":[{"source":"/a.txt","destination":"relative"}]}`, cookies, driveMutationHeaders(env.csrf.Value, "batch-copy-invalid-002"))
	if invalidDestination.Code != http.StatusBadRequest {
		t.Fatalf("invalid batch destination = %d %s", invalidDestination.Code, invalidDestination.Body.String())
	}
	copyResponse := performRequest(t, env.handler, http.MethodPost, "/api/v1/files/copy", origin, `{"items":[{"source":"/a.txt","destination":"/copy-a.txt"},{"source":"/b.txt","destination":"/copy-b.txt"}]}`, cookies, driveMutationHeaders(env.csrf.Value, "batch-copy-success-001"))
	if copyResponse.Code != http.StatusAccepted || !bytes.Contains(copyResponse.Body.Bytes(), []byte(`"items"`)) {
		t.Fatalf("batch copy = %d %s", copyResponse.Code, copyResponse.Body.String())
	}
	moveResponse := performRequest(t, env.handler, http.MethodPost, "/api/v1/files/move", origin, `{"items":[{"source":"/copy-a.txt","destination":"/moved-a.txt"},{"source":"/copy-b.txt","destination":"/moved-b.txt"}]}`, cookies, driveMutationHeaders(env.csrf.Value, "batch-move-success-001"))
	if moveResponse.Code != http.StatusAccepted {
		t.Fatalf("batch move = %d %s", moveResponse.Code, moveResponse.Body.String())
	}

	created := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads", origin, `{"path":"/abort.txt","size":1,"mediaType":"text/plain","resumable":true}`, cookies, driveMutationHeaders(env.csrf.Value, "upload-abort-route-0001"))
	if created.Code != http.StatusCreated {
		t.Fatalf("upload create = %d %s", created.Code, created.Body.String())
	}
	var capability domain.UploadCapability
	decodeResponse(t, created, &capability)
	status := performRequest(t, env.handler, http.MethodGet, "/api/v1/uploads/"+string(capability.UploadID), "", "", []*http.Cookie{env.session}, nil)
	if status.Code != http.StatusOK {
		t.Fatalf("upload status = %d %s", status.Code, status.Body.String())
	}
	var activeStatus domain.UploadStatus
	decodeResponse(t, status, &activeStatus)
	if activeStatus.State != domain.UploadStateActive {
		t.Fatalf("active upload status = %+v", activeStatus)
	}
	aborted := performRequest(t, env.handler, http.MethodDelete, "/api/v1/uploads/"+string(capability.UploadID), origin, `{}`, cookies, driveMutationHeaders(env.csrf.Value, ""))
	if aborted.Code != http.StatusNoContent {
		t.Fatalf("upload abort = %d %s", aborted.Code, aborted.Body.String())
	}
	abortedStatusResponse := performRequest(t, env.handler, http.MethodGet, "/api/v1/uploads/"+string(capability.UploadID), "", "", []*http.Cookie{env.session}, nil)
	if abortedStatusResponse.Code != http.StatusOK {
		t.Fatalf("aborted upload status = %d %s", abortedStatusResponse.Code, abortedStatusResponse.Body.String())
	}
	var abortedStatus domain.UploadStatus
	decodeResponse(t, abortedStatusResponse, &abortedStatus)
	if abortedStatus.State != domain.UploadStateAborted {
		t.Fatalf("aborted upload status = %+v", abortedStatus)
	}

	for _, target := range []string{"/api/v1/files?limit=invalid", "/api/v1/files?order=sideways", "/api/v1/trash?limit=10001", "/api/v1/public/shares/missing?limit=0"} {
		response := performRequest(t, env.handler, http.MethodGet, target, "", "", []*http.Cookie{env.session}, nil)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid query %q = %d %s", target, response.Code, response.Body.String())
		}
	}
}

func TestReservedNamespaceAndEncodingCorpusFailsBeforeProviderAccess(t *testing.T) {
	env := newDriveHTTPEnvironment(t)
	const origin = "https://drive.example.test"
	cookies := []*http.Cookie{env.session, env.csrf}
	segment256 := "/" + string(bytes.Repeat([]byte{'a'}, 256))
	path4097 := "/" + string(bytes.Repeat([]byte{'a'}, 4096))
	jsonPaths := []string{
		"", "relative", "//escape", "/escape/", "/./escape", "/../escape", "/safe/../../escape",
		`/safe\escape`, "/safe\x00escape", "/safe\x1fescape", "/safe\x7fescape",
		"/.endlessfs", "/.ENDLESSFS/records", "/.trash", "/.TrAsH/item", segment256, path4097,
	}
	before := env.storage.Instrumentation()
	for index, value := range jsonPaths {
		t.Run("json-"+strconv.Itoa(index), func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"path": value})
			response := performRequest(t, env.handler, http.MethodPost, "/api/v1/directories", origin, string(body), cookies, driveMutationHeaders(env.csrf.Value, ""))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("path %q status = %d body=%s", value, response.Code, response.Body.String())
			}
		})
	}
	queries := []string{
		"%2F..%2Fescape",
		"%252F..%252Fescape",
		"%2Fsafe%2F%2E%2E%2Fescape",
		"%2Fsafe%5Cescape",
		"%2F%2Eendlessfs%2Frecords",
		"%2F%2Etrash%2Fitem",
		"%2Fsafe%00escape",
	}
	for index, query := range queries {
		t.Run("query-"+strconv.Itoa(index), func(t *testing.T) {
			response := performRequest(t, env.handler, http.MethodGet, "/api/v1/files?path="+query, "", "", []*http.Cookie{env.session}, nil)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("query %q status = %d body=%s", query, response.Code, response.Body.String())
			}
		})
	}
	after := env.storage.Instrumentation()
	if after.ProviderCalls[providermemory.OperationCreateDirectory] != before.ProviderCalls[providermemory.OperationCreateDirectory] || after.ProviderCalls[providermemory.OperationList] != before.ProviderCalls[providermemory.OperationList] {
		t.Fatalf("traversal corpus reached provider: before=%+v after=%+v", before, after)
	}
}

func TestIntegrationCrossUserPrivateEndpointMatrix(t *testing.T) {
	env := newDriveHTTPEnvironment(t)
	const origin = "https://drive.example.test"
	ownerCookies := []*http.Cookie{env.otherSession, env.otherCSRF}
	attackerCookies := []*http.Cookie{env.session, env.csrf}

	for _, path := range []string{"/foreign", "/page-two"} {
		response := performRequest(t, env.handler, http.MethodPost, "/api/v1/directories", origin, `{"path":"`+path+`"}`, ownerCookies, driveMutationHeaders(env.otherCSRF.Value, ""))
		if response.Code != http.StatusCreated {
			t.Fatalf("create owner directory %s = %d %s", path, response.Code, response.Body.String())
		}
	}
	ownerEntry, ownerUpload := createHTTPFile(t, env, env.otherSession, env.otherCSRF, "/foreign/owned.txt", "owned", "cross-owner-upload-0001")
	attackerEntry, _ := createHTTPFile(t, env, env.session, env.csrf, "/same.txt", "attacker", "cross-attacker-upload-01")
	ownerSame, _ := createHTTPFile(t, env, env.otherSession, env.otherCSRF, "/same.txt", "owner", "cross-owner-upload-0002")
	if attackerEntry.Version == ownerSame.Version {
		t.Fatal("test setup requires distinct opaque versions")
	}

	ownerPage := performRequest(t, env.handler, http.MethodGet, "/api/v1/files?path=/&limit=1", "", "", []*http.Cookie{env.otherSession}, nil)
	var page domain.ListPage
	decodeResponse(t, ownerPage, &page)
	if page.NextCursor == "" {
		t.Fatal("test setup did not create an owner-scoped cursor")
	}

	copyResponse := performRequest(t, env.handler, http.MethodPost, "/api/v1/files/copy", origin, `{"source":"/foreign/owned.txt","destination":"/foreign/copied.txt"}`, ownerCookies, driveMutationHeaders(env.otherCSRF.Value, "cross-owner-copy-000001"))
	var ownerOperation domain.Operation
	decodeResponse(t, copyResponse, &ownerOperation)
	if copyResponse.Code != http.StatusAccepted || ownerOperation.ID == "" {
		t.Fatalf("owner copy = %d %s", copyResponse.Code, copyResponse.Body.String())
	}

	shareResponse := performRequest(t, env.handler, http.MethodPost, "/api/v1/shares", origin, `{"path":"/foreign"}`, ownerCookies, driveMutationHeaders(env.otherCSRF.Value, "cross-owner-share-00001"))
	var ownerShare struct {
		Share model.Share `json:"share"`
	}
	decodeResponse(t, shareResponse, &ownerShare)
	if shareResponse.Code != http.StatusCreated || ownerShare.Share.ShareID == "" {
		t.Fatalf("owner share = %d %s", shareResponse.Code, shareResponse.Body.String())
	}

	trashResponse := performRequest(t, env.handler, http.MethodPost, "/api/v1/files/trash", origin, `{"paths":["/foreign/copied.txt"]}`, ownerCookies, driveMutationHeaders(env.otherCSRF.Value, "cross-owner-trash-00001"))
	var ownerTrash drive.BatchResult
	decodeResponse(t, trashResponse, &ownerTrash)
	if trashResponse.Code != http.StatusAccepted || len(ownerTrash.Items) != 1 || ownerTrash.Items[0].TrashID == "" {
		t.Fatalf("owner trash = %d %s", trashResponse.Code, trashResponse.Body.String())
	}
	ownerTrashListing := performRequest(t, env.handler, http.MethodGet, "/api/v1/trash?limit=1000", "", "", []*http.Cookie{env.otherSession}, nil)
	if ownerTrashListing.Code != http.StatusOK || !bytes.Contains(ownerTrashListing.Body.Bytes(), []byte(ownerTrash.Items[0].TrashID)) || !bytes.Contains(ownerTrashListing.Body.Bytes(), []byte(`"size":5`)) {
		t.Fatalf("owner trash listing = %d %s", ownerTrashListing.Code, ownerTrashListing.Body.String())
	}

	checks := []struct {
		name, method, target, body string
		want                       int
		key                        string
	}{
		{name: "list path", method: http.MethodGet, target: "/api/v1/files?path=/foreign", want: http.StatusNotFound},
		{name: "stat", method: http.MethodGet, target: "/api/v1/files/stat?path=/foreign/owned.txt", want: http.StatusNotFound},
		{name: "cursor", method: http.MethodGet, target: "/api/v1/files?path=/&limit=1&cursor=" + page.NextCursor, want: http.StatusBadRequest},
		{name: "upload destination", method: http.MethodPost, target: "/api/v1/uploads", body: `{"path":"/foreign/new.txt","size":1,"mediaType":"text/plain"}`, want: http.StatusNotFound, key: "cross-attack-upload-001"},
		{name: "upload status", method: http.MethodGet, target: "/api/v1/uploads/" + string(ownerUpload.UploadID), want: http.StatusNotFound},
		{name: "upload complete", method: http.MethodPost, target: "/api/v1/uploads/" + string(ownerUpload.UploadID) + "/complete", body: `{"path":"/foreign/owned.txt","size":5,"mediaType":"text/plain"}`, want: http.StatusNotFound},
		{name: "upload abort", method: http.MethodDelete, target: "/api/v1/uploads/" + string(ownerUpload.UploadID), body: `{}`, want: http.StatusNotFound},
		{name: "download", method: http.MethodPost, target: "/api/v1/downloads", body: `{"path":"/foreign/owned.txt","version":"` + string(ownerEntry.Version) + `"}`, want: http.StatusNotFound},
		{name: "preview", method: http.MethodPost, target: "/api/v1/downloads", body: `{"path":"/foreign/owned.txt","version":"` + string(ownerEntry.Version) + `","preview":true}`, want: http.StatusNotFound},
		{name: "foreign version", method: http.MethodPost, target: "/api/v1/downloads", body: `{"path":"/same.txt","version":"` + string(ownerSame.Version) + `"}`, want: http.StatusPreconditionFailed},
		{name: "copy", method: http.MethodPost, target: "/api/v1/files/copy", body: `{"source":"/foreign/owned.txt","destination":"/stolen.txt","expectedSource":"` + string(ownerEntry.Version) + `"}`, want: http.StatusNotFound, key: "cross-attack-copy-00001"},
		{name: "move", method: http.MethodPost, target: "/api/v1/files/move", body: `{"source":"/foreign/owned.txt","destination":"/stolen.txt","expectedSource":"` + string(ownerEntry.Version) + `"}`, want: http.StatusNotFound, key: "cross-attack-move-00001"},
		{name: "operation", method: http.MethodGet, target: "/api/v1/operations/" + string(ownerOperation.ID), want: http.StatusNotFound},
		{name: "restore", method: http.MethodPost, target: "/api/v1/trash/" + ownerTrash.Items[0].TrashID + "/restore", body: `{}`, want: http.StatusNotFound, key: "cross-attack-restore-001"},
		{name: "batch restore", method: http.MethodPost, target: "/api/v1/trash/restore", body: `{"trashIDs":["` + ownerTrash.Items[0].TrashID + `"],"conflict":"fail"}`, want: http.StatusNotFound, key: "cross-attack-batch-restore-1"},
		{name: "permanent delete", method: http.MethodDelete, target: "/api/v1/trash/" + ownerTrash.Items[0].TrashID, body: `{}`, want: http.StatusNotFound, key: "cross-attack-delete-0001"},
		{name: "batch permanent delete", method: http.MethodPost, target: "/api/v1/trash/delete", body: `{"trashIDs":["` + ownerTrash.Items[0].TrashID + `"]}`, want: http.StatusNotFound, key: "cross-attack-batch-delete-01"},
		{name: "create share", method: http.MethodPost, target: "/api/v1/shares", body: `{"path":"/foreign/owned.txt"}`, want: http.StatusNotFound, key: "cross-attack-share-00001"},
		{name: "revoke share", method: http.MethodDelete, target: "/api/v1/shares/" + ownerShare.Share.ShareID, body: `{}`, want: http.StatusNotFound},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			headers := map[string]string(nil)
			cookies := []*http.Cookie{env.session}
			if check.method != http.MethodGet {
				headers = driveMutationHeaders(env.csrf.Value, check.key)
				cookies = attackerCookies
			}
			response := performRequest(t, env.handler, check.method, check.target, origin, check.body, cookies, headers)
			if response.Code != check.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, check.want, response.Body.String())
			}
			assertProblem(t, response)
		})
	}

	attackerTrash := performRequest(t, env.handler, http.MethodPost, "/api/v1/files/trash", origin, `{"paths":["/foreign/owned.txt"]}`, attackerCookies, driveMutationHeaders(env.csrf.Value, "cross-attack-trash-00001"))
	if attackerTrash.Code != http.StatusNotFound {
		t.Fatalf("cross-user trash result = %d %s", attackerTrash.Code, attackerTrash.Body.String())
	}
	assertProblem(t, attackerTrash)
	shares := performRequest(t, env.handler, http.MethodGet, "/api/v1/shares", "", "", []*http.Cookie{env.session}, nil)
	if shares.Code != http.StatusOK || bytes.Contains(shares.Body.Bytes(), []byte(ownerShare.Share.ShareID)) {
		t.Fatalf("attacker share listing leaked owner record: %d %s", shares.Code, shares.Body.String())
	}
	attackerTrashListing := performRequest(t, env.handler, http.MethodGet, "/api/v1/trash?limit=1000", "", "", []*http.Cookie{env.session}, nil)
	if attackerTrashListing.Code != http.StatusOK || bytes.Contains(attackerTrashListing.Body.Bytes(), []byte(ownerTrash.Items[0].TrashID)) {
		t.Fatalf("attacker trash listing leaked owner record: %d %s", attackerTrashListing.Code, attackerTrashListing.Body.String())
	}
	ownerStillPresent := performRequest(t, env.handler, http.MethodGet, "/api/v1/files/stat?path=/foreign/owned.txt", "", "", []*http.Cookie{env.otherSession}, nil)
	if ownerStillPresent.Code != http.StatusOK {
		t.Fatalf("cross-user matrix mutated owner file: %d %s", ownerStillPresent.Code, ownerStillPresent.Body.String())
	}
}

func createHTTPFile(t *testing.T, env driveHTTPEnvironment, session, csrf *http.Cookie, path, content, key string) (domain.Entry, domain.UploadCapability) {
	t.Helper()
	origin := "https://drive.example.test"
	cookies := []*http.Cookie{session, csrf}
	body, _ := json.Marshal(map[string]any{"path": path, "size": len(content), "mediaType": "text/plain", "resumable": true})
	created := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads", origin, string(body), cookies, driveMutationHeaders(csrf.Value, key))
	if created.Code != http.StatusCreated {
		t.Fatalf("create upload for %s = %d %s", path, created.Code, created.Body.String())
	}
	var capability domain.UploadCapability
	decodeResponse(t, created, &capability)
	request, _ := http.NewRequest(capability.Method, capability.URL, bytes.NewBufferString(content))
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	request.Header.Set("Origin", origin)
	response, err := env.data.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("upload file %s = %d", path, response.StatusCode)
	}
	completedBody, _ := json.Marshal(map[string]any{"path": path, "size": len(content), "mediaType": "text/plain"})
	completed := performRequest(t, env.handler, http.MethodPost, "/api/v1/uploads/"+string(capability.UploadID)+"/complete", origin, string(completedBody), cookies, driveMutationHeaders(csrf.Value, ""))
	if completed.Code != http.StatusOK {
		t.Fatalf("complete file %s = %d %s", path, completed.Code, completed.Body.String())
	}
	var entry domain.Entry
	decodeResponse(t, completed, &entry)
	return entry, capability
}
