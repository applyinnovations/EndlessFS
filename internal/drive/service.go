package drive

import (
	"context"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/provider"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
)

const MaxBatchItems = 100

type AccountReader interface {
	Account(context.Context, domain.UserID) (model.Account, state.Version, error)
}

type Service struct {
	storage        provider.Storage
	repository     *repository
	accounts       AccountReader
	ids            *domain.IDGenerator
	clock          domain.Clock
	tokenKey       secret.Value
	baseURL        string
	dataOrigin     string
	textPreviewMax int64
}

func NewService(storage provider.Storage, store state.Store, accounts AccountReader, ids *domain.IDGenerator, clock domain.Clock, tokenKey secret.Value, baseURL, dataOrigin string, textPreviewMax int64) (*Service, error) {
	if storage == nil || store == nil || accounts == nil || ids == nil || clock == nil || !secret.ValidBearerToken(tokenKey.Reveal()) || baseURL == "" || dataOrigin == "" || textPreviewMax < 1 {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid drive service configuration")
	}
	return &Service{storage: storage, repository: newRepository(store), accounts: accounts, ids: ids, clock: clock, tokenKey: tokenKey, baseURL: strings.TrimRight(baseURL, "/"), dataOrigin: strings.TrimRight(dataOrigin, "/"), textPreviewMax: textPreviewMax}, nil
}

func (s *Service) DataOrigin() string { return s.dataOrigin }

func liveScope(userID domain.UserID) (domain.Scope, error) {
	return domain.NewScope(userID, domain.AreaLive)
}
func trashScope(userID domain.UserID) (domain.Scope, error) {
	return domain.NewScope(userID, domain.AreaTrash)
}

func (s *Service) List(ctx context.Context, userID domain.UserID, request domain.ListRequest) (domain.ListPage, error) {
	scope, err := liveScope(userID)
	if err != nil {
		return domain.ListPage{}, err
	}
	return s.storage.List(ctx, scope, request)
}

func (s *Service) Stat(ctx context.Context, userID domain.UserID, path domain.UserPath) (domain.Entry, error) {
	scope, err := liveScope(userID)
	if err != nil {
		return domain.Entry{}, err
	}
	return s.storage.Stat(ctx, scope, path)
}

func (s *Service) duplicateStorage() (provider.DuplicateStorage, error) {
	storage, ok := s.storage.(provider.DuplicateStorage)
	if !ok {
		return nil, domain.NewError(domain.ErrorUnavailable, "duplicate catalog is not available")
	}
	return storage, nil
}

func (s *Service) DuplicateGroups(ctx context.Context, userID domain.UserID, request domain.DuplicateGroupRequest) (domain.DuplicateGroupPage, error) {
	storage, err := s.duplicateStorage()
	if err != nil {
		return domain.DuplicateGroupPage{}, err
	}
	return storage.ListDuplicateGroups(ctx, userID, request)
}

func (s *Service) DuplicateOccurrences(ctx context.Context, userID domain.UserID, request domain.DuplicateOccurrenceRequest) (domain.DuplicateOccurrencePage, error) {
	storage, err := s.duplicateStorage()
	if err != nil {
		return domain.DuplicateOccurrencePage{}, err
	}
	return storage.ListDuplicateOccurrences(ctx, userID, request)
}

func (s *Service) SetDuplicateIgnored(ctx context.Context, userID domain.UserID, request domain.SetDuplicateIgnoredRequest) (domain.DuplicateIgnore, error) {
	storage, err := s.duplicateStorage()
	if err != nil {
		return domain.DuplicateIgnore{}, err
	}
	return storage.SetDuplicateGroupIgnored(ctx, userID, request)
}

func (s *Service) CompareDuplicateDirectories(ctx context.Context, userID domain.UserID, request domain.DuplicateDirectoryComparisonRequest) (domain.DuplicateDirectoryComparison, error) {
	storage, err := s.duplicateStorage()
	if err != nil {
		return domain.DuplicateDirectoryComparison{}, err
	}
	return storage.CompareDuplicateDirectories(ctx, userID, request)
}

func (s *Service) DuplicateDirectoryOverlaps(ctx context.Context, userID domain.UserID, request domain.DuplicateDirectoryOverlapRequest) (domain.DuplicateDirectoryOverlapPage, error) {
	storage, err := s.duplicateStorage()
	if err != nil {
		return domain.DuplicateDirectoryOverlapPage{}, err
	}
	return storage.ListDuplicateDirectoryOverlaps(ctx, userID, request)
}

func (s *Service) SetDuplicateDirectoryIgnored(ctx context.Context, userID domain.UserID, request domain.SetDuplicateDirectoryIgnoredRequest) (domain.DuplicateDirectoryIgnore, error) {
	storage, err := s.duplicateStorage()
	if err != nil {
		return domain.DuplicateDirectoryIgnore{}, err
	}
	return storage.SetDuplicateDirectoryIgnored(ctx, userID, request)
}

