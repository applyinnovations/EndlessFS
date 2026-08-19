// Package httpapi owns the EndlessFS HTTP transport and security middleware.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/config"
	"github.com/applyinnovations/endlessfs/internal/drive"
	"github.com/applyinnovations/endlessfs/internal/identity"
	"github.com/applyinnovations/endlessfs/internal/preview"
	"github.com/applyinnovations/endlessfs/internal/theme"
	webassets "github.com/applyinnovations/endlessfs/internal/web"
)

// New constructs the HTTP handler from already-validated public configuration.
func New(cfg config.PublicConfig, version string) http.Handler {
	return newHandler(cfg, version, false, "", "", nil, nil, nil)
}

// NewApplication constructs the complete control-plane handler.
func NewApplication(cfg config.Config, version string, identityService *identity.Service, sessions *auth.SessionManager) http.Handler {
	api := &identityAPI{config: cfg, identity: identityService, sessions: sessions}
	return newHandler(cfg.Public(), version, cfg.Secure, "", "", api, nil, nil)
}

// NewCompleteApplication includes the file/data-capability control plane.
func NewCompleteApplication(cfg config.Config, version string, identityService *identity.Service, sessions *auth.SessionManager, driveService *drive.Service, themeManagers ...*theme.Manager) http.Handler {
	api := &identityAPI{config: cfg, identity: identityService, sessions: sessions, drive: driveService}
	if len(themeManagers) != 0 {
		api.themes = themeManagers[0]
	}
	return newHandler(cfg.Public(), version, cfg.Secure, driveService.DataOrigin(), "", api, nil, nil)
}

// NewCompleteApplicationWithPreview includes the optional generated-preview
// control plane and dynamic readiness dependency.
func NewCompleteApplicationWithPreview(cfg config.Config, version string, identityService *identity.Service, sessions *auth.SessionManager, driveService *drive.Service, previewService *preview.Service, themeManagers ...*theme.Manager) http.Handler {
	api := &identityAPI{config: cfg, identity: identityService, sessions: sessions, drive: driveService, previews: previewService}
	if len(themeManagers) != 0 {
		api.themes = themeManagers[0]
	}
	return newHandler(cfg.Public(), version, cfg.Secure, driveService.DataOrigin(), previewService.DataOrigin(), api, nil, previewService.Revalidate)
}

// NewCompleteApplicationWithLogger constructs the complete control plane with
// safe structured request-completion events.
func NewCompleteApplicationWithLogger(cfg config.Config, version string, identityService *identity.Service, sessions *auth.SessionManager, driveService *drive.Service, logger *slog.Logger, themeManagers ...*theme.Manager) http.Handler {
	api := &identityAPI{config: cfg, identity: identityService, sessions: sessions, drive: driveService, logger: logger}
	if len(themeManagers) != 0 {
		api.themes = themeManagers[0]
	}
	return newHandler(cfg.Public(), version, cfg.Secure, driveService.DataOrigin(), "", api, logger, nil)
}

// NewCompleteApplicationWithPreviewAndLogger constructs the complete control
// plane with generated previews and safe request logging.
func NewCompleteApplicationWithPreviewAndLogger(cfg config.Config, version string, identityService *identity.Service, sessions *auth.SessionManager, driveService *drive.Service, previewService *preview.Service, logger *slog.Logger, themeManagers ...*theme.Manager) http.Handler {
	api := &identityAPI{config: cfg, identity: identityService, sessions: sessions, drive: driveService, previews: previewService, logger: logger}
	if len(themeManagers) != 0 {
		api.themes = themeManagers[0]
	}
	return newHandler(cfg.Public(), version, cfg.Secure, driveService.DataOrigin(), previewService.DataOrigin(), api, logger, previewService.Revalidate)
}

func newHandler(cfg config.PublicConfig, version string, secure bool, dataOrigin, previewOrigin string, api *identityAPI, logger *slog.Logger, ready func(context.Context) bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", textStatus(http.StatusOK, "ok\n"))
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, request *http.Request) {
		if ready != nil && !ready(request.Context()) {
			textStatus(http.StatusServiceUnavailable, "not ready\n")(w, nil)
			return
		}
		textStatus(http.StatusOK, "ready\n")(w, nil)
	})
	mux.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Product             string                     `json:"product"`
			Version             string                     `json:"version"`
			PreviewCapabilities preview.CapabilityManifest `json:"previewCapabilities"`
			config.PublicConfig
		}{
			Product:             "EndlessFS",
			Version:             version,
			PreviewCapabilities: preview.BuildCapabilityManifest(version),
			PublicConfig:        cfg,
		})
	})
	if api != nil {
		api.routes(mux)
	}
	application := webassets.Handler()
	if api != nil && api.themes != nil {
		application = webassets.Handler(func(r *http.Request) string {
			value := ""
			if cookie, err := r.Cookie(api.themes.DeviceCookieName()); err == nil {
				value = cookie.Value
			}
			return api.themes.ResolveDevice(value, false, false).CSSURL
		})
	}
	mux.Handle("GET /", application)

	handler := securityHeaders(mux, secure, dataOrigin, previewOrigin)
	if logger != nil {
		handler = requestLogMiddleware(handler, logger)
	}
	return requestIDMiddleware(handler)
}

func textStatus(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func securityHeaders(next http.Handler, secure bool, dataOrigin, previewOrigin string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		imageSources := []string{"'self'", "blob:"}
		frameSources := []string{"'self'"}
		connectSources := []string{"'self'"}
		if dataOrigin != "" {
			imageSources = append(imageSources, dataOrigin)
			frameSources = append(frameSources, dataOrigin)
			connectSources = append(connectSources, dataOrigin)
		}
		if previewOrigin != "" && previewOrigin != dataOrigin {
			imageSources = append(imageSources, previewOrigin)
			connectSources = append(connectSources, previewOrigin)
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'; img-src "+strings.Join(imageSources, " ")+"; frame-src "+strings.Join(frameSources, " ")+"; font-src 'self'; style-src 'self'; style-src-attr 'none'; script-src 'self'; connect-src "+strings.Join(connectSources, " "))
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if secure {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (recorder *statusRecorder) WriteHeader(status int) {
	if recorder.status != 0 {
		return
	}
	recorder.status = status
	recorder.ResponseWriter.WriteHeader(status)
}

func (recorder *statusRecorder) Write(body []byte) (int, error) {
	if recorder.status == 0 {
		recorder.status = http.StatusOK
	}
	return recorder.ResponseWriter.Write(body)
}

func (recorder *statusRecorder) Unwrap() http.ResponseWriter {
	return recorder.ResponseWriter
}

func requestLogMiddleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		logger.InfoContext(r.Context(), "request_completed",
			"requestID", requestID(r),
			"route", route,
			"status", status,
			"result", resultClass(status),
			"duration", time.Since(started),
		)
	})
}

func resultClass(status int) string {
	switch {
	case status >= 500:
		return "server_error"
	case status >= 400:
		return "client_error"
	default:
		return "success"
	}
}
