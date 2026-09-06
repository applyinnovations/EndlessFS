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
	"time"

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
	maxMutationRecoveries   = 8
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
	recursiveBytes     int64
	recursiveFileCount int64
	contentAccumulator string
	contentDigest      string
	pending            bool
	transitionState    storageformat.FileOperationState
	transitionFence    uint64
	transitionExpires  time.Time
}

type preparedDirectory struct {
	manifestID         string
	recursiveBytes     int64
	recursiveFileCount int64
	contentAccumulator string
	contentDigest      string
	contentSketch      []string
	rootBody           []byte
	prerequisites      []storageformat.MutationObject
}

func directoryEntryStorageScope(inherited domain.Scope, entry storageformat.DirectoryEntry) (domain.Scope, error) {
	if !inherited.Valid() || entry.Kind != domain.EntryDirectory {
		return domain.Scope{}, domain.NewError(domain.ErrorInvalid, "invalid directory storage scope")
	}
	if entry.StorageArea == "" {
		return inherited, nil
	}
	return storedOperationScope(inherited.UserID(), entry.StorageArea)
}

func (s *FileStore) readDirectoryEntryMetadata(ctx context.Context, storageScope domain.Scope, entry storageformat.DirectoryEntry) (directorySnapshot, error) {
	if entry.Kind != domain.EntryDirectory || entry.DirectoryID == "" || !storageScope.Valid() {
		return directorySnapshot{}, domain.NewError(domain.ErrorInvalid, "invalid pinned directory entry")
	}
	if entry.ManifestID == "" {
		return s.readDirectoryMetadata(ctx, storageScope, entry.DirectoryID, false)
	}
	manifest, err := s.readDirectoryManifest(ctx, storageScope, entry.DirectoryID, entry.ManifestID)
	if err != nil {
		return directorySnapshot{}, err
	}
	if manifest.RecursiveBytes != entry.Size || manifest.RecursiveFileCount != entry.FileCount || manifest.ContentDigest != entry.ContentDigest {
		return directorySnapshot{}, domain.NewError(domain.ErrorInvalid, "pinned directory snapshot aggregate mismatch")
	}
	return directorySnapshot{
		exists: true, envelope: storageformat.Envelope{Revision: 1, LogicalVersion: entry.LogicalVersion},
		root:       storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: entry.DirectoryID, ManifestID: entry.ManifestID, RecursiveBytes: entry.Size, RecursiveFileCount: entry.FileCount, ContentDigest: entry.ContentDigest, ContentAccumulator: manifest.ContentAccumulator},
		manifestID: entry.ManifestID, manifest: manifest, recursiveBytes: entry.Size, recursiveFileCount: entry.FileCount,
		contentAccumulator: manifest.ContentAccumulator, contentDigest: entry.ContentDigest,
	}, nil
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
	var transitionExpires time.Time
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
		transitionExpires = operation.ExpiresAt
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
		return directorySnapshot{object: object, exists: true, envelope: envelope, root: root, recursiveBytes: recursiveBytes, recursiveFileCount: recursiveFileCount, contentAccumulator: contentAccumulator, contentDigest: contentDigest, pending: pending, transitionState: transitionState, transitionFence: transitionFence, transitionExpires: transitionExpires}, nil
	}
	manifest, err := s.readDirectoryManifest(ctx, scope, directoryID, manifestID)
	if err != nil {
		return directorySnapshot{}, err
	}
	if manifest.RecursiveBytes != recursiveBytes || manifest.RecursiveFileCount != recursiveFileCount || manifest.ContentAccumulator != contentAccumulator || manifest.ContentDigest != contentDigest {
		return directorySnapshot{}, domain.NewError(domain.ErrorInvalid, "directory root and manifest recursive aggregate mismatch")
	}
	return directorySnapshot{object: object, exists: true, envelope: envelope, root: root, manifestID: manifestID, manifest: manifest, recursiveBytes: recursiveBytes, recursiveFileCount: recursiveFileCount, contentAccumulator: contentAccumulator, contentDigest: contentDigest, pending: pending, transitionState: transitionState, transitionFence: transitionFence, transitionExpires: transitionExpires}, nil
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
	contentIndexErr := validateDirectoryManifestContent(manifest)
	if (manifest.SchemaVersion != 2 && manifest.SchemaVersion != 3) || manifest.DirectoryID != directoryID || manifest.ManifestID != manifestID || manifest.EntryCount < 0 || manifest.RecursiveBytes < 0 || manifest.RecursiveFileCount < 0 || accumulatorErr != nil || digestErr != nil || contentIndexErr != nil || manifest.ContentDigest == "" || expectedDigest != manifest.ContentDigest || len(manifest.PageIDs) != 0 || manifest.EntryCount == 0 && (manifest.IndexRootID != "" || manifest.IndexRootDigest != "") || manifest.EntryCount > 0 && (manifest.IndexRootID == "" || manifest.IndexRootDigest == "") || validateDirectorySortIndexRoots(manifest.SortIndexes, manifest.EntryCount) != nil {
		return storageformat.DirectoryManifest{}, domain.NewError(domain.ErrorInvalid, "invalid directory manifest")
	}
	return manifest, nil
}

