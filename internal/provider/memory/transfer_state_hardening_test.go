package memory

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func TestUploadMetadataStateTransitionsFailClosedWithoutBodyStreaming(t *testing.T) {
	ctx := context.Background()
	provider, scope, clock := boundaryProvider(t)

	expiring, err := provider.CreateUpload(ctx, scope, domain.CreateUploadRequest{
		Path: domain.MustParseUserPath("/expiring.bin"), Size: 0, MediaType: "application/octet-stream",
	})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(6 * time.Minute)
	if status, err := provider.UploadStatus(ctx, scope, expiring.UploadID); err != nil || status.State != domain.UploadStateExpired {
		t.Fatalf("expired upload status = %+v, %v", status, err)
	}

	provider, scope, _ = boundaryProvider(t)
	appearedPath := domain.MustParseUserPath("/appeared.bin")
	appeared, err := provider.CreateUpload(ctx, scope, domain.CreateUploadRequest{
		Path: appearedPath, Size: 0, MediaType: "application/octet-stream",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CreateDirectory(ctx, scope, domain.CreateDirectoryRequest{Path: appearedPath}); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CompleteUpload(ctx, scope, domain.CompleteUploadRequest{UploadID: appeared.UploadID, Path: appearedPath, Size: 0, MediaType: "application/octet-stream"}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("appeared destination error = %v", err)
	}

	replacePath := domain.MustParseUserPath("/replace.bin")
	original, err := provider.CreateDirectory(ctx, scope, domain.CreateDirectoryRequest{Path: replacePath})
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := provider.CreateUpload(ctx, scope, domain.CreateUploadRequest{
		Path: replacePath, Size: 0, MediaType: "application/octet-stream", Conflict: domain.ConflictReplace, ExpectedVersion: original.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CreateDirectory(ctx, scope, domain.CreateDirectoryRequest{Path: replacePath, Conflict: domain.ConflictReplace, ExpectedVersion: original.Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CompleteUpload(ctx, scope, domain.CompleteUploadRequest{UploadID: replacement.UploadID, Path: replacePath, Size: 0, MediaType: "application/octet-stream"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("changed destination error = %v", err)
	}
	if _, err := provider.CompleteUpload(ctx, scope, domain.CompleteUploadRequest{UploadID: replacement.UploadID, Path: replacePath, Size: 0, MediaType: "invalid media type"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid completion media type error = %v", err)
	}
}

func TestUnknownDownloadCapabilityIsDeniedWithoutProviderLookup(t *testing.T) {
	provider, _, _ := boundaryProvider(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/data/download/missing", nil)
	provider.serveDownload(recorder, request, "missing")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown download status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
}
