package architecturelab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

const batchPageItems = 256

type batchNamespaceHead struct {
	SchemaVersion int     `json:"schemaVersion"`
	Revision      uint64  `json:"revision"`
	LiveRef       string  `json:"liveRef"`
	TrashRef      string  `json:"trashRef"`
	BatchRef      string  `json:"batchRef,omitempty"`
	Latest        Outcome `json:"latest,omitempty"`
}

type batchNamespaceValue struct {
	Tombstone bool       `json:"tombstone,omitempty"`
	Entry     graphEntry `json:"entry,omitempty"`
}

type batchSelection struct {
	Source      string `json:"source"`
	Destination string `json:"destination,omitempty"`
}

// batchNamespace is a focused executable proof for large explicit selections.
// The ordinary hybrid keeps tiny mutations inline; this companion shape stores
// a large change set in immutable pages and publishes its root with one head
// CAS. It proves atomic visibility and the provider-request slope without
// pretending a 10,000-item delta fits in the conditional head.
type batchNamespace struct {
	backend   objectstore.Backend
	domainID  string
	headKey   objectstore.Key
	baseTree  immutableTree
	batchTree immutableTree
}

func openBatchNamespace(ctx context.Context, backend objectstore.Backend, domainID string) (*batchNamespace, error) {
	if err := validateOptions(backend, Options{DomainID: domainID}); err != nil {
		return nil, err
	}
	engine := &batchNamespace{
		backend: backend, domainID: domainID,
		headKey:   candidateKey("batch-namespace", domainID, "head.json"),
		baseTree:  immutableTree{backend: backend, domainID: domainID, candidate: "batch-base", pageItems: batchPageItems},
		batchTree: immutableTree{backend: backend, domainID: domainID, candidate: "batch-delta", pageItems: batchPageItems},
	}
	empty, err := engine.baseTree.empty(ctx, "initialize", "batch-base")
	if err != nil {
		return nil, err
	}
	body, _ := encode(batchNamespaceHead{SchemaVersion: 1, Revision: 1, LiveRef: empty, TrashRef: empty})
	if _, err := backend.Put(trace(ctx, "initialize", "batch-head", ""), engine.headKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil && !errors.Is(err, domain.ErrConflict) {
		return nil, err
	}
	if _, _, err := engine.load(ctx, "initialize"); err != nil {
		return nil, err
	}
	return engine, nil
}

func (engine *batchNamespace) load(ctx context.Context, operation MutationKind) (batchNamespaceHead, objectstore.NativeVersion, error) {
	object, err := engine.backend.Get(trace(ctx, operation, "batch-head", ""), engine.headKey)
	if err != nil {
		return batchNamespaceHead{}, "", err
	}
	var head batchNamespaceHead
	if decode(object.Body, &head) != nil || head.SchemaVersion != 1 || head.Revision == 0 || head.LiveRef == "" || head.TrashRef == "" {
		return batchNamespaceHead{}, "", domain.NewError(domain.ErrorInvalid, "invalid batch namespace head")
	}
	return head, object.Version, nil
}

// Seed creates a compacted baseline outside the measured foreground interval.
func (engine *batchNamespace) Seed(ctx context.Context, names []string) error {
	return engine.SeedArea(ctx, AreaLive, names)
}

func (engine *batchNamespace) SeedArea(ctx context.Context, area Area, names []string) error {
	if !area.valid() {
		return domain.NewError(domain.ErrorInvalid, "invalid batch seed area")
	}
	head, version, err := engine.load(ctx, "seed")
	if err != nil {
		return err
	}
	values := make([]treeValue, len(names))
	for index, name := range names {
		if name == "" || index > 0 && names[index-1] >= name {
			return domain.NewError(domain.ErrorInvalid, "batch seed names must be unique and sorted")
		}
		entry := graphEntry{NodeID: "node-" + name, Kind: NodeFile, Size: 1, FileCount: 1, BlobIdentity: "blob-" + name, ContentVersion: "version-" + name}
		body, _ := encode(entry)
		values[index] = treeValue{Key: name, Value: body}
	}
	ref, err := buildImmutableTree(ctx, engine.baseTree, "seed", "batch-base", values)
	if err != nil {
		return err
	}
	if area == AreaLive {
		head.LiveRef = ref
	} else {
		head.TrashRef = ref
	}
	head.Revision++
	body, _ := encode(head)
	_, err = engine.backend.Put(trace(ctx, "seed", "batch-head", ""), engine.headKey, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version})
	return err
}

