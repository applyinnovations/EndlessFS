package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/config"
	"github.com/applyinnovations/endlessfs/internal/domain"
	endlesslogging "github.com/applyinnovations/endlessfs/internal/logging"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/preview/imagegen"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestMain(m *testing.M) {
	if imagegen.IsWorkerInvocation() {
		if err := imagegen.RunWorker(os.Stdin, os.Stdout); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runtimeTestConfig(t *testing.T) config.Config {
	t.Helper()
	values := map[string]string{
		"ENDLESSFS_LISTEN_ADDR":     "127.0.0.1:0",
		"ENDLESSFS_SESSION_SECRET":  base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("s", 32))),
		"ENDLESSFS_BOOTSTRAP_TOKEN": base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("b", 32))),
	}
	cfg, err := config.Parse(func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestRunStartsAndGracefullyStopsCompleteApplication(t *testing.T) {
	cfg := runtimeTestConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	logger := endlesslogging.NewJSON(io.Discard, slog.LevelDebug)
	if err := run(ctx, logger, cfg); err != nil {
		t.Fatalf("run = %v", err)
	}
}

func TestStartupControlServerReportsLivenessWithoutClaimingReadiness(t *testing.T) {
	logger := endlesslogging.NewJSON(io.Discard, slog.LevelDebug)
	server, listener, handler, serveErrors, err := startControlServer("127.0.0.1:0", 30*time.Second, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
	})
	baseURL := "http://" + listener.Addr().String()
	assertStatus := func(path string, want int) {
		t.Helper()
		response, requestErr := http.Get(baseURL + path)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		_ = response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("GET %s status = %d; want %d", path, response.StatusCode, want)
		}
	}
	for range 30 {
		assertStatus("/healthz", http.StatusOK)
		assertStatus("/readyz", http.StatusServiceUnavailable)
	}
	assertStatus("/", http.StatusServiceUnavailable)

	ready := http.NewServeMux()
	ready.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	ready.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handler.Activate(ready)
	assertStatus("/readyz", http.StatusOK)
	select {
	case serveErr := <-serveErrors:
		t.Fatalf("control server stopped during handler activation: %v", serveErr)
	default:
	}
}

func TestControlWriteTimeoutContainsPreviewOperationDeadline(t *testing.T) {
	if got := controlWriteTimeout(false, 5*time.Minute); got != 30*time.Second {
		t.Fatalf("disabled preview write timeout = %s", got)
	}
	if got := controlWriteTimeout(true, 20*time.Second); got != 30*time.Second {
		t.Fatalf("short preview write timeout = %s", got)
	}
	if got := controlWriteTimeout(true, 45*time.Second); got != 50*time.Second {
		t.Fatalf("preview write timeout = %s", got)
	}
}

func TestRunRejectsMalformedConfiguredMockProviderURL(t *testing.T) {
	cfg := runtimeTestConfig(t)
	cfg.MockProviderURL = "%"
	if err := run(context.Background(), endlesslogging.NewJSON(io.Discard, slog.LevelInfo), cfg); err == nil {
		t.Fatal("malformed mock provider URL was accepted")
	}
}

func TestRunBuildsCompleteMockRuntimeBeforeGracefulShutdown(t *testing.T) {
	cfg := runtimeTestConfig(t)
	logger := endlesslogging.NewJSON(io.Discard, slog.LevelDebug)
	if err := run(newAlreadyDoneContext(), logger, cfg); err != nil {
		t.Fatalf("run = %v", err)
	}
}

func TestRunRejectsInvalidRuntimeOnlyConfiguration(t *testing.T) {
	logger := endlesslogging.NewJSON(io.Discard, slog.LevelInfo)
	cfg := runtimeTestConfig(t)
	cfg.SessionSecret = "not-base64"
	if err := run(context.Background(), logger, cfg); err == nil {
		t.Fatal("invalid session key was accepted")
	}
	cfg = runtimeTestConfig(t)
	cfg.StorageProvider = "unknown"
	if err := run(context.Background(), logger, cfg); err == nil {
		t.Fatal("unknown provider was accepted")
	}
	cfg = runtimeTestConfig(t)
	cfg.StorageProvider = "gcs"
	cfg.GCSFileBucket = "x"
	if err := run(context.Background(), logger, cfg); err == nil {
		t.Fatal("invalid GCS file bucket was accepted")
	}
	cfg = runtimeTestConfig(t)
	cfg.MockProviderURL = "http://127.0.0.1:invalid"
	if err := run(context.Background(), logger, cfg); err == nil {
		t.Fatal("invalid listen address was accepted")
	}
}

