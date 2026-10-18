package portable

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"strconv"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// This separator commits the deterministic traversal, ownership, hint,
// expanded-size grouping and midpoint-splitting policy. Any byte-affecting
// policy change needs a new separator: a final pack ID must never acquire
// different bytes after a partial/lost successful publication.
const domainClosureLayoutVersion = "endlessfs-domain-closure-postorder-v1"

type domainClosureIdentity struct{ packID, digest string }
type domainClosurePage struct {
	original domainPageRef
	page     storageformat.DomainPage
}
type domainClosureRange struct{ first, end int }
type domainClosure struct {
	source   *consistencyDomainTreeSession
	pages    []domainClosurePage
	indices  map[domainClosureIdentity]int
	owned    map[domainClosureIdentity]storageformat.DomainPage
	visiting map[domainClosureIdentity]bool
}

func domainClosureContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return domain.WrapError(domain.ErrorUnavailable, "metadata packing canceled", err)
	}
	return nil
}

func domainClosureChild(child storageformat.DomainPageChild) domainPageRef {
	return domainPageRef{root: storageformat.DomainTreeRoot{Digest: child.Digest, PackID: child.PackID, Level: child.Level, EntryCount: child.EntryCount, ByteCount: child.ByteCount}, firstKey: child.FirstKey, lastKey: child.LastKey, leafKeyFilter: child.LeafKeyFilter}
}

func domainClosureCopiedChild(reference domainPageRef) storageformat.DomainPageChild {
	return storageformat.DomainPageChild{Digest: reference.root.Digest, PackID: reference.root.PackID, Level: reference.root.Level, EntryCount: reference.root.EntryCount, ByteCount: reference.root.ByteCount, FirstKey: reference.firstKey, LastKey: reference.lastKey, LeafKeyFilter: reference.leafKeyFilter}
}

func ownDomainClosurePage(page storageformat.DomainPage) storageformat.DomainPage {
	if page.Children != nil {
		children := make([]storageformat.DomainPageChild, len(page.Children))
		copy(children, page.Children)
		page.Children = children
	}
	if page.Entries != nil {
		entries := make([]storageformat.DomainEntry, len(page.Entries))
		copy(entries, page.Entries)
		for index := range entries {
			entries[index].Value = bytes.Clone(entries[index].Value)
		}
		page.Entries = entries
	}
	return page
}

// Validate every incoming descriptor before memo reuse. A cached readPage only
// verifies descriptors, so validate the owned ORIGINAL digest as well; deriving
// a new digest first would normalize corrupt staged bytes into valid state.
func (closure *domainClosure) collect(ctx context.Context, reference domainPageRef) error {
	if err := domainClosureContext(ctx); err != nil {
		return err
	}
	if reference.root.Digest == "" || reference.root.PackID != closure.source.packID {
		return nil
	}
	identity := domainClosureIdentity{reference.root.PackID, reference.root.Digest}
	page, found := closure.owned[identity]
	if !found {
		original, err := closure.source.readPage(ctx, reference)
		if err != nil {
			return err
		}
		page = ownDomainClosurePage(original)
		if page.DomainID != closure.source.reference.ID || page.Kind != closure.source.reference.Kind {
			return domain.NewError(domain.ErrorInvalid, "consistency-domain closure page binding mismatch")
		}
		if err := storageformat.ValidateDomainPage(page, reference.root.Digest); err != nil {
			return err
		}
		closure.owned[identity] = page
	}
	if err := verifyConsistencyDomainPageRef(page, reference); err != nil {
		return err
	}
	if closure.visiting[identity] {
		return domain.NewError(domain.ErrorInvalid, "cyclic consistency-domain staged closure")
	}
	if _, found := closure.indices[identity]; found {
		return nil
	}
	closure.visiting[identity] = true
	for _, child := range page.Children {
		if err := closure.collect(ctx, domainClosureChild(child)); err != nil {
			return err
		}
	}
	original, err := consistencyDomainPageDescriptor(page, reference.root.Digest, reference.root.PackID)
	if err != nil {
		return err
	}
	closure.visiting[identity] = false
	closure.indices[identity] = len(closure.pages)
	closure.pages = append(closure.pages, domainClosurePage{original: original, page: page})
	return nil
}

