package architecturelab

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

type Engine interface {
	Name() string
	Mutate(context.Context, Mutation) (Outcome, error)
	Snapshot(context.Context) (Snapshot, error)
	Freeze(context.Context, string) (Checkpoint, error)
	Compact(context.Context) error
}

type Checkpoint struct {
	ID       string `json:"id"`
	Revision uint64 `json:"revision"`
	Digest   string `json:"digest"`
}

type Options struct {
	DomainID string
}

type Factory struct {
	Name string
	Open func(context.Context, objectstore.Backend, Options) (Engine, error)
}

func CandidateFactories() []Factory {
	return []Factory{
		{Name: "packed-snapshot", Open: openPacked},
		{Name: "immutable-journal", Open: openJournal},
		{Name: "bounded-delta", Open: openDelta},
		{Name: "immutable-directory-graph", Open: openGraph},
		{Name: "paged-directory-graph", Open: openPagedGraph},
		{Name: "embedded-paged-namespace", Open: openEmbeddedGraph},
		{Name: "claimed-paged-namespace", Open: openClaimedEmbeddedGraph},
		{Name: "paged-delta-hybrid", Open: openHybrid},
	}
}

var domainPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func validateOptions(backend objectstore.Backend, options Options) error {
	if backend == nil || !domainPattern.MatchString(options.DomainID) {
		return domain.NewError(domain.ErrorInvalid, "invalid architecture candidate options")
	}
	return nil
}

func candidateKey(candidate, domainID, suffix string) objectstore.Key {
	return objectstore.MustKey(fmt.Sprintf("endlessfs/research/%s/%s/%s", candidate, domainID, suffix))
}

func trace(ctx context.Context, operation MutationKind, subsystem, parallel string) context.Context {
	return providerbudget.WithTrace(ctx, providerbudget.Trace{Operation: string(operation), Subsystem: subsystem, ParallelGroup: parallel})
}

func checkpointTrace(ctx context.Context, subsystem string) context.Context {
	return providerbudget.WithTrace(ctx, providerbudget.Trace{Operation: "checkpoint", Subsystem: subsystem})
}

func createImmutable(ctx context.Context, backend objectstore.Backend, key objectstore.Key, body []byte) error {
	if _, err := backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err == nil {
		return nil
	} else if !errors.Is(err, domain.ErrConflict) {
		return err
	}
	existing, err := backend.Get(ctx, key)
	if err != nil {
		return err
	}
	if digest(existing.Body) != digest(body) {
		return domain.NewError(domain.ErrorInvalid, "immutable candidate object conflicts")
	}
	return nil
}
