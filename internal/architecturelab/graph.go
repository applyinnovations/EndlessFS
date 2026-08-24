package architecturelab

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

type graphEntry struct {
	NodeID         string   `json:"nodeID"`
	Kind           NodeKind `json:"kind"`
	Size           int64    `json:"size"`
	FileCount      int64    `json:"fileCount"`
	ChildCount     int64    `json:"childCount,omitempty"`
	BlobIdentity   string   `json:"blobIdentity,omitempty"`
	ContentVersion string   `json:"contentVersion,omitempty"`
	DirectoryRef   string   `json:"directoryRef,omitempty"`
	CloneSalt      string   `json:"cloneSalt,omitempty"`
}

type graphDirectory struct {
	SchemaVersion int                   `json:"schemaVersion"`
	ID            string                `json:"id"`
	Size          int64                 `json:"size"`
	FileCount     int64                 `json:"fileCount"`
	Entries       map[string]graphEntry `json:"entries"`
}

type graphHead struct {
	SchemaVersion int                `json:"schemaVersion"`
	Revision      uint64             `json:"revision"`
	Frozen        bool               `json:"frozen"`
	LiveRef       string             `json:"liveRef"`
	TrashRef      string             `json:"trashRef"`
	Outcomes      map[string]Outcome `json:"outcomes"`
}

type graphEngine struct {
	backend  objectstore.Backend
	domainID string
	headKey  objectstore.Key
}

type graphFrame struct {
	ref        string
	directory  graphDirectory
	parentID   string
	parentName string
	depth      int
	area       Area
}

