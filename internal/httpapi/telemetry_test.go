package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/config"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
)

func TestIntegrationHTTPMetricsUseTemplatesAndKeepDiagnosticsOffPublicSurface(t *testing.T) {
	observer := telemetry.New(nil, nil)
	handler := New(config.PublicConfig{}, "test")
	for _, path := range []string{"/api/v1/config?token=sentinel-secret", "/private/sentinel-user-file", "/debug/pprof/heap", "/metrics"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request = request.WithContext(telemetry.Context(context.Background(), observer))
		request.Header.Set("Authorization", "Bearer sentinel-secret")
		request.Header.Set("Cookie", "session=sentinel-cookie")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if (path == "/debug/pprof/heap" || path == "/metrics") && response.Code != 404 {
			t.Fatalf("public diagnostics %s status %d", path, response.Code)
		}
	}
	var output bytes.Buffer
	observer.WritePrometheus(&output)
	if strings.Contains(output.String(), "sentinel") {
		t.Fatal("request secrets or targets entered metrics")
	}
	if !strings.Contains(output.String(), `route="GET /api/v1/config",result="success"`) {
		t.Fatal("successful HTTP template not recorded")
	}
}

func TestHTTPPanicIsObservedWithoutChangingPanicBehavior(t *testing.T) {
	observer := telemetry.New(nil, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(http.ResponseWriter, *http.Request) { panic("sentinel-secret-panic") })
	handler := telemetryMiddleware(mux, mux)
	request := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(telemetry.Context(context.Background(), observer))
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic was swallowed")
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}()
	var output bytes.Buffer
	observer.WritePrometheus(&output)
	if !strings.Contains(output.String(), `result="panic"`) || strings.Contains(output.String(), "sentinel") {
		t.Fatal("panic class missing or unsafe")
	}
}
