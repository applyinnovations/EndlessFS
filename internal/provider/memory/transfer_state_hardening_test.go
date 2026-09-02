package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/integrity"
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

func TestCreateUploadBatchRollsBackEveryNewIntentOnLaterFailure(t *testing.T) {
	ctx := context.Background()
	provider, scope, _ := boundaryProvider(t)
	if _, err := provider.CreateUploadBatch(ctx, scope, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty upload batch error = %v; want invalid", err)
	}
	prior, err := provider.CreateUpload(ctx, scope, domain.CreateUploadRequest{
		Path: domain.MustParseUserPath("/prior.bin"), Size: 1, MediaType: "application/octet-stream", IdempotencyKey: "prior-upload-key-0001",
	})
	if err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	beforeUploads, beforeTokens, beforeIdempotency := len(provider.uploads), len(provider.uploadTokens), len(provider.uploadIdempotency)
	provider.mu.Unlock()
	_, err = provider.CreateUploadBatch(ctx, scope, []domain.CreateUploadRequest{
		{Path: domain.MustParseUserPath("/batch-first.bin"), Size: 1, MediaType: "application/octet-stream", IdempotencyKey: "batch-first-key-0001"},
		{Path: domain.MustParseUserPath("/"), Size: 1, MediaType: "application/octet-stream", IdempotencyKey: "batch-invalid-key-01"},
	})
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("partially invalid upload batch error = %v; want invalid", err)
	}
	provider.mu.Lock()
	afterUploads, afterTokens, afterIdempotency := len(provider.uploads), len(provider.uploadTokens), len(provider.uploadIdempotency)
	provider.mu.Unlock()
	if afterUploads != beforeUploads || afterTokens != beforeTokens || afterIdempotency != beforeIdempotency {
		t.Fatalf("failed batch leaked state: uploads %d->%d tokens %d->%d idempotency %d->%d", beforeUploads, afterUploads, beforeTokens, afterTokens, beforeIdempotency, afterIdempotency)
	}
	if status, err := provider.UploadStatus(ctx, scope, prior.UploadID); err != nil || status.State != domain.UploadStateActive {
		t.Fatalf("preexisting upload after rollback = %+v, %v", status, err)
	}
}

func TestUploadCompletionBatchIsAtomicReplayableAndStrictlyBound(t *testing.T) {
	ctx := context.Background()
	provider, scope, _ := boundaryProvider(t)
	requests := []domain.CreateUploadRequest{
		{Path: domain.MustParseUserPath("/complete-a.bin"), Size: 1, MediaType: "application/octet-stream", IdempotencyKey: "complete-batch-item-a"},
		{Path: domain.MustParseUserPath("/complete-b.bin"), Size: 1, MediaType: "application/octet-stream", IdempotencyKey: "complete-batch-item-b"},
	}
	capabilities, err := provider.CreateUploadBatch(ctx, scope, requests)
	if err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	for index, capability := range capabilities {
		session := provider.uploads[capability.UploadID]
		session.data = []byte{byte('a' + index)}
		session.offset = session.size
		session.materialized = true
	}
	provider.mu.Unlock()
	request := domain.CompleteUploadBatchRequest{
		Items: []domain.CompleteUploadBatchItem{
			{UploadID: capabilities[0].UploadID, CRC32C: integrity.CRC32C([]byte("a"))},
			{UploadID: capabilities[1].UploadID, CRC32C: integrity.CRC32C([]byte("b"))},
		},
		IdempotencyKey: "complete-batch-operation",
	}
	result, err := provider.CompleteUploadBatch(ctx, scope, request)
	if err != nil || len(result.Entries) != 2 {
		t.Fatalf("completion = %+v, %v", result, err)
	}
	replayed, err := provider.CompleteUploadBatch(ctx, scope, request)
	if err != nil || len(replayed.Entries) != 2 || replayed.Entries[0].Version != result.Entries[0].Version {
		t.Fatalf("completion replay = %+v, %v", replayed, err)
	}
	changed := request
	changed.Items = append([]domain.CompleteUploadBatchItem(nil), request.Items...)
	changed.Items[0].CRC32C = integrity.CRC32C([]byte("different"))
	if _, err := provider.CompleteUploadBatch(ctx, scope, changed); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("changed replay = %v", err)
	}

	for name, candidate := range map[string]domain.CompleteUploadBatchRequest{
		"empty":        {},
		"idempotency":  {Items: request.Items, IdempotencyKey: "short"},
		"empty-upload": {Items: []domain.CompleteUploadBatchItem{{CRC32C: integrity.CRC32C(nil)}}, IdempotencyKey: "complete-empty-upload"},
		"checksum":     {Items: []domain.CompleteUploadBatchItem{{UploadID: "upload", CRC32C: "invalid"}}, IdempotencyKey: "complete-invalid-crc"},
		"duplicate":    {Items: []domain.CompleteUploadBatchItem{request.Items[0], request.Items[0]}, IdempotencyKey: "complete-duplicate-id"},
		"missing":      {Items: []domain.CompleteUploadBatchItem{{UploadID: "missing", CRC32C: integrity.CRC32C(nil)}}, IdempotencyKey: "complete-missing-item"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := provider.CompleteUploadBatch(ctx, scope, candidate); err == nil {
				t.Fatal("invalid completion batch was accepted")
			}
		})
	}
	if _, err := provider.CompleteUploadBatch(ctx, domain.Scope{}, request); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("invalid scope = %v", err)
	}

	mismatch, err := provider.CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/mismatch.bin"), Size: 1, MediaType: "application/octet-stream"})
	if err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	provider.uploads[mismatch.UploadID].data = []byte("x")
	provider.uploads[mismatch.UploadID].offset = 1
	provider.uploads[mismatch.UploadID].materialized = true
	provider.mu.Unlock()
	if _, err := provider.CompleteUploadBatch(ctx, scope, domain.CompleteUploadBatchRequest{
		Items: []domain.CompleteUploadBatchItem{{UploadID: mismatch.UploadID, CRC32C: integrity.CRC32C([]byte("y"))}}, IdempotencyKey: "complete-checksum-mismatch",
	}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("checksum mismatch = %v", err)
	}
}

