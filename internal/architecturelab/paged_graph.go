package architecturelab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

const (
	maxRecentOutcomes = 16
	flushOutcomes     = 8
)

type pagedDirectory struct {
	SchemaVersion int    `json:"schemaVersion"`
	ID            string `json:"id"`
	IndexRef      string `json:"indexRef"`
	Size          int64  `json:"size"`
	FileCount     int64  `json:"fileCount"`
}

type pagedHead struct {
	SchemaVersion int       `json:"schemaVersion"`
	Revision      uint64    `json:"revision"`
	Frozen        bool      `json:"frozen"`
	LiveRef       string    `json:"liveRef"`
	TrashRef      string    `json:"trashRef"`
	OutcomeRef    string    `json:"outcomeRef"`
	OutcomeBloom  string    `json:"outcomeBloom"`
	Recent        []Outcome `json:"recent"`
}

type pagedGraphEngine struct {
	backend  objectstore.Backend
	domainID string
	headKey  objectstore.Key
	tree     immutableTree
}

type pagedFrame struct {
	ref        string
	directory  pagedDirectory
	parentID   string
	parentName string
	depth      int
	area       Area
}

func openPagedGraph(ctx context.Context, backend objectstore.Backend, options Options) (Engine, error) {
	if err := validateOptions(backend, options); err != nil {
		return nil, err
	}
	engine := &pagedGraphEngine{backend: backend, domainID: options.DomainID, headKey: candidateKey("paged", options.DomainID, "head.json"), tree: immutableTree{backend: backend, domainID: options.DomainID}}
	emptyRef, err := engine.tree.empty(ctx, "initialize", "paged-tree-initial")
	if err != nil {
		return nil, err
	}
	liveRef, err := engine.writeDirectory(ctx, "initialize", "paged-directory-initial", pagedDirectory{SchemaVersion: 1, ID: "live-root", IndexRef: emptyRef})
	if err != nil {
		return nil, err
	}
	trashRef, err := engine.writeDirectory(ctx, "initialize", "paged-directory-initial", pagedDirectory{SchemaVersion: 1, ID: "trash-root", IndexRef: emptyRef})
	if err != nil {
		return nil, err
	}
	head := pagedHead{SchemaVersion: 1, Revision: 1, LiveRef: liveRef, TrashRef: trashRef, OutcomeRef: emptyRef, OutcomeBloom: zeroBloom(), Recent: []Outcome{}}
	body, err := encode(head)
	if err != nil {
		return nil, err
	}
	if _, err := backend.Put(ctx, engine.headKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil && !errors.Is(err, domain.ErrConflict) {
		return nil, err
	}
	if _, _, err := engine.loadHead(ctx, "initialize"); err != nil {
		return nil, err
	}
	return engine, nil
}

func (engine *pagedGraphEngine) Name() string { return "paged-directory-graph" }

func (engine *pagedGraphEngine) loadHead(ctx context.Context, operation MutationKind) (pagedHead, objectstore.NativeVersion, error) {
	object, err := engine.backend.Get(trace(ctx, operation, "paged-head", ""), engine.headKey)
	if err != nil {
		return pagedHead{}, "", err
	}
	var head pagedHead
	if err := decode(object.Body, &head); err != nil || validatePagedHead(head) != nil {
		return pagedHead{}, "", domain.NewError(domain.ErrorInvalid, "invalid paged head")
	}
	return head, object.Version, nil
}

func (engine *pagedGraphEngine) readDirectory(ctx context.Context, operation MutationKind, ref string, cache map[string]pagedDirectory) (pagedDirectory, error) {
	if directory, ok := cache[ref]; ok {
		return directory, nil
	}
	key, err := objectstore.ParseKey(ref)
	if err != nil {
		return pagedDirectory{}, err
	}
	object, err := engine.backend.Get(trace(ctx, operation, "paged-directory", ""), key)
	if err != nil {
		return pagedDirectory{}, err
	}
	if digest(object.Body) != keyDigest(key) {
		return pagedDirectory{}, domain.NewError(domain.ErrorInvalid, "paged directory digest mismatch")
	}
	var directory pagedDirectory
	if err := decode(object.Body, &directory); err != nil || directory.SchemaVersion != 1 || directory.ID == "" || directory.IndexRef == "" || directory.Size < 0 || directory.FileCount < 0 {
		return pagedDirectory{}, domain.NewError(domain.ErrorInvalid, "invalid paged directory")
	}
	cache[ref] = directory
	return directory, nil
}

func (engine *pagedGraphEngine) resolveTrail(ctx context.Context, operation MutationKind, head pagedHead, area Area, path domain.UserPath, cache map[string]pagedDirectory) ([]pagedFrame, error) {
	ref := head.LiveRef
	if area == AreaTrash {
		ref = head.TrashRef
	}
	directory, err := engine.readDirectory(ctx, operation, ref, cache)
	if err != nil {
		return nil, err
	}
	trail := []pagedFrame{{ref: ref, directory: directory, depth: 0, area: area}}
	for _, segment := range path.Segments() {
		entry, found, err := engine.lookupEntry(ctx, operation, directory.IndexRef, segment)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, domain.NewError(domain.ErrorNotFound, "paged directory does not exist")
		}
		if entry.Kind != NodeDirectory || entry.DirectoryRef == "" {
			return nil, domain.NewError(domain.ErrorInvalid, "paged path is not a directory")
		}
		directory, err = engine.readDirectory(ctx, operation, entry.DirectoryRef, cache)
		if err != nil {
			return nil, err
		}
		trail = append(trail, pagedFrame{ref: entry.DirectoryRef, directory: directory, parentID: trail[len(trail)-1].directory.ID, parentName: segment, depth: len(trail), area: area})
	}
	return trail, nil
}