type alreadyDoneContext struct {
	done      chan struct{}
	once      sync.Once
	cancelled atomic.Bool
}

func newAlreadyDoneContext() *alreadyDoneContext {
	return &alreadyDoneContext{done: make(chan struct{})}
}

func (*alreadyDoneContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (ctx *alreadyDoneContext) Done() <-chan struct{} {
	ctx.once.Do(func() {
		ctx.cancelled.Store(true)
		close(ctx.done)
	})
	return ctx.done
}
func (ctx *alreadyDoneContext) Err() error {
	if ctx.cancelled.Load() {
		return context.Canceled
	}
	return nil
}
func (*alreadyDoneContext) Value(any) any { return nil }

func TestRunValidatesConfiguredPreviewDependenciesBeforeServing(t *testing.T) {
	cfg := runtimeTestConfig(t)
	cfg.PreviewProvider = "mock"
	cfg.PreviewAutomatic = true
	cfg.PreviewFormats = []string{"image"}
	cfg.PreviewResolutions = []int{256, 512, 1600}
	cfg.PreviewMaxConcurrency = 2
	cfg.PreviewOperationTimeout = 45 * time.Second
	cfg.PreviewStartupTimeout = 10 * time.Second
	cfg.PreviewKeySecret = secret.Value("invalid")
	if err := run(context.Background(), endlesslogging.NewJSON(io.Discard, slog.LevelInfo), cfg); err == nil {
		t.Fatal("configured preview store accepted an invalid key")
	}

	cfg.PreviewKeySecret = ""
	if err := run(newAlreadyDoneContext(), endlesslogging.NewJSON(io.Discard, slog.LevelInfo), cfg); err != nil {
		t.Fatalf("configured mock preview startup = %v", err)
	}
}

func TestWriterCompatibilityIncludesDurablePreviewConfiguration(t *testing.T) {
	base := runtimeTestConfig(t)
	base.PreviewProvider = "gcs"
	base.GCSPreviewBucket = "preview-bucket-one"
	base.PreviewFormats = []string{"image"}
	base.PreviewResolutions = []int{256, 512, 1600}
	base.PreviewKeySecret = secret.Value(base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("p", 32))))
	initial, err := buildWriterConfiguration(base, "session-keyring-v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(initial.KeyringIdentifiers) != 2 || !contains(initial.RequiredFeatures, "generated-previews-v1") || !contains(initial.RequiredFeatures, "preview-integrity-crc32c-v1") || !contains(initial.RequiredFeatures, storageformat.FeatureRecursiveBytes) {
		t.Fatalf("preview compatibility markers = %+v", initial)
	}
	variations := []config.Config{
		func() config.Config { value := base; value.PreviewProvider = "disabled"; return value }(),
		func() config.Config { value := base; value.GCSPreviewBucket = "preview-bucket-two"; return value }(),
		func() config.Config { value := base; value.PreviewResolutions = []int{256}; return value }(),
		func() config.Config {
			value := base
			value.PreviewKeySecret = secret.Value(base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("q", 32))))
			return value
		}(),
	}
	for index, variation := range variations {
		configuration, buildErr := buildWriterConfiguration(variation, "session-keyring-v1")
		if buildErr != nil {
			t.Fatalf("variation %d: %v", index, buildErr)
		}
		if configuration.ConfigurationDigest == initial.ConfigurationDigest && strings.Join(configuration.KeyringIdentifiers, "\x00") == strings.Join(initial.KeyringIdentifiers, "\x00") && strings.Join(configuration.RequiredFeatures, "\x00") == strings.Join(initial.RequiredFeatures, "\x00") {
			t.Fatalf("variation %d did not change replica compatibility", index)
		}
	}
}

type applicationMigrationFixture struct {
	SchemaVersion  int               `json:"schemaVersion"`
	SourceRelease  string            `json:"sourceRelease"`
	SourceCommit   string            `json:"sourceCommit"`
	CreatedAt      time.Time         `json:"createdAt"`
	UserID         string            `json:"userID"`
	StateObjects   map[string][]byte `json:"stateObjects"`
	FileObjects    map[string][]byte `json:"fileObjects"`
	SemanticOracle json.RawMessage   `json:"semanticOracle,omitempty"`
}

