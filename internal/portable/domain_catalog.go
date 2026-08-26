package portable

import (
	"context"
	"encoding/base64"
	"errors"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const domainCatalogHeadSchema = "consistency-domain-catalog-head-v1"

type domainCatalogSnapshot struct {
	head     storageformat.DomainCatalogHead
	envelope storageformat.Envelope
	object   objectstore.Object
	exists   bool
}

type domainCatalog struct {
	backend objectstore.Backend
	store   *consistencyDomainStore
}

func newDomainCatalog(backend objectstore.Backend, scheduler Scheduler) *domainCatalog {
	return &domainCatalog{backend: backend, store: newConsistencyDomainStore(backend, scheduler)}
}

func catalogEntryKey(reference consistencyDomainRef) string {
	return string(reference.Kind) + "/" + base64.RawURLEncoding.EncodeToString([]byte(reference.ID))
}

func (catalog *domainCatalog) load(ctx context.Context) (domainCatalogSnapshot, error) {
	key := storageformat.DomainCatalogHeadKey()
	object, err := catalog.backend.Get(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		return domainCatalogSnapshot{head: storageformat.DomainCatalogHead{SchemaVersion: 1}}, nil
	}
	if err != nil {
		return domainCatalogSnapshot{}, err
	}
	var envelope storageformat.Envelope
	var head storageformat.DomainCatalogHead
	if err := storageformat.DecodeEnvelope(object.Body, key, domainCatalogHeadSchema, &envelope, &head); err != nil {
		return domainCatalogSnapshot{}, err
	}
	if err := storageformat.ValidateDomainCatalogHead(head); err != nil {
		return domainCatalogSnapshot{}, err
	}
	return domainCatalogSnapshot{head: head, envelope: envelope, object: object, exists: true}, nil
}

func (catalog *domainCatalog) publish(ctx context.Context, snapshot domainCatalogSnapshot, next storageformat.DomainCatalogHead) error {
	key := storageformat.DomainCatalogHeadKey()
	body, err := storageformat.EncodeEnvelope(domainCatalogHeadSchema, key, snapshot.envelope.Revision+1, next)
	if err != nil {
		return err
	}
	condition := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
	if snapshot.exists {
		condition = objectstore.PutCondition{Mode: objectstore.PutMatch, Version: snapshot.object.Version}
	}
	_, err = catalog.backend.Put(ctx, key, body, condition)
	return err
}

func (catalog *domainCatalog) entryAt(ctx context.Context, head storageformat.DomainCatalogHead, reference consistencyDomainRef) (storageformat.DomainCatalogEntry, bool, error) {
	value, found, err := newDomainCatalogTreeSession(catalog.store).lookup(ctx, head.Root, catalogEntryKey(reference))
	if err != nil || !found {
		return storageformat.DomainCatalogEntry{}, found, err
	}
	var entry storageformat.DomainCatalogEntry
	if err := decodeCanonicalValue(value.Data, &entry); err != nil {
		return storageformat.DomainCatalogEntry{}, false, err
	}
	if entry.DomainID != reference.ID || entry.Kind != reference.Kind || entry.HeadKey != storageformat.DomainHeadKey(reference.Kind, reference.ID).String() {
		return storageformat.DomainCatalogEntry{}, false, domain.NewError(domain.ErrorInvalid, "misbound consistency-domain catalog entry")
	}
	return entry, true, nil
}

func (catalog *domainCatalog) register(ctx context.Context, reference consistencyDomainRef) error {
	if err := validateConsistencyDomainRef(reference); err != nil {
		return err
	}
	entry := storageformat.DomainCatalogEntry{DomainID: reference.ID, Kind: reference.Kind, HeadKey: storageformat.DomainHeadKey(reference.Kind, reference.ID).String()}
	body, err := storageformat.EncodeCanonical(entry)
	if err != nil {
		return err
	}
	change := storageformat.DomainChange{Key: catalogEntryKey(reference), Value: body, LogicalVersion: storageformat.Digest(append([]byte("endlessfs-domain-catalog-entry-v1\x00"), body...))}
	for {
		snapshot, err := catalog.load(ctx)
		if err != nil {
			return err
		}
		if existing, found, err := catalog.entryAt(ctx, snapshot.head, reference); err != nil {
			return err
		} else if found {
			if existing != entry {
				return domain.NewError(domain.ErrorConflict, "consistency-domain catalog identity conflict")
			}
			return nil
		}
		if snapshot.head.FreezeEpoch != 0 {
			return domain.NewError(domain.ErrorUnavailable, "consistency-domain catalog is frozen")
		}
		root, err := newDomainCatalogTreeSession(catalog.store).apply(ctx, snapshot.head.Root, []storageformat.DomainChange{change})
		if err != nil {
			return err
		}
		next := snapshot.head
		next.Revision++
		next.Root = root
		if err := catalog.publish(ctx, snapshot, next); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
	}
}

func (catalog *domainCatalog) entries(ctx context.Context, head storageformat.DomainCatalogHead) ([]storageformat.DomainCatalogEntry, error) {
	entries := make([]storageformat.DomainCatalogEntry, 0)
	err := catalog.visitEntries(ctx, head, func(entry storageformat.DomainCatalogEntry) error {
		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(left, right int) bool {
		if entries[left].Kind == entries[right].Kind {
			return entries[left].DomainID < entries[right].DomainID
		}
		return entries[left].Kind < entries[right].Kind
	})
	return entries, nil
}

// visitEntries keeps catalog traversal memory proportional to tree height.
// Checkpoint and cross-domain query paths use it instead of materializing the
// complete domain inventory.
func (catalog *domainCatalog) visitEntries(ctx context.Context, head storageformat.DomainCatalogHead, visit func(storageformat.DomainCatalogEntry) error) error {
	if visit == nil {
		return domain.NewError(domain.ErrorInvalid, "consistency-domain catalog visitor is required")
	}
	iterator, err := newConsistencyDomainTreeIterator(ctx, newDomainCatalogTreeSession(catalog.store), head.Root)
	if err != nil {
		return err
	}
	previous := ""
	for {
		value, found, err := iterator.Next()
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		var entry storageformat.DomainCatalogEntry
		if err := decodeCanonicalValue(value.Value, &entry); err != nil || catalogEntryKey(consistencyDomainRef{Kind: entry.Kind, ID: entry.DomainID}) != value.Key || entry.HeadKey != storageformat.DomainHeadKey(entry.Kind, entry.DomainID).String() {
			return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain catalog entry")
		}
		if previous != "" && value.Key <= previous {
			return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain catalog order")
		}
		previous = value.Key
		if err := visit(entry); err != nil {
			return err
		}
	}
}

func (catalog *domainCatalog) freeze(ctx context.Context, epoch uint64) ([]storageformat.DomainCatalogEntry, error) {
	if epoch == 0 {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid consistency-domain catalog freeze epoch")
	}
	for {
		snapshot, err := catalog.load(ctx)
		if err != nil {
			return nil, err
		}
		if snapshot.head.FreezeEpoch != 0 {
			if snapshot.head.FreezeEpoch != epoch {
				return nil, domain.NewError(domain.ErrorConflict, "consistency-domain catalog is frozen at another epoch")
			}
			return catalog.entries(ctx, snapshot.head)
		}
		next := snapshot.head
		next.Revision++
		next.FreezeEpoch = epoch
		if err := catalog.publish(ctx, snapshot, next); err == nil {
			return catalog.entries(ctx, next)
		} else if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return nil, err
		}
	}
}

func (catalog *domainCatalog) freezeDomains(ctx context.Context, epoch uint64) ([]storageformat.DomainCatalogEntry, error) {
	entries, err := catalog.freeze(ctx, epoch)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if err := catalog.store.freeze(ctx, consistencyDomainRef{Kind: entry.Kind, ID: entry.DomainID}, epoch); err != nil {
			return nil, domain.WrapError(domain.KindOf(err), "freeze registered consistency domain "+entry.DomainID, err)
		}
	}
	return entries, nil
}

func (catalog *domainCatalog) unfreeze(ctx context.Context, epoch uint64) error {
	if epoch == 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain catalog freeze epoch")
	}
	for {
		snapshot, err := catalog.load(ctx)
		if err != nil {
			return err
		}
		if snapshot.head.FreezeEpoch == 0 {
			return nil
		}
		if snapshot.head.FreezeEpoch != epoch {
			return domain.NewError(domain.ErrorConflict, "consistency-domain catalog is frozen at another epoch")
		}
		entries, err := catalog.entries(ctx, snapshot.head)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := catalog.store.unfreeze(ctx, consistencyDomainRef{Kind: entry.Kind, ID: entry.DomainID}, epoch); err != nil {
				return err
			}
		}
		next := snapshot.head
		next.Revision++
		next.FreezeEpoch = 0
		if err := catalog.publish(ctx, snapshot, next); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
	}
}
