package drive_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/drive"
	"github.com/applyinnovations/endlessfs/internal/identity"
	"github.com/applyinnovations/endlessfs/internal/model"
	providermemory "github.com/applyinnovations/endlessfs/internal/provider/memory"
	"github.com/applyinnovations/endlessfs/internal/secret"
	statememory "github.com/applyinnovations/endlessfs/internal/state"
)

type driveEnvironment struct {
	service *drive.Service
	storage *providermemory.Provider
	client  *http.Client
	clock   *domain.FixedClock
	repo    *identity.Repository
	owner   domain.UserID
	other   domain.UserID
}

type hashReader struct {
	counter uint64
	pending []byte
}

func (r *hashReader) Read(destination []byte) (int, error) {
	written := 0
	for written < len(destination) {
		if len(r.pending) == 0 {
			r.counter++
			sum := sha256.Sum256([]byte(time.Unix(int64(r.counter), 0).UTC().String()))
			r.pending = append(r.pending, sum[:]...)
		}
		count := copy(destination[written:], r.pending)
		written += count
		r.pending = r.pending[count:]
	}
	return written, nil
}

func newDriveEnvironment(t *testing.T) driveEnvironment {
	t.Helper()
	clock := domain.NewFixedClock(time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC))
	ids := domain.NewIDGenerator(&hashReader{})
	storage := providermemory.New(providermemory.Options{Clock: clock, IDs: ids, UploadTTL: 5 * time.Minute, DownloadTTL: time.Minute})
	server := httptest.NewServer(storage)
	t.Cleanup(server.Close)
	if err := storage.SetDataPlaneBaseURL(server.URL); err != nil {
		t.Fatal(err)
	}
	store := statememory.NewMemoryStore()
	repository := identity.NewRepository(store)
	owner := fixedUserID(t, 0x31)
	other := fixedUserID(t, 0x41)
	for _, userID := range []domain.UserID{owner, other} {
		now := clock.Now()
		if err := repository.CreateAccount(context.Background(), model.Account{SchemaVersion: model.SchemaVersion, UserID: userID, Status: model.AccountEnabled, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	key := secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x77}, 32)))
	service, err := drive.NewService(storage, store, repository, ids, clock, key, "http://127.0.0.1:8080", server.URL, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return driveEnvironment{service: service, storage: storage, client: server.Client(), clock: clock, repo: repository, owner: owner, other: other}
}

