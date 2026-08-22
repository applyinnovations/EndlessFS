package devfixture

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/drive"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/secret"
)

func TestSeedBuildsRepresentativeDeterministicWorkspace(t *testing.T) {
	t.Parallel()

	repository := &recordingRepository{}
	files := &recordingFiles{}
	data := &recordingDataPlane{files: files}
	clock := domain.NewFixedClock(time.Date(2026, 8, 19, 3, 0, 0, 0, time.UTC))

	result, err := Seed(context.Background(), repository, files, data, clock)
	if err != nil {
		t.Fatal(err)
	}
	if result.UserID != FixtureUserID() || result.CredentialID == "" {
		t.Fatalf("fixture identity = %+v", result)
	}
	if len(repository.profiles) < 150 || len(repository.accounts) < 150 {
		t.Fatalf("seeded identities = %d profiles, %d accounts", len(repository.profiles), len(repository.accounts))
	}
	if len(repository.roles.UserIDs) != 1 || repository.roles.UserIDs[0] != result.UserID {
		t.Fatalf("admin roles = %+v", repository.roles)
	}
	if len(repository.credentials) < 60 || len(repository.indexes) != 1 || len(repository.indexes[0].CredentialIDs) != len(repository.credentials) {
		t.Fatalf("passkeys = %d credentials, %d indexes", len(repository.credentials), len(repository.indexes))
	}
	if len(repository.invites) < 60 {
		t.Fatalf("invites = %d, want a dense administration collection", len(repository.invites))
	}
	if len(files.directories) < 6 {
		t.Fatalf("directories = %d, want representative hierarchy", len(files.directories))
	}
	if len(files.uploads) < 1000 || len(data.bodies) != len(files.uploads) || len(files.completed) != len(files.uploads) {
		t.Fatalf("files = %d uploads, %d transferred, %d completed", len(files.uploads), len(data.bodies), len(files.completed))
	}
	for _, prefix := range []string{
		"/Workspace item ", "/Brand/Brand asset ", "/Photography/Contact sheet ",
		"/Projects/Project file ", "/Projects/Archive/Archived file ", "/Scale Lab/Scale sample ",
		"/Scale Lab/Reference/Asset ",
	} {
		count := 0
		for _, upload := range files.uploads {
			if strings.HasPrefix(upload.Path.String(), prefix) {
				count++
			}
		}
		if count < 100 {
			t.Errorf("fixture collection %q has %d files, want automatic-paging scale", prefix, count)
		}
	}
	if len(files.trashed) < 120 {
		t.Fatalf("trashed files = %d, want a dense recovery collection", len(files.trashed))
	}
	if len(files.shared) < 60 {
		t.Fatalf("shares = %d, want a dense shares collection", len(files.shared))
	}
	if _, ok := data.bodies["/Photography/Coastline.png"]; !ok {
		t.Fatal("fixture is missing image preview ground truth")
	}
	if _, ok := data.bodies["/Projects/Launch plan.pdf"]; !ok {
		t.Fatal("fixture is missing PDF preview ground truth")
	}
	for left, right := range map[string]string{
		"/Projects/Project Atlas/Brief.txt":          "/Backups/Projects/Project Atlas/Brief.txt",
		"/Projects/Project Atlas/Assets/Palette.csv": "/Backups/Projects/Project Atlas/Assets/Palette.csv",
		"/Photography/Selects/Coastline notes.txt":   "/Backups/Photography/Selects/Coastline notes.txt",
		"/Brand/Published/Release notes.txt":         "/Backups/Brand/Published/Release notes.txt",
	} {
		if string(data.bodies[left]) == "" || string(data.bodies[left]) != string(data.bodies[right]) {
			t.Errorf("duplicate fixture bodies %q and %q do not match", left, right)
		}
	}
	if _, ok := data.bodies["/Photography/Selects/Only in working set.txt"]; !ok {
		t.Error("partial duplicate fixture is missing its left-only file")
	}
	if _, ok := data.bodies["/Backups/Photography/Selects/Only in backup.txt"]; !ok {
		t.Error("partial duplicate fixture is missing its right-only file")
	}
	if len(files.ignoredDirectoryPairs) != 1 || !files.ignoredDirectoryPairs[0].Ignored {
		t.Fatalf("ignored duplicate directory pairs = %+v, want one intentional mirror", files.ignoredDirectoryPairs)
	}
	if len(files.ignoredGroups) != 1 || !files.ignoredGroups[0].Ignored || files.ignoredGroups[0].GroupID != "fixture-brand-published" {
		t.Fatalf("ignored exact duplicate groups = %+v, want intentional published mirror", files.ignoredGroups)
	}
}