func (s *Service) PreviewDuplicateReconciliation(ctx context.Context, userID domain.UserID, request domain.DuplicateReconciliationPreviewRequest) (domain.DuplicateReconciliationPreview, error) {
	storage, err := s.duplicateStorage()
	if err != nil {
		return domain.DuplicateReconciliationPreview{}, err
	}
	return storage.PreviewDuplicateReconciliation(ctx, userID, request)
}

func (s *Service) ApplyDuplicateReconciliation(ctx context.Context, userID domain.UserID, planToken, idempotencyKey string) (BatchResult, error) {
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return BatchResult{}, err
	}
	storage, err := s.duplicateStorage()
	if err != nil {
		return BatchResult{}, err
	}
	selection, err := storage.ValidateDuplicateReconciliation(ctx, userID, planToken)
	if err != nil {
		return BatchResult{}, err
	}
	if len(selection.Items) < 1 || len(selection.Items) > MaxBatchItems {
		return BatchResult{}, domain.NewError(domain.ErrorInvalid, "duplicate reconciliation plan has no bounded selection")
	}
	live, err := liveScope(userID)
	if err != nil {
		return BatchResult{}, err
	}
	trash, err := trashScope(userID)
	if err != nil {
		return BatchResult{}, err
	}
	result := BatchResult{OperationID: domain.OperationID(s.derivedID("duplicate-reconciliation", userID, idempotencyKey)), Items: make([]ItemResult, 0, len(selection.Items))}
	for index, planned := range selection.Items {
		keep, keepErr := s.storage.Stat(ctx, live, planned.Keep.Path)
		if keepErr != nil || keep.Version != planned.Keep.Version {
			if keepErr == nil {
				keepErr = domain.NewError(domain.ErrorPreconditionFailed, "duplicate keep occurrence changed")
			}
			result.Items = append(result.Items, ItemResult{Path: planned.Remove.Path, State: domain.OperationFailed, ErrorKind: domain.KindOf(keepErr)})
			continue
		}
		item, itemErr := s.trashOneExpected(ctx, userID, live, trash, planned.Remove.Path, planned.Remove.Version, idempotencyKey+":"+strconv.Itoa(index))
		if itemErr != nil {
			item = ItemResult{Path: planned.Remove.Path, State: domain.OperationFailed, ErrorKind: domain.KindOf(itemErr)}
		}
		result.Items = append(result.Items, item)
	}
	if err := s.recordDuplicateReconciliationAudit(ctx, userID, planToken, selection, result); err != nil {
		return BatchResult{}, err
	}
	return result, nil
}

func (s *Service) recordDuplicateReconciliationAudit(ctx context.Context, userID domain.UserID, planToken string, selection domain.DuplicateReconciliationSelection, result BatchResult) error {
	stateValue := domain.OperationSucceeded
	succeeded := 0
	reclaimable := int64(0)
	for index, item := range result.Items {
		if item.State != domain.OperationSucceeded {
			stateValue = domain.OperationFailed
			continue
		}
		succeeded++
		if index < len(selection.Items) && selection.Items[index].Remove.Size <= math.MaxInt64-reclaimable {
			reclaimable += selection.Items[index].Remove.Size
		}
	}
	now := s.clock.Now()
	record := model.BatchOperation{
		SchemaVersion: model.SchemaVersion, OwnerUserID: userID, OperationID: result.OperationID,
		Kind: "duplicate_reconciliation", RequestDigest: secret.Hash(planToken), ItemCount: len(result.Items),
		SucceededCount: succeeded, ReclaimableBytes: reclaimable, State: stateValue, StartedAt: now, UpdatedAt: now,
	}
	return createOrMatch(s.repository.createBatchOperation(ctx, record))
}

func (s *Service) CreateDirectory(ctx context.Context, userID domain.UserID, request domain.CreateDirectoryRequest) (domain.Entry, error) {
	scope, err := liveScope(userID)
	if err != nil {
		return domain.Entry{}, err
	}
	return s.storage.CreateDirectory(ctx, scope, request)
}

func validateIdempotencyKey(key string) error {
	if len(key) < 16 || len(key) > 128 || !utf8.ValidString(key) || strings.ContainsAny(key, "\r\n\x00") {
		return domain.NewError(domain.ErrorInvalid, "a valid Idempotency-Key is required")
	}
	return nil
}

func (s *Service) CreateUpload(ctx context.Context, userID domain.UserID, request domain.CreateUploadRequest) (domain.UploadCapability, error) {
	if err := validateIdempotencyKey(request.IdempotencyKey); err != nil {
		return domain.UploadCapability{}, err
	}
	scope, err := liveScope(userID)
	if err != nil {
		return domain.UploadCapability{}, err
	}
	return s.storage.CreateUpload(ctx, scope, request)
}

func (s *Service) UploadStatus(ctx context.Context, userID domain.UserID, uploadID domain.UploadID) (domain.UploadStatus, error) {
	scope, err := liveScope(userID)
	if err != nil {
		return domain.UploadStatus{}, err
	}
	return s.storage.UploadStatus(ctx, scope, uploadID)
}

