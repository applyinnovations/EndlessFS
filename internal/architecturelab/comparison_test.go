package architecturelab

import (
	"context"
	"fmt"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

func TestCandidateComparativeEconomics(t *testing.T) {
	ctx := context.Background()
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	for _, descendants := range []int{1, 32} {
		scenario := fmt.Sprintf("move-directory-%d-descendants", descendants)
		measurements := make([]Measurement, 0, len(CandidateFactories()))
		for _, factory := range CandidateFactories() {
			base := objectmemory.New()
			ledger := providerbudget.NewLedger()
			backend := budgettest.Wrap(providerbudget.RoleState, base, ledger)
			engine, err := factory.Open(ctx, backend, Options{DomainID: fmt.Sprintf("scale-%d", descendants)})
			if err != nil {
				t.Fatalf("%s open: %v", factory.Name, err)
			}
			if _, err := engine.Mutate(ctx, Mutation{ID: "mkdir-project", Kind: MutationCreateDirectory, ToArea: AreaLive, Destination: "/Project", NodeID: "project"}); err != nil {
				t.Fatalf("%s mkdir: %v", factory.Name, err)
			}
			for index := 0; index < descendants; index++ {
				name := fmt.Sprintf("file-%04d", index)
				if _, err := engine.Mutate(ctx, Mutation{ID: "upload-" + name, Kind: MutationCreateFile, ToArea: AreaLive, Destination: "/Project/" + name, NodeID: name, Size: 1024, BlobIdentity: "blob-" + name}); err != nil {
					t.Fatalf("%s seed %d: %v", factory.Name, index, err)
				}
			}
			ledger.Reset()
			if _, err := engine.Mutate(ctx, Mutation{ID: "move-project", Kind: MutationMove, FromArea: AreaLive, ToArea: AreaLive, Source: "/Project", Destination: "/Moved"}); err != nil {
				t.Fatalf("%s move: %v", factory.Name, err)
			}
			events := ledger.Events()
			for _, event := range events {
				if event.Role != providerbudget.RoleState || event.Operation != string(MutationMove) || event.Subsystem == "" {
					t.Fatalf("%s invalid move event: %+v", factory.Name, event)
				}
			}
			totals, err := model.Estimate(events)
			if err != nil {
				t.Fatalf("%s estimate: %v", factory.Name, err)
			}
			objects, bytes, err := candidateInventory(ctx, base)
			if err != nil {
				t.Fatalf("%s inventory: %v", factory.Name, err)
			}
			measurement := MeasurementFromTotals(factory.Name, scenario, totals, objects, bytes)
			measurement.Limitations = candidateLimitations(factory.Name)
			measurements = append(measurements, measurement)
			t.Logf("%s %+v limitations=%v", scenario+"/"+factory.Name, measurement.Metrics, measurement.Limitations)
		}
		frontier := ParetoFrontier(measurements)
		if len(frontier) == 0 {
			t.Fatalf("%s has no valid frontier", scenario)
		}
		t.Logf("%s frontier=%v", scenario, candidateNames(frontier))
	}
}

func candidateInventory(ctx context.Context, backend objectstore.Backend) (int64, int64, error) {
	var objects, bytes int64
	cursor := ""
	for {
		page, err := backend.List(ctx, objectstore.ListRequest{Prefix: "endlessfs/research/", Limit: 1000, Cursor: cursor})
		if err != nil {
			return 0, 0, err
		}
		for _, object := range page.Objects {
			objects++
			bytes += object.Size
		}
		if page.NextCursor == "" {
			return objects, bytes, nil
		}
		cursor = page.NextCursor
	}
}

func candidateLimitations(name string) []string {
	switch name {
	case "packed-snapshot":
		return []string{"foreground request and write bytes scale with total namespace metadata"}
	case "immutable-journal":
		return []string{"read requests and replay work scale with uncompacted history"}
	case "bounded-delta":
		return []string{"base read and compaction bytes scale with total namespace metadata"}
	case "immutable-directory-graph":
		return []string{"directory read and rewrite bytes scale with immediate directory width", "idempotency outcomes grow in the head"}
	case "paged-directory-graph":
		return []string{"separate directory objects add avoidable reads and writes", "foreground work is bounded by path and page depth"}
	case "embedded-paged-namespace":
		return []string{"foreground work is bounded by path and page depth", "old idempotency lookup follows the paged outcome index", "immutable garbage requires watermark-aware collection"}
	case "claimed-paged-namespace":
		return []string{"foreground work is bounded by path and page depth", "durable claims add two writes to successful mutations", "claims require an explicit retention and collection policy", "immutable garbage requires watermark-aware collection"}
	case "paged-delta-hybrid":
		return []string{"foreground reads scale with path depth and the bounded in-head delta window", "compaction rewrites only pages changed by the bounded window", "durable claims require retention and collection", "saturation can move compaction onto the foreground path unless background capacity is sufficient"}
	default:
		return []string{"unclassified candidate"}
	}
}

func candidateNames(measurements []Measurement) []string {
	result := make([]string, len(measurements))
	for index := range measurements {
		result[index] = measurements[index].Candidate
	}
	return result
}
