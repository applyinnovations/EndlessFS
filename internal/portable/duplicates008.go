package portable

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const duplicateProjectionHeadSchema = "duplicate-projection-v1"

type duplicateProjectionCursor008 struct {
	SchemaVersion  int                          `json:"schemaVersion"`
	OwnerID        string                       `json:"ownerID"`
	Kind           domain.DuplicateKind         `json:"kind,omitempty"`
	GroupID        string                       `json:"groupID,omitempty"`
	IncludeIgnored bool                         `json:"includeIgnored,omitempty"`
	Limit          int                          `json:"limit"`
	ProjectionID   string                       `json:"projectionID"`
	Root           storageformat.DomainTreeRoot `json:"root"`
	After          string                       `json:"after"`
	ExpiresAt      time.Time                    `json:"expiresAt"`
}

type duplicateReconciliationCursor008 struct {
	SchemaVersion int                                 `json:"schemaVersion"`
	OwnerID       string                              `json:"ownerID"`
	Left          domain.DuplicateLocation            `json:"left"`
	Right         domain.DuplicateLocation            `json:"right"`
	RemoveFrom    domain.DuplicateSide                `json:"removeFrom"`
	Limit         int                                 `json:"limit"`
	LeftAfter     string                              `json:"leftAfter,omitempty"`
	RightAfter    string                              `json:"rightAfter,omitempty"`
	Comparison    domain.DuplicateDirectoryComparison `json:"comparison"`
	LeftNodeID    string                              `json:"leftNodeID"`
	RightNodeID   string                              `json:"rightNodeID"`
	LeftRoot      storageformat.DomainTreeRoot        `json:"leftRoot"`
	RightRoot     storageformat.DomainTreeRoot        `json:"rightRoot"`
	ExpiresAt     time.Time                           `json:"expiresAt"`
}

type duplicateReconciliationPlan008 struct {
	SchemaVersion int                                  `json:"schemaVersion"`
	OwnerID       string                               `json:"ownerID"`
	Left          domain.DuplicateOccurrence           `json:"left"`
	Right         domain.DuplicateOccurrence           `json:"right"`
	RemoveFrom    domain.DuplicateSide                 `json:"removeFrom"`
	Items         []domain.DuplicateReconciliationItem `json:"items"`
	LeftNodeID    string                               `json:"leftNodeID"`
	RightNodeID   string                               `json:"rightNodeID"`
	LeftRoot      storageformat.DomainTreeRoot         `json:"leftRoot"`
	RightRoot     storageformat.DomainTreeRoot         `json:"rightRoot"`
	ExpiresAt     time.Time                            `json:"expiresAt"`
}

type duplicateOverlapCursor008 struct {
	SchemaVersion  int                          `json:"schemaVersion"`
	OwnerID        string                       `json:"ownerID"`
	Directory      domain.DuplicateLocation     `json:"directory"`
	IncludeIgnored bool                         `json:"includeIgnored"`
	Limit          int                          `json:"limit"`
	ProjectionID   string                       `json:"projectionID"`
	ProjectionRoot storageformat.DomainTreeRoot `json:"projectionRoot"`
	NodeID         string                       `json:"nodeID"`
	DirectoryRoot  storageformat.DomainTreeRoot `json:"directoryRoot"`
	After          string                       `json:"after,omitempty"`
	ExpiresAt      time.Time                    `json:"expiresAt"`
}

type duplicateProjectionSnapshot008 struct {
	head         storageformat.ProjectionHead
	root         storageformat.DomainTreeRoot
	projectionID string
	session      *consistencyDomainTreeSession
}

func duplicateProjectionID008(owner domain.UserID) string {
	return storageformat.Digest([]byte("endlessfs-duplicate-projection-v1\x00" + owner.String()))
}

func duplicateProjectionSession008(engine *Engine, owner domain.UserID, projectionID string) *consistencyDomainTreeSession {
	return newNamespaceProjectionTreeSession(newConsistencyDomainStore(engine.backend, engine.scheduler, engine.clock), owner, projectionID, storageformat.ProjectionDuplicates)
}

func duplicateProjectionOccurrenceKey008(value storageformat.DuplicateProjectionOccurrence) string {
	location := base64.RawURLEncoding.EncodeToString([]byte(value.Occurrence.Area + "\x00" + value.Occurrence.Path))
	identity := location
	if value.BlobID != "" {
		identity = base64.RawURLEncoding.EncodeToString([]byte(value.BlobID)) + "/" + location
	}
	return "occurrence/" + string(value.Occurrence.Kind) + "/" + value.Occurrence.GroupID + "/" + identity
}

func duplicateProjectionSummaryKey008(kind domain.DuplicateKind, groupID string) string {
	return "group/" + string(kind) + "/" + groupID
}

func duplicateProjectionLocationKey008(area, path string) string {
	return "location/" + area + "/" + base64.RawURLEncoding.EncodeToString([]byte(path))
}

func (s *FileStore) duplicateProjection008(ctx context.Context, owner domain.UserID) (duplicateProjectionSnapshot008, error) {
	if !owner.Valid() {
		return duplicateProjectionSnapshot008{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate projection owner")
	}
	store := newNamespaceStore(s.engine)
	view, err := store.loadView(ctx, owner, "")
	if err != nil {
		return duplicateProjectionSnapshot008{}, err
	}
	if !view.head.Registered || view.head.Revision == 0 {
		id := duplicateProjectionID008(owner)
		return duplicateProjectionSnapshot008{projectionID: id, session: duplicateProjectionSession008(s.engine, owner, id)}, nil
	}
	projectionID := duplicateProjectionID008(owner)
	key := storageformat.ProjectionHeadKey(owner.String(), storageformat.ProjectionDuplicates)
	for {
		object, getErr := s.engine.backend.Get(ctx, key)
		var current storageformat.ProjectionHead
		var envelope storageformat.Envelope
		exists, valid := getErr == nil, false
		if getErr == nil {
			if decodeErr := storageformat.DecodeEnvelope(object.Body, key, duplicateProjectionHeadSchema, &envelope, &current); decodeErr == nil && storageformat.ValidateProjectionHead(current) == nil && current.OwnerID == owner.String() && current.ProjectionID == projectionID && current.Kind == storageformat.ProjectionDuplicates {
				valid = true
			}
		} else if !errors.Is(getErr, domain.ErrNotFound) {
			return duplicateProjectionSnapshot008{}, getErr
		}
		if valid && current.SourceDomainID == view.reference.ID && current.SourceRevision == view.head.Revision && current.SourceRoot == view.head.Base {
			return duplicateProjectionSnapshot008{head: current, root: current.Root, projectionID: projectionID, session: duplicateProjectionSession008(s.engine, owner, projectionID)}, nil
		}
		session := duplicateProjectionSession008(s.engine, owner, projectionID)
		root, buildErr := s.buildDuplicateProjection008(ctx, view, session)
		if buildErr != nil {
			return duplicateProjectionSnapshot008{}, buildErr
		}
		next := storageformat.ProjectionHead{SchemaVersion: 1, OwnerID: owner.String(), ProjectionID: projectionID, Kind: storageformat.ProjectionDuplicates, SourceDomainID: view.reference.ID, SourceRevision: view.head.Revision, SourceRoot: view.head.Base, Root: root}
		if err := storageformat.ValidateProjectionHead(next); err != nil {
			return duplicateProjectionSnapshot008{}, err
		}
		revision := uint64(1)
		condition := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
		if exists {
			condition = objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}
			if valid {
				revision = envelope.Revision + 1
			}
		}
		body, err := storageformat.EncodeEnvelope(duplicateProjectionHeadSchema, key, revision, next)
		if err != nil {
			return duplicateProjectionSnapshot008{}, err
		}
		if _, err := s.engine.backend.Put(ctx, key, body, condition); err == nil {
			return duplicateProjectionSnapshot008{head: next, root: root, projectionID: projectionID, session: session}, nil
		} else if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return duplicateProjectionSnapshot008{}, err
		}
	}
}

