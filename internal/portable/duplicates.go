package portable

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"reflect"
	"sort"
	"strings"

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

func (s *FileStore) visibleDuplicateSimilarityPostingForSchema(ctx context.Context, userID domain.UserID, object objectstore.Object, includeAreaRoots bool) (*storageformat.DuplicateSimilarityPosting, error) {
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
		// Schema 006 recorded live/trash area roots even though those navigation
		// containers cannot be selected for reconciliation. Schema 007 retains
		// those immutable historical objects for portable-copy compatibility but
		// treats them as absent from the user-addressable catalog.
		if current.Path == "/" && !includeAreaRoots {
			return nil, nil
		}
	}
	return current, nil
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
	scope     domain.Scope
	directory string
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

// rebuildDuplicateCatalog streams one indexed directory page at a time. Root
// keys are provider-ordered by owner, so only one owner and one active
// directory traversal are retained while summaries are finalized.
func (e *Engine) rebuildDuplicateCatalog(ctx context.Context) error {
	currentUser := ""
	var summaryUser domain.UserID
	flushSummaries := func() error {
		if currentUser == "" {
			return nil
		}
		return e.rebuildDuplicateSummaries(ctx, summaryUser)
	}
	err := visitObjectPages(ctx, e.backend, storageformat.FilesystemPrefix(), func(info objectstore.ObjectInfo) error {
		userValue, areaValue, directoryID, matched, err := storageformat.ParseDirectoryRootKey(info.Key)
		if err != nil || !matched || directoryID != storageformat.RootDirectoryID {
			return err
		}
		userID, err := domain.ParseUserID(userValue)
		if err != nil {
			return err
		}
		if currentUser != "" && currentUser != userValue {
			if err := flushSummaries(); err != nil {
				return err
			}
		}
		currentUser, summaryUser = userValue, userID
		area := domain.AreaLive
		switch areaValue {
		case "live":
		case "trash":
			area = domain.AreaTrash
		default:
			return domain.NewError(domain.ErrorInvalid, "duplicate catalog migration encountered an invalid area")
		}
		scope, err := domain.NewScope(userID, area)
		if err != nil {
			return err
		}
		root, err := e.Files().readDirectoryMetadata(ctx, scope, storageformat.RootDirectoryID, false)
		if err != nil {
			return err
		}
		return e.migrateDuplicateDirectoryStream(ctx, scope, domain.MustParseUserPath("/"), root)
	})
	if err != nil {
		return err
	}
	return flushSummaries()
}

func (e *Engine) migrateDuplicateDirectoryStream(ctx context.Context, scope domain.Scope, path domain.UserPath, snapshot directorySnapshot) error {
	if snapshot.pending {
		return domain.NewError(domain.ErrorUnavailable, "duplicate migration encountered a pending directory")
	}
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
	children := e.Files().directoryEntryIterator(ctx, scope, snapshot.root.DirectoryID, snapshot.manifest, domain.SortName, nil)
	for {
		entry, found, err := children()
		if err != nil || !found {
			return err
		}
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
		child, err := e.Files().readDirectoryMetadata(ctx, scope, entry.DirectoryID, false)
		if err != nil {
			return err
		}
		if child.recursiveBytes != entry.Size || child.recursiveFileCount != entry.FileCount || child.contentDigest != entry.ContentDigest {
			return domain.NewError(domain.ErrorInvalid, "duplicate migration encountered a stale directory aggregate")
		}
		if err := e.migrateDuplicateDirectoryStream(ctx, scope, childPath, child); err != nil {
			return err
		}
	}
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
		current, err := e.Files().visibleDuplicateSimilarityPostingForSchema(ctx, userID, object, true)
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
