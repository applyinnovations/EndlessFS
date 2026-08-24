package architecturelab

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

const (
	maxHybridDeltas    = 32
	maxHybridHeadBytes = 512 << 10
)

type hybridDirectoryChange struct {
	Directory  graphEntry             `json:"directory"`
	ParentID   string                 `json:"parentID,omitempty"`
	ParentName string                 `json:"parentName,omitempty"`
	Depth      int                    `json:"depth"`
	Entries    map[string]*graphEntry `json:"entries"`
}

type hybridDelta struct {
	Mutation    Mutation                         `json:"mutation"`
	Outcome     Outcome                          `json:"outcome"`
	Directories map[string]hybridDirectoryChange `json:"directories"`
}

type hybridHead struct {
	SchemaVersion int           `json:"schemaVersion"`
	Revision      uint64        `json:"revision"`
	Frozen        bool          `json:"frozen"`
	Live          graphEntry    `json:"live"`
	Trash         graphEntry    `json:"trash"`
	Deltas        []hybridDelta `json:"deltas"`
	Latest        Outcome       `json:"latest,omitempty"`
}

type hybridFrame struct {
	directory  graphEntry
	parentID   string
	parentName string
	depth      int
	area       Area
}

type hybridEngine struct {
	backend       objectstore.Backend
	domainID      string
	headKey       objectstore.Key
	tree          immutableTree
	deltaLimit    int
	headByteLimit int
}

func (engine *hybridEngine) maximumDeltas() int {
	if engine.deltaLimit > 0 {
		return engine.deltaLimit
	}
	return maxHybridDeltas
}

func (engine *hybridEngine) maximumHeadBytes() int {
	if engine.headByteLimit > 0 {
		return engine.headByteLimit
	}
	return maxHybridHeadBytes
}

func openHybrid(ctx context.Context, backend objectstore.Backend, options Options) (Engine, error) {
	if err := validateOptions(backend, options); err != nil {
		return nil, err
	}
	engine := &hybridEngine{
		backend: backend, domainID: options.DomainID,
		headKey: candidateKey("hybrid", options.DomainID, "head.json"),
		tree:    immutableTree{backend: backend, domainID: options.DomainID, candidate: "hybrid"},
	}
	emptyRef, err := engine.tree.empty(ctx, "initialize", "hybrid-tree-initial")
	if err != nil {
		return nil, err
	}
	head := hybridHead{
		SchemaVersion: 1, Revision: 1, Deltas: []hybridDelta{},
		Live:  graphEntry{NodeID: "live-root", Kind: NodeDirectory, DirectoryRef: emptyRef},
		Trash: graphEntry{NodeID: "trash-root", Kind: NodeDirectory, DirectoryRef: emptyRef},
	}
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

func (engine *hybridEngine) Name() string { return "paged-delta-hybrid" }

func (engine *hybridEngine) loadHead(ctx context.Context, operation MutationKind) (hybridHead, objectstore.NativeVersion, error) {
	object, err := engine.backend.Get(trace(ctx, operation, "hybrid-head", ""), engine.headKey)
	if err != nil {
		return hybridHead{}, "", err
	}
	if len(object.Body) > engine.maximumHeadBytes() {
		return hybridHead{}, "", domain.NewError(domain.ErrorInvalid, "hybrid head exceeds its prototype bound")
	}
	var head hybridHead
	if err := decode(object.Body, &head); err != nil || validateHybridHeadLimit(head, engine.maximumDeltas()) != nil {
		return hybridHead{}, "", domain.NewError(domain.ErrorInvalid, "invalid hybrid head")
	}
	return head, object.Version, nil
}

func (engine *hybridEngine) currentDirectory(head hybridHead, base graphEntry) graphEntry {
	for index := len(head.Deltas) - 1; index >= 0; index-- {
		if change, found := head.Deltas[index].Directories[base.NodeID]; found {
			return change.Directory
		}
	}
	return base
}

func (engine *hybridEngine) lookupEntry(ctx context.Context, operation MutationKind, session *treeSession, head hybridHead, directory graphEntry, name string) (graphEntry, bool, error) {
	for index := len(head.Deltas) - 1; index >= 0; index-- {
		if change, found := head.Deltas[index].Directories[directory.NodeID]; found {
			if entry, changed := change.Entries[name]; changed {
				if entry == nil {
					return graphEntry{}, false, nil
				}
				return *entry, true, nil
			}
		}
	}
	body, found, err := session.lookup(ctx, operation, "hybrid-base-index", directory.DirectoryRef, name)
	if err != nil || !found {
		return graphEntry{}, found, err
	}
	var entry graphEntry
	if json.Unmarshal(body, &entry) != nil || validateGraphEntry(entry) != nil {
		return graphEntry{}, false, domain.NewError(domain.ErrorInvalid, "invalid hybrid base entry")
	}
	return entry, true, nil
}

func (engine *hybridEngine) resolveTrail(ctx context.Context, operation MutationKind, session *treeSession, head hybridHead, area Area, path domain.UserPath) ([]hybridFrame, error) {
	directory := head.Live
	if area == AreaTrash {
		directory = head.Trash
	}
	directory = engine.currentDirectory(head, directory)
	trail := []hybridFrame{{directory: directory, area: area}}
	for _, segment := range path.Segments() {
		entry, found, err := engine.lookupEntry(ctx, operation, session, head, directory, segment)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, domain.NewError(domain.ErrorNotFound, "hybrid directory does not exist")
		}
		if entry.Kind != NodeDirectory {
			return nil, domain.NewError(domain.ErrorInvalid, "hybrid path is not a directory")
		}
		parent := directory
		directory = engine.currentDirectory(head, entry)
		trail = append(trail, hybridFrame{directory: directory, parentID: parent.NodeID, parentName: segment, depth: len(trail), area: area})
	}
	return trail, nil
}

