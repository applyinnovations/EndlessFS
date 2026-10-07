package portable_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
)

// Run in v0.7.1 without any production edits, using the exact v0.6.0 fixture.
// Preserve the objects left by its failed migration without normalization.
func TestProduceV071FailedCleanupMigrationFixture(t *testing.T) {
	body, err := os.ReadFile(os.Getenv("ENDLESSFS_RECOVERY_FIXTURE_INPUT"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture storageSchemaFixture
	if err := json.Unmarshal(body, &fixture); err != nil {
		t.Fatal(err)
	}
	stateBackend, fileBackend := objectmemory.New(), objectmemory.New()
	if err := stateBackend.Import(fixture.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Import(fixture.FileObjects); err != nil {
		t.Fatal(err)
	}
	clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
	options := schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 0xd3, nil)
	options.Writer = currentWriterForSchemaFixture(t, fixture)
	if _, err := portable.Open(context.Background(), options); err == nil || !strings.Contains(err.Error(), "consistency domain is frozen") {
		t.Fatalf("v0.7.1 migration error = %v", err)
	}
	fixture.SourceRelease = "v0.7.1-interrupted"
	fixture.SourceCommit = "9650c8a1c8e107f17e71b4272200c77f6ed42eac"
	fixture.CreatedAt = clock.Now()
	fixture.StateObjects, fixture.FileObjects = stateBackend.Export(), fileBackend.Export()
	body, err = json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("ENDLESSFS_RECOVERY_FIXTURE_OUTPUT"), append(body, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}
