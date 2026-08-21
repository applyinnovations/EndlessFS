package memory

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func boundaryProvider(t *testing.T) (*Provider, domain.Scope, *domain.FixedClock) {
	t.Helper()
	entropy := make([]byte, 1<<16)
	for index := range entropy {
		entropy[index] = byte(index*17 + 3)
	}
	clock := domain.NewFixedClock(time.Date(2034, 1, 2, 3, 4, 5, 0, time.UTC))
	provider := New(Options{Clock: clock, IDs: domain.NewIDGenerator(bytes.NewReader(entropy)), AllowedOrigin: "https://drive.example.test"})
	if err := provider.SetDataPlaneBaseURL("http://127.0.0.1:43210"); err != nil {
		t.Fatal(err)
	}
	userID, err := domain.ParseUserID("AAAAAAAAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	scope, err := domain.NewScope(userID, domain.AreaLive)
	if err != nil {
		t.Fatal(err)
	}
	return provider, scope, clock
}

func TestProviderConfigurationSortingFaultAndScopeBoundaries(t *testing.T) {
	defaults := New(Options{})
	if defaults.clock == nil || defaults.ids == nil || defaults.uploadTTL != 5*time.Minute || defaults.downloadTTL != time.Minute || defaults.maxMaterializedBytes != 16<<20 || defaults.chunkRules.MaximumSize != 8<<20 {
		t.Fatalf("provider defaults = %#v", defaults)
	}
	for _, value := range []string{"", "https://127.0.0.1:1", "http://example.test:1", "http://127.0.0.1", "http://user@127.0.0.1:1", "http://127.0.0.1:1/path", "http://127.0.0.1:1?q=x", "http://127.0.0.1:1#x"} {
		if err := defaults.SetDataPlaneBaseURL(value); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("SetDataPlaneBaseURL(%q) = %v", value, err)
		}
	}
	provider, scope, _ := boundaryProvider(t)
	provider.RecordControlPlaneBytes(17)
	metrics := provider.Instrumentation()
	metrics.ProviderCalls["mutated-copy"] = 1
	if provider.Instrumentation().ControlPlaneBytes != 17 || provider.Instrumentation().ProviderCalls["mutated-copy"] != 0 {
		t.Fatal("instrumentation was not copied")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := validateContextScope(canceled, scope); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled scope = %v", err)
	}
	if err := validateContextScope(context.Background(), domain.Scope{}); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("invalid scope = %v", err)
	}
	for value, wantError := range map[int]bool{0: false, 1: false, 1000: false, -1: true, 1001: true} {
		_, err := normalizePageSize(value)
		if (err != nil) != wantError {
			t.Fatalf("normalizePageSize(%d) = %v", value, err)
		}
	}
	if value, err := parseOffset("17"); err != nil || value != 17 {
		t.Fatalf("parseOffset valid = %d, %v", value, err)
	}
	for _, value := range []string{"", "-1", "1.5", "999999999999999999999"} {
		if _, err := parseOffset(value); err == nil {
			t.Fatalf("parseOffset(%q) succeeded", value)
		}
	}
	entries := []domain.Entry{
		{Path: domain.MustParseUserPath("/b"), Name: "same", Kind: domain.EntryFile, Size: 1, ModifiedAt: time.Unix(2, 0)},
		{Path: domain.MustParseUserPath("/a"), Name: "same", Kind: domain.EntryDirectory, Size: 2, ModifiedAt: time.Unix(1, 0)},
	}
	for _, field := range []domain.SortField{domain.SortName, domain.SortModified, domain.SortSize, domain.SortKind} {
		for _, descending := range []bool{false, true} {
			copyOfEntries := append([]domain.Entry(nil), entries...)
			sortEntries(copyOfEntries, field, descending)
			if len(copyOfEntries) != 2 {
				t.Fatal("sort lost entries")
			}
		}
	}
	if compare(int64(1), int64(2)) != -1 || compare(int64(2), int64(1)) != 1 || compare(int64(1), int64(1)) != 0 {
		t.Fatal("integer comparison is not total")
	}
	for fault, kind := range map[Fault]domain.ErrorKind{
		FaultNotFound: domain.ErrorNotFound, FaultConflict: domain.ErrorConflict, FaultRateLimited: domain.ErrorRateLimited,
		FaultUnavailable: domain.ErrorUnavailable, FaultStaleVersion: domain.ErrorPreconditionFailed, Fault("unknown"): domain.ErrorInternal,
	} {
		provider.InjectFault("boundary", fault)
		if err := provider.beforeLocked("boundary"); domain.KindOf(err) != kind {
			t.Fatalf("fault %q = %v", fault, err)
		}
	}
	provider.InjectFault("deferred", FaultExpired)
	if err := provider.beforeLocked("deferred"); err != nil || !provider.consumeSpecificFaultLocked("deferred", FaultExpired) || provider.consumeSpecificFaultLocked("deferred", FaultExpired) {
		t.Fatal("deferred fault was not preserved and consumed once")
	}
}

