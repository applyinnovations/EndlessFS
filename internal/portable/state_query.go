package portable

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func (e *Engine) readDomainCatalogIfPresent(ctx context.Context) (domainCatalogSnapshot, bool, error) {
	key := storageformat.DomainCatalogHeadKey()
	object, err := e.backend.Get(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		return domainCatalogSnapshot{}, false, nil
	}
	if err != nil {
		return domainCatalogSnapshot{}, false, err
	}
	var envelope storageformat.Envelope
	var head storageformat.DomainCatalogHead
	if err := storageformat.DecodeEnvelope(object.Body, key, domainCatalogHeadSchema, &envelope, &head); err != nil {
		return domainCatalogSnapshot{}, false, err
	}
	if err := storageformat.ValidateDomainCatalogHead(head); err != nil {
		return domainCatalogSnapshot{}, false, err
	}
	return domainCatalogSnapshot{head: head, envelope: envelope, object: object}, true, nil
}

func stateQueryReference(id string) consistencyDomainRef {
	return consistencyDomainRef{Kind: storageformat.DomainCapability, ID: id}
}

func (e *Engine) buildStateQuerySnapshot(ctx context.Context, prefix state.Prefix, expiresAt time.Time) (storageformat.StateQuerySnapshot, string, error) {
	catalogSnapshot, found, err := e.readDomainCatalogIfPresent(ctx)
	if err != nil {
		return storageformat.StateQuerySnapshot{}, "", err
	}
	id, err := e.ids.OpaqueID()
	if err != nil {
		return storageformat.StateQuerySnapshot{}, "", err
	}
	domainID := "state-query:" + id
	querySession := newConsistencyDomainTreeSession(e.stateDomainStore(), stateQueryReference(domainID))
	runs := make([]storageformat.DomainTreeRoot, 0)
	if found {
		catalog := newDomainCatalog(e.backend, e.scheduler)
		err = catalog.visitEntries(ctx, catalogSnapshot.head, func(entry storageformat.DomainCatalogEntry) error {
			reference := consistencyDomainRef{Kind: entry.Kind, ID: entry.DomainID}
			if err := e.resolveStateTransition009(ctx, reference); err != nil {
				return err
			}
			snapshot, err := e.stateDomainStore().loadHead(ctx, reference)
			if err != nil {
				return err
			}
			if !snapshot.exists || !snapshot.head.Registered {
				return nil
			}
			builder := newConsistencyDomainTreeBuilder(ctx, querySession)
			after, count := "", 0
			for {
				values, err := e.stateDomainStore().listAtHead(ctx, reference, snapshot.head, prefix.String(), after, domainPageMaximumItems)
				if err != nil {
					return err
				}
				for _, value := range values {
					if err := builder.Add(value); err != nil {
						return err
					}
					count++
				}
				if len(values) < domainPageMaximumItems {
					break
				}
				after = values[len(values)-1].Key
			}
			if count == 0 {
				return nil
			}
			root, err := builder.Finish()
			if err != nil {
				return err
			}
			querySession.pages = make(map[string]storageformat.DomainPage)
			runs = append(runs, root)
			return nil
		})
		if err != nil {
			return storageformat.StateQuerySnapshot{}, "", err
		}
	}
	for len(runs) > 1 {
		next := make([]storageformat.DomainTreeRoot, 0, (len(runs)+namespaceProjectionMergeFanIn-1)/namespaceProjectionMergeFanIn)
		for offset := 0; offset < len(runs); offset += namespaceProjectionMergeFanIn {
			end := min(offset+namespaceProjectionMergeFanIn, len(runs))
			root, err := mergeNamespaceProjectionRuns(ctx, querySession, runs[offset:end])
			if err != nil {
				return storageformat.StateQuerySnapshot{}, "", err
			}
			next = append(next, root)
		}
		runs = next
	}
	root := storageformat.DomainTreeRoot{}
	if len(runs) == 1 {
		root = runs[0]
	}
	snapshot := storageformat.StateQuerySnapshot{SchemaVersion: 1, Prefix: prefix.String(), DomainID: domainID, Root: root, ExpiresAt: expiresAt.UTC()}
	if err := storageformat.ValidateStateQuerySnapshot(snapshot); err != nil {
		return storageformat.StateQuerySnapshot{}, "", err
	}
	// An empty first page cannot produce a continuation cursor, so persisting an
	// immutable snapshot would add a paid write without preserving any state.
	if root.Digest == "" {
		return snapshot, "", nil
	}
	body, err := storageformat.EncodeCanonical(snapshot)
	if err != nil {
		return storageformat.StateQuerySnapshot{}, "", err
	}
	digest := storageformat.Digest(body)
	if _, err := e.backend.Put(ctx, storageformat.StateQuerySnapshotKey(digest), body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil && !errors.Is(err, domain.ErrConflict) {
		return storageformat.StateQuerySnapshot{}, "", err
	}
	return snapshot, digest, nil
}

func (e *Engine) loadStateQuerySnapshot(ctx context.Context, prefix state.Prefix, digest string) (storageformat.StateQuerySnapshot, error) {
	if digest == "" {
		return storageformat.StateQuerySnapshot{}, domain.NewError(domain.ErrorInvalid, "invalid state-query snapshot")
	}
	key := storageformat.StateQuerySnapshotKey(digest)
	object, err := e.backend.Get(ctx, key)
	if err != nil {
		return storageformat.StateQuerySnapshot{}, err
	}
	if storageformat.Digest(object.Body) != digest {
		return storageformat.StateQuerySnapshot{}, domain.NewError(domain.ErrorInvalid, "state-query snapshot digest mismatch")
	}
	var snapshot storageformat.StateQuerySnapshot
	if err := decodeCanonicalValue(object.Body, &snapshot); err != nil {
		return storageformat.StateQuerySnapshot{}, err
	}
	if err := storageformat.ValidateStateQuerySnapshot(snapshot); err != nil || snapshot.Prefix != prefix.String() || !e.clock.Now().Before(snapshot.ExpiresAt) {
		return storageformat.StateQuerySnapshot{}, domain.NewError(domain.ErrorInvalid, "invalid or expired state-query snapshot")
	}
	return snapshot, nil
}

func statePageFromEntries(entries []storageformat.DomainEntry) (state.Page, error) {
	page := state.Page{Items: make([]state.Item, 0, len(entries))}
	for _, entry := range entries {
		logical, err := parseExistingStateKey(entry.Key)
		if err != nil {
			return state.Page{}, err
		}
		data, err := decodeStateValue009(logical, entry.Value)
		if err != nil {
			return state.Page{}, err
		}
		page.Items = append(page.Items, state.Item{Key: logical, Value: state.Value{Data: data, Version: state.Version(entry.LogicalVersion)}})
	}
	return page, nil
}

func (e *Engine) listStateAcrossDomains(ctx context.Context, prefix state.Prefix, request state.PageRequest, limit int, after, digest string, expiresAt time.Time) (state.Page, error) {
	var snapshot storageformat.StateQuerySnapshot
	var err error
	if digest == "" {
		snapshot, digest, err = e.buildStateQuerySnapshot(ctx, prefix, expiresAt)
	} else {
		snapshot, err = e.loadStateQuerySnapshot(ctx, prefix, digest)
	}
	if err != nil {
		return state.Page{}, err
	}
	session := newConsistencyDomainTreeSession(e.stateDomainStore(), stateQueryReference(snapshot.DomainID))
	entries, err := session.collect(ctx, snapshot.Root, prefix.String(), after, limit+1)
	if err != nil {
		return state.Page{}, err
	}
	hasMore := len(entries) > limit
	if hasMore {
		entries = entries[:limit]
	}
	page, err := statePageFromEntries(entries)
	if err != nil {
		return state.Page{}, err
	}
	if hasMore {
		cursor := stateListCursor{SchemaVersion: 4, Prefix: prefix.String(), Limit: limit, Namespace: strings.SplitN(prefix.String(), "/", 2)[0], Revision: 1, Snapshot: digest, Composite: true, After: entries[len(entries)-1].Key, ExpiresAt: snapshot.ExpiresAt}
		page.NextCursor, err = e.encodeStateListCursor(cursor)
	}
	return page, err
}
