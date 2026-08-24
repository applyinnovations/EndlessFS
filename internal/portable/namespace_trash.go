package portable

import (
	"context"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func (s *FileStore) MoveToTrash(ctx context.Context, owner domain.UserID, request domain.TrashRequest) (domain.Operation, error) {
	if !owner.Valid() || !request.Path.Valid() || request.Path.IsRoot() || request.TrashID == "" {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "invalid trash request")
	}
	live, _ := domain.NewScope(owner, domain.AreaLive)
	trash, _ := domain.NewScope(owner, domain.AreaTrash)
	destination, err := domain.ParseUserPath("/" + request.TrashID)
	if err != nil || destination.Name() != request.TrashID {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "invalid trash identity")
	}
	return newNamespaceStore(s.engine).copyOrMove(ctx, true, live, trash, domain.MoveRequest{
		Source: request.Path, Destination: destination, Conflict: domain.ConflictFail,
		ExpectedSource: request.ExpectedVersion, IdempotencyKey: request.IdempotencyKey,
	})
}

func namespaceTrashEntry(owner domain.UserID, path domain.UserPath, entry storageformat.NamespaceEntry) (domain.TrashEntry, error) {
	if entry.Trash == nil || path.IsRoot() || path.Name() == "" {
		return domain.TrashEntry{}, domain.NewError(domain.ErrorInvalid, "trash entry metadata is missing")
	}
	original, err := domain.ParseUserPath(entry.Trash.OriginalPath)
	if err != nil || original.IsRoot() {
		return domain.TrashEntry{}, domain.NewError(domain.ErrorInvalid, "trash entry original path is invalid")
	}
	return domain.TrashEntry{
		TrashID: path.Name(), OwnerUserID: owner, OriginalPath: original, TrashedPath: path,
		Entry: namespaceDomainEntry(path, entry), TrashedAt: entry.Trash.TrashedAt, OriginalVersion: entry.Trash.OriginalVersion,
	}, nil
}

func (s *FileStore) ListTrash(ctx context.Context, owner domain.UserID, request domain.TrashListRequest) (domain.TrashListPage, error) {
	if !owner.Valid() {
		return domain.TrashListPage{}, domain.NewError(domain.ErrorInvalid, "invalid trash owner")
	}
	limit := request.Limit
	if limit == 0 {
		limit = 200
	}
	if limit < 1 || limit > 1000 {
		return domain.TrashListPage{}, domain.NewError(domain.ErrorInvalid, "trash page limit must be between 1 and 1000")
	}
	store := newNamespaceStore(s.engine)
	bound, snapshotDigest := "", ""
	expiresAt := s.engine.clock.Now().UTC().Add(s.engine.cursorTTL)
	if request.Cursor != "" {
		cursor, err := store.decodeListCursor(request.Cursor)
		if err != nil || cursor.OwnerID != owner.String() || cursor.Area != "trash" || cursor.Directory != "/" || cursor.Sort != domain.SortModified || !cursor.Descending || cursor.PageSize != limit || !s.engine.clock.Now().Before(cursor.ExpiresAt) {
			return domain.TrashListPage{}, domain.NewError(domain.ErrorInvalid, "invalid or out-of-scope trash cursor")
		}
		bound, snapshotDigest, expiresAt = cursor.Bound, cursor.Snapshot, cursor.ExpiresAt
	}
	view, err := store.loadView(ctx, owner, snapshotDigest)
	if err != nil {
		return domain.TrashListPage{}, err
	}
	root := view.roots[domain.AreaTrash]
	sortRoot, err := store.namespaceSortProjection(ctx, view, domain.AreaTrash, root, domain.SortModified)
	if err != nil {
		return domain.TrashListPage{}, err
	}
	values, err := newNamespaceProjectionTreeSession(store.domain, owner, namespaceProjectionID(owner, domain.AreaTrash, root, domain.SortModified), storageformat.ProjectionModified).collectOrdered(ctx, sortRoot, bound, limit+1, true)
	if err != nil {
		return domain.TrashListPage{}, err
	}
	hasMore := len(values) > limit
	if hasMore {
		values = values[:limit]
	}
	page := domain.TrashListPage{Items: make([]domain.TrashEntry, 0, len(values))}
	for _, value := range values {
		entry, err := decodeNamespaceEntry(value.Value)
		if err != nil {
			return domain.TrashListPage{}, err
		}
		path, err := domain.ParseUserPath("/" + entry.Entry.Name)
		if err != nil {
			return domain.TrashListPage{}, domain.NewError(domain.ErrorInvalid, "invalid stored trash path")
		}
		item, err := namespaceTrashEntry(owner, path, entry)
		if err != nil {
			return domain.TrashListPage{}, err
		}
		page.Items = append(page.Items, item)
	}
	if hasMore {
		if view.snapshotDigest == "" {
			view.snapshotDigest, err = store.domain.writeHeadSnapshot(ctx, view.reference, view.head, expiresAt)
			if err != nil {
				return domain.TrashListPage{}, err
			}
		}
		page.NextCursor, err = store.encodeListCursor(namespaceListCursor{
			SchemaVersion: 1, OwnerID: owner.String(), Area: "trash", Directory: "/", Sort: domain.SortModified,
			Descending: true, PageSize: limit, Snapshot: view.snapshotDigest, Bound: values[len(values)-1].Key, ExpiresAt: expiresAt,
		})
		if err != nil {
			return domain.TrashListPage{}, err
		}
	}
	return page, nil
}

func (s *FileStore) RestoreFromTrash(ctx context.Context, owner domain.UserID, trashID string, conflict domain.ConflictMode, idempotencyKey string) (domain.Operation, error) {
	if !owner.Valid() || trashID == "" {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "invalid trash restore request")
	}
	trashPath, err := domain.ParseUserPath("/" + trashID)
	if err != nil || trashPath.Name() != trashID {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "invalid trash identity")
	}
	from, _ := domain.NewScope(owner, domain.AreaTrash)
	to, _ := domain.NewScope(owner, domain.AreaLive)
	return newNamespaceStore(s.engine).restoreFromTrash(ctx, from, to, domain.MoveRequest{
		Source: trashPath, Conflict: conflict, IdempotencyKey: idempotencyKey,
	})
}

func (s *FileStore) DeleteFromTrash(ctx context.Context, owner domain.UserID, trashID, idempotencyKey string) (domain.Operation, error) {
	if !owner.Valid() || trashID == "" {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "invalid trash deletion request")
	}
	path, err := domain.ParseUserPath("/" + trashID)
	if err != nil || path.Name() != trashID {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "invalid trash identity")
	}
	scope, _ := domain.NewScope(owner, domain.AreaTrash)
	return newNamespaceStore(s.engine).deleteFromTrash(ctx, scope, domain.DeleteRequest{Path: path, IdempotencyKey: idempotencyKey})
}