func (s *Service) CompleteUpload(ctx context.Context, userID domain.UserID, request domain.CompleteUploadRequest) (domain.Entry, error) {
	scope, err := liveScope(userID)
	if err != nil {
		return domain.Entry{}, err
	}
	return s.storage.CompleteUpload(ctx, scope, request)
}

func (s *Service) AbortUpload(ctx context.Context, userID domain.UserID, uploadID domain.UploadID) error {
	scope, err := liveScope(userID)
	if err != nil {
		return err
	}
	return s.storage.AbortUpload(ctx, scope, uploadID)
}

func (s *Service) Download(ctx context.Context, userID domain.UserID, request domain.CreateDownloadRequest, preview bool) (domain.DownloadCapability, string, error) {
	scope, err := liveScope(userID)
	if err != nil {
		return domain.DownloadCapability{}, "", err
	}
	entry, err := s.storage.Stat(ctx, scope, request.Path)
	if err != nil {
		return domain.DownloadCapability{}, "", err
	}
	if entry.Kind != domain.EntryFile || request.Version == "" || entry.Version != request.Version {
		return domain.DownloadCapability{}, "", domain.NewError(domain.ErrorPreconditionFailed, "download version does not match")
	}
	previewKind := "download"
	request.Disposition = domain.DispositionAttachment
	if preview {
		previewKind, err = s.previewKind(entry)
		if err != nil {
			return domain.DownloadCapability{}, "", err
		}
		request.Disposition = domain.DispositionInline
	}
	capability, err := s.storage.CreateDownload(ctx, scope, request)
	return capability, previewKind, err
}

func (s *Service) previewKind(entry domain.Entry) (string, error) {
	switch entry.MediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return "image", nil
	case "application/pdf":
		return "pdf", nil
	case "text/plain":
		if entry.Size <= s.textPreviewMax {
			return "text", nil
		}
	}
	return "", domain.NewError(domain.ErrorInvalid, "this file type is download-only")
}

func (s *Service) Copy(ctx context.Context, userID domain.UserID, request domain.CopyRequest) (domain.Operation, error) {
	if err := validateIdempotencyKey(request.IdempotencyKey); err != nil {
		return domain.Operation{}, err
	}
	scope, err := liveScope(userID)
	if err != nil {
		return domain.Operation{}, err
	}
	return s.storage.Copy(ctx, scope, scope, request)
}

func (s *Service) Move(ctx context.Context, userID domain.UserID, request domain.MoveRequest) (domain.Operation, error) {
	if err := validateIdempotencyKey(request.IdempotencyKey); err != nil {
		return domain.Operation{}, err
	}
	scope, err := liveScope(userID)
	if err != nil {
		return domain.Operation{}, err
	}
	return s.storage.Move(ctx, scope, scope, request)
}

func (s *Service) BatchCopyMove(ctx context.Context, userID domain.UserID, requests []domain.CopyRequest, move bool, idempotencyKey string) (BatchResult, error) {
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return BatchResult{}, err
	}
	if len(requests) < 1 || len(requests) > MaxBatchItems {
		return BatchResult{}, domain.NewError(domain.ErrorInvalid, "copy/move batch must contain 1 to 100 items")
	}
	label := "copy-batch"
	if move {
		label = "move-batch"
	}
	result := BatchResult{OperationID: domain.OperationID(s.derivedID(label, userID, idempotencyKey)), Items: make([]ItemResult, 0, len(requests))}
	for index, request := range requests {
		request.IdempotencyKey = idempotencyKey + ":" + strconv.Itoa(index)
		var operation domain.Operation
		var err error
		if move {
			operation, err = s.Move(ctx, userID, request)
		} else {
			operation, err = s.Copy(ctx, userID, request)
		}
		item := ItemResult{Path: request.Source, OperationID: operation.ID, State: operation.State, ErrorKind: operation.ErrorKind}
		if err != nil {
			item.State, item.ErrorKind = domain.OperationFailed, domain.KindOf(err)
		}
		result.Items = append(result.Items, item)
	}
	if err := s.recordBatchOperation(ctx, userID, result); err != nil {
		return BatchResult{}, err
	}
	return result, nil
}

type ItemResult struct {
	Path        domain.UserPath       `json:"path"`
	TrashID     string                `json:"trashID,omitempty"`
	OperationID domain.OperationID    `json:"operationID,omitempty"`
	State       domain.OperationState `json:"state"`
	ErrorKind   domain.ErrorKind      `json:"errorKind,omitempty"`
}

type BatchResult struct {
	OperationID domain.OperationID `json:"operationID"`
	Items       []ItemResult       `json:"items"`
}

// TrashItem pins a reviewed live path to the portable logical version that was
// displayed to the caller. It keeps duplicate cleanup from moving a path after
// that path has changed into different content.
type TrashItem struct {
	Path    domain.UserPath
	Version domain.Version
}

type TrashPage struct {
	Items      []TrashEntry `json:"items"`
	NextCursor string       `json:"nextCursor,omitempty"`
}

