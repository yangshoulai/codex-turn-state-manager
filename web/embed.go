// Package web embeds the admin panel's static assets.
//
// These files are served from a Resource route, which bypasses CPA's
// management auth. They must therefore contain no secrets and must not fetch
// anything except the plugin's own Management API. The management key is held
// in memory for the page session only -- never localStorage, never a cookie.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed index.html app.js style.css
var assets embed.FS

// FS returns the panel's static file system.
func FS() fs.FS { return assets }

// Assets lists the files the panel is made of.
//
// CPA registers resource routes one exact path at a time -- there is no static
// directory and no wildcard -- so this list is also what gets declared to the
// host, and every entry must be a file that exists above.
var Assets = []string{"/index.html", "/app.js", "/style.css"}

// Handler serves the panel's assets.
//
// Routing is by exact path, matching how CPA dispatches resource requests, so
// the development harness exercises the same shape as production rather than a
// more permissive one. The bare base path is not a route in production either:
// CPA trims a trailing "/" and rejects the empty result, so the panel's entry
// point is <ResourceBasePath>/index.html. The harness redirects the bare base
// there for convenience, which is the one deliberate difference.
func Handler() http.Handler {
	mux := http.NewServeMux()
	for _, name := range Assets {
		file := name
		mux.HandleFunc("GET "+name, func(w http.ResponseWriter, r *http.Request) {
			serveAsset(w, r, file)
		})
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/index.html", http.StatusFound)
	})
	return mux
}

func serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	raw, err := assets.ReadFile(name[1:])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType(name))
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(raw)
}

func contentType(name string) string {
	switch {
	case len(name) > 5 && name[len(name)-5:] == ".html":
		return "text/html; charset=utf-8"
	case len(name) > 3 && name[len(name)-3:] == ".js":
		return "text/javascript; charset=utf-8"
	case len(name) > 4 && name[len(name)-4:] == ".css":
		return "text/css; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}
