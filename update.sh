#!/bin/sh
set -eu

INSTALL_URL=${INSTALL_URL:-https://raw.githubusercontent.com/hushkey-app/realtime-kit/main/install_sidecar.sh}
VERSION=${VERSION:-latest}

command -v curl >/dev/null 2>&1 || {
  printf 'realtime-kit update: curl is required\n' >&2
  exit 1
}

installer=$(mktemp)
trap 'rm -f "$installer"' EXIT HUP INT TERM
curl -fL --retry 3 --retry-delay 1 -o "$installer" "$INSTALL_URL"

if [ "$(id -u)" -eq 0 ]; then
  VERSION="$VERSION" sh "$installer"
elif command -v sudo >/dev/null 2>&1; then
  sudo env VERSION="$VERSION" sh "$installer"
else
  printf 'realtime-kit update: root or sudo is required\n' >&2
  exit 1
fi
