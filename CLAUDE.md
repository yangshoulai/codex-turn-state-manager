# CLAUDE.md

@AGENTS.md

The import above is the single source of truth for this repository: project
overview, hard invariants, layout, naming conventions, build/test commands, and
the list of unverified assumptions. **Read it before making changes.** This file
adds only what is specific to working here as Claude Code.

---

## Start here

1. Read `AGENTS.md`.
2. Skim [`docs/插件开发文档.md`](./docs/插件开发文档.md) — it is the specification. Sections
   3.3 (switch matrix), 3.4 (time windows), 3.7 (proxy traversal), 3.10
   (backoff), and chapter 7 (migrations) are where the subtle rules live.
3. Run `make run` to bring up the dev harness — mock CPA host, real management
   API, real panel, local SQLite. No CPA instance required. This is the fastest
   way to see whether something works.

Do not start a live CPA instance or touch real Codex accounts for development.
The harness exists precisely so you never have to.

---

## Working style for this repo

**Match the existing comment voice.** Comments here cite design-document
sections and explain *why* a constraint exists, because that is the part a
reader cannot recover from the code. Do not add comments that restate the next
line, and do not cite the design document for things that are self-evident.

**Implement the invariant, not just the happy path.** Most rules in `AGENTS.md`
section 2 exist because the underlying behaviour is undocumented and can change
underneath us. A feature that works but ignores the master switch, caches a
credential, or binds a non-target-length value is not partially done — it is a
defect. When a requirement and a shortcut conflict, the requirement wins.

**Boundary cases are the deliverable.** For anything time-, TTL-, or
ordering-related, write the boundary test as part of the change, not after.
Cross-midnight windows and the `FRESH`/`REFRESH_DUE` threshold are the two places
bugs have the longest half-life because they only surface in production at
specific hours.

**Do not add dependencies casually.** The module has one non-stdlib dependency
(the pure-Go SQLite driver) and that is deliberate — see the note in
`internal/storage/db.go` about avoiding a second copy of the sqlite3 C symbols
inside the plugin's shared library. Adding a CGO dependency re-introduces
exactly the problem that choice avoids.

---

## Things that are easy to get wrong here

- **The v1/v2 migration split is intentional.** v1 creates `proxy_node` without
  `last_used_at`; v2 adds it. Folding the column into v1 breaks every existing
  installation with `duplicate column name`. The reasoning is in
  `internal/storage/migrate.go` — read it before "cleaning up" that file.
- **`AuthIndex` and `AuthID` are different things.** `AuthIndex` is the
  persistence key; `AuthID` is CPA's runtime handle. See `AGENTS.md` section 4.
- **The `c-shared` build cannot be cross-compiled** by setting `GOOS`. Build on
  the target platform.
- **`cmd/plugin/cshared.go` deliberately exports almost nothing.** The real CPA
  ABI surface is unverified (`AGENTS.md` section 7). Do not invent registration
  symbols to make it look complete; the adapter belongs in `internal/pluginabi`
  once the SDK surface is confirmed.
- **`make test` is not the gate — `make test-race` is.** Probe workers and the
  management API write concurrently; a clean non-race test run proves little.

---

## When you finish a change

Run, in order:

```bash
make fmt vet test-race && make build
```

Then confirm, against `AGENTS.md` section 9:

- Behaviour change → is `docs/插件开发文档.md` updated in the same change?
- Schema change → new migration appended **and** `CurrentSchemaVersion` bumped?
- New host capability → added to `hostapi.Host` **and** `hostapi.MockHost`?
- New runtime knob → reachable from the panel or the settings table?

If any of those is a no, the change is not finished. Say so rather than
reporting it as complete.
