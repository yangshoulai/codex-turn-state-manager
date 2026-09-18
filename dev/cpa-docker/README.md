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

* panel: <http://127.0.0.1:18317/v0/resource/plugins/codex-turn-state-manager/index.html>,
  or through the host's management UI at
  <http://127.0.0.1:18317/management.html#/plugin-pages/codex-turn-state-manager/0>
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
  `GET /v0/management/plugins`. Two separate causes have been hit here:
  * Management route paths must carry the `plugins/<pluginID>` segment
    themselves, because the host resolves them as `<BasePath> + <Path>`.
    Resource routes are the opposite — the host adds the segment for those.
  * `management.handle` carries **both** management and resource requests, so
    the plugin has to branch on the path. Replaying a resource path through the
    management mux 404s while the host reports the dispatch as successful.

## State survives a restart

`plugin-data/` is a mounted volume, so the plugin's database — probe toggles,
bindings, proxy pool, settings — persists across `make cpa-docker-down` /
`cpa-docker-up`. Without that mount the database lives inside the container and
every rebuild silently resets the configuration, which is exactly the kind of
thing that makes a "did persistence work?" question unanswerable.

Delete `dev/cpa-docker/plugin-data/` to start from scratch.

## Adding a Codex account

The plugin lists accounts from CPA's auth pool, so with an empty `auths/`
directory every probe-scoped feature is inert (the pool is empty and nothing is
selected). To exercise probing end to end, drop a Codex auth JSON into
`auths/`; CPA reads that directory at startup and on change.

Without one you can still verify the load, the registration handshake, the
capability declaration, the management routes, and the panel's asset routes.

## Diagnosing a route that 404s

When a plugin route 404s, the host log alone will not tell you which side failed:
the dispatch is reported as successful either way. Build CPA with a debug print
at the dispatch point to find out — that is how the `management.handle` branch
above was found. `/tmp/cpa-src` in this repo's history is a clone of
`router-for-me/CLIProxyAPI` at the version the image runs.
