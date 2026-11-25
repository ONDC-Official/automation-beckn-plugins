#!/usr/bin/env bash
set -euo pipefail

# Builds Go plugins (.so) into a specified directory
#
# Usage:
#   ./buildplugins.sh [PLUGINS_DIR]
#
# Arguments:
#   PLUGINS_DIR - Optional. Directory where plugins will be built (default: ./plugins)
#
# Examples:
#   ./buildplugins.sh                          # Uses ./plugins
#   ./buildplugins.sh /custom/path/plugins     # Uses /custom/path/plugins
#   ./buildplugins.sh build-output/plugins     # Uses build-output/plugins
#   PWB_PROFILE=fd ./buildplugins.sh           # Also builds the FD-only plugins
#
# Environment:
#   PWB_PROFILE - Optional. "fd" also builds the FD (ONDC:FIS14:FD) plugins fdgate.so and
#                 fdtransport.so. Unset (Beckn images) builds exactly the Beckn set.
#
# Outputs:
#   <PLUGINS_DIR>/ondcValidator.so
#   <PLUGINS_DIR>/workbench.so
#   <PLUGINS_DIR>/keymanager.so
#   ... etc


ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ $# -gt 0 && -n "$1" ]]; then
    PLUGINS_DIR="$1"
    if [[ "$PLUGINS_DIR" != /* ]]; then
        PLUGINS_DIR="$ROOT_DIR/$PLUGINS_DIR"
    fi
else
    PLUGINS_DIR="$ROOT_DIR/plugins"
fi
GO_BIN="${GO:-go}"
TRIMPATH="${TRIMPATH:-0}"
mkdir -p "$PLUGINS_DIR"

build_plugin() {
    local name="$1"
    local module_dir="$2"
    local pkg="$3"
    local out="$PLUGINS_DIR/${name}.so"
    echo "==> Building ${name}.so from ${module_dir}"
    if [[ "$TRIMPATH" == "1" ]]; then
        ( cd "$ROOT_DIR/$module_dir" && "$GO_BIN" build -buildmode=plugin -trimpath -o "$out" "$pkg" )
    else
        ( cd "$ROOT_DIR/$module_dir" && "$GO_BIN" build -buildmode=plugin -o "$out" "$pkg" )
    fi
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
build_plugin "encryptionmiddleware" "encryption-middleware" "./cmd"
build_plugin "outgoingencryptionmiddleware" "outgoing-encryption-middleware" "./cmd"
build_plugin "callbackredirect" "workbench-callback-redirect" "./cmd"

# FD-only plugins, compiled into FD images only.
if [[ "${PWB_PROFILE:-}" == "fd" ]]; then
    build_plugin "fdgate" "fd-gate" "./cmd"
    build_plugin "fdtransport" "fd-transport" "./cmd"
fi

echo "✅ Done! Plugins are in: $PLUGINS_DIR"
