package architecturelab

import (
	"context"
	"errors"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

type derivedViewHead struct {
	SchemaVersion  int    `json:"schemaVersion"`
	SourceRevision uint64 `json:"sourceRevision"`
	PageRef        string `json:"pageRef"`
}

// derivedView is a disposable, rebuildable projection bound to an
// authoritative namespace revision. Foreground namespace commits never update
// it; readers either obtain a revision-matched immutable page or request a
// rebuild/fallback. This executable shape covers sort, duplicate, similarity,
// accounting, search, and audit projections.
type derivedView struct {
	backend objectstore.Backend
	id      string
	headKey objectstore.Key
}

func openDerivedView(ctx context.Context, backend objectstore.Backend, id string, sourceRevision uint64, page []byte) (*derivedView, error) {
	if backend == nil || !domainPattern.MatchString(id) || sourceRevision == 0 || len(page) == 0 {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid derived view")
	}
	view := &derivedView{backend: backend, id: id, headKey: candidateKey("derived-view", id, "head.json")}
	pageKey := candidateKey("derived-view", id, "pages/"+digest(page)+".json")
	if err := createImmutable(derivedTrace(ctx, "rebuild", "derived-page", "view-page"), backend, pageKey, page); err != nil {
		return nil, err
	}
	headBody, _ := encode(derivedViewHead{SchemaVersion: 1, SourceRevision: sourceRevision, PageRef: pageKey.String()})
	if _, err := backend.Put(derivedTrace(ctx, "rebuild", "derived-head", ""), view.headKey, headBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil && !errors.Is(err, domain.ErrConflict) {
		return nil, err
	}
	return view, nil
}

func derivedTrace(ctx context.Context, operation, subsystem, parallel string) context.Context {
	return trace(ctx, MutationKind(operation), subsystem, parallel)
}

func (view *derivedView) Read(ctx context.Context, expectedRevision uint64) ([]byte, error) {
	headObject, err := view.backend.Get(derivedTrace(ctx, "derived-read", "derived-head", ""), view.headKey)
	if err != nil {
		return nil, err
	}
	var head derivedViewHead
	if decode(headObject.Body, &head) != nil || head.SchemaVersion != 1 || head.SourceRevision != expectedRevision || head.PageRef == "" {
		return nil, domain.NewError(domain.ErrorPreconditionFailed, "derived view is stale or invalid")
	}
	pageKey, err := objectstore.ParseKey(head.PageRef)
	if err != nil {
		return nil, err
	}
	page, err := view.backend.Get(derivedTrace(ctx, "derived-read", "derived-page", ""), pageKey)
	if err != nil {
		return nil, err
	}
	if digest(page.Body) != keyDigest(pageKey) {
		return nil, domain.NewError(domain.ErrorInvalid, "derived view page is corrupt")
	}
	return append([]byte(nil), page.Body...), nil
}

func (view *derivedView) CreatePlan(ctx context.Context, planID string, body []byte) (objectstore.Key, error) {
	if planID == "" || len(body) == 0 {
		return objectstore.Key{}, domain.NewError(domain.ErrorInvalid, "invalid reconciliation plan")
	}
	key := candidateKey("derived-view", view.id, "plans/"+digest([]byte(planID))+".json")
	if err := createImmutable(derivedTrace(ctx, "reconciliation-preview", "reconciliation-plan", ""), view.backend, key, body); err != nil {
		return objectstore.Key{}, err
	}
	return key, nil
}
