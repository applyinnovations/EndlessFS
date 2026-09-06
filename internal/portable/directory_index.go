package portable

import (
	"context"
	"math"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const (
	directoryIndexNodeSchema = "directory-index-node-v1"
	maxDirectoryIndexItems   = 64
)

func validateDirectoryIndexEntry(entry storageformat.DirectoryEntry) error {
	if entry.Name == "" || storageformat.NameDigest(entry.Name) != entry.NameDigest || entry.LogicalVersion == "" || entry.Size < 0 || entry.ModifiedAt.IsZero() {
		return domain.NewError(domain.ErrorInvalid, "invalid directory index entry")
	}
	switch entry.Kind {
	case domain.EntryDirectory:
		if entry.DirectoryID == "" || entry.BlobID != "" || entry.MediaType != "" || entry.FileCount < 0 || entry.ContentDigest == "" || entry.MD5 != "" || entry.CRC32C != "" || entry.SHA256 != "" || entry.StorageArea != "" && entry.StorageArea != "live" && entry.StorageArea != "trash" {
			return domain.NewError(domain.ErrorInvalid, "invalid directory index directory entry")
		}
	case domain.EntryFile:
		if entry.BlobID == "" || entry.DirectoryID != "" || entry.ManifestID != "" || entry.StorageArea != "" || entry.MediaType == "" || entry.FileCount != 0 || entry.ContentDigest != "" || entry.SHA256 != "" || !(objectstore.ContentFingerprint{MD5: entry.MD5, CRC32C: entry.CRC32C}).Complete() {
			return domain.NewError(domain.ErrorInvalid, "invalid directory index file entry")
		}
	default:
		return domain.NewError(domain.ErrorInvalid, "invalid directory index entry kind")
	}
	version, err := directoryEntryVersion(entry)
	if err != nil || version != entry.LogicalVersion {
		return domain.NewError(domain.ErrorInvalid, "directory index entry logical version mismatch")
	}
	return nil
}

func validateDirectoryIndexChild(child storageformat.DirectoryIndexChild) error {
	if child.NodeID == "" || child.NodeDigest == "" || child.FirstName == "" || child.LastName < child.FirstName || child.EntryCount == 0 || child.RecursiveBytes < 0 || child.RecursiveFileCount < 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid directory index child")
	}
	return nil
}

func validateDirectoryIndexNode(directoryID string, node storageformat.DirectoryIndexNode) error {
	if node.SchemaVersion != 1 || node.DirectoryID != directoryID || node.NodeID == "" || node.Leaf == (len(node.Entries) == 0) || node.Leaf && len(node.Children) != 0 || !node.Leaf && len(node.Entries) != 0 || len(node.Entries) > maxDirectoryIndexItems || len(node.Children) > maxDirectoryIndexItems || !node.Leaf && len(node.Children) == 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid directory index node")
	}
	previous := ""
	if node.Leaf {
		for _, entry := range node.Entries {
			if err := validateDirectoryIndexEntry(entry); err != nil || entry.Name <= previous {
				return domain.NewError(domain.ErrorInvalid, "directory index leaf is not uniquely name-sorted")
			}
			previous = entry.Name
		}
		return nil
	}
	for _, child := range node.Children {
		if err := validateDirectoryIndexChild(child); err != nil || child.FirstName <= previous {
			return domain.NewError(domain.ErrorInvalid, "directory index branch is not uniquely ordered")
		}
		previous = child.LastName
	}
	return nil
}

