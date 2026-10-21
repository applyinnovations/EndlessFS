package portable

import (
	"context"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

func TestMetadataPackCacheReusesVerifiedWireAcrossFreshPlanningViews(t *testing.T) {
	ctx := context.Background()
	ledger := providerbudget.NewLedger()
	engine := openNamespaceTestEngine(t, budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger))
	scope := namespaceTestScope(t, domain.AreaLive)
	seedNamespaceBatchFiles(t, newNamespaceStore(engine), scope, 256)
	for range 2 {
		if _, err := engine.Files().PlanUploadSizes(ctx, scope.UserID(), uploadSizePlanFixture(1)); err != nil {
			t.Fatal(err)
		}
	}
	ledger.Reset()
	if _, err := engine.Files().PlanUploadSizes(ctx, scope.UserID(), uploadSizePlanFixture(1)); err != nil {
		t.Fatal(err)
	}
	mutableReads := 0
	for _, event := range ledger.Events() {
		if strings.Contains(event.Target, "/packs/") {
			t.Fatalf("fresh view downloaded previously authenticated immutable pack: %+v", event)
		}
		if event.Kind != providerbudget.RequestObjectGet {
			t.Fatalf("read-only warm planning mutated provider state: %+v", event)
		}
		mutableReads++
	}
	if mutableReads != 2 {
		t.Fatalf("fresh namespace/projection heads read %d times, want 2", mutableReads)
	}
}
