# vikunja-ntfy — Agent Notes

## Purpose

A Yaegi source plugin for [Vikunja](https://github.com/go-vikunja/vikunja) that forwards task
reminders and other notifications directly to one or more
[ntfy](https://github.com/binwiederhier/ntfy) topics — without forking or patching Vikunja, and
without a bridge service in between. Sibling project to
[vikunja-apprise](../vikunja-apprise), which does the same thing but through Apprise API for
100+ services. This plugin trades that breadth for simplicity: one less moving part, at the cost
of only reaching ntfy.

## Repository layout

Self-contained, same shape as `vikunja-apprise`:

- `main.go` — the plugin (`//go:build ignore`, package `main`). The only Go source file; all
  `.go` files directly in this directory are evaluated together by Vikunja's Yaegi loader as
  package `main`.
- `docker-compose.yml` — a local ntfy instance for development/testing.
- `README.md` — user-facing installation/usage/security docs.

## Architecture

Same rationale as `vikunja-apprise/AGENTS.md` — read that first if you haven't: Yaegi plugin
chosen over a webhook-consuming microservice or a core patch, for the same reasons (no extra
hosting needed on the Vikunja side, reuses Vikunja's own auth, full event coverage via
`notification.created`). This file only covers what's specific to ntfy.

### Why this plugin exists alongside vikunja-apprise, not instead of it

Apprise API gives 100+ delivery channels through one bridge service, and — because it owns a
persistent per-key config store — keeps all notification-service secrets (bot tokens, webhook
URLs, SMTP credentials) out of Vikunja's own database entirely. This plugin is for the simpler
case: you only care about ntfy, and would rather not run Apprise API at all. The cost is that ntfy
has no equivalent config-store service to offload state to, so **this plugin keeps a small local
table of each user's topics (and, for protected topics, their access token) in Vikunja's own
database** — a real trade-off, not a simplification for free. See "Security model" below and the
README's "Security" section.

### Event coverage — identical reasoning to vikunja-apprise

Four listeners, not one, for the same reasons as the sibling plugin:

- `task.reminder.fired` — dispatched unconditionally (gated only by the instance-wide
  `webhooks.enabled` setting), independent of the user's email-reminder preference. The reliable
  hook for reminder pushes.
- `task.overdue` / `tasks.overdue` — the corresponding `UndoneTask(s)OverdueNotification` types
  have `ToDB()` return `nil`, so they never reach the `notifications` table and never fire
  `notification.created`. These dedicated events are the only hook for overdue pushes.
- `notification.created` — the generic catch-all for everything that does get persisted
  (comments, assignments, mentions, deletions, project/team events). Explicitly skips
  `Name == "task.reminder"` to avoid double-sending, since that's already handled above.

## Security model

- ntfy topics are bearer of their own security: anyone who knows an unprotected topic name can
  publish to and subscribe to it. Treat topic names like shared secrets unless your ntfy server
  has topic-level access control configured (see [ntfy's ACL docs](https://docs.ntfy.sh/config/#access-control)).
- This plugin's authenticated routes (under Vikunja's own JWT/session auth) are the only
  sanctioned way for a user to read or change their own targets. Never add an unauthenticated
  route here.
- Access tokens for protected topics are stored in this plugin's own `plugin_ntfy_targets` table,
  inside Vikunja's database — unlike vikunja-apprise, there is nowhere else to put them. Anyone
  with direct database access (e.g. an instance admin) can read them, same as they could read any
  other plugin's or Vikunja's own stored credentials.

## Runtime constraints (Yaegi)

Same constraints as `vikunja-apprise` — only Go's standard library plus Vikunja's exposed
`pkg/yaegi_symbols` packages are importable, no third-party Go modules, interpreted types need
`.Table("...")` on every xorm query since `TableName()` isn't visible, and both
`NewPlugin()`/`NewMigrationPlugin()`/`NewAuthenticatedRouterPlugin()` typed factories are required
for Yaegi's return-type-based interface wrapping. See that file for the full explanation.

Two more, found only by live end-to-end testing, not by inspection:

- **`os.Getenv` does not see the real process environment from inside interpreted code.**
  Confirmed directly: with `VIKUNJA_NTFY_DEFAULT_SERVER` visibly set on the running process (checked
  via `ps eww <pid>`), `os.Getenv("VIKUNJA_NTFY_DEFAULT_SERVER")` called from `main.go` still
  returned `""` every time. The practical effect: this plugin silently fell back to the public
  `https://ntfy.sh` instead of the local dev server for an entire round of testing, with no error
  anywhere — the HTTP call to the *wrong but real* server just quietly succeeded. **Do not use
  `os.Getenv` for plugin configuration.** Use `config.Key("plugins.ntfy.<name>").GetString()`
  instead (see `defaultServer()` in `main.go`) — `pkg/config` wraps viper directly, and
  `viper.AutomaticEnv()` with `SetEnvPrefix("vikunja")` (set once in Vikunja's own `config.go`)
  binds `VIKUNJA_PLUGINS_NTFY_DEFAULTSERVER` to that key without Vikunja core having to pre-declare
  it. The actual env lookup happens in viper's native code, called via a normal method dispatch
  from interpreted code — a pattern already proven to work everywhere else in this plugin — not
  through `os.Getenv` directly, so it isn't affected by whatever breaks that path.
- **A plugin write that doesn't call `s.Commit()` silently rolls back with no error.**
  `db.NewSession()` opens a transaction (see its own doc comment in Vikunja's `pkg/db/db.go`); if a
  handler never calls `s.Commit()`, the deferred `s.Close()` rolls everything back. This looks
  identical to success from the caller's side: no error, a normal HTTP response — the row just
  isn't there on the next read. Both write handlers here (`handleSetConfig`, `handleDeleteConfig`)
  call `s.Commit()`; if you add another write path, don't forget it.

## Local development

Expects a sibling Vikunja checkout, same layout as `vikunja-apprise`:

```
some-parent-dir/
├── vikunja/            # https://github.com/go-vikunja/vikunja, native toolchain via mise.toml
├── vikunja-apprise/     # sibling plugin
└── vikunja-ntfy/        # this repo
```

Everything is wrapped in `scripts/dev-up.sh`:

```bash
./scripts/dev-up.sh
```

It starts a local ntfy instance (`docker compose up -d`, served on `localhost:8080`), creates a
real directory (not a symlink — Yaegi's loader requires an actual directory for the top-level
plugin entry, since `os.ReadDir` + `DirEntry.IsDir()` doesn't resolve symlinks) under the Vikunja
checkout's plugin dir with a symlinked `main.go` back to this repo, creates a placeholder
`frontend/dist/index.html` if missing (`go:embed all:dist` requires that directory to exist even
for a backend-only dev run), and starts Vikunja with `VIKUNJA_SERVICE_PUBLICURL` set (required
whenever `cors.enable` is true, the default) and `VIKUNJA_PLUGINS_NTFY_DEFAULTSERVER` pointed at the
local ntfy instance.

There is **no hot reload** — restart (re-run the script) after every change to `main.go`.

Both this plugin and `vikunja-apprise` can be loaded into the same Vikunja instance at once — the
loader iterates every subdirectory of `plugins.dir` independently.

## Testing

Same pattern as `vikunja-apprise`: the automated smoke test lives inside the Vikunja checkout,
since it needs Vikunja's own internal `yaegi.LoadPluginFull` test helper:
`vikunja/pkg/plugins/yaegi/ntfy_plugin_test.go`.

```bash
cd ../vikunja
go test ./pkg/plugins/yaegi/... -run Ntfy -v
```

That only proves the plugin loads, its migration runs, and its routes are wired correctly (401
without auth) — not real delivery. To verify delivery end-to-end, with Vikunja and ntfy both
running as above:

```bash
curl -X POST http://localhost:3456/api/v1/plugins/ntfy/config \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"targets": [{"topic": "test"}]}'

# then trigger a real Vikunja event (e.g. a task reminder due in the next minute)
# and check http://localhost:8080/test (web UI) or `curl http://localhost:8080/test/json?poll=1`
```

## Status

Implemented and verified end-to-end against a real local ntfy instance: registered a user, set a
target topic via `POST /plugins/ntfy/config`, created a task with a reminder due the next minute,
and confirmed the reminder actually arrived on the configured topic with the expected title/body —
after finding and fixing two real bugs along the way (see "Runtime constraints" above): a missing
`s.Commit()` that silently discarded the config write, and `os.Getenv` silently not working inside
the interpreter, which had the plugin quietly delivering to the public `https://ntfy.sh` instead of
the configured server. Both are fixed and covered by the reasoning above so they don't reappear.

Not yet exercised end-to-end: the `notification.created` catch-all path (comments, assignments,
mentions, etc.) and the overdue-task listeners — only the reminder path has been proven with a real
delivery so far. The code path is the same, so this is a smaller gap than it sounds, but worth
doing before relying on it.
