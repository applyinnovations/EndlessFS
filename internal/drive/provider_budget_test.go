package drive_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
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

type budgetAccountReader struct{}

type budgetHashReader struct {
	counter uint64
	pending []byte
}

func (reader *budgetHashReader) Read(destination []byte) (int, error) {
	written := 0
	for written < len(destination) {
		if len(reader.pending) == 0 {
			reader.counter++
			sum := sha256.Sum256([]byte(time.Unix(int64(reader.counter), 0).UTC().String()))
			reader.pending = append(reader.pending, sum[:]...)
		}
		count := copy(destination[written:], reader.pending)
		written += count
		reader.pending = reader.pending[count:]
	}
	return written, nil
}

func (budgetAccountReader) Account(context.Context, domain.UserID) (model.Account, state.Version, error) {
	return model.Account{SchemaVersion: model.SchemaVersion, Status: model.AccountEnabled}, "account-version", nil
}

func TestProviderBudgetTrashAndRestore(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2049, 1, 2, 3, 4, 5, 0, time.UTC))
	stateLedger, fileLedger := providerbudget.NewLedger(), providerbudget.NewLedger()
	stateBackend := budgettest.WrapClassified(providerbudget.RoleState, objectmemory.New(), stateLedger, func(_ providerbudget.RequestKind, target string) string {
		return storageformat.ClassifyEconomicsTarget(target)
	})
	fileBase := objectmemory.New()
	dataServer := httptest.NewServer(fileBase)
	t.Cleanup(dataServer.Close)
	if err := fileBase.ConfigureDataPlane(dataServer.URL, clock, domain.NewIDGenerator(&budgetHashReader{})); err != nil {
		t.Fatal(err)
	}
	fileBackend := budgettest.Wrap(providerbudget.RoleFile, fileBase, fileLedger)
	ids := domain.NewIDGenerator(&budgetHashReader{})
	engine, err := portable.Open(ctx, portable.Options{
		Backend: stateBackend, FileBackend: fileBackend, Clock: clock, IDs: ids,
		Writer:   portable.WriterConfiguration{WriterSetID: "provider-budget", ConfigurationDigest: "provider-budget-v1", KeyringIdentifiers: []string{"budget-key"}},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x71}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	tokenKey := secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x72}, 32)))
	service, err := drive.NewService(engine.Files(), engine, budgetAccountReader{}, ids, clock, tokenKey, "https://endlessfs.test", dataServer.URL, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	user, err := domain.ParseUserID("YnVkZ2V0LXVzZXItMDAwMDAwMA")
	if err != nil {
		t.Fatal(err)
	}
	path := domain.MustParseUserPath("/budget-file.txt")
	modelEconomics, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	ratchet, err := gcs.RegionalStandardFlatBudgetRatchet()
	if err != nil {
		t.Fatal(err)
	}
	check := func(name string) {
		t.Helper()
		events := append(stateLedger.Events(), fileLedger.Events()...)
		budget, ok := ratchet.Latest(name)
		if !ok {
			t.Fatalf("provider budget %q is missing", name)
		}
		if report, err := budget.CheckRatchet(modelEconomics, events); err != nil {
			t.Errorf("%s: %v; observed=%+v; events=%+v", name, err, report.Totals, events)
		}
	}
	stateLedger.Reset()
	fileLedger.Reset()

	capability, err := service.CreateUpload(ctx, user, domain.CreateUploadRequest{Path: path, Size: 7, MediaType: "text/plain", IdempotencyKey: "budget-upload-create-0001"})
	if err != nil {
		t.Fatal(err)
	}
	check("file-create-upload")

	stateLedger.Reset()
	fileLedger.Reset()
	if _, err := service.UploadStatus(ctx, user, capability.UploadID); err != nil {
		t.Fatal(err)
	}
	check("file-upload-status-active")

	upload, err := http.NewRequestWithContext(ctx, capability.Method, capability.URL, strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range capability.Headers {
		upload.Header.Set(name, value)
	}
	response, err := dataServer.Client().Do(upload)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("upload status = %d", response.StatusCode)
	}
	stateLedger.Reset()
	fileLedger.Reset()
	entry, err := service.CompleteUpload(ctx, user, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: 7, MediaType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	check("file-complete-upload")

	stateLedger.Reset()
	fileLedger.Reset()
	live, _ := domain.NewScope(user, domain.AreaLive)
	if _, err := engine.Files().CreateDownload(ctx, live, domain.CreateDownloadRequest{Path: path, Version: entry.Version, Disposition: domain.DispositionAttachment}); err != nil {
		t.Fatal(err)
	}
	check("file-create-download")

	stateLedger.Reset()
	fileLedger.Reset()
	trashed, err := service.Trash(ctx, user, []domain.UserPath{path}, "budget-trash-00000001")
	if err != nil || len(trashed.Items) != 1 || trashed.Items[0].TrashID == "" {
		t.Fatalf("Trash() = %+v, %v", trashed, err)
	}
	check("trash-one-file")

	stateLedger.Reset()
	fileLedger.Reset()
	if _, err := service.Restore(ctx, user, trashed.Items[0].TrashID, domain.ConflictFail, "budget-restore-000001"); err != nil {
		t.Fatal(err)
	}
	check("restore-one-file")

	trash, _ := domain.NewScope(user, domain.AreaTrash)
	stateLedger.Reset()
	fileLedger.Reset()
	if _, err := engine.Files().Move(ctx, live, trash, domain.MoveRequest{Source: path, Destination: domain.MustParseUserPath("/direct"), IdempotencyKey: "budget-direct-move-0001"}); err != nil {
		t.Fatal(err)
	}
	check("direct-move-one-file")

	abortCapability, err := service.CreateUpload(ctx, user, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/aborted.bin"), Size: 3, MediaType: "application/octet-stream", IdempotencyKey: "budget-upload-abort-0001"})
	if err != nil {
		t.Fatal(err)
	}
	stateLedger.Reset()
	fileLedger.Reset()
	if err := service.AbortUpload(ctx, user, abortCapability.UploadID); err != nil {
		t.Fatal(err)
	}
	check("file-abort-upload")

	if _, err := engine.Files().Move(ctx, trash, live, domain.MoveRequest{Source: domain.MustParseUserPath("/direct"), Destination: path, IdempotencyKey: "budget-direct-reset-0001"}); err != nil {
		t.Fatal(err)
	}
	stateLedger.Reset()
	fileLedger.Reset()
	if _, err := engine.Files().Copy(ctx, live, live, domain.CopyRequest{Source: path, Destination: domain.MustParseUserPath("/budget-copy.txt"), IdempotencyKey: "budget-direct-copy-0001"}); err != nil {
		t.Fatal(err)
	}
	check("direct-copy-one-file")

	stateLedger.Reset()
	fileLedger.Reset()
	if _, err := engine.Files().Delete(ctx, live, domain.DeleteRequest{Path: domain.MustParseUserPath("/budget-copy.txt"), IdempotencyKey: "budget-direct-delete-01"}); err != nil {
		t.Fatal(err)
	}
	check("direct-delete-one-file")

	stateLedger.Reset()
	fileLedger.Reset()
	if _, err := service.CreateDirectory(ctx, user, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/budget-directory")}); err != nil {
		t.Fatal(err)
	}
	check("file-create-directory")
}
