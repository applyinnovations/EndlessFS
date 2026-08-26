package portable

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
)

func TestDuplicateDirectoryComparisonRejectsByteOverflow(t *testing.T) {
	var files, bytes int64
	err := addDuplicateTotals(&files, &bytes, 2, math.MaxInt64)
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("addDuplicateTotals() error = %v; want invalid overflow", err)
	}
}

func TestDuplicateOccurrencePaginationSurfacesCursorGenerationFailure(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	live := namespaceTestScope(t, domain.AreaLive)
	seedNamespaceBatchFiles(t, newNamespaceStore(engine), live, 3)
	groups, err := engine.Files().ListDuplicateGroups(ctx, live.UserID(), domain.DuplicateGroupRequest{Kind: domain.DuplicateFile, Limit: 1})
	if err != nil || len(groups.Groups) != 1 {
		t.Fatalf("duplicate group seed = %+v, %v", groups, err)
	}
	engine.ids = domain.NewIDGenerator(strings.NewReader(""))
	if _, err := engine.Files().ListDuplicateOccurrences(ctx, live.UserID(), domain.DuplicateOccurrenceRequest{GroupID: groups.Groups[0].ID, Limit: 1}); err == nil {
		t.Fatal("duplicate occurrence pagination discarded cursor-generation failure")
	}
}
