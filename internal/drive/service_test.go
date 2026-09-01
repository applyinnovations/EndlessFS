package drive_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/drive"
	"github.com/applyinnovations/endlessfs/internal/identity"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/provider"
	providermemory "github.com/applyinnovations/endlessfs/internal/provider/memory"
	"github.com/applyinnovations/endlessfs/internal/secret"
	statememory "github.com/applyinnovations/endlessfs/internal/state"
)

type driveEnvironment struct {
	service *drive.Service
	storage *providermemory.Provider
	client  *http.Client
	clock   *domain.FixedClock
	repo    *identity.Repository
	store   *statememory.MemoryStore
	owner   domain.UserID
	other   domain.UserID
}

type hashReader struct {
	counter uint64
	pending []byte
}

func (r *hashReader) Read(destination []byte) (int, error) {
	written := 0
	for written < len(destination) {
		if len(r.pending) == 0 {
			r.counter++
			sum := sha256.Sum256([]byte(time.Unix(int64(r.counter), 0).UTC().String()))
			r.pending = append(r.pending, sum[:]...)
		}
		count := copy(destination[written:], r.pending)
		written += count
		r.pending = r.pending[count:]
	}
	return written, nil
}

func newDriveEnvironment(t *testing.T) driveEnvironment {
	t.Helper()
	clock := domain.NewFixedClock(time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC))
	ids := domain.NewIDGenerator(&hashReader{})
	storage := providermemory.New(providermemory.Options{Clock: clock, IDs: ids, UploadTTL: 5 * time.Minute, DownloadTTL: time.Minute})
	server := httptest.NewServer(storage)
	t.Cleanup(server.Close)
	if err := storage.SetDataPlaneBaseURL(server.URL); err != nil {
		t.Fatal(err)
	}
	store := statememory.NewMemoryStore()
	repository := identity.NewRepository(store)
	owner := fixedUserID(t, 0x31)
	other := fixedUserID(t, 0x41)
	for _, userID := range []domain.UserID{owner, other} {
		now := clock.Now()
		if err := repository.CreateAccount(context.Background(), model.Account{SchemaVersion: model.SchemaVersion, UserID: userID, Status: model.AccountEnabled, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	key := secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x77}, 32)))
	service, err := drive.NewService(storage, store, repository, ids, clock, key, "http://127.0.0.1:8080", server.URL, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return driveEnvironment{service: service, storage: storage, client: server.Client(), clock: clock, repo: repository, store: store, owner: owner, other: other}
}

type baseStorageOnly struct{ provider.Storage }

type reconciliationStorage struct {
	provider.NamespaceStorage
	selection  domain.DuplicateReconciliationSelection
	applyCalls int
}

type uploadPlanningStorage struct {
	provider.NamespaceStorage
	sizeOwner        domain.UserID
	sizeRequest      domain.UploadSizePlanRequest
	fingerprintOwner domain.UserID
	fingerprintReq   domain.UploadFingerprintPlanRequest
}

func (s *uploadPlanningStorage) PlanUploadSizes(_ context.Context, owner domain.UserID, request domain.UploadSizePlanRequest) (domain.UploadSizePlan, error) {
	s.sizeOwner = owner
	s.sizeRequest = request
	return domain.UploadSizePlan{
		Token: "size-plan-token",
		Items: []domain.UploadSizePlanDecision{{ID: request.Items[0].ID, FingerprintRequired: true}},
	}, nil
}

func (s *uploadPlanningStorage) PlanUploadFingerprints(_ context.Context, owner domain.UserID, request domain.UploadFingerprintPlanRequest) (domain.UploadFingerprintPlan, error) {
	s.fingerprintOwner = owner
	s.fingerprintReq = request
	return domain.UploadFingerprintPlan{
		Items: []domain.UploadFingerprintPlanDecision{{ID: request.Items[0].ID, Action: domain.UploadPlanSkip}},
	}, nil
}

func (s *reconciliationStorage) ListDuplicateGroups(context.Context, domain.UserID, domain.DuplicateGroupRequest) (domain.DuplicateGroupPage, error) {
	return domain.DuplicateGroupPage{}, nil
}

func (s *reconciliationStorage) ListDuplicateOccurrences(context.Context, domain.UserID, domain.DuplicateOccurrenceRequest) (domain.DuplicateOccurrencePage, error) {
	return domain.DuplicateOccurrencePage{}, nil
}

func (s *reconciliationStorage) SetDuplicateGroupIgnored(context.Context, domain.UserID, domain.SetDuplicateIgnoredRequest) (domain.DuplicateIgnore, error) {
	return domain.DuplicateIgnore{}, nil
}

func (s *reconciliationStorage) CompareDuplicateDirectories(context.Context, domain.UserID, domain.DuplicateDirectoryComparisonRequest) (domain.DuplicateDirectoryComparison, error) {
	return domain.DuplicateDirectoryComparison{}, nil
}

func (s *reconciliationStorage) ListDuplicateDirectoryOverlaps(context.Context, domain.UserID, domain.DuplicateDirectoryOverlapRequest) (domain.DuplicateDirectoryOverlapPage, error) {
	return domain.DuplicateDirectoryOverlapPage{}, nil
}

func (s *reconciliationStorage) SetDuplicateDirectoryIgnored(context.Context, domain.UserID, domain.SetDuplicateDirectoryIgnoredRequest) (domain.DuplicateDirectoryIgnore, error) {
	return domain.DuplicateDirectoryIgnore{}, nil
}

func (s *reconciliationStorage) PreviewDuplicateReconciliation(context.Context, domain.UserID, domain.DuplicateReconciliationPreviewRequest) (domain.DuplicateReconciliationPreview, error) {
	return domain.DuplicateReconciliationPreview{}, nil
}

func (s *reconciliationStorage) ValidateDuplicateReconciliation(_ context.Context, _ domain.UserID, token string) (domain.DuplicateReconciliationSelection, error) {
	if token != "valid-plan-token" {
		return domain.DuplicateReconciliationSelection{}, domain.NewError(domain.ErrorInvalid, "invalid plan")
	}
	return s.selection, nil
}

