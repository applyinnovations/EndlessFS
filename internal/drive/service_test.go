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
	"strings"
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

func TestServiceBoundaryAndInvalidScopeMatrix(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	if env.service.DataOrigin() == "" {
		t.Fatal("data origin is empty")
	}
	var invalidUser domain.UserID
	path := domain.MustParseUserPath("/boundary")
	uploadID := domain.UploadID("missing")
	operationID := domain.OperationID("missing")
	checks := []func() error{
		func() error {
			_, err := env.service.List(ctx, invalidUser, domain.ListRequest{Directory: domain.MustParseUserPath("/")})
			return err
		},
		func() error { _, err := env.service.Stat(ctx, invalidUser, path); return err },
		func() error {
			_, err := env.service.CreateDirectory(ctx, invalidUser, domain.CreateDirectoryRequest{Path: path})
			return err
		},
		func() error {
			_, err := env.service.CreateUpload(ctx, invalidUser, domain.CreateUploadRequest{Path: path, IdempotencyKey: "valid-boundary-key-001"})
			return err
		},
		func() error { _, err := env.service.UploadStatus(ctx, invalidUser, uploadID); return err },
		func() error {
			_, err := env.service.CompleteUpload(ctx, invalidUser, domain.CompleteUploadRequest{UploadID: uploadID, Path: path})
			return err
		},
		func() error { return env.service.AbortUpload(ctx, invalidUser, uploadID) },
		func() error {
			_, _, err := env.service.Download(ctx, invalidUser, domain.CreateDownloadRequest{Path: path, Version: "v1"}, false)
			return err
		},
		func() error {
			_, err := env.service.Copy(ctx, invalidUser, domain.CopyRequest{Source: path, Destination: domain.MustParseUserPath("/copy"), IdempotencyKey: "valid-boundary-key-002"})
			return err
		},
		func() error {
			_, err := env.service.Move(ctx, invalidUser, domain.MoveRequest{Source: path, Destination: domain.MustParseUserPath("/move"), IdempotencyKey: "valid-boundary-key-003"})
			return err
		},
		func() error {
			_, err := env.service.Trash(ctx, invalidUser, []domain.UserPath{path}, "valid-boundary-key-004")
			return err
		},
		func() error { _, err := env.service.Operation(ctx, invalidUser, operationID); return err },
	}
	for index, check := range checks {
		if err := check(); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid scope check %d = %v", index, err)
		}
	}
	for _, key := range []string{"", "short", strings.Repeat("x", 129), "valid-key-value\n"} {
		if _, err := env.service.CreateUpload(ctx, env.owner, domain.CreateUploadRequest{Path: path, IdempotencyKey: key}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("idempotency key %q = %v", key, err)
		}
	}
	if _, err := drive.NewService(nil, statememory.NewMemoryStore(), env.repo, nil, env.clock, "invalid", "", "", 0); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid service = %v", err)
	}
}

