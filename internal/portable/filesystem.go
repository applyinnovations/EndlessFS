package portable

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
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
	object   objectstore.Object
	exists   bool
	envelope storageformat.Envelope
	root     storageformat.DirectoryRoot
	entries  []storageformat.DirectoryEntry
}

type preparedDirectory struct {
	rootBody      []byte
	prerequisites []storageformat.MutationObject
}

type listCursor struct {
	SchemaVersion int              `json:"schemaVersion"`
	UserID        string           `json:"userID"`
	Area          string           `json:"area"`
	DirectoryPath string           `json:"directoryPath"`
	DirectoryID   string           `json:"directoryID"`
	ManifestID    string           `json:"manifestID"`
	PageSize      int              `json:"pageSize"`
	Sort          domain.SortField `json:"sort"`
	Descending    bool             `json:"descending"`
	Index         int              `json:"index"`
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
	directoryID, snapshot, err := s.resolveDirectory(ctx, scope, request.Directory)
	if err != nil {
		return domain.ListPage{}, err
	}
	start := 0
	manifestID := snapshot.root.ManifestID
	entries := snapshot.entries
	if request.Cursor != "" {
		cursor, decodeErr := decodeListCursor(request.Cursor)
		if decodeErr != nil || cursor.UserID != scope.UserID().String() || cursor.Area != areaName(scope.Area()) || cursor.DirectoryPath != request.Directory.String() || cursor.DirectoryID != directoryID || cursor.PageSize != pageSize || cursor.Sort != request.Sort || cursor.Descending != request.Descending || cursor.Index < 1 || cursor.ManifestID == "" {
			return domain.ListPage{}, domain.NewError(domain.ErrorInvalid, "invalid or out-of-scope list cursor")
		}
		entries, err = s.readManifestEntries(ctx, scope, directoryID, cursor.ManifestID)
		if err != nil {
			return domain.ListPage{}, err
		}
		manifestID = cursor.ManifestID
		start = cursor.Index
		if start > len(entries) {
			return domain.ListPage{}, domain.NewError(domain.ErrorInvalid, "invalid list cursor offset")
		}
	}
	result := make([]domain.Entry, 0, len(entries))
	for _, entry := range entries {
		path, joinErr := request.Directory.Join(entry.Name)
		if joinErr != nil {
			return domain.ListPage{}, domain.NewError(domain.ErrorInvalid, "stored directory name is invalid")
		}
		result = append(result, domainEntry(path, entry))
	}
	sortDomainEntries(result, request.Sort, request.Descending)
	if start == len(result) {
		return domain.ListPage{}, nil
	}
	end := min(start+pageSize, len(result))
	page := domain.ListPage{Entries: append([]domain.Entry(nil), result[start:end]...)}
	if end < len(result) {
		page.NextCursor, err = encodeListCursor(listCursor{
			SchemaVersion: 1, UserID: scope.UserID().String(), Area: areaName(scope.Area()),
			DirectoryPath: request.Directory.String(), DirectoryID: directoryID, ManifestID: manifestID,
			PageSize: pageSize, Sort: request.Sort, Descending: request.Descending, Index: end,
		})
		if err != nil {
			return domain.ListPage{}, err
		}
	}
	return page, nil
}

