package portable

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const (
	directoryRootSchema     = "directory-root-v1"
	directoryManifestSchema = "directory-manifest-v1"
	directoryPageSchema     = "directory-page-v1"
	maxEntriesPerPage       = 256
)

// FileStore is the application-facing filesystem facade over the same
// portable engine used for state. It is separate because Go cannot implement
// the provider and state interfaces on one value: both contracts name a List
// method with different signatures.
type FileStore struct{ engine *Engine }

func (e *Engine) Files() *FileStore { return &FileStore{engine: e} }

type directorySnapshot struct {
	object             objectstore.Object
	exists             bool
	envelope           storageformat.Envelope
	root               storageformat.DirectoryRoot
	manifestID         string
	manifest           storageformat.DirectoryManifest
	entries            []storageformat.DirectoryEntry
	recursiveBytes     int64
	recursiveFileCount int64
	contentAccumulator string
	contentDigest      string
	pending            bool
	transitionState    storageformat.FileOperationState
	transitionFence    uint64
}

type preparedDirectory struct {
	manifestID         string
	recursiveBytes     int64
	recursiveFileCount int64
	contentAccumulator string
	contentDigest      string
	rootBody           []byte
	prerequisites      []storageformat.MutationObject
}

type directoryTrailNode struct {
	scope       domain.Scope
	path        domain.UserPath
	directoryID string
	entry       storageformat.DirectoryEntry
	snapshot    directorySnapshot
}

type directoryUpdate struct {
	scope              domain.Scope
	path               domain.UserPath
	directoryID        string
	entry              storageformat.DirectoryEntry
	snapshot           directorySnapshot
	changes            map[string]directoryEntryMutation
	entryCount         int
	recursiveBytes     int64
	recursiveFileCount int64
	contentAccumulator string
	contentDigest      string
}

type directoryView struct {
	directoryID    string
	snapshot       directorySnapshot
	current        domain.Entry
	parentID       string
	parentManifest string
}