func (s *FileStore) buildDuplicateProjection008(ctx context.Context, view *namespaceView, session *consistencyDomainTreeSession) (storageformat.DomainTreeRoot, error) {
	chunk := make([]storageformat.DomainEntry, 0, domainPageMaximumItems)
	runs := make([]storageformat.DomainTreeRoot, 0)
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		sort.Slice(chunk, func(left, right int) bool { return chunk[left].Key < chunk[right].Key })
		root, err := session.buildTree(ctx, chunk)
		if err != nil {
			return err
		}
		runs = append(runs, root)
		session.pages = make(map[string]storageformat.DomainPage)
		chunk = chunk[:0]
		return nil
	}
	emit := func(occurrence storageformat.DuplicateProjectionOccurrence) error {
		body, err := storageformat.EncodeCanonical(occurrence)
		if err != nil {
			return err
		}
		add := func(entry storageformat.DomainEntry) error {
			if len(chunk) == domainPageMaximumItems {
				if err := flush(); err != nil {
					return err
				}
			}
			chunk = append(chunk, entry)
			return nil
		}
		if err := add(storageformat.DomainEntry{Key: duplicateProjectionOccurrenceKey008(occurrence), Value: body, LogicalVersion: storageformat.Digest(append([]byte("endlessfs-duplicate-projection-occurrence-v1\x00"), body...))}); err != nil {
			return err
		}
		if occurrence.Occurrence.Kind == domain.DuplicateDirectory {
			return add(storageformat.DomainEntry{Key: duplicateProjectionLocationKey008(occurrence.Occurrence.Area, occurrence.Occurrence.Path), Value: body, LogicalVersion: storageformat.Digest(append([]byte("endlessfs-duplicate-projection-location-v1\x00"), body...))})
		}
		return nil
	}
	for _, area := range []domain.Area{domain.AreaLive, domain.AreaTrash} {
		if err := s.walkDuplicateNamespace008(ctx, view, area, namespaceRootPath(), view.roots[area], emit); err != nil {
			return storageformat.DomainTreeRoot{}, err
		}
	}
	if err := flush(); err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	occurrences, err := mergeDuplicateProjectionRuns008(ctx, session, runs)
	if err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	if occurrences.Digest == "" {
		return storageformat.DomainTreeRoot{}, nil
	}
	summaryBuilder := newConsistencyDomainTreeBuilder(ctx, session)
	iterator, err := newConsistencyDomainTreeIterator(ctx, session, occurrences)
	if err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	var current storageformat.DuplicateProjectionSummary
	previousBlob := ""
	containerGroup := ""
	containerPossible := false
	flushSummary := func() error {
		if current.GroupID == "" {
			return nil
		}
		summary := current
		if containerPossible && current.OccurrenceCount > 1 {
			summary.ContainedBy = containerGroup
		}
		body, err := storageformat.EncodeCanonical(summary)
		if err != nil {
			return err
		}
		return summaryBuilder.Add(storageformat.DomainEntry{Key: duplicateProjectionSummaryKey008(summary.Kind, summary.GroupID), Value: body, LogicalVersion: storageformat.Digest(append([]byte("endlessfs-duplicate-projection-summary-v1\x00"), body...))})
	}
	for {
		entry, found, err := iterator.Next()
		if err != nil {
			return storageformat.DomainTreeRoot{}, err
		}
		if !found {
			break
		}
		if !strings.HasPrefix(entry.Key, "occurrence/") {
			continue
		}
		var occurrence storageformat.DuplicateProjectionOccurrence
		if err := decodeCanonicalValue(entry.Value, &occurrence); err != nil {
			return storageformat.DomainTreeRoot{}, err
		}
		if current.GroupID != occurrence.Occurrence.GroupID || current.Kind != occurrence.Occurrence.Kind {
			if err := flushSummary(); err != nil {
				return storageformat.DomainTreeRoot{}, err
			}
			current = storageformat.DuplicateProjectionSummary{SchemaVersion: 1, GroupID: occurrence.Occurrence.GroupID, Kind: occurrence.Occurrence.Kind, Size: occurrence.Occurrence.Size, FileCount: occurrence.Occurrence.FileCount}
			previousBlob = ""
			containerGroup, containerPossible = "", occurrence.Occurrence.Kind == domain.DuplicateDirectory
		}
		if current.Size != occurrence.Occurrence.Size || current.FileCount != occurrence.Occurrence.FileCount || current.OccurrenceCount == math.MaxInt64 {
			return storageformat.DomainTreeRoot{}, domain.NewError(domain.ErrorInvalid, "duplicate projection summary identity mismatch")
		}
		// Multiple namespace references to the same immutable blob consume no
		// additional provider storage. Count one representative per BlobID;
		// directory occurrences remain path-based structural duplicates.
		if occurrence.Occurrence.Kind == domain.DuplicateDirectory || previousBlob != occurrence.BlobID {
			current.OccurrenceCount++
			previousBlob = occurrence.BlobID
		}
		if occurrence.Occurrence.Kind == domain.DuplicateDirectory && containerPossible {
			path, err := domain.ParseUserPath(occurrence.Occurrence.Path)
			if err != nil || path.Parent().IsRoot() {
				containerPossible = false
				continue
			}
			parent, found, err := session.lookup(ctx, occurrences, duplicateProjectionLocationKey008(occurrence.Occurrence.Area, path.Parent().String()))
			if err != nil {
				return storageformat.DomainTreeRoot{}, err
			}
			if !found {
				containerPossible = false
				continue
			}
			var parentOccurrence storageformat.DuplicateProjectionOccurrence
			if err := decodeCanonicalValue(parent.Data, &parentOccurrence); err != nil || parentOccurrence.Occurrence.Kind != domain.DuplicateDirectory {
				return storageformat.DomainTreeRoot{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate parent projection")
			}
			if containerGroup == "" {
				containerGroup = parentOccurrence.Occurrence.GroupID
			} else if containerGroup != parentOccurrence.Occurrence.GroupID {
				containerPossible = false
			}
		}
	}
	if err := flushSummary(); err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	summaries, err := summaryBuilder.Finish()
	if err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	return mergeNamespaceProjectionRuns(ctx, session, []storageformat.DomainTreeRoot{summaries, occurrences})
}

func mergeDuplicateProjectionRuns008(ctx context.Context, session *consistencyDomainTreeSession, runs []storageformat.DomainTreeRoot) (storageformat.DomainTreeRoot, error) {
	for len(runs) > 1 {
		next := make([]storageformat.DomainTreeRoot, 0, (len(runs)+namespaceProjectionMergeFanIn-1)/namespaceProjectionMergeFanIn)
		for offset := 0; offset < len(runs); offset += namespaceProjectionMergeFanIn {
			end := min(offset+namespaceProjectionMergeFanIn, len(runs))
			root, err := mergeNamespaceProjectionRuns(ctx, session, runs[offset:end])
			if err != nil {
				return storageformat.DomainTreeRoot{}, err
			}
			next = append(next, root)
		}
		runs = next
	}
	if len(runs) == 0 {
		return storageformat.DomainTreeRoot{}, nil
	}
	return runs[0], nil
}

func (s *FileStore) walkDuplicateNamespace008(ctx context.Context, view *namespaceView, area domain.Area, path domain.UserPath, directory storageformat.NamespaceEntry, emit func(storageformat.DuplicateProjectionOccurrence) error) error {
	iterator, err := newConsistencyDomainTreeIterator(ctx, view.session, directory.Children)
	if err != nil {
		return err
	}
	for {
		value, found, err := iterator.Next()
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		entry, err := decodeNamespaceEntry(value.Value)
		if err != nil || entry.Entry.Name != value.Key {
			return domain.NewError(domain.ErrorInvalid, "invalid namespace occurrence projection source")
		}
		childPath, err := path.Join(entry.Entry.Name)
		if err != nil {
			return err
		}
		owner, err := domain.ParseUserID(view.reference.ID)
		if err != nil {
			return err
		}
		ownerScope, _ := domain.NewScope(owner, area)
		occurrence, err := catalogOccurrence(ownerScope, childPath, entry.Entry)
		if err != nil {
			return err
		}
		storedOccurrence := storageformat.DuplicateProjectionOccurrence{SchemaVersion: 1, Occurrence: occurrence}
		if entry.Entry.Kind == domain.EntryFile {
			storedOccurrence.BlobID = entry.Entry.BlobID
		}
		if err := emit(storedOccurrence); err != nil {
			return err
		}
		if entry.Entry.Kind == domain.EntryDirectory {
			if err := s.walkDuplicateNamespace008(ctx, view, area, childPath, entry, emit); err != nil {
				return err
			}
		}
	}
}

