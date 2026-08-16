package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/theme"
)

func TestIntegrationThemeHTTPMetadataPreferenceAssetsAndSafeFallback(t *testing.T) {
	env := newDriveHTTPEnvironment(t)
	origin := "https://drive.example.test"
	cookies := []*http.Cookie{env.session, env.csrf}
	listed := performRequest(t, env.handler, http.MethodGet, "/api/v1/themes", "", "", nil, nil)
	if listed.Code != http.StatusOK || !bytes.Contains(listed.Body.Bytes(), []byte("endlessfs-light")) || !bytes.Contains(listed.Body.Bytes(), []byte("color.accent")) {
		t.Fatalf("themes = %d %s", listed.Code, listed.Body.String())
	}
	selected := performRequest(t, env.handler, http.MethodPut, "/api/v1/me/preferences/theme", origin, `{"themeID":"endlessfs-dark"}`, cookies, driveMutationHeaders(env.csrf.Value, ""))
	if selected.Code != http.StatusOK || !bytes.Contains(selected.Body.Bytes(), []byte(`"id":"endlessfs-dark"`)) {
		t.Fatalf("select theme = %d %s", selected.Code, selected.Body.String())
	}
	device := responseCookie(t, selected, theme.SecureDeviceCookieName)
	if device.HttpOnly || !device.Secure || device.Value != "endlessfs-dark" {
		t.Fatalf("device cookie = %+v", device)
	}
	prepaint := performRequest(t, env.handler, http.MethodGet, "/settings", "", "", []*http.Cookie{device}, nil)
	darkSelection := env.themes.ResolveDevice(device.Value, false, false)
	if prepaint.Code != http.StatusOK || !bytes.Contains(prepaint.Body.Bytes(), []byte(`href="`+darkSelection.CSSURL+`"`)) {
		t.Fatalf("pre-paint theme shell = %d %s", prepaint.Code, prepaint.Body.String())
	}
	preference := performRequest(t, env.handler, http.MethodGet, "/api/v1/me/preferences/theme", "", "", []*http.Cookie{env.session}, nil)
	if preference.Code != http.StatusOK || !bytes.Contains(preference.Body.Bytes(), []byte(`"preference":"endlessfs-dark"`)) {
		t.Fatalf("theme preference = %d %s", preference.Code, preference.Body.String())
	}
	safe := performRequest(t, env.handler, http.MethodGet, "/api/v1/me/preferences/theme?safe-theme=1&dark=true", "", "", []*http.Cookie{env.session}, nil)
	if safe.Code != http.StatusOK || !bytes.Contains(safe.Body.Bytes(), []byte(`"id":"endlessfs-light"`)) || !bytes.Contains(safe.Body.Bytes(), []byte(`"fallback":true`)) {
		t.Fatalf("safe theme = %d %s", safe.Code, safe.Body.String())
	}
	selection, err := env.themes.ResolvePreference(context.Background(), httpUserID(t, 0x51), false, false)
	if err != nil {
		t.Fatal(err)
	}
	css := performRequest(t, env.handler, http.MethodGet, selection.CSSURL, "", "", nil, nil)
	if css.Code != http.StatusOK || css.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" || css.Header().Get("X-Content-Type-Options") != "nosniff" || !bytes.Contains(css.Body.Bytes(), []byte("--efs-color-accent")) {
		t.Fatalf("theme CSS = %d %v %s", css.Code, css.Header(), css.Body.String())
	}
	for _, asset := range selection.Assets {
		response := performRequest(t, env.handler, http.MethodGet, asset.URL, "", "", nil, nil)
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != asset.ContentType || response.Header().Get("Content-Security-Policy") != "default-src 'none'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; sandbox" {
			t.Fatalf("theme asset = %d %v", response.Code, response.Header())
		}
		break
	}
	missing := performRequest(t, env.handler, http.MethodGet, "/assets/themes/not-installed/theme.css", "", "", nil, nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing asset = %d", missing.Code)
	}
}