func (s *reconciliationStorage) ApplyDuplicateReconciliation(ctx context.Context, owner domain.UserID, token, idempotencyKey string) (domain.NamespaceBatchResult, error) {
	s.applyCalls++
	selection, err := s.ValidateDuplicateReconciliation(ctx, owner, token)
	if err != nil {
		return domain.NamespaceBatchResult{}, err
	}
	if len(selection.Items) < 1 || len(selection.Items) > drive.MaxBatchItems {
		return domain.NamespaceBatchResult{}, domain.NewError(domain.ErrorInvalid, "invalid bounded reconciliation selection")
	}
	live, err := domain.NewScope(owner, domain.AreaLive)
	if err != nil {
		return domain.NamespaceBatchResult{}, err
	}
	requests := make([]domain.TrashRequest, len(selection.Items))
	for index, item := range selection.Items {
		keep, statErr := s.NamespaceStorage.Stat(ctx, live, item.Keep.Path)
		if statErr != nil {
			return domain.NamespaceBatchResult{}, statErr
		}
		if keep.Version != item.Keep.Version {
			return domain.NamespaceBatchResult{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate keep occurrence changed")
		}
		requests[index] = domain.TrashRequest{Path: item.Remove.Path, ExpectedVersion: item.Remove.Version, TrashID: fmt.Sprintf("reconcile-%d", index)}
	}
	return s.NamespaceStorage.BatchMoveToTrash(ctx, owner, requests, idempotencyKey)
}

func fixedUserID(t *testing.T, value byte) domain.UserID {
	t.Helper()
	id, err := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{value}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func upload(t *testing.T, env driveEnvironment, owner domain.UserID, path string, body []byte, mediaType, key string) domain.Entry {
	t.Helper()
	parsed := domain.MustParseUserPath(path)
	capability, err := env.service.CreateUpload(context.Background(), owner, domain.CreateUploadRequest{Path: parsed, Size: int64(len(body)), MediaType: mediaType, IdempotencyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(capability.Method, capability.URL, bytes.NewReader(body))
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	response, err := env.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("upload status = %d", response.StatusCode)
	}
	entry, err := env.service.CompleteUpload(context.Background(), owner, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: parsed, Size: int64(len(body)), MediaType: mediaType})
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestIntegrationDirectTransfersAndIsolation(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	folder, err := env.service.CreateDirectory(ctx, env.owner, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/docs")})
	if err != nil || folder.Kind != domain.EntryDirectory {
		t.Fatalf("CreateDirectory = %+v, %v", folder, err)
	}
	capability, err := env.service.CreateUpload(ctx, env.owner, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/docs/hello.txt"), Size: 11, MediaType: "text/plain", Resumable: true, IdempotencyKey: "upload-idempotency-001"})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := env.service.CreateUpload(ctx, env.owner, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/docs/hello.txt"), Size: 11, MediaType: "text/plain", Resumable: true, IdempotencyKey: "upload-idempotency-001"})
	if err != nil || replayed.URL != capability.URL {
		t.Fatalf("upload replay = %+v, %v", replayed, err)
	}
	if _, err := env.service.CreateUpload(ctx, env.other, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/docs/hello.txt"), Size: 11, MediaType: "text/plain", IdempotencyKey: "upload-idempotency-001"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("other upload = %v", err)
	}
	request, _ := http.NewRequest(capability.Method, capability.URL, bytes.NewBufferString("hello world"))
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	response, err := env.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	status, err := env.service.UploadStatus(ctx, env.owner, capability.UploadID)
	if err != nil || status.ConfirmedOffset != 11 {
		t.Fatalf("UploadStatus = %+v, %v", status, err)
	}
	if _, err := env.service.UploadStatus(ctx, env.other, capability.UploadID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-user status = %v", err)
	}
	entry, err := env.service.CompleteUpload(ctx, env.owner, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: domain.MustParseUserPath("/docs/hello.txt"), Size: 11, MediaType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := env.service.List(ctx, env.owner, domain.ListRequest{Directory: domain.MustParseUserPath("/docs")})
	if err != nil || len(page.Entries) != 1 {
		t.Fatalf("List = %+v, %v", page, err)
	}
	download, mode, err := env.service.Download(ctx, env.owner, domain.CreateDownloadRequest{Path: entry.Path, Version: entry.Version}, true)
	if err != nil || mode != "text" {
		t.Fatalf("Download = %+v %s, %v", download, mode, err)
	}
	response, err = env.client.Get(download.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "hello world" || response.Header.Get("Content-Disposition") == "" {
		t.Fatalf("download = %q headers=%v", body, response.Header)
	}
	metrics := env.storage.Instrumentation()
	if metrics.ControlPlaneBytes != 0 || metrics.UploadBytes != 11 || metrics.DownloadBytes != 11 {
		t.Fatalf("instrumentation = %+v", metrics)
	}
}

func TestDuplicateReconciliationDelegatesOneAtomicProviderMutation(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	for _, path := range []string{"/left", "/right"} {
		if _, err := env.service.CreateDirectory(ctx, env.owner, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(path)}); err != nil {
			t.Fatal(err)
		}
	}
	remove := upload(t, env, env.owner, "/left/same.bin", []byte("same"), "application/octet-stream", "reconcile-remove-upload")
	keep := upload(t, env, env.owner, "/right/same.bin", []byte("same"), "application/octet-stream", "reconcile-keep-upload")
	groupID := secret.Hash("same-content-group")
	wrapped := &reconciliationStorage{NamespaceStorage: env.storage, selection: domain.DuplicateReconciliationSelection{
		Left:       domain.DuplicateOccurrence{GroupID: groupID, Kind: domain.DuplicateDirectory, Area: domain.AreaLive, AreaName: "live", Path: domain.MustParseUserPath("/left"), Version: "left-version"},
		Right:      domain.DuplicateOccurrence{GroupID: groupID, Kind: domain.DuplicateDirectory, Area: domain.AreaLive, AreaName: "live", Path: domain.MustParseUserPath("/right"), Version: "right-version"},
		RemoveFrom: domain.DuplicateSideLeft,
		Items: []domain.DuplicateReconciliationItem{{
			GroupID: groupID,
			Remove:  domain.DuplicateOccurrence{GroupID: groupID, Kind: domain.DuplicateFile, Area: domain.AreaLive, AreaName: "live", Path: remove.Path, Size: remove.Size, FileCount: 1, Version: remove.Version},
			Keep:    domain.DuplicateOccurrence{GroupID: groupID, Kind: domain.DuplicateFile, Area: domain.AreaLive, AreaName: "live", Path: keep.Path, Size: keep.Size, FileCount: 1, Version: keep.Version},
		}},
	}}
	ids := domain.NewIDGenerator(&hashReader{})
	key := secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x77}, 32)))
	service, err := drive.NewService(wrapped, env.store, env.repo, ids, env.clock, key, "http://127.0.0.1:8080", "http://127.0.0.1:8081", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.ApplyDuplicateReconciliation(ctx, env.owner, "valid-plan-token", "duplicate-reconcile-apply-001")
	if err != nil || len(result.Items) != 1 || result.Items[0].State != domain.OperationSucceeded || result.Items[0].TrashID == "" {
		t.Fatalf("ApplyDuplicateReconciliation() = %+v, %v", result, err)
	}
	if wrapped.applyCalls != 1 {
		t.Fatalf("atomic reconciliation calls = %d, want one", wrapped.applyCalls)
	}
	if _, err := service.Stat(ctx, env.owner, remove.Path); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("reconciled source remains live: %v", err)
	}
	if current, err := service.Stat(ctx, env.owner, keep.Path); err != nil || current.Version != keep.Version {
		t.Fatalf("preserved occurrence = %+v, %v", current, err)
	}
	if operation, err := wrapped.GetBatchOperation(ctx, env.owner, result.OperationID); err != nil || operation.State != domain.OperationSucceeded {
		t.Fatalf("durable reconciliation outcome = %+v, %v", operation, err)
	}
}