// TrashEntry is the authenticated API view of an unchanged canonical trash
// record joined to immutable metadata from the persisted trash directory tree.
type TrashEntry struct {
	SchemaVersion   int              `json:"schemaVersion"`
	TrashID         string           `json:"trashID"`
	OwnerUserID     domain.UserID    `json:"ownerUserID"`
	OriginalPath    domain.UserPath  `json:"originalPath"`
	TrashedPath     domain.UserPath  `json:"trashedPath"`
	Kind            domain.EntryKind `json:"kind"`
	Size            int64            `json:"size"`
	FileCount       int64            `json:"fileCount"`
	MediaType       string           `json:"mediaType,omitempty"`
	TrashedAt       time.Time        `json:"trashedAt"`
	OriginalVersion domain.Version   `json:"originalVersion"`
}

func (s *Service) Trash(ctx context.Context, userID domain.UserID, paths []domain.UserPath, idempotencyKey string) (BatchResult, error) {
	items := make([]TrashItem, 0, len(paths))
	for _, path := range paths {
		items = append(items, TrashItem{Path: path})
	}
	return s.trashItems(ctx, userID, items, idempotencyKey, false)
}

// TrashVersioned applies the ordinary recoverable trash workflow only when
// every selected path still has the logical version reviewed by the caller.
func (s *Service) TrashVersioned(ctx context.Context, userID domain.UserID, items []TrashItem, idempotencyKey string) (BatchResult, error) {
	return s.trashItems(ctx, userID, items, idempotencyKey, true)
}

func (s *Service) trashItems(ctx context.Context, userID domain.UserID, items []TrashItem, idempotencyKey string, requireVersion bool) (BatchResult, error) {
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return BatchResult{}, err
	}
	if len(items) < 1 || len(items) > MaxBatchItems {
		return BatchResult{}, domain.NewError(domain.ErrorInvalid, "trash batch must contain 1 to 100 items")
	}
	live, err := liveScope(userID)
	if err != nil {
		return BatchResult{}, err
	}
	trash, err := trashScope(userID)
	if err != nil {
		return BatchResult{}, err
	}
	batchID := domain.OperationID(s.derivedID("trash-batch", userID, idempotencyKey))
	result := BatchResult{OperationID: batchID, Items: make([]ItemResult, 0, len(items))}
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if !item.Path.Valid() || item.Path.IsRoot() {
			return BatchResult{}, domain.NewError(domain.ErrorInvalid, "trash path is invalid")
		}
		if requireVersion && !validReviewedVersion(item.Version) {
			return BatchResult{}, domain.NewError(domain.ErrorInvalid, "reviewed trash version is invalid")
		}
		if _, duplicate := seen[item.Path.String()]; duplicate {
			return BatchResult{}, domain.NewError(domain.ErrorInvalid, "trash batch contains duplicate paths")
		}
		seen[item.Path.String()] = struct{}{}
	}
	for index, requested := range items {
		item, itemErr := s.trashOneExpected(ctx, userID, live, trash, requested.Path, requested.Version, idempotencyKey+":"+strconv.Itoa(index))
		if itemErr != nil {
			item = ItemResult{Path: requested.Path, State: domain.OperationFailed, ErrorKind: domain.KindOf(itemErr)}
		}
		result.Items = append(result.Items, item)
	}
	if err := s.recordBatchOperation(ctx, userID, result); err != nil {
		return BatchResult{}, err
	}
	return result, nil
}

func validReviewedVersion(version domain.Version) bool {
	value := string(version)
	return value != "" && len(value) <= 512 && utf8.ValidString(value) && !strings.ContainsAny(value, "\r\n\x00")
}

func (s *Service) recordBatchOperation(ctx context.Context, userID domain.UserID, result BatchResult) error {
	stateValue := domain.OperationSucceeded
	for _, item := range result.Items {
		if item.State != domain.OperationSucceeded {
			stateValue = domain.OperationFailed
			break
		}
	}
	now := s.clock.Now()
	record := model.BatchOperation{SchemaVersion: model.SchemaVersion, OwnerUserID: userID, OperationID: result.OperationID, State: stateValue, StartedAt: now, UpdatedAt: now}
	return createOrMatch(s.repository.createBatchOperation(ctx, record))
}

