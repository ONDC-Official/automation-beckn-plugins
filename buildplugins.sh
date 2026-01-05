#!/usr/bin/env bash
set -euo pipefail

# Builds Go plugins (.so) into ./plugins/<name>.so
#
# Usage:
#   ./buildplugins.sh
#
# Outputs:
#   ./plugins/ondcValidator.so
#   ./plugins/workbench.so
#   ./plugins/keymanager.so

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PLUGINS_DIR="$ROOT_DIR/plugins"

mkdir -p "$PLUGINS_DIR"

GOOS="$(go env GOOS)"
if [[ "$GOOS" == "windows" ]]; then
	echo "Go plugins (-buildmode=plugin) are not supported on Windows." >&2
	exit 1
fi

build_plugin() {
	local name="$1"
	local module_dir="$2"
	local pkg="$3"
	local out="$PLUGINS_DIR/${name}.so"

	echo "==> Building ${name}.so from ${module_dir} (${pkg})"
	( cd "$ROOT_DIR/$module_dir" && go build -buildmode=plugin -trimpath -o "$out" "$pkg" )
	echo "    Wrote: $out"
}

build_plugin "ondcValidator" "ondcValidator" "./cmd"
build_plugin "workbench" "workbench-main" "./cmd"
build_plugin "keymanager" "workbench-keymanager" "./cmd"

echo "Done. Plugins are in: $PLUGINS_DIR"
