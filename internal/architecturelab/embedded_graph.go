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

type embeddedHead struct {
	SchemaVersion int        `json:"schemaVersion"`
	Revision      uint64     `json:"revision"`
	Frozen        bool       `json:"frozen"`
	Live          graphEntry `json:"live"`
	Trash         graphEntry `json:"trash"`
	OutcomeRef    string     `json:"outcomeRef"`
	OutcomeBloom  string     `json:"outcomeBloom"`
	Recent        []Outcome  `json:"recent"`
}

type embeddedFrame struct {
	directory  graphEntry
	parentID   string
	parentName string
	depth      int
	area       Area
}

type mutationClaim struct {
	SchemaVersion int     `json:"schemaVersion"`
	MutationID    string  `json:"mutationID"`
	Fingerprint   string  `json:"fingerprint"`
	Committed     bool    `json:"committed"`
	Outcome       Outcome `json:"outcome,omitempty"`
}

type embeddedGraphEngine struct {
	backend   objectstore.Backend
	domainID  string
	candidate string
	claims    bool
	headKey   objectstore.Key
	tree      immutableTree
}

func openEmbeddedGraph(ctx context.Context, backend objectstore.Backend, options Options) (Engine, error) {
	return openEmbeddedGraphMode(ctx, backend, options, "embedded", false)
}

func openClaimedEmbeddedGraph(ctx context.Context, backend objectstore.Backend, options Options) (Engine, error) {
	return openEmbeddedGraphMode(ctx, backend, options, "claimed", true)
}

