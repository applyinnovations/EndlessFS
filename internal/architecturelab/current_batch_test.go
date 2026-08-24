package architecturelab

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/drive"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type currentBatchAccounts struct{}

func (currentBatchAccounts) Account(context.Context, domain.UserID) (model.Account, state.Version, error) {
	return model.Account{SchemaVersion: model.SchemaVersion, Status: model.AccountEnabled}, "account", nil
}

type currentBatchEntropy struct {
	counter uint64
	pending []byte
}

func (reader *currentBatchEntropy) Read(destination []byte) (int, error) {
	written := 0
	for written < len(destination) {
		if len(reader.pending) == 0 {
			reader.counter++
			sum := sha256.Sum256([]byte(fmt.Sprintf("current-batch-%d", reader.counter)))
			reader.pending = append(reader.pending, sum[:]...)
		}
		count := copy(destination[written:], reader.pending)
		written += count
		reader.pending = reader.pending[count:]
	}
	return written, nil
}

func TestCurrentTrashBatchProviderSlope(t *testing.T) {
	modelEconomics, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{1, 100} {
		t.Run(fmt.Sprintf("items-%d", count), func(t *testing.T) {
			ctx := context.Background()
			clock := domain.NewFixedClock(time.Date(2049, 2, 3, 4, 5, 6, 0, time.UTC))
			ledger := providerbudget.NewLedger()
			stateBackend := budgettest.WrapClassified(providerbudget.RoleState, objectmemory.New(), ledger, func(_ providerbudget.RequestKind, target string) string {
				return storageformat.ClassifyEconomicsTarget(target)
			})
			fileBase := objectmemory.New()
			server := httptest.NewServer(fileBase)
			t.Cleanup(server.Close)
			ids := domain.NewIDGenerator(&currentBatchEntropy{})
			if err := fileBase.ConfigureDataPlane(server.URL, clock, ids); err != nil {
				t.Fatal(err)
			}
			fileBackend := budgettest.Wrap(providerbudget.RoleFile, fileBase, ledger)
			engine, err := portable.Open(ctx, portable.Options{
				Backend: stateBackend, FileBackend: fileBackend, Clock: clock, IDs: ids,
				Writer:   portable.WriterConfiguration{WriterSetID: "batch-baseline", ConfigurationDigest: "batch-baseline-v1", KeyringIdentifiers: []string{"batch-key"}},
				LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x31}, 32),
			})
			if err != nil {
				t.Fatal(err)
			}
			protection := secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x41}, 32)))
			service, err := drive.NewService(engine.Files(), engine, currentBatchAccounts{}, ids, clock, protection, "https://endlessfs.test", server.URL, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			user, err := domain.ParseUserID("YmF0Y2gtdXNlci0wMDAwMDAwMA")
			if err != nil {
				t.Fatal(err)
			}
			live, _ := domain.NewScope(user, domain.AreaLive)
			source := domain.MustParseUserPath("/file-00000")
			capability, err := service.CreateUpload(ctx, user, domain.CreateUploadRequest{Path: source, Size: 7, MediaType: "text/plain", IdempotencyKey: "batch-upload-source"})
			if err != nil {
				t.Fatal(err)
			}
			request, _ := http.NewRequestWithContext(ctx, capability.Method, capability.URL, strings.NewReader("payload"))
			for name, value := range capability.Headers {
				request.Header.Set(name, value)
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if _, err := service.CompleteUpload(ctx, user, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: source, Size: 7, MediaType: "text/plain"}); err != nil {
				t.Fatal(err)
			}
			paths := make([]domain.UserPath, count)
			paths[0] = source
			for index := 1; index < count; index++ {
				path := domain.MustParseUserPath(fmt.Sprintf("/file-%05d", index))
				if _, err := engine.Files().Copy(ctx, live, live, domain.CopyRequest{Source: source, Destination: path, IdempotencyKey: fmt.Sprintf("batch-seed-copy-%05d", index)}); err != nil {
					t.Fatal(err)
				}
				paths[index] = path
			}
			copyRequests := make([]domain.CopyRequest, count)
			moveRequests := make([]domain.CopyRequest, count)
			for index, path := range paths {
				copyPath := domain.MustParseUserPath(fmt.Sprintf("/copy-%05d", index))
				movedPath := domain.MustParseUserPath(fmt.Sprintf("/moved-%05d", index))
				copyRequests[index] = domain.CopyRequest{Source: path, Destination: copyPath}
				moveRequests[index] = domain.CopyRequest{Source: copyPath, Destination: movedPath}
			}
			ledger.Reset()
			if result, err := service.BatchCopyMove(ctx, user, copyRequests, false, "current-copy-batch-0001"); err != nil || len(result.Items) != count {
				t.Fatalf("BatchCopyMove(copy) = %+v, %v", result, err)
			}
			logCurrentEconomics(t, fmt.Sprintf("before/batch/copy-%d", count), modelEconomics, ledger)
			if result, err := service.BatchCopyMove(ctx, user, moveRequests, true, "current-move-batch-0001"); err != nil || len(result.Items) != count {
				t.Fatalf("BatchCopyMove(move) = %+v, %v", result, err)
			}
			logCurrentEconomics(t, fmt.Sprintf("before/batch/move-%d", count), modelEconomics, ledger)
			ledger.Reset()
			result, err := service.Trash(ctx, user, paths, "current-trash-batch")
			if err != nil || len(result.Items) != count {
				t.Fatalf("Trash() = %+v, %v", result, err)
			}
			logCurrentEconomics(t, fmt.Sprintf("before/batch/trash-%d", count), modelEconomics, ledger)
			if result, err := service.EmptyTrash(ctx, user, true, "current-empty-trash-0001"); err != nil || len(result.Items) != count {
				t.Fatalf("EmptyTrash() = %+v, %v", result, err)
			}
			logCurrentEconomics(t, fmt.Sprintf("before/batch/delete-%d", count), modelEconomics, ledger)
		})
	}
}
