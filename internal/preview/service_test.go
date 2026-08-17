package preview_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/preview"
	"github.com/applyinnovations/endlessfs/internal/preview/imagegen"
	previewmemory "github.com/applyinnovations/endlessfs/internal/preview/memory"
	providermemory "github.com/applyinnovations/endlessfs/internal/provider/memory"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
)

func TestResolveAutomaticPolicyExclusionsReadNoOriginalBytes(t *testing.T) {
	t.Run("age", func(t *testing.T) {
		env := newPreviewEnvironment(t, preview.Options{Automatic: true, MaxAge: durationPointer(time.Hour)})
		entry := env.uploadImage(t, "/old.png", 16, 8)
		env.clock.Advance(time.Hour + time.Second)
		before := env.source.Instrumentation()
		result, err := env.service.Resolve(context.Background(), env.owner, preview.ResolveRequest{Items: []preview.ItemRequest{{Path: entry.Path, Version: entry.Version, Variant: 256}}})
		if err != nil {
			t.Fatal(err)
		}
		if result.Items[0].State != preview.StateIneligible || result.Items[0].Reason != "age" {
			t.Fatalf("age result = %+v", result.Items[0])
		}
		assertNoSourceRead(t, before, env.source.Instrumentation())
		operation, err := env.service.Generate(context.Background(), env.owner, preview.GenerateRequest{Path: entry.Path, Version: entry.Version, Variant: 256, IdempotencyKey: "preview-explicit-age-0001"})
		if err != nil || operation.State != domain.OperationSucceeded || operation.Result == nil || operation.Result.State != preview.StateReady {
			t.Fatalf("explicit generation = %+v, %v", operation, err)
		}
	})

	t.Run("size", func(t *testing.T) {
		maximum := int64(32)
		env := newPreviewEnvironment(t, preview.Options{Automatic: true, MaxSourceBytes: &maximum})
		entry := env.uploadImage(t, "/large.png", 16, 8)
		before := env.source.Instrumentation()
		result, err := env.service.Resolve(context.Background(), env.owner, preview.ResolveRequest{Items: []preview.ItemRequest{{Path: entry.Path, Version: entry.Version, Variant: 256}}})
		if err != nil {
			t.Fatal(err)
		}
		if result.Items[0].State != preview.StateIneligible || result.Items[0].Reason != "size" {
			t.Fatalf("size result = %+v source size=%d", result.Items[0], entry.Size)
		}
		assertNoSourceRead(t, before, env.source.Instrumentation())
	})
}

func TestResolveAutomaticPolicyIncludesExactAgeAndSizeBoundaries(t *testing.T) {
	maximum := int64(1)
	maximumAge := time.Hour
	env := newPreviewEnvironment(t, preview.Options{Automatic: true, MaxAge: &maximumAge, MaxSourceBytes: &maximum})
	entry := env.uploadImage(t, "/boundary.png", 16, 8)
	maximum = entry.Size
	env.clock.Advance(maximumAge)
	before := env.source.Instrumentation()
	result, err := env.service.Resolve(context.Background(), env.owner, preview.ResolveRequest{Items: []preview.ItemRequest{{Path: entry.Path, Version: entry.Version, Variant: 256}}})
	if err != nil || result.Items[0].State != preview.StateReady {
		t.Fatalf("exact policy boundary Resolve() = %+v, %v", result, err)
	}
	after := env.source.Instrumentation()
	if after.DownloadBytes <= before.DownloadBytes {
		t.Fatalf("eligible exact boundary did not read source: before=%+v after=%+v", before, after)
	}
}