func openEmbeddedGraphMode(ctx context.Context, backend objectstore.Backend, options Options, candidate string, claims bool) (Engine, error) {
	if err := validateOptions(backend, options); err != nil {
		return nil, err
	}
	engine := &embeddedGraphEngine{
		backend: backend, domainID: options.DomainID, candidate: candidate, claims: claims,
		headKey: candidateKey(candidate, options.DomainID, "head.json"),
		tree:    immutableTree{backend: backend, domainID: options.DomainID, candidate: candidate},
	}
	emptyRef, err := engine.tree.empty(ctx, "initialize", "embedded-tree-initial")
	if err != nil {
		return nil, err
	}
	head := embeddedHead{
		SchemaVersion: 1,
		Revision:      1,
		Live:          graphEntry{NodeID: "live-root", Kind: NodeDirectory, DirectoryRef: emptyRef},
		Trash:         graphEntry{NodeID: "trash-root", Kind: NodeDirectory, DirectoryRef: emptyRef},
		OutcomeRef:    emptyRef,
		OutcomeBloom:  zeroBloom(),
		Recent:        []Outcome{},
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

func (engine *embeddedGraphEngine) Name() string {
	if engine.claims {
		return "claimed-paged-namespace"
	}
	return "embedded-paged-namespace"
}

func (engine *embeddedGraphEngine) loadHead(ctx context.Context, operation MutationKind) (embeddedHead, objectstore.NativeVersion, error) {
	object, err := engine.backend.Get(trace(ctx, operation, "embedded-head", ""), engine.headKey)
	if err != nil {
		return embeddedHead{}, "", err
	}
	var head embeddedHead
	if err := decode(object.Body, &head); err != nil || validateEmbeddedHead(head) != nil {
		return embeddedHead{}, "", domain.NewError(domain.ErrorInvalid, "invalid embedded namespace head")
	}
	return head, object.Version, nil
}

func (engine *embeddedGraphEngine) resolveTrail(ctx context.Context, operation MutationKind, head embeddedHead, area Area, path domain.UserPath, session *treeSession) ([]embeddedFrame, error) {
	directory := head.Live
	if area == AreaTrash {
		directory = head.Trash
	}
	trail := []embeddedFrame{{directory: directory, depth: 0, area: area}}
	for _, segment := range path.Segments() {
		entry, found, err := engine.lookupEntry(ctx, operation, session, directory.DirectoryRef, segment)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, domain.NewError(domain.ErrorNotFound, "embedded directory does not exist")
		}
		if entry.Kind != NodeDirectory || entry.DirectoryRef == "" {
			return nil, domain.NewError(domain.ErrorInvalid, "embedded path is not a directory")
		}
		parent := directory
		directory = entry
		trail = append(trail, embeddedFrame{directory: directory, parentID: parent.NodeID, parentName: segment, depth: len(trail), area: area})
	}
	return trail, nil
}

func (engine *embeddedGraphEngine) lookupEntry(ctx context.Context, operation MutationKind, session *treeSession, indexRef, name string) (graphEntry, bool, error) {
	body, found, err := session.lookup(ctx, operation, "embedded-directory-index", indexRef, name)
	if err != nil || !found {
		return graphEntry{}, found, err
	}
	var entry graphEntry
	if err := json.Unmarshal(body, &entry); err != nil || validateGraphEntry(entry) != nil {
		return graphEntry{}, false, domain.NewError(domain.ErrorInvalid, "invalid embedded directory entry")
	}
	return entry, true, nil
}

func (engine *embeddedGraphEngine) Mutate(ctx context.Context, mutation Mutation) (Outcome, error) {
	fingerprint, err := mutationFingerprint(mutation)
	if err != nil {
		return Outcome{}, err
	}
	head, version, err := engine.loadHead(ctx, mutation.Kind)
	if err != nil {
		return Outcome{}, err
	}
	session := newTreeSession(engine.tree)
	var claim mutationClaim
	var claimVersion objectstore.NativeVersion
	if engine.claims {
		var replay *Outcome
		claim, claimVersion, replay, err = engine.claimMutation(ctx, mutation.Kind, head, mutation.ID, fingerprint)
		if err != nil {
			return Outcome{}, err
		}
		if replay != nil {
			return *replay, nil
		}
	} else {
		if outcome, found, err := engine.lookupOutcome(ctx, mutation.Kind, session, head, mutation.ID); err != nil {
			return Outcome{}, err
		} else if found {
			if outcome.Fingerprint != fingerprint {
				return Outcome{}, domain.NewError(domain.ErrorConflict, "idempotency key was reused for another mutation")
			}
			outcome.Replayed = true
			return outcome, nil
		}
	}
	if head.Frozen {
		return Outcome{}, domain.NewError(domain.ErrorUnavailable, "consistency domain is frozen")
	}

	changes := make(map[string]graphEntry)
	frames := make(map[string]embeddedFrame)
	addTrail := func(trail []embeddedFrame) {
		for _, frame := range trail {
			frames[frame.directory.NodeID] = frame
		}
	}
	switch mutation.Kind {
	case MutationCreateDirectory, MutationCreateFile:
		path, parseErr := domain.ParseUserPath(mutation.Destination)
		if parseErr != nil || path.IsRoot() || !mutation.ToArea.valid() || mutation.NodeID == "" {
			return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid embedded create mutation")
		}
		trail, err := engine.resolveTrail(ctx, mutation.Kind, head, mutation.ToArea, path.Parent(), session)
		if err != nil {
			return Outcome{}, err
		}
		addTrail(trail)
		parent := trail[len(trail)-1].directory
		entry := graphEntry{NodeID: mutation.NodeID, Size: mutation.Size, ContentVersion: fingerprint}
		if mutation.Kind == MutationCreateDirectory {
			entry.Kind = NodeDirectory
			entry.DirectoryRef = engine.tree.emptyReference()
		} else {
			if mutation.Size < 0 || mutation.BlobIdentity == "" {
				return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid embedded file mutation")
			}
			entry.Kind, entry.FileCount, entry.BlobIdentity = NodeFile, 1, mutation.BlobIdentity
		}
		entryBody, _ := encode(entry)
		parent.DirectoryRef, err = session.apply(ctx, mutation.Kind, "embedded-directory-index", parent.DirectoryRef, []treeEdit{{Key: path.Name(), Value: entryBody, Requirement: treeAbsent}})
		if err != nil {
			return Outcome{}, err
		}
		parent.Size += entry.Size
		parent.FileCount += entry.FileCount
		changes[parent.NodeID] = parent
	case MutationMove:
		source, parseErr := domain.ParseUserPath(mutation.Source)
		if parseErr != nil || source.IsRoot() || !mutation.FromArea.valid() || !mutation.ToArea.valid() {
			return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid embedded move mutation")
		}
		destination, parseErr := domain.ParseUserPath(mutation.Destination)
		if parseErr != nil || destination.IsRoot() || mutation.FromArea == mutation.ToArea && destination.IsDescendantOf(source) {
			return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid embedded move destination")
		}
		sourceTrail, err := engine.resolveTrail(ctx, mutation.Kind, head, mutation.FromArea, source.Parent(), session)
		if err != nil {
			return Outcome{}, err
		}
		destinationTrail, err := engine.resolveTrail(ctx, mutation.Kind, head, mutation.ToArea, destination.Parent(), session)
		if err != nil {
			return Outcome{}, err
		}
		addTrail(sourceTrail)
		addTrail(destinationTrail)
		sourceParent := sourceTrail[len(sourceTrail)-1].directory
		destinationParent := destinationTrail[len(destinationTrail)-1].directory
		entry, found, err := engine.lookupEntry(ctx, mutation.Kind, session, sourceParent.DirectoryRef, source.Name())
		if err != nil {
			return Outcome{}, err
		}
		if !found {
			return Outcome{}, domain.NewError(domain.ErrorNotFound, "source does not exist")
		}
		if _, found, err := engine.lookupEntry(ctx, mutation.Kind, session, destinationParent.DirectoryRef, destination.Name()); err != nil {
			return Outcome{}, err
		} else if found {
			return Outcome{}, domain.NewError(domain.ErrorConflict, "destination already exists")
		}
		entryBody, _ := encode(entry)
		if sourceParent.NodeID == destinationParent.NodeID {
			sourceParent.DirectoryRef, err = session.apply(ctx, mutation.Kind, "embedded-directory-index", sourceParent.DirectoryRef, []treeEdit{
				{Key: source.Name(), Remove: true, Requirement: treePresent},
				{Key: destination.Name(), Value: entryBody, Requirement: treeAbsent},
			})
			if err != nil {
				return Outcome{}, err
			}
			changes[sourceParent.NodeID] = sourceParent
		} else {
			sourceParent.DirectoryRef, err = session.apply(ctx, mutation.Kind, "embedded-directory-index", sourceParent.DirectoryRef, []treeEdit{{Key: source.Name(), Remove: true, Requirement: treePresent}})
			if err != nil {
				return Outcome{}, err
			}
			destinationParent.DirectoryRef, err = session.apply(ctx, mutation.Kind, "embedded-directory-index", destinationParent.DirectoryRef, []treeEdit{{Key: destination.Name(), Value: entryBody, Requirement: treeAbsent}})
			if err != nil {
				return Outcome{}, err
			}
			sourceParent.Size -= entry.Size
			sourceParent.FileCount -= entry.FileCount
			destinationParent.Size += entry.Size
			destinationParent.FileCount += entry.FileCount
			changes[sourceParent.NodeID], changes[destinationParent.NodeID] = sourceParent, destinationParent
		}
	case MutationDelete:
		path, parseErr := domain.ParseUserPath(mutation.Source)
		if parseErr != nil || path.IsRoot() || !mutation.FromArea.valid() {
			return Outcome{}, domain.NewError(domain.ErrorInvalid, "invalid embedded delete mutation")
		}
		trail, err := engine.resolveTrail(ctx, mutation.Kind, head, mutation.FromArea, path.Parent(), session)
		if err != nil {
			return Outcome{}, err
		}
		addTrail(trail)
		parent := trail[len(trail)-1].directory
		entry, found, err := engine.lookupEntry(ctx, mutation.Kind, session, parent.DirectoryRef, path.Name())
		if err != nil {
			return Outcome{}, err
		}
		if !found {
			return Outcome{}, domain.NewError(domain.ErrorNotFound, "source does not exist")
		}
		parent.DirectoryRef, err = session.apply(ctx, mutation.Kind, "embedded-directory-index", parent.DirectoryRef, []treeEdit{{Key: path.Name(), Remove: true, Requirement: treePresent}})
		if err != nil {
			return Outcome{}, err
		}
		parent.Size -= entry.Size
		parent.FileCount -= entry.FileCount
		changes[parent.NodeID] = parent
	default:
		return Outcome{}, domain.NewError(domain.ErrorInvalid, "unsupported embedded mutation")
	}

	if err := engine.propagate(ctx, mutation.Kind, session, changes, frames); err != nil {
		return Outcome{}, err
	}
	if live, found := changes["live-root"]; found {
		head.Live = live
	}
	if trash, found := changes["trash-root"]; found {
		head.Trash = trash
	}
	head.Revision++
	outcome := Outcome{MutationID: mutation.ID, Fingerprint: fingerprint, Revision: head.Revision, Committed: true}
	if engine.claims {
		head.Recent = []Outcome{outcome}
	} else {
		if err := engine.addOutcome(ctx, mutation.Kind, session, &head, outcome); err != nil {
			return Outcome{}, err
		}
	}
	body, err := encode(head)
	if err != nil {
		return Outcome{}, err
	}
	if _, err := engine.backend.Put(trace(ctx, mutation.Kind, "namespace-commit", ""), engine.headKey, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version}); err != nil {
		return Outcome{}, err
	}
	if engine.claims {
		claim.Committed, claim.Outcome = true, outcome
		if err := engine.finalizeClaim(ctx, mutation.Kind, claim, claimVersion); err != nil {
			return Outcome{}, err
		}
	}
	return outcome, nil
}

