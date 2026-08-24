package architecturelab

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"

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
	ID         string
	Domains    map[string]Checkpoint
	DomainRefs map[string][]string
	Digest     string
}

type garbageCollectionResult struct {
	Inventoried int
	Deleted     int
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
	values, catalogRefs, err := readImmutableTreeWithRefs(ctx, catalog.tree, "checkpoint", "catalog-domain-index", head.DomainsRef)
	if err != nil {
		return catalogCheckpoint{}, err
	}
	for _, item := range values {
		id, body := item.Key, item.Value
		var value struct {
			HeadKey string `json:"headKey"`
		}
		if err := json.Unmarshal(body, &value); err != nil || value.HeadKey == "" {
			return catalogCheckpoint{}, domain.NewError(domain.ErrorInvalid, "invalid registered consistency domain")
		}
		domains[id] = value.HeadKey
	}
	ids := make([]string, 0, len(domains))
	for id := range domains {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	checkpoint := catalogCheckpoint{ID: checkpointID, Domains: make(map[string]Checkpoint, len(ids)), DomainRefs: make(map[string][]string, len(ids))}
	checkpoint.DomainRefs["__catalog__"] = append([]string(nil), catalogRefs...)
	type frozenCandidate struct {
		id      string
		engine  *embeddedGraphEngine
		head    embeddedHead
		version objectstore.NativeVersion
	}
	candidates := make([]frozenCandidate, 0, len(ids))
	for _, id := range ids {
		headKey, err := objectstore.ParseKey(domains[id])
		if err != nil {
			return catalogCheckpoint{}, err
		}
		engine := &embeddedGraphEngine{backend: catalog.backend, headKey: headKey, tree: immutableTree{backend: catalog.backend, domainID: id, candidate: "embedded"}}
		object, err := catalog.backend.Get(trace(ctx, "checkpoint", "freeze-domain-head", "domain-freeze-read"), headKey)
		if err != nil {
			return catalogCheckpoint{}, err
		}
		var domainHead embeddedHead
		if decode(object.Body, &domainHead) != nil || validateEmbeddedHead(domainHead) != nil {
			return catalogCheckpoint{}, domain.NewError(domain.ErrorInvalid, "invalid checkpoint domain head")
		}
		candidates = append(candidates, frozenCandidate{id: id, engine: engine, head: domainHead, version: object.Version})
	}
	for _, candidate := range candidates {
		if !candidate.head.Frozen {
			candidate.head.Frozen = true
			body, err := encode(candidate.head)
			if err != nil {
				return catalogCheckpoint{}, err
			}
			if _, err := catalog.backend.Put(trace(ctx, "checkpoint", "freeze-domain-commit", "domain-freeze-commit"), candidate.engine.headKey, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: candidate.version}); err != nil {
				return catalogCheckpoint{}, err
			}
		}
		body, _ := encode(candidate.head)
		checkpoint.Domains[candidate.id] = Checkpoint{ID: checkpointID + ":" + candidate.id, Revision: candidate.head.Revision, Digest: digest(body)}
		refs, err := candidate.engine.completeClosure(ctx, candidate.head)
		if err != nil {
			return catalogCheckpoint{}, err
		}
		checkpoint.DomainRefs[candidate.id] = refs
	}
	return checkpoint, nil
}

// CreateCheckpoint persists the portable closure roots after Freeze. Garbage
// collection is intentionally not part of checkpoint latency: unreachable
// immutable objects do not affect checkpoint authority and can be swept later
// behind a retained-checkpoint watermark.
func (catalog *domainCatalog) CreateCheckpoint(ctx context.Context, checkpointID string) (catalogCheckpoint, error) {
	checkpoint, err := catalog.Freeze(ctx, checkpointID)
	if err != nil {
		return catalogCheckpoint{}, err
	}
	body, err := encode(struct {
		SchemaVersion int                   `json:"schemaVersion"`
		ID            string                `json:"id"`
		Domains       map[string]Checkpoint `json:"domains"`
		DomainRefs    map[string][]string   `json:"domainRefs"`
	}{SchemaVersion: 1, ID: checkpoint.ID, Domains: checkpoint.Domains, DomainRefs: checkpoint.DomainRefs})
	if err != nil {
		return catalogCheckpoint{}, err
	}
	checkpoint.Digest = digest(body)
	key := candidateKey("catalog", catalog.id, "checkpoints/"+checkpoint.Digest+".json")
	if err := createImmutable(checkpointTrace(ctx, "checkpoint-closure"), catalog.backend, key, body); err != nil {
		return catalogCheckpoint{}, err
	}
	return checkpoint, nil
}