func TestResolveGeneratesOnceAndRenameReusesArtifact(t *testing.T) {
	env := newPreviewEnvironment(t, preview.Options{Automatic: true})
	entry := env.uploadImage(t, "/photo.png", 32, 16)
	first, err := env.service.Resolve(context.Background(), env.owner, preview.ResolveRequest{Items: []preview.ItemRequest{{Path: entry.Path, Version: entry.Version, Variant: 256}}})
	if err != nil || first.Items[0].State != preview.StateReady || first.Items[0].Artifact.Width != 32 || first.Items[0].Artifact.Height != 16 {
		t.Fatalf("first Resolve() = %+v, %v", first, err)
	}
	if env.generator.Calls() != 1 {
		t.Fatalf("generator calls = %d, want 1", env.generator.Calls())
	}
	if _, err := env.source.Move(context.Background(), env.scope, env.scope, domain.MoveRequest{
		Source: entry.Path, Destination: domain.MustParseUserPath("/renamed.png"), IdempotencyKey: "preview-rename-request-0001",
	}); err != nil {
		t.Fatal(err)
	}
	renamed, err := env.source.Stat(context.Background(), env.scope, domain.MustParseUserPath("/renamed.png"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := env.service.Resolve(context.Background(), env.owner, preview.ResolveRequest{Items: []preview.ItemRequest{{Path: renamed.Path, Version: renamed.Version, Variant: 256}}})
	if err != nil || second.Items[0].State != preview.StateReady {
		t.Fatalf("renamed Resolve() = %+v, %v", second, err)
	}
	if env.generator.Calls() != 1 {
		t.Fatalf("rename regenerated preview; calls = %d", env.generator.Calls())
	}
	if first.Items[0].Artifact.GenerationID != second.Items[0].Artifact.GenerationID {
		t.Fatalf("rename generation changed: %q != %q", first.Items[0].Artifact.GenerationID, second.Items[0].Artifact.GenerationID)
	}
}

func TestResolveCoalescesConcurrentGeneration(t *testing.T) {
	env := newPreviewEnvironment(t, preview.Options{Automatic: true})
	entry := env.uploadImage(t, "/concurrent.png", 64, 32)
	const callers = 8
	results := make(chan preview.ResolveResponse, callers)
	errorsFound := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := env.service.Resolve(context.Background(), env.owner, preview.ResolveRequest{Items: []preview.ItemRequest{{Path: entry.Path, Version: entry.Version, Variant: 256}}})
			results <- result
			errorsFound <- err
		}()
	}
	group.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	for result := range results {
		if len(result.Items) != 1 || result.Items[0].State != preview.StateReady {
			t.Fatalf("concurrent Resolve() = %+v", result)
		}
	}
	if env.generator.Calls() != 1 {
		t.Fatalf("concurrent generator calls = %d, want 1", env.generator.Calls())
	}
}

func TestGenerateIdempotencyIsSharedAcrossReplicas(t *testing.T) {
	env := newPreviewEnvironmentWithoutService(t)
	entry := env.uploadImage(t, "/replica-idempotency.png", 32, 16)
	generator := newLeaseTestGenerator()
	options := preview.Options{
		Automatic: true, Resolutions: []int{256}, MaxConcurrency: 1, OperationTimeout: 5 * time.Second,
		ApplicationState: env.applicationState,
	}
	firstService, err := preview.NewService(options, env.source, env.store, []preview.Generator{generator}, env.sourceServer.Client(), env.ids, env.clock)
	if err != nil {
		t.Fatal(err)
	}
	secondService, err := preview.NewService(options, env.source, env.store, []preview.Generator{generator}, env.sourceServer.Client(), env.ids, env.clock)
	if err != nil {
		t.Fatal(err)
	}
	request := preview.GenerateRequest{Path: entry.Path, Version: entry.Version, Variant: 256, IdempotencyKey: "preview-replica-idempotency-0001"}
	firstResult := make(chan preview.Operation, 1)
	firstError := make(chan error, 1)
	go func() {
		operation, generateErr := firstService.Generate(context.Background(), env.owner, request)
		firstResult <- operation
		firstError <- generateErr
	}()
	<-generator.firstStarted
	replayed, err := secondService.Generate(context.Background(), env.owner, request)
	if err != nil || replayed.State != domain.OperationRunning {
		t.Fatalf("replica replay = %+v, %v", replayed, err)
	}
	close(generator.releaseFirst)
	first := <-firstResult
	if err := <-firstError; err != nil || first.State != domain.OperationSucceeded || replayed.ID != first.ID {
		t.Fatalf("replica operations = first %+v replay %+v error %v", first, replayed, err)
	}
	stored, err := secondService.Operation(context.Background(), env.owner, first.ID)
	if err != nil || stored.State != domain.OperationSucceeded || stored.Result == nil || stored.Result.Capability == nil {
		t.Fatalf("durable replica operation = %+v, %v", stored, err)
	}
	durable, err := env.applicationState.Get(context.Background(), state.MustKey(state.NamespaceOperations, "preview", env.owner.String(), string(first.ID)))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(durable.Data, []byte(`"capability"`)) || bytes.Contains(durable.Data, []byte("/cap/preview/")) {
		t.Fatalf("durable operation persisted a bearer capability: %s", durable.Data)
	}
	if generator.Calls() != 1 {
		t.Fatalf("replica generator calls = %d, want 1", generator.Calls())
	}
}

