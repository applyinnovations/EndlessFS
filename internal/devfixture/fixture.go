// Package devfixture builds the explicit loopback-only workspace used for
// local UI evaluation. It is not available in normal server configurations.
package devfixture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/drive"
	"github.com/applyinnovations/endlessfs/internal/model"
)

const (
	// LoginPath installs the pre-issued fixture session in a local browser.
	LoginPath = "/__endlessfs/local-fixture"

	fixtureUserIDValue = "AAAAAAAAAAAAAAAAAAAAAA"
)

// Result identifies the seeded local account without exposing session tokens.
type Result struct {
	UserID       domain.UserID
	CredentialID string
}

// FixtureUserID is stable so screenshots and local automation are repeatable.
func FixtureUserID() domain.UserID {
	userID, err := domain.ParseUserID(fixtureUserIDValue)
	if err != nil {
		panic("invalid built-in fixture user ID")
	}
	return userID
}

// FixtureCredentialID is the inert passkey record shown in local settings.
func FixtureCredentialID() string {
	return base64.RawURLEncoding.EncodeToString([]byte("local fixture passkey"))
}

type identityRepository interface {
	CreateProfile(context.Context, model.Profile) error
	CreateAccount(context.Context, model.Account) error
	CreateCredential(context.Context, model.Credential) error
	CreateCredentialIndex(context.Context, model.CredentialIndex) error
	CreateInvite(context.Context, model.Invite) error
	CreateAdminRoles(context.Context, model.AdminRoles) error
}

type fileService interface {
	CreateDirectory(context.Context, domain.UserID, domain.CreateDirectoryRequest) (domain.Entry, error)
	CreateUpload(context.Context, domain.UserID, domain.CreateUploadRequest) (domain.UploadCapability, error)
	CompleteUpload(context.Context, domain.UserID, domain.CompleteUploadRequest) (domain.Entry, error)
	Trash(context.Context, domain.UserID, []domain.UserPath, string) (drive.BatchResult, error)
	CreateShare(context.Context, domain.UserID, domain.UserPath, *time.Time, string) (drive.CreatedShare, error)
	SetDuplicateDirectoryIgnored(context.Context, domain.UserID, domain.SetDuplicateDirectoryIgnoredRequest) (domain.DuplicateDirectoryIgnore, error)
	DuplicateGroups(context.Context, domain.UserID, domain.DuplicateGroupRequest) (domain.DuplicateGroupPage, error)
	DuplicateOccurrences(context.Context, domain.UserID, domain.DuplicateOccurrenceRequest) (domain.DuplicateOccurrencePage, error)
	SetDuplicateIgnored(context.Context, domain.UserID, domain.SetDuplicateIgnoredRequest) (domain.DuplicateIgnore, error)
}

// Seed creates dense accounts and file collections, recoverable trash, shares,
// passkeys, invites, and previewable media through normal contracts.
func Seed(ctx context.Context, repository identityRepository, files fileService, dataPlane http.Handler, clock domain.Clock) (Result, error) {
	if repository == nil || files == nil || dataPlane == nil || clock == nil {
		return Result{}, domain.NewError(domain.ErrorInvalid, "invalid local fixture dependencies")
	}
	userID := FixtureUserID()
	if err := seedIdentities(ctx, repository, clock.Now(), userID); err != nil {
		return Result{}, err
	}
	if err := seedWorkspace(ctx, files, dataPlane, userID); err != nil {
		return Result{}, err
	}
	return Result{UserID: userID, CredentialID: FixtureCredentialID()}, nil
}

