// Package providercontract defines reusable provider and data-plane semantics.
package providercontract

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/provider"
)

const (
	faultOperationUploadData = "upload_data"
	faultOperationStat       = "stat"
	faultOperationDelete     = "delete"
	faultInterruptedUpload   = "interrupted_upload"
	faultUnavailable         = "unavailable"
	faultPartialOperation    = "partial_operation"
)

type ByteCounts struct {
	Control  int64
	Upload   int64
	Download int64
}

type Harness struct {
	Storage        provider.Storage
	Client         *http.Client
	Advance        func(time.Duration)
	InjectFault    func(operation, fault string)
	UploadOffset   func(context.Context, domain.Scope, domain.UploadID) (int64, error)
	SimulateOffset func(context.Context, domain.Scope, domain.UploadID, int64) error
	ByteCounts     func() ByteCounts
}

type Factory func(t *testing.T) Harness

func Run(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("directories listings and isolation", func(t *testing.T) {
		harness := factory(t)
		userA := testScope(t, 0x11, domain.AreaLive)
		userB := testScope(t, 0x22, domain.AreaLive)
		root := domain.MustParseUserPath("/")
		for index := range 5 {
			path := domain.MustParseUserPath(fmt.Sprintf("/folder-%d", index))
			if _, err := harness.Storage.CreateDirectory(context.Background(), userA, domain.CreateDirectoryRequest{Path: path}); err != nil {
				t.Fatalf("CreateDirectory() error = %v", err)
			}
		}
		first, err := harness.Storage.List(context.Background(), userA, domain.ListRequest{Directory: root, PageSize: 2})
		if err != nil || first.Current.Path != root || first.Current.Kind != domain.EntryDirectory || first.Current.Size != 0 || first.Current.FileCount != 0 || len(first.Entries) != 2 || first.NextCursor == "" {
			t.Fatalf("first List() = %+v, %v", first, err)
		}
		_, _ = harness.Storage.CreateDirectory(context.Background(), userA, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/later")})
		if _, err := harness.Storage.List(context.Background(), userB, domain.ListRequest{Directory: root, PageSize: 2, Cursor: first.NextCursor}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("cross-scope cursor error = %v", err)
		}
		second, err := harness.Storage.List(context.Background(), userA, domain.ListRequest{Directory: root, PageSize: 2, Cursor: first.NextCursor})
		if err != nil || second.Current != first.Current || len(second.Entries) != 2 {
			t.Fatalf("second List() = %+v, %v", second, err)
		}
		third, err := harness.Storage.List(context.Background(), userA, domain.ListRequest{Directory: root, PageSize: 2, Cursor: second.NextCursor})
		if err != nil || third.Current != first.Current || len(third.Entries) != 1 || third.NextCursor != "" {
			t.Fatalf("third List() = %+v, %v", third, err)
		}
		if _, err := harness.Storage.Stat(context.Background(), userB, domain.MustParseUserPath("/folder-0")); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("cross-scope Stat() error = %v", err)
		}
	})

	t.Run("snapshot-consistent current directory and batched child lookup", func(t *testing.T) {
		harness := factory(t)
		scope := testScope(t, 0x23, domain.AreaLive)
		other := testScope(t, 0x24, domain.AreaLive)
		root := domain.MustParseUserPath("/")
		folder := domain.MustParseUserPath("/folder")
		empty := domain.MustParseUserPath("/empty")
		for _, path := range []domain.UserPath{folder, empty} {
			if _, err := harness.Storage.CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: path}); err != nil {
				t.Fatal(err)
			}
		}
		uploadFile(t, harness, scope, domain.MustParseUserPath("/folder/a.txt"), []byte("four"), false)
		uploadFile(t, harness, scope, domain.MustParseUserPath("/folder/b.txt"), []byte("sixsix"), false)

		first, err := harness.Storage.List(context.Background(), scope, domain.ListRequest{Directory: folder, PageSize: 1})
		if err != nil || first.Current.Path != folder || first.Current.Kind != domain.EntryDirectory || first.Current.Size != 10 || first.Current.FileCount != 2 || len(first.Entries) != 1 || first.NextCursor == "" {
			t.Fatalf("first folder List() = %+v, %v", first, err)
		}
		uploadFile(t, harness, scope, domain.MustParseUserPath("/folder/c.txt"), []byte("new"), false)
		second, err := harness.Storage.List(context.Background(), scope, domain.ListRequest{Directory: folder, PageSize: 1, Cursor: first.NextCursor})
		if err != nil || second.Current != first.Current || second.Current.Size != 10 || second.Current.FileCount != 2 || len(second.Entries) != 1 {
			t.Fatalf("snapshot folder List() = %+v, %v; want current %+v", second, err, first.Current)
		}
		fresh, err := harness.Storage.List(context.Background(), scope, domain.ListRequest{Directory: folder})
		if err != nil || fresh.Current.Size != 13 || fresh.Current.FileCount != 3 {
			t.Fatalf("fresh folder List() = %+v, %v; want current size/count 13/3", fresh, err)
		}
		emptyPage, err := harness.Storage.List(context.Background(), scope, domain.ListRequest{Directory: empty})
		if err != nil || emptyPage.Current.Path != empty || emptyPage.Current.Size != 0 || emptyPage.Current.FileCount != 0 || len(emptyPage.Entries) != 0 {
			t.Fatalf("empty List() = %+v, %v", emptyPage, err)
		}

		lookup, err := harness.Storage.LookupChildren(context.Background(), scope, domain.ChildLookupRequest{Directory: root, Names: []string{"empty", "folder"}})
		if err != nil || lookup.Current.Path != root || lookup.Current.Size != 13 || lookup.Current.FileCount != 3 || len(lookup.Entries) != 2 || lookup.Entries[0].Path != empty || lookup.Entries[0].Size != 0 || lookup.Entries[0].FileCount != 0 || lookup.Entries[1].Path != folder || lookup.Entries[1].Size != 13 || lookup.Entries[1].FileCount != 3 {
			t.Fatalf("LookupChildren() = %+v, %v", lookup, err)
		}
		if _, err := harness.Storage.LookupChildren(context.Background(), scope, domain.ChildLookupRequest{Directory: root, Names: []string{"folder", "folder"}}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("duplicate LookupChildren() error = %v", err)
		}
		if _, err := harness.Storage.LookupChildren(context.Background(), scope, domain.ChildLookupRequest{Directory: root}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("empty LookupChildren() error = %v", err)
		}
		if _, err := harness.Storage.LookupChildren(context.Background(), scope, domain.ChildLookupRequest{Directory: root, Names: []string{"../folder"}}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid-name LookupChildren() error = %v", err)
		}
		if _, err := harness.Storage.LookupChildren(context.Background(), scope, domain.ChildLookupRequest{Directory: root, Names: make([]string, 1001)}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("oversized LookupChildren() error = %v", err)
		}
		if _, err := harness.Storage.LookupChildren(context.Background(), scope, domain.ChildLookupRequest{Directory: root, Names: []string{"missing"}}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing LookupChildren() error = %v", err)
		}
		if _, err := harness.Storage.LookupChildren(context.Background(), other, domain.ChildLookupRequest{Directory: root, Names: []string{"folder"}}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("cross-scope LookupChildren() error = %v", err)
		}
	})

	t.Run("conflicts versions and concurrent create", func(t *testing.T) {
		harness := factory(t)
		scope := testScope(t, 0x31, domain.AreaLive)
		path := domain.MustParseUserPath("/folder")
		entry, err := harness.Storage.CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := harness.Storage.CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: path}); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("conflicting create error = %v", err)
		}
		renamed, err := harness.Storage.CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: path, Conflict: domain.ConflictRename})
		if err != nil || renamed.Path.String() != "/folder (1)" {
			t.Fatalf("renamed directory = %+v, %v", renamed, err)
		}
		if _, err := harness.Storage.CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: path, Conflict: domain.ConflictReplace, ExpectedVersion: "stale"}); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("stale replace error = %v", err)
		}
		replaced, err := harness.Storage.CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: path, Conflict: domain.ConflictReplace, ExpectedVersion: entry.Version})
		if err != nil || replaced.Version == entry.Version {
			t.Fatalf("replace = %+v, %v", replaced, err)
		}

		concurrent := domain.MustParseUserPath("/concurrent")
		var successes atomic.Int32
		var wait sync.WaitGroup
		for range 24 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				if _, err := harness.Storage.CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: concurrent}); err == nil {
					successes.Add(1)
				} else if !errors.Is(err, domain.ErrConflict) {
					t.Errorf("CreateDirectory() error = %v (cause: %v)", err, errors.Unwrap(err))
				}
			}()
		}
		wait.Wait()
		if successes.Load() != 1 {
			t.Fatalf("successful concurrent creates = %d", successes.Load())
		}
	})

	t.Run("single upload direct download and range", func(t *testing.T) {
		harness := factory(t)
		scope := testScope(t, 0x41, domain.AreaLive)
		path := domain.MustParseUserPath("/hello.txt")
		content := []byte("hello world")
		entry := uploadFile(t, harness, scope, path, content, false)
		capability, err := harness.Storage.CreateDownload(context.Background(), scope, domain.CreateDownloadRequest{
			Path: path, Version: entry.Version, Disposition: domain.DispositionAttachment,
		})
		if err != nil {
			t.Fatalf("CreateDownload() error = %v", err)
		}
		request, _ := http.NewRequest(capability.Method, capability.URL, nil)
		request.Header.Set("Range", "bytes=1-3")
		response, err := harness.Client.Do(request)
		if err != nil {
			t.Fatalf("download request error = %v", err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != http.StatusPartialContent || string(body) != "ell" || response.Header.Get("Content-Range") != "bytes 1-3/11" {
			t.Fatalf("range response = %d %q headers=%v", response.StatusCode, body, response.Header)
		}
		counts := harness.ByteCounts()
		if counts.Control != 0 || counts.Upload != int64(len(content)) || counts.Download != 3 {
			t.Fatalf("byte instrumentation = %+v", counts)
		}
	})

	t.Run("resumable retry checksum and abort", func(t *testing.T) {
		harness := factory(t)
		scope := testScope(t, 0x51, domain.AreaLive)
		path := domain.MustParseUserPath("/resumable.bin")
		capability, err := harness.Storage.CreateUpload(context.Background(), scope, domain.CreateUploadRequest{
			Path: path, Size: 8, MediaType: "application/octet-stream", Resumable: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if harness.InjectFault != nil {
			harness.InjectFault(faultOperationUploadData, faultInterruptedUpload)
		}
		response := sendUpload(t, harness.Client, capability, []byte("abcdefgh"), 0)
		if harness.InjectFault != nil && (response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Upload-Offset") != "4") {
			closeBody(t, response)
			t.Fatalf("interrupted response = %d offset=%q", response.StatusCode, response.Header.Get("Upload-Offset"))
		}
		closeBody(t, response)
		offset, err := harness.UploadOffset(context.Background(), scope, capability.UploadID)
		wantOffset := int64(8)
		if harness.InjectFault != nil {
			wantOffset = 4
		}
		if err != nil || offset != wantOffset {
			t.Fatalf("UploadOffset() = %d, %v", offset, err)
		}
		if harness.InjectFault != nil {
			response = sendUpload(t, harness.Client, capability, []byte("efgh"), offset)
			closeBody(t, response)
			if !successfulUploadStatus(response.StatusCode) {
				t.Fatalf("resume status = %d", response.StatusCode)
			}
		}
		completion := domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: 8, MediaType: "application/octet-stream"}
		if _, err := harness.Storage.CompleteUpload(context.Background(), scope, completion); err != nil {
			t.Fatalf("CompleteUpload() error = %v", err)
		}

		abortPath := domain.MustParseUserPath("/abort.bin")
		abortCapability, err := harness.Storage.CreateUpload(context.Background(), scope, domain.CreateUploadRequest{
			Path: abortPath, Size: 1, MediaType: "application/octet-stream",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := harness.Storage.AbortUpload(context.Background(), scope, abortCapability.UploadID); err != nil {
			t.Fatal(err)
		}
		response = sendUpload(t, harness.Client, abortCapability, []byte("x"), 0)
		closeBody(t, response)
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("aborted capability status = %d", response.StatusCode)
		}
		if _, err := harness.Storage.Stat(context.Background(), scope, abortPath); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("aborted upload became visible: %v", err)
		}
	})

	t.Run("capability expiry and scope", func(t *testing.T) {
		harness := factory(t)
		owner := testScope(t, 0x61, domain.AreaLive)
		other := testScope(t, 0x62, domain.AreaLive)
		path := domain.MustParseUserPath("/expiring.txt")
		entry := uploadFile(t, harness, owner, path, []byte("value"), false)
		if _, err := harness.Storage.CreateDownload(context.Background(), other, domain.CreateDownloadRequest{Path: path, Version: entry.Version}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("cross-scope download error = %v", err)
		}
		capability, err := harness.Storage.CreateDownload(context.Background(), owner, domain.CreateDownloadRequest{Path: path, Version: entry.Version})
		if err != nil {
			t.Fatal(err)
		}
		harness.Advance(11 * time.Minute)
		response, err := harness.Client.Get(capability.URL)
		if err != nil {
			t.Fatal(err)
		}
		closeBody(t, response)
		if response.StatusCode != http.StatusGone {
			t.Fatalf("expired capability status = %d", response.StatusCode)
		}
	})

	t.Run("recursive operations idempotency and faults", func(t *testing.T) {
		harness := factory(t)
		live := testScope(t, 0x71, domain.AreaLive)
		trash := testScope(t, 0x71, domain.AreaTrash)
		other := testScope(t, 0x72, domain.AreaLive)
		_, _ = harness.Storage.CreateDirectory(context.Background(), live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/tree")})
		uploadFile(t, harness, live, domain.MustParseUserPath("/tree/file.txt"), []byte("tree"), false)
		request := domain.CopyRequest{
			Source: domain.MustParseUserPath("/tree"), Destination: domain.MustParseUserPath("/copy"), IdempotencyKey: "copy-1",
		}
		operation, err := harness.Storage.Copy(context.Background(), live, live, request)
		if err != nil || operation.State != domain.OperationSucceeded {
			t.Fatalf("Copy() = %+v, %v", operation, err)
		}
		replayed, err := harness.Storage.Copy(context.Background(), live, live, request)
		if err != nil || replayed.ID != operation.ID {
			t.Fatalf("replayed Copy() = %+v, %v", replayed, err)
		}
		request.Destination = domain.MustParseUserPath("/different")
		if _, err := harness.Storage.Copy(context.Background(), live, live, request); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("changed idempotent request error = %v", err)
		}
		if _, err := harness.Storage.Copy(context.Background(), live, other, domain.CopyRequest{Source: domain.MustParseUserPath("/tree"), Destination: domain.MustParseUserPath("/stolen")}); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("cross-user Copy() error = %v", err)
		}
		samePathCopy, err := harness.Storage.Copy(context.Background(), live, live, domain.CopyRequest{
			Source:         domain.MustParseUserPath("/tree/file.txt"),
			Destination:    domain.MustParseUserPath("/tree/file.txt"),
			Conflict:       domain.ConflictRename,
			IdempotencyKey: "copy-same-path-rename",
		})
		if err != nil || samePathCopy.State != domain.OperationSucceeded {
			t.Fatalf("same-path renamed Copy() = %+v, %v", samePathCopy, err)
		}
		if _, err := harness.Storage.Stat(context.Background(), live, domain.MustParseUserPath("/tree/file (1).txt")); err != nil {
			t.Fatalf("same-path renamed copy missing: %v", err)
		}
		moved, err := harness.Storage.Move(context.Background(), live, trash, domain.MoveRequest{
			Source: domain.MustParseUserPath("/copy"), Destination: domain.MustParseUserPath("/trashed"), IdempotencyKey: "trash-1",
		})
		if err != nil || moved.State != domain.OperationSucceeded {
			t.Fatalf("Move() = %+v, %v", moved, err)
		}
		if _, err := harness.Storage.Stat(context.Background(), live, domain.MustParseUserPath("/copy")); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("moved source still exists: %v", err)
		}
		if _, err := harness.Storage.Stat(context.Background(), trash, domain.MustParseUserPath("/trashed/file.txt")); err != nil {
			t.Fatalf("moved descendant missing: %v", err)
		}

		if harness.InjectFault != nil {
			harness.InjectFault(faultOperationDelete, faultPartialOperation)
		}
		failed, err := harness.Storage.Delete(context.Background(), trash, domain.DeleteRequest{Path: domain.MustParseUserPath("/trashed"), IdempotencyKey: "delete-failed"})
		wantDeleteState := domain.OperationSucceeded
		if harness.InjectFault != nil {
			wantDeleteState = domain.OperationFailed
		}
		if err != nil || failed.State != wantDeleteState {
			t.Fatalf("failed Delete() = %+v, %v", failed, err)
		}
		if harness.InjectFault != nil {
			if _, err := harness.Storage.Stat(context.Background(), trash, domain.MustParseUserPath("/trashed")); err != nil {
				t.Fatalf("partial-failure tree was removed: %v", err)
			}
		}
		stored, err := harness.Storage.GetOperation(context.Background(), live.UserID(), failed.ID)
		if err != nil || stored.State != wantDeleteState {
			t.Fatalf("GetOperation() = %+v, %v", stored, err)
		}

		if harness.InjectFault != nil {
			harness.InjectFault(faultOperationStat, faultUnavailable)
			if _, err := harness.Storage.Stat(context.Background(), live, domain.MustParseUserPath("/tree")); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("injected unavailable error = %v", err)
			}
		}
	})

	t.Run("recursive byte and file-count aggregate lifecycle", func(t *testing.T) {
		harness := factory(t)
		live := testScope(t, 0x73, domain.AreaLive)
		trash := testScope(t, 0x73, domain.AreaTrash)
		for _, path := range []string{"/tree", "/tree/nested"} {
			if _, err := harness.Storage.CreateDirectory(context.Background(), live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(path)}); err != nil {
				t.Fatal(err)
			}
		}
		uploadFile(t, harness, live, domain.MustParseUserPath("/tree/nested/file.txt"), []byte("four"), false)
		assertAggregate := func(scope domain.Scope, path string, size, fileCount int64) {
			t.Helper()
			entry, err := harness.Storage.Stat(context.Background(), scope, domain.MustParseUserPath(path))
			if err != nil || entry.Size != size || entry.FileCount != fileCount {
				t.Fatalf("Stat(%s) = %+v, %v; want recursive size/count %d/%d", path, entry, err, size, fileCount)
			}
		}
		assertAggregate(live, "/", 4, 1)
		assertAggregate(live, "/tree", 4, 1)
		assertAggregate(live, "/tree/nested", 4, 1)
		page, err := harness.Storage.List(context.Background(), live, domain.ListRequest{Directory: domain.MustParseUserPath("/tree")})
		if err != nil || len(page.Entries) != 1 || page.Entries[0].Size != 4 || page.Entries[0].FileCount != 1 {
			t.Fatalf("List(/tree) = %+v, %v; want child recursive size/count 4/1", page, err)
		}
		if operation, err := harness.Storage.Copy(context.Background(), live, live, domain.CopyRequest{
			Source: domain.MustParseUserPath("/tree"), Destination: domain.MustParseUserPath("/copy"), IdempotencyKey: "aggregate-copy",
		}); err != nil || operation.State != domain.OperationSucceeded {
			t.Fatalf("Copy() = %+v, %v", operation, err)
		}
		assertAggregate(live, "/", 8, 2)
		assertAggregate(live, "/copy", 4, 1)
		if operation, err := harness.Storage.Move(context.Background(), live, trash, domain.MoveRequest{
			Source: domain.MustParseUserPath("/copy/nested"), Destination: domain.MustParseUserPath("/trashed"), IdempotencyKey: "aggregate-trash",
		}); err != nil || operation.State != domain.OperationSucceeded {
			t.Fatalf("trash Move() = %+v, %v", operation, err)
		}
		assertAggregate(live, "/", 4, 1)
		assertAggregate(trash, "/", 4, 1)
		if operation, err := harness.Storage.Move(context.Background(), trash, live, domain.MoveRequest{
			Source: domain.MustParseUserPath("/trashed"), Destination: domain.MustParseUserPath("/restored"), IdempotencyKey: "aggregate-restore",
		}); err != nil || operation.State != domain.OperationSucceeded {
			t.Fatalf("restore Move() = %+v, %v", operation, err)
		}
		assertAggregate(live, "/", 8, 2)
		assertAggregate(trash, "/", 0, 0)
		if operation, err := harness.Storage.Delete(context.Background(), live, domain.DeleteRequest{
			Path: domain.MustParseUserPath("/restored"), IdempotencyKey: "aggregate-delete",
		}); err != nil || operation.State != domain.OperationSucceeded {
			t.Fatalf("Delete() = %+v, %v", operation, err)
		}
		assertAggregate(live, "/", 4, 1)
	})

	t.Run("preview content identity lifecycle", func(t *testing.T) {
		harness := factory(t)
		live := testScope(t, 0x79, domain.AreaLive)
		trash := testScope(t, 0x79, domain.AreaTrash)
		originalPath := domain.MustParseUserPath("/original.txt")
		original := uploadFile(t, harness, live, originalPath, []byte("original"), false)
		if original.ContentID == "" || original.ContentVersion == "" || original.ContentModifiedAt.IsZero() {
			t.Fatalf("new file preview identity = %+v", original.PreviewContentIdentity())
		}

		if _, err := harness.Storage.Move(context.Background(), live, live, domain.MoveRequest{
			Source: originalPath, Destination: domain.MustParseUserPath("/renamed.txt"), IdempotencyKey: "preview-rename",
		}); err != nil {
			t.Fatal(err)
		}
		renamed, err := harness.Storage.Stat(context.Background(), live, domain.MustParseUserPath("/renamed.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if renamed.PreviewContentIdentity() != original.PreviewContentIdentity() {
			t.Fatalf("rename identity = %+v, want %+v", renamed.PreviewContentIdentity(), original.PreviewContentIdentity())
		}

		if _, err := harness.Storage.Move(context.Background(), live, trash, domain.MoveRequest{
			Source: renamed.Path, Destination: domain.MustParseUserPath("/trashed.txt"), IdempotencyKey: "preview-trash",
		}); err != nil {
			t.Fatal(err)
		}
		trashed, err := harness.Storage.Stat(context.Background(), trash, domain.MustParseUserPath("/trashed.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if trashed.PreviewContentIdentity() != original.PreviewContentIdentity() {
			t.Fatalf("trash identity = %+v, want %+v", trashed.PreviewContentIdentity(), original.PreviewContentIdentity())
		}

		if _, err := harness.Storage.Move(context.Background(), trash, live, domain.MoveRequest{
			Source: trashed.Path, Destination: domain.MustParseUserPath("/restored.txt"), IdempotencyKey: "preview-restore",
		}); err != nil {
			t.Fatal(err)
		}
		restored, err := harness.Storage.Stat(context.Background(), live, domain.MustParseUserPath("/restored.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if restored.PreviewContentIdentity() != original.PreviewContentIdentity() {
			t.Fatalf("restore identity = %+v, want %+v", restored.PreviewContentIdentity(), original.PreviewContentIdentity())
		}

		if _, err := harness.Storage.Copy(context.Background(), live, live, domain.CopyRequest{
			Source: restored.Path, Destination: domain.MustParseUserPath("/copy.txt"), IdempotencyKey: "preview-copy",
		}); err != nil {
			t.Fatal(err)
		}
		copied, err := harness.Storage.Stat(context.Background(), live, domain.MustParseUserPath("/copy.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if copied.PreviewContentIdentity() != restored.PreviewContentIdentity() {
			t.Fatalf("copy identity = %+v, want source identity %+v", copied.PreviewContentIdentity(), restored.PreviewContentIdentity())
		}

		harness.Advance(time.Second)
		replacementData := []byte("replacement")
		replacementUpload, err := harness.Storage.CreateUpload(context.Background(), live, domain.CreateUploadRequest{
			Path: restored.Path, Size: int64(len(replacementData)), MediaType: "text/plain", Conflict: domain.ConflictReplace,
			ExpectedVersion: restored.Version,
		})
		if err != nil {
			t.Fatal(err)
		}
		response := sendUpload(t, harness.Client, replacementUpload, replacementData, 0)
		closeBody(t, response)
		if !successfulUploadStatus(response.StatusCode) {
			t.Fatalf("replacement upload status = %d", response.StatusCode)
		}
		replacement, err := harness.Storage.CompleteUpload(context.Background(), live, domain.CompleteUploadRequest{
			UploadID: replacementUpload.UploadID, Path: restored.Path, Size: int64(len(replacementData)), MediaType: "text/plain",
		})
		if err != nil {
			t.Fatal(err)
		}
		if replacement.ContentID == restored.ContentID || replacement.ContentVersion == restored.ContentVersion || !replacement.ContentModifiedAt.After(restored.ContentModifiedAt) {
			t.Fatalf("replacement identity = %+v, previous = %+v", replacement.PreviewContentIdentity(), restored.PreviewContentIdentity())
		}
	})

	t.Run("large logical object", func(t *testing.T) {
		harness := factory(t)
		scope := testScope(t, 0x81, domain.AreaLive)
		path := domain.MustParseUserPath("/huge.bin")
		size := int64(1<<40) + 17
		capability, err := harness.Storage.CreateUpload(context.Background(), scope, domain.CreateUploadRequest{
			Path: path, Size: size, MediaType: "application/octet-stream", Resumable: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := harness.SimulateOffset(context.Background(), scope, capability.UploadID, size); err != nil {
			t.Fatalf("SimulateOffset() error = %v", err)
		}
		entry, err := harness.Storage.CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{
			UploadID: capability.UploadID, Path: path, Size: size, MediaType: "application/octet-stream",
		})
		if err != nil || entry.Size != size || entry.FileCount != 1 {
			t.Fatalf("CompleteUpload() = %+v, %v", entry, err)
		}
		download, err := harness.Storage.CreateDownload(context.Background(), scope, domain.CreateDownloadRequest{Path: path, Version: entry.Version})
		if err != nil {
			t.Fatal(err)
		}
		request, _ := http.NewRequest(download.Method, download.URL, nil)
		request.Header.Set("Range", "bytes="+strconv.FormatInt(size-4, 10)+"-")
		response, err := harness.Client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		closeBody(t, response)
		if response.StatusCode != http.StatusPartialContent || !bytes.Equal(body, []byte{0, 0, 0, 0}) {
			t.Fatalf("logical range = %d %v", response.StatusCode, body)
		}
	})
}

func uploadFile(t *testing.T, harness Harness, scope domain.Scope, path domain.UserPath, data []byte, resumable bool) domain.Entry {
	t.Helper()
	capability, err := harness.Storage.CreateUpload(context.Background(), scope, domain.CreateUploadRequest{
		Path: path, Size: int64(len(data)), MediaType: "text/plain", Resumable: resumable,
	})
	if err != nil {
		t.Fatalf("CreateUpload() error = %v", err)
	}
	response := sendUpload(t, harness.Client, capability, data, 0)
	closeBody(t, response)
	if !successfulUploadStatus(response.StatusCode) {
		t.Fatalf("upload status = %d", response.StatusCode)
	}
	entry, err := harness.Storage.CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{
		UploadID: capability.UploadID, Path: path, Size: int64(len(data)), MediaType: "text/plain",
	})
	if err != nil {
		t.Fatalf("CompleteUpload() error = %v", err)
	}
	return entry
}

func closeBody(t *testing.T, response *http.Response) {
	t.Helper()
	if err := response.Body.Close(); err != nil {
		t.Errorf("close HTTP response body: %v", err)
	}
}

func sendUpload(t *testing.T, client *http.Client, capability domain.UploadCapability, body []byte, offset int64) *http.Response {
	t.Helper()
	request, err := http.NewRequest(capability.Method, capability.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	switch capability.Framing {
	case domain.UploadFramingOffsetHeader:
		request.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
	case "":
		if capability.Protocol == domain.UploadResumable {
			request.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
		}
	case domain.UploadFramingContentRange:
		if len(body) == 0 {
			request.Header.Set("Content-Range", fmt.Sprintf("bytes */%d", capability.DeclaredSize))
		} else {
			end := offset + int64(len(body)) - 1
			request.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, end, capability.DeclaredSize))
		}
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("upload request error = %v", err)
	}
	return response
}

func successfulUploadStatus(status int) bool {
	return status == http.StatusOK || status == http.StatusCreated || status == http.StatusNoContent
}

func testScope(t *testing.T, value byte, area domain.Area) domain.Scope {
	t.Helper()
	userID, err := domain.ParseUserID(base64ID(value))
	if err != nil {
		t.Fatal(err)
	}
	scope, err := domain.NewScope(userID, area)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func base64ID(value byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{value}, 16))
}
