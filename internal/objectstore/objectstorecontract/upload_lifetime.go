package objectstorecontract

import (
	"context"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

type UploadLifetimeHarness struct {
	Backend objectstore.DirectTransferBackend
	Clock   *domain.FixedClock
}

// RunUploadLifetime proves that initiation expiry is not transfer expiry, and
// resume neither creates another destination nor extends the absolute deadline.
func RunUploadLifetime(t *testing.T, factory func(*testing.T) UploadLifetimeHarness) {
	t.Helper()
	h := factory(t)
	ctx := context.Background()
	started := h.Clock.Now()
	handle, err := h.Backend.BeginUpload(ctx, objectstore.UploadRequest{UploadID: "lifetime-contract", Key: objectstore.MustKey("endlessfs/v1/fs/lifetime-contract/blobs/object"), Size: 4, MediaType: "application/octet-stream", Resumable: true, ExpiresAt: started.Add(24 * time.Hour), InitiationExpiresAt: started.Add(5 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	h.Clock.Advance(6 * time.Hour)
	resumed, err := h.Backend.ResumeUpload(ctx, handle.Lease)
	if err != nil || resumed.URL != handle.Capability.URL || !resumed.ExpiresAt.Equal(handle.Capability.ExpiresAt) || resumed.DeclaredSize != 4 {
		t.Fatalf("resume changed admitted authority or inherited initiation expiry: %v, %+v", err, resumed)
	}
	for range 2 {
		again, err := h.Backend.ResumeUpload(ctx, handle.Lease)
		if err != nil || !again.ExpiresAt.Equal(started.Add(24*time.Hour)) {
			t.Fatalf("resume renewed authority: %v", err)
		}
	}
	tampered := append([]byte(nil), handle.Lease...)
	tampered[len(tampered)/2] ^= 0x80
	if _, err := h.Backend.ResumeUpload(ctx, tampered); err == nil {
		t.Fatal("tampered session lease resumed")
	}
	h.Clock.Advance(18 * time.Hour)
	if _, err := h.Backend.ResumeUpload(ctx, handle.Lease); err == nil {
		t.Fatal("expired session lease resumed")
	}
}
