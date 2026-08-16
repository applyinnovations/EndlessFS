// Package web exposes the immutable browser assets embedded in the binary.
package web

import (
	"embed"
	"net/http"
)

//go:embed static/*
var assets embed.FS

// Handler serves only the explicitly embedded application assets.
func Handler() http.Handler {
	index := mustRead("static/index.html")
	stylesheet := mustRead("static/app.css")
	script := mustRead("static/app.js")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		switch r.URL.Path {
		case "/":
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			body = index
		case "/assets/app.css":
			w.Header().Set("Cache-Control", "public, max-age=3600")
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			body = stylesheet
		case "/assets/app.js":
			w.Header().Set("Cache-Control", "public, max-age=3600")
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			body = script
		default:
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	})
}

func mustRead(name string) []byte {
	content, err := assets.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return content
}