func TestGenerateExpiredReplicaLeaseCanBeTakenOverAndFencesOldNode(t *testing.T) {
	env := newPreviewEnvironmentWithoutService(t)
	entry := env.uploadImage(t, "/replica-takeover.png", 32, 16)
	generator := newLeaseTestGenerator()
	options := preview.Options{
		Automatic: true, Resolutions: []int{256}, MaxConcurrency: 1, OperationTimeout: 5 * time.Second,
		ApplicationState: env.applicationState,
	}
	firstService, err := preview.NewService(options, env.source, env.store, []preview.Generator{generator}, env.sourceServer.Client(), env.ids, env.clock)
	if err != nil {
		t.Fatal(err)
	}
	secondService, err := preview.NewService(options, env.source, env.store, []preview.Generator{generator}, env.sourceServer.Client(), env.ids, env.clock)
	if err != nil {
		t.Fatal(err)
	}
	request := preview.GenerateRequest{Path: entry.Path, Version: entry.Version, Variant: 256, IdempotencyKey: "preview-replica-takeover-0001"}
	firstResult := make(chan preview.Operation, 1)
	firstError := make(chan error, 1)
	go func() {
		operation, generateErr := firstService.Generate(context.Background(), env.owner, request)
		firstResult <- operation
		firstError <- generateErr
	}()
	<-generator.firstStarted
	env.clock.Advance(options.OperationTimeout + time.Second)
	takenOver, err := secondService.Generate(context.Background(), env.owner, request)
	if err != nil || takenOver.State != domain.OperationSucceeded {
		t.Fatalf("takeover operation = %+v, %v", takenOver, err)
	}
	close(generator.releaseFirst)
	stale := <-firstResult
	if err := <-firstError; err != nil || stale.ID != takenOver.ID || stale.State != domain.OperationSucceeded {
		t.Fatalf("fenced stale replica = %+v, %v; takeover %+v", stale, err, takenOver)
	}
	if generator.Calls() != 2 {
		t.Fatalf("takeover generator calls = %d, want 2 attempts", generator.Calls())
	}
}

func TestResolveCopyAndReplacementRequireDistinctArtifacts(t *testing.T) {
	env := newPreviewEnvironment(t, preview.Options{Automatic: true})
	original := env.uploadImage(t, "/identity.png", 12, 6)
	first, err := env.service.Resolve(context.Background(), env.owner, preview.ResolveRequest{Items: []preview.ItemRequest{{Path: original.Path, Version: original.Version, Variant: 256}}})
	if err != nil || first.Items[0].State != preview.StateReady {
		t.Fatalf("original Resolve() = %+v, %v", first, err)
	}
	copyPath := domain.MustParseUserPath("/identity-copy.png")
	if operation, err := env.source.Copy(context.Background(), env.scope, env.scope, domain.CopyRequest{Source: original.Path, Destination: copyPath, ExpectedSource: original.Version, IdempotencyKey: "preview-copy-request-0001"}); err != nil || operation.State != domain.OperationSucceeded {
		t.Fatalf("Copy() = %+v, %v", operation, err)
	}
	copied, err := env.source.Stat(context.Background(), env.scope, copyPath)
	if err != nil {
		t.Fatal(err)
	}
	copyResult, err := env.service.Resolve(context.Background(), env.owner, preview.ResolveRequest{Items: []preview.ItemRequest{{Path: copied.Path, Version: copied.Version, Variant: 256}}})
	if err != nil || copyResult.Items[0].State != preview.StateReady || copyResult.Items[0].Artifact.GenerationID == first.Items[0].Artifact.GenerationID {
		t.Fatalf("copied Resolve() = %+v, %v", copyResult, err)
	}
	replacementData := encodePreviewPNG(t, 6, 12)
	replacement := env.uploadBytesWithConflict(t, original.Path.String(), replacementData, "image/png", domain.ConflictReplace, original.Version)
	replacementResult, err := env.service.Resolve(context.Background(), env.owner, preview.ResolveRequest{Items: []preview.ItemRequest{{Path: replacement.Path, Version: replacement.Version, Variant: 256}}})
	if err != nil || replacementResult.Items[0].State != preview.StateReady || replacementResult.Items[0].Artifact.GenerationID == first.Items[0].Artifact.GenerationID {
		t.Fatalf("replacement Resolve() = %+v, %v", replacementResult, err)
	}
	if env.generator.Calls() != 3 {
		t.Fatalf("copy/replacement generator calls = %d, want 3", env.generator.Calls())
	}
}