func (engine *hybridEngine) Mutate(ctx context.Context, mutation Mutation) (Outcome, error) {
	for attempt := 0; attempt < 2; attempt++ {
		fingerprint, err := mutationFingerprint(mutation)
		if err != nil {
			return Outcome{}, err
		}
		head, version, err := engine.loadHead(ctx, mutation.Kind)
		if err != nil {
			return Outcome{}, err
		}
		if len(head.Deltas) == engine.maximumDeltas() {
			if err := engine.compactHead(ctx, head, version); err != nil {
				return Outcome{}, err
			}
			continue
		}
		claim, claimVersion, replay, err := engine.claimMutation(ctx, mutation.Kind, head, mutation.ID, fingerprint)
		if err != nil {
			return Outcome{}, err
		}
		if replay != nil {
			return *replay, nil
		}
		if head.Frozen {
			return Outcome{}, domain.NewError(domain.ErrorUnavailable, "consistency domain is frozen")
		}
		session := newTreeSession(engine.tree)
		changes := make(map[string]*hybridDirectoryChange)
		frames := make(map[string]hybridFrame)
		addTrail := func(trail []hybridFrame) {
			for _, frame := range trail {
				frames[frame.directory.NodeID] = frame
			}
		}
		changeFor := func(frame hybridFrame) *hybridDirectoryChange {
			if change := changes[frame.directory.NodeID]; change != nil {
				return change
			}
			change := &hybridDirectoryChange{Directory: frame.directory, ParentID: frame.parentID, ParentName: frame.parentName, Depth: frame.depth, Entries: map[string]*graphEntry{}}
			changes[frame.directory.NodeID] = change
			return change
		}

		switch mutation.Kind {
		case MutationCreateDirectory, MutationCreateFile:
			path, parseErr := domain.ParseUserPath(mutation.Destination)
			if parseErr != nil || path.IsRoot() || !mutation.ToArea.valid() || mutation.NodeID == "" {
				return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid hybrid create mutation")
			}
			trail, err := engine.resolveTrail(ctx, mutation.Kind, session, head, mutation.ToArea, path.Parent())
			if err != nil {
				return Outcome{}, err
			}
			addTrail(trail)
			parentFrame := trail[len(trail)-1]
			if _, found, err := engine.lookupEntry(ctx, mutation.Kind, session, head, parentFrame.directory, path.Name()); err != nil {
				return Outcome{}, err
			} else if found {
				return Outcome{}, domain.NewError(domain.ErrorConflict, "destination already exists")
			}
			entry := graphEntry{NodeID: mutation.NodeID, Size: mutation.Size, ContentVersion: fingerprint}
			if mutation.Kind == MutationCreateDirectory {
				entry.Kind, entry.DirectoryRef = NodeDirectory, engine.tree.emptyReference()
			} else {
				if mutation.Size < 0 || mutation.BlobIdentity == "" {
					return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid hybrid file mutation")
				}
				entry.Kind, entry.FileCount, entry.BlobIdentity = NodeFile, 1, mutation.BlobIdentity
			}
			change := changeFor(parentFrame)
			copy := entry
			change.Entries[path.Name()] = &copy
			change.Directory.Size += entry.Size
			change.Directory.FileCount += entry.FileCount
		case MutationMove:
			source, parseErr := domain.ParseUserPath(mutation.Source)
			if parseErr != nil || source.IsRoot() || !mutation.FromArea.valid() || !mutation.ToArea.valid() {
				return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid hybrid move mutation")
			}
			destination, parseErr := domain.ParseUserPath(mutation.Destination)
			if parseErr != nil || destination.IsRoot() || mutation.FromArea == mutation.ToArea && destination.IsDescendantOf(source) {
				return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid hybrid move destination")
			}
			sourceTrail, err := engine.resolveTrail(ctx, mutation.Kind, session, head, mutation.FromArea, source.Parent())
			if err != nil {
				return Outcome{}, err
			}
			destinationTrail, err := engine.resolveTrail(ctx, mutation.Kind, session, head, mutation.ToArea, destination.Parent())
			if err != nil {
				return Outcome{}, err
			}
			addTrail(sourceTrail)
			addTrail(destinationTrail)
			sourceFrame, destinationFrame := sourceTrail[len(sourceTrail)-1], destinationTrail[len(destinationTrail)-1]
			entry, found, err := engine.lookupEntry(ctx, mutation.Kind, session, head, sourceFrame.directory, source.Name())
			if err != nil {
				return Outcome{}, err
			}
			if !found {
				return Outcome{}, domain.NewError(domain.ErrorNotFound, "source does not exist")
			}
			if _, found, err := engine.lookupEntry(ctx, mutation.Kind, session, head, destinationFrame.directory, destination.Name()); err != nil {
				return Outcome{}, err
			} else if found {
				return Outcome{}, domain.NewError(domain.ErrorConflict, "destination already exists")
			}
			sourceChange := changeFor(sourceFrame)
			sourceChange.Entries[source.Name()] = nil
			copy := entry
			if sourceFrame.directory.NodeID == destinationFrame.directory.NodeID {
				sourceChange.Entries[destination.Name()] = &copy
			} else {
				destinationChange := changeFor(destinationFrame)
				destinationChange.Entries[destination.Name()] = &copy
				sourceChange.Directory.Size -= entry.Size
				sourceChange.Directory.FileCount -= entry.FileCount
				destinationChange.Directory.Size += entry.Size
				destinationChange.Directory.FileCount += entry.FileCount
			}
			// A directory change carries the parent relationship that compaction
			// uses to publish its rewritten page reference. Rebind that
			// relationship even when this move does not otherwise mutate the
			// directory itself; retaining its former parent can make compaction
			// publish the subtree below the path it just left.
			if entry.Kind == NodeDirectory {
				moved := engine.currentDirectory(head, entry)
				movedFrame := hybridFrame{
					directory:  moved,
					parentID:   destinationFrame.directory.NodeID,
					parentName: destination.Name(),
					depth:      destinationFrame.depth + 1,
					area:       mutation.ToArea,
				}
				frames[moved.NodeID] = movedFrame
				changeFor(movedFrame)
			}
		case MutationDelete:
			path, parseErr := domain.ParseUserPath(mutation.Source)
			if parseErr != nil || path.IsRoot() || !mutation.FromArea.valid() {
				return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid hybrid delete mutation")
			}
			trail, err := engine.resolveTrail(ctx, mutation.Kind, session, head, mutation.FromArea, path.Parent())
			if err != nil {
				return Outcome{}, err
			}
			addTrail(trail)
			parentFrame := trail[len(trail)-1]
			entry, found, err := engine.lookupEntry(ctx, mutation.Kind, session, head, parentFrame.directory, path.Name())
			if err != nil {
				return Outcome{}, err
			}
			if !found {
				return Outcome{}, domain.NewError(domain.ErrorNotFound, "source does not exist")
			}
			change := changeFor(parentFrame)
			change.Entries[path.Name()] = nil
			change.Directory.Size -= entry.Size
			change.Directory.FileCount -= entry.FileCount
		default:
			return Outcome{}, domain.NewError(domain.ErrorInvalid, "unsupported hybrid mutation")
		}
		engine.propagate(changes, frames, changeFor)
		head.Revision++
		outcome := Outcome{MutationID: mutation.ID, Fingerprint: fingerprint, Revision: head.Revision, Committed: true}
		delta := hybridDelta{Mutation: mutation, Outcome: outcome, Directories: make(map[string]hybridDirectoryChange, len(changes))}
		for id, change := range changes {
			delta.Directories[id] = *change
		}
		head.Deltas = append(head.Deltas, delta)
		head.Latest = outcome
		body, err := encode(head)
		if err != nil {
			return Outcome{}, err
		}
		if len(body) > engine.maximumHeadBytes() {
			return Outcome{}, domain.NewError(domain.ErrorUnavailable, "hybrid delta window requires compaction")
		}
		if _, err := engine.backend.Put(trace(ctx, mutation.Kind, "namespace-commit", ""), engine.headKey, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version}); err != nil {
			return Outcome{}, err
		}
		claim.Committed, claim.Outcome = true, outcome
		if err := engine.finalizeClaim(ctx, mutation.Kind, claim, claimVersion); err != nil {
			return Outcome{}, err
		}
		return outcome, nil
	}
	return Outcome{}, domain.NewError(domain.ErrorUnavailable, "hybrid compaction did not make progress")
}