func directoryIndexNodeChild(node storageformat.DirectoryIndexNode, digest string) (storageformat.DirectoryIndexChild, error) {
	child := storageformat.DirectoryIndexChild{NodeID: node.NodeID, NodeDigest: digest}
	if node.Leaf {
		if len(node.Entries) == 0 {
			return storageformat.DirectoryIndexChild{}, domain.NewError(domain.ErrorInvalid, "empty directory index leaf")
		}
		child.FirstName, child.LastName, child.EntryCount = node.Entries[0].Name, node.Entries[len(node.Entries)-1].Name, uint64(len(node.Entries))
		bytes, err := recursiveByteSize(node.Entries)
		if err != nil {
			return storageformat.DirectoryIndexChild{}, err
		}
		files, err := recursiveFileCount(node.Entries)
		child.RecursiveBytes, child.RecursiveFileCount = bytes, files
		return child, err
	}
	if len(node.Children) == 0 {
		return storageformat.DirectoryIndexChild{}, domain.NewError(domain.ErrorInvalid, "empty directory index branch")
	}
	child.FirstName, child.LastName = node.Children[0].FirstName, node.Children[len(node.Children)-1].LastName
	for _, nested := range node.Children {
		if nested.EntryCount > ^uint64(0)-child.EntryCount || nested.RecursiveBytes > math.MaxInt64-child.RecursiveBytes || nested.RecursiveFileCount > math.MaxInt64-child.RecursiveFileCount {
			return storageformat.DirectoryIndexChild{}, domain.NewError(domain.ErrorInvalid, "directory index aggregate overflows")
		}
		child.EntryCount += nested.EntryCount
		child.RecursiveBytes += nested.RecursiveBytes
		child.RecursiveFileCount += nested.RecursiveFileCount
	}
	return child, nil
}

func (s *FileStore) makeDirectoryIndexNode(scope domain.Scope, directoryID string, leaf bool, entries []storageformat.DirectoryEntry, children []storageformat.DirectoryIndexChild) (storageformat.DirectoryIndexChild, storageformat.MutationObject, error) {
	identity, err := storageformat.EncodeCanonical(struct {
		DirectoryID string                              `json:"directoryID"`
		Leaf        bool                                `json:"leaf"`
		Entries     []storageformat.DirectoryEntry      `json:"entries,omitempty"`
		Children    []storageformat.DirectoryIndexChild `json:"children,omitempty"`
	}{directoryID, leaf, entries, children})
	if err != nil {
		return storageformat.DirectoryIndexChild{}, storageformat.MutationObject{}, err
	}
	nodeID := storageformat.Digest(append([]byte("endlessfs-directory-index-node-v1\x00"), identity...))
	node := storageformat.DirectoryIndexNode{SchemaVersion: 1, DirectoryID: directoryID, NodeID: nodeID, Leaf: leaf, Entries: entries, Children: children}
	if err := validateDirectoryIndexNode(directoryID, node); err != nil {
		return storageformat.DirectoryIndexChild{}, storageformat.MutationObject{}, err
	}
	key := storageformat.DirectoryIndexNodeKey(scope.UserID().String(), areaName(scope.Area()), directoryID, nodeID)
	body, err := storageformat.EncodeEnvelope(directoryIndexNodeSchema, key, 1, node)
	if err != nil {
		return storageformat.DirectoryIndexChild{}, storageformat.MutationObject{}, err
	}
	child, err := directoryIndexNodeChild(node, storageformat.Digest(body))
	return child, storageformat.MutationObject{Key: key.String(), Body: body}, err
}

