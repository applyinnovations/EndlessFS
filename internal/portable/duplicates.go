package portable

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const (
	duplicateOccurrenceSchema = "duplicate-occurrence-root-v1"
	duplicateSummarySchema    = "duplicate-summary-root-v1"
	duplicateIgnoreSchema     = "duplicate-ignore-v1"
)

type catalogChange struct {
	pre  *storageformat.DuplicateOccurrence
	post *storageformat.DuplicateOccurrence
}

type catalogRootChange struct {
	key          objectstore.Key
	preExisted   bool
	expected     string
	pendingBody  []byte
	finalBody    []byte
	rollbackBody []byte
}

type duplicateGroupCursor struct {
	SchemaVersion  int                             `json:"schemaVersion"`
	UserID         string                          `json:"userID"`
	KindFilter     domain.DuplicateKind            `json:"kindFilter,omitempty"`
	IncludeIgnored bool                            `json:"includeIgnored"`
	Limit          int                             `json:"limit"`
	KindIndex      int                             `json:"kindIndex"`
	After          string                          `json:"after,omitempty"`
	Pending        *storageformat.DuplicateSummary `json:"pending,omitempty"`
	ExpiresAt      time.Time                       `json:"expiresAt"`
}

type duplicateOccurrenceCursor struct {
	SchemaVersion int                  `json:"schemaVersion"`
	UserID        string               `json:"userID"`
	GroupID       string               `json:"groupID"`
	Kind          domain.DuplicateKind `json:"kind"`
	Limit         int                  `json:"limit"`
	After         string               `json:"after,omitempty"`
	ExpiresAt     time.Time            `json:"expiresAt"`
}

type duplicateReconciliationCursor struct {
	SchemaVersion int                      `json:"schemaVersion"`
	UserID        string                   `json:"userID"`
	Left          domain.DuplicateLocation `json:"left"`
	Right         domain.DuplicateLocation `json:"right"`
	RemoveFrom    domain.DuplicateSide     `json:"removeFrom"`
	Limit         int                      `json:"limit"`
	After         string                   `json:"after,omitempty"`
	LeftManifest  string                   `json:"leftManifest"`
	RightManifest string                   `json:"rightManifest"`
	GateEpoch     uint64                   `json:"gateEpoch"`
	GateVersion   string                   `json:"gateVersion"`
	ExpiresAt     time.Time                `json:"expiresAt"`
}

type duplicateReconciliationPlan struct {
	SchemaVersion int                                  `json:"schemaVersion"`
	UserID        string                               `json:"userID"`
	Left          domain.DuplicateOccurrence           `json:"left"`
	Right         domain.DuplicateOccurrence           `json:"right"`
	RemoveFrom    domain.DuplicateSide                 `json:"removeFrom"`
	Items         []domain.DuplicateReconciliationItem `json:"items"`
	LeftManifest  string                               `json:"leftManifest"`
	RightManifest string                               `json:"rightManifest"`
	GateEpoch     uint64                               `json:"gateEpoch"`
	GateVersion   string                               `json:"gateVersion"`
	ExpiresAt     time.Time                            `json:"expiresAt"`
}

func duplicateFileGroupID(entry storageformat.DirectoryEntry) (string, error) {
	if entry.Kind != domain.EntryFile || entry.Size < 0 || entry.SHA256 != "" || !(objectstore.ContentFingerprint{MD5: entry.MD5, CRC32C: entry.CRC32C}).Complete() {
		return "", domain.NewError(domain.ErrorInvalid, "file has no provider-backed duplicate identity")
	}
	body, err := storageformat.EncodeCanonical(struct {
		Kind   domain.DuplicateKind `json:"kind"`
		Size   int64                `json:"size"`
		MD5    string               `json:"md5"`
		CRC32C string               `json:"crc32c"`
	}{domain.DuplicateFile, entry.Size, entry.MD5, entry.CRC32C})
	if err != nil {
		return "", err
	}
	return storageformat.Digest(append([]byte("endlessfs-duplicate-group-v1\x00"), body...)), nil
}

func duplicateDirectoryGroupID(size, fileCount int64, contentDigest string) (string, error) {
	if size < 0 || fileCount < 0 || contentDigest == "" {
		return "", domain.NewError(domain.ErrorInvalid, "directory has no exact duplicate identity")
	}
	body, err := storageformat.EncodeCanonical(struct {
		Kind          domain.DuplicateKind `json:"kind"`
		Size          int64                `json:"size"`
		FileCount     int64                `json:"fileCount"`
		ContentDigest string               `json:"contentDigest"`
	}{domain.DuplicateDirectory, size, fileCount, contentDigest})
	if err != nil {
		return "", err
	}
	return storageformat.Digest(append([]byte("endlessfs-duplicate-group-v1\x00"), body...)), nil
}

func catalogOccurrence(scope domain.Scope, path domain.UserPath, entry storageformat.DirectoryEntry) (storageformat.DuplicateOccurrence, error) {
	if !scope.Valid() || !path.Valid() || path.IsRoot() {
		return storageformat.DuplicateOccurrence{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate occurrence location")
	}
	result := storageformat.DuplicateOccurrence{Area: areaName(scope.Area()), Path: path.String(), Size: entry.Size, FileCount: entry.FileCount, Version: entry.LogicalVersion}
	var err error
	switch entry.Kind {
	case domain.EntryFile:
		result.Kind = domain.DuplicateFile
		result.FileCount = 1
		result.GroupID, err = duplicateFileGroupID(entry)
	case domain.EntryDirectory:
		result.Kind = domain.DuplicateDirectory
		result.GroupID, err = duplicateDirectoryGroupID(entry.Size, entry.FileCount, entry.ContentDigest)
	default:
		err = domain.NewError(domain.ErrorInvalid, "invalid duplicate occurrence kind")
	}
	if err != nil {
		return storageformat.DuplicateOccurrence{}, err
	}
	if result.Version == "" {
		return storageformat.DuplicateOccurrence{}, domain.NewError(domain.ErrorInvalid, "duplicate occurrence has no logical version")
	}
	return result, nil
}

func validateDuplicateGroupID(value string) error {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return domain.NewError(domain.ErrorInvalid, "invalid duplicate group ID")
	}
	return nil
}

func duplicateOccurrenceKey(userID string, occurrence storageformat.DuplicateOccurrence) objectstore.Key {
	return storageformat.DuplicateOccurrenceKey(userID, string(occurrence.Kind), occurrence.GroupID, occurrence.Area, occurrence.Path)
}

func duplicateSummaryShard(occurrence storageformat.DuplicateOccurrence) string {
	digest := storageformat.NameDigest(occurrence.Area + "\x00" + occurrence.Path)
	return digest[:2]
}

