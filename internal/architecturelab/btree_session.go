package architecturelab

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

type treeRequirement uint8

const (
	treeAny treeRequirement = iota
	treeAbsent
	treePresent
)

type treeEdit struct {
	Key         string
	Value       json.RawMessage
	Remove      bool
	Requirement treeRequirement
}

type treeSession struct {
	tree  immutableTree
	pages map[string]treePage
}

func newTreeSession(tree immutableTree) *treeSession {
	return &treeSession{tree: tree, pages: make(map[string]treePage)}
}

func (session *treeSession) readPage(ctx context.Context, operation MutationKind, subsystem, ref string) (treePage, error) {
	if page, found := session.pages[ref]; found {
		return page, nil
	}
	page, err := session.tree.readPage(ctx, operation, subsystem, ref)
	if err == nil {
		session.pages[ref] = page
	}
	return page, err
}

func (session *treeSession) lookup(ctx context.Context, operation MutationKind, subsystem, ref, key string) (json.RawMessage, bool, error) {
	var expected *treeChild
	expectedLevel := -1
	for {
		page, err := session.readPage(ctx, operation, subsystem, ref)
		if err != nil {
			return nil, false, err
		}
		if expected != nil && verifyTreeChild(*expected, page, expectedLevel) != nil {
			return nil, false, domain.NewError(domain.ErrorInvalid, "tree child descriptor mismatch")
		}
		if page.Level == 0 {
			index := sort.Search(len(page.Values), func(index int) bool { return page.Values[index].Key >= key })
			if index >= len(page.Values) || page.Values[index].Key != key {
				return nil, false, nil
			}
			return append(json.RawMessage(nil), page.Values[index].Value...), true, nil
		}
		index := childIndex(page.Children, key)
		if index < 0 {
			return nil, false, nil
		}
		child := page.Children[index]
		expected, expectedLevel, ref = &child, page.Level-1, child.Ref
	}
}

