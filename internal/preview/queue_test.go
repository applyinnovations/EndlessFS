package preview_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/preview"
	"github.com/applyinnovations/endlessfs/internal/state"
)

func thumbnailJobs(t *testing.T, env previewEnvironment) int {
	t.Helper()
	page, err := env.applicationState.List(t.Context(), state.MustPrefix(state.NamespaceOperations, "admin", "thumbnail-job"), state.PageRequest{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return len(page.Items)
}

func queueImages(t *testing.T, env previewEnvironment, count int) []preview.ItemRequest {
	t.Helper()
	items := make([]preview.ItemRequest, count)
	for i := range items {
		entry := env.uploadImage(t, fmt.Sprintf("/queue-%02d.png", i), 8+i, 4)
		items[i] = preview.ItemRequest{Path: entry.Path, Version: entry.Version, Variant: 256}
	}
	return items
}

func resolveQueue(t *testing.T, service *preview.Service, env previewEnvironment, items []preview.ItemRequest) preview.ResolveResponse {
	t.Helper()
	response, err := service.Resolve(t.Context(), env.owner, preview.ResolveRequest{Items: items})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func queueReplica(t *testing.T, env previewEnvironment, generator preview.Generator, concurrency int) *preview.Service {
	t.Helper()
	service, err := preview.NewService(preview.Options{Automatic: true, ApplicationState: env.applicationState, MaxConcurrency: concurrency, OperationTimeout: 5 * time.Second}, env.source, env.store, []preview.Generator{generator}, env.sourceServer.Client(), env.ids, env.clock)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestThumbnailQueueOverlappingBatchesRemain64JobsAfter100Requests(t *testing.T) {
	env := newPreviewEnvironment(t, preview.Options{Automatic: true})
	items := queueImages(t, env, 64)
	for repeat := range 100 {
		// Rotating the slice changes batch identity and overlaps every job.
		rotated := append(append([]preview.ItemRequest{}, items[repeat%64:]...), items[:repeat%64]...)
		response := resolveQueue(t, env.service, env, rotated)
		for _, item := range response.Items {
			if item.State != preview.StateQueued {
				t.Fatalf("request %d: %+v", repeat, item)
			}
		}
	}
	if got := thumbnailJobs(t, env); got != 64 {
		t.Fatalf("jobs=%d, want 64", got)
	}
	if env.generator.Calls() != 0 {
		t.Fatal("resolve decoded a source before acknowledgement")
	}
	// A fresh process discovers all work without another browser request.
	restarted := queueReplica(t, env, env.generator, 1)
	for range 64 {
		if worked, err := restarted.WorkOne(t.Context()); err != nil || !worked {
			t.Fatalf("progress=%v: %v", worked, err)
		}
	}
	if got := thumbnailJobs(t, env); got != 0 {
		t.Fatalf("completed jobs=%d", got)
	}
	response := resolveQueue(t, restarted, env, items)
	for _, item := range response.Items {
		if item.State != preview.StateReady {
			t.Fatalf("did not complete: %+v", item)
		}
	}
	if env.generator.Calls() != 64 {
		t.Fatalf("decodes=%d, want 64", env.generator.Calls())
	}
	if got := thumbnailJobs(t, env); got != 0 {
		t.Fatalf("ready artifacts were queued again: %d", got)
	}
}

func TestThumbnailQueuePrioritizesRefreshedRequestsAndServesOldWork(t *testing.T) {
	env := newPreviewEnvironment(t, preview.Options{Automatic: true})
	items := queueImages(t, env, 10)
	for _, item := range items {
		resolveQueue(t, env.service, env, []preview.ItemRequest{item})
		env.clock.Advance(time.Second)
	}
	// Refreshing an old artifact raises priority without creating another job.
	resolveQueue(t, env.service, env, items[:1])
	if worked, err := env.service.WorkOne(t.Context()); err != nil || !worked {
		t.Fatal(err)
	}
	if got := resolveQueue(t, env.service, env, items[:1]).Items[0].State; got != preview.StateReady {
		t.Fatalf("refreshed old request=%s", got)
	}
	for range 7 {
		if worked, err := env.service.WorkOne(t.Context()); err != nil || !worked {
			t.Fatal(err)
		}
	}
	// Every eighth claim chooses the oldest original enqueue time, even if
	// newer requests continue to refresh recent priority.
	if got := resolveQueue(t, env.service, env, items[1:2]).Items[0].State; got != preview.StateReady {
		t.Fatalf("oldest work starved: %s", got)
	}
}

func TestThumbnailQueueCoordinatesGlobalConcurrencyAcrossReplicas(t *testing.T) {
	env := newPreviewEnvironmentWithoutService(t)
	generator := newLeaseTestGenerator()
	first, second := queueReplica(t, env, generator, 1), queueReplica(t, env, generator, 1)
	items := queueImages(t, env, 2)
	resolveQueue(t, first, env, items)
	done := make(chan error, 1)
	go func() { _, err := first.WorkOne(t.Context()); done <- err }()
	<-generator.firstStarted
	if worked, err := second.WorkOne(t.Context()); err != nil || worked {
		t.Fatalf("second replica exceeded shared concurrency: worked=%v err=%v", worked, err)
	}
	// Overlapping requests during a claim cannot reset its lease or duplicate it.
	resolveQueue(t, second, env, items)
	close(generator.releaseFirst)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if worked, err := second.WorkOne(t.Context()); err != nil || !worked {
		t.Fatalf("second job: %v %v", worked, err)
	}
	if generator.Calls() != 2 {
		t.Fatalf("decodes=%d", generator.Calls())
	}
}

func TestThumbnailQueueDoesNotReadReplacedSource(t *testing.T) {
	env := newPreviewEnvironment(t, preview.Options{Automatic: true})
	items := queueImages(t, env, 1)
	resolveQueue(t, env.service, env, items)
	env.uploadBytesWithConflict(t, items[0].Path.String(), encodePreviewPNG(t, 24, 12), "image/png", domain.ConflictReplace, items[0].Version)
	before := env.source.Instrumentation()
	if worked, err := env.service.WorkOne(t.Context()); err != nil || !worked {
		t.Fatal(err)
	}
	assertNoSourceRead(t, before, env.source.Instrumentation())
	if env.generator.Calls() != 0 {
		t.Fatal("decoded replaced source")
	}
}

func TestThumbnailQueueRunStopsWithoutWork(t *testing.T) {
	env := newPreviewEnvironment(t, preview.Options{Automatic: true})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); env.service.Run(ctx) }()
	cancel()
	<-done
}

func TestThumbnailQueueRechecksArtifactsCreatedAfterEnqueue(t *testing.T) {
	env := newPreviewEnvironment(t, preview.Options{Automatic: true})
	items := queueImages(t, env, 1)
	resolveQueue(t, env.service, env, items)
	item := items[0]
	operation, err := env.service.Generate(t.Context(), env.owner, preview.GenerateRequest{Path: item.Path, Version: item.Version, Variant: item.Variant, IdempotencyKey: "queue-manual-generation-0001"})
	if err != nil || operation.Result == nil || operation.Result.State != preview.StateReady {
		t.Fatalf("manual artifact: %+v %v", operation, err)
	}
	if worked, err := env.service.WorkOne(t.Context()); err != nil || !worked {
		t.Fatal(err)
	}
	if env.generator.Calls() != 1 || thumbnailJobs(t, env) != 0 {
		t.Fatal("worker regenerated an existing artifact")
	}
}