func seedIdentities(ctx context.Context, repository identityRepository, now time.Time, fixtureUserID domain.UserID) error {
	identities := []struct {
		id          string
		displayName string
		status      model.AccountStatus
		age         time.Duration
	}{
		{id: fixtureUserID.String(), displayName: "Local Tester", status: model.AccountEnabled, age: 90 * 24 * time.Hour},
		{id: "EREREREREREREREREREREQ", displayName: "Morgan Lee", status: model.AccountEnabled, age: 45 * 24 * time.Hour},
		{id: "IiIiIiIiIiIiIiIiIiIiIg", displayName: "Avery Quinn", status: model.AccountDisabled, age: 21 * 24 * time.Hour},
	}
	for index := 4; index <= 160; index++ {
		status := model.AccountEnabled
		if index%11 == 0 {
			status = model.AccountDisabled
		}
		identities = append(identities, struct {
			id          string
			displayName string
			status      model.AccountStatus
			age         time.Duration
		}{
			id:          deterministicOpaqueID(fmt.Sprintf("local-fixture-user-%04d", index)),
			displayName: fmt.Sprintf("Fixture member %03d", index),
			status:      status,
			age:         time.Duration(160-index) * 6 * time.Hour,
		})
	}
	for _, identity := range identities {
		userID, err := domain.ParseUserID(identity.id)
		if err != nil {
			return err
		}
		displayName, err := domain.ParseDisplayName(identity.displayName)
		if err != nil {
			return err
		}
		createdAt := now.Add(-identity.age).UTC()
		if err := repository.CreateProfile(ctx, model.Profile{UserID: userID, DisplayName: displayName}); err != nil {
			return fmt.Errorf("seed local profile: %w", err)
		}
		if err := repository.CreateAccount(ctx, model.Account{
			SchemaVersion: model.SchemaVersion, UserID: userID, Status: identity.status,
			CreatedAt: createdAt, UpdatedAt: createdAt,
		}); err != nil {
			return fmt.Errorf("seed local account: %w", err)
		}
	}
	credentialIDs := make([]string, 0, 64)
	for index := 1; index <= 64; index++ {
		label, err := domain.ParseCredentialLabel(fmt.Sprintf("Fixture device %02d", index))
		if err != nil {
			return err
		}
		credentialID := FixtureCredentialID()
		if index > 1 {
			credentialID = base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("local fixture passkey %02d", index)))
		}
		credential := model.Credential{
			SchemaVersion: model.SchemaVersion, CredentialID: credentialID, UserID: fixtureUserID,
			PublicKey:  base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("inert-local-fixture-public-key-%02d", index))),
			Transports: []string{"internal"}, Label: &label,
			CreatedAt: now.Add(-time.Duration(90-index) * 24 * time.Hour).UTC(), LastUsedAt: now.Add(-time.Duration(index-1) * time.Hour).UTC(),
		}
		if err := repository.CreateCredential(ctx, credential); err != nil {
			return fmt.Errorf("seed local credential: %w", err)
		}
		credentialIDs = append(credentialIDs, credentialID)
	}
	if err := repository.CreateCredentialIndex(ctx, model.CredentialIndex{
		SchemaVersion: model.SchemaVersion, UserID: fixtureUserID, CredentialIDs: credentialIDs,
	}); err != nil {
		return fmt.Errorf("seed local credential index: %w", err)
	}
	for index := 1; index <= 64; index++ {
		createdAt := now.Add(-time.Duration(index) * time.Hour).UTC()
		if err := repository.CreateInvite(ctx, model.Invite{
			SchemaVersion:   model.SchemaVersion,
			InviteID:        deterministicOpaqueID(fmt.Sprintf("local-fixture-invite-id-%04d", index)),
			TokenHash:       deterministicHash(fmt.Sprintf("local-fixture-invite-token-%04d", index)),
			CreatedByUserID: fixtureUserID,
			CreatedAt:       createdAt,
			MaxUses:         1,
		}); err != nil {
			return fmt.Errorf("seed local invite: %w", err)
		}
	}
	if err := repository.CreateAdminRoles(ctx, model.AdminRoles{SchemaVersion: model.SchemaVersion, UserIDs: []domain.UserID{fixtureUserID}}); err != nil {
		return fmt.Errorf("seed local administrator: %w", err)
	}
	return nil
}

func deterministicOpaqueID(value string) string {
	digest := sha256.Sum256([]byte(value))
	return base64.RawURLEncoding.EncodeToString(digest[:16])
}

func deterministicHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

type fixtureFile struct {
	path      string
	mediaType string
	body      []byte
}