func TestLoginHandlerIssuesLocalCookiesWithoutExposingTokens(t *testing.T) {
	t.Parallel()

	issuer := &recordingIssuer{}
	issued := auth.IssuedSession{
		Token:     secret.Value("session-token"),
		CSRFToken: secret.Value("csrf-token"),
		Record:    model.Session{ExpiresAt: time.Now().Add(time.Hour)},
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	handler := LoginHandler(next, issuer, issued)

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+LoginPath, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/" {
		t.Fatalf("login response = %d, location %q", response.Code, response.Header().Get("Location"))
	}
	if response.Header().Get("Cache-Control") != "no-store" || len(response.Result().Cookies()) != 2 {
		t.Fatalf("login headers = %+v", response.Header())
	}
	if strings.Contains(response.Header().Get("Location"), "session-token") || strings.Contains(response.Body.String(), "session-token") {
		t.Fatal("session token leaked into the redirect response")
	}

	post := httptest.NewRecorder()
	handler.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+LoginPath, nil))
	if post.Code != http.StatusMethodNotAllowed || post.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST response = %d, Allow %q", post.Code, post.Header().Get("Allow"))
	}

	passthrough := httptest.NewRecorder()
	handler.ServeHTTP(passthrough, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/asset", nil))
	if passthrough.Code != http.StatusTeapot {
		t.Fatalf("passthrough response = %d", passthrough.Code)
	}
}

type recordingRepository struct {
	profiles    []model.Profile
	accounts    []model.Account
	credentials []model.Credential
	indexes     []model.CredentialIndex
	invites     []model.Invite
	roles       model.AdminRoles
}

func (r *recordingRepository) CreateProfile(_ context.Context, value model.Profile) error {
	r.profiles = append(r.profiles, value)
	return value.Validate()
}
func (r *recordingRepository) CreateAccount(_ context.Context, value model.Account) error {
	r.accounts = append(r.accounts, value)
	return value.Validate()
}
func (r *recordingRepository) CreateCredential(_ context.Context, value model.Credential) error {
	r.credentials = append(r.credentials, value)
	return value.Validate()
}
func (r *recordingRepository) CreateCredentialIndex(_ context.Context, value model.CredentialIndex) error {
	r.indexes = append(r.indexes, value)
	return value.Validate()
}
func (r *recordingRepository) CreateInvite(_ context.Context, value model.Invite) error {
	r.invites = append(r.invites, value)
	return value.Validate()
}
func (r *recordingRepository) CreateAdminRoles(_ context.Context, value model.AdminRoles) error {
	r.roles = value
	return value.Validate()
}

type recordingFiles struct {
	directories           []domain.UserPath
	uploads               []domain.CreateUploadRequest
	completed             []domain.CompleteUploadRequest
	trashed               []domain.UserPath
	shared                []domain.UserPath
	ignoredDirectoryPairs []domain.SetDuplicateDirectoryIgnoredRequest
	ignoredGroups         []domain.SetDuplicateIgnoredRequest
}

func (f *recordingFiles) SetDuplicateDirectoryIgnored(_ context.Context, _ domain.UserID, request domain.SetDuplicateDirectoryIgnoredRequest) (domain.DuplicateDirectoryIgnore, error) {
	f.ignoredDirectoryPairs = append(f.ignoredDirectoryPairs, request)
	return domain.DuplicateDirectoryIgnore{Ignored: request.Ignored, Revision: 1}, nil
}