func TestDuplicateCatalogMethodsForwardAndFailClosedWhenUnsupported(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	ids := domain.NewIDGenerator(&hashReader{})
	key := secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x77}, 32)))
	wrapped := &reconciliationStorage{NamespaceStorage: env.storage}
	service, err := drive.NewService(wrapped, env.store, env.repo, ids, env.clock, key, "http://127.0.0.1:8080", "http://127.0.0.1:8081", 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.DuplicateGroups(ctx, env.owner, domain.DuplicateGroupRequest{}); err != nil {
		t.Fatalf("DuplicateGroups() error = %v", err)
	}
	if _, err := service.DuplicateOccurrences(ctx, env.owner, domain.DuplicateOccurrenceRequest{}); err != nil {
		t.Fatalf("DuplicateOccurrences() error = %v", err)
	}
	if _, err := service.SetDuplicateIgnored(ctx, env.owner, domain.SetDuplicateIgnoredRequest{}); err != nil {
		t.Fatalf("SetDuplicateIgnored() error = %v", err)
	}
	if _, err := service.CompareDuplicateDirectories(ctx, env.owner, domain.DuplicateDirectoryComparisonRequest{}); err != nil {
		t.Fatalf("CompareDuplicateDirectories() error = %v", err)
	}
	if _, err := service.DuplicateDirectoryOverlaps(ctx, env.owner, domain.DuplicateDirectoryOverlapRequest{}); err != nil {
		t.Fatalf("DuplicateDirectoryOverlaps() error = %v", err)
	}
	if _, err := service.SetDuplicateDirectoryIgnored(ctx, env.owner, domain.SetDuplicateDirectoryIgnoredRequest{}); err != nil {
		t.Fatalf("SetDuplicateDirectoryIgnored() error = %v", err)
	}
	if _, err := service.PreviewDuplicateReconciliation(ctx, env.owner, domain.DuplicateReconciliationPreviewRequest{}); err != nil {
		t.Fatalf("PreviewDuplicateReconciliation() error = %v", err)
	}

	unsupported := env.service
	checks := []func() error{
		func() error {
			_, err := unsupported.DuplicateGroups(ctx, env.owner, domain.DuplicateGroupRequest{})
			return err
		},
		func() error {
			_, err := unsupported.DuplicateOccurrences(ctx, env.owner, domain.DuplicateOccurrenceRequest{})
			return err
		},
		func() error {
			_, err := unsupported.SetDuplicateIgnored(ctx, env.owner, domain.SetDuplicateIgnoredRequest{})
			return err
		},
		func() error {
			_, err := unsupported.CompareDuplicateDirectories(ctx, env.owner, domain.DuplicateDirectoryComparisonRequest{})
			return err
		},
		func() error {
			_, err := unsupported.DuplicateDirectoryOverlaps(ctx, env.owner, domain.DuplicateDirectoryOverlapRequest{})
			return err
		},
		func() error {
			_, err := unsupported.SetDuplicateDirectoryIgnored(ctx, env.owner, domain.SetDuplicateDirectoryIgnoredRequest{})
			return err
		},
		func() error {
			_, err := unsupported.PreviewDuplicateReconciliation(ctx, env.owner, domain.DuplicateReconciliationPreviewRequest{})
			return err
		},
	}
	for index, check := range checks {
		if err := check(); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("unsupported duplicate method %d error = %v", index, err)
		}
	}
}

func TestUploadPlanningMethodsForwardAuthenticatedOwnerAndFailClosedWhenUnsupported(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	ids := domain.NewIDGenerator(&hashReader{})
	key := secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x77}, 32)))
	wrapper := &uploadPlanningStorage{NamespaceStorage: env.storage}
	service, err := drive.NewService(wrapper, env.store, env.repo, ids, env.clock, key, "http://127.0.0.1:8080", "http://127.0.0.1:8081", 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	sizeRequest := domain.UploadSizePlanRequest{Items: []domain.UploadSizePlanItem{{
		ID: "photo-1", Path: domain.MustParseUserPath("/photo.jpg"), Size: 12,
	}}}
	sizePlan, err := service.PlanUploadSizes(ctx, env.owner, sizeRequest)
	if err != nil || sizePlan.Token != "size-plan-token" || len(sizePlan.Items) != 1 || !sizePlan.Items[0].FingerprintRequired {
		t.Fatalf("PlanUploadSizes() = %+v, %v", sizePlan, err)
	}
	if wrapper.sizeOwner != env.owner || len(wrapper.sizeRequest.Items) != 1 || wrapper.sizeRequest.Items[0].Path != sizeRequest.Items[0].Path {
		t.Fatalf("size planning delegation = owner %q request %+v", wrapper.sizeOwner, wrapper.sizeRequest)
	}

	fingerprintRequest := domain.UploadFingerprintPlanRequest{
		Token: "size-plan-token",
		Items: []domain.UploadFingerprintPlanItem{{
			ID: "photo-1", Path: domain.MustParseUserPath("/photo.jpg"), Size: 12,
			MD5: "kAFQmDzST7DWlj99KOF/cg==", CRC32C: "9ONpcA==",
		}},
	}
	fingerprintPlan, err := service.PlanUploadFingerprints(ctx, env.owner, fingerprintRequest)
	if err != nil || len(fingerprintPlan.Items) != 1 || fingerprintPlan.Items[0].Action != domain.UploadPlanSkip {
		t.Fatalf("PlanUploadFingerprints() = %+v, %v", fingerprintPlan, err)
	}
	if wrapper.fingerprintOwner != env.owner || wrapper.fingerprintReq.Token != fingerprintRequest.Token {
		t.Fatalf("fingerprint planning delegation = owner %q request %+v", wrapper.fingerprintOwner, wrapper.fingerprintReq)
	}

	if _, err := env.service.PlanUploadSizes(ctx, env.owner, sizeRequest); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("unsupported PlanUploadSizes() error = %v", err)
	}
	if _, err := env.service.PlanUploadFingerprints(ctx, env.owner, fingerprintRequest); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("unsupported PlanUploadFingerprints() error = %v", err)
	}
}

func TestDuplicateReconciliationRejectsInvalidOrStalePlans(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	ids := domain.NewIDGenerator(&hashReader{})
	key := secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x77}, 32)))
	wrapped := &reconciliationStorage{NamespaceStorage: env.storage}
	service, err := drive.NewService(wrapped, env.store, env.repo, ids, env.clock, key, "http://127.0.0.1:8080", "http://127.0.0.1:8081", 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.ApplyDuplicateReconciliation(ctx, env.owner, "valid-plan-token", "short"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("short idempotency key error = %v", err)
	}
	if _, err := env.service.ApplyDuplicateReconciliation(ctx, env.owner, "valid-plan-token", "duplicate-unsupported-001"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("unsupported storage error = %v", err)
	}
	if _, err := service.ApplyDuplicateReconciliation(ctx, env.owner, "invalid-plan-token", "duplicate-invalid-plan-001"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid plan error = %v", err)
	}
	if _, err := service.ApplyDuplicateReconciliation(ctx, env.owner, "valid-plan-token", "duplicate-empty-plan-0001"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty plan error = %v", err)
	}
	wrapped.selection.Items = make([]domain.DuplicateReconciliationItem, drive.MaxBatchItems+1)
	if _, err := service.ApplyDuplicateReconciliation(ctx, env.owner, "valid-plan-token", "duplicate-large-plan-0001"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized plan error = %v", err)
	}
	wrapped.selection.Items = []domain.DuplicateReconciliationItem{{}}
	if _, err := service.ApplyDuplicateReconciliation(ctx, domain.UserID{}, "valid-plan-token", "duplicate-invalid-owner-01"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid owner error = %v", err)
	}

	remove := upload(t, env, env.owner, "/stale-remove.bin", []byte("remove"), "application/octet-stream", "stale-remove-upload")
	keep := upload(t, env, env.owner, "/stale-keep.bin", []byte("keep"), "application/octet-stream", "stale-keep-upload-01")
	wrapped.selection.Items = []domain.DuplicateReconciliationItem{{
		GroupID: secret.Hash("stale-group"),
		Remove:  domain.DuplicateOccurrence{Path: remove.Path, Version: remove.Version, Size: remove.Size},
		Keep:    domain.DuplicateOccurrence{Path: keep.Path, Version: "stale-version", Size: keep.Size},
	}}
	result, err := service.ApplyDuplicateReconciliation(ctx, env.owner, "valid-plan-token", "duplicate-stale-plan-001")
	if !errors.Is(err, domain.ErrPreconditionFailed) || len(result.Items) != 0 {
		t.Fatalf("stale plan result = %+v, %v", result, err)
	}
	if _, err := service.Stat(ctx, env.owner, remove.Path); err != nil {
		t.Fatalf("stale plan removed source: %v", err)
	}
	wrapped.selection.Items[0].Keep.Path = domain.MustParseUserPath("/missing-keep.bin")
	if result, err := service.ApplyDuplicateReconciliation(ctx, env.owner, "valid-plan-token", "duplicate-missing-keep-01"); !errors.Is(err, domain.ErrNotFound) || len(result.Items) != 0 {
		t.Fatalf("missing keep result = %+v, %v", result, err)
	}
}