func seedWorkspace(ctx context.Context, files fileService, dataPlane http.Handler, userID domain.UserID) error {
	directories := []string{
		"/Backups", "/Backups/Brand", "/Backups/Brand/Published", "/Backups/Photography", "/Backups/Photography/Selects",
		"/Backups/Projects", "/Backups/Projects/Project Atlas", "/Backups/Projects/Project Atlas/Assets",
		"/Brand", "/Brand/Published", "/Photography", "/Photography/Selects", "/Projects", "/Projects/Archive",
		"/Projects/Project Atlas", "/Projects/Project Atlas/Assets", "/Scale Lab", "/Scale Lab/Reference",
	}
	for _, value := range directories {
		path := domain.MustParseUserPath(value)
		if _, err := files.CreateDirectory(ctx, userID, domain.CreateDirectoryRequest{Path: path}); err != nil {
			return fmt.Errorf("seed local directory %s: %w", value, err)
		}
	}

	fixtureFiles, err := representativeFiles()
	if err != nil {
		return err
	}
	denseCollections := []struct {
		directory string
		prefix    string
		count     int
	}{
		{directory: "", prefix: "Workspace item", count: 140},
		{directory: "/Brand", prefix: "Brand asset", count: 110},
		{directory: "/Photography", prefix: "Contact sheet", count: 110},
		{directory: "/Projects", prefix: "Project file", count: 110},
		{directory: "/Projects/Archive", prefix: "Archived file", count: 110},
		{directory: "/Scale Lab", prefix: "Scale sample", count: 110},
	}
	for _, collection := range denseCollections {
		for index := 1; index <= collection.count; index++ {
			fixtureFiles = append(fixtureFiles, fixtureFile{
				path:      fmt.Sprintf("%s/%s %04d.txt", collection.directory, collection.prefix, index),
				mediaType: "text/plain",
				body:      []byte(fmt.Sprintf("%s %04d\n", collection.prefix, index)),
			})
		}
	}
	for index := 1; index <= 360; index++ {
		fixtureFiles = append(fixtureFiles, fixtureFile{
			path:      fmt.Sprintf("/Scale Lab/Reference/Asset %04d.txt", index),
			mediaType: "text/plain",
			body:      []byte(fmt.Sprintf("Asset %04d\n", index)),
		})
	}
	fixtureFiles = append(fixtureFiles, duplicateReviewFiles()...)
	for _, file := range fixtureFiles {
		if err := upload(ctx, files, dataPlane, userID, file); err != nil {
			return err
		}
	}
	if _, err := files.SetDuplicateDirectoryIgnored(ctx, userID, domain.SetDuplicateDirectoryIgnoredRequest{
		Left:    domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/Brand/Published")},
		Right:   domain.DuplicateLocation{Area: domain.AreaLive, Path: domain.MustParseUserPath("/Backups/Brand/Published")},
		Ignored: true,
	}); err != nil {
		return fmt.Errorf("seed ignored duplicate directory pair: %w", err)
	}
	if err := ignoreExactDuplicateFixture(ctx, files, userID, "/Brand/Published", "/Backups/Brand/Published"); err != nil {
		return err
	}

	trashed := []domain.UserPath{
		domain.MustParseUserPath("/Discarded direction.txt"),
		domain.MustParseUserPath("/Old contact sheet.png"),
	}
	for index := 1; index <= 140; index++ {
		trashed = append(trashed, domain.MustParseUserPath(fmt.Sprintf("/Scale Lab/Reference/Asset %04d.txt", index)))
	}
	for offset := 0; offset < len(trashed); offset += drive.MaxBatchItems {
		end := min(offset+drive.MaxBatchItems, len(trashed))
		trashResult, err := files.Trash(ctx, userID, trashed[offset:end], fmt.Sprintf("local-fixture-trash-%04d", offset/drive.MaxBatchItems+1))
		if err != nil {
			return fmt.Errorf("seed local trash: %w", err)
		}
		for _, item := range trashResult.Items {
			if item.State != domain.OperationSucceeded {
				return domain.NewError(domain.ErrorInternal, "local fixture trash operation failed")
			}
		}
	}
	sharePaths := []string{"/Photography"}
	for index := 201; index <= 263; index++ {
		sharePaths = append(sharePaths, fmt.Sprintf("/Scale Lab/Reference/Asset %04d.txt", index))
	}
	for index, value := range sharePaths {
		if _, err := files.CreateShare(ctx, userID, domain.MustParseUserPath(value), nil, fmt.Sprintf("local-fixture-share-%04d", index+1)); err != nil {
			return fmt.Errorf("seed local share: %w", err)
		}
	}
	return nil
}

