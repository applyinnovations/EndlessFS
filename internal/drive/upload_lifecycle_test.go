package drive_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/drive"
	"github.com/applyinnovations/endlessfs/internal/secret"
)

func TestUploadLifecycleServiceBoundsScopeAndUnsupportedStorage(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	unsupported, err := drive.NewService(&reconciliationStorage{NamespaceStorage: env.storage}, env.store, env.repo, domain.NewIDGenerator(&hashReader{}), env.clock, secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x77}, 32))), "http://127.0.0.1:8080", "http://127.0.0.1:8081", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, service := range []*drive.Service{env.service, unsupported} {
		for _, count := range []int{0, 101} {
			if _, err := service.UploadLifecycle(ctx, env.owner, make([]domain.UploadID, count), true); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("status page bound: %v", err)
			}
			if _, err := service.UploadActions(ctx, env.owner, make([]domain.UploadActionItem, count)); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("action page bound: %v", err)
			}
		}
		if _, err := service.UploadLifecycle(ctx, domain.UserID{}, []domain.UploadID{"missing"}, true); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid status owner: %v", err)
		}
		if _, err := service.UploadActions(ctx, domain.UserID{}, []domain.UploadActionItem{{UploadID: "missing", Action: "cancel"}}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid action owner: %v", err)
		}
	}
	if _, err := unsupported.UploadLifecycle(ctx, env.owner, []domain.UploadID{"missing"}, true); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("unsupported recovery failed open: %v", err)
	}
	if _, err := unsupported.UploadActions(ctx, env.owner, []domain.UploadActionItem{{UploadID: "missing", Action: "cancel"}}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("unsupported action failed open: %v", err)
	}
}