func (engine *pagedGraphEngine) lookupEntry(ctx context.Context, operation MutationKind, indexRef, name string) (graphEntry, bool, error) {
	body, found, err := engine.tree.lookup(ctx, operation, "paged-directory-index", indexRef, name)
	if err != nil || !found {
		return graphEntry{}, found, err
	}
	var entry graphEntry
	if err := json.Unmarshal(body, &entry); err != nil || entry.NodeID == "" || entry.Size < 0 {
		return graphEntry{}, false, domain.NewError(domain.ErrorInvalid, "invalid paged directory entry")
	}
	return entry, true, nil
}

func (engine *pagedGraphEngine) Mutate(ctx context.Context, mutation Mutation) (Outcome, error) {
	fingerprint, err := mutationFingerprint(mutation)
	if err != nil {
		return Outcome{}, err
	}
	head, version, err := engine.loadHead(ctx, mutation.Kind)
	if err != nil {
		return Outcome{}, err
	}
	if outcome, found, err := engine.lookupOutcome(ctx, mutation.Kind, head, mutation.ID); err != nil {
		return Outcome{}, err
	} else if found {
		if outcome.Fingerprint != fingerprint {
			return Outcome{}, domain.NewError(domain.ErrorConflict, "idempotency key was reused for another mutation")
		}
		outcome.Replayed = true
		return outcome, nil
	}
	if head.Frozen {
		return Outcome{}, domain.NewError(domain.ErrorUnavailable, "consistency domain is frozen")
	}
	cache := make(map[string]pagedDirectory)
	changes := make(map[string]pagedDirectory)
	frames := make(map[string]pagedFrame)
	addTrail := func(trail []pagedFrame) {
		for _, frame := range trail {
			frames[frame.directory.ID] = frame
		}
	}
	switch mutation.Kind {
	case MutationCreateDirectory, MutationCreateFile:
		path, parseErr := domain.ParseUserPath(mutation.Destination)
		if parseErr != nil || path.IsRoot() || !mutation.ToArea.valid() || mutation.NodeID == "" {
			return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid paged create mutation")
		}
		trail, err := engine.resolveTrail(ctx, mutation.Kind, head, mutation.ToArea, path.Parent(), cache)
		if err != nil {
			return Outcome{}, err
		}
		addTrail(trail)
		parent := trail[len(trail)-1].directory
		if _, found, err := engine.lookupEntry(ctx, mutation.Kind, parent.IndexRef, path.Name()); err != nil {
			return Outcome{}, err
		} else if found {
			return Outcome{}, domain.NewError(domain.ErrorConflict, "destination already exists")
		}
		entry := graphEntry{NodeID: mutation.NodeID, Size: mutation.Size, ContentVersion: fingerprint}
		if mutation.Kind == MutationCreateDirectory {
			entry.Kind = NodeDirectory
			emptyRef := engine.tree.emptyReference()
			entry.DirectoryRef, err = engine.writeDirectory(ctx, mutation.Kind, "paged-directory-preparation", pagedDirectory{SchemaVersion: 1, ID: mutation.NodeID, IndexRef: emptyRef})
			if err != nil {
				return Outcome{}, err
			}
		} else {
			if mutation.Size < 0 || mutation.BlobIdentity == "" {
				return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid paged file mutation")
			}
			entry.Kind, entry.FileCount, entry.BlobIdentity = NodeFile, 1, mutation.BlobIdentity
		}
		parent.IndexRef, _, err = engine.tree.upsert(ctx, mutation.Kind, "paged-directory-index", parent.IndexRef, path.Name(), entry)
		if err != nil {
			return Outcome{}, err
		}
		parent.Size += entry.Size
		parent.FileCount += entry.FileCount
		changes[parent.ID] = parent
	case MutationMove:
		source, parseErr := domain.ParseUserPath(mutation.Source)
		if parseErr != nil || source.IsRoot() || !mutation.FromArea.valid() || !mutation.ToArea.valid() {
			return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid paged move mutation")
		}
		destination, parseErr := domain.ParseUserPath(mutation.Destination)
		if parseErr != nil || destination.IsRoot() || mutation.FromArea == mutation.ToArea && destination.IsDescendantOf(source) {
			return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid paged move destination")
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
		sourceParent := sourceTrail[len(sourceTrail)-1].directory
		destinationParent := destinationTrail[len(destinationTrail)-1].directory
		entry, found, err := engine.lookupEntry(ctx, mutation.Kind, sourceParent.IndexRef, source.Name())
		if err != nil {
			return Outcome{}, err
		}
		if !found {
			return Outcome{}, domain.NewError(domain.ErrorNotFound, "source does not exist")
		}
		if _, found, err := engine.lookupEntry(ctx, mutation.Kind, destinationParent.IndexRef, destination.Name()); err != nil {
			return Outcome{}, err
		} else if found {
			return Outcome{}, domain.NewError(domain.ErrorConflict, "destination already exists")
		}
		sourceParent.IndexRef, _, err = engine.tree.remove(ctx, mutation.Kind, "paged-directory-index", sourceParent.IndexRef, source.Name())
		if err != nil {
			return Outcome{}, err
		}
		sourceParent.Size -= entry.Size
		sourceParent.FileCount -= entry.FileCount
		if sourceParent.ID == destinationParent.ID {
			sourceParent.IndexRef, _, err = engine.tree.upsert(ctx, mutation.Kind, "paged-directory-index", sourceParent.IndexRef, destination.Name(), entry)
			sourceParent.Size += entry.Size
			sourceParent.FileCount += entry.FileCount
			if err != nil {
				return Outcome{}, err
			}
			changes[sourceParent.ID] = sourceParent
		} else {
			destinationParent.IndexRef, _, err = engine.tree.upsert(ctx, mutation.Kind, "paged-directory-index", destinationParent.IndexRef, destination.Name(), entry)
			if err != nil {
				return Outcome{}, err
			}
			destinationParent.Size += entry.Size
			destinationParent.FileCount += entry.FileCount
			changes[sourceParent.ID], changes[destinationParent.ID] = sourceParent, destinationParent
		}
	case MutationDelete:
		path, parseErr := domain.ParseUserPath(mutation.Source)
		if parseErr != nil || path.IsRoot() || !mutation.FromArea.valid() {
			return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid paged delete mutation")
		}
		trail, err := engine.resolveTrail(ctx, mutation.Kind, head, mutation.FromArea, path.Parent(), cache)
		if err != nil {
			return Outcome{}, err
		}
		addTrail(trail)
		parent := trail[len(trail)-1].directory
		entry, found, err := engine.lookupEntry(ctx, mutation.Kind, parent.IndexRef, path.Name())
		if err != nil {
			return Outcome{}, err
		}
		if !found {
			return Outcome{}, domain.NewError(domain.ErrorNotFound, "source does not exist")
		}
		parent.IndexRef, _, err = engine.tree.remove(ctx, mutation.Kind, "paged-directory-index", parent.IndexRef, path.Name())
		if err != nil {
			return Outcome{}, err
		}
		parent.Size -= entry.Size
		parent.FileCount -= entry.FileCount
		changes[parent.ID] = parent
	default:
		return Outcome{}, domain.NewError(domain.ErrorInvalid, "unsupported paged mutation")
	}
	newRefs, err := engine.writeChangedDirectories(ctx, mutation.Kind, changes, frames)
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
	if err := engine.addOutcome(ctx, mutation.Kind, &head, outcome); err != nil {
		return Outcome{}, err
	}
	body, err := encode(head)
	if err != nil {
		return Outcome{}, err
	}
	if _, err := engine.backend.Put(trace(ctx, mutation.Kind, "namespace-commit", ""), engine.headKey, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version}); err != nil {
		return Outcome{}, err
	}
	return outcome, nil
}