func domainClosureHashField(digest hash.Hash, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = digest.Write(length[:])
	_, _ = digest.Write(value)
}

// Stream bounded records rather than encoding an unbounded aggregate. Original
// roots/digests commit opaque values and prior references; the source seed also
// commits preparation time, deadline and mutation identity. Rewritten digests
// cannot be used here because they contain the IDs being assigned.
func (closure *domainClosure) binding(ctx context.Context, roots []storageformat.DomainTreeRoot) (string, error) {
	digest := sha256.New()
	for _, value := range []string{domainClosureLayoutVersion, closure.source.reference.ID, string(closure.source.reference.Kind), closure.source.packSeed, closure.source.packID, strconv.Itoa(storageformat.MaxDomainPagePackBytes), strconv.Itoa(storageformat.MaxExpandedDomainPagePackBytes), strconv.Itoa(storageformat.MaxCanonicalBytes), "ordered-roots"} {
		domainClosureHashField(digest, []byte(value))
	}
	for _, root := range roots {
		if err := domainClosureContext(ctx); err != nil {
			return "", err
		}
		body, err := storageformat.EncodeCanonical(root)
		if err != nil {
			return "", err
		}
		domainClosureHashField(digest, body)
	}
	domainClosureHashField(digest, []byte("original-postorder-pages"))
	for _, node := range closure.pages {
		if err := domainClosureContext(ctx); err != nil {
			return "", err
		}
		body, err := storageformat.EncodeCanonical(node.original.root)
		if err != nil {
			return "", err
		}
		domainClosureHashField(digest, body)
		for _, value := range []string{node.original.firstKey, node.original.lastKey, node.original.leafKeyFilter} {
			domainClosureHashField(digest, []byte(value))
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func (closure *domainClosure) rewritePage(index int, references []domainPageRef) storageformat.DomainPage {
	page := closure.pages[index].page
	if len(page.Children) == 0 {
		return page
	}
	page.Children = append([]storageformat.DomainPageChild(nil), page.Children...)
	for index, child := range page.Children {
		if child.PackID != closure.source.packID {
			continue // Preserve prior packed/unpacked references and hints exactly.
		}
		copied := references[closure.indices[domainClosureIdentity{child.PackID, child.Digest}]]
		page.Children[index] = domainClosureCopiedChild(copied)
	}
	return page
}

func (closure *domainClosure) initialRanges(ctx context.Context) ([]domainClosureRange, error) {
	header, err := storageformat.EncodeCanonical(storageformat.DomainPagePack{SchemaVersion: 1, DomainID: closure.source.reference.ID, Kind: closure.source.reference.Kind, PackID: strings.Repeat("0", 64), Pages: []storageformat.DomainPackedPage{}})
	if err != nil {
		return nil, err
	}
	originals := make([]domainPageRef, len(closure.pages))
	for index, node := range closure.pages {
		originals[index] = node.original
	}
	var groups []domainClosureRange
	first, expanded := 0, len(header)
	for index := range closure.pages {
		if err := domainClosureContext(ctx); err != nil {
			return nil, err
		}
		// Rewritten digest/pack IDs retain their validated64-hex width. Restore
		// the same full staged-leaf hints as the previous fallback before sizing.
		body, err := storageformat.EncodeCanonical(closure.rewritePage(index, originals))
		if err != nil {
			return nil, err
		}
		memberBytes := len(`{"digest":"","page":}`) + 64 + len(body)
		comma := 0
		if index > first {
			comma = 1
		}
		if expanded+memberBytes+comma > storageformat.MaxExpandedDomainPagePackBytes && index > first {
			groups = append(groups, domainClosureRange{first: first, end: index})
			first, expanded, comma = index, len(header), 0
		}
		expanded += memberBytes + comma
	}
	if first < len(closure.pages) {
		groups = append(groups, domainClosureRange{first: first, end: len(closure.pages)})
	}
	return groups, nil
}

// Whole groups are preflighted before any NEW-layout PUT. Range IDs remain
// stable when another range splits; all byte-affecting choices still derive
// from the complete bound immutable input and fixed algorithm version. No
// provisional body under these IDs is published. Existing original-ID opaque
// dependencies may already be durable in the materialized caller.
func repackOwnedDomainClosure(ctx context.Context, source *consistencyDomainTreeSession, roots []storageformat.DomainTreeRoot) ([]storageformat.DomainTreeRoot, error) {
	closure := &domainClosure{source: source, indices: make(map[domainClosureIdentity]int), owned: make(map[domainClosureIdentity]storageformat.DomainPage), visiting: make(map[domainClosureIdentity]bool)}
	for _, root := range roots {
		if err := closure.collect(ctx, domainPageRef{root: root}); err != nil {
			return nil, err
		}
	}
	result := make([]storageformat.DomainTreeRoot, len(roots))
	copy(result, roots)
	if len(closure.pages) == 0 {
		return result, nil
	}
	binding, err := closure.binding(ctx, roots)
	if err != nil {
		return nil, err
	}
	groups, err := closure.initialRanges(ctx)
	if err != nil {
		return nil, err
	}
	for {
		if err := domainClosureContext(ctx); err != nil {
			return nil, err
		}
		sessions := make([]*consistencyDomainTreeSession, len(groups))
		owners := make([]int, len(closure.pages))
		for index, group := range groups {
			session := newConsistencyDomainTreeSession(source.store, source.reference)
			session.pageKey, session.packKey = source.pageKey, source.packKey
			session.enablePackedWrites(source.packSeed)
			if err := session.bindPackedMutation(binding + "/" + strconv.Itoa(group.first) + "/" + strconv.Itoa(group.end)); err != nil {
				return nil, err
			}
			if _, err := session.ensurePackID(closure.pages[group.first].original.root.Digest); err != nil {
				return nil, err
			}
			sessions[index] = session
			for page := group.first; page < group.end; page++ {
				owners[page] = index
			}
		}
		references := make([]domainPageRef, len(closure.pages))
		for index := range closure.pages {
			if err := domainClosureContext(ctx); err != nil {
				return nil, err
			}
			references[index], err = sessions[owners[index]].writePage(ctx, closure.rewritePage(index, references))
			if err != nil {
				return nil, err
			}
		}
		var next []domainClosureRange
		for index, group := range groups {
			if err := domainClosureContext(ctx); err != nil {
				return nil, err
			}
			session := sessions[index]
			pages := make([]storageformat.DomainPackedPage, 0, len(session.pendingPack))
			for digest, page := range session.pendingPack {
				pages = append(pages, storageformat.DomainPackedPage{Digest: digest, Page: page})
			}
			_, encodeErr := storageformat.EncodeDomainPagePack(storageformat.DomainPagePack{SchemaVersion: 1, DomainID: source.reference.ID, Kind: source.reference.Kind, PackID: session.packID, Pages: pages})
			if err := domainClosureContext(ctx); err != nil {
				return nil, err
			}
			if errors.Is(encodeErr, storageformat.ErrDomainPagePackTooLarge) && group.end-group.first > 1 {
				middle := group.first + (group.end-group.first)/2
				next = append(next, domainClosureRange{first: group.first, end: middle}, domainClosureRange{first: middle, end: group.end})
				continue
			}
			if encodeErr != nil {
				return nil, encodeErr
			}
			next = append(next, group)
		}
		if len(next) != len(groups) {
			groups = next // Strict refinement; at most N-1 splits before singleton.
			continue      // Rebuild from ORIGINAL pages and revalidate every final group.
		}
		for _, session := range sessions {
			if err := domainClosureContext(ctx); err != nil {
				return nil, err
			}
			if err := session.flushPack(ctx); err != nil {
				return nil, err
			}
		}
		for index, root := range roots {
			if page, found := closure.indices[domainClosureIdentity{root.PackID, root.Digest}]; found {
				result[index] = references[page].root
			}
		}
		return result, nil
	}
}