func TestIntegrationCopyMoveTrashRestoreAndDelete(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	_, _ = env.service.CreateDirectory(ctx, env.owner, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/tree")})
	upload(t, env, env.owner, "/tree/file.bin", []byte("data"), "application/octet-stream", "upload-tree-file-001")
	copyRequest := domain.CopyRequest{Source: domain.MustParseUserPath("/tree"), Destination: domain.MustParseUserPath("/copy"), IdempotencyKey: "copy-tree-request-001"}
	first, err := env.service.Copy(ctx, env.owner, copyRequest)
	if err != nil || first.State != domain.OperationSucceeded {
		t.Fatalf("Copy = %+v, %v", first, err)
	}
	replayed, err := env.service.Copy(ctx, env.owner, copyRequest)
	if err != nil || replayed.ID != first.ID {
		t.Fatalf("copy replay = %+v, %v", replayed, err)
	}
	move, err := env.service.Move(ctx, env.owner, domain.MoveRequest{Source: domain.MustParseUserPath("/copy"), Destination: domain.MustParseUserPath("/renamed"), IdempotencyKey: "move-tree-request-001"})
	if err != nil || move.State != domain.OperationSucceeded {
		t.Fatalf("Move = %+v, %v", move, err)
	}
	batchCopy, err := env.service.BatchCopyMove(ctx, env.owner, []domain.CopyRequest{{Source: domain.MustParseUserPath("/tree/file.bin"), Destination: domain.MustParseUserPath("/batch-one.bin")}, {Source: domain.MustParseUserPath("/tree/file.bin"), Destination: domain.MustParseUserPath("/batch-two.bin")}}, false, "copy-batch-request-001")
	if err != nil || len(batchCopy.Items) != 2 || batchCopy.Items[0].State != domain.OperationSucceeded || batchCopy.Items[1].State != domain.OperationSucceeded {
		t.Fatalf("BatchCopyMove = %+v, %v", batchCopy, err)
	}
	batch, err := env.service.Trash(ctx, env.owner, []domain.UserPath{domain.MustParseUserPath("/renamed")}, "trash-tree-request-001")
	if err != nil || batch.Items[0].TrashID == "" {
		t.Fatalf("Trash = %+v, %v", batch, err)
	}
	if _, err := env.service.Stat(ctx, env.owner, domain.MustParseUserPath("/renamed")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("trashed stat = %v", err)
	}
	if aggregate, err := env.service.Operation(ctx, env.owner, batch.OperationID); err != nil || aggregate.State != domain.OperationSucceeded {
		t.Fatalf("batch operation = %+v, %v", aggregate, err)
	}
	records, err := env.service.TrashList(ctx, env.owner)
	if err != nil || len(records) != 1 || records[0].OriginalPath.String() != "/renamed" {
		t.Fatalf("TrashList = %+v, %v", records, err)
	}
	_, _ = env.service.CreateDirectory(ctx, env.owner, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/renamed")})
	restored, err := env.service.Restore(ctx, env.owner, records[0].TrashID, domain.ConflictRename, "restore-tree-request-001")
	if err != nil || restored.State != domain.OperationSucceeded {
		t.Fatalf("Restore = %+v, %v", restored, err)
	}
	if replay, err := env.service.Restore(ctx, env.owner, records[0].TrashID, domain.ConflictRename, "restore-tree-request-001"); err != nil || replay.ID != restored.ID {
		t.Fatalf("restore replay = %+v, %v", replay, err)
	}
	if _, err := env.service.Stat(ctx, env.owner, domain.MustParseUserPath("/renamed (1)/file.bin")); err != nil {
		t.Fatalf("renamed restore stat = %v", err)
	}
	batch, err = env.service.Trash(ctx, env.owner, []domain.UserPath{domain.MustParseUserPath("/renamed (1)")}, "trash-again-request-001")
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := env.service.PermanentDelete(ctx, env.owner, batch.Items[0].TrashID, "delete-trash-request-001")
	if err != nil || deleted.State != domain.OperationSucceeded {
		t.Fatalf("PermanentDelete = %+v, %v", deleted, err)
	}
	if replay, err := env.service.PermanentDelete(ctx, env.owner, batch.Items[0].TrashID, "delete-trash-request-001"); err != nil || replay.ID != deleted.ID {
		t.Fatalf("delete replay = %+v, %v", replay, err)
	}
	if _, err := env.service.Operation(ctx, env.other, first.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-user operation = %v", err)
	}
}

func TestIntegrationSharesPreviewAndRevocation(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	for _, path := range []string{"/public", "/public/nested", "/public/nested/deeper"} {
		if _, err := env.service.CreateDirectory(ctx, env.owner, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(path)}); err != nil {
			t.Fatal(err)
		}
	}
	text := upload(t, env, env.owner, "/public/readme.txt", []byte("safe text"), "text/plain", "upload-public-text-01")
	html := upload(t, env, env.owner, "/public/index.html", []byte("<script>x</script>"), "text/html", "upload-public-html-01")
	upload(t, env, env.owner, "/public/nested/deeper/child.txt", []byte("child"), "text/plain", "upload-public-child-01")
	created, err := env.service.CreateShare(ctx, env.owner, domain.MustParseUserPath("/public"), nil, "share-folder-request-01")
	if err != nil {
		t.Fatal(err)
	}
	token := created.Link.Reveal()[len("http://127.0.0.1:8080/s/"):]
	page, err := env.service.PublicShare(ctx, token, "/", 10, "")
	if err != nil || page.Current.Path != "/" || page.Current.Size != int64(len("safe text")+len("<script>x</script>")+len("child")) || page.Current.FileCount != 3 || len(page.Entries) != 3 || page.Entries[0].Path == "/public/readme.txt" {
		t.Fatalf("PublicShare = %+v, %v", page, err)
	}
	var nestedRow drive.PublicEntry
	for _, entry := range page.Entries {
		if entry.Path == "/nested" {
			nestedRow = entry
		}
	}
	if nestedRow.Kind != domain.EntryDirectory || nestedRow.Size != 5 || nestedRow.FileCount != 1 {
		t.Fatalf("public nested child row = %+v; want recursive size/count 5/1", nestedRow)
	}
	nested, err := env.service.PublicShare(ctx, token, "/nested", 10, "")
	if err != nil || nested.Root.Path != "/" || nested.Current.Path != "/nested" || nested.Current.Kind != domain.EntryDirectory || nested.Current.Size != 5 || nested.Current.FileCount != 1 || len(nested.Entries) != 1 || nested.Entries[0].Path != "/nested/deeper" || nested.Entries[0].Size != 5 || nested.Entries[0].FileCount != 1 {
		t.Fatalf("nested PublicShare = %+v, %v", nested, err)
	}
	stat, err := env.service.PublicStat(ctx, token, "/readme.txt")
	if err != nil || stat.Path != "/readme.txt" || stat.Kind != domain.EntryFile || stat.Version != text.Version || stat.Size != int64(len("safe text")) {
		t.Fatalf("public file stat = %+v, %v", stat, err)
	}
	if _, err := env.service.PublicStat(ctx, token, "/../outside"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("public stat traversal = %v", err)
	}
	if _, err := env.service.PublicShare(ctx, token, "/../outside", 10, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("share traversal = %v", err)
	}
	capability, mode, err := env.service.PublicDownload(ctx, token, "/readme.txt", text.Version, true)
	if err != nil || mode != "text" || capability.URL == "" {
		t.Fatalf("public preview = %+v %s, %v", capability, mode, err)
	}
	if _, _, err := env.service.PublicDownload(ctx, token, "/index.html", html.Version, true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("HTML inline preview = %v", err)
	}
	if _, mode, err := env.service.PublicDownload(ctx, token, "/index.html", html.Version, false); err != nil || mode != "download" {
		t.Fatalf("HTML attachment = %s, %v", mode, err)
	}
	if err := env.service.RevokeShare(ctx, env.other, created.Record.ShareID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-user revoke = %v", err)
	}
	if err := env.service.RevokeShare(ctx, env.owner, created.Record.ShareID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.PublicShare(ctx, token, "/", 10, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("revoked share = %v", err)
	}
	if _, err := env.service.PublicStat(ctx, token, "/readme.txt"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("revoked public stat = %v", err)
	}
	created, err = env.service.CreateShare(ctx, env.owner, domain.MustParseUserPath("/public"), nil, "share-folder-request-02")
	if err != nil {
		t.Fatal(err)
	}
	token = created.Link.Reveal()[len("http://127.0.0.1:8080/s/"):]
	if _, err := env.service.PublicShare(ctx, token, "/", 10, ""); err != nil {
		t.Fatalf("fresh replacement share = %v", err)
	}
	expiry := env.clock.Now().Add(time.Minute)
	expiring, err := env.service.CreateShare(ctx, env.owner, domain.MustParseUserPath("/public"), &expiry, "share-expiring-request-1")
	if err != nil {
		t.Fatal(err)
	}
	expiringToken := expiring.Link.Reveal()[len("http://127.0.0.1:8080/s/"):]
	env.clock.Advance(2 * time.Minute)
	if _, err := env.service.PublicShare(ctx, expiringToken, "/", 10, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired share = %v", err)
	}
	movedShare, err := env.service.CreateShare(ctx, env.owner, domain.MustParseUserPath("/public"), nil, "share-moved-request-001")
	if err != nil {
		t.Fatal(err)
	}
	movedToken := movedShare.Link.Reveal()[len("http://127.0.0.1:8080/s/"):]
	if _, err := env.service.Move(ctx, env.owner, domain.MoveRequest{Source: domain.MustParseUserPath("/public"), Destination: domain.MustParseUserPath("/moved"), IdempotencyKey: "move-shared-root-0001"}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.PublicShare(ctx, movedToken, "/", 10, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("moved root share = %v", err)
	}
	_, err = env.service.Move(ctx, env.owner, domain.MoveRequest{Source: domain.MustParseUserPath("/moved"), Destination: domain.MustParseUserPath("/public"), IdempotencyKey: "move-shared-root-0002"})
	if err != nil {
		t.Fatal(err)
	}
	created, err = env.service.CreateShare(ctx, env.owner, domain.MustParseUserPath("/public"), nil, "share-disabled-request-1")
	if err != nil {
		t.Fatal(err)
	}
	token = created.Link.Reveal()[len("http://127.0.0.1:8080/s/"):]
	account, version, _ := env.repo.Account(ctx, env.owner)
	account.Status = model.AccountDisabled
	account.UpdatedAt = env.clock.Now()
	if _, err := env.repo.UpdateAccount(ctx, account, version); err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.PublicShare(ctx, token, "/", 10, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("disabled owner share = %v", err)
	}
}

func TestTrashPageReturnsExactPersistedMetadataWithoutPerRowStats(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	for _, path := range []string{"/tree", "/tree/nested", "/empty"} {
		if _, err := env.service.CreateDirectory(ctx, env.owner, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(path)}); err != nil {
			t.Fatal(err)
		}
	}
	upload(t, env, env.owner, "/tree/nested/child.bin", []byte("1234567"), "application/octet-stream", "trash-meta-child-01")
	upload(t, env, env.owner, "/zero.txt", nil, "text/plain", "trash-meta-zero-001")
	standaloneBody := []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00")
	standalone := upload(t, env, env.owner, "/standalone.jpg", standaloneBody, "image/jpeg", "trash-meta-file-001")
	paths := []domain.UserPath{
		domain.MustParseUserPath("/tree"), domain.MustParseUserPath("/empty"),
		domain.MustParseUserPath("/zero.txt"), standalone.Path,
	}
	if result, err := env.service.Trash(ctx, env.owner, paths, "trash-metadata-batch-001"); err != nil || len(result.Items) != len(paths) {
		t.Fatalf("Trash() = %+v, %v", result, err)
	}
	before := env.storage.Instrumentation()
	page, err := env.service.TrashPage(ctx, env.owner, 1000, "")
	after := env.storage.Instrumentation()
	if err != nil || len(page.Items) != len(paths) || page.NextCursor != "" {
		t.Fatalf("TrashPage() = %+v, %v", page, err)
	}
	if after.ProviderCalls[providermemory.OperationLookupChildren] != before.ProviderCalls[providermemory.OperationLookupChildren] || after.ProviderCalls[providermemory.OperationStat] != before.ProviderCalls[providermemory.OperationStat] {
		t.Fatalf("TrashPage provider calls before=%v after=%v; want authoritative namespace metadata with no legacy lookup or Stat", before.ProviderCalls, after.ProviderCalls)
	}
	items := make(map[string]drive.TrashEntry, len(page.Items))
	for _, item := range page.Items {
		items[item.OriginalPath.String()] = item
	}
	for _, test := range []struct {
		path, mediaType string
		kind            domain.EntryKind
		size            int64
	}{
		{path: "/tree", kind: domain.EntryDirectory, size: 7},
		{path: "/empty", kind: domain.EntryDirectory, size: 0},
		{path: "/zero.txt", kind: domain.EntryFile, size: 0, mediaType: "text/plain"},
		{path: "/standalone.jpg", kind: domain.EntryFile, size: int64(len(standaloneBody)), mediaType: "image/jpeg"},
	} {
		item, ok := items[test.path]
		wantCount := int64(1)
		if test.path == "/empty" {
			wantCount = 0
		}
		if !ok || item.Kind != test.kind || item.Size != test.size || item.FileCount != wantCount || item.MediaType != test.mediaType {
			t.Errorf("trash metadata %s = %+v, present=%t", test.path, item, ok)
		}
	}
	otherPage, err := env.service.TrashPage(ctx, env.other, 1000, "")
	if err != nil || len(otherPage.Items) != 0 {
		t.Fatalf("cross-owner TrashPage() = %+v, %v", otherPage, err)
	}
}

func TestSafePreviewAllowlistUsesProviderValidatedMedia(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	cases := []struct {
		name, mediaType, mode string
		body                  []byte
	}{
		{"png", "image/png", "image", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")},
		{"jpeg", "image/jpeg", "image", []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00")},
		{"gif", "image/gif", "image", []byte("GIF89a\x01\x00\x01\x00")},
		{"webp", "image/webp", "image", []byte("RIFF\x08\x00\x00\x00WEBPVP8 ")},
		{"pdf", "application/pdf", "pdf", []byte("%PDF-1.7\n%%EOF")},
		{"text", "text/plain", "text", []byte("plain UTF-8 text")},
	}
	for index, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			entry := upload(t, env, env.owner, "/"+testCase.name, testCase.body, testCase.mediaType, "preview-upload-key-00"+string(rune('a'+index)))
			_, mode, err := env.service.Download(ctx, env.owner, domain.CreateDownloadRequest{Path: entry.Path, Version: entry.Version}, true)
			if err != nil || mode != testCase.mode {
				t.Fatalf("preview = %s, %v entry=%+v", mode, err, entry)
			}
		})
	}
	hostile := upload(t, env, env.owner, "/fake.png", []byte("<script>alert(1)</script>"), "image/png", "preview-hostile-key-001")
	if hostile.MediaType != "application/octet-stream" {
		t.Fatalf("hostile media type = %q", hostile.MediaType)
	}
	if _, _, err := env.service.Download(ctx, env.owner, domain.CreateDownloadRequest{Path: hostile.Path, Version: hostile.Version}, true); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("hostile inline preview = %v", err)
	}
}

