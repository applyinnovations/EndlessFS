// Package architecturelab contains executable storage-architecture candidates
// and their shared semantic/economics harness. It is deliberately isolated
// from the production writer until comparative evidence selects a design.
package architecturelab

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

type Area string

const (
	AreaLive  Area = "live"
	AreaTrash Area = "trash"
)

func (area Area) valid() bool { return area == AreaLive || area == AreaTrash }

type NodeKind string

const (
	NodeFile      NodeKind = "file"
	NodeDirectory NodeKind = "directory"
)

type Node struct {
	ID             string            `json:"id"`
	Kind           NodeKind          `json:"kind"`
	Size           int64             `json:"size"`
	FileCount      int64             `json:"fileCount"`
	BlobIdentity   string            `json:"blobIdentity,omitempty"`
	Children       map[string]string `json:"children,omitempty"`
	ContentVersion string            `json:"contentVersion,omitempty"`
}

type MutationKind string

const (
	MutationCreateDirectory MutationKind = "create-directory"
	MutationCreateFile      MutationKind = "complete-upload"
	MutationCopy            MutationKind = "copy"
	MutationMove            MutationKind = "move"
	MutationDelete          MutationKind = "delete"
)

type Mutation struct {
	ID           string       `json:"id"`
	Kind         MutationKind `json:"kind"`
	FromArea     Area         `json:"fromArea,omitempty"`
	ToArea       Area         `json:"toArea,omitempty"`
	Source       string       `json:"source,omitempty"`
	Destination  string       `json:"destination,omitempty"`
	NodeID       string       `json:"nodeID,omitempty"`
	Size         int64        `json:"size,omitempty"`
	BlobIdentity string       `json:"blobIdentity,omitempty"`
}

type Outcome struct {
	MutationID  string `json:"mutationID"`
	Fingerprint string `json:"fingerprint"`
	Revision    uint64 `json:"revision"`
	Committed   bool   `json:"committed"`
	Replayed    bool   `json:"-"`
}

type Snapshot struct {
	SchemaVersion int                `json:"schemaVersion"`
	Revision      uint64             `json:"revision"`
	Frozen        bool               `json:"frozen"`
	LiveRootID    string             `json:"liveRootID"`
	TrashRootID   string             `json:"trashRootID"`
	Nodes         map[string]Node    `json:"nodes"`
	Outcomes      map[string]Outcome `json:"outcomes"`
}

func initialSnapshot() Snapshot {
	live := Node{ID: "live-root", Kind: NodeDirectory, Children: map[string]string{}}
	trash := Node{ID: "trash-root", Kind: NodeDirectory, Children: map[string]string{}}
	return Snapshot{
		SchemaVersion: 1, Revision: 1, LiveRootID: live.ID, TrashRootID: trash.ID,
		Nodes: map[string]Node{live.ID: live, trash.ID: trash}, Outcomes: map[string]Outcome{},
	}
}

func (snapshot Snapshot) Lookup(area Area, path string) (Node, bool) {
	parsed, err := domain.ParseUserPath(path)
	if err != nil || !area.valid() {
		return Node{}, false
	}
	id := snapshot.rootID(area)
	for _, segment := range parsed.Segments() {
		node, ok := snapshot.Nodes[id]
		if !ok || node.Kind != NodeDirectory {
			return Node{}, false
		}
		id, ok = node.Children[segment]
		if !ok {
			return Node{}, false
		}
	}
	node, ok := snapshot.Nodes[id]
	return node, ok
}

func (snapshot Snapshot) rootID(area Area) string {
	if area == AreaTrash {
		return snapshot.TrashRootID
	}
	return snapshot.LiveRootID
}