func (session *treeSession) first(ctx context.Context, operation MutationKind, subsystem, ref string, limit int) ([]treeValue, error) {
	if limit < 1 {
		return nil, domain.NewError(domain.ErrorInvalid, "tree page limit is required")
	}
	result := make([]treeValue, 0, limit)
	var visit func(string) error
	visit = func(pageRef string) error {
		page, err := session.readPage(ctx, operation, subsystem, pageRef)
		if err != nil {
			return err
		}
		if page.Level == 0 {
			remaining := limit - len(result)
			if remaining > len(page.Values) {
				remaining = len(page.Values)
			}
			for _, value := range page.Values[:remaining] {
				result = append(result, treeValue{Key: value.Key, Value: append(json.RawMessage(nil), value.Value...)})
			}
			return nil
		}
		for _, child := range page.Children {
			if len(result) == limit {
				return nil
			}
			if err := visit(child.Ref); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(ref); err != nil {
		return nil, err
	}
	return result, nil
}

func (session *treeSession) apply(ctx context.Context, operation MutationKind, subsystem, ref string, edits []treeEdit) (string, error) {
	if len(edits) == 0 {
		return ref, nil
	}
	edits = append([]treeEdit(nil), edits...)
	sort.Slice(edits, func(left, right int) bool { return edits[left].Key < edits[right].Key })
	for index, edit := range edits {
		if edit.Key == "" || index > 0 && edits[index-1].Key == edit.Key || (!edit.Remove && len(edit.Value) == 0) {
			return "", domain.NewError(domain.ErrorInvalid, "invalid tree edit")
		}
	}
	page, err := session.readPage(ctx, operation, subsystem, ref)
	if err != nil {
		return "", err
	}
	replacements, changed, err := session.rewrite(ctx, operation, subsystem, page, edits)
	if err != nil || !changed {
		return ref, err
	}
	if len(replacements) == 0 {
		return session.tree.emptyReference(), nil
	}
	if len(replacements) == 1 {
		return replacements[0].Ref, nil
	}
	return session.writePage(ctx, operation, subsystem, "", treePage{SchemaVersion: 1, Level: page.Level + 1, Children: replacements})
}

func (session *treeSession) rewrite(ctx context.Context, operation MutationKind, subsystem string, page treePage, edits []treeEdit) ([]treeChild, bool, error) {
	if page.Level == 0 {
		values := append([]treeValue(nil), page.Values...)
		changed := false
		for _, edit := range edits {
			index := sort.Search(len(values), func(index int) bool { return values[index].Key >= edit.Key })
			found := index < len(values) && values[index].Key == edit.Key
			if edit.Requirement == treeAbsent && found {
				return nil, false, domain.NewError(domain.ErrorConflict, "tree value already exists")
			}
			if edit.Requirement == treePresent && !found {
				return nil, false, domain.NewError(domain.ErrorNotFound, "tree value does not exist")
			}
			if edit.Remove {
				if found {
					values = append(values[:index], values[index+1:]...)
					changed = true
				}
				continue
			}
			if found {
				if string(values[index].Value) != string(edit.Value) {
					values[index].Value = append(json.RawMessage(nil), edit.Value...)
					changed = true
				}
				continue
			}
			values = append(values, treeValue{})
			copy(values[index+1:], values[index:])
			values[index] = treeValue{Key: edit.Key, Value: append(json.RawMessage(nil), edit.Value...)}
			changed = true
		}
		if !changed {
			return pageDescriptor(page), false, nil
		}
		return session.writeLeafGroups(ctx, operation, subsystem, values)
	}

	groups := make(map[int][]treeEdit)
	indices := make([]int, 0)
	for _, edit := range edits {
		index := childIndex(page.Children, edit.Key)
		if index < 0 {
			return nil, false, domain.NewError(domain.ErrorInvalid, "tree branch has no children")
		}
		if _, found := groups[index]; !found {
			indices = append(indices, index)
		}
		groups[index] = append(groups[index], edit)
	}
	sort.Ints(indices)
	children := append([]treeChild(nil), page.Children...)
	offset := 0
	changed := false
	for _, originalIndex := range indices {
		index := originalIndex + offset
		childPage, err := session.readPage(ctx, operation, subsystem, children[index].Ref)
		if err != nil {
			return nil, false, err
		}
		if err := verifyTreeChild(children[index], childPage, page.Level-1); err != nil {
			return nil, false, err
		}
		replacements, childChanged, err := session.rewrite(ctx, operation, subsystem, childPage, groups[originalIndex])
		if err != nil {
			return nil, false, err
		}
		if childChanged {
			next := make([]treeChild, 0, len(children)-1+len(replacements))
			next = append(next, children[:index]...)
			next = append(next, replacements...)
			next = append(next, children[index+1:]...)
			children = next
			offset += len(replacements) - 1
			changed = true
		}
	}
	if !changed {
		return pageDescriptor(page), false, nil
	}
	return session.writeBranchGroups(ctx, operation, subsystem, page.Level, children)
}

func (session *treeSession) writeLeafGroups(ctx context.Context, operation MutationKind, subsystem string, values []treeValue) ([]treeChild, bool, error) {
	if len(values) == 0 {
		return nil, true, nil
	}
	groups := splitCountLimit(len(values), session.tree.maximumPageItems())
	children := make([]treeChild, 0, len(groups))
	offset := 0
	for _, size := range groups {
		page := treePage{SchemaVersion: 1, Level: 0, Values: append([]treeValue(nil), values[offset:offset+size]...)}
		ref, err := session.writePage(ctx, operation, subsystem, "", page)
		if err != nil {
			return nil, false, err
		}
		child := pageDescriptor(page)[0]
		child.Ref = ref
		children = append(children, child)
		offset += size
	}
	return children, true, nil
}

func (session *treeSession) writeBranchGroups(ctx context.Context, operation MutationKind, subsystem string, level int, children []treeChild) ([]treeChild, bool, error) {
	if len(children) == 0 {
		return nil, true, nil
	}
	groups := splitCountLimit(len(children), session.tree.maximumPageItems())
	result := make([]treeChild, 0, len(groups))
	offset := 0
	for _, size := range groups {
		page := treePage{SchemaVersion: 1, Level: level, Children: append([]treeChild(nil), children[offset:offset+size]...)}
		ref, err := session.writePage(ctx, operation, subsystem, "", page)
		if err != nil {
			return nil, false, err
		}
		child := pageDescriptor(page)[0]
		child.Ref = ref
		result = append(result, child)
		offset += size
	}
	return result, true, nil
}

func (session *treeSession) writePage(ctx context.Context, operation MutationKind, subsystem, parallel string, page treePage) (string, error) {
	ref, _, err := session.tree.writePage(ctx, operation, subsystem, parallel, page)
	if err == nil {
		session.pages[ref] = page
	}
	return ref, err
}

func pageDescriptor(page treePage) []treeChild {
	child := treeChild{}
	if page.Level == 0 {
		if len(page.Values) > 0 {
			child.First, child.Last = page.Values[0].Key, page.Values[len(page.Values)-1].Key
		}
		child.Count = uint64(len(page.Values))
	} else if len(page.Children) > 0 {
		child.First, child.Last = page.Children[0].First, page.Children[len(page.Children)-1].Last
		for _, nested := range page.Children {
			child.Count += nested.Count
		}
	}
	return []treeChild{child}
}