type listCursor struct {
	SchemaVersion  int              `json:"schemaVersion"`
	UserID         string           `json:"userID"`
	Area           string           `json:"area"`
	DirectoryPath  string           `json:"directoryPath"`
	DirectoryID    string           `json:"directoryID"`
	ManifestID     string           `json:"manifestID"`
	ParentID       string           `json:"parentID"`
	ParentManifest string           `json:"parentManifest"`
	PageSize       int              `json:"pageSize"`
	Sort           domain.SortField `json:"sort"`
	Descending     bool             `json:"descending"`
	// Index is retained only to reject historical materialized-sort cursors.
	Index        int       `json:"index,omitempty"`
	AfterName    string    `json:"afterName,omitempty"`
	AfterSortKey string    `json:"afterSortKey,omitempty"`
	GateEpoch    uint64    `json:"gateEpoch"`
	GateVersion  string    `json:"gateVersion"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

func (s *FileStore) List(ctx context.Context, scope domain.Scope, request domain.ListRequest) (domain.ListPage, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return domain.ListPage{}, err
	}
	if !request.Directory.Valid() {
		return domain.ListPage{}, domain.NewError(domain.ErrorInvalid, "directory path is required")
	}
	pageSize, err := normalizeFilePageSize(request.PageSize)
	if err != nil {
		return domain.ListPage{}, err
	}
	if request.Sort == "" {
		request.Sort = domain.SortName
	}
	if !validSort(request.Sort) {
		return domain.ListPage{}, domain.NewError(domain.ErrorInvalid, "invalid list sort")
	}
	_, gateEnvelope, gate, err := s.engine.readGate(ctx)
	if err != nil {
		return domain.ListPage{}, err
	}
	afterName := ""
	afterSortKey := ""
	var directoryID, manifestID, parentID, parentManifest string
	var manifest storageformat.DirectoryManifest
	var current domain.Entry
	var entries []storageformat.DirectoryEntry
	if request.Cursor != "" {
		cursor, decodeErr := s.decodeListCursor(request.Cursor)
		nameCursor := request.Sort == domain.SortName && cursor.SchemaVersion == 3 && cursor.AfterName != "" && cursor.AfterSortKey == "" && cursor.Index == 0
		secondaryCursor := request.Sort != domain.SortName && cursor.SchemaVersion == 4 && cursor.AfterSortKey != "" && cursor.AfterName == "" && cursor.Index == 0
		if decodeErr != nil || cursor.UserID != scope.UserID().String() || cursor.Area != areaName(scope.Area()) || cursor.DirectoryPath != request.Directory.String() || cursor.DirectoryID == "" || cursor.PageSize != pageSize || cursor.Sort != request.Sort || cursor.Descending != request.Descending || (!nameCursor && !secondaryCursor) || cursor.ManifestID == "" || cursor.GateEpoch != gate.Epoch || cursor.GateVersion != gateEnvelope.LogicalVersion || !s.engine.clock.Now().Before(cursor.ExpiresAt) || (request.Directory.IsRoot() && (cursor.ParentID != "" || cursor.ParentManifest != "")) || (!request.Directory.IsRoot() && (cursor.ParentID == "" || cursor.ParentManifest == "")) {
			return domain.ListPage{}, domain.NewError(domain.ErrorInvalid, "invalid or out-of-scope list cursor")
		}
		if request.Directory.IsRoot() {
			if cursor.DirectoryID != storageformat.RootDirectoryID {
				return domain.ListPage{}, domain.NewError(domain.ErrorInvalid, "list cursor directory was replaced")
			}
		} else {
			liveEntry, err := s.resolveEntry(ctx, scope, request.Directory)
			if err != nil {
				return domain.ListPage{}, err
			}
			if liveEntry.Kind != domain.EntryDirectory || liveEntry.DirectoryID != cursor.DirectoryID {
				return domain.ListPage{}, domain.NewError(domain.ErrorInvalid, "list cursor directory was replaced")
			}
		}
		manifest, err = s.readDirectoryManifest(ctx, scope, cursor.DirectoryID, cursor.ManifestID)
		if err != nil {
			return domain.ListPage{}, err
		}
		directoryID, manifestID, parentID, parentManifest = cursor.DirectoryID, cursor.ManifestID, cursor.ParentID, cursor.ParentManifest
		current, err = s.historicalCurrentDirectory(ctx, scope, request.Directory, directoryID, parentID, parentManifest, manifest.RecursiveBytes, manifest.RecursiveFileCount, manifest.ContentDigest)
		if err != nil {
			return domain.ListPage{}, err
		}
		if request.Sort == domain.SortName {
			afterName = cursor.AfterName
		} else {
			afterSortKey = cursor.AfterSortKey
		}
	} else {
		view, err := s.resolveDirectoryMetadataView(ctx, scope, request.Directory)
		if err != nil {
			return domain.ListPage{}, err
		}
		directoryID, manifestID, parentID, parentManifest = view.directoryID, view.snapshot.manifestID, view.parentID, view.parentManifest
		manifest, current = view.snapshot.manifest, view.current
	}
	if request.Sort == domain.SortName {
		entries, err = s.collectDirectoryIndexEntries(ctx, scope, directoryID, manifest, afterName, request.Descending, pageSize+1)
		if err != nil {
			return domain.ListPage{}, err
		}
		result := make([]domain.Entry, 0, min(pageSize, len(entries)))
		for _, entry := range entries[:min(pageSize, len(entries))] {
			path, joinErr := request.Directory.Join(entry.Name)
			if joinErr != nil {
				return domain.ListPage{}, domain.NewError(domain.ErrorInvalid, "stored directory name is invalid")
			}
			result = append(result, domainEntry(path, entry))
		}
		page := domain.ListPage{Current: current, Entries: result}
		if len(entries) > pageSize {
			page.NextCursor, err = s.encodeListCursor(listCursor{
				SchemaVersion: 3, UserID: scope.UserID().String(), Area: areaName(scope.Area()),
				DirectoryPath: request.Directory.String(), DirectoryID: directoryID, ManifestID: manifestID,
				ParentID: parentID, ParentManifest: parentManifest,
				PageSize: pageSize, Sort: request.Sort, Descending: request.Descending, AfterName: entries[pageSize-1].Name,
				GateEpoch: gate.Epoch, GateVersion: gateEnvelope.LogicalVersion, ExpiresAt: s.engine.clock.Now().UTC().Add(s.engine.cursorTTL),
			})
			if err != nil {
				return domain.ListPage{}, err
			}
		}
		return page, nil
	}
	entries, err = s.collectDirectorySortIndexEntries(ctx, scope, directoryID, manifest, request.Sort, afterSortKey, request.Descending, pageSize+1)
	if err != nil {
		return domain.ListPage{}, err
	}
	result := make([]domain.Entry, 0, min(pageSize, len(entries)))
	for _, entry := range entries[:min(pageSize, len(entries))] {
		path, joinErr := request.Directory.Join(entry.Name)
		if joinErr != nil {
			return domain.ListPage{}, domain.NewError(domain.ErrorInvalid, "stored directory name is invalid")
		}
		result = append(result, domainEntry(path, entry))
	}
	page := domain.ListPage{Current: current, Entries: result}
	if len(entries) > pageSize {
		after, keyErr := directorySortKey(request.Sort, entries[pageSize-1])
		if keyErr != nil {
			return domain.ListPage{}, keyErr
		}
		page.NextCursor, err = s.encodeListCursor(listCursor{
			SchemaVersion: 4, UserID: scope.UserID().String(), Area: areaName(scope.Area()),
			DirectoryPath: request.Directory.String(), DirectoryID: directoryID, ManifestID: manifestID,
			ParentID: parentID, ParentManifest: parentManifest,
			PageSize: pageSize, Sort: request.Sort, Descending: request.Descending, AfterSortKey: after,
			GateEpoch: gate.Epoch, GateVersion: gateEnvelope.LogicalVersion, ExpiresAt: s.engine.clock.Now().UTC().Add(s.engine.cursorTTL),
		})
		if err != nil {
			return domain.ListPage{}, err
		}
	}
	return page, nil
}

func (s *FileStore) LookupChildren(ctx context.Context, scope domain.Scope, request domain.ChildLookupRequest) (domain.ChildLookup, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return domain.ChildLookup{}, err
	}
	if !request.Directory.Valid() || len(request.Names) < 1 || len(request.Names) > 1000 {
		return domain.ChildLookup{}, domain.NewError(domain.ErrorInvalid, "child lookup request is invalid")
	}
	paths := make([]domain.UserPath, 0, len(request.Names))
	seen := make(map[string]struct{}, len(request.Names))
	for _, name := range request.Names {
		path, err := request.Directory.Join(name)
		if err != nil {
			return domain.ChildLookup{}, domain.NewError(domain.ErrorInvalid, "child lookup name is invalid")
		}
		if _, duplicate := seen[name]; duplicate {
			return domain.ChildLookup{}, domain.NewError(domain.ErrorInvalid, "child lookup contains duplicate names")
		}
		seen[name] = struct{}{}
		paths = append(paths, path)
	}
	view, err := s.resolveDirectoryMetadataView(ctx, scope, request.Directory)
	if err != nil {
		return domain.ChildLookup{}, err
	}
	result := domain.ChildLookup{Current: view.current, Entries: make([]domain.Entry, 0, len(paths))}
	for index, name := range request.Names {
		entry, err := s.directoryIndexEntry(ctx, scope, view.directoryID, view.snapshot.manifest, name)
		if err != nil {
			return domain.ChildLookup{}, err
		}
		result.Entries = append(result.Entries, domainEntry(paths[index], entry))
	}
	return result, nil
}

func (s *FileStore) Stat(ctx context.Context, scope domain.Scope, path domain.UserPath) (domain.Entry, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return domain.Entry{}, err
	}
	if !path.Valid() {
		return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "path is required")
	}
	if path.IsRoot() {
		snapshot, err := s.readDirectoryMetadata(ctx, scope, storageformat.RootDirectoryID, true)
		if err != nil {
			return domain.Entry{}, err
		}
		return domain.Entry{Path: path, Kind: domain.EntryDirectory, Size: snapshot.recursiveBytes, FileCount: snapshot.recursiveFileCount, ModifiedAt: time.Unix(0, 0).UTC(), Version: "root"}, nil
	}
	entry, err := s.resolveEntry(ctx, scope, path)
	if err != nil {
		return domain.Entry{}, err
	}
	if entry.Kind == domain.EntryDirectory {
		snapshot, err := s.readDirectoryMetadata(ctx, scope, entry.DirectoryID, false)
		if err != nil {
			return domain.Entry{}, err
		}
		if snapshot.recursiveBytes != entry.Size || snapshot.recursiveFileCount != entry.FileCount || snapshot.contentDigest != entry.ContentDigest {
			return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "directory recursive aggregate mismatch")
		}
	}
	return domainEntry(path, entry), nil
}

func (s *FileStore) CreateDirectory(ctx context.Context, scope domain.Scope, request domain.CreateDirectoryRequest) (domain.Entry, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return domain.Entry{}, err
	}
	if !request.Path.Valid() || request.Path.IsRoot() {
		return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "directory path is invalid")
	}
	conflict, err := domain.NormalizeConflictMode(request.Conflict)
	if err != nil {
		return domain.Entry{}, err
	}
	for range 64 {
		parentTrail, err := s.resolveDirectoryMetadataTrail(ctx, scope, request.Path.Parent())
		if err != nil {
			return domain.Entry{}, err
		}
		parentNode := parentTrail[len(parentTrail)-1]
		parent := parentNode.snapshot
		if parent.pending {
			continue
		}
		path, existing, err := s.resolveIndexedDirectoryDestination(ctx, scope, parentNode.directoryID, parent.manifest, request.Path, conflict, request.ExpectedVersion)
		if err != nil {
			return domain.Entry{}, err
		}
		childID, err := s.engine.ids.OpaqueID()
		if err != nil {
			return domain.Entry{}, err
		}
		emptyDigest, err := directoryContentDigest(nil)
		if err != nil {
			return domain.Entry{}, err
		}
		entry := storageformat.DirectoryEntry{
			Name: path.Name(), NameDigest: storageformat.NameDigest(path.Name()), Kind: domain.EntryDirectory,
			DirectoryID: childID, ContentDigest: emptyDigest, ModifiedAt: s.engine.clock.Now().UTC(),
		}
		entry.LogicalVersion, err = directoryEntryVersion(entry)
		if err != nil {
			return domain.Entry{}, err
		}
		updates := make(map[string]directoryUpdate)
		if err := applyDirectoryEntryChange(updates, parentTrail, existing, &entry); err != nil {
			return domain.Entry{}, err
		}
		preparedChild, err := s.prepareDirectory(ctx, scope, childID, nil, 1)
		if err != nil {
			return domain.Entry{}, err
		}
		childRootKey := storageformat.DirectoryRootKey(scope.UserID().String(), areaName(scope.Area()), childID)
		prerequisites := append([]storageformat.MutationObject(nil), preparedChild.prerequisites...)
		prerequisites = append(prerequisites, storageformat.MutationObject{Key: childRootKey.String(), Body: preparedChild.rootBody})
		operationID, err := s.engine.ids.OpaqueID()
		if err != nil {
			return domain.Entry{}, err
		}
		ownerID, err := s.engine.ids.OpaqueID()
		if err != nil {
			return domain.Entry{}, err
		}
		occurrence, err := catalogOccurrence(scope, path, entry)
		if err != nil {
			return domain.Entry{}, err
		}
		catalogChanges := []catalogChange{{post: &occurrence}}
		if existing != nil {
			existingEntries, collectErr := s.collectCatalogTree(ctx, scope, *existing)
			if collectErr != nil {
				return domain.Entry{}, collectErr
			}
			for _, item := range existingEntries {
				existingPath := path
				for _, segment := range item.segments {
					existingPath, collectErr = existingPath.Join(segment)
					if collectErr != nil {
						return domain.Entry{}, collectErr
					}
				}
				removed, collectErr := catalogOccurrence(scope, existingPath, item.entry)
				if collectErr != nil {
					return domain.Entry{}, collectErr
				}
				catalogChanges = append(catalogChanges, catalogChange{pre: &removed})
			}
		}
		operation, operationBody, err := s.buildFileOperation(ctx, scope.UserID(), operationID, ownerID, operationCreateDirectory, updates, prerequisites, nil, catalogChanges)
		if err != nil {
			if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrUnavailable) {
				continue
			}
			return domain.Entry{}, err
		}
		result, err := s.startFileOperation(ctx, operation, operationBody, "", "")
		if err == nil && result.State == domain.OperationSucceeded {
			return domainEntry(path, entry), nil
		}
		if err == nil {
			err = domain.NewError(domain.ErrorConflict, "directory changed during creation")
		}
		if errors.Is(err, domain.ErrUnavailable) {
			return domain.Entry{}, err
		}
		if !errors.Is(err, domain.ErrPreconditionFailed) && !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrUnavailable) {
			return domain.Entry{}, err
		}
		// A root race is retried from authoritative state. The next pass turns
		// a same-name winner into Conflict while preserving unrelated updates.
	}
	return domain.Entry{}, domain.NewError(domain.ErrorConflict, "directory changed too frequently")
}

func (s *FileStore) resolveEntry(ctx context.Context, scope domain.Scope, path domain.UserPath) (storageformat.DirectoryEntry, error) {
	directoryID := storageformat.RootDirectoryID
	for index, segment := range path.Segments() {
		snapshot, err := s.readDirectoryMetadata(ctx, scope, directoryID, directoryID == storageformat.RootDirectoryID)
		if err != nil {
			return storageformat.DirectoryEntry{}, err
		}
		entry, err := s.directoryIndexEntry(ctx, scope, directoryID, snapshot.manifest, segment)
		if err != nil {
			return storageformat.DirectoryEntry{}, err
		}
		if index == len(path.Segments())-1 {
			return entry, nil
		}
		if entry.Kind != domain.EntryDirectory || entry.DirectoryID == "" {
			return storageformat.DirectoryEntry{}, domain.NewError(domain.ErrorNotFound, "parent directory not found")
		}
		directoryID = entry.DirectoryID
	}
	return storageformat.DirectoryEntry{}, domain.NewError(domain.ErrorNotFound, "entry not found")
}

func (s *FileStore) resolveDirectory(ctx context.Context, scope domain.Scope, path domain.UserPath) (string, directorySnapshot, error) {
	if path.IsRoot() {
		snapshot, err := s.readDirectory(ctx, scope, storageformat.RootDirectoryID, true)
		return storageformat.RootDirectoryID, snapshot, err
	}
	entry, err := s.resolveEntry(ctx, scope, path)
	if err != nil {
		return "", directorySnapshot{}, err
	}
	if entry.Kind != domain.EntryDirectory || entry.DirectoryID == "" {
		return "", directorySnapshot{}, domain.NewError(domain.ErrorNotFound, "directory not found")
	}
	snapshot, err := s.readDirectory(ctx, scope, entry.DirectoryID, false)
	if err == nil && (snapshot.recursiveBytes != entry.Size || snapshot.recursiveFileCount != entry.FileCount || snapshot.contentDigest != entry.ContentDigest) {
		return "", directorySnapshot{}, domain.NewError(domain.ErrorInvalid, "directory recursive aggregate mismatch")
	}
	return entry.DirectoryID, snapshot, err
}

func (s *FileStore) resolveDirectoryMetadataView(ctx context.Context, scope domain.Scope, path domain.UserPath) (directoryView, error) {
	current, err := s.readDirectoryMetadata(ctx, scope, storageformat.RootDirectoryID, true)
	if err != nil {
		return directoryView{}, err
	}
	if path.IsRoot() {
		return directoryView{
			directoryID: storageformat.RootDirectoryID, snapshot: current,
			current: rootDirectoryEntry(path, current.recursiveBytes, current.recursiveFileCount),
		}, nil
	}
	directoryID := storageformat.RootDirectoryID
	parentID, parentManifest := "", ""
	var currentEntry storageformat.DirectoryEntry
	for _, segment := range path.Segments() {
		entry, lookupErr := s.directoryIndexEntry(ctx, scope, directoryID, current.manifest, segment)
		if lookupErr != nil || entry.Kind != domain.EntryDirectory || entry.DirectoryID == "" {
			if lookupErr != nil {
				return directoryView{}, lookupErr
			}
			return directoryView{}, domain.NewError(domain.ErrorNotFound, "directory not found")
		}
		parentID, parentManifest = directoryID, current.manifestID
		next, readErr := s.readDirectoryMetadata(ctx, scope, entry.DirectoryID, false)
		if readErr != nil {
			return directoryView{}, readErr
		}
		if next.recursiveBytes != entry.Size || next.recursiveFileCount != entry.FileCount || next.contentDigest != entry.ContentDigest {
			return directoryView{}, domain.NewError(domain.ErrorInvalid, "directory trail recursive aggregate mismatch")
		}
		directoryID, current, currentEntry = entry.DirectoryID, next, entry
	}
	return directoryView{
		directoryID: directoryID, snapshot: current, current: domainEntry(path, currentEntry),
		parentID: parentID, parentManifest: parentManifest,
	}, nil
}

func (s *FileStore) historicalCurrentDirectory(ctx context.Context, scope domain.Scope, path domain.UserPath, directoryID, parentID, parentManifest string, recursiveBytes, recursiveFileCount int64, contentDigest string) (domain.Entry, error) {
	if path.IsRoot() {
		return rootDirectoryEntry(path, recursiveBytes, recursiveFileCount), nil
	}
	manifest, err := s.readDirectoryManifest(ctx, scope, parentID, parentManifest)
	if err != nil {
		return domain.Entry{}, err
	}
	entry, err := s.directoryIndexEntry(ctx, scope, parentID, manifest, path.Name())
	if err != nil || entry.Kind != domain.EntryDirectory || entry.DirectoryID != directoryID || entry.Size != recursiveBytes || entry.FileCount != recursiveFileCount || entry.ContentDigest != contentDigest {
		return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "list cursor directory aggregate mismatch")
	}
	return domainEntry(path, entry), nil
}

func rootDirectoryEntry(path domain.UserPath, recursiveBytes, recursiveFileCount int64) domain.Entry {
	return domain.Entry{Path: path, Kind: domain.EntryDirectory, Size: recursiveBytes, FileCount: recursiveFileCount, ModifiedAt: time.Unix(0, 0).UTC(), Version: "root"}
}

func (s *FileStore) resolveDirectoryTrail(ctx context.Context, scope domain.Scope, path domain.UserPath) ([]directoryTrailNode, error) {
	root, err := s.readDirectory(ctx, scope, storageformat.RootDirectoryID, true)
	if err != nil {
		return nil, err
	}
	trail := []directoryTrailNode{{scope: scope, path: domain.MustParseUserPath("/"), directoryID: storageformat.RootDirectoryID, snapshot: root}}
	current := root
	currentPath := domain.MustParseUserPath("/")
	for _, segment := range path.Segments() {
		entry, found := findDirectoryEntry(current.entries, segment)
		if !found || entry.Kind != domain.EntryDirectory || entry.DirectoryID == "" {
			return nil, domain.NewError(domain.ErrorNotFound, "directory not found")
		}
		currentPath, err = currentPath.Join(segment)
		if err != nil {
			return nil, err
		}
		current, err = s.readDirectory(ctx, scope, entry.DirectoryID, false)
		if err != nil {
			return nil, err
		}
		if current.recursiveBytes != entry.Size || current.recursiveFileCount != entry.FileCount || current.contentDigest != entry.ContentDigest {
			return nil, s.classifyDirectoryTrailMismatch(ctx, trail[len(trail)-1], entry, current)
		}
		trail = append(trail, directoryTrailNode{scope: scope, path: currentPath, directoryID: entry.DirectoryID, entry: entry, snapshot: current})
	}
	return trail, nil
}

func (s *FileStore) resolveDirectoryMetadataTrail(ctx context.Context, scope domain.Scope, path domain.UserPath) ([]directoryTrailNode, error) {
	root, err := s.readDirectoryMetadata(ctx, scope, storageformat.RootDirectoryID, true)
	if err != nil {
		return nil, err
	}
	trail := []directoryTrailNode{{scope: scope, path: domain.MustParseUserPath("/"), directoryID: storageformat.RootDirectoryID, snapshot: root}}
	current := root
	currentPath := domain.MustParseUserPath("/")
	for _, segment := range path.Segments() {
		entry, err := s.directoryIndexEntry(ctx, scope, trail[len(trail)-1].directoryID, current.manifest, segment)
		if err != nil {
			return nil, err
		}
		if entry.Kind != domain.EntryDirectory || entry.DirectoryID == "" {
			return nil, domain.NewError(domain.ErrorNotFound, "directory not found")
		}
		currentPath, err = currentPath.Join(segment)
		if err != nil {
			return nil, err
		}
		next, err := s.readDirectoryMetadata(ctx, scope, entry.DirectoryID, false)
		if err != nil {
			return nil, err
		}
		if next.recursiveBytes != entry.Size || next.recursiveFileCount != entry.FileCount || next.contentDigest != entry.ContentDigest {
			return nil, s.classifyDirectoryTrailMismatch(ctx, trail[len(trail)-1], entry, next)
		}
		trail = append(trail, directoryTrailNode{scope: scope, path: currentPath, directoryID: entry.DirectoryID, entry: entry, snapshot: next})
		current = next
	}
	return trail, nil
}

func (s *FileStore) classifyDirectoryTrailMismatch(ctx context.Context, parent directoryTrailNode, childEntry storageformat.DirectoryEntry, child directorySnapshot) error {
	parentAgain, parentErr := s.readDirectory(ctx, parent.scope, parent.directoryID, parent.path.IsRoot())
	if parentErr != nil {
		return parentErr
	}
	if !sameDirectoryVisibility(parent.snapshot, parentAgain) {
		return domain.NewError(domain.ErrorUnavailable, "directory changed while resolving recursive aggregates")
	}
	childAgain, childErr := s.readDirectory(ctx, parent.scope, childEntry.DirectoryID, false)
	if childErr != nil {
		if errors.Is(childErr, domain.ErrNotFound) {
			return domain.NewError(domain.ErrorInvalid, "directory aggregate references a missing child root")
		}
		return childErr
	}
	if !sameDirectoryVisibility(child, childAgain) {
		return domain.NewError(domain.ErrorUnavailable, "directory changed while resolving recursive aggregates")
	}
	return domain.NewError(domain.ErrorInvalid, "directory trail recursive aggregate mismatch")
}

func sameDirectoryVisibility(first, second directorySnapshot) bool {
	return first.exists == second.exists &&
		first.envelope.LogicalVersion == second.envelope.LogicalVersion &&
		first.manifestID == second.manifestID &&
		first.recursiveBytes == second.recursiveBytes &&
		first.recursiveFileCount == second.recursiveFileCount &&
		first.contentAccumulator == second.contentAccumulator &&
		first.contentDigest == second.contentDigest &&
		first.pending == second.pending &&
		first.transitionState == second.transitionState &&
		first.transitionFence == second.transitionFence
}

func (s *FileStore) readDirectory(ctx context.Context, scope domain.Scope, directoryID string, allowVirtualRoot bool) (directorySnapshot, error) {
	snapshot, err := s.readDirectoryMetadata(ctx, scope, directoryID, allowVirtualRoot)
	if err != nil || snapshot.manifestID == "" {
		return snapshot, err
	}
	entries, err := s.readManifestPageEntries(ctx, scope, directoryID, snapshot.manifest)
	if err != nil {
		return directorySnapshot{}, err
	}
	computedBytes, err := recursiveByteSize(entries)
	if err != nil || computedBytes != snapshot.recursiveBytes {
		return directorySnapshot{}, domain.NewError(domain.ErrorInvalid, "directory manifest entries recursive byte aggregate mismatch")
	}
	computedFiles, err := recursiveFileCount(entries)
	if err != nil || computedFiles != snapshot.recursiveFileCount {
		return directorySnapshot{}, domain.NewError(domain.ErrorInvalid, "directory manifest entries recursive file count mismatch")
	}
	computedAccumulator, computedDigest, err := directoryContentIdentity(entries)
	if err != nil || computedAccumulator != snapshot.contentAccumulator || computedDigest != snapshot.contentDigest {
		return directorySnapshot{}, domain.NewError(domain.ErrorInvalid, "directory manifest entries content digest mismatch")
	}
	snapshot.entries = entries
	return snapshot, nil
}

func (s *FileStore) readDirectoryMetadata(ctx context.Context, scope domain.Scope, directoryID string, allowVirtualRoot bool) (directorySnapshot, error) {
	key := storageformat.DirectoryRootKey(scope.UserID().String(), areaName(scope.Area()), directoryID)
	object, err := s.engine.backend.Get(ctx, key)
	if errors.Is(err, domain.ErrNotFound) && allowVirtualRoot && directoryID == storageformat.RootDirectoryID {
		return directorySnapshot{root: storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: directoryID}}, nil
	}
	if err != nil {
		return directorySnapshot{}, err
	}
	var envelope storageformat.Envelope
	var root storageformat.DirectoryRoot
	if err := storageformat.DecodeEnvelope(object.Body, key, directoryRootSchema, &envelope, &root); err != nil {
		return directorySnapshot{}, err
	}
	if root.SchemaVersion != 1 || root.DirectoryID != directoryID || (root.ManifestID == "" && root.Pending == nil) {
		return directorySnapshot{}, domain.NewError(domain.ErrorInvalid, "invalid directory root")
	}
	if root.RecursiveBytes < 0 || root.RecursiveFileCount < 0 {
		return directorySnapshot{}, domain.NewError(domain.ErrorInvalid, "invalid directory recursive aggregate")
	}
	manifestID := root.ManifestID
	recursiveBytes := root.RecursiveBytes
	recursiveFileCount := root.RecursiveFileCount
	contentAccumulator := root.ContentAccumulator
	contentDigest := root.ContentDigest
	pending := root.Pending != nil
	transitionState := storageformat.FileOperationState("")
	var transitionFence uint64
	if pending {
		if root.Pending.OperationID == "" || root.Pending.Fence == 0 || root.Pending.PostManifestID == "" || root.Pending.PreManifestID != root.ManifestID || root.Pending.PostRecursiveBytes < 0 || root.Pending.PostRecursiveFileCount < 0 || root.Pending.PostContentAccumulator == "" || root.Pending.PostContentDigest == "" {
			return directorySnapshot{}, domain.NewError(domain.ErrorInvalid, "invalid pending directory transition")
		}
		operation, operationErr := s.readFileOperation(ctx, scope.UserID(), root.Pending.OperationID)
		if operationErr != nil {
			return directorySnapshot{}, operationErr
		}
		if operation.Fence < root.Pending.Fence {
			return directorySnapshot{}, domain.NewError(domain.ErrorInvalid, "directory transition fence is invalid")
		}
		transitionState = operation.State
		transitionFence = operation.Fence
		if operation.State == storageformat.FileOperationCommitted || operation.State == storageformat.FileOperationSucceeded {
			manifestID = root.Pending.PostManifestID
			recursiveBytes = root.Pending.PostRecursiveBytes
			recursiveFileCount = root.Pending.PostRecursiveFileCount
			contentAccumulator = root.Pending.PostContentAccumulator
			contentDigest = root.Pending.PostContentDigest
		}
	}
	if manifestID == "" {
		if recursiveBytes != 0 || recursiveFileCount != 0 {
			return directorySnapshot{}, domain.NewError(domain.ErrorInvalid, "empty directory root recursive aggregate mismatch")
		}
		return directorySnapshot{object: object, exists: true, envelope: envelope, root: root, recursiveBytes: recursiveBytes, recursiveFileCount: recursiveFileCount, contentAccumulator: contentAccumulator, contentDigest: contentDigest, pending: pending, transitionState: transitionState, transitionFence: transitionFence}, nil
	}
	manifest, err := s.readDirectoryManifest(ctx, scope, directoryID, manifestID)
	if err != nil {
		return directorySnapshot{}, err
	}
	if manifest.RecursiveBytes != recursiveBytes || manifest.RecursiveFileCount != recursiveFileCount || manifest.ContentAccumulator != contentAccumulator || manifest.ContentDigest != contentDigest {
		return directorySnapshot{}, domain.NewError(domain.ErrorInvalid, "directory root and manifest recursive aggregate mismatch")
	}
	return directorySnapshot{object: object, exists: true, envelope: envelope, root: root, manifestID: manifestID, manifest: manifest, recursiveBytes: recursiveBytes, recursiveFileCount: recursiveFileCount, contentAccumulator: contentAccumulator, contentDigest: contentDigest, pending: pending, transitionState: transitionState, transitionFence: transitionFence}, nil
}

func (s *FileStore) readManifestSnapshot(ctx context.Context, scope domain.Scope, directoryID, manifestID string) (storageformat.DirectoryManifest, []storageformat.DirectoryEntry, error) {
	manifest, err := s.readDirectoryManifest(ctx, scope, directoryID, manifestID)
	if err != nil {
		return storageformat.DirectoryManifest{}, nil, err
	}
	entries, err := s.readManifestPageEntries(ctx, scope, directoryID, manifest)
	if err != nil {
		return storageformat.DirectoryManifest{}, nil, err
	}
	computedBytes, err := recursiveByteSize(entries)
	if err != nil || computedBytes != manifest.RecursiveBytes {
		return storageformat.DirectoryManifest{}, nil, domain.NewError(domain.ErrorInvalid, "directory manifest entries recursive byte aggregate mismatch")
	}
	computedFiles, err := recursiveFileCount(entries)
	if err != nil || computedFiles != manifest.RecursiveFileCount {
		return storageformat.DirectoryManifest{}, nil, domain.NewError(domain.ErrorInvalid, "directory manifest entries recursive file count mismatch")
	}
	computedAccumulator, computedDigest, err := directoryContentIdentity(entries)
	if err != nil || computedAccumulator != manifest.ContentAccumulator || computedDigest != manifest.ContentDigest {
		return storageformat.DirectoryManifest{}, nil, domain.NewError(domain.ErrorInvalid, "directory manifest content digest mismatch")
	}
	return manifest, entries, nil
}

func (s *FileStore) readDirectoryManifest(ctx context.Context, scope domain.Scope, directoryID, manifestID string) (storageformat.DirectoryManifest, error) {
	key := storageformat.DirectoryManifestKey(scope.UserID().String(), areaName(scope.Area()), directoryID, manifestID)
	object, err := s.engine.backend.Get(ctx, key)
	if err != nil {
		return storageformat.DirectoryManifest{}, err
	}
	var envelope storageformat.Envelope
	var manifest storageformat.DirectoryManifest
	if err := storageformat.DecodeEnvelope(object.Body, key, directoryManifestSchema, &envelope, &manifest); err != nil {
		return storageformat.DirectoryManifest{}, err
	}
	accumulator, accumulatorErr := decodeDirectoryContentAccumulator(manifest.ContentAccumulator)
	expectedDigest, digestErr := directoryContentAccumulatorDigest(accumulator, manifest.EntryCount)
	if manifest.SchemaVersion != 2 || manifest.DirectoryID != directoryID || manifest.ManifestID != manifestID || manifest.EntryCount < 0 || manifest.RecursiveBytes < 0 || manifest.RecursiveFileCount < 0 || accumulatorErr != nil || digestErr != nil || manifest.ContentDigest == "" || expectedDigest != manifest.ContentDigest || len(manifest.PageIDs) != 0 || manifest.EntryCount == 0 && (manifest.IndexRootID != "" || manifest.IndexRootDigest != "") || manifest.EntryCount > 0 && (manifest.IndexRootID == "" || manifest.IndexRootDigest == "") || validateDirectorySortIndexRoots(manifest.SortIndexes, manifest.EntryCount) != nil {
		return storageformat.DirectoryManifest{}, domain.NewError(domain.ErrorInvalid, "invalid directory manifest")
	}
	return manifest, nil
}

func (s *FileStore) readManifestPageEntries(ctx context.Context, scope domain.Scope, directoryID string, manifest storageformat.DirectoryManifest) ([]storageformat.DirectoryEntry, error) {
	if manifest.SchemaVersion == 2 {
		entries, err := s.collectDirectoryIndexEntries(ctx, scope, directoryID, manifest, "", false, manifest.EntryCount)
		if err != nil {
			return nil, err
		}
		if len(entries) != manifest.EntryCount {
			return nil, domain.NewError(domain.ErrorInvalid, "directory index entry count mismatch")
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].NameDigest == entries[j].NameDigest {
				return entries[i].Name < entries[j].Name
			}
			return entries[i].NameDigest < entries[j].NameDigest
		})
		if err := validateDirectoryEntries(entries); err != nil {
			return nil, err
		}
		return entries, nil
	}
	entries := make([]storageformat.DirectoryEntry, 0, manifest.EntryCount)
	for _, pageID := range manifest.PageIDs {
		pageKey := storageformat.DirectoryPageKey(scope.UserID().String(), areaName(scope.Area()), directoryID, pageID)
		pageObject, getErr := s.engine.backend.Get(ctx, pageKey)
		if getErr != nil {
			return nil, getErr
		}
		var pageEnvelope storageformat.Envelope
		var page storageformat.DirectoryPage
		if err := storageformat.DecodeEnvelope(pageObject.Body, pageKey, directoryPageSchema, &pageEnvelope, &page); err != nil {
			return nil, err
		}
		if page.SchemaVersion != 1 || page.DirectoryID != directoryID || page.PageID != pageID || len(page.Entries) > maxEntriesPerPage {
			return nil, domain.NewError(domain.ErrorInvalid, "invalid directory page")
		}
		entries = append(entries, page.Entries...)
	}
	if len(entries) != manifest.EntryCount {
		return nil, domain.NewError(domain.ErrorInvalid, "directory entry count mismatch")
	}
	if err := validateDirectoryEntries(entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func (s *FileStore) prepareDirectory(ctx context.Context, scope domain.Scope, directoryID string, entries []storageformat.DirectoryEntry, revision uint64) (preparedDirectory, error) {
	if err := validateDirectoryEntries(entries); err != nil {
		return preparedDirectory{}, err
	}
	indexRoot, nodes, err := s.buildDirectoryIndex(scope, directoryID, entries)
	if err != nil {
		return preparedDirectory{}, err
	}
	sortRoots, sortNodes, err := s.buildDirectorySortIndexes(scope, directoryID, entries)
	if err != nil {
		return preparedDirectory{}, err
	}
	nodes = append(nodes, sortNodes...)
	contentAccumulator, contentDigest, err := directoryContentIdentity(entries)
	if err != nil {
		return preparedDirectory{}, err
	}
	return s.prepareDirectoryWithIndex(scope, directoryID, entries, revision, indexRoot, sortRoots, nodes, contentAccumulator, contentDigest)
}

func (s *FileStore) prepareDirectoryUpdate(ctx context.Context, scope domain.Scope, directoryID string, snapshot directorySnapshot, entries []storageformat.DirectoryEntry, revision uint64) (preparedDirectory, error) {
	if err := validateDirectoryEntries(entries); err != nil {
		return preparedDirectory{}, err
	}
	if snapshot.manifestID == "" && len(snapshot.entries) == 0 {
		indexRoot, nodes, err := s.buildDirectoryIndex(scope, directoryID, entries)
		if err != nil {
			return preparedDirectory{}, err
		}
		sortRoots, sortNodes, err := s.buildDirectorySortIndexes(scope, directoryID, entries)
		if err != nil {
			return preparedDirectory{}, err
		}
		nodes = append(nodes, sortNodes...)
		contentAccumulator, contentDigest, err := directoryContentIdentity(entries)
		if err != nil {
			return preparedDirectory{}, err
		}
		return s.prepareDirectoryWithIndex(scope, directoryID, entries, revision, indexRoot, sortRoots, nodes, contentAccumulator, contentDigest)
	}
	if !snapshot.exists || snapshot.manifest.SchemaVersion != 2 || snapshot.manifest.EntryCount != len(snapshot.entries) {
		return preparedDirectory{}, domain.NewError(domain.ErrorInvalid, "directory update has no current index snapshot")
	}
	indexRoot, nodes, err := s.mutateDirectoryIndex(ctx, scope, directoryID, snapshot.manifest, snapshot.entries, entries)
	if err != nil {
		return preparedDirectory{}, err
	}
	names, replacements := directoryEntryChanges(snapshot.entries, entries)
	changes := make(map[string]directoryEntryMutation, len(names))
	old := make(map[string]storageformat.DirectoryEntry, len(snapshot.entries))
	for _, entry := range snapshot.entries {
		old[entry.Name] = entry
	}
	for _, name := range names {
		change := directoryEntryMutation{after: replacements[name]}
		if entry, found := old[name]; found {
			before := entry
			change.before = &before
		}
		changes[name] = change
	}
	sortRoots, sortNodes, err := s.mutateDirectorySortIndexes(ctx, scope, directoryID, snapshot.manifest, changes, len(entries))
	if err != nil {
		return preparedDirectory{}, err
	}
	nodes = append(nodes, sortNodes...)
	contentAccumulator, contentDigest, err := updateDirectoryContentIdentity(snapshot.contentAccumulator, snapshot.entries, entries)
	if err != nil {
		return preparedDirectory{}, err
	}
	return s.prepareDirectoryWithIndex(scope, directoryID, entries, revision, indexRoot, sortRoots, nodes, contentAccumulator, contentDigest)
}

func (s *FileStore) prepareDirectoryMutation(ctx context.Context, update directoryUpdate, revision uint64) (preparedDirectory, error) {
	if !update.scope.Valid() || update.directoryID == "" || !update.snapshot.exists && update.directoryID != storageformat.RootDirectoryID || update.entryCount < 0 || update.recursiveBytes < 0 || update.recursiveFileCount < 0 || len(update.changes) == 0 {
		return preparedDirectory{}, domain.NewError(domain.ErrorInvalid, "invalid directory mutation")
	}
	manifest := update.snapshot.manifest
	if update.snapshot.manifestID == "" {
		if update.snapshot.recursiveBytes != 0 || update.snapshot.recursiveFileCount != 0 || manifest.EntryCount != 0 {
			return preparedDirectory{}, domain.NewError(domain.ErrorInvalid, "invalid empty directory mutation source")
		}
		manifest = storageformat.DirectoryManifest{SchemaVersion: 2, DirectoryID: update.directoryID, EntryCount: 0, RecursiveBytes: 0, RecursiveFileCount: 0, ContentAccumulator: update.snapshot.contentAccumulator, ContentDigest: update.snapshot.contentDigest}
	}
	indexRoot, nodes, err := s.mutateDirectoryIndexChanges(ctx, update.scope, update.directoryID, manifest, update.changes)
	if err != nil {
		return preparedDirectory{}, err
	}
	sortRoots, sortNodes, err := s.mutateDirectorySortIndexes(ctx, update.scope, update.directoryID, manifest, update.changes, update.entryCount)
	if err != nil {
		return preparedDirectory{}, err
	}
	nodes = append(nodes, sortNodes...)
	if update.entryCount > 0 && (indexRoot.EntryCount != uint64(update.entryCount) || indexRoot.RecursiveBytes != update.recursiveBytes || indexRoot.RecursiveFileCount != update.recursiveFileCount) {
		return preparedDirectory{}, domain.NewError(domain.ErrorInvalid, "directory mutation index aggregate mismatch")
	}
	return s.prepareDirectoryWithIndexAggregates(update.scope, update.directoryID, update.entryCount, update.recursiveBytes, update.recursiveFileCount, revision, indexRoot, sortRoots, nodes, update.contentAccumulator, update.contentDigest)
}

func (s *FileStore) prepareDirectoryWithIndex(scope domain.Scope, directoryID string, entries []storageformat.DirectoryEntry, revision uint64, indexRoot storageformat.DirectoryIndexChild, sortRoots []storageformat.DirectorySortIndexRoot, nodes []storageformat.MutationObject, contentAccumulator, contentDigest string) (preparedDirectory, error) {
	recursiveBytes, err := recursiveByteSize(entries)
	if err != nil {
		return preparedDirectory{}, err
	}
	fileCount, err := recursiveFileCount(entries)
	if err != nil {
		return preparedDirectory{}, err
	}
	return s.prepareDirectoryWithIndexAggregates(scope, directoryID, len(entries), recursiveBytes, fileCount, revision, indexRoot, sortRoots, nodes, contentAccumulator, contentDigest)
}

func (s *FileStore) prepareDirectoryWithIndexAggregates(scope domain.Scope, directoryID string, entryCount int, recursiveBytes, fileCount int64, revision uint64, indexRoot storageformat.DirectoryIndexChild, sortRoots []storageformat.DirectorySortIndexRoot, nodes []storageformat.MutationObject, contentAccumulator, contentDigest string) (preparedDirectory, error) {
	if entryCount < 0 || recursiveBytes < 0 || fileCount < 0 || entryCount == 0 && (indexRoot.NodeID != "" || indexRoot.NodeDigest != "") || entryCount > 0 && (indexRoot.NodeID == "" || indexRoot.NodeDigest == "") || validateDirectorySortIndexRoots(sortRoots, entryCount) != nil {
		return preparedDirectory{}, domain.NewError(domain.ErrorInvalid, "invalid directory index aggregates")
	}
	accumulator, err := decodeDirectoryContentAccumulator(contentAccumulator)
	if err != nil {
		return preparedDirectory{}, err
	}
	expectedDigest, err := directoryContentAccumulatorDigest(accumulator, entryCount)
	if err != nil || expectedDigest != contentDigest {
		return preparedDirectory{}, domain.NewError(domain.ErrorInvalid, "directory content identity mismatch")
	}
	manifestID, err := s.engine.ids.OpaqueID()
	if err != nil {
		return preparedDirectory{}, err
	}
	manifestKey := storageformat.DirectoryManifestKey(scope.UserID().String(), areaName(scope.Area()), directoryID, manifestID)
	manifestBody, err := storageformat.EncodeEnvelope(directoryManifestSchema, manifestKey, 1, storageformat.DirectoryManifest{
		SchemaVersion: 2, DirectoryID: directoryID, ManifestID: manifestID, IndexRootID: indexRoot.NodeID, IndexRootDigest: indexRoot.NodeDigest,
		SortIndexes: sortRoots, EntryCount: entryCount, RecursiveBytes: recursiveBytes, RecursiveFileCount: fileCount, ContentAccumulator: contentAccumulator, ContentDigest: contentDigest, CreatedAt: s.engine.clock.Now().UTC(),
	})
	if err != nil {
		return preparedDirectory{}, err
	}
	prerequisites := append(nodes, storageformat.MutationObject{Key: manifestKey.String(), Body: manifestBody})
	sort.Slice(prerequisites, func(i, j int) bool { return prerequisites[i].Key < prerequisites[j].Key })
	rootKey := storageformat.DirectoryRootKey(scope.UserID().String(), areaName(scope.Area()), directoryID)
	rootBody, err := storageformat.EncodeEnvelope(directoryRootSchema, rootKey, revision, storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: manifestID, RecursiveBytes: recursiveBytes, RecursiveFileCount: fileCount, ContentAccumulator: contentAccumulator, ContentDigest: contentDigest})
	if err != nil {
		return preparedDirectory{}, err
	}
	return preparedDirectory{manifestID: manifestID, recursiveBytes: recursiveBytes, recursiveFileCount: fileCount, contentAccumulator: contentAccumulator, contentDigest: contentDigest, rootBody: rootBody, prerequisites: prerequisites}, nil
}

func validateDirectoryEntries(entries []storageformat.DirectoryEntry) error {
	previousDigest, previousName := "", ""
	for _, entry := range entries {
		if entry.Name == "" || storageformat.NameDigest(entry.Name) != entry.NameDigest || entry.LogicalVersion == "" || entry.Size < 0 || entry.ModifiedAt.IsZero() {
			return domain.NewError(domain.ErrorInvalid, "invalid directory entry")
		}
		if entry.Kind == domain.EntryDirectory {
			if entry.DirectoryID == "" || entry.BlobID != "" || entry.MediaType != "" || entry.FileCount < 0 {
				return domain.NewError(domain.ErrorInvalid, "invalid directory entry target")
			}
		} else if entry.Kind == domain.EntryFile {
			if entry.BlobID == "" || entry.DirectoryID != "" || entry.MediaType == "" || entry.FileCount != 0 {
				return domain.NewError(domain.ErrorInvalid, "invalid file entry target")
			}
		} else {
			return domain.NewError(domain.ErrorInvalid, "invalid directory entry kind")
		}
		version, err := directoryEntryVersion(entry)
		if err != nil || version != entry.LogicalVersion {
			return domain.NewError(domain.ErrorInvalid, "directory entry logical version mismatch")
		}
		if entry.NameDigest < previousDigest || entry.NameDigest == previousDigest && entry.Name <= previousName {
			return domain.NewError(domain.ErrorInvalid, "directory entries are not uniquely sorted")
		}
		if entry.NameDigest == previousDigest && entry.Name != previousName {
			return domain.NewError(domain.ErrorInvalid, "directory name digest collision")
		}
		previousDigest, previousName = entry.NameDigest, entry.Name
	}
	return nil
}

func recursiveByteSize(entries []storageformat.DirectoryEntry) (int64, error) {
	var total int64
	for _, entry := range entries {
		if entry.Size < 0 || entry.Size > math.MaxInt64-total {
			return 0, domain.NewError(domain.ErrorInvalid, "directory recursive byte aggregate overflows")
		}
		total += entry.Size
	}
	return total, nil
}

func recursiveFileCount(entries []storageformat.DirectoryEntry) (int64, error) {
	var total int64
	for _, entry := range entries {
		count := int64(1)
		if entry.Kind == domain.EntryDirectory {
			count = entry.FileCount
		}
		if count < 0 || count > math.MaxInt64-total {
			return 0, domain.NewError(domain.ErrorInvalid, "directory recursive file count overflows")
		}
		total += count
	}
	return total, nil
}

type directoryContentItem struct {
	Name          string           `json:"name"`
	Kind          domain.EntryKind `json:"kind"`
	Size          int64            `json:"size"`
	FileCount     int64            `json:"fileCount,omitempty"`
	MD5           string           `json:"md5,omitempty"`
	CRC32C        string           `json:"crc32c,omitempty"`
	ContentDigest string           `json:"contentDigest,omitempty"`
}

func directoryContentDigest(entries []storageformat.DirectoryEntry) (string, error) {
	_, digest, err := directoryContentIdentity(entries)
	return digest, err
}

const directoryContentAccumulatorBytes = 64

func directoryContentItemFor(entry storageformat.DirectoryEntry) (directoryContentItem, error) {
	item := directoryContentItem{Name: entry.Name, Kind: entry.Kind, Size: entry.Size, FileCount: entry.FileCount}
	switch entry.Kind {
	case domain.EntryFile:
		fingerprint := objectstore.ContentFingerprint{MD5: entry.MD5, CRC32C: entry.CRC32C}
		if entry.SHA256 != "" || !fingerprint.Complete() || entry.ContentDigest != "" {
			return directoryContentItem{}, domain.NewError(domain.ErrorInvalid, "file entry has no current provider content fingerprint")
		}
		item.MD5, item.CRC32C = entry.MD5, entry.CRC32C
	case domain.EntryDirectory:
		if entry.ContentDigest == "" || entry.MD5 != "" || entry.CRC32C != "" || entry.SHA256 != "" {
			return directoryContentItem{}, domain.NewError(domain.ErrorInvalid, "directory entry has no subtree content digest")
		}
		item.ContentDigest = entry.ContentDigest
	default:
		return directoryContentItem{}, domain.NewError(domain.ErrorInvalid, "invalid directory entry kind")
	}
	return item, nil
}

func directoryContentContribution(entry storageformat.DirectoryEntry) ([32]byte, error) {
	item, err := directoryContentItemFor(entry)
	if err != nil {
		return [32]byte{}, err
	}
	body, err := storageformat.EncodeCanonical(item)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(append([]byte("endlessfs-directory-content-item-v2\x00"), body...)), nil
}

func applyDirectoryContentContribution(accumulator *[directoryContentAccumulatorBytes]byte, contribution [32]byte, remove bool) {
	for index := range contribution {
		accumulator[index] ^= contribution[index]
	}
	carry := 0
	for index := len(contribution) - 1; index >= 0; index-- {
		position := 32 + index
		if remove {
			value := int(accumulator[position]) - int(contribution[index]) - carry
			if value < 0 {
				value += 256
				carry = 1
			} else {
				carry = 0
			}
			accumulator[position] = byte(value)
			continue
		}
		value := int(accumulator[position]) + int(contribution[index]) + carry
		accumulator[position] = byte(value)
		carry = value >> 8
	}
}

func encodeDirectoryContentAccumulator(accumulator [directoryContentAccumulatorBytes]byte) string {
	return base64.RawURLEncoding.EncodeToString(accumulator[:])
}

func decodeDirectoryContentAccumulator(value string) ([directoryContentAccumulatorBytes]byte, error) {
	var accumulator [directoryContentAccumulatorBytes]byte
	body, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(body) != len(accumulator) || base64.RawURLEncoding.EncodeToString(body) != value {
		return accumulator, domain.NewError(domain.ErrorInvalid, "invalid directory content accumulator")
	}
	copy(accumulator[:], body)
	return accumulator, nil
}

func directoryContentAccumulatorDigest(accumulator [directoryContentAccumulatorBytes]byte, entryCount int) (string, error) {
	if entryCount < 0 {
		return "", domain.NewError(domain.ErrorInvalid, "invalid directory content entry count")
	}
	count := make([]byte, 8)
	binary.BigEndian.PutUint64(count, uint64(entryCount))
	body := append([]byte("endlessfs-directory-content-v2\x00"), accumulator[:]...)
	body = append(body, count...)
	return storageformat.Digest(body), nil
}

func directoryContentIdentity(entries []storageformat.DirectoryEntry) (string, string, error) {
	var accumulator [directoryContentAccumulatorBytes]byte
	for _, entry := range entries {
		contribution, err := directoryContentContribution(entry)
		if err != nil {
			return "", "", err
		}
		applyDirectoryContentContribution(&accumulator, contribution, false)
	}
	digest, err := directoryContentAccumulatorDigest(accumulator, len(entries))
	return encodeDirectoryContentAccumulator(accumulator), digest, err
}

func updateDirectoryContentIdentity(encoded string, before, after []storageformat.DirectoryEntry) (string, string, error) {
	return updateDirectoryContentIdentityAtCount(encoded, before, after, len(after))
}

func updateDirectoryContentIdentityAtCount(encoded string, before, after []storageformat.DirectoryEntry, finalCount int) (string, string, error) {
	accumulator, err := decodeDirectoryContentAccumulator(encoded)
	if err != nil {
		return "", "", err
	}
	oldByName := make(map[string]storageformat.DirectoryEntry, len(before))
	for _, entry := range before {
		oldByName[entry.Name] = entry
	}
	names, changes := directoryEntryChanges(before, after)
	for _, name := range names {
		if old, exists := oldByName[name]; exists {
			contribution, contributionErr := directoryContentContribution(old)
			if contributionErr != nil {
				return "", "", contributionErr
			}
			applyDirectoryContentContribution(&accumulator, contribution, true)
		}
		if replacement := changes[name]; replacement != nil {
			contribution, contributionErr := directoryContentContribution(*replacement)
			if contributionErr != nil {
				return "", "", contributionErr
			}
			applyDirectoryContentContribution(&accumulator, contribution, false)
		}
	}
	digest, err := directoryContentAccumulatorDigest(accumulator, finalCount)
	return encodeDirectoryContentAccumulator(accumulator), digest, err
}

func currentDirectoryUpdate(updates map[string]directoryUpdate, node directoryTrailNode) directoryUpdate {
	key := storageformat.DirectoryRootKey(node.scope.UserID().String(), areaName(node.scope.Area()), node.directoryID).String()
	if update, exists := updates[key]; exists {
		return update
	}
	accumulator := node.snapshot.contentAccumulator
	digest := node.snapshot.contentDigest
	entryCount := node.snapshot.manifest.EntryCount
	if entryCount == 0 && len(node.snapshot.entries) != 0 {
		entryCount = len(node.snapshot.entries)
	}
	if accumulator == "" && len(node.snapshot.entries) != 0 {
		accumulator, digest, _ = directoryContentIdentity(node.snapshot.entries)
	}
	if accumulator == "" && entryCount == 0 {
		accumulator = encodeDirectoryContentAccumulator([directoryContentAccumulatorBytes]byte{})
		decoded, _ := decodeDirectoryContentAccumulator(accumulator)
		digest, _ = directoryContentAccumulatorDigest(decoded, 0)
	}
	return directoryUpdate{
		scope: node.scope, path: node.path, directoryID: node.directoryID, entry: node.entry, snapshot: node.snapshot,
		changes: make(map[string]directoryEntryMutation), entryCount: entryCount,
		recursiveBytes: node.snapshot.recursiveBytes, recursiveFileCount: node.snapshot.recursiveFileCount,
		contentAccumulator: accumulator, contentDigest: digest,
	}
}

func applyDirectoryChange(updates map[string]directoryUpdate, trail []directoryTrailNode, entries []storageformat.DirectoryEntry) error {
	if len(trail) == 0 {
		return domain.NewError(domain.ErrorInvalid, "directory aggregate trail is empty")
	}
	leaf := trail[len(trail)-1]
	before := append([]storageformat.DirectoryEntry(nil), leaf.snapshot.entries...)
	names, replacements := directoryEntryChanges(before, entries)
	old := make(map[string]storageformat.DirectoryEntry, len(before))
	for _, entry := range before {
		old[entry.Name] = entry
	}
	for _, name := range names {
		var beforeEntry *storageformat.DirectoryEntry
		if value, ok := old[name]; ok {
			copy := value
			beforeEntry = &copy
		}
		if err := applyDirectoryEntryChange(updates, trail, beforeEntry, replacements[name]); err != nil {
			return err
		}
	}
	return nil
}

func applyDirectoryEntryChange(updates map[string]directoryUpdate, trail []directoryTrailNode, before, after *storageformat.DirectoryEntry) error {
	if len(trail) == 0 || before == nil && after == nil {
		return domain.NewError(domain.ErrorInvalid, "directory entry mutation is empty")
	}
	name := ""
	if before != nil {
		name = before.Name
	}
	if after != nil {
		if name != "" && after.Name != name {
			return domain.NewError(domain.ErrorInvalid, "directory entry mutation changes its key")
		}
		name = after.Name
	}
	currentBefore, currentAfter := before, after
	for index := len(trail) - 1; index >= 0; index-- {
		node := trail[index]
		update := currentDirectoryUpdate(updates, node)
		priorBytes, priorFiles, priorDigest := update.recursiveBytes, update.recursiveFileCount, update.contentDigest
		if existing, ok := update.changes[name]; ok {
			if !sameOptionalDirectoryEntry(existing.after, currentBefore) {
				return domain.NewError(domain.ErrorPreconditionFailed, "directory entry changed while composing mutation")
			}
			currentBefore = existing.after
		} else if currentBefore == nil {
			// A creation is valid only when the caller performed the indexed
			// absence check against this exact manifest.
		} else if currentBefore.Name != name {
			return domain.NewError(domain.ErrorInvalid, "directory entry mutation key mismatch")
		}
		beforeBytes, beforeFiles, err := directoryEntryAggregates(currentBefore)
		if err != nil {
			return err
		}
		afterBytes, afterFiles, err := directoryEntryAggregates(currentAfter)
		if err != nil {
			return err
		}
		update.recursiveBytes, err = addAggregateDelta(update.recursiveBytes, afterBytes-beforeBytes, "directory recursive byte aggregate")
		if err != nil {
			return err
		}
		update.recursiveFileCount, err = addAggregateDelta(update.recursiveFileCount, afterFiles-beforeFiles, "directory recursive file count")
		if err != nil {
			return err
		}
		if currentBefore == nil {
			update.entryCount++
		} else if currentAfter == nil {
			update.entryCount--
		}
		if update.entryCount < 0 {
			return domain.NewError(domain.ErrorInvalid, "directory entry count underflows")
		}
		var beforeValues, afterValues []storageformat.DirectoryEntry
		if currentBefore != nil {
			beforeValues = []storageformat.DirectoryEntry{*currentBefore}
		}
		if currentAfter != nil {
			afterValues = []storageformat.DirectoryEntry{*currentAfter}
		}
		update.contentAccumulator, update.contentDigest, err = updateDirectoryContentIdentityAtCount(update.contentAccumulator, beforeValues, afterValues, update.entryCount)
		if err != nil {
			return err
		}
		change, exists := update.changes[name]
		if !exists {
			change.before = cloneDirectoryEntry(currentBefore)
		}
		change.after = cloneDirectoryEntry(currentAfter)
		if sameOptionalDirectoryEntry(change.before, change.after) {
			delete(update.changes, name)
		} else {
			update.changes[name] = change
		}
		key := storageformat.DirectoryRootKey(node.scope.UserID().String(), areaName(node.scope.Area()), node.directoryID).String()
		if len(update.changes) == 0 && update.entryCount == node.snapshot.manifest.EntryCount && update.recursiveBytes == node.snapshot.recursiveBytes && update.recursiveFileCount == node.snapshot.recursiveFileCount && update.contentAccumulator == node.snapshot.contentAccumulator && update.contentDigest == node.snapshot.contentDigest {
			delete(updates, key)
		} else {
			updates[key] = update
		}
		if index == 0 {
			break
		}
		child := trail[index].entry
		if child.Kind != domain.EntryDirectory || child.DirectoryID != node.directoryID || child.Name == "" {
			return domain.NewError(domain.ErrorInvalid, "directory aggregate ancestor is invalid")
		}
		oldChild := child
		oldChild.Size, oldChild.FileCount, oldChild.ContentDigest = priorBytes, priorFiles, priorDigest
		oldChild.LogicalVersion, err = directoryEntryVersion(oldChild)
		if err != nil {
			return err
		}
		newChild := oldChild
		newChild.Size, newChild.FileCount, newChild.ContentDigest = update.recursiveBytes, update.recursiveFileCount, update.contentDigest
		newChild.LogicalVersion, err = directoryEntryVersion(newChild)
		if err != nil {
			return err
		}
		name, currentBefore, currentAfter = child.Name, &oldChild, &newChild
	}
	return nil
}

func cloneDirectoryEntry(entry *storageformat.DirectoryEntry) *storageformat.DirectoryEntry {
	if entry == nil {
		return nil
	}
	copy := *entry
	return &copy
}

func sameOptionalDirectoryEntry(left, right *storageformat.DirectoryEntry) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func directoryEntryAggregates(entry *storageformat.DirectoryEntry) (int64, int64, error) {
	if entry == nil {
		return 0, 0, nil
	}
	if err := validateDirectoryIndexEntry(*entry); err != nil {
		return 0, 0, err
	}
	files := entry.FileCount
	if entry.Kind == domain.EntryFile {
		files = 1
	}
	return entry.Size, files, nil
}

func addAggregateDelta(value, delta int64, label string) (int64, error) {
	if delta > 0 && value > math.MaxInt64-delta || delta < 0 && value < -delta {
		return 0, domain.NewError(domain.ErrorInvalid, label+" overflows")
	}
	return value + delta, nil
}

func directoryEntryVersion(entry storageformat.DirectoryEntry) (string, error) {
	entry.LogicalVersion = ""
	body, err := storageformat.EncodeCanonical(entry)
	if err != nil {
		return "", err
	}
	return storageformat.Digest(body), nil
}

func resolveDirectoryDestination(requested domain.UserPath, conflict domain.ConflictMode, expected domain.Version, entries []storageformat.DirectoryEntry) (domain.UserPath, *storageformat.DirectoryEntry, error) {
	existing, found := findDirectoryEntry(entries, requested.Name())
	if !found {
		return requested, nil, nil
	}
	switch conflict {
	case domain.ConflictFail:
		return domain.UserPath{}, nil, domain.NewError(domain.ErrorConflict, "destination already exists")
	case domain.ConflictReplace:
		if expected == "" || expected != domain.Version(existing.LogicalVersion) {
			return domain.UserPath{}, nil, domain.NewError(domain.ErrorPreconditionFailed, "destination version does not match")
		}
		return requested, &existing, nil
	case domain.ConflictRename:
		path, err := availableDirectoryName(requested, entries)
		return path, nil, err
	default:
		return domain.UserPath{}, nil, domain.NewError(domain.ErrorInvalid, "invalid conflict mode")
	}
}

func (s *FileStore) resolveIndexedDirectoryDestination(ctx context.Context, scope domain.Scope, directoryID string, manifest storageformat.DirectoryManifest, requested domain.UserPath, conflict domain.ConflictMode, expected domain.Version) (domain.UserPath, *storageformat.DirectoryEntry, error) {
	existing, err := s.directoryIndexEntry(ctx, scope, directoryID, manifest, requested.Name())
	if errors.Is(err, domain.ErrNotFound) {
		return requested, nil, nil
	}
	if err != nil {
		return domain.UserPath{}, nil, err
	}
	switch conflict {
	case domain.ConflictFail:
		return domain.UserPath{}, nil, domain.NewError(domain.ErrorConflict, "destination already exists")
	case domain.ConflictReplace:
		if expected == "" || expected != domain.Version(existing.LogicalVersion) {
			return domain.UserPath{}, nil, domain.NewError(domain.ErrorPreconditionFailed, "destination version does not match")
		}
		return requested, &existing, nil
	case domain.ConflictRename:
		name := requested.Name()
		extensionIndex := strings.LastIndexByte(name, '.')
		base, extension := name, ""
		if extensionIndex > 0 {
			base, extension = name[:extensionIndex], name[extensionIndex:]
		}
		for index := 1; index <= 10_000; index++ {
			suffix := fmt.Sprintf(" (%d)", index)
			candidateBase := base
			for len(candidateBase)+len(suffix)+len(extension) > 255 && candidateBase != "" {
				_, size := utf8.DecodeLastRuneInString(candidateBase)
				candidateBase = candidateBase[:len(candidateBase)-size]
			}
			candidate, joinErr := requested.Parent().Join(candidateBase + suffix + extension)
			if joinErr != nil {
				return domain.UserPath{}, nil, joinErr
			}
			if _, lookupErr := s.directoryIndexEntry(ctx, scope, directoryID, manifest, candidate.Name()); errors.Is(lookupErr, domain.ErrNotFound) {
				return candidate, nil, nil
			} else if lookupErr != nil {
				return domain.UserPath{}, nil, lookupErr
			}
		}
		return domain.UserPath{}, nil, domain.NewError(domain.ErrorConflict, "unable to generate a conflict-free name")
	default:
		return domain.UserPath{}, nil, domain.NewError(domain.ErrorInvalid, "invalid conflict mode")
	}
}

func availableDirectoryName(path domain.UserPath, entries []storageformat.DirectoryEntry) (domain.UserPath, error) {
	names := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		names[entry.Name] = struct{}{}
	}
	name := path.Name()
	extensionIndex := strings.LastIndexByte(name, '.')
	base, extension := name, ""
	if extensionIndex > 0 {
		base, extension = name[:extensionIndex], name[extensionIndex:]
	}
	for index := 1; index <= 10_000; index++ {
		suffix := fmt.Sprintf(" (%d)", index)
		candidateBase := base
		for len(candidateBase)+len(suffix)+len(extension) > 255 && candidateBase != "" {
			_, size := utf8.DecodeLastRuneInString(candidateBase)
			candidateBase = candidateBase[:len(candidateBase)-size]
		}
		candidate, err := path.Parent().Join(candidateBase + suffix + extension)
		if err != nil {
			return domain.UserPath{}, err
		}
		if _, exists := names[candidate.Name()]; !exists {
			return candidate, nil
		}
	}
	return domain.UserPath{}, domain.NewError(domain.ErrorConflict, "unable to generate a conflict-free name")
}

func replaceDirectoryEntry(entries []storageformat.DirectoryEntry, existing *storageformat.DirectoryEntry, replacement storageformat.DirectoryEntry) []storageformat.DirectoryEntry {
	result := make([]storageformat.DirectoryEntry, 0, len(entries)+1)
	for _, entry := range entries {
		if existing != nil && entry.Name == existing.Name {
			continue
		}
		result = append(result, entry)
	}
	result = append(result, replacement)
	sort.Slice(result, func(i, j int) bool {
		if result[i].NameDigest == result[j].NameDigest {
			return result[i].Name < result[j].Name
		}
		return result[i].NameDigest < result[j].NameDigest
	})
	return result
}

func findDirectoryEntry(entries []storageformat.DirectoryEntry, name string) (storageformat.DirectoryEntry, bool) {
	digest := storageformat.NameDigest(name)
	index := sort.Search(len(entries), func(index int) bool {
		if entries[index].NameDigest == digest {
			return entries[index].Name >= name
		}
		return entries[index].NameDigest >= digest
	})
	if index < len(entries) && entries[index].NameDigest == digest && entries[index].Name == name {
		return entries[index], true
	}
	return storageformat.DirectoryEntry{}, false
}

func domainEntry(path domain.UserPath, entry storageformat.DirectoryEntry) domain.Entry {
	identity := previewContentIdentity(entry)
	return domain.Entry{
		Path: path, Name: entry.Name, Kind: entry.Kind, Size: entry.Size, FileCount: domainEntryFileCount(entry), MediaType: entry.MediaType,
		ModifiedAt: entry.ModifiedAt, Version: domain.Version(entry.LogicalVersion),
		ContentID: identity.ContentID, ContentVersion: identity.ContentVersion, ContentModifiedAt: identity.ContentModifiedAt,
	}
}

func domainEntryFileCount(entry storageformat.DirectoryEntry) int64 {
	if entry.Kind == domain.EntryFile {
		return 1
	}
	return entry.FileCount
}

func previewContentIdentity(entry storageformat.DirectoryEntry) domain.PreviewContentIdentity {
	if entry.Kind != domain.EntryFile {
		return domain.PreviewContentIdentity{}
	}
	contentID := storageformat.Digest([]byte("endlessfs-preview-content-id-v1\x00" + entry.BlobID))
	contentVersion := storageformat.Digest([]byte(fmt.Sprintf(
		"endlessfs-preview-content-version-v2\x00%s\x00%s\x00%s\x00%d\x00%s",
		entry.BlobID, entry.MD5, entry.CRC32C, entry.Size, entry.MediaType,
	)))
	return domain.PreviewContentIdentity{
		ContentID: domain.ContentID(contentID), ContentVersion: domain.ContentVersion(contentVersion), ContentModifiedAt: entry.ModifiedAt.UTC(),
	}
}

func validateFileRequest(ctx context.Context, scope domain.Scope) error {
	if err := ctx.Err(); err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "provider request canceled", err)
	}
	if !scope.Valid() {
		return domain.NewError(domain.ErrorUnauthorized, "invalid storage scope")
	}
	return nil
}

func areaName(area domain.Area) string {
	if area == domain.AreaTrash {
		return "trash"
	}
	return "live"
}

func normalizeFilePageSize(size int) (int, error) {
	if size == 0 {
		return 200, nil
	}
	if size < 1 || size > 1000 {
		return 0, domain.NewError(domain.ErrorInvalid, "page size must be between 1 and 1000")
	}
	return size, nil
}

func validSort(field domain.SortField) bool {
	return field == domain.SortName || field == domain.SortModified || field == domain.SortSize || field == domain.SortKind
}

func sortDomainEntries(entries []domain.Entry, field domain.SortField, descending bool) {
	sort.Slice(entries, func(i, j int) bool {
		comparison := 0
		switch field {
		case domain.SortModified:
			comparison = entries[i].ModifiedAt.Compare(entries[j].ModifiedAt)
		case domain.SortSize:
			if entries[i].Size < entries[j].Size {
				comparison = -1
			} else if entries[i].Size > entries[j].Size {
				comparison = 1
			}
		case domain.SortKind:
			comparison = strings.Compare(string(entries[i].Kind), string(entries[j].Kind))
		default:
			comparison = strings.Compare(entries[i].Name, entries[j].Name)
		}
		if comparison == 0 {
			comparison = strings.Compare(entries[i].Path.String(), entries[j].Path.String())
		}
		if descending {
			return comparison > 0
		}
		return comparison < 0
	})
}

func encodeListCursor(cursor listCursor) (string, error) {
	body, err := storageformat.EncodeCanonical(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(body), nil
}

func decodeListCursor(value string) (listCursor, error) {
	body, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return listCursor{}, err
	}
	var cursor listCursor
	if err := decodeCanonicalValue(body, &cursor); err != nil {
		return listCursor{}, err
	}
	if cursor.SchemaVersion != 2 && cursor.SchemaVersion != 3 && cursor.SchemaVersion != 4 {
		return listCursor{}, domain.NewError(domain.ErrorInvalid, "invalid list cursor schema")
	}
	return cursor, nil
}

func (s *FileStore) encodeListCursor(cursor listCursor) (string, error) {
	body, err := storageformat.EncodeCanonical(cursor)
	if err != nil {
		return "", err
	}
	random, err := s.engine.ids.BearerToken()
	if err != nil {
		return "", err
	}
	nonceMaterial, err := base64.RawURLEncoding.DecodeString(random)
	if err != nil || len(nonceMaterial) < s.engine.cursorAEAD.NonceSize() {
		return "", domain.NewError(domain.ErrorInternal, "secure file cursor randomness unavailable")
	}
	nonce := nonceMaterial[:s.engine.cursorAEAD.NonceSize()]
	sealed := s.engine.cursorAEAD.Seal(append([]byte(nil), nonce...), nonce, body, []byte("endlessfs-file-list-cursor-v3"))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (s *FileStore) decodeListCursor(value string) (listCursor, error) {
	sealed, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(sealed) <= s.engine.cursorAEAD.NonceSize() {
		return listCursor{}, domain.NewError(domain.ErrorInvalid, "invalid file list cursor")
	}
	nonceSize := s.engine.cursorAEAD.NonceSize()
	body, err := s.engine.cursorAEAD.Open(nil, sealed[:nonceSize], sealed[nonceSize:], []byte("endlessfs-file-list-cursor-v3"))
	if err != nil {
		return listCursor{}, domain.NewError(domain.ErrorInvalid, "invalid file list cursor")
	}
	return decodeListCursor(base64.RawURLEncoding.EncodeToString(body))
}

func decodeCanonicalValue(body []byte, destination any) error {
	if err := state.DecodeJSONWithLimit(body, destination, storageformat.MaxCanonicalBytes); err != nil {
		return err
	}
	canonical, err := storageformat.EncodeCanonical(destination)
	if err != nil || !bytes.Equal(canonical, body) {
		return domain.NewError(domain.ErrorInvalid, "non-canonical cursor")
	}
	return nil
}
