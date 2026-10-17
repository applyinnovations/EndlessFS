package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
	"go.opentelemetry.io/otel/trace"
)

type problemDiagnosticWriter struct{ records chan map[string]any }

func (writer *problemDiagnosticWriter) Write(body []byte) (int, error) {
	var record map[string]any
	if err := json.Unmarshal(body, &record); err != nil {
		return 0, err
	}
	writer.records <- record
	return len(body), nil
}

func TestIntegrationProblemDiagnosticSurvivesHTTPStatusAndNestedActivities(t *testing.T) {
	const diagnosticTraceID = "f639a73ef118d02f3de71db54f2e9165"
	if _, err := trace.TraceIDFromHex(diagnosticTraceID); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		cause      error
		result     string
		reason     telemetry.Reason
		reasonName string
	}{
		{"pack-capacity", errors.New("sentinel-provider-key"), "invalid", telemetry.PackCapacity, "pack_capacity"},
		{"deadline", context.DeadlineExceeded, "timeout", telemetry.ProviderFailure, "provider_failure"},
		{"canceled", context.Canceled, "canceled", telemetry.ContextCanceled, "canceled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			writer := &problemDiagnosticWriter{records: make(chan map[string]any, 4)}
			observer := telemetry.New(nil, slog.New(slog.NewJSONHandler(writer, nil)))
			defer observer.Close()
			observer.SetVersion("v0.8.0-corrective.12908dd")
			mux := http.NewServeMux()
			mux.HandleFunc("POST /api/v1/uploads/plan/sizes", func(w http.ResponseWriter, r *http.Request) {
				ctx, planning := telemetry.Start(r.Context(), telemetry.UploadSizes, telemetry.Application)
				// writeProblem must find the enclosing HTTP activity through nested
				// planner contexts and retain the original typed error for EndResult.
				failure := telemetry.Diagnostic(domain.WrapError(domain.ErrorInvalid, "sentinel-private-user/path/file.txt", test.cause), telemetry.ProjectionBuild, test.reason)
				writeProblem(w, r.WithContext(ctx), failure)
				planning.End(failure)
			})
			handler := requestIDMiddleware(telemetryMiddleware(mux, mux))
			request := httptest.NewRequest(http.MethodPost, "/api/v1/uploads/plan/sizes?capability=sentinel-secret", strings.NewReader(`{"path":"sentinel-private-file"}`))
			request = request.WithContext(telemetry.Context(context.Background(), observer))
			request.Header.Set("X-Request-ID", "mCiGkyMaenSBt5QY")
			request.Header.Set("Authorization", "Bearer sentinel-private-authorization")
			request.Header.Set("Cookie", "session=sentinel-private-cookie")
			request.Header.Set("traceparent", "00-"+diagnosticTraceID+"-0123456789abcdef-01")
			request.Header.Set("baggage", "user=sentinel-private-user")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || response.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatalf("diagnostics changed public problem semantics: %d %s", response.Code, response.Body)
			}
			var public problem
			if err := json.Unmarshal(response.Body.Bytes(), &public); err != nil {
				t.Fatal(err)
			}
			if public.RequestID != "mCiGkyMaenSBt5QY" || response.Header().Get("X-Request-ID") != public.RequestID || strings.Contains(response.Body.String(), "sentinel") {
				t.Fatal("public problem lost correlation or exposed underlying error data")
			}
			seen := map[string]bool{}
			for range 2 {
				select {
				case record := <-writer.records:
					operation, _ := record["operation"].(string)
					seen[operation] = true
					for key, want := range map[string]any{
						"msg": "operation_completed", "result": test.result, "stage": "projection_build", "reason": test.reasonName, "build": "v0.8.0-corrective.12908dd",
						"requestID": public.RequestID, "traceID": diagnosticTraceID,
					} {
						if record[key] != want {
							t.Errorf("%s log %s=%v, want %v", operation, key, record[key], want)
						}
					}
					if seconds, ok := record["durationSeconds"].(float64); !ok || seconds < 0 {
						t.Fatal("HTTP/planning duration missing")
					}
					encoded, err := json.Marshal(record)
					if err != nil {
						t.Fatal(err)
					}
					if bytes.Contains(encoded, []byte("sentinel")) {
						t.Fatal("request or underlying error private content entered logs")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("HTTP/planning diagnostic lifecycle event missing")
				}
			}
			if !seen["http.request"] || !seen["upload.plan.sizes"] {
				t.Fatal("HTTP/planning diagnostic correlation incomplete")
			}
			var metrics bytes.Buffer
			observer.WritePrometheus(&metrics)
			if !strings.Contains(metrics.String(), `route="POST /api/v1/uploads/plan/sizes",result="`+test.result+`"`) {
				t.Fatal("HTTP metric result lost the underlying context cancellation/deadline")
			}
			for _, forbidden := range []string{"sentinel", public.RequestID, diagnosticTraceID, "requestID=", "traceID="} {
				if strings.Contains(metrics.String(), forbidden) {
					t.Error("request correlation or private content entered metric labels")
				}
			}
		})
	}
}