func (engine *embeddedGraphEngine) claimKey(mutationID string) objectstore.Key {
	return candidateKey(engine.candidate, engine.domainID, "claims/"+digest([]byte(mutationID))+".json")
}

func (engine *embeddedGraphEngine) claimMutation(ctx context.Context, operation MutationKind, head embeddedHead, mutationID, fingerprint string) (mutationClaim, objectstore.NativeVersion, *Outcome, error) {
	claim := mutationClaim{SchemaVersion: 1, MutationID: mutationID, Fingerprint: fingerprint}
	body, err := encode(claim)
	if err != nil {
		return mutationClaim{}, "", nil, err
	}
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
	if err := decode(object.Body, &claim); err != nil || claim.SchemaVersion != 1 || claim.MutationID != mutationID || claim.Fingerprint == "" {
		return mutationClaim{}, "", nil, domain.NewError(domain.ErrorInvalid, "invalid mutation claim")
	}
	if claim.Fingerprint != fingerprint {
		return mutationClaim{}, "", nil, domain.NewError(domain.ErrorConflict, "idempotency key was reused for another mutation")
	}
	if claim.Committed {
		if !claim.Outcome.Committed || claim.Outcome.MutationID != mutationID || claim.Outcome.Fingerprint != fingerprint {
			return mutationClaim{}, "", nil, domain.NewError(domain.ErrorInvalid, "invalid committed mutation claim")
		}
		outcome := claim.Outcome
		outcome.Replayed = true
		return claim, object.Version, &outcome, nil
	}
	if len(head.Recent) == 1 && head.Recent[0].MutationID == mutationID {
		if head.Recent[0].Fingerprint != fingerprint || !head.Recent[0].Committed {
			return mutationClaim{}, "", nil, domain.NewError(domain.ErrorInvalid, "mutation claim disagrees with namespace head")
		}
		claim.Committed, claim.Outcome = true, head.Recent[0]
		if err := engine.finalizeClaim(ctx, operation, claim, object.Version); err != nil {
			return mutationClaim{}, "", nil, err
		}
		outcome := claim.Outcome
		outcome.Replayed = true
		return claim, object.Version, &outcome, nil
	}
	return claim, object.Version, nil, nil
}