func TestProviderControlMethodsRejectInvalidAndStaleRequests(t *testing.T) {
	provider, scope, _ := boundaryProvider(t)
	ctx := context.Background()
	root := domain.MustParseUserPath("/")
	if _, err := provider.List(ctx, scope, domain.ListRequest{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("List invalid path = %v", err)
	}
	if _, err := provider.List(ctx, scope, domain.ListRequest{Directory: root, PageSize: -1}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("List invalid size = %v", err)
	}
	if _, err := provider.List(ctx, scope, domain.ListRequest{Directory: root, Sort: "unknown"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("List invalid sort = %v", err)
	}
	if _, err := provider.List(ctx, scope, domain.ListRequest{Directory: root, Cursor: "unknown"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("List invalid cursor = %v", err)
	}
	if _, err := provider.Stat(ctx, scope, domain.UserPath{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("Stat invalid path = %v", err)
	}
	if entry, err := provider.Stat(ctx, scope, root); err != nil || entry.Version != "root" {
		t.Fatalf("Stat root = %+v, %v", entry, err)
	}
	if _, err := provider.CreateDirectory(ctx, scope, domain.CreateDirectoryRequest{Path: root}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("CreateDirectory root = %v", err)
	}
	if _, err := provider.CreateDirectory(ctx, scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/missing/child")}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("CreateDirectory missing parent = %v", err)
	}
	directory, err := provider.CreateDirectory(ctx, scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/directory")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CreateDirectory(ctx, scope, domain.CreateDirectoryRequest{Path: directory.Path, Conflict: domain.ConflictReplace}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("replace without version = %v", err)
	}
	provider.InjectFault(OperationStat, FaultRateLimited)
	if _, err := provider.Stat(ctx, scope, directory.Path); !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("injected stat = %v", err)
	}

	otherUser, _ := domain.ParseUserID("AQEBAQEBAQEBAQEBAQEBAQ")
	otherScope, _ := domain.NewScope(otherUser, domain.AreaLive)
	invalidCopy := []domain.CopyRequest{
		{},
		{Source: root, Destination: directory.Path},
		{Source: directory.Path, Destination: directory.Path},
		{Source: directory.Path, Destination: domain.MustParseUserPath("/directory/child")},
		{Source: directory.Path, Destination: domain.MustParseUserPath("/target"), Conflict: "invalid"},
		{Source: directory.Path, Destination: domain.MustParseUserPath("/target"), IdempotencyKey: string([]byte{0xff})},
	}
	for index, request := range invalidCopy {
		if _, err := provider.Copy(ctx, scope, scope, request); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid Copy[%d] = %v", index, err)
		}
	}
	if _, err := provider.Copy(ctx, scope, otherScope, domain.CopyRequest{Source: directory.Path, Destination: domain.MustParseUserPath("/other")}); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("cross-user Copy = %v", err)
	}
	if _, err := provider.Copy(ctx, scope, scope, domain.CopyRequest{Source: domain.MustParseUserPath("/missing"), Destination: domain.MustParseUserPath("/target")}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing Copy = %v", err)
	}
	if _, err := provider.Copy(ctx, scope, scope, domain.CopyRequest{Source: directory.Path, Destination: domain.MustParseUserPath("/target"), ExpectedSource: "stale"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale Copy = %v", err)
	}
	if _, err := provider.Delete(ctx, scope, domain.DeleteRequest{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid Delete = %v", err)
	}
	if _, err := provider.Delete(ctx, scope, domain.DeleteRequest{Path: domain.MustParseUserPath("/missing")}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing Delete = %v", err)
	}
	if _, err := provider.Delete(ctx, scope, domain.DeleteRequest{Path: directory.Path, ExpectedVersion: "stale"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale Delete = %v", err)
	}
	if _, err := provider.GetOperation(ctx, domain.UserID{}, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid GetOperation = %v", err)
	}
	if _, err := provider.GetOperation(ctx, scope.UserID(), "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing GetOperation = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := provider.GetOperation(canceled, scope.UserID(), "operation"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled GetOperation = %v", err)
	}
}

func TestProviderTransferAndDataPlaneNegativeMatrix(t *testing.T) {
	provider, scope, clock := boundaryProvider(t)
	ctx := context.Background()
	path := domain.MustParseUserPath("/file.txt")
	for index, request := range []domain.CreateUploadRequest{
		{},
		{Path: path, Size: -1, MediaType: "text/plain"},
		{Path: path, Size: 1, MediaType: "invalid"},
		{Path: path, Size: 1, MediaType: "text/plain", Conflict: "invalid"},
		{Path: path, Size: 1, MediaType: "text/plain", IdempotencyKey: strings.Repeat("x", 129)},
	} {
		if _, err := provider.CreateUpload(ctx, scope, request); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid CreateUpload[%d] = %v", index, err)
		}
	}
	withoutURL := New(Options{})
	if _, err := withoutURL.CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: path, Size: 1, MediaType: "text/plain"}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("CreateUpload without URL = %v", err)
	}
	large := New(Options{MaxMaterializedBytes: 1})
	_ = large.SetDataPlaneBaseURL("http://127.0.0.1:43211")
	if _, err := large.CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: path, Size: 2, MediaType: "text/plain"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("large single CreateUpload = %v", err)
	}

	capability, err := provider.CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: path, Size: 5, MediaType: "text/plain", Resumable: true, IdempotencyKey: "upload-boundary-key"})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := provider.CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: path, Size: 5, MediaType: "text/plain", Resumable: true, IdempotencyKey: "upload-boundary-key"})
	if err != nil || replayed.URL != capability.URL {
		t.Fatalf("upload replay = %+v, %v", replayed, err)
	}
	if _, err := provider.CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: path, Size: 6, MediaType: "text/plain", Resumable: true, IdempotencyKey: "upload-boundary-key"}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("changed upload replay = %v", err)
	}
	if _, err := provider.UploadStatus(ctx, scope, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty UploadStatus = %v", err)
	}
	if _, err := provider.UploadStatus(ctx, scope, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing UploadStatus = %v", err)
	}
	if err := provider.SimulateUploadOffset(ctx, scope, capability.UploadID, 6); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized simulated offset = %v", err)
	}

	for name, request := range map[string]*http.Request{
		"unknown route": httptest.NewRequest(http.MethodGet, "/unknown", nil),
		"wrong origin":  httptest.NewRequest(http.MethodPatch, capability.URL, strings.NewReader("hello")),
		"preflight":     httptest.NewRequest(http.MethodOptions, capability.URL, nil),
		"wrong method":  httptest.NewRequest(http.MethodPut, capability.URL, strings.NewReader("hello")),
		"wrong media":   httptest.NewRequest(http.MethodPatch, capability.URL, strings.NewReader("hello")),
		"wrong offset":  httptest.NewRequest(http.MethodPatch, capability.URL, strings.NewReader("hello")),
		"too large":     httptest.NewRequest(http.MethodPatch, capability.URL, strings.NewReader("123456")),
	} {
		request.URL, _ = url.Parse(request.URL.String())
		switch name {
		case "wrong origin":
			request.Header.Set("Origin", "https://evil.example")
		case "preflight":
			request.Header.Set("Origin", "https://drive.example.test")
		case "wrong media":
			request.Header.Set("Content-Type", "application/octet-stream")
			request.Header.Set("Upload-Offset", "0")
		case "wrong offset":
			request.Header.Set("Content-Type", "text/plain")
			request.Header.Set("Upload-Offset", "4")
		case "too large":
			request.Header.Set("Content-Type", "text/plain")
			request.Header.Set("Upload-Offset", "0")
		}
		response := httptest.NewRecorder()
		provider.ServeHTTP(response, request)
		if response.Code < 300 && name != "preflight" {
			t.Fatalf("%s unexpectedly succeeded: %d", name, response.Code)
		}
		if name == "preflight" && response.Code != http.StatusNoContent {
			t.Fatalf("preflight = %d", response.Code)
		}
	}

	uploadRequest := httptest.NewRequest(http.MethodPatch, capability.URL, strings.NewReader("hello"))
	uploadRequest.Header.Set("Content-Type", "text/plain")
	uploadRequest.Header.Set("Upload-Offset", "0")
	uploadResponse := httptest.NewRecorder()
	provider.ServeHTTP(uploadResponse, uploadRequest)
	if uploadResponse.Code != http.StatusNoContent {
		t.Fatalf("valid data upload = %d %s", uploadResponse.Code, uploadResponse.Body.String())
	}
	if _, err := provider.CompleteUpload(ctx, scope, domain.CompleteUploadRequest{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid CompleteUpload = %v", err)
	}
	if _, err := provider.CompleteUpload(ctx, scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: 4, MediaType: "text/plain"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("constraint CompleteUpload = %v", err)
	}
	entry, err := provider.CompleteUpload(ctx, scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: 5, MediaType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.AbortUpload(ctx, scope, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty AbortUpload = %v", err)
	}
	if err := provider.AbortUpload(ctx, scope, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing AbortUpload = %v", err)
	}
	if _, err := provider.CreateDownload(ctx, scope, domain.CreateDownloadRequest{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid CreateDownload = %v", err)
	}
	if _, err := provider.CreateDownload(ctx, scope, domain.CreateDownloadRequest{Path: path, Version: entry.Version, Disposition: "open"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid download disposition = %v", err)
	}
	if _, err := provider.CreateDownload(ctx, scope, domain.CreateDownloadRequest{Path: path, Version: "stale"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale download = %v", err)
	}
	download, err := provider.CreateDownload(ctx, scope, domain.CreateDownloadRequest{Path: path, Version: entry.Version})
	if err != nil {
		t.Fatal(err)
	}
	for name, request := range map[string]*http.Request{
		"wrong method": httptest.NewRequest(http.MethodPost, download.URL, nil),
		"bad range":    httptest.NewRequest(http.MethodGet, download.URL, nil),
	} {
		if name == "bad range" {
			request.Header.Set("Range", "bytes=99-100")
		}
		response := httptest.NewRecorder()
		provider.ServeHTTP(response, request)
		if response.Code < 400 {
			t.Fatalf("download %s = %d", name, response.Code)
		}
	}
	clock.Advance(2 * time.Minute)
	expired := httptest.NewRecorder()
	provider.ServeHTTP(expired, httptest.NewRequest(http.MethodGet, download.URL, nil))
	if expired.Code != http.StatusGone {
		t.Fatalf("expired download = %d", expired.Code)
	}
}

func TestProviderPureTransferHelpers(t *testing.T) {
	for _, test := range []struct {
		value string
		size  int64
		ok    bool
	}{
		{"", 0, true}, {"", 5, true}, {"bytes=-2", 5, true}, {"bytes=1-", 5, true}, {"bytes=1-99", 5, true},
		{"bytes=-0", 5, false}, {"items=0-1", 5, false}, {"bytes=0-1,2-3", 5, false}, {"bytes=x-1", 5, false}, {"bytes=2-1", 5, false}, {"bytes=0-x", 5, false}, {"bytes=0-1", 0, false}, {"", -1, false},
	} {
		_, _, _, err := parseRange(test.value, test.size)
		if (err == nil) != test.ok {
			t.Fatalf("parseRange(%q,%d) = %v", test.value, test.size, err)
		}
	}
	if got := safeDisposition(domain.Disposition("invalid\r\n"), "file.txt"); strings.ContainsAny(got, "\r\n") {
		t.Fatalf("unsafe fallback disposition = %q", got)
	}
	if written := writeZeros(io.Discard, 33<<10); written != 33<<10 {
		t.Fatalf("writeZeros = %d", written)
	}
	for kind, status := range map[domain.ErrorKind]int{
		domain.ErrorRateLimited: http.StatusTooManyRequests, domain.ErrorUnavailable: http.StatusServiceUnavailable,
		domain.ErrorConflict: http.StatusConflict, domain.ErrorPreconditionFailed: http.StatusConflict,
		domain.ErrorNotFound: http.StatusNotFound,
	} {
		response := httptest.NewRecorder()
		writeDataPlaneError(response, domain.NewError(kind, "safe"))
		if response.Code != status {
			t.Fatalf("writeDataPlaneError(%s) = %d", kind, response.Code)
		}
	}
}

func TestProviderFaultConflictAndCompletionMatrices(t *testing.T) {
	provider, scope, clock := boundaryProvider(t)
	ctx := context.Background()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	root := domain.MustParseUserPath("/")
	path := domain.MustParseUserPath("/file.txt")
	for name, call := range map[string]func() error{
		"list": func() error {
			_, err := provider.List(canceled, scope, domain.ListRequest{Directory: root})
			return err
		},
		"stat": func() error { _, err := provider.Stat(canceled, scope, path); return err },
		"directory": func() error {
			_, err := provider.CreateDirectory(canceled, scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/directory")})
			return err
		},
		"copy": func() error {
			_, err := provider.Copy(canceled, scope, scope, domain.CopyRequest{Source: path, Destination: domain.MustParseUserPath("/copy")})
			return err
		},
		"delete": func() error { _, err := provider.Delete(canceled, scope, domain.DeleteRequest{Path: path}); return err },
		"create upload": func() error {
			_, err := provider.CreateUpload(canceled, scope, domain.CreateUploadRequest{Path: path, Size: 1, MediaType: "text/plain"})
			return err
		},
		"upload status": func() error { _, err := provider.UploadStatus(canceled, scope, "id"); return err },
		"complete upload": func() error {
			_, err := provider.CompleteUpload(canceled, scope, domain.CompleteUploadRequest{UploadID: "id", Path: path, Size: 1, MediaType: "text/plain"})
			return err
		},
		"abort upload": func() error { return provider.AbortUpload(canceled, scope, "id") },
		"create download": func() error {
			_, err := provider.CreateDownload(canceled, scope, domain.CreateDownloadRequest{Path: path, Version: "v"})
			return err
		},
		"upload offset":   func() error { _, err := provider.UploadOffset(canceled, scope, "id"); return err },
		"simulate offset": func() error { return provider.SimulateUploadOffset(canceled, scope, "id", 0) },
	} {
		t.Run("canceled "+name, func(t *testing.T) {
			if err := call(); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("operation = %v", err)
			}
		})
	}

	for operation, call := range map[string]func() error{
		OperationList: func() error { _, err := provider.List(ctx, scope, domain.ListRequest{Directory: root}); return err },
		OperationCreateDirectory: func() error {
			_, err := provider.CreateDirectory(ctx, scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/fault-directory")})
			return err
		},
		OperationCreateUpload: func() error {
			_, err := provider.CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/fault-upload"), Size: 1, MediaType: "text/plain"})
			return err
		},
		OperationCompleteUpload: func() error {
			_, err := provider.CompleteUpload(ctx, scope, domain.CompleteUploadRequest{UploadID: "missing", Path: path, Size: 1, MediaType: "text/plain"})
			return err
		},
		OperationAbortUpload: func() error { return provider.AbortUpload(ctx, scope, "missing") },
		OperationCreateDownload: func() error {
			_, err := provider.CreateDownload(ctx, scope, domain.CreateDownloadRequest{Path: path, Version: "v"})
			return err
		},
		OperationDelete: func() error { _, err := provider.Delete(ctx, scope, domain.DeleteRequest{Path: path}); return err },
	} {
		t.Run("fault "+operation, func(t *testing.T) {
			provider.InjectFault(operation, FaultUnavailable)
			if err := call(); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("operation = %v", err)
			}
		})
	}

	entry := provider.newEntryLocked(path, domain.EntryFile, 5, "text/plain")
	provider.scopeObjectsLocked(scope)[path.String()] = object{entry: entry, data: []byte("hello"), materialized: true}
	if _, err := provider.CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: path, Size: 5, MediaType: "text/plain"}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("conflicting upload = %v", err)
	}
	if _, err := provider.CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: path, Size: 5, MediaType: "text/plain", Conflict: domain.ConflictReplace, ExpectedVersion: "stale"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale replacement upload = %v", err)
	}
	renamed, err := provider.CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: path, Size: 5, MediaType: "text/plain", Conflict: domain.ConflictRename, Resumable: true})
	if err != nil {
		t.Fatal(err)
	}
	status, err := provider.UploadStatus(ctx, scope, renamed.UploadID)
	if err != nil || status.State != domain.UploadStateActive || status.Path != path || status.ConfirmedOffset != 0 {
		t.Fatalf("renamed upload status = %+v, %v", status, err)
	}
	if _, err := provider.UploadOffset(ctx, scope, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing upload offset = %v", err)
	}
	if err := provider.SimulateUploadOffset(ctx, scope, "missing", 0); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing simulated upload = %v", err)
	}

	incomplete, err := provider.CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/incomplete.txt"), Size: 5, MediaType: "text/plain", Resumable: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CompleteUpload(ctx, scope, domain.CompleteUploadRequest{UploadID: incomplete.UploadID, Path: domain.MustParseUserPath("/incomplete.txt"), Size: 5, MediaType: "text/plain"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("incomplete completion = %v", err)
	}
	provider.InjectFault(OperationCompleteUpload, FaultChecksumMismatch)
	provider.uploads[incomplete.UploadID].offset = 5
	if _, err := provider.CompleteUpload(ctx, scope, domain.CompleteUploadRequest{UploadID: incomplete.UploadID, Path: domain.MustParseUserPath("/incomplete.txt"), Size: 5, MediaType: "text/plain"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("checksum fault completion = %v", err)
	}
	provider.uploads[incomplete.UploadID].expiresAt = clock.Now()
	if _, err := provider.CompleteUpload(ctx, scope, domain.CompleteUploadRequest{UploadID: incomplete.UploadID, Path: domain.MustParseUserPath("/incomplete.txt"), Size: 5, MediaType: "text/plain"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("expired completion = %v", err)
	}

	provider.InjectFault(OperationCopy, FaultPartialOperation)
	failed, err := provider.Copy(ctx, scope, scope, domain.CopyRequest{Source: path, Destination: domain.MustParseUserPath("/partial-copy"), IdempotencyKey: "partial-copy-key"})
	if err != nil || failed.State != domain.OperationFailed {
		t.Fatalf("partial Copy = %+v, %v", failed, err)
	}
	if _, err := provider.Copy(ctx, scope, scope, domain.CopyRequest{Source: path, Destination: domain.MustParseUserPath("/partial-copy"), IdempotencyKey: "partial-copy-key"}); err != nil {
		t.Fatalf("partial Copy replay = %v", err)
	}
	if err := provider.AbortUpload(ctx, scope, renamed.UploadID); err != nil {
		t.Fatal(err)
	}
	aborted, err := provider.UploadStatus(ctx, scope, renamed.UploadID)
	if err != nil || aborted.State != domain.UploadStateAborted {
		t.Fatalf("aborted UploadStatus = %+v, %v", aborted, err)
	}
}

func TestProviderDataPlaneFaultAndMutationBoundaries(t *testing.T) {
	provider, scope, clock := boundaryProvider(t)
	ctx := context.Background()
	path := domain.MustParseUserPath("/fault.txt")
	capability, err := provider.CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: path, Size: 4, MediaType: "text/plain", Resumable: true})
	if err != nil {
		t.Fatal(err)
	}
	provider.InjectFault(OperationUploadData, FaultRateLimited)
	response := httptest.NewRecorder()
	provider.ServeHTTP(response, httptest.NewRequest(http.MethodPatch, capability.URL, strings.NewReader("data")))
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("upload data fault = %d", response.Code)
	}
	provider.uploads[capability.UploadID].expiresAt = clock.Now()
	expired := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPatch, capability.URL, strings.NewReader("data"))
	request.Header.Set("Content-Type", "text/plain")
	request.Header.Set("Upload-Offset", "0")
	provider.ServeHTTP(expired, request)
	if expired.Code != http.StatusGone {
		t.Fatalf("expired upload = %d", expired.Code)
	}

	entry := provider.newEntryLocked(domain.MustParseUserPath("/download.txt"), domain.EntryFile, 4, "text/plain")
	provider.scopeObjectsLocked(scope)[entry.Path.String()] = object{entry: entry, data: []byte("data"), materialized: true}
	download, err := provider.CreateDownload(ctx, scope, domain.CreateDownloadRequest{Path: entry.Path, Version: entry.Version})
	if err != nil {
		t.Fatal(err)
	}
	provider.InjectFault(OperationDownloadData, FaultConflict)
	faulted := httptest.NewRecorder()
	provider.ServeHTTP(faulted, httptest.NewRequest(http.MethodGet, download.URL, nil))
	if faulted.Code != http.StatusConflict {
		t.Fatalf("download data fault = %d", faulted.Code)
	}
	delete(provider.scopeObjectsLocked(scope), entry.Path.String())
	missing := httptest.NewRecorder()
	provider.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, download.URL, nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("mutated download = %d", missing.Code)
	}

	for declared, data := range map[string][]byte{
		"image/png":       []byte("not-png"),
		"text/plain":      {0xff},
		"application/pdf": []byte("%PDF-1.7\n"),
	} {
		got := trustedMediaType(declared, data, true)
		if declared == "application/pdf" && got != declared {
			t.Fatalf("trusted PDF = %q", got)
		}
		if declared != "application/pdf" && got != "application/octet-stream" {
			t.Fatalf("spoofed %s = %q", declared, got)
		}
	}
	if got := trustedMediaType("text/plain", []byte("text"), false); got != "application/octet-stream" {
		t.Fatalf("nonmaterialized text = %q", got)
	}
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (failingBody) Close() error             { return nil }

