# Running a throwaway CPA in Docker

The development harness (`make run`) is the fast inner loop, but it runs against
a mock host: it cannot tell you whether the shared library actually loads, or
whether the callbacks arrive the way the domain assumes. This loop answers that
against a real CPA.

It also matters because `c-shared` output cannot be cross-compiled — a `.dylib`
built on macOS will not load into a Linux CPA — so the library is built inside a
Linux container that matches the deployment target.

## Usage

```bash
make cpa-docker-up      # build the .so for linux, start CPA, print the URLs
make cpa-docker-logs    # follow the host log
make cpa-docker-down    # stop and remove the container
```

Then:

* panel: <http://127.0.0.1:18317/v0/resource/plugins/codex-turn-state-manager/index.html>
  (see the caveat below — this currently 404s on v7.3.7)
* the plugin's own API:
  `curl -H 'Authorization: Bearer local-dev-key' http://127.0.0.1:18317/v0/management/plugins/codex-turn-state-manager/status`

The management key and API key are the throwaway values in `config.yaml`.

## What to look for in the log

A healthy load looks like this:

```
pluginhost: plugin loaded plugin_id=codex-turn-state-manager path=plugins/....so
pluginhost: plugin registered plugin_id=codex-turn-state-manager ...
management routes registered
```

Two failure modes worth recognising, both of which are silent from the plugin's
own side:

* `pluginhost: plugin ... returned invalid metadata or no capabilities` — the
  host discards **every** declared capability, so the plugin loads and is then
  never called. CPA requires Name, Version, Author and GitHubRepository to be
  non-empty. Both of those have been the cause at least once.
* A route that answers 404 despite being listed in
  `GET /v0/management/plugins`. Management routes must carry the
  `plugins/<pluginID>` segment themselves, because the host resolves them as
  `<BasePath> + <Path>`. Resource routes are the opposite — the host adds the
  segment for those.

## Adding a Codex account

The plugin lists accounts from CPA's auth pool, so with an empty `auths/`
directory every probe-scoped feature is inert (the pool is empty and nothing is
selected). To exercise probing end to end, drop a Codex auth JSON into
`auths/`; CPA reads that directory at startup and on change.

Without one you can still verify the load, the registration handshake, the
capability declaration, the management routes, and the panel's asset routes.

## Caveat: resource routes

On v7.3.7 the resource routes currently 404, for this plugin and for CPA's own
`examples/plugin/management-api` alike. They register correctly — the host lists
them under `menus` in `GET /v0/management/plugins` — and are then not served.
That is a host-side behaviour, not something the plugin can fix; the plugin's
management API, which is the part that carries the data, works.
