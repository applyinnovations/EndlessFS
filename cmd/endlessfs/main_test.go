package main

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/config"
	endlesslogging "github.com/applyinnovations/endlessfs/internal/logging"
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

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
