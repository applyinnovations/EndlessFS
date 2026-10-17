package providercontract

import (
	"context"
	"errors"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/integrity"
	"github.com/applyinnovations/endlessfs/internal/provider"
)

func runDirectoryPreparation(t *testing.T, factory Factory) {
	t.Run("bounded independent directory preparation and replay", func(t *testing.T) {
		h := factory(t)
		ctx := context.Background()
		owner := testScope(t, 0x31, domain.AreaLive)
		preparer, ok := h.Storage.(provider.DirectoryPreparationStorage)
		if !ok {
			t.Fatal("missing shared directory preparation contract")
		}
		paths := []domain.UserPath{domain.MustParseUserPath("/folder/deep"), domain.MustParseUserPath("/folder"), domain.MustParseUserPath("/sibling"), domain.MustParseUserPath("/absent/child")}
		result, err := preparer.PrepareDirectories(ctx, owner, paths)
		if err != nil || len(result) != len(paths) {
			t.Fatalf("prepare: %+v %v", result, err)
		}
		for index, item := range result {
			if item.Index != index || item.Path != paths[index] || item.Ready != (index < 3) {
				t.Fatalf("independent result: %+v", result)
			}
		}
		if result[3].ErrorCode != domain.ErrorNotFound {
			t.Fatalf("unselected parent: %+v", result[3])
		}
		before, err := h.Storage.Stat(ctx, owner, paths[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := preparer.PrepareDirectories(ctx, owner, paths[:3]); err != nil {
			t.Fatal(err)
		}
		after, err := h.Storage.Stat(ctx, owner, paths[0])
		if err != nil || before.Version != after.Version {
			t.Fatalf("replay changed existing directory: %+v %+v %v", before, after, err)
		}
		other := testScope(t, 0x32, domain.AreaLive)
		if _, err := h.Storage.Stat(ctx, other, paths[0]); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("owner isolation: %v", err)
		}
		capability, err := h.Storage.CreateUpload(ctx, owner, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/blocked"), Size: 3, Resumable: true, MediaType: "application/octet-stream"})
		if err != nil {
			t.Fatal(err)
		}
		response := sendUpload(t, h.Client, capability, []byte("abc"), 0)
		_ = response.Body.Close()
		if !successfulUploadStatus(response.StatusCode) {
			t.Fatalf("file ancestor fixture: %d", response.StatusCode)
		}
		file, err := h.Storage.CompleteUpload(ctx, owner, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: domain.MustParseUserPath("/blocked"), Size: 3, MediaType: "application/octet-stream", CRC32C: integrity.CRC32C([]byte("abc"))})
		if err != nil {
			t.Fatal(err)
		}
		mixed, err := preparer.PrepareDirectories(ctx, owner, []domain.UserPath{file.Path, domain.MustParseUserPath("/blocked/child"), domain.MustParseUserPath("/unrelated")})
		if err != nil || len(mixed) != 3 || mixed[0].Ready || mixed[1].Ready || mixed[0].ErrorCode != domain.ErrorConflict || mixed[1].ErrorCode != domain.ErrorConflict || !mixed[2].Ready {
			t.Fatalf("file ancestor blocked independent sibling: %+v %v", mixed, err)
		}
		if after, err := h.Storage.Stat(ctx, owner, file.Path); err != nil || after.Version != file.Version {
			t.Fatalf("preparation replaced existing file: %+v %v", after, err)
		}
		for _, bad := range [][]domain.UserPath{nil, {paths[0], paths[0]}, make([]domain.UserPath, 101), {domain.UserPath{}}, {domain.MustParseUserPath("/")}} {
			if _, err := preparer.PrepareDirectories(ctx, owner, bad); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("invalid page: %v", err)
			}
		}
		if _, err := preparer.PrepareDirectories(ctx, testScope(t, 0x31, domain.AreaTrash), paths[:1]); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("trash preparation: %v", err)
		}
	})
}
