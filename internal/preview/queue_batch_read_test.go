package preview

import (
	"context"
	"errors"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/state"
)

// Fault adapter keeps all existing fault injection active while
// exercising the optional completion batch path. Mutate remains authoritative.
func (s queueFaultStore) GetMany(ctx context.Context, keys []state.Key) ([]state.Value, error) {
	if s.get == nil {
		if reader, ok := s.AtomicStore.(interface {
			GetMany(context.Context, []state.Key) ([]state.Value, error)
		}); ok {
			return reader.GetMany(ctx, keys)
		}
	}
	values := make([]state.Value, len(keys))
	for i, key := range keys {
		value, err := s.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		values[i] = value
	}
	return values, nil
}

func TestThumbnailCompletionBatchReadFailuresPreserveClaim(t *testing.T) {
	for _, kind := range []string{"unavailable", "shape", "corrupt-job", "corrupt-lane", "missing-fallback"} {
		t.Run(kind, func(t *testing.T) {
			s, binding, item, _ := queueTestService(t)
			if _, err := s.enqueueThumbnail(t.Context(), binding, item); err != nil {
				t.Fatal(err)
			}
			claim, found, err := s.claimThumbnail(t.Context())
			if err != nil || !found {
				t.Fatalf("claim: %v %v", found, err)
			}
			base := s.state
			job, err := base.Get(t.Context(), itemKey(claim.job))
			if err != nil {
				t.Fatal(err)
			}
			lane, err := base.Get(t.Context(), claim.laneKey)
			if err != nil {
				t.Fatal(err)
			}
			reader := queueBatchReader{AtomicStore: base, values: []state.Value{job, lane}}
			switch kind {
			case "unavailable":
				reader.err = domain.ErrUnavailable
			case "shape":
				reader.values = reader.values[:1]
			case "corrupt-job":
				reader.values[0].Data = []byte(`{}`)
			case "corrupt-lane":
				reader.values[1].Data = []byte(`{}`)
			case "missing-fallback":
				reader.err = domain.ErrNotFound
			}
			s.state = reader
			err = s.finishThumbnail(t.Context(), claim, ItemResult{State: StateReady}, nil)
			if kind == "missing-fallback" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := base.Get(t.Context(), itemKey(claim.job)); !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("fallback did not finish: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("invalid batch completed work")
			}
			value, err := base.Get(t.Context(), itemKey(claim.job))
			if err != nil || value.Version != job.Version {
				t.Fatalf("invalid batch changed claimed job: %v", err)
			}
		})
	}
}