func TestUploadAbortBatchIsAtomicReplayableAndStrictlyBound(t *testing.T) {
	ctx := context.Background()
	provider, scope, _ := boundaryProvider(t)
	requests := []domain.CreateUploadRequest{
		{Path: domain.MustParseUserPath("/abort-a.bin"), Size: 1, MediaType: "application/octet-stream", IdempotencyKey: "abort-batch-item-a-01"},
		{Path: domain.MustParseUserPath("/abort-b.bin"), Size: 1, MediaType: "application/octet-stream", IdempotencyKey: "abort-batch-item-b-01"},
	}
	capabilities, err := provider.CreateUploadBatch(ctx, scope, requests)
	if err != nil {
		t.Fatal(err)
	}
	request := domain.AbortUploadBatchRequest{
		UploadIDs: []domain.UploadID{capabilities[0].UploadID, capabilities[1].UploadID},
		BatchID:   capabilities[0].BatchID, IdempotencyKey: "abort-batch-operation-01",
	}
	if err := provider.AbortUploadBatch(ctx, scope, request); err != nil {
		t.Fatal(err)
	}
	if err := provider.AbortUploadBatch(ctx, scope, request); err != nil {
		t.Fatalf("abort replay = %v", err)
	}
	changed := request
	changed.UploadIDs = []domain.UploadID{capabilities[0].UploadID}
	if err := provider.AbortUploadBatch(ctx, scope, changed); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("changed replay = %v", err)
	}

	for name, candidate := range map[string]domain.AbortUploadBatchRequest{
		"empty":        {},
		"idempotency":  {UploadIDs: request.UploadIDs, IdempotencyKey: "short"},
		"batch":        {UploadIDs: request.UploadIDs, BatchID: "invalid", IdempotencyKey: "abort-invalid-batch-01"},
		"empty-upload": {UploadIDs: []domain.UploadID{""}, IdempotencyKey: "abort-empty-upload-001"},
		"duplicate":    {UploadIDs: []domain.UploadID{"duplicate", "duplicate"}, IdempotencyKey: "abort-duplicate-item-01"},
		"missing":      {UploadIDs: []domain.UploadID{"missing"}, IdempotencyKey: "abort-missing-item-0001"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := provider.AbortUploadBatch(ctx, scope, candidate); err == nil {
				t.Fatal("invalid abort batch was accepted")
			}
		})
	}
	if err := provider.AbortUploadBatch(ctx, domain.Scope{}, request); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("invalid scope = %v", err)
	}

	standalone, err := provider.CreateUpload(ctx, scope, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/standalone.bin"), Size: 1, MediaType: "application/octet-stream"})
	if err != nil {
		t.Fatal(err)
	}
	wrongBatch := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x7a}, sha256.Size))
	if err := provider.AbortUploadBatch(ctx, scope, domain.AbortUploadBatchRequest{
		UploadIDs: []domain.UploadID{standalone.UploadID}, BatchID: wrongBatch, IdempotencyKey: "abort-batch-mismatch-01",
	}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("batch mismatch = %v", err)
	}
}