func sameDuplicateOccurrence(left, right *storageformat.DuplicateOccurrence) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func (s *FileStore) buildCatalogOperationRoots(ctx context.Context, userID domain.UserID, operationID string, changes []catalogChange) ([]storageformat.FileOperationRoot, error) {
	if len(changes) == 0 {
		return nil, nil
	}
	type occurrenceUpdate struct {
		pre  *storageformat.DuplicateOccurrence
		post *storageformat.DuplicateOccurrence
	}
	occurrences := make(map[string]occurrenceUpdate)
	type summaryDelta struct {
		groupID   string
		kind      domain.DuplicateKind
		shard     string
		size      int64
		fileCount int64
		delta     int64
	}
	summaries := make(map[string]summaryDelta)
	addSummary := func(value storageformat.DuplicateOccurrence, delta int64) error {
		shard := duplicateSummaryShard(value)
		key := storageformat.DuplicateSummaryKey(userID.String(), string(value.Kind), value.GroupID, shard).String()
		current := summaries[key]
		if current.groupID != "" && (current.groupID != value.GroupID || current.kind != value.Kind || current.shard != shard || current.size != value.Size || current.fileCount != value.FileCount) {
			return domain.NewError(domain.ErrorInvalid, "conflicting duplicate summary identity")
		}
		current.groupID, current.kind, current.shard, current.size, current.fileCount = value.GroupID, value.Kind, shard, value.Size, value.FileCount
		current.delta += delta
		summaries[key] = current
		return nil
	}
	for _, change := range changes {
		if sameDuplicateOccurrence(change.pre, change.post) {
			continue
		}
		if change.pre != nil {
			key := duplicateOccurrenceKey(userID.String(), *change.pre).String()
			update := occurrences[key]
			if update.pre != nil && !sameDuplicateOccurrence(update.pre, change.pre) {
				return nil, domain.NewError(domain.ErrorInvalid, "conflicting duplicate occurrence removal")
			}
			pre := *change.pre
			update.pre = &pre
			occurrences[key] = update
			if err := addSummary(pre, -1); err != nil {
				return nil, err
			}
		}
		if change.post != nil {
			key := duplicateOccurrenceKey(userID.String(), *change.post).String()
			update := occurrences[key]
			if update.post != nil && !sameDuplicateOccurrence(update.post, change.post) {
				return nil, domain.NewError(domain.ErrorInvalid, "conflicting duplicate occurrence addition")
			}
			post := *change.post
			update.post = &post
			occurrences[key] = update
			if err := addSummary(post, 1); err != nil {
				return nil, err
			}
		}
	}
	rootChanges := make([]catalogRootChange, 0, len(occurrences)+len(summaries))
	for keyValue, update := range occurrences {
		if sameDuplicateOccurrence(update.pre, update.post) {
			continue
		}
		rootChange, err := s.prepareOccurrenceRootChange(ctx, userID, operationID, objectstore.MustKey(keyValue), update.pre, update.post)
		if err != nil {
			return nil, err
		}
		rootChanges = append(rootChanges, rootChange)
	}
	for keyValue, delta := range summaries {
		if delta.delta == 0 {
			continue
		}
		rootChange, err := s.prepareSummaryRootChange(ctx, userID, operationID, objectstore.MustKey(keyValue), delta)
		if err != nil {
			return nil, err
		}
		rootChanges = append(rootChanges, rootChange)
	}
	sort.Slice(rootChanges, func(i, j int) bool { return rootChanges[i].key.String() < rootChanges[j].key.String() })
	result := make([]storageformat.FileOperationRoot, 0, len(rootChanges))
	for _, change := range rootChanges {
		result = append(result, storageformat.FileOperationRoot{
			Key: change.key.String(), ExpectedLogicalVersion: change.expected, PreExisted: change.preExisted,
			PendingBody: change.pendingBody, FinalBody: change.finalBody, RollbackBody: change.rollbackBody,
		})
	}
	return result, nil
}

