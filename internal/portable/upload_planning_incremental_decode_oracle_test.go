package portable

import (
	"context"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// Exact prechange implementation, independent of the production decode reuse.
func (s *FileStore) originalUploadPlanningProjection008(ctx context.Context, view *namespaceView, prior, current storageformat.DomainTreeRoot, projectionRoot storageformat.DomainTreeRoot, projectionSession *consistencyDomainTreeSession) (storageformat.DomainTreeRoot, error) {
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