func TestUploadAbortBatchMoveTrashPagingAndEmptyTrash(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	capability, err := env.service.CreateUpload(ctx, env.owner, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/abort.bin"), Size: 1, MediaType: "application/octet-stream", IdempotencyKey: "abort-upload-key-0001"})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.service.AbortUpload(ctx, env.owner, capability.UploadID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.UploadStatus(ctx, env.owner, capability.UploadID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("aborted upload status = %v", err)
	}
	source := upload(t, env, env.owner, "/source.txt", []byte("source"), "text/plain", "boundary-source-upload-1")
	if _, _, err := env.service.Download(ctx, env.owner, domain.CreateDownloadRequest{Path: source.Path, Version: "stale"}, false); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale download = %v", err)
	}
	if _, mode, err := env.service.Download(ctx, env.owner, domain.CreateDownloadRequest{Path: source.Path, Version: source.Version}, false); err != nil || mode != "download" {
		t.Fatalf("attachment download = %q, %v", mode, err)
	}
	batch, err := env.service.BatchCopyMove(ctx, env.owner, []domain.CopyRequest{
		{Source: source.Path, Destination: domain.MustParseUserPath("/moved.txt")},
		{Source: domain.MustParseUserPath("/missing.txt"), Destination: domain.MustParseUserPath("/never.txt")},
	}, true, "boundary-move-batch-01")
	if err != nil || len(batch.Items) != 2 || batch.Items[0].State != domain.OperationSucceeded || batch.Items[1].State != domain.OperationFailed {
		t.Fatalf("move batch = %+v, %v", batch, err)
	}
	if operation, err := env.service.Operation(ctx, env.owner, batch.Items[0].OperationID); err != nil || operation.State != domain.OperationSucceeded {
		t.Fatalf("provider operation = %+v, %v", operation, err)
	}
	if _, err := env.service.BatchCopyMove(ctx, env.owner, nil, false, "boundary-empty-batch-1"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty copy batch = %v", err)
	}
	if _, err := env.service.Trash(ctx, env.owner, nil, "boundary-empty-trash-1"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty trash batch = %v", err)
	}
	if _, err := env.service.Trash(ctx, env.owner, []domain.UserPath{domain.MustParseUserPath("/")}, "boundary-root-trash-01"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("root trash = %v", err)
	}
	duplicate := domain.MustParseUserPath("/moved.txt")
	if _, err := env.service.Trash(ctx, env.owner, []domain.UserPath{duplicate, duplicate}, "boundary-duplicate-01"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("duplicate trash = %v", err)
	}
	first := upload(t, env, env.owner, "/trash-one.txt", []byte("one"), "text/plain", "trash-one-upload-key-1")
	second := upload(t, env, env.owner, "/trash-two.txt", []byte("two"), "text/plain", "trash-two-upload-key-1")
	trashed, err := env.service.Trash(ctx, env.owner, []domain.UserPath{first.Path, second.Path}, "boundary-trash-batch-1")
	if err != nil || len(trashed.Items) != 2 {
		t.Fatalf("trash batch = %+v, %v", trashed, err)
	}
	page, err := env.service.TrashPage(ctx, env.owner, 1, "")
	if err != nil || len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("trash page = %+v, %v", page, err)
	}
	page, err = env.service.TrashPage(ctx, env.owner, 1, page.NextCursor)
	if err != nil || len(page.Items) != 1 || page.NextCursor != "" {
		t.Fatalf("trash final page = %+v, %v", page, err)
	}
	if _, err := env.service.TrashPage(ctx, env.owner, -1, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid trash page = %v", err)
	}
	if _, err := env.service.EmptyTrash(ctx, env.owner, false, "boundary-empty-trash-2"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("unconfirmed empty trash = %v", err)
	}
	emptied, err := env.service.EmptyTrash(ctx, env.owner, true, "boundary-empty-trash-3")
	if err != nil || len(emptied.Items) != 2 {
		t.Fatalf("empty trash = %+v, %v", emptied, err)
	}
	if records, err := env.service.TrashList(ctx, env.owner); err != nil || len(records) != 0 {
		t.Fatalf("trash after empty = %+v, %v", records, err)
	}
}

func TestShareIdempotencyFileRootAndPublicFailureMatrix(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	entry := upload(t, env, env.owner, "/shared.txt", []byte("public"), "text/plain", "share-file-upload-key-1")
	if _, err := env.service.CreateShare(ctx, env.owner, entry.Path, nil, "short"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("short share key = %v", err)
	}
	past := env.clock.Now()
	if _, err := env.service.CreateShare(ctx, env.owner, entry.Path, &past, "share-expired-key-001"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("expired share = %v", err)
	}
	expiry := env.clock.Now().Add(time.Hour)
	created, err := env.service.CreateShare(ctx, env.owner, entry.Path, &expiry, "share-file-key-00001")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := env.service.CreateShare(ctx, env.owner, entry.Path, &expiry, "share-file-key-00001")
	if err != nil || replayed.Record.ShareID != created.Record.ShareID {
		t.Fatalf("share replay = %+v, %v", replayed, err)
	}
	if _, err := env.service.CreateShare(ctx, env.owner, domain.MustParseUserPath("/missing"), nil, "share-file-key-00001"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("share key conflict = %v", err)
	}
	shares, err := env.service.Shares(ctx, env.owner)
	if err != nil || len(shares) != 1 {
		t.Fatalf("shares = %+v, %v", shares, err)
	}
	token := strings.TrimPrefix(created.Link.Reveal(), "http://127.0.0.1:8080/s/")
	page, err := env.service.PublicShare(ctx, token, "", 10, "")
	if err != nil || page.Root.Path != "/" || page.Root.Kind != domain.EntryFile {
		t.Fatalf("public file root = %+v, %v", page, err)
	}
	if _, err := env.service.PublicShare(ctx, token, "/child", 10, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("file-root child = %v", err)
	}
	for _, relative := range []string{"%2e%2e", `\\escape`} {
		if _, err := env.service.PublicShare(ctx, token, relative, 10, ""); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("public relative %q = %v", relative, err)
		}
	}
	for _, test := range []struct {
		token   string
		version domain.Version
	}{
		{"bad", entry.Version},
		{token, ""},
		{token, "stale"},
	} {
		if _, _, err := env.service.PublicDownload(ctx, test.token, "", test.version, false); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("public download failure = %v", err)
		}
	}
	if err := env.service.RevokeShare(ctx, env.owner, created.Record.ShareID); err != nil {
		t.Fatal(err)
	}
	if err := env.service.RevokeShare(ctx, env.owner, created.Record.ShareID); err != nil {
		t.Fatalf("idempotent revoke = %v", err)
	}
}