func TestUploadMutationBatchesRestoreAllAuthorityAfterProviderFailure(t *testing.T) {
	ctx := context.Background()
	newBatch := func(t *testing.T, prefix string) (*Provider, domain.Scope, []domain.UploadCapability) {
		t.Helper()
		provider, scope, _ := boundaryProvider(t)
		capabilities, err := provider.CreateUploadBatch(ctx, scope, []domain.CreateUploadRequest{
			{Path: domain.MustParseUserPath("/" + prefix + "-a.bin"), Size: 1, MediaType: "application/octet-stream", IdempotencyKey: prefix + "-item-a-0001"},
			{Path: domain.MustParseUserPath("/" + prefix + "-b.bin"), Size: 1, MediaType: "application/octet-stream", IdempotencyKey: prefix + "-item-b-0001"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return provider, scope, capabilities
	}

	t.Run("completion", func(t *testing.T) {
		provider, scope, capabilities := newBatch(t, "rollback-complete")
		items := make([]domain.CompleteUploadBatchItem, len(capabilities))
		provider.mu.Lock()
		for index, capability := range capabilities {
			body := []byte{byte('a' + index)}
			session := provider.uploads[capability.UploadID]
			session.data, session.offset, session.materialized = body, session.size, true
			items[index] = domain.CompleteUploadBatchItem{UploadID: capability.UploadID, CRC32C: integrity.CRC32C(body)}
		}
		beforeTokens := len(provider.uploadTokens)
		provider.mu.Unlock()
		provider.InjectFault(OperationCompleteUpload, FaultChecksumMismatch)
		if _, err := provider.CompleteUploadBatch(ctx, scope, domain.CompleteUploadBatchRequest{Items: items, IdempotencyKey: "rollback-completion-operation"}); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("completion failure = %v", err)
		}
		provider.mu.Lock()
		defer provider.mu.Unlock()
		if len(provider.uploadTokens) != beforeTokens || len(provider.uploadCompletions) != 0 {
			t.Fatalf("completion rollback tokens=%d/%d outcomes=%d", len(provider.uploadTokens), beforeTokens, len(provider.uploadCompletions))
		}
		for _, capability := range capabilities {
			if provider.uploads[capability.UploadID].state != domain.UploadStateActive {
				t.Fatalf("upload %s was not restored", capability.UploadID)
			}
		}
	})

	t.Run("abort", func(t *testing.T) {
		provider, scope, capabilities := newBatch(t, "rollback-abort")
		uploadIDs := make([]domain.UploadID, len(capabilities))
		for index, capability := range capabilities {
			uploadIDs[index] = capability.UploadID
		}
		provider.mu.Lock()
		beforeTokens := len(provider.uploadTokens)
		provider.mu.Unlock()
		provider.InjectFault(OperationAbortUpload, FaultUnavailable)
		if err := provider.AbortUploadBatch(ctx, scope, domain.AbortUploadBatchRequest{
			UploadIDs: uploadIDs, BatchID: capabilities[0].BatchID, IdempotencyKey: "rollback-abort-operation",
		}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("abort failure = %v", err)
		}
		provider.mu.Lock()
		defer provider.mu.Unlock()
		if len(provider.uploadTokens) != beforeTokens || len(provider.uploadAborts) != 0 {
			t.Fatalf("abort rollback tokens=%d/%d outcomes=%d", len(provider.uploadTokens), beforeTokens, len(provider.uploadAborts))
		}
		for _, capability := range capabilities {
			if provider.uploads[capability.UploadID].state != domain.UploadStateActive {
				t.Fatalf("upload %s was not restored", capability.UploadID)
			}
		}
	})
}