func ignoreExactDuplicateFixture(ctx context.Context, files fileService, userID domain.UserID, leftPath, rightPath string) error {
	groups, err := files.DuplicateGroups(ctx, userID, domain.DuplicateGroupRequest{Kind: domain.DuplicateDirectory, Limit: 100, IncludeIgnored: true})
	if err != nil {
		return fmt.Errorf("list exact duplicate fixture groups: %w", err)
	}
	for _, group := range groups.Groups {
		occurrences, occurrenceErr := files.DuplicateOccurrences(ctx, userID, domain.DuplicateOccurrenceRequest{GroupID: group.ID, Limit: 100})
		if occurrenceErr != nil {
			return fmt.Errorf("list exact duplicate fixture locations: %w", occurrenceErr)
		}
		foundLeft, foundRight := false, false
		for _, occurrence := range occurrences.Occurrences {
			foundLeft = foundLeft || occurrence.Area == domain.AreaLive && occurrence.Path.String() == leftPath
			foundRight = foundRight || occurrence.Area == domain.AreaLive && occurrence.Path.String() == rightPath
		}
		if !foundLeft || !foundRight {
			continue
		}
		if _, err := files.SetDuplicateIgnored(ctx, userID, domain.SetDuplicateIgnoredRequest{GroupID: group.ID, Ignored: true, ExpectedRevision: group.IgnoreRevision}); err != nil {
			return fmt.Errorf("seed ignored exact duplicate group: %w", err)
		}
		return nil
	}
	return domain.NewError(domain.ErrorInternal, "ignored exact duplicate fixture group was not indexed")
}

func duplicateReviewFiles() []fixtureFile {
	files := make([]fixtureFile, 0, 22)
	for _, item := range []struct {
		relative  string
		mediaType string
		body      string
	}{
		{relative: "Brief.txt", mediaType: "text/plain", body: "Project Atlas brief\nGoals, owners, delivery notes.\n"},
		{relative: "Schedule.csv", mediaType: "text/csv", body: "phase,status\nDiscovery,complete\nBuild,active\n"},
		{relative: "Assets/Palette.csv", mediaType: "text/csv", body: "token,value\nprimary,#166374\nsurface,#f7f7f4\n"},
		{relative: "Assets/Readme.txt", mediaType: "text/plain", body: "Approved Project Atlas working assets.\n"},
	} {
		for _, root := range []string{"/Projects/Project Atlas", "/Backups/Projects/Project Atlas"} {
			files = append(files, fixtureFile{path: root + "/" + item.relative, mediaType: item.mediaType, body: []byte(item.body)})
		}
	}
	for _, item := range []struct {
		relative string
		body     string
	}{
		{relative: "Coastline notes.txt", body: "Select 14 for the final coastline sequence.\n"},
		{relative: "Frame 001.txt", body: "shared frame metadata 001\n"},
		{relative: "Frame 002.txt", body: "shared frame metadata 002\n"},
	} {
		for _, root := range []string{"/Photography/Selects", "/Backups/Photography/Selects"} {
			files = append(files, fixtureFile{path: root + "/" + item.relative, mediaType: "text/plain", body: []byte(item.body)})
		}
	}
	files = append(files,
		fixtureFile{path: "/Photography/Selects/Only in working set.txt", mediaType: "text/plain", body: []byte("Unpublished shortlist notes.\n")},
		fixtureFile{path: "/Backups/Photography/Selects/Only in backup.txt", mediaType: "text/plain", body: []byte("Historic selection retained in backup.\n")},
	)
	for _, root := range []string{"/Brand/Published", "/Backups/Brand/Published"} {
		files = append(files,
			fixtureFile{path: root + "/Release notes.txt", mediaType: "text/plain", body: []byte("Intentional published mirror.\n")},
			fixtureFile{path: root + "/Manifest.csv", mediaType: "text/csv", body: []byte("asset,revision\nmark,4\nwordmark,2\n")},
		)
	}
	return files
}