func (s *FileStore) buildDirectoryIndex(scope domain.Scope, directoryID string, source []storageformat.DirectoryEntry) (storageformat.DirectoryIndexChild, []storageformat.MutationObject, error) {
	entries := append([]storageformat.DirectoryEntry(nil), source...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	if len(entries) == 0 {
		return storageformat.DirectoryIndexChild{}, nil, nil
	}
	refs := make([]storageformat.DirectoryIndexChild, 0, (len(entries)+maxDirectoryIndexItems-1)/maxDirectoryIndexItems)
	objects := make([]storageformat.MutationObject, 0, cap(refs))
	for start := 0; start < len(entries); start += maxDirectoryIndexItems {
		end := min(start+maxDirectoryIndexItems, len(entries))
		ref, object, err := s.makeDirectoryIndexNode(scope, directoryID, true, append([]storageformat.DirectoryEntry(nil), entries[start:end]...), nil)
		if err != nil {
			return storageformat.DirectoryIndexChild{}, nil, err
		}
		refs, objects = append(refs, ref), append(objects, object)
	}
	for len(refs) > 1 {
		next := make([]storageformat.DirectoryIndexChild, 0, (len(refs)+maxDirectoryIndexItems-1)/maxDirectoryIndexItems)
		for start := 0; start < len(refs); start += maxDirectoryIndexItems {
			end := min(start+maxDirectoryIndexItems, len(refs))
			ref, object, err := s.makeDirectoryIndexNode(scope, directoryID, false, nil, append([]storageformat.DirectoryIndexChild(nil), refs[start:end]...))
			if err != nil {
				return storageformat.DirectoryIndexChild{}, nil, err
			}
			next, objects = append(next, ref), append(objects, object)
		}
		refs = next
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	return refs[0], objects, nil
}

func (s *FileStore) readDirectoryIndexNode(ctx context.Context, scope domain.Scope, directoryID string, reference storageformat.DirectoryIndexChild) (storageformat.DirectoryIndexNode, error) {
	if err := validateDirectoryIndexChild(reference); err != nil {
		return storageformat.DirectoryIndexNode{}, err
	}
	key := storageformat.DirectoryIndexNodeKey(scope.UserID().String(), areaName(scope.Area()), directoryID, reference.NodeID)
	object, err := s.engine.backend.Get(ctx, key)
	if err != nil {
		return storageformat.DirectoryIndexNode{}, err
	}
	if storageformat.Digest(object.Body) != reference.NodeDigest {
		return storageformat.DirectoryIndexNode{}, domain.NewError(domain.ErrorInvalid, "directory index node digest mismatch")
	}
	var envelope storageformat.Envelope
	var node storageformat.DirectoryIndexNode
	if err := storageformat.DecodeEnvelope(object.Body, key, directoryIndexNodeSchema, &envelope, &node); err != nil {
		return storageformat.DirectoryIndexNode{}, err
	}
	if err := validateDirectoryIndexNode(directoryID, node); err != nil {
		return storageformat.DirectoryIndexNode{}, err
	}
	derived, err := directoryIndexNodeChild(node, reference.NodeDigest)
	if err != nil || derived != reference {
		return storageformat.DirectoryIndexNode{}, domain.NewError(domain.ErrorInvalid, "directory index child metadata mismatch")
	}
	return node, nil
}

func (s *FileStore) directoryIndexRoot(ctx context.Context, scope domain.Scope, directoryID string, manifest storageformat.DirectoryManifest) (storageformat.DirectoryIndexChild, error) {
	if (manifest.SchemaVersion != 2 && manifest.SchemaVersion != 3) || manifest.IndexRootID == "" || manifest.IndexRootDigest == "" || manifest.EntryCount <= 0 {
		return storageformat.DirectoryIndexChild{}, domain.NewError(domain.ErrorInvalid, "invalid non-empty directory index manifest")
	}
	key := storageformat.DirectoryIndexNodeKey(scope.UserID().String(), areaName(scope.Area()), directoryID, manifest.IndexRootID)
	object, err := s.engine.backend.Get(ctx, key)
	if err != nil {
		return storageformat.DirectoryIndexChild{}, err
	}
	if storageformat.Digest(object.Body) != manifest.IndexRootDigest {
		return storageformat.DirectoryIndexChild{}, domain.NewError(domain.ErrorInvalid, "directory index root digest mismatch")
	}
	var envelope storageformat.Envelope
	var node storageformat.DirectoryIndexNode
	if err := storageformat.DecodeEnvelope(object.Body, key, directoryIndexNodeSchema, &envelope, &node); err != nil {
		return storageformat.DirectoryIndexChild{}, err
	}
	if err := validateDirectoryIndexNode(directoryID, node); err != nil {
		return storageformat.DirectoryIndexChild{}, err
	}
	root, err := directoryIndexNodeChild(node, manifest.IndexRootDigest)
	if err != nil || root.EntryCount != uint64(manifest.EntryCount) || root.RecursiveBytes != manifest.RecursiveBytes || root.RecursiveFileCount != manifest.RecursiveFileCount {
		return storageformat.DirectoryIndexChild{}, domain.NewError(domain.ErrorInvalid, "directory index root aggregate mismatch")
	}
	return root, nil
}

func (s *FileStore) directoryIndexEntry(ctx context.Context, scope domain.Scope, directoryID string, manifest storageformat.DirectoryManifest, name string) (storageformat.DirectoryEntry, error) {
	if manifest.EntryCount == 0 {
		return storageformat.DirectoryEntry{}, domain.NewError(domain.ErrorNotFound, "entry not found")
	}
	reference, err := s.directoryIndexRoot(ctx, scope, directoryID, manifest)
	if err != nil {
		return storageformat.DirectoryEntry{}, err
	}
	for {
		node, err := s.readDirectoryIndexNode(ctx, scope, directoryID, reference)
		if err != nil {
			return storageformat.DirectoryEntry{}, err
		}
		if node.Leaf {
			index := sort.Search(len(node.Entries), func(index int) bool { return node.Entries[index].Name >= name })
			if index < len(node.Entries) && node.Entries[index].Name == name {
				return node.Entries[index], nil
			}
			return storageformat.DirectoryEntry{}, domain.NewError(domain.ErrorNotFound, "entry not found")
		}
		index := sort.Search(len(node.Children), func(index int) bool { return node.Children[index].LastName >= name })
		if index == len(node.Children) || node.Children[index].FirstName > name {
			return storageformat.DirectoryEntry{}, domain.NewError(domain.ErrorNotFound, "entry not found")
		}
		reference = node.Children[index]
	}
}

func (s *FileStore) collectDirectoryIndexEntries(ctx context.Context, scope domain.Scope, directoryID string, manifest storageformat.DirectoryManifest, after string, descending bool, limit int) ([]storageformat.DirectoryEntry, error) {
	if manifest.EntryCount == 0 {
		return nil, nil
	}
	root, err := s.directoryIndexRoot(ctx, scope, directoryID, manifest)
	if err != nil {
		return nil, err
	}
	result := make([]storageformat.DirectoryEntry, 0, limit)
	var walk func(storageformat.DirectoryIndexChild) error
	walk = func(reference storageformat.DirectoryIndexChild) error {
		if len(result) >= limit || !descending && after != "" && reference.LastName <= after || descending && after != "" && reference.FirstName >= after {
			return nil
		}
		node, err := s.readDirectoryIndexNode(ctx, scope, directoryID, reference)
		if err != nil {
			return err
		}
		if node.Leaf {
			if descending {
				for index := len(node.Entries) - 1; index >= 0; index-- {
					entry := node.Entries[index]
					if after != "" && entry.Name >= after {
						continue
					}
					result = append(result, entry)
					if len(result) == limit {
						break
					}
				}
				return nil
			}
			for _, entry := range node.Entries {
				if after != "" && entry.Name <= after {
					continue
				}
				result = append(result, entry)
				if len(result) == limit {
					break
				}
			}
			return nil
		}
		if descending {
			for index := len(node.Children) - 1; index >= 0; index-- {
				if err := walk(node.Children[index]); err != nil {
					return err
				}
				if len(result) == limit {
					break
				}
			}
			return nil
		}
		for _, child := range node.Children {
			if err := walk(child); err != nil {
				return err
			}
			if len(result) == limit {
				break
			}
		}
		return nil
	}
	return result, walk(root)
}
