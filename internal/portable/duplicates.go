package portable

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const (
	duplicateOccurrenceSchema      = "duplicate-occurrence-root-v1"
	duplicateSummarySchema         = "duplicate-summary-root-v1"
	duplicateIgnoreSchema          = "duplicate-ignore-v1"
	duplicateDirectoryIgnoreSchema = "duplicate-directory-ignore-v1"
	duplicateSimilaritySchema      = "duplicate-similarity-posting-root-v1"
)

type catalogChange struct {
	pre            *storageformat.DuplicateOccurrence
	post           *storageformat.DuplicateOccurrence
	similarityPre  []storageformat.DuplicateSimilarityPosting
	similarityPost []storageformat.DuplicateSimilarityPosting
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

type duplicateOverlapCursor struct {
	SchemaVersion  int                      `json:"schemaVersion"`
	UserID         string                   `json:"userID"`
	Directory      domain.DuplicateLocation `json:"directory"`
	IncludeIgnored bool                     `json:"includeIgnored"`
	Limit          int                      `json:"limit"`
	ManifestID     string                   `json:"manifestID"`
	Position       int                      `json:"position"`
	After          string                   `json:"after,omitempty"`
	GateEpoch      uint64                   `json:"gateEpoch"`
	GateVersion    string                   `json:"gateVersion"`
	ExpiresAt      time.Time                `json:"expiresAt"`
}

type duplicateReconciliationCursor struct {
	SchemaVersion int                                 `json:"schemaVersion"`
	UserID        string                              `json:"userID"`
	Left          domain.DuplicateLocation            `json:"left"`
	Right         domain.DuplicateLocation            `json:"right"`
	RemoveFrom    domain.DuplicateSide                `json:"removeFrom"`
	Limit         int                                 `json:"limit"`
	LeftAfter     string                              `json:"leftAfter,omitempty"`
	RightAfter    string                              `json:"rightAfter,omitempty"`
	Comparison    domain.DuplicateDirectoryComparison `json:"comparison"`
	LeftManifest  string                              `json:"leftManifest"`
	RightManifest string                              `json:"rightManifest"`
	GateEpoch     uint64                              `json:"gateEpoch"`
	GateVersion   string                              `json:"gateVersion"`
	ExpiresAt     time.Time                           `json:"expiresAt"`
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

func validateDuplicateSimilarityPosting(value storageformat.DuplicateSimilarityPosting) error {
	path, err := domain.ParseUserPath(value.Path)
	if err != nil || value.Position < 0 || value.Position >= directoryContentSketchSize || validateDuplicateGroupID(value.SketchValue) != nil || value.Area != "live" && value.Area != "trash" || value.DirectoryID == "" || !path.Valid() {
		return domain.NewError(domain.ErrorInvalid, "invalid duplicate similarity posting")
	}
	return nil
}

func duplicateSimilarityPostingKey(userID string, value storageformat.DuplicateSimilarityPosting) objectstore.Key {
	return storageformat.DuplicateSimilarityPostingKey(userID, value.Position, value.SketchValue, value.Area, value.DirectoryID)
}

func duplicateSimilarityPostings(scope domain.Scope, path domain.UserPath, directoryID string, sketch []string) ([]storageformat.DuplicateSimilarityPosting, error) {
	if !scope.Valid() || !path.Valid() || directoryID == "" || validateDirectoryContentSketch(sketch) != nil {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid directory similarity source")
	}
	result := make([]storageformat.DuplicateSimilarityPosting, 0, len(sketch))
	for position, value := range sketch {
		posting := storageformat.DuplicateSimilarityPosting{
			Position: position, SketchValue: value, Area: areaName(scope.Area()), DirectoryID: directoryID,
			Path: path.String(),
		}
		if err := validateDuplicateSimilarityPosting(posting); err != nil {
			return nil, err
		}
		result = append(result, posting)
	}
	return result, nil
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
	similarityRoots, err := s.buildSimilarityOperationRoots(ctx, userID, operationID, changes)
	if err != nil {
		return nil, err
	}
	result = append(result, similarityRoots...)
	return result, nil
}

func (s *FileStore) buildSimilarityOperationRoots(ctx context.Context, userID domain.UserID, operationID string, changes []catalogChange) ([]storageformat.FileOperationRoot, error) {
	type update struct {
		pre  *storageformat.DuplicateSimilarityPosting
		post *storageformat.DuplicateSimilarityPosting
	}
	updates := make(map[string]update)
	add := func(value storageformat.DuplicateSimilarityPosting, before bool) error {
		if err := validateDuplicateSimilarityPosting(value); err != nil {
			return err
		}
		key := duplicateSimilarityPostingKey(userID.String(), value).String()
		current := updates[key]
		copy := value
		if before {
			if current.pre != nil && !reflect.DeepEqual(current.pre, &copy) {
				return domain.NewError(domain.ErrorInvalid, "conflicting similarity posting removal")
			}
			current.pre = &copy
		} else {
			if current.post != nil && !reflect.DeepEqual(current.post, &copy) {
				return domain.NewError(domain.ErrorInvalid, "conflicting similarity posting addition")
			}
			current.post = &copy
		}
		updates[key] = current
		return nil
	}
	for _, change := range changes {
		for _, value := range change.similarityPre {
			if err := add(value, true); err != nil {
				return nil, err
			}
		}
		for _, value := range change.similarityPost {
			if err := add(value, false); err != nil {
				return nil, err
			}
		}
	}
	keys := make([]string, 0, len(updates))
	for key := range updates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]storageformat.FileOperationRoot, 0, len(keys))
	for _, keyValue := range keys {
		change := updates[keyValue]
		if reflect.DeepEqual(change.pre, change.post) {
			continue
		}
		root, err := s.prepareSimilarityPostingRootChange(ctx, userID, operationID, objectstore.MustKey(keyValue), change.pre, change.post)
		if err != nil {
			return nil, err
		}
		result = append(result, root)
	}
	return result, nil
}

