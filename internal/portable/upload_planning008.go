package portable

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const maximumUploadPlanItems = 1000

type uploadPlanProjectionToken008 struct {
	SchemaVersion int                          `json:"schemaVersion"`
	OwnerID       string                       `json:"ownerID"`
	ProjectionID  string                       `json:"projectionID"`
	Root          storageformat.DomainTreeRoot `json:"root"`
	SourceRoot    storageformat.DomainTreeRoot `json:"sourceRoot"`
	ExpiresAt     time.Time                    `json:"expiresAt"`
}

func uploadPlanningProjectionID008(owner domain.UserID) string {
	return storageformat.Digest([]byte("endlessfs-upload-planning-projection-v1\x00" + owner.String()))
}

func uploadPlanningProjectionChanges008(scope domain.Scope, path domain.UserPath, entry storageformat.NamespaceEntry, remove bool) (map[string]storageformat.DomainChange, error) {
	changes := make(map[string]storageformat.DomainChange)
	if entry.Entry.Kind != domain.EntryFile {
		return changes, nil
	}
	occurrence, err := catalogOccurrence(scope, path, entry.Entry)
	if err != nil {
		return nil, err
	}
	stored := storageformat.DuplicateProjectionOccurrence{SchemaVersion: 1, BlobID: entry.Entry.BlobID, Occurrence: occurrence}
	for _, key := range []string{duplicateProjectionSizeKey008(stored), duplicateProjectionLiveSourceKey008(stored)} {
		changes[key] = storageformat.DomainChange{Key: key, Delete: remove}
	}
	if remove {
		return changes, nil
	}
	body, err := storageformat.EncodeCanonical(stored)
	if err != nil {
		return nil, err
	}
	changes[duplicateProjectionSizeKey008(stored)] = storageformat.DomainChange{Key: duplicateProjectionSizeKey008(stored), Value: body, LogicalVersion: storageformat.Digest(append([]byte("endlessfs-upload-planning-size-v1\x00"), body...))}
	changes[duplicateProjectionLiveSourceKey008(stored)] = storageformat.DomainChange{Key: duplicateProjectionLiveSourceKey008(stored), Value: body, LogicalVersion: storageformat.Digest(append([]byte("endlessfs-upload-planning-source-v1\x00"), body...))}
	return changes, nil
}

func mergeUploadPlanningChanges008(target map[string]storageformat.DomainChange, incoming map[string]storageformat.DomainChange) {
	for key, change := range incoming {
		target[key] = change
	}
}

