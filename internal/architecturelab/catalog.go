package architecturelab

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

type catalogHead struct {
	SchemaVersion int    `json:"schemaVersion"`
	Revision      uint64 `json:"revision"`
	Frozen        bool   `json:"frozen"`
	DomainsRef    string `json:"domainsRef"`
}

type catalogCheckpoint struct {
	ID      string
	Domains map[string]Checkpoint
}

// domainCatalog tests the hypothesis that checkpoint quiescence belongs at
// domain registration and each domain's conditional head, rather than taxing
// every ordinary mutation with a global admission transaction.
type domainCatalog struct {
	backend objectstore.Backend
	id      string
	headKey objectstore.Key
	tree    immutableTree
}

func openDomainCatalog(ctx context.Context, backend objectstore.Backend, catalogID string) (*domainCatalog, error) {
	if backend == nil || !domainPattern.MatchString(catalogID) {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid domain catalog")
	}
	catalog := &domainCatalog{
		backend: backend,
		id:      catalogID,
		headKey: candidateKey("catalog", catalogID, "head.json"),
		tree:    immutableTree{backend: backend, domainID: catalogID, candidate: "catalog"},
	}
	emptyRef, err := catalog.tree.empty(ctx, "initialize", "catalog-tree-initial")
	if err != nil {
		return nil, err
	}
	body, err := encode(catalogHead{SchemaVersion: 1, Revision: 1, DomainsRef: emptyRef})
	if err != nil {
		return nil, err
	}
	if _, err := backend.Put(ctx, catalog.headKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil && !errors.Is(err, domain.ErrConflict) {
		return nil, err
	}
	if _, _, err := catalog.load(ctx, "initialize"); err != nil {
		return nil, err
	}
	return catalog, nil
}

func (catalog *domainCatalog) load(ctx context.Context, operation MutationKind) (catalogHead, objectstore.NativeVersion, error) {
	object, err := catalog.backend.Get(trace(ctx, operation, "catalog-head", ""), catalog.headKey)
	if err != nil {
		return catalogHead{}, "", err
	}
	var head catalogHead
	if err := decode(object.Body, &head); err != nil || head.SchemaVersion != 1 || head.Revision == 0 || head.DomainsRef == "" {
		return catalogHead{}, "", domain.NewError(domain.ErrorInvalid, "invalid domain catalog head")
	}
	return head, object.Version, nil
}

func (catalog *domainCatalog) Register(ctx context.Context, domainID string) (*embeddedGraphEngine, error) {
	if !domainPattern.MatchString(domainID) {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid consistency domain identity")
	}
	candidate, err := openEmbeddedGraph(ctx, catalog.backend, Options{DomainID: domainID})
	if err != nil {
		return nil, err
	}
	engine := candidate.(*embeddedGraphEngine)
	head, version, err := catalog.load(ctx, "register-domain")
	if err != nil {
		return nil, err
	}
	if head.Frozen {
		return nil, domain.NewError(domain.ErrorUnavailable, "domain catalog is frozen")
	}
	session := newTreeSession(catalog.tree)
	value, _ := encode(struct {
		HeadKey string `json:"headKey"`
	}{HeadKey: engine.headKey.String()})
	updated, err := session.apply(ctx, "register-domain", "catalog-domain-index", head.DomainsRef, []treeEdit{{Key: domainID, Value: value, Requirement: treeAbsent}})
	if errors.Is(err, domain.ErrConflict) {
		body, found, lookupErr := session.lookup(ctx, "register-domain", "catalog-domain-index", head.DomainsRef, domainID)
		if lookupErr != nil || !found {
			return nil, lookupErr
		}
		var existing struct {
			HeadKey string `json:"headKey"`
		}
		if json.Unmarshal(body, &existing) != nil || existing.HeadKey != engine.headKey.String() {
			return nil, domain.NewError(domain.ErrorConflict, "consistency domain identity is already registered")
		}
		return engine, nil
	}
	if err != nil {
		return nil, err
	}
	head.DomainsRef, head.Revision = updated, head.Revision+1
	body, err := encode(head)
	if err != nil {
		return nil, err
	}
	if _, err := catalog.backend.Put(trace(ctx, "register-domain", "catalog-commit", ""), catalog.headKey, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version}); err != nil {
		return nil, err
	}
	return engine, nil
}

func (catalog *domainCatalog) Freeze(ctx context.Context, checkpointID string) (catalogCheckpoint, error) {
	if checkpointID == "" {
		return catalogCheckpoint{}, domain.NewError(domain.ErrorInvalid, "checkpoint identity is required")
	}
	var head catalogHead
	for {
		candidate, version, err := catalog.load(ctx, "checkpoint")
		if err != nil {
			return catalogCheckpoint{}, err
		}
		head = candidate
		if head.Frozen {
			break
		}
		head.Frozen, head.Revision = true, head.Revision+1
		body, err := encode(head)
		if err != nil {
			return catalogCheckpoint{}, err
		}
		if _, err := catalog.backend.Put(checkpointTrace(ctx, "catalog-freeze-commit"), catalog.headKey, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version}); err == nil {
			break
		} else if !errors.Is(err, domain.ErrPreconditionFailed) && !errors.Is(err, domain.ErrConflict) {
			return catalogCheckpoint{}, err
		}
	}

	domains := make(map[string]string)
	if err := catalog.tree.walk(ctx, "checkpoint", "catalog-domain-index", head.DomainsRef, func(id string, body json.RawMessage) error {
		var value struct {
			HeadKey string `json:"headKey"`
		}
		if err := json.Unmarshal(body, &value); err != nil || value.HeadKey == "" {
			return domain.NewError(domain.ErrorInvalid, "invalid registered consistency domain")
		}
		domains[id] = value.HeadKey
		return nil
	}); err != nil {
		return catalogCheckpoint{}, err
	}
	ids := make([]string, 0, len(domains))
	for id := range domains {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	checkpoint := catalogCheckpoint{ID: checkpointID, Domains: make(map[string]Checkpoint, len(ids))}
	for _, id := range ids {
		headKey, err := objectstore.ParseKey(domains[id])
		if err != nil {
			return catalogCheckpoint{}, err
		}
		engine := &embeddedGraphEngine{backend: catalog.backend, headKey: headKey, tree: immutableTree{backend: catalog.backend, domainID: id, candidate: "embedded"}}
		frozen, err := engine.Freeze(ctx, checkpointID+":"+id)
		if err != nil {
			return catalogCheckpoint{}, err
		}
		checkpoint.Domains[id] = frozen
	}
	return checkpoint, nil
}
