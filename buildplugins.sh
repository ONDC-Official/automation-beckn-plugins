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
GO_BIN="${GO:-go}"
TRIMPATH="${TRIMPATH:-0}"

mkdir -p "$PLUGINS_DIR"

echo "Go: $($GO_BIN version)"
echo "Go env: GOOS=$($GO_BIN env GOOS) GOARCH=$($GO_BIN env GOARCH) GOROOT=$($GO_BIN env GOROOT)"

GOOS="$($GO_BIN env GOOS)"
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
	# IMPORTANT: Go plugins require the host binary and plugin to be built with the *same* toolchain
	# and the *same* relevant build flags. Using -trimpath only for plugins is a common cause of:
	#   "plugin was built with a different version of package runtime"
	# Enable TRIMPATH=1 only if you also build the host with -trimpath.
	if [[ "$TRIMPATH" == "1" ]]; then
		( cd "$ROOT_DIR/$module_dir" && "$GO_BIN" build -buildmode=plugin -trimpath -o "$out" "$pkg" )
	else
		( cd "$ROOT_DIR/$module_dir" && "$GO_BIN" build -buildmode=plugin -o "$out" "$pkg" )
	fi
	echo "    Wrote: $out"
}

build_plugin "ondcvalidator" "ondc-validator" "./cmd"
build_plugin "workbench" "workbench-main" "./cmd"
build_plugin "keymanager" "workbench-keymanager" "./cmd"
build_plugin "networkobservability" "network-observability" "./cmd"
build_plugin "cache" "cache" "./cmd"
build_plugin "router" "router" "./cmd"
build_plugin "schemavalidator" "schemavalidator" "./cmd"
build_plugin "signvalidator" "signvalidator" "./cmd"
build_plugin "signer" "signer" "./cmd"

echo "Done. Plugins are in: $PLUGINS_DIR"