// completeClosure verifies and returns every immutable tree page reachable
// from the authoritative domain roots. Directory entries can introduce more
// tree roots, so the walk follows both B-tree child descriptors and embedded
// directory references. Each page is read once.
func (engine *embeddedGraphEngine) completeClosure(ctx context.Context, head embeddedHead) ([]string, error) {
	type closureItem struct {
		ref           string
		expected      *treeChild
		expectedLevel int
	}
	pages := make(map[string]treePage)
	queue := []closureItem{{ref: head.Live.DirectoryRef}, {ref: head.Trash.DirectoryRef}, {ref: head.OutcomeRef}}
	refs := make([]string, 0)
	for depth := 0; len(queue) > 0; depth++ {
		next := make([]closureItem, 0)
		for _, item := range queue {
			if page, found := pages[item.ref]; found {
				if item.expected != nil && verifyTreeChild(*item.expected, page, item.expectedLevel) != nil {
					return nil, domain.NewError(domain.ErrorInvalid, "checkpoint tree descriptor mismatch")
				}
				continue
			}
			page, err := engine.tree.readPageGrouped(ctx, "checkpoint", "checkpoint-closure-read", "checkpoint-closure-depth-"+strconv.Itoa(depth), item.ref)
			if err != nil {
				return nil, err
			}
			if item.expected != nil && verifyTreeChild(*item.expected, page, item.expectedLevel) != nil {
				return nil, domain.NewError(domain.ErrorInvalid, "checkpoint tree descriptor mismatch")
			}
			pages[item.ref] = page
			refs = append(refs, item.ref)
			if page.Level > 0 {
				for index := range page.Children {
					child := page.Children[index]
					next = append(next, closureItem{ref: child.Ref, expected: &child, expectedLevel: page.Level - 1})
				}
				continue
			}
			for _, value := range page.Values {
				var entry graphEntry
				if json.Unmarshal(value.Value, &entry) == nil && validateGraphEntry(entry) == nil && entry.Kind == NodeDirectory {
					next = append(next, closureItem{ref: entry.DirectoryRef})
				}
			}
		}
		queue = next
	}
	sort.Strings(refs)
	return refs, nil
}

// CollectGarbage sweeps only immutable page namespaces named by a retained,
// fully verified checkpoint. It neither inventories nor deletes file blobs;
// file retention additionally depends on product trash policy and derived-view
// watermarks. Provider deletes are free in the selected GCS price profile and
// are modeled as one bounded parallel phase.
func (catalog *domainCatalog) CollectGarbage(ctx context.Context, checkpoint catalogCheckpoint) (garbageCollectionResult, error) {
	if checkpoint.ID == "" || checkpoint.Digest == "" || len(checkpoint.DomainRefs) == 0 {
		return garbageCollectionResult{}, domain.NewError(domain.ErrorInvalid, "invalid retained checkpoint")
	}
	marked := make(map[string]bool)
	prefixes := make(map[string]bool)
	for _, refs := range checkpoint.DomainRefs {
		for _, ref := range refs {
			key, err := objectstore.ParseKey(ref)
			if err != nil {
				return garbageCollectionResult{}, err
			}
			marker := "/pages/"
			index := strings.Index(key.String(), marker)
			if index < 0 {
				return garbageCollectionResult{}, domain.NewError(domain.ErrorInvalid, "checkpoint closure contains a non-page object")
			}
			marked[key.String()] = true
			prefixes[key.String()[:index+len(marker)]] = true
		}
	}
	result := garbageCollectionResult{}
	for _, prefix := range sortedStringKeys(prefixes) {
		cursor := ""
		for {
			page, err := catalog.backend.List(trace(ctx, "garbage-collection", "immutable-inventory", "garbage-inventory"), objectstore.ListRequest{Prefix: prefix, Limit: 1000, Cursor: cursor})
			if err != nil {
				return garbageCollectionResult{}, err
			}
			result.Inventoried += len(page.Objects)
			for _, info := range page.Objects {
				if marked[info.Key.String()] {
					continue
				}
				if err := catalog.backend.Delete(trace(ctx, "garbage-collection", "immutable-sweep", "garbage-delete"), info.Key, objectstore.DeleteCondition{Version: info.Version}); err != nil && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrPreconditionFailed) {
					return garbageCollectionResult{}, err
				}
				result.Deleted++
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
	}
	return result, nil
}

func sortedStringKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