func fixedUserID(t *testing.T, value byte) domain.UserID {
	t.Helper()
	id, err := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{value}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func upload(t *testing.T, env driveEnvironment, owner domain.UserID, path string, body []byte, mediaType, key string) domain.Entry {
	t.Helper()
	parsed := domain.MustParseUserPath(path)
	capability, err := env.service.CreateUpload(context.Background(), owner, domain.CreateUploadRequest{Path: parsed, Size: int64(len(body)), MediaType: mediaType, IdempotencyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(capability.Method, capability.URL, bytes.NewReader(body))
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	response, err := env.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("upload status = %d", response.StatusCode)
	}
	entry, err := env.service.CompleteUpload(context.Background(), owner, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: parsed, Size: int64(len(body)), MediaType: mediaType})
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestIntegrationDirectTransfersAndIsolation(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	folder, err := env.service.CreateDirectory(ctx, env.owner, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/docs")})
	if err != nil || folder.Kind != domain.EntryDirectory {
		t.Fatalf("CreateDirectory = %+v, %v", folder, err)
	}
	capability, err := env.service.CreateUpload(ctx, env.owner, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/docs/hello.txt"), Size: 11, MediaType: "text/plain", Resumable: true, IdempotencyKey: "upload-idempotency-001"})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := env.service.CreateUpload(ctx, env.owner, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/docs/hello.txt"), Size: 11, MediaType: "text/plain", Resumable: true, IdempotencyKey: "upload-idempotency-001"})
	if err != nil || replayed.URL != capability.URL {
		t.Fatalf("upload replay = %+v, %v", replayed, err)
	}
	if _, err := env.service.CreateUpload(ctx, env.other, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/docs/hello.txt"), Size: 11, MediaType: "text/plain", IdempotencyKey: "upload-idempotency-001"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("other upload = %v", err)
	}
	request, _ := http.NewRequest(capability.Method, capability.URL, bytes.NewBufferString("hello world"))
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	response, err := env.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	status, err := env.service.UploadStatus(ctx, env.owner, capability.UploadID)
	if err != nil || status.ConfirmedOffset != 11 {
		t.Fatalf("UploadStatus = %+v, %v", status, err)
	}
	if _, err := env.service.UploadStatus(ctx, env.other, capability.UploadID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-user status = %v", err)
	}
	entry, err := env.service.CompleteUpload(ctx, env.owner, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: domain.MustParseUserPath("/docs/hello.txt"), Size: 11, MediaType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := env.service.List(ctx, env.owner, domain.ListRequest{Directory: domain.MustParseUserPath("/docs")})
	if err != nil || len(page.Entries) != 1 {
		t.Fatalf("List = %+v, %v", page, err)
	}
	download, mode, err := env.service.Download(ctx, env.owner, domain.CreateDownloadRequest{Path: entry.Path, Version: entry.Version}, true)
	if err != nil || mode != "text" {
		t.Fatalf("Download = %+v %s, %v", download, mode, err)
	}
	response, err = env.client.Get(download.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "hello world" || response.Header.Get("Content-Disposition") == "" {
		t.Fatalf("download = %q headers=%v", body, response.Header)
	}
	metrics := env.storage.Instrumentation()
	if metrics.ControlPlaneBytes != 0 || metrics.UploadBytes != 11 || metrics.DownloadBytes != 11 {
		t.Fatalf("instrumentation = %+v", metrics)
	}
}

func TestIntegrationCopyMoveTrashRestoreAndDelete(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	_, _ = env.service.CreateDirectory(ctx, env.owner, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/tree")})
	upload(t, env, env.owner, "/tree/file.bin", []byte("data"), "application/octet-stream", "upload-tree-file-001")
	copyRequest := domain.CopyRequest{Source: domain.MustParseUserPath("/tree"), Destination: domain.MustParseUserPath("/copy"), IdempotencyKey: "copy-tree-request-001"}
	first, err := env.service.Copy(ctx, env.owner, copyRequest)
	if err != nil || first.State != domain.OperationSucceeded {
		t.Fatalf("Copy = %+v, %v", first, err)
	}
	replayed, err := env.service.Copy(ctx, env.owner, copyRequest)
	if err != nil || replayed.ID != first.ID {
		t.Fatalf("copy replay = %+v, %v", replayed, err)
	}
	move, err := env.service.Move(ctx, env.owner, domain.MoveRequest{Source: domain.MustParseUserPath("/copy"), Destination: domain.MustParseUserPath("/renamed"), IdempotencyKey: "move-tree-request-001"})
	if err != nil || move.State != domain.OperationSucceeded {
		t.Fatalf("Move = %+v, %v", move, err)
	}
	batchCopy, err := env.service.BatchCopyMove(ctx, env.owner, []domain.CopyRequest{{Source: domain.MustParseUserPath("/tree/file.bin"), Destination: domain.MustParseUserPath("/batch-one.bin")}, {Source: domain.MustParseUserPath("/tree/file.bin"), Destination: domain.MustParseUserPath("/batch-two.bin")}}, false, "copy-batch-request-001")
	if err != nil || len(batchCopy.Items) != 2 || batchCopy.Items[0].State != domain.OperationSucceeded || batchCopy.Items[1].State != domain.OperationSucceeded {
		t.Fatalf("BatchCopyMove = %+v, %v", batchCopy, err)
	}
	batch, err := env.service.Trash(ctx, env.owner, []domain.UserPath{domain.MustParseUserPath("/renamed")}, "trash-tree-request-001")
	if err != nil || batch.Items[0].TrashID == "" {
		t.Fatalf("Trash = %+v, %v", batch, err)
	}
	if _, err := env.service.Stat(ctx, env.owner, domain.MustParseUserPath("/renamed")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("trashed stat = %v", err)
	}
	if aggregate, err := env.service.Operation(ctx, env.owner, batch.OperationID); err != nil || aggregate.State != domain.OperationSucceeded {
		t.Fatalf("batch operation = %+v, %v", aggregate, err)
	}
	records, err := env.service.TrashList(ctx, env.owner)
	if err != nil || len(records) != 1 || records[0].OriginalPath.String() != "/renamed" {
		t.Fatalf("TrashList = %+v, %v", records, err)
	}
	_, _ = env.service.CreateDirectory(ctx, env.owner, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/renamed")})
	restored, err := env.service.Restore(ctx, env.owner, records[0].TrashID, domain.ConflictRename, "restore-tree-request-001")
	if err != nil || restored.State != domain.OperationSucceeded {
		t.Fatalf("Restore = %+v, %v", restored, err)
	}
	if replay, err := env.service.Restore(ctx, env.owner, records[0].TrashID, domain.ConflictRename, "restore-tree-request-001"); err != nil || replay.ID != restored.ID {
		t.Fatalf("restore replay = %+v, %v", replay, err)
	}
	if _, err := env.service.Stat(ctx, env.owner, domain.MustParseUserPath("/renamed (1)/file.bin")); err != nil {
		t.Fatalf("renamed restore stat = %v", err)
	}
	batch, err = env.service.Trash(ctx, env.owner, []domain.UserPath{domain.MustParseUserPath("/renamed (1)")}, "trash-again-request-001")
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := env.service.PermanentDelete(ctx, env.owner, batch.Items[0].TrashID, "delete-trash-request-001")
	if err != nil || deleted.State != domain.OperationSucceeded {
		t.Fatalf("PermanentDelete = %+v, %v", deleted, err)
	}
	if replay, err := env.service.PermanentDelete(ctx, env.owner, batch.Items[0].TrashID, "delete-trash-request-001"); err != nil || replay.ID != deleted.ID {
		t.Fatalf("delete replay = %+v, %v", replay, err)
	}
	if _, err := env.service.Operation(ctx, env.other, first.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-user operation = %v", err)
	}
}

func TestIntegrationSharesPreviewAndRevocation(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	_, _ = env.service.CreateDirectory(ctx, env.owner, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/public")})
	text := upload(t, env, env.owner, "/public/readme.txt", []byte("safe text"), "text/plain", "upload-public-text-01")
	html := upload(t, env, env.owner, "/public/index.html", []byte("<script>x</script>"), "text/html", "upload-public-html-01")
	created, err := env.service.CreateShare(ctx, env.owner, domain.MustParseUserPath("/public"), nil, "share-folder-request-01")
	if err != nil {
		t.Fatal(err)
	}
	token := created.Link.Reveal()[len("http://127.0.0.1:8080/s/"):]
	page, err := env.service.PublicShare(ctx, token, "/", 10, "")
	if err != nil || len(page.Entries) != 2 || page.Entries[0].Path == "/public/readme.txt" {
		t.Fatalf("PublicShare = %+v, %v", page, err)
	}
	if _, err := env.service.PublicShare(ctx, token, "/../outside", 10, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("share traversal = %v", err)
	}
	capability, mode, err := env.service.PublicDownload(ctx, token, "/readme.txt", text.Version, true)
	if err != nil || mode != "text" || capability.URL == "" {
		t.Fatalf("public preview = %+v %s, %v", capability, mode, err)
	}
	if _, _, err := env.service.PublicDownload(ctx, token, "/index.html", html.Version, true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("HTML inline preview = %v", err)
	}
	if _, mode, err := env.service.PublicDownload(ctx, token, "/index.html", html.Version, false); err != nil || mode != "download" {
		t.Fatalf("HTML attachment = %s, %v", mode, err)
	}
	if err := env.service.RevokeShare(ctx, env.other, created.Record.ShareID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-user revoke = %v", err)
	}
	if err := env.service.RevokeShare(ctx, env.owner, created.Record.ShareID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.PublicShare(ctx, token, "/", 10, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("revoked share = %v", err)
	}
	created, err = env.service.CreateShare(ctx, env.owner, domain.MustParseUserPath("/public"), nil, "share-folder-request-02")
	if err != nil {
		t.Fatal(err)
	}
	token = created.Link.Reveal()[len("http://127.0.0.1:8080/s/"):]
	if _, err := env.service.PublicShare(ctx, token, "/", 10, ""); err != nil {
		t.Fatalf("fresh replacement share = %v", err)
	}
	expiry := env.clock.Now().Add(time.Minute)
	expiring, err := env.service.CreateShare(ctx, env.owner, domain.MustParseUserPath("/public"), &expiry, "share-expiring-request-1")
	if err != nil {
		t.Fatal(err)
	}
	expiringToken := expiring.Link.Reveal()[len("http://127.0.0.1:8080/s/"):]
	env.clock.Advance(2 * time.Minute)
	if _, err := env.service.PublicShare(ctx, expiringToken, "/", 10, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired share = %v", err)
	}
	movedShare, err := env.service.CreateShare(ctx, env.owner, domain.MustParseUserPath("/public"), nil, "share-moved-request-001")
	if err != nil {
		t.Fatal(err)
	}
	movedToken := movedShare.Link.Reveal()[len("http://127.0.0.1:8080/s/"):]
	if _, err := env.service.Move(ctx, env.owner, domain.MoveRequest{Source: domain.MustParseUserPath("/public"), Destination: domain.MustParseUserPath("/moved"), IdempotencyKey: "move-shared-root-0001"}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.PublicShare(ctx, movedToken, "/", 10, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("moved root share = %v", err)
	}
	_, err = env.service.Move(ctx, env.owner, domain.MoveRequest{Source: domain.MustParseUserPath("/moved"), Destination: domain.MustParseUserPath("/public"), IdempotencyKey: "move-shared-root-0002"})
	if err != nil {
		t.Fatal(err)
	}
	created, err = env.service.CreateShare(ctx, env.owner, domain.MustParseUserPath("/public"), nil, "share-disabled-request-1")
	if err != nil {
		t.Fatal(err)
	}
	token = created.Link.Reveal()[len("http://127.0.0.1:8080/s/"):]
	account, version, _ := env.repo.Account(ctx, env.owner)
	account.Status = model.AccountDisabled
	account.UpdatedAt = env.clock.Now()
	if _, err := env.repo.UpdateAccount(ctx, account, version); err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.PublicShare(ctx, token, "/", 10, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("disabled owner share = %v", err)
	}
}

func TestSafePreviewAllowlistUsesProviderValidatedMedia(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	cases := []struct {
		name, mediaType, mode string
		body                  []byte
	}{
		{"png", "image/png", "image", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")},
		{"jpeg", "image/jpeg", "image", []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00")},
		{"gif", "image/gif", "image", []byte("GIF89a\x01\x00\x01\x00")},
		{"webp", "image/webp", "image", []byte("RIFF\x08\x00\x00\x00WEBPVP8 ")},
		{"pdf", "application/pdf", "pdf", []byte("%PDF-1.7\n%%EOF")},
		{"text", "text/plain", "text", []byte("plain UTF-8 text")},
	}
	for index, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			entry := upload(t, env, env.owner, "/"+testCase.name, testCase.body, testCase.mediaType, "preview-upload-key-00"+string(rune('a'+index)))
			_, mode, err := env.service.Download(ctx, env.owner, domain.CreateDownloadRequest{Path: entry.Path, Version: entry.Version}, true)
			if err != nil || mode != testCase.mode {
				t.Fatalf("preview = %s, %v entry=%+v", mode, err, entry)
			}
		})
	}
	hostile := upload(t, env, env.owner, "/fake.png", []byte("<script>alert(1)</script>"), "image/png", "preview-hostile-key-001")
	if hostile.MediaType != "application/octet-stream" {
		t.Fatalf("hostile media type = %q", hostile.MediaType)
	}
	if _, _, err := env.service.Download(ctx, env.owner, domain.CreateDownloadRequest{Path: hostile.Path, Version: hostile.Version}, true); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("hostile inline preview = %v", err)
	}
}
