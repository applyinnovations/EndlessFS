// Package web exposes the immutable browser assets embedded in the binary.
package web

import (
	"bytes"
	"embed"
	"net/http"
	"strings"
)

//go:embed ui/index.html ui/css/*.css ui/js/*.js ui/brand/endlessfs-mark.svg ui/fonts/*.woff2
var assets embed.FS

const themeLink = `<link id="theme-stylesheet" rel="stylesheet" disabled>`

// The browser keeps one immutable script and stylesheet request while the
// application sources remain organized by domain. Order is explicit because
// the JavaScript domains deliberately share one private application scope.
var applicationScriptSources = []string{
	"ui/js/core.js",
	"ui/js/files.js",
	"ui/js/transfers.js",
	"ui/js/previews.js",
	"ui/js/operations.js",
	"ui/js/account-admin.js",
	"ui/js/bootstrap.js",
}

var applicationStylesheetSources = []string{
	"ui/css/foundation.css",
	"ui/css/shell.css",
	"ui/css/files.css",
	"ui/css/transfers.css",
	"ui/css/settings-admin.css",
	"ui/css/overlays.css",
	"ui/css/responsive.css",
}

var applicationScript = mustJoin(applicationScriptSources...)
var applicationStylesheet = mustJoin(applicationStylesheetSources...)

// Handler serves only the explicitly embedded application assets. An optional
// resolver may select a validated same-origin theme stylesheet before paint.
func Handler(themeCSSResolvers ...func(*http.Request) string) http.Handler {
	index := mustRead("ui/index.html")
	assetManifest := map[string]struct {
		data        []byte
		contentType string
		cache       string
		isolated    bool
	}{
		"/assets/ui.css":                     {data: applicationStylesheet, contentType: "text/css; charset=utf-8", cache: "public, max-age=3600"},
		"/assets/ui.js":                      {data: applicationScript, contentType: "text/javascript; charset=utf-8", cache: "public, max-age=3600"},
		"/assets/brand/endlessfs-mark.svg":   {data: mustRead("ui/brand/endlessfs-mark.svg"), contentType: "image/svg+xml", cache: "public, max-age=31536000, immutable", isolated: true},
		"/assets/fonts/inter-regular.woff2":  {data: mustRead("ui/fonts/inter-regular.woff2"), contentType: "font/woff2", cache: "public, max-age=31536000, immutable"},
		"/assets/fonts/inter-medium.woff2":   {data: mustRead("ui/fonts/inter-medium.woff2"), contentType: "font/woff2", cache: "public, max-age=31536000, immutable"},
		"/assets/fonts/inter-semibold.woff2": {data: mustRead("ui/fonts/inter-semibold.woff2"), contentType: "font/woff2", cache: "public, max-age=31536000, immutable"},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		switch {
		case applicationPath(r.URL.Path):
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			body = index
			if len(themeCSSResolvers) != 0 {
				if href := themeCSSResolvers[0](r); validThemeCSSURL(href) {
					body = bytes.Replace(index, []byte(themeLink), []byte(`<link id="theme-stylesheet" rel="stylesheet" href="`+href+`">`), 1)
				}
			}
		case assetManifest[r.URL.Path].data != nil:
			asset := assetManifest[r.URL.Path]
			w.Header().Set("Cache-Control", asset.cache)
			w.Header().Set("Content-Type", asset.contentType)
			w.Header().Set("X-Content-Type-Options", "nosniff")
			if asset.isolated {
				w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'none'; sandbox")
			}
			body = asset.data
		default:
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	})
}

func validThemeCSSURL(value string) bool {
	return strings.HasPrefix(value, "/assets/themes/") && strings.HasSuffix(value, "/theme.css") && !strings.ContainsAny(value, "\"'<>&?#\\\r\n\x00")
}

func applicationPath(path string) bool {
	switch path {
	case "/", "/bootstrap", "/register", "/recover", "/trash", "/settings", "/admin":
		return true
	}
	return oneSegmentAfter(path, "/register/invite/") || oneSegmentAfter(path, "/recover/")
}

func oneSegmentAfter(path, prefix string) bool {
	value, found := strings.CutPrefix(path, prefix)
	return found && value != "" && !strings.Contains(value, "/")
}

func mustRead(name string) []byte {
	content, err := assets.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return content
}

func mustJoin(names ...string) []byte {
	parts := make([][]byte, 0, len(names))
	for _, name := range names {
		parts = append(parts, mustRead(name))
	}
	return bytes.Join(parts, []byte("\n"))
}