func TestApplicationWriterProfilesMigrateV014FixturesBeforeStartup(t *testing.T) {
	profiles := []struct {
		name      string
		fixture   string
		configure func(*config.Config)
	}{
		{name: "preview-disabled", fixture: "pre-aggregate-v0.1.4-application-disabled.json", configure: func(*config.Config) {}},
		{
			name: "preview-gcs", fixture: "pre-aggregate-v0.1.4-application-gcs.json",
			configure: func(cfg *config.Config) {
				cfg.PreviewProvider = "gcs"
				cfg.GCSPreviewBucket = "migration-preview-bucket"
				cfg.PreviewFormats = []string{"image"}
				cfg.PreviewResolutions = []int{256, 512, 1600}
				cfg.PreviewKeySecret = secret.Value(base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("p", 32))))
			},
		},
	}
	for _, profile := range profiles {
		t.Run(profile.name, func(t *testing.T) {
			testApplicationWriterProfileMigration(t, profile.fixture, "v0.1.4", "edb67f8e345694001b9614604c5baded9bde5d86", 22, profile.configure)
		})
	}
}

func TestApplicationWriterProfilesOpenSchema004Fixtures(t *testing.T) {
	profiles := []struct {
		name      string
		fixture   string
		configure func(*config.Config)
	}{
		{name: "preview-disabled", fixture: "schema-004-application-disabled.json", configure: func(*config.Config) {}},
		{name: "preview-gcs", fixture: "schema-004-application-gcs.json", configure: configureSchema005PreviewProfile},
	}
	for _, profile := range profiles {
		t.Run(profile.name, func(t *testing.T) {
			testApplicationWriterProfileMigration(t, profile.fixture, "schema-004", "f11fe68b2d731e8fd0228352a0b85255d7574abf", 18, profile.configure)
		})
	}
}

func TestApplicationWriterProfilesOpenSchema005Fixtures(t *testing.T) {
	profiles := []struct {
		name      string
		fixture   string
		configure func(*config.Config)
	}{
		{name: "preview-disabled", fixture: "schema-005-v0.2.0-application-disabled.json", configure: func(*config.Config) {}},
		{name: "preview-gcs", fixture: "schema-005-v0.2.0-application-gcs.json", configure: configureSchema005PreviewProfile},
	}
	for _, profile := range profiles {
		t.Run(profile.name, func(t *testing.T) {
			testApplicationWriterProfileMigration(t, profile.fixture, "v0.2.0", "97e70a84b12de0533b8a7cf4add62ecbf575a0fd", 18, profile.configure)
		})
	}
}

func TestApplicationWriterProfilesOpenSchema006Fixtures(t *testing.T) {
	profiles := []struct {
		name      string
		fixture   string
		configure func(*config.Config)
	}{
		{name: "preview-disabled", fixture: "schema-006-v0.3.0-application-disabled.json", configure: func(*config.Config) {}},
		{name: "preview-gcs", fixture: "schema-006-v0.3.0-application-gcs.json", configure: configureSchema005PreviewProfile},
	}
	for _, profile := range profiles {
		t.Run(profile.name, func(t *testing.T) {
			testApplicationWriterProfileMigration(t, profile.fixture, "v0.3.0", "2d2d49ec9f86e2a247781fd461bcc537459cfbf1", 18, profile.configure)
		})
	}
}

func TestApplicationWriterProfilesOpenSchema007Fixtures(t *testing.T) {
	profiles := []struct {
		name      string
		fixture   string
		configure func(*config.Config)
	}{
		{name: "preview-disabled", fixture: "schema-007-application-disabled.json", configure: func(*config.Config) {}},
		{name: "preview-gcs", fixture: "schema-007-application-gcs.json", configure: configureSchema005PreviewProfile},
	}
	for _, profile := range profiles {
		t.Run(profile.name, func(t *testing.T) {
			testApplicationWriterProfileMigration(t, profile.fixture, "schema-007", "43171275e93717b1261eeff3b98ecd11b08c9e3f", 18, profile.configure)
		})
	}
}

func TestApplicationWriterProfilesOpenSchema008Fixtures(t *testing.T) {
	profiles := []struct {
		name      string
		fixture   string
		configure func(*config.Config)
	}{
		{name: "preview-disabled", fixture: "schema-008-application-disabled.json", configure: func(*config.Config) {}},
		{name: "preview-gcs", fixture: "schema-008-application-gcs.json", configure: configureSchema005PreviewProfile},
	}
	for _, profile := range profiles {
		t.Run(profile.name, func(t *testing.T) {
			testApplicationWriterProfileMigration(t, profile.fixture, "schema-008", "359ec9fbc9e8020257659c0d91e64372baece1b9", 18, profile.configure)
		})
	}
}

