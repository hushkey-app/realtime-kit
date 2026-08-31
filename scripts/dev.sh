#!/bin/sh
set -eu

cd "$(dirname "$0")/.."

fail() {
  printf 'realtime-kit dev: %s\n' "$*" >&2
  exit 1
}

command -v openssl >/dev/null 2>&1 || fail "openssl is required to generate local credentials"
command -v curl >/dev/null 2>&1 || fail "curl is required for the local readiness check"

livekit_bin=${LIVEKIT_SERVER_BIN:-}
if [ -z "$livekit_bin" ]; then
  livekit_bin=$(command -v livekit-server || true)
fi
if [ -z "$livekit_bin" ]; then
  case "$(uname -s)" in
    Darwin) fail "livekit-server is missing; install the native binary with: brew install livekit" ;;
    Linux) fail "livekit-server is missing; install the native binary with: curl -sSL https://get.livekit.io | bash" ;;
    *) fail "livekit-server is missing; install it and set LIVEKIT_SERVER_BIN" ;;
  esac
fi
[ -x "$livekit_bin" ] || fail "LIVEKIT_SERVER_BIN is not executable: $livekit_bin"

touch .env
chmod 0600 .env

# Read existing values first. Appending only missing values makes generation
# portable across macOS and Linux without sed -i differences. If a template has
# an empty assignment, the later generated assignment intentionally wins.
set -a
. ./.env
set +a

append_if_missing() {
  name=$1
  value=$2
  eval "current=\${$name:-}"
  if [ -z "$current" ]; then
    printf '%s=%s\n' "$name" "$value" >>.env
    eval "$name=\$value"
    export "$name"
  fi
}

append_if_missing REALTIME_KIT_TOKEN "$(openssl rand -hex 32)"
append_if_missing REALTIME_KIT_ADDR "127.0.0.1:7890"
append_if_missing LIVEKIT_API_KEY "API$(openssl rand -hex 12)"
append_if_missing LIVEKIT_API_SECRET "$(openssl rand -hex 32)"
append_if_missing LIVEKIT_URL "ws://127.0.0.1:7880"

case "$LIVEKIT_URL" in
  ws://127.0.0.1:7880 | ws://localhost:7880) ;;
  *) fail "make dev owns local port 7880; set LIVEKIT_URL=ws://127.0.0.1:7880 in .env" ;;
esac

livekit_pid=
cleanup() {
  if [ -n "$livekit_pid" ] && kill -0 "$livekit_pid" 2>/dev/null; then
    kill "$livekit_pid" 2>/dev/null || true
    wait "$livekit_pid" 2>/dev/null || true
  fi
}
trap cleanup EXIT HUP INT TERM

LIVEKIT_KEYS="$LIVEKIT_API_KEY: $LIVEKIT_API_SECRET" \
  "$livekit_bin" --dev --bind 127.0.0.1 &
livekit_pid=$!

# Do not launch the sidecar until LiveKit signalling is accepting connections.
attempt=0
until curl -sS --max-time 1 http://127.0.0.1:7880 >/dev/null 2>&1; do
  if ! kill -0 "$livekit_pid" 2>/dev/null; then
    wait "$livekit_pid" || true
    fail "livekit-server exited during startup"
  fi
  attempt=$((attempt + 1))
  [ "$attempt" -lt 50 ] || fail "livekit-server did not listen on 127.0.0.1:7880"
  sleep 0.1
done

printf '\nPack process environment:\n'
printf '  DENO_REALTIME_KIT_URL=http://127.0.0.1:7890\n'
if [ "${DEV_SHOW_SECRETS:-true}" = true ]; then
  printf '  DENO_REALTIME_KIT_TOKEN=%s\n' "$REALTIME_KIT_TOKEN"
else
  printf '  DENO_REALTIME_KIT_TOKEN=<hidden>\n'
fi
printf '\nWorkspace voice provider form:\n'
if [ "${DEV_SHOW_SECRETS:-true}" = true ]; then
  printf '  API key:    %s\n' "$LIVEKIT_API_KEY"
  printf '  API secret: %s\n' "$LIVEKIT_API_SECRET"
else
  printf '  API key:    <hidden>\n'
  printf '  API secret: <hidden>\n'
fi
printf '  Server URL: %s\n\n' "$LIVEKIT_URL"
printf 'Credentials are preserved in %s/.env (mode 0600).\n\n' "$(pwd)"

go run .
