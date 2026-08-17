package main

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/config"
	endlesslogging "github.com/applyinnovations/endlessfs/internal/logging"
)

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
	cfg.GCSBucket = "x"
	if err := run(context.Background(), logger, cfg); err == nil {
		t.Fatal("invalid GCS bucket was accepted")
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
