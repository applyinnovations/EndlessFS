package main

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"strings"
	"testing"

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
