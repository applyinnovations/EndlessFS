package httpapi

import (
	"net/http"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func (api *identityAPI) themeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/themes", api.listThemes)
	mux.HandleFunc("GET /api/v1/me/preferences/theme", api.themePreference)
	mux.HandleFunc("PUT /api/v1/me/preferences/theme", api.setThemePreference)
	mux.HandleFunc("GET /assets/themes/{digest}/{asset}", api.themeAsset)
}

func (api *identityAPI) listThemes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"themeAPI": map[string]int{"major": 1, "minor": 0}, "themes": api.themes.Metadata(), "tokens": api.themes.TokenRegistry(), "mediaSlots": api.themes.MediaRegistry()})
}

func parseThemeResolutionQuery(r *http.Request) (bool, bool, error) {
	dark := false
	switch r.URL.Query().Get("dark") {
	case "", "false":
	case "true":
		dark = true
	default:
		return false, false, domain.NewError(domain.ErrorInvalid, "dark must be true or false")
	}
	safe := false
	switch r.URL.Query().Get("safe-theme") {
	case "":
	case "1":
		safe = true
	default:
		return false, false, domain.NewError(domain.ErrorInvalid, "safe-theme must be 1 when supplied")
	}
	return dark, safe, nil
}

func (api *identityAPI) themePreference(w http.ResponseWriter, r *http.Request) {
	current, ok := api.authenticated(w, r)
	if !ok {
		return
	}
	dark, safe, err := parseThemeResolutionQuery(r)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	selection, err := api.themes.ResolvePreference(r.Context(), current.Record.UserID, dark, safe)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, selection)
}

func (api *identityAPI) setThemePreference(w http.ResponseWriter, r *http.Request) {
	current, ok := api.mutation(w, r)
	if !ok {
		return
	}
	var request struct {
		ThemeID string `json:"themeID"`
		Dark    bool   `json:"dark,omitempty"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	selection, err := api.themes.SetPreference(r.Context(), current.Record.UserID, request.ThemeID)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	if request.ThemeID == "system" && request.Dark {
		selection = api.themes.Resolve("system", true, false)
	}
	http.SetCookie(w, api.themes.DeviceCookie(selection))
	writeJSON(w, http.StatusOK, selection)
}

func (api *identityAPI) themeAsset(w http.ResponseWriter, r *http.Request) {
	asset, found := api.themes.Asset(r.PathValue("digest"), r.PathValue("asset"))
	if !found {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Type", asset.ContentType)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	if strings.HasPrefix(asset.ContentType, "font/") {
		w.Header().Set("Access-Control-Allow-Origin", api.config.AllowedOrigin)
	}
	_, _ = w.Write(asset.Data)
}