func TestServiceBoundaryAndInvalidScopeMatrix(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	if env.service.DataOrigin() == "" {
		t.Fatal("data origin is empty")
	}
	var invalidUser domain.UserID
	path := domain.MustParseUserPath("/boundary")
	uploadID := domain.UploadID("missing")
	operationID := domain.OperationID("missing")
	checks := []func() error{
		func() error {
			_, err := env.service.List(ctx, invalidUser, domain.ListRequest{Directory: domain.MustParseUserPath("/")})
			return err
		},
		func() error { _, err := env.service.Stat(ctx, invalidUser, path); return err },
		func() error {
			_, err := env.service.CreateDirectory(ctx, invalidUser, domain.CreateDirectoryRequest{Path: path})
			return err
		},
		func() error {
			_, err := env.service.CreateUpload(ctx, invalidUser, domain.CreateUploadRequest{Path: path, IdempotencyKey: "valid-boundary-key-001"})
			return err
		},
		func() error { _, err := env.service.UploadStatus(ctx, invalidUser, uploadID); return err },
		func() error {
			_, err := env.service.CompleteUpload(ctx, invalidUser, domain.CompleteUploadRequest{UploadID: uploadID, Path: path})
			return err
		},
		func() error { return env.service.AbortUpload(ctx, invalidUser, uploadID) },
		func() error {
			_, _, err := env.service.Download(ctx, invalidUser, domain.CreateDownloadRequest{Path: path, Version: "v1"}, false)
			return err
		},
		func() error {
			_, err := env.service.Copy(ctx, invalidUser, domain.CopyRequest{Source: path, Destination: domain.MustParseUserPath("/copy"), IdempotencyKey: "valid-boundary-key-002"})
			return err
		},
		func() error {
			_, err := env.service.Move(ctx, invalidUser, domain.MoveRequest{Source: path, Destination: domain.MustParseUserPath("/move"), IdempotencyKey: "valid-boundary-key-003"})
			return err
		},
		func() error {
			_, err := env.service.Trash(ctx, invalidUser, []domain.UserPath{path}, "valid-boundary-key-004")
			return err
		},
		func() error { _, err := env.service.Operation(ctx, invalidUser, operationID); return err },
	}
	for index, check := range checks {
		if err := check(); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid scope check %d = %v", index, err)
		}
	}
	for _, key := range []string{"", "short", strings.Repeat("x", 129), "valid-key-value\n"} {
		if _, err := env.service.CreateUpload(ctx, env.owner, domain.CreateUploadRequest{Path: path, IdempotencyKey: key}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("idempotency key %q = %v", key, err)
		}
	}
	if _, err := drive.NewService(nil, statememory.NewMemoryStore(), env.repo, nil, env.clock, "invalid", "", "", 0); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid service = %v", err)
	}
}

