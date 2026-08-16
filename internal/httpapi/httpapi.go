// Package httpapi owns the EndlessFS HTTP transport and security middleware.
package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/applyinnovations/endlessfs/internal/config"
	webassets "github.com/applyinnovations/endlessfs/internal/web"
)

// New constructs the HTTP handler from already-validated public configuration.
func New(cfg config.PublicConfig, version string) http.Handler {
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
	mux.Handle("GET /", webassets.Handler())

	return securityHeaders(mux)
}

func textStatus(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'; img-src 'self'; font-src 'self'; style-src 'self'; script-src 'self'; connect-src 'self'")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}
