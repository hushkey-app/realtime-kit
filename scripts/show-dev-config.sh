#!/bin/sh
set -eu

cd "$(dirname "$0")/.."
[ -f .env ] || {
  printf 'No generated development credentials yet; run make dev first.\n' >&2
  exit 1
}

set -a
. ./.env
set +a

: "${REALTIME_KIT_TOKEN:?run make dev to generate REALTIME_KIT_TOKEN}"
: "${LIVEKIT_API_KEY:?run make dev to generate LIVEKIT_API_KEY}"
: "${LIVEKIT_API_SECRET:?run make dev to generate LIVEKIT_API_SECRET}"
: "${LIVEKIT_URL:?run make dev to generate LIVEKIT_URL}"

printf 'Pack process environment:\n'
printf '  DENO_REALTIME_KIT_URL=http://127.0.0.1:7890\n'
printf '  DENO_REALTIME_KIT_TOKEN=%s\n' "$REALTIME_KIT_TOKEN"
printf '\nWorkspace voice provider form:\n'
printf '  API key:    %s\n' "$LIVEKIT_API_KEY"
printf '  API secret: %s\n' "$LIVEKIT_API_SECRET"
printf '  Server URL: %s\n' "$LIVEKIT_URL"