func (engine *batchNamespace) Trash(ctx context.Context, mutationID string, names []string) (Outcome, error) {
	changes := make([]batchSelection, len(names))
	for index, name := range names {
		changes[index] = batchSelection{Source: name, Destination: name}
	}
	return engine.ApplySelection(ctx, mutationID, "batch-trash", AreaLive, AreaTrash, changes, false)
}

func (engine *batchNamespace) ApplySelection(ctx context.Context, mutationID string, kind MutationKind, from, to Area, selection []batchSelection, copySource bool) (Outcome, error) {
	deleteOnly := kind == MutationDelete
	if mutationID == "" || len(selection) == 0 || !from.valid() || !deleteOnly && !to.valid() {
		return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid batch selection")
	}
	head, version, err := engine.load(ctx, "batch-trash")
	if err != nil {
		return Outcome{}, err
	}
	fingerprintBody, _ := encode(struct {
		ID        string           `json:"id"`
		Kind      MutationKind     `json:"kind"`
		From      Area             `json:"from"`
		To        Area             `json:"to,omitempty"`
		Selection []batchSelection `json:"selection"`
		Copy      bool             `json:"copy"`
	}{ID: mutationID, Kind: kind, From: from, To: to, Selection: selection, Copy: copySource})
	fingerprint := digest(fingerprintBody)
	claim := mutationClaim{SchemaVersion: 1, MutationID: mutationID, Fingerprint: fingerprint}
	claimBody, _ := encode(claim)
	claimKey := candidateKey("batch-namespace", engine.domainID, "claims/"+digest([]byte(mutationID))+".json")
	claimVersion, err := engine.backend.Put(trace(ctx, "batch-trash", "idempotency-claim", ""), claimKey, claimBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
	if err != nil {
		return Outcome{}, err
	}
	baseRef := head.LiveRef
	if from == AreaTrash {
		baseRef = head.TrashRef
	}
	baseValues, err := readImmutableTree(ctx, engine.baseTree, "batch-trash", "batch-base", baseRef)
	if err != nil {
		return Outcome{}, err
	}
	base := make(map[string]json.RawMessage, len(baseValues))
	for _, value := range baseValues {
		base[value.Key] = value.Value
	}
	edits := make([]treeValue, 0, len(selection)*2)
	seenSources := make(map[string]bool, len(selection))
	seenDestinations := make(map[string]bool, len(selection))
	for _, item := range selection {
		if item.Source == "" || seenSources[item.Source] || !deleteOnly && (item.Destination == "" || seenDestinations[item.Destination]) {
			return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid batch selection")
		}
		seenSources[item.Source] = true
		seenDestinations[item.Destination] = true
		body, found := base[item.Source]
		if !found {
			return Outcome{}, domain.NewError(domain.ErrorNotFound, "batch source does not exist")
		}
		var entry graphEntry
		if json.Unmarshal(body, &entry) != nil || validateGraphEntry(entry) != nil {
			return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid batch source entry")
		}
		if !copySource {
			tombstone, _ := encode(batchNamespaceValue{Tombstone: true})
			edits = append(edits, treeValue{Key: batchValueKey(from, item.Source), Value: tombstone})
		}
		if !deleteOnly {
			if copySource {
				entry.CloneSalt = digest([]byte(fingerprint + "\x00" + item.Source + "\x00" + item.Destination))
			}
			destination, _ := encode(batchNamespaceValue{Entry: entry})
			edits = append(edits, treeValue{Key: batchValueKey(to, item.Destination), Value: destination})
		}
	}
	sort.Slice(edits, func(left, right int) bool { return edits[left].Key < edits[right].Key })
	for index := 1; index < len(edits); index++ {
		if edits[index-1].Key == edits[index].Key {
			return Outcome{}, domain.NewError(domain.ErrorInvalid, "batch selection has overlapping changes")
		}
	}
	batchRef, err := buildImmutableTree(ctx, engine.batchTree, "batch-trash", "batch-delta", edits)
	if err != nil {
		return Outcome{}, err
	}
	head.Revision++
	outcome := Outcome{MutationID: mutationID, Fingerprint: fingerprint, Revision: head.Revision, Committed: true}
	head.BatchRef, head.Latest = batchRef, outcome
	body, _ := encode(head)
	if _, err := engine.backend.Put(trace(ctx, "batch-trash", "namespace-commit", ""), engine.headKey, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version}); err != nil {
		return Outcome{}, err
	}
	claim.Committed, claim.Outcome = true, outcome
	claimBody, _ = encode(claim)
	if _, err := engine.backend.Put(trace(ctx, "batch-trash", "idempotency-finalize", ""), claimKey, claimBody, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: claimVersion}); err != nil {
		return Outcome{}, err
	}
	return outcome, nil
}