func (s *FileStore) prepareSimilarityPostingRootChange(ctx context.Context, userID domain.UserID, operationID string, key objectstore.Key, pre, post *storageformat.DuplicateSimilarityPosting) (storageformat.FileOperationRoot, error) {
	object, err := s.engine.backend.Get(ctx, key)
	revision := uint64(0)
	expected := ""
	preExisted := false
	var current *storageformat.DuplicateSimilarityPosting
	if err == nil {
		preExisted = true
		var envelope storageformat.Envelope
		var root storageformat.DuplicateSimilarityPostingRoot
		if err := storageformat.DecodeEnvelope(object.Body, key, duplicateSimilaritySchema, &envelope, &root); err != nil {
			return storageformat.FileOperationRoot{}, err
		}
		if root.SchemaVersion != 1 || root.UserID != userID.String() || root.Pending != nil {
			return storageformat.FileOperationRoot{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate similarity posting is changing concurrently")
		}
		current, revision, expected = root.Current, envelope.Revision, envelope.LogicalVersion
	} else if !errors.Is(err, domain.ErrNotFound) {
		return storageformat.FileOperationRoot{}, err
	}
	if !reflect.DeepEqual(current, pre) {
		return storageformat.FileOperationRoot{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate similarity posting changed")
	}
	pendingBody, err := storageformat.EncodeEnvelope(duplicateSimilaritySchema, key, revision+1, storageformat.DuplicateSimilarityPostingRoot{
		SchemaVersion: 1, UserID: userID.String(), Current: current,
		Pending: &storageformat.DuplicateSimilarityPostingTransition{OperationID: operationID, Fence: 1, Pre: pre, Post: post},
	})
	if err != nil {
		return storageformat.FileOperationRoot{}, err
	}
	finalBody, err := storageformat.EncodeEnvelope(duplicateSimilaritySchema, key, revision+2, storageformat.DuplicateSimilarityPostingRoot{SchemaVersion: 1, UserID: userID.String(), Current: post})
	if err != nil {
		return storageformat.FileOperationRoot{}, err
	}
	var rollbackBody []byte
	if preExisted {
		rollbackBody, err = storageformat.EncodeEnvelope(duplicateSimilaritySchema, key, revision+2, storageformat.DuplicateSimilarityPostingRoot{SchemaVersion: 1, UserID: userID.String(), Current: pre})
		if err != nil {
			return storageformat.FileOperationRoot{}, err
		}
	}
	return storageformat.FileOperationRoot{Key: key.String(), ExpectedLogicalVersion: expected, PreExisted: preExisted, PendingBody: pendingBody, FinalBody: finalBody, RollbackBody: rollbackBody}, nil
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
			return catalogRootChange{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate occurrence is changing concurrently")
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
			return catalogRootChange{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate summary is changing concurrently")
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
	if current != nil && (validateDuplicateSummary(*current) != nil || storageformat.DuplicateSummaryKey(userID.String(), string(current.Kind), current.GroupID, current.Shard) != object.Key) {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid duplicate summary")
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
	if current != nil {
		occurrence, err := domainDuplicateOccurrence(*current)
		if err != nil || occurrence.Kind == domain.DuplicateFile && occurrence.FileCount != 1 || duplicateOccurrenceKey(userID.String(), *current) != object.Key {
			return nil, domain.NewError(domain.ErrorInvalid, "invalid duplicate occurrence")
		}
	}
	return current, nil
}

func (s *FileStore) visibleDuplicateSimilarityPosting(ctx context.Context, userID domain.UserID, object objectstore.Object) (*storageformat.DuplicateSimilarityPosting, error) {
	var envelope storageformat.Envelope
	var root storageformat.DuplicateSimilarityPostingRoot
	if err := storageformat.DecodeEnvelope(object.Body, object.Key, duplicateSimilaritySchema, &envelope, &root); err != nil {
		return nil, err
	}
	if root.SchemaVersion != 1 || root.UserID != userID.String() {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid duplicate similarity posting")
	}
	current := root.Current
	if root.Pending != nil {
		operation, err := s.readFileOperation(ctx, userID, root.Pending.OperationID)
		if err != nil {
			return nil, err
		}
		if operation.Fence < root.Pending.Fence {
			return nil, domain.NewError(domain.ErrorInvalid, "duplicate similarity transition fence is invalid")
		}
		if operation.State == storageformat.FileOperationCommitted || operation.State == storageformat.FileOperationSucceeded {
			current = root.Pending.Post
		} else {
			current = root.Pending.Pre
		}
	}
	if current != nil {
		if validateDuplicateSimilarityPosting(*current) != nil || duplicateSimilarityPostingKey(userID.String(), *current) != object.Key {
			return nil, domain.NewError(domain.ErrorInvalid, "invalid duplicate similarity posting")
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

func duplicateDirectoryIgnoreRecord(userID domain.UserID, left, right duplicateDirectoryContentInventory) (storageformat.DuplicateDirectoryIgnore, error) {
	if !userID.Valid() || !left.scope.Valid() || !right.scope.Valid() || left.scope.UserID() != userID || right.scope.UserID() != userID || left.directory == "" || right.directory == "" {
		return storageformat.DuplicateDirectoryIgnore{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate directory pair")
	}
	leftArea, leftID := areaName(left.scope.Area()), left.directory
	rightArea, rightID := areaName(right.scope.Area()), right.directory
	leftIdentity, rightIdentity := leftArea+"\x00"+leftID, rightArea+"\x00"+rightID
	if leftIdentity == rightIdentity {
		return storageformat.DuplicateDirectoryIgnore{}, domain.NewError(domain.ErrorInvalid, "duplicate directory pair must contain two directories")
	}
	if leftIdentity > rightIdentity {
		leftArea, rightArea = rightArea, leftArea
		leftID, rightID = rightID, leftID
		leftIdentity, rightIdentity = rightIdentity, leftIdentity
	}
	pairID := storageformat.Digest([]byte("endlessfs-duplicate-directory-pair-v1\x00" + leftIdentity + "\x00" + rightIdentity))
	return storageformat.DuplicateDirectoryIgnore{
		SchemaVersion: 1, UserID: userID.String(), PairID: pairID,
		LeftArea: leftArea, LeftDirectoryID: leftID, RightArea: rightArea, RightDirectoryID: rightID,
	}, nil
}

func validateDuplicateDirectoryIgnore(value storageformat.DuplicateDirectoryIgnore) error {
	userID, err := domain.ParseUserID(value.UserID)
	if err != nil || value.SchemaVersion != 1 || value.LeftArea != "live" && value.LeftArea != "trash" || value.RightArea != "live" && value.RightArea != "trash" || value.LeftDirectoryID == "" || value.RightDirectoryID == "" || value.Revision == 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid duplicate directory ignore")
	}
	leftScope, _ := domain.NewScope(userID, domain.AreaLive)
	if value.LeftArea == "trash" {
		leftScope, _ = domain.NewScope(userID, domain.AreaTrash)
	}
	rightScope, _ := domain.NewScope(userID, domain.AreaLive)
	if value.RightArea == "trash" {
		rightScope, _ = domain.NewScope(userID, domain.AreaTrash)
	}
	canonical, err := duplicateDirectoryIgnoreRecord(userID,
		duplicateDirectoryContentInventory{scope: leftScope, directory: value.LeftDirectoryID},
		duplicateDirectoryContentInventory{scope: rightScope, directory: value.RightDirectoryID},
	)
	if err != nil || canonical.PairID != value.PairID || canonical.LeftArea != value.LeftArea || canonical.LeftDirectoryID != value.LeftDirectoryID || canonical.RightArea != value.RightArea || canonical.RightDirectoryID != value.RightDirectoryID {
		return domain.NewError(domain.ErrorInvalid, "invalid duplicate directory ignore identity")
	}
	return nil
}

func (s *FileStore) SetDuplicateDirectoryIgnored(ctx context.Context, userID domain.UserID, request domain.SetDuplicateDirectoryIgnoredRequest) (domain.DuplicateDirectoryIgnore, error) {
	if !userID.Valid() || request.Left.Area != domain.AreaLive && request.Left.Area != domain.AreaTrash || request.Right.Area != domain.AreaLive && request.Right.Area != domain.AreaTrash || !request.Left.Path.Valid() || !request.Right.Path.Valid() {
		return domain.DuplicateDirectoryIgnore{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate directory ignore request")
	}
	leftScope, _ := domain.NewScope(userID, request.Left.Area)
	rightScope, _ := domain.NewScope(userID, request.Right.Area)
	left, err := s.readDuplicateDirectoryContentInventory(ctx, leftScope, request.Left.Path)
	if err != nil {
		return domain.DuplicateDirectoryIgnore{}, err
	}
	right, err := s.readDuplicateDirectoryContentInventory(ctx, rightScope, request.Right.Path)
	if err != nil {
		return domain.DuplicateDirectoryIgnore{}, err
	}
	record, err := duplicateDirectoryIgnoreRecord(userID, left, right)
	if err != nil {
		return domain.DuplicateDirectoryIgnore{}, err
	}
	key := storageformat.DuplicateDirectoryIgnoreKey(userID.String(), record.PairID)
	object, err := s.engine.backend.Get(ctx, key)
	revision := uint64(1)
	condition := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
	if err == nil {
		var envelope storageformat.Envelope
		var current storageformat.DuplicateDirectoryIgnore
		if err := storageformat.DecodeEnvelope(object.Body, key, duplicateDirectoryIgnoreSchema, &envelope, &current); err != nil {
			return domain.DuplicateDirectoryIgnore{}, err
		}
		if validateDuplicateDirectoryIgnore(current) != nil || current.PairID != record.PairID {
			return domain.DuplicateDirectoryIgnore{}, domain.NewError(domain.ErrorInvalid, "invalid stored duplicate directory ignore")
		}
		if request.ExpectedRevision != 0 && request.ExpectedRevision != current.Revision {
			return domain.DuplicateDirectoryIgnore{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate directory ignore revision changed")
		}
		if current.Ignored == request.Ignored {
			return domain.DuplicateDirectoryIgnore{Ignored: current.Ignored, Revision: current.Revision}, nil
		}
		revision = current.Revision + 1
		condition = objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.DuplicateDirectoryIgnore{}, err
	} else if request.ExpectedRevision != 0 {
		return domain.DuplicateDirectoryIgnore{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate directory ignore is missing")
	}
	record.Ignored, record.Revision = request.Ignored, revision
	body, err := storageformat.EncodeEnvelope(duplicateDirectoryIgnoreSchema, key, revision, record)
	if err != nil {
		return domain.DuplicateDirectoryIgnore{}, err
	}
	intent := storageformat.MutationIntent{Action: storageformat.MutationCreate, TargetKey: key.String(), TargetBody: body}
	if condition.Mode == objectstore.PutMatch {
		intent.Action = storageformat.MutationCAS
		intent.ExpectedLogicalVersion, err = canonicalLogicalVersion(object.Body)
		if err != nil {
			return domain.DuplicateDirectoryIgnore{}, err
		}
	}
	err = s.engine.withAdmission(ctx, intent, func() error {
		_, putErr := s.engine.backend.Put(ctx, key, body, condition)
		return putErr
	})
	if err != nil {
		return domain.DuplicateDirectoryIgnore{}, err
	}
	return domain.DuplicateDirectoryIgnore{Ignored: record.Ignored, Revision: record.Revision}, nil
}

func (s *FileStore) duplicateDirectoryIgnoreState(ctx context.Context, userID domain.UserID, left, right duplicateDirectoryContentInventory) (bool, uint64, error) {
	record, err := duplicateDirectoryIgnoreRecord(userID, left, right)
	if err != nil {
		return false, 0, err
	}
	key := storageformat.DuplicateDirectoryIgnoreKey(userID.String(), record.PairID)
	object, err := s.engine.backend.Get(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	var envelope storageformat.Envelope
	var current storageformat.DuplicateDirectoryIgnore
	if err := storageformat.DecodeEnvelope(object.Body, key, duplicateDirectoryIgnoreSchema, &envelope, &current); err != nil {
		return false, 0, err
	}
	if validateDuplicateDirectoryIgnore(current) != nil || current.PairID != record.PairID {
		return false, 0, domain.NewError(domain.ErrorInvalid, "invalid stored duplicate directory ignore")
	}
	return current.Ignored, current.Revision, nil
}

func (s *FileStore) CompareDuplicateDirectories(ctx context.Context, userID domain.UserID, request domain.DuplicateDirectoryComparisonRequest) (domain.DuplicateDirectoryComparison, error) {
	if !userID.Valid() || request.Left.Area != domain.AreaLive && request.Left.Area != domain.AreaTrash || request.Right.Area != domain.AreaLive && request.Right.Area != domain.AreaTrash || !request.Left.Path.Valid() || !request.Right.Path.Valid() {
		return domain.DuplicateDirectoryComparison{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate directory comparison")
	}
	leftScope, _ := domain.NewScope(userID, request.Left.Area)
	rightScope, _ := domain.NewScope(userID, request.Right.Area)
	left, err := s.readDuplicateDirectoryContentInventory(ctx, leftScope, request.Left.Path)
	if err != nil {
		return domain.DuplicateDirectoryComparison{}, err
	}
	right, err := s.readDuplicateDirectoryContentInventory(ctx, rightScope, request.Right.Path)
	if err != nil {
		return domain.DuplicateDirectoryComparison{}, err
	}
	return s.compareDuplicateDirectoryContentIndexes(ctx, left, right)
}

func (s *FileStore) ListDuplicateDirectoryOverlaps(ctx context.Context, userID domain.UserID, request domain.DuplicateDirectoryOverlapRequest) (domain.DuplicateDirectoryOverlapPage, error) {
	if !userID.Valid() || request.Directory.Area != domain.AreaLive && request.Directory.Area != domain.AreaTrash || !request.Directory.Path.Valid() {
		return domain.DuplicateDirectoryOverlapPage{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate overlap request")
	}
	limit := request.Limit
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 {
		return domain.DuplicateDirectoryOverlapPage{}, domain.NewError(domain.ErrorInvalid, "duplicate overlap page limit must be between 1 and 100")
	}
	_, gateEnvelope, gate, err := s.engine.readGate(ctx)
	if err != nil {
		return domain.DuplicateDirectoryOverlapPage{}, err
	}
	scope, _ := domain.NewScope(userID, request.Directory.Area)
	selected, err := s.readDuplicateDirectoryContentInventory(ctx, scope, request.Directory.Path)
	if err != nil {
		return domain.DuplicateDirectoryOverlapPage{}, err
	}
	if selected.manifest.RecursiveFileCount == 0 || validateDirectoryContentSketch(selected.manifest.ContentSketch) != nil {
		return domain.DuplicateDirectoryOverlapPage{Candidates: []domain.DuplicateDirectoryOverlapCandidate{}}, nil
	}
	cursor := duplicateOverlapCursor{
		SchemaVersion: 1, UserID: userID.String(), Directory: request.Directory, IncludeIgnored: request.IncludeIgnored, Limit: limit,
		ManifestID: selected.manifestID, GateEpoch: gate.Epoch, GateVersion: gateEnvelope.LogicalVersion,
		ExpiresAt: s.engine.clock.Now().UTC().Add(s.engine.cursorTTL),
	}
	if request.Cursor != "" {
		if err := s.decodeDuplicateCursor(request.Cursor, &cursor); err != nil || cursor.SchemaVersion != 1 || cursor.UserID != userID.String() || cursor.Directory != request.Directory || cursor.IncludeIgnored != request.IncludeIgnored || cursor.Limit != limit || cursor.ManifestID != selected.manifestID || cursor.Position < 0 || cursor.Position >= directoryContentSketchSize || cursor.GateEpoch != gate.Epoch || cursor.GateVersion != gateEnvelope.LogicalVersion || !s.engine.clock.Now().Before(cursor.ExpiresAt) {
			return domain.DuplicateDirectoryOverlapPage{}, domain.NewError(domain.ErrorInvalid, "invalid or stale duplicate overlap cursor")
		}
	}
	result := domain.DuplicateDirectoryOverlapPage{Candidates: make([]domain.DuplicateDirectoryOverlapCandidate, 0, limit)}
	for cursor.Position < directoryContentSketchSize {
		prefix := storageformat.DuplicateSimilarityPostingPrefix(userID.String(), cursor.Position, selected.manifest.ContentSketch[cursor.Position])
		page, err := s.engine.backend.List(ctx, objectstore.ListRequest{Prefix: prefix, Limit: 256, After: cursor.After})
		if err != nil {
			return domain.DuplicateDirectoryOverlapPage{}, err
		}
		for index, info := range page.Objects {
			cursor.After = info.Key.String()
			object, err := s.engine.backend.Get(ctx, info.Key)
			if err != nil {
				return domain.DuplicateDirectoryOverlapPage{}, err
			}
			posting, err := s.visibleDuplicateSimilarityPosting(ctx, userID, object)
			if err != nil {
				return domain.DuplicateDirectoryOverlapPage{}, err
			}
			if posting == nil || posting.Area == areaName(selected.scope.Area()) && posting.DirectoryID == selected.directory {
				continue
			}
			candidate, err := s.similarityPostingInventory(ctx, userID, *posting)
			if err != nil {
				return domain.DuplicateDirectoryOverlapPage{}, err
			}
			shared, first := sharedDirectoryContentSketch(selected.manifest.ContentSketch, candidate.manifest.ContentSketch)
			if shared == 0 || first != cursor.Position {
				continue
			}
			pairIgnored, pairIgnoreRevision, err := s.duplicateDirectoryIgnoreState(ctx, userID, selected, candidate)
			if err != nil {
				return domain.DuplicateDirectoryOverlapPage{}, err
			}
			if pairIgnored && !request.IncludeIgnored {
				continue
			}
			comparison, err := s.compareDuplicateDirectoryContentIndexes(ctx, selected, candidate)
			if err != nil {
				return domain.DuplicateDirectoryOverlapPage{}, err
			}
			exactGroupIgnored, exactGroupIgnoreRevision := false, uint64(0)
			if comparison.Exact {
				exactGroupIgnored, exactGroupIgnoreRevision, err = s.duplicateGroupIgnoreState(ctx, userID, comparison.Left.GroupID)
				if err != nil {
					return domain.DuplicateDirectoryOverlapPage{}, err
				}
				if exactGroupIgnored && !request.IncludeIgnored {
					continue
				}
			}
			result.Candidates = append(result.Candidates, domain.DuplicateDirectoryOverlapCandidate{
				SharedSketch: shared, SketchSize: directoryContentSketchSize,
				Ignored: pairIgnored, IgnoreRevision: pairIgnoreRevision,
				ExactGroupIgnored: exactGroupIgnored, ExactGroupIgnoreRevision: exactGroupIgnoreRevision,
				Comparison: comparison,
			})
			if len(result.Candidates) == limit {
				if index+1 < len(page.Objects) || page.NextCursor != "" || cursor.Position+1 < directoryContentSketchSize {
					result.NextCursor, err = s.encodeDuplicateCursor(cursor)
					if err != nil {
						return domain.DuplicateDirectoryOverlapPage{}, err
					}
				}
				return result, nil
			}
		}
		if page.NextCursor != "" {
			continue
		}
		cursor.Position++
		cursor.After = ""
	}
	return result, nil
}

func sharedDirectoryContentSketch(left, right []string) (shared, first int) {
	first = -1
	if len(left) != directoryContentSketchSize || len(right) != directoryContentSketchSize {
		return 0, -1
	}
	for position := range directoryContentSketchSize {
		if left[position] != right[position] {
			continue
		}
		if first == -1 {
			first = position
		}
		shared++
	}
	return shared, first
}

func (s *FileStore) similarityPostingInventory(ctx context.Context, userID domain.UserID, posting storageformat.DuplicateSimilarityPosting) (duplicateDirectoryContentInventory, error) {
	if err := validateDuplicateSimilarityPosting(posting); err != nil {
		return duplicateDirectoryContentInventory{}, err
	}
	area := domain.AreaLive
	if posting.Area == "trash" {
		area = domain.AreaTrash
	}
	scope, _ := domain.NewScope(userID, area)
	path, err := domain.ParseUserPath(posting.Path)
	if err != nil {
		return duplicateDirectoryContentInventory{}, err
	}
	inventory, err := s.readDuplicateDirectoryContentInventory(ctx, scope, path)
	if err != nil {
		return duplicateDirectoryContentInventory{}, err
	}
	if inventory.directory != posting.DirectoryID || validateDirectoryContentSketch(inventory.manifest.ContentSketch) != nil || inventory.manifest.ContentSketch[posting.Position] != posting.SketchValue {
		return duplicateDirectoryContentInventory{}, domain.NewError(domain.ErrorInvalid, "duplicate similarity posting is stale")
	}
	return inventory, nil
}

// pruneDuplicateTombstones runs only after the closed-gate operation drain.
// Nil visibility roots are no longer needed for recovery at that point and
// otherwise grow without bound as files and directory sketches change.
func (e *Engine) pruneDuplicateTombstones(ctx context.Context) error {
	return visitObjectPages(ctx, e.backend, storageformat.DuplicateRecordsPrefix(), func(info objectstore.ObjectInfo) error {
		object, err := e.backend.Get(ctx, info.Key)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var generic storageformat.Envelope
		if err := state.DecodeJSONWithLimit(object.Body, &generic, storageformat.MaxCanonicalBytes); err != nil {
			return err
		}
		remove := false
		switch generic.Schema {
		case duplicateOccurrenceSchema:
			var envelope storageformat.Envelope
			var root storageformat.DuplicateOccurrenceRoot
			if err := storageformat.DecodeEnvelope(object.Body, info.Key, duplicateOccurrenceSchema, &envelope, &root); err != nil {
				return err
			}
			if err := validateDuplicateMaintenanceRoot(root.UserID, root.SchemaVersion, root.Pending != nil, info.Key); err != nil {
				return err
			}
			if root.Current == nil {
				remove = true
			} else if _, err := domainDuplicateOccurrence(*root.Current); err != nil || duplicateOccurrenceKey(root.UserID, *root.Current) != info.Key {
				return domain.NewError(domain.ErrorInvalid, "invalid duplicate occurrence during tombstone pruning")
			}
		case duplicateSummarySchema:
			var envelope storageformat.Envelope
			var root storageformat.DuplicateSummaryRoot
			if err := storageformat.DecodeEnvelope(object.Body, info.Key, duplicateSummarySchema, &envelope, &root); err != nil {
				return err
			}
			if err := validateDuplicateMaintenanceRoot(root.UserID, root.SchemaVersion, root.Pending != nil, info.Key); err != nil {
				return err
			}
			if root.Current == nil {
				remove = true
			} else if validateDuplicateSummary(*root.Current) != nil || storageformat.DuplicateSummaryKey(root.UserID, string(root.Current.Kind), root.Current.GroupID, root.Current.Shard) != info.Key {
				return domain.NewError(domain.ErrorInvalid, "invalid duplicate summary during tombstone pruning")
			}
		case duplicateSimilaritySchema:
			var envelope storageformat.Envelope
			var root storageformat.DuplicateSimilarityPostingRoot
			if err := storageformat.DecodeEnvelope(object.Body, info.Key, duplicateSimilaritySchema, &envelope, &root); err != nil {
				return err
			}
			if err := validateDuplicateMaintenanceRoot(root.UserID, root.SchemaVersion, root.Pending != nil, info.Key); err != nil {
				return err
			}
			if root.Current == nil {
				remove = true
			} else if validateDuplicateSimilarityPosting(*root.Current) != nil || duplicateSimilarityPostingKey(root.UserID, *root.Current) != info.Key {
				return domain.NewError(domain.ErrorInvalid, "invalid duplicate similarity posting during tombstone pruning")
			}
		case duplicateIgnoreSchema:
			var envelope storageformat.Envelope
			var value storageformat.DuplicateIgnore
			if err := storageformat.DecodeEnvelope(object.Body, info.Key, duplicateIgnoreSchema, &envelope, &value); err != nil {
				return err
			}
			if _, err := domain.ParseUserID(value.UserID); err != nil || value.SchemaVersion != 1 || validateDuplicateGroupID(value.GroupID) != nil || value.Revision == 0 || storageformat.DuplicateIgnoreKey(value.UserID, value.GroupID) != info.Key {
				return domain.NewError(domain.ErrorInvalid, "invalid duplicate ignore during tombstone pruning")
			}
		case duplicateDirectoryIgnoreSchema:
			var envelope storageformat.Envelope
			var value storageformat.DuplicateDirectoryIgnore
			if err := storageformat.DecodeEnvelope(object.Body, info.Key, duplicateDirectoryIgnoreSchema, &envelope, &value); err != nil {
				return err
			}
			if validateDuplicateDirectoryIgnore(value) != nil || storageformat.DuplicateDirectoryIgnoreKey(value.UserID, value.PairID) != info.Key {
				return domain.NewError(domain.ErrorInvalid, "invalid duplicate directory ignore during tombstone pruning")
			}
		default:
			return domain.NewError(domain.ErrorInvalid, "unknown duplicate record during tombstone pruning")
		}
		if remove {
			return deleteMaintenanceObject(ctx, e.backend, object)
		}
		return nil
	})
}

func validateDuplicateMaintenanceRoot(userID string, schemaVersion int, pending bool, key objectstore.Key) error {
	if _, err := domain.ParseUserID(userID); err != nil || schemaVersion != 1 || pending || !strings.HasPrefix(key.String(), storageformat.DuplicateOccurrenceOwnerPrefix(userID)) {
		return domain.NewError(domain.ErrorInvalid, "invalid or unresolved duplicate root during tombstone pruning")
	}
	return nil
}

func validateDuplicateSummary(value storageformat.DuplicateSummary) error {
	if validateDuplicateGroupID(value.GroupID) != nil || !value.Kind.Valid() || len(value.Shard) != 2 || value.OccurrenceCount <= 0 || value.Size < 0 || value.FileCount < 0 || value.Kind == domain.DuplicateFile && value.FileCount != 1 {
		return domain.NewError(domain.ErrorInvalid, "invalid duplicate summary")
	}
	for _, character := range value.Shard {
		if (character < 'a' || character > 'z') && (character < '2' || character > '7') {
			return domain.NewError(domain.ErrorInvalid, "invalid duplicate summary shard")
		}
	}
	return nil
}

type duplicateDirectoryContentInventory struct {
	occurrence domain.DuplicateOccurrence
	scope      domain.Scope
	path       domain.UserPath
	directory  string
	manifestID string
	manifest   storageformat.DirectoryManifest
}

func (s *FileStore) readDuplicateDirectoryContentInventory(ctx context.Context, scope domain.Scope, path domain.UserPath) (duplicateDirectoryContentInventory, error) {
	view, err := s.resolveDirectoryMetadataView(ctx, scope, path)
	if err != nil {
		return duplicateDirectoryContentInventory{}, err
	}
	leaf, err := s.readDirectoryMetadata(ctx, scope, view.directoryID, path.IsRoot())
	if err != nil {
		return duplicateDirectoryContentInventory{}, err
	}
	version := view.current.Version
	if path.IsRoot() {
		version = domain.Version("root")
	}
	groupID, err := duplicateDirectoryGroupID(leaf.recursiveBytes, leaf.recursiveFileCount, leaf.contentDigest)
	if err != nil {
		return duplicateDirectoryContentInventory{}, err
	}
	occurrence := domain.DuplicateOccurrence{
		GroupID: groupID, Kind: domain.DuplicateDirectory, Area: scope.Area(), AreaName: areaName(scope.Area()),
		Path: path, Size: leaf.recursiveBytes, FileCount: leaf.recursiveFileCount, Version: version,
	}
	return duplicateDirectoryContentInventory{occurrence: occurrence, scope: scope, path: path, directory: view.directoryID, manifestID: leaf.manifestID, manifest: leaf.manifest}, nil
}

type directoryContentIndexIterator struct {
	store     *FileStore
	ctx       context.Context
	inventory duplicateDirectoryContentInventory
	after     string
	values    []storageformat.DirectoryContentIndexEntry
	index     int
	exhausted bool
}

func newDirectoryContentIndexIterator(store *FileStore, ctx context.Context, inventory duplicateDirectoryContentInventory, after string) *directoryContentIndexIterator {
	return &directoryContentIndexIterator{store: store, ctx: ctx, inventory: inventory, after: after}
}

func (iterator *directoryContentIndexIterator) peek() (storageformat.DirectoryContentIndexEntry, bool, error) {
	if iterator.index < len(iterator.values) {
		return iterator.values[iterator.index], true, nil
	}
	if iterator.exhausted {
		return storageformat.DirectoryContentIndexEntry{}, false, nil
	}
	values, err := iterator.store.collectDirectoryContentIndexEntries(iterator.ctx, iterator.inventory.scope, iterator.inventory.directory, iterator.inventory.manifest, iterator.after, maxEntriesPerPage)
	if err != nil {
		return storageformat.DirectoryContentIndexEntry{}, false, err
	}
	iterator.values, iterator.index = values, 0
	if len(values) < maxEntriesPerPage {
		iterator.exhausted = true
	}
	if len(values) == 0 {
		return storageformat.DirectoryContentIndexEntry{}, false, nil
	}
	return values[0], true, nil
}

func (iterator *directoryContentIndexIterator) pop() (storageformat.DirectoryContentIndexEntry, string, bool, error) {
	value, found, err := iterator.peek()
	if err != nil || !found {
		return storageformat.DirectoryContentIndexEntry{}, "", found, err
	}
	key, err := directoryContentIndexKey(value)
	if err != nil {
		return storageformat.DirectoryContentIndexEntry{}, "", false, err
	}
	iterator.index++
	iterator.after = key
	return value, key, true, nil
}

func (s *FileStore) nextDirectoryContentGroup(iterator *directoryContentIndexIterator) (string, int64, int64, bool, error) {
	first, found, err := iterator.peek()
	if err != nil || !found {
		return "", 0, 0, found, err
	}
	groupID, size := first.GroupID, first.Size
	count := int64(0)
	for {
		value, found, err := iterator.peek()
		if err != nil {
			return "", 0, 0, false, err
		}
		if !found || value.GroupID != groupID {
			return groupID, count, size, true, nil
		}
		if value.Size != size || count == math.MaxInt64 {
			return "", 0, 0, false, domain.NewError(domain.ErrorInvalid, "directory content-index group is inconsistent")
		}
		if _, _, _, err := iterator.pop(); err != nil {
			return "", 0, 0, false, err
		}
		count++
	}
}

func (s *FileStore) compareDuplicateDirectoryContentIndexes(ctx context.Context, left, right duplicateDirectoryContentInventory) (domain.DuplicateDirectoryComparison, error) {
	result := domain.DuplicateDirectoryComparison{Left: left.occurrence, Right: right.occurrence, Exact: left.occurrence.GroupID == right.occurrence.GroupID}
	if result.Exact {
		result.CommonFiles, result.CommonBytes = left.occurrence.FileCount, left.occurrence.Size
		return result, nil
	}
	leftIterator := newDirectoryContentIndexIterator(s, ctx, left, "")
	rightIterator := newDirectoryContentIndexIterator(s, ctx, right, "")
	leftGroup, leftCount, leftSize, leftFound, err := s.nextDirectoryContentGroup(leftIterator)
	if err != nil {
		return domain.DuplicateDirectoryComparison{}, err
	}
	rightGroup, rightCount, rightSize, rightFound, err := s.nextDirectoryContentGroup(rightIterator)
	if err != nil {
		return domain.DuplicateDirectoryComparison{}, err
	}
	var leftFiles, leftBytes, rightFiles, rightBytes int64
	for leftFound || rightFound {
		switch {
		case !rightFound || leftFound && leftGroup < rightGroup:
			if err := addDuplicateTotals(&result.LeftOnlyFiles, &result.LeftOnlyBytes, leftCount, leftSize); err != nil || addDuplicateTotals(&leftFiles, &leftBytes, leftCount, leftSize) != nil {
				return domain.DuplicateDirectoryComparison{}, domain.NewError(domain.ErrorInvalid, "duplicate directory comparison overflows")
			}
			leftGroup, leftCount, leftSize, leftFound, err = s.nextDirectoryContentGroup(leftIterator)
		case !leftFound || rightGroup < leftGroup:
			if err := addDuplicateTotals(&result.RightOnlyFiles, &result.RightOnlyBytes, rightCount, rightSize); err != nil || addDuplicateTotals(&rightFiles, &rightBytes, rightCount, rightSize) != nil {
				return domain.DuplicateDirectoryComparison{}, domain.NewError(domain.ErrorInvalid, "duplicate directory comparison overflows")
			}
			rightGroup, rightCount, rightSize, rightFound, err = s.nextDirectoryContentGroup(rightIterator)
		default:
			if leftSize != rightSize {
				return domain.DuplicateDirectoryComparison{}, domain.NewError(domain.ErrorInvalid, "duplicate file identity size mismatch")
			}
			common := min(leftCount, rightCount)
			if addDuplicateTotals(&result.CommonFiles, &result.CommonBytes, common, leftSize) != nil || addDuplicateTotals(&result.LeftOnlyFiles, &result.LeftOnlyBytes, leftCount-common, leftSize) != nil || addDuplicateTotals(&result.RightOnlyFiles, &result.RightOnlyBytes, rightCount-common, rightSize) != nil || addDuplicateTotals(&leftFiles, &leftBytes, leftCount, leftSize) != nil || addDuplicateTotals(&rightFiles, &rightBytes, rightCount, rightSize) != nil {
				return domain.DuplicateDirectoryComparison{}, domain.NewError(domain.ErrorInvalid, "duplicate directory comparison overflows")
			}
			leftGroup, leftCount, leftSize, leftFound, err = s.nextDirectoryContentGroup(leftIterator)
			if err == nil {
				rightGroup, rightCount, rightSize, rightFound, err = s.nextDirectoryContentGroup(rightIterator)
			}
		}
		if err != nil {
			return domain.DuplicateDirectoryComparison{}, err
		}
	}
	if leftFiles != left.occurrence.FileCount || leftBytes != left.occurrence.Size || rightFiles != right.occurrence.FileCount || rightBytes != right.occurrence.Size {
		return domain.DuplicateDirectoryComparison{}, domain.NewError(domain.ErrorInvalid, "directory content index disagrees with recursive aggregates")
	}
	return result, nil
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
	left, err := s.readDuplicateDirectoryContentInventory(ctx, leftScope, request.Left.Path)
	if err != nil {
		return domain.DuplicateReconciliationPreview{}, err
	}
	right, err := s.readDuplicateDirectoryContentInventory(ctx, rightScope, request.Right.Path)
	if err != nil {
		return domain.DuplicateReconciliationPreview{}, err
	}
	cursor := duplicateReconciliationCursor{
		SchemaVersion: 2, UserID: userID.String(), Left: request.Left, Right: request.Right, RemoveFrom: request.RemoveFrom, Limit: limit,
		LeftManifest: left.manifestID, RightManifest: right.manifestID, GateEpoch: gate.Epoch, GateVersion: gateEnvelope.LogicalVersion,
		ExpiresAt: s.engine.clock.Now().UTC().Add(s.engine.cursorTTL),
	}
	if request.Cursor != "" {
		if err := s.decodeDuplicateCursor(request.Cursor, &cursor); err != nil || cursor.SchemaVersion != 2 || cursor.UserID != userID.String() || cursor.Left != request.Left || cursor.Right != request.Right || cursor.RemoveFrom != request.RemoveFrom || cursor.Limit != limit || cursor.LeftManifest != left.manifestID || cursor.RightManifest != right.manifestID || cursor.GateEpoch != gate.Epoch || cursor.GateVersion != gateEnvelope.LogicalVersion || !s.engine.clock.Now().Before(cursor.ExpiresAt) {
			return domain.DuplicateReconciliationPreview{}, domain.NewError(domain.ErrorInvalid, "invalid or stale duplicate reconciliation cursor")
		}
		cursor.Comparison.Left, cursor.Comparison.Right = left.occurrence, right.occurrence
	} else {
		cursor.Comparison, err = s.compareDuplicateDirectoryContentIndexes(ctx, left, right)
		if err != nil {
			return domain.DuplicateReconciliationPreview{}, err
		}
	}
	var pageItems []domain.DuplicateReconciliationItem
	more := false
	if cursor.Comparison.Exact {
		ignored, revision, ignoreErr := s.duplicateGroupIgnoreState(ctx, userID, left.occurrence.GroupID)
		if ignoreErr != nil {
			return domain.DuplicateReconciliationPreview{}, ignoreErr
		}
		if !ignored && request.Cursor == "" {
			remove, keep := left.occurrence, right.occurrence
			if request.RemoveFrom == domain.DuplicateSideRight {
				remove, keep = right.occurrence, left.occurrence
			}
			pageItems = []domain.DuplicateReconciliationItem{{GroupID: left.occurrence.GroupID, Remove: remove, Keep: keep, IgnoreRevision: revision}}
		}
	} else {
		pageItems, cursor.LeftAfter, cursor.RightAfter, more, err = s.reconciliationIndexItems(ctx, userID, left, right, request.RemoveFrom, cursor.LeftAfter, cursor.RightAfter, limit)
		if err != nil {
			return domain.DuplicateReconciliationPreview{}, err
		}
	}
	result := domain.DuplicateReconciliationPreview{Comparison: cursor.Comparison, RemoveFrom: request.RemoveFrom, Items: pageItems}
	for _, item := range pageItems {
		if item.Remove.Size > math.MaxInt64-result.ReclaimableBytes {
			return domain.DuplicateReconciliationPreview{}, domain.NewError(domain.ErrorInvalid, "duplicate reconciliation bytes overflow")
		}
		result.ReclaimableBytes += item.Remove.Size
	}
	if more {
		result.NextCursor, err = s.encodeDuplicateCursor(cursor)
		if err != nil {
			return domain.DuplicateReconciliationPreview{}, err
		}
	}
	if len(pageItems) != 0 {
		result.PlanToken, err = s.encodeDuplicateCursor(duplicateReconciliationPlan{
			SchemaVersion: 1, UserID: userID.String(), Left: left.occurrence, Right: right.occurrence, RemoveFrom: request.RemoveFrom, Items: pageItems,
			LeftManifest: left.manifestID, RightManifest: right.manifestID, GateEpoch: gate.Epoch, GateVersion: gateEnvelope.LogicalVersion,
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
		if len(leftValues) != 0 && len(rightValues) != 0 && size != rightValues[0].Size {
			return domain.DuplicateDirectoryComparison{}, domain.NewError(domain.ErrorInvalid, "duplicate file identity size mismatch")
		}
		if size == 0 && len(rightValues) != 0 {
			size = rightValues[0].Size
		}
		common := min(int64(len(leftValues)), int64(len(rightValues)))
		if err := addDuplicateTotals(&result.CommonFiles, &result.CommonBytes, common, size); err != nil {
			return domain.DuplicateDirectoryComparison{}, err
		}
		if err := addDuplicateTotals(&result.LeftOnlyFiles, &result.LeftOnlyBytes, int64(len(leftValues))-common, size); err != nil {
			return domain.DuplicateDirectoryComparison{}, err
		}
		if err := addDuplicateTotals(&result.RightOnlyFiles, &result.RightOnlyBytes, int64(len(rightValues))-common, size); err != nil {
			return domain.DuplicateDirectoryComparison{}, err
		}
	}
	return result, nil
}

func addDuplicateTotals(files, bytes *int64, count, size int64) error {
	if files == nil || bytes == nil || count < 0 || size < 0 || count > math.MaxInt64-*files || count != 0 && size > math.MaxInt64/count {
		return domain.NewError(domain.ErrorInvalid, "duplicate directory comparison overflows")
	}
	additionalBytes := count * size
	if additionalBytes > math.MaxInt64-*bytes {
		return domain.NewError(domain.ErrorInvalid, "duplicate directory comparison overflows")
	}
	*files += count
	*bytes += additionalBytes
	return nil
}

func (s *FileStore) reconciliationIndexItems(ctx context.Context, userID domain.UserID, left, right duplicateDirectoryContentInventory, removeFrom domain.DuplicateSide, leftAfter, rightAfter string, limit int) ([]domain.DuplicateReconciliationItem, string, string, bool, error) {
	leftIterator := newDirectoryContentIndexIterator(s, ctx, left, leftAfter)
	rightIterator := newDirectoryContentIndexIterator(s, ctx, right, rightAfter)
	items := make([]domain.DuplicateReconciliationItem, 0, limit)
	ignoreGroup := ""
	ignored := false
	ignoreRevision := uint64(0)
	for {
		leftValue, leftFound, err := leftIterator.peek()
		if err != nil {
			return nil, "", "", false, err
		}
		rightValue, rightFound, err := rightIterator.peek()
		if err != nil {
			return nil, "", "", false, err
		}
		if !leftFound || !rightFound {
			return items, leftIterator.after, rightIterator.after, false, nil
		}
		switch {
		case leftValue.GroupID < rightValue.GroupID:
			if _, _, _, err := leftIterator.pop(); err != nil {
				return nil, "", "", false, err
			}
			continue
		case rightValue.GroupID < leftValue.GroupID:
			if _, _, _, err := rightIterator.pop(); err != nil {
				return nil, "", "", false, err
			}
			continue
		}
		if leftValue.Size != rightValue.Size {
			return nil, "", "", false, domain.NewError(domain.ErrorInvalid, "duplicate file identity size mismatch")
		}
		if ignoreGroup != leftValue.GroupID {
			ignoreGroup = leftValue.GroupID
			ignored, ignoreRevision, err = s.duplicateGroupIgnoreState(ctx, userID, ignoreGroup)
			if err != nil {
				return nil, "", "", false, err
			}
		}
		if ignored {
			if _, _, _, err := leftIterator.pop(); err != nil {
				return nil, "", "", false, err
			}
			if _, _, _, err := rightIterator.pop(); err != nil {
				return nil, "", "", false, err
			}
			continue
		}
		if len(items) == limit {
			return items, leftIterator.after, rightIterator.after, true, nil
		}
		leftValue, _, _, err = leftIterator.pop()
		if err != nil {
			return nil, "", "", false, err
		}
		rightValue, _, _, err = rightIterator.pop()
		if err != nil {
			return nil, "", "", false, err
		}
		leftOccurrence, err := s.duplicateContentIndexOccurrence(ctx, left, leftValue)
		if err != nil {
			return nil, "", "", false, err
		}
		rightOccurrence, err := s.duplicateContentIndexOccurrence(ctx, right, rightValue)
		if err != nil {
			return nil, "", "", false, err
		}
		remove, keep := leftOccurrence, rightOccurrence
		if removeFrom == domain.DuplicateSideRight {
			remove, keep = rightOccurrence, leftOccurrence
		}
		items = append(items, domain.DuplicateReconciliationItem{GroupID: leftValue.GroupID, Remove: remove, Keep: keep, IgnoreRevision: ignoreRevision})
	}
}

func (s *FileStore) duplicateContentIndexOccurrence(ctx context.Context, inventory duplicateDirectoryContentInventory, value storageformat.DirectoryContentIndexEntry) (domain.DuplicateOccurrence, error) {
	relative, err := domain.ParseUserPath(value.RelativePath)
	if err != nil || relative.IsRoot() {
		return domain.DuplicateOccurrence{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate content relative path")
	}
	path := inventory.path
	for _, segment := range relative.Segments() {
		path, err = path.Join(segment)
		if err != nil {
			return domain.DuplicateOccurrence{}, err
		}
	}
	entry, err := s.resolveDirectoryContentIndexEntry(ctx, inventory.scope, inventory.directory, value.RelativePath)
	if err != nil {
		return domain.DuplicateOccurrence{}, err
	}
	groupID, err := duplicateFileGroupID(entry)
	if err != nil || groupID != value.GroupID || entry.Size != value.Size {
		return domain.DuplicateOccurrence{}, domain.NewError(domain.ErrorInvalid, "duplicate content index disagrees with the selected file")
	}
	return domain.DuplicateOccurrence{
		GroupID: value.GroupID, Kind: domain.DuplicateFile, Area: inventory.scope.Area(), AreaName: areaName(inventory.scope.Area()),
		Path: path, Size: value.Size, FileCount: 1, Version: domain.Version(entry.LogicalVersion),
	}, nil
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
	if snapshot.recursiveFileCount > 0 {
		postings, err := duplicateSimilarityPostings(scope, path, snapshot.root.DirectoryID, snapshot.manifest.ContentSketch)
		if err != nil {
			return err
		}
		for _, posting := range postings {
			if err := e.ensureMigratedSimilarityPosting(ctx, scope.UserID(), posting); err != nil {
				return err
			}
		}
	}
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

func (e *Engine) ensureMigratedSimilarityPosting(ctx context.Context, userID domain.UserID, posting storageformat.DuplicateSimilarityPosting) error {
	if err := validateDuplicateSimilarityPosting(posting); err != nil {
		return err
	}
	key := duplicateSimilarityPostingKey(userID.String(), posting)
	body, err := storageformat.EncodeEnvelope(duplicateSimilaritySchema, key, 1, storageformat.DuplicateSimilarityPostingRoot{SchemaVersion: 1, UserID: userID.String(), Current: &posting})
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
		current, err := e.Files().visibleDuplicateSimilarityPosting(ctx, userID, object)
		if err != nil {
			return err
		}
		if current == nil || !reflect.DeepEqual(current, &posting) {
			return domain.NewError(domain.ErrorInvalid, "migrated similarity posting disagrees with the filesystem")
		}
		return nil
	}
	return domain.NewError(domain.ErrorUnavailable, "duplicate similarity posting migration remained contended")
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