type shortWriter struct{}

func (shortWriter) Write(value []byte) (int, error) { return len(value) / 2, nil }

func TestProviderRemainingTreeAndTransferBranches(t *testing.T) {
	provider, scope, clock := boundaryProvider(t)
	ctx := context.Background()
	directory, err := provider.CreateDirectory(ctx, scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/directory")})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c"} {
		if _, err := provider.CreateDirectory(ctx, scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/" + name)}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := provider.List(ctx, scope, domain.ListRequest{Directory: domain.MustParseUserPath("/"), PageSize: 1})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	for page.NextCursor != "" {
		page, err = provider.List(ctx, scope, domain.ListRequest{Directory: domain.MustParseUserPath("/"), PageSize: 1, Cursor: page.NextCursor})
		if err != nil {
			t.Fatal(err)
		}
	}
	if listed, err := provider.List(ctx, scope, domain.ListRequest{Directory: directory.Path}); err != nil || len(listed.Entries) != 0 {
		t.Fatalf("nested list = %+v, %v", listed, err)
	}
	if _, err := provider.CreateDirectory(ctx, scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/bad-conflict"), Conflict: "unknown"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid directory conflict = %v", err)
	}
	longPath := domain.MustParseUserPath("/" + strings.Repeat("é", 125) + ".txt")
	longEntry := provider.newEntryLocked(longPath, domain.EntryFile, 1, "text/plain")
	provider.scopeObjectsLocked(scope)[longPath.String()] = object{entry: longEntry, data: []byte("x"), materialized: true}
	provider.mu.Lock()
	renamed, err := provider.availableRenamedPathLocked(scope, longPath)
	provider.mu.Unlock()
	if err != nil || len(renamed.Name()) > 255 || renamed == longPath {
		t.Fatalf("long renamed path = %q, %v", renamed.String(), err)
	}

	source := domain.MustParseUserPath("/source")
	sourceEntry, _ := provider.CreateDirectory(ctx, scope, domain.CreateDirectoryRequest{Path: source})
	destination := domain.MustParseUserPath("/destination")
	destinationEntry, _ := provider.CreateDirectory(ctx, scope, domain.CreateDirectoryRequest{Path: destination})
	if _, err := provider.Copy(ctx, scope, scope, domain.CopyRequest{Source: source, Destination: destination}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("destination conflict Copy = %v", err)
	}
	if _, err := provider.Copy(ctx, scope, scope, domain.CopyRequest{Source: source, Destination: destination, Conflict: domain.ConflictReplace, ExpectedTarget: "stale"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale destination Copy = %v", err)
	}
	if _, err := provider.Copy(ctx, scope, scope, domain.CopyRequest{Source: source, Destination: domain.MustParseUserPath("/missing/target")}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing parent Copy = %v", err)
	}
	replaced, err := provider.Copy(ctx, scope, scope, domain.CopyRequest{Source: source, Destination: destination, Conflict: domain.ConflictReplace, ExpectedSource: sourceEntry.Version, ExpectedTarget: destinationEntry.Version})
	if err != nil || replaced.State != domain.OperationSucceeded {
		t.Fatalf("replacement Copy = %+v, %v", replaced, err)
	}
	if _, err := provider.Delete(ctx, scope, domain.DeleteRequest{Path: destination, IdempotencyKey: "delete-replay-key"}); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Delete(ctx, scope, domain.DeleteRequest{Path: destination, IdempotencyKey: "delete-replay-key", ExpectedVersion: "changed"}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("changed Delete replay = %v", err)
	}
	if err := validateIdempotencyKey("line\nbreak"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("control idempotency key = %v", err)
	}

	if _, err := provider.CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/missing/file"), Size: 1, MediaType: "text/plain"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing upload parent = %v", err)
	}
	provider.InjectFault(OperationCreateUpload, FaultExpired)
	expiredUpload, err := provider.CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/expired.txt"), Size: 1, MediaType: "text/plain"})
	if err != nil || expiredUpload.ExpiresAt.After(clock.Now()) {
		t.Fatalf("expired upload capability = %+v, %v", expiredUpload, err)
	}

	interrupt, err := provider.CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/read-error.txt"), Size: 4, MediaType: "text/plain", Resumable: true})
	if err != nil {
		t.Fatal(err)
	}
	readFailure := httptest.NewRequest(http.MethodPatch, interrupt.URL, nil)
	readFailure.Body = failingBody{}
	readFailure.Header.Set("Content-Type", "text/plain")
	readFailure.Header.Set("Upload-Offset", "0")
	readFailureResponse := httptest.NewRecorder()
	provider.ServeHTTP(readFailureResponse, readFailure)
	if readFailureResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("read failure upload = %d", readFailureResponse.Code)
	}

	single, err := provider.CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/single.txt"), Size: 4, MediaType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	short := httptest.NewRequest(http.MethodPut, single.URL, strings.NewReader("x"))
	short.Header.Set("Content-Type", "text/plain")
	shortResponse := httptest.NewRecorder()
	provider.ServeHTTP(shortResponse, short)
	if shortResponse.Code != http.StatusPreconditionFailed {
		t.Fatalf("short single upload = %d", shortResponse.Code)
	}

	chunked := New(Options{Clock: clock, IDs: provider.ids, ChunkRules: domain.ChunkRules{MinimumSize: 4, MaximumSize: 8, Multiple: 4}})
	_ = chunked.SetDataPlaneBaseURL("http://127.0.0.1:43212")
	chunkCapability, err := chunked.CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/chunked.bin"), Size: 10, MediaType: "application/octet-stream", Resumable: true})
	if err != nil {
		t.Fatal(err)
	}
	badChunk := httptest.NewRequest(http.MethodPatch, chunkCapability.URL, strings.NewReader("xx"))
	badChunk.Header.Set("Content-Type", "application/octet-stream")
	badChunk.Header.Set("Upload-Offset", "0")
	badChunkResponse := httptest.NewRecorder()
	chunked.ServeHTTP(badChunkResponse, badChunk)
	if badChunkResponse.Code != http.StatusBadRequest {
		t.Fatalf("bad chunk = %d", badChunkResponse.Code)
	}

	withoutURL := New(Options{})
	filePath := domain.MustParseUserPath("/download.txt")
	fileEntry := withoutURL.newEntryLocked(filePath, domain.EntryFile, 1, "text/plain")
	withoutURL.scopeObjectsLocked(scope)[filePath.String()] = object{entry: fileEntry, data: []byte("x"), materialized: true}
	if _, err := withoutURL.CreateDownload(ctx, scope, domain.CreateDownloadRequest{Path: filePath, Version: fileEntry.Version}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("download without URL = %v", err)
	}
	provider.InjectFault(OperationCreateDownload, FaultExpired)
	downloadEntry := provider.newEntryLocked(filePath, domain.EntryFile, 1<<20, "application/octet-stream")
	provider.scopeObjectsLocked(scope)[filePath.String()] = object{entry: downloadEntry, materialized: false}
	download, err := provider.CreateDownload(ctx, scope, domain.CreateDownloadRequest{Path: filePath, Version: downloadEntry.Version})
	if err != nil || download.ExpiresAt.After(clock.Now()) {
		t.Fatalf("expired download capability = %+v, %v", download, err)
	}
	if written := writeZeros(shortWriter{}, 100); written != 50 {
		t.Fatalf("short writeZeros = %d", written)
	}
}