func applyMutation(snapshot Snapshot, mutation Mutation) (Snapshot, Outcome, bool, error) {
	fingerprint, err := mutationFingerprint(mutation)
	if err != nil {
		return Snapshot{}, Outcome{}, false, err
	}
	if existing, ok := snapshot.Outcomes[mutation.ID]; ok {
		if existing.Fingerprint != fingerprint {
			return Snapshot{}, Outcome{}, false, domain.NewError(domain.ErrorConflict, "idempotency key was reused for another mutation")
		}
		existing.Replayed = true
		return snapshot, existing, false, nil
	}
	if snapshot.Frozen {
		return Snapshot{}, Outcome{}, false, domain.NewError(domain.ErrorUnavailable, "consistency domain is frozen")
	}
	next := cloneSnapshot(snapshot)
	switch mutation.Kind {
	case MutationCreateDirectory:
		err = next.createNode(mutation, Node{ID: mutation.NodeID, Kind: NodeDirectory, Children: map[string]string{}})
	case MutationCreateFile:
		err = next.createNode(mutation, Node{ID: mutation.NodeID, Kind: NodeFile, Size: mutation.Size, FileCount: 1, BlobIdentity: mutation.BlobIdentity, ContentVersion: fingerprint})
	case MutationMove:
		err = next.moveNode(mutation)
	case MutationDelete:
		err = next.deleteNode(mutation)
	default:
		err = domain.NewError(domain.ErrorInvalid, "unsupported architecture mutation")
	}
	if err != nil {
		return Snapshot{}, Outcome{}, false, err
	}
	if err := next.recalculate(); err != nil {
		return Snapshot{}, Outcome{}, false, err
	}
	next.Revision++
	outcome := Outcome{MutationID: mutation.ID, Fingerprint: fingerprint, Revision: next.Revision, Committed: true}
	next.Outcomes[mutation.ID] = outcome
	return next, outcome, true, nil
}

func (snapshot *Snapshot) createNode(mutation Mutation, node Node) error {
	if mutation.ID == "" || !mutation.ToArea.valid() || node.ID == "" || node.Size < 0 || (node.Kind == NodeFile && node.BlobIdentity == "") {
		return domain.NewError(domain.ErrorInvalid, "invalid create mutation")
	}
	path, err := domain.ParseUserPath(mutation.Destination)
	if err != nil || path.IsRoot() {
		return domain.NewError(domain.ErrorInvalid, "invalid create destination")
	}
	if _, exists := snapshot.Nodes[node.ID]; exists {
		return domain.NewError(domain.ErrorConflict, "node identity already exists")
	}
	parentID, parent, err := snapshot.resolveDirectory(mutation.ToArea, path.Parent())
	if err != nil {
		return err
	}
	if _, exists := parent.Children[path.Name()]; exists {
		return domain.NewError(domain.ErrorConflict, "destination already exists")
	}
	parent.Children[path.Name()] = node.ID
	snapshot.Nodes[parentID] = parent
	snapshot.Nodes[node.ID] = node
	return nil
}

func (snapshot *Snapshot) moveNode(mutation Mutation) error {
	if mutation.ID == "" || !mutation.FromArea.valid() || !mutation.ToArea.valid() {
		return domain.NewError(domain.ErrorInvalid, "invalid move mutation")
	}
	source, err := domain.ParseUserPath(mutation.Source)
	if err != nil || source.IsRoot() {
		return domain.NewError(domain.ErrorInvalid, "invalid move source")
	}
	destination, err := domain.ParseUserPath(mutation.Destination)
	if err != nil || destination.IsRoot() {
		return domain.NewError(domain.ErrorInvalid, "invalid move destination")
	}
	if mutation.FromArea == mutation.ToArea && destination.IsDescendantOf(source) {
		return domain.NewError(domain.ErrorInvalid, "destination is inside source")
	}
	sourceParentID, sourceParent, err := snapshot.resolveDirectory(mutation.FromArea, source.Parent())
	if err != nil {
		return err
	}
	nodeID, exists := sourceParent.Children[source.Name()]
	if !exists {
		return domain.NewError(domain.ErrorNotFound, "source does not exist")
	}
	destinationParentID, destinationParent, err := snapshot.resolveDirectory(mutation.ToArea, destination.Parent())
	if err != nil {
		return err
	}
	if _, exists := destinationParent.Children[destination.Name()]; exists {
		return domain.NewError(domain.ErrorConflict, "destination already exists")
	}
	delete(sourceParent.Children, source.Name())
	destinationParent.Children[destination.Name()] = nodeID
	if sourceParentID == destinationParentID {
		snapshot.Nodes[sourceParentID] = destinationParent
		return nil
	}
	snapshot.Nodes[sourceParentID] = sourceParent
	snapshot.Nodes[destinationParentID] = destinationParent
	return nil
}