func openGraph(ctx context.Context, backend objectstore.Backend, options Options) (Engine, error) {
	if err := validateOptions(backend, options); err != nil {
		return nil, err
	}
	engine := &graphEngine{backend: backend, domainID: options.DomainID, headKey: candidateKey("graph", options.DomainID, "head.json")}
	live := graphDirectory{SchemaVersion: 1, ID: "live-root", Entries: map[string]graphEntry{}}
	trash := graphDirectory{SchemaVersion: 1, ID: "trash-root", Entries: map[string]graphEntry{}}
	liveRef, err := engine.writeDirectory(ctx, "initialize", "graph-initial", live)
	if err != nil {
		return nil, err
	}
	trashRef, err := engine.writeDirectory(ctx, "initialize", "graph-initial", trash)
	if err != nil {
		return nil, err
	}
	headBody, err := encode(graphHead{SchemaVersion: 1, Revision: 1, LiveRef: liveRef, TrashRef: trashRef, Outcomes: map[string]Outcome{}})
	if err != nil {
		return nil, err
	}
	if _, err := backend.Put(ctx, engine.headKey, headBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil && !errors.Is(err, domain.ErrConflict) {
		return nil, err
	}
	if _, _, err := engine.loadHead(ctx, "initialize"); err != nil {
		return nil, err
	}
	return engine, nil
}

func (engine *graphEngine) Name() string { return "immutable-directory-graph" }

func (engine *graphEngine) loadHead(ctx context.Context, operation MutationKind) (graphHead, objectstore.NativeVersion, error) {
	object, err := engine.backend.Get(trace(ctx, operation, "graph-head", ""), engine.headKey)
	if err != nil {
		return graphHead{}, "", err
	}
	var head graphHead
	if err := decode(object.Body, &head); err != nil || head.SchemaVersion != 1 || head.Revision == 0 || head.LiveRef == "" || head.TrashRef == "" || head.Outcomes == nil {
		return graphHead{}, "", domain.NewError(domain.ErrorInvalid, "invalid graph head")
	}
	return head, object.Version, nil
}

func (engine *graphEngine) readDirectory(ctx context.Context, operation MutationKind, ref string, cache map[string]graphDirectory) (graphDirectory, error) {
	key, err := objectstore.ParseKey(ref)
	if err != nil {
		return graphDirectory{}, err
	}
	if directory, ok := cache[ref]; ok {
		return cloneGraphDirectory(directory), nil
	}
	object, err := engine.backend.Get(trace(ctx, operation, "graph-directory", ""), key)
	if err != nil {
		return graphDirectory{}, err
	}
	if digest(object.Body) != keyDigest(key) {
		return graphDirectory{}, domain.NewError(domain.ErrorInvalid, "graph directory digest mismatch")
	}
	var directory graphDirectory
	if err := decode(object.Body, &directory); err != nil || directory.SchemaVersion != 1 || directory.ID == "" || directory.Entries == nil {
		return graphDirectory{}, domain.NewError(domain.ErrorInvalid, "invalid graph directory")
	}
	if err := validateGraphDirectory(directory); err != nil {
		return graphDirectory{}, err
	}
	cache[ref] = directory
	return cloneGraphDirectory(directory), nil
}

func (engine *graphEngine) resolveTrail(ctx context.Context, operation MutationKind, head graphHead, area Area, path domain.UserPath, cache map[string]graphDirectory) ([]graphFrame, error) {
	ref := head.LiveRef
	if area == AreaTrash {
		ref = head.TrashRef
	}
	directory, err := engine.readDirectory(ctx, operation, ref, cache)
	if err != nil {
		return nil, err
	}
	trail := []graphFrame{{ref: ref, directory: directory, depth: 0, area: area}}
	for _, segment := range path.Segments() {
		entry, ok := directory.Entries[segment]
		if !ok {
			return nil, domain.NewError(domain.ErrorNotFound, "graph directory does not exist")
		}
		if entry.Kind != NodeDirectory || entry.DirectoryRef == "" {
			return nil, domain.NewError(domain.ErrorInvalid, "graph path is not a directory")
		}
		directory, err = engine.readDirectory(ctx, operation, entry.DirectoryRef, cache)
		if err != nil {
			return nil, err
		}
		trail = append(trail, graphFrame{ref: entry.DirectoryRef, directory: directory, parentID: trail[len(trail)-1].directory.ID, parentName: segment, depth: len(trail), area: area})
	}
	return trail, nil
}

func (engine *graphEngine) Mutate(ctx context.Context, mutation Mutation) (Outcome, error) {
	fingerprint, err := mutationFingerprint(mutation)
	if err != nil {
		return Outcome{}, err
	}
	head, version, err := engine.loadHead(ctx, mutation.Kind)
	if err != nil {
		return Outcome{}, err
	}
	if existing, ok := head.Outcomes[mutation.ID]; ok {
		if existing.Fingerprint != fingerprint {
			return Outcome{}, domain.NewError(domain.ErrorConflict, "idempotency key was reused for another mutation")
		}
		existing.Replayed = true
		return existing, nil
	}
	if head.Frozen {
		return Outcome{}, domain.NewError(domain.ErrorUnavailable, "consistency domain is frozen")
	}
	cache := make(map[string]graphDirectory)
	changes := make(map[string]graphDirectory)
	frames := make(map[string]graphFrame)
	addTrail := func(trail []graphFrame) {
		for _, frame := range trail {
			frames[frame.directory.ID] = frame
		}
	}
	switch mutation.Kind {
	case MutationCreateDirectory, MutationCreateFile:
		path, parseErr := domain.ParseUserPath(mutation.Destination)
		if parseErr != nil || path.IsRoot() || !mutation.ToArea.valid() || mutation.NodeID == "" {
			return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid graph create mutation")
		}
		trail, err := engine.resolveTrail(ctx, mutation.Kind, head, mutation.ToArea, path.Parent(), cache)
		if err != nil {
			return Outcome{}, err
		}
		addTrail(trail)
		parent := cloneGraphDirectory(trail[len(trail)-1].directory)
		if _, exists := parent.Entries[path.Name()]; exists {
			return Outcome{}, domain.NewError(domain.ErrorConflict, "destination already exists")
		}
		entry := graphEntry{NodeID: mutation.NodeID, Size: mutation.Size, ContentVersion: fingerprint}
		if mutation.Kind == MutationCreateDirectory {
			entry.Kind = NodeDirectory
			directory := graphDirectory{SchemaVersion: 1, ID: mutation.NodeID, Entries: map[string]graphEntry{}}
			ref, err := engine.writeDirectory(trace(ctx, mutation.Kind, "graph-preparation", "prepare-leaf"), mutation.Kind, "graph-preparation", directory)
			if err != nil {
				return Outcome{}, err
			}
			entry.DirectoryRef = ref
		} else {
			if mutation.Size < 0 || mutation.BlobIdentity == "" {
				return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid graph file mutation")
			}
			entry.Kind, entry.FileCount, entry.BlobIdentity = NodeFile, 1, mutation.BlobIdentity
		}
		parent.Entries[path.Name()] = entry
		changes[parent.ID] = parent
	case MutationMove:
		source, parseErr := domain.ParseUserPath(mutation.Source)
		if parseErr != nil || source.IsRoot() || !mutation.FromArea.valid() || !mutation.ToArea.valid() {
			return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid graph move mutation")
		}
		destination, parseErr := domain.ParseUserPath(mutation.Destination)
		if parseErr != nil || destination.IsRoot() || mutation.FromArea == mutation.ToArea && destination.IsDescendantOf(source) {
			return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid graph move destination")
		}
		sourceTrail, err := engine.resolveTrail(ctx, mutation.Kind, head, mutation.FromArea, source.Parent(), cache)
		if err != nil {
			return Outcome{}, err
		}
		destinationTrail, err := engine.resolveTrail(ctx, mutation.Kind, head, mutation.ToArea, destination.Parent(), cache)
		if err != nil {
			return Outcome{}, err
		}
		addTrail(sourceTrail)
		addTrail(destinationTrail)
		sourceParent := cloneGraphDirectory(sourceTrail[len(sourceTrail)-1].directory)
		destinationParent := cloneGraphDirectory(destinationTrail[len(destinationTrail)-1].directory)
		entry, exists := sourceParent.Entries[source.Name()]
		if !exists {
			return Outcome{}, domain.NewError(domain.ErrorNotFound, "source does not exist")
		}
		if _, exists := destinationParent.Entries[destination.Name()]; exists {
			return Outcome{}, domain.NewError(domain.ErrorConflict, "destination already exists")
		}
		delete(sourceParent.Entries, source.Name())
		if sourceParent.ID == destinationParent.ID {
			sourceParent.Entries[destination.Name()] = entry
			changes[sourceParent.ID] = sourceParent
		} else {
			destinationParent.Entries[destination.Name()] = entry
			changes[sourceParent.ID], changes[destinationParent.ID] = sourceParent, destinationParent
		}
	case MutationDelete:
		path, parseErr := domain.ParseUserPath(mutation.Source)
		if parseErr != nil || path.IsRoot() || !mutation.FromArea.valid() {
			return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid graph delete mutation")
		}
		trail, err := engine.resolveTrail(ctx, mutation.Kind, head, mutation.FromArea, path.Parent(), cache)
		if err != nil {
			return Outcome{}, err
		}
		addTrail(trail)
		parent := cloneGraphDirectory(trail[len(trail)-1].directory)
		if _, exists := parent.Entries[path.Name()]; !exists {
			return Outcome{}, domain.NewError(domain.ErrorNotFound, "source does not exist")
		}
		delete(parent.Entries, path.Name())
		changes[parent.ID] = parent
	default:
		return Outcome{}, domain.NewError(domain.ErrorInvalid, "unsupported graph mutation")
	}
	newRefs, err := engine.writeChangedGraph(ctx, mutation.Kind, changes, frames)
	if err != nil {
		return Outcome{}, err
	}
	if ref, ok := newRefs["live-root"]; ok {
		head.LiveRef = ref
	}
	if ref, ok := newRefs["trash-root"]; ok {
		head.TrashRef = ref
	}
	head.Revision++
	outcome := Outcome{MutationID: mutation.ID, Fingerprint: fingerprint, Revision: head.Revision, Committed: true}
	head.Outcomes[mutation.ID] = outcome
	headBody, err := encode(head)
	if err != nil {
		return Outcome{}, err
	}
	if _, err := engine.backend.Put(trace(ctx, mutation.Kind, "namespace-commit", ""), engine.headKey, headBody, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version}); err != nil {
		return Outcome{}, err
	}
	return outcome, nil
}