func (s *Service) trashOneExpected(ctx context.Context, userID domain.UserID, live, trash domain.Scope, path domain.UserPath, expected domain.Version, key string) (ItemResult, error) {
	trashID := s.derivedID("trash", userID, key)
	if existing, _, err := s.repository.trash(ctx, userID, trashID); err == nil {
		if existing.OriginalPath != path {
			return ItemResult{}, domain.NewError(domain.ErrorConflict, "idempotency key was used for a different request")
		}
		return ItemResult{Path: existing.OriginalPath, TrashID: trashID, State: domain.OperationSucceeded}, nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return ItemResult{}, err
	}
	entry, err := s.storage.Stat(ctx, live, path)
	if err != nil {
		return ItemResult{}, err
	}
	if expected != "" && entry.Version != expected {
		return ItemResult{}, domain.NewError(domain.ErrorPreconditionFailed, "trash source version changed")
	}
	trashedPath := domain.MustParseUserPath("/" + trashID)
	operation, err := s.storage.Move(ctx, live, trash, domain.MoveRequest{Source: path, Destination: trashedPath, Conflict: domain.ConflictFail, ExpectedSource: entry.Version, IdempotencyKey: key})
	if err != nil {
		return ItemResult{}, err
	}
	if operation.State != domain.OperationSucceeded {
		return ItemResult{Path: path, OperationID: operation.ID, State: operation.State, ErrorKind: operation.ErrorKind}, nil
	}
	record := model.Trash{SchemaVersion: model.SchemaVersion, TrashID: trashID, OwnerUserID: userID, OriginalPath: path, TrashedPath: trashedPath, Kind: entry.Kind, TrashedAt: s.clock.Now(), OriginalVersion: entry.Version}
	if err := createOrMatch(s.repository.createTrash(ctx, record)); err != nil {
		return ItemResult{}, err
	}
	return ItemResult{Path: path, TrashID: trashID, OperationID: operation.ID, State: operation.State}, nil
}

func (s *Service) TrashList(ctx context.Context, userID domain.UserID) ([]model.Trash, error) {
	records, err := s.repository.trashList(ctx, userID)
	if err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].TrashedAt.Equal(records[j].TrashedAt) {
			return records[i].TrashID < records[j].TrashID
		}
		return records[i].TrashedAt.After(records[j].TrashedAt)
	})
	return records, nil
}

func (s *Service) TrashPage(ctx context.Context, userID domain.UserID, limit int, cursor string) (TrashPage, error) {
	if limit == 0 {
		limit = 200
	}
	if limit < 1 || limit > 1000 {
		return TrashPage{}, domain.NewError(domain.ErrorInvalid, "trash page limit must be between 1 and 1000")
	}
	records, next, err := s.repository.trashPage(ctx, userID, limit, cursor)
	if err != nil {
		return TrashPage{}, err
	}
	if len(records) == 0 {
		return TrashPage{Items: []TrashEntry{}, NextCursor: next}, nil
	}
	scope, err := trashScope(userID)
	if err != nil {
		return TrashPage{}, err
	}
	names := make([]string, 0, len(records))
	for _, record := range records {
		expectedPath, pathErr := domain.ParseUserPath("/" + record.TrashID)
		if pathErr != nil || record.OwnerUserID != userID || record.TrashedPath != expectedPath {
			return TrashPage{}, domain.NewError(domain.ErrorInvalid, "trash record metadata is inconsistent")
		}
		names = append(names, record.TrashID)
	}
	lookup, err := s.storage.LookupChildren(ctx, scope, domain.ChildLookupRequest{Directory: domain.MustParseUserPath("/"), Names: names})
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return TrashPage{}, domain.NewError(domain.ErrorInvalid, "trash tree metadata is incomplete")
		}
		return TrashPage{}, err
	}
	if lookup.Current.Path != domain.MustParseUserPath("/") || lookup.Current.Kind != domain.EntryDirectory || lookup.Current.Size < 0 || lookup.Current.FileCount < 0 || lookup.Current.MediaType != "" || len(lookup.Entries) != len(records) {
		return TrashPage{}, domain.NewError(domain.ErrorInvalid, "trash tree metadata is inconsistent")
	}
	items := make([]TrashEntry, 0, len(records))
	for index, record := range records {
		entry := lookup.Entries[index]
		if entry.Path != record.TrashedPath || entry.Name != record.TrashID || entry.Kind != record.Kind || entry.Size < 0 || entry.FileCount < 0 || (entry.Kind == domain.EntryDirectory && entry.MediaType != "") || (entry.Kind == domain.EntryFile && (entry.MediaType == "" || entry.FileCount != 1)) {
			return TrashPage{}, domain.NewError(domain.ErrorInvalid, "trash entry metadata is inconsistent")
		}
		items = append(items, TrashEntry{
			SchemaVersion: record.SchemaVersion, TrashID: record.TrashID, OwnerUserID: record.OwnerUserID,
			OriginalPath: record.OriginalPath, TrashedPath: record.TrashedPath, Kind: record.Kind,
			Size: entry.Size, FileCount: entry.FileCount, MediaType: entry.MediaType, TrashedAt: record.TrashedAt, OriginalVersion: record.OriginalVersion,
		})
	}
	return TrashPage{Items: items, NextCursor: next}, nil
}