func TestResolveDisabledManualUnsupportedAndStalePaths(t *testing.T) {
	t.Run("disabled has zero provider calls", func(t *testing.T) {
		env := newPreviewEnvironmentWithoutStore(t)
		before := env.source.Instrumentation()
		result, err := env.service.Resolve(context.Background(), env.owner, preview.ResolveRequest{Items: []preview.ItemRequest{{Path: domain.MustParseUserPath("/anything.png"), Version: "v1", Variant: 256}}})
		if err != nil || result.Items[0].State != preview.StateDisabled {
			t.Fatalf("disabled Resolve() = %+v, %v", result, err)
		}
		after := env.source.Instrumentation()
		if len(after.ProviderCalls) != len(before.ProviderCalls) {
			t.Fatalf("disabled preview reached source provider: before=%+v after=%+v", before, after)
		}
	})

	t.Run("manual-only resolve does not read", func(t *testing.T) {
		env := newPreviewEnvironment(t, preview.Options{Automatic: false})
		entry := env.uploadImage(t, "/manual.png", 8, 8)
		before := env.source.Instrumentation()
		result, err := env.service.Resolve(context.Background(), env.owner, preview.ResolveRequest{Items: []preview.ItemRequest{{Path: entry.Path, Version: entry.Version, Variant: 256}}})
		if err != nil || result.Items[0].State != preview.StateMissing {
			t.Fatalf("manual Resolve() = %+v, %v", result, err)
		}
		assertNoSourceRead(t, before, env.source.Instrumentation())
	})

	t.Run("stale exact version is denied", func(t *testing.T) {
		env := newPreviewEnvironment(t, preview.Options{Automatic: true})
		entry := env.uploadImage(t, "/stale.png", 8, 8)
		_, err := env.service.Resolve(context.Background(), env.owner, preview.ResolveRequest{Items: []preview.ItemRequest{{Path: entry.Path, Version: "stale", Variant: 256}}})
		if err == nil || !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("stale Resolve() error = %v", err)
		}
	})
}

func TestPreviewStoreOperatesIndependentlyOfMediaBrowser(t *testing.T) {
	env := newPreviewEnvironmentWithoutService(t)
	service, err := preview.NewService(preview.Options{
		Automatic: false, Resolutions: []int{256}, MaxConcurrency: 1, OperationTimeout: time.Second, ApplicationState: env.applicationState,
	}, env.source, env.store, []preview.Generator{env.generator}, env.sourceServer.Client(), env.ids, env.clock)
	if err != nil {
		t.Fatal(err)
	}
	entry := env.uploadImage(t, "/api-only.png", 16, 8)
	operation, err := service.Generate(context.Background(), env.owner, preview.GenerateRequest{
		Path: entry.Path, Version: entry.Version, Variant: 256, IdempotencyKey: "preview-api-only-0001",
	})
	if err != nil || operation.State != domain.OperationSucceeded || operation.Result == nil || operation.Result.State != preview.StateReady {
		t.Fatalf("API-only Generate() = %+v, %v", operation, err)
	}
	resolved, err := service.Resolve(context.Background(), env.owner, preview.ResolveRequest{Items: []preview.ItemRequest{{Path: entry.Path, Version: entry.Version, Variant: 256}}})
	if err != nil || len(resolved.Items) != 1 || resolved.Items[0].State != preview.StateReady {
		t.Fatalf("API-only Resolve() = %+v, %v", resolved, err)
	}
}

func TestNewServiceFailsFastForGeneratorAndStoreMisconfiguration(t *testing.T) {
	env := newPreviewEnvironmentWithoutService(t)
	failing := &fakeGenerator{selfTestError: domain.NewError(domain.ErrorUnavailable, "private decoder path")}
	_, err := preview.NewService(preview.Options{Automatic: true, Resolutions: []int{256}, OperationTimeout: time.Second, MaxConcurrency: 1, ApplicationState: env.applicationState}, env.source, env.store, []preview.Generator{failing}, env.sourceServer.Client(), env.ids, env.clock)
	if err == nil || bytes.Contains([]byte(err.Error()), []byte("private decoder path")) || !bytes.Contains([]byte(err.Error()), []byte("generator integrity")) {
		t.Fatalf("generator startup error = %v", err)
	}
	env.store.SetAvailable(false)
	_, err = preview.NewService(preview.Options{Automatic: true, Resolutions: []int{256}, OperationTimeout: time.Second, MaxConcurrency: 1, ApplicationState: env.applicationState}, env.source, env.store, []preview.Generator{&fakeGenerator{}}, env.sourceServer.Client(), env.ids, env.clock)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("preview store access")) {
		t.Fatalf("store startup error = %v", err)
	}
}

