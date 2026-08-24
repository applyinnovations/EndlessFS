package architecturelab

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

const maxTreePageItems = 64

type treeValue struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

type treeChild struct {
	First string `json:"first"`
	Last  string `json:"last"`
	Ref   string `json:"ref"`
	Count uint64 `json:"count"`
}

type treePage struct {
	SchemaVersion int         `json:"schemaVersion"`
	Level         int         `json:"level"`
	Values        []treeValue `json:"values,omitempty"`
	Children      []treeChild `json:"children,omitempty"`
}

type immutableTree struct {
	backend   objectstore.Backend
	domainID  string
	candidate string
	pageItems int
}

func (tree immutableTree) maximumPageItems() int {
	if tree.pageItems > 0 {
		return tree.pageItems
	}
	return maxTreePageItems
}

func (tree immutableTree) empty(ctx context.Context, operation MutationKind, subsystem string) (string, error) {
	ref, _, err := tree.writePage(ctx, operation, subsystem, "", treePage{SchemaVersion: 1, Level: 0, Values: []treeValue{}})
	return ref, err
}

func (tree immutableTree) emptyReference() string {
	body, _ := encode(treePage{SchemaVersion: 1, Level: 0, Values: []treeValue{}})
	return tree.pageKey(body).String()
}

func (tree immutableTree) lookup(ctx context.Context, operation MutationKind, subsystem, ref, key string) (json.RawMessage, bool, error) {
	var expected *treeChild
	expectedLevel := -1
	for {
		page, err := tree.readPage(ctx, operation, subsystem, ref)
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

func (tree immutableTree) upsert(ctx context.Context, operation MutationKind, subsystem, ref, key string, value any) (string, bool, error) {
	body, err := encode(value)
	if err != nil {
		return "", false, err
	}
	return tree.mutate(ctx, operation, subsystem, ref, key, body, false)
}

func (tree immutableTree) remove(ctx context.Context, operation MutationKind, subsystem, ref, key string) (string, bool, error) {
	return tree.mutate(ctx, operation, subsystem, ref, key, nil, true)
}

func (tree immutableTree) mutate(ctx context.Context, operation MutationKind, subsystem, ref, key string, value json.RawMessage, remove bool) (string, bool, error) {
	page, err := tree.readPage(ctx, operation, subsystem, ref)
	if err != nil {
		return "", false, err
	}
	replacements, existed, err := tree.rewrite(ctx, operation, subsystem, page, key, value, remove)
	if err != nil {
		return "", false, err
	}
	if remove && !existed {
		return ref, false, nil
	}
	if len(replacements) == 0 {
		rootRef, err := tree.empty(ctx, operation, subsystem)
		return rootRef, existed, err
	}
	if len(replacements) == 1 {
		return replacements[0].Ref, existed, nil
	}
	root := treePage{SchemaVersion: 1, Level: page.Level + 1, Children: replacements}
	rootRef, _, err := tree.writePage(ctx, operation, subsystem, "tree-root", root)
	return rootRef, existed, err
}

func (tree immutableTree) rewrite(ctx context.Context, operation MutationKind, subsystem string, page treePage, key string, value json.RawMessage, remove bool) ([]treeChild, bool, error) {
	if page.Level == 0 {
		values := append([]treeValue(nil), page.Values...)
		index := sort.Search(len(values), func(index int) bool { return values[index].Key >= key })
		existed := index < len(values) && values[index].Key == key
		if remove {
			if !existed {
				return nil, false, nil
			}
			values = append(values[:index], values[index+1:]...)
		} else if existed {
			values[index].Value = append(json.RawMessage(nil), value...)
		} else {
			values = append(values, treeValue{})
			copy(values[index+1:], values[index:])
			values[index] = treeValue{Key: key, Value: append(json.RawMessage(nil), value...)}
		}
		return tree.writeLeafGroups(ctx, operation, subsystem, values, existed)
	}
	index := childIndex(page.Children, key)
	if index < 0 {
		if remove {
			return nil, false, nil
		}
		index = len(page.Children) - 1
		if index < 0 {
			return nil, false, domain.NewError(domain.ErrorInvalid, "tree branch has no children")
		}
	}
	childPage, err := tree.readPage(ctx, operation, subsystem, page.Children[index].Ref)
	if err != nil {
		return nil, false, err
	}
	if err := verifyTreeChild(page.Children[index], childPage, page.Level-1); err != nil {
		return nil, false, err
	}
	childReplacements, existed, err := tree.rewrite(ctx, operation, subsystem, childPage, key, value, remove)
	if err != nil || remove && !existed {
		return nil, existed, err
	}
	children := make([]treeChild, 0, len(page.Children)-1+len(childReplacements))
	children = append(children, page.Children[:index]...)
	children = append(children, childReplacements...)
	children = append(children, page.Children[index+1:]...)
	return tree.writeBranchGroups(ctx, operation, subsystem, page.Level, children, existed)
}

func (tree immutableTree) writeLeafGroups(ctx context.Context, operation MutationKind, subsystem string, values []treeValue, existed bool) ([]treeChild, bool, error) {
	if len(values) == 0 {
		return nil, existed, nil
	}
	groups := splitCountLimit(len(values), tree.maximumPageItems())
	result := make([]treeChild, 0, len(groups))
	offset := 0
	for index, size := range groups {
		pageValues := append([]treeValue(nil), values[offset:offset+size]...)
		ref, child, err := tree.writePage(ctx, operation, subsystem, parallelPageGroup(index), treePage{SchemaVersion: 1, Level: 0, Values: pageValues})
		if err != nil {
			return nil, false, err
		}
		child.Ref = ref
		result = append(result, child)
		offset += size
	}
	return result, existed, nil
}

func (tree immutableTree) writeBranchGroups(ctx context.Context, operation MutationKind, subsystem string, level int, children []treeChild, existed bool) ([]treeChild, bool, error) {
	if len(children) == 0 {
		return nil, existed, nil
	}
	groups := splitCountLimit(len(children), tree.maximumPageItems())
	result := make([]treeChild, 0, len(groups))
	offset := 0
	for index, size := range groups {
		pageChildren := append([]treeChild(nil), children[offset:offset+size]...)
		ref, child, err := tree.writePage(ctx, operation, subsystem, parallelPageGroup(index), treePage{SchemaVersion: 1, Level: level, Children: pageChildren})
		if err != nil {
			return nil, false, err
		}
		child.Ref = ref
		result = append(result, child)
		offset += size
	}
	return result, existed, nil
}

func (tree immutableTree) walk(ctx context.Context, operation MutationKind, subsystem, ref string, visit func(string, json.RawMessage) error) error {
	page, err := tree.readPage(ctx, operation, subsystem, ref)
	if err != nil {
		return err
	}
	if page.Level == 0 {
		for _, value := range page.Values {
			if err := visit(value.Key, append(json.RawMessage(nil), value.Value...)); err != nil {
				return err
			}
		}
		return nil
	}
	for _, child := range page.Children {
		childPage, err := tree.readPage(ctx, operation, subsystem, child.Ref)
		if err != nil {
			return err
		}
		if err := verifyTreeChild(child, childPage, page.Level-1); err != nil {
			return err
		}
		if err := tree.walk(ctx, operation, subsystem, child.Ref, visit); err != nil {
			return err
		}
	}
	return nil
}

func verifyTreeChild(expected treeChild, page treePage, level int) error {
	actual := pageDescriptor(page)[0]
	if page.Level != level || actual.First != expected.First || actual.Last != expected.Last || actual.Count != expected.Count {
		return domain.NewError(domain.ErrorInvalid, "tree child descriptor does not match child page")
	}
	return nil
}

func (tree immutableTree) readPage(ctx context.Context, operation MutationKind, subsystem, ref string) (treePage, error) {
	return tree.readPageGrouped(ctx, operation, subsystem, "", ref)
}

func (tree immutableTree) readPageGrouped(ctx context.Context, operation MutationKind, subsystem, parallel, ref string) (treePage, error) {
	key, err := objectstore.ParseKey(ref)
	if err != nil {
		return treePage{}, err
	}
	object, err := tree.backend.Get(trace(ctx, operation, subsystem, parallel), key)
	if err != nil {
		return treePage{}, err
	}
	if digest(object.Body) != keyDigest(key) {
		return treePage{}, domain.NewError(domain.ErrorInvalid, "tree page digest mismatch")
	}
	var page treePage
	if err := decode(object.Body, &page); err != nil || validateTreePageLimit(page, tree.maximumPageItems()) != nil {
		return treePage{}, domain.NewError(domain.ErrorInvalid, "invalid tree page")
	}
	return page, nil
}

func (tree immutableTree) writePage(ctx context.Context, operation MutationKind, subsystem, parallel string, page treePage) (string, treeChild, error) {
	if err := validateTreePageLimit(page, tree.maximumPageItems()); err != nil {
		return "", treeChild{}, err
	}
	body, err := encode(page)
	if err != nil {
		return "", treeChild{}, err
	}
	key := tree.pageKey(body)
	if err := createImmutable(trace(ctx, operation, subsystem, parallel), tree.backend, key, body); err != nil {
		return "", treeChild{}, err
	}
	child := treeChild{Ref: key.String()}
	if page.Level == 0 {
		if len(page.Values) > 0 {
			child.First, child.Last = page.Values[0].Key, page.Values[len(page.Values)-1].Key
		}
		child.Count = uint64(len(page.Values))
	} else {
		child.First, child.Last = page.Children[0].First, page.Children[len(page.Children)-1].Last
		for _, nested := range page.Children {
			child.Count += nested.Count
		}
	}
	return key.String(), child, nil
}

func (tree immutableTree) pageKey(body []byte) objectstore.Key {
	candidate := tree.candidate
	if candidate == "" {
		candidate = "paged"
	}
	return candidateKey(candidate, tree.domainID, "pages/"+digest(body)+".json")
}

func validateTreePage(page treePage) error {
	return validateTreePageLimit(page, maxTreePageItems)
}

func validateTreePageLimit(page treePage, maximum int) error {
	if maximum < 2 || page.SchemaVersion != 1 || page.Level < 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid tree page header")
	}
	if page.Level == 0 {
		if page.Children != nil || len(page.Values) > maximum {
			return domain.NewError(domain.ErrorInvalid, "invalid tree leaf")
		}
		for index, value := range page.Values {
			if value.Key == "" || len(value.Value) == 0 || index > 0 && page.Values[index-1].Key >= value.Key {
				return domain.NewError(domain.ErrorInvalid, "invalid tree value order")
			}
		}
		return nil
	}
	if page.Values != nil || len(page.Children) == 0 || len(page.Children) > maximum {
		return domain.NewError(domain.ErrorInvalid, "invalid tree branch")
	}
	for index, child := range page.Children {
		if child.First == "" || child.Last < child.First || child.Ref == "" || child.Count == 0 || index > 0 && page.Children[index-1].Last >= child.First {
			return domain.NewError(domain.ErrorInvalid, "invalid tree child order")
		}
	}
	return nil
}

func childIndex(children []treeChild, key string) int {
	if len(children) == 0 {
		return -1
	}
	index := sort.Search(len(children), func(index int) bool { return children[index].Last >= key })
	if index == len(children) {
		return len(children) - 1
	}
	return index
}

func splitCountLimit(count, maximum int) []int {
	if count <= 0 {
		return nil
	}
	if count <= maximum {
		return []int{count}
	}
	groups := (count + maximum - 1) / maximum
	base, remainder := count/groups, count%groups
	result := make([]int, groups)
	for index := range result {
		result[index] = base
		if index < remainder {
			result[index]++
		}
	}
	return result
}

func parallelPageGroup(index int) string {
	if index == 0 {
		return "tree-page-left"
	}
	return "tree-page-right"
}
