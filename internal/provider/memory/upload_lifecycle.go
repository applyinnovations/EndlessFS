package memory

import (
	"context"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func (p *Provider) UploadLifecycle(ctx context.Context, scope domain.Scope, ids []domain.UploadID, resume bool) ([]domain.UploadLifecycleItem, error) {
	if err := validateContextScope(ctx, scope); err != nil {
		return nil, err
	}
	if len(ids) < 1 || len(ids) > domain.MaxUploadLifecycleItems {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid upload recovery page size")
	}
	seen := make(map[domain.UploadID]bool, len(ids))
	for _, id := range ids {
		if id == "" || len(id) > 256 || seen[id] {
			return nil, domain.NewError(domain.ErrorInvalid, "invalid or repeated upload recovery ID")
		}
		seen[id] = true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	items := make([]domain.UploadLifecycleItem, len(ids))
	for index, id := range ids {
		item := &items[index]
		item.Index, item.UploadID = index, id
		session, found := p.uploads[id]
		if !found || session.scope != scope {
			item.ErrorKind, item.NextAction = domain.ErrorNotFound, "attention"
			continue
		}
		state := session.state
		if state == domain.UploadStateActive && !p.clock.Now().Before(session.expiresAt) {
			state = domain.UploadStateExpired
		}
		status := domain.UploadStatus{UploadID: id, State: state, Path: session.requestedPath, Protocol: session.protocol, ConfirmedOffset: session.offset, DeclaredSize: session.size, ExpiresAt: session.expiresAt,
			DataComplete: (state == domain.UploadStateActive || state == domain.UploadStateCompleted) && session.offset == session.size && (session.size > 0 || session.dataReceived || state == domain.UploadStateCompleted)}
		item.Status = &status
		switch state {
		case domain.UploadStateExpired, domain.UploadStateAborted:
			item.NextAction = "restart"
		case domain.UploadStateCompleted:
			item.NextAction = "done"
		default:
			item.NextAction = "resume"
			if status.DataComplete {
				item.NextAction = "complete"
			} else if resume {
				capability := session.capability
				capability.BatchID, capability.BatchIndex, capability.BatchCount = session.batchID, session.batchIndex, session.batchCount
				capability.Headers = make(map[string]string, len(session.capability.Headers))
				for name, value := range session.capability.Headers {
					capability.Headers[name] = value
				}
				item.Capability, item.NextAction = &capability, "transfer"
			}
		}
	}
	return items, ctx.Err()
}