func TestPreviewServiceValidationIdempotencyAndUnsupportedOperation(t *testing.T) {
	env := newPreviewEnvironment(t, preview.Options{Automatic: true})
	if !env.service.Ready() || env.service.DataOrigin() != env.storeServer.URL {
		t.Fatalf("service origins/readiness = %q %v", env.service.DataOrigin(), env.service.Ready())
	}
	for _, request := range []preview.ResolveRequest{
		{},
		{Items: make([]preview.ItemRequest, preview.MaxResolveItems+1)},
		{Items: []preview.ItemRequest{{Path: domain.MustParseUserPath("/"), Version: "v1", Variant: 256}}},
		{Items: []preview.ItemRequest{{Path: domain.MustParseUserPath("/file.png"), Version: "v1", Variant: 999}}},
	} {
		if _, err := env.service.Resolve(context.Background(), env.owner, request); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid Resolve(%+v) error = %v", request, err)
		}
	}
	entry := env.uploadImage(t, "/idempotent.png", 4, 2)
	if _, err := env.service.Generate(context.Background(), env.owner, preview.GenerateRequest{Path: entry.Path, Version: entry.Version, Variant: 256, IdempotencyKey: "short"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("short idempotency error = %v", err)
	}
	request := preview.GenerateRequest{Path: entry.Path, Version: entry.Version, Variant: 256, IdempotencyKey: "preview-idempotency-0001"}
	first, err := env.service.Generate(context.Background(), env.owner, request)
	if err != nil || first.State != domain.OperationSucceeded {
		t.Fatalf("first generation = %+v, %v", first, err)
	}
	replayed, err := env.service.Generate(context.Background(), env.owner, request)
	if err != nil || replayed.ID != first.ID {
		t.Fatalf("idempotent replay = %+v, %v", replayed, err)
	}
	request.Regenerate = true
	if _, err := env.service.Generate(context.Background(), env.owner, request); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("idempotency fingerprint error = %v", err)
	}
	if operation, err := env.service.Operation(context.Background(), env.owner, first.ID); err != nil || operation.ID != first.ID {
		t.Fatalf("Operation() = %+v, %v", operation, err)
	}
	if _, err := env.service.Operation(context.Background(), env.owner, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid Operation error = %v", err)
	}
	if _, err := env.service.Operation(context.Background(), env.owner, "missing-operation"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing Operation error = %v", err)
	}
	unsupported := env.uploadBytes(t, "/archive.bin", []byte("archive"), "application/octet-stream")
	operation, err := env.service.Generate(context.Background(), env.owner, preview.GenerateRequest{Path: unsupported.Path, Version: unsupported.Version, Variant: 256, IdempotencyKey: "preview-unsupported-0001"})
	if err != nil || operation.State != domain.OperationFailed || operation.ErrorKind != domain.ErrorInvalid || operation.Result == nil || operation.Result.State != preview.StateUnsupported {
		t.Fatalf("unsupported operation = %+v, %v", operation, err)
	}
	disabled := newPreviewEnvironmentWithoutStore(t)
	if disabled.service.DataOrigin() != "" || !disabled.service.Ready() {
		t.Fatalf("disabled service origin/readiness = %q %v", disabled.service.DataOrigin(), disabled.service.Ready())
	}
}