func (s *Service) Restore(ctx context.Context, userID domain.UserID, trashID string, conflict domain.ConflictMode, idempotencyKey string) (domain.Operation, error) {
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return domain.Operation{}, err
	}
	fingerprint := mutationFingerprint("restore", trashID, string(conflict))
	if prior, err := s.replayMutation(ctx, userID, idempotencyKey, "restore", fingerprint); err != nil || prior.ID != "" {
		return prior, err
	}
	record, version, err := s.repository.trash(ctx, userID, trashID)
	if err != nil {
		return domain.Operation{}, err
	}
	from, _ := trashScope(userID)
	to, _ := liveScope(userID)
	entry, err := s.storage.Stat(ctx, from, record.TrashedPath)
	if err != nil {
		return domain.Operation{}, err
	}
	operation, err := s.storage.Move(ctx, from, to, domain.MoveRequest{Source: record.TrashedPath, Destination: record.OriginalPath, Conflict: conflict, ExpectedSource: entry.Version, IdempotencyKey: idempotencyKey})
	if err != nil {
		return domain.Operation{}, err
	}
	if operation.State == domain.OperationSucceeded {
		if err := s.repository.deleteTrash(ctx, userID, trashID, version); err != nil && !errors.Is(err, domain.ErrNotFound) {
			return domain.Operation{}, err
		}
	}
	if err := s.saveMutation(ctx, userID, idempotencyKey, "restore", fingerprint, operation); err != nil {
		return domain.Operation{}, err
	}
	return operation, nil
}

func (s *Service) PermanentDelete(ctx context.Context, userID domain.UserID, trashID, idempotencyKey string) (domain.Operation, error) {
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return domain.Operation{}, err
	}
	fingerprint := mutationFingerprint("permanent_delete", trashID)
	if prior, err := s.replayMutation(ctx, userID, idempotencyKey, "permanent_delete", fingerprint); err != nil || prior.ID != "" {
		return prior, err
	}
	record, version, err := s.repository.trash(ctx, userID, trashID)
	if err != nil {
		return domain.Operation{}, err
	}
	scope, _ := trashScope(userID)
	entry, err := s.storage.Stat(ctx, scope, record.TrashedPath)
	if err != nil {
		return domain.Operation{}, err
	}
	operation, err := s.storage.Delete(ctx, scope, domain.DeleteRequest{Path: record.TrashedPath, ExpectedVersion: entry.Version, IdempotencyKey: idempotencyKey})
	if err != nil {
		return domain.Operation{}, err
	}
	if operation.State == domain.OperationSucceeded {
		if err := s.repository.deleteTrash(ctx, userID, trashID, version); err != nil && !errors.Is(err, domain.ErrNotFound) {
			return domain.Operation{}, err
		}
	}
	if err := s.saveMutation(ctx, userID, idempotencyKey, "permanent_delete", fingerprint, operation); err != nil {
		return domain.Operation{}, err
	}
	return operation, nil
}

func mutationFingerprint(values ...string) string { return secret.Hash(strings.Join(values, "\x00")) }

func (s *Service) replayMutation(ctx context.Context, owner domain.UserID, key, kind, fingerprint string) (domain.Operation, error) {
	prior, err := s.repository.mutationOutcome(ctx, owner, key)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Operation{}, nil
	}
	if err != nil {
		return domain.Operation{}, err
	}
	if prior.Kind != kind || prior.Fingerprint != fingerprint {
		return domain.Operation{}, domain.NewError(domain.ErrorConflict, "idempotency key was used for a different request")
	}
	return prior.Operation, nil
}

func (s *Service) saveMutation(ctx context.Context, owner domain.UserID, key, kind, fingerprint string, operation domain.Operation) error {
	record := model.MutationOutcome{SchemaVersion: model.SchemaVersion, OwnerUserID: owner, KeyHash: secret.Hash(key), Kind: kind, Fingerprint: fingerprint, Operation: operation}
	return createOrMatch(s.repository.createMutationOutcome(ctx, key, record))
}

func (s *Service) EmptyTrash(ctx context.Context, userID domain.UserID, confirmed bool, idempotencyKey string) (BatchResult, error) {
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return BatchResult{}, err
	}
	if !confirmed {
		return BatchResult{}, domain.NewError(domain.ErrorInvalid, "empty trash requires confirmation")
	}
	records, err := s.TrashList(ctx, userID)
	if err != nil {
		return BatchResult{}, err
	}
	if len(records) > MaxBatchItems {
		return BatchResult{}, domain.NewError(domain.ErrorInvalid, "empty trash is limited to 100 items per request")
	}
	result := BatchResult{OperationID: domain.OperationID(s.derivedID("empty-trash", userID, idempotencyKey))}
	for index, record := range records {
		op, itemErr := s.PermanentDelete(ctx, userID, record.TrashID, idempotencyKey+":"+strconv.Itoa(index))
		item := ItemResult{Path: record.OriginalPath, TrashID: record.TrashID, OperationID: op.ID, State: op.State}
		if itemErr != nil {
			item.State, item.ErrorKind = domain.OperationFailed, domain.KindOf(itemErr)
		}
		result.Items = append(result.Items, item)
	}
	if err := s.recordBatchOperation(ctx, userID, result); err != nil {
		return BatchResult{}, err
	}
	return result, nil
}