func TestUploadAbortBatchMoveTrashPagingAndEmptyTrash(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	capability, err := env.service.CreateUpload(ctx, env.owner, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/abort.bin"), Size: 1, MediaType: "application/octet-stream", IdempotencyKey: "abort-upload-key-0001"})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.service.AbortUpload(ctx, env.owner, capability.UploadID); err != nil {
		t.Fatal(err)
	}
	status, err := env.service.UploadStatus(ctx, env.owner, capability.UploadID)
	if err != nil || status.State != domain.UploadStateAborted {
		t.Fatalf("aborted upload status = %+v, %v", status, err)
	}
	source := upload(t, env, env.owner, "/source.txt", []byte("source"), "text/plain", "boundary-source-upload-1")
	if _, _, err := env.service.Download(ctx, env.owner, domain.CreateDownloadRequest{Path: source.Path, Version: "stale"}, false); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("stale download = %v", err)
	}
	if _, mode, err := env.service.Download(ctx, env.owner, domain.CreateDownloadRequest{Path: source.Path, Version: source.Version}, false); err != nil || mode != "download" {
		t.Fatalf("attachment download = %q, %v", mode, err)
	}
	batch, err := env.service.BatchCopyMove(ctx, env.owner, []domain.CopyRequest{
		{Source: source.Path, Destination: domain.MustParseUserPath("/moved.txt")},
		{Source: domain.MustParseUserPath("/missing.txt"), Destination: domain.MustParseUserPath("/never.txt")},
	}, true, "boundary-move-batch-01")
	if !errors.Is(err, domain.ErrNotFound) || len(batch.Items) != 0 {
		t.Fatalf("atomic move batch = %+v, %v", batch, err)
	}
	if current, statErr := env.service.Stat(ctx, env.owner, source.Path); statErr != nil || current.Version != source.Version {
		t.Fatalf("failed atomic batch changed source = %+v, %v", current, statErr)
	}
	if _, err := env.service.BatchCopyMove(ctx, env.owner, nil, false, "boundary-empty-batch-1"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty copy batch = %v", err)
	}
	if _, err := env.service.Trash(ctx, env.owner, nil, "boundary-empty-trash-1"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty trash batch = %v", err)
	}
	if _, err := env.service.Trash(ctx, env.owner, []domain.UserPath{domain.MustParseUserPath("/")}, "boundary-root-trash-01"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("root trash = %v", err)
	}
	duplicate := domain.MustParseUserPath("/moved.txt")
	if _, err := env.service.Trash(ctx, env.owner, []domain.UserPath{duplicate, duplicate}, "boundary-duplicate-01"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("duplicate trash = %v", err)
	}
	first := upload(t, env, env.owner, "/trash-one.txt", []byte("one"), "text/plain", "trash-one-upload-key-1")
	second := upload(t, env, env.owner, "/trash-two.txt", []byte("two"), "text/plain", "trash-two-upload-key-1")
	trashed, err := env.service.Trash(ctx, env.owner, []domain.UserPath{first.Path, second.Path}, "boundary-trash-batch-1")
	if err != nil || len(trashed.Items) != 2 {
		t.Fatalf("trash batch = %+v, %v", trashed, err)
	}
	page, err := env.service.TrashPage(ctx, env.owner, 1, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].MediaType != "text/plain" || page.Items[0].Size != 3 || page.NextCursor == "" {
		t.Fatalf("trash page = %+v, %v", page, err)
	}
	page, err = env.service.TrashPage(ctx, env.owner, 1, page.NextCursor)
	if err != nil || len(page.Items) != 1 || page.NextCursor != "" {
		t.Fatalf("trash final page = %+v, %v", page, err)
	}
	if _, err := env.service.TrashPage(ctx, env.owner, -1, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid trash page = %v", err)
	}
	if _, err := env.service.EmptyTrash(ctx, env.owner, false, "boundary-empty-trash-2"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("unconfirmed empty trash = %v", err)
	}
	emptied, err := env.service.EmptyTrash(ctx, env.owner, true, "boundary-empty-trash-3")
	if err != nil || len(emptied.Items) != 2 {
		t.Fatalf("empty trash = %+v, %v", emptied, err)
	}
	if records, err := env.service.TrashList(ctx, env.owner); err != nil || len(records) != 0 {
		t.Fatalf("trash after empty = %+v, %v", records, err)
	}
}

