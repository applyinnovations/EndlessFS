package httpapi

import (
	"github.com/applyinnovations/endlessfs/internal/telemetry"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"net/http"
)

func telemetryMiddleware(next http.Handler, mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract only standard trace context. Cookies, baggage, raw targets, headers,
		// identities and capabilities never become attributes or metric dimensions.
		ctx := propagation.TraceContext{}.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		// Correlation IDs may cross the boundary; vendor state/baggage may not.
		remote := trace.SpanContextFromContext(ctx)
		if remote.IsValid() {
			ctx = trace.ContextWithRemoteSpanContext(ctx, remote.WithTraceState(trace.TraceState{}))
		}
		ctx, activity := telemetry.Start(ctx, telemetry.HTTP, telemetry.Application)
		_, pattern := mux.Handler(r)
		activity.Route(pattern)
		recorder := &statusRecorder{ResponseWriter: w}
		completed := false
		defer func() {
			if !completed {
				activity.EndResult(telemetry.Panicked)
			}
		}()
		next.ServeHTTP(recorder, r.WithContext(ctx))
		result := telemetry.Success
		switch {
		case recorder.status == http.StatusUnauthorized || recorder.status == http.StatusForbidden:
			result = telemetry.Denied
		case recorder.status == http.StatusNotFound:
			result = telemetry.NotFound
		case recorder.status == http.StatusConflict || recorder.status == http.StatusPreconditionFailed:
			result = telemetry.Conflict
		case recorder.status == http.StatusTooManyRequests || recorder.status == http.StatusServiceUnavailable:
			result = telemetry.Unavailable
		case recorder.status >= 500:
			result = telemetry.Internal
		case recorder.status >= 400:
			result = telemetry.Invalid
		}
		activity.EndResult(result)
		completed = true
	})
}
