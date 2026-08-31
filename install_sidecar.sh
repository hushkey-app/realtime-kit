#!/bin/sh
set -eu

REPOSITORY=${REPOSITORY:-hushkey-app/realtime-kit}
VERSION=${VERSION:-latest}
INSTALL_DIR=${INSTALL_DIR:-/usr/local/bin}
CONFIG_DIR=${CONFIG_DIR:-/etc/realtime-kit}
SERVICE_FILE=${SERVICE_FILE:-/etc/systemd/system/realtime-kit.service}
BINARY_NAME=realtime-kit
ENV_FILE="$CONFIG_DIR/realtime-kit.env"

fail() {
  printf 'realtime-kit sidecar install: %s\n' "$*" >&2
  exit 1
}

[ "$(id -u)" -eq 0 ] || fail "run as root (for example: curl ... | sudo sh)"
[ "$(uname -s)" = Linux ] || fail "only native Linux installations are supported"
command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v systemctl >/dev/null 2>&1 || fail "systemd is required"

case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac

asset="${BINARY_NAME}_linux_${arch}"
if [ "$VERSION" = latest ]; then
  release_url="https://github.com/$REPOSITORY/releases/latest/download"
else
  case "$VERSION" in v*) ;; *) VERSION="v$VERSION" ;; esac
  release_url="https://github.com/$REPOSITORY/releases/download/$VERSION"
fi

work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM

printf 'Downloading %s (%s)...\n' "$BINARY_NAME" "$arch"
curl -fL --retry 3 --retry-delay 1 -o "$work_dir/$asset" "$release_url/$asset"
curl -fL --retry 3 --retry-delay 1 -o "$work_dir/$asset.sha256" "$release_url/$asset.sha256"

expected=$(awk 'NR == 1 { print $1 }' "$work_dir/$asset.sha256")
case "$expected" in
  *[!0-9a-fA-F]* | '') fail "release checksum is invalid" ;;
esac
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$work_dir/$asset" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$work_dir/$asset" | awk '{ print $1 }')
else
  fail "sha256sum or shasum is required"
fi
[ "$actual" = "$expected" ] || fail "release checksum does not match"

install -d -m 0755 "$INSTALL_DIR"
install -m 0755 "$work_dir/$asset" "$INSTALL_DIR/$BINARY_NAME.new"
mv -f "$INSTALL_DIR/$BINARY_NAME.new" "$INSTALL_DIR/$BINARY_NAME"

if ! id realtime-kit >/dev/null 2>&1; then
  useradd --system --home-dir /nonexistent --shell /usr/sbin/nologin realtime-kit
fi

install -d -m 0750 "$CONFIG_DIR"
if [ ! -f "$ENV_FILE" ]; then
  command -v openssl >/dev/null 2>&1 || fail "openssl is required for the first install"
  token=$(openssl rand -hex 32)
  umask 077
  cat >"$ENV_FILE" <<EOF
# Private bearer token used only between Pack and realtime-kit.
# This is not a LiveKit API key or API secret.
REALTIME_KIT_TOKEN=$token
REALTIME_KIT_ADDR=127.0.0.1:7890
REALTIME_KIT_UPSTREAM_TIMEOUT=10s
REALTIME_KIT_READ_TIMEOUT=15s
REALTIME_KIT_WRITE_TIMEOUT=30s
REALTIME_KIT_SHUTDOWN_GRACE=10s
REALTIME_KIT_DEBUG=false
EOF
  chmod 0600 "$ENV_FILE"
  created_config=true
else
  created_config=false
fi

cat >"$SERVICE_FILE" <<EOF
[Unit]
Description=realtime-kit LiveKit gateway
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=realtime-kit
Group=realtime-kit
EnvironmentFile=$ENV_FILE
ExecStart=$INSTALL_DIR/$BINARY_NAME
Restart=on-failure
RestartSec=2s
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable realtime-kit.service
systemctl restart realtime-kit.service
systemctl is-active --quiet realtime-kit.service || fail "service did not become active; inspect: journalctl -u realtime-kit"

printf '\nInstalled: '
"$INSTALL_DIR/$BINARY_NAME" --version
printf 'Service:   systemctl status realtime-kit\n'
printf 'Config:    %s\n' "$ENV_FILE"
if [ "$created_config" = true ]; then
  printf 'Important: set Pack DENO_REALTIME_KIT_URL=http://127.0.0.1:7890\n'
  printf 'Important: copy REALTIME_KIT_TOKEN from the root-only config into Pack.\n'
else
  printf 'Configuration and REALTIME_KIT_TOKEN were preserved.\n'
fi