func TestTrashBatchRestoreAndPermanentDeleteAreAtomic(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	first := upload(t, env, env.owner, "/batch-restore-one.txt", []byte("one"), "text/plain", "batch-restore-one-upload")
	second := upload(t, env, env.owner, "/batch-restore-two.txt", []byte("two"), "text/plain", "batch-restore-two-upload")
	trashed, err := env.service.Trash(ctx, env.owner, []domain.UserPath{first.Path, second.Path}, "batch-restore-trash-request")
	if err != nil || len(trashed.Items) != 2 {
		t.Fatalf("Trash() = %+v, %v", trashed, err)
	}
	trashIDs := []string{trashed.Items[0].TrashID, trashed.Items[1].TrashID}
	restored, err := env.service.RestoreBatch(ctx, env.owner, trashIDs, domain.ConflictFail, "batch-restore-request-001")
	if err != nil || len(restored.Items) != 2 {
		t.Fatalf("RestoreBatch() = %+v, %v", restored, err)
	}
	for _, entry := range []domain.Entry{first, second} {
		if current, statErr := env.service.Stat(ctx, env.owner, entry.Path); statErr != nil || current.Size != entry.Size {
			t.Fatalf("restored %s = %+v, %v", entry.Path, current, statErr)
		}
	}
	if replay, replayErr := env.service.RestoreBatch(ctx, env.owner, trashIDs, domain.ConflictFail, "batch-restore-request-001"); replayErr != nil || replay.OperationID != restored.OperationID {
		t.Fatalf("RestoreBatch(replay) = %+v, %v", replay, replayErr)
	}
	if _, conflictErr := env.service.RestoreBatch(ctx, env.owner, trashIDs, domain.ConflictRename, "batch-restore-request-001"); !errors.Is(conflictErr, domain.ErrConflict) {
		t.Fatalf("RestoreBatch(changed replay) error = %v", conflictErr)
	}

	trashed, err = env.service.Trash(ctx, env.owner, []domain.UserPath{first.Path, second.Path}, "batch-delete-trash-request")
	if err != nil || len(trashed.Items) != 2 {
		t.Fatalf("Trash(for delete) = %+v, %v", trashed, err)
	}
	trashIDs = []string{trashed.Items[0].TrashID, trashed.Items[1].TrashID}
	deleted, err := env.service.PermanentDeleteBatch(ctx, env.owner, trashIDs, "batch-delete-request-001")
	if err != nil || len(deleted.Items) != 2 {
		t.Fatalf("PermanentDeleteBatch() = %+v, %v", deleted, err)
	}
	if records, listErr := env.service.TrashList(ctx, env.owner); listErr != nil || len(records) != 0 {
		t.Fatalf("trash after permanent delete = %+v, %v", records, listErr)
	}
	if _, invalidErr := env.service.RestoreBatch(ctx, env.owner, nil, domain.ConflictFail, "batch-restore-invalid-1"); !errors.Is(invalidErr, domain.ErrInvalid) {
		t.Fatalf("RestoreBatch(empty) error = %v", invalidErr)
	}
	if _, invalidErr := env.service.PermanentDeleteBatch(ctx, env.owner, nil, "batch-delete-invalid-1"); !errors.Is(invalidErr, domain.ErrInvalid) {
		t.Fatalf("PermanentDeleteBatch(empty) error = %v", invalidErr)
	}
}

func TestDriveServiceRejectsStorageWithoutAtomicNamespaceExtensions(t *testing.T) {
	env := newDriveEnvironment(t)
	key := secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x77}, 32)))
	if _, err := drive.NewService(baseStorageOnly{Storage: env.storage}, env.store, env.repo, domain.NewIDGenerator(&hashReader{}), env.clock, key, "http://127.0.0.1:8080", "http://127.0.0.1:8081", 1<<20); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("storage without schema-008 extensions error = %v", err)
	}
}

func TestShareIdempotencyFileRootAndPublicFailureMatrix(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	entry := upload(t, env, env.owner, "/shared.txt", []byte("public"), "text/plain", "share-file-upload-key-1")
	if _, err := env.service.CreateShare(ctx, env.owner, entry.Path, nil, "short"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("short share key = %v", err)
	}
	past := env.clock.Now()
	if _, err := env.service.CreateShare(ctx, env.owner, entry.Path, &past, "share-expired-key-001"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("expired share = %v", err)
	}
	expiry := env.clock.Now().Add(time.Hour)
	created, err := env.service.CreateShare(ctx, env.owner, entry.Path, &expiry, "share-file-key-00001")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := env.service.CreateShare(ctx, env.owner, entry.Path, &expiry, "share-file-key-00001")
	if err != nil || replayed.Record.ShareID != created.Record.ShareID {
		t.Fatalf("share replay = %+v, %v", replayed, err)
	}
	if _, err := env.service.CreateShare(ctx, env.owner, domain.MustParseUserPath("/missing"), nil, "share-file-key-00001"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("share key conflict = %v", err)
	}
	shares, err := env.service.Shares(ctx, env.owner)
	if err != nil || len(shares) != 1 {
		t.Fatalf("shares = %+v, %v", shares, err)
	}
	token := strings.TrimPrefix(created.Link.Reveal(), "http://127.0.0.1:8080/s/")
	page, err := env.service.PublicShare(ctx, token, "", 10, "")
	if err != nil || page.Root.Path != "/" || page.Root.Kind != domain.EntryFile || page.Current != page.Root || page.Current.Size != int64(len("public")) || page.Current.FileCount != 1 {
		t.Fatalf("public file root = %+v, %v", page, err)
	}
	if _, err := env.service.PublicShare(ctx, token, "/child", 10, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("file-root child = %v", err)
	}
	for _, relative := range []string{"%2e%2e", `\\escape`} {
		if _, err := env.service.PublicShare(ctx, token, relative, 10, ""); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("public relative %q = %v", relative, err)
		}
	}
	for _, test := range []struct {
		token   string
		version domain.Version
	}{
		{"bad", entry.Version},
		{token, ""},
		{token, "stale"},
	} {
		if _, _, err := env.service.PublicDownload(ctx, test.token, "", test.version, false); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("public download failure = %v", err)
		}
	}
	if err := env.service.RevokeShare(ctx, env.owner, created.Record.ShareID); err != nil {
		t.Fatal(err)
	}
	if err := env.service.RevokeShare(ctx, env.owner, created.Record.ShareID); err != nil {
		t.Fatalf("idempotent revoke = %v", err)
	}
}