func validateDirectoryManifestContent(manifest storageformat.DirectoryManifest) error {
	if manifest.SchemaVersion == 2 {
		if manifest.ContentBase != nil || len(manifest.ContentDeltas) != 0 {
			return domain.NewError(domain.ErrorInvalid, "materialized directory manifest has lazy content sources")
		}
		_, err := directoryContentIndexManifestRoot(manifest)
		return err
	}
	if manifest.SchemaVersion != 3 {
		return domain.NewError(domain.ErrorInvalid, "unsupported directory manifest content representation")
	}
	if manifest.ContentIndexRootID != "" || manifest.ContentIndexRootDigest != "" || len(manifest.ContentSketch) != 0 && validateDirectoryContentSketch(manifest.ContentSketch) != nil {
		return domain.NewError(domain.ErrorInvalid, "lazy directory manifest has a materialized content root")
	}
	if manifest.RecursiveFileCount == 0 {
		if manifest.ContentBase != nil || len(manifest.ContentDeltas) != 0 {
			return domain.NewError(domain.ErrorInvalid, "empty lazy directory has content sources")
		}
		return nil
	}
	if manifest.ContentBase == nil && len(manifest.ContentDeltas) == 0 {
		return domain.NewError(domain.ErrorInvalid, "non-empty lazy directory has no content source")
	}
	if manifest.ContentBase != nil {
		if (manifest.ContentBase.Area != "live" && manifest.ContentBase.Area != "trash") || manifest.ContentBase.DirectoryID == "" || manifest.ContentBase.ManifestID == "" {
			return domain.NewError(domain.ErrorInvalid, "invalid lazy directory content base")
		}
	}
	if len(manifest.ContentDeltas) > 256 {
		return domain.NewError(domain.ErrorInvalid, "too many lazy directory content deltas")
	}
	for _, delta := range manifest.ContentDeltas {
		direct := delta.Entry != nil
		subtree := delta.DirectoryID != "" || delta.ManifestID != "" || delta.Area != "" || delta.Prefix != ""
		if direct == subtree {
			return domain.NewError(domain.ErrorInvalid, "invalid lazy directory content delta source")
		}
		if direct {
			if _, err := directoryContentIndexKey(*delta.Entry); err != nil {
				return err
			}
			continue
		}
		prefix, err := domain.ParseUserPath(delta.Prefix)
		if err != nil || prefix.IsRoot() || (delta.Area != "live" && delta.Area != "trash") || delta.DirectoryID == "" || delta.ManifestID == "" {
			return domain.NewError(domain.ErrorInvalid, "invalid lazy subtree content delta")
		}
	}
	return nil
}

func (s *FileStore) readManifestPageEntries(ctx context.Context, scope domain.Scope, directoryID string, manifest storageformat.DirectoryManifest) ([]storageformat.DirectoryEntry, error) {
	if manifest.SchemaVersion == 2 || manifest.SchemaVersion == 3 {
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
			return nil, domain.WrapError(domain.ErrorInvalid, "cannot decode directory page "+pageKey.String(), err)
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

func validateDirectoryEntries(entries []storageformat.DirectoryEntry) error {
	previousDigest, previousName := "", ""
	for _, entry := range entries {
		if entry.Name == "" || storageformat.NameDigest(entry.Name) != entry.NameDigest || entry.LogicalVersion == "" || entry.Size < 0 || entry.ModifiedAt.IsZero() {
			return domain.NewError(domain.ErrorInvalid, "invalid directory entry")
		}
		if entry.Kind == domain.EntryDirectory {
			if entry.DirectoryID == "" || entry.BlobID != "" || entry.MediaType != "" || entry.FileCount < 0 || entry.StorageArea != "" && entry.StorageArea != "live" && entry.StorageArea != "trash" {
				return domain.NewError(domain.ErrorInvalid, "invalid directory entry target")
			}
		} else if entry.Kind == domain.EntryFile {
			if entry.BlobID == "" || entry.DirectoryID != "" || entry.ManifestID != "" || entry.StorageArea != "" || entry.MediaType == "" || entry.FileCount != 0 {
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
			accumulator[position] = byte(value) // #nosec G115 -- borrow normalization bounds value to [0, 255].
			continue
		}
		value := int(accumulator[position]) + int(contribution[index]) + carry
		accumulator[position] = byte(value) // #nosec G115 -- truncation is the intended modulo-256 accumulator arithmetic.
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

func updateDirectoryContentIdentityAtCount(encoded string, before, after []storageformat.DirectoryEntry, finalCount int) (string, string, error) {
	accumulator, err := decodeDirectoryContentAccumulator(encoded)
	if err != nil {
		return "", "", err
	}
	for _, entry := range before {
		contribution, contributionErr := directoryContentContribution(entry)
		if contributionErr != nil {
			return "", "", contributionErr
		}
		applyDirectoryContentContribution(&accumulator, contribution, true)
	}
	for _, entry := range after {
		contribution, contributionErr := directoryContentContribution(entry)
		if contributionErr != nil {
			return "", "", contributionErr
		}
		applyDirectoryContentContribution(&accumulator, contribution, false)
	}
	digest, err := directoryContentAccumulatorDigest(accumulator, finalCount)
	return encodeDirectoryContentAccumulator(accumulator), digest, err
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

// storedOperationScope decodes the provider-independent area recorded by the
// schema-007 compatibility graph. It remains only for migration, garbage
// collection, and validation of predecessor objects.
func storedOperationScope(userID domain.UserID, area string) (domain.Scope, error) {
	value := domain.AreaLive
	if area == "trash" {
		value = domain.AreaTrash
	} else if area != "live" {
		return domain.Scope{}, domain.NewError(domain.ErrorInvalid, "invalid stored operation area")
	}
	return domain.NewScope(userID, value)
}

func normalizeFilePageSize(size int) (int, error) {
	if size == 0 {
		return 200, nil
	}
	if size < 1 || size > 10_000 {
		return 0, domain.NewError(domain.ErrorInvalid, "page size must be between 1 and 10000")
	}
	return size, nil
}

func validSort(field domain.SortField) bool {
	return field == domain.SortName || field == domain.SortModified || field == domain.SortSize || field == domain.SortKind
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