func (s *FileStore) listDuplicateGroups008(ctx context.Context, userID domain.UserID, request domain.DuplicateGroupRequest) (domain.DuplicateGroupPage, error) {
	limit := request.Limit
	if limit == 0 {
		limit = 100
	}
	if !userID.Valid() || limit < 1 || limit > 1000 || request.Kind != "" && !request.Kind.Valid() {
		return domain.DuplicateGroupPage{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate group page")
	}
	projection, err := s.duplicateProjection008(ctx, userID)
	if err != nil {
		return domain.DuplicateGroupPage{}, err
	}
	prefix := "group/"
	if request.Kind.Valid() {
		prefix += string(request.Kind) + "/"
	}
	after := ""
	if request.Cursor != "" {
		var cursor duplicateProjectionCursor008
		if err := s.decodeDuplicateCursor(request.Cursor, &cursor); err != nil || cursor.SchemaVersion != 1 || cursor.OwnerID != userID.String() || cursor.Kind != request.Kind || cursor.IncludeIgnored != request.IncludeIgnored || cursor.Limit != limit || cursor.ProjectionID != projection.projectionID || !s.engine.clock.Now().Before(cursor.ExpiresAt) {
			return domain.DuplicateGroupPage{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate projection cursor")
		}
		projection.root, after = cursor.Root, cursor.After
	}
	values, err := projection.session.collect(ctx, projection.root, prefix, after, limit+1)
	if err != nil {
		return domain.DuplicateGroupPage{}, err
	}
	result := domain.DuplicateGroupPage{Groups: make([]domain.DuplicateGroup, 0, limit)}
	consumed := 0
	for _, value := range values {
		var summary storageformat.DuplicateProjectionSummary
		if err := decodeCanonicalValue(value.Value, &summary); err != nil || storageformat.ValidateDuplicateProjectionSummary(summary) != nil || value.Key != duplicateProjectionSummaryKey008(summary.Kind, summary.GroupID) {
			return domain.DuplicateGroupPage{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate projection group")
		}
		consumed++
		if summary.OccurrenceCount < 2 {
			continue
		}
		if summary.Kind == domain.DuplicateDirectory && summary.ContainedBy != "" {
			continue
		}
		ignored, revision, err := s.duplicateGroupIgnoreState008(ctx, userID, summary.GroupID)
		if err != nil {
			return domain.DuplicateGroupPage{}, err
		}
		if ignored && !request.IncludeIgnored {
			continue
		}
		reclaimable := summary.Size * (summary.OccurrenceCount - 1)
		result.Groups = append(result.Groups, domain.DuplicateGroup{ID: summary.GroupID, Kind: summary.Kind, OccurrenceCount: summary.OccurrenceCount, Size: summary.Size, FileCount: summary.FileCount, ReclaimableBytes: reclaimable, Ignored: ignored, IgnoreRevision: revision})
		if len(result.Groups) == limit {
			break
		}
	}
	if consumed < len(values) || len(values) == limit+1 {
		bound := values[max(0, consumed-1)].Key
		cursor := duplicateProjectionCursor008{SchemaVersion: 1, OwnerID: userID.String(), Kind: request.Kind, IncludeIgnored: request.IncludeIgnored, Limit: limit, ProjectionID: projection.projectionID, Root: projection.root, After: bound, ExpiresAt: s.engine.clock.Now().UTC().Add(s.engine.cursorTTL)}
		result.NextCursor, err = s.encodeDuplicateCursor(cursor)
	}
	return result, err
}

func (s *FileStore) listDuplicateOccurrences008(ctx context.Context, userID domain.UserID, request domain.DuplicateOccurrenceRequest) (domain.DuplicateOccurrencePage, error) {
	limit := request.Limit
	if limit == 0 {
		limit = 100
	}
	if !userID.Valid() || validateDuplicateGroupID(request.GroupID) != nil || limit < 1 || limit > 1000 {
		return domain.DuplicateOccurrencePage{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate occurrence page")
	}
	projection, err := s.duplicateProjection008(ctx, userID)
	if err != nil {
		return domain.DuplicateOccurrencePage{}, err
	}
	kind, after := domain.DuplicateKind(""), ""
	if request.Cursor != "" {
		var cursor duplicateProjectionCursor008
		if err := s.decodeDuplicateCursor(request.Cursor, &cursor); err != nil || cursor.SchemaVersion != 1 || cursor.OwnerID != userID.String() || cursor.GroupID != request.GroupID || cursor.Limit != limit || cursor.ProjectionID != projection.projectionID || !cursor.Kind.Valid() || !s.engine.clock.Now().Before(cursor.ExpiresAt) {
			return domain.DuplicateOccurrencePage{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate occurrence cursor")
		}
		projection.root, kind, after = cursor.Root, cursor.Kind, cursor.After
	}
	// The group digest is globally domain-separated, so at most one kind can
	// match. Resolve it from the authenticated projection when this is page one.
	if kind == "" {
		for _, candidate := range []domain.DuplicateKind{domain.DuplicateFile, domain.DuplicateDirectory} {
			if _, found, err := projection.session.lookup(ctx, projection.root, duplicateProjectionSummaryKey008(candidate, request.GroupID)); err != nil {
				return domain.DuplicateOccurrencePage{}, err
			} else if found {
				kind = candidate
				break
			}
		}
	}
	if kind == "" {
		return domain.DuplicateOccurrencePage{}, domain.NewError(domain.ErrorNotFound, "duplicate group does not exist")
	}
	values, err := projection.session.collect(ctx, projection.root, "occurrence/"+string(kind)+"/"+request.GroupID+"/", after, limit+1)
	if err != nil {
		return domain.DuplicateOccurrencePage{}, err
	}
	hasMore := len(values) > limit
	if len(values) > limit {
		values = values[:limit]
	}
	result := domain.DuplicateOccurrencePage{Occurrences: make([]domain.DuplicateOccurrence, 0, len(values))}
	for _, value := range values {
		var stored storageformat.DuplicateProjectionOccurrence
		if err := decodeCanonicalValue(value.Value, &stored); err != nil || duplicateProjectionOccurrenceKey008(stored) != value.Key {
			return domain.DuplicateOccurrencePage{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate projection occurrence")
		}
		occurrence, err := domainDuplicateOccurrence(stored.Occurrence)
		if err != nil {
			return domain.DuplicateOccurrencePage{}, err
		}
		result.Occurrences = append(result.Occurrences, occurrence)
	}
	if hasMore {
		cursor := duplicateProjectionCursor008{SchemaVersion: 1, OwnerID: userID.String(), Kind: kind, GroupID: request.GroupID, Limit: limit, ProjectionID: projection.projectionID, Root: projection.root, After: values[len(values)-1].Key, ExpiresAt: s.engine.clock.Now().UTC().Add(s.engine.cursorTTL)}
		result.NextCursor, err = s.encodeDuplicateCursor(cursor)
		if err != nil {
			return domain.DuplicateOccurrencePage{}, err
		}
	}
	return result, nil
}

func duplicateIgnoreKey008(groupID string) string { return "duplicates/ignore/group/" + groupID }

func (s *FileStore) duplicateGroupIgnoreState008(ctx context.Context, owner domain.UserID, groupID string) (bool, uint64, error) {
	value, err := newConsistencyDomainStore(s.engine.backend, s.engine.scheduler, s.engine.clock).get(ctx, namespaceReference(owner), duplicateIgnoreKey008(groupID))
	if errors.Is(err, domain.ErrNotFound) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	var record domain.DuplicateIgnore
	if err := decodeCanonicalValue(value.Data, &record); err != nil || record.GroupID != groupID || record.Revision == 0 {
		return false, 0, domain.NewError(domain.ErrorInvalid, "invalid duplicate ignore authority")
	}
	return record.Ignored, record.Revision, nil
}

func (s *FileStore) setDuplicateGroupIgnored008(ctx context.Context, owner domain.UserID, request domain.SetDuplicateIgnoredRequest) (domain.DuplicateIgnore, error) {
	if !owner.Valid() || validateDuplicateGroupID(request.GroupID) != nil {
		return domain.DuplicateIgnore{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate ignore request")
	}
	store := newConsistencyDomainStore(s.engine.backend, s.engine.scheduler, s.engine.clock)
	reference := namespaceReference(owner)
	key := duplicateIgnoreKey008(request.GroupID)
	current, err := store.get(ctx, reference, key)
	revision := uint64(1)
	requirement, expected := domainValueAbsent, ""
	if err == nil {
		var record domain.DuplicateIgnore
		if err := decodeCanonicalValue(current.Data, &record); err != nil || record.GroupID != request.GroupID || record.Revision == 0 {
			return domain.DuplicateIgnore{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate ignore authority")
		}
		if request.ExpectedRevision != 0 && request.ExpectedRevision != record.Revision {
			return domain.DuplicateIgnore{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate ignore revision changed")
		}
		if record.Ignored == request.Ignored {
			return record, nil
		}
		revision, requirement, expected = record.Revision+1, domainValuePresent, current.LogicalVersion
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.DuplicateIgnore{}, err
	} else if request.ExpectedRevision != 0 {
		return domain.DuplicateIgnore{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate ignore authority is missing")
	}
	record := domain.DuplicateIgnore{GroupID: request.GroupID, Ignored: request.Ignored, Revision: revision}
	body, err := storageformat.EncodeCanonical(record)
	if err != nil {
		return domain.DuplicateIgnore{}, err
	}
	mutationID, err := s.engine.ids.OpaqueID()
	if err != nil {
		return domain.DuplicateIgnore{}, err
	}
	_, err = store.mutate(ctx, reference, consistencyDomainMutation{ID: mutationID, Changes: []consistencyDomainChange{{Key: key, Require: requirement, ExpectedVersion: expected, Value: body}}})
	return record, err
}

type duplicateDirectoryContent008 struct {
	occurrence domain.DuplicateOccurrence
	nodeID     string
	identity   string
	root       storageformat.DomainTreeRoot
	sourceRoot storageformat.DomainTreeRoot
	sketch     []string
	session    *consistencyDomainTreeSession
}

func duplicateDirectoryContentProjectionID008(owner domain.UserID, identity string) string {
	return storageformat.Digest([]byte("endlessfs-duplicate-directory-content-v2\x00" + owner.String() + "\x00" + identity))
}

func (s *FileStore) loadDuplicateDirectoryContentProjection008(ctx context.Context, owner domain.UserID, projectionID string) (namespaceProjectionSnapshot, error) {
	key := storageformat.ScopedProjectionHeadKey(owner.String(), storageformat.ProjectionDuplicates, projectionID)
	object, err := s.engine.backend.Get(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		return namespaceProjectionSnapshot{}, nil
	}
	if err != nil {
		return namespaceProjectionSnapshot{}, err
	}
	snapshot := namespaceProjectionSnapshot{object: object, exists: true}
	if err := storageformat.DecodeEnvelope(object.Body, key, duplicateProjectionHeadSchema, &snapshot.envelope, &snapshot.head); err != nil {
		return snapshot, nil
	}
	if err := storageformat.ValidateProjectionHead(snapshot.head); err != nil || snapshot.head.OwnerID != owner.String() || snapshot.head.ProjectionID != projectionID || snapshot.head.Kind != storageformat.ProjectionDuplicates {
		return snapshot, nil
	}
	snapshot.valid = true
	return snapshot, nil
}

func (s *FileStore) resolveNamespaceAtView008(ctx context.Context, view *namespaceView, area domain.Area, path domain.UserPath) (storageformat.NamespaceEntry, error) {
	entry, _, err := s.resolveNamespaceOccurrenceAtView008(ctx, view, area, path)
	return entry, err
}

func (s *FileStore) resolveNamespaceOccurrenceAtView008(ctx context.Context, view *namespaceView, area domain.Area, path domain.UserPath) (storageformat.NamespaceEntry, string, error) {
	if path.IsRoot() {
		return view.roots[area], "", nil
	}
	store := newNamespaceStore(s.engine)
	current := view.roots[area]
	contextID := ""
	for _, segment := range path.Segments() {
		entry, found, err := store.child(ctx, view, current, segment)
		if err != nil {
			return storageformat.NamespaceEntry{}, "", err
		}
		if !found {
			return storageformat.NamespaceEntry{}, "", domain.NewError(domain.ErrorNotFound, "duplicate directory does not exist")
		}
		if contextID == "" && entry.OccurrenceContextID != "" {
			contextID = entry.OccurrenceContextID
		}
		current = entry
	}
	identity := areaName(area) + ":" + current.NodeID
	if contextID != "" {
		identity = areaName(area) + ":" + contextID + ":" + current.NodeID
	}
	return current, identity, nil
}

func mergeDuplicateSketch008(current []string, groupID string) ([]string, error) {
	incoming, err := directoryContentSketch([]storageformat.DirectoryContentIndexEntry{{GroupID: groupID}}, nil)
	if err != nil {
		return nil, err
	}
	if len(current) == 0 {
		return incoming, nil
	}
	if validateDirectoryContentSketch(current) != nil {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid duplicate content sketch")
	}
	for index := range current {
		if incoming[index] < current[index] {
			current[index] = incoming[index]
		}
	}
	return current, nil
}

func (s *FileStore) duplicateDirectoryContent008(ctx context.Context, view *namespaceView, owner domain.UserID, location domain.DuplicateLocation) (duplicateDirectoryContent008, error) {
	entry, identity, err := s.resolveNamespaceOccurrenceAtView008(ctx, view, location.Area, location.Path)
	if err != nil {
		return duplicateDirectoryContent008{}, err
	}
	if entry.Entry.Kind != domain.EntryDirectory || location.Path.IsRoot() {
		return duplicateDirectoryContent008{}, domain.NewError(domain.ErrorInvalid, "duplicate location is not a selectable directory")
	}
	scope, _ := domain.NewScope(owner, location.Area)
	storedOccurrence, err := catalogOccurrence(scope, location.Path, entry.Entry)
	if err != nil {
		return duplicateDirectoryContent008{}, err
	}
	occurrence, err := domainDuplicateOccurrence(storedOccurrence)
	if err != nil {
		return duplicateDirectoryContent008{}, err
	}
	projectionID := duplicateDirectoryContentProjectionID008(owner, identity)
	for {
		snapshot, err := s.loadDuplicateDirectoryContentProjection008(ctx, owner, projectionID)
		if err != nil {
			return duplicateDirectoryContent008{}, err
		}
		if snapshot.valid && snapshot.head.SourceDomainID == view.reference.ID && snapshot.head.SourceRoot == entry.Children {
			sketch, err := duplicateContentSketch008(ctx, duplicateProjectionSession008(s.engine, owner, projectionID), snapshot.head.Root)
			if err != nil {
				return duplicateDirectoryContent008{}, err
			}
			return duplicateDirectoryContent008{occurrence: occurrence, nodeID: entry.NodeID, identity: identity, root: snapshot.head.Root, sourceRoot: entry.Children, sketch: sketch, session: duplicateProjectionSession008(s.engine, owner, projectionID)}, nil
		}
		content, err := s.buildDuplicateDirectoryContent008(ctx, view, owner, location, entry, occurrence, identity, projectionID)
		if err != nil {
			return duplicateDirectoryContent008{}, err
		}
		next := storageformat.ProjectionHead{SchemaVersion: 1, OwnerID: owner.String(), ProjectionID: projectionID, Kind: storageformat.ProjectionDuplicates, SourceDomainID: view.reference.ID, SourceRevision: view.head.Revision, SourceRoot: entry.Children, Root: content.root}
		body, err := storageformat.EncodeEnvelope(duplicateProjectionHeadSchema, storageformat.ScopedProjectionHeadKey(owner.String(), storageformat.ProjectionDuplicates, projectionID), max(uint64(1), snapshot.envelope.Revision+1), next)
		if err != nil {
			return duplicateDirectoryContent008{}, err
		}
		condition := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
		if snapshot.exists {
			condition = objectstore.PutCondition{Mode: objectstore.PutMatch, Version: snapshot.object.Version}
		}
		if _, err := s.engine.backend.Put(ctx, storageformat.ScopedProjectionHeadKey(owner.String(), storageformat.ProjectionDuplicates, projectionID), body, condition); err == nil {
			return content, nil
		} else if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return duplicateDirectoryContent008{}, err
		}
	}
}

func duplicateContentSketch008(ctx context.Context, session *consistencyDomainTreeSession, root storageformat.DomainTreeRoot) ([]string, error) {
	iterator, err := newConsistencyDomainTreeIterator(ctx, session, root)
	if err != nil {
		return nil, err
	}
	var sketch []string
	lastGroup := ""
	for {
		entry, found, err := iterator.Next()
		if err != nil || !found {
			return sketch, err
		}
		groupID := duplicateContentGroup008(entry)
		if groupID == "" {
			return nil, domain.NewError(domain.ErrorInvalid, "invalid duplicate content projection key")
		}
		if groupID != lastGroup {
			sketch, err = mergeDuplicateSketch008(sketch, groupID)
			if err != nil {
				return nil, err
			}
			lastGroup = groupID
		}
	}
}

func (s *FileStore) buildDuplicateDirectoryContent008(ctx context.Context, view *namespaceView, owner domain.UserID, location domain.DuplicateLocation, entry storageformat.NamespaceEntry, occurrence domain.DuplicateOccurrence, identity, projectionID string) (duplicateDirectoryContent008, error) {
	session := duplicateProjectionSession008(s.engine, owner, projectionID)
	scope, err := domain.NewScope(owner, location.Area)
	if err != nil {
		return duplicateDirectoryContent008{}, err
	}
	chunk := make([]storageformat.DomainEntry, 0, domainPageMaximumItems)
	runs := make([]storageformat.DomainTreeRoot, 0)
	sketch := []string(nil)
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		sort.Slice(chunk, func(left, right int) bool { return chunk[left].Key < chunk[right].Key })
		root, err := session.buildTree(ctx, chunk)
		if err != nil {
			return err
		}
		runs = append(runs, root)
		session.pages = make(map[string]storageformat.DomainPage)
		chunk = chunk[:0]
		return nil
	}
	var walk func(storageformat.NamespaceEntry, domain.UserPath) error
	walk = func(directory storageformat.NamespaceEntry, path domain.UserPath) error {
		iterator, err := newConsistencyDomainTreeIterator(ctx, view.session, directory.Children)
		if err != nil {
			return err
		}
		for {
			value, found, err := iterator.Next()
			if err != nil {
				return err
			}
			if !found {
				return nil
			}
			child, err := decodeNamespaceEntry(value.Value)
			if err != nil {
				return err
			}
			childPath, err := path.Join(child.Entry.Name)
			if err != nil {
				return err
			}
			if child.Entry.Kind == domain.EntryDirectory {
				if err := walk(child, childPath); err != nil {
					return err
				}
				continue
			}
			groupID, err := duplicateFileGroupID(child.Entry)
			if err != nil {
				return err
			}
			sketch, err = mergeDuplicateSketch008(sketch, groupID)
			if err != nil {
				return err
			}
			fileOccurrence, err := catalogOccurrence(scope, childPath, child.Entry)
			if err != nil {
				return err
			}
			stored := storageformat.DuplicateProjectionOccurrence{SchemaVersion: 1, Occurrence: fileOccurrence, BlobID: child.Entry.BlobID}
			body, err := storageformat.EncodeCanonical(stored)
			if err != nil {
				return err
			}
			relative := strings.TrimPrefix(childPath.String(), location.Path.String()+"/")
			key := groupID + "/" + base64.RawURLEncoding.EncodeToString([]byte(relative))
			chunk = append(chunk, storageformat.DomainEntry{Key: key, Value: body, LogicalVersion: storageformat.Digest(append([]byte("endlessfs-directory-content-projection-v1\x00"), body...))})
			if len(chunk) == domainPageMaximumItems {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
	if err := walk(entry, location.Path); err != nil {
		return duplicateDirectoryContent008{}, err
	}
	if err := flush(); err != nil {
		return duplicateDirectoryContent008{}, err
	}
	root, err := mergeDuplicateProjectionRuns008(ctx, session, runs)
	if err != nil {
		return duplicateDirectoryContent008{}, err
	}
	return duplicateDirectoryContent008{occurrence: occurrence, nodeID: entry.NodeID, identity: identity, root: root, sourceRoot: entry.Children, sketch: sketch, session: session}, nil
}

func duplicateContentGroup008(entry storageformat.DomainEntry) string {
	if index := strings.IndexByte(entry.Key, '/'); index > 0 {
		return entry.Key[:index]
	}
	return ""
}

func compareDuplicateContentRoots008(ctx context.Context, left, right duplicateDirectoryContent008) (domain.DuplicateDirectoryComparison, error) {
	comparison := domain.DuplicateDirectoryComparison{Left: left.occurrence, Right: right.occurrence}
	leftIterator, err := newConsistencyDomainTreeIterator(ctx, left.session, left.root)
	if err != nil {
		return comparison, err
	}
	rightIterator, err := newConsistencyDomainTreeIterator(ctx, right.session, right.root)
	if err != nil {
		return comparison, err
	}
	leftValue, leftFound, err := leftIterator.Next()
	if err != nil {
		return comparison, err
	}
	rightValue, rightFound, err := rightIterator.Next()
	if err != nil {
		return comparison, err
	}
	for leftFound || rightFound {
		leftGroup, rightGroup := duplicateContentGroup008(leftValue), duplicateContentGroup008(rightValue)
		switch {
		case !rightFound || leftFound && leftGroup < rightGroup:
			var stored storageformat.DuplicateProjectionOccurrence
			if err := decodeCanonicalValue(leftValue.Value, &stored); err != nil || addDuplicateTotals(&comparison.LeftOnlyFiles, &comparison.LeftOnlyBytes, 1, stored.Occurrence.Size) != nil {
				return comparison, domain.NewError(domain.ErrorInvalid, "invalid left duplicate content projection")
			}
			leftValue, leftFound, err = leftIterator.Next()
		case !leftFound || rightGroup < leftGroup:
			var stored storageformat.DuplicateProjectionOccurrence
			if err := decodeCanonicalValue(rightValue.Value, &stored); err != nil || addDuplicateTotals(&comparison.RightOnlyFiles, &comparison.RightOnlyBytes, 1, stored.Occurrence.Size) != nil {
				return comparison, domain.NewError(domain.ErrorInvalid, "invalid right duplicate content projection")
			}
			rightValue, rightFound, err = rightIterator.Next()
		default:
			var leftStored, rightStored storageformat.DuplicateProjectionOccurrence
			if decodeCanonicalValue(leftValue.Value, &leftStored) != nil || decodeCanonicalValue(rightValue.Value, &rightStored) != nil || leftStored.Occurrence.Size != rightStored.Occurrence.Size || addDuplicateTotals(&comparison.CommonFiles, &comparison.CommonBytes, 1, leftStored.Occurrence.Size) != nil {
				return comparison, domain.NewError(domain.ErrorInvalid, "invalid common duplicate content projection")
			}
			leftValue, leftFound, err = leftIterator.Next()
			if err == nil {
				rightValue, rightFound, err = rightIterator.Next()
			}
		}
		if err != nil {
			return comparison, err
		}
	}
	comparison.Exact = comparison.LeftOnlyFiles == 0 && comparison.RightOnlyFiles == 0 && comparison.Left.GroupID == comparison.Right.GroupID
	return comparison, nil
}

func (s *FileStore) compareDuplicateDirectories008(ctx context.Context, owner domain.UserID, request domain.DuplicateDirectoryComparisonRequest) (domain.DuplicateDirectoryComparison, []string, []string, error) {
	if !owner.Valid() || request.Left.Area != domain.AreaLive && request.Left.Area != domain.AreaTrash || request.Right.Area != domain.AreaLive && request.Right.Area != domain.AreaTrash || !request.Left.Path.Valid() || !request.Right.Path.Valid() {
		return domain.DuplicateDirectoryComparison{}, nil, nil, domain.NewError(domain.ErrorInvalid, "invalid duplicate directory comparison")
	}
	view, err := newNamespaceStore(s.engine).loadView(ctx, owner, "")
	if err != nil {
		return domain.DuplicateDirectoryComparison{}, nil, nil, err
	}
	left, err := s.duplicateDirectoryContent008(ctx, view, owner, request.Left)
	if err != nil {
		return domain.DuplicateDirectoryComparison{}, nil, nil, err
	}
	right, err := s.duplicateDirectoryContent008(ctx, view, owner, request.Right)
	if err != nil {
		return domain.DuplicateDirectoryComparison{}, nil, nil, err
	}
	comparison, err := compareDuplicateContentRoots008(ctx, left, right)
	return comparison, left.sketch, right.sketch, err
}

func duplicateDirectoryPreference008(leftIdentity, rightIdentity string) (storageformat.DuplicateDirectoryPreference, error) {
	if leftIdentity == rightIdentity {
		return storageformat.DuplicateDirectoryPreference{}, domain.NewError(domain.ErrorInvalid, "duplicate directory pair must differ")
	}
	if leftIdentity > rightIdentity {
		leftIdentity, rightIdentity = rightIdentity, leftIdentity
	}
	pairID := storageformat.Digest([]byte("endlessfs-duplicate-directory-pair-v2\x00" + leftIdentity + "\x00" + rightIdentity))
	return storageformat.DuplicateDirectoryPreference{SchemaVersion: 1, PairID: pairID, LeftIdentity: leftIdentity, RightIdentity: rightIdentity}, nil
}

func duplicateDirectoryIgnoreKey008(pairID string) string { return "duplicates/ignore/pair/" + pairID }

func (s *FileStore) setDuplicateDirectoryIgnored008(ctx context.Context, owner domain.UserID, request domain.SetDuplicateDirectoryIgnoredRequest) (domain.DuplicateDirectoryIgnore, error) {
	if !owner.Valid() || !request.Left.Path.Valid() || !request.Right.Path.Valid() {
		return domain.DuplicateDirectoryIgnore{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate directory preference")
	}
	view, err := newNamespaceStore(s.engine).loadView(ctx, owner, "")
	if err != nil {
		return domain.DuplicateDirectoryIgnore{}, err
	}
	left, leftIdentity, err := s.resolveNamespaceOccurrenceAtView008(ctx, view, request.Left.Area, request.Left.Path)
	if err != nil || left.Entry.Kind != domain.EntryDirectory {
		return domain.DuplicateDirectoryIgnore{}, domain.NewError(domain.ErrorInvalid, "left duplicate preference location is not a directory")
	}
	right, rightIdentity, err := s.resolveNamespaceOccurrenceAtView008(ctx, view, request.Right.Area, request.Right.Path)
	if err != nil || right.Entry.Kind != domain.EntryDirectory {
		return domain.DuplicateDirectoryIgnore{}, domain.NewError(domain.ErrorInvalid, "right duplicate preference location is not a directory")
	}
	preference, err := duplicateDirectoryPreference008(leftIdentity, rightIdentity)
	if err != nil {
		return domain.DuplicateDirectoryIgnore{}, err
	}
	store := newConsistencyDomainStore(s.engine.backend, s.engine.scheduler, s.engine.clock)
	reference := namespaceReference(owner)
	key := duplicateDirectoryIgnoreKey008(preference.PairID)
	current, err := store.get(ctx, reference, key)
	requirement, expected := domainValueAbsent, ""
	preference.Revision = 1
	if err == nil {
		var stored storageformat.DuplicateDirectoryPreference
		if err := decodeCanonicalValue(current.Data, &stored); err != nil || storageformat.ValidateDuplicateDirectoryPreference(stored) != nil || stored.PairID != preference.PairID {
			return domain.DuplicateDirectoryIgnore{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate directory preference authority")
		}
		if request.ExpectedRevision != 0 && request.ExpectedRevision != stored.Revision {
			return domain.DuplicateDirectoryIgnore{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate directory preference changed")
		}
		if stored.Ignored == request.Ignored {
			return domain.DuplicateDirectoryIgnore{Ignored: stored.Ignored, Revision: stored.Revision}, nil
		}
		preference.Revision, requirement, expected = stored.Revision+1, domainValuePresent, current.LogicalVersion
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.DuplicateDirectoryIgnore{}, err
	} else if request.ExpectedRevision != 0 {
		return domain.DuplicateDirectoryIgnore{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate directory preference is missing")
	}
	preference.Ignored = request.Ignored
	body, err := storageformat.EncodeCanonical(preference)
	if err != nil {
		return domain.DuplicateDirectoryIgnore{}, err
	}
	mutationID, err := s.engine.ids.OpaqueID()
	if err != nil {
		return domain.DuplicateDirectoryIgnore{}, err
	}
	_, err = store.mutate(ctx, reference, consistencyDomainMutation{ID: mutationID, Changes: []consistencyDomainChange{{Key: key, Require: requirement, ExpectedVersion: expected, Value: body}}})
	return domain.DuplicateDirectoryIgnore{Ignored: preference.Ignored, Revision: preference.Revision}, err
}

func sharedDuplicateSketch008(left, right []string) int {
	if len(left) != directoryContentSketchSize || len(right) != directoryContentSketchSize {
		return 0
	}
	shared := 0
	for index := range left {
		if left[index] == right[index] {
			shared++
		}
	}
	return shared
}

func (s *FileStore) listDuplicateDirectoryOverlaps008(ctx context.Context, owner domain.UserID, request domain.DuplicateDirectoryOverlapRequest) (domain.DuplicateDirectoryOverlapPage, error) {
	limit := request.Limit
	if limit == 0 {
		limit = 50
	}
	if !owner.Valid() || request.Directory.Area != domain.AreaLive && request.Directory.Area != domain.AreaTrash || !request.Directory.Path.Valid() || request.Directory.Path.IsRoot() || limit < 1 || limit > 100 {
		return domain.DuplicateDirectoryOverlapPage{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate overlap request")
	}
	projection, err := s.duplicateProjection008(ctx, owner)
	if err != nil {
		return domain.DuplicateDirectoryOverlapPage{}, err
	}
	view, err := newNamespaceStore(s.engine).loadView(ctx, owner, "")
	if err != nil {
		return domain.DuplicateDirectoryOverlapPage{}, err
	}
	selectedEntry, selectedIdentity, err := s.resolveNamespaceOccurrenceAtView008(ctx, view, request.Directory.Area, request.Directory.Path)
	if err != nil {
		return domain.DuplicateDirectoryOverlapPage{}, err
	}
	if selectedEntry.Entry.Kind != domain.EntryDirectory {
		return domain.DuplicateDirectoryOverlapPage{}, domain.NewError(domain.ErrorInvalid, "duplicate overlap location is not a directory")
	}
	cursor := duplicateOverlapCursor008{SchemaVersion: 8, OwnerID: owner.String(), Directory: request.Directory, IncludeIgnored: request.IncludeIgnored, Limit: limit, ProjectionID: projection.projectionID, ProjectionRoot: projection.root, NodeID: selectedEntry.NodeID, DirectoryRoot: selectedEntry.Children, ExpiresAt: s.engine.clock.Now().UTC().Add(s.engine.cursorTTL)}
	if request.Cursor != "" {
		if err := s.decodeDuplicateCursor(request.Cursor, &cursor); err != nil || cursor.SchemaVersion != 8 || cursor.OwnerID != owner.String() || cursor.Directory != request.Directory || cursor.IncludeIgnored != request.IncludeIgnored || cursor.Limit != limit || cursor.ProjectionID != projection.projectionID || cursor.NodeID != selectedEntry.NodeID || cursor.DirectoryRoot != selectedEntry.Children || !s.engine.clock.Now().Before(cursor.ExpiresAt) {
			return domain.DuplicateDirectoryOverlapPage{}, domain.NewError(domain.ErrorInvalid, "invalid or stale duplicate overlap cursor")
		}
		projection.root = cursor.ProjectionRoot
	}
	ignore, err := s.newDuplicateIgnoreReader008(ctx, owner)
	if err != nil {
		return domain.DuplicateDirectoryOverlapPage{}, err
	}
	result := domain.DuplicateDirectoryOverlapPage{Candidates: make([]domain.DuplicateDirectoryOverlapCandidate, 0, limit)}
	for {
		values, err := projection.session.collect(ctx, projection.root, "location/", cursor.After, domainPageMaximumItems)
		if err != nil {
			return domain.DuplicateDirectoryOverlapPage{}, err
		}
		if len(values) == 0 {
			break
		}
		for _, value := range values {
			cursor.After = value.Key
			var stored storageformat.DuplicateProjectionOccurrence
			if err := decodeCanonicalValue(value.Value, &stored); err != nil {
				return domain.DuplicateDirectoryOverlapPage{}, err
			}
			candidate, err := domainDuplicateOccurrence(stored.Occurrence)
			if err != nil || candidate.Path == request.Directory.Path && candidate.Area == request.Directory.Area {
				continue
			}
			candidateLocation := domain.DuplicateLocation{Area: candidate.Area, Path: candidate.Path}
			comparison, leftSketch, rightSketch, err := s.compareDuplicateDirectories008(ctx, owner, domain.DuplicateDirectoryComparisonRequest{Left: request.Directory, Right: candidateLocation})
			if err != nil {
				return domain.DuplicateDirectoryOverlapPage{}, err
			}
			if comparison.CommonFiles == 0 {
				continue
			}
			candidateEntry, candidateIdentity, err := s.resolveNamespaceOccurrenceAtView008(ctx, view, candidate.Area, candidate.Path)
			if err != nil || candidateEntry.Entry.Kind != domain.EntryDirectory {
				if err == nil {
					err = domain.NewError(domain.ErrorInvalid, "duplicate overlap candidate is not a directory")
				}
				return domain.DuplicateDirectoryOverlapPage{}, err
			}
			preference, err := duplicateDirectoryPreference008(selectedIdentity, candidateIdentity)
			if err != nil {
				return domain.DuplicateDirectoryOverlapPage{}, err
			}
			ignored, ignoreRevision, err := ignore.pair(ctx, preference)
			if err != nil {
				return domain.DuplicateDirectoryOverlapPage{}, err
			}
			if ignored && !request.IncludeIgnored {
				continue
			}
			exactIgnored, exactRevision := false, uint64(0)
			if comparison.Exact {
				exactIgnored, exactRevision, err = ignore.group(ctx, comparison.Left.GroupID)
				if err != nil {
					return domain.DuplicateDirectoryOverlapPage{}, err
				}
				if exactIgnored && !request.IncludeIgnored {
					continue
				}
			}
			result.Candidates = append(result.Candidates, domain.DuplicateDirectoryOverlapCandidate{SharedSketch: sharedDuplicateSketch008(leftSketch, rightSketch), SketchSize: directoryContentSketchSize, Ignored: ignored, IgnoreRevision: ignoreRevision, ExactGroupIgnored: exactIgnored, ExactGroupIgnoreRevision: exactRevision, Comparison: comparison})
			if len(result.Candidates) == limit {
				result.NextCursor, err = s.encodeDuplicateCursor(cursor)
				return result, err
			}
		}
		if len(values) < domainPageMaximumItems {
			break
		}
	}
	return result, nil
}

type duplicateIgnoreReader008 struct {
	store     *consistencyDomainStore
	reference consistencyDomainRef
	head      storageformat.DomainHead
}

func (s *FileStore) newDuplicateIgnoreReader008(ctx context.Context, owner domain.UserID) (duplicateIgnoreReader008, error) {
	store := newConsistencyDomainStore(s.engine.backend, s.engine.scheduler, s.engine.clock)
	reference := namespaceReference(owner)
	snapshot, err := store.loadHead(ctx, reference)
	if err != nil {
		return duplicateIgnoreReader008{}, err
	}
	return duplicateIgnoreReader008{store: store, reference: reference, head: snapshot.head}, nil
}

func (s *FileStore) duplicateIgnoreReaderAtNamespaceView008(view *namespaceView) duplicateIgnoreReader008 {
	return duplicateIgnoreReader008{store: newConsistencyDomainStore(s.engine.backend, s.engine.scheduler, s.engine.clock), reference: view.reference, head: view.head}
}

func (reader duplicateIgnoreReader008) group(ctx context.Context, groupID string) (bool, uint64, error) {
	if !reader.head.Registered {
		return false, 0, nil
	}
	value, found, err := reader.store.lookupAtHead(ctx, reader.reference, reader.head, duplicateIgnoreKey008(groupID))
	if err != nil || !found {
		return false, 0, err
	}
	var record domain.DuplicateIgnore
	if err := decodeCanonicalValue(value.Data, &record); err != nil || record.GroupID != groupID || record.Revision == 0 {
		return false, 0, domain.NewError(domain.ErrorInvalid, "invalid duplicate ignore authority")
	}
	return record.Ignored, record.Revision, nil
}

func (reader duplicateIgnoreReader008) pair(ctx context.Context, preference storageformat.DuplicateDirectoryPreference) (bool, uint64, error) {
	if !reader.head.Registered {
		return false, 0, nil
	}
	value, found, err := reader.store.lookupAtHead(ctx, reader.reference, reader.head, duplicateDirectoryIgnoreKey008(preference.PairID))
	if err != nil || !found {
		return false, 0, err
	}
	var stored storageformat.DuplicateDirectoryPreference
	if err := decodeCanonicalValue(value.Data, &stored); err != nil || storageformat.ValidateDuplicateDirectoryPreference(stored) != nil || stored.PairID != preference.PairID || stored.LeftIdentity != preference.LeftIdentity || stored.RightIdentity != preference.RightIdentity {
		return false, 0, domain.NewError(domain.ErrorInvalid, "invalid duplicate directory preference authority")
	}
	return stored.Ignored, stored.Revision, nil
}

func nextDuplicateContent008(iterator *consistencyDomainTreeIterator) (storageformat.DomainEntry, storageformat.DuplicateProjectionOccurrence, bool, error) {
	entry, found, err := iterator.Next()
	if err != nil || !found {
		return storageformat.DomainEntry{}, storageformat.DuplicateProjectionOccurrence{}, found, err
	}
	var stored storageformat.DuplicateProjectionOccurrence
	if err := decodeCanonicalValue(entry.Value, &stored); err != nil || storageformat.ValidateDuplicateProjectionOccurrence(stored) != nil || duplicateContentGroup008(entry) != stored.Occurrence.GroupID {
		return storageformat.DomainEntry{}, storageformat.DuplicateProjectionOccurrence{}, false, domain.NewError(domain.ErrorInvalid, "invalid duplicate reconciliation projection entry")
	}
	return entry, stored, true, nil
}

func (s *FileStore) reconciliationItems008(ctx context.Context, owner domain.UserID, left, right duplicateDirectoryContent008, removeFrom domain.DuplicateSide, leftAfter, rightAfter string, limit int) ([]domain.DuplicateReconciliationItem, string, string, bool, error) {
	leftIterator, err := newConsistencyDomainTreeIteratorAfter(ctx, left.session, left.root, leftAfter)
	if err != nil {
		return nil, "", "", false, err
	}
	rightIterator, err := newConsistencyDomainTreeIteratorAfter(ctx, right.session, right.root, rightAfter)
	if err != nil {
		return nil, "", "", false, err
	}
	ignore, err := s.newDuplicateIgnoreReader008(ctx, owner)
	if err != nil {
		return nil, "", "", false, err
	}
	leftEntry, leftStored, leftFound, err := nextDuplicateContent008(leftIterator)
	if err != nil {
		return nil, "", "", false, err
	}
	rightEntry, rightStored, rightFound, err := nextDuplicateContent008(rightIterator)
	if err != nil {
		return nil, "", "", false, err
	}
	items := make([]domain.DuplicateReconciliationItem, 0, limit)
	for leftFound && rightFound {
		leftGroup, rightGroup := leftStored.Occurrence.GroupID, rightStored.Occurrence.GroupID
		switch {
		case leftGroup < rightGroup:
			leftAfter = leftEntry.Key
			leftEntry, leftStored, leftFound, err = nextDuplicateContent008(leftIterator)
		case rightGroup < leftGroup:
			rightAfter = rightEntry.Key
			rightEntry, rightStored, rightFound, err = nextDuplicateContent008(rightIterator)
		default:
			if leftStored.Occurrence.Size != rightStored.Occurrence.Size {
				return nil, "", "", false, domain.NewError(domain.ErrorInvalid, "duplicate file identity size mismatch")
			}
			ignored, revision, readErr := ignore.group(ctx, leftGroup)
			if readErr != nil {
				return nil, "", "", false, readErr
			}
			if !ignored && len(items) == limit {
				return items, leftAfter, rightAfter, true, nil
			}
			leftOccurrence, occurrenceErr := domainDuplicateOccurrence(leftStored.Occurrence)
			if occurrenceErr != nil {
				return nil, "", "", false, occurrenceErr
			}
			rightOccurrence, occurrenceErr := domainDuplicateOccurrence(rightStored.Occurrence)
			if occurrenceErr != nil {
				return nil, "", "", false, occurrenceErr
			}
			if !ignored {
				remove, keep := leftOccurrence, rightOccurrence
				if removeFrom == domain.DuplicateSideRight {
					remove, keep = rightOccurrence, leftOccurrence
				}
				items = append(items, domain.DuplicateReconciliationItem{GroupID: leftGroup, Remove: remove, Keep: keep, IgnoreRevision: revision})
			}
			leftAfter, rightAfter = leftEntry.Key, rightEntry.Key
			leftEntry, leftStored, leftFound, err = nextDuplicateContent008(leftIterator)
			if err == nil {
				rightEntry, rightStored, rightFound, err = nextDuplicateContent008(rightIterator)
			}
		}
		if err != nil {
			return nil, "", "", false, err
		}
	}
	return items, leftAfter, rightAfter, false, nil
}

func (s *FileStore) previewDuplicateReconciliation008(ctx context.Context, owner domain.UserID, request domain.DuplicateReconciliationPreviewRequest) (domain.DuplicateReconciliationPreview, error) {
	if !owner.Valid() || !request.RemoveFrom.Valid() || request.Left.Area != domain.AreaLive || request.Right.Area != domain.AreaLive || !request.Left.Path.Valid() || !request.Right.Path.Valid() || request.Left.Path == request.Right.Path || request.Left.Path.IsDescendantOf(request.Right.Path) || request.Right.Path.IsDescendantOf(request.Left.Path) {
		return domain.DuplicateReconciliationPreview{}, domain.NewError(domain.ErrorInvalid, "duplicate reconciliation requires two disjoint live directories")
	}
	limit := request.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 100 {
		return domain.DuplicateReconciliationPreview{}, domain.NewError(domain.ErrorInvalid, "duplicate reconciliation page limit must be between 1 and 100")
	}
	view, err := newNamespaceStore(s.engine).loadView(ctx, owner, "")
	if err != nil {
		return domain.DuplicateReconciliationPreview{}, err
	}
	left, err := s.duplicateDirectoryContent008(ctx, view, owner, request.Left)
	if err != nil {
		return domain.DuplicateReconciliationPreview{}, err
	}
	right, err := s.duplicateDirectoryContent008(ctx, view, owner, request.Right)
	if err != nil {
		return domain.DuplicateReconciliationPreview{}, err
	}
	cursor := duplicateReconciliationCursor008{SchemaVersion: 8, OwnerID: owner.String(), Left: request.Left, Right: request.Right, RemoveFrom: request.RemoveFrom, Limit: limit, LeftNodeID: left.nodeID, RightNodeID: right.nodeID, LeftRoot: left.root, RightRoot: right.root, ExpiresAt: s.engine.clock.Now().UTC().Add(s.engine.cursorTTL)}
	if request.Cursor == "" {
		cursor.Comparison, err = compareDuplicateContentRoots008(ctx, left, right)
		if err != nil {
			return domain.DuplicateReconciliationPreview{}, err
		}
	} else {
		if err := s.decodeDuplicateCursor(request.Cursor, &cursor); err != nil || cursor.SchemaVersion != 8 || cursor.OwnerID != owner.String() || cursor.Left != request.Left || cursor.Right != request.Right || cursor.RemoveFrom != request.RemoveFrom || cursor.Limit != limit || cursor.LeftNodeID != left.nodeID || cursor.RightNodeID != right.nodeID || cursor.LeftRoot != left.root || cursor.RightRoot != right.root || !s.engine.clock.Now().Before(cursor.ExpiresAt) {
			return domain.DuplicateReconciliationPreview{}, domain.NewError(domain.ErrorInvalid, "invalid or stale duplicate reconciliation cursor")
		}
		if err := restoreDuplicateComparisonAreas008(&cursor.Comparison); err != nil {
			return domain.DuplicateReconciliationPreview{}, err
		}
	}
	result := domain.DuplicateReconciliationPreview{Comparison: cursor.Comparison, RemoveFrom: request.RemoveFrom}
	more := false
	if cursor.Comparison.Exact {
		ignore, err := s.newDuplicateIgnoreReader008(ctx, owner)
		if err != nil {
			return domain.DuplicateReconciliationPreview{}, err
		}
		ignored, revision, err := ignore.group(ctx, left.occurrence.GroupID)
		if err != nil {
			return domain.DuplicateReconciliationPreview{}, err
		}
		if !ignored && request.Cursor == "" {
			remove, keep := left.occurrence, right.occurrence
			if request.RemoveFrom == domain.DuplicateSideRight {
				remove, keep = right.occurrence, left.occurrence
			}
			result.Items = []domain.DuplicateReconciliationItem{{GroupID: left.occurrence.GroupID, Remove: remove, Keep: keep, IgnoreRevision: revision}}
		}
	} else {
		result.Items, cursor.LeftAfter, cursor.RightAfter, more, err = s.reconciliationItems008(ctx, owner, left, right, request.RemoveFrom, cursor.LeftAfter, cursor.RightAfter, limit)
		if err != nil {
			return domain.DuplicateReconciliationPreview{}, err
		}
	}
	for _, item := range result.Items {
		if item.Remove.Size > math.MaxInt64-result.ReclaimableBytes {
			return domain.DuplicateReconciliationPreview{}, domain.NewError(domain.ErrorInvalid, "duplicate reconciliation bytes overflow")
		}
		result.ReclaimableBytes += item.Remove.Size
	}
	if more {
		result.NextCursor, err = s.encodeDuplicateCursor(cursor)
		if err != nil {
			return domain.DuplicateReconciliationPreview{}, err
		}
	}
	if len(result.Items) != 0 {
		plan := duplicateReconciliationPlan008{SchemaVersion: 8, OwnerID: owner.String(), Left: left.occurrence, Right: right.occurrence, RemoveFrom: request.RemoveFrom, Items: result.Items, LeftNodeID: left.nodeID, RightNodeID: right.nodeID, LeftRoot: left.sourceRoot, RightRoot: right.sourceRoot, ExpiresAt: s.engine.clock.Now().UTC().Add(s.engine.cursorTTL)}
		result.PlanToken, err = s.encodeDuplicateCursor(plan)
	}
	return result, err
}

func restoreDuplicateOccurrenceArea008(value *domain.DuplicateOccurrence) error {
	switch value.AreaName {
	case "live":
		value.Area = domain.AreaLive
	case "trash":
		value.Area = domain.AreaTrash
	default:
		return domain.NewError(domain.ErrorInvalid, "invalid duplicate occurrence area")
	}
	return nil
}

func restoreDuplicateComparisonAreas008(comparison *domain.DuplicateDirectoryComparison) error {
	if comparison == nil {
		return domain.NewError(domain.ErrorInvalid, "missing duplicate comparison")
	}
	if err := restoreDuplicateOccurrenceArea008(&comparison.Left); err != nil {
		return err
	}
	return restoreDuplicateOccurrenceArea008(&comparison.Right)
}

func (s *FileStore) decodeDuplicateReconciliationPlan008(owner domain.UserID, token string) (duplicateReconciliationPlan008, error) {
	if !owner.Valid() || token == "" {
		return duplicateReconciliationPlan008{}, domain.NewError(domain.ErrorInvalid, "duplicate reconciliation plan is required")
	}
	var plan duplicateReconciliationPlan008
	if err := s.decodeDuplicateCursor(token, &plan); err != nil || plan.SchemaVersion != 8 || plan.OwnerID != owner.String() || !plan.RemoveFrom.Valid() || len(plan.Items) < 1 || len(plan.Items) > 100 || !s.engine.clock.Now().Before(plan.ExpiresAt) {
		return duplicateReconciliationPlan008{}, domain.NewError(domain.ErrorInvalid, "invalid or expired duplicate reconciliation plan")
	}
	for _, occurrence := range []*domain.DuplicateOccurrence{&plan.Left, &plan.Right} {
		if err := restoreDuplicateOccurrenceArea008(occurrence); err != nil {
			return duplicateReconciliationPlan008{}, err
		}
	}
	for index := range plan.Items {
		if err := restoreDuplicateOccurrenceArea008(&plan.Items[index].Remove); err != nil {
			return duplicateReconciliationPlan008{}, err
		}
		if err := restoreDuplicateOccurrenceArea008(&plan.Items[index].Keep); err != nil {
			return duplicateReconciliationPlan008{}, err
		}
	}
	return plan, nil
}

func (s *FileStore) validateDuplicateReconciliationPlanAtView008(ctx context.Context, owner domain.UserID, view *namespaceView, plan duplicateReconciliationPlan008) (domain.DuplicateReconciliationSelection, error) {
	if view == nil || view.reference != namespaceReference(owner) {
		return domain.DuplicateReconciliationSelection{}, domain.NewError(domain.ErrorInvalid, "duplicate reconciliation snapshot is invalid")
	}
	for _, selected := range []struct {
		occurrence domain.DuplicateOccurrence
		nodeID     string
		root       storageformat.DomainTreeRoot
	}{{plan.Left, plan.LeftNodeID, plan.LeftRoot}, {plan.Right, plan.RightNodeID, plan.RightRoot}} {
		entry, err := s.resolveNamespaceAtView008(ctx, view, selected.occurrence.Area, selected.occurrence.Path)
		if err != nil {
			return domain.DuplicateReconciliationSelection{}, err
		}
		if entry.Entry.Kind != domain.EntryDirectory || entry.NodeID != selected.nodeID {
			return domain.DuplicateReconciliationSelection{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate reconciliation directory identity changed")
		}
		if entry.Children != selected.root {
			return domain.DuplicateReconciliationSelection{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate reconciliation directory content changed")
		}
		if domain.Version(entry.Entry.LogicalVersion) != selected.occurrence.Version {
			return domain.DuplicateReconciliationSelection{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate reconciliation directory changed")
		}
	}
	ignore := s.duplicateIgnoreReaderAtNamespaceView008(view)
	seen := make(map[string]struct{}, len(plan.Items))
	for _, item := range plan.Items {
		if item.GroupID == "" || item.Remove.GroupID != item.GroupID || item.Keep.GroupID != item.GroupID || item.Remove.Area != domain.AreaLive || item.Keep.Area != domain.AreaLive || item.Remove.Path == item.Keep.Path {
			return domain.DuplicateReconciliationSelection{}, domain.NewError(domain.ErrorInvalid, "invalid duplicate reconciliation item")
		}
		identity := item.Remove.Path.String()
		if _, found := seen[identity]; found {
			return domain.DuplicateReconciliationSelection{}, domain.NewError(domain.ErrorInvalid, "duplicate reconciliation item repeats a removal")
		}
		seen[identity] = struct{}{}
		ignored, revision, err := ignore.group(ctx, item.GroupID)
		if err != nil {
			return domain.DuplicateReconciliationSelection{}, err
		}
		if ignored || revision != item.IgnoreRevision {
			return domain.DuplicateReconciliationSelection{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate reconciliation ignore policy changed")
		}
		for _, occurrence := range []domain.DuplicateOccurrence{item.Remove, item.Keep} {
			entry, err := s.resolveNamespaceAtView008(ctx, view, occurrence.Area, occurrence.Path)
			if err != nil {
				return domain.DuplicateReconciliationSelection{}, err
			}
			scope, _ := domain.NewScope(owner, occurrence.Area)
			stored, err := catalogOccurrence(scope, occurrence.Path, entry.Entry)
			if err != nil || stored.GroupID != item.GroupID || domain.Version(entry.Entry.LogicalVersion) != occurrence.Version {
				return domain.DuplicateReconciliationSelection{}, domain.NewError(domain.ErrorPreconditionFailed, "duplicate reconciliation occurrence changed")
			}
		}
	}
	return domain.DuplicateReconciliationSelection{Left: plan.Left, Right: plan.Right, RemoveFrom: plan.RemoveFrom, Items: append([]domain.DuplicateReconciliationItem(nil), plan.Items...)}, nil
}

func (s *FileStore) validateDuplicateReconciliation008(ctx context.Context, owner domain.UserID, token string) (domain.DuplicateReconciliationSelection, error) {
	plan, err := s.decodeDuplicateReconciliationPlan008(owner, token)
	if err != nil {
		return domain.DuplicateReconciliationSelection{}, err
	}
	view, err := newNamespaceStore(s.engine).loadView(ctx, owner, "")
	if err != nil {
		return domain.DuplicateReconciliationSelection{}, err
	}
	return s.validateDuplicateReconciliationPlanAtView008(ctx, owner, view, plan)
}

func duplicateReconciliationTrashID008(owner domain.UserID, idempotencyKey string, index int, path domain.UserPath) string {
	return storageformat.Digest([]byte(fmt.Sprintf("endlessfs-duplicate-reconciliation-trash-v1\x00%s\x00%s\x00%08x\x00%s", owner.String(), idempotencyKey, index, path.String())))
}

func (s *FileStore) applyDuplicateReconciliation008(ctx context.Context, owner domain.UserID, token, idempotencyKey string) (domain.NamespaceBatchResult, error) {
	plan, err := s.decodeDuplicateReconciliationPlan008(owner, token)
	if err != nil {
		return domain.NamespaceBatchResult{}, err
	}
	if err := validatePortableIdempotencyKey(idempotencyKey); err != nil {
		return domain.NamespaceBatchResult{}, err
	}
	live, err := domain.NewScope(owner, domain.AreaLive)
	if err != nil {
		return domain.NamespaceBatchResult{}, err
	}
	trash, err := domain.NewScope(owner, domain.AreaTrash)
	if err != nil {
		return domain.NamespaceBatchResult{}, err
	}
	specs := make([]namespaceBatchMoveSpec, len(plan.Items))
	for index, item := range plan.Items {
		trashID := duplicateReconciliationTrashID008(owner, idempotencyKey, index, item.Remove.Path)
		destination, parseErr := domain.ParseUserPath("/" + trashID)
		if parseErr != nil {
			return domain.NamespaceBatchResult{}, parseErr
		}
		specs[index] = namespaceBatchMoveSpec{
			from: live, to: trash, trashID: trashID, attachTrash: true,
			request: domain.CopyRequest{Source: item.Remove.Path, Destination: destination, Conflict: domain.ConflictFail, ExpectedSource: item.Remove.Version},
		}
	}
	validate := func(ctx context.Context, view *namespaceView) error {
		_, err := s.validateDuplicateReconciliationPlanAtView008(ctx, owner, view, plan)
		return err
	}
	return newNamespaceStore(s.engine).batchCopyOrMoveValidated(ctx, owner, specs, true, "duplicate-reconciliation", idempotencyKey, storageformat.Digest([]byte(token)), validate)
}