func TestDriveMutationAndProviderFaultMatrix(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	path := domain.MustParseUserPath("/fault.txt")
	invalidKeyCalls := []func() error{
		func() error {
			_, err := env.service.Copy(ctx, env.owner, domain.CopyRequest{IdempotencyKey: "short"})
			return err
		},
		func() error {
			_, err := env.service.Move(ctx, env.owner, domain.MoveRequest{IdempotencyKey: "short"})
			return err
		},
		func() error {
			_, err := env.service.BatchCopyMove(ctx, env.owner, []domain.CopyRequest{{}}, false, "short")
			return err
		},
		func() error {
			_, err := env.service.Trash(ctx, env.owner, []domain.UserPath{path}, "short")
			return err
		},
		func() error {
			_, err := env.service.Restore(ctx, env.owner, "missing", domain.ConflictFail, "short")
			return err
		},
		func() error { _, err := env.service.PermanentDelete(ctx, env.owner, "missing", "short"); return err },
		func() error { _, err := env.service.EmptyTrash(ctx, env.owner, true, "short"); return err },
	}
	for index, call := range invalidKeyCalls {
		if err := call(); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid mutation key %d = %v", index, err)
		}
	}
	missingBatch, err := env.service.Trash(ctx, env.owner, []domain.UserPath{domain.MustParseUserPath("/missing.txt")}, "trash-missing-item-001")
	if !errors.Is(err, domain.ErrNotFound) || len(missingBatch.Items) != 0 {
		t.Fatalf("missing trash item = %+v, %v", missingBatch, err)
	}
	replayEntry := upload(t, env, env.owner, "/replay.txt", []byte("replay"), "text/plain", "trash-replay-upload-01")
	first, err := env.service.Trash(ctx, env.owner, []domain.UserPath{replayEntry.Path}, "trash-replay-key-0001")
	if err != nil || first.Items[0].TrashID == "" {
		t.Fatalf("first replay trash = %+v, %v", first, err)
	}
	replay, err := env.service.Trash(ctx, env.owner, []domain.UserPath{replayEntry.Path}, "trash-replay-key-0001")
	if err != nil || replay.Items[0].TrashID != first.Items[0].TrashID {
		t.Fatalf("trash replay = %+v, %v", replay, err)
	}
	conflict, err := env.service.Trash(ctx, env.owner, []domain.UserPath{domain.MustParseUserPath("/different.txt")}, "trash-replay-key-0001")
	if !errors.Is(err, domain.ErrConflict) || len(conflict.Items) != 0 {
		t.Fatalf("trash replay conflict = %+v, %v", conflict, err)
	}
	if _, err := env.service.Restore(ctx, env.owner, first.Items[0].TrashID, domain.ConflictFail, "restore-fault-key-0001"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.Restore(ctx, env.owner, first.Items[0].TrashID, domain.ConflictRename, "restore-fault-key-0001"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("restore replay conflict = %v", err)
	}

	statEntry := upload(t, env, env.owner, "/restore-stat.txt", []byte("x"), "text/plain", "restore-stat-upload-01")
	statTrash, err := env.service.Trash(ctx, env.owner, []domain.UserPath{statEntry.Path}, "restore-stat-trash-001")
	if err != nil {
		t.Fatal(err)
	}
	env.storage.InjectFault(providermemory.OperationMove, providermemory.FaultUnavailable)
	if _, err := env.service.Restore(ctx, env.owner, statTrash.Items[0].TrashID, domain.ConflictFail, "restore-move-fault-01"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("restore move fault = %v", err)
	}
	deleteEntry := upload(t, env, env.owner, "/delete-fault.txt", []byte("x"), "text/plain", "delete-fault-upload-01")
	deleteTrash, err := env.service.Trash(ctx, env.owner, []domain.UserPath{deleteEntry.Path}, "delete-fault-trash-001")
	if err != nil {
		t.Fatal(err)
	}
	env.storage.InjectFault(providermemory.OperationDelete, providermemory.FaultUnavailable)
	if _, err := env.service.PermanentDelete(ctx, env.owner, deleteTrash.Items[0].TrashID, "delete-data-fault-001"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("delete provider fault = %v", err)
	}
}

func TestPublicShareProviderFailureMatrix(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	_, _ = env.service.CreateDirectory(ctx, env.owner, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/fault-public")})
	entry := upload(t, env, env.owner, "/fault-public/file.txt", []byte("public"), "text/plain", "public-fault-upload-01")
	created, err := env.service.CreateShare(ctx, env.owner, domain.MustParseUserPath("/fault-public"), nil, "public-fault-share-001")
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := env.service.CreateShare(ctx, env.owner, domain.MustParseUserPath("/fault-public"), nil, "public-fault-share-001"); err != nil || replay.Record.ShareID != created.Record.ShareID {
		t.Fatalf("nil-expiry share replay = %+v, %v", replay, err)
	}
	if _, err := env.service.CreateShare(ctx, env.owner, domain.MustParseUserPath("/missing"), nil, "public-missing-share-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("share missing root = %v", err)
	}
	token := strings.TrimPrefix(created.Link.Reveal(), "http://127.0.0.1:8080/s/")
	validMissingToken := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x44}, 32))
	if _, err := env.service.PublicShare(ctx, validMissingToken, "", 10, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown valid token = %v", err)
	}
	env.storage.InjectFault(providermemory.OperationStat, providermemory.FaultUnavailable)
	if _, err := env.service.PublicShare(ctx, token, "", 10, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("public stat fault = %v", err)
	}
	env.storage.InjectFault(providermemory.OperationList, providermemory.FaultUnavailable)
	if _, err := env.service.PublicShare(ctx, token, "", 10, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("public list fault = %v", err)
	}
	env.storage.InjectFault(providermemory.OperationCreateDownload, providermemory.FaultUnavailable)
	if _, _, err := env.service.PublicDownload(ctx, token, "/file.txt", entry.Version, false); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("public download provider fault = %v", err)
	}
}

func TestDriveCancellationAndLateFailureMatrix(t *testing.T) {
	env := newDriveEnvironment(t)
	ctx := context.Background()
	if _, _, err := env.service.Download(ctx, env.owner, domain.CreateDownloadRequest{Path: domain.MustParseUserPath("/missing"), Version: "v1"}, false); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing download = %v", err)
	}
	source := upload(t, env, env.owner, "/cancel-source.txt", []byte("x"), "text/plain", "cancel-source-upload-1")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := env.service.BatchCopyMove(canceled, env.owner, []domain.CopyRequest{{Source: source.Path, Destination: domain.MustParseUserPath("/cancel-copy.txt")}}, false, "cancel-copy-batch-001"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled copy batch = %v", err)
	}
	if _, err := env.service.Trash(canceled, env.owner, []domain.UserPath{source.Path}, "cancel-trash-batch-01"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled trash batch = %v", err)
	}
	if _, err := env.service.TrashList(canceled, env.owner); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled trash list = %v", err)
	}
	if _, err := env.service.TrashPage(canceled, env.owner, 0, ""); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled trash page = %v", err)
	}
	if _, err := env.service.Restore(canceled, env.owner, "missing", domain.ConflictFail, "cancel-restore-key-001"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled restore = %v", err)
	}
	if _, err := env.service.PermanentDelete(canceled, env.owner, "missing", "cancel-delete-key-0001"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled permanent delete = %v", err)
	}
	if _, err := env.service.EmptyTrash(canceled, env.owner, true, "cancel-empty-trash-001"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled empty trash = %v", err)
	}
	if _, err := env.service.CreateShare(canceled, env.owner, source.Path, nil, "cancel-share-key-00001"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled create share = %v", err)
	}
}