func TestPreviewServiceRejectsInvalidAssemblyOptions(t *testing.T) {
	env := newPreviewEnvironmentWithoutService(t)
	zeroDuration := time.Duration(0)
	zeroBytes := int64(0)
	valid := preview.Options{Automatic: true, Resolutions: []int{256}, MaxConcurrency: 1, OperationTimeout: time.Second, StartupTimeout: time.Second}
	if _, err := preview.NewService(valid, nil, env.store, []preview.Generator{env.generator}, env.sourceServer.Client(), env.ids, env.clock); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("nil source error = %v", err)
	}
	tests := []preview.Options{
		{Resolutions: []int{256}, MaxConcurrency: 9, OperationTimeout: time.Second, StartupTimeout: time.Second},
		{Resolutions: []int{63}, MaxConcurrency: 1, OperationTimeout: time.Second, StartupTimeout: time.Second},
		{Resolutions: []int{256, 256}, MaxConcurrency: 1, OperationTimeout: time.Second, StartupTimeout: time.Second},
		{Resolutions: []int{64, 128, 256, 512, 1024}, MaxConcurrency: 1, OperationTimeout: time.Second, StartupTimeout: time.Second},
		{Resolutions: []int{256}, MaxConcurrency: 1, OperationTimeout: 6 * time.Minute, StartupTimeout: time.Second},
		{Resolutions: []int{256}, MaxConcurrency: 1, OperationTimeout: time.Second, StartupTimeout: 61 * time.Second},
		{Resolutions: []int{256}, MaxConcurrency: 1, OperationTimeout: time.Second, StartupTimeout: time.Second, MaxAge: &zeroDuration},
		{Resolutions: []int{256}, MaxConcurrency: 1, OperationTimeout: time.Second, StartupTimeout: time.Second, MaxSourceBytes: &zeroBytes},
	}
	for _, options := range tests {
		if _, err := preview.NewService(options, env.source, env.store, []preview.Generator{env.generator}, env.sourceServer.Client(), env.ids, env.clock); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid options %+v error = %v", options, err)
		}
	}
}

type previewEnvironment struct {
	service          *preview.Service
	source           *providermemory.Provider
	store            *previewmemory.Store
	sourceServer     *httptest.Server
	storeServer      *httptest.Server
	generator        *fakeGenerator
	clock            *domain.FixedClock
	ids              *domain.IDGenerator
	applicationState state.Store
	owner            domain.UserID
	scope            domain.Scope
}

func newPreviewEnvironment(t *testing.T, options preview.Options) previewEnvironment {
	t.Helper()
	env := newPreviewEnvironmentWithoutService(t)
	if len(options.Resolutions) == 0 {
		options.Resolutions = []int{256, 512, 1600}
	}
	if options.OperationTimeout == 0 {
		options.OperationTimeout = 5 * time.Second
	}
	if options.MaxConcurrency == 0 {
		options.MaxConcurrency = 2
	}
	options.ApplicationState = env.applicationState
	var err error
	env.service, err = preview.NewService(options, env.source, env.store, []preview.Generator{env.generator}, env.sourceServer.Client(), env.ids, env.clock)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func newPreviewEnvironmentWithoutStore(t *testing.T) previewEnvironment {
	t.Helper()
	env := newPreviewEnvironmentWithoutService(t)
	var err error
	env.service, err = preview.NewService(preview.Options{}, env.source, nil, []preview.Generator{env.generator}, env.sourceServer.Client(), env.ids, env.clock)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func newPreviewEnvironmentWithoutService(t *testing.T) previewEnvironment {
	t.Helper()
	clock := domain.NewFixedClock(time.Date(2036, 2, 3, 4, 5, 6, 0, time.UTC))
	ids := domain.NewIDGenerator(bytes.NewReader(previewDeterministicBytes(4 << 20)))
	source := providermemory.New(providermemory.Options{Clock: clock, IDs: ids})
	sourceServer := httptest.NewServer(source)
	t.Cleanup(sourceServer.Close)
	if err := source.SetDataPlaneBaseURL(sourceServer.URL); err != nil {
		t.Fatal(err)
	}
	store, err := previewmemory.New(previewmemory.Options{Clock: clock, IDs: ids, Key: secret.Value(previewBearer(0x77))})
	if err != nil {
		t.Fatal(err)
	}
	storeServer := httptest.NewServer(store)
	t.Cleanup(storeServer.Close)
	if err := store.SetDataPlaneBaseURL(storeServer.URL); err != nil {
		t.Fatal(err)
	}
	owner, err := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x55}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	scope, err := domain.NewScope(owner, domain.AreaLive)
	if err != nil {
		t.Fatal(err)
	}
	return previewEnvironment{source: source, store: store, sourceServer: sourceServer, storeServer: storeServer, generator: &fakeGenerator{}, clock: clock, ids: ids, applicationState: state.NewMemoryStore(), owner: owner, scope: scope}
}

func (e previewEnvironment) uploadImage(t *testing.T, pathValue string, width, height int) domain.Entry {
	t.Helper()
	return e.uploadBytes(t, pathValue, encodePreviewPNG(t, width, height), "image/png")
}

func encodePreviewPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	imageValue := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			imageValue.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 7), G: uint8(y * 11), B: 90, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, imageValue); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func (e previewEnvironment) uploadBytes(t *testing.T, pathValue string, data []byte, mediaType string) domain.Entry {
	return e.uploadBytesWithConflict(t, pathValue, data, mediaType, domain.ConflictFail, "")
}