func TestDriveMutationAndProviderFaultMatrix(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	path := domain.MustParseUserPath("/fault.txt")
	invalidKeyCalls := []func() error{
		func() error {
			_, err := env.service.Copy(ctx, env.owner, domain.CopyRequest{IdempotencyKey: "short"})
			return err
		},
		func() error {
			_, err := env.service.Move(ctx, env.owner, domain.MoveRequest{IdempotencyKey: "short"})
			return err
		},
		func() error {
			_, err := env.service.BatchCopyMove(ctx, env.owner, []domain.CopyRequest{{}}, false, "short")
			return err
		},
		func() error {
			_, err := env.service.Trash(ctx, env.owner, []domain.UserPath{path}, "short")
			return err
		},
		func() error {
			_, err := env.service.Restore(ctx, env.owner, "missing", domain.ConflictFail, "short")
			return err
		},
		func() error { _, err := env.service.PermanentDelete(ctx, env.owner, "missing", "short"); return err },
		func() error { _, err := env.service.EmptyTrash(ctx, env.owner, true, "short"); return err },
	}
	for index, call := range invalidKeyCalls {
		if err := call(); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid mutation key %d = %v", index, err)
		}
	}
	missingBatch, err := env.service.Trash(ctx, env.owner, []domain.UserPath{domain.MustParseUserPath("/missing.txt")}, "trash-missing-item-001")
	if err != nil || len(missingBatch.Items) != 1 || missingBatch.Items[0].State != domain.OperationFailed {
		t.Fatalf("missing trash item = %+v, %v", missingBatch, err)
	}
	replayEntry := upload(t, env, env.owner, "/replay.txt", []byte("replay"), "text/plain", "trash-replay-upload-01")
	first, err := env.service.Trash(ctx, env.owner, []domain.UserPath{replayEntry.Path}, "trash-replay-key-0001")
	if err != nil || first.Items[0].TrashID == "" {
		t.Fatalf("first replay trash = %+v, %v", first, err)
	}
	replay, err := env.service.Trash(ctx, env.owner, []domain.UserPath{replayEntry.Path}, "trash-replay-key-0001")
	if err != nil || replay.Items[0].TrashID != first.Items[0].TrashID {
		t.Fatalf("trash replay = %+v, %v", replay, err)
	}
	conflict, err := env.service.Trash(ctx, env.owner, []domain.UserPath{domain.MustParseUserPath("/different.txt")}, "trash-replay-key-0001")
	if err != nil || conflict.Items[0].State != domain.OperationFailed || conflict.Items[0].ErrorKind != domain.ErrorConflict {
		t.Fatalf("trash replay conflict = %+v, %v", conflict, err)
	}
	if _, err := env.service.Restore(ctx, env.owner, first.Items[0].TrashID, domain.ConflictFail, "restore-fault-key-0001"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.Restore(ctx, env.owner, first.Items[0].TrashID, domain.ConflictRename, "restore-fault-key-0001"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("restore replay conflict = %v", err)
	}

	statEntry := upload(t, env, env.owner, "/restore-stat.txt", []byte("x"), "text/plain", "restore-stat-upload-01")
	statTrash, err := env.service.Trash(ctx, env.owner, []domain.UserPath{statEntry.Path}, "restore-stat-trash-001")
	if err != nil {
		t.Fatal(err)
	}
	env.storage.InjectFault(providermemory.OperationStat, providermemory.FaultUnavailable)
	if _, err := env.service.Restore(ctx, env.owner, statTrash.Items[0].TrashID, domain.ConflictFail, "restore-stat-fault-01"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("restore stat fault = %v", err)
	}
	env.storage.InjectFault(providermemory.OperationMove, providermemory.FaultUnavailable)
	if _, err := env.service.Restore(ctx, env.owner, statTrash.Items[0].TrashID, domain.ConflictFail, "restore-move-fault-01"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("restore move fault = %v", err)
	}
	deleteEntry := upload(t, env, env.owner, "/delete-fault.txt", []byte("x"), "text/plain", "delete-fault-upload-01")
	deleteTrash, err := env.service.Trash(ctx, env.owner, []domain.UserPath{deleteEntry.Path}, "delete-fault-trash-001")
	if err != nil {
		t.Fatal(err)
	}
	env.storage.InjectFault(providermemory.OperationStat, providermemory.FaultUnavailable)
	if _, err := env.service.PermanentDelete(ctx, env.owner, deleteTrash.Items[0].TrashID, "delete-stat-fault-001"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("delete stat fault = %v", err)
	}
	env.storage.InjectFault(providermemory.OperationDelete, providermemory.FaultUnavailable)
	if _, err := env.service.PermanentDelete(ctx, env.owner, deleteTrash.Items[0].TrashID, "delete-data-fault-001"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("delete provider fault = %v", err)
	}
}