func (s *Service) Operation(ctx context.Context, userID domain.UserID, operationID domain.OperationID) (domain.Operation, error) {
	operation, err := s.storage.GetOperation(ctx, userID, operationID)
	if err == nil || !errors.Is(err, domain.ErrNotFound) {
		return operation, err
	}
	batch, err := s.repository.batchOperation(ctx, userID, operationID)
	if err != nil {
		return domain.Operation{}, err
	}
	return domain.Operation{ID: batch.OperationID, State: batch.State, StartedAt: batch.StartedAt, UpdatedAt: batch.UpdatedAt}, nil
}

func (s *Service) derivedID(label string, owner domain.UserID, key string) string {
	return secret.KeyedHash(s.tokenKey, label+"\x00"+owner.String()+"\x00"+key)
}

type CreatedShare struct {
	Record model.Share  `json:"share"`
	Link   secret.Value `json:"-"`
}

func (s *Service) CreateShare(ctx context.Context, userID domain.UserID, path domain.UserPath, expiresAt *time.Time, idempotencyKey string) (CreatedShare, error) {
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return CreatedShare{}, err
	}
	if expiresAt != nil && !expiresAt.After(s.clock.Now()) {
		return CreatedShare{}, domain.NewError(domain.ErrorInvalid, "share expiry must be in the future")
	}
	shareID := s.derivedID("share-id", userID, idempotencyKey)
	token := s.derivedID("share-token", userID, idempotencyKey)
	if prior, _, err := s.repository.shareByID(ctx, userID, shareID); err == nil {
		if prior.RootPath != path || !sameTime(prior.ExpiresAt, expiresAt) {
			return CreatedShare{}, domain.NewError(domain.ErrorConflict, "idempotency key was used for a different request")
		}
		return CreatedShare{Record: prior, Link: secret.Value(s.baseURL + "/s/" + token)}, nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return CreatedShare{}, err
	}
	scope, _ := liveScope(userID)
	entry, err := s.storage.Stat(ctx, scope, path)
	if err != nil {
		return CreatedShare{}, err
	}
	record := model.Share{SchemaVersion: model.SchemaVersion, ShareID: shareID, TokenHash: secret.Hash(token), OwnerUserID: userID, RootPath: path, RootVersion: entry.Version, Kind: entry.Kind, CreatedAt: s.clock.Now(), ExpiresAt: expiresAt}
	if err := s.repository.createShare(ctx, record); err != nil {
		return CreatedShare{}, err
	}
	return CreatedShare{Record: record, Link: secret.Value(s.baseURL + "/s/" + token)}, nil
}

func sameTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func (s *Service) Shares(ctx context.Context, userID domain.UserID) ([]model.Share, error) {
	return s.repository.shares(ctx, userID)
}

func (s *Service) RevokeShare(ctx context.Context, userID domain.UserID, shareID string) error {
	record, version, err := s.repository.shareByID(ctx, userID, shareID)
	if err != nil {
		return err
	}
	if record.RevokedAt != nil {
		return nil
	}
	now := s.clock.Now()
	record.RevokedAt = &now
	return s.repository.updateShare(ctx, record, version)
}

