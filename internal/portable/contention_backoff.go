package portable

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

// Replicas that lose a conditional write on the shared write gate or
// superblock retry. Retrying at once lets several replicas keep
// invalidating each other's attempts indefinitely when they are short of
// CPU; a jittered exponential delay lets one of them finish its step.
const (
	contentionBackoffBase = 500 * time.Microsecond
	contentionBackoffCap  = 50 * time.Millisecond
)

// contentionBackoff spaces out retries after lost compare-and-swap races.
// The zero value is ready to use; use one value per retry loop.
type contentionBackoff struct {
	attempt uint
}

// wait blocks for a random delay in (0, limit], where limit doubles per
// attempt from contentionBackoffBase up to contentionBackoffCap. It returns
// an unavailable error when ctx ends first.
func (backoff *contentionBackoff) wait(ctx context.Context) error {
	limit := contentionBackoffCap
	if backoff.attempt < 16 {
		limit = min(contentionBackoffBase<<backoff.attempt, contentionBackoffCap)
	}
	backoff.attempt++
	timer := time.NewTimer(rand.N(limit) + 1)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return domain.WrapError(domain.ErrorUnavailable, "contended storage write retry cancelled", ctx.Err())
	case <-timer.C:
		return nil
	}
}