func (engine *embeddedGraphEngine) finalizeClaim(ctx context.Context, operation MutationKind, claim mutationClaim, version objectstore.NativeVersion) error {
	body, err := encode(claim)
	if err != nil {
		return err
	}
	key := engine.claimKey(claim.MutationID)
	if _, err := engine.backend.Put(trace(ctx, operation, "idempotency-finalize", ""), key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version}); err == nil {
		return nil
	} else if !errors.Is(err, domain.ErrPreconditionFailed) && !errors.Is(err, domain.ErrConflict) {
		return err
	}
	object, readErr := engine.backend.Get(trace(ctx, operation, "idempotency-finalize", ""), key)
	if readErr != nil {
		return readErr
	}
	var existing mutationClaim
	if decode(object.Body, &existing) != nil || existing != claim {
		return domain.NewError(domain.ErrorInvalid, "mutation claim finalization conflicted")
	}
	return nil
}

func (engine *embeddedGraphEngine) propagate(ctx context.Context, operation MutationKind, session *treeSession, changes map[string]graphEntry, frames map[string]embeddedFrame) error {
	dirty := make(map[string]bool)
	maxDepth := 0
	for id := range changes {
		for current := id; current != ""; {
			dirty[current] = true
			frame, found := frames[current]
			if !found {
				break
			}
			if frame.depth > maxDepth {
				maxDepth = frame.depth
			}
			current = frame.parentID
		}
	}
	for depth := maxDepth; depth > 0; depth-- {
		childrenByParent := make(map[string][]string)
		for id := range dirty {
			frame, found := frames[id]
			if found && frame.depth == depth {
				childrenByParent[frame.parentID] = append(childrenByParent[frame.parentID], id)
			}
		}
		parentIDs := make([]string, 0, len(childrenByParent))
		for parentID := range childrenByParent {
			parentIDs = append(parentIDs, parentID)
		}
		sort.Strings(parentIDs)
		for _, parentID := range parentIDs {
			parent, found := changes[parentID]
			if !found {
				parent = frames[parentID].directory
			}
			childIDs := childrenByParent[parentID]
			sort.Strings(childIDs)
			edits := make([]treeEdit, 0, len(childIDs))
			for _, childID := range childIDs {
				child := changes[childID]
				original := frames[childID]
				body, err := encode(child)
				if err != nil {
					return err
				}
				edits = append(edits, treeEdit{Key: original.parentName, Value: body, Requirement: treePresent})
				parent.Size += child.Size - original.directory.Size
				parent.FileCount += child.FileCount - original.directory.FileCount
			}
			var err error
			parent.DirectoryRef, err = session.apply(ctx, operation, "embedded-directory-index", parent.DirectoryRef, edits)
			if err != nil {
				return err
			}
			changes[parentID] = parent
		}
	}
	return nil
}