func (engine *graphEngine) writeChangedGraph(ctx context.Context, operation MutationKind, changes map[string]graphDirectory, frames map[string]graphFrame) (map[string]string, error) {
	dirty := make(map[string]bool)
	maxDepth := 0
	for id := range changes {
		for current := id; current != ""; {
			dirty[current] = true
			frame, ok := frames[current]
			if !ok {
				break
			}
			if frame.depth > maxDepth {
				maxDepth = frame.depth
			}
			current = frame.parentID
		}
	}
	refs := make(map[string]string)
	for depth := maxDepth; depth >= 0; depth-- {
		ids := make([]string, 0)
		for id := range dirty {
			if frame, ok := frames[id]; ok && frame.depth == depth {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		type prepared struct {
			id   string
			key  objectstore.Key
			body []byte
		}
		preparedLevel := make([]prepared, 0, len(ids))
		for _, id := range ids {
			directory, ok := changes[id]
			if !ok {
				directory = cloneGraphDirectory(frames[id].directory)
			}
			recalculateGraphDirectory(&directory)
			body, err := encode(directory)
			if err != nil {
				return nil, err
			}
			key := candidateKey("graph", engine.domainID, fmt.Sprintf("directories/%s.json", digest(body)))
			refs[id] = key.String()
			preparedLevel = append(preparedLevel, prepared{id: id, key: key, body: body})
			frame := frames[id]
			if frame.parentID != "" {
				parent, ok := changes[frame.parentID]
				if !ok {
					parent = cloneGraphDirectory(frames[frame.parentID].directory)
				}
				entry := parent.Entries[frame.parentName]
				entry.DirectoryRef, entry.Size, entry.FileCount = key.String(), directory.Size, directory.FileCount
				parent.Entries[frame.parentName] = entry
				changes[parent.ID] = parent
			}
		}
		var wait sync.WaitGroup
		var firstErr error
		var errorMu sync.Mutex
		for _, item := range preparedLevel {
			item := item
			wait.Add(1)
			go func() {
				defer wait.Done()
				parallel := fmt.Sprintf("graph-depth-%d", depth)
				if err := createImmutable(trace(ctx, operation, "graph-preparation", parallel), engine.backend, item.key, item.body); err != nil {
					errorMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errorMu.Unlock()
				}
			}()
		}
		wait.Wait()
		if firstErr != nil {
			return nil, firstErr
		}
	}
	return refs, nil
}

func (engine *graphEngine) writeDirectory(ctx context.Context, operation MutationKind, subsystem string, directory graphDirectory) (string, error) {
	recalculateGraphDirectory(&directory)
	body, err := encode(directory)
	if err != nil {
		return "", err
	}
	key := candidateKey("graph", engine.domainID, fmt.Sprintf("directories/%s.json", digest(body)))
	if err := createImmutable(trace(ctx, operation, subsystem, ""), engine.backend, key, body); err != nil {
		return "", err
	}
	return key.String(), nil
}

func (engine *graphEngine) Snapshot(ctx context.Context) (Snapshot, error) {
	head, _, err := engine.loadHead(ctx, "snapshot")
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{SchemaVersion: 1, Revision: head.Revision, Frozen: head.Frozen, LiveRootID: "live-root", TrashRootID: "trash-root", Nodes: map[string]Node{}, Outcomes: map[string]Outcome{}}
	for id, outcome := range head.Outcomes {
		snapshot.Outcomes[id] = outcome
	}
	cache := make(map[string]graphDirectory)
	var visit func(string) error
	visit = func(ref string) error {
		directory, err := engine.readDirectory(ctx, "snapshot", ref, cache)
		if err != nil {
			return err
		}
		if _, exists := snapshot.Nodes[directory.ID]; exists {
			return nil
		}
		node := Node{ID: directory.ID, Kind: NodeDirectory, Size: directory.Size, FileCount: directory.FileCount, Children: map[string]string{}}
		for name, entry := range directory.Entries {
			node.Children[name] = entry.NodeID
			if entry.Kind == NodeFile {
				snapshot.Nodes[entry.NodeID] = Node{ID: entry.NodeID, Kind: NodeFile, Size: entry.Size, FileCount: 1, BlobIdentity: entry.BlobIdentity, ContentVersion: entry.ContentVersion}
			} else if err := visit(entry.DirectoryRef); err != nil {
				return err
			}
		}
		snapshot.Nodes[directory.ID] = node
		return nil
	}
	if err := visit(head.LiveRef); err != nil {
		return Snapshot{}, err
	}
	if err := visit(head.TrashRef); err != nil {
		return Snapshot{}, err
	}
	return snapshot, validateSnapshot(snapshot)
}

func (engine *graphEngine) Freeze(ctx context.Context, checkpointID string) (Checkpoint, error) {
	if checkpointID == "" {
		return Checkpoint{}, domain.NewError(domain.ErrorInvalid, "checkpoint identity is required")
	}
	head, version, err := engine.loadHead(ctx, "checkpoint")
	if err != nil {
		return Checkpoint{}, err
	}
	if !head.Frozen {
		head.Frozen = true
		body, err := encode(head)
		if err != nil {
			return Checkpoint{}, err
		}
		if _, err := engine.backend.Put(checkpointTrace(ctx, "freeze-commit"), engine.headKey, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version}); err != nil {
			return Checkpoint{}, err
		}
	}
	body, _ := encode(head)
	return Checkpoint{ID: checkpointID, Revision: head.Revision, Digest: digest(body)}, nil
}

func (engine *graphEngine) Compact(context.Context) error { return nil }

func validateGraphDirectory(directory graphDirectory) error {
	var size, count int64
	for name, entry := range directory.Entries {
		if name == "" || entry.NodeID == "" || entry.Size < 0 {
			return domain.NewError(domain.ErrorInvalid, "invalid graph directory entry")
		}
		if entry.Kind == NodeFile {
			if entry.FileCount != 1 || entry.BlobIdentity == "" || entry.DirectoryRef != "" {
				return domain.NewError(domain.ErrorInvalid, "invalid graph file entry")
			}
		} else if entry.Kind != NodeDirectory || entry.DirectoryRef == "" || entry.BlobIdentity != "" {
			return domain.NewError(domain.ErrorInvalid, "invalid graph directory entry")
		}
		size += entry.Size
		count += entry.FileCount
	}
	if size != directory.Size || count != directory.FileCount {
		return domain.NewError(domain.ErrorInvalid, "graph directory aggregate mismatch")
	}
	return nil
}

func recalculateGraphDirectory(directory *graphDirectory) {
	directory.Size, directory.FileCount = 0, 0
	if directory.Entries == nil {
		directory.Entries = map[string]graphEntry{}
	}
	for _, entry := range directory.Entries {
		directory.Size += entry.Size
		directory.FileCount += entry.FileCount
	}
}

func cloneGraphDirectory(source graphDirectory) graphDirectory {
	result := source
	result.Entries = make(map[string]graphEntry, len(source.Entries))
	for name, entry := range source.Entries {
		result.Entries[name] = entry
	}
	return result
}

func keyDigest(key objectstore.Key) string {
	value := key.String()
	start := len(value) - len(".json") - 64
	if start < 0 || value[len(value)-5:] != ".json" {
		return ""
	}
	return value[start : len(value)-5]
}