func (s *FileStore) updateUploadPlanningProjection008(ctx context.Context, view *namespaceView, prior, current storageformat.DomainTreeRoot, projectionRoot storageformat.DomainTreeRoot, projectionSession *consistencyDomainTreeSession) (storageformat.DomainTreeRoot, error) {
	if prior == current {
		return projectionRoot, nil
	}
	owner, err := domain.ParseUserID(view.reference.ID)
	if err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	scope, _ := domain.NewScope(owner, domain.AreaLive)
	changes := make(map[string]storageformat.DomainChange)
	var walk func(storageformat.DomainTreeRoot, storageformat.DomainTreeRoot, domain.UserPath) error
	walk = func(oldRoot, newRoot storageformat.DomainTreeRoot, parent domain.UserPath) error {
		if oldRoot == newRoot {
			return nil
		}
		oldIterator, err := newConsistencyDomainTreeIterator(ctx, view.session, oldRoot)
		if err != nil {
			return err
		}
		newIterator, err := newConsistencyDomainTreeIterator(ctx, view.session, newRoot)
		if err != nil {
			return err
		}
		oldValue, oldFound, err := oldIterator.Next()
		if err != nil {
			return err
		}
		newValue, newFound, err := newIterator.Next()
		if err != nil {
			return err
		}
		for oldFound || newFound {
			name := ""
			if !newFound || oldFound && oldValue.Key < newValue.Key {
				name = oldValue.Key
			} else {
				name = newValue.Key
			}
			var oldEntry, newEntry storageformat.NamespaceEntry
			hasOld, hasNew := oldFound && oldValue.Key == name, newFound && newValue.Key == name
			if hasOld {
				oldEntry, err = decodeNamespaceEntry(oldValue.Value)
				if err != nil || oldEntry.Entry.Name != name {
					return domain.NewError(domain.ErrorInvalid, "invalid prior upload planning namespace entry")
				}
				oldValue, oldFound, err = oldIterator.Next()
				if err != nil {
					return err
				}
			}
			if hasNew {
				newEntry, err = decodeNamespaceEntry(newValue.Value)
				if err != nil || newEntry.Entry.Name != name {
					return domain.NewError(domain.ErrorInvalid, "invalid current upload planning namespace entry")
				}
				newValue, newFound, err = newIterator.Next()
				if err != nil {
					return err
				}
			}
			childPath, err := parent.Join(name)
			if err != nil {
				return err
			}
			if hasOld && hasNew && oldEntry.NodeID == newEntry.NodeID && oldEntry.Entry.LogicalVersion == newEntry.Entry.LogicalVersion && oldEntry.Children == newEntry.Children {
				continue
			}
			if hasOld && oldEntry.Entry.Kind == domain.EntryFile {
				removed, err := uploadPlanningProjectionChanges008(scope, childPath, oldEntry, true)
				if err != nil {
					return err
				}
				mergeUploadPlanningChanges008(changes, removed)
			}
			if hasNew && newEntry.Entry.Kind == domain.EntryFile {
				added, err := uploadPlanningProjectionChanges008(scope, childPath, newEntry, false)
				if err != nil {
					return err
				}
				mergeUploadPlanningChanges008(changes, added)
			}
			oldChildren, newChildren := storageformat.DomainTreeRoot{}, storageformat.DomainTreeRoot{}
			if hasOld && oldEntry.Entry.Kind == domain.EntryDirectory {
				oldChildren = oldEntry.Children
			}
			if hasNew && newEntry.Entry.Kind == domain.EntryDirectory {
				newChildren = newEntry.Children
			}
			if oldChildren.Digest != "" || newChildren.Digest != "" {
				if err := walk(oldChildren, newChildren, childPath); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(prior, current, namespaceRootPath()); err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	return projectionSession.apply(ctx, projectionRoot, normalizeDomainChangeMap(changes))
}

func (s *FileStore) buildUploadPlanningProjection008(ctx context.Context, view *namespaceView, session *consistencyDomainTreeSession) (storageformat.DomainTreeRoot, error) {
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
		if occurrence.Occurrence.Kind != domain.DuplicateFile || occurrence.Occurrence.Area != "live" {
			return nil
		}
		body, err := storageformat.EncodeCanonical(occurrence)
		if err != nil {
			return err
		}
		for _, entry := range []storageformat.DomainEntry{
			{Key: duplicateProjectionSizeKey008(occurrence), Value: body, LogicalVersion: storageformat.Digest(append([]byte("endlessfs-upload-planning-size-v1\x00"), body...))},
			{Key: duplicateProjectionLiveSourceKey008(occurrence), Value: body, LogicalVersion: storageformat.Digest(append([]byte("endlessfs-upload-planning-source-v1\x00"), body...))},
		} {
			if len(chunk) == domainPageMaximumItems {
				if err := flush(); err != nil {
					return err
				}
			}
			chunk = append(chunk, entry)
		}
		return nil
	}
	if err := s.walkDuplicateNamespace008(ctx, view, domain.AreaLive, namespaceRootPath(), view.roots[domain.AreaLive], emit); err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	if err := flush(); err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	return mergeDuplicateProjectionRuns008(ctx, session, runs)
}

func (s *FileStore) uploadPlanningProjection008(ctx context.Context, owner domain.UserID) (duplicateProjectionSnapshot008, error) {
	if !owner.Valid() {
		return duplicateProjectionSnapshot008{}, domain.NewError(domain.ErrorInvalid, "invalid upload planning owner")
	}
	store := newNamespaceStore(s.engine)
	projectionID := uploadPlanningProjectionID008(owner)
	key := storageformat.ScopedProjectionHeadKey(owner.String(), storageformat.ProjectionDuplicates, projectionID)
	var aheadSourceRevision uint64
	for {
		view, err := store.loadView(ctx, owner, "")
		if err != nil {
			return duplicateProjectionSnapshot008{}, err
		}
		if !view.head.Registered || view.head.Revision == 0 {
			return duplicateProjectionSnapshot008{head: storageformat.ProjectionHead{SourceRoot: view.roots[domain.AreaLive].Children}, projectionID: projectionID, session: duplicateProjectionSession008(s.engine, owner, projectionID)}, nil
		}
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
		currentNamespaceRoot := view.roots[domain.AreaLive].Children
		if valid && current.SourceDomainID == view.reference.ID && current.SourceRevision > view.head.Revision {
			// A concurrent planner observed and published a newer immutable
			// namespace root. Reload instead of ever moving a derived head
			// backward to this stale view.
			if aheadSourceRevision == current.SourceRevision {
				return duplicateProjectionSnapshot008{}, domain.NewError(domain.ErrorInvalid, "upload planning projection source revision is ahead of its namespace")
			}
			aheadSourceRevision = current.SourceRevision
			continue
		}
		if valid && current.SourceDomainID == view.reference.ID && current.SourceRevision == view.head.Revision && current.SourceRoot != currentNamespaceRoot {
			return duplicateProjectionSnapshot008{}, domain.NewError(domain.ErrorInvalid, "upload planning projection source is inconsistent")
		}
		if valid && current.SourceDomainID == view.reference.ID && current.SourceRoot == currentNamespaceRoot {
			return duplicateProjectionSnapshot008{head: current, root: current.Root, projectionID: projectionID, session: duplicateProjectionSession008(s.engine, owner, projectionID)}, nil
		}
		session := duplicateProjectionSession008(s.engine, owner, projectionID)
		var root storageformat.DomainTreeRoot
		var buildErr error
		if valid && current.SourceDomainID == view.reference.ID {
			root, buildErr = s.updateUploadPlanningProjection008(ctx, view, current.SourceRoot, currentNamespaceRoot, current.Root, session)
		} else {
			root, buildErr = s.buildUploadPlanningProjection008(ctx, view, session)
		}
		if buildErr != nil {
			return duplicateProjectionSnapshot008{}, buildErr
		}
		next := storageformat.ProjectionHead{SchemaVersion: 1, OwnerID: owner.String(), ProjectionID: projectionID, Kind: storageformat.ProjectionDuplicates, SourceDomainID: view.reference.ID, SourceRevision: view.head.Revision, SourceRoot: currentNamespaceRoot, Root: root}
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

func validUploadPlanID008(value string) bool {
	return value != "" && len(value) <= 128 && !strings.ContainsAny(value, "\r\n\x00")
}

func validateUploadSizePlan008(owner domain.UserID, request domain.UploadSizePlanRequest) error {
	if !owner.Valid() || len(request.Items) < 1 || len(request.Items) > maximumUploadPlanItems {
		return domain.NewError(domain.ErrorInvalid, "upload size plan must contain 1 to 1000 items")
	}
	seen := make(map[string]struct{}, len(request.Items))
	for _, item := range request.Items {
		if !validUploadPlanID008(item.ID) || !item.Path.Valid() || item.Path.IsRoot() || item.Size < 0 {
			return domain.NewError(domain.ErrorInvalid, "invalid upload size plan item")
		}
		if _, found := seen[item.ID]; found {
			return domain.NewError(domain.ErrorInvalid, "duplicate upload plan item ID")
		}
		seen[item.ID] = struct{}{}
	}
	return nil
}

func uploadPlanTarget008(ctx context.Context, store *namespaceStore, view *namespaceView, scope domain.Scope, path domain.UserPath) (storageformat.NamespaceEntry, bool, error) {
	entry, err := store.resolveEntryAtView(ctx, view, scope, path)
	if errors.Is(err, domain.ErrNotFound) {
		return storageformat.NamespaceEntry{}, false, nil
	}
	return entry, err == nil, err
}

func uploadPlanningCollect008(ctx context.Context, session *consistencyDomainTreeSession, root storageformat.DomainTreeRoot, prefix string) ([]storageformat.DomainEntry, error) {
	values, err := session.collect(ctx, root, prefix, "", 1)
	if errors.Is(err, domain.ErrNotFound) {
		// Rebuildable projection pages may be collected by a portability
		// checkpoint after a plan token is issued. That is a stale optimistic
		// snapshot, not a missing user file and not a terminal planning error.
		return nil, domain.NewError(domain.ErrorConflict, "upload planning snapshot is no longer available")
	}
	return values, err
}

func (s *FileStore) planUploadSizes008(ctx context.Context, owner domain.UserID, request domain.UploadSizePlanRequest) (domain.UploadSizePlan, error) {
	if err := validateUploadSizePlan008(owner, request); err != nil {
		return domain.UploadSizePlan{}, err
	}
	store := newNamespaceStore(s.engine)
	projection, err := s.uploadPlanningProjection008(ctx, owner)
	if err != nil {
		return domain.UploadSizePlan{}, err
	}
	view, err := store.loadView(ctx, owner, "")
	if err != nil {
		return domain.UploadSizePlan{}, err
	}
	if view.roots[domain.AreaLive].Children != projection.head.SourceRoot {
		return domain.UploadSizePlan{}, domain.NewError(domain.ErrorConflict, "upload planning snapshot changed")
	}
	live, _ := domain.NewScope(owner, domain.AreaLive)
	result := domain.UploadSizePlan{Items: make([]domain.UploadSizePlanDecision, len(request.Items))}
	for index, item := range request.Items {
		values, err := uploadPlanningCollect008(ctx, projection.session, projection.root, duplicateProjectionSizePrefix008(item.Size))
		if err != nil {
			return domain.UploadSizePlan{}, err
		}
		decision := domain.UploadSizePlanDecision{ID: item.ID, FingerprintRequired: len(values) != 0}
		target, found, err := uploadPlanTarget008(ctx, store, view, live, item.Path)
		if err != nil {
			return domain.UploadSizePlan{}, err
		}
		if found {
			decision.TargetExists, decision.TargetKind = true, target.Entry.Kind
			decision.TargetSize, decision.TargetVersion = target.Entry.Size, domain.Version(target.Entry.LogicalVersion)
			if target.Entry.Kind == domain.EntryFile && target.Entry.Size == item.Size {
				decision.FingerprintRequired = true
			}
		}
		result.Items[index] = decision
	}
	token, err := s.encodeDuplicateCursor(uploadPlanProjectionToken008{
		SchemaVersion: 1, OwnerID: owner.String(), ProjectionID: projection.projectionID,
		Root: projection.root, SourceRoot: projection.head.SourceRoot, ExpiresAt: s.engine.clock.Now().UTC().Add(s.engine.cursorTTL),
	})
	result.Token = token
	return result, err
}

func validateUploadFingerprintPlan008(owner domain.UserID, request domain.UploadFingerprintPlanRequest) error {
	if !owner.Valid() || request.Token == "" || len(request.Items) < 1 || len(request.Items) > maximumUploadPlanItems {
		return domain.NewError(domain.ErrorInvalid, "upload fingerprint plan must contain 1 to 1000 items")
	}
	seen := make(map[string]struct{}, len(request.Items))
	for _, item := range request.Items {
		fingerprint := objectstore.ContentFingerprint{MD5: item.MD5, CRC32C: item.CRC32C}
		if !validUploadPlanID008(item.ID) || !item.Path.Valid() || item.Path.IsRoot() || item.Size < 0 || fingerprint.Validate() != nil {
			return domain.NewError(domain.ErrorInvalid, "invalid upload fingerprint plan item")
		}
		if _, found := seen[item.ID]; found {
			return domain.NewError(domain.ErrorInvalid, "duplicate upload plan item ID")
		}
		seen[item.ID] = struct{}{}
	}
	return nil
}

func (s *FileStore) planUploadFingerprints008(ctx context.Context, owner domain.UserID, request domain.UploadFingerprintPlanRequest) (domain.UploadFingerprintPlan, error) {
	if err := validateUploadFingerprintPlan008(owner, request); err != nil {
		return domain.UploadFingerprintPlan{}, err
	}
	var token uploadPlanProjectionToken008
	if err := s.decodeDuplicateCursor(request.Token, &token); err != nil || token.SchemaVersion != 1 || token.OwnerID != owner.String() || token.ProjectionID != uploadPlanningProjectionID008(owner) || !s.engine.clock.Now().Before(token.ExpiresAt) {
		return domain.UploadFingerprintPlan{}, domain.NewError(domain.ErrorInvalid, "invalid upload planning token")
	}
	session := duplicateProjectionSession008(s.engine, owner, token.ProjectionID)
	store := newNamespaceStore(s.engine)
	view, err := store.loadView(ctx, owner, "")
	if err != nil {
		return domain.UploadFingerprintPlan{}, err
	}
	if view.roots[domain.AreaLive].Children != token.SourceRoot {
		return domain.UploadFingerprintPlan{}, domain.NewError(domain.ErrorConflict, "upload planning snapshot changed")
	}
	live, _ := domain.NewScope(owner, domain.AreaLive)
	result := domain.UploadFingerprintPlan{Items: make([]domain.UploadFingerprintPlanDecision, len(request.Items))}
	for index, item := range request.Items {
		groupID, err := duplicateFileGroupID(storageformat.DirectoryEntry{Kind: domain.EntryFile, Size: item.Size, MD5: item.MD5, CRC32C: item.CRC32C})
		if err != nil {
			return domain.UploadFingerprintPlan{}, err
		}
		decision := domain.UploadFingerprintPlanDecision{ID: item.ID, Action: domain.UploadPlanUpload}
		target, targetFound, err := uploadPlanTarget008(ctx, store, view, live, item.Path)
		if err != nil {
			return domain.UploadFingerprintPlan{}, err
		}
		if targetFound {
			decision.TargetExists, decision.TargetKind, decision.TargetVersion = true, target.Entry.Kind, domain.Version(target.Entry.LogicalVersion)
			if target.Entry.Kind == domain.EntryFile {
				targetGroup, groupErr := duplicateFileGroupID(target.Entry)
				if groupErr == nil && targetGroup == groupID {
					decision.Action = domain.UploadPlanSkip
					result.Items[index] = decision
					continue
				}
			}
		}
		values, err := uploadPlanningCollect008(ctx, session, token.Root, "source/"+groupID+"/")
		if err != nil {
			return domain.UploadFingerprintPlan{}, err
		}
		for _, value := range values {
			var stored storageformat.DuplicateProjectionOccurrence
			if err := decodeCanonicalValue(value.Value, &stored); err != nil || storageformat.ValidateDuplicateProjectionOccurrence(stored) != nil || duplicateProjectionLiveSourceKey008(stored) != value.Key || stored.Occurrence.Area != "live" {
				return domain.UploadFingerprintPlan{}, domain.NewError(domain.ErrorInvalid, "invalid upload planning occurrence")
			}
			source, err := domainDuplicateOccurrence(stored.Occurrence)
			if err != nil {
				return domain.UploadFingerprintPlan{}, err
			}
			decision.Action, decision.SourcePath, decision.SourceVersion = domain.UploadPlanReuse, &source.Path, source.Version
			break
		}
		result.Items[index] = decision
	}
	return result, nil
}
