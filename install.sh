#!/bin/sh
set -eu

# Backward-compatible name. New documentation uses install_sidecar.sh so the
# gateway and the LiveKit media server cannot be confused during deployment.
case "$0" in
  */*)
    script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
    if [ -f "$script_dir/install_sidecar.sh" ]; then
      exec sh "$script_dir/install_sidecar.sh" "$@"
    fi
    ;;
esac

url=${INSTALL_SIDECAR_URL:-https://raw.githubusercontent.com/hushkey-app/realtime-kit/main/install_sidecar.sh}
command -v curl >/dev/null 2>&1 || {
  printf 'realtime-kit install: curl is required\n' >&2
  exit 1
}
installer=$(mktemp)
trap 'rm -f "$installer"' EXIT HUP INT TERM
curl -fL --retry 3 --retry-delay 1 -o "$installer" "$url"
sh "$installer" "$@"