func TestProviderTransferEntropyAndDispatchFailuresFailClosed(t *testing.T) {
	_, scope, clock := boundaryProvider(t)
	ctx := context.Background()
	path := domain.MustParseUserPath("/entropy.txt")
	request := domain.CreateUploadRequest{Path: path, Size: 1, MediaType: "text/plain"}

	for name, entropyBytes := range map[string]int{"upload ID": 0, "upload token": 16} {
		t.Run(name, func(t *testing.T) {
			provider := New(Options{Clock: clock, IDs: domain.NewIDGenerator(bytes.NewReader(make([]byte, entropyBytes)))})
			if err := provider.SetDataPlaneBaseURL("http://127.0.0.1:43213"); err != nil {
				t.Fatal(err)
			}
			if _, err := provider.CreateUpload(ctx, scope, request); !errors.Is(err, domain.ErrInternal) {
				t.Fatalf("CreateUpload with unavailable %s entropy = %v", name, err)
			}
		})
	}

	provider, scope, _ := boundaryProvider(t)
	if _, err := provider.UploadStatus(ctx, scope, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty UploadStatus = %v", err)
	}
	if _, err := provider.CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: path, Size: provider.maxMaterializedBytes + 1, MediaType: "application/octet-stream"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized single CreateUpload = %v", err)
	}

	capability, err := provider.CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: path, Size: 4, MediaType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	upload := httptest.NewRequest(http.MethodPut, capability.URL, strings.NewReader("data"))
	upload.Header.Set("Content-Type", "text/plain")
	uploadResponse := httptest.NewRecorder()
	provider.ServeHTTP(uploadResponse, upload)
	if uploadResponse.Code != http.StatusNoContent {
		t.Fatalf("upload response = %d", uploadResponse.Code)
	}
	provider.ids = domain.NewIDGenerator(bytes.NewReader(nil))
	if _, err := provider.CompleteUpload(ctx, scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: 4, MediaType: "text/plain"}); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("CompleteUpload with unavailable entry entropy = %v", err)
	}

	downloadPath := domain.MustParseUserPath("/download-entropy.txt")
	downloadEntry := provider.newEntryLocked(downloadPath, domain.EntryFile, 1, "text/plain")
	provider.scopeObjectsLocked(scope)[downloadPath.String()] = object{entry: downloadEntry, data: []byte("x"), materialized: true}
	if _, err := provider.CreateDownload(ctx, scope, domain.CreateDownloadRequest{Path: downloadPath, Version: downloadEntry.Version}); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("CreateDownload with unavailable token entropy = %v", err)
	}

	unknown := httptest.NewRecorder()
	provider.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:43210/cap/unknown/token", nil))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown capability route = %d", unknown.Code)
	}
	const missingSessionToken = "missing-session"
	provider.uploadTokens[tokenHash(missingSessionToken)] = domain.UploadID("missing")
	missingSession := httptest.NewRecorder()
	provider.ServeHTTP(missingSession, httptest.NewRequest(http.MethodPut, "http://127.0.0.1:43210/cap/upload/"+missingSessionToken, nil))
	if missingSession.Code != http.StatusNotFound {
		t.Fatalf("missing upload session = %d", missingSession.Code)
	}
	if _, _, _, err := parseRange("bytes=1-2-3", 4); err == nil {
		t.Fatal("multi-dash byte range succeeded")
	}
}

