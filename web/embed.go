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

// Handler serves the panel.
func Handler() http.Handler {
	return http.FileServer(http.FS(assets))
}