func (e previewEnvironment) uploadBytesWithConflict(t *testing.T, pathValue string, data []byte, mediaType string, conflict domain.ConflictMode, expected domain.Version) domain.Entry {
	t.Helper()
	path := domain.MustParseUserPath(pathValue)
	upload, err := e.source.CreateUpload(context.Background(), e.scope, domain.CreateUploadRequest{Path: path, Size: int64(len(data)), MediaType: mediaType, Conflict: conflict, ExpectedVersion: expected})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(upload.Method, upload.URL, bytes.NewReader(data))
	for name, value := range upload.Headers {
		request.Header.Set(name, value)
	}
	response, err := e.sourceServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("upload status = %d", response.StatusCode)
	}
	sum := sha256.Sum256(data)
	entry, err := e.source.CompleteUpload(context.Background(), e.scope, domain.CompleteUploadRequest{UploadID: upload.UploadID, Path: path, Size: int64(len(data)), MediaType: mediaType, ChecksumSHA256: hex.EncodeToString(sum[:])})
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

type fakeGenerator struct {
	mu            sync.Mutex
	calls         int
	selfTestError error
}

type leaseTestGenerator struct {
	mu           sync.Mutex
	calls        int
	firstStarted chan struct{}
	releaseFirst chan struct{}
}

func newLeaseTestGenerator() *leaseTestGenerator {
	return &leaseTestGenerator{firstStarted: make(chan struct{}), releaseFirst: make(chan struct{})}
}

func (*leaseTestGenerator) Capability() string             { return "image" }
func (*leaseTestGenerator) RecipeID() string               { return "image-webp-q80-v1" }
func (*leaseTestGenerator) Supports(string) bool           { return true }
func (*leaseTestGenerator) SelfTest(context.Context) error { return nil }
func (g *leaseTestGenerator) Generate(ctx context.Context, request preview.GenerationRequest) (preview.GeneratedArtifact, error) {
	g.mu.Lock()
	g.calls++
	call := g.calls
	if call == 1 {
		close(g.firstStarted)
	}
	g.mu.Unlock()
	if call == 1 {
		select {
		case <-g.releaseFirst:
		case <-ctx.Done():
			return preview.GeneratedArtifact{}, ctx.Err()
		}
	}
	return imagegen.New(imagegen.Options{}).Generate(ctx, request)
}
func (g *leaseTestGenerator) Calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func (g *fakeGenerator) Capability() string { return "image" }
func (g *fakeGenerator) RecipeID() string   { return "image-webp-q80-v1" }
func (g *fakeGenerator) Supports(mediaType string) bool {
	return mediaType == "image/png" || mediaType == "image/jpeg" || mediaType == "image/gif" || mediaType == "image/webp"
}
func (g *fakeGenerator) SelfTest(context.Context) error { return g.selfTestError }
func (g *fakeGenerator) Generate(_ context.Context, request preview.GenerationRequest) (preview.GeneratedArtifact, error) {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	return imagegen.New(imagegen.Options{}).Generate(context.Background(), request)
}
func (g *fakeGenerator) Calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func assertNoSourceRead(t *testing.T, before, after providermemory.Instrumentation) {
	t.Helper()
	if after.ProviderCalls[providermemory.OperationCreateDownload] != before.ProviderCalls[providermemory.OperationCreateDownload] ||
		after.ProviderCalls[providermemory.OperationDownloadData] != before.ProviderCalls[providermemory.OperationDownloadData] || after.DownloadBytes != before.DownloadBytes {
		t.Fatalf("excluded preview read source: before=%+v after=%+v", before, after)
	}
}

func durationPointer(value time.Duration) *time.Duration { return &value }
func previewBearer(fill byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}
func previewDeterministicBytes(size int) []byte {
	value := make([]byte, size)
	for index := range value {
		value[index] = byte(index*17 + 3)
	}
	return value
}