func TestRecursiveAggregateOverflowFailsClosedAndRollsBackUpload(t *testing.T) {
	provider, scope, _ := boundaryProvider(t)
	ctx := context.Background()
	hugePath := domain.MustParseUserPath("/huge.bin")
	huge := provider.newEntryLocked(hugePath, domain.EntryFile, math.MaxInt64, "application/octet-stream")
	provider.scopeObjectsLocked(scope)[hugePath.String()] = object{entry: huge}
	if root, err := provider.Stat(ctx, scope, domain.MustParseUserPath("/")); err != nil || root.Size != math.MaxInt64 {
		t.Fatalf("Stat(root) = %+v, %v", root, err)
	}

	path := domain.MustParseUserPath("/overflow.bin")
	capability, err := provider.CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: path, Size: 1, MediaType: "application/octet-stream", Resumable: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.SimulateUploadOffset(ctx, scope, capability.UploadID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CompleteUpload(ctx, scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: 1, MediaType: "application/octet-stream"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("CompleteUpload() error = %v", err)
	}
	if _, err := provider.Stat(ctx, scope, path); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("rolled-back path Stat() error = %v", err)
	}
	if root, err := provider.Stat(ctx, scope, domain.MustParseUserPath("/")); err != nil || root.Size != math.MaxInt64 {
		t.Fatalf("rolled-back root Stat() = %+v, %v", root, err)
	}

	secondPath := domain.MustParseUserPath("/corrupt.bin")
	second := provider.newEntryLocked(secondPath, domain.EntryFile, 1, "application/octet-stream")
	provider.scopeObjectsLocked(scope)[secondPath.String()] = object{entry: second}
	if _, err := provider.Stat(ctx, scope, domain.MustParseUserPath("/")); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("overflowing root Stat() error = %v", err)
	}
	directoryPath := domain.MustParseUserPath("/must-rollback")
	if _, err := provider.CreateDirectory(ctx, scope, domain.CreateDirectoryRequest{Path: directoryPath}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("CreateDirectory() error = %v", err)
	}
	if _, found := provider.scopeObjectsLocked(scope)[directoryPath.String()]; found {
		t.Fatal("overflowing CreateDirectory() was not rolled back")
	}

	copyProvider, live, _ := boundaryProvider(t)
	trash, err := domain.NewScope(live.UserID(), domain.AreaTrash)
	if err != nil {
		t.Fatal(err)
	}
	sourcePath := domain.MustParseUserPath("/source.bin")
	source := copyProvider.newEntryLocked(sourcePath, domain.EntryFile, 1, "application/octet-stream")
	copyProvider.scopeObjectsLocked(live)[sourcePath.String()] = object{entry: source}
	trashHugePath := domain.MustParseUserPath("/huge.bin")
	trashHuge := copyProvider.newEntryLocked(trashHugePath, domain.EntryFile, math.MaxInt64, "application/octet-stream")
	copyProvider.scopeObjectsLocked(trash)[trashHugePath.String()] = object{entry: trashHuge}
	copyPath := domain.MustParseUserPath("/copy.bin")
	if _, err := copyProvider.Copy(ctx, live, trash, domain.CopyRequest{Source: sourcePath, Destination: copyPath}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("Copy() error = %v", err)
	}
	if _, found := copyProvider.scopeObjectsLocked(trash)[copyPath.String()]; found {
		t.Fatal("overflowing Copy() destination was not rolled back")
	}
	if _, found := copyProvider.scopeObjectsLocked(live)[sourcePath.String()]; !found {
		t.Fatal("overflowing Copy() removed its source")
	}
}