func (snapshot *Snapshot) deleteNode(mutation Mutation) error {
	if mutation.ID == "" || !mutation.FromArea.valid() {
		return domain.NewError(domain.ErrorInvalid, "invalid delete mutation")
	}
	path, err := domain.ParseUserPath(mutation.Source)
	if err != nil || path.IsRoot() {
		return domain.NewError(domain.ErrorInvalid, "invalid delete source")
	}
	parentID, parent, err := snapshot.resolveDirectory(mutation.FromArea, path.Parent())
	if err != nil {
		return err
	}
	if _, exists := parent.Children[path.Name()]; !exists {
		return domain.NewError(domain.ErrorNotFound, "source does not exist")
	}
	delete(parent.Children, path.Name())
	snapshot.Nodes[parentID] = parent
	return nil
}

func (snapshot Snapshot) resolveDirectory(area Area, path domain.UserPath) (string, Node, error) {
	id := snapshot.rootID(area)
	for _, segment := range path.Segments() {
		node, ok := snapshot.Nodes[id]
		if !ok || node.Kind != NodeDirectory {
			return "", Node{}, domain.NewError(domain.ErrorInvalid, "namespace graph is corrupt")
		}
		next, ok := node.Children[segment]
		if !ok {
			return "", Node{}, domain.NewError(domain.ErrorNotFound, "directory does not exist")
		}
		id = next
	}
	node, ok := snapshot.Nodes[id]
	if !ok || node.Kind != NodeDirectory {
		return "", Node{}, domain.NewError(domain.ErrorInvalid, "path is not a directory")
	}
	return id, cloneNode(node), nil
}

func (snapshot *Snapshot) recalculate() error {
	visiting := make(map[string]bool)
	visited := make(map[string]bool)
	var visit func(string) (int64, int64, error)
	visit = func(id string) (int64, int64, error) {
		node, ok := snapshot.Nodes[id]
		if !ok {
			return 0, 0, errors.New("namespace graph references a missing node")
		}
		if node.Kind == NodeFile {
			if node.Size < 0 || node.BlobIdentity == "" {
				return 0, 0, errors.New("namespace graph contains an invalid file")
			}
			return node.Size, 1, nil
		}
		if node.Kind != NodeDirectory || visiting[id] {
			return 0, 0, errors.New("namespace graph contains an invalid directory cycle")
		}
		if visited[id] {
			return node.Size, node.FileCount, nil
		}
		visiting[id] = true
		var size, count int64
		for name, childID := range node.Children {
			if name == "" {
				return 0, 0, errors.New("namespace graph contains an invalid name")
			}
			childSize, childCount, err := visit(childID)
			if err != nil {
				return 0, 0, err
			}
			size += childSize
			count += childCount
		}
		visiting[id] = false
		visited[id] = true
		node.Size, node.FileCount = size, count
		snapshot.Nodes[id] = node
		return size, count, nil
	}
	if _, _, err := visit(snapshot.LiveRootID); err != nil {
		return err
	}
	if _, _, err := visit(snapshot.TrashRootID); err != nil {
		return err
	}
	return nil
}

func cloneSnapshot(source Snapshot) Snapshot {
	result := source
	result.Nodes = make(map[string]Node, len(source.Nodes))
	for id, node := range source.Nodes {
		result.Nodes[id] = cloneNode(node)
	}
	result.Outcomes = make(map[string]Outcome, len(source.Outcomes))
	for id, outcome := range source.Outcomes {
		result.Outcomes[id] = outcome
	}
	return result
}

func cloneNode(source Node) Node {
	result := source
	if source.Children != nil {
		result.Children = make(map[string]string, len(source.Children))
		for name, id := range source.Children {
			result.Children[name] = id
		}
	} else if source.Kind == NodeDirectory {
		result.Children = map[string]string{}
	}
	return result
}

func mutationFingerprint(mutation Mutation) (string, error) {
	if mutation.ID == "" || strings.ContainsAny(mutation.ID, "\r\n\x00") {
		return "", domain.NewError(domain.ErrorInvalid, "invalid mutation identity")
	}
	body, err := json.Marshal(mutation)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

func encode(value any) ([]byte, error) { return json.Marshal(value) }

func decode(body []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing architecture candidate data")
		}
		return err
	}
	return nil
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func validateSnapshot(snapshot Snapshot) error {
	if snapshot.SchemaVersion != 1 || snapshot.Revision == 0 || snapshot.LiveRootID == "" || snapshot.TrashRootID == "" || snapshot.Nodes == nil || snapshot.Outcomes == nil {
		return fmt.Errorf("invalid architecture snapshot")
	}
	copy := cloneSnapshot(snapshot)
	return copy.recalculate()
}