func (s *FileStore) prepareOccurrenceRootChange(ctx context.Context, userID domain.UserID, operationID string, key objectstore.Key, pre, post *storageformat.DuplicateOccurrence) (catalogRootChange, error) {
	object, err := s.engine.backend.Get(ctx, key)
	revision := uint64(0)
	expected := ""
	preExisted := false
	var physicalCurrent *storageformat.DuplicateOccurrence
	if err == nil {
		preExisted = true
		var envelope storageformat.Envelope
		var root storageformat.DuplicateOccurrenceRoot
		if decodeErr := storageformat.DecodeEnvelope(object.Body, key, duplicateOccurrenceSchema, &envelope, &root); decodeErr != nil {
			return catalogRootChange{}, decodeErr
		}
		if root.SchemaVersion != 1 || root.UserID != userID.String() || root.Pending != nil {
			return catalogRootChange{}, domain.NewError(domain.ErrorUnavailable, "duplicate occurrence is changing concurrently")
		}
		physicalCurrent = root.Current
		revision = envelope.Revision
		expected = envelope.LogicalVersion
	} else if !errors.Is(err, domain.ErrNotFound) {
		return catalogRootChange{}, err
	}
	if !sameDuplicateOccurrence(physicalCurrent, pre) {
		return catalogRootChange{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate occurrence changed")
	}
	pending := storageformat.DuplicateOccurrenceRoot{SchemaVersion: 1, UserID: userID.String(), Current: physicalCurrent, Pending: &storageformat.DuplicateOccurrenceTransition{OperationID: operationID, Fence: 1, Pre: pre, Post: post}}
	pendingBody, err := storageformat.EncodeEnvelope(duplicateOccurrenceSchema, key, revision+1, pending)
	if err != nil {
		return catalogRootChange{}, err
	}
	finalBody, err := storageformat.EncodeEnvelope(duplicateOccurrenceSchema, key, revision+2, storageformat.DuplicateOccurrenceRoot{SchemaVersion: 1, UserID: userID.String(), Current: post})
	if err != nil {
		return catalogRootChange{}, err
	}
	var rollbackBody []byte
	if preExisted {
		rollbackBody, err = storageformat.EncodeEnvelope(duplicateOccurrenceSchema, key, revision+2, storageformat.DuplicateOccurrenceRoot{SchemaVersion: 1, UserID: userID.String(), Current: pre})
		if err != nil {
			return catalogRootChange{}, err
		}
	}
	return catalogRootChange{key: key, preExisted: preExisted, expected: expected, pendingBody: pendingBody, finalBody: finalBody, rollbackBody: rollbackBody}, nil
}

func (s *FileStore) prepareSummaryRootChange(ctx context.Context, userID domain.UserID, operationID string, key objectstore.Key, delta struct {
	groupID   string
	kind      domain.DuplicateKind
	shard     string
	size      int64
	fileCount int64
	delta     int64
}) (catalogRootChange, error) {
	object, err := s.engine.backend.Get(ctx, key)
	revision := uint64(0)
	expected := ""
	preExisted := false
	var pre *storageformat.DuplicateSummary
	if err == nil {
		preExisted = true
		var envelope storageformat.Envelope
		var root storageformat.DuplicateSummaryRoot
		if decodeErr := storageformat.DecodeEnvelope(object.Body, key, duplicateSummarySchema, &envelope, &root); decodeErr != nil {
			return catalogRootChange{}, decodeErr
		}
		if root.SchemaVersion != 1 || root.UserID != userID.String() || root.Pending != nil {
			return catalogRootChange{}, domain.NewError(domain.ErrorUnavailable, "duplicate summary is changing concurrently")
		}
		pre = root.Current
		revision = envelope.Revision
		expected = envelope.LogicalVersion
	} else if !errors.Is(err, domain.ErrNotFound) {
		return catalogRootChange{}, err
	}
	currentCount := int64(0)
	if pre != nil {
		if pre.GroupID != delta.groupID || pre.Kind != delta.kind || pre.Shard != delta.shard || pre.Size != delta.size || pre.FileCount != delta.fileCount || pre.OccurrenceCount <= 0 {
			return catalogRootChange{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate summary root")
		}
		currentCount = pre.OccurrenceCount
	}
	if delta.delta < 0 && currentCount < -delta.delta || delta.delta > 0 && currentCount > math.MaxInt64-delta.delta {
		return catalogRootChange{}, domain.NewError(domain.ErrorInvalid, "duplicate summary count overflows")
	}
	postCount := currentCount + delta.delta
	if postCount < 0 {
		return catalogRootChange{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate summary is missing an occurrence")
	}
	var post *storageformat.DuplicateSummary
	if postCount > 0 {
		post = &storageformat.DuplicateSummary{GroupID: delta.groupID, Kind: delta.kind, Shard: delta.shard, OccurrenceCount: postCount, Size: delta.size, FileCount: delta.fileCount}
	}
	pendingBody, err := storageformat.EncodeEnvelope(duplicateSummarySchema, key, revision+1, storageformat.DuplicateSummaryRoot{SchemaVersion: 1, UserID: userID.String(), Current: pre, Pending: &storageformat.DuplicateSummaryTransition{OperationID: operationID, Fence: 1, Pre: pre, Post: post}})
	if err != nil {
		return catalogRootChange{}, err
	}
	finalBody, err := storageformat.EncodeEnvelope(duplicateSummarySchema, key, revision+2, storageformat.DuplicateSummaryRoot{SchemaVersion: 1, UserID: userID.String(), Current: post})
	if err != nil {
		return catalogRootChange{}, err
	}
	var rollbackBody []byte
	if preExisted {
		rollbackBody, err = storageformat.EncodeEnvelope(duplicateSummarySchema, key, revision+2, storageformat.DuplicateSummaryRoot{SchemaVersion: 1, UserID: userID.String(), Current: pre})
		if err != nil {
			return catalogRootChange{}, err
		}
	}
	return catalogRootChange{key: key, preExisted: preExisted, expected: expected, pendingBody: pendingBody, finalBody: finalBody, rollbackBody: rollbackBody}, nil
}

func (s *FileStore) visibleDuplicateSummary(ctx context.Context, userID domain.UserID, object objectstore.Object) (*storageformat.DuplicateSummary, error) {
	var envelope storageformat.Envelope
	var root storageformat.DuplicateSummaryRoot
	if err := storageformat.DecodeEnvelope(object.Body, object.Key, duplicateSummarySchema, &envelope, &root); err != nil {
		return nil, err
	}
	if root.SchemaVersion != 1 || root.UserID != userID.String() {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid duplicate summary")
	}
	current := root.Current
	if root.Pending != nil {
		operation, err := s.readFileOperation(ctx, userID, root.Pending.OperationID)
		if err != nil {
			return nil, err
		}
		if operation.Fence < root.Pending.Fence {
			return nil, domain.NewError(domain.ErrorInvalid, "duplicate summary transition fence is invalid")
		}
		if operation.State == storageformat.FileOperationCommitted || operation.State == storageformat.FileOperationSucceeded {
			current = root.Pending.Post
		} else {
			current = root.Pending.Pre
		}
	}
	return current, nil
}

func (s *FileStore) visibleDuplicateOccurrence(ctx context.Context, userID domain.UserID, object objectstore.Object) (*storageformat.DuplicateOccurrence, error) {
	var envelope storageformat.Envelope
	var root storageformat.DuplicateOccurrenceRoot
	if err := storageformat.DecodeEnvelope(object.Body, object.Key, duplicateOccurrenceSchema, &envelope, &root); err != nil {
		return nil, err
	}
	if root.SchemaVersion != 1 || root.UserID != userID.String() {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid duplicate occurrence")
	}
	current := root.Current
	if root.Pending != nil {
		operation, err := s.readFileOperation(ctx, userID, root.Pending.OperationID)
		if err != nil {
			return nil, err
		}
		if operation.Fence < root.Pending.Fence {
			return nil, domain.NewError(domain.ErrorInvalid, "duplicate occurrence transition fence is invalid")
		}
		if operation.State == storageformat.FileOperationCommitted || operation.State == storageformat.FileOperationSucceeded {
			current = root.Pending.Post
		} else {
			current = root.Pending.Pre
		}
	}
	return current, nil
}

func (s *FileStore) ListDuplicateGroups(ctx context.Context, userID domain.UserID, request domain.DuplicateGroupRequest) (domain.DuplicateGroupPage, error) {
	if !userID.Valid() {
		return domain.DuplicateGroupPage{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate group request")
	}
	limit := request.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 1000 || request.Kind != "" && !request.Kind.Valid() {
		return domain.DuplicateGroupPage{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate group page")
	}
	cursor := duplicateGroupCursor{
		SchemaVersion: 1, UserID: userID.String(), KindFilter: request.Kind,
		IncludeIgnored: request.IncludeIgnored, Limit: limit,
		ExpiresAt: s.engine.clock.Now().UTC().Add(s.engine.cursorTTL),
	}
	if request.Cursor != "" {
		if err := s.decodeDuplicateCursor(request.Cursor, &cursor); err != nil || cursor.SchemaVersion != 1 || cursor.UserID != userID.String() || cursor.KindFilter != request.Kind || cursor.IncludeIgnored != request.IncludeIgnored || cursor.Limit != limit || cursor.KindIndex < 0 || cursor.KindIndex > 1 || !s.engine.clock.Now().Before(cursor.ExpiresAt) {
			return domain.DuplicateGroupPage{}, domain.NewError(domain.ErrorInvalid, "invalid or out-of-scope duplicate group cursor")
		}
	}
	kinds := []domain.DuplicateKind{domain.DuplicateFile, domain.DuplicateDirectory}
	if request.Kind.Valid() {
		kinds = []domain.DuplicateKind{request.Kind}
	}
	result := domain.DuplicateGroupPage{Groups: make([]domain.DuplicateGroup, 0, limit)}
	for cursor.KindIndex < len(kinds) {
		kind := kinds[cursor.KindIndex]
		listRequest := objectstore.ListRequest{Prefix: storageformat.DuplicateSummaryPrefix(userID.String(), string(kind)), Limit: 256, After: cursor.After}
		page, err := s.engine.backend.List(ctx, listRequest)
		if err != nil {
			return domain.DuplicateGroupPage{}, err
		}
		for _, info := range page.Objects {
			object, err := s.engine.backend.Get(ctx, info.Key)
			if err != nil {
				return domain.DuplicateGroupPage{}, err
			}
			summary, err := s.visibleDuplicateSummary(ctx, userID, object)
			if err != nil {
				return domain.DuplicateGroupPage{}, err
			}
			cursor.After = info.Key.String()
			if summary == nil {
				continue
			}
			if cursor.Pending == nil {
				cursor.Pending = summary
				continue
			}
			if cursor.Pending.GroupID == summary.GroupID {
				if err := mergeDuplicateSummary(cursor.Pending, summary); err != nil {
					return domain.DuplicateGroupPage{}, err
				}
				continue
			}
			if err := s.appendVisibleDuplicateGroup(ctx, userID, cursor.Pending, request.IncludeIgnored, &result.Groups); err != nil {
				return domain.DuplicateGroupPage{}, err
			}
			cursor.Pending = summary
			if len(result.Groups) == limit {
				next, err := s.encodeDuplicateCursor(cursor)
				if err != nil {
					return domain.DuplicateGroupPage{}, err
				}
				result.NextCursor = next
				return result, nil
			}
		}
		if page.NextCursor != "" {
			continue
		}
		if cursor.Pending != nil {
			if err := s.appendVisibleDuplicateGroup(ctx, userID, cursor.Pending, request.IncludeIgnored, &result.Groups); err != nil {
				return domain.DuplicateGroupPage{}, err
			}
			cursor.Pending = nil
		}
		cursor.After = ""
		cursor.KindIndex++
		if len(result.Groups) == limit && cursor.KindIndex < len(kinds) {
			next, err := s.encodeDuplicateCursor(cursor)
			if err != nil {
				return domain.DuplicateGroupPage{}, err
			}
			result.NextCursor = next
			return result, nil
		}
	}
	return result, nil
}

func mergeDuplicateSummary(target, value *storageformat.DuplicateSummary) error {
	if target == nil || value == nil || target.GroupID != value.GroupID || target.Kind != value.Kind || target.Size != value.Size || target.FileCount != value.FileCount || target.OccurrenceCount <= 0 || value.OccurrenceCount <= 0 || target.OccurrenceCount > math.MaxInt64-value.OccurrenceCount {
		return domain.NewError(domain.ErrorInvalid, "duplicate summary shards disagree")
	}
	target.OccurrenceCount += value.OccurrenceCount
	return nil
}

func (s *FileStore) appendVisibleDuplicateGroup(ctx context.Context, userID domain.UserID, summary *storageformat.DuplicateSummary, includeIgnored bool, groups *[]domain.DuplicateGroup) error {
	if summary == nil || summary.OccurrenceCount < 2 {
		return nil
	}
	group := domain.DuplicateGroup{ID: summary.GroupID, Kind: summary.Kind, OccurrenceCount: summary.OccurrenceCount, Size: summary.Size, FileCount: summary.FileCount}
	ignore, err := s.readDuplicateIgnore(ctx, userID, group.ID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if err == nil {
		group.Ignored, group.IgnoreRevision = ignore.Ignored, ignore.Revision
	}
	if group.Ignored && !includeIgnored {
		return nil
	}
	if group.Size > 0 && group.OccurrenceCount-1 > math.MaxInt64/group.Size {
		return domain.NewError(domain.ErrorInvalid, "duplicate reclaimable bytes overflow")
	}
	group.ReclaimableBytes = (group.OccurrenceCount - 1) * group.Size
	*groups = append(*groups, group)
	return nil
}

func (s *FileStore) ListDuplicateOccurrences(ctx context.Context, userID domain.UserID, request domain.DuplicateOccurrenceRequest) (domain.DuplicateOccurrencePage, error) {
	if !userID.Valid() || validateDuplicateGroupID(request.GroupID) != nil {
		return domain.DuplicateOccurrencePage{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate occurrence request")
	}
	limit := request.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 1000 {
		return domain.DuplicateOccurrencePage{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate occurrence page")
	}
	cursor := duplicateOccurrenceCursor{SchemaVersion: 1, UserID: userID.String(), GroupID: request.GroupID, Limit: limit, ExpiresAt: s.engine.clock.Now().UTC().Add(s.engine.cursorTTL)}
	if request.Cursor != "" {
		if err := s.decodeDuplicateCursor(request.Cursor, &cursor); err != nil || cursor.SchemaVersion != 1 || cursor.UserID != userID.String() || cursor.GroupID != request.GroupID || cursor.Limit != limit || !cursor.Kind.Valid() || !s.engine.clock.Now().Before(cursor.ExpiresAt) {
			return domain.DuplicateOccurrencePage{}, domain.NewError(domain.ErrorInvalid, "invalid or out-of-scope duplicate occurrence cursor")
		}
	}
	result := domain.DuplicateOccurrencePage{Occurrences: make([]domain.DuplicateOccurrence, 0, limit)}
	kinds := []domain.DuplicateKind{domain.DuplicateFile, domain.DuplicateDirectory}
	for _, kind := range kinds {
		if cursor.Kind.Valid() && cursor.Kind != kind {
			continue
		}
		for {
			page, err := s.engine.backend.List(ctx, objectstore.ListRequest{Prefix: storageformat.DuplicateOccurrenceGroupPrefix(userID.String(), string(kind), request.GroupID), Limit: 256, After: cursor.After})
			if err != nil {
				return domain.DuplicateOccurrencePage{}, err
			}
			for index, info := range page.Objects {
				object, err := s.engine.backend.Get(ctx, info.Key)
				if err != nil {
					return domain.DuplicateOccurrencePage{}, err
				}
				occurrence, err := s.visibleDuplicateOccurrence(ctx, userID, object)
				if err != nil {
					return domain.DuplicateOccurrencePage{}, err
				}
				cursor.After = info.Key.String()
				if occurrence == nil {
					continue
				}
				domainOccurrence, err := domainDuplicateOccurrence(*occurrence)
				if err != nil {
					return domain.DuplicateOccurrencePage{}, err
				}
				result.Occurrences = append(result.Occurrences, domainOccurrence)
				cursor.Kind = kind
				if len(result.Occurrences) == limit {
					if index+1 < len(page.Objects) || page.NextCursor != "" {
						next, err := s.encodeDuplicateCursor(cursor)
						if err != nil {
							return domain.DuplicateOccurrencePage{}, err
						}
						result.NextCursor = next
					}
					return result, nil
				}
			}
			if page.NextCursor == "" {
				if len(result.Occurrences) != 0 {
					return result, nil
				}
				break
			}
		}
		cursor.After = ""
	}
	return result, nil
}

func (s *FileStore) encodeDuplicateCursor(value any) (string, error) {
	body, err := storageformat.EncodeCanonical(value)
	if err != nil {
		return "", err
	}
	random, err := s.engine.ids.BearerToken()
	if err != nil {
		return "", err
	}
	nonceMaterial, err := base64.RawURLEncoding.DecodeString(random)
	if err != nil || len(nonceMaterial) < s.engine.cursorAEAD.NonceSize() {
		return "", domain.NewError(domain.ErrorInternal, "secure duplicate cursor randomness unavailable")
	}
	nonce := nonceMaterial[:s.engine.cursorAEAD.NonceSize()]
	sealed := s.engine.cursorAEAD.Seal(append([]byte(nil), nonce...), nonce, body, []byte("endlessfs-duplicate-cursor-v1"))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (s *FileStore) decodeDuplicateCursor(value string, destination any) error {
	sealed, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(sealed) <= s.engine.cursorAEAD.NonceSize() {
		return domain.NewError(domain.ErrorInvalid, "invalid duplicate cursor")
	}
	nonceSize := s.engine.cursorAEAD.NonceSize()
	body, err := s.engine.cursorAEAD.Open(nil, sealed[:nonceSize], sealed[nonceSize:], []byte("endlessfs-duplicate-cursor-v1"))
	if err != nil {
		return domain.NewError(domain.ErrorInvalid, "invalid duplicate cursor")
	}
	return decodeCanonicalValue(body, destination)
}

func domainDuplicateOccurrence(value storageformat.DuplicateOccurrence) (domain.DuplicateOccurrence, error) {
	path, err := domain.ParseUserPath(value.Path)
	if err != nil || path.IsRoot() || !value.Kind.Valid() || value.Size < 0 || value.FileCount < 0 || value.Version == "" || validateDuplicateGroupID(value.GroupID) != nil {
		return domain.DuplicateOccurrence{}, domain.NewError(domain.ErrorInvalid, "invalid stored duplicate occurrence")
	}
	area := domain.AreaLive
	if value.Area == "trash" {
		area = domain.AreaTrash
	} else if value.Area != "live" {
		return domain.DuplicateOccurrence{}, domain.NewError(domain.ErrorInvalid, "invalid stored duplicate occurrence area")
	}
	return domain.DuplicateOccurrence{GroupID: value.GroupID, Kind: value.Kind, Area: area, AreaName: value.Area, Path: path, Size: value.Size, FileCount: value.FileCount, Version: domain.Version(value.Version)}, nil
}

func (s *FileStore) SetDuplicateGroupIgnored(ctx context.Context, userID domain.UserID, request domain.SetDuplicateIgnoredRequest) (domain.DuplicateIgnore, error) {
	if !userID.Valid() || validateDuplicateGroupID(request.GroupID) != nil {
		return domain.DuplicateIgnore{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate ignore request")
	}
	key := storageformat.DuplicateIgnoreKey(userID.String(), request.GroupID)
	object, err := s.engine.backend.Get(ctx, key)
	revision := uint64(1)
	condition := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
	if err == nil {
		var envelope storageformat.Envelope
		var current storageformat.DuplicateIgnore
		if decodeErr := storageformat.DecodeEnvelope(object.Body, key, duplicateIgnoreSchema, &envelope, &current); decodeErr != nil {
			return domain.DuplicateIgnore{}, decodeErr
		}
		if current.SchemaVersion != 1 || current.UserID != userID.String() || current.GroupID != request.GroupID || current.Revision == 0 {
			return domain.DuplicateIgnore{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate ignore record")
		}
		if request.ExpectedRevision != 0 && request.ExpectedRevision != current.Revision {
			return domain.DuplicateIgnore{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate ignore revision changed")
		}
		if current.Ignored == request.Ignored {
			return domain.DuplicateIgnore{GroupID: current.GroupID, Ignored: current.Ignored, Revision: current.Revision}, nil
		}
		revision = current.Revision + 1
		condition = objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.DuplicateIgnore{}, err
	} else if request.ExpectedRevision != 0 {
		return domain.DuplicateIgnore{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate ignore record is missing")
	}
	record := storageformat.DuplicateIgnore{SchemaVersion: 1, UserID: userID.String(), GroupID: request.GroupID, Ignored: request.Ignored, Revision: revision}
	body, err := storageformat.EncodeEnvelope(duplicateIgnoreSchema, key, revision, record)
	if err != nil {
		return domain.DuplicateIgnore{}, err
	}
	intent := storageformat.MutationIntent{Action: storageformat.MutationCreate, TargetKey: key.String(), TargetBody: body}
	if condition.Mode == objectstore.PutMatch {
		intent.Action = storageformat.MutationCAS
		intent.ExpectedLogicalVersion, err = canonicalLogicalVersion(object.Body)
		if err != nil {
			return domain.DuplicateIgnore{}, err
		}
	}
	err = s.engine.withAdmission(ctx, intent, func() error {
		_, putErr := s.engine.backend.Put(ctx, key, body, condition)
		return putErr
	})
	if err != nil {
		return domain.DuplicateIgnore{}, err
	}
	return domain.DuplicateIgnore{GroupID: request.GroupID, Ignored: request.Ignored, Revision: revision}, nil
}

func (s *FileStore) readDuplicateIgnore(ctx context.Context, userID domain.UserID, groupID string) (storageformat.DuplicateIgnore, error) {
	key := storageformat.DuplicateIgnoreKey(userID.String(), groupID)
	object, err := s.engine.backend.Get(ctx, key)
	if err != nil {
		return storageformat.DuplicateIgnore{}, err
	}
	var envelope storageformat.Envelope
	var record storageformat.DuplicateIgnore
	if err := storageformat.DecodeEnvelope(object.Body, key, duplicateIgnoreSchema, &envelope, &record); err != nil {
		return storageformat.DuplicateIgnore{}, err
	}
	if record.SchemaVersion != 1 || record.UserID != userID.String() || record.GroupID != groupID || record.Revision == 0 {
		return storageformat.DuplicateIgnore{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate ignore record")
	}
	return record, nil
}

func (s *FileStore) CompareDuplicateDirectories(ctx context.Context, userID domain.UserID, request domain.DuplicateDirectoryComparisonRequest) (domain.DuplicateDirectoryComparison, error) {
	if !userID.Valid() || request.Left.Area != domain.AreaLive && request.Left.Area != domain.AreaTrash || request.Right.Area != domain.AreaLive && request.Right.Area != domain.AreaTrash || !request.Left.Path.Valid() || !request.Right.Path.Valid() {
		return domain.DuplicateDirectoryComparison{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate directory comparison")
	}
	leftScope, _ := domain.NewScope(userID, request.Left.Area)
	rightScope, _ := domain.NewScope(userID, request.Right.Area)
	leftOccurrence, leftCounts, err := s.directoryComparisonInventory(ctx, leftScope, request.Left.Path)
	if err != nil {
		return domain.DuplicateDirectoryComparison{}, err
	}
	rightOccurrence, rightCounts, err := s.directoryComparisonInventory(ctx, rightScope, request.Right.Path)
	if err != nil {
		return domain.DuplicateDirectoryComparison{}, err
	}
	result := domain.DuplicateDirectoryComparison{Left: leftOccurrence, Right: rightOccurrence, Exact: leftOccurrence.GroupID == rightOccurrence.GroupID}
	all := make(map[string]struct{}, len(leftCounts)+len(rightCounts))
	for group := range leftCounts {
		all[group] = struct{}{}
	}
	for group := range rightCounts {
		all[group] = struct{}{}
	}
	for group := range all {
		left := leftCounts[group]
		right := rightCounts[group]
		common := min(left.count, right.count)
		result.CommonFiles += common
		result.CommonBytes += common * left.size
		result.LeftOnlyFiles += left.count - common
		result.LeftOnlyBytes += (left.count - common) * left.size
		result.RightOnlyFiles += right.count - common
		result.RightOnlyBytes += (right.count - common) * right.size
	}
	return result, nil
}

type duplicateFileCount struct {
	count int64
	size  int64
}

func (s *FileStore) directoryComparisonInventory(ctx context.Context, scope domain.Scope, path domain.UserPath) (domain.DuplicateOccurrence, map[string]duplicateFileCount, error) {
	occurrence, files, _, err := s.directoryReconciliationInventory(ctx, scope, path)
	if err != nil {
		return domain.DuplicateOccurrence{}, nil, err
	}
	counts := make(map[string]duplicateFileCount)
	for groupID, occurrences := range files {
		if len(occurrences) == 0 {
			continue
		}
		counts[groupID] = duplicateFileCount{count: int64(len(occurrences)), size: occurrences[0].Size}
	}
	return occurrence, counts, nil
}

func (s *FileStore) directoryReconciliationInventory(ctx context.Context, scope domain.Scope, path domain.UserPath) (domain.DuplicateOccurrence, map[string][]domain.DuplicateOccurrence, string, error) {
	view, err := s.resolveDirectoryMetadataView(ctx, scope, path)
	if err != nil {
		return domain.DuplicateOccurrence{}, nil, "", err
	}
	leaf, err := s.readDirectory(ctx, scope, view.directoryID, path.IsRoot())
	if err != nil {
		return domain.DuplicateOccurrence{}, nil, "", err
	}
	version := view.current.Version
	if path.IsRoot() {
		version = domain.Version("root")
	}
	groupID, err := duplicateDirectoryGroupID(leaf.recursiveBytes, leaf.recursiveFileCount, leaf.contentDigest)
	if err != nil {
		return domain.DuplicateOccurrence{}, nil, "", err
	}
	rootOccurrence := domain.DuplicateOccurrence{GroupID: groupID, Kind: domain.DuplicateDirectory, Area: scope.Area(), AreaName: areaName(scope.Area()), Path: path, Size: leaf.recursiveBytes, FileCount: leaf.recursiveFileCount, Version: version}
	files := make(map[string][]domain.DuplicateOccurrence)
	var walk func(domain.UserPath, directorySnapshot) error
	walk = func(currentPath domain.UserPath, snapshot directorySnapshot) error {
		for _, entry := range snapshot.entries {
			childPath, err := currentPath.Join(entry.Name)
			if err != nil {
				return err
			}
			if entry.Kind == domain.EntryDirectory {
				child, err := s.readDirectory(ctx, scope, entry.DirectoryID, false)
				if err != nil {
					return err
				}
				if child.recursiveBytes != entry.Size || child.recursiveFileCount != entry.FileCount || child.contentDigest != entry.ContentDigest {
					return domain.NewError(domain.ErrorInvalid, "directory comparison encountered a stale aggregate")
				}
				if err := walk(childPath, child); err != nil {
					return err
				}
				continue
			}
			stored, err := catalogOccurrence(scope, childPath, entry)
			if err != nil {
				return err
			}
			occurrence, err := domainDuplicateOccurrence(stored)
			if err != nil {
				return err
			}
			files[occurrence.GroupID] = append(files[occurrence.GroupID], occurrence)
		}
		return nil
	}
	if err := walk(path, leaf); err != nil {
		return domain.DuplicateOccurrence{}, nil, "", err
	}
	for groupID := range files {
		sort.Slice(files[groupID], func(i, j int) bool {
			return duplicateOccurrenceLocation(files[groupID][i]) < duplicateOccurrenceLocation(files[groupID][j])
		})
	}
	return rootOccurrence, files, leaf.manifestID, nil
}

func duplicateOccurrenceLocation(value domain.DuplicateOccurrence) string {
	return value.AreaName + "\x00" + value.Path.String()
}

func (s *FileStore) PreviewDuplicateReconciliation(ctx context.Context, userID domain.UserID, request domain.DuplicateReconciliationPreviewRequest) (domain.DuplicateReconciliationPreview, error) {
	if !userID.Valid() || !request.RemoveFrom.Valid() || request.Left.Area != domain.AreaLive || request.Right.Area != domain.AreaLive || !request.Left.Path.Valid() || !request.Right.Path.Valid() || request.Left.Path == request.Right.Path || request.Left.Path.IsDescendantOf(request.Right.Path) || request.Right.Path.IsDescendantOf(request.Left.Path) {
		return domain.DuplicateReconciliationPreview{}, domain.NewError(domain.ErrorInvalid, "duplicate reconciliation requires two disjoint live directories")
	}
	limit := request.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 100 {
		return domain.DuplicateReconciliationPreview{}, domain.NewError(domain.ErrorInvalid, "duplicate reconciliation page limit must be between 1 and 100")
	}
	_, gateEnvelope, gate, err := s.engine.readGate(ctx)
	if err != nil {
		return domain.DuplicateReconciliationPreview{}, err
	}
	leftScope, _ := domain.NewScope(userID, request.Left.Area)
	rightScope, _ := domain.NewScope(userID, request.Right.Area)
	left, leftFiles, leftManifest, err := s.directoryReconciliationInventory(ctx, leftScope, request.Left.Path)
	if err != nil {
		return domain.DuplicateReconciliationPreview{}, err
	}
	right, rightFiles, rightManifest, err := s.directoryReconciliationInventory(ctx, rightScope, request.Right.Path)
	if err != nil {
		return domain.DuplicateReconciliationPreview{}, err
	}
	comparison, err := compareDirectoryInventories(left, right, leftFiles, rightFiles)
	if err != nil {
		return domain.DuplicateReconciliationPreview{}, err
	}
	cursor := duplicateReconciliationCursor{
		SchemaVersion: 1, UserID: userID.String(), Left: request.Left, Right: request.Right, RemoveFrom: request.RemoveFrom, Limit: limit,
		LeftManifest: leftManifest, RightManifest: rightManifest, GateEpoch: gate.Epoch, GateVersion: gateEnvelope.LogicalVersion,
		ExpiresAt: s.engine.clock.Now().UTC().Add(s.engine.cursorTTL),
	}
	if request.Cursor != "" {
		if err := s.decodeDuplicateCursor(request.Cursor, &cursor); err != nil || cursor.SchemaVersion != 1 || cursor.UserID != userID.String() || cursor.Left != request.Left || cursor.Right != request.Right || cursor.RemoveFrom != request.RemoveFrom || cursor.Limit != limit || cursor.LeftManifest != leftManifest || cursor.RightManifest != rightManifest || cursor.GateEpoch != gate.Epoch || cursor.GateVersion != gateEnvelope.LogicalVersion || !s.engine.clock.Now().Before(cursor.ExpiresAt) {
			return domain.DuplicateReconciliationPreview{}, domain.NewError(domain.ErrorInvalid, "invalid or stale duplicate reconciliation cursor")
		}
	}
	items, err := s.reconciliationItems(ctx, userID, left, right, leftFiles, rightFiles, request.RemoveFrom)
	if err != nil {
		return domain.DuplicateReconciliationPreview{}, err
	}
	start := 0
	if cursor.After != "" {
		start = sort.Search(len(items), func(index int) bool { return duplicateOccurrenceLocation(items[index].Remove) > cursor.After })
	}
	end := min(start+limit, len(items))
	pageItems := append([]domain.DuplicateReconciliationItem(nil), items[start:end]...)
	result := domain.DuplicateReconciliationPreview{Comparison: comparison, RemoveFrom: request.RemoveFrom, Items: pageItems}
	for _, item := range pageItems {
		if item.Remove.Size > math.MaxInt64-result.ReclaimableBytes {
			return domain.DuplicateReconciliationPreview{}, domain.NewError(domain.ErrorInvalid, "duplicate reconciliation bytes overflow")
		}
		result.ReclaimableBytes += item.Remove.Size
	}
	if end < len(items) {
		cursor.After = duplicateOccurrenceLocation(items[end-1].Remove)
		result.NextCursor, err = s.encodeDuplicateCursor(cursor)
		if err != nil {
			return domain.DuplicateReconciliationPreview{}, err
		}
	}
	if len(pageItems) != 0 {
		result.PlanToken, err = s.encodeDuplicateCursor(duplicateReconciliationPlan{
			SchemaVersion: 1, UserID: userID.String(), Left: left, Right: right, RemoveFrom: request.RemoveFrom, Items: pageItems,
			LeftManifest: leftManifest, RightManifest: rightManifest, GateEpoch: gate.Epoch, GateVersion: gateEnvelope.LogicalVersion,
			ExpiresAt: s.engine.clock.Now().UTC().Add(s.engine.cursorTTL),
		})
		if err != nil {
			return domain.DuplicateReconciliationPreview{}, err
		}
	}
	return result, nil
}

func compareDirectoryInventories(left, right domain.DuplicateOccurrence, leftFiles, rightFiles map[string][]domain.DuplicateOccurrence) (domain.DuplicateDirectoryComparison, error) {
	result := domain.DuplicateDirectoryComparison{Left: left, Right: right, Exact: left.GroupID == right.GroupID}
	groups := make(map[string]struct{}, len(leftFiles)+len(rightFiles))
	for groupID := range leftFiles {
		groups[groupID] = struct{}{}
	}
	for groupID := range rightFiles {
		groups[groupID] = struct{}{}
	}
	for groupID := range groups {
		leftValues, rightValues := leftFiles[groupID], rightFiles[groupID]
		size := int64(0)
		if len(leftValues) != 0 {
			size = leftValues[0].Size
		}
		if len(rightValues) != 0 && (size != 0 && size != rightValues[0].Size) {
			return domain.DuplicateDirectoryComparison{}, domain.NewError(domain.ErrorInvalid, "duplicate file identity size mismatch")
		}
		if size == 0 && len(rightValues) != 0 {
			size = rightValues[0].Size
		}
		common := min(int64(len(leftValues)), int64(len(rightValues)))
		result.CommonFiles += common
		result.CommonBytes += common * size
		result.LeftOnlyFiles += int64(len(leftValues)) - common
		result.LeftOnlyBytes += (int64(len(leftValues)) - common) * size
		result.RightOnlyFiles += int64(len(rightValues)) - common
		result.RightOnlyBytes += (int64(len(rightValues)) - common) * size
	}
	return result, nil
}

func (s *FileStore) reconciliationItems(ctx context.Context, userID domain.UserID, left, right domain.DuplicateOccurrence, leftFiles, rightFiles map[string][]domain.DuplicateOccurrence, removeFrom domain.DuplicateSide) ([]domain.DuplicateReconciliationItem, error) {
	if left.GroupID == right.GroupID {
		ignored, revision, err := s.duplicateGroupIgnoreState(ctx, userID, left.GroupID)
		if err != nil || ignored {
			return nil, err
		}
		remove, keep := left, right
		if removeFrom == domain.DuplicateSideRight {
			remove, keep = right, left
		}
		return []domain.DuplicateReconciliationItem{{GroupID: left.GroupID, Remove: remove, Keep: keep, IgnoreRevision: revision}}, nil
	}
	removeFiles, keepFiles := leftFiles, rightFiles
	if removeFrom == domain.DuplicateSideRight {
		removeFiles, keepFiles = rightFiles, leftFiles
	}
	groups := make([]string, 0, len(removeFiles))
	for groupID := range removeFiles {
		if len(keepFiles[groupID]) != 0 {
			groups = append(groups, groupID)
		}
	}
	sort.Strings(groups)
	var result []domain.DuplicateReconciliationItem
	for _, groupID := range groups {
		ignored, revision, err := s.duplicateGroupIgnoreState(ctx, userID, groupID)
		if err != nil {
			return nil, err
		}
		if ignored {
			continue
		}
		removeValues, keepValues := removeFiles[groupID], keepFiles[groupID]
		count := min(len(removeValues), len(keepValues))
		for index := 0; index < count; index++ {
			result = append(result, domain.DuplicateReconciliationItem{GroupID: groupID, Remove: removeValues[index], Keep: keepValues[index], IgnoreRevision: revision})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return duplicateOccurrenceLocation(result[i].Remove) < duplicateOccurrenceLocation(result[j].Remove)
	})
	return result, nil
}

func (s *FileStore) duplicateGroupIgnoreState(ctx context.Context, userID domain.UserID, groupID string) (bool, uint64, error) {
	ignore, err := s.readDuplicateIgnore(ctx, userID, groupID)
	if errors.Is(err, domain.ErrNotFound) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	return ignore.Ignored, ignore.Revision, nil
}

func (s *FileStore) ValidateDuplicateReconciliation(ctx context.Context, userID domain.UserID, token string) (domain.DuplicateReconciliationSelection, error) {
	if !userID.Valid() || token == "" {
		return domain.DuplicateReconciliationSelection{}, domain.NewError(domain.ErrorInvalid, "duplicate reconciliation plan is required")
	}
	var plan duplicateReconciliationPlan
	if err := s.decodeDuplicateCursor(token, &plan); err != nil || plan.SchemaVersion != 1 || plan.UserID != userID.String() || !plan.RemoveFrom.Valid() || len(plan.Items) < 1 || len(plan.Items) > 100 || !s.engine.clock.Now().Before(plan.ExpiresAt) {
		return domain.DuplicateReconciliationSelection{}, domain.NewError(domain.ErrorInvalid, "invalid or expired duplicate reconciliation plan")
	}
	if err := restoreDuplicateOccurrenceAreas(&plan); err != nil {
		return domain.DuplicateReconciliationSelection{}, err
	}
	_, gateEnvelope, gate, err := s.engine.readGate(ctx)
	if err != nil {
		return domain.DuplicateReconciliationSelection{}, err
	}
	if gate.Epoch != plan.GateEpoch || gateEnvelope.LogicalVersion != plan.GateVersion {
		return domain.DuplicateReconciliationSelection{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate reconciliation gate changed")
	}
	for location, manifestID := range map[domain.DuplicateLocation]string{{Area: plan.Left.Area, Path: plan.Left.Path}: plan.LeftManifest, {Area: plan.Right.Area, Path: plan.Right.Path}: plan.RightManifest} {
		scope, _ := domain.NewScope(userID, location.Area)
		view, err := s.resolveDirectoryMetadataView(ctx, scope, location.Path)
		if err != nil {
			return domain.DuplicateReconciliationSelection{}, err
		}
		if view.snapshot.manifestID != manifestID {
			return domain.DuplicateReconciliationSelection{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate reconciliation directory changed")
		}
	}
	seen := make(map[string]struct{}, len(plan.Items))
	for _, item := range plan.Items {
		if item.GroupID == "" || item.Remove.GroupID != item.GroupID || item.Keep.GroupID != item.GroupID || item.Remove.Area != domain.AreaLive || item.Keep.Area != domain.AreaLive || item.Remove.Path == item.Keep.Path {
			return domain.DuplicateReconciliationSelection{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate reconciliation item")
		}
		location := duplicateOccurrenceLocation(item.Remove)
		if _, duplicate := seen[location]; duplicate {
			return domain.DuplicateReconciliationSelection{}, domain.NewError(domain.ErrorInvalid, "duplicate reconciliation item repeats a removal")
		}
		seen[location] = struct{}{}
		ignored, revision, err := s.duplicateGroupIgnoreState(ctx, userID, item.GroupID)
		if err != nil {
			return domain.DuplicateReconciliationSelection{}, err
		}
		if ignored || revision != item.IgnoreRevision {
			return domain.DuplicateReconciliationSelection{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate reconciliation ignore policy changed")
		}
		for _, occurrence := range []domain.DuplicateOccurrence{item.Remove, item.Keep} {
			scope, _ := domain.NewScope(userID, occurrence.Area)
			entry, err := s.resolveEntry(ctx, scope, occurrence.Path)
			if err != nil {
				return domain.DuplicateReconciliationSelection{}, err
			}
			stored, err := catalogOccurrence(scope, occurrence.Path, entry)
			if err != nil {
				return domain.DuplicateReconciliationSelection{}, err
			}
			if stored.GroupID != item.GroupID || entry.LogicalVersion != string(occurrence.Version) {
				return domain.DuplicateReconciliationSelection{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate reconciliation occurrence changed")
			}
		}
	}
	return domain.DuplicateReconciliationSelection{Left: plan.Left, Right: plan.Right, RemoveFrom: plan.RemoveFrom, Items: append([]domain.DuplicateReconciliationItem(nil), plan.Items...)}, nil
}

func restoreDuplicateOccurrenceAreas(plan *duplicateReconciliationPlan) error {
	values := []*domain.DuplicateOccurrence{&plan.Left, &plan.Right}
	for index := range plan.Items {
		values = append(values, &plan.Items[index].Remove, &plan.Items[index].Keep)
	}
	for _, value := range values {
		switch value.AreaName {
		case "live":
			value.Area = domain.AreaLive
		case "trash":
			value.Area = domain.AreaTrash
		default:
			return domain.NewError(domain.ErrorInvalid, "invalid duplicate reconciliation occurrence area")
		}
	}
	return nil
}

func (e *Engine) rebuildDuplicateCatalog(ctx context.Context) error {
	type rootScope struct {
		scope domain.Scope
		key   string
	}
	scopes := make(map[string]rootScope)
	users := make(map[string]domain.UserID)
	if err := visitObjectPages(ctx, e.backend, storageformat.FilesystemPrefix(), func(info objectstore.ObjectInfo) error {
		userValue, areaValue, directoryID, matched, err := storageformat.ParseDirectoryRootKey(info.Key)
		if err != nil {
			return err
		}
		if !matched || directoryID != storageformat.RootDirectoryID {
			return nil
		}
		userID, err := domain.ParseUserID(userValue)
		if err != nil {
			return err
		}
		area := domain.AreaLive
		if areaValue == "trash" {
			area = domain.AreaTrash
		}
		scope, err := domain.NewScope(userID, area)
		if err != nil {
			return err
		}
		key := userValue + "\x00" + areaValue
		scopes[key] = rootScope{scope: scope, key: key}
		users[userValue] = userID
		return nil
	}); err != nil {
		return err
	}
	ordered := make([]string, 0, len(scopes))
	for key := range scopes {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	for _, key := range ordered {
		scope := scopes[key].scope
		root, err := e.Files().readDirectory(ctx, scope, storageformat.RootDirectoryID, false)
		if err != nil {
			return err
		}
		if err := e.migrateDuplicateDirectory(ctx, scope, domain.MustParseUserPath("/"), root); err != nil {
			return err
		}
	}
	userValues := make([]string, 0, len(users))
	for userValue := range users {
		userValues = append(userValues, userValue)
	}
	sort.Strings(userValues)
	for _, userValue := range userValues {
		if err := e.rebuildDuplicateSummaries(ctx, users[userValue]); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) migrateDuplicateDirectory(ctx context.Context, scope domain.Scope, path domain.UserPath, snapshot directorySnapshot) error {
	for _, entry := range snapshot.entries {
		childPath, err := path.Join(entry.Name)
		if err != nil {
			return err
		}
		occurrence, err := catalogOccurrence(scope, childPath, entry)
		if err != nil {
			return err
		}
		if err := e.ensureMigratedDuplicateOccurrence(ctx, scope.UserID(), occurrence); err != nil {
			return err
		}
		if entry.Kind != domain.EntryDirectory {
			continue
		}
		child, err := e.Files().readDirectory(ctx, scope, entry.DirectoryID, false)
		if err != nil {
			return err
		}
		if child.recursiveBytes != entry.Size || child.recursiveFileCount != entry.FileCount || child.contentDigest != entry.ContentDigest {
			return domain.NewError(domain.ErrorInvalid, "duplicate migration encountered a stale directory aggregate")
		}
		if err := e.migrateDuplicateDirectory(ctx, scope, childPath, child); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) ensureMigratedDuplicateOccurrence(ctx context.Context, userID domain.UserID, occurrence storageformat.DuplicateOccurrence) error {
	key := duplicateOccurrenceKey(userID.String(), occurrence)
	body, err := storageformat.EncodeEnvelope(duplicateOccurrenceSchema, key, 1, storageformat.DuplicateOccurrenceRoot{SchemaVersion: 1, UserID: userID.String(), Current: &occurrence})
	if err != nil {
		return err
	}
	for range 2 {
		object, err := e.backend.Get(ctx, key)
		if errors.Is(err, domain.ErrNotFound) {
			if _, err := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err == nil {
				return nil
			} else if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPreconditionFailed) {
				continue
			} else {
				return err
			}
		}
		if err != nil {
			return err
		}
		current, err := e.Files().visibleDuplicateOccurrence(ctx, userID, object)
		if err != nil {
			return err
		}
		if current == nil || *current != occurrence {
			return domain.NewError(domain.ErrorInvalid, "migrated duplicate occurrence disagrees with the filesystem")
		}
		return nil
	}
	return domain.NewError(domain.ErrorUnavailable, "duplicate occurrence migration remained contended")
}

func (e *Engine) rebuildDuplicateSummaries(ctx context.Context, userID domain.UserID) error {
	for _, kind := range []domain.DuplicateKind{domain.DuplicateFile, domain.DuplicateDirectory} {
		request := objectstore.ListRequest{Prefix: storageformat.DuplicateOccurrenceOwnerPrefix(userID.String()) + string(kind) + "/occurrences/", Limit: 1000}
		currentGroup := ""
		shards := make(map[string]storageformat.DuplicateSummary)
		flush := func() error {
			if currentGroup == "" {
				return nil
			}
			keys := make([]string, 0, len(shards))
			for shard := range shards {
				keys = append(keys, shard)
			}
			sort.Strings(keys)
			for _, shard := range keys {
				if err := e.ensureMigratedDuplicateSummary(ctx, userID, shards[shard]); err != nil {
					return err
				}
			}
			return nil
		}
		for {
			page, err := e.backend.List(ctx, request)
			if err != nil {
				return err
			}
			for _, info := range page.Objects {
				object, err := e.backend.Get(ctx, info.Key)
				if err != nil {
					return err
				}
				occurrence, err := e.Files().visibleDuplicateOccurrence(ctx, userID, object)
				if err != nil {
					return err
				}
				if occurrence == nil {
					return domain.NewError(domain.ErrorInvalid, "schema migration encountered a duplicate occurrence tombstone")
				}
				if currentGroup != "" && occurrence.GroupID != currentGroup {
					if err := flush(); err != nil {
						return err
					}
					shards = make(map[string]storageformat.DuplicateSummary)
				}
				currentGroup = occurrence.GroupID
				shard := duplicateSummaryShard(*occurrence)
				summary := shards[shard]
				if summary.GroupID != "" && (summary.GroupID != occurrence.GroupID || summary.Kind != occurrence.Kind || summary.Size != occurrence.Size || summary.FileCount != occurrence.FileCount) {
					return domain.NewError(domain.ErrorInvalid, "duplicate occurrence group identity mismatch")
				}
				summary.GroupID, summary.Kind, summary.Shard, summary.Size, summary.FileCount = occurrence.GroupID, occurrence.Kind, shard, occurrence.Size, occurrence.FileCount
				summary.OccurrenceCount++
				shards[shard] = summary
			}
			if page.NextCursor == "" {
				break
			}
			request.Cursor = page.NextCursor
		}
		if err := flush(); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) ensureMigratedDuplicateSummary(ctx context.Context, userID domain.UserID, summary storageformat.DuplicateSummary) error {
	key := storageformat.DuplicateSummaryKey(userID.String(), string(summary.Kind), summary.GroupID, summary.Shard)
	body, err := storageformat.EncodeEnvelope(duplicateSummarySchema, key, 1, storageformat.DuplicateSummaryRoot{SchemaVersion: 1, UserID: userID.String(), Current: &summary})
	if err != nil {
		return err
	}
	for range 2 {
		object, err := e.backend.Get(ctx, key)
		if errors.Is(err, domain.ErrNotFound) {
			if _, err := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err == nil {
				return nil
			} else if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPreconditionFailed) {
				continue
			} else {
				return err
			}
		}
		if err != nil {
			return err
		}
		current, err := e.Files().visibleDuplicateSummary(ctx, userID, object)
		if err != nil {
			return err
		}
		if current == nil || *current != summary {
			return domain.NewError(domain.ErrorInvalid, "migrated duplicate summary disagrees with the occurrence index")
		}
		return nil
	}
	return domain.NewError(domain.ErrorUnavailable, "duplicate summary migration remained contended")
}
