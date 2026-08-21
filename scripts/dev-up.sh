#!/usr/bin/env bash
# Local workbench for this plugin: brings up a local ntfy server, wires this
# repo into a sibling Vikunja checkout, and runs Vikunja with the plugin loaded.
#
# Layout expected (see AGENTS.md):
#   some-parent-dir/
#   ├── vikunja/         (https://github.com/go-vikunja/vikunja, mise-managed toolchain)
#   └── vikunja-ntfy/     (this repo)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PLUGIN_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
VIKUNJA_DIR="$(cd "$PLUGIN_DIR/.." && pwd)/vikunja"

if [ ! -f "$VIKUNJA_DIR/go.mod" ]; then
	echo "error: expected a Vikunja checkout at $VIKUNJA_DIR (see AGENTS.md's 'Local development')" >&2
	exit 1
fi

echo "==> Starting local ntfy server (docker compose)…"
(cd "$PLUGIN_DIR" && docker compose up -d)

echo "==> Wiring the plugin into $VIKUNJA_DIR/plugins/vikunja-ntfy …"
mkdir -p "$VIKUNJA_DIR/plugins/vikunja-ntfy"
ln -sf "$PLUGIN_DIR/main.go" "$VIKUNJA_DIR/plugins/vikunja-ntfy/main.go"

# go:embed all:dist (frontend/embed.go) requires this directory to exist, even
# for a backend-only dev run with no real frontend build.
if [ ! -e "$VIKUNJA_DIR/frontend/dist/index.html" ]; then
	echo "==> Creating a placeholder frontend/dist (backend-only dev build)…"
	mkdir -p "$VIKUNJA_DIR/frontend/dist"
	echo "<html><body>placeholder for backend-only dev</body></html>" >"$VIKUNJA_DIR/frontend/dist/index.html"
fi

echo "==> Starting Vikunja with the plugin loaded on http://localhost:3456 …"
cd "$VIKUNJA_DIR"
if command -v mise >/dev/null 2>&1; then
	eval "$(mise activate bash)"
fi

exec env \
	VIKUNJA_PLUGINS_ENABLED=true \
	VIKUNJA_PLUGINS_LOADER=yaegi \
	VIKUNJA_PLUGINS_NTFY_DEFAULTSERVER=http://localhost:8080 \
	VIKUNJA_SERVICE_PUBLICURL=http://localhost:3456/ \
	VIKUNJA_DATABASE_PATH=./vikunja-workbench.db \
	VIKUNJA_SERVICE_INTERFACE=:3456 \
	go run main.go