func (s *FileStore) Stat(ctx context.Context, scope domain.Scope, path domain.UserPath) (domain.Entry, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return domain.Entry{}, err
	}
	if !path.Valid() {
		return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "path is required")
	}
	if path.IsRoot() {
		return domain.Entry{Path: path, Kind: domain.EntryDirectory, ModifiedAt: time.Unix(0, 0).UTC(), Version: "root"}, nil
	}
	entry, err := s.resolveEntry(ctx, scope, path)
	if err != nil {
		return domain.Entry{}, err
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
		parentID, parent, err := s.resolveDirectory(ctx, scope, request.Path.Parent())
		if err != nil {
			return domain.Entry{}, err
		}
		path, existing, err := resolveDirectoryDestination(request.Path, conflict, request.ExpectedVersion, parent.entries)
		if err != nil {
			return domain.Entry{}, err
		}
		childID, err := s.engine.ids.OpaqueID()
		if err != nil {
			return domain.Entry{}, err
		}
		entry := storageformat.DirectoryEntry{
			Name: path.Name(), NameDigest: storageformat.NameDigest(path.Name()), Kind: domain.EntryDirectory,
			DirectoryID: childID, ModifiedAt: s.engine.clock.Now().UTC(),
		}
		entry.LogicalVersion, err = directoryEntryVersion(entry)
		if err != nil {
			return domain.Entry{}, err
		}
		updated := replaceDirectoryEntry(parent.entries, existing, entry)
		parentRevision := uint64(1)
		if parent.exists {
			parentRevision = parent.envelope.Revision + 1
		}
		preparedParent, err := s.prepareDirectory(ctx, scope, parentID, updated, parentRevision)
		if err != nil {
			return domain.Entry{}, err
		}
		preparedChild, err := s.prepareDirectory(ctx, scope, childID, nil, 1)
		if err != nil {
			return domain.Entry{}, err
		}
		childRootKey := storageformat.DirectoryRootKey(scope.UserID().String(), areaName(scope.Area()), childID)
		prerequisites := append(preparedParent.prerequisites, preparedChild.prerequisites...)
		prerequisites = append(prerequisites, storageformat.MutationObject{Key: childRootKey.String(), Body: preparedChild.rootBody})
		sort.Slice(prerequisites, func(i, j int) bool { return prerequisites[i].Key < prerequisites[j].Key })
		parentKey := storageformat.DirectoryRootKey(scope.UserID().String(), areaName(scope.Area()), parentID)
		action := storageformat.MutationCreate
		expected := ""
		condition := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
		if parent.exists {
			action = storageformat.MutationCAS
			expected = parent.envelope.LogicalVersion
			condition = objectstore.PutCondition{Mode: objectstore.PutMatch, Version: parent.object.Version}
		}
		intent := storageformat.MutationIntent{Action: action, TargetKey: parentKey.String(), ExpectedLogicalVersion: expected, TargetBody: preparedParent.rootBody, Prerequisites: prerequisites}
		err = s.engine.withAdmission(ctx, intent, func() error {
			if err := s.engine.ensureMutationPrerequisites(ctx, prerequisites); err != nil {
				return err
			}
			_, putErr := s.engine.backend.Put(ctx, parentKey, preparedParent.rootBody, condition)
			return putErr
		})
		if err == nil {
			return domainEntry(path, entry), nil
		}
		if !errors.Is(err, domain.ErrPreconditionFailed) && !errors.Is(err, domain.ErrConflict) {
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
		snapshot, err := s.readDirectory(ctx, scope, directoryID, directoryID == storageformat.RootDirectoryID)
		if err != nil {
			return storageformat.DirectoryEntry{}, err
		}
		entry, found := findDirectoryEntry(snapshot.entries, segment)
		if !found {
			return storageformat.DirectoryEntry{}, domain.NewError(domain.ErrorNotFound, "entry not found")
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
	return entry.DirectoryID, snapshot, err
}

func (s *FileStore) readDirectory(ctx context.Context, scope domain.Scope, directoryID string, allowVirtualRoot bool) (directorySnapshot, error) {
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
	if root.SchemaVersion != 1 || root.DirectoryID != directoryID || root.ManifestID == "" {
		return directorySnapshot{}, domain.NewError(domain.ErrorInvalid, "invalid directory root")
	}
	entries, err := s.readManifestEntries(ctx, scope, directoryID, root.ManifestID)
	if err != nil {
		return directorySnapshot{}, err
	}
	return directorySnapshot{object: object, exists: true, envelope: envelope, root: root, entries: entries}, nil
}

func (s *FileStore) readManifestEntries(ctx context.Context, scope domain.Scope, directoryID, manifestID string) ([]storageformat.DirectoryEntry, error) {
	key := storageformat.DirectoryManifestKey(scope.UserID().String(), areaName(scope.Area()), directoryID, manifestID)
	object, err := s.engine.backend.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	var envelope storageformat.Envelope
	var manifest storageformat.DirectoryManifest
	if err := storageformat.DecodeEnvelope(object.Body, key, directoryManifestSchema, &envelope, &manifest); err != nil {
		return nil, err
	}
	if manifest.SchemaVersion != 1 || manifest.DirectoryID != directoryID || manifest.ManifestID != manifestID || manifest.EntryCount < 0 || len(manifest.PageIDs) == 0 {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid directory manifest")
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
	manifestID, err := s.engine.ids.OpaqueID()
	if err != nil {
		return preparedDirectory{}, err
	}
	pages := make([]storageformat.MutationObject, 0, max(1, (len(entries)+maxEntriesPerPage-1)/maxEntriesPerPage))
	pageIDs := make([]string, 0, cap(pages))
	for start := 0; start < max(1, len(entries)); start += maxEntriesPerPage {
		end := min(start+maxEntriesPerPage, len(entries))
		pageEntries := entries[start:end]
		pageID, idErr := s.engine.ids.OpaqueID()
		if idErr != nil {
			return preparedDirectory{}, idErr
		}
		pageKey := storageformat.DirectoryPageKey(scope.UserID().String(), areaName(scope.Area()), directoryID, pageID)
		body, encodeErr := storageformat.EncodeEnvelope(directoryPageSchema, pageKey, 1, storageformat.DirectoryPage{SchemaVersion: 1, DirectoryID: directoryID, PageID: pageID, Entries: append([]storageformat.DirectoryEntry(nil), pageEntries...)})
		if encodeErr != nil {
			return preparedDirectory{}, encodeErr
		}
		pages = append(pages, storageformat.MutationObject{Key: pageKey.String(), Body: body})
		pageIDs = append(pageIDs, pageID)
		if len(entries) == 0 {
			break
		}
	}
	manifestKey := storageformat.DirectoryManifestKey(scope.UserID().String(), areaName(scope.Area()), directoryID, manifestID)
	manifestBody, err := storageformat.EncodeEnvelope(directoryManifestSchema, manifestKey, 1, storageformat.DirectoryManifest{
		SchemaVersion: 1, DirectoryID: directoryID, ManifestID: manifestID, PageIDs: pageIDs,
		EntryCount: len(entries), CreatedAt: s.engine.clock.Now().UTC(),
	})
	if err != nil {
		return preparedDirectory{}, err
	}
	prerequisites := append(pages, storageformat.MutationObject{Key: manifestKey.String(), Body: manifestBody})
	sort.Slice(prerequisites, func(i, j int) bool { return prerequisites[i].Key < prerequisites[j].Key })
	rootKey := storageformat.DirectoryRootKey(scope.UserID().String(), areaName(scope.Area()), directoryID)
	rootBody, err := storageformat.EncodeEnvelope(directoryRootSchema, rootKey, revision, storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: directoryID, ManifestID: manifestID})
	if err != nil {
		return preparedDirectory{}, err
	}
	return preparedDirectory{rootBody: rootBody, prerequisites: prerequisites}, nil
}

func validateDirectoryEntries(entries []storageformat.DirectoryEntry) error {
	previousDigest, previousName := "", ""
	for _, entry := range entries {
		if entry.Name == "" || storageformat.NameDigest(entry.Name) != entry.NameDigest || entry.LogicalVersion == "" || entry.Size < 0 || entry.ModifiedAt.IsZero() {
			return domain.NewError(domain.ErrorInvalid, "invalid directory entry")
		}
		if entry.Kind == domain.EntryDirectory {
			if entry.DirectoryID == "" || entry.BlobID != "" || entry.Size != 0 || entry.MediaType != "" {
				return domain.NewError(domain.ErrorInvalid, "invalid directory entry target")
			}
		} else if entry.Kind == domain.EntryFile {
			if entry.BlobID == "" || entry.DirectoryID != "" || entry.MediaType == "" {
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
	return domain.Entry{Path: path, Name: entry.Name, Kind: entry.Kind, Size: entry.Size, MediaType: entry.MediaType, ModifiedAt: entry.ModifiedAt, Version: domain.Version(entry.LogicalVersion)}
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
	if cursor.SchemaVersion != 1 {
		return listCursor{}, domain.NewError(domain.ErrorInvalid, "invalid list cursor schema")
	}
	return cursor, nil
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
