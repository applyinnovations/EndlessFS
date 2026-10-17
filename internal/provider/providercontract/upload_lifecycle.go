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

func runUploadLifecycle(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("bounded independent lifecycle receipts and isolation", func(t *testing.T) {
		h := factory(t)
		ctx := context.Background()
		owner := testScope(t, 0x31, domain.AreaLive)
		lifecycle, ok := h.Storage.(provider.UploadLifecycleStorage)
		if !ok {
			t.Fatal("missing upload lifecycle contract")
		}
		actions, ok := h.Storage.(provider.UploadActionStorage)
		if !ok {
			t.Fatal("missing upload actions contract")
		}
		caps, err := h.Storage.(provider.UploadBatchStorage).CreateUploadBatch(ctx, owner, []domain.CreateUploadRequest{
			{Path: domain.MustParseUserPath("/complete.bin"), Size: 3, Resumable: true, MediaType: "application/octet-stream", IdempotencyKey: "lifecycle-contract"},
			{Path: domain.MustParseUserPath("/cancel.bin"), Size: 3, Resumable: true, MediaType: "application/octet-stream", IdempotencyKey: "lifecycle-contract-cancel"},
		})
		if err != nil {
			t.Fatal(err)
		}
		ids := []domain.UploadID{caps[0].UploadID, "missing", caps[1].UploadID}
		page, err := lifecycle.UploadLifecycle(ctx, owner, ids, true)
		if err != nil || len(page) != 3 {
			t.Fatalf("page: %+v %v", page, err)
		}
		if page[0].Capability == nil || !page[0].Capability.ExpiresAt.Equal(caps[0].ExpiresAt) || page[1].ErrorKind != domain.ErrorNotFound || page[1].Status != nil || page[2].Capability == nil {
			t.Fatalf("mixed lifecycle: %+v", page)
		}
		for _, scope := range []domain.Scope{testScope(t, 0x32, domain.AreaLive), testScope(t, 0x31, domain.AreaTrash)} {
			denied, err := lifecycle.UploadLifecycle(ctx, scope, ids, true)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range denied {
				if item.ErrorKind != domain.ErrorNotFound || item.Status != nil || item.Capability != nil {
					t.Fatalf("cross-scope disclosure: %+v", item)
				}
			}
			deniedActions, err := actions.UploadActions(ctx, scope, []domain.UploadActionItem{{UploadID: caps[0].UploadID, Action: "cancel"}})
			if err != nil || deniedActions[0].ErrorKind != domain.ErrorNotFound || deniedActions[0].Status != nil {
				t.Fatalf("cross-scope action: %+v %v", deniedActions, err)
			}
		}
		response := sendUpload(t, h.Client, caps[0], []byte("abc"), 0)
		_ = response.Body.Close()
		if !successfulUploadStatus(response.StatusCode) {
			t.Fatalf("data upload: %d", response.StatusCode)
		}
		checksum := integrity.CRC32C([]byte("abc"))
		requested := []domain.UploadActionItem{{UploadID: caps[0].UploadID, Action: "complete", CRC32C: checksum}, {UploadID: caps[1].UploadID, Action: "cancel"}, {UploadID: "missing", Action: "complete", CRC32C: checksum}}
		result, err := actions.UploadActions(ctx, owner, requested)
		if err != nil || len(result) != 3 || result[0].Outcome != "applied" || result[0].Entry == nil || result[1].Outcome != "applied" || result[2].ErrorKind != domain.ErrorNotFound {
			t.Fatalf("independent actions: %+v %v", result, err)
		}
		replay, err := actions.UploadActions(ctx, owner, []domain.UploadActionItem{requested[1], requested[0]})
		if err != nil || replay[0].Outcome != "already_applied" || replay[1].Outcome != "already_applied" || replay[1].Entry == nil || replay[1].Entry.Version != result[0].Entry.Version {
			t.Fatalf("selective reordered replay: %+v %v", replay, err)
		}
		changed := requested[0]
		changed.CRC32C = integrity.CRC32C([]byte("xyz"))
		rejected, err := actions.UploadActions(ctx, owner, []domain.UploadActionItem{changed})
		if err != nil || rejected[0].ErrorKind != domain.ErrorConflict {
			t.Fatalf("changed completed intent: %+v %v", rejected, err)
		}
		h.Advance(24 * time.Hour)
		terminal, err := lifecycle.UploadLifecycle(ctx, owner, ids, true)
		if err != nil || terminal[0].Status.State != domain.UploadStateCompleted || terminal[2].Status.State != domain.UploadStateAborted || terminal[0].Capability != nil || terminal[2].Capability != nil {
			t.Fatalf("terminal state precedence: %+v %v", terminal, err)
		}
		for _, bad := range [][]domain.UploadID{nil, {""}, {"same", "same"}, make([]domain.UploadID, 101)} {
			if _, err := lifecycle.UploadLifecycle(ctx, owner, bad, true); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("invalid page: %v", err)
			}
		}
	})
}