func (engine *embeddedGraphEngine) lookupOutcome(ctx context.Context, operation MutationKind, session *treeSession, head embeddedHead, mutationID string) (Outcome, bool, error) {
	for index := len(head.Recent) - 1; index >= 0; index-- {
		if head.Recent[index].MutationID == mutationID {
			return head.Recent[index], true, nil
		}
	}
	if !bloomMaybe(head.OutcomeBloom, mutationID) {
		return Outcome{}, false, nil
	}
	body, found, err := session.lookup(ctx, operation, "embedded-outcome-index", head.OutcomeRef, mutationID)
	if err != nil || !found {
		return Outcome{}, found, err
	}
	var outcome Outcome
	if err := json.Unmarshal(body, &outcome); err != nil || outcome.MutationID != mutationID || !outcome.Committed {
		return Outcome{}, false, domain.NewError(domain.ErrorInvalid, "invalid embedded outcome")
	}
	return outcome, true, nil
}

func (engine *embeddedGraphEngine) addOutcome(ctx context.Context, operation MutationKind, session *treeSession, head *embeddedHead, outcome Outcome) error {
	if len(head.Recent) == maxRecentOutcomes {
		edits := make([]treeEdit, 0, flushOutcomes)
		for _, old := range head.Recent[:flushOutcomes] {
			body, _ := encode(old)
			edits = append(edits, treeEdit{Key: old.MutationID, Value: body, Requirement: treeAbsent})
		}
		var err error
		head.OutcomeRef, err = session.apply(ctx, operation, "embedded-outcome-index", head.OutcomeRef, edits)
		if err != nil {
			return err
		}
		head.Recent = append([]Outcome(nil), head.Recent[flushOutcomes:]...)
	}
	head.Recent = append(head.Recent, outcome)
	head.OutcomeBloom = bloomAdd(head.OutcomeBloom, outcome.MutationID)
	return nil
}