func TestIntegrationProblemDiagnosticRequestIDValidationSharesGeneratedCorrelation(t *testing.T) {
	writer := &problemDiagnosticWriter{records: make(chan map[string]any, 1)}
	observer := telemetry.New(nil, slog.New(slog.NewJSONHandler(writer, nil)))
	defer observer.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/uploads/plan/sizes", func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, r, telemetry.Diagnostic(domain.ErrInvalid, telemetry.RequestValidation, telemetry.Validation))
	})
	handler := requestIDMiddleware(telemetryMiddleware(mux, mux))
	request := httptest.NewRequest(http.MethodPost, "/api/v1/uploads/plan/sizes", nil)
	request = request.WithContext(telemetry.Context(context.Background(), observer))
	request.Header.Set("X-Request-ID", "sentinel/private?capability=secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	id := response.Header().Get("X-Request-ID")
	if !validRequestID.MatchString(id) || strings.Contains(id, "sentinel") {
		t.Fatal("unsafe client correlation ID was accepted")
	}
	select {
	case record := <-writer.records:
		if record["requestID"] != id || record["stage"] != "request_validation" || record["reason"] != "validation" {
			t.Fatal("HTTP diagnostic log did not share the validated response correlation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP diagnostic lifecycle event missing")
	}
}

func TestIntegrationProblemDiagnosticJSONValidationNamesTheActualRequestStage(t *testing.T) {
	for _, test := range []struct {
		name, mediaType, body string
	}{
		{"content-type", "text/plain", `{}`},
		{"malformed", "application/json", `{"`},
		{"duplicate", "application/json", `{"items":[],"items":[]}`},
		{"oversized", "application/json", strings.Repeat("x", maxControlBodyBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			writer := &problemDiagnosticWriter{records: make(chan map[string]any, 1)}
			observer := telemetry.New(nil, slog.New(slog.NewJSONHandler(writer, nil)))
			defer observer.Close()
			mux := http.NewServeMux()
			mux.HandleFunc("POST /api/v1/uploads/plan/sizes", func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Items []string `json:"items"`
				}
				if decodeJSON(w, r, &body) {
					w.WriteHeader(http.StatusNoContent)
				}
			})
			handler := requestIDMiddleware(telemetryMiddleware(mux, mux))
			request := httptest.NewRequest(http.MethodPost, "/api/v1/uploads/plan/sizes", strings.NewReader(test.body))
			request = request.WithContext(telemetry.Context(context.Background(), observer))
			request.Header.Set("Content-Type", test.mediaType)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("strict JSON request denial changed: %d", response.Code)
			}
			select {
			case record := <-writer.records:
				if record["stage"] != "request_validation" || record["reason"] != "validation" {
					t.Fatalf("decoder failure must be distinguishable from stored metadata corruption: stage=%v reason=%v", record["stage"], record["reason"])
				}
			case <-time.After(5 * time.Second):
				t.Fatal("request validation diagnostic missing")
			}
		})
	}
}
