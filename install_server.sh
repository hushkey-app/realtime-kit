#!/bin/sh
set -eu

VERSION=${VERSION:-latest}
INSTALL_DIR=${INSTALL_DIR:-/usr/local/bin}
CONFIG_DIR=${CONFIG_DIR:-/etc/livekit}
CONFIG_FILE=${CONFIG_FILE:-$CONFIG_DIR/livekit.yaml}
SERVICE_FILE=${SERVICE_FILE:-/etc/systemd/system/livekit-server.service}
RTC_PORT_START=${RTC_PORT_START:-50000}
RTC_PORT_END=${RTC_PORT_END:-60000}
USE_EXTERNAL_IP=${USE_EXTERNAL_IP:-true}
SHOW_CREDENTIALS=${SHOW_CREDENTIALS:-false}
BINARY_NAME=livekit-server

fail() {
  printf 'LiveKit server install: %s\n' "$*" >&2
  exit 1
}

[ "$(id -u)" -eq 0 ] || fail "run as root (for example: curl ... | sudo sh)"
[ "$(uname -s)" = Linux ] || fail "only native Linux installations are supported"
command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v systemctl >/dev/null 2>&1 || fail "systemd is required"
command -v tar >/dev/null 2>&1 || fail "tar is required"

case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac
case "$RTC_PORT_START:$RTC_PORT_END" in
  *[!0-9:]* | :* | *:) fail "RTC_PORT_START and RTC_PORT_END must be port numbers" ;;
esac
[ "$RTC_PORT_START" -le "$RTC_PORT_END" ] || fail "RTC_PORT_START must not exceed RTC_PORT_END"
case "$USE_EXTERNAL_IP" in true | false) ;; *) fail "USE_EXTERNAL_IP must be true or false" ;; esac
case "$SHOW_CREDENTIALS" in true | false) ;; *) fail "SHOW_CREDENTIALS must be true or false" ;; esac

if [ "$VERSION" = latest ]; then
  release_json=$(curl -fsSL --retry 3 https://api.github.com/repos/livekit/livekit/releases/latest)
  tag=$(printf '%s\n' "$release_json" | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
  [ -n "$tag" ] || fail "could not determine the latest LiveKit release"
else
  case "$VERSION" in v*) tag=$VERSION ;; *) tag="v$VERSION" ;; esac
fi
release_version=${tag#v}
asset="livekit_${release_version}_linux_${arch}.tar.gz"
release_url="https://github.com/livekit/livekit/releases/download/$tag"

work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM

printf 'Downloading LiveKit %s (%s)...\n' "$tag" "$arch"
curl -fL --retry 3 --retry-delay 1 -o "$work_dir/$asset" "$release_url/$asset"
curl -fL --retry 3 --retry-delay 1 -o "$work_dir/checksums.txt" "$release_url/checksums.txt"

expected=$(awk -v asset="$asset" '$2 == asset { print $1 }' "$work_dir/checksums.txt")
case "$expected" in
  *[!0-9a-fA-F]* | '') fail "official release checksum is missing or invalid" ;;
esac
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$work_dir/$asset" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$work_dir/$asset" | awk '{ print $1 }')
else
  fail "sha256sum or shasum is required"
fi
[ "$actual" = "$expected" ] || fail "official release checksum does not match"

tar -xzf "$work_dir/$asset" -C "$work_dir"
[ -x "$work_dir/$BINARY_NAME" ] || fail "release archive contains no livekit-server binary"
install -d -m 0755 "$INSTALL_DIR"
install -m 0755 "$work_dir/$BINARY_NAME" "$INSTALL_DIR/$BINARY_NAME.new"
mv -f "$INSTALL_DIR/$BINARY_NAME.new" "$INSTALL_DIR/$BINARY_NAME"

if ! id livekit >/dev/null 2>&1; then
  useradd --system --home-dir /nonexistent --shell /usr/sbin/nologin livekit
fi

install -d -o root -g livekit -m 0750 "$CONFIG_DIR"
if [ ! -f "$CONFIG_FILE" ]; then
  command -v openssl >/dev/null 2>&1 || fail "openssl is required for the first install"
  api_key="API$(openssl rand -hex 12)"
  api_secret=$(openssl rand -hex 32)
  umask 077
  cat >"$CONFIG_FILE" <<EOF
# Native single-node LiveKit. Put port 7880 behind trusted TLS termination.
port: 7880
rtc:
  tcp_port: 7881
  port_range_start: $RTC_PORT_START
  port_range_end: $RTC_PORT_END
  use_external_ip: $USE_EXTERNAL_IP
keys:
  $api_key: $api_secret
logging:
  level: info
  json: true
room:
  empty_timeout: 300
  departure_timeout: 20
prometheus_port: 6789
EOF
  chown root:livekit "$CONFIG_FILE"
  chmod 0640 "$CONFIG_FILE"
  created_config=true
else
  created_config=false
fi
chown root:livekit "$CONFIG_FILE"
chmod 0640 "$CONFIG_FILE"

cat >"$SERVICE_FILE" <<EOF
[Unit]
Description=LiveKit realtime media server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=livekit
Group=livekit
ExecStart=$INSTALL_DIR/$BINARY_NAME --config $CONFIG_FILE
Restart=on-failure
RestartSec=2s
LimitNOFILE=65535
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
systemctl enable livekit-server.service
systemctl restart livekit-server.service
systemctl is-active --quiet livekit-server.service || fail "service did not become active; inspect: journalctl -u livekit-server"

printf '\nInstalled: '
"$INSTALL_DIR/$BINARY_NAME" --version
printf 'Service:   systemctl status livekit-server\n'
printf 'Config:    %s\n' "$CONFIG_FILE"
if [ "$created_config" = true ] && [ "$SHOW_CREDENTIALS" = true ]; then
  printf '\nWorkspace provider credentials (store these securely):\n'
  printf '  API key:    %s\n' "$api_key"
  printf '  API secret: %s\n' "$api_secret"
elif [ "$created_config" = true ]; then
  printf 'A new API key and secret were written to the protected config.\n'
  printf 'Inspect %s interactively as root; credentials are hidden from install logs.\n' "$CONFIG_FILE"
else
  printf 'Existing configuration and API keys were preserved.\n'
fi
printf '\nRequired before public use:\n'
printf '  - Terminate trusted TLS for port 7880 and use wss://your-livekit-domain\n'
printf '  - Open TCP 7881 and UDP %s-%s to clients\n' "$RTC_PORT_START" "$RTC_PORT_END"
printf '  - Configure TURN/TLS for restrictive client networks\n'