func (engine *pagedGraphEngine) writeChangedDirectories(ctx context.Context, operation MutationKind, changes map[string]pagedDirectory, frames map[string]pagedFrame) (map[string]string, error) {
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
		for _, id := range ids {
			directory, ok := changes[id]
			if !ok {
				directory = frames[id].directory
			}
			ref, err := engine.writeDirectory(ctx, operation, "paged-directory-preparation", directory)
			if err != nil {
				return nil, err
			}
			refs[id] = ref
			frame := frames[id]
			if frame.parentID != "" {
				parent, ok := changes[frame.parentID]
				if !ok {
					parent = frames[frame.parentID].directory
				}
				entry, found, err := engine.lookupEntry(ctx, operation, parent.IndexRef, frame.parentName)
				if err != nil || !found {
					return nil, domain.NewError(domain.ErrorInvalid, "paged parent entry is missing")
				}
				parent.Size += directory.Size - entry.Size
				parent.FileCount += directory.FileCount - entry.FileCount
				entry.DirectoryRef, entry.Size, entry.FileCount = ref, directory.Size, directory.FileCount
				parent.IndexRef, _, err = engine.tree.upsert(ctx, operation, "paged-directory-index", parent.IndexRef, frame.parentName, entry)
				if err != nil {
					return nil, err
				}
				changes[parent.ID] = parent
			}
		}
	}
	return refs, nil
}

