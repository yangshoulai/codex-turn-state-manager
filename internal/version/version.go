// Package version carries build identity for the plugin.
package version

// Version is overridden at build time via -ldflags. See the Makefile.
var Version = "dev"

// PluginName is the identifier CPA uses for this plugin. The shared library
// base name and the CPA `plugins.configs` key must both match this value.
const PluginName = "codex-turn-state-manager"

// ManagementBasePath is the prefix for every Management API route.
const ManagementBasePath = "/v0/management/plugins/" + PluginName

// ResourceBasePath is the prefix for the embedded admin panel assets. Resource
// routes bypass CPA management auth, so nothing secret may be served here.
const ResourceBasePath = "/v0/resource/plugins/" + PluginName
