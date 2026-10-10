package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/config"
	"github.com/applyinnovations/endlessfs/internal/httpapi"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
)

func TestDiagnosticsFailureAndCollectorConfigurationDoNotChangeWriterOrReadiness(t *testing.T) {
	cfg := runtimeTestConfig(t)
	original, err := buildWriterConfiguration(cfg, "keyring")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Telemetry = config.Telemetry{DiagnosticsAddr: "not-a-listener", TraceEndpoint: "http://127.0.0.1:1/v1/traces", SampleRatio: 1}
	current, err := buildWriterConfiguration(cfg, "keyring")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, current) {
		t.Fatal("non-authoritative telemetry changed persisted writer identity")
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	ctx, stop := startTelemetry(context.Background(), logger, cfg.Telemetry)
	defer stop()
	if telemetry.From(ctx) == nil {
		t.Fatal("local metrics unavailable after diagnostics bind failure")
	}
	server, listener, handler, serveErrors, err := startControlServer(ctx, "127.0.0.1:0", time.Second, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	handler.Activate(httpapi.New(cfg.Public(), "test"))
	response, err := http.Get("http://" + listener.Addr().String() + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("telemetry changed readiness")
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serveErrors; err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
