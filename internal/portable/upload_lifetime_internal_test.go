package portable

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func TestUploadSessionLifetimeSurvivesSlowSourceWithoutRenewingExpiry(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "batch"}[batch], func(t *testing.T) {
			fixture := newUploadBatchScaleFixture(t, 0x91348326)
			fixture.engine.uploadTTL = 5 * time.Minute
			fixture.engine.uploadSessionTTL = 24 * time.Hour
			ctx := context.Background()
			clock := fixture.engine.clock.(*domain.FixedClock)
			started := clock.Now()
			requests := scaleUploadRequestsCount("slow-source", 2)
			var capabilities []domain.UploadCapability
			var err error
			if batch {
				capabilities, err = fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, requests)
			} else {
				var cap domain.UploadCapability
				cap, err = fixture.engine.Files().CreateUpload(ctx, fixture.scope, requests[0])
				capabilities = []domain.UploadCapability{cap}
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, cap := range capabilities {
				if !cap.ExpiresAt.Equal(started.Add(24 * time.Hour)) {
					t.Fatalf("session collapsed into initiation deadline: %v", cap.ExpiresAt)
				}
			}
			clock.Advance(6 * time.Hour)
			second, err := Open(ctx, Options{Backend: fixture.engine.backend, FileBackend: fixture.engine.fileBackend, Clock: clock,
				IDs: domain.NewIDGenerator(&deterministicScaleReader{state: 0x71342186}), Writer: WriterConfiguration{WriterSetID: fixture.engine.writer.WriterSetID, ConfigurationDigest: fixture.engine.writer.ConfigurationDigest, KeyringIdentifiers: fixture.engine.writer.KeyringIdentifiers},
				LeaseTTL: time.Minute, UploadTTL: 5 * time.Minute, UploadSessionTTL: 48 * time.Hour, CursorKey: bytes.Repeat([]byte{0x64}, 32)})
			if err != nil {
				t.Fatal(err)
			}
			for _, cap := range capabilities {
				other, err := second.Files().ResumeUpload(ctx, fixture.scope, cap.UploadID)
				if err != nil || !other.ExpiresAt.Equal(cap.ExpiresAt) {
					t.Fatalf("replica default changed persisted deadline: %v, %+v", err, other)
				}
				resumed, err := fixture.engine.Files().ResumeUpload(ctx, fixture.scope, cap.UploadID)
				if err != nil || resumed.UploadID != cap.UploadID || !resumed.ExpiresAt.Equal(cap.ExpiresAt) {
					t.Fatalf("slow source resume changed authority: %v, %+v", err, resumed)
				}
			}
			clock.Advance(18 * time.Hour)
			for _, cap := range capabilities {
				if _, err := second.Files().ResumeUpload(ctx, fixture.scope, cap.UploadID); !errors.Is(err, domain.ErrConflict) {
					t.Fatalf("longer-default replica renewed expired authority: %v", err)
				}
				if _, err := fixture.engine.Files().ResumeUpload(ctx, fixture.scope, cap.UploadID); !errors.Is(err, domain.ErrConflict) {
					t.Fatalf("expired session resumed: %v", err)
				}
			}
		})
	}
}
