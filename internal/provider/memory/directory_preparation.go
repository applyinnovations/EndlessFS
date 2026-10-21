package memory

import (
	"context"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func (p *Provider) PrepareDirectories(ctx context.Context, scope domain.Scope, paths []domain.UserPath) ([]domain.DirectoryPreparationResult, error) {
	if err := validateContextScope(ctx, scope); err != nil {
		return nil, err
	}
	if scope.Area() != domain.AreaLive {
		return nil, domain.NewError(domain.ErrorInvalid, "directory preparation requires live scope")
	}
	if err := domain.ValidateDirectoryPreparationPaths(paths); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beforeLocked(OperationCreateDirectory); err != nil {
		return nil, err
	}
	order := make([]int, len(paths))
	for index := range paths {
		order[index] = index
	}
	sort.Slice(order, func(i, j int) bool { return paths[order[i]].String() < paths[order[j]].String() })
	result := make([]domain.DirectoryPreparationResult, len(paths))
	for _, index := range order {
		path := paths[index]
		item := domain.DirectoryPreparationResult{Index: index, Path: path}
		if existing, found := p.scopeObjectsLocked(scope)[path.String()]; found {
			if existing.entry.Kind == domain.EntryDirectory {
				item.Ready = true
			} else {
				item.ErrorCode = domain.ErrorConflict
			}
		} else {
			for parent := path.Parent(); !parent.IsRoot(); parent = parent.Parent() {
				if ancestor, found := p.scopeObjectsLocked(scope)[parent.String()]; found && ancestor.entry.Kind != domain.EntryDirectory {
					item.ErrorCode = domain.ErrorConflict
					break
				}
			}
			if item.ErrorCode == "" {
				if err := p.requireParentLocked(scope, path); err != nil {
					item.ErrorCode = domain.ErrorNotFound
				} else {
					p.scopeObjectsLocked(scope)[path.String()] = object{entry: p.newEntryLocked(path, domain.EntryDirectory, 0, ""), materialized: true}
					item.Ready = true
				}
			}
		}
		result[index] = item
	}
	return result, nil
}