func (engine *batchNamespace) Exists(ctx context.Context, area Area, name string) (bool, error) {
	if !area.valid() || name == "" {
		return false, domain.NewError(domain.ErrorInvalid, "invalid batch lookup")
	}
	head, _, err := engine.load(ctx, "batch-stat")
	if err != nil {
		return false, err
	}
	if head.BatchRef != "" {
		body, found, err := engine.batchTree.lookup(ctx, "batch-stat", "batch-delta", head.BatchRef, batchValueKey(area, name))
		if err != nil {
			return false, err
		}
		if found {
			var value batchNamespaceValue
			if json.Unmarshal(body, &value) != nil || value.Tombstone && value.Entry.NodeID != "" || !value.Tombstone && validateGraphEntry(value.Entry) != nil {
				return false, domain.NewError(domain.ErrorInvalid, "invalid batch delta value")
			}
			return !value.Tombstone, nil
		}
	}
	ref := head.LiveRef
	if area == AreaTrash {
		ref = head.TrashRef
	}
	_, found, err := engine.baseTree.lookup(ctx, "batch-stat", "batch-base", ref, name)
	return found, err
}

func batchValueKey(area Area, name string) string { return string(area) + "/" + name }

func buildImmutableTree(ctx context.Context, tree immutableTree, operation MutationKind, subsystem string, values []treeValue) (string, error) {
	if len(values) == 0 {
		return tree.empty(ctx, operation, subsystem)
	}
	for index, value := range values {
		if value.Key == "" || len(value.Value) == 0 || index > 0 && values[index-1].Key >= value.Key {
			return "", domain.NewError(domain.ErrorInvalid, "invalid bulk tree values")
		}
	}
	level := 0
	children := make([]treeChild, 0, (len(values)+tree.maximumPageItems()-1)/tree.maximumPageItems())
	for offset, index := 0, 0; offset < len(values); index++ {
		end := min(offset+tree.maximumPageItems(), len(values))
		page := treePage{SchemaVersion: 1, Level: 0, Values: append([]treeValue(nil), values[offset:end]...)}
		ref, child, err := tree.writePage(ctx, operation, subsystem, fmt.Sprintf("bulk-write-level-%d", level), page)
		if err != nil {
			return "", err
		}
		child.Ref = ref
		children = append(children, child)
		offset = end
	}
	for len(children) > 1 {
		level++
		next := make([]treeChild, 0, (len(children)+tree.maximumPageItems()-1)/tree.maximumPageItems())
		for offset := 0; offset < len(children); {
			end := min(offset+tree.maximumPageItems(), len(children))
			page := treePage{SchemaVersion: 1, Level: level, Children: append([]treeChild(nil), children[offset:end]...)}
			ref, child, err := tree.writePage(ctx, operation, subsystem, fmt.Sprintf("bulk-write-level-%d", level), page)
			if err != nil {
				return "", err
			}
			child.Ref = ref
			next = append(next, child)
			offset = end
		}
		children = next
	}
	return children[0].Ref, nil
}

func readImmutableTree(ctx context.Context, tree immutableTree, operation MutationKind, subsystem, root string) ([]treeValue, error) {
	values, _, err := readImmutableTreeWithRefs(ctx, tree, operation, subsystem, root)
	return values, err
}

func readImmutableTreeWithRefs(ctx context.Context, tree immutableTree, operation MutationKind, subsystem, root string) ([]treeValue, []string, error) {
	type queued struct {
		ref      string
		expected *treeChild
	}
	queue := []queued{{ref: root}}
	result := make([]treeValue, 0)
	refs := make([]string, 0)
	for level := 0; len(queue) > 0; level++ {
		next := make([]queued, 0)
		for _, item := range queue {
			page, err := tree.readPageGrouped(ctx, operation, subsystem, fmt.Sprintf("bulk-read-level-%d", level), item.ref)
			if err != nil {
				return nil, nil, err
			}
			refs = append(refs, item.ref)
			if item.expected != nil && verifyTreeChild(*item.expected, page, page.Level) != nil {
				return nil, nil, domain.NewError(domain.ErrorInvalid, "bulk tree descriptor mismatch")
			}
			if page.Level == 0 {
				result = append(result, page.Values...)
				continue
			}
			for index := range page.Children {
				child := page.Children[index]
				next = append(next, queued{ref: child.Ref, expected: &child})
			}
		}
		queue = next
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Key < result[right].Key })
	return result, refs, nil
}