func (engine *hybridEngine) propagate(changes map[string]*hybridDirectoryChange, frames map[string]hybridFrame, changeFor func(hybridFrame) *hybridDirectoryChange) {
	maxDepth := 0
	for id := range changes {
		if frame, found := frames[id]; found && frame.depth > maxDepth {
			maxDepth = frame.depth
		}
	}
	for depth := maxDepth; depth > 0; depth-- {
		ids := make([]string, 0)
		for id, change := range changes {
			if change.Depth == depth {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			child := changes[id]
			parentFrame := frames[child.ParentID]
			parent := changeFor(parentFrame)
			original := frames[id].directory
			copy := child.Directory
			parent.Entries[child.ParentName] = &copy
			parent.Directory.Size += child.Directory.Size - original.Size
			parent.Directory.FileCount += child.Directory.FileCount - original.FileCount
		}
	}
}

func (engine *hybridEngine) claimKey(mutationID string) objectstore.Key {
	return candidateKey("hybrid", engine.domainID, "claims/"+digest([]byte(mutationID))+".json")
}

func (engine *hybridEngine) claimMutation(ctx context.Context, operation MutationKind, head hybridHead, mutationID, fingerprint string) (mutationClaim, objectstore.NativeVersion, *Outcome, error) {
	claim := mutationClaim{SchemaVersion: 1, MutationID: mutationID, Fingerprint: fingerprint}
	body, _ := encode(claim)
	key := engine.claimKey(mutationID)
	version, err := engine.backend.Put(trace(ctx, operation, "idempotency-claim", ""), key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
	if err == nil {
		return claim, version, nil, nil
	}
	if !errors.Is(err, domain.ErrConflict) {
		return mutationClaim{}, "", nil, err
	}
	object, err := engine.backend.Get(trace(ctx, operation, "idempotency-claim", ""), key)
	if err != nil {
		return mutationClaim{}, "", nil, err
	}
	if decode(object.Body, &claim) != nil || claim.SchemaVersion != 1 || claim.MutationID != mutationID || claim.Fingerprint != fingerprint {
		return mutationClaim{}, "", nil, domain.NewError(domain.ErrorConflict, "idempotency key was reused or corrupt")
	}
	if claim.Committed {
		outcome := claim.Outcome
		outcome.Replayed = true
		return claim, object.Version, &outcome, nil
	}
	if head.Latest.MutationID == mutationID {
		if head.Latest.Fingerprint != fingerprint || !head.Latest.Committed {
			return mutationClaim{}, "", nil, domain.NewError(domain.ErrorInvalid, "hybrid claim disagrees with head")
		}
		claim.Committed, claim.Outcome = true, head.Latest
		if err := engine.finalizeClaim(ctx, operation, claim, object.Version); err != nil {
			return mutationClaim{}, "", nil, err
		}
		outcome := claim.Outcome
		outcome.Replayed = true
		return claim, object.Version, &outcome, nil
	}
	return claim, object.Version, nil, nil
}

func (engine *hybridEngine) finalizeClaim(ctx context.Context, operation MutationKind, claim mutationClaim, version objectstore.NativeVersion) error {
	body, _ := encode(claim)
	_, err := engine.backend.Put(trace(ctx, operation, "idempotency-finalize", ""), engine.claimKey(claim.MutationID), body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version})
	return err
}

func (engine *hybridEngine) entries(ctx context.Context, operation MutationKind, head hybridHead, directory graphEntry) (map[string]graphEntry, error) {
	entries := make(map[string]graphEntry)
	if err := engine.tree.walk(ctx, operation, "hybrid-base-index", directory.DirectoryRef, func(name string, body json.RawMessage) error {
		var entry graphEntry
		if json.Unmarshal(body, &entry) != nil || validateGraphEntry(entry) != nil {
			return domain.NewError(domain.ErrorInvalid, "invalid hybrid base entry")
		}
		entries[name] = entry
		return nil
	}); err != nil {
		return nil, err
	}
	for _, delta := range head.Deltas {
		if change, found := delta.Directories[directory.NodeID]; found {
			for name, entry := range change.Entries {
				if entry == nil {
					delete(entries, name)
				} else {
					entries[name] = *entry
				}
			}
		}
	}
	return entries, nil
}

func (engine *hybridEngine) Snapshot(ctx context.Context) (Snapshot, error) {
	head, _, err := engine.loadHead(ctx, "snapshot")
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{SchemaVersion: 1, Revision: head.Revision, Frozen: head.Frozen, LiveRootID: "live-root", TrashRootID: "trash-root", Nodes: map[string]Node{}, Outcomes: map[string]Outcome{}}
	if head.Latest.MutationID != "" {
		snapshot.Outcomes[head.Latest.MutationID] = head.Latest
	}
	visited := make(map[string]bool)
	var visit func(graphEntry) error
	visit = func(base graphEntry) error {
		directory := engine.currentDirectory(head, base)
		if visited[directory.NodeID] {
			return nil
		}
		visited[directory.NodeID] = true
		entries, err := engine.entries(ctx, "snapshot", head, directory)
		if err != nil {
			return err
		}
		node := Node{ID: directory.NodeID, Kind: NodeDirectory, Size: directory.Size, FileCount: directory.FileCount, Children: map[string]string{}}
		names := make([]string, 0, len(entries))
		for name := range entries {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			entry := entries[name]
			node.Children[name] = entry.NodeID
			if entry.Kind == NodeFile {
				snapshot.Nodes[entry.NodeID] = Node{ID: entry.NodeID, Kind: NodeFile, Size: entry.Size, FileCount: 1, BlobIdentity: entry.BlobIdentity, ContentVersion: entry.ContentVersion}
			} else if err := visit(entry); err != nil {
				return err
			}
		}
		snapshot.Nodes[directory.NodeID] = node
		return nil
	}
	if err := visit(head.Live); err != nil {
		return Snapshot{}, err
	}
	if err := visit(head.Trash); err != nil {
		return Snapshot{}, err
	}
	return snapshot, validateSnapshot(snapshot)
}

func (engine *hybridEngine) Compact(ctx context.Context) error {
	head, version, err := engine.loadHead(ctx, "compaction")
	if err != nil || len(head.Deltas) == 0 {
		return err
	}
	return engine.compactHead(ctx, head, version)
}

func (engine *hybridEngine) compactHead(ctx context.Context, head hybridHead, version objectstore.NativeVersion) error {
	merged := make(map[string]*hybridDirectoryChange)
	for _, delta := range head.Deltas {
		for id, source := range delta.Directories {
			change := merged[id]
			if change == nil {
				copy := source
				copy.Entries = make(map[string]*graphEntry)
				change = &copy
				merged[id] = change
			}
			change.Directory, change.ParentID, change.ParentName, change.Depth = source.Directory, source.ParentID, source.ParentName, source.Depth
			for name, entry := range source.Entries {
				change.Entries[name] = entry
			}
		}
	}
	maxDepth := 0
	for _, change := range merged {
		if change.Depth > maxDepth {
			maxDepth = change.Depth
		}
	}
	session := newTreeSession(engine.tree)
	for depth := maxDepth; depth >= 0; depth-- {
		ids := make([]string, 0)
		for id, change := range merged {
			if change.Depth == depth {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			change := merged[id]
			names := make([]string, 0, len(change.Entries))
			for name := range change.Entries {
				names = append(names, name)
			}
			sort.Strings(names)
			edits := make([]treeEdit, 0, len(names))
			for _, name := range names {
				entry := change.Entries[name]
				if entry == nil {
					edits = append(edits, treeEdit{Key: name, Remove: true, Requirement: treeAny})
				} else {
					body, _ := encode(*entry)
					edits = append(edits, treeEdit{Key: name, Value: body, Requirement: treeAny})
				}
			}
			ref, err := session.apply(ctx, "compaction", "hybrid-base-index", change.Directory.DirectoryRef, edits)
			if err != nil {
				return err
			}
			change.Directory.DirectoryRef = ref
			if change.ParentID != "" {
				parent := merged[change.ParentID]
				if parent == nil {
					return domain.NewError(domain.ErrorInvalid, "hybrid compaction is missing a dirty ancestor")
				}
				copy := change.Directory
				parent.Entries[change.ParentName] = &copy
			}
		}
	}
	if live := merged["live-root"]; live != nil {
		head.Live = live.Directory
	}
	if trash := merged["trash-root"]; trash != nil {
		head.Trash = trash.Directory
	}
	head.Deltas = []hybridDelta{}
	body, err := encode(head)
	if err != nil {
		return err
	}
	_, err = engine.backend.Put(trace(ctx, "compaction", "compaction-commit", ""), engine.headKey, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version})
	return err
}

func (engine *hybridEngine) Freeze(ctx context.Context, checkpointID string) (Checkpoint, error) {
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

func validateHybridHeadLimit(head hybridHead, maximumDeltas int) error {
	if maximumDeltas < 1 || head.SchemaVersion != 1 || head.Revision == 0 || len(head.Deltas) > maximumDeltas || validateRootEntry(head.Live, "live-root") != nil || validateRootEntry(head.Trash, "trash-root") != nil {
		return errors.New("invalid hybrid head")
	}
	for _, delta := range head.Deltas {
		if delta.Mutation.ID == "" || delta.Outcome.MutationID != delta.Mutation.ID || !delta.Outcome.Committed || len(delta.Directories) == 0 {
			return errors.New("invalid hybrid delta")
		}
		for id, change := range delta.Directories {
			if id != change.Directory.NodeID || change.Directory.Kind != NodeDirectory || change.Directory.DirectoryRef == "" || change.Depth < 0 || change.Entries == nil || change.Depth == 0 && change.ParentID != "" || change.Depth > 0 && (change.ParentID == "" || change.ParentName == "") {
				return errors.New("invalid hybrid directory delta")
			}
		}
	}
	return nil
}
