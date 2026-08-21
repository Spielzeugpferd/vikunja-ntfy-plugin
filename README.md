# vikunja-ntfy

A [Vikunja](https://github.com/go-vikunja/vikunja) plugin that forwards task reminders and other
notifications directly to one or more [ntfy](https://github.com/binwiederhier/ntfy) topics — no
bridge service in between. If you already run (or are fine using the public `ntfy.sh`) an ntfy
instance, this is a simpler alternative to [vikunja-apprise](../vikunja-apprise), at the cost of
only reaching ntfy instead of 100+ services.

It's a source plugin for Vikunja's Yaegi plugin loader — Vikunja interprets `main.go` at startup,
no compiled binary and no Vikunja fork required.

## What it does

- Exposes three authenticated routes for managing your own ntfy targets:
  `POST/GET/DELETE /api/v1/plugins/ntfy/config`. Each target is a topic (plus an optional server
  override and an optional access token, for protected topics).
- Listens on Vikunja's internal event bus for task reminders, overdue tasks, and every other
  notification type (comments, assignments, mentions, project/team events), and publishes each one
  to every configured target for the affected user via ntfy's JSON publish API.

## Requirements

- A Vikunja instance with `plugins.enabled: true` and `plugins.loader: yaegi`.
- An ntfy server reachable from the Vikunja backend — either the public `https://ntfy.sh` (no
  setup required, but do not use it for sensitive data — topics are public unless protected) or
  your own self-hosted instance. See `docker-compose.yml` in this repo for a local example.

## Installation

1. Copy (or, for local development, symlink) this directory into Vikunja's plugin directory, e.g.
   `plugins/vikunja-ntfy/` relative to Vikunja's `service.rootpath` (`plugins.dir` if changed).
2. Set the default ntfy server and enable the Vikunja plugin system:

   ```bash
   VIKUNJA_PLUGINS_ENABLED=true
   VIKUNJA_PLUGINS_LOADER=yaegi
   VIKUNJA_PLUGINS_NTFY_DEFAULTSERVER=https://ntfy.sh   # or your own self-hosted instance
   ```

3. Restart Vikunja. There is no hot reload — the plugin directory is only read at startup.

## Usage

Pick a topic name (treat it like a shared secret unless your server protects it) and register it:

```bash
curl -X POST https://your-vikunja-instance/api/v1/plugins/ntfy/config \
  -H "Authorization: Bearer $VIKUNJA_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"targets": [{"topic": "my-vikunja-notifications"}]}'
```

Subscribe to that topic in the [ntfy app](https://ntfy.sh/#subscribe) or web UI, and you'll start
receiving Vikunja's reminders and notifications there.

For a protected topic on your own server, include a token:

```bash
curl -X POST https://your-vikunja-instance/api/v1/plugins/ntfy/config \
  -H "Authorization: Bearer $VIKUNJA_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"targets": [{"server": "https://ntfy.example.com", "topic": "my-topic", "token": "tk_..."}]}'
```

## Security

Unlike [vikunja-apprise](../vikunja-apprise), which keeps all notification-service secrets out of
Vikunja's own database (Apprise API owns that store), this plugin has nowhere else to put a
protected topic's access token — ntfy has no equivalent config-store service — so it stores it in
Vikunja's own database, in the `plugin_ntfy_targets` table. That's the trade-off for this plugin's
simplicity: no extra bridge service, but a small amount of secret material now lives in Vikunja's
data at rest. If that's not acceptable for your deployment, use unprotected topics with
hard-to-guess names on a server you trust, or use vikunja-apprise instead.

## Development

See [AGENTS.md](./AGENTS.md) for the architecture rationale, Yaegi runtime constraints, and the
local dev/test workflow.

## License

MIT — see [LICENSE](./LICENSE). This plugin is not affiliated with the Vikunja or ntfy projects;
it's a small bridge between two independently licensed open-source tools (Vikunja: AGPL-3.0, ntfy:
Apache-2.0/GPLv2 dual-licensed). Running this plugin against your own Vikunja instance over its
public API/event bus does not modify Vikunja's own source.
