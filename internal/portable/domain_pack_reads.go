package portable

import (
	"context"
	"errors"
	"sync"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// Share only in-flight authenticated immutable reads. No head, error, or idle
// pack is cached. The coordinator belongs to one engine/backend; the full
// canonical key binds the owner, domain kind and physical pack identity.
type consistencyDomainPackReads struct {
	mu    sync.Mutex
	loads map[objectstore.Key]*consistencyDomainPackLoad
}

func (reads *consistencyDomainPackReads) read(ctx context.Context, key objectstore.Key, fetch func() (map[string]storageformat.DomainPage, error)) (map[string]storageformat.DomainPage, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, domain.WrapError(domain.ErrorUnavailable, "consistency-domain pack read canceled", err)
		}
		reads.mu.Lock()
		if load := reads.loads[key]; load != nil {
			reads.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, domain.WrapError(domain.ErrorUnavailable, "consistency-domain pack read canceled", ctx.Err())
			case <-load.done:
				// A canceled leader does not cancel independent callers. Retry
				// through their own context; all-waiter cancellation leaves no
				// detached provider work or background goroutine.
				if errors.Is(load.err, context.Canceled) || errors.Is(load.err, context.DeadlineExceeded) {
					continue
				}
				return load.pages, load.err
			}
		}
		load := &consistencyDomainPackLoad{done: make(chan struct{})}
		if reads.loads == nil {
			reads.loads = make(map[objectstore.Key]*consistencyDomainPackLoad)
		}
		reads.loads[key] = load
		reads.mu.Unlock()
		load.pages, load.err = fetch()
		reads.mu.Lock()
		delete(reads.loads, key)
		close(load.done)
		reads.mu.Unlock()
		return load.pages, load.err
	}
}
