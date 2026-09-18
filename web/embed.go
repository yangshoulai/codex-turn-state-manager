// Package web embeds the admin panel's static assets.
//
// These files are served from a Resource route, which bypasses CPA's
// management auth. They must therefore contain no secrets and must not fetch
// anything except the plugin's own Management API. The management key is held
// in memory for the page session only -- never localStorage, never a cookie.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"sort"
	"strings"
)

//go:embed index.html app.js style.css
var assets embed.FS

// FS returns the panel's static file system.
func FS() fs.FS { return assets }

// ReadAsset returns one embedded asset by its route path (leading slash
// optional).
func ReadAsset(name string) ([]byte, error) {
	return assets.ReadFile(strings.TrimPrefix(name, "/"))
}

// The panel's files, and the routes they are served from.
//
// The route names are not the file names for the two assets that get cached:
// each carries a hash of its own contents, so a new build is a URL that no cache
// has ever seen. This is stronger than a version query string, which a cache is
// free to ignore when it keys on the path alone.
//
// Measured against a real deployment: a CDN in front of CPA cached .js and .css
// with its own four-hour TTL, overriding the Cache-Control this package sets,
// while .html passed through untouched. A browser refresh could not get past the
// edge cache -- a hard refresh only bypasses the browser's own -- so for hours
// after an update the previous release's panel kept loading and calling
// management routes that no longer existed.
//
// index.html is deliberately NOT hashed. It is the entry point the host's menu
// links to, so it has to keep one stable address, and it is the one file a CDN
// leaves alone -- which is exactly what makes it the right place to record which
// app and style build to fetch.
const (
	indexPath = "/index.html"
	appFile   = "app.js"
	styleFile = "style.css"
)

// assetRoutes maps a route path to the embedded file it serves. Computed once:
// the contents are embedded, so the hashes cannot change at runtime.
var assetRoutes = func() map[string]string {
	routes := map[string]string{indexPath: indexPath}

	// A short hash is enough: the goal is telling two builds apart, not
	// resisting a collision attack on a file that ships inside this library.
	hashed := func(file string) string {
		raw, err := assets.ReadFile(file)
		if err != nil {
			// Unreachable: the embed directive above lists the file. Panicking
			// at init beats serving a panel whose routes are quietly missing.
			panic("web: embedded asset " + file + " is unreadable: " + err.Error())
		}
		sum := sha256.Sum256(raw)
		ext := file[strings.LastIndex(file, "."):]
		return "/" + strings.TrimSuffix(file, ext) + "." + hex.EncodeToString(sum[:])[:12] + ext
	}

	routes[hashed(appFile)] = appFile
	routes[hashed(styleFile)] = styleFile
	return routes
}()

// Assets lists the routes the panel is served from, in declaration order.
//
// CPA registers resource routes one exact path at a time -- there is no static
// directory and no wildcard -- so this list is also what gets declared to the
// host, and every entry must resolve through ServedAsset.
var Assets = func() []string {
	out := make([]string, 0, len(assetRoutes))
	out = append(out, indexPath)
	for route, file := range assetRoutes {
		if file != indexPath {
			out = append(out, route)
		}
	}
	// Stable order: sort so the declaration does not depend on map iteration.
	sort.Strings(out[1:])
	return out
}()

// Handler serves the panel's assets.
//
// Routing is by exact path, matching how CPA dispatches resource requests, so
// the development harness exercises the same shape as production rather than a
// more permissive one. The bare base path is not a route in production either:
// CPA trims a trailing "/" and rejects the empty result, so the panel's entry
// point is <ResourceBasePath>/index.html. The harness redirects the bare base
// there for convenience, which is the one deliberate difference -- and it does
// so at the mount, in app.Handler, not here.
func Handler() http.Handler {
	mux := http.NewServeMux()
	for _, name := range Assets {
		file := name
		mux.HandleFunc("GET "+name, func(w http.ResponseWriter, r *http.Request) {
			serveAsset(w, r, file)
		})
	}
	return mux
}

func serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	raw, err := ServedAsset(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ContentType(name))
	w.Header().Set("Cache-Control", CacheControl(name))
	_, _ = w.Write(raw)
}

// ServedAsset returns the bytes to send for a route: the embedded file, with the
// panel's asset references rewritten to their hashed routes.
//
// Every path that serves an asset goes through this rather than ReadAsset. The
// first version of this rewriting was wired into the development harness only,
// and the production handler in internal/pluginabi kept calling ReadAsset -- so
// the fix was absent from the released binary while every test passed. One entry
// point is what stops the two from diverging again.
func ServedAsset(route string) ([]byte, error) {
	file, ok := assetRoutes[route]
	if !ok {
		return nil, fs.ErrNotExist
	}
	raw, err := ReadAsset(file)
	if err != nil {
		return nil, err
	}
	if file != indexPath {
		return raw, nil
	}
	out := string(raw)
	for route, source := range assetRoutes {
		if source == indexPath {
			continue
		}
		// Relative, not absolute: index.html is served from
		// /v0/resource/plugins/<id>/, and the original references are relative
		// to it. A leading slash would resolve to the site root and 404.
		out = strings.ReplaceAll(out, `"`+source+`"`, `"`+strings.TrimPrefix(route, "/")+`"`)
	}
	return []byte(out), nil
}

// ImmutableRoute reports whether a route's contents can never change, so it may
// be cached indefinitely. Asset routes carry a content hash, so a change to the
// file changes the route; index.html does not, and must be revalidated.
func ImmutableRoute(route string) bool {
	return route != indexPath && assetRoutes[route] != ""
}

// CacheControl is the Cache-Control value for a route.
//
// The hashed asset routes are immutable by construction, so they are the ones
// that can be cached hard. index.html keeps no-cache because it is what records
// which asset build is current: caching it is how a page ends up loading the
// previous release.
func CacheControl(route string) string {
	if ImmutableRoute(route) {
		return "public, max-age=31536000, immutable"
	}
	return "no-cache"
}

// ContentType maps an asset name to its Content-Type.
func ContentType(name string) string {
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