func (*recordingFiles) DuplicateGroups(_ context.Context, _ domain.UserID, _ domain.DuplicateGroupRequest) (domain.DuplicateGroupPage, error) {
	return domain.DuplicateGroupPage{Groups: []domain.DuplicateGroup{{ID: "fixture-brand-published", Kind: domain.DuplicateDirectory, OccurrenceCount: 2}}}, nil
}

func (*recordingFiles) DuplicateOccurrences(_ context.Context, _ domain.UserID, request domain.DuplicateOccurrenceRequest) (domain.DuplicateOccurrencePage, error) {
	if request.GroupID != "fixture-brand-published" {
		return domain.DuplicateOccurrencePage{}, nil
	}
	return domain.DuplicateOccurrencePage{Occurrences: []domain.DuplicateOccurrence{
		{GroupID: request.GroupID, Kind: domain.DuplicateDirectory, Area: domain.AreaLive, AreaName: "live", Path: domain.MustParseUserPath("/Brand/Published")},
		{GroupID: request.GroupID, Kind: domain.DuplicateDirectory, Area: domain.AreaLive, AreaName: "live", Path: domain.MustParseUserPath("/Backups/Brand/Published")},
	}}, nil
}

func (f *recordingFiles) SetDuplicateIgnored(_ context.Context, _ domain.UserID, request domain.SetDuplicateIgnoredRequest) (domain.DuplicateIgnore, error) {
	f.ignoredGroups = append(f.ignoredGroups, request)
	return domain.DuplicateIgnore{GroupID: request.GroupID, Ignored: request.Ignored, Revision: 1}, nil
}

func (f *recordingFiles) CreateDirectory(_ context.Context, _ domain.UserID, request domain.CreateDirectoryRequest) (domain.Entry, error) {
	f.directories = append(f.directories, request.Path)
	return domain.Entry{Path: request.Path, Kind: domain.EntryDirectory}, nil
}
func (f *recordingFiles) CreateUpload(_ context.Context, _ domain.UserID, request domain.CreateUploadRequest) (domain.UploadCapability, error) {
	f.uploads = append(f.uploads, request)
	capabilityURL := (&url.URL{Scheme: "http", Host: "fixture.invalid", Path: request.Path.String()}).String()
	return domain.UploadCapability{
		UploadID: domain.UploadID("upload-" + strings.ReplaceAll(strings.TrimPrefix(request.Path.String(), "/"), "/", "-")),
		URL:      capabilityURL,
		Method:   http.MethodPut,
		Headers:  map[string]string{"Authorization": "Bearer fixture-capability"},
	}, nil
}
func (f *recordingFiles) CompleteUpload(_ context.Context, _ domain.UserID, request domain.CompleteUploadRequest) (domain.Entry, error) {
	f.completed = append(f.completed, request)
	return domain.Entry{Path: request.Path, Kind: domain.EntryFile, Size: request.Size, MediaType: request.MediaType}, nil
}
func (f *recordingFiles) Trash(_ context.Context, _ domain.UserID, paths []domain.UserPath, _ string) (drive.BatchResult, error) {
	f.trashed = append(f.trashed, paths...)
	return drive.BatchResult{}, nil
}
func (f *recordingFiles) CreateShare(_ context.Context, _ domain.UserID, path domain.UserPath, _ *time.Time, _ string) (drive.CreatedShare, error) {
	f.shared = append(f.shared, path)
	return drive.CreatedShare{}, nil
}

type recordingDataPlane struct {
	files  *recordingFiles
	bodies map[string][]byte
}

func (d *recordingDataPlane) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPut || request.Header.Get("Authorization") != "Bearer fixture-capability" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	body, _ := io.ReadAll(request.Body)
	if d.bodies == nil {
		d.bodies = make(map[string][]byte)
	}
	d.bodies[request.URL.Path] = body
	w.WriteHeader(http.StatusNoContent)
}

type recordingIssuer struct{}

func (*recordingIssuer) Cookie(auth.IssuedSession) *http.Cookie {
	return &http.Cookie{Name: "session", Value: "session-token", Path: "/", HttpOnly: true}
}
func (*recordingIssuer) CSRFCookie(auth.IssuedSession) *http.Cookie {
	return &http.Cookie{Name: "csrf", Value: "csrf-token", Path: "/"}
}