type PublicEntry struct {
	Path       string           `json:"path"`
	Name       string           `json:"name"`
	Kind       domain.EntryKind `json:"kind"`
	Size       int64            `json:"size"`
	FileCount  int64            `json:"fileCount"`
	MediaType  string           `json:"mediaType,omitempty"`
	ModifiedAt time.Time        `json:"modifiedAt"`
	Version    domain.Version   `json:"version"`
}
type PublicPage struct {
	Root       PublicEntry   `json:"root"`
	Current    PublicEntry   `json:"current"`
	Entries    []PublicEntry `json:"entries,omitempty"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

func (s *Service) PublicShare(ctx context.Context, token, relative string, limit int, cursor string) (PublicPage, error) {
	record, root, scope, err := s.authorizeShare(ctx, token)
	if err != nil {
		return PublicPage{}, publicShareError()
	}
	target, err := sharedPath(record, relative)
	if err != nil {
		return PublicPage{}, publicShareError()
	}
	result := PublicPage{Root: publicEntry(record.RootPath, root, root)}
	if record.Kind == domain.EntryFile {
		if target != record.RootPath {
			return PublicPage{}, publicShareError()
		}
		result.Current = result.Root
		return result, nil
	}
	page, err := s.storage.List(ctx, scope, domain.ListRequest{Directory: target, PageSize: limit, Cursor: cursor, Sort: domain.SortName})
	if err != nil {
		return PublicPage{}, publicShareError()
	}
	if page.Current.Kind != domain.EntryDirectory || page.Current.Size < 0 || page.Current.FileCount < 0 || page.Current.MediaType != "" {
		return PublicPage{}, publicShareError()
	}
	rootAgain, err := s.storage.Stat(ctx, scope, record.RootPath)
	if err != nil || rootAgain.Version != root.Version || rootAgain.Kind != root.Kind {
		return PublicPage{}, publicShareError()
	}
	result.Current = publicEntry(record.RootPath, page.Current, root)
	for _, child := range page.Entries {
		result.Entries = append(result.Entries, publicEntry(record.RootPath, child, root))
	}
	result.NextCursor = page.NextCursor
	return result, nil
}

func (s *Service) PublicStat(ctx context.Context, token, relative string) (PublicEntry, error) {
	record, root, scope, err := s.authorizeShare(ctx, token)
	if err != nil {
		return PublicEntry{}, publicShareError()
	}
	path, err := sharedPath(record, relative)
	if err != nil {
		return PublicEntry{}, publicShareError()
	}
	entry, err := s.storage.Stat(ctx, scope, path)
	if err != nil {
		return PublicEntry{}, publicShareError()
	}
	rootAgain, err := s.storage.Stat(ctx, scope, record.RootPath)
	if err != nil || rootAgain.Version != root.Version || rootAgain.Kind != root.Kind {
		return PublicEntry{}, publicShareError()
	}
	return publicEntry(record.RootPath, entry, root), nil
}

func (s *Service) PublicDownload(ctx context.Context, token, relative string, version domain.Version, preview bool) (domain.DownloadCapability, string, error) {
	record, _, scope, err := s.authorizeShare(ctx, token)
	if err != nil {
		return domain.DownloadCapability{}, "", publicShareError()
	}
	path, err := sharedPath(record, relative)
	if err != nil {
		return domain.DownloadCapability{}, "", publicShareError()
	}
	entry, err := s.storage.Stat(ctx, scope, path)
	if err != nil || entry.Kind != domain.EntryFile || version == "" || entry.Version != version {
		return domain.DownloadCapability{}, "", publicShareError()
	}
	disposition, previewKind := domain.DispositionAttachment, "download"
	if preview {
		previewKind, err = s.previewKind(entry)
		if err != nil {
			return domain.DownloadCapability{}, "", publicShareError()
		}
		disposition = domain.DispositionInline
	}
	capability, err := s.storage.CreateDownload(ctx, scope, domain.CreateDownloadRequest{Path: path, Version: version, Disposition: disposition})
	if err != nil {
		return domain.DownloadCapability{}, "", publicShareError()
	}
	return capability, previewKind, nil
}

func (s *Service) authorizeShare(ctx context.Context, token string) (model.Share, domain.Entry, domain.Scope, error) {
	if !secret.ValidBearerToken(token) {
		return model.Share{}, domain.Entry{}, domain.Scope{}, publicShareError()
	}
	record, _, err := s.repository.shareByTokenHash(ctx, secret.Hash(token))
	if err != nil {
		return model.Share{}, domain.Entry{}, domain.Scope{}, publicShareError()
	}
	if record.RevokedAt != nil || (record.ExpiresAt != nil && !s.clock.Now().Before(*record.ExpiresAt)) {
		return model.Share{}, domain.Entry{}, domain.Scope{}, publicShareError()
	}
	account, _, err := s.accounts.Account(ctx, record.OwnerUserID)
	if err != nil || account.Status != model.AccountEnabled {
		return model.Share{}, domain.Entry{}, domain.Scope{}, publicShareError()
	}
	scope, _ := liveScope(record.OwnerUserID)
	root, err := s.storage.Stat(ctx, scope, record.RootPath)
	if err != nil || root.Version != record.RootVersion || root.Kind != record.Kind {
		return model.Share{}, domain.Entry{}, domain.Scope{}, publicShareError()
	}
	return record, root, scope, nil
}

func sharedPath(record model.Share, relative string) (domain.UserPath, error) {
	if relative == "" {
		relative = "/"
	}
	if strings.Contains(relative, "%") || strings.Contains(relative, "\\") {
		return domain.UserPath{}, domain.NewError(domain.ErrorInvalid, "invalid shared path")
	}
	parsed, err := domain.ParseUserPath(relative)
	if err != nil {
		return domain.UserPath{}, err
	}
	if parsed.IsRoot() {
		return record.RootPath, nil
	}
	if record.Kind != domain.EntryDirectory {
		return domain.UserPath{}, domain.NewError(domain.ErrorNotFound, "shared path unavailable")
	}
	target := record.RootPath
	for _, segment := range parsed.Segments() {
		target, err = target.Join(segment)
		if err != nil {
			return domain.UserPath{}, err
		}
	}
	if target != record.RootPath && !target.IsDescendantOf(record.RootPath) {
		return domain.UserPath{}, domain.NewError(domain.ErrorUnauthorized, "shared path escape")
	}
	return target, nil
}

func publicEntry(rootPath domain.UserPath, entry, root domain.Entry) PublicEntry {
	path := "/"
	if entry.Path != rootPath {
		path = strings.TrimPrefix(entry.Path.String(), rootPath.String())
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
	}
	name := entry.Name
	if entry.Path == rootPath {
		name = root.Name
	}
	return PublicEntry{Path: path, Name: name, Kind: entry.Kind, Size: entry.Size, FileCount: entry.FileCount, MediaType: entry.MediaType, ModifiedAt: entry.ModifiedAt, Version: entry.Version}
}

func publicShareError() error {
	return domain.NewError(domain.ErrorNotFound, "shared content is unavailable")
}
