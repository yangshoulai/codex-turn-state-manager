// Package version carries build identity for the plugin.
package version

// Version is overridden at build time via -ldflags. See the Makefile.
var Version = "dev"

// PluginName is the identifier CPA uses for this plugin. The shared library
// base name and the CPA `plugins.configs` key must both match this value.
const PluginName = "codex-turn-state-manager"

// Registration metadata. CPA rejects a registration that leaves Name, Version,
// Author or Repository empty -- and it rejects it by ignoring every declared
// capability, so the plugin loads and then never gets called.
const (
	Author     = "codex-turn-state-manager contributors"
	Repository = "https://github.com/yangshoulai/codex-turn-state-manager"
	Logo       = ""
)

// ManagementBasePath is the prefix for every Management API route.
const ManagementBasePath = "/v0/management/plugins/" + PluginName

// ResourceBasePath is the prefix for the embedded admin panel assets. Resource
// routes bypass CPA management auth, so nothing secret may be served here.
const ResourceBasePath = "/v0/resource/plugins/" + PluginName
