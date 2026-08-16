// Package httpapi owns the EndlessFS HTTP transport and security middleware.
package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/config"
	"github.com/applyinnovations/endlessfs/internal/drive"
	"github.com/applyinnovations/endlessfs/internal/identity"
	"github.com/applyinnovations/endlessfs/internal/theme"
	webassets "github.com/applyinnovations/endlessfs/internal/web"
)

// New constructs the HTTP handler from already-validated public configuration.
func New(cfg config.PublicConfig, version string) http.Handler {
	return newHandler(cfg, version, false, "", nil)
}

// NewApplication constructs the complete control-plane handler.
func NewApplication(cfg config.Config, version string, identityService *identity.Service, sessions *auth.SessionManager) http.Handler {
	api := &identityAPI{config: cfg, identity: identityService, sessions: sessions}
	return newHandler(cfg.Public(), version, cfg.Secure, "", api)
}

// NewCompleteApplication includes the file/data-capability control plane.
func NewCompleteApplication(cfg config.Config, version string, identityService *identity.Service, sessions *auth.SessionManager, driveService *drive.Service, themeManagers ...*theme.Manager) http.Handler {
	api := &identityAPI{config: cfg, identity: identityService, sessions: sessions, drive: driveService}
	if len(themeManagers) != 0 {
		api.themes = themeManagers[0]
	}
	return newHandler(cfg.Public(), version, cfg.Secure, driveService.DataOrigin(), api)
}

func newHandler(cfg config.PublicConfig, version string, secure bool, dataOrigin string, api *identityAPI) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", textStatus(http.StatusOK, "ok\n"))
	mux.HandleFunc("GET /readyz", textStatus(http.StatusOK, "ready\n"))
	mux.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Product string `json:"product"`
			Version string `json:"version"`
			config.PublicConfig
		}{
			Product:      "EndlessFS",
			Version:      version,
			PublicConfig: cfg,
		})
	})
	if api != nil {
		api.routes(mux)
	}
	mux.Handle("GET /", webassets.Handler())

	return requestIDMiddleware(securityHeaders(mux, secure, dataOrigin))
}

func textStatus(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func securityHeaders(next http.Handler, secure bool, dataOrigin string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connectSource := "'self'"
		if dataOrigin != "" {
			connectSource += " " + dataOrigin
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'; img-src 'self' "+dataOrigin+"; font-src 'self'; style-src 'self'; script-src 'self'; connect-src "+connectSource)
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
