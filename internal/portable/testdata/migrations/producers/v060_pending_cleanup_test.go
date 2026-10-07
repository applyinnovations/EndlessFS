package portable_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
)

// Run only in an unmodified v0.6.0 production checkout. The wrapper interrupts
// transient lease deletion after the predecessor has committed terminal state.
type recoveryFixtureBackend struct {
	*objectmemory.Backend
	interruptCleanup bool
}

func (backend *recoveryFixtureBackend) Delete(ctx context.Context, key objectstore.Key, condition objectstore.DeleteCondition) error {
	if backend.interruptCleanup && strings.Contains(key.String(), "/leases/") {
		return domain.NewError(domain.ErrorUnavailable, "fixture: interrupted provider cleanup")
	}
	return backend.Backend.Delete(ctx, key, condition)
}

func TestProduceV060PendingCleanupFixture(t *testing.T) {
	output := os.Getenv("ENDLESSFS_RECOVERY_FIXTURE_OUTPUT")
	if output == "" {
		t.Fatal("ENDLESSFS_RECOVERY_FIXTURE_OUTPUT is required")
	}
	var family storageSchemaFixtureEntry
	for _, candidate := range storageSchemaFixtures {
		if candidate.file == "schema-010-application-complete.json" {
			family = candidate
		}
	}
	fixture := loadStorageSchemaFixture(t, family)
	stateBackend := &recoveryFixtureBackend{Backend: objectmemory.New()}
	fileBackend := objectmemory.New()
	if err := stateBackend.Import(fixture.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Import(fixture.FileObjects); err != nil {
		t.Fatal(err)
	}
	clock := domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour))
	server := newPortableDataServer(t, fileBackend, clock, 0xd1)
	options := schemaSplitMigrationOptions(stateBackend, fileBackend, clock, 0xd2, nil)
	options.Writer = currentWriterForSchemaFixture(t, fixture)
	engine, err := portable.Open(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := domain.ParseUserID(fixture.UserID)
	if err != nil {
		t.Fatal(err)
	}
	scope, _ := domain.NewScope(owner, domain.AreaLive)
	for _, completed := range []bool{true, false} {
		path := domain.MustParseUserPath("/pending-completed.txt")
		if !completed {
			path = domain.MustParseUserPath("/pending-aborted.txt")
		}
		capability, err := engine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: path, Size: 4, MediaType: "text/plain"})
		if err != nil {
			t.Fatal(err)
		}
		stateBackend.interruptCleanup = true
		if completed {
			request, _ := http.NewRequest(capability.Method, capability.URL, bytes.NewReader([]byte("safe")))
			for name, value := range capability.Headers {
				request.Header.Set(name, value)
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				t.Fatalf("upload status = %d", response.StatusCode)
			}
			_, err = engine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: path, Size: 4, MediaType: "text/plain"})
		} else {
			err = engine.Files().AbortUpload(context.Background(), scope, capability.UploadID)
		}
		if err != nil {
			t.Fatal(err)
		}
		stateBackend.interruptCleanup = false
	}
	fixture.SourceRelease = "v0.6.0"
	fixture.SourceCommit = "e7a9a46afede8e4b154e876700ed372e97105aed"
	fixture.CreatedAt = clock.Now()
	fixture.StateObjects, fixture.FileObjects = stateBackend.Export(), fileBackend.Export()
	body, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, append(body, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}