func (engine *pagedGraphEngine) writeDirectory(ctx context.Context, operation MutationKind, subsystem string, directory pagedDirectory) (string, error) {
	body, err := encode(directory)
	if err != nil {
		return "", err
	}
	key := candidateKey("paged", engine.domainID, "directories/"+digest(body)+".json")
	if err := createImmutable(trace(ctx, operation, subsystem, ""), engine.backend, key, body); err != nil {
		return "", err
	}
	return key.String(), nil
}

func (engine *pagedGraphEngine) lookupOutcome(ctx context.Context, operation MutationKind, head pagedHead, mutationID string) (Outcome, bool, error) {
	for index := len(head.Recent) - 1; index >= 0; index-- {
		if head.Recent[index].MutationID == mutationID {
			return head.Recent[index], true, nil
		}
	}
	if !bloomMaybe(head.OutcomeBloom, mutationID) {
		return Outcome{}, false, nil
	}
	body, found, err := engine.tree.lookup(ctx, operation, "paged-outcome-index", head.OutcomeRef, mutationID)
	if err != nil || !found {
		return Outcome{}, found, err
	}
	var outcome Outcome
	if err := json.Unmarshal(body, &outcome); err != nil || outcome.MutationID != mutationID || !outcome.Committed {
		return Outcome{}, false, domain.NewError(domain.ErrorInvalid, "invalid paged outcome")
	}
	return outcome, true, nil
}

func (engine *pagedGraphEngine) addOutcome(ctx context.Context, operation MutationKind, head *pagedHead, outcome Outcome) error {
	if len(head.Recent) == maxRecentOutcomes {
		for _, old := range head.Recent[:flushOutcomes] {
			var err error
			head.OutcomeRef, _, err = engine.tree.upsert(ctx, operation, "paged-outcome-index", head.OutcomeRef, old.MutationID, old)
			if err != nil {
				return err
			}
		}
		head.Recent = append([]Outcome(nil), head.Recent[flushOutcomes:]...)
	}
	head.Recent = append(head.Recent, outcome)
	head.OutcomeBloom = bloomAdd(head.OutcomeBloom, outcome.MutationID)
	return nil
}