func (engine *embeddedGraphEngine) Snapshot(ctx context.Context) (Snapshot, error) {
	head, _, err := engine.loadHead(ctx, "snapshot")
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{SchemaVersion: 1, Revision: head.Revision, Frozen: head.Frozen, LiveRootID: "live-root", TrashRootID: "trash-root", Nodes: map[string]Node{}, Outcomes: map[string]Outcome{}}
	if err := engine.tree.walk(ctx, "snapshot", "embedded-outcome-index", head.OutcomeRef, func(_ string, body json.RawMessage) error {
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
	visited := make(map[string]bool)
	var visit func(graphEntry) error
	visit = func(directory graphEntry) error {
		if visited[directory.NodeID] {
			return nil
		}
		visited[directory.NodeID] = true
		node := Node{ID: directory.NodeID, Kind: NodeDirectory, Size: directory.Size, FileCount: directory.FileCount, Children: map[string]string{}}
		if err := engine.tree.walk(ctx, "snapshot", "embedded-directory-index", directory.DirectoryRef, func(name string, body json.RawMessage) error {
			var entry graphEntry
			if err := json.Unmarshal(body, &entry); err != nil || validateGraphEntry(entry) != nil {
				return domain.NewError(domain.ErrorInvalid, "invalid embedded snapshot entry")
			}
			node.Children[name] = entry.NodeID
			if entry.Kind == NodeFile {
				snapshot.Nodes[entry.NodeID] = Node{ID: entry.NodeID, Kind: NodeFile, Size: entry.Size, FileCount: 1, BlobIdentity: entry.BlobIdentity, ContentVersion: entry.ContentVersion}
				return nil
			}
			return visit(entry)
		}); err != nil {
			return err
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

func (engine *embeddedGraphEngine) Freeze(ctx context.Context, checkpointID string) (Checkpoint, error) {
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

func (engine *embeddedGraphEngine) Compact(ctx context.Context) error {
	if engine.claims {
		return nil
	}
	head, version, err := engine.loadHead(ctx, "compaction")
	if err != nil || len(head.Recent) == 0 {
		return err
	}
	session := newTreeSession(engine.tree)
	edits := make([]treeEdit, 0, len(head.Recent))
	for _, outcome := range head.Recent {
		body, _ := encode(outcome)
		edits = append(edits, treeEdit{Key: outcome.MutationID, Value: body, Requirement: treeAbsent})
	}
	head.OutcomeRef, err = session.apply(ctx, "compaction", "embedded-outcome-index", head.OutcomeRef, edits)
	if err != nil {
		return err
	}
	head.Recent = []Outcome{}
	body, err := encode(head)
	if err != nil {
		return err
	}
	_, err = engine.backend.Put(trace(ctx, "compaction", "compaction-commit", ""), engine.headKey, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version})
	return err
}

func validateEmbeddedHead(head embeddedHead) error {
	if head.SchemaVersion != 1 || head.Revision == 0 || len(head.Recent) > maxRecentOutcomes || validateRootEntry(head.Live, "live-root") != nil || validateRootEntry(head.Trash, "trash-root") != nil || head.OutcomeRef == "" {
		return errors.New("invalid embedded head")
	}
	bloom, err := hex.DecodeString(head.OutcomeBloom)
	if err != nil || len(bloom) != sha256.Size {
		return errors.New("invalid embedded outcome bloom")
	}
	return nil
}

func validateRootEntry(entry graphEntry, id string) error {
	if entry.NodeID != id || entry.Kind != NodeDirectory || entry.DirectoryRef == "" || entry.Size < 0 || entry.FileCount < 0 || entry.BlobIdentity != "" {
		return errors.New("invalid embedded root")
	}
	return nil
}

func validateGraphEntry(entry graphEntry) error {
	if entry.NodeID == "" || entry.Size < 0 || entry.FileCount < 0 {
		return errors.New("invalid graph entry")
	}
	if entry.Kind == NodeFile {
		if entry.FileCount != 1 || entry.BlobIdentity == "" || entry.DirectoryRef != "" {
			return errors.New("invalid graph file entry")
		}
		return nil
	}
	if entry.Kind != NodeDirectory || entry.DirectoryRef == "" || entry.BlobIdentity != "" {
		return errors.New("invalid graph directory entry")
	}
	return nil
}
