package providercontract

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/integrity"
	"github.com/applyinnovations/endlessfs/internal/provider"
)

func runUploadAdmission(t *testing.T, factory Factory) {
	t.Run("independent admission outcomes and selective replay", func(t *testing.T) {
		h := factory(t)
		storage, ok := h.Storage.(provider.UploadAdmissionStorage)
		if !ok {
			t.Fatal("missing independent admission contract")
		}
		ctx, scope := context.Background(), testScope(t, 0x63, domain.AreaLive)
		requests := []domain.CreateUploadRequest{
			{Path: domain.MustParseUserPath("/admit-a.bin"), Size: 3, MediaType: "application/octet-stream", Resumable: true, IdempotencyKey: "independent-admission-a"},
			{Path: domain.MustParseUserPath("/missing/admit.bin"), Size: 3, MediaType: "application/octet-stream", Resumable: true, IdempotencyKey: "independent-admission-b"},
			{Path: domain.MustParseUserPath("/admit-c.bin"), Size: 3, MediaType: "application/octet-stream", Resumable: true, IdempotencyKey: "independent-admission-c"},
		}
		page, err := storage.AdmitUploads(ctx, scope, requests)
		if err != nil || len(page) != 3 || page[0].Capability == nil || page[2].Capability == nil || page[1].ErrorKind != domain.ErrorNotFound {
			t.Fatalf("mixed admission: %+v %v", page, err)
		}
		for i, item := range page {
			if item.Index != i || item.ID != requests[i].IdempotencyKey {
				t.Fatalf("misbound item: %+v", item)
			}
		}
		accepted := *page[0].Capability
		replay, err := storage.AdmitUploads(ctx, scope, []domain.CreateUploadRequest{requests[2], requests[0]})
		if err != nil || len(replay) != 2 || replay[0].UploadID != page[2].UploadID || replay[1].UploadID != page[0].UploadID {
			t.Fatalf("selective reordered replay: %+v %v", replay, err)
		}
		changed := requests[0]
		changed.Size++
		denied, err := storage.AdmitUploads(ctx, scope, []domain.CreateUploadRequest{changed, requests[2]})
		if err != nil || denied[0].ErrorKind != domain.ErrorConflict || denied[1].UploadID != page[2].UploadID {
			t.Fatalf("changed intent poisoned sibling: %+v %v", denied, err)
		}
		response := sendUpload(t, h.Client, accepted, []byte("abc"), 0)
		_ = response.Body.Close()
		if !successfulUploadStatus(response.StatusCode) {
			t.Fatalf("upload=%d", response.StatusCode)
		}
		ready, err := storage.AdmitUploads(ctx, scope, requests[:1])
		if err != nil || ready[0].UploadID != accepted.UploadID || ready[0].Capability != nil || ready[0].Status == nil || !ready[0].Status.DataComplete || ready[0].NextAction != "complete" {
			t.Fatalf("uploaded item must finalize without another data capability: %+v %v", ready, err)
		}
		entry, err := h.Storage.CompleteUpload(ctx, scope, domain.CompleteUploadRequest{UploadID: accepted.UploadID, Path: requests[0].Path, Size: 3, MediaType: requests[0].MediaType, CRC32C: integrity.CRC32C([]byte("abc"))})
		if err != nil {
			t.Fatal(err)
		}
		terminal, err := storage.AdmitUploads(ctx, scope, []domain.CreateUploadRequest{requests[0]})
		if err != nil || terminal[0].Outcome != "completed" || terminal[0].Capability != nil || terminal[0].UploadID != accepted.UploadID {
			t.Fatalf("completed admission replay: %+v %v", terminal, err)
		}
		current, err := h.Storage.Stat(ctx, scope, requests[0].Path)
		if err != nil || current.Version != entry.Version {
			t.Fatalf("completed sibling republished: %+v %v", current, err)
		}
		if _, err := h.Storage.CreateDirectory(ctx, scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/missing"), Conflict: domain.ConflictFail}); err != nil {
			t.Fatal(err)
		}
		retried, err := storage.AdmitUploads(ctx, scope, []domain.CreateUploadRequest{requests[1]})
		if err != nil || retried[0].Capability == nil {
			t.Fatalf("isolated failed-item retry: %+v %v", retried, err)
		}
		for _, bad := range [][]domain.CreateUploadRequest{nil, make([]domain.CreateUploadRequest, 101), {requests[0], requests[0]}} {
			if _, err := storage.AdmitUploads(ctx, scope, bad); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("invalid envelope: %v", err)
			}
		}
		other := testScope(t, 0x64, domain.AreaLive)
		separate, err := storage.AdmitUploads(ctx, other, []domain.CreateUploadRequest{requests[0]})
		if err != nil || separate[0].Capability == nil || separate[0].UploadID == accepted.UploadID {
			t.Fatalf("another owner aliased an item key: %+v %v", separate, err)
		}
		if _, err := h.Storage.ResumeUpload(ctx, other, accepted.UploadID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("foreign capability recovery: %v", err)
		}
		if err := h.Storage.AbortUpload(ctx, scope, retried[0].UploadID); err != nil {
			t.Fatal(err)
		}
		aborted, err := storage.AdmitUploads(ctx, scope, requests[1:2])
		if err != nil || aborted[0].Outcome != "aborted" || aborted[0].Capability != nil {
			t.Fatalf("aborted replay: %+v %v", aborted, err)
		}
		h.Advance(24 * time.Hour)
		expired, err := storage.AdmitUploads(ctx, scope, requests[2:])
		if err != nil || expired[0].Outcome != "expired" || expired[0].Capability != nil {
			t.Fatalf("expired replay: %+v %v", expired, err)
		}
		invalid := requests[0]
		invalid.IdempotencyKey = "short"
		if _, err := storage.AdmitUploads(ctx, scope, []domain.CreateUploadRequest{invalid}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("short durable key: %v", err)
		}
		if _, err := storage.AdmitUploads(ctx, domain.Scope{}, requests[:1]); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("invalid owner: %v", err)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := storage.AdmitUploads(cancelled, scope, requests[:1]); err == nil {
			t.Fatal("cancelled admission accepted")
		}
	})
}