func (engine *pagedGraphEngine) Snapshot(ctx context.Context) (Snapshot, error) {
	head, _, err := engine.loadHead(ctx, "snapshot")
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{SchemaVersion: 1, Revision: head.Revision, Frozen: head.Frozen, LiveRootID: "live-root", TrashRootID: "trash-root", Nodes: map[string]Node{}, Outcomes: map[string]Outcome{}}
	if err := engine.tree.walk(ctx, "snapshot", "paged-outcome-index", head.OutcomeRef, func(_ string, body json.RawMessage) error {
		var outcome Outcome
		if err := json.Unmarshal(body, &outcome); err != nil {
			return err
		}
		snapshot.Outcomes[outcome.MutationID] = outcome
		return nil
	}); err != nil {
		return Snapshot{}, err
	}
	for _, outcome := range head.Recent {
		snapshot.Outcomes[outcome.MutationID] = outcome
	}
	cache := make(map[string]pagedDirectory)
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
		if err := engine.tree.walk(ctx, "snapshot", "paged-directory-index", directory.IndexRef, func(name string, body json.RawMessage) error {
			var entry graphEntry
			if err := json.Unmarshal(body, &entry); err != nil {
				return err
			}
			node.Children[name] = entry.NodeID
			if entry.Kind == NodeFile {
				snapshot.Nodes[entry.NodeID] = Node{ID: entry.NodeID, Kind: NodeFile, Size: entry.Size, FileCount: 1, BlobIdentity: entry.BlobIdentity, ContentVersion: entry.ContentVersion}
				return nil
			}
			return visit(entry.DirectoryRef)
		}); err != nil {
			return err
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

func (engine *pagedGraphEngine) Freeze(ctx context.Context, checkpointID string) (Checkpoint, error) {
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

func (engine *pagedGraphEngine) Compact(ctx context.Context) error {
	head, version, err := engine.loadHead(ctx, "compaction")
	if err != nil || len(head.Recent) == 0 {
		return err
	}
	for _, outcome := range head.Recent {
		head.OutcomeRef, _, err = engine.tree.upsert(ctx, "compaction", "paged-outcome-index", head.OutcomeRef, outcome.MutationID, outcome)
		if err != nil {
			return err
		}
	}
	head.Recent = []Outcome{}
	body, err := encode(head)
	if err != nil {
		return err
	}
	_, err = engine.backend.Put(trace(ctx, "compaction", "compaction-commit", ""), engine.headKey, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version})
	return err
}

func validatePagedHead(head pagedHead) error {
	if head.SchemaVersion != 1 || head.Revision == 0 || head.LiveRef == "" || head.TrashRef == "" || head.OutcomeRef == "" || len(head.Recent) > maxRecentOutcomes {
		return errors.New("invalid paged head")
	}
	decoded, err := hex.DecodeString(head.OutcomeBloom)
	if err != nil || len(decoded) != sha256.Size {
		return errors.New("invalid paged outcome bloom")
	}
	return nil
}

func zeroBloom() string { return hex.EncodeToString(make([]byte, sha256.Size)) }

func bloomAdd(encoded, value string) string {
	bits, _ := hex.DecodeString(encoded)
	if len(bits) != sha256.Size {
		bits = make([]byte, sha256.Size)
	}
	sum := sha256.Sum256([]byte(value))
	for index := 0; index < 4; index++ {
		position := int(sum[index])
		bits[position/8] |= 1 << uint(position%8)
	}
	return hex.EncodeToString(bits)
}

func bloomMaybe(encoded, value string) bool {
	bits, err := hex.DecodeString(encoded)
	if err != nil || len(bits) != sha256.Size {
		return true
	}
	sum := sha256.Sum256([]byte(value))
	for index := 0; index < 4; index++ {
		position := int(sum[index])
		if bits[position/8]&(1<<uint(position%8)) == 0 {
			return false
		}
	}
	return true
}
