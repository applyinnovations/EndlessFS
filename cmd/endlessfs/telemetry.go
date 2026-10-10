package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/applyinnovations/endlessfs/internal/config"
	"github.com/applyinnovations/endlessfs/internal/diagnostics"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
	"github.com/applyinnovations/endlessfs/internal/telemetry/otlpexport"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// startTelemetry is outside application readiness and storage configuration.
// Collector/listener failures leave the application available with safe warnings.
func startTelemetry(ctx context.Context, logger *slog.Logger, options config.Telemetry) (context.Context, func()) {
	observer := telemetry.New(nil, logger)
	observer.SetVersion(version)
	var provider *sdktrace.TracerProvider
	if options.TraceEndpoint != "" {
		exporter, err := otlpexport.New(ctx, options.TraceEndpoint)
		if err != nil {
			logger.Warn("telemetry_export_unavailable", "result", "unavailable")
		} else {
			provider = telemetry.TraceProvider(exporter, observer, version, options.SampleRatio)
		}
	}
	ctx = telemetry.Context(ctx, observer)
	var server *http.Server
	if options.DiagnosticsAddr != "" {
		listener, err := net.Listen("tcp", options.DiagnosticsAddr)
		if err != nil {
			logger.Warn("diagnostics_unavailable", "result", "unavailable")
		} else {
			server = &http.Server{Handler: diagnostics.Handler(observer, options.DiagnosticsToken.Reveal()), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10}
			go func() {
				if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
					logger.Warn("diagnostics_stopped", "result", "unavailable")
				}
			}()
		}
	}
	return ctx, func() {
		observer.Close()
		// Diagnostic requests and exports cannot extend application shutdown.
		if server != nil {
			_ = server.Close()
		}
		if provider != nil {
			shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = provider.Shutdown(shutdown)
		}
	}
}