func TestPublicShareProviderFailureMatrix(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	_, _ = env.service.CreateDirectory(ctx, env.owner, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/fault-public")})
	entry := upload(t, env, env.owner, "/fault-public/file.txt", []byte("public"), "text/plain", "public-fault-upload-01")
	created, err := env.service.CreateShare(ctx, env.owner, domain.MustParseUserPath("/fault-public"), nil, "public-fault-share-001")
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := env.service.CreateShare(ctx, env.owner, domain.MustParseUserPath("/fault-public"), nil, "public-fault-share-001"); err != nil || replay.Record.ShareID != created.Record.ShareID {
		t.Fatalf("nil-expiry share replay = %+v, %v", replay, err)
	}
	if _, err := env.service.CreateShare(ctx, env.owner, domain.MustParseUserPath("/missing"), nil, "public-missing-share-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("share missing root = %v", err)
	}
	token := strings.TrimPrefix(created.Link.Reveal(), "http://127.0.0.1:8080/s/")
	validMissingToken := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x44}, 32))
	if _, err := env.service.PublicShare(ctx, validMissingToken, "", 10, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown valid token = %v", err)
	}
	env.storage.InjectFault(providermemory.OperationStat, providermemory.FaultUnavailable)
	if _, err := env.service.PublicShare(ctx, token, "", 10, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("public stat fault = %v", err)
	}
	env.storage.InjectFault(providermemory.OperationList, providermemory.FaultUnavailable)
	if _, err := env.service.PublicShare(ctx, token, "", 10, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("public list fault = %v", err)
	}
	env.storage.InjectFault(providermemory.OperationCreateDownload, providermemory.FaultUnavailable)
	if _, _, err := env.service.PublicDownload(ctx, token, "/file.txt", entry.Version, false); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("public download provider fault = %v", err)
	}
}

func TestDriveCancellationAndLateFailureMatrix(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	if _, _, err := env.service.Download(ctx, env.owner, domain.CreateDownloadRequest{Path: domain.MustParseUserPath("/missing"), Version: "v1"}, false); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing download = %v", err)
	}
	source := upload(t, env, env.owner, "/cancel-source.txt", []byte("x"), "text/plain", "cancel-source-upload-1")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := env.service.BatchCopyMove(canceled, env.owner, []domain.CopyRequest{{Source: source.Path, Destination: domain.MustParseUserPath("/cancel-copy.txt")}}, false, "cancel-copy-batch-001"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled copy batch = %v", err)
	}
	if _, err := env.service.Trash(canceled, env.owner, []domain.UserPath{source.Path}, "cancel-trash-batch-01"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled trash batch = %v", err)
	}
	if _, err := env.service.TrashList(canceled, env.owner); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled trash list = %v", err)
	}
	if _, err := env.service.TrashPage(canceled, env.owner, 0, ""); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled trash page = %v", err)
	}
	if _, err := env.service.Restore(canceled, env.owner, "missing", domain.ConflictFail, "cancel-restore-key-001"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled restore = %v", err)
	}
	if _, err := env.service.PermanentDelete(canceled, env.owner, "missing", "cancel-delete-key-0001"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled permanent delete = %v", err)
	}
	if _, err := env.service.EmptyTrash(canceled, env.owner, true, "cancel-empty-trash-001"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled empty trash = %v", err)
	}
	if _, err := env.service.CreateShare(canceled, env.owner, source.Path, nil, "cancel-share-key-00001"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled create share = %v", err)
	}
}
