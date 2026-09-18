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

// MenuLabel is what CPA's menu bar shows for this plugin. The plugin id stays
// as-is -- it must match the library name and the plugins.configs key -- so the
// readable name lives here instead.
const MenuLabel = "Codex Turn State"

// ManagementBasePath is the prefix for every Management API route.
//
// Flat -- "/v0/management/<id>", not "/v0/management/plugins/<id>". The host
// hands plugins BasePath "/v0/management" and resolves routes as
// <BasePath> + <Path>, so the plugin segment is ours to choose; nothing
// requires it to be "plugins/". Two reasons it must not be:
//
//   - CLIProxyAPI's own plugin administration API already lives at
//     /v0/management/plugins(/<id>/config, /<id>/quota, ...). Registering a
//     plugin's own routes inside that subtree mixes two owners into one
//     namespace.
//   - Third-party management front ends classify management paths by their
//     first segment. CPA-Manager-Plus treats "plugins" as reserved and demands
//     its own admin credential for anything under it, so a plugin registered
//     there is unreachable from that panel unless the operator logs in first.
//     Its own tests use the flat shape for plugins ("/v0/management/codex-invite
//     /accounts"), which is what the rest of the ecosystem does.
//
// Resource routes are the opposite: the host inserts "/plugins/<id>" itself.
const ManagementBasePath = "/v0/management/" + PluginName

// ResourceBasePath is the prefix for the embedded admin panel assets. Resource
// routes bypass CPA management auth, so nothing secret may be served here.
const ResourceBasePath = "/v0/resource/plugins/" + PluginName