func representativeFiles() ([]fixtureFile, error) {
	primaryMark, err := samplePNG(640, 400, color.RGBA{R: 0x22, G: 0x2A, B: 0x3A, A: 0xFF}, color.RGBA{R: 0xEB, G: 0xFF, B: 0x57, A: 0xFF})
	if err != nil {
		return nil, err
	}
	coastline, err := samplePNG(900, 600, color.RGBA{R: 0xD8, G: 0xEC, B: 0xF2, A: 0xFF}, color.RGBA{R: 0x16, G: 0x63, B: 0x74, A: 0xFF})
	if err != nil {
		return nil, err
	}
	nightStudy, err := samplePNG(900, 600, color.RGBA{R: 0x15, G: 0x18, B: 0x21, A: 0xFF}, color.RGBA{R: 0xCE, G: 0x82, B: 0xFF, A: 0xFF})
	if err != nil {
		return nil, err
	}
	contactSheet, err := samplePNG(720, 480, color.RGBA{R: 0xEE, G: 0xEC, B: 0xE4, A: 0xFF}, color.RGBA{R: 0xB2, G: 0x30, B: 0x3C, A: 0xFF})
	if err != nil {
		return nil, err
	}
	return []fixtureFile{
		{path: "/Start here.txt", mediaType: "text/plain", body: []byte("EndlessFS local UI fixture\nBrowse, sort, search, preview, share, trash, and restore.\n")},
		{path: "/Brand/EndlessFS mark.png", mediaType: "image/png", body: primaryMark},
		{path: "/Brand/Token reference.txt", mediaType: "text/plain", body: []byte("primary\nprimary tint\nsurface\ntext\nsuccess\nwarning\nerror\n")},
		{path: "/Photography/Coastline.png", mediaType: "image/png", body: coastline},
		{path: "/Photography/Night study.png", mediaType: "image/png", body: nightStudy},
		{path: "/Photography/Field notes.txt", mediaType: "text/plain", body: []byte("Frame studies\nContact sheets\nFinal selects\n")},
		{path: "/Projects/Launch plan.pdf", mediaType: "application/pdf", body: minimalPDF("EndlessFS launch plan")},
		{path: "/Projects/Budget.csv", mediaType: "text/csv", body: []byte("item,status\nDesign,complete\nImplementation,active\nValidation,pending\n")},
		{path: "/Projects/Archive/Decision log.txt", mediaType: "text/plain", body: []byte("Deterministic layout\nSemantic tokens\nSparse interface text\n")},
		{path: "/Discarded direction.txt", mediaType: "text/plain", body: []byte("Recoverable local fixture item.\n")},
		{path: "/Old contact sheet.png", mediaType: "image/png", body: contactSheet},
	}, nil
}

func upload(ctx context.Context, files fileService, dataPlane http.Handler, userID domain.UserID, file fixtureFile) error {
	path := domain.MustParseUserPath(file.path)
	digest := sha256.Sum256([]byte(file.path))
	idempotencyKey := "local-fixture-upload-" + base64.RawURLEncoding.EncodeToString(digest[:12])
	capability, err := files.CreateUpload(ctx, userID, domain.CreateUploadRequest{
		Path: path, Size: int64(len(file.body)), MediaType: file.mediaType, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return fmt.Errorf("seed local upload %s: %w", file.path, err)
	}
	request := httptest.NewRequest(capability.Method, capability.URL, bytes.NewReader(file.body)).WithContext(ctx)
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	dataPlane.ServeHTTP(response, request)
	if response.Code < http.StatusOK || response.Code >= http.StatusMultipleChoices {
		return domain.NewError(domain.ErrorInternal, "local fixture data transfer failed")
	}
	if _, err := files.CompleteUpload(ctx, userID, domain.CompleteUploadRequest{
		UploadID: capability.UploadID, Path: path, Size: int64(len(file.body)), MediaType: file.mediaType,
	}); err != nil {
		return fmt.Errorf("complete local upload %s: %w", file.path, err)
	}
	return nil
}

func samplePNG(width, height int, surface, accent color.RGBA) ([]byte, error) {
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			value := surface
			if (x+y)/48%2 == 0 || (x > width/5 && x < width*4/5 && y > height/4 && y < height*3/4) {
				value = accent
			}
			canvas.SetRGBA(x, y, value)
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, canvas); err != nil {
		return nil, fmt.Errorf("encode local fixture image: %w", err)
	}
	return output.Bytes(), nil
}

func minimalPDF(title string) []byte {
	content := "BT /F1 24 Tf 72 720 Td (" + strings.NewReplacer("\\", "\\\\", "(", "\\(", ")", "\\)").Replace(title) + ") Tj ET"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
		"<< /Length " + strconv.Itoa(len(content)) + " >>\nstream\n" + content + "\nendstream",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var output bytes.Buffer
	output.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects)+1)
	for index, object := range objects {
		offsets[index+1] = output.Len()
		fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := output.Len()
	fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for index := 1; index <= len(objects); index++ {
		fmt.Fprintf(&output, "%010d 00000 n \n", offsets[index])
	}
	fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return output.Bytes()
}

type cookieIssuer interface {
	Cookie(auth.IssuedSession) *http.Cookie
	CSRFCookie(auth.IssuedSession) *http.Cookie
}

// LoginHandler exposes one side-effect-free GET that places the session issued
// during fixture startup into a loopback browser and redirects to the app.
func LoginHandler(next http.Handler, cookies cookieIssuer, issued auth.IssuedSession) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != LoginPath {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.SetCookie(w, cookies.Cookie(issued))
		http.SetCookie(w, cookies.CSRFCookie(issued))
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
}