func TestApplicationWriterProfilesOpenSchema009Fixtures(t *testing.T) {
	profiles := []struct {
		name      string
		fixture   string
		configure func(*config.Config)
	}{
		{name: "preview-disabled", fixture: "schema-009-application-disabled.json", configure: func(*config.Config) {}},
		{name: "preview-gcs", fixture: "schema-009-application-gcs.json", configure: configureSchema005PreviewProfile},
	}
	for _, profile := range profiles {
		t.Run(profile.name, func(t *testing.T) {
			testApplicationWriterProfileMigration(t, profile.fixture, "schema-009", "86ad9d8da0e6c45f98d85006f440937557e758dd", 18, profile.configure)
		})
	}
}

func TestApplicationWriterProfilesOpenSchema010Fixtures(t *testing.T) {
	profiles := []struct {
		name      string
		fixture   string
		configure func(*config.Config)
	}{
		{name: "preview-disabled", fixture: "schema-010-application-disabled.json", configure: func(*config.Config) {}},
		{name: "preview-gcs", fixture: "schema-010-application-gcs.json", configure: configureSchema005PreviewProfile},
	}
	for _, profile := range profiles {
		t.Run(profile.name, func(t *testing.T) {
			testApplicationWriterProfileMigration(t, profile.fixture, "schema-010", "c9f0561564bd7cc0f4e260d17585c628b245654c", 18, profile.configure)
		})
	}
}

func testApplicationWriterProfileMigration(t *testing.T, fixtureName, sourceRelease, sourceCommit string, wantSize int64, configure func(*config.Config)) {
	t.Helper()
	body, err := os.ReadFile("../../internal/portable/testdata/migrations/" + fixtureName)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var fixture applicationMigrationFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("fixture trailing JSON error = %v; want EOF", err)
	}
	if fixture.SourceRelease != sourceRelease || fixture.SourceCommit != sourceCommit {
		t.Fatalf("unexpected production-profile fixture provenance: %s %s", fixture.SourceRelease, fixture.SourceCommit)
	}
	stateBackend := objectmemory.New()
	fileBackend := objectmemory.New()
	if err := stateBackend.Import(fixture.StateObjects); err != nil {
		t.Fatal(err)
	}
	if err := fileBackend.Import(fixture.FileObjects); err != nil {
		t.Fatal(err)
	}
	cfg := runtimeTestConfig(t)
	configure(&cfg)
	writer, err := buildWriterConfiguration(cfg, "session-keyring-v1")
	if err != nil {
		t.Fatal(err)
	}
	engine, err := portable.Open(context.Background(), portable.Options{
		Backend: stateBackend, FileBackend: fileBackend,
		Clock: domain.NewFixedClock(fixture.CreatedAt.Add(time.Hour)),
		IDs:   domain.NewIDGenerator(bytes.NewReader(applicationMigrationBytes(0x61, 1<<20))), Writer: writer,
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x63}, 32),
	})
	if err != nil {
		t.Fatalf("application startup migration from deployed v0.1.4 profile: %v", err)
	}
	user, err := domain.ParseUserID(fixture.UserID)
	if err != nil {
		t.Fatal(err)
	}
	live, _ := domain.NewScope(user, domain.AreaLive)
	root, err := engine.Files().Stat(context.Background(), live, domain.MustParseUserPath("/"))
	wantFiles := int64(2)
	if sourceRelease == "schema-004" || sourceRelease == "v0.2.0" || sourceRelease == "v0.3.0" || sourceRelease == "schema-007" || sourceRelease == "schema-008" || sourceRelease == "schema-009" || sourceRelease == "schema-010" {
		wantFiles = 3
	}
	if err != nil || root.Size != wantSize || root.FileCount != wantFiles {
		t.Fatalf("migrated application root = %+v, %v; want %d bytes/%d files", root, err, wantSize, wantFiles)
	}
	if _, err := engine.Files().CreateDirectory(context.Background(), live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/after-upgrade")}); err != nil {
		t.Fatalf("post-migration application mutation: %v", err)
	}
}

func applicationMigrationBytes(seed byte, size int) []byte {
	body := make([]byte, size)
	value := uint32(seed) + 1
	for index := range body {
		value = value*1664525 + 1013904223
		body[index] = byte(value >> 24)
	}
	return body
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
